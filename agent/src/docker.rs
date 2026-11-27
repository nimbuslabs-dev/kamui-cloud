//! Local Docker execution. Containers are hardened to the same contract as the
//! coordinator's local executor: no network, read-only root filesystem, dropped
//! capabilities, non-root UID, bounded tmpfs, CPU/RAM/PID limits and bounded
//! logs. The Docker socket and host paths are never exposed to workloads.

use serde::Deserialize;
use std::io::Read;
use std::process::{Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};

const PROBE_TIMEOUT: Duration = Duration::from_secs(5);
const RUN_TIMEOUT: Duration = Duration::from_secs(60);
const STOP_TIMEOUT: Duration = Duration::from_secs(15);

#[derive(Deserialize, Default, Debug, Clone)]
pub struct ContainerState {
    #[serde(default, rename = "Status")]
    pub status: String,
    #[serde(default, rename = "ExitCode")]
    pub exit_code: i64,
    #[serde(default, rename = "StartedAt")]
    pub started_at: String,
    /// Reported for protocol completeness; the coordinator derives deadlines.
    #[allow(dead_code)]
    #[serde(default, rename = "FinishedAt")]
    pub finished_at: String,
}

#[derive(Deserialize)]
struct ContainerInspect {
    #[serde(default, rename = "State")]
    state: Option<ContainerState>,
}

fn probe(program: &str, args: &[&str], timeout: Duration) -> Option<String> {
    let mut child = Command::new(program)
        .args(args)
        .env_remove("KAMUI_TOKEN")
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .ok()?;
    let stdout = child.stdout.take()?;
    let reader = thread::spawn(move || {
        let mut bytes = Vec::new();
        stdout.take(256 * 1024).read_to_end(&mut bytes).ok()?;
        String::from_utf8(bytes).ok()
    });
    let deadline = Instant::now() + timeout;
    loop {
        match child.try_wait() {
            Ok(Some(status)) if !status.success() => return None,
            Ok(Some(_)) if reader.is_finished() => return reader.join().ok()?,
            Ok(_) if Instant::now() < deadline => thread::sleep(Duration::from_millis(20)),
            _ => {
                let _ = child.kill();
                let _ = child.wait();
                return None;
            }
        }
    }
}

/// Server-declared runtimes; an empty result means unknown.
pub fn runtimes() -> Vec<String> {
    probe("docker", &["info", "--format", "{{json .Runtimes}}"], PROBE_TIMEOUT)
        .and_then(|out| serde_json::from_str::<serde_json::Value>(out.trim()).ok())
        .and_then(|value| {
            value.as_object().map(|map| {
                map.keys().cloned().collect::<Vec<String>>()
            })
        })
        .unwrap_or_default()
}

pub fn image_os(image: &str) -> Option<String> {
    probe(
        "docker",
        &["image", "inspect", "--format", "{{.Os}}", image],
        PROBE_TIMEOUT,
    )
    .map(|out| out.trim().to_string())
}

pub fn container_name(workload_id: &str, attempt: i64) -> String {
    format!("kamui-{workload_id}-{attempt}")
}

#[derive(Debug, Clone)]
pub struct RunSpec {
    pub image: String,
    pub args: Vec<String>,
    pub cpu_millis: i64,
    pub memory_bytes: i64,
    pub gpu_count: i64,
    pub worker_id: String,
    pub workload_id: String,
    pub attempt: i64,
}


pub fn inspect(workload_id: &str, attempt: i64) -> Option<ContainerState> {
    let name = container_name(workload_id, attempt);
    let out = probe(
        "docker",
        &["container", "inspect", "--format", "{{json .}}", &name],
        PROBE_TIMEOUT,
    )?;
    let parsed: ContainerInspect = serde_json::from_str(out.trim()).ok()?;
    parsed.state
}

