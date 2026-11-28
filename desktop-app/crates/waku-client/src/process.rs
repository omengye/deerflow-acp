use std::collections::HashSet;
use std::io::{BufRead as _, BufReader};
use std::path::{Path, PathBuf};
use std::process::{Child, Command as ProcessCommand, Stdio};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::mpsc;
use std::time::SystemTime;
use std::time::{Duration, Instant};

use anyhow::{Context as _, bail};
use crossbeam_channel::{Receiver, Sender, unbounded};
use parking_lot::Mutex;
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::DaemonClient;
use waku_protocol::connection_diagnostics::Event;
use waku_protocol::{
    APP_EXECUTABLE_ENV, Command, DAEMON_TOKEN_ENV, DaemonReady, DaemonSettings, PROTOCOL_VERSION,
    ResponsePayload,
};
const START_TIMEOUT: Duration = Duration::from_secs(15);
const SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(1);
const REBUILD_POLL_INTERVAL: Duration = Duration::from_millis(500);
pub const DEFAULT_EXPOSED_DAEMON_PORT: u16 = 34_123;

/// Desktop-owned launch configuration for the daemon it supervises.
///
/// Provider settings belong to the daemon and live in `settings.json`; this
/// is an app preference because it controls how the desktop launches its own
/// child process. The bearer token is intentionally stable across daemon-only
/// rebuilds and desktop relaunches so a configured web client keeps working.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(default)]
pub struct DaemonExposureSettings {
    pub enabled: bool,
    pub port: u16,
    pub allowed_origins: Vec<String>,
    pub token: String,
}

impl Default for DaemonExposureSettings {
    fn default() -> Self {
        Self {
            enabled: false,
            port: DEFAULT_EXPOSED_DAEMON_PORT,
            allowed_origins: vec!["http://localhost:3001".into()],
            token: Self::new_token(),
        }
    }
}

impl DaemonExposureSettings {
    pub fn new_token() -> String {
        Uuid::new_v4().simple().to_string()
    }

    pub fn ensure_token(&mut self) -> bool {
        if !self.token.trim().is_empty() {
            return false;
        }
        self.token = Self::new_token();
        true
    }

    pub fn allowed_origins_text(&self) -> String {
        self.allowed_origins.join(", ")
    }

    pub fn with_allowed_origins_text(mut self, text: &str) -> anyhow::Result<Self> {
        self.allowed_origins = parse_allowed_origins(text)?;
        Ok(self)
    }

    pub fn validate(mut self) -> anyhow::Result<Self> {
        if self.port == 0 {
            bail!("daemon port must be between 1 and 65535");
        }
        if self.token.trim().is_empty() {
            bail!("daemon authentication token is empty");
        }
        self.allowed_origins = parse_allowed_origins(&self.allowed_origins_text())?;
        Ok(self)
    }

    fn bind_address(&self) -> String {
        if self.enabled {
            format!("0.0.0.0:{}", self.port)
        } else {
            "127.0.0.1:0".into()
        }
    }
}

/// Parse the comma-separated exact browser origins edited by the desktop.
/// Browser Origin headers contain only an HTTP(S) origin, never a path.
pub fn parse_allowed_origins(text: &str) -> anyhow::Result<Vec<String>> {
    let mut origins = Vec::new();
    let mut seen = HashSet::new();
    for candidate in text
        .split(',')
        .map(str::trim)
        .filter(|value| !value.is_empty())
    {
        let url = url::Url::parse(candidate)
            .with_context(|| format!("invalid browser origin {candidate:?}"))?;
        if !matches!(url.scheme(), "http" | "https")
            || !url.username().is_empty()
            || url.password().is_some()
            || url.query().is_some()
            || url.fragment().is_some()
            || url.path() != "/"
        {
            bail!(
                "browser origin {candidate:?} must be an exact http:// or https:// origin without a path"
            );
        }
        let origin = url.origin().ascii_serialization();
        if origin == "null" {
            bail!("browser origin {candidate:?} is not a network origin");
        }
        if seen.insert(origin.clone()) {
            origins.push(origin);
        }
    }
    Ok(origins)
}

