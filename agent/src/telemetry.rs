//! Best-effort host telemetry. Values the platform does not expose reliably
//! stay `None` — telemetry is never fabricated.

use crate::client::Telemetry;
use sysinfo::{CpuRefreshKind, MemoryRefreshKind, System};

pub struct Sampler {
    system: System,
}

impl Sampler {
    pub fn new() -> Self {
        let mut system = System::new();
        system.refresh_cpu_specifics(CpuRefreshKind::everything());
        system.refresh_memory_specifics(MemoryRefreshKind::everything());
        Sampler { system }
    }

    pub fn sample(&mut self) -> Telemetry {
        self.system.refresh_cpu_specifics(CpuRefreshKind::everything());
        self.system.refresh_memory_specifics(MemoryRefreshKind::everything());
        let cpu = self.system.global_cpu_usage();
        let cpu_pct = if cpu.is_finite() {
            Some(cpu.clamp(0.0, 100.0) as f64)
        } else {
            None
        };
        Telemetry {
            cpu_pct,
            on_battery: on_battery(),
            temperature_c: temperature_c(),
        }
    }
}

/// Battery state where the OS exposes it reliably. `None` means unknown.
fn on_battery() -> Option<bool> {
    #[cfg(target_os = "linux")]
    {
        let mut discharging = None;
        if let Ok(entries) = std::fs::read_dir("/sys/class/power_supply") {
            for entry in entries.flatten() {
                let path = entry.path();
                if !path.join("status").exists() {
                    continue;
                }
                if let Ok(status) = std::fs::read_to_string(path.join("status")) {
                    match status.trim() {
                        "Discharging" => return Some(true),
                        "Charging" | "Full" | "Not charging" | "Unknown" => discharging = Some(false),
                        _ => {}
                    }
                }
            }
        }
        discharging
    }
    #[cfg(target_os = "macos")]
    {
        let output = std::process::Command::new("pmset")
            .arg("-g")
            .arg("batt")
            .output()
            .ok()?;
        let text = String::from_utf8_lossy(&output.stdout);
        if text.contains("Battery Power") {
            Some(true)
        } else if text.contains("AC Power") {
            Some(false)
        } else {
            None
        }
    }
    #[cfg(not(any(target_os = "linux", target_os = "macos")))]
    {
        None
    }
}

/// Temperature in Celsius when a thermal zone reports it. `None` means unknown.
fn temperature_c() -> Option<f64> {
    #[cfg(target_os = "linux")]
    {
        let mut hottest: Option<f64> = None;
        if let Ok(entries) = std::fs::read_dir("/sys/class/thermal") {
            for entry in entries.flatten() {
                let path = entry.path().join("temp");
                let Ok(raw) = std::fs::read_to_string(&path) else {
                    continue;
                };
                let Ok(milli_c) = raw.trim().parse::<f64>() else {
                    continue;
                };
                let celsius = milli_c / 1000.0;
                // Reject implausible readings rather than trusting them.
                if !(0.0..=150.0).contains(&celsius) {
                    continue;
                }
                hottest = Some(hottest.map_or(celsius, |prev: f64| prev.max(celsius)));
            }
        }
        hottest
    }
    #[cfg(not(target_os = "linux"))]
    {
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sample_reports_values_or_unknowns() {
        let mut sampler = Sampler::new();
        let telemetry = sampler.sample();
        if let Some(cpu) = telemetry.cpu_pct {
            assert!((0.0..=100.0).contains(&cpu));
        }
        if let Some(temp) = telemetry.temperature_c {
            assert!((0.0..=150.0).contains(&temp));
        }
    }
}
