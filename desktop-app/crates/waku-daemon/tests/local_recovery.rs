use std::fs;
use std::path::Path;
use std::process::Command;
use std::time::{Duration, Instant};

use serde_json::Value;
use waku_client::DaemonSupervisor;

fn read_connection_logs(directory: &Path) -> (Vec<Value>, Vec<Value>) {
    let mut desktop = Vec::new();
    let mut daemon = Vec::new();
    let entries = match fs::read_dir(directory) {
        Ok(entries) => entries,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return (desktop, daemon),
        Err(error) => panic!("cannot read test logs in {directory:?}: {error}"),
    };
    for entry in entries {
        let file = entry.unwrap();
        let name = file.file_name();
        let name = name.to_string_lossy();
        let rows = if name.starts_with("deerflow-desktop-connection-") {
            &mut desktop
        } else if name.starts_with("waku-daemon-connection-") {
            &mut daemon
        } else {
            continue;
        };
        let data = fs::read_to_string(file.path()).unwrap();
        // Writers are concurrent. Only inspect complete JSONL records.
        for line in data
            .split_inclusive('\n')
            .filter(|line| line.ends_with('\n'))
        {
            rows.push(serde_json::from_str(line).unwrap());
        }
    }
    (desktop, daemon)
}

/// Exercise the real Windows/Unix listener, managed child and JSONL writers
/// without pointing either process at a user's existing portable profile.
#[test]
fn local_transport_reconnect_and_process_exit() {
    const CHILD: &str = "DEERFLOW_TEST_RECOVERY_PROFILE";
    let Ok(profile) = std::env::var(CHILD) else {
        let profile = tempfile::tempdir().unwrap();
        let status = Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "local_transport_reconnect_and_process_exit",
                "--nocapture",
            ])
            .env(CHILD, profile.path())
            .env("DEER_FLOW_PORTABLE_ROOT", profile.path())
            .status()
            .unwrap();
        assert!(status.success());
        return;
    };
    assert_eq!(
        waku_protocol::identity::portable_root(),
        Path::new(&profile),
        "the supervisor and the managed daemon must share the disposable profile",
    );

    let supervisor =
        DaemonSupervisor::spawn(Path::new(env!("CARGO_BIN_EXE_waku-daemon")), false).unwrap();
    let updates = supervisor.subscribe_clients();
    let first = updates.recv().unwrap();
    first.disconnect(); // Close this transport while the daemon continues serving.
    let second = updates.recv_timeout(Duration::from_secs(15)).unwrap();
    assert!(!second.same_connection(&first));
    second.shutdown(); // The disposable managed child receives a real shutdown.
    let third = updates.recv_timeout(Duration::from_secs(15)).unwrap();
    assert!(!third.same_connection(&second));
    assert!(!supervisor.local_transport_unavailable());

    let logs = Path::new(&profile).join("user-data/logs");
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        let (desktop, daemon) = read_connection_logs(&logs);
        let resumed_pid = |old: &waku_client::DaemonClient, new: &waku_client::DaemonClient| {
            desktop
                .iter()
                .find(|row| {
                    row["phase"] == "recovered"
                        && row["connection_id"] == old.connection_id().to_string()
                        && row["new_connection_id"] == new.connection_id().to_string()
                })
                .and_then(|row| row["daemon_pid"].as_u64())
        };
        if let (Some(original), Some(restarted)) =
            (resumed_pid(&first, &second), resumed_pid(&second, &third))
        {
            assert_ne!(original, restarted);
            let old_id = first.connection_id().to_string();
            let desktop_saw_exit = desktop.iter().any(|row| {
                row["phase"] == "child_exited"
                    && row["daemon_pid"] == original
                    && row["exit_code"] == 0
            });
            let daemon_saw_close = daemon.iter().any(|row| {
                row["pid"] == original
                    && row["connection_id"] == old_id
                    && row["phase"] == "connection_closed"
            });
            let started = desktop.iter().any(|row| {
                row["phase"] == "recovery_started"
                    && row["daemon_pid"] == original
                    && row["connection_id"] == old_id
            });
            if desktop_saw_exit && daemon_saw_close && started {
                assert!(supervisor.local_recovery_elapsed().is_none());
                break;
            }
        }
        assert!(
            Instant::now() < deadline,
            "missing correlated recovery logs in {logs:?}: {desktop:?}"
        );
        std::thread::sleep(Duration::from_millis(25));
    }
}
