use serde::Deserialize;
use std::env;
use std::error::Error;
use std::ffi::OsString;
use std::fs;
use std::io::{self, Read, Write};
use std::net::{Shutdown, SocketAddr, TcpStream, ToSocketAddrs};
use std::path::{Path, PathBuf};
#[cfg(not(windows))]
use std::process::{Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};

mod gateway;
mod remote;
mod v2;
#[cfg(windows)]
mod windows_daemon;

const HANDSHAKE_VERSION: &str = "DFACP/1";
const ENDPOINT_FILENAME: &str = "endpoint.json";
const DEFAULT_START_TIMEOUT: Duration = Duration::from_secs(120);
const DEFAULT_STOP_TIMEOUT: Duration = Duration::from_secs(10);
const FORCED_STOP_TIMEOUT: Duration = Duration::from_secs(5);
const CONNECT_TIMEOUT: Duration = Duration::from_secs(2);
const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(3);
const MANAGEMENT_TIMEOUT: Duration = Duration::from_secs(10 * 60);
const MANAGEMENT_REQUEST_LIMIT: usize = 64 * 1024;
const MANAGEMENT_RESPONSE_LIMIT: usize = 4 * 1024 * 1024;

type AnyError = Box<dyn Error + Send + Sync>;
type Result<T> = std::result::Result<T, AnyError>;

#[derive(Debug, Deserialize)]
pub(crate) struct Endpoint {
    pub(crate) host: String,
    pub(crate) port: u16,
    pub(crate) token: String,
    pub(crate) pid: u32,
    pub(crate) build_id: String,
    pub(crate) config_path: String,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Mode {
    Proxy,
    Status,
    Start,
    Stop,
    Manage,
    Gateway,
    Remote,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum AcpProtocol {
    V1,
    V2,
}

#[derive(Debug)]
struct Cli {
    mode: Mode,
    protocol: AcpProtocol,
    config: Option<PathBuf>,
    daemon: Option<PathBuf>,
    mcp_allow_commands: Vec<PathBuf>,
    runtime_dir: Option<PathBuf>,
    auto_start: bool,
    gateway_listen: String,
    gateway_workspace: Option<PathBuf>,
    gateway_path: String,
    remote_url: Option<String>,
    token_env: String,
}

#[derive(Debug)]
enum ConnectError {
    Unavailable(String),
    Rejected(String),
}

impl std::fmt::Display for ConnectError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Unavailable(message) | Self::Rejected(message) => f.write_str(message),
        }
    }
}

impl Error for ConnectError {}

fn print_help() {
    println!(
        "deerflow-acp - native stdio bridge for the local DeerFlow ACP daemon\n\n\
Usage:\n  deerflow-acp [--config PATH] [--daemon PATH] [--no-auto-start]\n  \
deerflow-acp --protocol v2 [--config PATH] [--daemon PATH]\n  \
deerflow-acp --status [--config PATH]\n  deerflow-acp --start-daemon [--config PATH]\n  \
deerflow-acp --stop-daemon [--config PATH]\n  \
deerflow-acp --manage [--config PATH]\n  \
deerflow-acp --gateway --workspace PATH [--listen ADDR] [--config PATH]\n  \
deerflow-acp --remote URL [--token-env NAME]\n\n\
Options:\n  --config PATH       DeerFlow config.yaml used when starting the daemon\n  \
--protocol VERSION  ACP wire protocol for stdio proxy mode: v1 (default) or v2\n  \
--daemon PATH       Eino Go deerflow-acpd executable (default: sibling file)\n  --runtime-dir PATH  Override daemon endpoint directory\n  \
--mcp-allow-command PATH  Host-authorized absolute MCP executable for Go daemon (repeatable)\n  \
--no-auto-start      Fail instead of starting a missing daemon\n  --status             Check daemon status\n  \
--start-daemon       Start the daemon without entering ACP proxy mode\n  --stop-daemon        Stop the daemon\n  \
--manage             Send one JSON management request from stdin\n  \
--gateway           Expose the local daemon through ACP HTTP + SSE\n  \
--listen ADDR       Gateway listen address (default 127.0.0.1:8787)\n  \
--workspace PATH    Fixed local workspace used by remote ACP sessions\n  \
--gateway-path PATH ACP HTTP endpoint path (default /acp)\n  \
--remote URL        Bridge stdio to a remote ACP HTTP + SSE endpoint\n  \
--token-env NAME    Environment variable containing the fixed gateway token\n  \
-h, --help           Show this help\n  -V, --version        Show bridge version"
    );
}

fn push_mcp_allow_command(commands: &mut Vec<PathBuf>, command: PathBuf) -> Result<()> {
    if !command.is_absolute() || !command.is_file() || commands.len() >= 32 {
        return Err(
            "--mcp-allow-command requires an existing absolute file; at most 32 are allowed".into(),
        );
    }
    commands.push(command);
    Ok(())
}

