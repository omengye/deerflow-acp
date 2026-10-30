use uuid::Uuid;
use waku_protocol::provider_session::{ProviderSessionFork, ProviderSessionForkRequest};
use waku_protocol::{Command, ResponsePayload, WorkspaceOperation, WorkspaceResult};

use crate::DaemonClient;

type HistoryKey = (Uuid, &'static str, usize);
fn pending_history_operations()
-> &'static parking_lot::Mutex<std::collections::HashMap<HistoryKey, Uuid>> {
    static PENDING: std::sync::OnceLock<
        parking_lot::Mutex<std::collections::HashMap<HistoryKey, Uuid>>,
    > = std::sync::OnceLock::new();
    PENDING.get_or_init(Default::default)
}

/// Typed convenience wrapper around daemon-owned filesystem and Git RPCs.
#[derive(Clone)]
pub struct WorkspaceClient {
    client: DaemonClient,
}

impl WorkspaceClient {
    pub fn has_pending_history(session_id: Uuid) -> bool {
        pending_history_operations()
            .try_lock()
            .is_none_or(|pending| pending.keys().any(|(id, _, _)| *id == session_id))
    }
    pub fn restore_pending_history(
        session_id: Uuid,
        operation: &waku_protocol::PendingHistoryOperation,
    ) {
        let kind = match operation.kind.as_str() {
            "fork" => "fork",
            "rewind" => "rewind",
            _ => return,
        };
        if operation.turn_count == 0 || operation.operation_id.is_nil() {
            return;
        }
        let mut pending = pending_history_operations().lock();
        pending.retain(|(id, _, _), _| *id != session_id);
        pending.insert(
            (session_id, kind, operation.turn_count),
            operation.operation_id,
        );
    }

    pub fn fork_action_enabled(session_id: Uuid, turn_count: usize, negotiated: bool) -> bool {
        Self::history_action_enabled(session_id, "fork", turn_count, negotiated)
    }

    pub fn rewind_action_enabled(session_id: Uuid, turn_count: usize, negotiated: bool) -> bool {
        Self::history_action_enabled(session_id, "rewind", turn_count, negotiated)
    }

    fn history_action_enabled(
        session_id: Uuid,
        kind: &'static str,
        turn_count: usize,
        negotiated: bool,
    ) -> bool {
        let Some(pending) = pending_history_operations().try_lock() else {
            return false;
        };
        if pending.keys().any(|(id, _, _)| *id == session_id) {
            pending.contains_key(&(session_id, kind, turn_count))
        } else {
            negotiated
        }
    }
    /// UI-only retry admission. A render frame must not wait for the worker
    /// holding the map; contention conservatively keeps the action disabled.
    pub fn has_pending_fork(session_id: Uuid, turn_count: usize) -> bool {
        pending_history_operations()
            .try_lock()
            .is_some_and(|pending| pending.contains_key(&(session_id, "fork", turn_count)))
    }

    pub fn has_pending_rewind(session_id: Uuid, turn_count: usize) -> bool {
        pending_history_operations()
            .try_lock()
            .is_some_and(|pending| pending.contains_key(&(session_id, "rewind", turn_count)))
    }

    pub fn fork_session_from_response(
        &self,
        session_id: Uuid,
        turn_count: usize,
    ) -> anyhow::Result<(waku_protocol::model::AgentSession, Option<String>)> {
        let key = (session_id, "fork", turn_count);
        let operation_id = Some(
            *pending_history_operations()
                .lock()
                .entry(key)
                .or_insert_with(Uuid::new_v4),
        );
        match self.history_request(
            key,
            Command::ForkSessionFromResponse {
                turn_count,
                operation_id,
            },
        )? {
            ResponsePayload::SessionForked {
                session,
                checkpoint_warning,
            } => {
                pending_history_operations().lock().remove(&key);
                Ok((session, checkpoint_warning))
            }
            _ => anyhow::bail!("Waku daemon returned an invalid conversation fork response"),
        }
    }

