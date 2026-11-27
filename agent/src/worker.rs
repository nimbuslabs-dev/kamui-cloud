//! The distributed worker loop: enrollment-gated polling, local Docker
//! execution, lease renewal, cancellation, reconciliation and reconnects.

use crate::capabilities;
use crate::client::{Coordinator, Heartbeat, ReportBody, Telemetry};
use crate::docker::{self, RunSpec};
use crate::state::{self, WorkerState};
use crate::telemetry::Sampler;
use serde_json::json;
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

const POLL_INTERVAL: Duration = Duration::from_secs(2);
const MAX_BACKOFF: Duration = Duration::from_secs(30);
const HEARTBEAT_INTERVAL: Duration = Duration::from_secs(15);
const RENEW_INTERVAL: Duration = Duration::from_secs(10);
const MAX_PENDING_REPORTS: usize = 128;

struct Tracked {
    attempt: i64,
    fence: String,
    running_reported: bool,
    last_renew: Instant,
}

#[derive(Clone)]
struct QueuedReport {
    workload_id: String,
    fence: String,
    phase: &'static str,
    body: ReportBody,
}

pub struct WorkerLoop {
    state: WorkerState,
    coordinator: Coordinator,
    tracked: Arc<Mutex<HashMap<String, Tracked>>>,
    pending: Arc<Mutex<Vec<QueuedReport>>>,
    running: Arc<AtomicBool>,
    storage_path: PathBuf,
    max_concurrent: u32,
}

pub fn run(args: Vec<String>) -> Result<(), String> {
    let mut ca_file = None;
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--ca-file" => {
                i += 1;
                ca_file = Some(PathBuf::from(
                    args.get(i).ok_or("--ca-file requires a path")?.clone(),
                ));
            }
            other => return Err(format!("unknown worker argument {other}")),
        }
        i += 1;
    }
    let Some(state) = state::load() else {
        return Err(
            "no enrolled worker credentials; run `kamui-agent enroll --url … --token …` first"
                .to_string(),
        );
    };
    let coordinator = Coordinator::new(&state.coordinator, &state.worker_token, ca_file.as_deref())?;
    println!("worker {} started; coordinator {}", state.worker_id, state.coordinator);
    WorkerLoop::new(state, coordinator)?.run()
}

impl WorkerLoop {
    fn new(state: WorkerState, coordinator: Coordinator) -> Result<Self, String> {
        let storage_path = state::home();
        Ok(WorkerLoop {
            state,
            coordinator,
            tracked: Arc::new(Mutex::new(HashMap::new())),
            pending: Arc::new(Mutex::new(Vec::new())),
            running: Arc::new(AtomicBool::new(true)),
            storage_path,
            max_concurrent: 0,
        })
    }

    fn run(mut self) -> Result<(), String> {
        let running = Arc::clone(&self.running);
        let _ = ctrlc::set_handler(move || {
            running.store(false, Ordering::SeqCst);
            println!("\nstopping worker…");
        });

        let heartbeat_running = Arc::clone(&self.running);
        let heartbeat_worker = self.clone_for_heartbeat();
        thread::spawn(move || heartbeat_worker.heartbeat_loop(heartbeat_running));

        let mut backoff = POLL_INTERVAL;
        while self.running.load(Ordering::SeqCst) {
            let cycle = Instant::now();
            match self.tick() {
                Ok(()) => backoff = POLL_INTERVAL,
                Err(err) => {
                    eprintln!("worker cycle failed: {err}; retrying in {backoff:?}");
                    thread::sleep(backoff.min(POLL_INTERVAL));
                    backoff = (backoff * 2).min(MAX_BACKOFF);
                    self.sleep_interruptible(POLL_INTERVAL);
                    continue;
                }
            }
            let elapsed = cycle.elapsed();
            if elapsed < POLL_INTERVAL {
                self.sleep_interruptible(POLL_INTERVAL - elapsed);
            }
        }
        println!("worker stopped; owned containers continue running and will be reconciled on restart");
        Ok(())
    }

    fn clone_for_heartbeat(&self) -> HeartbeatWorker {
        HeartbeatWorker {
            state: self.state.clone(),
            coordinator: Coordinator::new(
                &self.state.coordinator,
                &self.state.worker_token,
                None,
            )
            .expect("heartbeat client"),
            sampler: Mutex::new(Sampler::new()),
            storage_path: self.storage_path.clone(),
            rtt_ms: Mutex::new(0.0),
        }
    }