fn parse_cli() -> Result<Cli> {
    parse_args(env::args_os().skip(1))
}

fn parse_args(mut args: impl Iterator<Item = OsString>) -> Result<Cli> {
    let mut mode = Mode::Proxy;
    let mut protocol = AcpProtocol::V1;
    let mut config = None;
    let mut daemon = None;
    let mut mcp_allow_commands = Vec::new();
    let mut runtime_dir = None;
    let mut auto_start = true;
    let mut gateway_listen = "127.0.0.1:8787".to_owned();
    let mut gateway_workspace = None;
    let mut gateway_path = "/acp".to_owned();
    let mut remote_url = None;
    let mut token_env = "DEER_FLOW_ACP_GATEWAY_TOKEN".to_owned();
    while let Some(raw) = args.next() {
        let value = raw.to_string_lossy();
        match value.as_ref() {
            "--protocol" => {
                let version = args
                    .next()
                    .ok_or("--protocol requires v1 or v2")?
                    .to_string_lossy()
                    .into_owned();
                protocol = match version.to_ascii_lowercase().as_str() {
                    "1" | "v1" => AcpProtocol::V1,
                    "2" | "v2" => AcpProtocol::V2,
                    _ => return Err(format!("unsupported ACP protocol: {version}").into()),
                };
            }
            "--config" => {
                config = Some(PathBuf::from(
                    args.next().ok_or("--config requires a path")?,
                ));
            }
            value if value == "--python" || value.starts_with("--python=") => {
                return Err("--python has been removed; use the Eino Go daemon with --daemon PATH or install deerflow-acpd beside the Bridge".into());
            }
            "--daemon" => {
                daemon = Some(PathBuf::from(
                    args.next().ok_or("--daemon requires a path")?,
                ));
            }
            "--mcp-allow-command" => {
                push_mcp_allow_command(
                    &mut mcp_allow_commands,
                    PathBuf::from(args.next().ok_or("--mcp-allow-command requires a path")?),
                )?;
            }
            "--runtime-dir" => {
                runtime_dir = Some(PathBuf::from(
                    args.next().ok_or("--runtime-dir requires a path")?,
                ));
            }
            "--no-auto-start" => auto_start = false,
            "--status" => set_mode(&mut mode, Mode::Status)?,
            "--start-daemon" => set_mode(&mut mode, Mode::Start)?,
            "--stop-daemon" => set_mode(&mut mode, Mode::Stop)?,
            "--manage" => set_mode(&mut mode, Mode::Manage)?,
            "--gateway" => set_mode(&mut mode, Mode::Gateway)?,
            "--listen" => {
                gateway_listen = args
                    .next()
                    .ok_or("--listen requires an address")?
                    .to_string_lossy()
                    .into_owned();
            }
            "--workspace" => {
                gateway_workspace = Some(PathBuf::from(
                    args.next().ok_or("--workspace requires a path")?,
                ));
            }
            "--gateway-path" => {
                gateway_path = args
                    .next()
                    .ok_or("--gateway-path requires a path")?
                    .to_string_lossy()
                    .into_owned();
            }
            "--remote" => {
                set_mode(&mut mode, Mode::Remote)?;
                remote_url = Some(
                    args.next()
                        .ok_or("--remote requires an ACP endpoint URL")?
                        .to_string_lossy()
                        .into_owned(),
                );
            }
            "--token-env" => {
                token_env = args
                    .next()
                    .ok_or("--token-env requires an environment variable name")?
                    .to_string_lossy()
                    .into_owned();
            }
            "-h" | "--help" => {
                print_help();
                std::process::exit(0);
            }
            "-V" | "--version" => {
                println!("deerflow-acp bridge {}", env!("CARGO_PKG_VERSION"));
                std::process::exit(0);
            }
            _ => return Err(format!("unknown argument: {value}").into()),
        }
    }
    Ok(Cli {
        mode,
        protocol,
        config,
        daemon,
        mcp_allow_commands,
        runtime_dir,
        auto_start,
        gateway_listen,
        gateway_workspace,
        gateway_path,
        remote_url,
        token_env,
    })
}

fn set_mode(current: &mut Mode, requested: Mode) -> Result<()> {
    if *current != Mode::Proxy {
        return Err("only one bridge mode may be selected".into());
    }
    *current = requested;
    Ok(())
}

fn portable_root() -> Option<PathBuf> {
    let executable = env::current_exe().ok()?;
    portable_root_for(&executable)
}

fn portable_root_for(executable: &Path) -> Option<PathBuf> {
    let root = executable.parent()?;
    let portable_marker = root.join("resources").join("default-config.yaml").is_file()
        || root
            .join("user-data")
            .join("config")
            .join("config.yaml")
            .is_file();
    portable_marker.then(|| root.to_path_buf())
}

fn portable_config_path() -> Option<PathBuf> {
    let path = portable_root()?
        .join("user-data")
        .join("config")
        .join("config.yaml");
    path.is_file().then_some(path)
}