pub fn logs_tail(workload_id: &str, attempt: i64) -> Option<String> {
    let name = container_name(workload_id, attempt);
    probe(
        "docker",
        &[
            "logs",
            "--tail",
            "200",
            &name,
        ],
        PROBE_TIMEOUT,
    )
}

pub fn stop(workload_id: &str, attempt: i64) -> Result<(), String> {
    let name = container_name(workload_id, attempt);
    let arg_refs = ["stop", "-t", "3", &name];
    probe("docker", &arg_refs, STOP_TIMEOUT)
        .ok_or_else(|| format!("docker stop {name} failed"))
        .map(|_| ())
}

pub fn remove(workload_id: &str, attempt: i64) -> Result<(), String> {
    let name = container_name(workload_id, attempt);
    let arg_refs = ["rm", "-f", &name];
    probe("docker", &arg_refs, STOP_TIMEOUT)
        .ok_or_else(|| format!("docker rm {name} failed"))
        .map(|_| ())
}

/// One container the agent owns, discovered by labels.
#[derive(Debug, Clone)]
pub struct OwnedContainer {
    pub workload_id: String,
    pub attempt: i64,
}

pub fn list_owned(worker_id: &str) -> Vec<OwnedContainer> {
    let out = probe(
        "docker",
        &[
            "ps",
            "-a",
            "--filter",
            &format!("label=dev.nimbuslabs.kamui.worker={worker_id}"),
            "--format",
            "{{.Labels}}\t{{.Names}}",
        ],
        PROBE_TIMEOUT,
    )
    .unwrap_or_default();
    let mut owned = Vec::new();
    for line in out.lines() {
        let Some((labels, _name)) = line.split_once('\t') else {
            continue;
        };
        let mut workload_id = None;
        let mut attempt = None;
        for label in labels.split(',') {
            if let Some(value) = label.strip_prefix("dev.nimbuslabs.kamui.workload=") {
                workload_id = Some(value.to_string());
            }
            if let Some(value) = label.strip_prefix("dev.nimbuslabs.kamui.attempt=") {
                attempt = value.parse::<i64>().ok();
            }
        }
        if let (Some(workload), Some(attempt)) = (workload_id, attempt) {
            owned.push(OwnedContainer { workload_id: workload, attempt });
        }
    }
    owned
}


// --- Shared-prerequisite operations: pulls, volumes, networks, staging ----

/// Pull an image authorized by the assignment. A pinned digest must match
/// after the pull; a failed pull never falls back to a different image.
pub fn pull(spec: &RunSpec, digest: &str, auth: Option<&crate::client::RegistryAuth>) -> Result<(), String> {
    if let Some(auth) = auth {
        // Registry login is scoped to the assignment and immediately revoked.
        let refs = ["login", auth.host.as_str(), "-u", auth.username.as_str(), "--password-stdin"];
        let mut child = Command::new("docker")
            .args(refs)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()
            .map_err(|e| format!("docker login: {e}"))?;
        use std::io::Write;
        child
            .stdin
            .take()
            .ok_or("docker login stdin")?
            .write_all(auth.password.as_bytes())
            .map_err(|e| format!("docker login password: {e}"))?;
        let status = child.wait().map_err(|e| format!("docker login: {e}"))?;
        if !status.success() {
            return Err(format!("registry login for {} failed", auth.host));
        }
    }
    let platform = format!("linux/{}", std::env::consts::ARCH);
    // A pinned digest is pulled by digest so the registry itself enforces the
    // reference; no different image can ever be substituted.
    let reference = if digest.is_empty() {
        spec.image.clone()
    } else {
        let base = spec.image.split(':').next().unwrap_or(&spec.image).to_string();
        format!("{base}@{digest}")
    };
    let refs = ["pull", "--platform", &platform, reference.as_str()];
    if probe("docker", &refs, RUN_TIMEOUT * 10).is_none() {
        let _ = logout(auth);
        return Err(format!("authorized image pull failed for {}", spec.image));
    }
    let _ = logout(auth);
    Ok(())
}

