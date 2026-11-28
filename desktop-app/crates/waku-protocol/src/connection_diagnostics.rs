//! Local desktop transport diagnostics. Only host-defined categories and numeric
//! metadata can enter a record. Callers never format RPCs, credentials or peer text.
use std::fs::{self, File, OpenOptions};
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::sync::OnceLock;
use std::sync::mpsc::{Receiver, SyncSender, TrySendError, sync_channel};
use std::time::{Duration, Instant};

use chrono::{DateTime, Utc};
use serde::Serialize;
use uuid::Uuid;

enum WriteCommand {
    Record(Event),
    Drain(SyncSender<()>),
}

type Writer = OnceLock<Option<SyncSender<WriteCommand>>>;
static DAEMON: Writer = OnceLock::new();

#[derive(Serialize, Default)]
pub struct Event {
    time: DateTime<Utc>,
    pid: u32,
    phase: &'static str,
    connection_id: Uuid,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub new_connection_id: Option<Uuid>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub session_id: Option<Uuid>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub runtime_id: Option<Uuid>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<&'static str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub daemon_pid: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub exit_code: Option<i32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub attempt: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub elapsed_ms: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub close_code: Option<u16>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub queue_depth: Option<usize>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub pending_requests: Option<usize>,
    #[serde(skip_serializing_if = "Option::is_none")]
    io_kind: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    os_error: Option<i32>,
}

impl Event {
    pub fn new(phase: &'static str, connection_id: Uuid) -> Self {
        Self {
            time: Utc::now(),
            pid: std::process::id(),
            phase,
            connection_id,
            ..Self::default()
        }
    }

    pub fn because(mut self, reason: &'static str) -> Self {
        self.reason = Some(reason);
        self
    }

    pub fn with_error(mut self, error: &(dyn std::error::Error + 'static)) -> Self {
        let mut cause = Some(error);
        while let Some(current) = cause {
            if let Some(io) = current.downcast_ref::<io::Error>() {
                self.io_kind = Some(format!("{:?}", io.kind()));
                self.os_error = io.raw_os_error();
                break;
            }
            cause = current.source();
        }
        self
    }

    pub fn write_desktop(self) {
        static DESKTOP: Writer = OnceLock::new();
        self.submit(&DESKTOP, "deerflow-desktop-connection");
    }

    pub fn write_daemon(self) {
        self.submit(&DAEMON, "waku-daemon-connection");
    }

    fn submit(self, writer: &Writer, component: &'static str) {
        let channel = writer.get_or_init(|| {
            let (tx, rx) = sync_channel(256);
            std::thread::Builder::new()
                .name(format!("{component}-log"))
                .spawn(move || {
                    let directory = crate::identity::portable_root()
                        .join("user-data")
                        .join("logs");
                    let path = directory.join(format!(
                        "{component}-{}-{}.jsonl",
                        std::process::id(),
                        Uuid::new_v4()
                    ));
                    let log = RotatingFile {
                        path,
                        file: None,
                        bytes: 0,
                        limit: 2 * 1024 * 1024,
                    };
                    log.run(rx);
                })
                .ok()?;
            Some(tx)
        });
        if let Some(channel) = channel {
            let _ = channel.try_send(WriteCommand::Record(self));
        }
    }
}

/// Called only by the exiting local service. No filesystem wait on render,
/// socket or supervisor threads. A stalled disk cannot hold up app shutdown.
pub fn drain_daemon_log(timeout: Duration) {
    if let Some(Some(writer)) = DAEMON.get() {
        drain_writer(writer, timeout);
    }
}

fn drain_writer(writer: &SyncSender<WriteCommand>, timeout: Duration) {
    let deadline = Instant::now() + timeout;
    let (ack, drained) = sync_channel(1);
    let mut fence = WriteCommand::Drain(ack);
    loop {
        match writer.try_send(fence) {
            Ok(()) => {
                let _ = drained.recv_timeout(deadline.saturating_duration_since(Instant::now()));
                return;
            }
            Err(TrySendError::Full(command)) if Instant::now() < deadline => {
                fence = command;
                std::thread::sleep(Duration::from_millis(1));
            }
            Err(_) => return,
        }
    }
}

struct RotatingFile {
    path: PathBuf,
    file: Option<File>,
    bytes: usize,
    limit: usize,
}

fn move_if_present(from: &Path, to: &Path) -> io::Result<()> {
    match fs::rename(from, to) {
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(()),
        result => result,
    }
}

impl RotatingFile {
    fn run(mut self, commands: Receiver<WriteCommand>) {
        for command in commands {
            match command {
                WriteCommand::Record(event) => {
                    let _ = self.write(&event);
                }
                WriteCommand::Drain(ack) => {
                    if let Some(file) = &self.file {
                        let _ = file.sync_data();
                    }
                    let _ = ack.send(());
                }
            }
        }
    }