fn runtime_dir(override_path: Option<&Path>) -> Result<PathBuf> {
    if let Some(path) = override_path {
        return absolute_path(path);
    }
    if let Some(value) = env::var_os("DEER_FLOW_ACP_RUNTIME_DIR") {
        return absolute_path(Path::new(&value));
    }
    if let Some(root) = portable_root() {
        return Ok(root.join("user-data").join("runtime").join("acp"));
    }
    if let Some(value) = env::var_os("LOCALAPPDATA") {
        return Ok(PathBuf::from(value).join("DeerFlow").join("acp"));
    }
    if let Some(value) = env::var_os("XDG_RUNTIME_DIR") {
        return Ok(PathBuf::from(value).join("deerflow-acp"));
    }
    if let Some(value) = env::var_os("XDG_CACHE_HOME") {
        return Ok(PathBuf::from(value).join("deerflow").join("acp"));
    }
    let home = env::var_os("HOME").ok_or("cannot determine per-user ACP runtime directory")?;
    Ok(PathBuf::from(home)
        .join(".cache")
        .join("deerflow")
        .join("acp"))
}

fn absolute_path(path: &Path) -> Result<PathBuf> {
    if path.is_absolute() {
        Ok(path.to_path_buf())
    } else {
        Ok(env::current_dir()?.join(path))
    }
}

fn strip_windows_verbatim_prefix(path: PathBuf) -> PathBuf {
    #[cfg(windows)]
    {
        let value = path.to_string_lossy();
        if let Some(stripped) = value.strip_prefix(r"\\?\UNC\") {
            return PathBuf::from(format!(r"\\{stripped}"));
        }
        if let Some(stripped) = value.strip_prefix(r"\\?\") {
            return PathBuf::from(stripped);
        }
    }
    path
}

fn endpoint_path(runtime_dir: &Path) -> PathBuf {
    runtime_dir.join(ENDPOINT_FILENAME)
}

pub(crate) fn load_endpoint(path: &Path) -> Result<Endpoint> {
    let content = fs::read_to_string(path)?;
    Ok(serde_json::from_str(&content)?)
}

pub(crate) fn gateway_token(name: &str) -> Result<String> {
    let token = env::var(name).map_err(|_| format!("{name} is not set"))?;
    if token.len() < 32 {
        return Err(format!("{name} must contain at least 32 characters").into());
    }
    if token.chars().any(char::is_whitespace) {
        return Err(format!("{name} must not contain whitespace").into());
    }
    Ok(token)
}

fn normalize_path(path: &Path) -> Result<String> {
    let canonical = match fs::canonicalize(path) {
        Ok(value) => value,
        Err(_) => absolute_path(path)?,
    };
    let mut value = strip_windows_verbatim_prefix(canonical)
        .to_string_lossy()
        .into_owned();
    if cfg!(windows) {
        value.make_ascii_lowercase();
        value = value.replace('/', "\\");
    }
    Ok(value)
}

fn validate_config(endpoint: &Endpoint, requested: Option<&Path>) -> Result<()> {
    let Some(requested) = requested else {
        return Ok(());
    };
    if normalize_path(Path::new(&endpoint.config_path))? != normalize_path(requested)? {
        return Err(format!(
            "the running DeerFlow ACP daemon uses a different config: {} (requested {})",
            endpoint.config_path,
            requested.display()
        )
        .into());
    }
    Ok(())
}

fn socket_address(endpoint: &Endpoint) -> Result<SocketAddr> {
    format!("{}:{}", endpoint.host, endpoint.port)
        .to_socket_addrs()?
        .next()
        .ok_or_else(|| "daemon endpoint did not resolve to a socket address".into())
}

fn read_line(stream: &mut TcpStream, limit: usize) -> io::Result<String> {
    let mut output = Vec::new();
    let mut byte = [0_u8; 1];
    while output.len() < limit {
        let read = stream.read(&mut byte)?;
        if read == 0 || byte[0] == b'\n' {
            break;
        }
        output.push(byte[0]);
    }
    if output.len() == limit {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "daemon handshake response is too long",
        ));
    }
    String::from_utf8(output)
        .map(|value| value.trim().to_owned())
        .map_err(|error| io::Error::new(io::ErrorKind::InvalidData, error))
}