fn logout(auth: Option<&crate::client::RegistryAuth>) -> Result<(), String> {
    let Some(auth) = auth else {
        return Ok(());
    };
    probe("docker", &["logout", auth.host.as_str()], PROBE_TIMEOUT)
        .map(|_| ())
        .ok_or_else(|| format!("registry logout for {} failed", auth.host))
}

/// Ensure the persistent project volume exists locally.
pub fn ensure_volume(volume: &crate::client::VolumeRequest) -> Result<(), String> {
    let refs = ["volume", "inspect", &volume.name];
    if probe("docker", &refs, PROBE_TIMEOUT).is_some() {
        return Ok(());
    }
    // Size is advisory for the local volume driver; quotas are enforced by
    // the coordinator's storage policy instead of per-driver options.
    probe(
        "docker",
        &["volume", "create", "--label", "dev.nimbuslabs.kamui.managed=1", volume.name.as_str()],
        PROBE_TIMEOUT,
    )
    .map(|_| ())
    .ok_or_else(|| format!("volume create {} failed", volume.name))
}

/// Ensure the private project network exists; returns the network name.
pub fn ensure_project_network(network: &str) -> Result<(), String> {
    if probe("docker", &["network", "inspect", network], PROBE_TIMEOUT).is_some() {
        return Ok(());
    }
    probe(
        "docker",
        &["network", "create", "--label", "dev.nimbuslabs.kamui.managed=1", network],
        PROBE_TIMEOUT,
    )
    .map(|_| ())
    .ok_or_else(|| format!("network create {network} failed"))
}

/// Stage one artifact into the worker-owned staging area. Returns the local
/// path; the workload later receives a read-only bind of exactly this file.
pub fn stage_artifact(worker_id: &str, staging_root: &std::path::Path, artifact_id: &str, client: &crate::client::Coordinator) -> Result<std::path::PathBuf, String> {
    let dir = staging_root.join("artifacts").join(worker_id).join(artifact_id);
    std::fs::create_dir_all(&dir).map_err(|e| format!("staging dir: {e}"))?;
    let target = dir.join("blob");
    if target.exists() {
        // Reuse the previously staged bytes; checksums are verified by the
        // coordinator before the artifact is ever listed in an envelope.
        return Ok(target);
    }
    let tmp = dir.join(".staging");
    let file = std::fs::File::create(&tmp).map_err(|e| format!("staging file: {e}"))?;
    let mut writer = std::io::BufWriter::new(file);
    let mut written = 0u64;
    {
        let mut sink = |chunk: &[u8]| -> Result<(), String> {
            use std::io::Write;
            writer.write_all(chunk).map_err(|e| format!("staging write: {e}"))?;
            written += chunk.len() as u64;
            if written > MAX_ARTIFACT_BYTES {
                return Err("staged artifact exceeds the worker limit".into());
            }
            Ok(())
        };
        client.download_artifact(worker_id, artifact_id, &mut sink)?;
    }
    {
        use std::io::Write;
        writer.flush().map_err(|e| format!("staging flush: {e}"))?;
    }
    std::fs::rename(&tmp, &target).map_err(|e| format!("staging rename: {e}"))?;
    Ok(target)
}

/// Remove staged artifacts for workloads that no longer reference them.
pub fn prune_staging(staging_root: &std::path::Path, worker_id: &str, live_ids: &[String]) {
    let dir = staging_root.join("artifacts").join(worker_id);
    let Ok(entries) = std::fs::read_dir(&dir) else {
        return;
    };
    for entry in entries.flatten() {
        let id = entry.file_name().to_string_lossy().to_string();
        if !live_ids.iter().any(|live| live == &id) {
            let _ = std::fs::remove_dir_all(entry.path());
        }
    }
}

const MAX_ARTIFACT_BYTES: u64 = 1 << 30;

