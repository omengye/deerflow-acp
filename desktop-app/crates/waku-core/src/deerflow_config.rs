//! Host-side adapter for the existing DeerFlow configuration JSON service.
//! No YAML or credentials are interpreted by the GPUI client.

use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, OnceLock};
use std::time::{Duration, Instant};

use anyhow::{Context, Result, anyhow, bail};
use parking_lot::Mutex;
use serde_json::{Value, json};

const DEFAULT_CONFIG: &str = include_str!("../../../resources/deerflow/default-config.yaml");
const MAX_OUTPUT: u64 = 16 * 1024 * 1024;
static BOOTSTRAP: OnceLock<Mutex<()>> = OnceLock::new();

#[derive(Clone, Debug)]
struct Paths {
    root: PathBuf,
    user_data: PathBuf,
    config: PathBuf,
    resources: PathBuf,
    python: PathBuf,
    acp_backend: AcpBackend,
    bridge: PathBuf,
    runtime: PathBuf,
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum AcpBackend {
    Python,
    Go(PathBuf),
}

fn select_acp_backend(
    root: &Path,
    user_data: &Path,
    override_name: Option<&str>,
) -> Result<AcpBackend> {
    let go = root.join(if cfg!(windows) {
        "deerflow-acpd.exe"
    } else {
        "deerflow-acpd"
    });
    match override_name {
        Some("python") => return Ok(AcpBackend::Python),
        Some("go") if go.is_file() => return Ok(AcpBackend::Go(go)),
        Some("go") => bail!("找不到 DeerFlow Go ACP daemon：{}", go.display()),
        Some(value) => bail!("DEER_FLOW_DESKTOP_ACP_BACKEND 只能是 go 或 python：{value}"),
        None => {}
    }
    // Existing Python sessions have no automatic Go checkpoint migration.
    // Keep their current backend when an updated package reuses user-data.
    if go.is_file() && user_data.join("data/go-harness/harness.db").is_file() {
        return Ok(AcpBackend::Go(go));
    }
    let old_data = user_data.join("data");
    if old_data.join("acp-sessions.db").is_file() || old_data.join("acp-checkpoints.db").is_file() {
        return Ok(AcpBackend::Python);
    }
    if go.is_file() {
        Ok(AcpBackend::Go(go))
    } else {
        Ok(AcpBackend::Python)
    }
}

fn mcp_bridge_arguments(backend: &AcpBackend, policy: &Value) -> Result<Vec<String>> {
    if *backend == AcpBackend::Python {
        return Ok(Vec::new());
    }
    let enabled = policy["enabled"]
        .as_bool()
        .ok_or_else(|| anyhow!("DeerFlow MCP host policy is missing its enabled flag"))?;
    if !enabled {
        return Ok(Vec::new());
    }
    let commands = policy["allowed_commands"]
        .as_array()
        .ok_or_else(|| anyhow!("DeerFlow MCP host policy has no command list"))?;
    if commands.is_empty() || commands.len() > 32 {
        bail!("DeerFlow MCP host policy requires 1..32 executable commands");
    }
    let mut arguments = Vec::with_capacity(commands.len() * 2);
    for command in commands {
        let path = command
            .as_str()
            .map(Path::new)
            .ok_or_else(|| anyhow!("DeerFlow MCP host policy contains a non-path command"))?;
        if !path.is_absolute() || !path.is_file() {
            bail!("DeerFlow MCP host policy requires an existing absolute executable path");
        }
        arguments.push("--mcp-allow-command".to_owned());
        arguments.push(path.to_string_lossy().into_owned());
    }
    Ok(arguments)
}

impl Paths {
    fn discover() -> Result<Self> {
        let root = waku_protocol::identity::portable_root();
        let source = Path::new(env!("CARGO_MANIFEST_DIR"))
            .ancestors()
            .nth(3)
            .expect("workspace layout");
        let python = root.join("runtime/python.exe");
        let python = if python.is_file() {
            python
        } else {
            source.join(if cfg!(windows) {
                ".venv/Scripts/python.exe"
            } else {
                ".venv/bin/python"
            })
        };
        let bridge = waku_protocol::identity::bundled_acp_binary().unwrap_or_else(|| {
            root.join(if cfg!(windows) {
                "deerflow-acp.exe"
            } else {
                "deerflow-acp"
            })
        });
        let user_data = root.join("user-data");
        let acp_backend = select_acp_backend(
            &root,
            &user_data,
            std::env::var("DEER_FLOW_DESKTOP_ACP_BACKEND")
                .ok()
                .as_deref(),
        )?;
        let runtime = user_data.join(match &acp_backend {
            AcpBackend::Python => "runtime/acp",
            AcpBackend::Go(_) => "runtime/acp-go",
        });
        Ok(Self {
            config: user_data.join("config/config.yaml"),
            resources: root.join("resources"),
            runtime,
            root,
            user_data,
            python,
            acp_backend,
            bridge,
        })
    }

