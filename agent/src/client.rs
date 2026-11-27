use serde::{Deserialize, Serialize};
use std::path::Path;
use std::time::{Duration, Instant};

#[derive(Deserialize)]
pub struct EnrollResponse {
    pub worker_id: String,
    pub worker_token: String,
    pub approved: bool,
}

#[derive(Serialize)]
pub struct Telemetry {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cpu_pct: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub on_battery: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub temperature_c: Option<f64>,
}

#[derive(Serialize)]
pub struct Heartbeat<'a> {
    pub capabilities: &'a serde_json::Value,
    pub telemetry: Telemetry,
    pub rtt_ms: f64,
}

#[derive(Deserialize)]
pub struct PollResponse {
    #[serde(default)]
    pub assignments: Vec<PollAssignment>,
    #[serde(default)]
    pub stop: Vec<PollStop>,
    #[serde(default)]
    pub active: Vec<PollActive>,
    #[serde(default)]
    pub max_concurrent: u32,
}

#[derive(Deserialize)]
pub struct PollAssignment {
    pub workload_id: String,
    pub attempt: i64,
    pub fence: String,
    pub image: String,
    #[serde(default)]
    pub args: Vec<String>,
    pub resources: Resources,
    #[serde(default)]
    pub envelope: Option<AssignmentEnvelope>,
}

/// Coordinator-resolved, assignment-scoped execution payload. Problems abort
/// the assignment before any container is created.
#[derive(Deserialize, Default, Clone)]
pub struct AssignmentEnvelope {
    /// The profile recorded on the submission; workers act on project_network.
    #[allow(dead_code)]
    #[serde(default)]
    pub network: String,
    #[serde(default)]
    pub image: String,
    #[serde(default)]
    pub image_digest: String,
    #[serde(default)]
    pub args: Vec<String>,
    #[serde(default)]
    pub env: std::collections::HashMap<String, String>,
    #[serde(default)]
    pub project_network: String,
    #[serde(default)]
    pub volumes: Vec<VolumeRequest>,
    #[serde(default)]
    pub artifacts: Vec<ArtifactMount>,
    #[serde(default)]
    pub outputs: Vec<String>,
    #[serde(default)]
    pub health_check: Option<HealthCheck>,
    #[serde(default)]
    pub registry_auth: Option<RegistryAuth>,
    #[serde(default)]
    pub problems: Vec<String>,
}

#[derive(Deserialize, Clone)]
pub struct VolumeRequest {
    pub name: String,
    /// Advisory on the local volume driver; coordinator quotas enforce size.
    #[allow(dead_code)]
    #[serde(default)]
    pub size_bytes: i64,
}

#[derive(Deserialize, Clone)]
pub struct ArtifactMount {
    pub artifact_id: String,
    pub path: String,
}

#[derive(Deserialize, Clone)]
pub struct HealthCheck {
    pub kind: String,
    pub port: i64,
    #[serde(default)]
    pub path: String,
    #[serde(default)]
    pub interval_seconds: i64,
}

#[derive(Deserialize, Clone)]
pub struct RegistryAuth {
    pub host: String,
    pub username: String,
    pub password: String,
}

#[derive(Deserialize)]
pub struct Resources {
    #[serde(default)]
    pub cpu_millis: i64,
    #[serde(default)]
    pub memory_bytes: i64,
    #[serde(default)]
    pub gpu_count: i64,
    /// Requested GPU capability set (eligibility is decided by the
    /// coordinator; the worker only honors gpu_count for passthrough).
    #[allow(dead_code)]
    #[serde(default)]
    pub gpu_capabilities: Vec<String>,
}

#[derive(Deserialize)]
pub struct PollStop {
    pub workload_id: String,
    pub attempt: i64,
    pub fence: String,
}

#[derive(Deserialize)]
pub struct PollActive {
    pub workload_id: String,
    pub attempt: i64,
    /// The fence comes back with each active assignment for reconciliation
    /// endpoints that may act on it; kept for protocol completeness.
    #[allow(dead_code)]
    pub fence: String,
}

