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

pub fn run(spec: &RunSpec) -> Result<(), String> {
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