    fn sleep_interruptible(&self, budget: Duration) {
        let deadline = Instant::now() + budget;
        while self.running.load(Ordering::SeqCst) && Instant::now() < deadline {
            thread::sleep(Duration::from_millis(100));
        }
    }

    /// One reconcile cycle: flush reports, poll, adopt assignments, supervise.
    fn tick(&mut self) -> Result<(), String> {
        self.flush_reports()?;

        let response = self.coordinator.poll(&self.state.worker_id)?;
        self.max_concurrent = response.max_concurrent;

        let mut keep: HashMap<String, i64> = HashMap::new();
        for assignment in &response.assignments {
            keep.insert(assignment.workload_id.clone(), assignment.attempt);
            self.adopt(assignment);
        }
        for stop in &response.stop {
            keep.entry(stop.workload_id.clone()).or_insert(stop.attempt);
            self.stop_workload(&stop.workload_id, &stop.fence);
        }
        for active in &response.active {
            keep.entry(active.workload_id.clone()).or_insert(active.attempt);
        }
        self.supervise();
        self.reconcile_containers(&keep);
        self.flush_reports()?;
        Ok(())
    }

    fn queue_report(&self, report: QueuedReport) {
        let mut pending = self.pending.lock().expect("pending reports");
        if pending.len() >= MAX_PENDING_REPORTS {
            pending.remove(0);
        }
        pending.push(report);
    }

    fn flush_reports(&self) -> Result<(), String> {
        let batch: Vec<QueuedReport> = {
            let mut pending = self.pending.lock().expect("pending reports");
            let take = pending.len().min(16);
            pending.drain(..take).collect()
        };
        for report in batch {
            let result = self.coordinator.report(
                &self.state.worker_id,
                &report.workload_id,
                &report.fence,
                report.phase,
                &report.body,
            );
            if let Err(err) = result {
                // Requeue at the front so ordering is preserved across reconnects.
                self.pending.lock().expect("pending reports").insert(0, report);
                return Err(err);
            }
        }
        Ok(())
    }

    fn adopt(&mut self, assignment: &crate::client::PollAssignment) {
        let mut tracked = self.tracked.lock().expect("tracked assignments");
        if tracked.contains_key(&assignment.workload_id) {
            return;
        }
        let spec = RunSpec {
            image: assignment.image.clone(),
            args: assignment.args.clone(),
            cpu_millis: assignment.resources.cpu_millis,
            memory_bytes: assignment.resources.memory_bytes,
            gpu_count: assignment.resources.gpu_count,
            worker_id: self.state.worker_id.clone(),
            workload_id: assignment.workload_id.clone(),
            attempt: assignment.attempt,
        };
        match docker::run(&spec) {
            Ok(()) => {
                tracked.insert(
                    assignment.workload_id.clone(),
                    Tracked {
                        attempt: assignment.attempt,
                        fence: assignment.fence.clone(),
                        running_reported: false,
                        last_renew: Instant::now(),
                    },
                );
                drop(tracked);
                self.queue_report(QueuedReport {
                    workload_id: assignment.workload_id.clone(),
                    fence: assignment.fence.clone(),
                    phase: "ack",
                    body: ReportBody::default(),
                });
            }
            Err(err) => {
                drop(tracked);
                eprintln!(
                    "start attempt for {} failed: {err}",
                    assignment.workload_id
                );
                self.queue_report(QueuedReport {
                    workload_id: assignment.workload_id.clone(),
                    fence: assignment.fence.clone(),
                    phase: "exit",
                    body: ReportBody {
                        exit_code: None,
                        started: false,
                        started_at: None,
                        log_tail: err,
                    },
                });
            }
        }
    }