/// Report body sent to the coordinator. `phase` selects ack|running|exit|renew.
#[derive(Serialize, Default, Clone)]
pub struct ReportBody {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub exit_code: Option<i32>,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub started: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub started_at: Option<String>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub log_tail: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub port: Option<i64>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub route_kind: String,
}

pub struct Coordinator {
    base: String,
    worker_token: String,
    http: reqwest::blocking::Client,
}

const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);

impl Coordinator {
    pub fn new(base: &str, worker_token: &str, ca_file: Option<&Path>) -> Result<Self, String> {
        let base = base.trim_end_matches('/').to_string();
        let mut builder = reqwest::blocking::Client::builder()
            .timeout(REQUEST_TIMEOUT)
            .connect_timeout(Duration::from_secs(10));
        if let Some(path) = ca_file {
            let pem = std::fs::read(path).map_err(|e| format!("read CA {}: {e}", path.display()))?;
            let cert = reqwest::Certificate::from_pem(&pem)
                .map_err(|e| format!("parse CA {}: {e}", path.display()))?;
            builder = builder.add_root_certificate(cert);
        }
        Ok(Coordinator {
            base,
            worker_token: worker_token.to_string(),
            http: builder.build().map_err(|e| format!("build HTTP client: {e}"))?,
        })
    }

    /// Enroll exchanges a single-use coordinator token for worker credentials.
    pub fn enroll(
        base: &str,
        token: &str,
        name: &str,
        hostname: &str,
        ca_file: Option<&Path>,
    ) -> Result<EnrollResponse, String> {
        let url = format!("{}/v1/workers/enroll", base.trim_end_matches('/'));
        let mut builder = reqwest::blocking::Client::builder().timeout(REQUEST_TIMEOUT);
        if let Some(path) = ca_file {
            let pem = std::fs::read(path).map_err(|e| format!("read CA {}: {e}", path.display()))?;
            let cert = reqwest::Certificate::from_pem(&pem)
                .map_err(|e| format!("parse CA {}: {e}", path.display()))?;
            builder = builder.add_root_certificate(cert);
        }
        let http = builder.build().map_err(|e| format!("build HTTP client: {e}"))?;
        let payload = serde_json::json!({ "token": token, "name": name, "hostname": hostname });
        let response = http
            .post(&url)
            .json(&payload)
            .send()
            .map_err(|e| format!("POST {url}: {e}"))?;
        if !response.status().is_success() {
            let status = response.status();
            let body = response.text().unwrap_or_default();
            return Err(format!("enrollment failed: {status} — {}", summarize(&body)));
        }
        response.json().map_err(|e| format!("decode enrollment: {e}"))
    }

    /// Fetch an assignment-scoped artifact as a stream.
    pub fn download_artifact(&self, worker_id: &str, artifact_id: &str, sink: &mut dyn FnMut(&[u8]) -> Result<(), String>) -> Result<u64, String> {
        let url = format!("{}/v1/workers/{worker_id}/artifacts/{artifact_id}/content", self.base);
        let response = self
            .http
            .get(&url)
            .bearer_auth(&self.worker_token)
            .header("X-Kamui-Worker", worker_id)
            .send()
            .map_err(|e| format!("GET {url}: {e}"))?;
        let status = response.status();
        if !status.is_success() {
            let text = response.text().unwrap_or_default();
            return Err(format!("artifact fetch failed: {status} — {}", summarize(&text)));
        }
        use std::io::Read;
        let mut reader = response;
        let mut total = 0u64;
        let mut buffer = [0u8; 64 * 1024];
        loop {
            let n = reader.read(&mut buffer).map_err(|e| format!("artifact stream: {e}"))?;
            if n == 0 {
                break;
            }
            sink(&buffer[..n])?;
            total += n as u64;
        }
        Ok(total)
    }

