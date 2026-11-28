use super::*;

fn workspace_ack(
    workspace: &waku_client::WorkspaceClient,
    operation: waku_client::WorkspaceOperation,
) -> anyhow::Result<()> {
    match workspace.request(operation)? {
        waku_client::WorkspaceResult::Ack => Ok(()),
        _ => anyhow::bail!("the daemon returned an invalid workspace response"),
    }
}

fn start_driver(mut request: DriverStartRequest, cwd: PathBuf) -> anyhow::Result<PreparedDriver> {
    request.options.cwd = cwd;
    let (event_tx, events) = driver::event_channel(request.event_wake);
    let handle = driver::start_remote(
        request.daemon,
        request.session_id,
        request.provider,
        request.options,
        event_tx,
    )?;
    Ok(PreparedDriver { handle, events })
}

fn attach_driver(
    daemon: waku_client::DaemonSupervisor,
    session_id: Uuid,
    event_wake: smol::channel::Sender<()>,
) -> anyhow::Result<Option<(AgentSession, PreparedDriver)>> {
    let Some(mut session) = waku_client::persistence::hydrate_session(&daemon, session_id)? else {
        return Ok(None);
    };
    let client = daemon.client();
    let response = client.request(session_id, Uuid::nil(), waku_client::Command::AttachSession)?;
    let waku_client::ResponsePayload::SessionRuntime {
        runtime_id,
        supports_steer,
        steering_run_id,
        conversation_capability,
        deerflow_ready,
        pending_history,
    } = response
    else {
        anyhow::bail!("Waku daemon returned an invalid runtime attachment response");
    };
    if let Some(operation) = pending_history {
        waku_client::WorkspaceClient::restore_pending_history(session_id, &operation);
    }
    let Some(runtime_id) = runtime_id else {
        return Ok(None);
    };
    session.conversation_capability = conversation_capability;
    let (event_tx, events) = driver::event_channel(event_wake);
    let _ = event_tx.send(DriverEvent::SteeringCapability {
        supported: supports_steer,
        run_id: steering_run_id,
    });
    if let Some(supported) = conversation_capability {
        let _ = event_tx.send(DriverEvent::ConversationCapability { supported });
    }
    if let Some(ready) = deerflow_ready {
        let _ = event_tx.send(DriverEvent::DeerFlowTasks(ready));
    }
    let handle = driver::attach_remote(
        daemon,
        client,
        session_id,
        runtime_id,
        supports_steer,
        session.runtime_event_cursor,
        event_tx,
    )?;
    Ok(Some((session, PreparedDriver { handle, events })))
}

fn load_remote_task_state(
    client: &waku_client::DaemonClient,
) -> anyhow::Result<RemoteTaskStateSnapshot> {
    let response = client.request(
        Uuid::nil(),
        Uuid::nil(),
        waku_client::Command::LoadTaskState,
    )?;
    let waku_client::ResponsePayload::TaskState {
        projects,
        mut sessions,
        ..
    } = response
    else {
        anyhow::bail!("Waku daemon returned an invalid task-state response");
    };
    for session in &mut sessions {
        session.detail_loaded = false;
    }
    Ok(RemoteTaskStateSnapshot { projects, sessions })
}

/// The running turn and the user message that opened it — the identity other
/// clients adopt when the daemon publishes this submission to the runtime.
pub(super) fn submitted_prompt_identity(session: &AgentSession) -> (Option<Uuid>, Option<Uuid>) {
    let Some(turn_id) = session.active_turn_id() else {
        return (None, None);
    };
    let message_id = session
        .messages
        .iter()
        .find(|message| message.turn_id == Some(turn_id) && message.role == MessageRole::User)
        .map(|message| message.id);
    (Some(turn_id), message_id)
}

pub(super) fn session_has_active_provider_turn(session: &AgentSession) -> bool {
    session.is_busy()
        && session
            .turns
            .last()
            .is_some_and(|turn| turn.status == TurnStatus::Running && turn.provider_turn_started)
}

/// Merge the daemon's list-only session projection into the desktop catalog.
///
/// Existing rows may already contain a hydrated transcript, so only list
/// metadata is copied from the projection. A locally attached runtime remains
/// authoritative for transient status and timestamps until its own events are
/// drained.
pub(super) fn merge_remote_session_catalog(
    local: &mut Vec<AgentSession>,
    remote: Vec<AgentSession>,
    has_local_runtime: impl Fn(Uuid) -> bool,
) -> Vec<Uuid> {
    let remote_ids = remote
        .iter()
        .map(|session| session.id)
        .collect::<HashSet<_>>();
    let removed = local
        .iter()
        .filter(|session| session.has_started() && !remote_ids.contains(&session.id))
        .map(|session| session.id)
        .collect::<Vec<_>>();
    local.retain(|session| !session.has_started() || remote_ids.contains(&session.id));

    for remote in remote {
        if let Some(local) = local.iter_mut().find(|session| session.id == remote.id) {
            local.title = remote.title;
            local.auto_title = remote.auto_title;
            local.project_id = remote.project_id;
            local.provider = remote.provider;
            local.model = remote.model;
            local.created_at = remote.created_at;
            local.last_reply_at = remote.last_reply_at;
            if !has_local_runtime(local.id) {
                local.status = remote.status;
                local.updated_at = remote.updated_at;
            }
        } else {
            local.push(remote);
        }
    }

    removed
}

/// Perform every blocking operation between accepting a submission and
/// starting its provider. This function is called only from the background
/// executor; the UI thread owns applying the returned workspace afterward.
enum PreparedIdleRuntime {
    Attached(AgentSession, PreparedDriver),
    Started(PreparedSubmission),
}

fn prepare_submission(
    workspace_client: waku_client::WorkspaceClient,
    project: Project,
    workspace: SessionWorkspace,
    driver_start: Option<anyhow::Result<DriverStartRequest>>,
    session_id: Uuid,
    prompt: &str,
    turn_count: usize,
) -> anyhow::Result<PreparedSubmission> {
    let workspace = match workspace {
        SessionWorkspace::NewWorktree { base_branch } => {
            if project.is_projectless() {
                anyhow::bail!("a projectless task cannot create a Git worktree");
            }
            let created =
                match workspace_client.request(waku_client::WorkspaceOperation::CreateWorktree {
                    project_path: project.path.clone(),
                    project_id: project.id,
                    session_id,
                    prompt: prompt.to_owned(),
                    base_branch,
                })? {
                    waku_client::WorkspaceResult::WorktreeCreated { worktree } => worktree,
                    _ => anyhow::bail!("the daemon returned an invalid worktree response"),
                };
            SessionWorkspace::Worktree {
                path: created.path,
                branch: created.branch,
            }
        }
        workspace => workspace,
    };
    let project_path = workspace.path().unwrap_or(&project.path);

    // Every turn gets its own immutable starting snapshot. Reusing the prior
    // response's ending ref would attribute branch switches or terminal edits
    // made between turns to the next response.
    let checkpoint_warning = workspace_ack(
        &workspace_client,
        waku_client::WorkspaceOperation::CaptureTurnStart {
            cwd: project_path.to_path_buf(),
            session_id,
            turn_count,
        },
    )
    .err();

    // Process startup can synchronously resolve executables, bind sockets,
    // and spawn children. It belongs behind the same animated preparation
    // boundary as Git work, otherwise the last spinner frame visibly freezes
    // just before Stop appears.
    let driver = driver_start.map(|request| {
        request.and_then(|request| start_driver(request, project_path.to_path_buf()))
    });

    Ok(PreparedSubmission {
        workspace,
        checkpoint_warning,
        driver,
    })
}

/// Everything a past-message resend needs after the UI accepts it.
///
/// The request owns only thread-safe snapshots. Git, provider RPCs, process
/// startup, and native transcript reads all happen in
/// [`perform_message_rewind`] on the background executor.
struct MessageRewindRequest {
    workspace_client: waku_client::WorkspaceClient,
    session_id: Uuid,
    turn_count: usize,
}

fn perform_message_rewind(request: MessageRewindRequest) -> Result<(AgentSession, Option<String>), String> {
    request.workspace_client.rewind_session_to_message(request.session_id, request.turn_count)
        .map_err(|error| error.to_string())
}


pub(super) fn recovery_notice_due(transport: Option<Duration>, session: Option<Duration>) -> bool {
    transport.max(session).is_some_and(|elapsed| elapsed >= Duration::from_secs(3))
}

impl Waku {
    pub(super) fn local_session_unavailable(&self, session_id: Uuid) -> bool {
        self.daemon.local_transport_unavailable()
            || self.runtimes.get(&session_id).is_some_and(|runtime| runtime.driver.is_local_reconnecting())
    }

    pub(super) fn show_local_recovery_notice(&self) -> bool {
        let session = self.selected_runtime()
            .and_then(|runtime| runtime.driver.local_reconnecting_since())
            .map(|since| since.elapsed());
        recovery_notice_due(self.daemon.local_recovery_elapsed(), session)
    }

    pub(super) fn update_local_recovery_notice(&mut self, cx: &mut Context<Self>) {
        let visible = self.show_local_recovery_notice();
        if self.local_recovery_notice_visible != visible {
            self.local_recovery_notice_visible = visible;
            cx.notify();
        }
        if self.local_action_notice.is_some_and(|(id, _)| !self.local_session_unavailable(id)) {
            self.local_action_notice = None;
            cx.notify();
        }
        for session_id in std::mem::take(&mut self.queues_waiting_for_connection) {
            self.drain_queued_message(session_id, cx);
        }
        if (self.retry_local_draft_save || self.retry_local_state_save) && !self.daemon.local_transport_unavailable() {
            if std::mem::take(&mut self.retry_local_draft_save) {
                self.schedule_composer_draft_save(cx);
            }
            if std::mem::take(&mut self.retry_local_state_save) {
                self.save();
            }
        }
    }

    pub(super) fn restart_task_state_sync(&self) {
        let clients = self.daemon.subscribe_clients();
        let results = self.task_state_sync_tx.clone();
        let event_wake = self.event_wake_tx.clone();
        std::thread::Builder::new()
            .name("waku-task-state-sync".into())
            .spawn(move || {
                let Ok(mut client) = clients.recv() else {
                    return;
                };
                loop {
                    while let Ok(newer) = clients.try_recv() {
                        client = newer;
                    }
                    let revisions = client.subscribe_task_state();
                    let result = load_remote_task_state(&client).map_err(|error| error.to_string());
                    if results.send(result).is_err() {
                        return;
                    }
                    signal_event_pump(&event_wake);
                    client = loop {
                        crossbeam_channel::select! {
                            recv(clients) -> replacement => {
                                let Ok(mut replacement) = replacement else {
                                    return;
                                };
                                while let Ok(newer) = clients.try_recv() {
                                    replacement = newer;
                                }
                                break replacement;
                            }
                            recv(revisions) -> revision => {
                                if revision.is_err() {
                                    // Managed replacement publishes the new
                                    // client after the old socket closes. Wait
                                    // for that publication instead of exiting
                                    // the task-state sync worker permanently.
                                    let Ok(replacement) = clients.recv() else {
                                        return;
                                    };
                                    break replacement;
                                }
                                while revisions.try_recv().is_ok() {}
                                let result = load_remote_task_state(&client)
                                    .map_err(|error| error.to_string());
                                if results.send(result).is_err() {
                                    return;
                                }
                                signal_event_pump(&event_wake);
                            }
                        }
                    };
                }
            })
            .ok();
    }

    fn drain_task_state_sync_events(&mut self, cx: &mut Context<Self>) -> bool {
        let mut latest = None;
        while let Ok(result) = self.task_state_sync_events.try_recv() {
            latest = Some(result);
        }
        let Some(result) = latest else {
            return false;
        };
        match result {
            Ok(snapshot) => {
                self.apply_remote_task_state(snapshot, cx);
                true
            }
            Err(error) => {
                eprintln!("could not refresh daemon task state: {error}");
                false
            }
        }
    }

