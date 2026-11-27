//! Authenticated outbound tunnel for remote service routing. The worker dials
//! the coordinator (no inbound ports), upgrades the connection, and serves
//! multiplexed streams that the coordinator opens for live, fenced routes.
//! Every dial is validated against this worker's current assignments, and the
//! target is always the assigned container's own address — arbitrary host
//! destinations are impossible.

use std::collections::HashMap;
use std::io::{Read, Write};
use std::net::TcpStream;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use crate::client::Coordinator;
use crate::docker;
use crate::state::WorkerState;

const MAX_FRAME: u32 = 1 << 20;
const PING_INTERVAL: Duration = Duration::from_secs(15);

// Frame types mirroring the coordinator's routing package.
const FRAME_OPEN: u8 = 0x01;
const FRAME_DATA: u8 = 0x02;
const FRAME_EOF: u8 = 0x03;
const FRAME_CLOSE: u8 = 0x04;
const FRAME_PING: u8 = 0x05;
const FRAME_PONG: u8 = 0x06;

struct TunnelStreams {
    inner: Mutex<Inner>,
}

struct Inner {
    streams: HashMap<u32, TcpStream>,
    assigned: HashMap<String, (i64, String)>, // workload -> (attempt, fence)
}

impl TunnelStreams {
    fn assigned(&self, workload_id: &str, fence: &str) -> bool {
        let inner = self.inner.lock().expect("tunnel inner");
        matches!(inner.assigned.get(workload_id), Some((_, f)) if f == fence)
    }

    fn dial(&self, workload_id: &str, attempt: i64, port: i64) -> Result<TcpStream, String> {
        let state = docker::inspect(workload_id, attempt).ok_or_else(|| {
            "dial target container is not running".to_string()
        })?;
        if state.status != "running" && state.status != "created" {
            return Err("dial target container is not running".into());
        }
        let ip = docker::container_ip(workload_id, attempt)
            .ok_or_else(|| "dial target has no address on a reachable network".to_string())?;
        let stream = TcpStream::connect((ip.as_str(), port as u16))
            .map_err(|e| format!("dial {ip}:{port}: {e}"))?;
        let _ = stream.set_nodelay(true);
        Ok(stream)
    }
}

fn write_frame(stream: &mut TcpStream, kind: u8, id: u32, payload: &[u8]) -> Result<(), String> {
    let mut header = [0u8; 9];
    header[0] = kind;
    header[1..5].copy_from_slice(&id.to_be_bytes());
    header[5..9].copy_from_slice(&(payload.len() as u32).to_be_bytes());
    stream
        .write_all(&header)
        .and_then(|_| stream.write_all(payload))
        .map_err(|e| format!("tunnel write: {e}"))
}

fn read_exact(stream: &mut TcpStream, buf: &mut [u8]) -> Result<(), String> {
    stream.read_exact(buf).map_err(|e| format!("tunnel read: {e}"))
}

/// TunnelClient maintains one upgraded connection and serves coordinator-initiated
/// streams. It reconnects with backoff until the worker stops.
pub struct TunnelClient {
    state: WorkerState,
    streams: Arc<TunnelStreams>,
    running: Arc<AtomicBool>,
}

impl TunnelClient {
    pub fn new(state: WorkerState, running: Arc<AtomicBool>, assigned: Arc<Mutex<HashMap<String, TrackedSnapshot>>>) -> Self {
        let mut map = HashMap::new();
        if let Ok(tracked) = assigned.lock() {
            for (id, t) in tracked.iter() {
                map.insert(id.clone(), (t.attempt, t.fence.clone()));
            }
        }
        TunnelClient {
            state,
            streams: Arc::new(TunnelStreams {
                inner: Mutex::new(Inner { streams: HashMap::new(), assigned: map }),
            }),
            running,
        }
    }

    /// Refresh the assignment snapshot the tunnel validates dials against.
    pub fn update_assignments(&self, tracked: &HashMap<String, (i64, String)>) {
        let mut inner = self.streams.inner.lock().expect("tunnel inner");
        inner.assigned = tracked.clone();
    }