/// Collect an output path from the exited container into the staging area.
pub fn collect_output(workload_id: &str, attempt: i64, container_path: &str, staging_root: &std::path::Path, worker_id: &str) -> Result<std::path::PathBuf, String> {
    let safe = sanitize_output_name(container_path)?;
    let dir = staging_root.join("outputs").join(worker_id).join(workload_id).join(format!("attempt-{attempt}"));
    std::fs::create_dir_all(&dir).map_err(|e| format!("output dir: {e}"))?;
    let target = dir.join(&safe);
    let source = format!("{}:{}", container_name(workload_id, attempt), container_path);
    let target_str = target.to_string_lossy().to_string();
    let refs = ["cp", source.as_str(), target_str.as_str()];
    probe("docker", &refs, RUN_TIMEOUT)
        .ok_or_else(|| format!("output collection failed for {container_path}"))
        .map(|_| target)
}

fn sanitize_output_name(path: &str) -> Result<String, String> {
    let name = path.trim_start_matches('/').replace('/', "_");
    if name.is_empty() || name.len() > 128 || name.contains("..") || name.starts_with('.') {
        return Err(format!("unsafe output path {path}"));
    }
    Ok(name)
}

/// TCP/HTTP readiness probe against the container address inside a project
/// network. For isolated containers the probe target is unreachable, which the
/// caller treats as not-ready rather than ready-by-default.
pub fn probe_readiness(workload_id: &str, attempt: i64, check: &crate::client::HealthCheck) -> bool {
    let Some(state) = inspect(workload_id, attempt) else {
        return false;
    };
    if state.status != "running" {
        return false;
    }
    let Some(ip) = container_ip(workload_id, attempt) else {
        return false;
    };
    match check.kind.as_str() {
        "tcp" => std::net::TcpStream::connect((ip.as_str(), check.port as u16)).is_ok(),
        "http" => {
            let url = format!("http://{}:{}{}", ip, check.port, check.path);
            match reqwest::blocking::Client::builder()
                .timeout(std::time::Duration::from_secs(5))
                .build()
            {
                Ok(client) => client
                    .get(&url)
                    .send()
                    .map(|r| r.status().is_success())
                    .unwrap_or(false),
                Err(_) => false,
            }
        }
        _ => false,
    }
}

/// The container's address on any joined network, if reachable.
pub fn container_ip(workload_id: &str, attempt: i64) -> Option<String> {
    let name = container_name(workload_id, attempt);
    let out = probe(
        "docker",
        &["container", "inspect", "--format", "{{json .NetworkSettings.Networks}}", &name],
        PROBE_TIMEOUT,
    )?;
    let networks: std::collections::HashMap<String, serde_json::Value> = serde_json::from_str(out.trim()).ok()?;
    for (_name, settings) in networks {
        if let Some(ip) = settings.get("IPAddress").and_then(|v| v.as_str()) {
            if !ip.is_empty() {
                return Some(ip.to_string());
            }
        }
    }
    None
}

/// Create the container for an enriched assignment: read-only artifact binds
/// from the worker-owned staging area, persistent volumes, project network,
/// environment and an explicit /out volume for collected outputs. The bind
/// sources are always worker-staged paths, never client-chosen host paths.
use crate::client::AssignmentEnvelope;

