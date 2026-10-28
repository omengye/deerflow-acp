use std::io::Write as _;
use std::net::{SocketAddr, TcpListener};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use anyhow::{Context as _, anyhow, bail};
use waku_protocol::{DAEMON_TOKEN_ENV, DaemonReady, PROTOCOL_VERSION};

fn main() -> anyhow::Result<()> {
    let authentication_timeout = match std::env::var("WAKU_AUTHENTICATION_TIMEOUT_SECONDS") {
        Ok(value) => Some(value),
        Err(std::env::VarError::NotPresent) => None,
        Err(_) => bail!("WAKU_AUTHENTICATION_TIMEOUT_SECONDS must be valid Unicode"),
    };
    let arguments = Arguments::parse_with_environment(
        std::env::args().skip(1),
        authentication_timeout.as_deref(),
    )?;
    let token =
        std::env::var(DAEMON_TOKEN_ENV).context("Waku daemon authentication token is missing")?;
    // The bearer capability belongs only to this server process. Remove it
    // before any provider or workspace subprocess can inherit the daemon's
    // environment.
    unsafe { std::env::remove_var(DAEMON_TOKEN_ENV) };
    let listener = TcpListener::bind(&arguments.bind)
        .with_context(|| format!("could not bind Waku daemon to {}", arguments.bind))?;
    let address = listener.local_addr()?;
    ensure_bind_allowed(address, arguments.allow_non_loopback)?;
    let ready = DaemonReady {
        address: address.to_string(),
        protocol_version: PROTOCOL_VERSION,
        pid: std::process::id(),
    };
    println!("{}", serde_json::to_string(&ready)?);
    std::io::stdout().flush()?;

    let shutdown = Arc::new(AtomicBool::new(false));
    if let Some(parent_pid) = arguments.parent_pid {
        let monitor_shutdown = shutdown.clone();
        std::thread::Builder::new()
            .name("waku-daemon-parent".into())
            .spawn(move || {
                while !monitor_shutdown.load(Ordering::Acquire) {
                    if !process_is_alive(parent_pid) {
                        monitor_shutdown.store(true, Ordering::Release);
                        break;
                    }
                    std::thread::sleep(Duration::from_millis(500));
                }
            })?;
    }

    let task_path = waku_core::persistence::StateStore::default_path();
    let settings = waku_core::DaemonSettingsStore::open_with_legacy(
        waku_core::DaemonSettings::default_path(),
        [task_path.with_file_name("settings.json")],
    )
    .context("could not load daemon settings")?;
    let task_store = waku_core::persistence::StateStore::daemon(task_path);
    waku_core::serve(
        listener,
        token,
        Arc::new(waku_core::daemon::WakuBackend::new(settings, task_store)?),
        shutdown,
        waku_core::ServerOptions {
            allowed_origins: arguments.allowed_origins.into_iter().collect(),
            allow_shutdown: arguments.parent_pid.is_some(),
            authentication_timeout: arguments.authentication_timeout,
        },
    )
}

fn ensure_bind_allowed(address: SocketAddr, allow_non_loopback: bool) -> anyhow::Result<()> {
    if address.ip().is_loopback() || allow_non_loopback {
        return Ok(());
    }
    bail!(
        "refusing non-loopback daemon bind {address}; pass --allow-non-loopback only after configuring authentication and exact browser origins"
    )
}

struct Arguments {
    bind: String,
    parent_pid: Option<u32>,
    allowed_origins: Vec<String>,
    allow_non_loopback: bool,
    authentication_timeout: Duration,
}

impl Arguments {
    #[cfg(test)]
    fn parse(arguments: impl IntoIterator<Item = String>) -> anyhow::Result<Self> {
        Self::parse_with_environment(arguments, None)
    }

    fn parse_with_environment(
        arguments: impl IntoIterator<Item = String>,
        authentication_timeout_env: Option<&str>,
    ) -> anyhow::Result<Self> {
        let mut bind = "127.0.0.1:0".to_owned();
        let mut parent_pid = None;
        let mut allowed_origins = Vec::new();
        let mut allow_non_loopback = false;
        let mut authentication_timeout = None;
        let mut arguments = arguments.into_iter();
        while let Some(argument) = arguments.next() {
            match argument.as_str() {
                "--bind" => {
                    bind = arguments
                        .next()
                        .ok_or_else(|| anyhow!("--bind requires an address"))?;
                }
                "--parent-pid" => {
                    parent_pid = Some(
                        arguments
                            .next()
                            .ok_or_else(|| anyhow!("--parent-pid requires a process id"))?
                            .parse()
                            .context("--parent-pid is not a valid process id")?,
                    );
                }
                "--allow-origin" => {
                    let origin = arguments
                        .next()
                        .filter(|origin| !origin.trim().is_empty())
                        .ok_or_else(|| anyhow!("--allow-origin requires an origin"))?;
                    allowed_origins.push(origin);
                }
                "--allow-non-loopback" => {
                    allow_non_loopback = true;
                }
                "--authentication-timeout-seconds" => {
                    authentication_timeout = Some(arguments.next().ok_or_else(|| {
                        anyhow!("--authentication-timeout-seconds requires a value")
                    })?);
                }
                "--help" | "-h" => {
                    println!(
                        "usage: {} [--bind ADDRESS] [--allow-non-loopback] [--parent-pid PID] [--authentication-timeout-seconds SECONDS] [--allow-origin ORIGIN]...",
                        env!("CARGO_BIN_NAME")
                    );
                    std::process::exit(0);
                }
                unknown => bail!("unknown argument {unknown:?}"),
            }
        }
        let authentication_timeout = match authentication_timeout
            .as_deref()
            .or(authentication_timeout_env)
        {
            Some(value) => {
                let seconds: u64 = value
                    .parse()
                    .context("authentication timeout requires an integer from 1 to 60 seconds")?;
                let options = waku_core::ServerOptions {
                    authentication_timeout: Duration::from_secs(seconds),
                    ..Default::default()
                };
                options.validate()?;
                options.authentication_timeout
            }
            None => waku_core::ServerOptions::default().authentication_timeout,
        };
        Ok(Self {
            bind,
            parent_pid,
            allowed_origins,
            allow_non_loopback,
            authentication_timeout,
        })
    }
}

