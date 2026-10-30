//! Cross-store deletion: intent -> provider confirmation -> local tombstone.
//! No operation in this module removes a workspace or worktree directory.

use super::*;
use crate::persistence::{ComposerDraftChange, ComposerDraftTarget, SessionDeletion};
use std::time::Duration;

#[derive(Clone)]
pub(super) struct DeletionContext {
    deerflow: crate::deerflow_config::DeerFlowService,
    task_store: Arc<StateStore>,
    task_state: Arc<Mutex<PersistedState>>,
    removed_session_ids: Arc<Mutex<HashSet<Uuid>>>,
    sessions: Arc<Mutex<HashMap<Uuid, (Uuid, DriverHandle)>>>,
    composer_drafts: ComposerDraftStore,
}

impl WakuBackend {
    pub(super) fn deletion_context(&self) -> DeletionContext {
        DeletionContext {
            deerflow: self.deerflow.clone(),
            task_store: self.task_store.clone(),
            task_state: self.task_state.clone(),
            removed_session_ids: self.removed_session_ids.clone(),
            sessions: self.sessions.clone(),
            composer_drafts: self.composer_drafts.clone(),
        }
    }

    pub(super) fn start_deletion_recovery(&self) -> anyhow::Result<()> {
        let pending = self
            .task_store
            .session_deletions()?
            .into_iter()
            .filter(|entry| entry.stage != "completed")
            .map(|entry| entry.session_id)
            .collect::<Vec<_>>();
        if pending.is_empty() {
            return Ok(());
        }
        let context = self.deletion_context();
        let locks = self.session_lifecycle_locks.clone();
        std::thread::Builder::new()
            .name("waku-session-deletion-recovery".into())
            .spawn(move || {
                for id in pending {
                    let lock = locks
                        .lock()
                        .entry(id)
                        .or_insert_with(|| Arc::new(Mutex::new(())))
                        .clone();
                    let _guard = lock.lock();
                    if let Err(error) = context.remove_session(id) {
                        // Keep the durable intent and original list row. The next
                        // explicit delete or daemon restart retries the same target.
                        eprintln!("session deletion recovery {id}: {error:#}");
                    }
                }
            })
            .context("could not start session deletion recovery")?;
        Ok(())
    }
}

impl DeletionContext {
    pub(super) fn remove_session(&self, session_id: Uuid) -> anyhow::Result<()> {
        self.remove_with(session_id, |entry| self.delete_provider(entry))
    }

    fn remove_with(
        &self,
        session_id: Uuid,
        delete_provider: impl FnOnce(&SessionDeletion) -> Result<(), DeleteFailure>,
    ) -> anyhow::Result<()> {
        let mut entry = {
            let mut state = self.task_state.lock();
            if let Some(existing) = self
                .task_store
                .session_deletions()?
                .into_iter()
                .find(|entry| entry.session_id == session_id)
            {
                existing
            } else {
                let mut native_session_id = None;
                let mut workspace_path = None;
                let mut turn_count = 0;
                if let Some(index) = state
                    .sessions
                    .iter()
                    .position(|session| session.id == session_id)
                {
                    self.task_store.hydrate(&mut state.sessions[index])?;
                    let session = &state.sessions[index];
                    if session.is_busy() || !session.queued_messages.is_empty() {
                        bail!("会话仍有运行或等待中的任务，请先完成或停止任务后再删除");
                    }
                    if session.provider == ProviderKind::DeerFlow {
                        match session.provider_cursor.as_ref() {
                            Some(ProviderResumeCursor::DeerFlow { session_id })
                                if !session_id.trim().is_empty() =>
                            {
                                native_session_id = Some(session_id.clone())
                            }
                            Some(_) => bail!("会话后端身份不匹配，未执行删除"),
                            None if session.has_started()
                                || self.sessions.lock().contains_key(&session_id) =>
                            {
                                bail!("无法确认会话后端身份，请先重新连接会话后再删除")
                            }
                            None => {}
                        }
                    }
                    workspace_path =
                        session.workspace.path().map(Path::to_path_buf).or_else(|| {
                            state
                                .projects
                                .iter()
                                .find(|project| project.id == session.project_id)
                                .map(|project| project.path.clone())
                        });
                    turn_count = session.turns.len();
                } else if self.sessions.lock().contains_key(&session_id) {
                    bail!("会话尚未持久保存，无法确认删除目标");
                }
                let entry = SessionDeletion {
                    session_id,
                    operation_id: Uuid::new_v4(),
                    native_session_id,
                    workspace_path,
                    turn_count,
                    stage: "pending".into(),
                };
                self.task_store.begin_session_deletion(&entry)?;
                self.removed_session_ids.lock().insert(session_id);
                entry
            }
        };
        if entry.stage == "pending" {
            if let Err(failure) = delete_provider(&entry) {
                if failure.definite_refusal {
                    let _state = self.task_state.lock();
                    self.task_store.cancel_session_deletion(&entry)?;
                    self.removed_session_ids.lock().remove(&session_id);
                }
                bail!("{}", failure.message);
            }
            self.task_store.mark_session_backend_deleted(&entry)?;
            entry.stage = "backend_deleted".into();
        }
        if entry.stage != "completed" {
            // The backend confirmation is durable before cleanup. Failure here
            // leaves rows visible and restart can finish without another RPC.
            self.composer_drafts
                .apply_changes(vec![ComposerDraftChange {
                    target: ComposerDraftTarget::Session { session_id },
                    draft: None,
                }])?;
            if let Some(cwd) = &entry.workspace_path {
                crate::checkpoint::delete_all_session_refs(cwd, session_id)?;
            }
            let mut state = self.task_state.lock();
            self.task_store.complete_session_deletion(&entry)?;
            state.sessions.retain(|session| session.id != session_id);
            if state.selected_session == Some(session_id) {
                state.selected_session = None;
            }
        }
        drop(self.sessions.lock().remove(&session_id));
        Ok(())
    }