    fn write(&mut self, event: &Event) -> io::Result<()> {
        let mut line = serde_json::to_vec(event)?;
        line.push(b'\n');
        if self.bytes != 0 && self.bytes.saturating_add(line.len()) > self.limit {
            self.file.take();
            let first = self.path.with_added_extension("1");
            let second = self.path.with_added_extension("2");
            match fs::remove_file(&second) {
                Err(e) if e.kind() != io::ErrorKind::NotFound => return Err(e),
                _ => {}
            }
            move_if_present(&first, &second)?;
            move_if_present(&self.path, &first)?;
            self.bytes = 0;
        }
        if self.file.is_none() {
            if let Some(directory) = self.path.parent() {
                fs::create_dir_all(directory)?;
            }
            let mut options = OpenOptions::new();
            options.create(true).append(true);
            #[cfg(unix)]
            {
                use std::os::unix::fs::OpenOptionsExt;
                options.mode(0o600);
            }
            self.file = Some(options.open(&self.path)?);
        }
        self.file.as_mut().expect("log is open").write_all(&line)?;
        self.bytes += line.len();
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn file_rotation_keeps_two_backups_and_only_safe_error_fields() {
        let dir = std::env::temp_dir().join(format!("deerflow-log-test-{}", Uuid::new_v4()));
        let path = dir.join("connection.jsonl");
        let mut file = RotatingFile {
            path: path.clone(),
            file: None,
            bytes: 0,
            limit: 1,
        };
        let connection = Uuid::new_v4();
        for phase in ["one", "two", "three", "four"] {
            let error = io::Error::other("secret fixture text");
            file.write(&Event::new(phase, connection).with_error(&error))
                .unwrap();
        }
        drop(file);
        assert_eq!(fs::read_dir(&dir).unwrap().count(), 3);
        for (file, phase) in [
            (path.clone(), "four"),
            (path.with_added_extension("1"), "three"),
            (path.with_added_extension("2"), "two"),
        ] {
            let line = fs::read_to_string(file).unwrap();
            assert!(!line.contains("secret fixture text"));
            let record: serde_json::Value = serde_json::from_str(&line).unwrap();
            assert_eq!(record["phase"], phase);
            assert_eq!(record["connection_id"], connection.to_string());
            assert_eq!(record["io_kind"], "Other");
        }
        fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn daemon_shutdown_drains_previous_records_before_exit() {
        let dir = std::env::temp_dir().join(format!("deerflow-drain-test-{}", Uuid::new_v4()));
        let path = dir.join("connection.jsonl");
        let (tx, rx) = sync_channel(1);
        let event = Event::new("connection_closed", Uuid::new_v4()).because("requested_shutdown");
        tx.send(WriteCommand::Record(event)).unwrap();
        let log = RotatingFile {
            path: path.clone(),
            file: None,
            bytes: 0,
            limit: 2048,
        };
        let worker = std::thread::spawn(move || log.run(rx));
        drain_writer(&tx, Duration::from_secs(2));
        assert!(
            fs::read_to_string(path)
                .unwrap()
                .contains("requested_shutdown")
        );
        drop(tx);
        worker.join().unwrap();
        fs::remove_dir_all(dir).unwrap();
    }
}
