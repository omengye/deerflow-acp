//! Provider backend and driver-event wire translation for `waku-daemon`.

mod session_deletion;
mod session_history;

use std::collections::{HashMap, HashSet};
use std::path::{Path, PathBuf};
use std::sync::{Arc, OnceLock};

use crate::{
    Backend, Command, EventSink, Request, ResponsePayload, WireDriverEvent, WorkspaceOperation,
    WorkspaceResult,
};
use anyhow::{Context as _, anyhow, bail};
use parking_lot::Mutex;
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use uuid::Uuid;

use crate::attachments::AttachmentStore;
use crate::computer_use::{ComputerTarget, ComputerUsePhase, ComputerUseState};
use crate::driver::{self, DriverHandle, DriverStartOptions, SessionOptions};
use crate::model::{
    ActivityKind, AgentSession, Checkpoint, CheckpointStatus, DriverEvent, PermissionOption,
    ProviderKind, ProviderResumeCursor, SessionStatus,
};
use crate::persistence::{ComposerDraftStore, PersistedState, StateStore};
use crate::settings::DaemonSettingsStore;

/// How many fully hydrated transcripts the daemon keeps resident.
///
/// Hydration is a cache: consumers reload a released session from the store on
/// demand. Without a cap, a daemon that lives for days adopts the transcript
/// of every session its clients have touched — SaveTaskState pushes, hydrate
/// requests, forks, checkpoints — and resident memory grows without bound.
const RESIDENT_TRANSCRIPT_WINDOW: usize = 24;

/// Releases resident transcripts beyond the recency window after a save.
/// `pinned` names sessions with live runtimes; dirty sessions are skipped
/// inside [`PersistedState::trim_idle_transcripts`] because they hold unsaved
/// work.
fn trim_resident_transcripts(state: &mut PersistedState, pinned: &HashSet<Uuid>) {
    state.trim_idle_transcripts(pinned, RESIDENT_TRANSCRIPT_WINDOW);
}

pub struct WakuBackend {
    deerflow: crate::deerflow_config::DeerFlowService,
    sessions: Arc<Mutex<HashMap<Uuid, (Uuid, DriverHandle)>>>,
    terminals: Mutex<HashMap<Uuid, (Uuid, crate::terminal::DaemonTerminal)>>,
    #[cfg(all(test, unix))]
    terminal_shell: Option<alacritty_terminal::tty::Shell>,
    settings: DaemonSettingsStore,
    task_store: Arc<StateStore>,
    task_state: Arc<Mutex<PersistedState>>,
    removed_session_ids: Arc<Mutex<HashSet<Uuid>>>,
    session_lifecycle_locks: Arc<Mutex<HashMap<Uuid, Arc<Mutex<()>>>>>,
    workspace_admission_locks: Arc<Mutex<HashMap<PathBuf, Arc<Mutex<()>>>>>,
    active_workspace_prompts: Arc<Mutex<HashMap<Uuid, (Uuid, PathBuf)>>>,
    runtime_workspaces: Arc<Mutex<HashMap<Uuid, (Uuid, PathBuf)>>>,
    conversation_capabilities: Arc<Mutex<HashMap<Uuid, (Uuid, bool)>>>,
    deerflow_ready: Arc<Mutex<HashMap<Uuid, (Uuid, waku_protocol::deerflow_tasks::Event)>>>,
    composer_drafts: ComposerDraftStore,
    attachments: AttachmentStore,
    checkpoint_capture_locks: Mutex<HashMap<(PathBuf, Uuid, usize), Arc<Mutex<()>>>>,
    default_cwd: std::path::PathBuf,
}

impl WakuBackend {
    pub fn new(settings: DaemonSettingsStore, task_store: StateStore) -> anyhow::Result<Self> {
        let mut task_state = task_store
            .load()
            .context("could not load Waku task database")?;
        migrate_projectless_state(&task_store, &mut task_state)?;
        let composer_drafts = ComposerDraftStore::for_state_path(task_store.path());
        let attachments = AttachmentStore::new(
            task_store
                .path()
                .parent()
                .unwrap_or_else(|| std::path::Path::new("."))
                .join("attachments"),
        );
        let removed_session_ids = task_store
            .session_deletions()?
            .into_iter()
            .map(|deletion| deletion.session_id)
            .collect();
        let backend = Self {
            deerflow: crate::deerflow_config::DeerFlowService::default(),
            sessions: Arc::new(Mutex::new(HashMap::new())),
            terminals: Mutex::new(HashMap::new()),
            #[cfg(all(test, unix))]
            terminal_shell: None,
            settings,
            task_store: Arc::new(task_store),
            task_state: Arc::new(Mutex::new(task_state)),
            removed_session_ids: Arc::new(Mutex::new(removed_session_ids)),
            session_lifecycle_locks: Arc::new(Mutex::new(HashMap::new())),
            workspace_admission_locks: Arc::new(Mutex::new(HashMap::new())),
            active_workspace_prompts: Arc::new(Mutex::new(HashMap::new())),
            runtime_workspaces: Arc::new(Mutex::new(HashMap::new())),
            conversation_capabilities: Arc::new(Mutex::new(HashMap::new())),
            deerflow_ready: Arc::new(Mutex::new(HashMap::new())),
            composer_drafts,
            attachments,
            checkpoint_capture_locks: Mutex::new(HashMap::new()),
            default_cwd: std::env::current_dir().unwrap_or_else(|_| std::path::PathBuf::from(".")),
        };
        backend.start_deletion_recovery()?;
        backend.start_history_recovery()?;
        Ok(backend)
    }

    #[cfg(all(test, unix))]
    pub(crate) fn with_terminal_shell(mut self, shell: alacritty_terminal::tty::Shell) -> Self {
        self.terminal_shell = Some(shell);
        self
    }

    fn open_terminal(
        &self,
        cwd: &Path,
        cols: u16,
        rows: u16,
        events: EventSink,
    ) -> anyhow::Result<crate::terminal::DaemonTerminal> {
        #[cfg(all(test, unix))]
        if let Some(shell) = &self.terminal_shell {
            return crate::terminal::DaemonTerminal::open_with_shell(
                cwd,
                cols,
                rows,
                events,
                shell.clone(),
            );
        }
        ensure_shell_environment();
        crate::terminal::DaemonTerminal::open(cwd, cols, rows, events)
    }

    /// Capture and persist one ending checkpoint exactly once per daemon.
    /// Desktop and Web may observe the same turn completion concurrently; a
    /// per-turn lock prevents both clients from running the expensive Git
    /// snapshot while leaving unrelated tasks independent.
    fn capture_turn_checkpoint(
        &self,
        cwd: PathBuf,
        session_id: Uuid,
        turn_count: usize,
    ) -> anyhow::Result<Checkpoint> {
        let key = (cwd.clone(), session_id, turn_count);
        let capture_lock = self
            .checkpoint_capture_locks
            .lock()
            .entry(key)
            .or_insert_with(|| Arc::new(Mutex::new(())))
            .clone();
        let _capture = capture_lock.lock();

        {
            let mut state = self.task_state.lock();
            if let Some(index) = state
                .sessions
                .iter()
                .position(|session| session.id == session_id)
            {
                self.task_store.hydrate(&mut state.sessions[index])?;
                if let Some(checkpoint) = state.sessions[index]
                    .turns
                    .iter()
                    .find(|turn| turn.turn_count == turn_count)
                    .and_then(|turn| turn.checkpoint.as_ref())
                    .filter(|checkpoint| {
                        matches!(
                            checkpoint.status,
                            CheckpointStatus::Ready | CheckpointStatus::Unavailable
                        )
                    })
                {
                    return Ok(checkpoint.clone());
                }
            }
        }

        let checkpoint = crate::checkpoint::capture_turn(&cwd, session_id, turn_count)?;
        let mut state = self.task_state.lock();
        if let Some(index) = state
            .sessions
            .iter()
            .position(|session| session.id == session_id)
        {
            self.task_store.hydrate(&mut state.sessions[index])?;
            if let Some(turn) = state.sessions[index]
                .turns
                .iter_mut()
                .find(|turn| turn.turn_count == turn_count)
            {
                turn.checkpoint = Some(checkpoint.clone());
                state.mark_session_dirty(session_id);
                self.task_store.save(&mut state)?;
            }
        }
        Ok(checkpoint)
    }
}