#[cfg(unix)]
fn process_is_alive(pid: u32) -> bool {
    let result = unsafe { libc::kill(pid as i32, 0) };
    result == 0 || std::io::Error::last_os_error().raw_os_error() == Some(libc::EPERM)
}

/// Windows reuses process ids, so the handle is opened for the narrowest
/// right that answers the question and closed immediately. A pid that no
/// longer exists fails to open; one that has exited but is still held open by
/// another handle reports an exit code instead of `STILL_ACTIVE`.
#[cfg(windows)]
fn process_is_alive(pid: u32) -> bool {
    use windows_sys::Win32::Foundation::{CloseHandle, STILL_ACTIVE};
    use windows_sys::Win32::System::Threading::{
        GetExitCodeProcess, OpenProcess, PROCESS_QUERY_LIMITED_INFORMATION,
    };

    unsafe {
        let handle = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, 0, pid);
        if handle.is_null() {
            return false;
        }
        let mut exit_code = 0_u32;
        let read = GetExitCodeProcess(handle, &mut exit_code);
        CloseHandle(handle);
        // A failed read leaves the parent's state unknown; outliving the app
        // is the safer error than shutting a live daemon down.
        read == 0 || exit_code == STILL_ACTIVE as u32
    }
}

#[cfg(not(any(unix, windows)))]
fn process_is_alive(_pid: u32) -> bool {
    true
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn non_loopback_listener_requires_an_explicit_flag() {
        assert!(ensure_bind_allowed("127.0.0.1:3000".parse().unwrap(), false).is_ok());
        assert!(ensure_bind_allowed("[::1]:3000".parse().unwrap(), false).is_ok());
        assert!(ensure_bind_allowed("0.0.0.0:3000".parse().unwrap(), false).is_err());
        assert!(ensure_bind_allowed("[::]:3000".parse().unwrap(), false).is_err());
        assert!(ensure_bind_allowed("0.0.0.0:3000".parse().unwrap(), true).is_ok());
    }

    #[test]
    fn parses_repeated_browser_origin_allowlist_entries() {
        let arguments = Arguments::parse([
            "--allow-origin".into(),
            "https://app.waku.test".into(),
            "--allow-origin".into(),
            "http://localhost:3000".into(),
        ])
        .unwrap();

        assert_eq!(
            arguments.allowed_origins,
            ["https://app.waku.test", "http://localhost:3000"]
        );
        assert!(!arguments.allow_non_loopback);
    }

    #[test]
    fn parses_explicit_non_loopback_opt_in() {
        let arguments = Arguments::parse(["--allow-non-loopback".into()]).unwrap();
        assert!(arguments.allow_non_loopback);
    }

    #[test]
    fn authentication_timeout_defaults_to_twenty_seconds_and_accepts_override() {
        let defaults = Arguments::parse([]).unwrap();
        assert_eq!(defaults.authentication_timeout, Duration::from_secs(20));
        for seconds in ["1", "30", "60"] {
            let arguments =
                Arguments::parse(["--authentication-timeout-seconds".into(), seconds.into()])
                    .unwrap();
            assert_eq!(
                arguments.authentication_timeout,
                Duration::from_secs(seconds.parse().unwrap()),
            );
        }
    }

    #[test]
    fn authentication_timeout_rejects_invalid_or_unbounded_configuration() {
        for value in ["0", "61", "-1", "1.5", "invalid", "18446744073709551616"] {
            assert!(
                Arguments::parse(["--authentication-timeout-seconds".into(), value.into(),])
                    .is_err()
            );
        }
        assert!(Arguments::parse(["--authentication-timeout-seconds".into()]).is_err());
    }

    #[test]
    fn authentication_timeout_environment_is_overridden_by_cli() {
        let from_env = Arguments::parse_with_environment([], Some("25")).unwrap();
        assert_eq!(from_env.authentication_timeout, Duration::from_secs(25));
        let from_cli = Arguments::parse_with_environment(
            ["--authentication-timeout-seconds".into(), "30".into()],
            Some("25"),
        )
        .unwrap();
        assert_eq!(from_cli.authentication_timeout, Duration::from_secs(30));
        assert!(Arguments::parse_with_environment([], Some("0")).is_err());
        assert!(Arguments::parse_with_environment([], Some("invalid")).is_err());
        assert!(Arguments::parse_with_environment([], Some("")).is_err());
    }
}