    fn bootstrap(&self) -> Result<()> {
        let _guard = BOOTSTRAP.get_or_init(|| Mutex::new(())).lock();
        if !self.python.is_file() {
            bail!("找不到 DeerFlow Python 运行时：{}", self.python.display());
        }
        let template = self.resources.join("default-config.yaml");
        if !template.is_file() {
            std::fs::create_dir_all(&self.resources)?;
            // Create-new protects a package template provided by another launcher.
            match std::fs::OpenOptions::new()
                .write(true)
                .create_new(true)
                .open(&template)
            {
                Ok(mut file) => file.write_all(DEFAULT_CONFIG.as_bytes())?,
                Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => {}
                Err(error) => return Err(error.into()),
            }
        }
        if !self.config.is_file() {
            self.config_command("init", &Value::Null)?;
        }
        Ok(())
    }

    fn environment(&self) -> Vec<(String, String)> {
        let mut values: Vec<(String, String)> = [
            ("DEER_FLOW_PORTABLE_ROOT", &self.root),
            ("DEER_FLOW_CONFIG_PATH", &self.config),
            ("DEER_FLOW_ACP_RUNTIME_DIR", &self.runtime),
        ]
        .into_iter()
        .map(|(key, value)| (key.into(), value.to_string_lossy().into_owned()))
        .collect();
        match &self.acp_backend {
            AcpBackend::Python => values.push((
                "DEER_FLOW_ACP_PYTHON".into(),
                self.python.to_string_lossy().into_owned(),
            )),
            AcpBackend::Go(_) => values.push((
                "DEERFLOW_GO_DATA_DIR".into(),
                self.user_data
                    .join("data/go-harness")
                    .to_string_lossy()
                    .into_owned(),
            )),
        }
        values
    }

    fn backend_arguments(&self) -> Vec<String> {
        match &self.acp_backend {
            AcpBackend::Python => vec![
                "--python".into(),
                self.python.to_string_lossy().into_owned(),
            ],
            AcpBackend::Go(path) => vec!["--daemon".into(), path.to_string_lossy().into_owned()],
        }
    }

    fn go_mcp_arguments(&self) -> Result<Vec<String>> {
        if self.acp_backend == AcpBackend::Python {
            return Ok(Vec::new());
        }
        let policy = self.config_command("bridge-policy", &Value::Null)?;
        mcp_bridge_arguments(&self.acp_backend, &policy)
    }

    fn config_command(&self, operation: &str, input: &Value) -> Result<Value> {
        let mut command = hidden_command(&self.python);
        command
            .args(["-m", "deerflow.config_tool", "--config"])
            .arg(&self.config)
            .arg("--user-data")
            .arg(&self.user_data)
            .arg("--resources")
            .arg(&self.resources)
            .arg(operation)
            .envs(self.environment())
            .env("PYTHONUTF8", "1")
            .env("PYTHONIOENCODING", "utf-8");
        decode_envelope(&run(command, Some(input), Duration::from_secs(100))?)
    }

    fn bridge_command(&self, mode: &str, input: Option<&Value>) -> Result<String> {
        if !self.bridge.is_file() {
            bail!("找不到 ACP Bridge：{}", self.bridge.display());
        }
        let mcp_arguments = if mode == "--start-daemon" {
            self.go_mcp_arguments()?
        } else {
            Vec::new()
        };
        let mut command = hidden_command(&self.bridge);
        command
            .arg(mode)
            .arg("--config")
            .arg(&self.config)
            .args(self.backend_arguments())
            .args(mcp_arguments)
            .arg("--runtime-dir")
            .arg(&self.runtime)
            .envs(self.environment());
        run(
            command,
            input,
            Duration::from_secs(if mode == "--start-daemon" { 130 } else { 30 }),
        )
    }

    fn manage(&self, input: &Value) -> Result<Value> {
        decode_envelope(&self.bridge_command("--manage", Some(input))?)
    }

