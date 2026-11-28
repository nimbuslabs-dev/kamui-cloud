//! Outbound HTTP/1.1 upgrade over the worker's verified coordinator connection.

use crate::{docker, state::WorkerState};
use reqwest::{Client, StatusCode, Url};
use std::collections::HashMap;
use std::net::IpAddr;
use std::path::Path;
use std::sync::{
    Arc,
    atomic::{AtomicBool, Ordering},
};
use std::time::Duration;
use tokio::{
    io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt},
    net::TcpStream,
    sync::mpsc,
    task::JoinHandle,
    time::timeout,
};

const OPEN: u8 = 1;
const DATA: u8 = 2;
const EOF: u8 = 3;
const CLOSE: u8 = 4;
const PING: u8 = 5;
const PONG: u8 = 6;
const LIMIT: usize = 1 << 20;
type Frame = (u8, u32, Vec<u8>);
pub type Assignment = Arc<dyn Fn(&str, &str, u16) -> Option<i64> + Send + Sync>;

struct Stream {
    input: mpsc::Sender<Option<Vec<u8>>>,
    task: JoinHandle<()>,
}
impl Drop for Stream {
    fn drop(&mut self) {
        self.task.abort();
    }
}

pub fn run(
    state: WorkerState,
    ca: Option<&Path>,
    running: Arc<AtomicBool>,
    assignment: Assignment,
) -> Result<(), String> {
    let mut url = Url::parse(&state.coordinator).map_err(|_| "invalid coordinator URL")?;
    if url.scheme() != "https"
        && !(url.scheme() == "http"
            && url.host_str().is_some_and(|h| {
                h.trim_matches(['[', ']'])
                    .parse::<IpAddr>()
                    .is_ok_and(|ip| ip.is_loopback())
            }))
    {
        return Err("worker service routing requires HTTPS or literal loopback HTTP".into());
    }
    if !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err("invalid coordinator authority".into());
    }
    url.path_segments_mut()
        .map_err(|_| "invalid coordinator URL")?
        .pop_if_empty()
        .extend(["v1", "workers", &state.worker_id, "tunnel"]);
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .map_err(|e| e.to_string())?;
    runtime.block_on(async move {
        let mut builder = Client::builder()
            .http1_only()
            .no_proxy()
            .redirect(reqwest::redirect::Policy::none())
            .connect_timeout(Duration::from_secs(10));
        if let Some(path) = ca {
            let pem = std::fs::read(path).map_err(|_| "cannot read coordinator CA")?;
            let cert =
                reqwest::Certificate::from_pem(&pem).map_err(|_| "invalid coordinator CA")?;
            builder = builder.add_root_certificate(cert);
        }
        let client = builder
            .build()
            .map_err(|_| "cannot initialize tunnel client")?;
        while running.load(Ordering::SeqCst) {
            let result = async {
                let response = timeout(
                    Duration::from_secs(15),
                    client
                        .get(url.clone())
                        .bearer_auth(&state.worker_token)
                        .header("Connection", "Upgrade")
                        .header("Upgrade", "kamui-tunnel")
                        .send(),
                )
                .await
                .map_err(|_| "tunnel handshake timed out")?
                .map_err(|_| "verified tunnel connection failed")?;
                if response.status() != StatusCode::SWITCHING_PROTOCOLS
                    || response
                        .headers()
                        .get("Upgrade")
                        .and_then(|v| v.to_str().ok())
                        != Some("kamui-tunnel")
                {
                    return Err("coordinator rejected the tunnel upgrade");
                }
                let connection = response
                    .upgrade()
                    .await
                    .map_err(|_| "tunnel upgrade failed")?;
                serve(connection, &assignment)
                    .await
                    .map_err(|_| "tunnel disconnected")
            }
            .await;
            if let Err(error) = result {
                eprintln!("{error}");
            }
            tokio::time::sleep(Duration::from_secs(3)).await;
        }
        Ok(())
    })
}

async fn serve(
    connection: impl AsyncRead + AsyncWrite + Unpin + Send + 'static,
    assignment: &Assignment,
) -> std::io::Result<()> {
    let (mut reader, mut writer) = tokio::io::split(connection);
    let (output, mut outgoing) = mpsc::channel::<Frame>(32);
    let write_task = tokio::spawn(async move {
        while let Some((kind, id, data)) = outgoing.recv().await {
            if !matches!(
                timeout(Duration::from_secs(10), async {
                    let mut header = [kind; 9];
                    header[1..5].copy_from_slice(&id.to_be_bytes());
                    header[5..9].copy_from_slice(&(data.len() as u32).to_be_bytes());
                    writer.write_all(&header).await?;
                    writer.write_all(&data).await
                })
                .await,
                Ok(Ok(()))
            ) {
                break;
            }
        }
    });
    let result = receive(&mut reader, &output, assignment).await;
    write_task.abort();
    result
}