/// Storage-layout migrations belong to the daemon because both the database
/// rows and the directories name paths on its host. Persist after each move
/// so a later failure cannot leave an earlier project pointing at its old
/// location in SQLite.
fn migrate_projectless_state(
    task_store: &StateStore,
    task_state: &mut PersistedState,
) -> anyhow::Result<()> {
    let indices = task_state
        .projects
        .iter()
        .enumerate()
        .filter_map(|(index, project)| {
            crate::projectless::needs_migration(&project.path).then_some(index)
        })
        .collect::<Vec<_>>();
    for index in indices {
        let old_path = task_state.projects[index].path.clone();
        let workspace = crate::projectless::migrate_workspace(&old_path).with_context(|| {
            format!(
                "could not move projectless workspace {} under ~/.waku/projects",
                old_path.display()
            )
        })?;
        task_state.projects[index].name = crate::model::Project::PROJECTLESS_NAME.to_owned();
        task_state.projects[index].path = workspace.cwd;
        task_store
            .save(task_state)
            .context("could not persist migrated projectless workspace")?;
    }
    Ok(())
}

impl Backend for WakuBackend {
    fn handle(&self, request: Request, events: EventSink) -> anyhow::Result<ResponsePayload> {
        let session_id = request.session_id;
        let runtime_id = request.runtime_id;
        // Start, prompt and deletion of one desktop identity share a fence.
        // The daemon worker owns this lock; no UI/render thread waits on it.
        let lifecycle_lock = self
            .session_lifecycle_locks
            .lock()
            .entry(session_id)
            .or_insert_with(|| Arc::new(Mutex::new(())))
            .clone();
        let _lifecycle = lifecycle_lock.lock();
        if !matches!(
            &request.command,
            Command::RemoveSession | Command::CloseSession
        ) && self.removed_session_ids.lock().contains(&session_id)
        {
            bail!("会话正在删除或已删除，请重试删除以完成操作");
        }
        if !matches!(
            &request.command,
            Command::ForkSessionFromResponse { .. }
                | Command::RewindSessionToMessage { .. }
                | Command::AttachSession
                | Command::CloseSession
                | Command::HydrateSession { .. }
        ) && self.history_pending(session_id)?
        {
            bail!(
                "A conversation history operation is pending; retry the original fork or rewind to reconcile it"
            );
        }
        // A rewind's eligibility check, intent, workspace restore, and commit
        // share the same fence used to admit work into this Git worktree.
        let workspace_admission = self.history_admission_lock(session_id, &request.command)?;
        let _workspace_admission = workspace_admission.as_ref().map(|(lock, _)| lock.lock());
        if let Some((_, cwd)) = &workspace_admission {
            if !matches!(
                &request.command,
                Command::ForkSessionFromResponse { .. } | Command::RewindSessionToMessage { .. }
            ) && self.history_workspace_pending(cwd)?
            {
                bail!(
                    "The workspace has a pending conversation rewind; retry it before starting work"
                );
            }
        }
        match request.command {
            Command::AttachSession => {
                let pending_history = self.pending_history_operation(session_id)?;
                let sessions = self.sessions.lock();
                let Some((runtime_id, driver)) = sessions.get(&session_id) else {
                    return Ok(ResponsePayload::SessionRuntime {
                        runtime_id: None,
                        supports_steer: false,
                        steering_run_id: None,
                        conversation_capability: None,
                        deerflow_ready: None,
                        pending_history,
                    });
                };
                Ok(ResponsePayload::SessionRuntime {
                    runtime_id: Some(*runtime_id),
                    supports_steer: driver.supports_steer(),
                    steering_run_id: driver.steering_run_id(),
                    conversation_capability: self
                        .conversation_capabilities
                        .lock()
                        .get(&session_id)
                        .filter(|(id, _)| id == runtime_id)
                        .map(|(_, supported)| *supported),
                    deerflow_ready: self
                        .deerflow_ready
                        .lock()
                        .get(&session_id)
                        .filter(|(id, _)| id == runtime_id)
                        .map(|(_, ready)| ready.clone()),
                    pending_history,
                })
            }
            Command::GetSettings => Ok(ResponsePayload::Settings {
                settings: self.settings.get(),
            }),
            Command::DeerFlow { operation, input } => Ok(ResponsePayload::DeerFlow {
                data: self.deerflow.request(&operation, input)?,
            }),
            Command::UpdateSettings { settings } => {
                self.settings.replace(settings)?;
                Ok(ResponsePayload::Ack)
            }
            Command::ProbeProvider {
                provider,
                binary_override,
                discover_models,
                probe_version,
            } => {
                anyhow::ensure!(provider.is_available(), "{} sessions are read-only", provider.display_name());
                ensure_shell_environment();
                let mut probe = crate::model::provider_probe(provider, binary_override.as_deref());
                let version = probe_version
                    .then(|| {
                        probe
                            .path
                            .as_deref()
                            .and_then(crate::model::probe_provider_version)
                    })
                    .flatten();
                if discover_models {
                    probe = crate::model::discover_provider_models(probe);
                }
                Ok(ResponsePayload::ProviderProbe { probe, version })
            }
            Command::ProbeComputerPermissions { prompt } => {
                Ok(ResponsePayload::ComputerPermissions {
                    permissions: crate::computer_use::probe_permissions(prompt)?,
                })
            }
            Command::LoadSkills { projects } => {
                let locations = crate::skills::skill_locations(&projects);
                Ok(ResponsePayload::SkillsCatalog {
                    catalog: crate::skills::scan_skills(&locations),
                })
            }
            Command::SetSkillsEnabled { dirs, enabled } => {
                for dir in dirs {
                    crate::skills::set_skill_enabled(&dir, enabled)
                        .map_err(|error| anyhow!(error))?;
                }
                Ok(ResponsePayload::Ack)
            }
            Command::TrashSkills { dirs } => {
                crate::skills::trash_skills(&dirs).map_err(|error| anyhow!(error))?;
                Ok(ResponsePayload::Ack)
            }
            Command::LoadTaskState => {
                let state = self.task_state.lock();
                Ok(ResponsePayload::TaskState {
                    projects: state.projects.clone(),
                    sessions: state
                        .sessions
                        .iter()
                        .map(AgentSession::list_projection)
                        .collect(),
                    default_cwd: self.default_cwd.clone(),
                    projectless_root: crate::projectless::workspace_root(),
                })
            }
            Command::SaveTaskState {
                projects,
                live_session_ids: _,
                sessions,
            } => {
                let active_runtimes = self
                    .sessions
                    .lock()
                    .iter()
                    .map(|(session_id, (runtime_id, _))| (*session_id, *runtime_id))
                    .collect::<HashMap<_, _>>();
                let mut state = self.task_state.lock();
                let mut positions = state.sessions.iter().enumerate()
                    .map(|(index, session)| (session.id, index))
                    .collect::<HashMap<_, _>>();
                let sessions = {
                    let removed = self.removed_session_ids.lock();
                    sessions.into_iter().filter(|session| !removed.contains(&session.id)).collect::<Vec<_>>()
                };
                for incoming in &sessions {
                    match positions.get(&incoming.id) {
                        Some(&index) => anyhow::ensure!(
                            state.sessions[index].provider == incoming.provider,
                            "A stored session cannot change its agent provider"
                        ),
                        None => anyhow::ensure!(incoming.provider.is_available(), "Archived sessions are read-only"),
                    }
                }
                for project in projects {
                    if let Some(existing) = state
                        .projects
                        .iter_mut()
                        .find(|existing| existing.id == project.id)
                    {
                        *existing = project;
                    } else {
                        state.projects.push(project);
                    }
                }
                let saved_ids = sessions
                    .iter()
                    .map(|session| session.id)
                    .collect::<Vec<_>>();
                for mut session in sessions {
                    if self.history_pending(session.id)? {
                        continue;
                    }
                    if let Some(&index) = positions.get(&session.id) {
                        let existing = &mut state.sessions[index];
                        self.task_store.hydrate(existing)?;
                        if !existing.provider.is_available() {
                            if session.updated_at >= existing.updated_at {
                                existing.title = session.title;
                                existing.project_id = session.project_id;
                                existing.updated_at = session.updated_at;
                            }
                            continue;
                        }
                        if session.history_revision != existing.history_revision {
                            continue;
                        }
                        if session_projection_precedes(
                            existing,
                            &session,
                            active_runtimes.get(&session.id).copied(),
                        ) {
                            merge_stale_session_metadata(existing, session);
                        } else {
                            preserve_daemon_checkpoints(existing, &mut session);
                            *existing = session;
                        }
                    } else {
                        positions.insert(session.id, state.sessions.len());
                        state.sessions.push(session);
                    }
                }
                let used_project_ids = state
                    .sessions
                    .iter()
                    .map(|session| session.project_id)
                    .collect::<std::collections::HashSet<_>>();
                state.projects.retain(|project| {
                    !project.is_projectless() || used_project_ids.contains(&project.id)
                });
                for session_id in &saved_ids {
                    state.mark_session_dirty(*session_id);
                }
                self.task_store.save(&mut state)?;
                let sessions = saved_ids
                    .into_iter()
                    .filter_map(|session_id| {
                        state
                            .sessions
                            .iter()
                            .find(|session| session.id == session_id)
                            .cloned()
                    })
                    .collect();
                // The save above can adopt full transcripts for every session
                // the client touched. Keep only the recent window resident;
                // the echoed clones above still carry the saved detail.
                trim_resident_transcripts(&mut state, &active_runtimes.keys().copied().collect());
                Ok(ResponsePayload::TaskStateSaved { sessions })
            }
            Command::RemoveSession => {
                self.deletion_context().remove_session(session_id)?;
                Ok(ResponsePayload::Ack)
            }
            Command::HydrateSession { session_id } => {
                // Live runtimes stay resident; everything else is trimmed to
                // the recency window once the response is built.
                let pinned = self.sessions.lock().keys().copied().collect();
                let mut state = self.task_state.lock();
                let session = if let Some(session) = state
                    .sessions
                    .iter_mut()
                    .find(|session| session.id == session_id)
                {
                    self.task_store.hydrate(session)?;
                    Some(session.clone())
                } else {
                    None
                };
                trim_resident_transcripts(&mut state, &pinned);
                Ok(ResponsePayload::Session { session })
            }
            Command::SearchSessionMessages { query, limit } => {
                let matches = self.task_store.session_message_search(query, limit)()?;
                Ok(ResponsePayload::SessionMessageMatches { matches })
            }
            Command::ListProviderSessions { provider, limit } => {
                anyhow::ensure!(provider.is_available(), "{} sessions are read-only", provider.display_name());
                const MAX_PROVIDER_SESSIONS: usize = 500;
                let limit = limit.min(MAX_PROVIDER_SESSIONS);
                if limit == 0 {
                    return Ok(ResponsePayload::ProviderSessions {
                        sessions: Vec::new(),
                    });
                }
                ensure_shell_environment();
                let settings = self.settings.get();
                if settings.disabled_providers.contains(&provider) {
                    return Ok(ResponsePayload::ProviderSessions {
                        sessions: Vec::new(),
                    });
                }
                let binary_override = settings
                    .provider_binary_overrides
                    .get(&provider)
                    .map(String::as_str);
                let Some(binary) = crate::model::provider_probe(provider, binary_override).path
                else {
                    return Ok(ResponsePayload::ProviderSessions {
                        sessions: Vec::new(),
                    });
                };
                let mut sessions = crate::acp_session::list_provider_sessions(provider, &binary, &[], limit)?;
                sessions.sort_by(|a, b| {
                    b.updated_at
                        .cmp(&a.updated_at)
                        .then_with(|| a.title.cmp(&b.title))
                });
                let imported = {
                    let state = self.task_state.lock();
                    state
                        .sessions
                        .iter()
                        .filter_map(|session| session.provider_cursor.as_ref())
                        .map(|cursor| (cursor.provider(), cursor.native_id().to_owned()))
                        .collect::<HashSet<_>>()
                };
                sessions.retain(|session| {
                    !imported.contains(&(session.provider(), session.cursor.native_id().to_owned()))
                });
                sessions.truncate(limit);
                Ok(ResponsePayload::ProviderSessions { sessions })
            }
            Command::LoadProviderSession { cursor, cwd } => {
                // Preserve every native turn shell for exact provider turn
                // numbering, but bound imported display text to recent turns.
                const VISIBLE_TURN_LIMIT: usize = 100;
                let ProviderResumeCursor::DeerFlow { session_id } = &cursor else {
                    bail!("{} sessions are read-only", cursor.provider().display_name());
                };
                let binary = self.provider_binary(ProviderKind::DeerFlow)?;
                let history = crate::acp_session::provider_session_history(
                    ProviderKind::DeerFlow, &binary, &cwd, session_id, VISIBLE_TURN_LIMIT, &self.task_store.blobs(),
                )?;
                Ok(ResponsePayload::ProviderSessionHistory { history })
            }
            Command::LoadComposerDrafts => Ok(ResponsePayload::ComposerDrafts {
                drafts: self.composer_drafts.load()?,
            }),
            Command::SaveComposerDrafts {
                mut drafts,
                generation,
            } => {
                let blocked = self.removed_session_ids.lock();
                drafts.sessions.retain(|id, _| !blocked.contains(id));
                self.composer_drafts.save(drafts, generation)?;
                Ok(ResponsePayload::Ack)
            }
            Command::ApplyComposerDraftChanges { mut changes } => {
                let blocked = self.removed_session_ids.lock();
                changes.retain(|change| !matches!(change.target, crate::persistence::ComposerDraftTarget::Session { session_id } if blocked.contains(&session_id)));
                self.composer_drafts.apply_changes(changes)?;
                Ok(ResponsePayload::Ack)
            }
            Command::StoreBlob { mime_type, bytes } => {
                let reference = self
                    .task_store
                    .blobs()
                    .store_image_bytes(&mime_type, &bytes)?;
                let path = self
                    .task_store
                    .blobs()
                    .path_for(&reference)
                    .ok_or_else(|| anyhow!("stored blob has no daemon path"))?;
                Ok(ResponsePayload::BlobStored { reference, path })
            }
            Command::ImportAttachment { name, upload } => Ok(ResponsePayload::AttachmentStored {
                attachment: self.attachments.import(&name, upload)?,
            }),
            Command::ImportPathAttachment { path } => Ok(ResponsePayload::AttachmentStored {
                attachment: self.attachments.import_path(&path)?,
            }),
            Command::ReadBlob { reference } => {
                let path = self
                    .task_store
                    .blobs()
                    .path_for(&reference)
                    .ok_or_else(|| anyhow!("invalid blob reference"))?;
                Ok(ResponsePayload::BlobData {
                    bytes: std::fs::read(path)?,
                })
            }
            Command::ReadAttachment { reference, path } => Ok(ResponsePayload::BlobData {
                bytes: self.attachments.read_file(&reference, &path)?,
            }),
            Command::SweepBlobs => {
                self.task_store.blob_sweep()();
                Ok(ResponsePayload::Ack)
            }
            Command::ForkSessionFromResponse {
                turn_count,
                operation_id,
            } => {
                let session = self.deerflow_history_mutation(session_id, turn_count, "fork", operation_id)?;
                Ok(ResponsePayload::SessionForked {
                    session,
                    checkpoint_warning: None,
                })
            }
            Command::RewindSessionToMessage {
                turn_count,
                operation_id,
            } => {
                let session = self.deerflow_history_mutation(session_id, turn_count, "rewind", operation_id)?;
                Ok(ResponsePayload::SessionRewound {
                    session,
                    cleanup_warning: None,
                })
            }
            Command::Workspace {
                operation:
                    WorkspaceOperation::CaptureTurn {
                        cwd,
                        session_id,
                        turn_count,
                    },
            } => Ok(ResponsePayload::Workspace {
                result: WorkspaceResult::Checkpoint {
                    checkpoint: self.capture_turn_checkpoint(cwd, session_id, turn_count)?,
                },
            }),
            Command::Workspace { operation } => Ok(ResponsePayload::Workspace {
                result: crate::workspace::execute(operation)?,
            }),
            Command::OpenTerminal { cwd, cols, rows } => {
                let terminal = self.open_terminal(&cwd, cols, rows, events)?;
                let previous = self
                    .terminals
                    .lock()
                    .insert(session_id, (runtime_id, terminal));
                drop(previous);
                Ok(ResponsePayload::Ack)
            }
            Command::WriteTerminal { data } => {
                let terminals = self.terminals.lock();
                let (active_runtime_id, terminal) = terminals
                    .get(&session_id)
                    .ok_or_else(|| anyhow!("daemon terminal {session_id} is not running"))?;
                if *active_runtime_id != runtime_id {
                    bail!(
                        "daemon terminal {session_id} belongs to runtime {active_runtime_id}, not {runtime_id}"
                    );
                }
                terminal.write(data)?;
                Ok(ResponsePayload::Ack)
            }
            Command::ResizeTerminal { cols, rows } => {
                let terminals = self.terminals.lock();
                let (active_runtime_id, terminal) = terminals
                    .get(&session_id)
                    .ok_or_else(|| anyhow!("daemon terminal {session_id} is not running"))?;
                if *active_runtime_id != runtime_id {
                    bail!(
                        "daemon terminal {session_id} belongs to runtime {active_runtime_id}, not {runtime_id}"
                    );
                }
                terminal.resize(cols, rows);
                Ok(ResponsePayload::Ack)
            }
            Command::CloseTerminal => {
                let removed = {
                    let mut terminals = self.terminals.lock();
                    if let Some((active_runtime_id, _)) = terminals.get(&session_id) {
                        if *active_runtime_id != runtime_id {
                            bail!(
                                "daemon terminal {session_id} belongs to runtime {active_runtime_id}, not {runtime_id}"
                            );
                        }
                    }
                    terminals.remove(&session_id)
                };
                drop(removed);
                Ok(ResponsePayload::Ack)
            }
            Command::Start { options } => {
                let provider: ProviderKind = decode_enum(&options.provider)?;
                anyhow::ensure!(provider.is_available(), "{} sessions are read-only", provider.display_name());
                anyhow::ensure!(
                    !self.task_state.lock().sessions.iter().any(|s| s.id == session_id && !s.provider.is_available()),
                    "Archived sessions are read-only"
                );
                let workspace_identity = session_history::workspace_identity(&options.cwd)?;
                if self.history_workspace_pending(&options.cwd)? {
                    bail!(
                        "The workspace has a pending conversation rewind; reconcile it before starting work"
                    );
                }
                let options = DriverStartOptions {
                    binary: options.binary,
                    cwd: options.cwd,
                    mode: decode_enum(&options.mode)?,
                    model: options.model,
                    reasoning_effort: options.reasoning_effort,
                    thinking_mode: options.thinking_mode,
                    service_tier: options.service_tier,
                    context_window: options.context_window,
                    agent_preset: options.agent_preset,
                    computer_use_enabled: options.computer_use_enabled,
                    provider_cursor: options
                        .provider_cursor
                        .map(serde_json::from_value)
                        .transpose()
                        .context("daemon received an invalid provider cursor")?,
                };
                anyhow::ensure!(
                    options.provider_cursor.as_ref().is_none_or(|cursor| cursor.provider() == provider),
                    "This cursor belongs to a different agent provider"
                );
                let previous = self.sessions.lock().remove(&session_id);
                drop(previous);
                self.conversation_capabilities.lock().remove(&session_id);
                self.deerflow_ready.lock().remove(&session_id);
                let (wake, _wake_events) = smol::channel::bounded(1);
                let (event_sender, event_receiver) = driver::event_channel(wake);
                let handle = driver::start_local(provider, options, event_sender)?;
                let supports_steer = handle.supports_steer();
                // Register before forwarding: a failed launch can exit immediately.
                self.sessions
                    .lock()
                    .insert(session_id, (runtime_id, handle));
                self.runtime_workspaces
                    .lock()
                    .insert(session_id, (runtime_id, workspace_identity.clone()));
                let runtimes = Arc::downgrade(&self.sessions);
                let active_workspace_prompts = self.active_workspace_prompts.clone();
                let runtime_workspaces = self.runtime_workspaces.clone();
                let conversation_capabilities = self.conversation_capabilities.clone();
                let deerflow_ready = self.deerflow_ready.clone();
                let forwarding = std::thread::Builder::new()
                    .name(format!("waku-daemon-events-{session_id}"))
                    .spawn(move || {
                        while let Ok(event) = event_receiver.recv() {
                            let exited = matches!(event, DriverEvent::ProcessExited);
                            if matches!(event, DriverEvent::TurnStarted) {
                                if runtime_workspaces
                                    .lock()
                                    .get(&session_id)
                                    .is_some_and(|(id, _)| *id == runtime_id)
                                {
                                    active_workspace_prompts.lock().insert(
                                        session_id,
                                        (runtime_id, workspace_identity.clone()),
                                    );
                                }
                            }
                            if let DriverEvent::DeerFlowTasks(task_event) = &event {
                                if let Some(runtimes) = runtimes.upgrade() {
                                    cache_deerflow_ready(
                                        &runtimes,
                                        &deerflow_ready,
                                        session_id,
                                        runtime_id,
                                        task_event,
                                    );
                                }
                            }
                            if let DriverEvent::ConversationCapability { supported } = &event {
                                if let Some(runtimes) = runtimes.upgrade() {
                                    let runtimes = runtimes.lock();
                                    if runtimes
                                        .get(&session_id)
                                        .is_some_and(|(id, _)| *id == runtime_id)
                                    {
                                        conversation_capabilities
                                            .lock()
                                            .insert(session_id, (runtime_id, *supported));
                                    }
                                }
                            }
                            if matches!(
                                event,
                                DriverEvent::TurnFinished { .. } | DriverEvent::ProcessExited
                            ) {
                                remove_finished_runtime(
                                    &active_workspace_prompts,
                                    session_id,
                                    runtime_id,
                                );
                            }
                            if exited {
                                remove_finished_runtime(
                                    &runtime_workspaces,
                                    session_id,
                                    runtime_id,
                                );
                                {
                                    let mut capabilities = conversation_capabilities.lock();
                                    if capabilities
                                        .get(&session_id)
                                        .is_some_and(|(id, _)| *id == runtime_id)
                                    {
                                        capabilities.remove(&session_id);
                                    }
                                    remove_finished_runtime(
                                        &deerflow_ready,
                                        session_id,
                                        runtime_id,
                                    );
                                }
                                if let Some(runtimes) = runtimes.upgrade() {
                                    remove_finished_runtime(&runtimes, session_id, runtime_id);
                                }
                            }
                            let wire = event_to_wire(event).unwrap_or_else(|error| {
                                WireDriverEvent::new(
                                    "error",
                                    Value::String(format!(
                                        "could not encode daemon event: {error}"
                                    )),
                                )
                            });
                            if events.send(wire).is_err() || exited {
                                break;
                            }
                        }
                    });
                if let Err(error) = forwarding {
                    remove_finished_runtime(&self.sessions, session_id, runtime_id);
                    remove_finished_runtime(&self.runtime_workspaces, session_id, runtime_id);
                    return Err(error).context("could not start daemon event forwarding thread");
                }
                Ok(ResponsePayload::Started { supports_steer })
            }
            Command::CloseSession => {
                let removed = {
                    let mut sessions = self.sessions.lock();
                    sessions
                        .get(&session_id)
                        .is_some_and(|(active_runtime_id, _)| *active_runtime_id == runtime_id)
                        .then(|| sessions.remove(&session_id))
                        .flatten()
                };
                drop(removed);
                remove_finished_runtime(&self.runtime_workspaces, session_id, runtime_id);
                remove_finished_runtime(&self.active_workspace_prompts, session_id, runtime_id);
                Ok(ResponsePayload::Ack)
            }
            command => {
                let driver = {
                    let sessions = self.sessions.lock();
                    let (active_runtime_id, driver) = sessions
                        .get(&session_id)
                        .ok_or_else(|| anyhow!("daemon session {session_id} is not running"))?;
                    if *active_runtime_id != runtime_id {
                        bail!(
                            "daemon session {session_id} belongs to runtime {active_runtime_id}, not {runtime_id}"
                        );
                    }
                    driver.clone()
                };
                if let Command::Prompt {
                    prompt,
                    turn_id,
                    message_id,
                }
                | Command::PromptWithAttachments {
                    prompt,
                    turn_id,
                    message_id,
                    ..
                } = &command
                {
                    let cwd = {
                        let state = self.task_state.lock();
                        state
                            .sessions
                            .iter()
                            .find(|session| session.id == session_id)
                            .and_then(|session| {
                                session.workspace.path().map(Path::to_path_buf).or_else(|| {
                                    state
                                        .projects
                                        .iter()
                                        .find(|project| project.id == session.project_id)
                                        .map(|project| project.path.clone())
                                })
                            })
                    }
                    .or_else(|| {
                        self.runtime_workspaces
                            .lock()
                            .get(&session_id)
                            .filter(|(id, _)| *id == runtime_id)
                            .map(|(_, path)| path.clone())
                    });
                    if cwd
                        .as_ref()
                        .is_some_and(|cwd| self.history_workspace_pending(cwd).unwrap_or(true))
                    {
                        bail!(
                            "The workspace has a pending conversation rewind; reconcile it before sending a new message"
                        );
                    }
                    if let Some(cwd) = &cwd {
                        self.active_workspace_prompts.lock().insert(
                            session_id,
                            (runtime_id, session_history::workspace_identity(cwd)?),
                        );
                    }
                    // Publish the submission into the runtime's event stream
                    // before the provider can start the turn. Every attached
                    // client mirrors the user message and its turn from this
                    // event, so the submitting client's own save is no longer
                    // the only record of the prompt — a follower that only
                    // knew the provider's `turnStarted` used to persist a
                    // projection without it, erasing the message for everyone.
                    if let Err(error) = events.send(event_to_wire(DriverEvent::PromptSubmitted {
                        message: prompt.clone(),
                        turn_id: turn_id.unwrap_or_else(Uuid::new_v4),
                        message_id: message_id.unwrap_or_else(Uuid::new_v4),
                    })?) {
                        remove_finished_runtime(
                            &self.active_workspace_prompts,
                            session_id,
                            runtime_id,
                        );
                        return Err(error.into());
                    }
                }
                handle_driver_command(&driver, command)
            }
        }
    }

