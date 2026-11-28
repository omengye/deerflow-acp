use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, Instant};

use crossbeam_channel::{Receiver, after, never, select_biased};
use uuid::Uuid;
use waku_client::{Command, DaemonClient, ResponsePayload};
use waku_protocol::connection_diagnostics::Event;

/// Render reads the start timestamp without taking a transport lock. A
/// generation survives fast reconnects so the UI can reconcile pending work.
pub(super) struct SessionRecovery {
    pub local: bool,
    origin: Instant,
    started_ms: AtomicU64,
    pub generation: AtomicU64,
}

impl SessionRecovery {
    pub fn new(local: bool) -> Self {
        Self {
            local,
            origin: Instant::now(),
            started_ms: AtomicU64::new(0),
            generation: AtomicU64::new(0),
        }
    }

    pub fn begin(&self) {
        if !self.local {
            return;
        }
        let offset = self.origin.elapsed().as_millis() as u64 + 1;
        if self
            .started_ms
            .compare_exchange(0, offset, Ordering::AcqRel, Ordering::Acquire)
            .is_ok()
        {
            self.generation.fetch_add(1, Ordering::Release);
        }
    }

    pub fn since(&self) -> Option<Instant> {
        let offset = self.started_ms.load(Ordering::Acquire).checked_sub(1)?;
        Some(self.origin + Duration::from_millis(offset))
    }

    fn finish(&self) {
        self.started_ms.store(0, Ordering::Release);
    }

    pub fn install(
        &self,
        active: &parking_lot::Mutex<DaemonClient>,
        replacement: &DaemonClient,
        session: Uuid,
        runtime: Uuid,
    ) {
        {
            let mut client = active.lock();
            *client = replacement.clone();
            self.finish();
        }
        if self.local {
            // One fresh snapshot covers every poll skipped during the gap.
            let _ = replacement.notify(session, runtime, Command::RefreshBackgroundWork);
        }
    }
}

/// While disconnected, wait for a newer client. Once a local socket is
/// healthy, retry only the read-only attachment on it, with capped backoff.
/// A fresh supervisor publication or a session close interrupts the delay.
pub(super) fn reattach(
    previous: &DaemonClient,
    is_current: impl Fn(&DaemonClient) -> bool,
    updates: &Receiver<DaemonClient>,
    shutdown: &Receiver<()>,
    recovery: &SessionRecovery,
    session: Uuid,
    runtime: Uuid,
) -> Option<(DaemonClient, anyhow::Result<ResponsePayload>)> {
    let mut client = previous.clone();
    let mut retry = never();
    let mut attempt = 0u32;
    let mut last_reason = None;
    let mut delay = Duration::from_millis(250);
    loop {
        select_biased! {
            recv(shutdown) -> _ => return None,
            recv(updates) -> update => {
                let mut next = update.ok()?;
                while let Ok(newer) = updates.try_recv() {
                    next = newer;
                }
                if next.same_connection(&client) {
                    continue;
                }
                client = next;
                delay = Duration::from_millis(250);
                attempt = 0;
                last_reason = None;
            },
            recv(retry) -> _ => {}
        }
        retry = never();
        if client.is_disconnected() {
            continue;
        }
        attempt = attempt.saturating_add(1);
        let result = if recovery.local {
            client.request_with_timeout(
                session,
                Uuid::nil(),
                Command::AttachSession,
                Duration::from_secs(2),
            )
        } else {
            client.request(session, Uuid::nil(), Command::AttachSession)
        };
        if !is_current(&client) {
            continue;
        }
        match result {
            Ok(response) if !client.is_disconnected() => return Some((client, Ok(response))),
            Err(error) if recovery.local => {
                let reason = if client.is_disconnected() || DaemonClient::is_transport_lost(&error)
                {
                    "connection_lost"
                } else if DaemonClient::is_request_timeout(&error) {
                    "attach_timeout"
                } else {
                    "attach_rejected"
                };
                if last_reason != Some(reason) || attempt.is_power_of_two() {
                    log_attachment(
                        "session_reattach_failed",
                        &client,
                        recovery,
                        session,
                        runtime,
                        Some(attempt),
                        reason,
                    );
                }
                last_reason = Some(reason);
                if reason == "attach_rejected" {
                    return Some((client, Err(error)));
                }
                if reason == "attach_timeout" {
                    retry = after(delay);
                    delay = (delay * 2).min(Duration::from_secs(10));
                }
            }
            _ => {}
        }
    }
}

pub(super) fn log_attachment(
    name: &'static str,
    client: &DaemonClient,
    recovery: &SessionRecovery,
    session: Uuid,
    runtime: Uuid,
    attempt: Option<u32>,
    reason: &'static str,
) {
    if recovery.local {
        let mut event = Event::new(name, client.connection_id());
        event.reason = Some(reason);
        event.session_id = Some(session);
        event.runtime_id = Some(runtime);
        event.attempt = attempt;
        event.elapsed_ms = recovery
            .since()
            .map(|start| start.elapsed().as_millis() as u64);
        event.write_desktop();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn repeated_local_transport_errors_share_one_recovery_generation() {
        let state = SessionRecovery::new(true);
        state.begin();
        let original = state.since();
        assert!(original.is_some());
        state.begin();
        assert_eq!(state.since(), original);
        assert_eq!(state.generation.load(Ordering::Acquire), 1);
        state.finish();
        assert!(state.since().is_none());
        state.begin();
        assert_eq!(state.generation.load(Ordering::Acquire), 2);
    }

    #[test]
    fn remote_session_uses_its_existing_error_surface() {
        let state = SessionRecovery::new(false);
        state.begin();
        assert!(state.since().is_none());
        assert_eq!(state.generation.load(Ordering::Acquire), 0);
    }
}
