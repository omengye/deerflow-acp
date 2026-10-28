//! Desktop ownership of the Waku daemon process.

use std::path::PathBuf;
use std::sync::OnceLock;

use anyhow::{Context as _, anyhow, bail};

pub mod connection;

#[derive(Clone, Default)]
pub struct StartupConnection {
    pub settings: connection::ConnectionSettings,
    pub error: Option<String>,
}

static STARTUP_CONNECTION: OnceLock<StartupConnection> = OnceLock::new();

pub fn startup_connection() -> StartupConnection {
    STARTUP_CONNECTION.get().cloned().unwrap_or_default()
}

pub fn start_process() -> anyhow::Result<waku_client::DaemonSupervisor> {
    let mut startup = StartupConnection::default();
    let saved = connection::ConnectionSettings::default_path().is_file();
    match connection::ConnectionSettings::load() {
        Ok(settings) => startup.settings = settings,
        Err(error) => startup.error = Some(format!("{error:#}")),
    }
    // Saved UI preferences supersede the legacy environment-only launcher.
    // This also lets Save and Restart leave an old SSH-tunnel launch behind.
    let address = std::env::var(waku_client::DAEMON_ADDRESS_ENV)
        .ok()
        .filter(|value| !value.trim().is_empty());
    let token = std::env::var(waku_client::DAEMON_TOKEN_ENV)
        .ok()
        .filter(|value| !value.is_empty());
    if !saved {
        match (address, token) {
            (Some(address), Some(token)) => {
                startup.settings = connection::ConnectionSettings {
                    remote: true,
                    address,
                    token,
                };
            }
            (Some(_), None) | (None, Some(_)) => {
                startup.error = Some("服务器地址和 Token 必须同时设置".into());
            }
            (None, None) => {}
        }
    }
    if startup.settings.remote {
        let remote = startup.settings.validate().and_then(|settings| {
            waku_client::DaemonSupervisor::connect(&settings.address, settings.token)
        });
        match remote {
            Ok(daemon) => {
                let _ = STARTUP_CONNECTION.set(startup);
                return Ok(daemon);
            }
            Err(error) => {
                startup.error = Some(format!("远程连接失败，已进入本地设置以便修复：{error:#}"))
            }
        }
    }
    let _ = STARTUP_CONNECTION.set(startup);
    let app_settings = waku_client::persistence::load_or_create_app_settings()
        .context("could not load desktop daemon settings")?;
    waku_client::DaemonSupervisor::spawn_configured(
        &daemon_executable_path()?,
        cfg!(debug_assertions),
        app_settings.daemon_exposure,
    )
}

/// Runs only on a worker. Authentication plus a settings RPC verifies that
/// this is a compatible DeerFlow endpoint, rather than just an open TCP port.
pub fn test_connection(settings: &connection::ConnectionSettings) -> anyhow::Result<()> {
    let settings = settings.validate()?;
    let client = waku_client::DaemonClient::connect(&settings.address, settings.token)?;
    let result = client.request_with_timeout(
        uuid::Uuid::nil(),
        uuid::Uuid::nil(),
        waku_client::Command::GetSettings,
        std::time::Duration::from_secs(5),
    );
    client.disconnect();
    match result? {
        waku_client::ResponsePayload::Settings { .. } => Ok(()),
        _ => Err(anyhow!("服务器返回了不兼容的设置响应")),
    }
}

/// Resolve the local host name once during app construction. Settings can
/// then show a useful LAN URL without touching the OS from a render frame.
pub fn local_hostname() -> Option<String> {
    #[cfg(unix)]
    {
        let mut buffer = [0_u8; 256];
        let result = unsafe { libc::gethostname(buffer.as_mut_ptr().cast(), buffer.len()) };
        if result == 0 {
            let length = buffer
                .iter()
                .position(|byte| *byte == 0)
                .unwrap_or(buffer.len());
            let hostname = String::from_utf8_lossy(&buffer[..length]).trim().to_owned();
            if !hostname.is_empty() {
                return Some(hostname);
            }
        }
    }
    // `COMPUTERNAME` is the Windows equivalent and is always set; `HOSTNAME`
    // covers the shells that export it.
    ["COMPUTERNAME", "HOSTNAME"]
        .into_iter()
        .filter_map(|name| std::env::var(name).ok())
        .map(|hostname| hostname.trim().to_owned())
        .find(|hostname| !hostname.is_empty())
}

fn daemon_executable_path() -> anyhow::Result<PathBuf> {
    if let Some(path) = std::env::var_os("WAKU_DAEMON_PATH").filter(|path| !path.is_empty()) {
        return Ok(path.into());
    }
    let executable = format!("waku-daemon{}", std::env::consts::EXE_SUFFIX);
    let current = std::env::current_exe().context("could not locate the Waku executable")?;

    // Development keeps the daemon beside Cargo's debug artifacts rather than
    // inside Waku Debug.app. The supervisor watches this file and swaps only
    // the daemon when the development watcher relinks it.
    #[cfg(debug_assertions)]
    if let Some(debug_directory) = current
        .ancestors()
        .find(|candidate| candidate.file_name().is_some_and(|name| name == "debug"))
    {
        let external = debug_directory.join(&executable);
        if external.is_file() {
            return Ok(external);
        }
    }

    let sibling = current
        .parent()
        .map(|directory| directory.join(&executable))
        .ok_or_else(|| anyhow!("Waku executable has no parent directory"))?;
    if sibling.is_file() {
        return Ok(sibling);
    }
    #[cfg(debug_assertions)]
    bail!(
        "Waku daemon was not found in Cargo's debug directory or next to the app executable: {}",
        sibling.display(),
    );
    #[cfg(not(debug_assertions))]
    bail!(
        "Waku daemon is missing next to the app executable: {}",
        sibling.display(),
    )
}
