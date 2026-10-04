use std::{
    fs,
    path::Path,
    process::{Command, Output, Stdio},
    thread,
    time::{Duration, Instant},
};

fn invoke(command: &mut Command) -> Output {
    let mut child = command
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        if child.try_wait().unwrap().is_some() {
            return child.wait_with_output().unwrap();
        }
        if Instant::now() >= deadline {
            let _ = child.kill();
            let output = child.wait_with_output().unwrap();
            panic!("Bridge did not fail promptly: {output:?}");
        }
        thread::sleep(Duration::from_millis(10));
    }
}

fn inert_file(path: &Path) {
    fs::create_dir_all(path.parent().unwrap()).unwrap();
    fs::write(path, b"inert legacy-discovery fixture; not an interpreter").unwrap();
}

#[test]
fn missing_go_daemon_never_uses_legacy_layout_environment_or_path() {
    let temp = tempfile::tempdir().unwrap();
    let root = temp.path();
    let bundle = root.join("bundle with spaces");
    let suffix = if cfg!(windows) { ".exe" } else { "" };
    fs::create_dir_all(&bundle).unwrap();
    let bridge = bundle.join(format!("deerflow-acp{suffix}"));
    fs::copy(env!("CARGO_BIN_EXE_deerflow-acp"), &bridge).unwrap();
    let interpreter = if cfg!(windows) {
        "python.exe"
    } else {
        "python3"
    };
    let venv = root
        .join(".venv")
        .join(if cfg!(windows) { "Scripts" } else { "bin" })
        .join(interpreter);
    let bundled_runtime = bundle
        .join("runtime")
        .join(if cfg!(windows) { "" } else { "bin" })
        .join(interpreter);
    let path_dir = root.join("old-path");
    for path in [
        venv,
        bundled_runtime,
        bundle.join(interpreter),
        path_dir.join(interpreter),
        path_dir.join(format!("deerflow-acpd{suffix}")),
    ] {
        inert_file(&path);
    }
    let config = root.join("config.yaml");
    fs::write(&config, b"models: []\n").unwrap();

    // Cover the obsolete environment override both present and absent: it
    // used to bypass all layout selection before checking the Go sibling.
    for old_override in [false, true] {
        let runtime = root.join(format!("ipc-{old_override}"));
        let mut command = Command::new(&bridge);
        command
            .arg("--config")
            .arg(&config)
            .arg("--runtime-dir")
            .arg(&runtime)
            .arg("--start-daemon")
            .env_remove("DEER_FLOW_ACP_DAEMON")
            .env_remove("DEER_FLOW_ACP_PYTHON")
            .env("PATH", &path_dir)
            .env("DEER_FLOW_ACP_DAEMON_START_TIMEOUT_MS", "300");
        if old_override {
            command.env("DEER_FLOW_ACP_PYTHON", path_dir.join(interpreter));
        }
        let output = invoke(&mut command);
        assert!(!output.status.success());
        assert!(output.stdout.is_empty());
        let error = String::from_utf8_lossy(&output.stderr);
        assert!(
            error.contains("Eino Go daemon executable is missing"),
            "{error}"
        );
        assert!(
            error.contains(
                &bundle
                    .join(format!("deerflow-acpd{suffix}"))
                    .display()
                    .to_string()
            ),
            "{error}"
        );
        assert!(!runtime.exists(), "failure must precede daemon startup");
    }
}

#[test]
fn removed_python_argument_is_rejected_before_touching_runtime() {
    let temp = tempfile::tempdir().unwrap();
    for flag in ["--python", "--python=old-runtime"] {
        let runtime = temp.path().join("ipc");
        let output = invoke(
            Command::new(env!("CARGO_BIN_EXE_deerflow-acp"))
                .arg("--runtime-dir")
                .arg(&runtime)
                .arg("--start-daemon")
                .arg(flag),
        );
        assert!(!output.status.success());
        let error = String::from_utf8_lossy(&output.stderr);
        assert!(error.contains("--python has been removed"), "{error}");
        assert!(error.contains("--daemon PATH"), "{error}");
        assert!(!runtime.exists());
    }
}
