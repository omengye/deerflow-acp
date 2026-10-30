//! Native DeerFlow history mutations with durable request identities.
use super::*;
use crate::model::TurnStatus;

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct NativeTurn {
    turn_id: String,
    event_id: String,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct NativeHistory {
    session_id: String,
    head_event_id: String,
    turns: Vec<NativeTurn>,
}

#[derive(Clone, Deserialize, Serialize)]
struct HistoryIntent {
    source: AgentSession,
    cwd: PathBuf,
    binary: PathBuf,
    kind: String,
    turn_count: usize,
    request: Value,
    result: AgentSession,
    restore_ref: Option<String>,
    safety_ref: Option<String>,
    workspace_applied: bool,
    native_applied: bool,
    completed: bool,
    #[serde(default)]
    previous_operation_ids: Vec<Uuid>,
    #[serde(default)]
    operation_aliases: Vec<Uuid>,
    #[serde(default)]
    failure: Option<String>,
}

#[derive(Clone)]
struct HistoryContext {
    store: Arc<StateStore>,
    state: Arc<Mutex<PersistedState>>,
    sessions: Arc<Mutex<HashMap<Uuid, (Uuid, DriverHandle)>>>,
}

impl WakuBackend {
    pub(super) fn history_admission_lock(
        &self,
        session_id: Uuid,
        command: &Command,
    ) -> anyhow::Result<Option<(Arc<Mutex<()>>, PathBuf)>> {
        let cwd = match command {
            Command::Start { options } => Some(options.cwd.clone()),
            Command::Prompt { .. }
            | Command::PromptWithAttachments { .. }
            | Command::ForkSessionFromResponse { .. }
            | Command::RewindSessionToMessage { .. }
            | Command::DeerFlowTasks { .. } => {
                let state = self.task_state.lock();
                state
                    .sessions
                    .iter()
                    .find(|session| session.id == session_id)
                    .and_then(|session| {
                        session.workspace.path().map(Path::to_owned).or_else(|| {
                            state
                                .projects
                                .iter()
                                .find(|project| project.id == session.project_id)
                                .map(|project| project.path.clone())
                        })
                    })
            }
            _ => None,
        };
        let cwd = cwd.or_else(|| {
            if matches!(
                command,
                Command::Prompt { .. }
                    | Command::PromptWithAttachments { .. }
                    | Command::DeerFlowTasks { .. }
            ) {
                self.runtime_workspaces
                    .lock()
                    .get(&session_id)
                    .map(|(_, path)| path.clone())
            } else {
                None
            }
        });
        cwd.map(|cwd| {
            let key = workspace_identity(&cwd)?;
            let lock = self
                .workspace_admission_locks
                .lock()
                .entry(key)
                .or_insert_with(|| Arc::new(Mutex::new(())))
                .clone();
            Ok((lock, cwd))
        })
        .transpose()
    }

    fn history_context(&self) -> HistoryContext {
        HistoryContext {
            store: self.task_store.clone(),
            state: self.task_state.clone(),
            sessions: self.sessions.clone(),
        }
    }

    pub(super) fn history_pending(&self, session_id: Uuid) -> anyhow::Result<bool> {
        self.task_store.has_pending_history(session_id)
    }

    pub(super) fn pending_history_operation(
        &self,
        session_id: Uuid,
    ) -> anyhow::Result<Option<waku_protocol::PendingHistoryOperation>> {
        self.task_store
            .history_operation(session_id)?
            .as_ref()
            .map(pending_operation)
            .transpose()
            .map(Option::flatten)
    }

    pub(super) fn history_workspace_pending(&self, cwd: &Path) -> anyhow::Result<bool> {
        let cwd = workspace_identity(cwd)?;
        for data in self.task_store.pending_history_operations()? {
            let intent: HistoryIntent = serde_json::from_value(data)?;
            if intent.restore_ref.is_some() && workspace_identity(&intent.cwd)? == cwd {
                return Ok(true);
            }
        }
        Ok(false)
    }

    pub(super) fn start_history_recovery(&self) -> anyhow::Result<()> {
        let pending = self
            .task_store
            .pending_history_operations()?
            .into_iter()
            .map(serde_json::from_value::<HistoryIntent>)
            .collect::<Result<Vec<_>, _>>()?
            .into_iter()
            .filter(|entry| !entry.completed)
            .collect::<Vec<_>>();
        if pending.is_empty() {
            return Ok(());
        }
        let context = self.history_context();
        let locks = self.session_lifecycle_locks.clone();
        let workspace_locks = self.workspace_admission_locks.clone();
        std::thread::Builder::new()
            .name("waku-history-recovery".into())
            .spawn(move || {
                for intent in pending {
                    let id = intent.source.id;
                    let lock = locks
                        .lock()
                        .entry(id)
                        .or_insert_with(|| Arc::new(Mutex::new(())))
                        .clone();
                    let _guard = lock.lock();
                    let key = match workspace_identity(&intent.cwd) {
                        Ok(key) => key,
                        Err(error) => {
                            eprintln!("history recovery {id}: {error:#}");
                            continue;
                        }
                    };
                    let workspace_lock = workspace_locks
                        .lock()
                        .entry(key)
                        .or_insert_with(|| Arc::new(Mutex::new(())))
                        .clone();
                    let _workspace_guard = workspace_lock.lock();
                    if let Err(error) = context.resume(intent, None) {
                        eprintln!("history recovery {id}: {error:#}");
                    }
                }
            })
            .context("could not start native history recovery")?;
        Ok(())
    }

    pub(super) fn deerflow_history_mutation(
        &self,
        session_id: Uuid,
        turn_count: usize,
        kind: &str,
        operation_id: Option<Uuid>,
    ) -> anyhow::Result<AgentSession> {
        let context = self.history_context();
        let mut previous_operation_ids = Vec::new();
        if let Some(data) = self.task_store.history_operation(session_id)? {
            if data["deleted_result"] == true {
                previous_operation_ids = serde_json::from_value(data["operation_ids"].clone())?;
                if operation_id.is_some_and(|id| previous_operation_ids.contains(&id)) {
                    bail!(
                        "rejected: The forked conversation was deleted; this operation cannot be replayed"
                    );
                }
            } else {
                let mut intent: HistoryIntent = serde_json::from_value(data)?;
                if reuse_intent(&intent, kind, turn_count, operation_id)? {
                    if let Some(id) = operation_id {
                        if intent.request["operationId"] != id.to_string()
                            && !intent.operation_aliases.contains(&id)
                        {
                            intent.operation_aliases.push(id);
                            context.persist(&intent)?;
                        }
                    }
                    return context.resume(intent, None);
                }
                previous_operation_ids = intent.previous_operation_ids;
                previous_operation_ids.extend(intent.operation_aliases);
                if let Some(id) = intent.request["operationId"]
                    .as_str()
                    .and_then(|id| Uuid::parse_str(id).ok())
                {
                    previous_operation_ids.push(id);
                }
            }
        }
        let (source, cwd, title) = {
            let mut state = self.task_state.lock();
            let index = state
                .sessions
                .iter()
                .position(|session| session.id == session_id)
                .ok_or_else(|| anyhow!("the source task is unavailable"))?;
            self.task_store.hydrate(&mut state.sessions[index])?;
            let source = state.sessions[index].clone();
            if source.provider != ProviderKind::DeerFlow
                || source.is_busy()
                || !source.queued_messages.is_empty()
            {
                bail!("Finish active and queued work before changing conversation history");
            }
            let project = state
                .projects
                .iter()
                .find(|project| project.id == source.project_id)
                .ok_or_else(|| anyhow!("the project is unavailable"))?;
            let cwd = source.workspace.path().unwrap_or(&project.path).to_owned();
            let workspace_key = workspace_identity(&cwd)?;
            if kind == "rewind"
                && self
                    .active_workspace_prompts
                    .lock()
                    .iter()
                    .any(|(_, (_, key))| key == &workspace_key)
            {
                bail!("A conversation is running in this workspace; wait for it before rewinding");
            }
            if kind == "rewind"
                && state.sessions.iter().any(|other| {
                    other.id != source.id
                        && (other.is_busy() || !other.queued_messages.is_empty())
                        && other
                            .workspace
                            .path()
                            .or_else(|| {
                                state
                                    .projects
                                    .iter()
                                    .find(|project| project.id == other.project_id)
                                    .map(|project| project.path.as_path())
                            })
                            .is_some_and(|path| {
                                workspace_identity(path).is_ok_and(|key| key == workspace_key)
                            })
                })
            {
                bail!(
                    "Another conversation is working in this workspace; wait for it before rewinding"
                );
            }
            let title = next_response_fork_title(
                source.display_title(),
                state.sessions.iter().map(AgentSession::display_title),
            );
            (source, cwd, title)
        };
        if kind == "fork" {
            validate_response_fork(&source, turn_count)?;
        } else {
            validate_message_rewind(&source, turn_count)?;
            // A management command may have entered another resident DeerFlow
            // connection before its UI projection becomes busy. Its native
            // history guard checks foreground executions and the full task
            // graph while the shared workspace admission fence is held.
            let peers = {
                let drivers = self.sessions.lock().clone();
                let state = self.task_state.lock();
                let key = workspace_identity(&cwd)?;
                state
                    .sessions
                    .iter()
                    .filter(|other| {
                        other.id != source.id && other.provider == ProviderKind::DeerFlow
                    })
                    .filter_map(|other| {
                        let (_, driver) = drivers.get(&other.id)?;
                        let path = other.workspace.path().or_else(|| {
                            state
                                .projects
                                .iter()
                                .find(|project| project.id == other.project_id)
                                .map(|project| project.path.as_path())
                        })?;
                        if !workspace_identity(path).is_ok_and(|identity| identity == key) {
                            return None;
                        }
                        let Some(ProviderResumeCursor::DeerFlow { session_id }) =
                            &other.provider_cursor
                        else {
                            return Some((driver.clone(), None));
                        };
                        Some((driver.clone(), Some(session_id.clone())))
                    })
                    .collect::<Vec<_>>()
            };
            for (peer, native_id) in peers {
                let native_id = native_id.ok_or_else(|| {
                    anyhow!("Another conversation in this workspace has not finished connecting")
                })?;
                peer.deerflow_history("_deerflow/history/turns", json!({"sessionId":native_id}))
                    .context(
                        "Another conversation has active or unreconciled work in this workspace",
                    )?;
            }
        }
        let native_id = match &source.provider_cursor {
            Some(ProviderResumeCursor::DeerFlow { session_id }) if !session_id.is_empty() => {
                session_id.clone()
            }
            _ => bail!("The DeerFlow native conversation identity is unavailable"),
        };
        let binary = self.provider_binary(ProviderKind::DeerFlow)?;
        let driver = context.driver(&source, &cwd, &binary)?;
        let snapshot: NativeHistory = serde_json::from_value(
            driver.deerflow_history("_deerflow/history/turns", json!({"sessionId": native_id}))?,
        )?;
        let retained = if kind == "rewind" {
            turn_count.saturating_sub(1)
        } else {
            turn_count
        };
        let target = select_boundary(&source, &snapshot, &native_id, retained, kind == "fork")?;
        let operation_id = operation_id.unwrap_or_else(Uuid::new_v4);
        let request = json!({"sessionId": native_id, "expectedHeadEventId": snapshot.head_event_id, "targetEventId": target, "operationId": operation_id});
        let mut result = if kind == "fork" {
            source
                .fork_through_turn(
                    retained,
                    ProviderResumeCursor::DeerFlow {
                        session_id: native_id.clone(),
                    },
                    &title,
                )
                .ok_or_else(|| anyhow!("the selected response cannot be copied"))?
        } else {
            let mut result = source.clone();
            result.truncate_after_turn(retained);
            result.history_revision = source
                .history_revision
                .checked_add(1)
                .ok_or_else(|| anyhow!("conversation revision exhausted"))?;
            result.status = SessionStatus::Idle;
            result.runtime_event_cursor = None;
            result.context_usage = None;
            result
        };
        result.queued_messages.clear();
        let (restore_ref, safety_ref) = if kind == "rewind" {
            // A current checkpoint must include every completed/failed turn.
            // Human or other-session edits made since it are not this operation's files.
            let expected = crate::checkpoint::checkpoint_ref(source.id, source.turns.len());
            let safety = format!("refs/waku/history-safety-{operation_id}");
            crate::checkpoint::capture_ref(&cwd, &safety)?;
            if !crate::checkpoint::same_tree(&cwd, &safety, &expected)? {
                let _ = crate::checkpoint::delete_ref(&cwd, &safety);
                bail!(
                    "The workspace changed after this conversation's last checkpoint; preserve those edits before rewinding"
                );
            }
            let start = crate::checkpoint::turn_start_ref(source.id, turn_count);
            let target = if crate::checkpoint::has_ref(&cwd, &start) {
                start
            } else {
                crate::checkpoint::checkpoint_ref(source.id, retained)
            };
            if !crate::checkpoint::has_ref(&cwd, &target) {
                bail!("the checkpoint before this message is unavailable");
            }
            (Some(target), Some(safety))
        } else {
            (None, None)
        };
        let intent = HistoryIntent {
            source,
            cwd,
            binary,
            kind: kind.into(),
            turn_count,
            request,
            result,
            restore_ref,
            safety_ref,
            workspace_applied: false,
            native_applied: false,
            completed: false,
            previous_operation_ids,
            operation_aliases: Vec::new(),
            failure: None,
        };
        context.persist(&intent)?;
        context.resume(intent, Some(driver))
    }
}

pub(super) fn workspace_identity(cwd: &Path) -> anyhow::Result<PathBuf> {
    let root = crate::checkpoint::workspace_root(cwd)?;
    #[cfg(windows)]
    let root = PathBuf::from(root.to_string_lossy().to_lowercase());
    Ok(root)
}

fn pending_operation(
    data: &Value,
) -> anyhow::Result<Option<waku_protocol::PendingHistoryOperation>> {
    if data["completed"] == true {
        return Ok(None);
    }
    let kind = data["kind"]
        .as_str()
        .ok_or_else(|| anyhow!("Invalid pending history action"))?;
    if !matches!(kind, "fork" | "rewind") {
        bail!("Invalid pending history action");
    }
    Ok(Some(waku_protocol::PendingHistoryOperation {
        kind: kind.to_owned(),
        turn_count: serde_json::from_value(data["turn_count"].clone())?,
        operation_id: serde_json::from_value(data["request"]["operationId"].clone())?,
    }))
}

fn reuse_intent(
    intent: &HistoryIntent,
    kind: &str,
    turn_count: usize,
    operation_id: Option<Uuid>,
) -> anyhow::Result<bool> {
    if operation_id.is_some_and(|id| intent.previous_operation_ids.contains(&id)) {
        bail!(
            "rejected: This history operation receipt has expired after a later action; reload the conversation"
        );
    }
    let same_id = operation_id.is_some_and(|id| {
        intent.request["operationId"] == id.to_string() || intent.operation_aliases.contains(&id)
    });
    if same_id || !intent.completed {
        if intent.kind != kind || intent.turn_count != turn_count {
            bail!("rejected: This history operation ID was already used for a different action");
        }
        return Ok(true);
    }
    Ok(false)
}

/// The native fork returns only complete references it actually copied. Never
/// rewrite naked asset IDs, map keys, or arbitrary relative filesystem paths.
fn remap_fork_references(session: &mut AgentSession, map: &Value) -> anyhow::Result<()> {
    if map.is_null() {
        return Ok(());
    }
    let entries = map
        .as_object()
        .ok_or_else(|| anyhow!("Invalid fork reference map"))?;
    let mut replacements = entries
        .iter()
        .map(|(old, new)| {
            let new = new
                .as_str()
                .filter(|value| !value.is_empty())
                .ok_or_else(|| anyhow!("Invalid fork reference destination"))?;
            let complete = |value: &str| {
                value.starts_with("deerflow-asset://")
                    || value.starts_with("file://")
                    || Path::new(value).is_absolute()
            };
            if !complete(old) || !complete(new) {
                bail!("The native fork returned an incomplete attachment reference");
            }
            Ok((old.as_str(), new))
        })
        .collect::<anyhow::Result<Vec<_>>>()?;
    replacements.sort_by_key(|(old, _)| std::cmp::Reverse(old.len()));
    let mut serialized = serde_json::to_value(&*session)?;
    remap_value(&mut serialized, &replacements);
    *session = serde_json::from_value(serialized)?;
    Ok(())
}

fn remap_value(value: &mut Value, replacements: &[(&str, &str)]) {
    match value {
        Value::String(text) => {
            let source = text.clone();
            let mut result = String::with_capacity(source.len());
            let mut offset = 0;
            while offset < source.len() {
                let tail = &source[offset..];
                let boundary = |character: char| {
                    !character.is_alphanumeric() && !":/\\._%~-".contains(character)
                };
                let found = replacements.iter().find(|(old, _)| {
                    tail.starts_with(old)
                        && source[..offset].chars().next_back().is_none_or(boundary)
                        && tail[old.len()..].chars().next().is_none_or(boundary)
                });
                if let Some((old, new)) = found {
                    result.push_str(new);
                    offset += old.len();
                } else {
                    let character = tail.chars().next().unwrap();
                    result.push(character);
                    offset += character.len_utf8();
                }
            }
            *text = result;
        }
        Value::Array(values) => values
            .iter_mut()
            .for_each(|value| remap_value(value, replacements)),
        Value::Object(values) => values
            .values_mut()
            .for_each(|value| remap_value(value, replacements)),
        _ => {}
    }
}

fn select_boundary(
    source: &AgentSession,
    snapshot: &NativeHistory,
    native_id: &str,
    retained: usize,
    fork: bool,
) -> anyhow::Result<String> {
    if snapshot.session_id != native_id
        || snapshot.head_event_id.is_empty()
        || retained > source.turns.len()
    {
        bail!("The native history identity or boundary is invalid");
    }
    let complete = source
        .turns
        .iter()
        .filter(|turn| turn.provider_turn_started && turn.status == TurnStatus::Completed)
        .count();
    if complete != snapshot.turns.len() {
        bail!(
            "The displayed history differs from the backend's completed turns; reload before changing history"
        );
    }
    let mut ids = HashSet::new();
    let mut events = HashSet::new();
    if snapshot.turns.iter().any(|turn| {
        turn.turn_id.is_empty()
            || turn.event_id.is_empty()
            || !ids.insert(&turn.turn_id)
            || !events.insert(&turn.event_id)
    }) {
        bail!("The backend returned ambiguous turn boundaries");
    }
    if fork
        && source
            .turns
            .get(retained.saturating_sub(1))
            .is_none_or(|turn| turn.status != TurnStatus::Completed || !turn.provider_turn_started)
    {
        bail!("Only a completed native response can be forked");
    }
    let retained_completed = source
        .turns
        .iter()
        .take(retained)
        .filter(|turn| turn.provider_turn_started && turn.status == TurnStatus::Completed)
        .count();
    Ok(retained_completed
        .checked_sub(1)
        .map(|index| snapshot.turns[index].event_id.clone())
        .unwrap_or_default())
}

impl HistoryContext {
    fn persist(&self, intent: &HistoryIntent) -> anyhow::Result<()> {
        self.store
            .save_history_operation(intent.source.id, &serde_json::to_value(intent)?)
    }

    fn driver(
        &self,
        source: &AgentSession,
        cwd: &Path,
        binary: &Path,
    ) -> anyhow::Result<DriverHandle> {
        if let Some((_, driver)) = self.sessions.lock().get(&source.id) {
            return Ok(driver.clone());
        }
        if !matches!(&source.provider_cursor, Some(ProviderResumeCursor::DeerFlow { session_id }) if !session_id.is_empty())
        {
            bail!(
                "A native conversation is required; history recovery cannot start an empty session"
            );
        }
        let (wake, _wakes) = smol::channel::bounded(1);
        let (events, _received) = driver::event_channel(wake);
        driver::start_local(
            ProviderKind::DeerFlow,
            DriverStartOptions {
                binary: binary.to_owned(),
                cwd: cwd.to_owned(),
                mode: source.runtime_mode,
                model: source.model.clone(),
                reasoning_effort: source.reasoning_effort.clone(),
                thinking_mode: source.thinking_mode.clone(),
                service_tier: source.service_tier.clone(),
                context_window: source.context_window.clone(),
                agent_preset: source.agent_preset.clone(),
                computer_use_enabled: false,
                provider_cursor: source.provider_cursor.clone(),
            },
            events,
        )
    }

    fn resume(
        &self,
        intent: HistoryIntent,
        driver: Option<DriverHandle>,
    ) -> anyhow::Result<AgentSession> {
        self.resume_with(intent, |intent| {
            let driver = match driver {
                Some(driver) => driver,
                None => self.driver(&intent.source, &intent.cwd, &intent.binary)?,
            };
            driver.deerflow_history(
                &format!("_deerflow/history/{}", intent.kind),
                intent.request.clone(),
            )
        })
    }

    fn resume_with(
        &self,
        mut intent: HistoryIntent,
        apply: impl FnOnce(&HistoryIntent) -> anyhow::Result<Value>,
    ) -> anyhow::Result<AgentSession> {
        if intent.completed {
            if let Some(failure) = intent.failure {
                bail!(failure);
            }
            let mut state = self.state.lock();
            let current = state
                .sessions
                .iter_mut()
                .find(|session| session.id == intent.result.id)
                .ok_or_else(|| {
                    anyhow!(
                        "rejected: The conversation returned by this operation no longer exists"
                    )
                })?;
            self.store.hydrate(current)?;
            if current.history_revision < intent.result.history_revision {
                bail!(
                    "rejected: The conversation revision changed unexpectedly; reload before retrying"
                );
            }
            return Ok(current.clone());
        }
        if intent.failure.is_some() {
            return self.finish_refused(intent);
        }
        if !intent.workspace_applied {
            if let (Some(target), Some(safety)) = (&intent.restore_ref, &intent.safety_ref) {
                // A crash can happen after Git restore and before the stage save.
                let expected = if crate::checkpoint::same_tree(
                    &intent.cwd,
                    &crate::checkpoint::capture_worktree_commit(&intent.cwd)?,
                    target,
                )? {
                    target
                } else {
                    safety
                };
                crate::checkpoint::restore_ref_if_unchanged(&intent.cwd, target, expected)?;
            }
            intent.workspace_applied = true;
            self.persist(&intent)?;
        }
        if !intent.native_applied {
            let result = match apply(&intent) {
                Ok(result) => result,
                Err(error) => {
                    if error.to_string().starts_with("rejected:") {
                        intent.failure = Some(error.to_string());
                        self.persist(&intent)?;
                        return self.finish_refused(intent);
                    }
                    return Err(error);
                }
            };
            let native_id = result["sessionId"].as_str().filter(|id| !id.is_empty()).ok_or_else(|| anyhow!("The native history operation returned no session identity; retry to reconcile"))?;
            if intent.kind == "rewind" && result["sessionId"] != intent.request["sessionId"] {
                bail!("The rewound conversation identity changed unexpectedly");
            }
            if intent.kind == "fork" && result["sessionId"] == intent.request["sessionId"] {
                bail!("The backend did not create an independent conversation");
            }
            intent.result.provider_cursor = Some(ProviderResumeCursor::DeerFlow {
                session_id: native_id.into(),
            });
            if intent.kind == "fork" {
                remap_fork_references(&mut intent.result, &result["referenceMap"])?;
            }
            intent.native_applied = true;
            self.persist(&intent)?;
        }
        if intent.kind == "fork" {
            for turn in &mut intent.result.turns {
                if let Some(checkpoint) = turn.checkpoint.as_mut() {
                    checkpoint.git_ref =
                        crate::checkpoint::checkpoint_ref(intent.result.id, checkpoint.turn_count);
                }
            }
            crate::checkpoint::copy_session_refs(
                &intent.cwd,
                intent.source.id,
                intent.result.id,
                intent.turn_count,
            )?;
        } else {
            drop(self.sessions.lock().remove(&intent.source.id));
        }
        {
            let mut state = self.state.lock();
            if let Some(existing) = state
                .sessions
                .iter_mut()
                .find(|session| session.id == intent.result.id)
            {
                *existing = intent.result.clone();
            } else {
                state.push_session(intent.result.clone());
            }
            state.mark_session_dirty(intent.result.id);
            self.store.save(&mut state).context("Native history changed but saving the desktop projection is pending; retry this action")?;
        }
        // The durable result is retained: process failure after the database save
        // can safely replay the same native operation and local projection.
        intent.completed = true;
        self.persist(&intent)?;
        if let Some(safety) = &intent.safety_ref {
            let _ = crate::checkpoint::delete_ref(&intent.cwd, safety);
        }
        Ok(intent.result)
    }

    fn finish_refused(&self, mut intent: HistoryIntent) -> anyhow::Result<AgentSession> {
        if let (Some(target), Some(safety)) = (&intent.restore_ref, &intent.safety_ref) {
            let current = crate::checkpoint::capture_worktree_commit(&intent.cwd)?;
            let expected = if crate::checkpoint::same_tree(&intent.cwd, &current, safety)? {
                safety
            } else {
                target
            };
            crate::checkpoint::restore_ref_if_unchanged(&intent.cwd, safety, expected).context(
                "The provider refused the operation, but restoring its safety snapshot is pending",
            )?;
        }
        intent.completed = true;
        intent.result = intent.source.clone();
        self.persist(&intent)?;
        if let Some(safety) = &intent.safety_ref {
            let _ = crate::checkpoint::delete_ref(&intent.cwd, safety);
        }
        bail!(
            intent
                .failure
                .unwrap_or_else(|| "The native history operation was refused".into())
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    struct TestRoot(PathBuf);
    impl TestRoot {
        fn path(&self) -> &Path {
            &self.0
        }
    }
    impl Drop for TestRoot {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.0);
        }
    }

    fn fixture() -> (TestRoot, HistoryContext, HistoryIntent) {
        let root = TestRoot(std::env::temp_dir().join(format!("waku-history-{}", Uuid::new_v4())));
        std::fs::create_dir_all(root.path()).unwrap();
        let store = Arc::new(StateStore::daemon(root.path().join("app.db")));
        let mut state = PersistedState::fresh(root.path().to_owned());
        let source = &mut state.sessions[0];
        source.provider = ProviderKind::DeerFlow;
        source.provider_cursor = Some(ProviderResumeCursor::DeerFlow {
            session_id: "native".into(),
        });
        source.begin_turn("retained until the native operation confirms");
        source.finish_active_turn(TurnStatus::Completed);
        let source = source.clone();
        store.save(&mut state).unwrap();
        let context = HistoryContext {
            store,
            state: Arc::new(Mutex::new(state)),
            sessions: Arc::new(Mutex::new(HashMap::new())),
        };
        let mut result = source.clone();
        result.truncate_after_turn(0);
        result.history_revision += 1;
        let intent = HistoryIntent {
            source,
            cwd: root.path().to_owned(),
            binary: "unused".into(),
            kind: "rewind".into(),
            turn_count: 1,
            request: json!({"sessionId":"native","expectedHeadEventId":"head","targetEventId":"","operationId":Uuid::new_v4()}),
            result,
            restore_ref: None,
            safety_ref: None,
            workspace_applied: false,
            native_applied: false,
            completed: false,
            previous_operation_ids: Vec::new(),
            operation_aliases: Vec::new(),
            failure: None,
        };
        context.persist(&intent).unwrap();
        (root, context, intent)
    }

    #[test]
    fn unknown_history_result_replays_the_same_request_after_restart() {
        let (root, context, intent) = fixture();
        let expected = intent.request.clone();
        assert!(
            context
                .resume_with(intent, |_| anyhow::bail!(
                    "unknown: disconnected after commit"
                ))
                .is_err()
        );
        let store = Arc::new(StateStore::daemon(root.path().join("app.db")));
        let state = store.load().unwrap();
        let restored = HistoryContext {
            store,
            state: Arc::new(Mutex::new(state)),
            sessions: Arc::new(Mutex::new(HashMap::new())),
        };
        let pending =
            serde_json::from_value(restored.store.history_operations().unwrap()[0].1.clone())
                .unwrap();
        let result = restored
            .resume_with(pending, |intent| {
                assert_eq!(intent.request, expected);
                Ok(json!({"sessionId":"native"}))
            })
            .unwrap();
        assert_eq!(result.history_revision, 1);
        assert!(result.messages.is_empty());
        let completed =
            serde_json::from_value(restored.store.history_operations().unwrap()[0].1.clone())
                .unwrap();
        restored
            .resume_with(completed, |_| {
                panic!("a completed operation must not call the provider")
            })
            .unwrap();
    }

    #[test]
    fn native_success_is_durable_before_projection_write_and_resumes_without_reapplying() {
        let (_root, context, intent) = fixture();
        let sql = rusqlite::Connection::open(context.store.path()).unwrap();
        sql.execute_batch("CREATE TRIGGER fail_history_projection BEFORE UPDATE ON sessions BEGIN SELECT RAISE(FAIL, 'test save interruption'); END;").unwrap();
        assert!(
            context
                .resume_with(intent, |_| Ok(json!({"sessionId":"native"})))
                .is_err()
        );
        let pending: HistoryIntent =
            serde_json::from_value(context.store.history_operations().unwrap()[0].1.clone())
                .unwrap();
        assert!(pending.native_applied);
        assert!(!pending.completed);
        sql.execute_batch("DROP TRIGGER fail_history_projection")
            .unwrap();
        let result = context
            .resume_with(pending, |_| {
                panic!("native application was already confirmed")
            })
            .unwrap();
        assert_eq!(result.history_revision, 1);
    }

    #[test]
    fn fork_projection_survives_completion_receipt_failure_without_a_duplicate() {
        let (_root, context, mut intent) = fixture();
        let source_id = intent.source.id;
        intent.kind = "fork".into();
        intent.result = intent.source.clone();
        intent.result.id = Uuid::new_v4();
        let result_id = intent.result.id;
        context.persist(&intent).unwrap();
        let sql = rusqlite::Connection::open(context.store.path()).unwrap();
        sql.execute_batch("CREATE TRIGGER fail_history_completion BEFORE UPDATE ON session_history_operations WHEN json_extract(NEW.data,'$.completed')=1 BEGIN SELECT RAISE(FAIL, 'test interrupted receipt'); END;").unwrap();
        assert!(
            context
                .resume_with(intent, |_| Ok(json!({"sessionId":"forked-native"})))
                .is_err()
        );
        assert_eq!(context.store.load().unwrap().sessions.len(), 2);
        let pending: HistoryIntent =
            serde_json::from_value(context.store.history_operation(source_id).unwrap().unwrap())
                .unwrap();
        assert!(pending.native_applied);
        sql.execute_batch("DROP TRIGGER fail_history_completion")
            .unwrap();
        let result = context
            .resume_with(pending, |_| panic!("the native fork is already durable"))
            .unwrap();
        assert_eq!(result.id, result_id);
        assert_eq!(context.store.load().unwrap().sessions.len(), 2);
    }

    #[test]
    fn completed_replay_returns_current_projection_and_rejects_deleted_targets() {
        let (_root, context, intent) = fixture();
        let id = intent.source.id;
        context
            .resume_with(intent, |_| Ok(json!({"sessionId":"native"})))
            .unwrap();
        let completed: HistoryIntent =
            serde_json::from_value(context.store.history_operation(id).unwrap().unwrap()).unwrap();
        {
            let mut state = context.state.lock();
            let session = state
                .sessions
                .iter_mut()
                .find(|session| session.id == id)
                .unwrap();
            session.begin_turn("new work after rewind");
            session.finish_active_turn(TurnStatus::Completed);
            state.mark_session_dirty(id);
            context.store.save(&mut state).unwrap();
        }
        let result = context
            .resume_with(completed.clone(), |_| {
                panic!("completed replay must stay local")
            })
            .unwrap();
        assert_eq!(result.turns.len(), 1);
        assert_eq!(result.messages[0].content, "new work after rewind");
        context.state.lock().sessions.clear();
        assert!(
            context
                .resume_with(completed, |_| panic!(
                    "deleted target must not be recreated"
                ))
                .unwrap_err()
                .to_string()
                .starts_with("rejected:")
        );
    }

    #[test]
    fn deleting_a_fork_erases_its_content_but_keeps_only_non_replayable_operation_ids() {
        let (_root, context, mut intent) = fixture();
        intent.kind = "fork".into();
        intent.result = intent.source.clone();
        intent.result.id = Uuid::new_v4();
        let source_id = intent.source.id;
        let target_id = intent.result.id;
        let operation_id = intent.request["operationId"].clone();
        context.persist(&intent).unwrap();
        context
            .resume_with(intent, |_| Ok(json!({"sessionId":"forked-native"})))
            .unwrap();
        let deletion = crate::persistence::SessionDeletion {
            session_id: target_id,
            operation_id: Uuid::new_v4(),
            native_session_id: Some("forked-native".into()),
            workspace_path: None,
            turn_count: 1,
            stage: "pending".into(),
        };
        context.store.begin_session_deletion(&deletion).unwrap();
        context
            .store
            .mark_session_backend_deleted(&deletion)
            .unwrap();
        context.store.complete_session_deletion(&deletion).unwrap();
        let receipt = context.store.history_operation(source_id).unwrap().unwrap();
        assert_eq!(
            receipt,
            json!({"completed":true,"deleted_result":true,"operation_ids":[operation_id]})
        );
        assert_eq!(context.store.load().unwrap().sessions.len(), 1);
    }

    #[test]
    fn a_definite_refusal_retains_the_original_projection() {
        let (_root, context, intent) = fixture();
        let source_id = intent.source.id;
        assert!(
            context
                .resume_with(intent, |_| anyhow::bail!(
                    "rejected: unresolved tool receipt"
                ))
                .is_err()
        );
        assert_eq!(
            context
                .state
                .lock()
                .sessions
                .iter()
                .find(|session| session.id == source_id)
                .unwrap()
                .history_revision,
            0
        );
        let saved: HistoryIntent =
            serde_json::from_value(context.store.history_operations().unwrap()[0].1.clone())
                .unwrap();
        assert!(saved.completed);
        assert!(!saved.native_applied);
        assert!(
            context
                .resume_with(saved, |_| panic!("refused receipts must not execute again"))
                .unwrap_err()
                .to_string()
                .starts_with("rejected:")
        );
    }

    #[test]
    fn operation_identity_replays_completed_results_and_rejects_reuse_and_expired_ids() {
        let (_root, _context, mut intent) = fixture();
        let id = Uuid::parse_str(intent.request["operationId"].as_str().unwrap()).unwrap();
        assert!(reuse_intent(&intent, "rewind", 1, Some(id)).unwrap());
        assert!(reuse_intent(&intent, "fork", 1, Some(id)).is_err());
        assert!(reuse_intent(&intent, "rewind", 1, Some(Uuid::new_v4())).unwrap());
        intent.completed = true;
        assert!(reuse_intent(&intent, "rewind", 1, Some(id)).unwrap());
        assert!(!reuse_intent(&intent, "rewind", 1, Some(Uuid::new_v4())).unwrap());
        let expired = Uuid::new_v4();
        intent.previous_operation_ids.push(expired);
        assert!(
            reuse_intent(&intent, "rewind", 1, Some(expired))
                .unwrap_err()
                .to_string()
                .contains("expired")
        );
    }

    #[test]
    fn fork_reference_map_rewrites_complete_embedded_references_without_cascading() {
        let old = "deerflow-asset://source/image-1";
        let new = "deerflow-asset://fork/image-2";
        let mut value = json!({
            "nested": [old, format!("See [{old}] and file:///old/image.blob"), "image-1", format!("{old}-unrelated")],
            old: old,
        });
        remap_value(
            &mut value,
            &[
                (old, new),
                (new, "deerflow-asset://wrong/image-3"),
                ("file:///old/image.blob", "file:///fork/image.blob"),
            ],
        );
        assert_eq!(value["nested"][0], new);
        assert_eq!(
            value["nested"][1],
            format!("See [{new}] and file:///fork/image.blob")
        );
        assert_eq!(value["nested"][2], "image-1");
        assert_eq!(value["nested"][3], format!("{old}-unrelated"));
        assert_eq!(value[old], new);
        let mut session = AgentSession::new(Uuid::new_v4(), ProviderKind::DeerFlow);
        assert!(remap_fork_references(&mut session, &json!({"image-1":"image-2"})).is_err());
    }

    #[test]
    fn pending_history_guards_both_source_and_result_and_release_after_completion() {
        let (_root, context, mut intent) = fixture();
        intent.result.id = Uuid::new_v4();
        context.persist(&intent).unwrap();
        assert!(context.store.has_pending_history(intent.source.id).unwrap());
        assert!(context.store.has_pending_history(intent.result.id).unwrap());
        assert_eq!(context.store.pending_history_operations().unwrap().len(), 1);
        intent.completed = true;
        context.persist(&intent).unwrap();
        assert!(!context.store.has_pending_history(intent.source.id).unwrap());
        assert!(!context.store.has_pending_history(intent.result.id).unwrap());
        assert!(
            context
                .store
                .pending_history_operations()
                .unwrap()
                .is_empty()
        );
    }

    #[test]
    fn boundaries_reject_divergence_and_failed_fork_targets() {
        let mut source = AgentSession::new(Uuid::new_v4(), ProviderKind::DeerFlow);
        let snapshot = NativeHistory {
            session_id: "native".into(),
            head_event_id: "head".into(),
            turns: Vec::new(),
        };
        assert_eq!(
            select_boundary(&source, &snapshot, "native", 0, false).unwrap(),
            ""
        );
        assert!(select_boundary(&source, &snapshot, "wrong", 0, false).is_err());
        source.begin_turn("test");
        assert!(select_boundary(&source, &snapshot, "native", 1, true).is_err());
    }
}