    fn live_status(&self) -> Value {
        match self.manage(&json!({"operation":"daemon.status"})) {
            Ok(mut value) => {
                value["running"] = json!(true);
                value
            }
            Err(error) => json!({"running":false,"status_error":error.to_string()}),
        }
    }
}

/// Called from provider workers before launching the ACP bridge. Development
/// must never attach to the unrelated API server's configuration or daemon.
pub fn launch_environment() -> Result<Vec<(String, String)>> {
    let paths = Paths::discover()?;
    paths.bootstrap()?;
    Ok(paths.environment())
}

/// The bridge selects its own configuration and daemon before spawning it, so
/// env overrides alone are insufficient when using a bridge from another bundle.
pub fn launch_arguments() -> Result<Vec<String>> {
    let paths = Paths::discover()?;
    paths.bootstrap()?;
    let mut arguments = vec![
        "--config".into(),
        paths.config.to_string_lossy().into_owned(),
    ];
    arguments.extend(paths.backend_arguments());
    arguments.extend(paths.go_mcp_arguments()?);
    arguments.extend([
        "--runtime-dir".into(),
        paths.runtime.to_string_lossy().into_owned(),
    ]);
    Ok(arguments)
}

#[derive(Default)]
struct ServiceState {
    applying: AtomicBool,
    starting: AtomicBool,
    pending_restart: AtomicBool,
    applied_generation: AtomicU64,
    cancel: AtomicBool,
    apply_error: Mutex<Option<String>>,
    // Serializes config writes against the final restart, not against polling.
    mutation: Mutex<()>,
}

#[derive(Clone, Default)]
pub struct DeerFlowService {
    state: Arc<ServiceState>,
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum ApplyIntent {
    EnsureRunning,
    Restart,
}

impl DeerFlowService {
    pub fn shutdown(&self) {
        self.state.cancel.store(true, Ordering::Release);
    }

    pub fn request(&self, operation: &str, input: Value) -> Result<Value> {
        ensure_operation(operation)?;
        let paths = Paths::discover()?;
        paths.bootstrap()?;
        match operation {
            "snapshot" | "validate" | "test-model" => paths.config_command(operation, &input),
            "save" | "save-and-apply" => {
                let _guard = self.state.mutation.lock();
                if self.state.applying.load(Ordering::Acquire) {
                    bail!("正在应用配置，请先等待或取消应用");
                }
                if operation == "save-and-apply" {
                    save_and_apply(
                        || paths.config_command("save", &input),
                        || self.begin_apply(&paths, ApplyIntent::Restart),
                    )
                } else {
                    paths.config_command("save", &input)
                }
            }
            "manage" => paths.manage(&input),
            "status" => Ok(self.status(&paths)),
            "apply" => {
                let _guard = self.state.mutation.lock();
                self.begin_apply(&paths, ApplyIntent::Restart)
            }
            "cancel-apply" => {
                self.state.cancel.store(true, Ordering::Release);
                self.state.pending_restart.store(false, Ordering::Release);
                Ok(self.status(&paths))
            }
            "start" => {
                // Use the same background workflow so cold starts cannot time out the RPC.
                let _guard = self.state.mutation.lock();
                if paths.live_status()["running"] == true {
                    return Ok(self.status(&paths));
                }
                self.begin_apply(&paths, ApplyIntent::EnsureRunning)
            }
            "stop" => {
                let _guard = self.state.mutation.lock();
                if self.state.applying.load(Ordering::Acquire) {
                    bail!("请先取消正在进行的配置应用");
                }
                let value = paths.manage(&json!({"operation":"daemon.drain"}))?;
                if value["active_operations"].as_u64().unwrap_or(1) != 0 {
                    let _ = paths.manage(&json!({"operation":"daemon.resume"}));
                    bail!("仍有正在运行或排队的任务，请先停止任务再关闭服务");
                }
                if let Err(error) = paths.bridge_command("--stop-daemon", None) {
                    let _ = paths.manage(&json!({"operation":"daemon.resume"}));
                    return Err(error);
                }
                *self.state.apply_error.lock() = None;
                Ok(self.status(&paths))
            }
            _ => unreachable!("operation allowlist"),
        }
    }

