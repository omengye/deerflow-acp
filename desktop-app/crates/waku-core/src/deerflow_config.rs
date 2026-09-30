//! Host-side adapter for the Go DeerFlow configuration JSON service.
//! No YAML or credentials are interpreted by the GPUI client.

use std::collections::HashSet;
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, OnceLock};
use std::time::{Duration, Instant};

use agent_client_protocol::schema::v1::{EnvVariable, McpServer, McpServerStdio};
use anyhow::{Context, Result, anyhow, bail};
use parking_lot::Mutex;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};

const DEFAULT_CONFIG: &str = include_str!("../../../resources/deerflow/default-config.yaml");
const MAX_OUTPUT: u64 = 16 * 1024 * 1024;
const MAX_CLIENT_MCP_CONFIG: u64 = 1024 * 1024;
const REDACTED_VALUE: &str = "__DEERFLOW_REDACTED__";
static BOOTSTRAP: OnceLock<Mutex<()>> = OnceLock::new();

#[derive(Clone, Debug)]
struct Paths {
    root: PathBuf,
    user_data: PathBuf,
    config: PathBuf,
    client_mcp_config: PathBuf,
    resources: PathBuf,
    config_cli: PathBuf,
    daemon: PathBuf,
    bridge: PathBuf,
    runtime: PathBuf,
}

/// Waku-owned per-session stdio servers. The host's YAML allowlist remains
/// authoritative; this file only chooses servers from that list and supplies
/// their arguments and session-private environment.
#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct ClientMcpServer {
    name: String,
    command: PathBuf,
    #[serde(default)]
    args: Vec<String>,
    #[serde(default)]
    env: Vec<ClientMcpEnv>,
    #[serde(default, rename = "type", skip_serializing_if = "Option::is_none")]
    transport: Option<String>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct ClientMcpEnv {
    name: String,
    value: String,
}