    fn apply_remote_task_state(
        &mut self,
        snapshot: RemoteTaskStateSnapshot,
        cx: &mut Context<Self>,
    ) {
        let runtime_ids = self.runtimes.keys().copied().collect::<HashSet<_>>();
        let removed = merge_remote_session_catalog(
            &mut self.state.sessions,
            snapshot.sessions,
            |session_id| runtime_ids.contains(&session_id),
        );
        for session_id in &removed {
            self.runtime_attach_pending.remove(session_id);
            self.runtime_attach_misses.remove(session_id);
            self.runtimes.remove(session_id);
            self.background_work.remove(session_id);
            self.deerflow_tasks.remove(session_id);
            self.remove_right_panel_session_state(*session_id);
            self.task_switcher.remove(*session_id);
        }
        self.state.projects = snapshot.projects;

        let attach = self
            .state
            .sessions
            .iter()
            .filter(|session| {
                session.status.is_busy()
                    || (self.state.selected_session == Some(session.id) && session.has_started())
            })
            .map(|session| session.id)
            .collect::<Vec<_>>();
        for session_id in attach {
            self.start_runtime_attachment(session_id, cx);
        }

        if self.state.selected_session.is_some_and(|selected| {
            !self
                .state
                .sessions
                .iter()
                .any(|session| session.id == selected)
        }) {
            let previous_project = self.state.selected_project;
            self.state.selected_session = None;
            let next = self
                .state
                .sessions
                .iter()
                .filter(|session| {
                    previous_project.is_none_or(|project| session.project_id == project)
                })
                .max_by_key(|session| session.updated_at)
                .map(|session| session.id)
                .or_else(|| {
                    self.state
                        .sessions
                        .iter()
                        .max_by_key(|session| session.updated_at)
                        .map(|session| session.id)
                });
            if let Some(next) = next {
                self.select_session(next, cx);
            } else if let Some(project_id) = self
                .state
                .selected_project
                .filter(|project_id| {
                    self.state
                        .projects
                        .iter()
                        .any(|project| project.id == *project_id)
                })
                .or_else(|| self.state.projects.first().map(|project| project.id))
            {
                self.state.selected_project = Some(project_id);
                self.create_session_for(project_id, cx);
            }
        }
    }

    pub(super) fn start_runtime_attachment(&mut self, session_id: Uuid, cx: &mut Context<Self>) {
        if self.runtimes.contains_key(&session_id)
            || !self.runtime_attach_pending.insert(session_id)
        {
            return;
        }
        let daemon = self.daemon.clone();
        let event_wake = self.event_wake_tx.clone();
        cx.spawn(async move |waku, cx| {
            let result = cx
                .background_executor()
                .spawn(async move { attach_driver(daemon, session_id, event_wake) })
                .await;
            let _ = waku.update(cx, move |waku, cx| {
                waku.finish_runtime_attachment(session_id, result, cx);
            });
        })
        .detach();
    }

    fn finish_runtime_attachment(
        &mut self,
        session_id: Uuid,
        result: anyhow::Result<Option<(AgentSession, PreparedDriver)>>,
        cx: &mut Context<Self>,
    ) {
        if !self.runtime_attach_pending.remove(&session_id) {
            return;
        }
        match result {
            Ok(Some((session, prepared))) => {
                self.runtime_attach_misses.remove(&session_id);
                let Some(index) = self
                    .state
                    .sessions
                    .iter()
                    .position(|candidate| candidate.id == session_id)
                else {
                    return;
                };
                if !self.runtimes.contains_key(&session_id) {
                    self.state.sessions[index] = session;
                    self.install_prepared_driver(session_id, prepared);
                    if self.state.selected_session == Some(session_id) {
                        self.reset_visible_state();
                        self.reset_transcript_rows(self.transcript_row_count());
                    }
                    cx.notify();
                }
            }
            Ok(None) => {
                // Attach restores a pending history action even without a
                // provider process. Notify so the exact retry becomes usable.
                cx.notify();
                let busy = self
                    .state
                    .sessions
                    .iter()
                    .find(|session| session.id == session_id)
                    .is_some_and(|session| session.status.is_busy());
                if !busy {
                    self.runtime_attach_misses.remove(&session_id);
                    if self.state.selected_session == Some(session_id) {
                        self.maybe_refresh_deerflow_tasks(cx);
                    }
                    return;
                }
                let misses = self.runtime_attach_misses.entry(session_id).or_default();
                *misses = misses.saturating_add(1);
                if *misses < 4 {
                    cx.spawn(async move |waku, cx| {
                        cx.background_executor()
                            .timer(Duration::from_millis(250))
                            .await;
                        let _ = waku.update(cx, |waku, cx| {
                            waku.start_runtime_attachment(session_id, cx);
                        });
                    })
                    .detach();
                } else {
                    self.runtime_attach_misses.remove(&session_id);
                    self.interrupt_orphaned_runtime(session_id, cx);
                }
            }
            Err(error) => {
                eprintln!("could not attach desktop to daemon session {session_id}: {error:#}");
            }
        }
    }

    fn interrupt_orphaned_runtime(&mut self, session_id: Uuid, cx: &mut Context<Self>) {
        let project_paths = self
            .state
            .projects
            .iter()
            .map(|project| (project.id, project.path.clone()))
            .collect::<HashMap<_, _>>();
        let mut checkpoint = None;
        if let Some(session) = self.state.session_mut(session_id) {
            if !session.status.is_busy() {
                return;
            }
            session.status = SessionStatus::Idle;
            let interrupted_turn_count = session
                .turns
                .last_mut()
                .filter(|turn| turn.status == TurnStatus::Running)
                .map(|turn| {
                    turn.status = TurnStatus::Interrupted;
                    turn.completed_at = Some(unix_time());
                    turn.turn_count
                });
            if let Some(turn_count) = interrupted_turn_count {
                let project_path = session
                    .workspace
                    .path()
                    .map(Path::to_path_buf)
                    .or_else(|| project_paths.get(&session.project_id).cloned());
                checkpoint = project_path.map(|project_path| PendingCheckpointCapture {
                    session_id,
                    turn_count,
                    project_path,
                });
            }
            for message in &mut session.messages {
                message.streaming = false;
            }
            for block in &mut session.transcript_blocks {
                block.activities.retain(|activity| {
                    activity
                        .reasoning
                        .as_ref()
                        .is_none_or(|reasoning| !reasoning.content.trim().is_empty())
                });
                for activity in &mut block.activities {
                    activity.complete = true;
                }
            }
            session
                .transcript_blocks
                .retain(|block| !block.activities.is_empty());
        }
        if let Some(checkpoint) = checkpoint {
            self.pending_checkpoint_captures.push(checkpoint);
            self.start_pending_checkpoint_captures(cx);
        }
        if self.state.selected_session == Some(session_id) {
            self.reset_visible_state();
            self.reset_transcript_rows(self.transcript_row_count());
        }
        self.save();
        cx.notify();
    }

    pub fn composer_focus(&self, cx: &App) -> FocusHandle {
        self.composer.read(cx).focus()
    }

    pub(super) fn selected_project(&self) -> Option<&Project> {
        let id = self.state.selected_project?;
        self.state.projects.iter().find(|project| project.id == id)
    }

    pub(super) fn selected_session(&self) -> Option<&AgentSession> {
        let id = self.state.selected_session?;
        self.state.sessions.iter().find(|session| session.id == id)
    }

    fn active_turn_finished_event(
        &self,
        session_id: Uuid,
        outcome: crate::analytics::TurnOutcome,
    ) -> Option<crate::analytics::Event> {
        let session = self
            .state
            .sessions
            .iter()
            .find(|session| session.id == session_id)?;
        let turn = session
            .turns
            .last()
            .filter(|turn| turn.status == TurnStatus::Running)?;
        Some(crate::analytics::Event::TurnFinished {
            provider: session.provider.id(),
            turn_number: turn.turn_count,
            outcome,
            duration_seconds: unix_time().saturating_sub(turn.started_at),
        })
    }

    /// Completes a persisted turn and emits its anonymous outcome exactly
    /// once. All production turn-settlement paths go through this seam.
    pub(super) fn finish_active_turn_with_analytics(
        &mut self,
        session_id: Uuid,
        status: TurnStatus,
        outcome: crate::analytics::TurnOutcome,
    ) -> Option<(Uuid, usize)> {
        let event = self.active_turn_finished_event(session_id, outcome);
        let result = self
            .state
            .session_mut(session_id)?
            .finish_active_turn(status);
        if result.is_some()
            && let Some(event) = event
        {
            self.analytics.track(event);
        }
        result
    }

    /// Records a failed submission that is about to be unwound and therefore
    /// will not remain as a persisted turn.
    fn track_active_turn_outcome(&self, session_id: Uuid, outcome: crate::analytics::TurnOutcome) {
        if let Some(event) = self.active_turn_finished_event(session_id, outcome) {
            self.analytics.track(event);
        }
    }

