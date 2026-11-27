//! One-shot enrollment: exchange a coordinator-issued single-use token for a
//! persistent worker identity stored under the agent state directory.

use crate::client::Coordinator;
use crate::state::{self, WorkerState};
use std::path::PathBuf;

pub fn run(args: Vec<String>) -> Result<(), String> {
    let mut url = String::new();
    let mut token = String::new();
    let mut name = std::env::var("KAMUI_AGENT_NAME").unwrap_or_default();
    let mut ca_file = None;
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--url" => {
                i += 1;
                url = args.get(i).ok_or("--url requires a value")?.clone();
            }
            "--token" => {
                i += 1;
                token = args.get(i).ok_or("--token requires a value")?.clone();
            }
            "--name" => {
                i += 1;
                name = args.get(i).ok_or("--name requires a value")?.clone();
            }
            "--ca-file" => {
                i += 1;
                ca_file = Some(PathBuf::from(
                    args.get(i).ok_or("--ca-file requires a path")?.clone(),
                ));
            }
            other => return Err(format!("unknown enroll argument {other}")),
        }
        i += 1;
    }
    if url.is_empty() || token.is_empty() {
        return Err("usage: kamui-agent enroll --url URL --token TOKEN [--name NAME] [--ca-file PATH]".into());
    }
    let hostname = std::env::var("KAMUI_AGENT_HOSTNAME").unwrap_or_default();
    let response = Coordinator::enroll(&url, &token, &name, &hostname, ca_file.as_deref())?;
    let display_name = if name.is_empty() { hostname_for_display() } else { name };
    state::save(&WorkerState {
        coordinator: url.trim_end_matches('/').to_string(),
        worker_id: response.worker_id.clone(),
        worker_token: response.worker_token,
        name: display_name.clone(),
    })?;
    println!("enrolled as {} ({})", response.worker_id, display_name);
    if response.approved {
        println!("worker is approved and may receive assignments");
        println!("start it with: kamui-agent worker");
    } else {
        println!("worker is pending owner approval; assignments are withheld until approval");
    }
    Ok(())
}

fn hostname_for_display() -> String {
    #[cfg(target_os = "macos")]
    {
        if let Ok(output) = std::process::Command::new("hostname").output() {
            return String::from_utf8_lossy(&output.stdout).trim().to_string();
        }
    }
    sysinfo::System::host_name().unwrap_or_else(|| "unnamed-worker".to_string())
}