fn connect_command(
    endpoint: &Endpoint,
    command: &str,
) -> std::result::Result<(TcpStream, String), ConnectError> {
    let address =
        socket_address(endpoint).map_err(|error| ConnectError::Unavailable(error.to_string()))?;
    let mut stream = TcpStream::connect_timeout(&address, CONNECT_TIMEOUT)
        .map_err(|error| ConnectError::Unavailable(error.to_string()))?;
    let _ = stream.set_nodelay(true);
    let _ = stream.set_read_timeout(Some(HANDSHAKE_TIMEOUT));
    let request = format!("{HANDSHAKE_VERSION} {} {command}\n", endpoint.token);
    stream
        .write_all(request.as_bytes())
        .and_then(|()| stream.flush())
        .map_err(|error| ConnectError::Unavailable(error.to_string()))?;
    let response = read_line(&mut stream, 4096)
        .map_err(|error| ConnectError::Unavailable(error.to_string()))?;
    let _ = stream.set_read_timeout(None);
    if response == "OK" || response.starts_with("OK ") {
        Ok((stream, response))
    } else {
        Err(ConnectError::Rejected(if response.is_empty() {
            "daemon closed the connection during handshake".to_owned()
        } else {
            response
        }))
    }
}

fn status(endpoint: &Endpoint) -> std::result::Result<String, ConnectError> {
    connect_command(endpoint, "STATUS").map(|(_stream, response)| response)
}

fn endpoint_is_live(endpoint: &Endpoint) -> bool {
    status(endpoint).is_ok()
}

fn find_daemon(cli: &Cli) -> Result<PathBuf> {
    let configured = env::var_os("DEER_FLOW_ACP_DAEMON").map(PathBuf::from);
    let executable = env::current_exe()?;
    resolve_daemon(cli.daemon.as_deref(), configured.as_deref(), &executable)
}

fn resolve_daemon(
    requested: Option<&Path>,
    configured: Option<&Path>,
    executable: &Path,
) -> Result<PathBuf> {
    // Resolve explicit paths before spawn changes cwd to the config directory.
    // Only the installed sibling is discovered automatically. Searching PATH
    // can pick up obsolete entry-point wrappers from another installation.
    let path = if let Some(path) = requested.or(configured) {
        absolute_path(path)?
    } else {
        executable
            .parent()
            .ok_or("cannot locate the Bridge directory; supply --daemon PATH")?
            .join(if cfg!(windows) {
                "deerflow-acpd.exe"
            } else {
                "deerflow-acpd"
            })
    };
    if !path.is_file() {
        return Err(format!(
            "Eino Go daemon executable is missing: {}; install deerflow-acpd beside the Bridge or supply --daemon PATH (DEER_FLOW_ACP_DAEMON also accepts a file path)",
            path.display()
        )
        .into());
    }
    Ok(path)
}

fn mcp_daemon_arguments(allowed: &[PathBuf]) -> Vec<OsString> {
    let mut arguments = Vec::with_capacity(allowed.len() * 2);
    for command in allowed {
        arguments.push(OsString::from("--mcp-allow-command"));
        arguments.push(command.as_os_str().to_owned());
    }
    arguments
}

fn spawn_daemon(cli: &Cli, runtime_dir: &Path) -> Result<()> {
    let daemon = find_daemon(cli)?;
    fs::create_dir_all(runtime_dir)?;
    let mut args = Vec::new();
    let mut cwd = None;
    if let Some(config) = &cli.config {
        args.push(OsString::from("--config"));
        args.push(config.as_os_str().to_owned());
        cwd = config.parent();
    }
    args.extend(mcp_daemon_arguments(&cli.mcp_allow_commands));
    args.push(OsString::from("--runtime-dir"));
    args.push(runtime_dir.as_os_str().to_owned());
    spawn_detached(&daemon, &args, cwd).map_err(|error| {
        format!(
            "failed to start Eino Go daemon {}: {error}",
            daemon.display()
        )
    })?;
    Ok(())
}

#[cfg(windows)]
fn spawn_detached(program: &Path, args: &[OsString], cwd: Option<&Path>) -> io::Result<()> {
    windows_daemon::spawn(program, args, cwd)
}

#[cfg(not(windows))]
fn spawn_detached(program: &Path, args: &[OsString], cwd: Option<&Path>) -> io::Result<()> {
    let mut command = Command::new(program);
    command
        .args(args)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    if let Some(cwd) = cwd {
        command.current_dir(cwd);
    }
    command.spawn().map(|_child| ())
}

fn start_timeout() -> Duration {
    env::var("DEER_FLOW_ACP_DAEMON_START_TIMEOUT_MS")
        .ok()
        .and_then(|value| value.parse::<u64>().ok())
        .map(Duration::from_millis)
        .unwrap_or(DEFAULT_START_TIMEOUT)
}

fn stop_timeout() -> Duration {
    env::var("DEER_FLOW_ACP_DAEMON_STOP_TIMEOUT_MS")
        .ok()
        .and_then(|value| value.parse::<u64>().ok())
        .map(Duration::from_millis)
        .unwrap_or(DEFAULT_STOP_TIMEOUT)
}