    fn stop_workload(&mut self, workload_id: &str, fence: &str) {
        let attempt = {
            let mut tracked = self.tracked.lock().expect("tracked assignments");
            tracked.remove(workload_id).map(|t| t.attempt)
        };
        let Some(attempt) = attempt else {
            return; // already reported or never adopted; reconciliation cleans up
        };
        let _ = docker::stop(workload_id, attempt);
        let logs = docker::logs_tail(workload_id, attempt).unwrap_or_default();
        let exit_code = docker::inspect(workload_id, attempt).map(|state| state.exit_code as i32);
        let _ = docker::remove(workload_id, attempt);
        self.queue_report(QueuedReport {
            workload_id: workload_id.to_string(),
            fence: fence.to_string(),
            phase: "exit",
            body: ReportBody {
                exit_code,
                started: true,
                started_at: None,
                log_tail: logs,
            },
        });
    }

    fn supervise(&mut self) {
        let snapshot: Vec<(String, i64, String, bool, Instant)> = {
            let tracked = self.tracked.lock().expect("tracked assignments");
            tracked
                .iter()
                .map(|(id, t)| {
                    (
                        id.clone(),
                        t.attempt,
                        t.fence.clone(),
                        t.running_reported,
                        t.last_renew,
                    )
                })
                .collect()
        };
        for (workload_id, attempt, fence, running_reported, last_renew) in snapshot {
            match docker::inspect(&workload_id, attempt) {
                Some(state) if state.status == "running" || state.status == "created" => {
                    let now = Instant::now();
                    if !running_reported && state.status == "running" {
                        self.queue_report(QueuedReport {
                            workload_id: workload_id.clone(),
                            fence: fence.clone(),
                            phase: "running",
                            body: ReportBody {
                                exit_code: None,
                                started: true,
                                started_at: Some(now_or(&state.started_at)),
                                log_tail: docker::logs_tail(&workload_id, attempt).unwrap_or_default(),
                            },
                        });
                        self.mark_running(&workload_id);
                    } else if now.duration_since(last_renew) >= RENEW_INTERVAL {
                        self.queue_report(QueuedReport {
                            workload_id: workload_id.clone(),
                            fence: fence.clone(),
                            phase: "renew",
                            body: ReportBody::default(),
                        });
                        self.mark_renewed(&workload_id);
                    }
                }
                Some(state) => {
                    // Exited (or dead): report the exit and clean up.
                    let logs = docker::logs_tail(&workload_id, attempt).unwrap_or_default();
                    let exit_code = state.exit_code as i32;
                    let started = !state.started_at.is_empty();
                    let _ = docker::remove(&workload_id, attempt);
                    self.tracked
                        .lock()
                        .expect("tracked assignments")
                        .remove(&workload_id);
                    self.queue_report(QueuedReport {
                        workload_id,
                        fence,
                        phase: "exit",
                        body: ReportBody {
                            exit_code: Some(exit_code),
                            started,
                            started_at: None,
                            log_tail: logs,
                        },
                    });
                }
                None => {
                    // The container vanished without our involvement.
                    self.tracked
                        .lock()
                        .expect("tracked assignments")
                        .remove(&workload_id);
                    self.queue_report(QueuedReport {
                        workload_id,
                        fence,
                        phase: "exit",
                        body: ReportBody {
                            exit_code: None,
                            started: false,
                            started_at: None,
                            log_tail: "container disappeared from the local runtime".into(),
                        },
                    });
                }
            }
        }
    }

    fn mark_running(&self, workload_id: &str) {
        if let Some(tracked) = self
            .tracked
            .lock()
            .expect("tracked assignments")
            .get_mut(workload_id)
        {
            tracked.running_reported = true;
            tracked.last_renew = Instant::now();
        }
    }

    fn mark_renewed(&self, workload_id: &str) {
        if let Some(tracked) = self
            .tracked
            .lock()
            .expect("tracked assignments")
            .get_mut(workload_id)
        {
            tracked.last_renew = Instant::now();
        }
    }

    /// Worker-side reconciliation: remove containers the coordinator no longer
    /// knows about (stale attempts, fenced-out orphans).
    fn reconcile_containers(&self, keep: &HashMap<String, i64>) {
        let pending_workloads: Vec<String> = {
            let pending = self.pending.lock().expect("pending reports");
            pending.iter().map(|r| r.workload_id.clone()).collect()
        };
        for owned in docker::list_owned(&self.state.worker_id) {
            let tracked = self
                .tracked
                .lock()
                .expect("tracked assignments")
                .contains_key(&owned.workload_id);
            let expected = keep.get(&owned.workload_id);
            let kept = tracked || expected == Some(&owned.attempt);
            if kept || pending_workloads.iter().any(|id| id == &owned.workload_id) {
                continue;
            }
            // An attempt the coordinator dropped: remove it so stale attempts
            // can never run unobserved.
            println!(
                "reconciling: removing orphan container for {} attempt {}",
                owned.workload_id, owned.attempt
            );
            let _ = docker::remove(&owned.workload_id, owned.attempt);
        }
    }

}