    fn shutdown(&self) {
        self.deerflow.shutdown();
        let sessions = std::mem::take(&mut *self.sessions.lock());
        drop(sessions);
        let terminals = std::mem::take(&mut *self.terminals.lock());
        drop(terminals);
    }
}

/// Ready belongs to the live ACP connection. Task-data invalidation asks clients
/// to refresh their lists; it does not revoke this connection or its capabilities.
fn cache_deerflow_ready<T>(
    runtimes: &Mutex<HashMap<Uuid, (Uuid, T)>>,
    ready: &Mutex<HashMap<Uuid, (Uuid, waku_protocol::deerflow_tasks::Event)>>,
    session_id: Uuid,
    runtime_id: Uuid,
    event: &waku_protocol::deerflow_tasks::Event,
) {
    let runtimes = runtimes.lock();
    if !runtimes
        .get(&session_id)
        .is_some_and(|(id, _)| *id == runtime_id)
    {
        return;
    }
    if let waku_protocol::deerflow_tasks::Event::Ready { .. } = event {
        ready.lock().insert(session_id, (runtime_id, event.clone()));
    }
}

/// Late exit events from an old driver must never remove its replacement.
fn remove_finished_runtime<T>(
    runtimes: &Mutex<HashMap<Uuid, (Uuid, T)>>,
    session_id: Uuid,
    runtime_id: Uuid,
) {
    let removed = {
        let mut runtimes = runtimes.lock();
        if runtimes
            .get(&session_id)
            .is_some_and(|(active, _)| *active == runtime_id)
        {
            runtimes.remove(&session_id)
        } else {
            None
        }
    };
    drop(removed);
}