#[cfg(windows)]
fn wait_for_process_exit(pid: u32, timeout: Duration) -> io::Result<bool> {
    use std::ffi::c_void;

    const SYNCHRONIZE: u32 = 0x0010_0000;
    const WAIT_OBJECT_0: u32 = 0x0000_0000;
    const WAIT_TIMEOUT: u32 = 0x0000_0102;
    const WAIT_FAILED: u32 = 0xffff_ffff;
    const ERROR_INVALID_PARAMETER: i32 = 87;

    #[link(name = "kernel32")]
    unsafe extern "system" {
        fn OpenProcess(access: u32, inherit_handle: i32, process_id: u32) -> *mut c_void;
        fn WaitForSingleObject(handle: *mut c_void, milliseconds: u32) -> u32;
        fn CloseHandle(handle: *mut c_void) -> i32;
    }

    let handle = unsafe { OpenProcess(SYNCHRONIZE, 0, pid) };
    if handle.is_null() {
        let error = io::Error::last_os_error();
        return match error.raw_os_error() {
            Some(ERROR_INVALID_PARAMETER) => Ok(true),
            _ => Err(error),
        };
    }
    let milliseconds = timeout.as_millis().min(u128::from(u32::MAX - 1)) as u32;
    let result = unsafe { WaitForSingleObject(handle, milliseconds) };
    unsafe {
        CloseHandle(handle);
    }
    match result {
        WAIT_OBJECT_0 => Ok(true),
        WAIT_TIMEOUT => Ok(false),
        WAIT_FAILED => Err(io::Error::last_os_error()),
        value => Err(io::Error::other(format!(
            "unexpected process wait result: {value}"
        ))),
    }
}

#[cfg(windows)]
fn force_terminate_process(pid: u32) -> io::Result<()> {
    use std::ffi::c_void;

    const PROCESS_TERMINATE: u32 = 0x0000_0001;
    const ERROR_INVALID_PARAMETER: i32 = 87;

    #[link(name = "kernel32")]
    unsafe extern "system" {
        fn OpenProcess(access: u32, inherit_handle: i32, process_id: u32) -> *mut c_void;
        fn TerminateProcess(process: *mut c_void, exit_code: u32) -> i32;
        fn CloseHandle(handle: *mut c_void) -> i32;
    }

    let handle = unsafe { OpenProcess(PROCESS_TERMINATE, 0, pid) };
    if handle.is_null() {
        let error = io::Error::last_os_error();
        return match error.raw_os_error() {
            Some(ERROR_INVALID_PARAMETER) => Ok(()),
            _ => Err(error),
        };
    }
    let terminated = unsafe { TerminateProcess(handle, 1) };
    let error = if terminated == 0 {
        Some(io::Error::last_os_error())
    } else {
        None
    };
    unsafe {
        CloseHandle(handle);
    }
    match error {
        Some(error) => Err(error),
        None => Ok(()),
    }
}

#[cfg(unix)]
fn process_is_running(pid: u32) -> io::Result<bool> {
    const ESRCH: i32 = 3;

    unsafe extern "C" {
        fn kill(pid: i32, signal: i32) -> i32;
    }

    if unsafe { kill(pid as i32, 0) } == 0 {
        return Ok(true);
    }
    let error = io::Error::last_os_error();
    match error.raw_os_error() {
        Some(ESRCH) => Ok(false),
        _ => Err(error),
    }
}

#[cfg(unix)]
fn wait_for_process_exit(pid: u32, timeout: Duration) -> io::Result<bool> {
    let deadline = Instant::now() + timeout;
    while process_is_running(pid)? && Instant::now() < deadline {
        thread::sleep(Duration::from_millis(50));
    }
    Ok(!process_is_running(pid)?)
}

#[cfg(unix)]
fn force_terminate_process(pid: u32) -> io::Result<()> {
    const ESRCH: i32 = 3;
    const SIGKILL: i32 = 9;

    unsafe extern "C" {
        fn kill(pid: i32, signal: i32) -> i32;
    }

    if unsafe { kill(pid as i32, SIGKILL) } == 0 {
        return Ok(());
    }
    let error = io::Error::last_os_error();
    match error.raw_os_error() {
        Some(ESRCH) => Ok(()),
        _ => Err(error),
    }
}

fn wait_for_status(cli: &Cli, path: &Path) -> Result<Endpoint> {
    let deadline = Instant::now() + start_timeout();
    let mut last_error = String::from("endpoint has not been published");
    while Instant::now() < deadline {
        if let Ok(endpoint) = load_endpoint(path) {
            match validate_config(&endpoint, cli.config.as_deref()) {
                Ok(()) => match status(&endpoint) {
                    Ok(_) => return Ok(endpoint),
                    Err(error) => last_error = error.to_string(),
                },
                Err(error) => last_error = error.to_string(),
            }
        }
        thread::sleep(Duration::from_millis(50));
    }
    Err(format!("timed out waiting for DeerFlow ACP daemon: {last_error}").into())
}

fn ensure_running(cli: &Cli, path: &Path) -> Result<Endpoint> {
    if let Ok(endpoint) = load_endpoint(path)
        && endpoint_is_live(&endpoint)
    {
        validate_config(&endpoint, cli.config.as_deref())?;
        return Ok(endpoint);
    }
    if !cli.auto_start && cli.mode != Mode::Start {
        return Err("DeerFlow ACP daemon is not running".into());
    }
    spawn_daemon(cli, path.parent().ok_or("invalid endpoint path")?)?;
    wait_for_status(cli, path)
}