    fn status(&self, paths: &Paths) -> Value {
        let mut value = paths.live_status();
        value["applying"] = json!(self.state.applying.load(Ordering::Acquire));
        value["applied_generation"] = json!(self.state.applied_generation.load(Ordering::Acquire));
        value["apply_error"] = json!(*self.state.apply_error.lock());
        value["acp_backend"] = json!(match &paths.acp_backend {
            AcpBackend::Python => "python",
            AcpBackend::Go(_) => "go",
        });
        value["config_path"] = json!(paths.config);
        value["user_data"] = json!(paths.user_data);
        value
    }

    /// Caller holds mutation: the saved revision and scheduled restart cannot
    /// be interleaved with another config write.
    fn begin_apply(&self, paths: &Paths, intent: ApplyIntent) -> Result<Value> {
        if self.state.applying.swap(true, Ordering::AcqRel) {
            // An explicit apply must still run if it arrives during automatic startup.
            // Repeated apply requests during an ordinary restart are redundant.
            if intent == ApplyIntent::Restart && self.state.starting.load(Ordering::Acquire) {
                self.state.cancel.store(false, Ordering::Release);
                self.state.pending_restart.store(true, Ordering::Release);
            }
            return Ok(self.status(paths));
        }
        self.state
            .starting
            .store(intent == ApplyIntent::EnsureRunning, Ordering::Release);
        self.state.cancel.store(false, Ordering::Release);
        *self.state.apply_error.lock() = None;
        let service = self.clone();
        let worker_paths = paths.clone();
        if let Err(error) = std::thread::Builder::new()
            .name("deerflow-apply".into())
            .spawn(move || {
                let mut intent = intent;
                loop {
                    if let Err(error) = service.apply(&worker_paths, intent) {
                        let _ = worker_paths.manage(&json!({"operation":"daemon.resume"}));
                        *service.state.apply_error.lock() = Some(error.to_string());
                    }
                    // begin_apply is called under mutation. Holding it here makes
                    // checking the queued restart and clearing applying atomic
                    // with respect to the next explicit apply request.
                    let _guard = service.state.mutation.lock();
                    if intent == ApplyIntent::EnsureRunning
                        && service.state.pending_restart.swap(false, Ordering::AcqRel)
                    {
                        *service.state.apply_error.lock() = None;
                        service.state.starting.store(false, Ordering::Release);
                        intent = ApplyIntent::Restart;
                        continue;
                    }
                    service.state.starting.store(false, Ordering::Release);
                    service.state.applying.store(false, Ordering::Release);
                    break;
                }
            })
        {
            *self.state.apply_error.lock() = Some(error.to_string());
            self.state.starting.store(false, Ordering::Release);
            self.state.applying.store(false, Ordering::Release);
            return Err(error.into());
        }
        Ok(self.status(paths))
    }