pub struct DaemonProcess {
    client: DaemonClient,
    address: String,
    token: String,
    child: Child,
}

impl DaemonProcess {
    pub fn spawn(executable: &Path) -> anyhow::Result<Self> {
        Self::spawn_configured(executable, DaemonExposureSettings::default())
    }

    fn spawn_configured(
        executable: &Path,
        settings: DaemonExposureSettings,
    ) -> anyhow::Result<Self> {
        let settings = settings.validate()?;
        let token = settings.token.clone();
        let app_executable = std::env::current_exe().context("could not locate Waku executable")?;
        let mut command = ProcessCommand::new(executable);
        // The desktop is a GUI-subsystem binary on Windows, so a console
        // child would get a console window of its own. `stderr` still reaches
        // the app's inherited handle.
        #[cfg(windows)]
        {
            use std::os::windows::process::CommandExt as _;

            const CREATE_NO_WINDOW: u32 = 0x0800_0000;
            command.creation_flags(CREATE_NO_WINDOW);
        }
        command
            .arg("--bind")
            .arg(settings.bind_address())
            .arg("--parent-pid")
            .arg(std::process::id().to_string());
        if settings.enabled {
            command.arg("--allow-non-loopback");
        }
        for origin in &settings.allowed_origins {
            command.arg("--allow-origin").arg(origin);
        }
        let mut child = command
            .env(DAEMON_TOKEN_ENV, &token)
            .env(APP_EXECUTABLE_ENV, app_executable)
            .env(
                "DEER_FLOW_PORTABLE_ROOT",
                waku_protocol::identity::portable_root(),
            )
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .spawn()
            .with_context(|| format!("could not launch {}", executable.display()))?;
        let stdout = child
            .stdout
            .take()
            .context("Waku daemon did not expose its readiness stream")?;
        let (ready_tx, ready_rx) = mpsc::sync_channel(1);
        std::thread::Builder::new()
            .name("waku-daemon-ready".into())
            .spawn(move || {
                let mut line = String::new();
                let result = BufReader::new(stdout)
                    .read_line(&mut line)
                    .map_err(anyhow::Error::from)
                    .and_then(|bytes| {
                        if bytes == 0 {
                            bail!("Waku daemon exited before becoming ready")
                        }
                        serde_json::from_str::<DaemonReady>(&line).map_err(anyhow::Error::from)
                    });
                let _ = ready_tx.send(result);
            })
            .context("could not start Waku daemon readiness reader")?;
        let ready = match ready_rx.recv_timeout(START_TIMEOUT) {
            Ok(Ok(ready)) => ready,
            Ok(Err(error)) => {
                let _ = child.kill();
                let _ = child.wait();
                return Err(error);
            }
            Err(error) => {
                let _ = child.kill();
                let _ = child.wait();
                bail!("timed out waiting for Waku daemon: {error}");
            }
        };
        if ready.protocol_version != PROTOCOL_VERSION {
            let _ = child.kill();
            let _ = child.wait();
            bail!(
                "daemon protocol {} does not match desktop protocol {}",
                ready.protocol_version,
                PROTOCOL_VERSION
            );
        }
        let client_address = match desktop_client_address(&ready.address) {
            Ok(address) => address,
            Err(error) => {
                let _ = child.kill();
                let _ = child.wait();
                return Err(error);
            }
        };
        let client = match DaemonClient::connect_local_with_resume(
            &client_address,
            token.clone(),
            Vec::new(),
        ) {
            Ok(client) => client,
            Err(error) => {
                let _ = child.kill();
                let _ = child.wait();
                return Err(error);
            }
        };
        Ok(Self {
            client,
            address: client_address,
            token,
            child,
        })
    }

    pub fn client(&self) -> DaemonClient {
        self.client.clone()
    }