fn connect_proxy(cli: &Cli, path: &Path) -> Result<TcpStream> {
    let endpoint = ensure_running(cli, path)?;
    match connect_command(&endpoint, "ACP") {
        Ok((stream, _)) => Ok(stream),
        Err(ConnectError::Rejected(message)) if message == "BUSY" => {
            Err("DeerFlow ACP daemon has reached its connection limit".into())
        }
        Err(error) => Err(format!("failed to connect to DeerFlow ACP daemon: {error}").into()),
    }
}

fn proxy(mut stream: TcpStream) -> Result<()> {
    let mut upload = stream.try_clone()?;
    thread::spawn(move || {
        let mut input = io::stdin().lock();
        let _ = io::copy(&mut input, &mut upload);
        let _ = upload.shutdown(Shutdown::Write);
    });

    let mut output = io::stdout().lock();
    let mut buffer = [0_u8; 16 * 1024];
    loop {
        match stream.read(&mut buffer) {
            Ok(0) => break,
            Ok(size) => {
                output.write_all(&buffer[..size])?;
                output.flush()?;
            }
            Err(error) if error.kind() == io::ErrorKind::Interrupted => continue,
            Err(error) => return Err(error.into()),
        }
    }
    Ok(())
}

fn management_request(endpoint: &Endpoint, request: &[u8]) -> Result<String> {
    if request.is_empty() {
        return Err("management request is empty".into());
    }
    if request.len() > MANAGEMENT_REQUEST_LIMIT {
        return Err("management request exceeds the 64 KiB limit".into());
    }
    let value: serde_json::Value = serde_json::from_slice(request)
        .map_err(|error| format!("invalid management request JSON: {error}"))?;
    if !value.is_object() {
        return Err("management request must be a JSON object".into());
    }

    let (mut stream, _) = connect_command(endpoint, "MANAGE")
        .map_err(|error| format!("failed to connect to DeerFlow ACP daemon: {error}"))?;
    stream.set_read_timeout(Some(MANAGEMENT_TIMEOUT))?;
    stream.set_write_timeout(Some(MANAGEMENT_TIMEOUT))?;
    stream.write_all(request)?;
    stream.write_all(b"\n")?;
    stream.flush()?;
    let response = read_line(&mut stream, MANAGEMENT_RESPONSE_LIMIT)?;
    if response.is_empty() {
        return Err("daemon returned an empty management response".into());
    }
    serde_json::from_str::<serde_json::Value>(&response)
        .map_err(|error| format!("daemon returned invalid management JSON: {error}"))?;
    Ok(response)
}

fn manage(cli: &Cli, path: &Path) -> Result<()> {
    let mut request = Vec::new();
    io::stdin()
        .lock()
        .take((MANAGEMENT_REQUEST_LIMIT + 1) as u64)
        .read_to_end(&mut request)?;
    if request.len() > MANAGEMENT_REQUEST_LIMIT {
        return Err("management request exceeds the 64 KiB limit".into());
    }
    while request
        .last()
        .is_some_and(|byte| byte.is_ascii_whitespace())
    {
        request.pop();
    }

    let endpoint = load_endpoint(path).map_err(|_| "DeerFlow ACP daemon is not running")?;
    validate_config(&endpoint, cli.config.as_deref())?;
    println!("{}", management_request(&endpoint, &request)?);
    Ok(())
}

async fn run() -> Result<()> {
    let mut cli = parse_cli()?;
    if cli.config.is_none() {
        cli.config = portable_config_path();
    }
    if let Some(config) = &cli.config
        && !config.is_file()
    {
        return Err(format!("config file does not exist: {}", config.display()).into());
    }
    if let Some(config) = &cli.config {
        // std::fs::canonicalize returns a Windows verbatim path (\\?\D:\...)
        // on Windows.  That form is useful to Win32 APIs but leaks through the
        // daemon environment into bash's WSL working-directory wrapper, where
        // it is not a valid Linux path.
        cli.config = Some(strip_windows_verbatim_prefix(fs::canonicalize(config)?));
    }
    match cli.mode {
        Mode::Remote => {
            let url = cli
                .remote_url
                .as_deref()
                .ok_or("--remote requires an ACP endpoint URL")?;
            remote::run(url, &cli.token_env).await
        }
        Mode::Proxy | Mode::Status | Mode::Start | Mode::Stop | Mode::Manage | Mode::Gateway => {
            let runtime_dir = runtime_dir(cli.runtime_dir.as_deref())?;
            let path = endpoint_path(&runtime_dir);
            run_local_mode(&cli, &path).await
        }
    }
}