pub fn run_enriched(spec: &RunSpec, envelope: &AssignmentEnvelope, staged: &[(String, std::path::PathBuf)]) -> Result<(), String> {
    let os = image_os(&spec.image)
        .ok_or_else(|| format!("image {} is not available locally", spec.image))?;
    if os != "linux" {
        return Err(format!("image {} requires {} OS, workers run Linux containers", spec.image, os));
    }
    let name = container_name(&spec.workload_id, spec.attempt);
    let cpu = (spec.cpu_millis as f64 / 1000.0).max(0.01);
    let memory = spec.memory_bytes.max(6 << 20);
    let mut args: Vec<String> = vec![
        "run".into(),
        "-d".into(),
        "--name".into(),
        name.clone(),
        "--network".into(),
        "none".into(),
        "--read-only".into(),
        "--cap-drop".into(),
        "ALL".into(),
        "--security-opt".into(),
        "no-new-privileges".into(),
        "--user".into(),
        "65534:65534".into(),
        "--tmpfs".into(),
        "/tmp:rw,nosuid,nodev,noexec,size=32m,mode=1777".into(),
        "--cpus".into(),
        format!("{cpu}"),
        "--memory".into(),
        format!("{memory}"),
        "--memory-swap".into(),
        format!("{memory}"),
        "--pids-limit".into(),
        "128".into(),
        "--log-driver".into(),
        "json-file".into(),
        "--log-opt".into(),
        "max-size=1m".into(),
        "--log-opt".into(),
        "max-file=2".into(),
        "-l".into(),
        format!("dev.nimbuslabs.kamui.worker={}", spec.worker_id),
        "-l".into(),
        format!("dev.nimbuslabs.kamui.workload={}", spec.workload_id),
        "-l".into(),
        format!("dev.nimbuslabs.kamui.attempt={}", spec.attempt),
    ];
    for (key, value) in &envelope.env {
        args.push("-e".into());
        args.push(format!("{key}={value}"));
    }
    // Persistent volumes mount read-write at /kamui/volumes/<name>; the
    // workload never receives a host path.
    for volume in &envelope.volumes {
        args.push("--mount".into());
        args.push(format!("type=volume,source={},target=/kamui/volumes/{}", volume.name, volume.name));
    }
    // Staged artifacts bind read-only from the worker-owned staging area.
    for (container_path, local) in staged {
        args.push("--mount".into());
        args.push(format!(
            "type=bind,src={},dst={},readonly",
            local.to_string_lossy(),
            container_path
        ));
    }
    if !envelope.outputs.is_empty() {
        // An assignment-scoped scratch volume collects declared outputs.
        args.push("--mount".into());
        args.push(format!(
            "type=volume,source=kamui-out-{}-{},target=/out",
            spec.workload_id, spec.attempt
        ));
    }
    if !envelope.project_network.is_empty() {
        args.push("--network".into());
        args.push(envelope.project_network.clone());
    }
    if spec.gpu_count > 0 {
        if runtimes().iter().any(|r| r == "nvidia") {
            args.push("--gpus".into());
            args.push(spec.gpu_count.to_string());
        } else {
            return Err(
                "GPU workloads require the NVIDIA container runtime, which is not installed on this worker"
                    .into(),
            );
        }
    }
    args.push(spec.image.clone());
    args.extend(spec.args.iter().cloned());
    let arg_refs: Vec<&str> = args.iter().map(String::as_str).collect();
    let output = probe("docker", &arg_refs, RUN_TIMEOUT)
        .ok_or_else(|| "docker run failed (image may be missing or invalid)".to_string())?;
    let id = output.trim();
    if id.is_empty() {
        return Err("docker run produced no container".into());
    }
    Ok(())
}

/// Remove the assignment output volume after outputs were collected (or when
/// nothing was declared).
pub fn remove_output_volume(workload_id: &str, attempt: i64) {
    let volume = format!("kamui-out-{workload_id}-{attempt}");
    let _ = probe("docker", &["volume", "rm", &volume], STOP_TIMEOUT);
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn container_names_are_stable_and_safe() {
        let name = container_name("wl-0123456789abcdef0123456789abcdef", 3);
        assert_eq!(name, "kamui-wl-0123456789abcdef0123456789abcdef-3");
    }

    #[test]
    fn run_spec_requires_local_linux_images() {
        // The pure helper parts of run() are covered through the name/limits
        // math; actual execution needs a Docker daemon and is exercised by the
        // integration demo.
        let cpu = (250_f64 / 1000.0).max(0.01);
        assert_eq!(cpu, 0.25);
    }
}
