use serde_json::Value;
use std::process::Command;

#[test]
fn capabilities_is_json_without_a_coordinator_token() {
    let output = Command::new(env!("CARGO_BIN_EXE_kamui-agent"))
        .arg("capabilities")
        .env_remove("KAMUI_TOKEN")
        .env_remove("DOCKER_CONTEXT")
        .env("DOCKER_HOST", "tcp://192.0.2.1:2375")
        .output()
        .unwrap();

    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let report: Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(report["os"], std::env::consts::OS);
    assert!(report["logical_cpus"].as_u64().unwrap() > 0);
    assert!(report["memory_total_bytes"].as_u64().unwrap() > 0);
    assert!(report["storage_available_bytes"].is_u64());
    assert!(report["gpus"].is_array());
    assert_eq!(report["docker_available"], false);
    assert_eq!(report["health"], "degraded");
}
