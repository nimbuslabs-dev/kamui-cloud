use serde::Serialize;
use std::env;
use std::io::{Read, Write};
use std::path::Path;
use std::process::{Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};
use sysinfo::{CpuRefreshKind, Disks, MemoryRefreshKind, System};

#[derive(Serialize)]
struct Capabilities {
    hostname: Option<String>,
    os: &'static str,
    arch: String,
    logical_cpus: Option<usize>,
    memory_total_bytes: Option<u64>,
    memory_available_bytes: Option<u64>,
    storage_available_bytes: Option<u64>,
    gpus: Vec<Gpu>,
    docker_available: bool,
    runtimes: Vec<&'static str>,
    health: &'static str,
}

#[derive(Serialize)]
struct Gpu {
    vendor: String,
    model: Option<String>,
    available_memory_bytes: Option<u64>,
    capabilities: Vec<&'static str>,
}

pub fn report(path: &Path, output: impl Write) -> serde_json::Result<()> {
    let mut system = System::new();
    system.refresh_cpu_list(CpuRefreshKind::nothing());
    system.refresh_memory_specifics(MemoryRefreshKind::nothing().with_ram());
    let total = system.total_memory();
    let available = system.available_memory();
    let memory_sampled = (available > 0 || system.used_memory() > 0) && available <= total;
    let cpu_count = system.cpus().len();
    let docker_available = docker_available();
    serde_json::to_writer(
        output,
        &Capabilities {
            hostname: System::host_name(),
            os: env::consts::OS,
            arch: canonical_arch(&System::cpu_arch()).to_owned(),
            logical_cpus: (cpu_count > 0).then_some(cpu_count),
            memory_total_bytes: (total > 0).then_some(total),
            memory_available_bytes: (total > 0 && memory_sampled).then_some(available),
            storage_available_bytes: available_storage(path),
            gpus: gpus(),
            docker_available,
            runtimes: if docker_available {
                vec!["docker"]
            } else {
                vec![]
            },
            health: if docker_available {
                "healthy"
            } else {
                "degraded"
            },
        },
    )
}

fn canonical_arch(arch: &str) -> &str {
    match arch {
        "aarch64" | "ARM64" => "arm64",
        "x86_64" | "AMD64" | "x64" => "amd64",
        other => other,
    }
}