    fn stop(&mut self) {
        self.client.shutdown();
        let deadline = Instant::now() + SHUTDOWN_TIMEOUT;
        while Instant::now() < deadline {
            match self.child.try_wait() {
                Ok(Some(_)) => return,
                Ok(None) => std::thread::sleep(Duration::from_millis(25)),
                Err(_) => break,
            }
        }
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

impl Drop for DaemonProcess {
    fn drop(&mut self) {
        self.stop();
    }
}

fn desktop_client_address(address: &str) -> anyhow::Result<String> {
    let address = address
        .parse::<std::net::SocketAddr>()
        .with_context(|| format!("Waku daemon returned an invalid address {address:?}"))?;
    let ip = if address.ip().is_unspecified() {
        if address.is_ipv4() {
            std::net::IpAddr::V4(std::net::Ipv4Addr::LOCALHOST)
        } else {
            std::net::IpAddr::V6(std::net::Ipv6Addr::LOCALHOST)
        }
    } else {
        address.ip()
    };
    Ok(std::net::SocketAddr::new(ip, address.port()).to_string())
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct ExecutableStamp {
    modified: Option<SystemTime>,
    len: u64,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum LocalDaemonRecovery {
    None,
    Reconnect,
    Restart,
}

fn local_daemon_recovery(process_exited: bool, client_disconnected: bool) -> LocalDaemonRecovery {
    if process_exited {
        LocalDaemonRecovery::Restart
    } else if client_disconnected {
        LocalDaemonRecovery::Reconnect
    } else {
        LocalDaemonRecovery::None
    }
}

impl ExecutableStamp {
    fn read(path: &Path) -> anyhow::Result<Self> {
        let metadata = std::fs::metadata(path)
            .with_context(|| format!("could not inspect {}", path.display()))?;
        Ok(Self {
            modified: metadata.modified().ok(),
            len: metadata.len(),
        })
    }
}

struct SupervisorInner {
    executable: Option<PathBuf>,
    target: Mutex<DaemonTarget>,
    exposure: Mutex<Option<DaemonExposureSettings>>,
    restart: Mutex<()>,
    settings: Mutex<DaemonSettings>,
    persisted_settings: Mutex<Option<DaemonSettings>>,
    settings_updates: Sender<DaemonSettings>,
    client_updates: Mutex<Vec<Sender<DaemonClient>>>,
    running: AtomicBool,
    clock_origin: Instant,
    // Milliseconds past clock_origin, plus one. Zero means the transport is ready.
    recovery_started: AtomicU64,
}

enum DaemonTarget {
    Local(DaemonProcess),
    Restarting(DaemonClient),
    Remote {
        client: DaemonClient,
        address: String,
        token: String,
    },
}

impl DaemonTarget {
    fn client(&self) -> DaemonClient {
        match self {
            Self::Local(process) => process.client(),
            Self::Restarting(client) => client.clone(),
            Self::Remote { client, .. } => client.clone(),
        }
    }
}

/// Owns the current daemon and, in development, swaps it after a successful
/// rebuild without requiring the desktop process to relaunch.
#[derive(Clone)]
pub struct DaemonSupervisor {
    inner: Arc<SupervisorInner>,
}

impl DaemonSupervisor {
    pub fn spawn(executable: &Path, watch_for_rebuilds: bool) -> anyhow::Result<Self> {
        Self::spawn_configured(
            executable,
            watch_for_rebuilds,
            DaemonExposureSettings::default(),
        )
    }

    pub fn spawn_configured(
        executable: &Path,
        watch_for_rebuilds: bool,
        exposure: DaemonExposureSettings,
    ) -> anyhow::Result<Self> {
        let exposure = exposure.validate()?;
        let process = DaemonProcess::spawn_configured(executable, exposure.clone())?;
        let settings = read_settings(&process.client())?;
        let initial_stamp = ExecutableStamp::read(executable)?;
        let supervisor = Self::from_target(
            DaemonTarget::Local(process),
            Some(executable.to_owned()),
            Some(exposure),
            settings,
        )?;
        let weak_inner = Arc::downgrade(&supervisor.inner);
        std::thread::Builder::new()
            .name("waku-daemon-supervisor".into())
            .spawn(move || monitor_daemon(weak_inner, Some(initial_stamp), watch_for_rebuilds))
            .context("could not start Waku daemon supervisor")?;
        Ok(supervisor)
    }

    /// Connect to a daemon managed on another host (or by an external local
    /// service manager). Dropping the desktop never shuts this daemon down.
    pub fn connect(address: &str, token: String) -> anyhow::Result<Self> {
        let client = DaemonClient::connect(address, token.clone())?;
        let settings = match read_settings(&client) {
            Ok(settings) => settings,
            Err(error) => {
                client.disconnect();
                return Err(error);
            }
        };
        let supervisor = Self::from_target(
            DaemonTarget::Remote {
                client,
                address: address.to_owned(),
                token,
            },
            None,
            None,
            settings,
        )?;
        let weak_inner = Arc::downgrade(&supervisor.inner);
        std::thread::Builder::new()
            .name("waku-remote-daemon-supervisor".into())
            .spawn(move || monitor_daemon(weak_inner, None, false))
            .context("could not start remote Waku daemon supervisor")?;
        Ok(supervisor)
    }

    fn from_target(
        target: DaemonTarget,
        executable: Option<PathBuf>,
        exposure: Option<DaemonExposureSettings>,
        settings: DaemonSettings,
    ) -> anyhow::Result<Self> {
        let (settings_updates, settings_update_rx) = unbounded();
        let inner = Arc::new(SupervisorInner {
            executable,
            target: Mutex::new(target),
            exposure: Mutex::new(exposure),
            restart: Mutex::new(()),
            settings: Mutex::new(settings),
            // The desktop sends one normalized snapshot after it has migrated
            // the legacy combined settings document into app.json.
            persisted_settings: Mutex::new(None),
            settings_updates,
            client_updates: Mutex::new(Vec::new()),
            running: AtomicBool::new(true),
            clock_origin: Instant::now(),
            recovery_started: AtomicU64::new(0),
        });
        let weak_inner = Arc::downgrade(&inner);
        std::thread::Builder::new()
            .name("waku-daemon-settings".into())
            .spawn(move || persist_settings(weak_inner, settings_update_rx))
            .context("could not start Waku daemon settings writer")?;
        Ok(Self { inner })
    }

    pub fn client(&self) -> DaemonClient {
        self.inner.target.lock().client()
    }

    /// Subscribe to the active daemon connection. The current client is sent
    /// immediately, followed by each replacement after a managed restart.
    pub fn subscribe_clients(&self) -> Receiver<DaemonClient> {
        let (updates, receiver) = unbounded();
        // Holding the target lock through registration makes the initial send
        // atomic with respect to replacement: a subscriber sees either the old
        // client followed by the new one, or the new client directly.
        let target = self.inner.target.lock();
        self.inner.client_updates.lock().push(updates.clone());
        let _ = updates.send(target.client());
        receiver
    }

    pub fn is_remote(&self) -> bool {
        self.inner.executable.is_none()
    }

    /// UI ticks can sample this without locking the supervisor or probing the process.
    pub fn local_recovery_elapsed(&self) -> Option<Duration> {
        let start = self
            .inner
            .recovery_started
            .load(Ordering::Acquire)
            .checked_sub(1)?;
        Some(
            self.inner
                .clock_origin
                .elapsed()
                .saturating_sub(Duration::from_millis(start)),
        )
    }

    /// Admission check for an explicit action, including a close before the next monitor tick.
    pub fn local_transport_unavailable(&self) -> bool {
        if self.is_remote() {
            return false;
        }
        match &*self.inner.target.lock() {
            DaemonTarget::Local(process) => process.client.is_disconnected(),
            DaemonTarget::Restarting(_) => true,
            DaemonTarget::Remote { .. } => false,
        }
    }

    pub fn settings(&self) -> DaemonSettings {
        self.inner.settings.lock().clone()
    }

    /// Restart only the desktop-managed daemon with a new listener policy.
    /// The caller should run this off the UI thread.
    pub fn reconfigure(&self, exposure: DaemonExposureSettings) -> anyhow::Result<()> {
        let exposure = exposure.validate()?;
        let executable = self
            .inner
            .executable
            .as_ref()
            .context("the connected daemon is managed outside Waku Desktop")?
            .clone();
        let _restart = self.inner.restart.lock();
        let previous = self
            .inner
            .exposure
            .lock()
            .clone()
            .context("managed daemon launch settings are unavailable")?;
        match replace_local_daemon(&self.inner, &executable, &exposure) {
            Ok(()) => {
                *self.inner.exposure.lock() = Some(exposure);
                queue_settings_refresh(&self.inner);
                Ok(())
            }
            Err(error) => {
                let restore = replace_local_daemon(&self.inner, &executable, &previous);
                if restore.is_ok() {
                    queue_settings_refresh(&self.inner);
                    Err(error)
                } else {
                    Err(error.context(format!(
                        "the previous daemon configuration also failed to restart: {:#}",
                        restore.unwrap_err()
                    )))
                }
            }
        }
    }

    /// Queue a daemon settings update without blocking the desktop UI thread.
    pub fn update_settings(&self, settings: DaemonSettings) -> anyhow::Result<()> {
        *self.inner.settings.lock() = settings.clone();
        if self.inner.persisted_settings.lock().as_ref() == Some(&settings) {
            return Ok(());
        }
        self.inner
            .settings_updates
            .send(settings)
            .map_err(|_| anyhow::anyhow!("Waku daemon settings writer is closed"))
    }
}

impl Drop for DaemonSupervisor {
    fn drop(&mut self) {
        if Arc::strong_count(&self.inner) == 1 {
            self.inner.running.store(false, Ordering::Release);
        }
    }
}

fn monitor_daemon(
    weak_inner: std::sync::Weak<SupervisorInner>,
    mut active_stamp: Option<ExecutableStamp>,
    watch_for_rebuilds: bool,
) {
    let mut episode = None;
    loop {
        std::thread::sleep(REBUILD_POLL_INTERVAL);
        let Some(inner) = weak_inner.upgrade() else {
            return;
        };
        if !inner.running.load(Ordering::Acquire) {
            return;
        }
        if let Some(executable) = inner.executable.as_ref() {
            monitor_local(
                &inner,
                executable,
                &mut active_stamp,
                watch_for_rebuilds,
                &mut episode,
            );
            continue;
        }
        let remote_reconnect = {
            let target = inner.target.lock();
            match &*target {
                DaemonTarget::Remote {
                    client,
                    address,
                    token,
                } if client.is_disconnected() => Some((
                    client.clone(),
                    address.clone(),
                    token.clone(),
                    client.last_sequences(),
                )),
                _ => None,
            }
        };
        if let Some((disconnected, address, token, resume_from)) = remote_reconnect {
            let _restart = inner.restart.lock();
            let still_current = matches!(
                &*inner.target.lock(),
                DaemonTarget::Remote { client, .. }
                    if client.same_connection(&disconnected) && client.is_disconnected()
            );
            if !still_current {
                continue;
            }
            let Ok(replacement) =
                DaemonClient::connect_with_resume(&address, token.clone(), resume_from)
            else {
                continue;
            };
            *inner.target.lock() = DaemonTarget::Remote {
                client: replacement.clone(),
                address,
                token,
            };
            inner
                .client_updates
                .lock()
                .retain(|subscriber| subscriber.send(replacement.clone()).is_ok());
        }
    }
}

struct RecoveryEpisode {
    connection: Uuid,
    started: Instant,
    attempts: u32,
    last_error: Option<&'static str>,
}

impl RecoveryEpisode {
    fn event(&self, phase: &'static str) -> Event {
        let mut event = Event::new(phase, self.connection);
        event.attempt = Some(self.attempts);
        event.elapsed_ms = Some(self.started.elapsed().as_millis() as u64);
        event
    }

    fn failed(&mut self, phase: &'static str, error: &anyhow::Error) {
        let reason = DaemonClient::connection_error_category(error);
        // Summarize prolonged failures while recording a change of cause immediately.
        if self.last_error != Some(reason) || self.attempts.is_power_of_two() {
            let mut event = self.event(phase).with_error(error.as_ref());
            event.reason = Some(reason);
            event.write_desktop();
        }
        self.last_error = Some(reason);
    }
}

fn monitor_local(
    inner: &SupervisorInner,
    executable: &Path,
    active_stamp: &mut Option<ExecutableStamp>,
    watch_for_rebuilds: bool,
    episode: &mut Option<RecoveryEpisode>,
) {
    // Reconfiguration also takes this lock. A check for an exited process must never
    // restart a newer healthy child which was installed after that check.
    let _restart = inner.restart.lock();
    let stamp = watch_for_rebuilds
        .then(|| ExecutableStamp::read(executable).ok())
        .flatten();
    let rebuilt = stamp.is_some() && stamp != *active_stamp;
    let mut target = inner.target.lock();
    let (action, old_connection, pid, status) = match &mut *target {
        DaemonTarget::Local(p) => {
            let status = p.child.try_wait().ok().flatten();
            let action = local_daemon_recovery(status.is_some(), p.client.is_disconnected());
            (action, p.client.connection_id(), Some(p.child.id()), status)
        }
        DaemonTarget::Restarting(client) => (
            LocalDaemonRecovery::Restart,
            client.connection_id(),
            None,
            None,
        ),
        DaemonTarget::Remote { .. } => return,
    };
    if action == LocalDaemonRecovery::None && !rebuilt {
        if let Some(previous) = episode.take() {
            // Explicit settings reconfiguration recovered the connection between checks.
            let mut event = previous.event("recovered");
            event.new_connection_id = Some(old_connection);
            event.daemon_pid = pid;
            event.write_desktop();
            inner.recovery_started.store(0, Ordering::Release);
        }
        return;
    }

    let trace = episode.get_or_insert_with(|| {
        let started = Instant::now();
        inner.recovery_started.store(
            (started.duration_since(inner.clock_origin).as_millis() as u64) + 1,
            Ordering::Release,
        );
        let mut event = Event::new("recovery_started", old_connection);
        event.daemon_pid = pid;
        event.reason = Some(if status.is_some() {
            "child_exited"
        } else if rebuilt {
            "executable_updated"
        } else {
            "transport_closed"
        });
        event.write_desktop();
        RecoveryEpisode {
            connection: old_connection,
            started,
            attempts: 0,
            last_error: None,
        }
    });
    trace.attempts = trace.attempts.saturating_add(1);
    if let Some(status) = status {
        let mut event = trace.event("child_exited");
        event.exit_code = status.code();
        event.daemon_pid = pid;
        event.write_desktop();
    }
    if action == LocalDaemonRecovery::Reconnect && !rebuilt {
        let DaemonTarget::Local(p) = &*target else {
            return;
        };
        let (address, token, previous) = (p.address.clone(), p.token.clone(), p.client.clone());
        drop(target);
        let client = match DaemonClient::connect_local_with_resume(
            &address,
            token,
            previous.last_sequences(),
        ) {
            Ok(client) => client,
            Err(error) => {
                trace.failed("reconnect_failed", &error);
                return;
            }
        };
        if let DaemonTarget::Local(p) = &mut *inner.target.lock() {
            p.client = client.clone();
        }
        inner
            .client_updates
            .lock()
            .retain(|s| s.send(client.clone()).is_ok());
    } else {
        drop(target);
        let Some(exposure) = inner.exposure.lock().clone() else {
            return;
        };
        if let Err(error) = replace_local_daemon(inner, executable, &exposure) {
            trace.failed("restart_failed", &error);
            return;
        }
        if stamp.is_some() {
            *active_stamp = stamp;
        }
    }
    inner.recovery_started.store(0, Ordering::Release);
    if let DaemonTarget::Local(p) = &*inner.target.lock() {
        let mut event = trace.event("recovered");
        event.new_connection_id = Some(p.client.connection_id());
        event.daemon_pid = Some(p.child.id());
        event.write_desktop();
    }
    *episode = None;
    queue_settings_refresh(inner);
}

fn replace_local_daemon(
    inner: &SupervisorInner,
    executable: &Path,
    exposure: &DaemonExposureSettings,
) -> anyhow::Result<()> {
    let previous = {
        let mut target = inner.target.lock();
        match &*target {
            DaemonTarget::Remote { .. } => {
                bail!("the connected daemon is managed outside Waku Desktop")
            }
            DaemonTarget::Restarting(_) => None,
            DaemonTarget::Local(process) => {
                let disconnected = process.client();
                let previous =
                    std::mem::replace(&mut *target, DaemonTarget::Restarting(disconnected));
                match previous {
                    DaemonTarget::Local(process) => Some(process),
                    _ => unreachable!("local daemon target changed while locked"),
                }
            }
        }
    };
    // Dropping can wait briefly for graceful shutdown, but the target lock is
    // already released so UI actions never block behind process teardown.
    drop(previous);
    let replacement = DaemonProcess::spawn_configured(executable, exposure.clone())?;
    let client = replacement.client();
    *inner.target.lock() = DaemonTarget::Local(replacement);
    inner
        .client_updates
        .lock()
        .retain(|subscriber| subscriber.send(client.clone()).is_ok());
    Ok(())
}

fn queue_settings_refresh(inner: &SupervisorInner) {
    let settings = inner.settings.lock().clone();
    *inner.persisted_settings.lock() = None;
    let _ = inner.settings_updates.send(settings);
}

fn read_settings(client: &DaemonClient) -> anyhow::Result<DaemonSettings> {
    match client.request_with_timeout(
        Uuid::nil(),
        Uuid::nil(),
        Command::GetSettings,
        Duration::from_secs(5),
    )? {
        ResponsePayload::Settings { settings } => Ok(settings),
        _ => bail!("Waku daemon returned an invalid settings response"),
    }
}

fn persist_settings(
    weak_inner: std::sync::Weak<SupervisorInner>,
    updates: Receiver<DaemonSettings>,
) {
    while let Ok(mut settings) = updates.recv() {
        while let Ok(newer) = updates.try_recv() {
            settings = newer;
        }
        loop {
            let Some(inner) = weak_inner.upgrade() else {
                return;
            };
            if !inner.running.load(Ordering::Acquire) {
                return;
            }
            let desired = inner.settings.lock().clone();
            if desired != settings {
                settings = desired;
            }
            let client = inner.target.lock().client();
            let result = client.request(
                Uuid::nil(),
                Uuid::nil(),
                Command::UpdateSettings {
                    settings: settings.clone(),
                },
            );
            match result {
                Ok(ResponsePayload::Ack) => {
                    *inner.persisted_settings.lock() = Some(settings);
                    break;
                }
                Ok(_) => {
                    eprintln!("Waku daemon returned an invalid settings update response");
                }
                Err(error) => {
                    eprintln!("could not persist Waku daemon settings: {error:#}");
                }
            }
            drop(inner);
            std::thread::sleep(REBUILD_POLL_INTERVAL);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn browser_origins_are_exact_and_deduplicated() {
        assert_eq!(
            parse_allowed_origins(
                "https://app.waku.test, http://localhost:3001, https://app.waku.test"
            )
            .unwrap(),
            ["https://app.waku.test", "http://localhost:3001"]
        );
        assert!(parse_allowed_origins("https://app.waku.test/path").is_err());
        assert!(parse_allowed_origins("ws://app.waku.test").is_err());
    }

    #[test]
    fn desktop_uses_loopback_to_reach_an_unspecified_listener() {
        assert_eq!(
            desktop_client_address("0.0.0.0:34123").unwrap(),
            "127.0.0.1:34123"
        );
        assert_eq!(desktop_client_address("[::]:34123").unwrap(), "[::1]:34123");
    }

    #[test]
    fn live_local_daemon_with_a_dropped_client_reconnects_without_restart() {
        assert_eq!(
            local_daemon_recovery(false, true),
            LocalDaemonRecovery::Reconnect
        );
        assert_eq!(
            local_daemon_recovery(true, true),
            LocalDaemonRecovery::Restart
        );
        assert_eq!(
            local_daemon_recovery(false, false),
            LocalDaemonRecovery::None
        );
    }
}