fn session_projection_precedes(
    existing: &AgentSession,
    incoming: &AgentSession,
    active_runtime_id: Option<Uuid>,
) -> bool {
    let existing_cursor = existing.runtime_event_cursor;
    let incoming_cursor = incoming.runtime_event_cursor;
    if let Some(active_runtime_id) = active_runtime_id {
        let existing_is_active =
            existing_cursor.is_some_and(|cursor| cursor.runtime_id == active_runtime_id);
        let incoming_is_active =
            incoming_cursor.is_some_and(|cursor| cursor.runtime_id == active_runtime_id);
        if existing_is_active != incoming_is_active {
            return existing_is_active;
        }
    }
    match (existing_cursor, incoming_cursor) {
        (Some(existing), Some(incoming))
            if existing.runtime_id == incoming.runtime_id && existing.epoch == incoming.epoch =>
        {
            incoming.sequence < existing.sequence
        }
        (Some(_), None) if existing.status.is_busy() => true,
        _ => incoming.updated_at < existing.updated_at,
    }
}

fn merge_stale_session_metadata(existing: &mut AgentSession, incoming: AgentSession) {
    if incoming.updated_at >= existing.updated_at {
        existing.title = incoming.title;
        existing.project_id = incoming.project_id;
        existing.workspace = incoming.workspace;
        existing.provider = incoming.provider;
        existing.model = incoming.model;
        existing.runtime_mode = incoming.runtime_mode;
        existing.reasoning_effort = incoming.reasoning_effort;
        existing.thinking_mode = incoming.thinking_mode;
        existing.service_tier = incoming.service_tier;
        existing.context_window = incoming.context_window;
        existing.agent_preset = incoming.agent_preset;
        existing.updated_at = incoming.updated_at;
        existing.last_reply_at = incoming.last_reply_at.or(existing.last_reply_at);
    }
    for queued in incoming.queued_messages {
        if !existing
            .queued_messages
            .iter()
            .any(|candidate| candidate.id == queued.id)
        {
            existing.queued_messages.push(queued);
        }
    }
}