async fn receive(
    reader: &mut (impl AsyncRead + Unpin),
    output: &mpsc::Sender<Frame>,
    assignment: &Assignment,
) -> std::io::Result<()> {
    let mut streams = HashMap::<u32, Stream>::new();
    loop {
        let (kind, id, data) = timeout(Duration::from_secs(45), async {
            let mut header = [0; 9];
            reader.read_exact(&mut header).await?;
            let id = u32::from_be_bytes(header[1..5].try_into().unwrap());
            let length = u32::from_be_bytes(header[5..9].try_into().unwrap()) as usize;
            if length > LIMIT {
                return Err(std::io::Error::other("oversized frame"));
            }
            let mut payload = vec![0; length];
            reader.read_exact(&mut payload).await?;
            Ok::<Frame, std::io::Error>((header[0], id, payload))
        })
        .await??;
        match kind {
            PING => send(output, (PONG, 0, vec![])).await?,
            OPEN => {
                streams.retain(|_, stream| !stream.task.is_finished());
                if streams.contains_key(&id) || streams.len() >= 64 {
                    send(output, (CLOSE, id, vec![])).await?;
                    continue;
                }
                if let Some(stream) = open_stream(output.clone(), id, data, assignment.clone()) {
                    streams.insert(id, stream);
                } else {
                    send(output, (CLOSE, id, vec![])).await?;
                }
            }
            DATA | EOF => {
                if let Some(stream) = streams.get(&id) {
                    if stream
                        .input
                        .try_send((kind == DATA).then_some(data))
                        .is_err()
                    {
                        streams.remove(&id);
                        send(output, (CLOSE, id, vec![])).await?;
                    }
                }
            }
            CLOSE => {
                streams.remove(&id);
            }
            _ => return Err(std::io::Error::other("unexpected frame")),
        }
    }
}

#[derive(serde::Deserialize)]
struct Open {
    workload_id: String,
    fence: String,
    port: u16,
}

fn open_stream(
    output: mpsc::Sender<Frame>,
    id: u32,
    payload: Vec<u8>,
    assignment: Assignment,
) -> Option<Stream> {
    let open: Open = serde_json::from_slice(&payload).ok()?;
    let attempt = assignment(&open.workload_id, &open.fence, open.port)?;
    let (input, mut incoming) = mpsc::channel::<Option<Vec<u8>>>(8);
    let task = tokio::spawn(async move {
        let result = async {
            let workload = open.workload_id.clone();
            let ip = tokio::task::spawn_blocking(move || docker::container_ip(&workload, attempt))
                .await
                .ok()
                .flatten()
                .and_then(|ip| ip.parse::<IpAddr>().ok())
                .ok_or_else(|| std::io::Error::other("missing container IP"))?;
            if assignment(&open.workload_id, &open.fence, open.port) != Some(attempt) {
                return Err(std::io::Error::other("assignment changed"));
            }
            let socket =
                timeout(Duration::from_secs(5), TcpStream::connect((ip, open.port))).await??;
            let (mut rx, mut tx) = socket.into_split();
            send(&output, (DATA, id, vec![])).await?;
            let request = async {
                while let Some(data) = incoming.recv().await {
                    if let Some(bytes) = data {
                        tx.write_all(&bytes).await?;
                    } else {
                        tx.shutdown().await?;
                        break;
                    }
                }
                Ok::<(), std::io::Error>(())
            };
            let response = async {
                let mut buf = [0; 16384];
                loop {
                    let n = rx.read(&mut buf).await?;
                    if n == 0 {
                        return send(&output, (EOF, id, vec![])).await;
                    }
                    send(&output, (DATA, id, buf[..n].to_vec())).await?;
                }
            };
            tokio::try_join!(request, response)?;
            Ok::<(), std::io::Error>(())
        }
        .await;
        if result.is_err() {
            let _ = send(&output, (CLOSE, id, vec![])).await;
        }
    });
    Some(Stream { input, task })
}

async fn send(output: &mpsc::Sender<Frame>, frame: Frame) -> std::io::Result<()> {
    timeout(Duration::from_secs(10), output.send(frame))
        .await?
        .map_err(|_| std::io::Error::other("tunnel writer disconnected"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn rejects_stale_assignment_and_keeps_transport_open() {
        let (agent, mut peer) = tokio::io::duplex(1024);
        let assignment: Assignment = Arc::new(|_, _, _| None);
        let task = tokio::spawn(async move { serve(agent, &assignment).await });
        let payload = br#"{"workload_id":"wl-1","fence":"stale","port":8080}"#;
        let mut header = [OPEN, 0, 0, 0, 1, 0, 0, 0, 0];
        header[5..].copy_from_slice(&(payload.len() as u32).to_be_bytes());
        peer.write_all(&header).await.unwrap();
        peer.write_all(payload).await.unwrap();
        timeout(Duration::from_secs(1), peer.read_exact(&mut header))
            .await
            .unwrap()
            .unwrap();
        assert_eq!(header, [CLOSE, 0, 0, 0, 1, 0, 0, 0, 0]);
        peer.write_all(&[PING, 0, 0, 0, 0, 0, 0, 0, 0])
            .await
            .unwrap();
        timeout(Duration::from_secs(1), peer.read_exact(&mut header))
            .await
            .unwrap()
            .unwrap();
        assert_eq!(header[0], PONG);
        drop(peer);
        assert!(task.await.unwrap().is_err());
    }
}
