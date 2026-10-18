use serde::{Deserialize, Serialize};
use std::env;
use std::net::UdpSocket;
use std::sync::{
    Arc,
    atomic::{AtomicBool, Ordering},
};
use std::thread;
use std::time::Duration;
use sysinfo::System;

#[derive(Serialize)]
struct NodeInfo {
    hostname: String,
    os: String,
    arch: String,
    cpu_cores: usize,
    ram_mb: u64,
    ip: String,
    roles: Vec<String>,
}

#[derive(Deserialize, Debug)]
struct RegistrationResponse {
    id: String,
    heartbeat_interval_s: u64,
    offline_after_s: u64,
}

#[derive(Serialize)]
struct Heartbeat {
    cpu_pct: f32,
    mem_pct: f32,
}

fn local_ip() -> Result<String, Box<dyn std::error::Error>> {
    let socket = UdpSocket::bind("0.0.0.0:0")?;
    socket.connect("192.0.2.1:80")?;
    Ok(socket.local_addr()?.ip().to_string())
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let api_url = env::var("KAMUI_API_URL").unwrap_or_else(|_| "http://localhost:8080".to_string());

    let token =
        env::var("KAMUI_TOKEN").map_err(|_| "KAMUI_TOKEN environment variable is required")?;

    let mut system = System::new_all();
    system.refresh_all();

    let hostname = System::host_name().unwrap_or_else(|| "unknown-mac".to_string());

    let os = match env::consts::OS {
        "macos" => "macos",
        "windows" => "windows",
        _ => "linux",
    }
    .to_string();

    let arch = match env::consts::ARCH {
        "aarch64" => "arm64",
        "x86_64" => "amd64",
        other => other,
    }
    .to_string();

    let info = NodeInfo {
        hostname,
        os,
        arch,
        cpu_cores: system.cpus().len().max(1),
        ram_mb: (system.total_memory() / 1024 / 1024).max(1),
        ip: local_ip()?,
        roles: vec![],
    };

    let client = reqwest::blocking::Client::new();
    let base_url = api_url.trim_end_matches('/');

    println!("Registering node: {}", info.hostname);

    let response = client
        .post(format!("{base_url}/v1/nodes/register"))
        .bearer_auth(&token)
        .json(&info)
        .send()?;

    if !response.status().is_success() {
        let status = response.status();
        let body = response.text().unwrap_or_default();
        return Err(format!("Registration failed: {status} — {body}").into());
    }

    let registration: RegistrationResponse = response.json()?;

    println!("Node registered successfully!");
    println!("Node ID: {}", registration.id);
    println!(
        "Heartbeat interval: {} seconds",
        registration.heartbeat_interval_s
    );
    println!("Offline timeout: {} seconds", registration.offline_after_s);

    let running = Arc::new(AtomicBool::new(true));
    let running_for_handler = Arc::clone(&running);

    ctrlc::set_handler(move || {
        running_for_handler.store(false, Ordering::SeqCst);
        println!("\nStopping agent...");
    })?;

    let interval = registration.heartbeat_interval_s.max(1);
    let node_id = registration.id;

    while running.load(Ordering::SeqCst) {
        system.refresh_cpu_usage();
        system.refresh_memory();

        let cpu_pct = system.global_cpu_usage();
        let total_memory = system.total_memory();
        let used_memory = system.used_memory();

        let mem_pct = if total_memory > 0 {
            (used_memory as f32 / total_memory as f32) * 100.0
        } else {
            0.0
        };

        let heartbeat = Heartbeat {
            cpu_pct: cpu_pct.clamp(0.0, 100.0),
            mem_pct: mem_pct.clamp(0.0, 100.0),
        };

        let result = client
            .post(format!("{base_url}/v1/nodes/{node_id}/heartbeat"))
            .bearer_auth(&token)
            .json(&heartbeat)
            .send();

        match result {
            Ok(response) if response.status().is_success() => {
                println!(
                    "Heartbeat sent successfully — CPU: {:.1}%, Memory: {:.1}%",
                    heartbeat.cpu_pct, heartbeat.mem_pct
                );
            }
            Ok(response) => {
                eprintln!("Heartbeat failed: HTTP {}", response.status());
            }
            Err(error) => {
                eprintln!("Heartbeat request failed: {error}");
            }
        }

        for _ in 0..interval {
            if !running.load(Ordering::SeqCst) {
                break;
            }
            thread::sleep(Duration::from_secs(1));
        }
    }

    println!("Agent stopped.");
    Ok(())
}