fn available_storage(path: &Path) -> Option<u64> {
    let path = path.canonicalize().ok()?;
    // Windows canonicalize adds a verbatim prefix that mount points do not use.
    #[cfg(windows)]
    let path = {
        let printable = path.to_str()?;
        std::path::PathBuf::from(printable.strip_prefix(r"\\?\").unwrap_or(printable))
    };
    Disks::new_with_refreshed_list()
        .iter()
        .filter(|disk| path.starts_with(disk.mount_point()) && disk.total_space() != 0)
        .max_by_key(|disk| disk.mount_point().components().count())
        .map(|disk| disk.available_space())
}

fn docker_available() -> bool {
    let endpoint = match env::var("DOCKER_HOST") {
        Ok(host) if env::var_os("DOCKER_CONTEXT").is_none_or(|context| context.is_empty()) => {
            Some(host)
        }
        _ => probe(
            "docker",
            &[
                "context",
                "inspect",
                "--format",
                "{{.Endpoints.docker.Host}}",
            ],
        ),
    };
    let Some(endpoint) = endpoint else {
        return false;
    };
    let endpoint = endpoint.trim();
    if !endpoint.starts_with("unix:///") && !endpoint.starts_with("npipe:////./pipe/") {
        return false;
    }
    // Pin the endpoint we just checked. A changed default context must not redirect the probe.
    probe(
        "docker",
        &[
            "--host",
            endpoint,
            "version",
            "--format",
            "{{.Server.Version}}",
        ],
    )
    .is_some_and(|version| !version.trim().is_empty())
}

fn probe(program: &str, args: &[&str]) -> Option<String> {
    let mut child = Command::new(program)
        .args(args)
        .env_remove("KAMUI_TOKEN")
        .envs(
            (program == "docker" && args.first() == Some(&"--host"))
                .then_some(("DOCKER_CONTEXT", "")),
        )
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
    let deadline = Instant::now() + Duration::from_secs(3);
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

#[cfg(any(target_os = "linux", windows))]
fn nvidia_gpus() -> Vec<Gpu> {
    probe(
        "nvidia-smi",
        &[
            "--query-gpu=name,memory.free,compute_cap",
            "--format=csv,noheader,nounits",
        ],
    )
    .map(|output| {
        output
            .lines()
            .filter_map(|line| {
                let (metrics, compute_cap) = line.rsplit_once(',')?;
                let (model, free) = metrics.rsplit_once(',')?;
                let supports_cuda =
                    compute_cap
                        .trim()
                        .split_once('.')
                        .is_some_and(|(major, minor)| {
                            major.parse::<u32>().is_ok_and(|value| value > 0)
                                && minor.parse::<u32>().is_ok()
                        });
                Some(Gpu {
                    vendor: "nvidia".into(),
                    model: Some(model.trim().to_owned()),
                    available_memory_bytes: free
                        .trim()
                        .parse::<u64>()
                        .ok()
                        .and_then(|mib| mib.checked_mul(1024 * 1024)),
                    capabilities: if supports_cuda { vec!["cuda"] } else { vec![] },
                })
            })
            .collect()
    })
    .unwrap_or_default()
}

#[cfg(any(target_os = "linux", windows))]
fn pci_vendor(id: &str) -> String {
    match id {
        "10de" => "nvidia".into(),
        "1002" => "amd".into(),
        "8086" => "intel".into(),
        "5143" => "qualcomm".into(),
        _ => format!("pci:{id}"),
    }
}

#[cfg(target_os = "linux")]
fn gpus() -> Vec<Gpu> {
    let mut gpus = nvidia_gpus();
    let nvidia_found = !gpus.is_empty();
    if let Ok(devices) = std::fs::read_dir("/sys/bus/pci/devices") {
        for entry in devices.flatten() {
            let path = entry.path();
            let read = |field| std::fs::read_to_string(path.join(field)).ok();
            if !read("class").is_some_and(|class| class.starts_with("0x03")) {
                continue;
            }
            let Some(vendor_id) = read("vendor") else {
                continue;
            };
            let vendor = pci_vendor(vendor_id.trim().trim_start_matches("0x"));
            if nvidia_found && vendor == "nvidia" {
                continue;
            }
            let free = || {
                let total = read("mem_info_vram_total")?.trim().parse::<u64>().ok()?;
                let used = read("mem_info_vram_used")?.trim().parse::<u64>().ok()?;
                total.checked_sub(used)
            };
            gpus.push(Gpu {
                vendor,
                model: read("device").map(|id| id.trim().to_owned()),
                available_memory_bytes: free(),
                capabilities: vec![],
            });
        }
    }
    gpus
}

#[cfg(windows)]
fn gpus() -> Vec<Gpu> {
    let mut gpus = nvidia_gpus();
    let nvidia_found = !gpus.is_empty();
    let Some(json) = probe(
        "powershell.exe",
        &[
            "-NoProfile",
            "-NonInteractive",
            "-Command",
            "[Console]::OutputEncoding = [Text.Encoding]::UTF8; ConvertTo-Json -Compress -InputObject @(Get-CimInstance Win32_VideoController | Select-Object Name,PNPDeviceID,AdapterCompatibility)",
        ],
    ) else {
        return gpus;
    };
    let Ok(entries) = serde_json::from_str::<Vec<serde_json::Value>>(&json) else {
        return gpus;
    };
    for entry in entries {
        let pci = entry["PNPDeviceID"]
            .as_str()
            .unwrap_or_default()
            .to_ascii_lowercase();
        let vendor = if let Some(id) = pci.split_once("ven_").and_then(|(_, tail)| tail.get(..4)) {
            pci_vendor(id)
        } else if let Some(name) = entry["AdapterCompatibility"].as_str() {
            name.trim().to_ascii_lowercase()
        } else {
            continue;
        };
        if !vendor.is_empty() && !(nvidia_found && vendor == "nvidia") {
            gpus.push(Gpu {
                vendor,
                model: entry["Name"].as_str().map(str::to_owned),
                available_memory_bytes: None,
                capabilities: vec![],
            });
        }
    }
    gpus
}

#[cfg(target_os = "macos")]
fn gpus() -> Vec<Gpu> {
    let Some(json) = probe(
        "/usr/sbin/system_profiler",
        &["-json", "SPDisplaysDataType"],
    ) else {
        return vec![];
    };
    let Ok(report) = serde_json::from_str::<serde_json::Value>(&json) else {
        return vec![];
    };
    let Some(entries) = report["SPDisplaysDataType"].as_array() else {
        return vec![];
    };
    entries
        .iter()
        .filter_map(|entry| {
            let vendor = entry["spdisplays_vendor"]
                .as_str()?
                .trim_start_matches("sppci_vendor_")
                .to_ascii_lowercase();
            let capabilities = if entry["spdisplays_metal"] == "spdisplays_supported"
                || entry["spdisplays_mtlgpufamilysupport"]
                    .as_str()
                    .is_some_and(|value| value.starts_with("spdisplays_metal"))
            {
                vec!["metal"]
            } else {
                vec![]
            };
            Some(Gpu {
                vendor,
                model: entry["_name"].as_str().map(str::to_owned),
                available_memory_bytes: None,
                capabilities,
            })
        })
        .collect()
}

#[cfg(not(any(target_os = "linux", target_os = "macos", windows)))]
fn gpus() -> Vec<Gpu> {
    vec![]
}