    fn post(&self, path: &str, payload: &impl Serialize) -> Result<reqwest::blocking::Response, String> {
        let url = format!("{}{}", self.base, path);
        let response = self
            .http
            .post(&url)
            .bearer_auth(&self.worker_token)
            .json(payload)
            .send()
            .map_err(|e| format!("POST {url}: {e}"))?;
        Ok(response)
    }

    /// Heartbeat returns the measured client-side round trip in milliseconds.
    pub fn heartbeat(&self, worker_id: &str, beat: &Heartbeat) -> Result<f64, String> {
        let path = format!("/v1/workers/{worker_id}/heartbeat");
        let started = Instant::now();
        let response = self.post(&path, &beat)?;
        let rtt = started.elapsed().as_secs_f64() * 1000.0;
        let status = response.status();
        let body = response.text().unwrap_or_default();
        if !status.is_success() {
            return Err(format!("heartbeat failed: {status} — {}", summarize(&body)));
        }
        Ok(rtt)
    }

    pub fn poll(&self, worker_id: &str) -> Result<PollResponse, String> {
        let path = format!("/v1/workers/{worker_id}/poll");
        let response = self.post(&path, &serde_json::json!({}))?;
        let status = response.status();
        let body = response.text().unwrap_or_default();
        if !status.is_success() {
            return Err(format!("poll failed: {status} — {}", summarize(&body)));
        }
        serde_json::from_str(&body).map_err(|e| format!("decode poll response: {e}"))
    }

    pub fn report(
        &self,
        worker_id: &str,
        workload_id: &str,
        fence: &str,
        phase: &str,
        body: &ReportBody,
    ) -> Result<(), String> {
        if phase == "route" {
            let path = format!("/v1/workers/{worker_id}/assignments/{workload_id}/report");
            let mut payload = serde_json::json!({ "fence": fence, "phase": "route", "port": body.port.unwrap_or(0) });
            if !body.route_kind.is_empty() {
                payload["route_kind"] = serde_json::json!(body.route_kind);
            }
            let response = self.post(&path, &payload)?;
            let status = response.status();
            let text = response.text().unwrap_or_default();
            if !status.is_success() {
                return Err(format!("route registration failed: {status} — {}", summarize(&text)));
            }
            return Ok(());
        }
        let path = format!("/v1/workers/{worker_id}/assignments/{workload_id}/report");
        let payload = serde_json::json!({ "fence": fence, "phase": phase });
        let mut payload = payload;
        if let Some(code) = body.exit_code {
            payload["exit_code"] = serde_json::json!(code);
        }
        if body.started {
            payload["started"] = serde_json::json!(true);
        }
        if let Some(started_at) = &body.started_at {
            payload["started_at"] = serde_json::json!(started_at);
        }
        if !body.log_tail.is_empty() {
            payload["log_tail"] = serde_json::json!(body.log_tail);
        }
        let response = self.post(&path, &payload)?;
        let status = response.status();
        let text = response.text().unwrap_or_default();
        if !status.is_success() {
            return Err(format!("report {phase} failed: {status} — {}", summarize(&text)));
        }
        Ok(())
    }
}

fn summarize(body: &str) -> String {
    let trimmed = body.trim();
    if trimmed.len() > 200 {
        format!("{}…", &trimmed[..200])
    } else {
        trimmed.to_string()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn report_body_serializes_without_unknown_fields() {
        let body = ReportBody {
            exit_code: Some(0),
            started: true,
            started_at: Some("2026-01-01T00:00:00Z".into()),
            log_tail: "hello".into(),
            port: Some(8080),
            route_kind: "http".into(),
        };
        let value = serde_json::to_value(&body).unwrap();
        assert_eq!(value["exit_code"], 0);
        assert_eq!(value["started"], true);
        assert_eq!(value["log_tail"], "hello");
        let empty = serde_json::to_value(ReportBody::default()).unwrap();
        assert_eq!(empty, serde_json::json!({}));
    }

    #[test]
    fn poll_response_defaults() {
        let response: PollResponse = serde_json::from_str("{}").unwrap();
        assert!(response.assignments.is_empty());
        assert_eq!(response.max_concurrent, 0);
    }
}