async fn run_local_mode(cli: &Cli, path: &Path) -> Result<()> {
    match cli.mode {
        Mode::Proxy => match cli.protocol {
            AcpProtocol::V1 => proxy(connect_proxy(cli, path)?),
            AcpProtocol::V2 => {
                ensure_running(cli, path)?;
                v2::run(path).await
            }
        },
        Mode::Status => {
            let endpoint = load_endpoint(path).map_err(|_| "DeerFlow ACP daemon is not running")?;
            validate_config(&endpoint, cli.config.as_deref())?;
            let response = status(&endpoint).map_err(|error| error.to_string())?;
            println!(
                "running pid={} build={} endpoint={}:{} ({response})",
                endpoint.pid, endpoint.build_id, endpoint.host, endpoint.port
            );
            Ok(())
        }
        Mode::Start => {
            let endpoint = ensure_running(cli, path)?;
            println!(
                "running pid={} build={} endpoint={}:{}",
                endpoint.pid, endpoint.build_id, endpoint.host, endpoint.port
            );
            Ok(())
        }
        Mode::Stop => {
            let endpoint = load_endpoint(path).map_err(|_| "DeerFlow ACP daemon is not running")?;
            validate_config(&endpoint, cli.config.as_deref())?;
            connect_command(&endpoint, "STOP").map_err(|error| error.to_string())?;
            if !wait_for_process_exit(endpoint.pid, stop_timeout())? {
                eprintln!(
                    "deerflow-acp: daemon pid={} did not exit after STOP; forcing termination",
                    endpoint.pid
                );
                force_terminate_process(endpoint.pid)?;
                if !wait_for_process_exit(endpoint.pid, FORCED_STOP_TIMEOUT)? {
                    return Err(format!(
                        "daemon pid={} did not exit after forced termination",
                        endpoint.pid
                    )
                    .into());
                }
            }
            if let Ok(current) = load_endpoint(path)
                && current.pid == endpoint.pid
                && current.token == endpoint.token
            {
                fs::remove_file(path)?;
            }
            println!("stopped");
            Ok(())
        }
        Mode::Manage => manage(cli, path),
        Mode::Gateway => {
            ensure_running(cli, path)?;
            let workspace = cli
                .gateway_workspace
                .as_deref()
                .ok_or("--gateway requires --workspace PATH")?;
            fs::create_dir_all(workspace)?;
            let workspace = strip_windows_verbatim_prefix(fs::canonicalize(workspace)?);
            let listen = cli
                .gateway_listen
                .parse::<SocketAddr>()
                .map_err(|error| format!("invalid --listen address: {error}"))?;
            if !cli.gateway_path.starts_with('/') {
                return Err("--gateway-path must begin with /".into());
            }
            gateway::run(
                listen,
                path.to_path_buf(),
                workspace,
                gateway_token(&cli.token_env)?,
                cli.gateway_path.clone(),
            )
            .await
        }
        Mode::Remote => unreachable!("remote mode is handled before local endpoint discovery"),
    }
}

#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!("deerflow-acp: {error}");
        std::process::exit(1);
    }
}

#[cfg(test)]
mod process_tests {
    use super::*;

    #[test]
    fn host_mcp_allowlist_preserves_literal_paths() {
        let command = PathBuf::from(if cfg!(windows) {
            r"C:\MCP\server.exe"
        } else {
            "/opt/mcp/server"
        });
        assert_eq!(
            mcp_daemon_arguments(&[command.clone()]),
            vec![
                OsString::from("--mcp-allow-command"),
                command.into_os_string()
            ]
        );
        assert!(mcp_daemon_arguments(&[]).is_empty());
    }

    #[test]
    fn host_mcp_allowlist_rejects_relative_and_missing_commands() {
        let temporary = tempfile::tempdir().unwrap();
        let allowed = temporary.path().join("server");
        fs::write(&allowed, b"fixture").unwrap();
        let mut commands = Vec::new();
        assert!(push_mcp_allow_command(&mut commands, PathBuf::from("server")).is_err());
        assert!(push_mcp_allow_command(&mut commands, temporary.path().join("missing")).is_err());
        push_mcp_allow_command(&mut commands, allowed).unwrap();
        assert_eq!(commands.len(), 1);
    }

    #[test]
    fn current_process_is_not_reported_as_exited() {
        assert!(!wait_for_process_exit(std::process::id(), Duration::ZERO).unwrap());
    }

    #[test]
    fn portable_layout_uses_executable_directory() {
        let temporary = tempfile::tempdir().unwrap();
        let root = temporary.path();
        fs::create_dir_all(root.join("resources")).unwrap();
        fs::write(
            root.join("resources").join("default-config.yaml"),
            b"models: []\n",
        )
        .unwrap();
        assert_eq!(
            portable_root_for(&root.join("deerflow-acp.exe")),
            Some(root.to_path_buf())
        );
    }