    fn delete_provider(&self, entry: &SessionDeletion) -> Result<(), DeleteFailure> {
        let Some(native_id) = entry.native_session_id.as_ref() else {
            return Ok(());
        };
        let driver = self
            .sessions
            .lock()
            .get(&entry.session_id)
            .map(|(_, driver)| driver.clone());
        if let Some(driver) = driver {
            return driver
                .delete_session(native_id.clone())
                .recv_timeout(Duration::from_secs(35))
                .map_err(|error| {
                    DeleteFailure::unknown(format!("删除结果尚未确认，请重试删除：{error}"))
                })?
                .map_err(|message| DeleteFailure {
                    definite_refusal: message.starts_with("rejected:"),
                    message,
                });
        }
        let envelope = self
            .deerflow
            .request(
                "manage-envelope",
                json!({"operation":"session.delete","session_id":native_id}),
            )
            .map_err(|error| {
                DeleteFailure::unknown(format!("删除结果尚未确认，请重试删除：{error:#}"))
            })?;
        if envelope["ok"] == true {
            return Ok(());
        }
        // These codes are emitted before purge by Go's management endpoint.
        // Other failures can occur after SQL commit and must keep the intent.
        let definite_refusal = matches!(
            envelope["code"].as_str(),
            Some("busy" | "unresolved_receipts" | "invalid_input" | "permission_denied")
        );
        Err(DeleteFailure {
            definite_refusal,
            message: envelope["error"]
                .as_str()
                .unwrap_or("删除未完成，请重试")
                .to_owned(),
        })
    }
}

struct DeleteFailure {
    definite_refusal: bool,
    message: String,
}