fn mcp_config_revision(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn read_client_mcp_config(path: &Path) -> Result<(Vec<ClientMcpServer>, String)> {
    let bytes = match std::fs::File::open(path) {
        Ok(file) => {
            let mut bytes = Vec::new();
            file.take(MAX_CLIENT_MCP_CONFIG + 1)
                .read_to_end(&mut bytes)?;
            if bytes.len() as u64 > MAX_CLIENT_MCP_CONFIG {
                bail!("桌面 MCP 服务器配置超过大小限制");
            }
            bytes
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Ok((vec![], String::new()));
        }
        Err(error) => return Err(error).context("无法读取桌面 MCP 服务器配置"),
    };
    let servers = serde_json::from_slice(&bytes)
        .map_err(|_| anyhow!("桌面 MCP 服务器配置不是有效的 JSON 数组"))?;
    Ok((servers, mcp_config_revision(&bytes)))
}

fn parse_client_mcp_config(value: &Value) -> Result<Vec<ClientMcpServer>> {
    let servers: Vec<ClientMcpServer> = serde_json::from_value(value.clone())
        .map_err(|_| anyhow!("会话 MCP 服务器字段结构无效；仅支持 stdio JSON 数组"))?;
    if servers.len() > 8 {
        bail!("会话 MCP 服务器最多配置 8 个");
    }
    let mut names = HashSet::new();
    for server in &servers {
        if server
            .transport
            .as_deref()
            .is_some_and(|kind| kind != "stdio")
        {
            bail!("会话 MCP 服务器仅支持 stdio 类型");
        }
        if server.name.trim().is_empty()
            || server.name.len() > 128
            || server.name.chars().any(char::is_control)
            || !names.insert(&server.name)
        {
            bail!("会话 MCP 服务器名称为空、重复或无效");
        }
        if !server.command.is_absolute() || !server.command.is_file() {
            bail!(
                "会话 MCP 服务器 {} 的 command 必须是已存在的绝对文件路径",
                server.name
            );
        }
        if server.args.len() > 256
            || server.env.len() > 128
            || server
                .args
                .iter()
                .any(|arg| arg.len() > 64 * 1024 || arg.contains('\0'))
        {
            bail!("会话 MCP 服务器 {} 的参数超过限制", server.name);
        }
        let mut env_names = HashSet::new();
        for env in &server.env {
            let key = if cfg!(windows) {
                env.name.to_ascii_uppercase()
            } else {
                env.name.clone()
            };
            if env.name.is_empty()
                || env.name.len() > 256
                || env.name.contains(['=', '\0'])
                || env.value.len() > 64 * 1024
                || env.value.contains('\0')
                || !env_names.insert(key)
            {
                bail!("会话 MCP 服务器 {} 的环境变量名称或值无效", server.name);
            }
        }
    }
    Ok(servers)
}

fn redacted_client_mcp_config(servers: &[ClientMcpServer]) -> Result<Value> {
    let mut visible = servers.to_vec();
    for server in &mut visible {
        for env in &mut server.env {
            if !env.value.is_empty() {
                env.value = REDACTED_VALUE.to_owned();
            }
        }
    }
    Ok(serde_json::to_value(visible)?)
}

fn restore_client_mcp_secrets(
    servers: &mut [ClientMcpServer],
    previous: &[ClientMcpServer],
) -> Result<()> {
    for server in servers {
        for env in &mut server.env {
            if env.value == REDACTED_VALUE {
                let saved = previous
                    .iter()
                    .find(|item| {
                        item.name == server.name
                            && item.command == server.command
                            && item.args == server.args
                    })
                    .and_then(|item| item.env.iter().find(|item| item.name == env.name))
                    .filter(|item| !item.value.is_empty())
                    .ok_or_else(|| {
                        anyhow!(
                            "会话 MCP 服务器 {} 的环境变量 {} 已变化，请重新输入值",
                            server.name,
                            env.name
                        )
                    })?;
                env.value.clone_from(&saved.value);
            }
        }
    }
    Ok(())
}

fn allowed_client_mcp_servers(
    servers: Vec<ClientMcpServer>,
    policy: &Value,
) -> Result<Vec<McpServer>> {
    if servers.is_empty() {
        return Ok(vec![]);
    }
    if policy["enabled"] != true {
        bail!(
            "会话 MCP 服务器未启用；请先在宿主 config.yaml 中启用 local_acp.accept_client_mcp_servers 并配置可执行文件允许列表"
        );
    }
    let allowed = policy["allowed_commands"]
        .as_array()
        .ok_or_else(|| anyhow!("宿主 MCP 可执行文件允许列表无效"))?;
    let mut result = Vec::with_capacity(servers.len());
    for server in servers {
        let command = server
            .command
            .canonicalize()
            .with_context(|| format!("会话 MCP 服务器 {} 的可执行文件不存在", server.name))?;
        if !allowed.iter().filter_map(Value::as_str).any(|item| {
            let Ok(item) = Path::new(item).canonicalize() else {
                return false;
            };
            if cfg!(windows) {
                item.to_string_lossy()
                    .eq_ignore_ascii_case(&command.to_string_lossy())
            } else {
                item == command
            }
        }) {
            bail!(
                "会话 MCP 服务器 {} 的可执行文件不在宿主允许列表中",
                server.name
            );
        }
        let env = server
            .env
            .into_iter()
            .map(|entry| EnvVariable::new(entry.name, entry.value))
            .collect();
        // Preserve the user-facing path on the wire. Windows canonicalize may
        // add a \\?\ prefix that Go's filepath handling does not need.
        result.push(McpServer::Stdio(
            McpServerStdio::new(server.name, server.command)
                .args(server.args)
                .env(env),
        ));
    }
    Ok(result)
}

fn write_client_mcp_config(path: &Path, bytes: &[u8]) -> Result<()> {
    let parent = path
        .parent()
        .ok_or_else(|| anyhow!("桌面 MCP 服务器配置路径无效"))?;
    std::fs::create_dir_all(parent)?;
    let temporary = parent.join(format!(".client-mcp-servers-{}.tmp", uuid::Uuid::new_v4()));
    let result = (|| {
        let mut options = std::fs::OpenOptions::new();
        options.write(true).create_new(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        let mut file = options.open(&temporary)?;
        file.write_all(bytes)?;
        file.sync_all()?;
        std::fs::rename(&temporary, path)?;
        Ok::<_, std::io::Error>(())
    })();
    if result.is_err() {
        let _ = std::fs::remove_file(&temporary);
    }
    result.context("无法保存桌面 MCP 服务器配置")
}

fn mcp_bridge_arguments(policy: &Value) -> Result<Vec<String>> {
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
        let config_cli = root.join(if cfg!(windows) {
            "deerflow-config-go.exe"
        } else {
            "deerflow-config-go"
        });
        let daemon = root.join(if cfg!(windows) {
            "deerflow-acpd.exe"
        } else {
            "deerflow-acpd"
        });
        let bridge = waku_protocol::identity::bundled_acp_binary().unwrap_or_else(|| {
            root.join(if cfg!(windows) {
                "deerflow-acp.exe"
            } else {
                "deerflow-acp"
            })
        });
        let user_data = root.join("user-data");
        let runtime = user_data.join("runtime/acp-go");
        Ok(Self {
            config: user_data.join("config/config.yaml"),
            client_mcp_config: user_data.join("config/client-mcp-servers.json"),
            resources: root.join("resources"),
            runtime,
            root,
            user_data,
            config_cli,
            daemon,
            bridge,
        })
    }

    fn bootstrap(&self) -> Result<()> {
        let _guard = BOOTSTRAP.get_or_init(|| Mutex::new(())).lock();
        if !self.config_cli.is_file() {
            bail!("找不到 DeerFlow Go 配置服务：{}", self.config_cli.display());
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
        let data_dir = self.user_data.join("data/go-harness");
        [
            ("DEER_FLOW_PORTABLE_ROOT", &self.root),
            ("DEER_FLOW_CONFIG_PATH", &self.config),
            ("DEER_FLOW_ACP_RUNTIME_DIR", &self.runtime),
            ("DEERFLOW_GO_DATA_DIR", &data_dir),
        ]
        .into_iter()
        .map(|(key, value)| (key.into(), value.to_string_lossy().into_owned()))
        .collect()
    }

    fn backend_arguments(&self) -> Vec<String> {
        vec![
            "--daemon".into(),
            self.daemon.to_string_lossy().into_owned(),
        ]
    }

    fn go_mcp_arguments(&self) -> Result<Vec<String>> {
        let policy = self.config_command("bridge-policy", &Value::Null)?;
        mcp_bridge_arguments(&policy)
    }

    fn client_mcp_servers(&self) -> Result<Vec<McpServer>> {
        let (servers, _) = read_client_mcp_config(&self.client_mcp_config)?;
        if servers.is_empty() {
            return Ok(vec![]);
        }
        let value = serde_json::to_value(&servers)?;
        let servers = parse_client_mcp_config(&value)?;
        allowed_client_mcp_servers(
            servers,
            &self.config_command("bridge-policy", &Value::Null)?,
        )
    }

    fn config_snapshot(&self) -> Result<Value> {
        let document = self.config_command("snapshot", &Value::Null)?;
        self.with_client_mcp_document(document)
    }

    fn with_client_mcp_document(&self, mut document: Value) -> Result<Value> {
        let (servers, revision) = read_client_mcp_config(&self.client_mcp_config)?;
        document["client_mcp_servers"] = redacted_client_mcp_config(&servers)?;
        document["client_mcp_servers_revision"] = json!(revision);
        Ok(document)
    }

    fn save_config_document(&self, operation: &str, input: &Value) -> Result<Value> {
        // Older Waku clients do not send the MCP fields. Preserve the sidecar
        // verbatim rather than interpreting their absence as a request to clear it.
        let has_servers = input.get("client_mcp_servers").is_some();
        let has_revision = input.get("client_mcp_servers_revision").is_some();
        if !has_servers && !has_revision {
            let document = self.config_command(operation, input)?;
            return if operation == "validate" {
                Ok(document)
            } else {
                self.with_client_mcp_document(document)
            };
        }
        if !has_servers || !has_revision {
            bail!("桌面 MCP 服务器配置缺少列表或版本，请重新加载");
        }
        let (previous, revision) = read_client_mcp_config(&self.client_mcp_config)?;
        if input["client_mcp_servers_revision"].as_str() != Some(revision.as_str()) {
            bail!("桌面 MCP 服务器配置已变化，请重新加载后保存");
        }
        let mut servers = parse_client_mcp_config(&input["client_mcp_servers"])?;
        restore_client_mcp_secrets(&mut servers, &previous)?;
        if !servers.is_empty() {
            allowed_client_mcp_servers(
                servers.clone(),
                &self.config_command("bridge-policy", &Value::Null)?,
            )?;
        }
        let mut serialized = serde_json::to_vec_pretty(&servers)?;
        serialized.push(b'\n');
        if serialized.len() as u64 > MAX_CLIENT_MCP_CONFIG {
            bail!("桌面 MCP 服务器配置超过大小限制");
        }
        // Keep MCP environment values out of the Go YAML configuration service.
        // Waku owns this separate file and sends the values only on ACP session setup.
        let mut config_input = input.clone();
        if let Some(object) = config_input.as_object_mut() {
            object.remove("client_mcp_servers");
            object.remove("client_mcp_servers_revision");
        }
        let document = self.config_command(operation, &config_input)?;
        if operation == "validate" {
            return Ok(document);
        }
        write_client_mcp_config(&self.client_mcp_config, &serialized)
            .context("主配置已保存，但桌面 MCP 服务器配置写入失败")?;
        self.with_client_mcp_document(document)
    }

    fn config_command(&self, operation: &str, input: &Value) -> Result<Value> {
        let mut command = hidden_command(&self.config_cli);
        command
            .arg("--config")
            .arg(&self.config)
            .arg("--user-data")
            .arg(&self.user_data)
            .arg("--resources")
            .arg(&self.resources)
            .arg(operation)
            .envs(self.environment());
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

/// Session-scoped stdio MCP servers selected by the Waku host. The Go daemon
/// independently enforces its executable allowlist again when binding them.
pub fn client_mcp_servers() -> Result<Vec<McpServer>> {
    let paths = Paths::discover()?;
    paths.bootstrap()?;
    paths.client_mcp_servers()
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
            "snapshot" => paths.config_snapshot(),
            "validate" => paths.save_config_document(operation, &input),
            "test-model" => paths.config_command(operation, &input),
            "save" | "save-and-apply" => {
                let _guard = self.state.mutation.lock();
                if self.state.applying.load(Ordering::Acquire) {
                    bail!("正在应用配置，请先等待或取消应用");
                }
                if operation == "save-and-apply" {
                    save_and_apply(
                        || paths.save_config_document("save", &input),
                        || self.begin_apply(&paths, ApplyIntent::Restart),
                    )
                } else {
                    paths.save_config_document("save", &input)
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
        value["acp_backend"] = json!("go");
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
    fn client_mcp_config_redacts_env_and_requires_host_allowlist() {
        let root =
            std::env::temp_dir().join(format!("deerflow-client-mcp-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&root).unwrap();
        let executable = root.join(if cfg!(windows) {
            "server.exe"
        } else {
            "server"
        });
        std::fs::write(&executable, b"fixture").unwrap();
        let path = root.join("client-mcp-servers.json");
        let input = json!([{
            "name":"workspace", "type":"stdio", "command":executable,
            "args":["serve"], "env":[{"name":"MCP_TOKEN","value":"private-token"}]
        }]);
        let servers = parse_client_mcp_config(&input).unwrap();
        write_client_mcp_config(&path, &serde_json::to_vec(&servers).unwrap()).unwrap();
        let (saved, revision) = read_client_mcp_config(&path).unwrap();
        assert!(!revision.is_empty());
        let visible = redacted_client_mcp_config(&saved).unwrap();
        assert_eq!(visible[0]["env"][0]["value"], REDACTED_VALUE);
        assert!(!visible.to_string().contains("private-token"));
        let mut edited = parse_client_mcp_config(&visible).unwrap();
        restore_client_mcp_secrets(&mut edited, &saved).unwrap();
        assert_eq!(edited[0].env[0].value, "private-token");
        let mut retargeted = parse_client_mcp_config(&visible).unwrap();
        retargeted[0].args = vec!["different".into()];
        assert!(restore_client_mcp_secrets(&mut retargeted, &saved).is_err());
        let mut retargeted = parse_client_mcp_config(&visible).unwrap();
        retargeted[0].command = root.join("other-executable");
        assert!(restore_client_mcp_secrets(&mut retargeted, &saved).is_err());
        assert!(allowed_client_mcp_servers(edited.clone(), &json!({"enabled":false})).is_err());
        assert!(
            allowed_client_mcp_servers(
                edited.clone(),
                &json!({"enabled":true,"allowed_commands":[]})
            )
            .is_err()
        );
        let result = allowed_client_mcp_servers(
            edited,
            &json!({
                "enabled":true, "allowed_commands":[executable]
            }),
        )
        .unwrap();
        let wire = serde_json::to_value(result).unwrap();
        assert_eq!(wire[0]["name"], "workspace");
        assert_eq!(wire[0]["env"][0]["value"], "private-token");
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn client_mcp_config_rejects_network_and_unknown_fields_without_echoing_secrets() {
        let invalid = json!([{
            "name":"remote", "type":"http", "url":"https://example.invalid/mcp",
            "command":"secret-token"
        }]);
        let error = parse_client_mcp_config(&invalid).unwrap_err().to_string();
        assert!(!error.contains("secret-token"));
        assert!(parse_client_mcp_config(&json!([{"name":"x","command":"relative"}])).is_err());
    }

    #[test]
    fn client_mcp_config_second_save_replaces_existing_file() {
        let root =
            std::env::temp_dir().join(format!("deerflow-client-mcp-save-{}", uuid::Uuid::new_v4()));
        let path = root.join("config/client-mcp-servers.json");
        write_client_mcp_config(&path, b"[]\n").unwrap();
        write_client_mcp_config(&path, b"[{\"name\":\"updated\"}]\n").unwrap();
        assert_eq!(std::fs::read(&path).unwrap(), b"[{\"name\":\"updated\"}]\n");
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn go_mcp_policy_passes_only_host_allowed_absolute_commands() {
        let root =
            std::env::temp_dir().join(format!("deerflow-mcp-policy-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&root).unwrap();
        let executable = root.join(if cfg!(windows) { "mcp.exe" } else { "mcp" });
        std::fs::write(&executable, b"fixture").unwrap();
        let enabled = json!({"enabled":true,"allowed_commands":[executable.to_string_lossy()]});
        assert_eq!(
            mcp_bridge_arguments(&enabled).unwrap(),
            vec!["--mcp-allow-command", executable.to_str().unwrap()]
        );
        assert!(mcp_bridge_arguments(&json!({"enabled":true,"allowed_commands":[]})).is_err());
        assert!(
            mcp_bridge_arguments(&json!({"enabled":true,"allowed_commands":["relative-command"]}))
                .is_err()
        );
        assert!(
            mcp_bridge_arguments(
                &json!({"enabled":true,"allowed_commands":[root.join("missing")]})
            )
            .is_err()
        );
        assert!(
            mcp_bridge_arguments(&json!({"enabled":false,"allowed_commands":[executable]}))
                .unwrap()
                .is_empty()
        );
        std::fs::remove_dir_all(root).unwrap();
    }
    #[test]
    fn go_only_paths_use_the_bundled_config_cli_and_daemon() {
        let root = std::env::temp_dir().join(format!("deerflow-go-paths-{}", uuid::Uuid::new_v4()));
        let paths = Paths {
            config_cli: root.join(if cfg!(windows) {
                "deerflow-config-go.exe"
            } else {
                "deerflow-config-go"
            }),
            daemon: root.join(if cfg!(windows) {
                "deerflow-acpd.exe"
            } else {
                "deerflow-acpd"
            }),
            bridge: root.join(if cfg!(windows) {
                "deerflow-acp.exe"
            } else {
                "deerflow-acp"
            }),
            config: root.join("user-data/config/config.yaml"),
            client_mcp_config: root.join("user-data/config/client-mcp-servers.json"),
            user_data: root.join("user-data"),
            resources: root.join("resources"),
            runtime: root.join("user-data/runtime/acp-go"),
            root,
        };
        assert_eq!(
            paths.backend_arguments(),
            vec!["--daemon", paths.daemon.to_str().unwrap()]
        );
        assert!(
            paths
                .environment()
                .iter()
                .any(|(key, value)| key == "DEERFLOW_GO_DATA_DIR" && value.ends_with("go-harness"))
        );
        assert!(
            !paths
                .environment()
                .iter()
                .any(|(key, _)| key.contains("PYTHON"))
        );
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