/// Ending checkpoints are produced and stored by the daemon. A second client
/// may still save a projection created just before capture completed; never
/// let that stale projection erase the canonical Git snapshot.
fn preserve_daemon_checkpoints(existing: &AgentSession, incoming: &mut AgentSession) {
    for turn in &mut incoming.turns {
        let Some(checkpoint) = existing
            .turns
            .iter()
            .find(|candidate| candidate.turn_count == turn.turn_count)
            .and_then(|candidate| candidate.checkpoint.as_ref())
            .filter(|checkpoint| {
                matches!(
                    checkpoint.status,
                    CheckpointStatus::Ready | CheckpointStatus::Unavailable
                )
            })
        else {
            continue;
        };
        turn.checkpoint = Some(checkpoint.clone());
    }
}

impl WakuBackend {
    fn provider_binary(&self, provider: ProviderKind) -> anyhow::Result<PathBuf> {
        ensure_shell_environment();
        let settings = self.settings.get();
        let binary_override = settings
            .provider_binary_overrides
            .get(&provider)
            .map(String::as_str);
        crate::model::provider_probe(provider, binary_override)
            .path
            .ok_or_else(|| anyhow!("{} is not installed on the daemon", provider.display_name()))
    }
}

fn validate_message_rewind(source: &AgentSession, turn_count: usize) -> anyhow::Result<()> {
    if !matches!(source.status, SessionStatus::Idle | SessionStatus::Failed) {
        bail!("stop the task before editing a prior message");
    }
    let Some(turn) = source
        .turns
        .iter()
        .find(|turn| turn.turn_count == turn_count)
    else {
        bail!("the selected message is unavailable");
    };
    if !source.messages.iter().any(|message| {
        message.turn_id == Some(turn.id) && message.role == crate::model::MessageRole::User
    }) {
        bail!("the selected user message is unavailable");
    }
    let rollback_turns = source.provider_turns_after(turn_count.saturating_sub(1));
    if rollback_turns > 0 && source.provider_cursor.is_none() {
        bail!("the provider conversation is unavailable");
    }
    Ok(())
}

fn validate_response_fork(source: &AgentSession, turn_count: usize) -> anyhow::Result<()> {
    if !matches!(source.status, SessionStatus::Idle | SessionStatus::Failed) {
        bail!("stop the task before forking a response");
    }
    let cursor = source
        .provider_cursor
        .as_ref()
        .ok_or_else(|| anyhow!("the provider conversation is unavailable"))?;
    if cursor.provider() != source.provider {
        bail!("the provider conversation does not match this task");
    }
    if source
        .turns
        .get(turn_count.saturating_sub(1))
        .is_none_or(|turn| turn.turn_count != turn_count || !turn.provider_turn_started)
    {
        bail!("the selected response cannot be forked");
    }
    Ok(())
}