impl DeleteFailure {
    fn unknown(message: String) -> Self {
        Self {
            definite_refusal: false,
            message,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::TurnStatus;
    use rusqlite::Connection;

    struct Fixture {
        root: PathBuf,
        context: DeletionContext,
        id: Uuid,
    }

    impl Fixture {
        fn new() -> Self {
            let root = std::env::temp_dir().join(format!("waku-delete-{}", Uuid::new_v4()));
            std::fs::create_dir_all(&root).unwrap();
            let store = Arc::new(StateStore::daemon(root.join("app.db")));
            let mut state = PersistedState::fresh(root.clone());
            let session = &mut state.sessions[0];
            session.provider = ProviderKind::DeerFlow;
            session.provider_cursor = Some(ProviderResumeCursor::DeerFlow {
                session_id: "go-native-fixture".into(),
            });
            session.begin_turn("Keep this history until both stores confirm deletion");
            session.finish_active_turn(TurnStatus::Completed);
            let id = session.id;
            store.save(&mut state).unwrap();
            let context = DeletionContext {
                deerflow: crate::deerflow_config::DeerFlowService::default(),
                task_store: store,
                task_state: Arc::new(Mutex::new(state)),
                removed_session_ids: Arc::new(Mutex::new(HashSet::new())),
                sessions: Arc::new(Mutex::new(HashMap::new())),
                composer_drafts: ComposerDraftStore::for_state_path(&root.join("app.db")),
            };
            Self { root, context, id }
        }

        fn restored(&self) -> DeletionContext {
            let store = Arc::new(StateStore::daemon(self.root.join("app.db")));
            let state = store.load().unwrap();
            let blocked = store
                .session_deletions()
                .unwrap()
                .into_iter()
                .map(|entry| entry.session_id)
                .collect();
            DeletionContext {
                task_store: store,
                task_state: Arc::new(Mutex::new(state)),
                removed_session_ids: Arc::new(Mutex::new(blocked)),
                ..self.context.clone()
            }
        }
    }

    impl Drop for Fixture {
        fn drop(&mut self) {
            // The isolated test DB can remain open on Windows until teardown.
            let _ = std::fs::remove_dir_all(&self.root);
        }
    }

    #[test]
    fn session_deletion_unknown_result_retries_same_durable_target() {
        let f = Fixture::new();
        let mut operation = Uuid::nil();
        let result = f.context.remove_with(f.id, |entry| {
            operation = entry.operation_id;
            assert_eq!(
                entry.native_session_id.as_deref(),
                Some("go-native-fixture")
            );
            assert_eq!(f.context.task_store.session_deletions().unwrap()[0], *entry);
            Err(DeleteFailure::unknown("connection lost".into()))
        });
        assert!(result.is_err());
        assert_eq!(f.context.task_store.load().unwrap().sessions.len(), 1);
        let restored = f.restored();
        assert!(restored.removed_session_ids.lock().contains(&f.id));
        restored
            .remove_with(f.id, |entry| {
                assert_eq!(entry.operation_id, operation);
                Ok(()) // includes the provider's already-deleted response
            })
            .unwrap();
        assert!(restored.task_store.load().unwrap().sessions.is_empty());
        restored
            .remove_with(f.id, |_| panic!("completed delete must be idempotent"))
            .unwrap();
    }

    #[test]
    fn session_deletion_finishes_when_project_directory_was_removed() {
        let f = Fixture::new();
        {
            let mut state = f.context.task_state.lock();
            state.projects[0].path = f.root.join("already-removed-project");
            f.context.task_store.save(&mut state).unwrap();
        }
        f.context.remove_with(f.id, |_| Ok(())).unwrap();
        assert!(f.context.task_store.load().unwrap().sessions.is_empty());
        assert_eq!(
            f.context.task_store.session_deletions().unwrap()[0].stage,
            "completed"
        );
    }

    #[test]
    fn session_deletion_refusal_preserves_history_and_allows_retry() {
        let f = Fixture::new();
        let result = f.context.remove_with(f.id, |_| {
            Err(DeleteFailure {
                definite_refusal: true,
                message: "unresolved tool receipt".into(),
            })
        });
        assert!(result.is_err());
        assert!(f.context.task_store.session_deletions().unwrap().is_empty());
        assert!(!f.context.removed_session_ids.lock().contains(&f.id));
        assert_eq!(f.context.task_store.load().unwrap().sessions.len(), 1);
    }

    #[test]
    fn session_deletion_local_failure_recovers_without_repeating_backend() {
        let f = Fixture::new();
        let connection = Connection::open(f.context.task_store.path()).unwrap();
        connection.execute_batch("CREATE TRIGGER fail_local_delete BEFORE DELETE ON sessions BEGIN SELECT RAISE(ABORT,'fixture disk failure'); END;").unwrap();
        assert!(f.context.remove_with(f.id, |_| Ok(())).is_err());
        assert_eq!(
            f.context.task_store.session_deletions().unwrap()[0].stage,
            "backend_deleted"
        );
        assert_eq!(f.context.task_store.load().unwrap().sessions.len(), 1);
        connection
            .execute_batch("DROP TRIGGER fail_local_delete;")
            .unwrap();
        let restored = f.restored();
        restored
            .remove_with(f.id, |_| panic!("backend already confirmed"))
            .unwrap();
        assert!(restored.task_store.load().unwrap().sessions.is_empty());
        assert_eq!(
            restored.task_store.session_deletions().unwrap()[0].stage,
            "completed"
        );
        assert!(f.root.is_dir(), "deletion must not remove the workspace");
    }

    #[test]
    fn session_deletion_old_snapshot_cannot_resurrect_history() {
        let f = Fixture::new();
        let mut stale = f.context.task_store.load().unwrap();
        f.context
            .task_store
            .hydrate(&mut stale.sessions[0])
            .unwrap();
        f.context.remove_with(f.id, |_| Ok(())).unwrap();
        let restored = StateStore::daemon(f.root.join("app.db"));
        stale.mark_session_dirty(f.id);
        restored.save(&mut stale).unwrap();
        assert!(restored.load().unwrap().sessions.is_empty());
        let connection = Connection::open(restored.path()).unwrap();
        for query in [
            "SELECT count(*) FROM sessions",
            "SELECT count(*) FROM session_details",
            "SELECT count(*) FROM messages",
        ] {
            assert_eq!(
                connection
                    .query_row(query, [], |row| row.get::<_, i64>(0))
                    .unwrap(),
                0
            );
        }
    }

    #[test]
    fn session_deletion_requires_mapping_before_backend_request() {
        let f = Fixture::new();
        f.context.task_state.lock().sessions[0].provider_cursor = None;
        assert!(
            f.context
                .remove_with(f.id, |_| panic!("missing identity"))
                .is_err()
        );
        assert!(f.context.task_store.session_deletions().unwrap().is_empty());
    }
}