    fn apply(&self, paths: &Paths, intent: ApplyIntent) -> Result<()> {
        if intent == ApplyIntent::EnsureRunning {
            if self.state.cancel.load(Ordering::Acquire) {
                return Ok(());
            }
            // The bridge's --start-daemon is idempotent. Another ACP client may
            // have started the service since request("start") checked status;
            // never drain or stop that process for a startup request.
            paths.bridge_command("--start-daemon", None)?;
            self.state.applied_generation.fetch_add(1, Ordering::AcqRel);
            return Ok(());
        }
        if paths.bridge_command("--status", None).is_ok() {
            paths.manage(&json!({"operation":"daemon.drain"}))?;
            loop {
                if self.state.cancel.load(Ordering::Acquire) {
                    paths.manage(&json!({"operation":"daemon.resume"}))?;
                    return Ok(());
                }
                let live = paths.manage(&json!({"operation":"daemon.status"}))?;
                if live["active_operations"].as_u64() == Some(0) {
                    break;
                }
                std::thread::sleep(Duration::from_millis(500));
            }
            let _guard = self.state.mutation.lock();
            if self.state.cancel.load(Ordering::Acquire) {
                paths.manage(&json!({"operation":"daemon.resume"}))?;
                return Ok(());
            }
            paths.bridge_command("--stop-daemon", None)?;
        }
        if !self.state.cancel.load(Ordering::Acquire) {
            paths.bridge_command("--start-daemon", None)?;
            self.state.applied_generation.fetch_add(1, Ordering::AcqRel);
        }
        Ok(())
    }
}

fn save_and_apply(
    save: impl FnOnce() -> Result<Value>,
    apply: impl FnOnce() -> Result<Value>,
) -> Result<Value> {
    let document = save()?;
    // A failed restart must never disguise a successful save. Return the new
    // revision so the user can retry application without resaving a stale draft.
    let status =
        apply().unwrap_or_else(|error| json!({"applying":false,"apply_error":error.to_string()}));
    Ok(json!({"document":document,"status":status}))
}

fn ensure_operation(operation: &str) -> Result<()> {
    if !matches!(
        operation,
        "snapshot"
            | "save"
            | "save-and-apply"
            | "validate"
            | "test-model"
            | "manage"
            | "status"
            | "start"
            | "stop"
            | "apply"
            | "cancel-apply"
    ) {
        bail!("不支持的 DeerFlow 操作：{operation}");
    }
    Ok(())
}

fn decode_envelope(output: &str) -> Result<Value> {
    let envelope: Value =
        serde_json::from_str(output.trim()).context("DeerFlow 返回了无效 JSON")?;
    if envelope["ok"] != true {
        bail!(
            "{}",
            envelope["error"].as_str().unwrap_or("DeerFlow 操作失败")
        );
    }
    envelope
        .get("data")
        .cloned()
        .ok_or_else(|| anyhow!("DeerFlow 响应缺少数据"))
}

fn hidden_command(program: &Path) -> Command {
    let mut command = Command::new(program);
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        command.creation_flags(0x0800_0000);
    }
    command
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    command
}

fn run(mut command: Command, input: Option<&Value>, timeout: Duration) -> Result<String> {
    let mut child = command.spawn().context("无法启动 DeerFlow 服务进程")?;
    let stdout = child.stdout.take().expect("piped stdout");
    let stderr = child.stderr.take().expect("piped stderr");
    let out = std::thread::spawn(move || {
        let mut bytes = Vec::new();
        stdout
            .take(MAX_OUTPUT + 1)
            .read_to_end(&mut bytes)
            .map(|_| bytes)
    });
    let err = std::thread::spawn(move || {
        let mut bytes = Vec::new();
        stderr
            .take(MAX_OUTPUT + 1)
            .read_to_end(&mut bytes)
            .map(|_| bytes)
    });
    if let Some(mut stdin) = child.stdin.take() {
        if let Some(input) = input {
            if let Err(error) = stdin.write_all(&serde_json::to_vec(input)?) {
                let _ = child.kill();
                let _ = child.wait();
                return Err(error.into());
            }
        }
    }
    let deadline = Instant::now() + timeout;
    let status = loop {
        if let Some(status) = child.try_wait()? {
            break status;
        }
        if Instant::now() >= deadline {
            let _ = child.kill();
            let _ = child.wait();
            bail!("DeerFlow 操作超时，请稍后重试");
        }
        std::thread::sleep(Duration::from_millis(25));
    };
    let output = out
        .join()
        .map_err(|_| anyhow!("读取 DeerFlow 输出失败"))??;
    let errors = err
        .join()
        .map_err(|_| anyhow!("读取 DeerFlow 错误输出失败"))??;
    if output.len() as u64 > MAX_OUTPUT || errors.len() as u64 > MAX_OUTPUT {
        bail!("DeerFlow 响应超过大小限制");
    }
    let output = String::from_utf8(output).context("DeerFlow 输出不是 UTF-8")?;
    if !status.success() {
        if let Ok(envelope) = serde_json::from_str::<Value>(&output) {
            if let Some(error) = envelope["error"].as_str() {
                bail!("{error}");
            }
        }
        let error = String::from_utf8_lossy(&errors);
        bail!(
            "{}",
            if error.trim().is_empty() {
                "DeerFlow 服务操作失败"
            } else {
                error.trim()
            }
        );
    }
    Ok(output)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn go_mcp_policy_passes_only_host_allowed_absolute_commands() {
        let root =
            std::env::temp_dir().join(format!("deerflow-mcp-policy-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&root).unwrap();
        let daemon = root.join("deerflow-acpd");
        let executable = root.join(if cfg!(windows) { "mcp.exe" } else { "mcp" });
        std::fs::write(&executable, b"fixture").unwrap();
        let go = AcpBackend::Go(daemon);
        let enabled = json!({"enabled":true,"allowed_commands":[executable.to_string_lossy()]});
        assert_eq!(
            mcp_bridge_arguments(&go, &enabled).unwrap(),
            vec!["--mcp-allow-command", executable.to_str().unwrap()]
        );
        assert!(mcp_bridge_arguments(&go, &json!({"enabled":true,"allowed_commands":[]})).is_err());
        assert!(
            mcp_bridge_arguments(
                &go,
                &json!({"enabled":true,"allowed_commands":["relative-command"]})
            )
            .is_err()
        );
        assert!(
            mcp_bridge_arguments(
                &go,
                &json!({"enabled":true,"allowed_commands":[root.join("missing")]})
            )
            .is_err()
        );
        assert!(
            mcp_bridge_arguments(
                &go,
                &json!({"enabled":false,"allowed_commands":[executable]})
            )
            .unwrap()
            .is_empty()
        );
        assert!(
            mcp_bridge_arguments(&AcpBackend::Python, &enabled)
                .unwrap()
                .is_empty()
        );
        std::fs::remove_dir_all(root).unwrap();
    }
    #[test]
    fn new_desktop_uses_go_and_existing_python_sessions_keep_python() {
        let root =
            std::env::temp_dir().join(format!("deerflow-acp-backend-{}", uuid::Uuid::new_v4()));
        let user_data = root.join("user-data");
        std::fs::create_dir_all(&user_data).unwrap();
        assert_eq!(
            select_acp_backend(&root, &user_data, None).unwrap(),
            AcpBackend::Python
        );
        assert!(select_acp_backend(&root, &user_data, Some("go")).is_err());
        let daemon = root.join(if cfg!(windows) {
            "deerflow-acpd.exe"
        } else {
            "deerflow-acpd"
        });
        std::fs::write(&daemon, b"test binary").unwrap();
        assert_eq!(
            select_acp_backend(&root, &user_data, None).unwrap(),
            AcpBackend::Go(daemon.clone())
        );
        std::fs::create_dir_all(user_data.join("data")).unwrap();
        std::fs::write(user_data.join("data/acp-sessions.db"), b"legacy").unwrap();
        assert_eq!(
            select_acp_backend(&root, &user_data, None).unwrap(),
            AcpBackend::Python
        );
        assert_eq!(
            select_acp_backend(&root, &user_data, Some("go")).unwrap(),
            AcpBackend::Go(daemon.clone())
        );
        assert_eq!(
            select_acp_backend(&root, &user_data, Some("python")).unwrap(),
            AcpBackend::Python
        );
        std::fs::create_dir_all(user_data.join("data/go-harness")).unwrap();
        std::fs::write(user_data.join("data/go-harness/harness.db"), b"go state").unwrap();
        assert_eq!(
            select_acp_backend(&root, &user_data, None).unwrap(),
            AcpBackend::Go(daemon)
        );
        assert!(select_acp_backend(&root, &user_data, Some("unknown")).is_err());
        std::fs::remove_dir_all(root).unwrap();
    }
    #[test]
    fn invalid_save_never_applies() {
        let result = save_and_apply(
            || bail!("invalid model"),
            || panic!("must not restart on invalid save"),
        );
        assert_eq!(result.unwrap_err().to_string(), "invalid model");
    }

    #[test]
    fn saved_revision_is_returned_even_when_application_fails() {
        let result = save_and_apply(
            || Ok(json!({"config_revision":"saved","models":[]})),
            || bail!("restart failed"),
        )
        .unwrap();
        assert_eq!(result["document"]["config_revision"], "saved");
        assert_eq!(result["status"]["apply_error"], "restart failed");
        assert_eq!(result["status"]["applying"], false);
    }

    #[test]
    fn application_starts_only_after_save_completes() {
        let saved = std::cell::Cell::new(false);
        let result = save_and_apply(
            || {
                saved.set(true);
                Ok(json!({"config_revision":"new"}))
            },
            || {
                assert!(saved.get());
                Ok(json!({"applying":true}))
            },
        )
        .unwrap();
        assert_eq!(result["document"]["config_revision"], "new");
        assert_eq!(result["status"]["applying"], true);
    }

    #[test]
    fn config_errors_preserve_validation_feedback_without_echoing_input() {
        let error = decode_envelope(r#"{"ok":false,"error":"configuration changed"}"#).unwrap_err();
        assert_eq!(error.to_string(), "configuration changed");
        assert_eq!(
            decode_envelope(r#"{"ok":true,"data":{"models":[]}}"#).unwrap()["models"],
            json!([])
        );
    }
    #[test]
    fn arbitrary_operations_never_reach_a_subprocess() {
        for operation in [
            "powershell",
            "--gateway",
            "save && echo secret",
            "",
            "inspect",
            "restore",
        ] {
            assert!(ensure_operation(operation).is_err());
        }
        assert!(ensure_operation("validate").is_ok());
    }
}