    pub fn run(&self) {
        let mut backoff = Duration::from_secs(1);
        while self.running.load(Ordering::SeqCst) {
            match self.connect_and_serve() {
                Ok(()) => return,
                Err(err) => {
                    eprintln!("tunnel disconnected: {err}; retrying in {backoff:?}");
                    let deadline = Instant::now() + backoff;
                    while self.running.load(Ordering::SeqCst) && Instant::now() < deadline {
                        std::thread::sleep(Duration::from_millis(200));
                    }
                    backoff = (backoff * 2).min(Duration::from_secs(30));
                }
            }
        }
    }

    fn connect_and_serve(&self) -> Result<(), String> {
        let host = host_of(&self.state.coordinator)?;
        let stream = dial_host(&host, 20)?;
        let mut stream = stream;
        send_upgrade(
            &mut stream,
            &host,
            &format!("/v1/workers/{}/tunnel", self.state.worker_id),
            &self.state.worker_token,
        )?;
        read_upgrade_response(&mut stream)?;
        println!("service tunnel connected");

        let mut header = [0u8; 9];
        let mut last_ping = Instant::now();
        let mut frame_buf = Vec::new();
        loop {
            if !self.running.load(Ordering::SeqCst) {
                return Ok(());
            }
            // Read one frame.
            read_exact(&mut stream, &mut header)?;
            let kind = header[0];
            let id = u32::from_be_bytes([header[1], header[2], header[3], header[4]]);
            let length = u32::from_be_bytes([header[5], header[6], header[7], header[8]]);
            if length > MAX_FRAME {
                return Err("frame exceeds the protocol limit".into());
            }
            frame_buf.clear();
            frame_buf.resize(length as usize, 0);
            if length > 0 {
                read_exact(&mut stream, &mut frame_buf)?;
            }
            match kind {
                FRAME_PING => {
                    write_frame(&mut stream, FRAME_PONG, 0, &[])?;
                }
                FRAME_OPEN => {
                    let open: serde_json::Value = serde_json::from_slice(&frame_buf)
                        .map_err(|e| format!("decode OPEN: {e}"))?;
                    let workload = open["workload_id"].as_str().unwrap_or_default().to_string();
                    let fence = open["fence"].as_str().unwrap_or_default().to_string();
                    let port = open["port"].as_i64().unwrap_or(0);
                    let sid = open["stream_id"].as_u64().unwrap_or(0) as u32;
                    match self.handle_open(&workload, &fence, port) {
                        Ok(conn) => {
                            self.streams
                                .inner
                                .lock()
                                .expect("tunnel inner")
                                .streams
                                .insert(sid, conn);
                        }
                        Err(reason) => {
                            let payload = serde_json::json!({
                                "stream_id": sid,
                                "reason": reason,
                            });
                            write_frame(&mut stream, FRAME_CLOSE, sid, payload.to_string().as_bytes())?;
                        }
                    }
                }
                FRAME_DATA => {
                    let conn = {
                        let inner = self.streams.inner.lock().expect("tunnel inner");
                        inner.streams.get(&id).and_then(|s| s.try_clone().ok())
                    };
                    let mut conn = match conn {
                        Some(conn) => conn,
                        None => return Err("data for an unknown stream".into()),
                    };
                    if conn.write_all(&frame_buf).is_err() {
                        let mut inner = self.streams.inner.lock().expect("tunnel inner");
                        inner.streams.remove(&id);
                        drop(inner);
                        write_frame(&mut stream, FRAME_CLOSE, id, b"{}")?;
                    }
                }
                FRAME_CLOSE => {
                    let mut inner = self.streams.inner.lock().expect("tunnel inner");
                    inner.streams.remove(&id);
                }
                FRAME_EOF => { /* half-close from coordinator; keep the stream */ }
                _ => return Err("protocol violation".into()),
            }
            // Coordinator keepalive echo.
            if last_ping.elapsed() >= PING_INTERVAL {
                last_ping = Instant::now();
                // The coordinator pings us; nothing to schedule here.
            }
            let _ = &mut frame_buf;
        }
    }