fn numbered_title_suffix(title: &str) -> Option<(&str, usize)> {
    let (base, suffix) = title.rsplit_once(" (")?;
    let number = suffix.strip_suffix(')')?.parse().ok()?;
    (!base.is_empty() && number >= 2).then_some((base, number))
}

fn next_response_fork_title<'a>(
    source_title: &str,
    existing_titles: impl IntoIterator<Item = &'a str>,
) -> String {
    let existing_titles = existing_titles.into_iter().collect::<Vec<_>>();
    let base = numbered_title_suffix(source_title)
        .filter(|(base, _)| existing_titles.iter().any(|title| title == base))
        .map_or(source_title, |(base, _)| base);
    let highest_number = existing_titles
        .iter()
        .filter_map(|title| {
            if *title == base {
                Some(1)
            } else {
                numbered_title_suffix(title)
                    .filter(|(candidate_base, _)| *candidate_base == base)
                    .map(|(_, number)| number)
            }
        })
        .max()
        .unwrap_or(1);
    format!("{base} ({})", highest_number.saturating_add(1).max(2))
}

fn handle_driver_command(
    driver: &DriverHandle,
    command: Command,
) -> anyhow::Result<ResponsePayload> {
    match command {
        Command::Prompt { prompt, .. } => driver.prompt(prompt),
        Command::PromptWithAttachments {
            prompt,
            attachments,
            ..
        } => driver.prompt_with_attachments(prompt, attachments),
        Command::Steer { prompt, run_id } => match run_id {
            Some(run) => driver.steer_run(prompt, run),
            None => driver.steer(prompt),
        },
        Command::Cancel => driver.cancel(),
        Command::CancelComputerUse => driver.cancel_computer_use(),
        Command::RefreshBackgroundWork => driver.refresh_background_work(),
        Command::DeerFlowTasks { command } => driver.deerflow_tasks(command),
        Command::StopBackgroundWork { key, control_id } => {
            driver.stop_background_work(
                serde_json::from_value(key).context("invalid background-work key")?,
                control_id,
            );
        }
        Command::Respond {
            request_id,
            option_id,
        } => driver.respond(request_id, option_id).map_err(anyhow::Error::msg)?,

        Command::RespondCredential { request_id, sealed } => driver.respond_credential(request_id, sealed).map_err(anyhow::Error::msg)?,
        Command::RespondUserInput {
            request_id,
            answers,
        } => driver.respond_user_input(request_id, answers),
        Command::RunComputerTool { request } => {
            driver.run_computer_tool(crate::computer_use::ComputerToolRequest {
                call_id: request.call_id,
                tool: request.tool,
                arguments: request.arguments,
            });
        }
        Command::RejectComputerTool { request, reason } => {
            driver.reject_computer_tool(
                crate::computer_use::ComputerToolRequest {
                    call_id: request.call_id,
                    tool: request.tool,
                    arguments: request.arguments,
                },
                reason,
            );
        }
        Command::ApplyOptions { options } => {
            return Ok(ResponsePayload::OptionsApplied {
                applied: driver.apply_options(SessionOptions {
                    mode: decode_enum(&options.mode)?,
                    model: options.model,
                    reasoning_effort: options.reasoning_effort,
                    thinking_mode: options.thinking_mode,
                    service_tier: options.service_tier,
                    context_window: options.context_window,
                }),
            });
        }
        Command::SetToolApproval { mode } => {
            return Ok(ResponsePayload::ToolApproval {
                mode: driver.set_tool_approval(mode)?,
            });
        }
        Command::AttachSession
        | Command::ReplayEvents { .. }
        | Command::Start { .. }
        | Command::GetSettings
        | Command::DeerFlow { .. }
        | Command::UpdateSettings { .. }
        | Command::ProbeProvider { .. }
        | Command::ProbeComputerPermissions { .. }
        | Command::LoadSkills { .. }
        | Command::SetSkillsEnabled { .. }
        | Command::TrashSkills { .. }
        | Command::LoadTaskState
        | Command::SaveTaskState { .. }
        | Command::RemoveSession
        | Command::HydrateSession { .. }
        | Command::SearchSessionMessages { .. }
        | Command::ListProviderSessions { .. }
        | Command::LoadProviderSession { .. }
        | Command::LoadComposerDrafts
        | Command::SaveComposerDrafts { .. }
        | Command::ApplyComposerDraftChanges { .. }
        | Command::StoreBlob { .. }
        | Command::ImportAttachment { .. }
        | Command::ImportPathAttachment { .. }
        | Command::ReadBlob { .. }
        | Command::ReadAttachment { .. }
        | Command::SweepBlobs
        | Command::ForkSessionFromResponse { .. }
        | Command::RewindSessionToMessage { .. }
        | Command::Workspace { .. }
        | Command::OpenTerminal { .. }
        | Command::WriteTerminal { .. }
        | Command::ResizeTerminal { .. }
        | Command::CloseTerminal
        | Command::CloseSession => {
            bail!("daemon received a command in the wrong dispatch path")
        }
    }
    Ok(ResponsePayload::Ack)
}

fn ensure_shell_environment() {
    static REFRESHED: OnceLock<()> = OnceLock::new();
    REFRESHED.get_or_init(|| {
        crate::command_env::refresh_from_default_shell();
    });
}

fn decode_enum<T: DeserializeOwned>(value: &str) -> anyhow::Result<T> {
    serde_json::from_value(Value::String(value.to_owned()))
        .with_context(|| format!("invalid protocol enum value {value:?}"))
}

pub fn encode_enum<T: Serialize>(value: T) -> anyhow::Result<String> {
    serde_json::to_value(value)?
        .as_str()
        .map(str::to_owned)
        .ok_or_else(|| anyhow!("protocol enum did not serialize as a string"))
}

fn event_to_wire(event: DriverEvent) -> anyhow::Result<WireDriverEvent> {
    let (kind, payload) = match event {
        DriverEvent::RuntimeEventCursorAdvanced(_) => {
            bail!("client-only runtime cursors cannot be sent by the daemon")
        }
        DriverEvent::Connected { provider_cursor } => {
            ("connected", serde_json::to_value(provider_cursor)?)
        }
        DriverEvent::AgentPresetSelected(preset) => {
            ("agentPresetSelected", serde_json::to_value(preset)?)
        }
        DriverEvent::ToolApprovalChanged(mode) => ("toolApprovalChanged", Value::String(mode)),
        DriverEvent::DeerFlowModelsChanged {
            models,
            model,
            reasoning_effort,
            thinking_mode,
        } => (
            "deerflowModelsChanged",
            json!({"models": models, "model": model, "reasoningEffort": reasoning_effort, "thinkingMode": thinking_mode}),
        ),
        DriverEvent::AutoTitleUpdated(title) => ("autoTitleUpdated", serde_json::to_value(title)?),
        DriverEvent::AvailableCommands(commands) => {
            ("availableCommands", serde_json::to_value(commands)?)
        }
        DriverEvent::TurnStarted => ("turnStarted", Value::Null),
        DriverEvent::TurnParked => ("turnParked", Value::Null),
        DriverEvent::TextDelta(text) => ("textDelta", Value::String(text)),
        DriverEvent::ReasoningDelta(text) => ("reasoningDelta", Value::String(text)),
        DriverEvent::Activity {
            id,
            kind,
            title,
            detail,
            complete,
        } => (
            "activity",
            json!({
                "id": id,
                "kind": kind,
                "title": title,
                "detail": detail,
                "complete": complete,
            }),
        ),
        DriverEvent::RichActivity(activity) => ("richActivity", serde_json::to_value(activity)?),
        DriverEvent::BackgroundWork(work) => ("backgroundWork", serde_json::to_value(work)?),
        DriverEvent::DeerFlowTasks(event) => ("deerflowTasks", serde_json::to_value(event)?),
        DriverEvent::Permission {
            request_id,
            title,
            detail,
            options,
        } => (
            "permission",
            json!({
                "requestId": request_id,
                "title": title,
                "detail": detail,
                "options": options,
            }),
        ),
        DriverEvent::PermissionResolved { request_id } => (
            "permissionResolved",
            json!({ "requestId": request_id }),
        ),
        DriverEvent::UserInputRequested {
            request_id,
            questions,
        } => (
            "userInputRequested",
            json!({
                "requestId": request_id,
                "questions": questions,
            }),
        ),
        DriverEvent::ComputerUseUpdated(state) => (
            "computerUseUpdated",
            serde_json::to_value(ComputerUseWire {
                target: state.target,
                phase: state.phase,
                visible: state.visible,
                image_url: state.image_url,
            })?,
        ),
        DriverEvent::PromptSubmitted {
            message,
            turn_id,
            message_id,
        } => (
            "promptSubmitted",
            json!({ "message": message, "turnId": turn_id, "messageId": message_id }),
        ),
        DriverEvent::SteeringCapability { supported, run_id } => (
            "steeringCapability",
            json!({ "supported": supported, "runId": run_id }),
        ),
        DriverEvent::ConversationCapability { supported } => {
            ("conversationCapability", json!({ "supported": supported }))
        }
        DriverEvent::SteeringUpdated(update) => ("steeringUpdated", serde_json::to_value(update)?),
        DriverEvent::SteerAccepted { message } => ("steerAccepted", json!({ "message": message })),
        DriverEvent::SteerRejected { message, reason } => (
            "steerRejected",
            json!({ "message": message, "reason": reason }),
        ),
        DriverEvent::UsageUpdated {
            context_tokens,
            context_window,
        } => (
            "usageUpdated",
            json!({
                "contextTokens": context_tokens,
                "contextWindow": context_window,
            }),
        ),
        DriverEvent::TurnFinished { success, summary } => (
            "turnFinished",
            json!({ "success": success, "summary": summary }),
        ),
        DriverEvent::Error(error) => ("error", Value::String(error)),
        DriverEvent::ProcessExited => ("processExited", Value::Null),
    };
    Ok(WireDriverEvent::new(kind, payload))
}