    /// The directory every filesystem and provider operation for `session`
    /// must use. A not-yet-materialized worktree draft deliberately reads the
    /// local checkout until its first submission creates the isolated copy.
    pub(super) fn workspace_path_for_session<'a>(
        &'a self,
        session: &'a AgentSession,
    ) -> Option<&'a std::path::Path> {
        let project = self
            .state
            .projects
            .iter()
            .find(|project| project.id == session.project_id)?;
        Some(session.workspace.path().unwrap_or(&project.path))
    }

    pub(super) fn selected_workspace_path(&self) -> Option<&std::path::Path> {
        let session = self.selected_session()?;
        self.workspace_path_for_session(session)
    }

    /// Marks the session for the next save; see `PersistedState::session_mut`.
    pub(super) fn selected_session_mut(&mut self) -> Option<&mut AgentSession> {
        let id = self.state.selected_session?;
        self.state.session_mut(id)
    }

    pub(super) fn selected_runtime(&self) -> Option<&SessionRuntime> {
        self.runtimes.get(&self.state.selected_session?)
    }

    pub(super) fn provider_probe(&self, provider: ProviderKind) -> Option<&ProviderProbe> {
        self.probes.iter().find(|probe| probe.provider == provider)
    }

    pub(super) fn request_provider_model_discovery(&mut self, provider: ProviderKind) {
        if !provider.supports_model_discovery()
            || self.provider_model_discoveries.contains(&provider)
        {
            return;
        }
        let Some(probe) = self
            .provider_probe(provider)
            .filter(|probe| probe.installed)
            .cloned()
        else {
            return;
        };
        self.provider_model_discoveries.insert(provider);
        self.provider_model_discoveries_pending.insert(provider);
        let provider_probe_tx = self.provider_probe_tx.clone();
        let event_wake = self.event_wake_tx.clone();
        let daemon = self.daemon.client();
        let binary_override = self.state.provider_binary_overrides.get(&provider).cloned();
        if std::thread::Builder::new()
            .name(format!("waku-{}-model-discovery", provider.id()))
            .spawn(move || {
                let discovered = match daemon.request(
                    Uuid::nil(),
                    Uuid::nil(),
                    waku_client::Command::ProbeProvider {
                        provider,
                        binary_override,
                        discover_models: true,
                        probe_version: false,
                    },
                ) {
                    Ok(waku_client::ResponsePayload::ProviderProbe { probe, .. }) => probe,
                    _ => probe,
                };
                if provider_probe_tx.send(discovered).is_ok() {
                    signal_event_pump(&event_wake);
                }
            })
            .is_err()
        {
            self.provider_model_discoveries.remove(&provider);
            self.provider_model_discoveries_pending.remove(&provider);
        }
    }

    /// Re-run one provider's model-owned catalog discovery, for selectors whose
    /// contents can change while Waku stays open — models the user just
    /// authored in a provider's config, or DeepSeek's custom agent presets.
    /// The stale catalog stays on screen until the fresh probe lands, so an
    /// open menu never blanks into a loading state while it refreshes.
    pub(super) fn refresh_provider_model_discovery(&mut self, provider: ProviderKind) {
        if self.provider_model_discoveries_pending.contains(&provider) {
            return;
        }
        self.provider_model_discoveries.remove(&provider);
        self.request_provider_model_discovery(provider);
    }

    /// Ask every installed CLI for its version, one short-lived subprocess per
    /// provider on its own thread. Answers land in `provider_versions` through
    /// the drain loop; render reads only that map.
    pub(super) fn request_provider_version_probes(&mut self) {
        let targets = self
            .probes
            .iter()
            .filter(|probe| probe.installed)
            .map(|probe| probe.provider)
            .collect::<Vec<_>>();
        for provider in targets {
            if !self.provider_version_probes_pending.insert(provider) {
                continue;
            }
            let provider_version_tx = self.provider_version_tx.clone();
            let event_wake = self.event_wake_tx.clone();
            let daemon = self.daemon.client();
            let binary_override = self.state.provider_binary_overrides.get(&provider).cloned();
            if std::thread::Builder::new()
                .name(format!("waku-{}-version-probe", provider.id()))
                .spawn(move || {
                    let version = match daemon.request(
                        Uuid::nil(),
                        Uuid::nil(),
                        waku_client::Command::ProbeProvider {
                            provider,
                            binary_override,
                            discover_models: false,
                            probe_version: true,
                        },
                    ) {
                        Ok(waku_client::ResponsePayload::ProviderProbe { version, .. }) => version,
                        _ => None,
                    };
                    if provider_version_tx.send((provider, version)).is_ok() {
                        signal_event_pump(&event_wake);
                    }
                })
                .is_err()
            {
                self.provider_version_probes_pending.remove(&provider);
            }
        }
    }

    pub(super) fn drain_provider_version_events(&mut self) -> bool {
        let mut changed = false;
        while let Ok((provider, version)) = self.provider_version_events.try_recv() {
            self.provider_version_probes_pending.remove(&provider);
            self.provider_versions.insert(provider, version);
            changed = true;
        }
        changed
    }

    /// Re-detect provider CLIs off-thread — every provider for the Providers
    /// page's refresh, or one whose binary override just changed. Also re-runs
    /// model discovery and version probes for whatever the detection finds
    /// installed.
    pub(super) fn refresh_provider_detection(&mut self, scope: Option<ProviderKind>) {
        if self.provider_detection_remaining > 0 {
            return;
        }
        let providers = match scope {
            Some(provider) => vec![provider],
            None => ProviderKind::ALL.to_vec(),
        };
        self.provider_detection_remaining = providers.len();
        let overrides = self.state.provider_binary_overrides.clone();
        let provider_detection_tx = self.provider_detection_tx.clone();
        let event_wake = self.event_wake_tx.clone();
        let detect_providers = providers.clone();
        let daemon = self.daemon.client();
        if std::thread::Builder::new()
            .name("waku-provider-detection".into())
            .spawn(move || {
                for provider in detect_providers {
                    let response = daemon.request(
                        Uuid::nil(),
                        Uuid::nil(),
                        waku_client::Command::ProbeProvider {
                            provider,
                            binary_override: overrides.get(&provider).cloned(),
                            discover_models: false,
                            probe_version: false,
                        },
                    );
                    let probe = match response {
                        Ok(waku_client::ResponsePayload::ProviderProbe { probe, .. }) => probe,
                        _ => ProviderProbe {
                            provider,
                            installed: false,
                            path: None,
                            models: Vec::new(),
                            agent_presets: Vec::new(),
                        },
                    };
                    if provider_detection_tx.send(probe).is_ok() {
                        signal_event_pump(&event_wake);
                    }
                }
            })
            .is_err()
        {
            self.provider_detection_remaining = 0;
            return;
        }
        // A refresh means "re-check everything about these providers":
        // clearing the per-launch guard lets each one's catalog discovery run
        // again as its detection lands below.
        for provider in providers {
            self.provider_model_discoveries.remove(&provider);
        }
    }

    pub(super) fn drain_provider_detection_events(&mut self) -> bool {
        let mut changed = false;
        let mut installed_providers = Vec::new();
        while let Ok(probe) = self.provider_detection_events.try_recv() {
            let provider = probe.provider;
            let installed = probe.installed;
            self.provider_detection_remaining = self.provider_detection_remaining.saturating_sub(1);
            if self.provider_detection_remaining == 0 {
                self.provider_detection_checked_at = Some(Instant::now());
            }
            if let Some(existing) = self
                .probes
                .iter_mut()
                .find(|existing| existing.provider == provider)
            {
                if self.provider_model_discoveries_pending.contains(&provider) {
                    // A manual refresh may overlap an older live discovery.
                    // Keep that newer catalog while still accepting PATH
                    // detection from this response.
                    existing.installed = probe.installed;
                    existing.path = probe.path;
                } else {
                    *existing = probe;
                }
            } else {
                self.probes.push(probe);
            }
            if installed {
                installed_providers.push(provider);
            } else {
                self.provider_versions.remove(&provider);
            }
            changed = true;
        }
        for provider in installed_providers {
            self.request_provider_model_discovery(provider);
        }
        if changed {
            self.request_provider_version_probes();
        }
        changed
    }

    /// Whether the provider can back a new session: installed and not switched
    /// off in the Providers settings.
    pub(super) fn provider_enabled(&self, provider: ProviderKind) -> bool {
        !self.state.disabled_providers.contains(&provider)
            && self
                .provider_probe(provider)
                .is_some_and(|probe| probe.installed)
    }

    /// Whether the model picker has no provider left to offer — nothing
    /// detected on this machine, or everything switched off — so the
    /// composer's trigger, the picker panel, and the send button all swap to
    /// their unavailable state.
    pub(super) fn model_picker_has_no_providers(&self) -> bool {
        let locked_provider = self
            .selected_session()
            .filter(|session| !session.messages.is_empty())
            .map(|session| session.provider);
        super::composer::picker_has_no_providers(
            &self.probes,
            &self.state.disabled_providers,
            locked_provider,
            self.provider_detection_checked_at.is_some(),
        )
    }

    pub(super) fn model_for_session<'a>(&'a self, session: &'a AgentSession) -> Option<&'a str> {
        session.model.as_deref().or_else(|| {
            self.provider_probe(session.provider)
                .and_then(ProviderProbe::preferred_model)
                .map(|model| model.id.as_str())
        })
    }

    pub(super) fn model_display_name(&self, provider: ProviderKind, model: Option<&str>) -> String {
        let Some(model) = model else {
            return provider.short_name().to_owned();
        };
        self.provider_probe(provider)
            .and_then(|probe| probe.model(model))
            .map(|candidate| candidate.name.clone())
            .unwrap_or_else(|| model.to_owned())
    }

    pub(super) fn model_metadata_for_session(
        &self,
        session: &AgentSession,
    ) -> Option<&ProviderModel> {
        let model = self.model_for_session(session)?;
        self.provider_probe(session.provider)?.model(model)
    }

    pub(super) fn catalog_model_id_for_session<'a>(
        &'a self,
        session: &'a AgentSession,
    ) -> Option<&'a str> {
        self.model_metadata_for_session(session)
            .map(|model| model.id.as_str())
            .or_else(|| self.model_for_session(session))
    }

    pub(super) fn selected_transcript_blocks(&self) -> &[TranscriptBlock] {
        self.selected_session()
            .map(|session| session.transcript_blocks.as_slice())
            .unwrap_or(&[])
    }

    pub(super) fn save(&mut self) {
        self.last_stream_save = Instant::now();
        let daemon_error = self
            .daemon
            .update_settings(self.state.daemon_settings())
            .err()
            .map(|error| error.to_string());
        let app_error = match self.store.save(&mut self.state) {
            Err(error) if !self.daemon.is_remote() && error.kind() == std::io::ErrorKind::ConnectionAborted => {
                self.retry_local_state_save = true;
                None
            }
            outcome => outcome.err().map(|error| error.to_string()),
        };
        if let Some(error) = daemon_error.or(app_error) {
            self.show_toast(tr!("errors.save_local_state", error = error));
        } else if !self.retry_local_state_save {
            self.stream_state_dirty = false;
        }
    }

    fn checkpoint_capture_pending(&self, session_id: Uuid, turn_count: usize) -> bool {
        self.checkpoint_captures_in_flight
            .contains(&(session_id, turn_count))
            || self
                .pending_checkpoint_captures
                .iter()
                .any(|capture| capture.session_id == session_id && capture.turn_count == turn_count)
    }

    fn ending_checkpoint_pending(&self, session_id: Uuid) -> bool {
        self.state
            .sessions
            .iter()
            .find(|session| session.id == session_id)
            .and_then(|session| session.turns.last())
            .filter(|turn| turn.status != TurnStatus::Running)
            .is_some_and(|turn| self.checkpoint_capture_pending(session_id, turn.turn_count))
    }

    fn defer_queue_drain(&mut self, session_id: Uuid) {
        if !self.pending_queue_drains.contains(&session_id) {
            self.pending_queue_drains.push(session_id);
        }
    }

    /// Queues the newest finished turn's checkpoint for capture.
    ///
    /// Bookkeeping only. The capture itself is upwards of ten `git`
    /// invocations, one of them a `git add -A` over the whole worktree, and the
    /// hottest caller is the driver-event drain that shares the UI thread with
    /// rendering — so the work belongs to
    /// [`Self::start_pending_checkpoint_captures`], which every caller that
    /// holds a `Context` runs straight after queueing.
    pub(super) fn capture_latest_turn_checkpoint_for(&mut self, session_id: Uuid) {
        let Some((session, turn_count)) = self
            .state
            .sessions
            .iter()
            .find(|session| session.id == session_id)
            .and_then(|session| {
                session
                    .turns
                    .last()
                    .filter(|turn| turn.status != TurnStatus::Running)
                    .map(|turn| (session, turn.turn_count))
            })
        else {
            return;
        };
        if self.checkpoint_capture_pending(session_id, turn_count) {
            return;
        }
        let Some(project_path) = self
            .workspace_path_for_session(session)
            .map(std::path::Path::to_path_buf)
        else {
            return;
        };
        self.pending_checkpoint_captures
            .push(PendingCheckpointCapture {
                session_id,
                turn_count,
                project_path,
            });
    }

    /// Runs queued turn checkpoints on the background executor.
    ///
    /// A capture lands a frame or many later, and the turn it belongs to may be
    /// gone by then, so the result is matched back by turn count rather than
    /// position. Nothing on screen waits for it: the transcript's rewind
    /// affordance appears when `invalidate_checkpoint_refs` prompts the next
    /// prefetch to notice the new ref.
    pub(super) fn start_pending_checkpoint_captures(&mut self, cx: &mut Context<Self>) {
        for request in std::mem::take(&mut self.pending_checkpoint_captures) {
            let PendingCheckpointCapture {
                session_id,
                turn_count,
                project_path,
            } = request;
            if !self
                .checkpoint_captures_in_flight
                .insert((session_id, turn_count))
            {
                continue;
            }
            let workspace = waku_client::WorkspaceClient::new(self.daemon.client());
            cx.spawn(async move |waku, cx| {
                let captured = cx
                    .background_executor()
                    .spawn({
                        let project_path = project_path.clone();
                        async move {
                            match workspace.request(
                                waku_client::WorkspaceOperation::CaptureTurn {
                                    cwd: project_path,
                                    session_id,
                                    turn_count,
                                },
                            )? {
                                waku_client::WorkspaceResult::Checkpoint { checkpoint } => {
                                    Ok(checkpoint)
                                }
                                _ => anyhow::bail!(
                                    "the daemon returned an invalid checkpoint response"
                                ),
                            }
                        }
                    })
                    .await;
                waku.update(cx, |waku, cx| {
                    waku.checkpoint_captures_in_flight
                        .remove(&(session_id, turn_count));
                    let selected = waku.state.selected_session == Some(session_id);
                    if selected {
                        waku.sync_transcript_rows();
                    }
                    let previous_kinds = if selected {
                        waku.transcript_row_kinds.borrow().clone()
                    } else {
                        Vec::new()
                    };
                    let checkpoint = match captured {
                        Ok(checkpoint) => checkpoint,
                        Err(error) => {
                            if waku.daemon.is_remote() || !waku_client::DaemonClient::is_transport_lost(&error) {
                                waku.show_toast(tr!("errors.capture_turn_checkpoint", error = error));
                            }
                            Checkpoint {
                                turn_count,
                                git_ref: checkpoint::checkpoint_ref(session_id, turn_count),
                                status: CheckpointStatus::Error,
                                files: Vec::new(),
                                additions: 0,
                                deletions: 0,
                                created_at: unix_time(),
                            }
                        }
                    };
                    waku.invalidate_checkpoint_refs();
                    let mut attached_turn_id = None;
                    if let Some(session) = waku.state.session_mut(session_id)
                        && let Some(turn) = session
                            .turns
                            .iter_mut()
                            .find(|turn| turn.turn_count == turn_count)
                    {
                        turn.checkpoint = Some(checkpoint);
                        attached_turn_id = Some(turn.id);
                    }
                    if let Some(turn_id) = attached_turn_id
                        && selected
                    {
                        // Reconcile a standalone card by row identity, then
                        // remeasure the terminal response when the card is
                        // hosted inline before its footer.
                        waku.splice_transcript_rows_after_visibility_change(&previous_kinds);
                        waku.remeasure_changed_files(turn_id);
                    }
                    let resume_queue = waku.pending_queue_drains.contains(&session_id);
                    if resume_queue {
                        waku.pending_queue_drains.retain(|id| *id != session_id);
                        waku.drain_queued_message(session_id, cx);
                    }
                    cx.notify();
                    if attached_turn_id.is_some() {
                        // Let the new transcript row paint before SQLite work.
                        // Without this save, a checkpoint that lands after the
                        // turn's final stream save can disappear on relaunch.
                        cx.spawn(async move |waku, cx| {
                            cx.background_executor().timer(STREAM_FRAME_INTERVAL).await;
                            let _ = waku.update(cx, |waku, _| waku.save());
                        })
                        .detach();
                    }
                })
                .ok();
            })
            .detach();
        }
    }

    pub(super) fn fork_session_from_response(
        &mut self,
        session_id: Uuid,
        turn_count: usize,
        cx: &mut Context<Self>,
    ) {
        if self.response_fork_preparations.contains_key(&session_id)
            || self.submission_preparations.contains(&session_id)
        {
            self.show_toast(tr!("session.response_cannot_fork"));
            cx.notify();
            return;
        }
        let Some(source) = self
            .state
            .sessions
            .iter()
            .find(|session| session.id == session_id)
            .cloned()
        else {
            self.show_toast(tr!("session.response_unavailable"));
            cx.notify();
            return;
        };
        if self.state.selected_session != Some(session_id)
            || !matches!(source.status, SessionStatus::Idle | SessionStatus::Failed)
            || !source.provider.supports_conversation_fork()
            || !waku_client::WorkspaceClient::fork_action_enabled(
                session_id,
                turn_count,
                source.conversation_actions_enabled(),
            )
            || source
                .turns
                .get(turn_count.saturating_sub(1))
                .is_none_or(|turn| turn.turn_count != turn_count || !turn.provider_turn_started)
        {
            self.show_toast(tr!("session.response_cannot_fork"));
            cx.notify();
            return;
        }
        let provider = source.provider;
        let workspace = waku_client::WorkspaceClient::new(self.daemon.client());

        self.response_fork_preparations
            .insert(session_id, turn_count);
        self.hide_toast();
        cx.notify();

        cx.spawn(async move |waku, cx| {
            let result = cx
                .background_executor()
                .spawn(async move { workspace.fork_session_from_response(session_id, turn_count).map_err(|error| error.to_string()) })
                .await;
            let _ = waku.update(cx, move |waku, cx| {
                waku.finish_response_fork(session_id, turn_count, provider, result, cx);
            });
        })
        .detach();
    }

    fn finish_response_fork(
        &mut self,
        session_id: Uuid,
        turn_count: usize,
        provider: ProviderKind,
        result: Result<(AgentSession, Option<String>), String>,
        cx: &mut Context<Self>,
    ) {
        if self.response_fork_preparations.get(&session_id) != Some(&turn_count) {
            return;
        }
        self.response_fork_preparations.remove(&session_id);

        let (forked, checkpoint_warning) = match result {
            Ok(prepared) => prepared,
            Err(error) => {
                self.drain_queued_message(session_id, cx);
                self.show_toast(error);
                cx.notify();
                return;
            }
        };

        self.invalidate_checkpoint_refs();

        let fork_id = forked.id;
        self.state.push_session(forked);
        self.analytics
            .track(crate::analytics::Event::ResponseForked {
                provider: provider.id(),
                turn_number: turn_count,
            });
        self.select_session(fork_id, cx);
        self.drain_queued_message(session_id, cx);
        match checkpoint_warning {
            Some(error) => {
                self.show_toast(tr!("session.forked_with_checkpoint_warning", error = error))
            }
            None => self.show_success_toast(tr!("session.forked_from_response")),
        }
        cx.notify();
    }

    pub(super) fn begin_message_edit(
        &mut self,
        action: UserMessageAction,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let UserMessageAction {
            session_id,
            message_id,
            turn_count,
            ..
        } = action;
        let Some((message_index, initial_message, attachments)) = self
            .state
            .sessions
            .iter()
            .find(|session| {
                session.id == session_id
                    && session.provider.supports_conversation_rollback()
                    && waku_client::WorkspaceClient::rewind_action_enabled(
                        session_id,
                        turn_count,
                        session.conversation_actions_enabled(),
                    )
                    && matches!(session.status, SessionStatus::Idle | SessionStatus::Failed)
            })
            .and_then(|session| {
                let turn = session
                    .turns
                    .iter()
                    .find(|turn| turn.turn_count == turn_count)?;
                session
                    .messages
                    .iter()
                    .enumerate()
                    .find_map(|(index, message)| {
                        (message.id == message_id
                            && message.turn_id == Some(turn.id)
                            && message.role == MessageRole::User)
                            .then(|| {
                                (
                                    index,
                                    message.visible_content().to_owned(),
                                    message.attachments.clone(),
                                )
                            })
                    })
            })
        else {
            self.show_toast(tr!("session.message_not_editable"));
            cx.notify();
            return;
        };

        let input = cx.new(|cx| ComposerInput::new(window, cx).padding_x(px(12.0), cx));
        input.update(cx, |input, cx| input.set_content(initial_message, cx));
        cx.subscribe(
            &input,
            |this: &mut Self, _, event: &ComposerEvent, cx| match event {
                ComposerEvent::Submit(prompt) => {
                    this.submit_message_edit_prompt(prompt.clone(), cx)
                }
                // An edited past message resubmits from that point; there is
                // no running turn for it to steer.
                ComposerEvent::SubmitSteer(prompt) => {
                    this.submit_message_edit_prompt(prompt.clone(), cx)
                }
                ComposerEvent::SteerQueued => {}
                ComposerEvent::Edited => cx.notify(),
                ComposerEvent::Focus => {}
                ComposerEvent::BackspaceOnEmpty => {}
            },
        )
        .detach();
        self.message_edit = Some(MessageEdit {
            session_id,
            message_id,
            turn_count,
            input: input.clone(),
            attachments,
        });
        self.hide_toast();
        self.remeasure_transcript_message(message_index);
        let focus_handle = input.read(cx).focus();
        window.focus(&focus_handle, cx);
        cx.notify();
    }

    pub(super) fn cancel_message_edit(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self
            .message_edit
            .as_ref()
            .is_some_and(|edit| self.submission_preparations.contains(&edit.session_id))
        {
            return;
        }
        let Some(edit) = self.message_edit.take() else {
            return;
        };
        let message_index = self.selected_session().and_then(|session| {
            session
                .messages
                .iter()
                .position(|message| message.id == edit.message_id)
        });
        if let Some(message_index) = message_index {
            self.remeasure_transcript_message(message_index);
        }
        let focus_handle = self.composer_focus(cx);
        window.focus(&focus_handle, cx);
        cx.notify();
    }

    pub(super) fn submit_message_edit(&mut self, cx: &mut Context<Self>) {
        let prompt = self
            .message_edit
            .as_ref()
            .map(|edit| edit.input.read(cx).content(cx).to_owned())
            .unwrap_or_default();
        self.submit_message_edit_prompt(prompt, cx);
    }

    fn submit_message_edit_prompt(&mut self, prompt: String, cx: &mut Context<Self>) {
        let Some(edit) = self.message_edit.clone() else {
            return;
        };
        if self.submission_preparations.contains(&edit.session_id) || self.local_session_unavailable(edit.session_id) {
            return;
        }
        let prompt = prompt.trim().to_owned();
        if prompt.is_empty() && edit.attachments.is_empty() {
            self.show_toast(tr!("session.edited_message_empty"));
            cx.notify();
            return;
        }
        let mentions = edit
            .attachments
            .iter()
            .map(|attachment| attachment.mention.clone())
            .collect::<Vec<_>>();
        let provider_prompt = composer::merged_submission(&prompt, &mentions)
            .expect("edited text or retained attachments always form a submission");
        let display_content = (!edit.attachments.is_empty()).then_some(prompt);
        self.start_message_rewind(
            edit.clone(),
            ComposerSubmission {
                prompt: provider_prompt,
                display_content,
                attachments: edit.attachments,
            },
            cx,
        );
    }

    fn start_message_rewind(
        &mut self,
        edit: MessageEdit,
        submission: ComposerSubmission,
        cx: &mut Context<Self>,
    ) {
        let session_id = edit.session_id;
        let turn_count = edit.turn_count;
        let retained_turn_count = turn_count.saturating_sub(1);
        let Some(source) = self
            .state
            .sessions
            .iter()
            .find(|session| session.id == session_id)
            .filter(|session| {
                session
                    .turns
                    .iter()
                    .any(|turn| turn.turn_count == turn_count)
            })
        else {
            self.show_toast(tr!("session.message_unavailable"));
            cx.notify();
            return;
        };
        if self.state.selected_session != Some(session_id) {
            self.show_toast(tr!("session.select_before_rewind"));
            cx.notify();
            return;
        }
        if !matches!(source.status, SessionStatus::Idle | SessionStatus::Failed) {
            self.show_toast(tr!("session.stop_before_rewind"));
            cx.notify();
            return;
        }
        let rollback_turns = source.provider_turns_after(retained_turn_count);
        if !source.provider.supports_conversation_rollback()
            || !waku_client::WorkspaceClient::rewind_action_enabled(
                session_id,
                turn_count,
                source.conversation_actions_enabled(),
            )
            || (rollback_turns > 0 && source.provider_cursor.is_none())
        {
            self.show_toast(tr!(
                "session.provider_cannot_rewind",
                provider = source.provider.display_name()
            ));
            cx.notify();
            return;
        }
        let previous_status = source.status;
        let edited_message_id = edit.message_id;
        let Some(edited_message_index) = source
            .turns
            .iter()
            .find(|turn| turn.turn_count == turn_count)
            .and_then(|turn| {
                source.messages.iter().position(|message| {
                    message.id == edited_message_id
                        && message.turn_id == Some(turn.id)
                        && message.role == MessageRole::User
                })
            })
        else {
            self.show_toast(tr!("session.message_unavailable"));
            cx.notify();
            return;
        };
        let request = MessageRewindRequest {
            workspace_client: waku_client::WorkspaceClient::new(self.daemon.client()),
            session_id,
            turn_count,
        };

        // Optimistically leave edit mode and show the replacement bubble at
        // accept time. The main composer switches to its non-cancellable
        // spinner while every Git, process, native transcript, and provider
        // operation runs off the UI thread. Failure restores both the original
        // bubble and this edit input.
        let original_message = self.state.session_mut(session_id).and_then(|session| {
            let message = session
                .messages
                .iter_mut()
                .find(|message| message.id == edited_message_id)?;
            let original = message.clone();
            message.content = submission.prompt.clone();
            message.display_content = submission.display_content.clone();
            message.attachments = submission.attachments.clone();
            session.status = SessionStatus::Connecting;
            session.updated_at = unix_time();
            Some(original)
        });
        let Some(original_message) = original_message else {
            self.show_toast(tr!("session.message_unavailable"));
            cx.notify();
            return;
        };
        self.message_edit = None;
        self.submission_preparations.insert(session_id);
        self.hide_toast();
        self.remeasure_transcript_message(edited_message_index);
        cx.notify();

        cx.spawn(async move |waku, cx| {
            let result = cx
                .background_executor()
                .spawn(async move { perform_message_rewind(request) })
                .await;
            let _ = waku.update(cx, move |waku, cx| {
                waku.finish_message_rewind(
                    edit,
                    submission,
                    edited_message_id,
                    original_message,
                    previous_status,
                    result,
                    cx,
                );
            });
        })
        .detach();
    }

    fn finish_message_rewind(
        &mut self,
        edit: MessageEdit,
        submission: ComposerSubmission,
        edited_message_id: Uuid,
        original_message: Message,
        previous_status: SessionStatus,
        result: Result<(AgentSession, Option<String>), String>,
        cx: &mut Context<Self>,
    ) {
        let session_id = edit.session_id;
        let turn_count = edit.turn_count;
        if !self.submission_preparations.remove(&session_id) {
            return;
        }
        let selected = self.state.selected_session == Some(session_id);
        let prepared = match result {
            Ok(prepared) => prepared,
            Err(error) => {
                if let Some(session) = self.state.session_mut(session_id) {
                    if let Some(message) = session
                        .messages
                        .iter_mut()
                        .find(|message| message.id == edited_message_id)
                    {
                        *message = original_message;
                    }
                    if session.status == SessionStatus::Connecting {
                        session.status = previous_status;
                    }
                }
                if selected && self.message_edit.is_none() {
                    self.message_edit = Some(edit.clone());
                }
                if selected
                    && let Some(message_index) = self.selected_session().and_then(|session| {
                        session
                            .messages
                            .iter()
                            .position(|message| message.id == edited_message_id)
                    })
                {
                    self.remeasure_transcript_message(message_index);
                }
                self.show_toast(error);
                cx.notify();
                return;
            }
        };
        let (authoritative_session, cleanup_error) = prepared;
        let has_cursor = authoritative_session.provider_cursor.is_some();
        let retained_turn_count = turn_count.saturating_sub(1);
        let provider_and_removed_turns = self
            .state
            .sessions
            .iter()
            .find(|session| session.id == session_id)
            .map(|session| {
                (
                    session.provider,
                    session.turns.len().saturating_sub(retained_turn_count),
                )
            });
        let Some((provider, removed_turns)) = provider_and_removed_turns else {
            return;
        };
        if selected {
            self.sync_transcript_rows();
        }
        let previous_kinds = if selected {
            self.transcript_row_kinds.borrow().clone()
        } else {
            Vec::new()
        };
        if let Some(session) = self.state.session_mut(session_id) {
            *session = authoritative_session;
            session.truncate_after_turn(retained_turn_count);
            session.status = SessionStatus::Idle;
        }

        if has_cursor {
            // Headless drivers retain their original native session ID. Recreate
            // them lazily so the next prompt resumes the fork instead.
            if let Some(runtime) = self.runtimes.remove(&session_id) {
                runtime.driver.close();
            }
            self.mark_background_work_lost(session_id);
        } else if let Some(runtime) = self.runtimes.get_mut(&session_id) {
            runtime.pending_events.retain(|event| {
                matches!(
                    event,
                    DriverEvent::BackgroundWork(_) | DriverEvent::DeerFlowTasks(_)
                )
            });
            runtime.stream_remeasure_pending = false;
            runtime.stream_phase = None;
            runtime.pending_permission = None;
            runtime.pending_user_input = None;
            runtime.pending_computer_approval = None;
        }
        self.invalidate_checkpoint_refs();
        if self
            .message_edit
            .as_ref()
            .is_some_and(|current| current.session_id == session_id)
        {
            self.message_edit = None;
        }
        if selected {
            self.activities_expanded.clear();
            self.expanded_activity_items.clear();
            self.expanded_turns.clear();
            self.expanded_changed_files.clear();
            self.transcript_control_focuses.borrow_mut().clear();
            self.splice_transcript_rows_after_visibility_change(&previous_kinds);
            self.show_toast(match cleanup_error {
                None => tr!("session.rewound", turn = turn_count),
                Some(error) => tr!(
                    "session.rewound_with_stale_refs",
                    turn = turn_count,
                    error = error
                ),
            });
        }
        self.analytics
            .track(crate::analytics::Event::ConversationRolledBack {
                provider: provider.id(),
                turns: removed_turns,
            });
        cx.notify();
        self.submit_submission_for_session(session_id, submission, cx);
    }

    /// Resolves the turn options a driver should run with, dropping a reasoning
    /// effort or service tier the resolved model does not offer. Driver start
    /// and in-session option changes both go through this so they cannot
    /// disagree about what the session is currently set to.
    pub(super) fn session_options(&self, session: &AgentSession) -> SessionOptions {
        let model = session.model.clone().or_else(|| {
            self.provider_probe(session.provider)
                .and_then(ProviderProbe::preferred_model)
                .map(|model| model.id.clone())
        });
        let model_metadata = self.model_metadata_for_session(session);
        let reasoning_effort = session.reasoning_effort.clone().filter(|effort| {
            model_metadata.is_some_and(|model| {
                model
                    .reasoning_efforts
                    .iter()
                    .any(|option| option.id == *effort)
            })
        });
        let service_tier = session.service_tier.clone().filter(|tier| {
            tier == "default"
                || model_metadata.is_some_and(|model| {
                    model.service_tiers.iter().any(|option| option.id == *tier)
                })
        });
        let context_window = session.context_window.clone().filter(|window| {
            model_metadata.is_some_and(|model| {
                model
                    .context_windows
                    .iter()
                    .any(|option| option.id == *window)
            })
        });
        let thinking_mode = session.thinking_mode.clone();
        SessionOptions {
            mode: session.runtime_mode,
            model,
            reasoning_effort,
            thinking_mode,
            service_tier,
            context_window,
        }
    }

    pub(super) fn reap_idle_sessions(&mut self) {
        if self.last_idle_session_sweep.elapsed() < IDLE_SESSION_SWEEP_INTERVAL {
            return;
        }
        self.last_idle_session_sweep = Instant::now();
        let idle = self
            .runtimes
            .iter()
            .filter(|(session_id, runtime)| {
                let session = self
                    .state
                    .sessions
                    .iter()
                    .find(|session| session.id == **session_id);
                session_is_reapable(
                    session,
                    runtime.last_active_at.elapsed(),
                    self.session_has_live_background_work(**session_id),
                )
            })
            .map(|(session_id, _)| *session_id)
            .collect::<Vec<_>>();
        for session_id in idle {
            // Idle reaping is an explicit daemon-runtime release. Merely
            // dropping a client attachment must not stop work observed by a
            // second desktop or browser client.
            if let Some(runtime) = self.runtimes.remove(&session_id) {
                runtime.driver.close();
            }
        }
    }

    /// Applies a changed model, effort, tier, or mode to a session. Transports
    /// that carry these per turn absorb the change and keep running; the rest
    /// are torn down so the next prompt starts with the new options.
    pub(super) fn apply_session_options(&mut self, session_id: Uuid, cx: &mut Context<Self>) {
        let Some(options) = self
            .state
            .sessions
            .iter()
            .find(|session| session.id == session_id)
            .map(|session| self.session_options(session))
        else {
            return;
        };
        let Some(runtime) = self.runtimes.get_mut(&session_id) else {
            return;
        };
        runtime.options_generation = runtime.options_generation.wrapping_add(1);
        runtime.deerflow_options_pending =
            self.state.sessions.iter().any(|session| {
                session.id == session_id && session.provider == ProviderKind::DeerFlow
            });
        let generation = runtime.options_generation;
        let driver = runtime.driver.clone();
        cx.spawn(async move |waku, cx| {
            let applied = cx
                .background_executor()
                .spawn(async move { driver.apply_options(options) })
                .await;
            let _ = waku.update(cx, |waku, cx| {
                let is_current = waku
                    .runtimes
                    .get(&session_id)
                    .is_some_and(|runtime| runtime.options_generation == generation);
                if is_current && !applied {
                    waku.reset_session_runtime(session_id);
                    cx.notify();
                }
            });
        })
        .detach();
    }

    fn driver_start_request_for_session(
        &self,
        session: &AgentSession,
        cwd: PathBuf,
    ) -> anyhow::Result<DriverStartRequest> {
        let binary = self
            .probes
            .iter()
            .find(|probe| probe.provider == session.provider)
            .and_then(|probe| probe.path.clone())
            .ok_or_else(|| {
                anyhow::anyhow!(tr!(
                    "errors.provider_not_found",
                    provider = session.provider.display_name()
                ))
            })?;
        let SessionOptions {
            mode,
            model,
            reasoning_effort,
            thinking_mode,
            service_tier,
            context_window,
        } = self.session_options(&session);
        Ok(DriverStartRequest {
            session_id: session.id,
            provider: session.provider,
            options: DriverStartOptions {
                binary,
                cwd,
                mode,
                model,
                reasoning_effort,
                thinking_mode,
                service_tier,
                context_window,
                agent_preset: None,
                computer_use_enabled: self.state.computer_use_enabled,
                provider_cursor: session.provider_cursor.clone(),
            },
            event_wake: self.event_wake_tx.clone(),
            daemon: self.daemon.clone(),
        })
    }

    /// Attach or start an idle runtime for approvals, tasks and session controls.
    pub(super) fn start_idle_runtime(&mut self, session_id: Uuid, cx: &mut Context<Self>) {
        if self.runtimes.contains_key(&session_id)
            || self.idle_runtime_starts.contains(&session_id)
            || self.submission_preparations.contains(&session_id)
        {
            return;
        }
        let Some(session) = self
            .state
            .sessions
            .iter()
            .find(|session| session.id == session_id)
        else {
            self.fail_deerflow_budget_connection(session_id, "会话已不存在".into());
            return;
        };
        if !session.provider.is_available() {
            return;
        }
        let project_id = session.project_id;
        let workspace = session.workspace.clone();
        let next_turn_count = session.turns.len() + 1;
        let provisional_cwd = self
            .workspace_path_for_session(session)
            .map(Path::to_path_buf)
            .unwrap_or_default();
        let driver_start = self.driver_start_request_for_session(session, provisional_cwd);
        let Some(project) = self
            .state
            .projects
            .iter()
            .find(|project| project.id == project_id)
            .cloned()
        else {
            self.fail_deerflow_budget_connection(
                session_id,
                tr!("errors.prepare_task_project_not_found"),
            );
            self.show_toast(tr!("errors.prepare_task_project_not_found"));
            cx.notify();
            return;
        };
        let naming_prompt = session.display_title().to_owned();
        self.idle_runtime_starts.insert(session_id);
        cx.notify();
        let daemon = self.daemon.clone();
        let event_wake = self.event_wake_tx.clone();
        let workspace_client = waku_client::WorkspaceClient::new(self.daemon.client());
        cx.spawn(async move |waku, cx| {
            let prepared = cx
                .background_executor()
                .spawn(async move {
                    if let Some((session, prepared)) =
                        attach_driver(daemon, session_id, event_wake)?
                    {
                        return Ok(PreparedIdleRuntime::Attached(session, prepared));
                    }
                    if waku_client::WorkspaceClient::has_pending_history(session_id) {
                        anyhow::bail!("会话历史操作的结果尚未确认，请重试原回合的分叉或回退操作");
                    }
                    prepare_submission(
                        workspace_client,
                        project,
                        workspace,
                        Some(driver_start),
                        session_id,
                        &naming_prompt,
                        next_turn_count,
                    )
                    .map(PreparedIdleRuntime::Started)
                })
                .await;
            let _ = waku.update(cx, move |waku, cx| match prepared {
                Ok(PreparedIdleRuntime::Attached(session, driver)) => {
                    if !waku.idle_runtime_starts.remove(&session_id) {
                        return;
                    }
                    waku.runtime_attach_pending.insert(session_id);
                    waku.finish_runtime_attachment(session_id, Ok(Some((session, driver))), cx);
                    cx.notify();
                }
                Ok(PreparedIdleRuntime::Started(prepared)) => {
                    waku.finish_idle_runtime_start(session_id, Ok(prepared), cx)
                }
                Err(error) => waku.finish_idle_runtime_start(session_id, Err(error), cx),
            });
        })
        .detach();
    }

    fn finish_idle_runtime_start(
        &mut self,
        session_id: Uuid,
        prepared: anyhow::Result<PreparedSubmission>,
        cx: &mut Context<Self>,
    ) {
        if !self.idle_runtime_starts.remove(&session_id) {
            return;
        }
        let prepared = match prepared {
            Ok(prepared) => prepared,
            Err(error) => {
                    // behind this start resubmit through the ordinary path,
                // which starts its own runtime.
                self.pending_deerflow_approvals.remove(&session_id);
                self.fail_deerflow_budget_connection(session_id, error.to_string());
                if self.daemon.is_remote() || !waku_client::DaemonClient::is_transport_lost(&error) {
                    self.show_toast(error.to_string());
                }
                self.drain_queued_message(session_id, cx);
                cx.notify();
                return;
            }
        };
        let PreparedSubmission {
            workspace,
            checkpoint_warning: _,
            driver,
        } = prepared;
        if !self
            .state
            .sessions
            .iter()
            .any(|session| session.id == session_id)
        {
            // The task was removed while its provider was starting.
            if let Some(Ok(prepared)) = driver {
                prepared.handle.close();
            }
            return;
        }
        let workspace_changed = self.state.session_mut(session_id).is_some_and(|session| {
            let changed = session.workspace != workspace;
            session.workspace = workspace;
            changed
        });
        if workspace_changed && self.state.selected_session == Some(session_id) {
            self.invalidate_workspace_queries(cx);
            self.reload_clean_right_panel_file_editors(cx);
            self.ensure_right_panel_terminals(cx);
        }
        match driver {
            Some(Ok(prepared)) => {
                if self.runtimes.contains_key(&session_id) {
                    prepared.handle.close();
                } else {
                    // Install drains the pending operations itself.
                    self.install_prepared_driver(session_id, prepared);
                }
            }
            None => {
                if !self.runtimes.contains_key(&session_id) {
                    self.fail_deerflow_budget_connection(
                        session_id,
                        tr!("errors.prepared_runtime_unavailable"),
                    );
                }
            }
            Some(Err(error)) => {
                self.pending_deerflow_approvals.remove(&session_id);
                self.fail_deerflow_budget_connection(session_id, error.to_string());
                if self.daemon.is_remote() || !waku_client::DaemonClient::is_transport_lost(&error) {
                    self.show_toast(error.to_string());
                }
                self.drain_queued_message(session_id, cx);
                cx.notify();
                return;
            }
        }
        self.save();
        self.drain_queued_message(session_id, cx);
        cx.notify();
    }

    fn install_prepared_driver(
        &mut self,
        session_id: Uuid,
        prepared: PreparedDriver,
    ) -> DriverHandle {
        let handle = prepared.handle.clone();
        self.runtimes.insert(
            session_id,
            SessionRuntime {
                driver: prepared.handle,
                steering_run_id: None,
                options_generation: 0,
                deerflow_options_pending: false,
                cancel_drain: Default::default(),
                events: prepared.events,
                pending_events: VecDeque::new(),
                pending_steers: VecDeque::new(),
                pending_local_prompt: None,
                pending_local_permission: None,
                local_cancel_generation: None,
                stream_phase: None,
                park_announced: false,
                stream_remeasure_pending: false,
                pending_permission: None,
                pending_user_input: None,
                pending_computer_approval: None,
                computer_use_previews: Vec::new(),
                computer_session_grants: HashSet::new(),
                last_driver_error: None,
                failure_notice: None,
                last_active_at: Instant::now(),
                last_background_refresh_at: Instant::now()
                    .checked_sub(BACKGROUND_WORK_REFRESH_INTERVAL)
                    .unwrap_or_else(Instant::now),
            },
        );
        // Startup can emit before the background task hands this receiver to
        // the runtime map. Wake once after installation so those buffered
        // events cannot be stranded behind an already-consumed edge.
        signal_event_pump(&self.event_wake_tx);
        // its thread opens, before any queued prompt.
        handle
    }

    pub(super) fn submit_composer_submission(
        &mut self,
        submission: ComposerSubmission,
        cx: &mut Context<Self>,
    ) {
        if self
            .selected_session()
            .is_some_and(|session| session.provider == ProviderKind::DeerFlow)
        {
            self.search_plugins_request("search-status", serde_json::json!({}), cx);
        }
        let Some(session) = self.selected_session() else {
            return;
        };
        if !session.provider.is_available() || self.response_fork_preparations.contains_key(&session.id) {
            return;
        }
        if session.status == SessionStatus::Background {
            // The turn is parked on detached work and the provider is idle,
            // so the message goes straight in as a steer: queued, it would
            // wait for a settle that only the message itself could hasten.
            self.steer_composer_submission(submission, cx);
            return;
        }
        if self.runtimes.get(&session.id).is_some_and(|runtime| runtime.cancel_drain.is_pending()) {
            self.submit_submission_for_session(session.id, submission, cx);
            return;
        }
        if session.is_busy() {
            // While the agent is working, Enter queues a follow-up instead of
            // refusing the message. The queue drains once the turn settles.
            self.enqueue_follow_up_submission(session.id, submission, cx);
            return;
        }
        self.submit_submission_for_session(session.id, submission, cx);
    }

    /// Deliver a steering message into the running turn. Providers without a
    /// live-turn transport (or a session that is not actively working) fall
    /// back to queueing a follow-up.
    pub(super) fn steer_composer_submission(
        &mut self,
        submission: ComposerSubmission,
        cx: &mut Context<Self>,
    ) {
        let Some(session) = self.selected_session().cloned() else {
            return;
        };
        if !session.provider.is_available() {
            return;
        }
        if !session.is_busy() || !self.session_can_steer(&session)
        {
            if let Some(current) = self.state.session_mut(session.id) {
                current.apply_steering_update(&waku_client::model::SteeringUpdate {
                    request_id: Uuid::new_v4().to_string(), run_id: String::new(),
                    status: "rejected".into(), message: submission.prompt,
                    reason: "The current run is no longer available for intervention. Submit explicitly as a new turn to continue.".into(),
                });
            }
            cx.notify();
            return;
        }
        if !session.is_busy() {
            self.submit_composer_submission(submission, cx);
            return;
        }
        // A turn that has not reached the provider yet cannot be steered; the
        // driver reports the outcome asynchronously via SteerAccepted or
        // SteerRejected once it is handed off.
        if !self.session_can_steer(&session) {
            self.enqueue_follow_up_submission(session.id, submission, cx);
            return;
        }
        let provider_prompt = submission.prompt.clone();
        if let Some(runtime) = self.runtimes.get_mut(&session.id) {
            let admitted = if let Some(run) = &runtime.steering_run_id {
                runtime.driver.try_steer_run(provider_prompt, run.clone())
            } else {
                runtime.driver.try_steer(provider_prompt)
            };
            if !admitted {
                self.restore_unsent_submission(session.id, submission, cx);
                return;
            }
            runtime.pending_steers.push_back(submission);
        } else {
            self.enqueue_follow_up_submission(session.id, submission, cx);
        }
        cx.notify();
    }

    pub(super) fn session_can_steer(&self, session: &AgentSession) -> bool {
        session_has_active_provider_turn(session)
            && self.runtimes.get(&session.id).is_some_and(|runtime| {
                runtime.driver.supports_steer()
                    && (session.provider != ProviderKind::DeerFlow
                        || (runtime.steering_run_id.is_some()
                            && !runtime.cancel_drain.is_pending()))
            })
    }

    /// Resolve presentation-preserving composer syntax immediately before a
    /// prompt crosses into a provider transport.
    fn resolve_provider_submission(&self, provider: ProviderKind, prompt: &str) -> String {
        crate::composer_complete::resolved_submission(provider, prompt, &self.slash_command_index)
            .unwrap_or_else(|| prompt.to_owned())
    }

    pub(super) fn enqueue_follow_up_submission(
        &mut self,
        session_id: Uuid,
        mut submission: ComposerSubmission,
        cx: &mut Context<Self>,
    ) {
        submission.prompt = submission.prompt.trim().to_owned();
        if submission.prompt.is_empty() {
            return;
        }
        if let Some(session) = self.state.session_mut(session_id) {
            session
                .queued_messages
                .push(submission.into_queued_message());
            session.updated_at = unix_time();
        }
        self.save();
        cx.notify();
    }

    pub(super) fn remove_queued_message(
        &mut self,
        session_id: Uuid,
        message_id: Uuid,
        cx: &mut Context<Self>,
    ) {
        if let Some(session) = self.state.session_mut(session_id) {
            session
                .queued_messages
                .retain(|message| message.id != message_id);
        }
        self.save();
        cx.notify();
    }

    /// Pop a queued message back into the composer so the user can edit and
    /// resubmit it.
    pub(super) fn edit_queued_message(
        &mut self,
        session_id: Uuid,
        message_id: Uuid,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Some(message) = self.state.session_mut(session_id).and_then(|session| {
            let index = session
                .queued_messages
                .iter()
                .position(|message| message.id == message_id)?;
            Some(session.queued_messages.remove(index))
        }) else {
            return;
        };
        self.restore_composer_submission(ComposerSubmission::from_queued_message(message), cx);
        let focus_handle = self.composer_focus(cx);
        window.focus(&focus_handle, cx);
        self.save();
        cx.notify();
    }

    /// Deliver a queued follow-up into the running turn right away instead of
    /// waiting for the turn to settle. Falls through the same paths as a
    /// composer steer: an idle session starts a fresh turn, an unsteerable
    /// one re-queues the message.
    pub(super) fn steer_queued_message(
        &mut self,
        session_id: Uuid,
        message_id: Uuid,
        turn_id: Option<Uuid>,
        cx: &mut Context<Self>,
    ) {
        if self.local_session_unavailable(session_id) || !self.selected_session().is_some_and(|session| {
            session.id == session_id
                && (session.provider != ProviderKind::DeerFlow
                    || (session.active_turn_id() == turn_id && self.session_can_steer(session)))
        }) {
            return;
        }
        let Some(message) = self.state.session_mut(session_id).and_then(|session| {
            let index = session
                .queued_messages
                .iter()
                .position(|message| message.id == message_id)?;
            Some(session.queued_messages.remove(index))
        }) else {
            return;
        };
        self.save();
        self.steer_composer_submission(ComposerSubmission::from_queued_message(message), cx);
    }

    /// Activate the same action as the oldest queued row's Steer control.
    /// When that control is unavailable, leave the queue untouched rather
    /// than removing and re-queueing its first message at the back.
    pub(super) fn steer_oldest_queued_message(&mut self, cx: &mut Context<Self>) {
        let Some((session_id, message_id, turn_id)) = self.selected_session().and_then(|session| {
            if !self.session_can_steer(session) {
                return None;
            }
            Some((
                session.id,
                session.queued_messages.first()?.id,
                session.active_turn_id(),
            ))
        }) else {
            return;
        };
        self.steer_queued_message(session_id, message_id, turn_id, cx);
    }

    /// Start the next queued follow-up as a fresh turn. Only called once a
    /// settled turn has been fully closed, so the session is Idle.
    pub(super) fn drain_queued_message(&mut self, session_id: Uuid, cx: &mut Context<Self>) {
        if self.local_session_unavailable(session_id) {
            if self.state.sessions.iter().any(|s| s.id == session_id && !s.queued_messages.is_empty()) {
                self.queues_waiting_for_connection.insert(session_id);
            }
            return;
        }
        if self.response_fork_preparations.contains_key(&session_id) {
            return;
        }
        let Some(session) = self
            .state
            .sessions
            .iter()
            .find(|session| session.id == session_id)
        else {
            return;
        };
        if session.is_busy()
            || session.queued_messages.is_empty()
            || self.pending_deerflow_approvals.contains_key(&session_id)
            || self.runtimes.get(&session_id).is_some_and(|runtime| runtime.deerflow_options_pending || runtime.cancel_drain.is_pending())
            || self.ending_checkpoint_pending(session_id)
            // Deliver queued messages after the idle runtime connects.
            || self.idle_runtime_starts.contains(&session_id)
            || self.deerflow_tasks.get_mut(&session_id).is_some_and(|browser| browser.defer_queue_for_foreground())
        {
            return;
        }
        let Some(message) = self
            .state
            .session_mut(session_id)
            .map(|session| session.queued_messages.remove(0))
        else {
            return;
        };
        self.submit_submission_for_session(
            session_id,
            ComposerSubmission::from_queued_message(message),
            cx,
        );
    }

    fn submit_submission_for_session(
        &mut self,
        session_id: Uuid,
        submission: ComposerSubmission,
        cx: &mut Context<Self>,
    ) {
        if self.response_fork_preparations.contains_key(&session_id) {
            return;
        }
        let selected = self.state.selected_session == Some(session_id);
        let Some(session) = self
            .state
            .sessions
            .iter()
            .find(|session| session.id == session_id && session.provider.is_available())
        else {
            return;
        };
        if let Some(runtime) = self.runtimes.get_mut(&session_id)
            && runtime.cancel_drain.is_pending()
        {
            let position = runtime.cancel_drain.queue_followup();
            if let Some(session) = self.state.session_mut(session_id) {
                let position = position.min(session.queued_messages.len());
                session
                    .queued_messages
                    .insert(position, submission.into_queued_message());
            }
            self.save();
            cx.notify();
            return;
        }
        if self.ending_checkpoint_pending(session_id)
            || self
                .runtimes
                .get(&session_id)
                .is_some_and(|runtime| runtime.deerflow_options_pending)
        {
            self.enqueue_follow_up_submission(session_id, submission, cx);
            self.defer_queue_drain(session_id);
            return;
        }
        // Reuse the ACP connection already being started for this session.
        if self.idle_runtime_starts.contains(&session_id) {
            self.enqueue_follow_up_submission(session_id, submission, cx);
            self.defer_queue_drain(session_id);
            return;
        }
        if session.status.is_busy() {
            self.enqueue_follow_up_submission(session_id, submission, cx);
            return;
        }
        let prompt = submission.prompt.clone();
        let human_prompt = submission.human_prompt();
        let has_input = !submission
            .display_content
            .as_deref()
            .unwrap_or(&submission.prompt)
            .trim()
            .is_empty();
        let next_turn_count = session.turns.len() + 1;
        let provider = session.provider.id();
        let model = self
            .session_options(session)
            .model
            .unwrap_or_else(|| "default".into());
        let workspace_kind = if session.workspace.is_worktree() {
            "worktree"
        } else {
            "local"
        };
        let attachment_count = submission.attachments.len();
        let project_id = session.project_id;
        let workspace = session.workspace.clone();
        let driver_start = (!self.runtimes.contains_key(&session_id)).then(|| {
            let provisional_cwd = self
                .workspace_path_for_session(session)
                .map(std::path::Path::to_path_buf)
                .unwrap_or_default();
            self.driver_start_request_for_session(session, provisional_cwd)
        });
        let Some(project) = self
            .state
            .projects
            .iter()
            .find(|project| project.id == project_id)
            .cloned()
        else {
            if selected {
                self.restore_composer_submission(submission, cx);
                self.show_toast(tr!("errors.prepare_task_project_not_found"));
            }
            cx.notify();
            return;
        };
        let projectless = project.is_projectless();
        // Busy is visible before any Git work begins. The separate transient
        // set keeps this non-cancellable phase visually distinct from a
        // connecting provider, whose runtime already has a working Stop path.
        //
        // The turn also begins now, not once preparation settles: the sent
        // message and its working indicator belong in the transcript the
        // moment the submission is accepted — a first prompt otherwise leaves
        // the empty state on screen for as long as a `git add -A` takes.
        // Preparation failure unwinds the turn and restores the prompt.
        if selected {
            self.sync_transcript_rows();
        }
        let previous_kinds = if selected {
            self.transcript_row_kinds.borrow().clone()
        } else {
            Vec::new()
        };
        let transcript_anchor = if let Some(session) = self.state.session_mut(session_id) {
            session.set_title_from_prompt(&human_prompt);
            let turn_id = session.begin_turn_with_presentation(
                &prompt,
                submission.display_content.clone(),
                submission.attachments.clone(),
            );
            session.status = SessionStatus::Connecting;
            session.updated_at = unix_time();
            selected.then_some(TranscriptAnchor {
                session_id,
                turn_id,
            })
        } else {
            None
        };
        self.analytics
            .track(crate::analytics::Event::TurnSubmitted {
                provider,
                model,
                turn_number: next_turn_count,
                workspace: workspace_kind,
                projectless,
                attachment_count,
                has_input,
            });
        self.submission_preparations.insert(session_id);
        if selected {
            self.activities_expanded.clear();
            self.expanded_activity_items.clear();
            self.expanded_turns.clear();
            self.expanded_changed_files.clear();
            self.transcript_control_focuses.borrow_mut().clear();
            self.message_edit = None;
            self.hide_toast();
            self.transcript_anchor.set(transcript_anchor);
            // Provisional reservation: the anchored list has no measured
            // bounds until its first paint, and a zero end space cannot hold
            // the sent row at the viewport top — without scroll room past the
            // tail, the list clamps to its end and the prompt paints a frame
            // at the bottom before the first measured frame lifts it. Seed a
            // full viewport of end space instead; the overshoot is invisible
            // under the top anchor and the first measured frame trues it up.
            let mut provisional = self.transcript_rows.viewport_bounds().size.height;
            if provisional <= Pixels::ZERO {
                provisional = self.anchored_transcript_rows.viewport_bounds().size.height;
            }
            self.transcript_anchor_end_space.set(provisional);
            self.transcript_anchor_following.set(true);
            self.splice_transcript_rows_after_visibility_change(&previous_kinds);
            self.scroll_transcript_to_anchor();
        }
        cx.notify();

        let preparation_prompt = human_prompt;
        let workspace_client = waku_client::WorkspaceClient::new(self.daemon.client());
        cx.spawn(async move |waku, cx| {
            let prepared = cx
                .background_executor()
                .spawn(async move {
                    prepare_submission(
                        workspace_client,
                        project,
                        workspace,
                        driver_start,
                        session_id,
                        &preparation_prompt,
                        next_turn_count,
                    )
                })
                .await;
            let _ = waku.update(cx, move |waku, cx| {
                waku.finish_submission_preparation(session_id, submission, prepared, cx);
            });
        })
        .detach();
    }

    fn finish_submission_preparation(
        &mut self,
        session_id: Uuid,
        submission: ComposerSubmission,
        prepared: anyhow::Result<PreparedSubmission>,
        cx: &mut Context<Self>,
    ) {
        if !self.submission_preparations.contains(&session_id) {
            return;
        }
        let selected = self.state.selected_session == Some(session_id);
        let prepared = match prepared {
            Ok(prepared) => prepared,
            Err(error) => {
                self.submission_preparations.remove(&session_id);
                let recovery = !self.daemon.is_remote() && waku_client::DaemonClient::is_transport_lost(&error);
                if !recovery {
                    self.fail_deerflow_budget_connection(session_id, error.to_string());
                }
                self.track_active_turn_outcome(
                    session_id,
                    crate::analytics::TurnOutcome::PreparationFailed,
                );
                if selected {
                    self.sync_transcript_rows();
                }
                let previous_kinds = if selected {
                    self.transcript_row_kinds.borrow().clone()
                } else {
                    Vec::new()
                };
                if let Some(session) = self.state.session_mut(session_id)
                    && session.status == SessionStatus::Connecting
                {
                    // The submission never reached a provider and its prompt
                    // returns to the composer, so the eagerly-begun turn and
                    // its message leave the transcript with it.
                    if let Some(turn_id) = session.active_turn_id() {
                        session.unwind_unstarted_turn(turn_id);
                    }
                    session.status = SessionStatus::Idle;
                }
                if selected {
                    if self
                        .transcript_anchor
                        .get()
                        .is_some_and(|anchor| anchor.session_id == session_id)
                    {
                        self.transcript_anchor.set(None);
                        self.transcript_anchor_following.set(false);
                    }
                    self.splice_transcript_rows_after_visibility_change(&previous_kinds);
                    if !recovery {
                        self.show_toast(tr!("errors.create_worktree", error = error));
                    }
                }
                self.restore_unsent_submission(session_id, submission, cx);
                cx.notify();
                return;
            }
        };
        let PreparedSubmission {
            workspace,
            checkpoint_warning,
            driver: prepared_driver,
        } = prepared;
        // The turn began at accept time; it must still be the untouched one
        // this preparation belongs to. Cancellation is blocked while the
        // preparation set holds the session, so a mismatch means the session
        // was replaced under the preparation rather than a user action.
        let can_start = self
            .state
            .sessions
            .iter()
            .find(|session| session.id == session_id)
            .is_some_and(|session| {
                session.status == SessionStatus::Connecting
                    && session.turns.last().is_some_and(|turn| {
                        turn.status == TurnStatus::Running && !turn.provider_turn_started
                    })
            });
        if !can_start {
            self.submission_preparations.remove(&session_id);
            cx.notify();
            return;
        }

        let workspace_changed = self.state.session_mut(session_id).is_some_and(|session| {
            let changed = session.workspace != workspace;
            session.workspace = workspace;
            changed
        });
        if selected && workspace_changed {
            self.invalidate_workspace_queries(cx);
            self.reload_clean_right_panel_file_editors(cx);
            self.ensure_right_panel_terminals(cx);
        }
        let driver = match prepared_driver {
            None => self
                .runtimes
                .get(&session_id)
                .map(|runtime| runtime.driver.clone())
                .ok_or_else(|| anyhow::anyhow!(tr!("errors.prepared_runtime_unavailable"))),
            Some(Ok(prepared)) => Ok(self.install_prepared_driver(session_id, prepared)),
            Some(Err(error)) => Err(error),
        };
        self.invalidate_checkpoint_refs();
        if let Some(runtime) = self.runtimes.get_mut(&session_id) {
            runtime.pending_events.retain(|event| {
                matches!(
                    event,
                    DriverEvent::BackgroundWork(_) | DriverEvent::DeerFlowTasks(_)
                )
            });
            runtime.pending_steers.clear();
            runtime.stream_remeasure_pending = false;
            runtime.stream_phase = None;
            runtime.pending_permission = None;
            runtime.pending_user_input = None;
            runtime.pending_computer_approval = None;
            runtime.last_active_at = Instant::now();
        }
        // The transcript already shows the turn — the prompt message, its
        // anchor, and the working indicator all landed at accept time. Only
        // preparation's own output surfaces here.
        if selected && let Some(error) = checkpoint_warning
            && (self.daemon.is_remote() || !waku_client::DaemonClient::is_transport_lost(&error))
        {
            self.show_toast(tr!("errors.capture_pre_turn_checkpoint", error = error));
        }
        let provider = self
            .state
            .sessions
            .iter()
            .find(|session| session.id == session_id)
            .map(|session| session.provider)
            .unwrap_or(self.state.last_provider);
        // The transcript keeps the typed slash command while the transport receives the expanded template.
        let driver_prompt = self.resolve_provider_submission(provider, &submission.prompt);
        // The turn and its user message landed at accept time. Their ids go
        // with the prompt so every other client attached to the runtime
        // mirrors the same rows instead of minting its own.
        let (turn_id, message_id) = self
            .state
            .sessions
            .iter()
            .find(|session| session.id == session_id)
            .map(submitted_prompt_identity)
            .unwrap_or((None, None));
        let mut failed_to_start = false;
        match driver {
            Ok(driver) => {
                let generation = driver.local_connection_generation();
                let admitted = driver.try_prompt_with_attachments(
                    driver_prompt, turn_id, message_id, submission.attachments.clone(),
                );
                if !admitted {
                    self.restore_unsent_turn(session_id, turn_id, submission, cx);
                    return;
                }
                if !self.daemon.is_remote() && let (Some(turn), Some(message)) = (turn_id, message_id)
                    && let Some(runtime) = self.runtimes.get_mut(&session_id)
                {
                    runtime.pending_local_prompt = Some((turn, message, generation));
                }
            }
            Err(error) => {
                if !self.daemon.is_remote() && waku_client::DaemonClient::is_transport_lost(&error) {
                    self.restore_unsent_turn(session_id, turn_id, submission, cx);
                    return;
                }
                failed_to_start = true;
                self.fail_deerflow_budget_connection(session_id, error.to_string());
                let message = tr!("errors.start_agent", error = error);
                if let Some(session) = self.state.session_mut(session_id) {
                    session.status = SessionStatus::Failed;
                    session.push_message(MessageRole::Assistant, message);
                }
                self.finish_active_turn_with_analytics(
                    session_id,
                    TurnStatus::Failed,
                    crate::analytics::TurnOutcome::StartFailed,
                );
            }
        }
        // From this point onward `cancel_turn` has either a live driver to
        // cancel or a settled startup failure. The next frame must therefore
        // show Stop (or Send after failure), never the preparation spinner.
        self.submission_preparations.remove(&session_id);
        if failed_to_start {
            self.capture_latest_turn_checkpoint_for(session_id);
            self.start_pending_checkpoint_captures(cx);
        }
        cx.notify();
        // Persist on the next frame boundary. Saving is intentionally after
        // the spinner-to-Stop paint: SQLite or blob externalization must not
        // hold the final preparation frame motionless.
        cx.spawn(async move |waku, cx| {
            cx.background_executor().timer(STREAM_FRAME_INTERVAL).await;
            let _ = waku.update(cx, |waku, _| waku.save());
        })
        .detach();
    }

    fn restore_unsent_turn(&mut self, session_id: Uuid, turn_id: Option<Uuid>, submission: ComposerSubmission, cx: &mut Context<Self>) {
        self.submission_preparations.remove(&session_id);
        self.local_action_notice = Some((session_id, "本地连接已变更，消息尚未发送，草稿已保留。"));
        if let Some(session) = self.state.session_mut(session_id)
            && let Some(turn_id) = turn_id
            && session.active_turn_id() == Some(turn_id)
        {
            session.unwind_unstarted_turn(turn_id);
            session.status = SessionStatus::Idle;
        }
        if self.state.selected_session == Some(session_id) {
            self.transcript_anchor.set(None);
            self.transcript_anchor_following.set(false);
            self.sync_transcript_rows();
            self.remeasure_transcript_tail();
        }
        self.restore_unsent_submission(session_id, submission, cx);
        self.save();
    }

    pub(super) fn collect_runtime_events(runtime: &mut SessionRuntime) {
        while let Ok(event) = runtime.events.try_recv() {
            runtime.pending_events.push_back(event);
        }
    }

    pub(super) fn drain_event_pump(&mut self, cx: &mut Context<Self>) -> EventPumpSchedule {
        // `|` on purpose: a busy provider must not starve the other result
        // queues just because its own drain reported a change first.
        if self.drain_driver_events(cx)
            | self.drain_provider_probe_events()
            | self.drain_provider_version_events()
            | self.drain_provider_detection_events()
            | self.drain_computer_permission_events()
            | self.drain_task_state_sync_events(cx)
        {
            cx.notify();
        }
        if std::mem::take(&mut self.workspace_queries_stale) {
            self.invalidate_workspace_queries(cx);
        }
        if std::mem::take(&mut self.composer_sources_stale) {
            self.refresh_composer_sources(cx);
        }
        self.maybe_refresh_background_work(cx);
        // A finished turn asks for a checkpoint from a handler with no
        // `Context`; this is where that `git` work leaves the UI thread.
        self.start_pending_checkpoint_captures(cx);

        if self
            .runtimes
            .values()
            .any(|runtime| !runtime.pending_events.is_empty() || runtime.stream_remeasure_pending)
        {
            EventPumpSchedule::StreamFrame
        } else if let Some(delay) = self.background_output_refresh_delay() {
            EventPumpSchedule::BackgroundOutput(delay)
        } else {
            EventPumpSchedule::Idle
        }
    }

    pub(super) fn drain_provider_probe_events(&mut self) -> bool {
        let mut changed = false;
        while let Ok(probe) = self.provider_probe_events.try_recv() {
            self.provider_model_discoveries_pending
                .remove(&probe.provider);
            if let Some(existing) = self
                .probes
                .iter_mut()
                .find(|existing| existing.provider == probe.provider)
            {
                *existing = probe;
            } else {
                self.probes.push(probe);
            }
            changed = true;
        }
        changed
    }

    pub(super) fn drain_computer_permission_events(&mut self) -> bool {
        let mut changed = false;
        while let Ok(result) = self.computer_permission_events.try_recv() {
            self.computer_permission_request_pending = false;
            match result {
                Ok(permissions) => self.computer_permissions = permissions,
                Err(error) => self.show_toast(error),
            }
            changed = true;
        }
        changed
    }

    pub(super) fn drain_driver_events(&mut self, cx: &mut Context<Self>) -> bool {
        let session_ids = self.runtimes.keys().copied().collect::<Vec<_>>();
        let mut changed = false;
        let mut persisted_state_changed = false;
        let mut force_save = false;
        let mut selected_changed = false;
        for session_id in session_ids {
            let Some(mut runtime) = self.runtimes.remove(&session_id) else {
                continue;
            };
            let follow_up_remeasure = std::mem::take(&mut runtime.stream_remeasure_pending);
            Self::collect_runtime_events(&mut runtime);
            let mut runtime_changed = false;
            let mut background_changed = false;
            let mut markdown_changed = false;
            let mut keep_runtime = true;
            while let Some(event) = runtime.pending_events.front() {
                let kind = stream_delta_kind(event);
                let event = if let Some(kind) = kind {
                    pop_stream_batch(&mut runtime.pending_events, kind)
                } else {
                    runtime.pending_events.pop_front()
                };
                let Some(event) = event else {
                    break;
                };
                let background_event = matches!(
                    event,
                    DriverEvent::BackgroundWork(_) | DriverEvent::DeerFlowTasks(_)
                );
                let background_output_delta = matches!(
                    event,
                    DriverEvent::BackgroundWork(BackgroundWorkEvent::OutputDelta { .. })
                );
                force_save |= matches!(
                    event,
                    DriverEvent::Connected { .. }
                        | DriverEvent::ToolApprovalChanged(_)
                        | DriverEvent::DeerFlowModelsChanged { .. }
                        | DriverEvent::AgentPresetSelected(_)
                        | DriverEvent::AutoTitleUpdated(_)
                        | DriverEvent::Permission { .. }
                        | DriverEvent::PermissionResolved { .. }
                        | DriverEvent::PromptSubmitted { .. }
                        | DriverEvent::SteerAccepted { .. }
                        | DriverEvent::SteerRejected { .. }
                        | DriverEvent::SteeringCapability { .. }
                        | DriverEvent::ConversationCapability { .. }
                        | DriverEvent::SteeringUpdated(_)
                        | DriverEvent::TurnFinished { .. } | DriverEvent::TurnCancelled
                        | DriverEvent::Error(_)
                        | DriverEvent::ProcessExited
                );
                // Reasoning is markdown too (the live peek renders it), and
                // this flag is also what routes the pump onto the coalesced
                // `StreamFrame` cadence: without it a reasoning-only drain
                // reported Idle, so every fast thinking chunk woke the pump
                // for an immediate drain-and-notify — 40+ full re-renders a
                // second, sailing straight past the 120 ms commit floor.
                markdown_changed |= matches!(
                    event,
                    DriverEvent::TextDelta(_) | DriverEvent::ReasoningDelta(_)
                );
                if background_output_delta {
                    // The registry batches log text into SharedString at 10Hz;
                    // repainting and saving for every provider chunk would
                    // turn a noisy command into UI-thread work.
                } else if background_event {
                    background_changed = true;
                } else {
                    runtime_changed = true;
                }
                keep_runtime &= self.handle_driver_event(session_id, &mut runtime, event, true, cx);
                if !keep_runtime {
                    break;
                }
            }
            runtime.stream_remeasure_pending = markdown_changed;
            if keep_runtime {
                self.runtimes.insert(session_id, runtime);
            }
            changed |= runtime_changed || background_changed;
            persisted_state_changed |= runtime_changed;
            if self.state.selected_session == Some(session_id)
                && (runtime_changed || follow_up_remeasure)
            {
                selected_changed = true;
            }
        }

        if !self.pending_queue_drains.is_empty() {
            let drains = std::mem::take(&mut self.pending_queue_drains);
            for session_id in drains {
                if self.ending_checkpoint_pending(session_id) {
                    self.defer_queue_drain(session_id);
                } else {
                    self.drain_queued_message(session_id, cx);
                }
            }
            changed = true;
        }

        if persisted_state_changed {
            self.stream_state_dirty = true;
        }
        if selected_changed {
            self.remeasure_transcript_tail();
        }
        if self.stream_state_dirty
            && (force_save || self.last_stream_save.elapsed() >= STREAM_SAVE_INTERVAL)
        {
            self.save();
        }
        changed || selected_changed
    }
}


#[cfg(test)]
mod version_tests {
    use crate::model::parse_cli_version;

    #[test]
    fn parses_common_cli_version_banners() {
        assert_eq!(
            parse_cli_version("codex-cli 0.45.0\n"),
            Some("0.45.0".to_owned())
        );
        assert_eq!(
            parse_cli_version("2.1.24 (Claude Code)\n"),
            Some("2.1.24".to_owned())
        );
        assert_eq!(
            parse_cli_version("v1.3.0-beta.2"),
            Some("1.3.0-beta.2".to_owned())
        );
        assert_eq!(
            parse_cli_version("\nAmp CLI version 0.9.12\n"),
            Some("0.9.12".to_owned())
        );
        assert_eq!(parse_cli_version("not a version"), None);
        assert_eq!(parse_cli_version(""), None);
    }

    #[test]
    fn version_requires_a_dotted_number_not_a_bare_digit() {
        // "2024" alone or a hash must not read as a version.
        assert_eq!(parse_cli_version("build 2024 f3a9c1"), None);
        assert_eq!(
            parse_cli_version("cursor-agent 2025.09.12-4f8d8e2"),
            Some("2025.09.12-4f8d8e2".to_owned())
        );
    }
}
