use serde::{Deserialize, Serialize};
use std::fs;
use std::path::PathBuf;

#[derive(Serialize, Deserialize, Clone)]
pub struct WorkerState {
    pub coordinator: String,
    pub worker_id: String,
    pub worker_token: String,
    pub name: String,
}

/// The agent state directory. `KAMUI_AGENT_HOME` overrides `~/.kamui-agent`.
pub fn home() -> PathBuf {
    home_from(
        std::env::var_os("KAMUI_AGENT_HOME").as_deref(),
        std::env::var_os("HOME").as_deref(),
    )
}

/// Pure resolution so tests do not mutate process state.
fn home_from(agent_home: Option<&std::ffi::OsStr>, user_home: Option<&std::ffi::OsStr>) -> PathBuf {
    if let Some(dir) = agent_home {
        return PathBuf::from(dir);
    }
    let home = user_home.map(PathBuf::from).unwrap_or_else(|| PathBuf::from("."));
    home.join(".kamui-agent")
}

pub fn state_path() -> PathBuf {
    home().join("worker.json")
}

pub fn load() -> Option<WorkerState> {
    let data = fs::read(state_path()).ok()?;
    serde_json::from_slice(&data).ok()
}

pub fn save(state: &WorkerState) -> Result<(), String> {
    let dir = home();
    fs::create_dir_all(&dir).map_err(|e| format!("create {}: {e}", dir.display()))?;
    let path = state_path();
    let tmp = dir.join(".worker.json.tmp");
    let data = serde_json::to_vec_pretty(state).map_err(|e| e.to_string())?;
    fs::write(&tmp, &data).map_err(|e| format!("write {}: {e}", tmp.display()))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = fs::set_permissions(&tmp, fs::Permissions::from_mode(0o600));
    }
    fs::rename(&tmp, &path).map_err(|e| format!("rename {}: {e}", path.display()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn home_resolution_prefers_override() {
        let custom: &std::ffi::OsStr = "/tmp/custom".as_ref();
        let user: &std::ffi::OsStr = "/Users/test".as_ref();
        assert_eq!(home_from(Some(custom), Some(user)), PathBuf::from("/tmp/custom"));
        assert_eq!(home_from(None, Some(user)), PathBuf::from("/Users/test/.kamui-agent"));
        assert_eq!(home_from(None, None), PathBuf::from("./.kamui-agent"));
    }

    #[test]
    fn state_round_trips() {
        let dir = std::env::temp_dir().join(format!("kamui-state-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).expect("mkdir");
        let original = std::env::var_os("KAMUI_AGENT_HOME");
        // SAFETY: single-threaded test binary section; the variable is restored below.
        unsafe { std::env::set_var("KAMUI_AGENT_HOME", dir.as_os_str()) };
        let state = WorkerState {
            coordinator: "https://coordinator.example".into(),
            worker_id: "wk-00000000000000000000000000000000".into(),
            worker_token: "kwt0000".into(),
            name: "test laptop".into(),
        };
        save(&state).expect("save");
        let loaded = load().expect("load");
        assert_eq!(loaded.worker_id, state.worker_id);
        assert_eq!(loaded.coordinator, state.coordinator);
        match original {
            Some(value) => unsafe { std::env::set_var("KAMUI_AGENT_HOME", value.as_os_str()) },
            None => unsafe { std::env::remove_var("KAMUI_AGENT_HOME") },
        }
        std::fs::remove_dir_all(&dir).ok();
    }
}