pub fn event_from_wire(event: WireDriverEvent) -> anyhow::Result<DriverEvent> {
    let payload = event.payload;
    Ok(match event.kind.as_str() {
        "connected" => DriverEvent::Connected {
            provider_cursor: serde_json::from_value(payload)?,
        },
        "agentPresetSelected" => DriverEvent::AgentPresetSelected(serde_json::from_value(payload)?),
        "toolApprovalChanged" => DriverEvent::ToolApprovalChanged(serde_json::from_value(payload)?),
        "deerflowModelsChanged" => DriverEvent::DeerFlowModelsChanged {
            models: serde_json::from_value(payload["models"].clone())?,
            model: serde_json::from_value(payload["model"].clone())?,
            reasoning_effort: serde_json::from_value(payload["reasoningEffort"].clone())?,
            thinking_mode: serde_json::from_value(payload["thinkingMode"].clone())?,
        },
        "autoTitleUpdated" => DriverEvent::AutoTitleUpdated(serde_json::from_value(payload)?),
        "availableCommands" => DriverEvent::AvailableCommands(serde_json::from_value(payload)?),
        "turnStarted" => DriverEvent::TurnStarted,
        "turnParked" => DriverEvent::TurnParked,
        "textDelta" => DriverEvent::TextDelta(serde_json::from_value(payload)?),
        "reasoningDelta" => DriverEvent::ReasoningDelta(serde_json::from_value(payload)?),
        "activity" => {
            let activity: ActivityWire = serde_json::from_value(payload)?;
            DriverEvent::Activity {
                id: activity.id,
                kind: activity.kind,
                title: activity.title,
                detail: activity.detail,
                complete: activity.complete,
            }
        }
        "richActivity" => DriverEvent::RichActivity(serde_json::from_value(payload)?),
        "backgroundWork" => DriverEvent::BackgroundWork(serde_json::from_value(payload)?),
        "deerflowTasks" => DriverEvent::DeerFlowTasks(serde_json::from_value(payload)?),
        "permission" => {
            let permission: PermissionWire = serde_json::from_value(payload)?;
            DriverEvent::Permission {
                request_id: permission.request_id,
                title: permission.title,
                detail: permission.detail,
                options: permission.options,
            }
        }
        "permissionResolved" => DriverEvent::PermissionResolved {
            request_id: serde_json::from_value(payload["requestId"].clone())?,
        },
        "userInputRequested" => {
            let request: UserInputWire = serde_json::from_value(payload)?;
            DriverEvent::UserInputRequested {
                request_id: request.request_id,
                questions: request.questions,
            }
        }
        "computerUseUpdated" => {
            let state: ComputerUseWire = serde_json::from_value(payload)?;
            DriverEvent::ComputerUseUpdated(ComputerUseState {
                target: state.target,
                phase: state.phase,
                visible: state.visible,
                image_url: state.image_url,
            })
        }
        "promptSubmitted" => {
            let submitted: SubmittedPromptWire = serde_json::from_value(payload)?;
            DriverEvent::PromptSubmitted {
                message: submitted.message,
                turn_id: submitted.turn_id,
                message_id: submitted.message_id,
            }
        }
        "steeringCapability" => DriverEvent::SteeringCapability {
            supported: payload["supported"].as_bool().unwrap_or(false),
            run_id: payload["runId"].as_str().map(str::to_owned),
        },
        "conversationCapability" => DriverEvent::ConversationCapability {
            supported: payload["supported"].as_bool().unwrap_or(false),
        },
        "steeringUpdated" => DriverEvent::SteeringUpdated(serde_json::from_value(payload)?),
        "steerAccepted" => {
            let steer: AcceptedSteerWire = serde_json::from_value(payload)?;
            DriverEvent::SteerAccepted {
                message: steer.message,
            }
        }
        "steerRejected" => {
            let steer: RejectedSteerWire = serde_json::from_value(payload)?;
            DriverEvent::SteerRejected {
                message: steer.message,
                reason: steer.reason,
            }
        }
        "usageUpdated" => {
            let usage: UsageWire = serde_json::from_value(payload)?;
            DriverEvent::UsageUpdated {
                context_tokens: usage.context_tokens,
                context_window: usage.context_window,
            }
        }
        "turnFinished" => {
            let finished: TurnFinishedWire = serde_json::from_value(payload)?;
            DriverEvent::TurnFinished {
                success: finished.success,
                summary: finished.summary,
            }
        }
        "error" => DriverEvent::Error(serde_json::from_value(payload)?),
        "processExited" => DriverEvent::ProcessExited,
        kind => bail!("daemon sent an unsupported driver event {kind:?}"),
    })
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct SubmittedPromptWire {
    message: String,
    turn_id: Uuid,
    message_id: Uuid,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct ActivityWire {
    id: Option<String>,
    kind: ActivityKind,
    title: String,
    detail: Option<String>,
    complete: bool,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct PermissionWire {
    request_id: String,
    title: String,
    detail: String,
    options: Vec<PermissionOption>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct UserInputWire {
    request_id: String,
    questions: Vec<crate::model::UserInputQuestion>,
}

#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct ComputerUseWire {
    target: Option<ComputerTarget>,
    phase: ComputerUsePhase,
    visible: bool,
    image_url: Option<String>,
}

#[derive(Deserialize)]
struct AcceptedSteerWire {
    message: String,
}

#[derive(Deserialize)]
struct RejectedSteerWire {
    message: String,
    reason: String,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct UsageWire {
    context_tokens: Option<u64>,
    context_window: Option<u64>,
}

#[derive(Deserialize)]
struct TurnFinishedWire {
    success: bool,
    summary: Option<String>,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn steering_target_reaches_desktop_over_the_daemon_wire() {
        for run in [Some("approved-run"), None] {
            let wire = event_to_wire(DriverEvent::SteeringCapability {
                supported: true,
                run_id: run.map(str::to_owned),
            })
            .unwrap();
            assert!(matches!(
                waku_protocol::event_from_wire(wire).unwrap(),
                DriverEvent::SteeringCapability { supported: true, run_id }
                    if run_id.as_deref() == run
            ));
        }
    }

    #[test]
    fn tool_approval_authority_survives_daemon_wire_round_trip() {
        for mode in ["ask", "allow_always", "reject_always", "off"] {
            let wire = event_to_wire(DriverEvent::ToolApprovalChanged(mode.into())).unwrap();
            assert!(
                matches!(event_from_wire(wire).unwrap(), DriverEvent::ToolApprovalChanged(actual) if actual == mode)
            );
        }
    }

    #[test]
    fn deerflow_ready_survives_normal_prompt_invalidation() {
        use waku_protocol::deerflow_tasks::{Capabilities, Event};

        let session = Uuid::new_v4();
        let runtime = Uuid::new_v4();
        let runtimes = Mutex::new(HashMap::from([(session, (runtime, ()))]));
        let cached = Mutex::new(HashMap::new());
        let ready = Event::Ready {
            connection_id: "connection".into(),
            session_id: "native-session".into(),
            capabilities: Capabilities {
                tree: true,
                artifact_list: true,
                artifact_read: true,
                ..Capabilities::default()
            },
        };
        cache_deerflow_ready(&runtimes, &cached, session, runtime, &ready);
        // The ACP prompt completion callback emits this event while its connection stays alive.
        cache_deerflow_ready(
            &runtimes,
            &cached,
            session,
            runtime,
            &Event::Invalidated {
                connection_id: "connection".into(),
                session_id: "native-session".into(),
            },
        );
        assert_eq!(runtimes.lock().get(&session).unwrap().0, runtime);
        let snapshot = cached.lock();
        let (cached_runtime, current_ready) = snapshot
            .get(&session)
            .expect("attachSession must still expose Ready after a normal prompt completes");
        assert_eq!(*cached_runtime, runtime);
        assert_eq!(
            serde_json::to_value(current_ready).unwrap(),
            serde_json::to_value(&ready).unwrap()
        );
    }

    #[test]
    fn deerflow_ready_ignores_replaced_runtime_events_and_clears_on_exit() {
        use waku_protocol::deerflow_tasks::{Capabilities, Event};

        let session = Uuid::new_v4();
        let stale = Uuid::new_v4();
        let current = Uuid::new_v4();
        let runtimes = Mutex::new(HashMap::from([(session, (current, ()))]));
        let cached = Mutex::new(HashMap::new());
        let ready = |connection: &str| Event::Ready {
            connection_id: connection.into(),
            session_id: "native-session".into(),
            capabilities: Capabilities::default(),
        };
        cache_deerflow_ready(&runtimes, &cached, session, current, &ready("current"));
        cache_deerflow_ready(&runtimes, &cached, session, stale, &ready("stale"));
        cache_deerflow_ready(
            &runtimes,
            &cached,
            session,
            stale,
            &Event::Invalidated {
                connection_id: "stale".into(),
                session_id: "native-session".into(),
            },
        );
        remove_finished_runtime(&cached, session, stale);
        assert_eq!(
            serde_json::to_value(&cached.lock().get(&session).unwrap().1).unwrap()["connectionId"],
            "current"
        );
        remove_finished_runtime(&cached, session, current);
        assert!(cached.lock().is_empty());
        runtimes.lock().remove(&session);
        cache_deerflow_ready(&runtimes, &cached, session, current, &ready("late"));
        assert!(cached.lock().is_empty());
    }

    #[test]
    fn deerflow_exited_runtime_cannot_remove_a_reconnected_session() {
        let session = Uuid::new_v4();
        let stale = Uuid::new_v4();
        let current = Uuid::new_v4();
        let runtimes = Mutex::new(HashMap::from([(session, (current, ()))]));
        remove_finished_runtime(&runtimes, session, stale);
        assert_eq!(runtimes.lock().get(&session).unwrap().0, current);
        remove_finished_runtime(&runtimes, session, current);
        assert!(runtimes.lock().is_empty());
    }

    #[test]
    fn stale_runtime_projection_keeps_newer_transcript_cursor() {
        let runtime_id = Uuid::new_v4();
        let epoch = Uuid::new_v4();
        let mut existing = AgentSession::new(Uuid::new_v4(), ProviderKind::Codex);
        existing.status = SessionStatus::Working;
        existing.runtime_event_cursor = Some(crate::model::RuntimeEventCursor {
            runtime_id,
            epoch,
            sequence: 10,
        });
        existing.push_message(crate::model::MessageRole::Assistant, "complete so far");

        let mut stale = existing.clone();
        stale.title = "Renamed elsewhere".into();
        stale.messages.clear();
        stale.runtime_event_cursor = Some(crate::model::RuntimeEventCursor {
            runtime_id,
            epoch,
            sequence: 7,
        });

        assert!(session_projection_precedes(
            &existing,
            &stale,
            Some(runtime_id)
        ));
        merge_stale_session_metadata(&mut existing, stale);
        assert_eq!(existing.title, "Renamed elsewhere");
        assert_eq!(existing.messages.len(), 1);
        assert_eq!(existing.runtime_event_cursor.unwrap().sequence, 10);
    }

    #[test]
    fn client_projection_cannot_replace_a_daemon_checkpoint() {
        let mut existing = AgentSession::new(Uuid::new_v4(), ProviderKind::Codex);
        existing.begin_turn("change it");
        existing.finish_active_turn(crate::model::TurnStatus::Completed);
        let checkpoint = Checkpoint {
            turn_count: 1,
            git_ref: "refs/waku/canonical".into(),
            status: CheckpointStatus::Ready,
            files: Vec::new(),
            additions: 0,
            deletions: 0,
            created_at: 1,
        };
        existing.turns[0].checkpoint = Some(checkpoint.clone());

        let mut incoming = existing.clone();
        incoming.turns[0].checkpoint = Some(Checkpoint {
            git_ref: "refs/waku/stale-client".into(),
            ..checkpoint.clone()
        });
        preserve_daemon_checkpoints(&existing, &mut incoming);

        assert_eq!(incoming.turns[0].checkpoint.as_ref(), Some(&checkpoint));
    }

    #[test]
    fn response_fork_titles_follow_one_numbered_sequence() {
        assert_eq!(
            next_response_fork_title("Fix the bug", ["Fix the bug"]),
            "Fix the bug (2)"
        );
        assert_eq!(
            next_response_fork_title(
                "Fix the bug (2)",
                ["Fix the bug", "Fix the bug (2)", "Fix the bug (4)"]
            ),
            "Fix the bug (5)"
        );
        assert_eq!(
            next_response_fork_title("Plan (2026)", ["Plan (2026)"]),
            "Plan (2026) (2)"
        );
    }

    #[test]
    fn message_rewind_requires_a_settled_user_turn_and_provider_cursor() {
        let mut session = AgentSession::new(Uuid::new_v4(), ProviderKind::Codex);
        session.begin_turn("change it");
        session.mark_active_turn_provider_started();
        session.provider_cursor = Some(ProviderResumeCursor::Codex {
            thread_id: "thread".into(),
        });
        session.finish_active_turn(crate::model::TurnStatus::Completed);

        assert!(validate_message_rewind(&session, 1).is_ok());

        let mut busy = session.clone();
        busy.status = SessionStatus::Working;
        assert!(validate_message_rewind(&busy, 1).is_err());

        let mut missing_cursor = session.clone();
        missing_cursor.provider_cursor = None;
        assert!(validate_message_rewind(&missing_cursor, 1).is_err());

        let mut missing_message = session;
        missing_message.messages.clear();
        assert!(validate_message_rewind(&missing_message, 1).is_err());
    }

    #[test]
    fn wire_event_round_trip_preserves_ordered_delta_payload() {
        let wire = event_to_wire(DriverEvent::TextDelta("hello".into())).unwrap();
        assert_eq!(wire.kind, "textDelta");
        assert!(matches!(
            event_from_wire(wire).unwrap(),
            DriverEvent::TextDelta(text) if text == "hello"
        ));
    }

    #[test]
    fn wire_event_round_trip_preserves_prompt_submission_identity() {
        let turn_id = Uuid::new_v4();
        let message_id = Uuid::new_v4();
        let wire = event_to_wire(DriverEvent::PromptSubmitted {
            message: "ship it".into(),
            turn_id,
            message_id,
        })
        .unwrap();
        assert_eq!(wire.kind, "promptSubmitted");
        assert_eq!(wire.payload["message"], "ship it");
        assert_eq!(wire.payload["turnId"], turn_id.to_string());
        assert_eq!(wire.payload["messageId"], message_id.to_string());
        assert!(matches!(
            event_from_wire(wire).unwrap(),
            DriverEvent::PromptSubmitted { message, turn_id: decoded_turn, message_id: decoded_message }
                if message == "ship it" && decoded_turn == turn_id && decoded_message == message_id
        ));
    }
}