    #[test]
    fn packaged_go_daemon_ignores_legacy_layouts_and_refuses_missing_sibling() {
        let temporary = tempfile::tempdir().unwrap();
        let root = temporary.path();
        let bundle = root.join("bundle");
        let venv = if cfg!(windows) {
            root.join(".venv").join("Scripts").join("python.exe")
        } else {
            root.join(".venv").join("bin").join("python3")
        };
        fs::create_dir_all(&bundle).unwrap();
        fs::create_dir_all(venv.parent().unwrap()).unwrap();
        fs::write(&venv, b"python").unwrap();
        let bridge = bundle.join(if cfg!(windows) {
            "deerflow-acp.exe"
        } else {
            "deerflow-acp"
        });
        let sibling = bundle.join(if cfg!(windows) {
            "deerflow-acpd.exe"
        } else {
            "deerflow-acpd"
        });
        fs::write(&sibling, b"go daemon").unwrap();
        assert_eq!(resolve_daemon(None, None, &bridge).unwrap(), sibling);

        let bundled_python = if cfg!(windows) {
            bundle.join("runtime").join("python.exe")
        } else {
            bundle.join("runtime").join("bin").join("python3")
        };
        fs::create_dir_all(bundled_python.parent().unwrap()).unwrap();
        fs::write(&bundled_python, b"python").unwrap();
        assert_eq!(resolve_daemon(None, None, &bridge).unwrap(), sibling);
        assert_eq!(portable_root_for(&bridge), None);
        fs::remove_file(&sibling).unwrap();
        let error = resolve_daemon(None, None, &bridge).unwrap_err().to_string();
        assert!(error.contains("Eino Go daemon executable is missing"));
        assert!(error.contains(&sibling.display().to_string()));
    }

    #[test]
    fn explicit_daemon_precedence_does_not_fall_back_after_missing_override() {
        let temporary = tempfile::tempdir().unwrap();
        let root = temporary.path();
        let cli = root.join("cli-daemon");
        let configured = root.join("env-daemon");
        fs::write(&cli, b"fixture").unwrap();
        fs::write(&configured, b"fixture").unwrap();
        let bridge = root.join("deerflow-acp");
        assert_eq!(
            resolve_daemon(Some(&cli), Some(&configured), &bridge).unwrap(),
            cli
        );
        assert_eq!(
            resolve_daemon(None, Some(&configured), &bridge).unwrap(),
            configured
        );
        fs::remove_file(&cli).unwrap();
        assert!(resolve_daemon(Some(&cli), Some(&configured), &bridge).is_err());
        assert!(resolve_daemon(None, Some(root), &bridge).is_err());
    }

    #[test]
    fn removed_python_flag_gives_migration_error() {
        for args in [
            vec!["--python", "old-runtime"],
            vec!["--python=old-runtime"],
            vec!["--python"],
        ] {
            let error = parse_args(args.into_iter().map(OsString::from))
                .unwrap_err()
                .to_string();
            assert!(error.contains("--python has been removed"));
            assert!(error.contains("--daemon PATH"));
        }
    }

    #[test]
    fn management_request_roundtrips_json_and_large_response() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let server = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let handshake = read_line(&mut stream, 4096).unwrap();
            assert_eq!(handshake, "DFACP/1 secret MANAGE");
            stream.write_all(b"OK\n").unwrap();
            let request = read_line(&mut stream, MANAGEMENT_REQUEST_LIMIT).unwrap();
            assert_eq!(
                serde_json::from_str::<serde_json::Value>(&request).unwrap(),
                serde_json::json!({"operation": "proposal.list"})
            );
            let response = serde_json::json!({
                "ok": true,
                "data": {"diff": "x".repeat(128 * 1024)},
            });
            writeln!(stream, "{response}").unwrap();
        });
        let endpoint = Endpoint {
            host: address.ip().to_string(),
            port: address.port(),
            token: "secret".into(),
            pid: 1,
            build_id: "test".into(),
            config_path: "config.yaml".into(),
        };

        let response = management_request(&endpoint, br#"{"operation":"proposal.list"}"#).unwrap();
        let parsed: serde_json::Value = serde_json::from_str(&response).unwrap();
        assert_eq!(parsed["data"]["diff"].as_str().unwrap().len(), 128 * 1024);
        server.join().unwrap();
    }
}

#[cfg(all(test, windows))]
mod tests {
    use super::strip_windows_verbatim_prefix;
    use std::path::PathBuf;

    #[test]
    fn strips_verbatim_drive_prefix() {
        assert_eq!(
            strip_windows_verbatim_prefix(PathBuf::from(r"\\?\D:\Tools\deerflow-api")),
            PathBuf::from(r"D:\Tools\deerflow-api")
        );
    }

    #[test]
    fn converts_verbatim_unc_prefix() {
        assert_eq!(
            strip_windows_verbatim_prefix(PathBuf::from(r"\\?\UNC\server\share\config.yaml")),
            PathBuf::from(r"\\server\share\config.yaml")
        );
    }
}