    pub fn rewind_session_to_message(
        &self,
        session_id: Uuid,
        turn_count: usize,
    ) -> anyhow::Result<(waku_protocol::model::AgentSession, Option<String>)> {
        let key = (session_id, "rewind", turn_count);
        let operation_id = Some(
            *pending_history_operations()
                .lock()
                .entry(key)
                .or_insert_with(Uuid::new_v4),
        );
        match self.history_request(
            key,
            Command::RewindSessionToMessage {
                turn_count,
                operation_id,
            },
        )? {
            ResponsePayload::SessionRewound {
                session,
                cleanup_warning,
            } => {
                pending_history_operations().lock().remove(&key);
                Ok((session, cleanup_warning))
            }
            _ => anyhow::bail!("Waku daemon returned an invalid conversation rewind response"),
        }
    }
    fn history_request(
        &self,
        key: HistoryKey,
        command: Command,
    ) -> anyhow::Result<ResponsePayload> {
        let result = self.client.request(key.0, Uuid::nil(), command);
        if result
            .as_ref()
            .is_err_and(|error| error.to_string().starts_with("rejected:"))
        {
            // A confirmed refusal ended this operation. A later user retry may
            // use a new identity after resolving the conflict. Unknown outcomes
            // retain their identity so a retry cannot apply the change twice.
            pending_history_operations().lock().remove(&key);
        }
        result
    }

    pub fn new(client: DaemonClient) -> Self {
        Self { client }
    }

    pub fn request(&self, operation: WorkspaceOperation) -> anyhow::Result<WorkspaceResult> {
        match self
            .client
            .request(Uuid::nil(), Uuid::nil(), Command::Workspace { operation })?
        {
            ResponsePayload::Workspace { result } => Ok(result),
            _ => anyhow::bail!("Waku daemon returned an invalid workspace response"),
        }
    }

    pub fn fork_provider_session(
        &self,
        request: ProviderSessionForkRequest,
    ) -> anyhow::Result<ProviderSessionFork> {
        match self.client.request(
            Uuid::nil(),
            Uuid::nil(),
            Command::ForkProviderSession { request },
        )? {
            ResponsePayload::ProviderSessionForked { result } => Ok(result),
            _ => anyhow::bail!("Waku daemon returned an invalid provider-fork response"),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pending_history_retry_admission_is_bound_to_session_kind_and_turn() {
        let session = Uuid::new_v4();
        let key = (session, "fork", 3);
        pending_history_operations()
            .lock()
            .insert(key, Uuid::new_v4());
        assert!(WorkspaceClient::has_pending_fork(session, 3));
        assert!(WorkspaceClient::fork_action_enabled(session, 3, false));
        assert!(!WorkspaceClient::fork_action_enabled(session, 2, true));
        assert!(!WorkspaceClient::rewind_action_enabled(session, 3, true));
        assert!(!WorkspaceClient::has_pending_fork(session, 2));
        assert!(!WorkspaceClient::has_pending_rewind(session, 3));
        let operation_id = Uuid::new_v4();
        WorkspaceClient::restore_pending_history(
            session,
            &waku_protocol::PendingHistoryOperation {
                kind: "rewind".into(),
                turn_count: 4,
                operation_id,
            },
        );
        assert!(WorkspaceClient::rewind_action_enabled(session, 4, false));
        assert_eq!(
            pending_history_operations()
                .lock()
                .get(&(session, "rewind", 4)),
            Some(&operation_id)
        );
        assert!(!WorkspaceClient::fork_action_enabled(session, 4, true));
        pending_history_operations()
            .lock()
            .remove(&(session, "rewind", 4));
        assert!(!WorkspaceClient::has_pending_fork(Uuid::new_v4(), 3));
        pending_history_operations().lock().remove(&key);
        assert!(!WorkspaceClient::has_pending_fork(session, 3));
        let key = (session, "rewind", 3);
        pending_history_operations()
            .lock()
            .insert(key, Uuid::new_v4());
        assert!(WorkspaceClient::has_pending_rewind(session, 3));
        assert!(!WorkspaceClient::has_pending_fork(session, 3));
        pending_history_operations().lock().remove(&key);
        assert!(!WorkspaceClient::has_pending_rewind(session, 3));
    }
}