struct HeartbeatWorker {
    state: WorkerState,
    coordinator: Coordinator,
    sampler: Mutex<Sampler>,
    storage_path: PathBuf,
    rtt_ms: Mutex<f64>,
}

impl HeartbeatWorker {
    fn heartbeat_loop(&self, running: Arc<AtomicBool>) {
        let mut backoff = HEARTBEAT_INTERVAL;
        while running.load(Ordering::SeqCst) {
            let capabilities = self.capabilities();
            let telemetry = self
                .sampler
                .lock()
                .expect("sampler")
                .sample();
            let rtt = self.rtt_ms.lock().map(|v| *v).unwrap_or(0.0);
            let beat = Heartbeat {
                capabilities: &capabilities,
                telemetry: Telemetry {
                    cpu_pct: telemetry.cpu_pct,
                    on_battery: telemetry.on_battery,
                    temperature_c: telemetry.temperature_c,
                },
                rtt_ms: rtt,
            };
            match self.coordinator.heartbeat(&self.state.worker_id, &beat) {
                Ok(measured) => {
                    if let Ok(mut rtt) = self.rtt_ms.lock() {
                        *rtt = if *rtt <= 0.0 { measured } else { 0.7 * *rtt + 0.3 * measured };
                    }
                    backoff = HEARTBEAT_INTERVAL;
                }
                Err(err) => {
                    eprintln!("heartbeat failed: {err}; retrying in {backoff:?}");
                    backoff = (backoff * 2).min(MAX_BACKOFF);
                }
            }
            let deadline = Instant::now() + backoff;
            while running.load(Ordering::SeqCst) && Instant::now() < deadline {
                thread::sleep(Duration::from_millis(200));
            }
        }
    }

    fn capabilities(&self) -> serde_json::Value {
        let mut buffer = Vec::new();
        if capabilities::report(&self.storage_path, &mut buffer).is_ok() {
            if let Ok(mut value) = serde_json::from_slice::<serde_json::Value>(&buffer) {
                // The coordinator's capabilities contract has no health field;
                // heartbeat freshness conveys liveness.
                if let Some(map) = value.as_object_mut() {
                    map.remove("health");
                }
                return value;
            }
        }
        json!({ "os": std::env::consts::OS, "arch": std::env::consts::ARCH })
    }
}

fn now_or(raw: &str) -> String {
    if raw.is_empty() || raw.starts_with("0001-01-01") {
        rfc3339_now()
    } else {
        raw.to_string()
    }
}

fn rfc3339_now() -> String {
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs();
    format!("{now}")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pending_reports_bounded_and_ordered() {
        let loop_worker = WorkerLoop::new(
            WorkerState {
                coordinator: "http://127.0.0.1:1".into(),
                worker_id: "wk-00000000000000000000000000000000".into(),
                worker_token: "kwtx".into(),
                name: "test".into(),
            },
            Coordinator::new("http://127.0.0.1:1", "kwtx", None).unwrap(),
        )
        .unwrap();
        for i in 0..(MAX_PENDING_REPORTS + 8) {
            loop_worker.queue_report(QueuedReport {
                workload_id: format!("wl-{i}"),
                fence: "f".into(),
                phase: "ack",
                body: ReportBody::default(),
            });
        }
        let pending = loop_worker.pending.lock().unwrap();
        assert_eq!(pending.len(), MAX_PENDING_REPORTS);
        assert_eq!(pending[0].workload_id, "wl-8");
    }

    #[test]
    fn timestamp_fallback_is_safe() {
        assert_eq!(now_or(""), rfc3339_now());
        assert_eq!(now_or("0001-01-01T00:00:00Z"), rfc3339_now());
        assert_eq!(now_or("2026-01-01T00:00:00Z"), "2026-01-01T00:00:00Z");
    }
}