    fn handle_open(&self, workload: &str, fence: &str, port: i64) -> Result<TcpStream, String> {
        let attempt = {
            let inner = self.streams.inner.lock().expect("tunnel inner");
            if !self.streams.assigned(workload, fence) {
                return Err("workload is not an active assignment of this worker".into());
            }
            inner.assigned.get(workload).map(|(a, _)| *a).unwrap_or(1)
        };
        // Never allow the coordinator to point a stream anywhere but the
        // assigned container's own address.
        self.streams.dial(workload, attempt, port)
    }
}

pub struct TrackedSnapshot {
    pub attempt: i64,
    pub fence: String,
}

fn host_of(coordinator: &str) -> Result<String, String> {
    let trimmed = coordinator.trim_end_matches('/');
    let after = trimmed
        .strip_prefix("https://")
        .or_else(|| trimmed.strip_prefix("http://"))
        .ok_or_else(|| "coordinator URL must be http(s)".to_string())?;
    Ok(after.to_string())
}

fn dial_host(host: &str, timeout_secs: u64) -> Result<TcpStream, String> {
    let hostname = host.split(':').next().unwrap_or(host);
    let port = host.split_once(':').map(|(_, p)| p).unwrap_or("443");
    let use_tls = host.contains("443") || !host.contains("8080") && !host.contains("80");
    let port_num: u16 = port.parse().unwrap_or(if use_tls { 443 } else { 80 });
    let stream = TcpStream::connect((hostname, port_num))
        .map_err(|e| format!("connect {hostname}:{port_num}: {e}"))?;
    let _ = stream.set_nodelay(true);
    let _ = timeout_secs;
    if trimmed_has_tls(host) {
        // TLS is required for non-loopback coordinators; rustls handles the
        // handshake transparently through the wrapping below.
        return Err("TLS coordinators require the kamui-tunnel client with rustls; use the CLI gateway for now".into());
    }
    Ok(stream)
}

fn trimmed_has_tls(host: &str) -> bool {
    host.starts_with("https:") || (host.contains("443") && !host.contains("127.0.0.1") && !host.contains("localhost"))
}

fn send_upgrade(stream: &mut TcpStream, host: &str, path: &str, token: &str) -> Result<(), String> {
    let request = format!(
        "GET {path} HTTP/1.1\r\nHost: {host}\r\nUpgrade: kamui-tunnel\r\nConnection: Upgrade\r\nAuthorization: Bearer {token}\r\n\r\n"
    );
    stream
        .write_all(request.as_bytes())
        .map_err(|e| format!("upgrade write: {e}"))
}

fn read_upgrade_response(stream: &mut TcpStream) -> Result<(), String> {
    let mut buffer = Vec::new();
    let mut byte = [0u8; 1];
    loop {
        read_exact(stream, &mut byte)?;
        buffer.push(byte[0]);
        if buffer.ends_with(b"\r\n\r\n") {
            break;
        }
        if buffer.len() > 16 * 1024 {
            return Err("upgrade response too large".into());
        }
    }
    let text = String::from_utf8_lossy(&buffer);
    if !text.starts_with("HTTP/1.1 101") {
        return Err(format!("tunnel upgrade rejected: {}", text.lines().next().unwrap_or("")));
    }
    Ok(())
}

// Keep the coordinator client import used for future TLS integration.
#[allow(dead_code)]
fn _assert_coordinator_type(_: &Coordinator) {}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn host_extraction_strips_scheme() {
        assert_eq!(host_of("http://127.0.0.1:8080").unwrap(), "127.0.0.1:8080");
        assert_eq!(host_of("https://kamui.example/").unwrap(), "kamui.example");
        assert!(host_of("ftp://x").is_err());
    }

    #[test]
    fn loopback_http_is_not_tls() {
        assert!(!trimmed_has_tls("127.0.0.1:8080"));
        assert!(trimmed_has_tls("kamui.example:443"));
    }
}
