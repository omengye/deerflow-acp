//! Agent Client Protocol transport backed by the official Rust SDK.
//!
//! The SDK owns JSON-RPC framing, request IDs, response routing, cancellation,
//! unknown-method errors, stdio lifetime, and protocol type validation. Waku
//! only adapts typed ACP messages to its provider-neutral [`DriverEvent`]s.

use std::collections::HashMap;
use std::io::Read as _;
use std::path::Path;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::thread;
use std::time::{Duration, Instant};

use agent_client_protocol::schema::ProtocolVersion;
use agent_client_protocol::schema::v1::{
    CancelNotification, ClientCapabilities, ContentBlock, Implementation, InitializeRequest,
    InitializeResponse, LoadSessionRequest, McpServer, NewSessionRequest, PermissionOptionKind,
    PromptRequest, PromptResponse, RequestId, RequestPermissionOutcome, RequestPermissionRequest,
    RequestPermissionResponse, ResumeSessionRequest, SelectedPermissionOutcome, SessionConfigKind,
    SessionConfigOption, SessionConfigOptionCategory, SessionConfigOptionValue,
    SessionConfigSelectOptions, SessionId, SessionModeState, SessionNotification,
    SetSessionConfigOptionRequest, StopReason, TextContent,
};
use agent_client_protocol::{
    AcpAgent, AcpAgentConfig, Agent, Client, ConnectionTo, Handled, LineDirection, Responder,
    UntypedMessage,
};
use anyhow::{Context as _, anyhow};
use base64::Engine as _;
use parking_lot::Mutex;
use serde_json::{Value, json};

use super::activity;
mod deerflow_steering;
mod deerflow_tasks;
mod deerflow_user_input;
use crate::driver::{
    DriverControl, DriverEventSender, DriverEventSink, DriverStartOptions, SessionOptions,
};
use crate::model::{
    ActivityKind, DriverEvent, PermissionOption, ProviderKind, ProviderModel, ProviderResumeCursor,
    RuntimeMode, UserInputAnswer, UserInputOption, UserInputQuestion,
};

enum CommandMessage {
    RespondCredential {
        id: String,
        sealed: crate::model::SealedCredential,
        deadline: Instant,
        reply: crossbeam_channel::Sender<Result<(), String>>,
    },
    DeerFlowTasks(waku_protocol::deerflow_tasks::Command),
    Prompt(String, Vec<crate::model::MessageAttachment>),
    Steer(String, Option<String>),
    Cancel,
    DeleteSession(String, crossbeam_channel::Sender<Result<(), String>>),
    History {
        method: String,
        params: Value,
        deadline: Instant,
        reply: crossbeam_channel::Sender<Result<Value, String>>,
    },
    Respond {
        request_id: String,
        option_id: String,
        deadline: Instant,
        reply: crossbeam_channel::Sender<Result<(), String>>,
    },
    RespondUserInput {
        request_id: String,
        answers: Vec<UserInputAnswer>,
        receipt: Option<(Instant, crossbeam_channel::Sender<crate::model::LocalUserInputResult>)>,
    },
    Options(SessionOptions),
    SetToolApproval {
        mode: String,
        deadline: Instant,
        reply: crossbeam_channel::Sender<Result<String, String>>,
    },
    Shutdown,
}

const TOOL_APPROVAL_TIMEOUT: Duration = Duration::from_secs(10);

struct AcpAccessState {
    runtime_mode: RuntimeMode,
    tool_approval: Option<String>,
    ready: bool,
    pending: bool,
}

impl AcpAccessState {
    fn new(mode: RuntimeMode) -> Self {
        Self {
            runtime_mode: mode,
            tool_approval: None,
            ready: false,
            pending: false,
        }
    }

    fn confirm(&mut self, mode: &str) {
        self.runtime_mode = match mode {
            "allow_always" | "off" => RuntimeMode::FullAccess,
            _ => RuntimeMode::Ask,
        };
        self.tool_approval = Some(mode.to_owned());
        self.ready = true;
        self.pending = false;
    }
}

fn close_command_loop(commands: &smol::channel::Receiver<CommandMessage>) {
    commands.close();
}

pub struct AcpDriver {
    commands: smol::channel::Sender<CommandMessage>,
    events: DriverEventSender,
    steering_availability: Arc<Mutex<deerflow_steering::Availability>>,
    provider: ProviderKind,
    access: Arc<Mutex<AcpAccessState>>,
}

/// Per-provider launch details. Everything after process launch is ACP.
struct AcpLaunch {
    args: Vec<String>,
    env: Vec<(String, String)>,
}

fn deerflow_launch() -> anyhow::Result<AcpLaunch> {
    Ok(AcpLaunch {
        args: crate::deerflow_config::launch_arguments()?,
        env: crate::deerflow_config::launch_environment()?,
    })
}

impl AcpDriver {
    fn enqueue_prompt(&self, prompt: String, attachments: Vec<crate::model::MessageAttachment>) {
        if self
            .commands
            .try_send(CommandMessage::Prompt(prompt, attachments))
            .is_err()
        {
            // A settings restart can close stdio while a UI submission is
            // preparing. Do not silently accept a prompt on a dead channel;
            // settle this turn and let the next explicit submission reconnect
            // using the persisted provider cursor.
            let _ = self.events.send(DriverEvent::Error(
                "The ACP connection closed before this message was sent. Retry the message to reconnect to this conversation.".into(),
            ));
            let _ = self.events.send(DriverEvent::TurnFinished {
                success: false,
                summary: None,
            });
            let _ = self.events.send(DriverEvent::ProcessExited);
        }
    }

    pub fn start(
        provider: ProviderKind,
        options: DriverStartOptions,
        events: DriverEventSender,
    ) -> anyhow::Result<Self> {
        let DriverStartOptions {
            binary,
            cwd,
            mode,
            model,
            reasoning_effort,
            thinking_mode,
            provider_cursor,
            ..
        } = options;
        let resume_session_id = match provider_cursor {
            Some(cursor) if cursor.provider() == provider => {
                let id = cursor.native_id();
                (!id.is_empty()).then(|| id.to_owned())
            }
            Some(cursor) => {
                return Err(anyhow!(
                    "cannot resume {} from a {} cursor",
                    provider.display_name(),
                    cursor.provider().display_name()
                ));
            }
            None => None,
        };

        let launch = deerflow_launch()?;
        let client_mcp_servers = crate::deerflow_config::client_mcp_servers();
        let stderr_lines = Arc::new(Mutex::new(Vec::new()));
        let agent = sdk_agent(&binary, &cwd, launch, stderr_lines.clone())?;
        let (commands, command_rx) = smol::channel::unbounded();
        let access = Arc::new(Mutex::new(AcpAccessState::new(mode)));
        let connection_access = access.clone();
        let steering_availability = Arc::new(Mutex::new(deerflow_steering::Availability::default()));
        let connection_steer = steering_availability.clone();
        let provider_name = provider.display_name();
        let thread_events = events.clone();

        thread::Builder::new()
            .name(format!("waku-{}-acp", provider.id()))
            .spawn(move || {
                if let Err(error) = crate::command_env::unblock_sigchld_for_current_thread() {
                    let _ = thread_events.send(DriverEvent::Error(format!(
                        "{provider_name}: failed to normalize the provider signal mask: {error}"
                    )));
                    let _ = thread_events.send(DriverEvent::ProcessExited);
                    return;
                }
                let result = smol::block_on(run_sdk_connection(
                    agent,
                    provider,
                    cwd,
                    mode,
                    connection_access,
                    connection_steer,
                    model,
                    reasoning_effort,
                    thinking_mode,
                    resume_session_id,
                    client_mcp_servers,
                    command_rx,
                    thread_events.clone(),
                ));
                if let Err(error) = result {
                    let stderr = super::support::provider_stderr_error(stderr_lines.lock().clone());
                    let detail = match stderr {
                        Some(stderr) => format!("{error}\n{stderr}"),
                        None => error.to_string(),
                    };
                    let _ = thread_events
                        .send(DriverEvent::Error(format!("{provider_name}: {detail}")));
                }
                let _ = thread_events.send(DriverEvent::ProcessExited);
            })
            .with_context(|| format!("failed to start {provider_name} ACP runtime"))?;

        Ok(Self {
            commands,
            events,
            steering_availability,
            provider,
            access,
        })
    }
}

fn sdk_agent(
    binary: &Path,
    _cwd: &Path,
    mut launch: AcpLaunch,
    stderr_lines: Arc<Mutex<Vec<String>>>,
) -> anyhow::Result<AcpAgent> {
    let binary = binary
        .to_str()
        .ok_or_else(|| anyhow!("the ACP executable path is not valid UTF-8"))?;
    #[cfg(unix)]
    let cwd = _cwd
        .to_str()
        .ok_or_else(|| anyhow!("the ACP working directory is not valid UTF-8"))?;
    let mut environment = crate::command_env::shell_environment()
        .into_iter()
        .map(|(name, value)| {
            (
                name.to_string_lossy().into_owned(),
                value.to_string_lossy().into_owned(),
            )
        })
        .collect::<Vec<_>>();
    environment.append(&mut launch.env);


    // `AcpAgentConfig` deliberately contains only argv and environment. macOS
    // and Linux `env -C` supplies the session cwd without a shell, preserving exact
    // argument boundaries and the SDK's process-group lifecycle management.
    #[cfg(unix)]
    let (program, args) = {
        let mut args = vec!["-C".to_owned(), cwd.to_owned(), binary.to_owned()];
        args.extend(launch.args);
        ("/usr/bin/env".to_owned(), args)
    };
    #[cfg(windows)]
    let (program, args) = (binary.to_owned(), launch.args);

    let config = AcpAgentConfig::new(program).args(args).envs(environment);
    Ok(AcpAgent::new(config).with_debug(move |line, direction| {
        if direction != LineDirection::Stderr || line.trim().is_empty() {
            return;
        }
        let mut lines = stderr_lines.lock();
        if lines.len() == 128 {
            lines.remove(0);
        }
        lines.push(line.to_owned());
    }))
}

/// Builds a short-lived ACP process for session discovery or history replay.
///
/// Catalog work runs on the daemon request thread, never a render path. It
/// intentionally shares the production launch contract so provider argv and
/// environment quirks cannot drift between a resumed task and the picker that
/// discovered it. Background model probes must not restart a stopped DeerFlow
/// service; explicit session discovery and replay may start it.
pub(crate) fn catalog_agent(
    provider: ProviderKind,
    binary: &Path,
    cwd: &Path,
    auto_start: bool,
) -> anyhow::Result<AcpAgent> {
    anyhow::ensure!(provider.is_available(), "Archived sessions are read-only");
    let mut launch = deerflow_launch()?;
    if !auto_start {
        launch.args.push("--no-auto-start".into());
    }
    sdk_agent(binary, cwd, launch, Arc::new(Mutex::new(Vec::new())))
}

type PermissionResponder = Responder<RequestPermissionResponse>;
struct PendingAcpPermission {
    responder: PermissionResponder,
    public_key: Option<String>,
}
impl PendingAcpPermission {
    fn respond(self, response: RequestPermissionResponse) -> agent_client_protocol::Result<()> {
        self.responder.respond(response)
    }
}
type PendingPermissions = Arc<Mutex<HashMap<String, PendingAcpPermission>>>;

/// An unsealed allow must leave an enrollment pending for another capable client.
fn complete_permission(pending: &PendingPermissions, id: &str, option: &str) -> Result<(), String> {
    let responder = {
        let mut entries = pending.lock();
        let entry = entries.get(id).ok_or("Permission is no longer pending")?;
        if entry.public_key.is_some() && option != "reject_once" {
            return Err("This permission requires the encrypted credential form".into());
        }
        entries.remove(id).expect("entry checked under the same lock")
    };
    let response = RequestPermissionResponse::new(RequestPermissionOutcome::Selected(
        SelectedPermissionOutcome::new(option.to_owned()),
    ));
    responder.respond(response).map_err(|_| "Permission is no longer active".into())
}

// DER SubjectPublicKeyInfo header for an uncompressed P-256 public key, including
// the SEC1 uncompressed-point byte. The Go vault validates the point and the GCM tag.
const P256_PUBLIC_HEADER: &[u8] = &[
    0x30, 0x59, 0x30, 0x13, 0x06, 0x07, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01, 0x06,
    0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07, 0x03, 0x42, 0x00, 0x04,
];

fn credential_public_key(encoded: &str) -> bool {
    encoded.len() == 124 && base64::engine::general_purpose::STANDARD.decode(encoded)
        .is_ok_and(|der| der.len() == 91 && der.starts_with(P256_PUBLIC_HEADER))
}

fn complete_credential(pending: &PendingPermissions, id: &str, sealed: crate::model::SealedCredential) -> Result<(), String> {
    let base64 = base64::engine::general_purpose::STANDARD;
    if !credential_public_key(&sealed.public_key) || !credential_public_key(&sealed.client_key)
        || sealed.nonce.len() != 16 || !base64.decode(&sealed.nonce).is_ok_and(|n| n.len() == 12)
        || !(28..=174792).contains(&sealed.ciphertext.len())
        || !base64.decode(&sealed.ciphertext).is_ok_and(|c| (21..=131092).contains(&c.len()))
    {
        return Err("Invalid credential envelope".into());
    }
    let responder = {
        let mut entries = pending.lock();
        if entries.get(id).and_then(|entry| entry.public_key.as_deref()) != Some(sealed.public_key.as_str()) {
            return Err("Credential approval has expired".into());
        }
        entries.remove(id).expect("entry checked under the same lock")
    };
    let response = RequestPermissionResponse::new(RequestPermissionOutcome::Selected(SelectedPermissionOutcome::new("allow_once")))
        .meta(serde_json::Map::from_iter([("sealedCredential".into(), json!(sealed))]));
    responder.respond(response).map_err(|_| "Credential approval is no longer active".into())
}

struct PendingAcpUserInput {
    params: Value,
    responder: Responder<Value>,
}

type PendingAcpUserInputs = Arc<Mutex<HashMap<String, PendingAcpUserInput>>>;

#[derive(Default)]
struct PendingPrompts {
    requests: Vec<PendingPrompt>,
    user_inputs: Option<PendingAcpUserInputs>,
    user_input_stopped: bool,
}

struct PendingPrompt {
    request_id: RequestId,
    session_id: String,
}

impl PendingPrompts {
    fn insert(&mut self, request_id: RequestId, session_id: String) {
        if self.requests.is_empty() {
            self.user_input_stopped = false;
        }
        self.requests.push(PendingPrompt {
            request_id,
            session_id,
        });
    }

    fn is_empty(&self) -> bool {
        self.requests.is_empty()
    }

    fn accepts_user_input(&self, session_id: &str) -> bool {
        !self.user_input_stopped
            && self
                .requests
                .iter()
                .any(|prompt| prompt.session_id == session_id)
    }

    fn settle_request(&mut self, request_id: &RequestId) -> bool {
        let Some(index) = self
            .requests
            .iter()
            .position(|prompt| &prompt.request_id == request_id)
        else {
            return false;
        };
        self.requests.remove(index);
        self.finish_if_idle()
    }

    fn finish_if_idle(&self) -> bool {
        if !self.requests.is_empty() {
            return false;
        }
        if let Some(pending) = &self.user_inputs {
            cancel_pending_user_inputs(pending);
        }
        true
    }
}

type PendingPromptRequests = Arc<Mutex<PendingPrompts>>;

#[allow(clippy::too_many_arguments)]
async fn run_sdk_connection(
    agent: AcpAgent,
    provider: ProviderKind,
    cwd: std::path::PathBuf,
    mode: RuntimeMode,
    access: Arc<Mutex<AcpAccessState>>,
    steering_availability: Arc<Mutex<deerflow_steering::Availability>>,
    model: Option<String>,
    reasoning_effort: Option<String>,
    thinking_mode: Option<String>,
    resume_session_id: Option<String>,
    client_mcp_config: (Vec<McpServer>, String),
    commands: smol::channel::Receiver<CommandMessage>,
    events: DriverEventSender,
) -> agent_client_protocol::Result<()> {
    let suppress_session_updates = Arc::new(AtomicBool::new(false));
    let stream_state = Arc::new(Mutex::new(AcpStreamState {
        steering_availability,
        image_store: Some(Arc::new(crate::blob_store::BlobStore::new(
            waku_protocol::identity::desktop_data_directory().join("blobs"),
        ))),
        ..AcpStreamState::default()
    }));
    let pending_permissions: PendingPermissions = Arc::new(Mutex::new(HashMap::new()));
    let pending_user_inputs: PendingAcpUserInputs = Arc::new(Mutex::new(HashMap::new()));
    let prompt_requests = Arc::new(Mutex::new(PendingPrompts {
        user_inputs: Some(pending_user_inputs.clone()),
        ..PendingPrompts::default()
    }));
    let close_commands = commands.clone();

    Client
        .builder()
        .name("waku")
        .on_close(async move |_connection| {
            // `connect_with` does not cancel its foreground future on incoming
            // EOF. Close the receiver so an idle command loop wakes and lets
            // the outer driver thread publish `ProcessExited`.
            close_command_loop(&close_commands);
            Ok(())
        })
        .on_receive_notification(
            {
                let events = events.clone();
                let suppress_session_updates = suppress_session_updates.clone();
                let stream_state = stream_state.clone();
                let prompt_requests = prompt_requests.clone();
                async move |notification: SessionNotification, _connection| {
                    if !suppress_session_updates.load(Ordering::Acquire) {
                        let mut stream = stream_state.lock();
                        deerflow_tasks::receive_update(&mut stream, deerflow_tasks::EarlyUpdate::Session(notification), !prompt_requests.lock().is_empty(), &events)?;
                    }
                    Ok(())
                }
            },
            agent_client_protocol::on_receive_notification!(),
        )
        .on_receive_notification(
            {
                let events = events.clone();
                let prompt_requests = prompt_requests.clone();
                let stream_state = stream_state.clone();
                async move |notification: UntypedMessage, _connection| {
                    if notification.method() == "_deerflow/executions/updated" {
                        let mut stream = stream_state.lock();
                        deerflow_tasks::receive_update(&mut stream,deerflow_tasks::EarlyUpdate::Lifecycle(notification.params().clone()),!prompt_requests.lock().is_empty(),&events)?;
                    }
                    if notification.method() == "_deerflow/steering/target" {
                        deerflow_steering::receive_target(&mut stream_state.lock(), notification.params(), &events);
                    }
                    if notification.method() == "_deerflow/steering/updated" {
                        if let Some(state) = &mut stream_state.lock().deerflow_steering {
                            state.receive(notification.params(), &events);
                        }
                    }
                    Ok(())
                }
            },
            agent_client_protocol::on_receive_notification!(),
        )
        .on_receive_request(
            {
                let events = events.clone();
                let pending_permissions = pending_permissions.clone();
                let stream_state = stream_state.clone();
                async move |request: RequestPermissionRequest, responder, _connection| {
                    deerflow_tasks::receive_permission(&mut stream_state.lock(), request, responder, mode, &pending_permissions, &events)
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let events = events.clone();
                let pending = pending_user_inputs.clone();
                let prompt_requests = prompt_requests.clone();
                async move |request: UntypedMessage, responder, _connection| {
                    if request.method() != deerflow_user_input::METHOD {
                        return Ok(Handled::No { message: (request, responder), retry: false });
                    }
                    deerflow_user_input::receive(request.params().clone(), responder, &prompt_requests, &pending, &events)
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .connect_with(agent, async move |connection: ConnectionTo<Agent>| {
            let client_capabilities = ClientCapabilities::new().terminal(false).meta(deerflow_user_input::capability());
            let initialize = connection
                .send_request(
                    InitializeRequest::new(ProtocolVersion::V1)
                        .client_capabilities(client_capabilities)
                        .client_info(Implementation::new("waku", env!("CARGO_PKG_VERSION"))),
                )
                .block_task()
                .await?;
            let ((session_id, _modes, config_options), mut mcp_revision) =
                apply_session_mcp(client_mcp_config, async |servers| {
                    establish_session(&connection, &initialize, resume_session_id.as_deref(),
                        &cwd, &servers, &suppress_session_updates).await
                }).await?;
            let approval_config_id = deerflow_approval_option(config_options.as_deref())
                .map(|option| option.id.to_string())
                .unwrap_or_else(|| "tool_approval".to_owned());

            let approval = if provider == ProviderKind::DeerFlow {
                Some(initialize_deerflow_approval(
                    &connection,
                    &session_id,
                    config_options.as_deref(),
                    resume_session_id.is_some(),
                    mode,
                ).await?)
            } else {
                None
            };

            let native_session_id = session_id.to_string();
            if provider == ProviderKind::DeerFlow {
                let supported = deerflow_steering::supported(&initialize);
                {
                    let mut stream = stream_state.lock();
                    stream.steering_availability.lock().supported = supported;
                    stream.deerflow_steering = supported.then(|| deerflow_steering::State::new(&native_session_id));
                }
                let _ = events.send(DriverEvent::SteeringCapability { supported, run_id: None });
                if supported { deerflow_steering::restore(&connection, &native_session_id, stream_state.clone(), events.clone()); }
            }
            let history_supported = provider == ProviderKind::DeerFlow
                && deerflow_history_capability(&serde_json::to_value(&initialize)?);
            if provider == ProviderKind::DeerFlow {
                let _ = events.send(DriverEvent::ConversationCapability { supported: history_supported });
            }
            let _ = events.send(DriverEvent::Connected {
                provider_cursor: Some(ProviderResumeCursor::from_session_id(
                    provider,
                    native_session_id.clone(),
                )),
            });
            let task_connection = (provider == ProviderKind::DeerFlow).then(|| deerflow_tasks::TaskConnection::new(&initialize, &native_session_id, &events));
            stream_state.lock().deerflow_tasks = task_connection.as_ref().map(deerflow_tasks::TaskConnection::notifications);
            if provider == ProviderKind::DeerFlow {
                deerflow_tasks::flush_early_updates(&mut stream_state.lock(), !prompt_requests.lock().is_empty(), &events)?;
            }
            if let Some(approval) = approval {
                access.lock().confirm(&approval);
                let _ = events.send(DriverEvent::ToolApprovalChanged(approval));
            }

            stream_state.lock().deerflow_config_options = config_options.clone();
            let mut model_options_ready = apply_model(
                &connection,
                &session_id,
                config_options.as_deref(),
                model.as_deref(),
                reasoning_effort.as_deref(),
                thinking_mode.as_deref(),
                &events,
                &stream_state,
            )
            .await;

            while let Ok(command) = commands.recv().await {
                match command {
                    CommandMessage::DeerFlowTasks(command) => {
                        if let Some(tasks) = &task_connection {
                            if matches!(command.action, waku_protocol::deerflow_tasks::Action::ExecutionResume { .. }) {
                                if !access.lock().ready {
                                    deerflow_tasks::failed(&events, command, "approval_not_ready", "工具审批策略尚未确认，请等待设置同步后恢复。");
                                } else {
                                    tasks.resume_execution(&connection, command, &prompt_requests, &events, &stream_state);
                                }
                            } else {
                                tasks.dispatch(&connection, command, &events);
                            }
                        }
                    }
                    CommandMessage::Prompt(text, attachments) => {
                        if !access.lock().ready {
                            let _ = events.send(DriverEvent::TurnStarted);
                            let _ = events.send(DriverEvent::Error(
                                "DeerFlow tool approval policy has not been confirmed. Wait for the pending change or retry the setting before sending a message.".into(),
                            ));
                            let _ = events.send(DriverEvent::TurnFinished { success: false, summary: None });
                            continue;
                        }
                        if !model_options_ready {
                            let _ = events.send(DriverEvent::TurnStarted);
                            let _ = events.send(DriverEvent::Error(
                                "DeerFlow could not apply the selected model or thinking setting. Select valid options and retry before sending this message.".into(),
                            ));
                            let _ = events.send(DriverEvent::TurnFinished {
                                success: false,
                                summary: None,
                            });
                            continue;
                        }
                        let _ = events.send(DriverEvent::TurnStarted);
                        if let Err(error) = refresh_session_mcp(&connection, &session_id, &cwd, &mut mcp_revision).await {
                            let _ = events.send(DriverEvent::Error(format!("当前会话应用 MCP 配置失败：{error}")));
                            let _ = events.send(DriverEvent::TurnFinished { success: false, summary: None });
                            continue;
                        }
                        if let Err(error) = send_prompt(
                            &connection,
                            &session_id,
                            text,
                            &attachments,
                            &prompt_requests,
                            &events,
                            &native_session_id,
                            stream_state.clone(),
                        ) {
                            let _ = events.send(DriverEvent::Error(error.to_string()));
                            let _ = events.send(DriverEvent::TurnFinished {
                                success: false,
                                summary: None,
                            });
                        }
                    }
                    CommandMessage::Steer(text, run_id) => {
                        deerflow_steering::submit(&connection, &native_session_id, text, run_id, stream_state.clone(), &events);
                    }
                    CommandMessage::Cancel => {
                        prompt_requests.lock().user_input_stopped = true;
                        let _ = connection
                            .send_notification(CancelNotification::new(session_id.clone()));
                        cancel_pending_permissions(&pending_permissions, &events);
                        stream_state.lock().deerflow_early.items.retain(|update| !matches!(update, deerflow_tasks::EarlyUpdate::Driver(DriverEvent::Permission { .. })));
                        cancel_pending_user_inputs(&pending_user_inputs);
                    }
                    CommandMessage::RespondCredential { id, sealed, deadline, reply } => {
                        let result = if Instant::now() < deadline { complete_credential(&pending_permissions, &id, sealed) }
                            else { Err("Credential approval timed out".into()) };
                        if result.is_ok() {
                            let _ = events.send(DriverEvent::PermissionResolved { request_id: id });
                        }
                        let _ = reply.send(result);
                    }
                    CommandMessage::Respond { request_id, option_id, deadline, reply } => {
                        let result = if Instant::now() < deadline {
                            complete_permission(&pending_permissions, &request_id, &option_id)
                        } else {
                            Err("Permission reply timed out".into())
                        };
                        if result.is_ok() {
                            let _ = events.send(DriverEvent::PermissionResolved { request_id });
                        }
                        let _ = reply.send(result);
                    }
                    CommandMessage::RespondUserInput {
                        request_id,
                        answers,
                        receipt,
                    } => {
                        let result = if receipt.as_ref().is_some_and(|(deadline, _)| Instant::now() >= *deadline) {
                            crate::model::LocalUserInputResult::NotSent
                        } else {
                            deerflow_user_input::complete(&pending_user_inputs, &request_id, &answers)
                        };
                        if let Some((_, reply)) = receipt {
                            let _ = reply.send(result);
                        }
                    }
                    CommandMessage::SetToolApproval { mode, deadline, reply } => {
                        if Instant::now() >= deadline {
                            let _ = reply.send(Err("Tool approval change expired before it could be sent".into()));
                            continue;
                        }
                        if !prompt_requests.lock().is_empty() {
                            let _ = reply.send(Err("Tool approvals can only change while the DeerFlow session is idle".into()));
                            continue;
                        }
                        if let Err(error) = request_deerflow_approval(
                            &connection, &session_id, &approval_config_id, mode, &access, &events, reply.clone(),
                        ) {
                            let _ = reply.send(Err(error.to_string()));
                        }
                    }
                    CommandMessage::Options(options) => {
                        if !prompt_requests.lock().is_empty() {
                            let _ = events.send(DriverEvent::Error("Wait for the current turn before changing model settings.".into()));
                            if let Some(options) = &stream_state.lock().deerflow_config_options {
                                report_deerflow_models(options, &events);
                            }
                            continue;
                        }
                        model_options_ready = apply_model(
                            &connection,
                            &session_id,
                            config_options.as_deref(),
                            options.model.as_deref(),
                            options.reasoning_effort.as_deref(),
                            options.thinking_mode.as_deref(),
                            &events,
                            &stream_state,
                        ).await;
                    }
                    CommandMessage::History { method, params, deadline, reply } => {
                        if !history_supported
                            || !matches!(method.as_str(), "_deerflow/history/turns" | "_deerflow/history/fork" | "_deerflow/history/rewind")
                            || params["sessionId"] != native_session_id
                            || Instant::now() >= deadline
                            || !prompt_requests.lock().is_empty()
                            || !pending_permissions.lock().is_empty()
                            || !pending_user_inputs.lock().is_empty()
                        {
                            let _ = reply.send(Err("rejected: Native history capability, identity or idle state is unavailable".into()));
                            continue;
                        }
                        let request = UntypedMessage::new(&method, params);
                        match request {
                            Ok(request) => {
                                let fallback = reply.clone();
                                if let Err(error) = connection.send_request(request).on_receiving_result(async move |result| {
                                    let _ = reply.send(result.map_err(|error| {
                                        let code: i32 = error.code.into();
                                        let outcome = if matches!(code, -32600 | -32601 | -32602 | -32000 | -32020 | -32021 | -32022) { "rejected" } else { "unknown" };
                                        format!("{outcome}: {error}")
                                    }));
                                    Ok(())
                                }) {
                                    let _ = fallback.send(Err(format!("unknown: {error}")));
                                }
                            }
                            Err(error) => { let _ = reply.send(Err(format!("rejected: {error}"))); }
                        }
                    }
                    CommandMessage::DeleteSession(expected, reply) => {
                        if expected != native_session_id {
                            let _ = reply.send(Err("rejected: Native session identity mismatch".into()));
                            continue;
                        }
                        let request = UntypedMessage::new("session/delete", json!({"sessionId": native_session_id}));
                        match request {
                            Ok(request) => {
                                let fallback = reply.clone();
                                if let Err(error) = connection.send_request(request).on_receiving_result(async move |result| {
                                    let _ = reply.send(result.map(|_| ()).map_err(|error| {
                                        let code: i32 = error.code.into();
                                        let outcome = if matches!(code, -32600 | -32601 | -32602 | -32000 | -32010 | -32011) { "rejected" } else { "unknown" };
                                        format!("{outcome}: {error}")
                                    }));
                                    Ok(())
                                }) {
                                    let _ = fallback.send(Err(error.to_string()));
                                }
                            }
                            Err(error) => { let _ = reply.send(Err(error.to_string())); }
                        }
                    }
                    CommandMessage::Shutdown => break,
                }
            }
            cancel_pending_permissions(&pending_permissions, &events);
            cancel_pending_user_inputs(&pending_user_inputs);
            Ok(())
        })
        .await
}

fn deerflow_history_capability(initialize: &Value) -> bool {
    let history = &initialize["_meta"]["deerflow"]["conversation"];
    history["version"] == 1
        && history["turnsMethod"] == "_deerflow/history/turns"
        && history["forkMethod"] == "_deerflow/history/fork"
        && history["rewindMethod"] == "_deerflow/history/rewind"
}

fn approval_error(message: impl Into<String>) -> agent_client_protocol::Error {
    agent_client_protocol::Error::new(
        agent_client_protocol::ErrorCode::InvalidParams.into(),
        message,
    )
}

fn deerflow_approval_option(
    options: Option<&[SessionConfigOption]>,
) -> Option<&SessionConfigOption> {
    options
        .unwrap_or_default()
        .iter()
        .find(|option| matches!(option.id.to_string().as_str(), "tool_approval" | "approval"))
}

fn deerflow_approval(
    options: Option<&[SessionConfigOption]>,
) -> agent_client_protocol::Result<String> {
    let Some(option) = deerflow_approval_option(options) else {
        return Ok("off".into());
    };
    match session_config_current_value(option) {
        Some(mode @ ("global" | "ask" | "allow_always" | "reject_always" | "read_only")) => {
            Ok(mode.to_owned())
        }
        _ => Err(approval_error(
            "DeerFlow returned an invalid tool approval policy",
        )),
    }
}

fn confirmed_deerflow_approval(
    options: &[SessionConfigOption],
    desired: &str,
) -> agent_client_protocol::Result<String> {
    let actual = deerflow_approval(Some(options))?;
    if actual != desired {
        return Err(approval_error(format!(
            "DeerFlow did not confirm the requested tool approval policy ({desired}); returned {actual}",
        )));
    }
    Ok(actual)
}

async fn initialize_deerflow_approval(
    connection: &ConnectionTo<Agent>,
    session_id: &SessionId,
    options: Option<&[SessionConfigOption]>,
    restoring: bool,
    mode: RuntimeMode,
) -> agent_client_protocol::Result<String> {
    let actual = deerflow_approval(options)?;
    if restoring {
        return Ok(actual);
    }
    let desired = if mode == RuntimeMode::FullAccess {
        "allow_always"
    } else {
        "ask"
    };
    if actual == "off" {
        return if desired == "allow_always" {
            Ok(actual)
        } else {
            Err(approval_error(
                "DeerFlow deployment policy disables tool approvals. Enable approvals in DeerFlow settings before choosing Ask.",
            ))
        };
    }
    let config_id = deerflow_approval_option(options)
        .expect("a configured approval policy has an option")
        .id
        .to_string();
    smol::future::or(
        async {
            let response = connection
                .send_request(SetSessionConfigOptionRequest::new(
                    session_id.clone(),
                    config_id.clone(),
                    desired,
                ))
                .block_task()
                .await?;
            confirmed_deerflow_approval(&response.config_options, desired)
        },
        async {
            smol::Timer::after(TOOL_APPROVAL_TIMEOUT).await;
            Err(approval_error(
                "Timed out initializing DeerFlow tool approvals; no prompt was sent",
            ))
        },
    )
    .await
}

/// Keep the callback alive after the caller's deadline. A late server reply
/// still updates the authoritative state and every attached client; until it
/// arrives, prompts and additional permission changes remain blocked.
fn request_deerflow_approval(
    connection: &ConnectionTo<Agent>,
    session_id: &SessionId,
    config_id: &str,
    desired: String,
    access: &Arc<Mutex<AcpAccessState>>,
    events: &DriverEventSender,
    reply: crossbeam_channel::Sender<Result<String, String>>,
) -> agent_client_protocol::Result<()> {
    if !matches!(desired.as_str(), "ask" | "allow_always") {
        return Err(approval_error(
            "Tool approval mode must be ask or allow_always",
        ));
    }
    {
        let mut state = access.lock();
        if state.pending {
            return Err(approval_error("A tool approval change is still pending"));
        }
        if state.tool_approval.as_deref() == Some("off") {
            return Err(approval_error(
                "DeerFlow deployment policy disables tool approvals. Enable approvals in DeerFlow settings first.",
            ));
        }
        state.pending = true;
        state.ready = false;
    }
    let callback_access = access.clone();
    let callback_events = events.clone();
    let sent = connection
        .send_request(SetSessionConfigOptionRequest::new(
            session_id.clone(),
            config_id.to_owned(),
            desired.as_str(),
        ))
        .on_receiving_result(async move |result| {
            let result = result.and_then(|response| {
                confirmed_deerflow_approval(&response.config_options, &desired)
            });
            {
                let mut state = callback_access.lock();
                state.pending = false;
                match &result {
                    Ok(actual) => state.confirm(actual),
                    Err(_) => state.ready = false,
                }
            }
            match &result {
                Ok(actual) => {
                    let _ = callback_events.send(DriverEvent::ToolApprovalChanged(actual.clone()));
                }
                Err(error) => {
                    let _ = callback_events.send(DriverEvent::Error(format!(
                        "DeerFlow tool approval change failed: {error}"
                    )));
                }
            }
            let _ = reply.send(result.map_err(|error| error.to_string()));
            Ok(())
        });
    if sent.is_err() {
        access.lock().pending = false;
    }
    sent
}

async fn establish_session(
    connection: &ConnectionTo<Agent>,
    initialize: &InitializeResponse,
    resume_session_id: Option<&str>,
    cwd: &Path,
    client_mcp_servers: &[McpServer],
    suppress_session_updates: &AtomicBool,
) -> agent_client_protocol::Result<(
    SessionId,
    Option<SessionModeState>,
    Option<Vec<SessionConfigOption>>,
)> {
    if let Some(existing) = resume_session_id {
        let mut failures = Vec::new();
        if initialize
            .agent_capabilities
            .session_capabilities
            .resume
            .is_some()
        {
            match connection
                .send_request(
                    ResumeSessionRequest::new(existing.to_owned(), cwd)
                        .mcp_servers(client_mcp_servers.to_vec()),
                )
                .block_task()
                .await
            {
                Ok(response) => {
                    return Ok((
                        SessionId::new(existing.to_owned()),
                        response.modes,
                        response.config_options,
                    ));
                }
                Err(error) if mcp_launch_denied(&error) => return Err(error),
                Err(error) => failures.push(format!("session/resume: {error}")),
            }
        }

        if initialize.agent_capabilities.load_session {
            suppress_session_updates.store(true, Ordering::Release);
            let response = connection
                .send_request(
                    LoadSessionRequest::new(existing.to_owned(), cwd)
                        .mcp_servers(client_mcp_servers.to_vec()),
                )
                .block_task()
                .await;
            suppress_session_updates.store(false, Ordering::Release);
            match response {
                Ok(response) => {
                    return Ok((
                        SessionId::new(existing.to_owned()),
                        response.modes,
                        response.config_options,
                    ));
                }
                Err(error) if mcp_launch_denied(&error) => return Err(error),
                Err(error) => failures.push(format!("session/load: {error}")),
            }
        }
        // Starting an empty session behind an existing transcript silently
        // discards its model context. Only an explicit new-session action may
        // create a replacement; keep the original cursor available for retry.
        let detail = if failures.is_empty() {
            "the agent does not advertise session/resume or session/load".to_owned()
        } else {
            failures.join("; ")
        };
        return Err(agent_client_protocol::Error::new(
            agent_client_protocol::ErrorCode::InternalError.into(),
            format!(
                "Could not restore session {existing}. {detail}. Retry this session or create a new conversation."
            ),
        ));
    }

    let response = connection
        .send_request(NewSessionRequest::new(cwd).mcp_servers(client_mcp_servers.to_vec()))
        .block_task()
        .await?;
    Ok((response.session_id, response.modes, response.config_options))
}

fn mcp_launch_denied(error: &agent_client_protocol::Error) -> bool {
    error.data.as_ref().is_some_and(|data| data["kind"] == "mcp_launch_denied")
}

// A save may commit between reading the profile and binding it. Only a
// definite launch rejection is retried here, before a prompt is submitted.
async fn apply_session_mcp<T>(
    (servers, revision): (Vec<McpServer>, String),
    mut apply: impl AsyncFnMut(Vec<McpServer>) -> agent_client_protocol::Result<T>,
) -> agent_client_protocol::Result<(T, Option<String>)> {
    match apply(servers).await {
        Err(error) if mcp_launch_denied(&error) => {}
        result => return result.map(|value| (value, Some(revision))),
    }
    let (latest, latest_revision) = smol::unblock(crate::deerflow_config::client_mcp_servers).await;
    if latest_revision != revision {
        match apply(latest).await {
            Err(error) if mcp_launch_denied(&error) => {}
            result => return result.map(|value| (value, Some(latest_revision))),
        }
    }
    apply(Vec::new()).await.map(|value| (value, None))
}

async fn refresh_session_mcp(
    connection: &ConnectionTo<Agent>,
    session_id: &SessionId,
    cwd: &std::path::Path,
    applied_revision: &mut Option<String>,
) -> anyhow::Result<()> {
    let config = smol::unblock(crate::deerflow_config::client_mcp_servers).await;
    if applied_revision.as_ref() != Some(&config.1) {
        // Resume rebinds under the idle session lease, without replaying the transcript.
        let (_, revision) = apply_session_mcp(config, async |servers| {
            connection.send_request(ResumeSessionRequest::new(session_id.clone(), cwd).mcp_servers(servers))
                .block_task().await
        }).await?;
        *applied_revision = revision;
    }
    Ok(())
}

fn session_config_select_values(option: &SessionConfigOption) -> Vec<&str> {
    let SessionConfigKind::Select(select) = &option.kind else {
        return Vec::new();
    };
    match &select.options {
        SessionConfigSelectOptions::Ungrouped(options) => options
            .iter()
            .map(|option| option.value.0.as_ref())
            .collect(),
        SessionConfigSelectOptions::Grouped(groups) => groups
            .iter()
            .flat_map(|group| group.options.iter())
            .map(|option| option.value.0.as_ref())
            .collect(),
        _ => Vec::new(),
    }
}

fn session_config_current_value(option: &SessionConfigOption) -> Option<&str> {
    let SessionConfigKind::Select(select) = &option.kind else {
        return None;
    };
    Some(select.current_value.0.as_ref())
}

pub(crate) fn parse_acp_config_models(option: &SessionConfigOption) -> Vec<ProviderModel> {
    let SessionConfigKind::Select(select) = &option.kind else {
        return Vec::new();
    };
    let current_value = select.current_value.0.as_ref();
    let mut models = Vec::new();

    let extract_model = |value: &str, name: &str| {
        let (sub_provider, model_id) = value
            .split_once('/')
            .map_or((None, value), |(p, m)| (Some(p), m));
        let display_name = if name.is_empty() || name == value {
            crate::model_catalog::display_name_from_slug(model_id)
        } else {
            name.to_owned()
        };
        let mut model = ProviderModel::new(value, display_name);
        if let Some(sub) = sub_provider.filter(|s| !s.is_empty()) {
            model = model.sub_provider(crate::model_catalog::display_name_from_slug(sub));
        }
        if value == current_value {
            model = model.default();
        }
        model
    };

    match &select.options {
        SessionConfigSelectOptions::Ungrouped(options) => {
            for opt in options {
                models.push(extract_model(opt.value.0.as_ref(), &opt.name));
            }
        }
        SessionConfigSelectOptions::Grouped(groups) => {
            for group in groups {
                for opt in &group.options {
                    models.push(extract_model(opt.value.0.as_ref(), &opt.name));
                }
            }
        }
        _ => {}
    }
    models
}

#[allow(clippy::too_many_arguments)]
async fn apply_model(
    connection: &ConnectionTo<Agent>,
    session_id: &SessionId,
    config_options: Option<&[SessionConfigOption]>,
    model: Option<&str>,
    reasoning_effort: Option<&str>,
    thinking_mode: Option<&str>,
    events: &DriverEventSender,
    stream_state: &Arc<Mutex<AcpStreamState>>,
) -> bool {
    let options = {
        let mut state = stream_state.lock();
        state.deerflow_applying_model = true;
        state.deerflow_config_options.clone()
    };
    let result = apply_deerflow_model(
        connection,
        session_id,
        options.as_deref().or(config_options),
        model,
        reasoning_effort,
        thinking_mode,
    )
    .await;
    let mut state = stream_state.lock();
    state.deerflow_applying_model = false;
    let ready = match result {
        Ok(options) => {
            state.deerflow_config_options = Some(options);
            true
        }
        Err(error) => {
            let _ = events.send(DriverEvent::Error(tr!(
                "errors.select_model",
                error = error
            )));
            false
        }
    };
    if let Some(options) = &state.deerflow_config_options {
        report_deerflow_models(options, events);
    }
    ready
}

async fn apply_deerflow_model(
    connection: &ConnectionTo<Agent>,
    session_id: &SessionId,
    config_options: Option<&[SessionConfigOption]>,
    model: Option<&str>,
    effort: Option<&str>,
    thinking: Option<&str>,
) -> agent_client_protocol::Result<Vec<SessionConfigOption>> {
    let mut options = config_options.unwrap_or_default().to_vec();
    let model_changed = model.is_some_and(|model| {
        !options.iter().any(|option| {
            option.id.to_string() == "model" && session_config_current_value(option) == Some(model)
        })
    });
    if let Some(model) = model.filter(|model| !model.is_empty() && model_changed) {
        let response = connection
            .send_request(SetSessionConfigOptionRequest::new(
                session_id.clone(),
                "model",
                model,
            ))
            .block_task()
            .await?;
        options = response.config_options;
    }
    // Release an explicit on override before resetting effort. Shared-parameter
    // endpoints cannot acknowledge on + an unknown provider-default effort.
    if let Some(mode @ ("off" | "default")) = thinking
        && deerflow_thinking_mode(&options).as_deref() == Some("on")
    {
        let value = deerflow_thinking_value(&options, mode)?;
        options = connection
            .send_request(SetSessionConfigOptionRequest::new(
                session_id.clone(),
                "thinking_enabled",
                value,
            ))
            .block_task()
            .await?
            .config_options;
        if deerflow_thinking_mode(&options).as_deref() != Some(mode) {
            return Err(approval_error(
                "The agent did not confirm the requested thinking mode",
            ));
        }
    }
    if let Some(option) = deerflow_effort_option(&options) {
        let values = session_config_select_values(option);
        let requested = effort.or_else(|| model_changed.then_some("default"));
        if let Some(requested) = requested {
            let value = if values.contains(&requested) {
                requested
            } else if model_changed && values.contains(&"default") {
                "default"
            } else {
                return Err(approval_error(
                    "The selected reasoning effort is not supported by this model",
                ));
            };
            if session_config_current_value(option) != Some(value) {
                let response = connection
                    .send_request(SetSessionConfigOptionRequest::new(
                        session_id.clone(),
                        option.id.clone(),
                        value,
                    ))
                    .block_task()
                    .await?;
                options = response.config_options;
                if !deerflow_effort_option(&options)
                    .is_some_and(|option| session_config_current_value(option) == Some(value))
                {
                    return Err(approval_error(
                        "The agent did not confirm the requested reasoning effort",
                    ));
                }
            }
        }
    }
    let thinking_option = options
        .iter()
        .find(|option| option.id.to_string() == "thinking_enabled");
    // Only old clients encode on/off in effort. Keep that path separate from levels.
    let requested = thinking.or_else(|| {
        deerflow_effort_option(&options)
            .is_none()
            .then_some(effort.filter(|value| matches!(*value, "on" | "off")))
            .flatten()
    });
    if let Some(requested) = requested.filter(|value| !value.is_empty()) {
        if thinking.is_some() && thinking_option.is_none() && requested != "default" {
            return Err(approval_error(
                "This model or daemon does not support a thinking switch",
            ));
        }
        // Legacy effort=on/off is only a hint when the model has no switch.
        // Explicit thinking requests are rejected above instead of ignored.
        if thinking_option.is_some() || options.is_empty() {
            let current = deerflow_thinking_mode(&options);
            if current.as_deref() != Some(requested) {
                let value = deerflow_thinking_value(&options, requested)?;
                let response = connection
                    .send_request(SetSessionConfigOptionRequest::new(
                        session_id.clone(),
                        "thinking_enabled",
                        value,
                    ))
                    .block_task()
                    .await?;
                options = response.config_options;
                // Legacy servers may omit all configuration metadata.
                if (thinking.is_some() || !options.is_empty())
                    && deerflow_thinking_mode(&options).as_deref() != Some(requested)
                {
                    return Err(approval_error(
                        "The agent did not confirm the requested thinking mode",
                    ));
                }
            }
        }
    }
    Ok(options)
}

fn deerflow_effort_option(options: &[SessionConfigOption]) -> Option<&SessionConfigOption> {
    options.iter().find(|option| {
        option.id.to_string() == "reasoning_effort"
            || (option.category == Some(SessionConfigOptionCategory::ThoughtLevel)
                && option.id.to_string() != "thinking_enabled")
    })
}

fn report_deerflow_models(options: &[SessionConfigOption], events: &impl DriverEventSink) {
    let models = crate::model_catalog::parse_deerflow_models(options);
    if models.is_empty() {
        return;
    }
    let model = models
        .iter()
        .find(|model| model.is_default)
        .map(|model| model.id.clone());
    let reasoning_effort = deerflow_effort_option(options)
        .and_then(session_config_current_value)
        .map(str::to_owned);
    let thinking_mode = deerflow_thinking_mode(options);
    let _ = events.send(DriverEvent::DeerFlowModelsChanged {
        models,
        model,
        reasoning_effort,
        thinking_mode,
    });
}

fn deerflow_thinking_mode(options: &[SessionConfigOption]) -> Option<String> {
    options
        .iter()
        .find(|option| option.id.to_string() == "thinking_enabled")
        .and_then(|option| match &option.kind {
            SessionConfigKind::Boolean(toggle) => {
                Some(if toggle.current_value { "on" } else { "off" }.to_owned())
            }
            _ => session_config_current_value(option).map(str::to_owned),
        })
}

fn deerflow_thinking_value(
    options: &[SessionConfigOption],
    requested: &str,
) -> agent_client_protocol::Result<SessionConfigOptionValue> {
    if requested == "default" {
        if options.iter().any(|option| {
            option.id.to_string() == "thinking_enabled"
                && session_config_select_values(option).contains(&"default")
        }) {
            return Ok(SessionConfigOptionValue::value_id(
                agent_client_protocol::schema::v1::SessionConfigValueId::new("default"),
            ));
        }
        return Err(approval_error(
            "The agent does not support the default thinking mode",
        ));
    }
    let enabled = match requested {
        "on" => true,
        "off" => false,
        _ => {
            return Err(agent_client_protocol::Error::new(
                agent_client_protocol::ErrorCode::InvalidParams.into(),
                format!("DeerFlow thinking must be on or off, received {requested}"),
            ));
        }
    };
    match options
        .iter()
        .find(|option| option.id.to_string() == "thinking_enabled")
    {
        Some(SessionConfigOption {
            kind: SessionConfigKind::Boolean(_),
            ..
        }) => Ok(SessionConfigOptionValue::boolean(enabled)),
        // ACP v1's current DeerFlow implementation advertises a select.
        // Preserve support for servers that omit config options on restore.
        _ => Ok(SessionConfigOptionValue::value_id(
            agent_client_protocol::schema::v1::SessionConfigValueId::new(requested.to_owned()),
        )),
    }
}

#[allow(clippy::too_many_arguments)]
#[allow(clippy::too_many_arguments)]
fn send_prompt(
    connection: &ConnectionTo<Agent>,
    session_id: &SessionId,
    text: String,
    attachments: &[crate::model::MessageAttachment],
    prompt_requests: &PendingPromptRequests,
    events: &DriverEventSender,
    native_session_id: &str,
    stream_state: Arc<Mutex<AcpStreamState>>,
) -> agent_client_protocol::Result<()> {
    stream_state.lock().produced_content = false;
    let content = deerflow_prompt_content(text, attachments, &waku_protocol::identity::desktop_data_directory())?;
    let mut request = PromptRequest::new(session_id.clone(), content);
    let foreground_id = {
        let mut stream = stream_state.lock();
        let id = stream
            .deerflow_steering
            .as_mut()
            .map(deerflow_steering::State::begin_prompt);
        deerflow_steering::publish_target(&stream, events);
        id
    };
    if let Some(id) = &foreground_id {
        let mut meta = serde_json::Map::new();
        meta.insert("deerflow".into(), json!({"promptId": id}));
        request = request.meta(meta);
    }
    let sent = connection.send_request(request);
    let request_id = sent.id().clone();
    prompt_requests.lock().insert(
        request_id.clone(),
        native_session_id.to_owned(),
    );
    let callback_request_id = request_id.clone();
    let callback_requests = prompt_requests.clone();
    let callback_events = events.clone();
    let callback_stream = stream_state.clone();
    let callback_foreground = foreground_id.clone();
    let registered = sent.on_receiving_result(async move |result| {
        let stream_state = callback_stream;
        let settled = settle_prompt_request(&callback_requests, &callback_request_id);
        if let Some(id) = &callback_foreground {
            let mut stream = stream_state.lock();
            if let Some(state) = &mut stream.deerflow_steering {
                state.finish_prompt(id);
            }
            if !settled {
                deerflow_steering::publish_target(&stream, &callback_events);
            }
        }
        if settled {
            let mut stream = stream_state.lock();
            deerflow_tasks::finish_prompt(result, &mut stream, &callback_events);
            if let Some(tasks) = &stream.deerflow_tasks {
                tasks.invalidate(&callback_events);
            }
            deerflow_tasks::flush_early_updates(&mut stream, false, &callback_events)?;
        }
        Ok(())
    });
    if registered.is_err() {
        prompt_requests.lock().settle_request(&request_id);
        if let Some(id) = &foreground_id {
            let mut stream = stream_state.lock();
            if let Some(state) = &mut stream.deerflow_steering {
                state.finish_prompt(id);
            }
            deerflow_steering::publish_target(&stream, events);
        }
    }
    registered
}

const MAX_DEERFLOW_IMAGES: usize = 8;
const MAX_DEERFLOW_IMAGE_BYTES: u64 = 20 * 1024 * 1024;
const MAX_DEERFLOW_TOTAL_IMAGE_BYTES: usize = 40 * 1024 * 1024;

fn image_attachment_error(message: impl Into<String>) -> agent_client_protocol::Error {
    agent_client_protocol::Error::new(
        agent_client_protocol::ErrorCode::InvalidParams.into(),
        message,
    )
}

fn image_mime_type(bytes: &[u8]) -> Option<&'static str> {
    if bytes.starts_with(b"\x89PNG\r\n\x1a\n") {
        Some("image/png")
    } else if bytes.starts_with(&[0xff, 0xd8, 0xff]) {
        Some("image/jpeg")
    } else if bytes.starts_with(b"GIF87a") || bytes.starts_with(b"GIF89a") {
        Some("image/gif")
    } else if bytes.starts_with(b"RIFF") && bytes.get(8..12) == Some(b"WEBP".as_slice()) {
        Some("image/webp")
    } else {
        None
    }
}

/// Only explicit, daemon-managed composer attachments become media/resources.
/// Textual @paths never grant permission to read a local file. The canonical
/// containment check also excludes symlinks that escape the attachment store.
fn deerflow_prompt_content(
    text: String,
    attachments: &[crate::model::MessageAttachment],
    data_root: &Path,
) -> agent_client_protocol::Result<Vec<ContentBlock>> {
    let mut content = vec![ContentBlock::Text(TextContent::new(text))];
    content.extend(deerflow_text_attachments(attachments, data_root)?);
    let images = attachments
        .iter()
        .filter(|attachment| attachment.is_image)
        .collect::<Vec<_>>();
    if images.is_empty() {
        return Ok(content);
    }
    if images.len() > MAX_DEERFLOW_IMAGES {
        return Err(image_attachment_error(
            "DeerFlow accepts at most 8 images per message.",
        ));
    }
    let roots = [data_root.join("attachments"), data_root.join("blobs")]
        .into_iter()
        .filter_map(|root| root.canonicalize().ok())
        .collect::<Vec<_>>();
    let mut total = 0;
    for attachment in images {
        if attachment.is_dir
            || attachment
                .blob_reference
                .as_deref()
                .is_none_or(|reference| reference.trim().is_empty())
        {
            return Err(image_attachment_error(format!(
                "Image {} is not a stored attachment. Attach it again.",
                attachment.name
            )));
        }
        let path = attachment.path.canonicalize().map_err(|error| {
            image_attachment_error(format!(
                "Could not read attached image {}: {error}",
                attachment.name
            ))
        })?;
        if !roots.iter().any(|root| path.starts_with(root)) {
            return Err(image_attachment_error(format!(
                "Image {} is outside the desktop attachment store. Attach it again.",
                attachment.name
            )));
        }
        let file = std::fs::File::open(&path).map_err(|error| {
            image_attachment_error(format!(
                "Could not open attached image {}: {error}",
                attachment.name
            ))
        })?;
        let metadata = file
            .metadata()
            .map_err(|error| image_attachment_error(error.to_string()))?;
        // An uncompressed BMP from an earlier draft or an imported file may
        // be larger than its PNG. Bound the read and the outgoing payload separately.
        let source_limit = crate::attachments::MAX_ATTACHMENT_BYTES;
        if !metadata.is_file() || metadata.len() > source_limit as u64 {
            return Err(image_attachment_error(format!(
                "Image {} must be a file no larger than 32 MB.",
                attachment.name
            )));
        }
        let mut bytes = Vec::with_capacity(metadata.len() as usize);
        file.take(source_limit as u64 + 1)
            .read_to_end(&mut bytes)
            .map_err(|error| {
                image_attachment_error(format!("Could not read image {}: {error}", attachment.name))
            })?;
        if bytes.len() > source_limit {
            return Err(image_attachment_error(format!(
                "Image {} exceeds the 32 MB attachment limit.",
                attachment.name
            )));
        }
        let bytes = crate::blob_store::png_from_bmp(&bytes)
            .map_err(|error| {
                image_attachment_error(format!(
                    "Could not convert BMP image {} to PNG: {error}",
                    attachment.name
                ))
            })?
            .unwrap_or(bytes);
        if bytes.len() > MAX_DEERFLOW_IMAGE_BYTES as usize {
            return Err(image_attachment_error(format!(
                "Image {} exceeds the 20 MB limit.",
                attachment.name
            )));
        }
        total += bytes.len();
        if total > MAX_DEERFLOW_TOTAL_IMAGE_BYTES {
            return Err(image_attachment_error(
                "Attached images exceed the 40 MB total limit.",
            ));
        }
        let mime = image_mime_type(&bytes).ok_or_else(|| {
            image_attachment_error(format!(
                "Image {} must contain PNG, JPEG, GIF, WebP, or BMP data.",
                attachment.name
            ))
        })?;
        content.push(ContentBlock::Image(
            agent_client_protocol::schema::v1::ImageContent::new(
                base64::engine::general_purpose::STANDARD.encode(bytes),
                mime,
            ),
        ));
    }
    Ok(content)
}

fn deerflow_text_attachments(
    attachments: &[crate::model::MessageAttachment],
    data_root: &Path,
) -> agent_client_protocol::Result<Vec<ContentBlock>> {
    let files = attachments
        .iter()
        .filter(|a| !a.is_image)
        .collect::<Vec<_>>();
    if files.len() > 8 {
        return Err(image_attachment_error("Attach at most 8 files."));
    }
    let roots = [data_root.join("attachments"), data_root.join("blobs")]
        .into_iter()
        .filter_map(|p| p.canonicalize().ok())
        .collect::<Vec<_>>();
    let mut result = Vec::new();
    let mut total = 0;
    let mut document_total = 0;
    for attachment in files {
        let extension = Path::new(&attachment.name)
            .extension()
            .and_then(|s| s.to_str())
            .unwrap_or("")
            .to_ascii_lowercase();
        let document_mime = match extension.as_str() {
            "pdf" => Some("application/pdf"),
            "docx" => {
                Some("application/vnd.openxmlformats-officedocument.wordprocessingml.document")
            }
            "xlsx" => Some("application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"),
            _ => None,
        };
        if attachment.is_dir
            || !matches!(
                extension.as_str(),
                "txt"
                    | "md"
                    | "markdown"
                    | "csv"
                    | "tsv"
                    | "json"
                    | "yaml"
                    | "yml"
                    | "log"
                    | "pdf"
                    | "docx"
                    | "xlsx"
            )
        {
            return Err(image_attachment_error(
                "Supported files: UTF-8 text, PDF, DOCX and XLSX. Directories and other binary formats are not supported.",
            ));
        }
        if attachment.name.len() > 255
            || attachment
                .name
                .chars()
                .any(|c| c.is_control() || c == '/' || c == '\\')
            || attachment
                .blob_reference
                .as_deref()
                .is_none_or(|s| s.trim().is_empty())
        {
            return Err(image_attachment_error(
                "Invalid stored attachment. Attach it again.",
            ));
        }
        let path = attachment
            .path
            .canonicalize()
            .map_err(|_| image_attachment_error("Attachment is unavailable. Attach it again."))?;
        if !roots.iter().any(|root| path.starts_with(root)) {
            return Err(image_attachment_error(
                "Attachment is outside the desktop attachment store.",
            ));
        }
        let file = std::fs::File::open(path)
            .map_err(|_| image_attachment_error("Could not open attachment."))?;
        let metadata = file
            .metadata()
            .map_err(|_| image_attachment_error("Could not inspect attachment."))?;
        let limit: u64 = if document_mime.is_some() {
            8 * 1024 * 1024
        } else {
            1024 * 1024
        };
        if !metadata.is_file() || metadata.len() > limit {
            return Err(image_attachment_error(
                "Attachments must be regular files: text up to 1 MiB; PDF/DOCX/XLSX up to 8 MiB.",
            ));
        }
        let mut bytes = Vec::new();
        file.take(limit + 1)
            .read_to_end(&mut bytes)
            .map_err(|_| image_attachment_error("Could not read attachment."))?;
        if bytes.len() > limit as usize {
            return Err(image_attachment_error(
                "Attachment grew beyond its size limit.",
            ));
        }
        let mut uri = url::Url::parse("attachment:///").expect("fixed attachment URI");
        uri.path_segments_mut()
            .expect("hierarchical attachment URI")
            .clear()
            .push(&attachment.name);
        if let Some(mime) = document_mime {
            document_total += bytes.len();
            if document_total > 16 * 1024 * 1024 {
                return Err(image_attachment_error(
                    "Documents exceed 16 MiB per message.",
                ));
            }
            result.push(
                serde_json::from_value(json!({"type":"resource", "resource":{
                    "uri":uri.as_str(), "mimeType":mime,
                    "blob":base64::engine::general_purpose::STANDARD.encode(bytes)
                }}))
                .map_err(|_| image_attachment_error("Could not encode document attachment."))?,
            );
            continue;
        }
        total += bytes.len();
        if bytes.len() > 1024 * 1024 || total > 4 * 1024 * 1024 {
            return Err(image_attachment_error(
                "Text attachments exceed 1 MiB per file or 4 MiB per message.",
            ));
        }
        let text = String::from_utf8(bytes)
            .map_err(|_| image_attachment_error("Text attachments must use UTF-8 encoding."))?;
        if text
            .chars()
            .any(|c| c < ' ' && !matches!(c, '\n' | '\r' | '\t'))
        {
            return Err(image_attachment_error(
                "Binary content is not a supported text attachment.",
            ));
        }
        result.push(
            serde_json::from_value(json!({"type":"resource", "resource":{
                "uri":uri.as_str(), "mimeType":"text/plain", "text":text
            }}))
            .map_err(|_| image_attachment_error("Could not encode text attachment."))?,
        );
    }
    Ok(result)
}

fn settle_prompt_request(prompt_requests: &Mutex<PendingPrompts>, request_id: &RequestId) -> bool {
    prompt_requests.lock().settle_request(request_id)
}

fn token_limit_summary(limit: Option<&str>) -> String {
    match limit {
        Some("output_tokens") => tr!("session.model_output_limit"),
        Some("tokens") => tr!("session.run_token_budget_reached"),
        _ => tr!("session.token_limit_reached"),
    }
}

// Both prompt and explicit resume can fail after an approval was accepted.
// Use the durable execution status before telling the user the turn has ended.
fn deerflow_failure_message(error: &agent_client_protocol::Error) -> Option<&'static str> {
    let data = error.data.as_ref();
    let kind = data.and_then(|data| data["kind"].as_str());
    let status = data.and_then(|data| data["execution"]["status"].as_str());
    match (i32::from(error.code), kind) {
        (_, Some("pinned_search_config_invalid")) if status == Some("failed") => Some(
            "本轮保存的搜索配置未通过完整性校验，本轮已停止。已完成的工具结果已保留，可在此对话发送新消息继续。",
        ),
        (_, Some("pinned_search_config_invalid")) => Some(
            "本轮保存的搜索配置未通过完整性校验。已完成的工具结果已保留。",
        ),
        (-32012, _) | (_, Some("execution_waiting_input")) => Some(
            "上一轮执行仍在等待处理。可在此对话继续或结束上一轮。",
        ),
        (-32014, _) | (_, Some("execution_unresumable")) if status == Some("failed") => Some(
            "上一次执行无法从保存的进度恢复，本轮已停止。已完成的工具结果已保留，可在此对话发送新消息继续。",
        ),
        (-32014, _) | (_, Some("execution_unresumable")) => Some(
            "上一次执行无法从保存的进度恢复。已完成的工具结果已保留。",
        ),
        (_, Some("execution_stopped")) => Some(
            "上一次执行已经停止，可在此对话发送新消息继续。",
        ),
        (-32013, None) | (_, Some("execution_conflict")) => Some(
            "执行状态已经变化，请按此对话中的最新状态继续。",
        ),
        (-32026, _) | (_, Some("model_response_invalid")) => Some(
            "模型流式响应解析失败，本轮已停止。已完成的工具结果已保留，请发送新消息继续。",
        ),
        (-32033, _) | (_, Some("model_transport_failed")) => Some(
            "模型服务或本地代理连接中断，本轮已停止。已完成的工具结果已保留；请检查网络或代理后在原会话发送新消息继续，系统不会自动重放已完成的操作。",
        ),
        (-32028, _) | (_, Some("checkpoint_too_large")) => Some(
            "执行状态过大，无法保存本轮进度。请减少并行子任务后发送新消息继续；已保存的工具结果会保留。",
        ),
        (-32029, _) | (_, Some("checkpoint_invalid")) => Some(
            "执行状态校验失败，暂时无法恢复。本轮已停止，已保存的记录会保留；请发送新消息继续。",
        ),
        (-32030, _) | (_, Some("checkpoint_persist_failed")) => Some(
            "执行进度保存失败，本轮已停止。请检查本机存储后重试；已保存的记录会保留。",
        ),
        (-32027, _) | (_, Some("model_rate_limited")) => Some(
            "模型服务请求受到限流，本轮已停止。请稍后重试或切换模型；已完成的工具结果已保留，请检查结果后再继续。",
        ),
        _ => None,
    }
}

fn finish_prompt(
    provider: ProviderKind,
    result: agent_client_protocol::Result<PromptResponse>,
    native_failure: Option<String>,
    events: &impl DriverEventSink,
) -> bool {
    let response = match result {
        Ok(response) => response,
        Err(error) => {
            let known_failure_message = (provider == ProviderKind::DeerFlow)
                .then(|| deerflow_failure_message(&error))
                .flatten();
            let message = error.to_string();
            let message = if let Some(known_failure_message) = known_failure_message {
                known_failure_message.to_owned()
            } else if provider == ProviderKind::DeerFlow
                && (message.contains("reconciliation_required")
                    || message.contains("Uncertain tool effects require review")
                    || message.contains("previous execution could not finish safely"))
            {
                "上一次执行未能正常结束，已保留执行记录。系统不会自动重放旧操作。".to_owned()
            } else {
                message
            };
            let _ = events.send(DriverEvent::Error(message));
            let _ = events.send(DriverEvent::TurnFinished {
                success: false,
                summary: None,
            });
            return false;
        }
    };
    // An agent can end a turn cleanly and still have failed upstream. Where
    // that failure is recoverable from the provider's own records, it outranks
    // the protocol's verdict: reporting success here would show the user an
    // empty answer and no reason for it.
    if let Some(failure) = native_failure {
        let _ = events.send(DriverEvent::Error(failure));
        let _ = events.send(DriverEvent::TurnFinished {
            success: false,
            summary: None,
        });
        return false;
    }
    let (success, summary) = match response.stop_reason {
        StopReason::Cancelled => {
            let _ = events.send(DriverEvent::TurnCancelled);
            return true;
        }
        StopReason::EndTurn => (true, None),
        StopReason::MaxTokens => (
            false,
            Some(token_limit_summary(
                response
                    .meta
                    .as_ref()
                    .and_then(|meta| meta.get("deerflow"))
                    .and_then(|meta| meta.get("limit"))
                    .and_then(Value::as_str),
            )),
        ),
        StopReason::Refusal => (false, Some(tr!("session.agent_declined_turn"))),
        StopReason::MaxTurnRequests => (
            false,
            Some(tr!(
                "session.agent_stopped_reason",
                reason = "max_turn_requests"
            )),
        ),
        _ => (
            false,
            Some(tr!("session.agent_stopped_reason", reason = "unknown")),
        ),
    };
    let _ = events.send(DriverEvent::TurnFinished { success, summary });
    success
}

fn cancel_pending_permissions(pending: &PendingPermissions, events: &impl DriverEventSink) {
    let entries = std::mem::take(&mut *pending.lock());
    for (request_id, responder) in entries {
        let _ = responder.respond(RequestPermissionResponse::new(RequestPermissionOutcome::Cancelled));
        let _ = events.send(DriverEvent::PermissionResolved { request_id });
    }
}

fn cancel_pending_user_inputs(pending: &PendingAcpUserInputs) {
    for (_, pending) in pending.lock().drain() {
        let _ = pending
            .responder
            .respond(cancelled_user_input_response());
    }
}

fn cancelled_user_input_response() -> Value {
    json!({"outcome": "cancelled"})
}

fn valid_credential_form(form: &crate::model::CredentialForm) -> bool {
    let required = match form.required_fields.as_slice() {
        [one] => one == "username" || one == "key",
        [first, second] => first == "username" && second == "key",
        _ => false,
    };
    form.version == 2 && required && credential_public_key(&form.public_key)
        && !form.consumer.is_empty() && !form.service.is_empty() && !form.account.is_empty()
}

fn handle_permission_request(
    request: RequestPermissionRequest,
    responder: PermissionResponder,
    provider: ProviderKind,
    _mode: RuntimeMode,
    pending: &PendingPermissions,
    events: &impl DriverEventSink,
) -> agent_client_protocol::Result<()> {
    // A delayed response from a disconnected UI must never match a later ACP request.
    let request_id = uuid::Uuid::new_v4().to_string();
    let params = serde_json::to_value(&request)?;
    let credential_form = if provider == ProviderKind::DeerFlow {
        match params.pointer("/_meta/deerflow/credentialForm") {
            Some(raw) => {
                let form = serde_json::from_value::<crate::model::CredentialForm>(raw.clone())
                    .map_err(|_| approval_error("Invalid DeerFlow credential form"))?;
                if params.pointer("/_meta/deerflow/credentialApproval") != Some(&Value::Bool(true))
                    || !valid_credential_form(&form)
                    || !request.options.iter().any(|option| option.kind == PermissionOptionKind::AllowOnce
                        && option.option_id.to_string() == "allow_once")
                {
                    return Err(approval_error("Unsupported DeerFlow credential form"));
                }
                Some(form)
            }
            None => None,
        }
    } else {
        None
    };
    let options = request
        .options
        .iter()
        .map(|option| PermissionOption {
            credential_form: if option.kind == PermissionOptionKind::AllowOnce && option.option_id.to_string() == "allow_once" { credential_form.clone() } else { None },
            id: option.option_id.to_string(),
            label: option.name.clone(),
            allow: matches!(
                option.kind,
                PermissionOptionKind::AllowOnce | PermissionOptionKind::AllowAlways
            ),
        })
        .collect::<Vec<_>>();

    let title = params
        .pointer("/toolCall/title")
        .and_then(Value::as_str)
        .map(str::to_owned)
        .unwrap_or_else(|| tr!("permission.run_a_tool"));
    let detail = permission_reason(&params).unwrap_or_else(|| {
        params
            .pointer("/toolCall/kind")
            .and_then(Value::as_str)
            .map(|kind| tr!("permission.agent_wants_to", action = kind))
            .unwrap_or_else(|| tr!("permission.agent_asks_for_permission"))
    });
    pending.lock().insert(request_id.clone(), PendingAcpPermission { responder, public_key: credential_form.map(|form| form.public_key) });
    if events
        .send(DriverEvent::Permission {
            request_id: request_id.clone(),
            title,
            detail,
            options,
        })
        .is_err()
        && let Some(responder) = pending.lock().remove(&request_id)
    {
        let _ = responder.respond(RequestPermissionResponse::new(
            RequestPermissionOutcome::Cancelled,
        ));
    }
    Ok(())
}

fn handle_session_update(
    provider: ProviderKind,
    notification: SessionNotification,
    events: &impl DriverEventSink,
    state: &mut AcpStreamState,
) -> agent_client_protocol::Result<()> {
    let update = serde_json::to_value(notification.update)?;
    let kind = update.get("sessionUpdate").and_then(Value::as_str);
    if matches!(
        kind,
        Some(
            "agent_message_chunk"
                | "agent_thought_chunk"
                | "tool_call"
                | "tool_call_update"
                | "plan"
        )
    ) {
        state.produced_content = true;
    }
    match kind {
        Some("agent_message_chunk") => {
            if let Some(text) = update.get("content").and_then(|content| {
                crate::acp_session::content_markdown(content, state.image_store.as_deref())
            }) {
                let _ = events.send(DriverEvent::TextDelta(text));
            }
        }
        Some("config_option_update") if provider == ProviderKind::DeerFlow => {
            if let Some(value) = update.get("configOptions") {
                let options: Vec<SessionConfigOption> = serde_json::from_value(value.clone())?;
                if !state.deerflow_applying_model {
                    report_deerflow_models(&options, events);
                }
                state.deerflow_config_options = Some(options);
            }
        }
        Some("agent_thought_chunk") => {
            if let Some(text) = update
                .pointer("/content/text")
                .and_then(Value::as_str)
                .filter(|text| !text.is_empty())
            {
                let _ = events.send(DriverEvent::ReasoningDelta(text.to_owned()));
            }
        }
        Some("tool_call" | "tool_call_update") => tool_activity(&update, events, state),
        Some("plan") => {
            let entries = update.get("entries").and_then(Value::as_array);
            let completed = entries.map_or(0, |entries| {
                entries
                    .iter()
                    .filter(|entry| {
                        entry.get("status").and_then(Value::as_str) == Some("completed")
                    })
                    .count()
            });
            let total = entries.map_or(0, Vec::len);
            let output = entries.map(|entries| {
                entries
                    .iter()
                    .filter_map(|entry| {
                        let content = entry.get("content").and_then(Value::as_str)?;
                        let status = entry
                            .get("status")
                            .and_then(Value::as_str)
                            .unwrap_or("pending");
                        let marker = if status == "completed" { "x" } else { " " };
                        let suffix = if status == "in_progress" {
                            " (in progress)"
                        } else {
                            ""
                        };
                        Some(format!("- [{marker}] {content}{suffix}"))
                    })
                    .collect::<Vec<_>>()
                    .join("\n")
            });
            let item = crate::model::ActivityItem::new(
                Some("acp-plan".into()),
                ActivityKind::Plan,
                tr!("activity.plan_updated"),
                Some(format!("{completed}/{total} completed")),
                total > 0 && completed == total,
            )
            // Keep the native entries for runtime progress. Markdown remains
            // the transcript presentation, not the source of todo status.
            .with_arguments(Some(json!({"entries": entries}).to_string()))
            .with_output(output);
            let _ = events.send(DriverEvent::RichActivity(item));
        }
        Some("available_commands_update") => {
            let commands = update
                .get("availableCommands")
                .and_then(Value::as_array)
                .map(|list| {
                    list.iter()
                        .filter_map(|command| {
                            let name = command.get("name").and_then(Value::as_str)?;
                            Some(crate::model::ReportedCommand {
                                name: name.to_owned(),
                                description: command
                                    .get("description")
                                    .and_then(Value::as_str)
                                    .unwrap_or_default()
                                    .to_owned(),
                            })
                        })
                        .collect::<Vec<_>>()
                })
                .unwrap_or_default();
            if !commands.is_empty() {
                let _ = events.send(DriverEvent::AvailableCommands(commands));
            }
        }
        Some("session_info_update") => {
            if update.get("title").is_some() {
                let title = update
                    .get("title")
                    .and_then(Value::as_str)
                    .map(str::to_owned);
                let _ = events.send(DriverEvent::AutoTitleUpdated(title));
            }
        }
        Some("usage_update") => {
            let used = update
                .get("used")
                .and_then(Value::as_u64)
                .filter(|used| *used > 0);
            let window = ["max", "limit", "size", "contextWindow", "context_window"]
                .into_iter()
                .find_map(|key| update.get(key).and_then(Value::as_u64))
                .filter(|window| *window > 0);
            if used.is_some() || window.is_some() {
                let _ = events.send(DriverEvent::UsageUpdated {
                    context_tokens: used,
                    context_window: window,
                });
            }
        }
        // `user_message_chunk` is Waku's own prompt echoed back. Other typed
        // updates currently have no transcript representation.
        _ => {}
    }
    Ok(())
}

#[derive(Default)]
struct AcpStreamState {
    steering_availability: Arc<Mutex<deerflow_steering::Availability>>,
    image_store: Option<Arc<crate::blob_store::BlobStore>>,
    deerflow_steering: Option<deerflow_steering::State>,
    deerflow_config_options: Option<Vec<SessionConfigOption>>,
    deerflow_applying_model: bool,
    deerflow_tasks: Option<deerflow_tasks::NotificationState>,
    deerflow_early: deerflow_tasks::EarlyUpdates,
    deerflow_buffering: bool,
    tools: HashMap<String, (ActivityKind, String)>,
    /// Whether the running turn has produced anything visible. A turn that
    /// ends having produced nothing is the shape a swallowed provider error
    /// takes, which is what makes a native failure worth looking up.
    produced_content: bool,
}

/// Pull the agent's explanation out of a permission request's tool call.
fn permission_reason(params: &Value) -> Option<String> {
    let content = params
        .pointer("/toolCall/content")
        .and_then(Value::as_array)?;
    let reason = content
        .iter()
        .filter_map(|entry| {
            entry
                .pointer("/content/text")
                .or_else(|| entry.get("text"))
                .and_then(Value::as_str)
                .map(str::trim)
                .filter(|text| !text.is_empty())
        })
        .collect::<Vec<_>>()
        .join("\n");
    let limit = if params.pointer("/_meta/deerflow/credentialApproval") == Some(&Value::Bool(true)) {
        131_072
    } else {
        400
    };
    (!reason.is_empty()).then(|| truncate(&reason, limit))
}

fn truncate(text: &str, max_chars: usize) -> String {
    if text.chars().count() <= max_chars {
        return text.to_owned();
    }
    text.chars()
        .take(max_chars)
        .chain(std::iter::once('…'))
        .collect()
}

fn tool_activity(update: &Value, events: &impl DriverEventSink, state: &mut AcpStreamState) {
    let id = update
        .get("toolCallId")
        .and_then(Value::as_str)
        .map(str::to_owned);
    let status = update
        .get("status")
        .and_then(Value::as_str)
        .unwrap_or("pending");
    let complete = matches!(status, "completed" | "failed");
    let failed = status == "failed";

    let wire_kind = update.get("kind").and_then(Value::as_str);
    let wire_title = update.get("title").and_then(Value::as_str);
    let stored = id.as_ref().and_then(|id| {
        if complete {
            state.tools.remove(id)
        } else {
            state.tools.get(id).cloned()
        }
    });
    let mut kind = wire_kind
        .map(classify)
        .or_else(|| stored.as_ref().map(|(kind, _)| *kind))
        .unwrap_or(ActivityKind::Tool);
    if matches!(kind, ActivityKind::Search | ActivityKind::Tool)
        && let Some(wire_title) = wire_title
    {
        let named_kind = ActivityKind::from_tool_name(wire_title);
        if named_kind != ActivityKind::Tool {
            kind = named_kind;
        }
    }
    let arguments = update.get("rawInput").filter(|value| !value.is_null());
    let title = activity::input_title(arguments)
        .or_else(|| {
            wire_title
                .filter(|title| !title.is_empty())
                .map(str::to_owned)
        })
        .or_else(|| stored.map(|(_, title)| title))
        .unwrap_or_else(|| "Tool".to_owned());
    if !complete && let Some(id) = id.as_ref() {
        state.tools.insert(id.clone(), (kind, title.clone()));
    }

    let output = update
        .get("content")
        .filter(|value| !value.is_null())
        .or_else(|| update.get("rawOutput").filter(|value| !value.is_null()));
    let item =
        activity::tool_activity(id, kind, title, arguments, output, output, failed, complete)
            .with_tool_name(
                arguments
                    .and_then(|input| input.get("tool_name"))
                    .and_then(Value::as_str)
                    .or_else(|| wire_title.filter(|title| title.starts_with("mcp__"))),
            );
    let _ = events.send(DriverEvent::RichActivity(item));
}

fn classify(kind: &str) -> ActivityKind {
    match kind {
        "execute" => ActivityKind::Command,
        "edit" | "delete" | "move" => ActivityKind::FileChange,
        "read" => ActivityKind::FileRead,
        "search" | "fetch" => ActivityKind::Search,
        "think" => ActivityKind::Reasoning,
        _ => ActivityKind::Tool,
    }
}

impl DriverControl for AcpDriver {
    fn delete_session(
        &self,
        expected_native_session_id: String,
    ) -> crossbeam_channel::Receiver<Result<(), String>> {
        let (reply, result) = crossbeam_channel::bounded(1);
        if self
            .commands
            .try_send(CommandMessage::DeleteSession(
                expected_native_session_id,
                reply.clone(),
            ))
            .is_err()
        {
            let _ = reply.send(Err("The DeerFlow ACP connection is closed".into()));
        }
        result
    }
    fn deerflow_tasks(&self, command: waku_protocol::deerflow_tasks::Command) {
        if self
            .commands
            .try_send(CommandMessage::DeerFlowTasks(command.clone()))
            .is_err()
        {
            let _ = self.events.send(DriverEvent::DeerFlowTasks(
                waku_protocol::deerflow_tasks::Event::Failed {
                    command,
                    code: "connection_closed".into(),
                    message: "任务连接已断开，请重新连接后刷新。".into(),
                },
            ));
        }
    }
    fn prompt(&self, prompt: String) {
        self.enqueue_prompt(prompt, Vec::new());
    }

    fn prompt_with_attachments(
        &self,
        prompt: String,
        attachments: Vec<crate::model::MessageAttachment>,
    ) {
        self.enqueue_prompt(prompt, attachments);
    }

    fn supports_steer(&self) -> bool {
        self.steering_availability.lock().supported
    }

    fn steer(&self, prompt: String) {
        let _ = self
            .commands
            .try_send(CommandMessage::Steer(prompt, self.steering_run_id()));
    }

    fn steering_run_id(&self) -> Option<String> {
        self.steering_availability.lock().run_id.clone()
    }

    fn steer_run(&self, prompt: String, run_id: String) {
        if self.provider == ProviderKind::DeerFlow {
            let _ = self
                .commands
                .try_send(CommandMessage::Steer(prompt, Some(run_id)));
        }
    }

    fn cancel(&self) {
        let _ = self.commands.try_send(CommandMessage::Cancel);
    }

    fn respond_credential(&self, id: String, sealed: crate::model::SealedCredential) -> Result<(), String> {
        let (reply, result) = crossbeam_channel::bounded(1);
        let deadline = Instant::now() + TOOL_APPROVAL_TIMEOUT;
        self.commands.try_send(CommandMessage::RespondCredential { id, sealed, deadline, reply })
            .map_err(|_| "Credential request could not be queued".to_owned())?;
        result.recv_deadline(deadline).map_err(|_| "Credential delivery status unknown".to_owned())?
    }

    fn respond(&self, request_id: String, option_id: String) {
        let _ = self.respond_checked(request_id, option_id);
    }

    fn respond_checked(&self, request_id: String, option_id: String) -> Result<(), String> {
        let (reply, result) = crossbeam_channel::bounded(1);
        let deadline = Instant::now() + TOOL_APPROVAL_TIMEOUT;
        self.commands.try_send(CommandMessage::Respond { request_id, option_id, deadline, reply })
            .map_err(|_| "Permission reply could not be queued".to_owned())?;
        result.recv_deadline(deadline).map_err(|_| "Permission delivery status unknown".to_owned())?
    }

    fn respond_user_input(&self, request_id: String, answers: Vec<UserInputAnswer>) {
        let _ = self.commands.try_send(CommandMessage::RespondUserInput {
            request_id,
            answers,
            receipt: None,
        });
    }

    fn respond_user_input_checked(&self, request_id: String, answers: Vec<UserInputAnswer>) -> crate::model::LocalUserInputResult {
        use crate::model::LocalUserInputResult;
        let (reply, result) = crossbeam_channel::bounded(1);
        let deadline = Instant::now() + TOOL_APPROVAL_TIMEOUT;
        if self.commands.try_send(CommandMessage::RespondUserInput {
            request_id,
            answers,
            receipt: Some((deadline, reply)),
        }).is_err() {
            return LocalUserInputResult::NotSent;
        }
        result.recv_deadline(deadline).unwrap_or(LocalUserInputResult::Unknown)
    }

    fn apply_options(&self, options: SessionOptions) -> bool {
        // DeerFlow owns its policy through its advertised approval option.
        // A stale client mode accompanying a model change must never
        // overwrite it or force a restart that pretends the stale mode is
        // authoritative.
        if self.provider != ProviderKind::DeerFlow
            && options.mode != self.access.lock().runtime_mode
        {
            return false;
        }
        self.commands
            .try_send(CommandMessage::Options(options))
            .is_ok()
    }

    fn set_tool_approval(&self, mode: String) -> anyhow::Result<String> {
        if self.provider != ProviderKind::DeerFlow
            || !matches!(mode.as_str(), "ask" | "allow_always")
        {
            anyhow::bail!("tool approval changes require DeerFlow and mode ask or allow_always");
        }
        let (reply, result) = crossbeam_channel::bounded(1);
        self.commands
            .try_send(CommandMessage::SetToolApproval {
                mode,
                deadline: Instant::now() + TOOL_APPROVAL_TIMEOUT,
                reply,
            })
            .map_err(|_| anyhow!("The DeerFlow ACP connection is closed"))?;
        result
            .recv_timeout(TOOL_APPROVAL_TIMEOUT)
            .context(
                "Timed out changing DeerFlow tool approvals; awaiting the backend's final state",
            )?
            .map_err(anyhow::Error::msg)
    }

    fn deerflow_history(&self, method: &str, params: Value) -> anyhow::Result<Value> {
        if self.provider != ProviderKind::DeerFlow {
            anyhow::bail!("native conversation history requires DeerFlow");
        }
        let timeout = Duration::from_secs(35);
        let (reply, result) = crossbeam_channel::bounded(1);
        self.commands
            .try_send(CommandMessage::History {
                method: method.to_owned(),
                params,
                deadline: Instant::now() + timeout,
                reply,
            })
            .map_err(|_| anyhow!("unknown: The DeerFlow ACP connection is closed"))?;
        result
            .recv_timeout(timeout)
            .context("unknown: Native history outcome is pending; retry the same operation")?
            .map_err(anyhow::Error::msg)
    }

}

impl Drop for AcpDriver {
    fn drop(&mut self) {
        self.cancel_computer_use();
        let _ = self.commands.try_send(CommandMessage::Shutdown);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use agent_client_protocol::schema::v1::{
        SessionConfigSelectOption, SessionMode, SessionModeState, ToolCallUpdate,
        ToolCallUpdateFields,
    };

    #[test]
    fn native_history_requires_all_versioned_methods() {
        let mut initialization = json!({"_meta":{"deerflow":{"conversation":{
            "version":1,"turnsMethod":"_deerflow/history/turns",
            "forkMethod":"_deerflow/history/fork","rewindMethod":"_deerflow/history/rewind"
        }}}});
        assert!(deerflow_history_capability(&initialization));
        initialization["_meta"]["deerflow"]["conversation"]["version"] = json!(2);
        assert!(!deerflow_history_capability(&initialization));
        assert!(!deerflow_history_capability(&json!({})));
    }

    #[test]
    fn failed_resume_and_load_never_create_a_replacement_session() {
        use agent_client_protocol::schema::v1::{
            LoadSessionResponse, NewSessionResponse, ResumeSessionResponse,
        };
        let calls = Arc::new(Mutex::new(Vec::new()));
        let resume_calls = calls.clone();
        let load_calls = calls.clone();
        let new_calls = calls.clone();
        let agent = Agent
            .builder()
            .on_receive_request(
                async move |_request: ResumeSessionRequest,
                            responder: Responder<ResumeSessionResponse>,
                            _connection| {
                    resume_calls.lock().push("resume");
                    responder.respond_with_internal_error("stored session is unavailable")
                },
                agent_client_protocol::on_receive_request!(),
            )
            .on_receive_request(
                async move |_request: LoadSessionRequest,
                            responder: Responder<LoadSessionResponse>,
                            _connection| {
                    load_calls.lock().push("load");
                    responder.respond_with_internal_error("checkpoint cannot be loaded")
                },
                agent_client_protocol::on_receive_request!(),
            )
            .on_receive_request(
                async move |_request: NewSessionRequest,
                            responder: Responder<NewSessionResponse>,
                            _connection| {
                    new_calls.lock().push("new");
                    responder.respond(NewSessionResponse::new("replacement"))
                },
                agent_client_protocol::on_receive_request!(),
            );
        let initialize: InitializeResponse = serde_json::from_value(json!({
            "protocolVersion": 1,
            "agentCapabilities": {"loadSession": true, "sessionCapabilities": {"resume": {}}}
        }))
        .unwrap();
        let suppressed = Arc::new(AtomicBool::new(false));
        let suppression_state = suppressed.clone();
        let cwd = std::env::temp_dir();
        let result = smol::block_on(Client.builder().connect_with(
            agent,
            async move |connection| {
                establish_session(
                    &connection,
                    &initialize,
                    Some("original-session"),
                    &cwd,
                    &[],
                    &suppression_state,
                )
                .await
            },
        ));
        let error = result.unwrap_err().to_string();
        assert!(error.contains("original-session"), "{error}");
        assert!(
            error.contains("session/resume") && error.contains("session/load"),
            "{error}"
        );
        assert_eq!(*calls.lock(), ["resume", "load"]);
        assert!(!suppressed.load(Ordering::Acquire));
    }

    #[test]
    fn deerflow_passes_client_mcp_servers_to_new_resume_and_load() {
        use agent_client_protocol::schema::v1::{
            EnvVariable, LoadSessionResponse, McpServerStdio, NewSessionResponse,
            ResumeSessionResponse,
        };
        let cwd = std::env::temp_dir();
        let servers = vec![McpServer::Stdio(
            McpServerStdio::new("workspace", cwd.join("mcp-server"))
                .args(vec!["serve".into()])
                .env(vec![EnvVariable::new("MCP_TOKEN", "private")]),
        )];
        for (expected_method, existing, resume_capability) in [
            ("new", None, false),
            ("resume", Some("session-1"), true),
            ("load", Some("session-1"), false),
        ] {
            let calls = Arc::new(Mutex::new(Vec::<(&'static str, Vec<McpServer>)>::new()));
            let new_calls = calls.clone();
            let resume_calls = calls.clone();
            let load_calls = calls.clone();
            let agent = Agent
                .builder()
                .on_receive_request(
                    async move |request: NewSessionRequest,
                                responder: Responder<NewSessionResponse>,
                                _connection| {
                        new_calls.lock().push(("new", request.mcp_servers));
                        responder.respond(NewSessionResponse::new("session-1"))
                    },
                    agent_client_protocol::on_receive_request!(),
                )
                .on_receive_request(
                    async move |request: ResumeSessionRequest,
                                responder: Responder<ResumeSessionResponse>,
                                _connection| {
                        resume_calls.lock().push(("resume", request.mcp_servers));
                        responder.respond(ResumeSessionResponse::new())
                    },
                    agent_client_protocol::on_receive_request!(),
                )
                .on_receive_request(
                    async move |request: LoadSessionRequest,
                                responder: Responder<LoadSessionResponse>,
                                _connection| {
                        load_calls.lock().push(("load", request.mcp_servers));
                        responder.respond(LoadSessionResponse::new())
                    },
                    agent_client_protocol::on_receive_request!(),
                );
            let initialize: InitializeResponse = serde_json::from_value(json!({
                "protocolVersion": 1,
                "agentCapabilities": {
                    "loadSession": true,
                    "sessionCapabilities": {"resume": if resume_capability { json!({}) } else { Value::Null }}
                }
            }))
            .unwrap();
            let suppression = AtomicBool::new(false);
            smol::block_on(Client.builder().connect_with(agent, async |connection| {
                establish_session(
                    &connection,
                    &initialize,
                    existing,
                    &cwd,
                    &servers,
                    &suppression,
                )
                .await
            }))
            .unwrap();
            assert_eq!(*calls.lock(), [(expected_method, servers.clone())]);
        }
    }

    #[test]
    fn deerflow_can_change_thinking_without_selecting_a_model() {
        use agent_client_protocol::schema::v1::SetSessionConfigOptionResponse;
        let calls = Arc::new(Mutex::new(Vec::new()));
        let captured = calls.clone();
        let agent = Agent.builder().on_receive_request(
            async move |request: SetSessionConfigOptionRequest,
                        responder: Responder<SetSessionConfigOptionResponse>,
                        _connection| {
                captured.lock().push(serde_json::to_value(request)?);
                responder.respond(SetSessionConfigOptionResponse::new(Vec::new()))
            },
            agent_client_protocol::on_receive_request!(),
        );
        smol::block_on(
            Client
                .builder()
                .connect_with(agent, async move |connection| {
                    apply_deerflow_model(
                        &connection,
                        &SessionId::new("s"),
                        None,
                        None,
                        Some("off"),
                        None,
                    )
                    .await
                }),
        )
        .unwrap();
        let calls = calls.lock();
        assert_eq!(calls.len(), 1);
        assert_eq!(calls[0]["configId"], "thinking_enabled");
        assert_eq!(calls[0]["value"], "off");
    }

    fn approval_options_with_id(config_id: &str, mode: &str) -> Vec<SessionConfigOption> {
        serde_json::from_value(json!([{
            "id":config_id, "name":"Tool approvals", "type":"select",
            "currentValue":mode,
            "options":[
                {"value":"global", "name":"Follow global policy"},
                {"value":"ask", "name":"Ask"},
                {"value":"allow_always", "name":"Allow"},
                {"value":"reject_always", "name":"Reject"},
                {"value":"read_only", "name":"Read only"}
            ]
        }]))
        .unwrap()
    }

    fn approval_options(mode: &str) -> Vec<SessionConfigOption> {
        approval_options_with_id("tool_approval", mode)
    }

    fn fixture_credential_public_key(point: u8) -> String {
        base64::engine::general_purpose::STANDARD.encode([P256_PUBLIC_HEADER, &[point; 64]].concat())
    }

    #[test]
    fn credential_form_accepts_only_supported_field_sets() {
        let mut form = crate::model::CredentialForm {
            version: 2,
            public_key: fixture_credential_public_key(1),
            consumer: "tool:execute".into(),
            service: "https://service.example".into(),
            account: "default".into(),
            required_fields: Vec::new(),
        };
        for (names, valid) in [
            (vec!["key"], true),
            (vec!["username"], true),
            (vec!["username", "key"], true),
            (vec![], false),
            (vec!["key", "username"], false),
            (vec!["key", "key"], false),
            (vec!["password"], false),
        ] {
            form.required_fields = names.iter().map(|s| s.to_string()).collect();
            assert_eq!(valid_credential_form(&form), valid, "{names:?}");
        }
        form.required_fields = vec!["key".into()];
        form.version = 1;
        assert!(!valid_credential_form(&form));
    }

    #[test]
    fn mobile_enrollment_roundtrip_is_bound_to_one_live_deerflow_permission() {
        let agent = Agent.builder().on_receive_request(
            async move |_prompt: PromptRequest, responder: Responder<PromptResponse>, connection: ConnectionTo<Client>| {
                let request: RequestPermissionRequest = serde_json::from_value(json!({
                    "sessionId":"s", "toolCall":{"toolCallId":"call","title":"execute"},
                    "options":[{"optionId":"allow_once","name":"Allow once","kind":"allow_once"}],
                    "_meta":{"deerflow":{"credentialApproval":true,"credentialForm":{
                        "version":2,"requiredFields":["key"],"publicKey":fixture_credential_public_key(1),"consumer":"tool:execute","service":"https://example.com","account":"default"
                    }}}
                }))?;
                connection.send_request(request).on_receiving_result(async move |result| {
                    let response = serde_json::to_value(result?)?;
                    assert_eq!(response["outcome"]["optionId"], "allow_once");
                    assert_eq!(response["_meta"]["sealedCredential"]["ciphertext"], "B".repeat(28));
                    responder.respond(PromptResponse::new(StopReason::EndTurn))
                })
            },
            agent_client_protocol::on_receive_request!(),
        );
        let (events, received) = crate::driver::test_event_channel();
        let pending: PendingPermissions = Arc::new(Mutex::new(HashMap::new()));
        let client = Client.builder().on_receive_request(
            async move |request: RequestPermissionRequest, responder, _connection| {
                handle_permission_request(request, responder, ProviderKind::DeerFlow, RuntimeMode::FullAccess, &pending, &events)?;
                let DriverEvent::Permission { request_id, options, .. } = received.try_recv().unwrap() else {
                    panic!("missing permission");
                };
                let form = options[0].credential_form.as_ref().unwrap();
                assert!(complete_permission(&pending, &request_id, "allow_once").is_err());
                assert!(pending.lock().contains_key(&request_id));
                let mut sealed = crate::model::SealedCredential {
                    public_key: fixture_credential_public_key(2), client_key: fixture_credential_public_key(3),
                    nonce: "N".repeat(16), ciphertext: "B".repeat(28),
                };
                assert!(complete_credential(&pending, &request_id, sealed.clone()).is_err());
                sealed.public_key.clone_from(&form.public_key);
                for field in ["nonce", "ciphertext", "clientKey"] {
                    let mut invalid = sealed.clone();
                    match field {
                        "nonce" => invalid.nonce = "!".repeat(16),
                        "clientKey" => invalid.client_key = "A".repeat(124),
                        _ => invalid.ciphertext = "!".repeat(28),
                    }
                    assert!(complete_credential(&pending, &request_id, invalid).is_err());
                    assert!(pending.lock().contains_key(&request_id));
                }
                let mut short = sealed.clone();
                short.ciphertext = base64::engine::general_purpose::STANDARD.encode([1; 20]);
                assert!(complete_credential(&pending, &request_id, short).is_err());
                // Two attached clients seal against the same pending public form.
                let start = std::sync::Barrier::new(2);
                let outcomes = std::thread::scope(|race| {
                    let submit = || {
                        start.wait();
                        complete_credential(&pending, &request_id, sealed.clone()).is_ok()
                    };
                    let phone = race.spawn(submit);
                    let desktop = race.spawn(submit);
                    [phone.join().unwrap(), desktop.join().unwrap()]
                });
                assert_eq!(outcomes.iter().filter(|accepted| **accepted).count(), 1);
                assert!(complete_credential(&pending, &request_id, sealed).is_err());
                assert!(complete_permission(&pending, &request_id, "reject_once").is_err());
                Ok(())
            },
            agent_client_protocol::on_receive_request!(),
        );
        smol::block_on(smol::future::or(
            client.connect_with(agent, async move |connection| {
                connection.send_request(PromptRequest::new("s", vec![ContentBlock::Text(TextContent::new("use service"))])).block_task().await?;
                Ok(())
            }),
            async { smol::Timer::after(Duration::from_secs(10)).await; Err(approval_error("mobile credential response timed out")) },
        )).expect("encrypted response must reach the agent");
    }

    #[test]
    fn deerflow_full_access_never_answers_backend_permission_requests_for_the_user() {
        let agent = Agent.builder().on_receive_request(
            async move |_request: PromptRequest, responder: Responder<PromptResponse>, connection: ConnectionTo<Client>| {
                let permission: RequestPermissionRequest = serde_json::from_value(json!({
                    "sessionId":"s",
                    "toolCall":{"toolCallId":"write-1", "title":"write_file", "kind":"edit", "status":"pending"},
                    "options":[
                        {"optionId":"allow", "name":"Always allow", "kind":"allow_always"},
                        {"optionId":"reject", "name":"Reject", "kind":"reject_once"}
                    ]
                }))?;
                // Request callbacks run inside the SDK's incoming dispatch
                // loop. Yield it before waiting for the permission response,
                // otherwise that very response cannot be dispatched.
                connection.send_request(permission).on_receiving_result(async move |result| {
                    let outcome = result?;
                    assert_eq!(serde_json::to_value(outcome)?["outcome"]["optionId"], "reject");
                    responder.respond(PromptResponse::new(StopReason::EndTurn))
                })
            },
            agent_client_protocol::on_receive_request!(),
        );
        let (events, received) = crate::driver::test_event_channel();
        let pending: PendingPermissions = Arc::new(Mutex::new(HashMap::new()));
        let client = Client.builder().on_receive_request(
            async move |request: RequestPermissionRequest, responder, _connection| {
                handle_permission_request(
                    request,
                    responder,
                    ProviderKind::DeerFlow,
                    RuntimeMode::FullAccess,
                    &pending,
                    &events,
                )?;
                let DriverEvent::Permission { request_id, .. } = received
                    .try_recv()
                    .expect("DeerFlow must expose the permission request")
                else {
                    panic!("expected a real user permission request");
                };
                pending
                    .lock()
                    .remove(&request_id)
                    .expect("request awaits user response")
                    .respond(RequestPermissionResponse::new(
                        RequestPermissionOutcome::Selected(SelectedPermissionOutcome::new(
                            "reject",
                        )),
                    ))
            },
            agent_client_protocol::on_receive_request!(),
        );
        smol::block_on(smol::future::or(
            client.connect_with(agent, async move |connection| {
                connection
                    .send_request(PromptRequest::new(
                        "s",
                        vec![ContentBlock::Text(TextContent::new("write a file"))],
                    ))
                    .block_task()
                    .await?;
                Ok(())
            }),
            async {
                smol::Timer::after(Duration::from_secs(5)).await;
                Err(approval_error(
                    "SDK permission round trip timed out after 5 seconds",
                ))
            },
        ))
        .expect("DeerFlow permission must reach the user and return their decision");
    }

    #[test]
    fn deerflow_new_session_applies_default_but_resume_preserves_backend_policy() {
        use agent_client_protocol::schema::v1::SetSessionConfigOptionResponse;
        let calls = Arc::new(Mutex::new(Vec::new()));
        let captured = calls.clone();
        let agent = Agent.builder().on_receive_request(
            async move |request: SetSessionConfigOptionRequest,
                        responder: Responder<SetSessionConfigOptionResponse>,
                        _connection| {
                let value = serde_json::to_value(request)?;
                captured.lock().push(value.clone());
                responder.respond(SetSessionConfigOptionResponse::new(approval_options(
                    value["value"].as_str().unwrap(),
                )))
            },
            agent_client_protocol::on_receive_request!(),
        );
        smol::block_on(
            Client
                .builder()
                .connect_with(agent, async move |connection| {
                    let session = SessionId::new("s");
                    assert_eq!(
                        initialize_deerflow_approval(
                            &connection,
                            &session,
                            Some(&approval_options("ask")),
                            false,
                            RuntimeMode::FullAccess
                        )
                        .await?,
                        "allow_always"
                    );
                    for stored in [
                        "global",
                        "ask",
                        "allow_always",
                        "reject_always",
                        "read_only",
                    ] {
                        assert_eq!(
                            initialize_deerflow_approval(
                                &connection,
                                &session,
                                Some(&approval_options(stored)),
                                true,
                                RuntimeMode::FullAccess
                            )
                            .await?,
                            stored
                        );
                    }
                    assert_eq!(
                        initialize_deerflow_approval(
                            &connection,
                            &session,
                            None,
                            true,
                            RuntimeMode::Ask
                        )
                        .await?,
                        "off"
                    );
                    assert_eq!(
                        initialize_deerflow_approval(
                            &connection,
                            &session,
                            None,
                            false,
                            RuntimeMode::FullAccess
                        )
                        .await?,
                        "off"
                    );
                    assert!(
                        initialize_deerflow_approval(
                            &connection,
                            &session,
                            None,
                            false,
                            RuntimeMode::Ask
                        )
                        .await
                        .is_err()
                    );
                    Ok(())
                }),
        )
        .unwrap();
        let calls = calls.lock();
        assert_eq!(
            calls.len(),
            1,
            "restore and off policies must not be overwritten"
        );
        assert_eq!(calls[0]["configId"], "tool_approval");
        assert_eq!(calls[0]["value"], "allow_always");
    }

    #[test]
    fn deerflow_go_approval_uses_advertised_config_id() {
        use agent_client_protocol::schema::v1::SetSessionConfigOptionResponse;
        let calls = Arc::new(Mutex::new(Vec::new()));
        let captured = calls.clone();
        let agent = Agent.builder().on_receive_request(
            async move |request: SetSessionConfigOptionRequest,
                        responder: Responder<SetSessionConfigOptionResponse>,
                        _connection| {
                let value = serde_json::to_value(request)?;
                captured.lock().push(value.clone());
                responder.respond(SetSessionConfigOptionResponse::new(
                    approval_options_with_id("approval", value["value"].as_str().unwrap()),
                ))
            },
            agent_client_protocol::on_receive_request!(),
        );
        smol::block_on(
            Client
                .builder()
                .connect_with(agent, async move |connection| {
                    let session = SessionId::new("go-session");
                    assert_eq!(
                        initialize_deerflow_approval(
                            &connection,
                            &session,
                            Some(&approval_options_with_id("approval", "ask")),
                            false,
                            RuntimeMode::FullAccess,
                        )
                        .await?,
                        "allow_always"
                    );
                    assert_eq!(
                        deerflow_approval(Some(&approval_options_with_id(
                            "approval",
                            "read_only"
                        )))?,
                        "read_only"
                    );
                    Ok(())
                }),
        )
        .unwrap();
        let calls = calls.lock();
        assert_eq!(calls.len(), 1);
        assert_eq!(calls[0]["configId"], "approval");
        assert_eq!(calls[0]["value"], "allow_always");
    }

    #[test]
    fn deerflow_startup_requires_a_matching_backend_confirmation() {
        use agent_client_protocol::schema::v1::SetSessionConfigOptionResponse;
        for returned in ["ask", "unknown-policy", "__reject"] {
            let agent = Agent.builder().on_receive_request(
                async move |_request: SetSessionConfigOptionRequest,
                            responder: Responder<SetSessionConfigOptionResponse>,
                            _connection| {
                    if returned == "__reject" {
                        responder.respond_with_internal_error("configuration is locked")
                    } else {
                        responder.respond(SetSessionConfigOptionResponse::new(approval_options(
                            returned,
                        )))
                    }
                },
                agent_client_protocol::on_receive_request!(),
            );
            let result = smol::block_on(Client.builder().connect_with(
                agent,
                async move |connection| {
                    initialize_deerflow_approval(
                        &connection,
                        &SessionId::new("s"),
                        Some(&approval_options("ask")),
                        false,
                        RuntimeMode::FullAccess,
                    )
                    .await
                },
            ));
            assert!(
                result.is_err(),
                "a rejected or mismatched approval setting must stop startup"
            );
        }
    }

    #[test]
    fn deerflow_approval_late_reply_updates_authority_after_the_caller_leaves() {
        use agent_client_protocol::schema::v1::SetSessionConfigOptionResponse;
        let agent = Agent.builder().on_receive_request(
            async move |_request: SetSessionConfigOptionRequest,
                        responder: Responder<SetSessionConfigOptionResponse>,
                        _connection| {
                smol::Timer::after(Duration::from_millis(20)).await;
                responder.respond(SetSessionConfigOptionResponse::new(
                    approval_options_with_id("approval", "ask"),
                ))
            },
            agent_client_protocol::on_receive_request!(),
        );
        smol::block_on(
            Client
                .builder()
                .connect_with(agent, async move |connection| {
                    let access = Arc::new(Mutex::new(AcpAccessState::new(RuntimeMode::FullAccess)));
                    access.lock().confirm("allow_always");
                    let (events, received) = crate::driver::test_event_channel();
                    let (reply, result) = crossbeam_channel::bounded(1);
                    request_deerflow_approval(
                        &connection,
                        &SessionId::new("s"),
                        "approval",
                        "ask".into(),
                        &access,
                        &events,
                        reply,
                    )?;
                    assert!(!access.lock().ready);
                    assert!(access.lock().pending);
                    assert_eq!(access.lock().runtime_mode, RuntimeMode::FullAccess);
                    // The synchronous caller may time out before ACP settles. Its
                    // dropped receiver must not discard the server's eventual truth.
                    drop(result);
                    let event =
                        smol::unblock(move || received.recv_timeout(Duration::from_secs(3)))
                            .await
                            .unwrap();
                    assert!(
                        matches!(event, DriverEvent::ToolApprovalChanged(mode) if mode == "ask")
                    );
                    assert!(access.lock().ready);
                    assert!(!access.lock().pending);
                    assert_eq!(access.lock().runtime_mode, RuntimeMode::Ask);
                    Ok(())
                }),
        )
        .unwrap();
    }

    #[test]
    fn deerflow_failed_approval_keeps_old_display_mode_and_blocks_prompts() {
        use agent_client_protocol::schema::v1::SetSessionConfigOptionResponse;
        let agent = Agent.builder().on_receive_request(
            async move |_request: SetSessionConfigOptionRequest,
                        responder: Responder<SetSessionConfigOptionResponse>,
                        _connection| {
                responder.respond_with_internal_error("Session is busy")
            },
            agent_client_protocol::on_receive_request!(),
        );
        smol::block_on(
            Client
                .builder()
                .connect_with(agent, async move |connection| {
                    let access = Arc::new(Mutex::new(AcpAccessState::new(RuntimeMode::FullAccess)));
                    access.lock().confirm("allow_always");
                    let (events, _received) = crate::driver::test_event_channel();
                    let (reply, result) = crossbeam_channel::bounded(1);
                    request_deerflow_approval(
                        &connection,
                        &SessionId::new("s"),
                        "tool_approval",
                        "ask".into(),
                        &access,
                        &events,
                        reply,
                    )?;
                    let result = smol::unblock(move || result.recv_timeout(Duration::from_secs(3)))
                        .await
                        .unwrap();
                    assert!(result.unwrap_err().contains("busy"));
                    let state = access.lock();
                    assert_eq!(state.runtime_mode, RuntimeMode::FullAccess);
                    assert_eq!(state.tool_approval.as_deref(), Some("allow_always"));
                    assert!(!state.ready);
                    assert!(!state.pending);
                    Ok(())
                }),
        )
        .unwrap();
    }

    #[test]
    fn deerflow_model_error_stops_before_applying_thinking() {
        use agent_client_protocol::schema::v1::SetSessionConfigOptionResponse;
        let calls = Arc::new(Mutex::new(Vec::new()));
        let captured = calls.clone();
        let agent = Agent.builder().on_receive_request(
            async move |request: SetSessionConfigOptionRequest,
                        responder: Responder<SetSessionConfigOptionResponse>,
                        _connection| {
                captured.lock().push(request.config_id.to_string());
                responder.respond_with_internal_error("Unknown configured model")
            },
            agent_client_protocol::on_receive_request!(),
        );
        let result = smol::block_on(Client.builder().connect_with(
            agent,
            async move |connection| {
                apply_deerflow_model(
                    &connection,
                    &SessionId::new("s"),
                    None,
                    Some("missing"),
                    Some("on"),
                    None,
                )
                .await
            },
        ));
        assert!(
            result
                .unwrap_err()
                .to_string()
                .contains("Unknown configured model")
        );
        assert_eq!(*calls.lock(), ["model"]);
    }

    fn session_reasoning_options(model: &str, effort: &str) -> Vec<SessionConfigOption> {
        serde_json::from_value(json!([
            {"id":"model","name":"Model","type":"select","currentValue":model,"options":[
                {"value":"a","name":"A"},{"value":"b","name":"B"}
            ]},
            {"id":"effort-id","name":"Reasoning","category":"thought_level","type":"select","currentValue":effort,"options":[
                {"value":"default","name":"Default"},{"value":"low","name":"Low"},{"value":"high","name":"High"}
            ]}
        ])).unwrap()
    }

    #[test]
    fn deerflow_thinking_and_effort_are_independent_and_confirmed() {
        use agent_client_protocol::schema::v1::SetSessionConfigOptionResponse;
        fn options(effort: &str, mode: &str) -> Vec<SessionConfigOption> {
            let mut options = session_reasoning_options("a", effort);
            options.push(serde_json::from_value(json!({
                "id":"thinking_enabled", "name":"Thinking", "type":"select", "currentValue":mode,
                "options":[{"value":"default","name":"Default"},{"value":"on","name":"On"},{"value":"off","name":"Off"}]
            })).unwrap());
            options
        }
        let calls = Arc::new(Mutex::new(Vec::new()));
        let captured = calls.clone();
        let state = Arc::new(Mutex::new(("default".to_owned(), "default".to_owned())));
        let agent = Agent.builder().on_receive_request(
            async move |request: SetSessionConfigOptionRequest,
                        responder: Responder<SetSessionConfigOptionResponse>,
                        _connection| {
                let id = request.config_id.to_string();
                let value = request.value.as_value_id().unwrap().to_string();
                captured.lock().push((id.clone(), value.clone()));
                let mut state = state.lock();
                if id == "effort-id" {
                    state.0 = value;
                } else {
                    state.1 = value;
                }
                responder.respond(SetSessionConfigOptionResponse::new(options(
                    &state.0, &state.1,
                )))
            },
            agent_client_protocol::on_receive_request!(),
        );
        smol::block_on(Client.builder().connect_with(agent, async move |connection| {
            let mut current = options("default", "default");
            for mode in ["on", "off", "on", "default"] {
                current = apply_deerflow_model(&connection, &SessionId::new("s"), Some(&current), Some("a"), Some("high"), Some(mode)).await?;
                assert_eq!(deerflow_thinking_mode(&current).as_deref(), Some(mode));
                assert_eq!(session_config_current_value(deerflow_effort_option(&current).unwrap()), Some("high"));
                let (events, rx) = crossbeam_channel::unbounded();
                report_deerflow_models(&current, &events);
                assert!(matches!(rx.recv().unwrap(), DriverEvent::DeerFlowModelsChanged { thinking_mode, reasoning_effort, .. }
                    if thinking_mode.as_deref() == Some(mode) && reasoning_effort.as_deref() == Some("high")));
            }
            Ok(())
        })).unwrap();
        assert_eq!(
            *calls.lock(),
            [
                ("effort-id".into(), "high".into()),
                ("thinking_enabled".into(), "on".into()),
                ("thinking_enabled".into(), "off".into()),
                ("thinking_enabled".into(), "on".into()),
                ("thinking_enabled".into(), "default".into()),
            ]
        );
    }

    #[test]
    fn deerflow_clears_thinking_before_resetting_shared_effort() {
        use agent_client_protocol::schema::v1::SetSessionConfigOptionResponse;
        fn options(mode: &str, effort: &str) -> Vec<SessionConfigOption> {
            let mut options = session_reasoning_options("a", effort);
            options.push(serde_json::from_value(json!({
                "id":"thinking_enabled","name":"Thinking","type":"select","currentValue":mode,
                "options":[{"value":"default","name":"Default"},{"value":"on","name":"On"},{"value":"off","name":"Off"}]
            })).unwrap());
            options
        }
        let calls = Arc::new(Mutex::new(Vec::new()));
        let captured = calls.clone();
        let agent = Agent.builder().on_receive_request(
            async move |request: SetSessionConfigOptionRequest,
                        responder: Responder<SetSessionConfigOptionResponse>,
                        _connection| {
                let id = request.config_id.to_string();
                captured.lock().push(id.clone());
                responder.respond(SetSessionConfigOptionResponse::new(options(
                    "default",
                    if id == "thinking_enabled" {
                        "high"
                    } else {
                        "default"
                    },
                )))
            },
            agent_client_protocol::on_receive_request!(),
        );
        smol::block_on(
            Client
                .builder()
                .connect_with(agent, async move |connection| {
                    let result = apply_deerflow_model(
                        &connection,
                        &SessionId::new("s"),
                        Some(&options("on", "high")),
                        Some("a"),
                        Some("default"),
                        Some("default"),
                    )
                    .await?;
                    assert_eq!(deerflow_thinking_mode(&result).as_deref(), Some("default"));
                    Ok(())
                }),
        )
        .unwrap();
        assert_eq!(*calls.lock(), ["thinking_enabled", "effort-id"]);
    }

    #[test]
    fn deerflow_thinking_rejects_missing_or_unconfirmed_capability() {
        use agent_client_protocol::schema::v1::SetSessionConfigOptionResponse;
        let initial: Vec<SessionConfigOption> = serde_json::from_value(json!([
            {"id":"model","name":"Model","type":"select","currentValue":"a","options":[{"value":"a","name":"A"}]},
            {"id":"thinking_enabled","name":"Thinking","type":"select","currentValue":"on","options":[{"value":"on","name":"On"},{"value":"off","name":"Off"}]}
        ])).unwrap();
        let unchanged = initial.clone();
        let agent = Agent.builder().on_receive_request(
            async move |_request: SetSessionConfigOptionRequest,
                        responder: Responder<SetSessionConfigOptionResponse>,
                        _connection| {
                responder.respond(SetSessionConfigOptionResponse::new(unchanged.clone()))
            },
            agent_client_protocol::on_receive_request!(),
        );
        smol::block_on(
            Client
                .builder()
                .connect_with(agent, async move |connection| {
                    for options in [&initial[..], &initial[..1], &[]] {
                        let result = apply_deerflow_model(
                            &connection,
                            &SessionId::new("s"),
                            Some(options),
                            None,
                            None,
                            Some("off"),
                        )
                        .await;
                        assert!(result.is_err(), "unconfirmed mode was accepted");
                    }
                    Ok(())
                }),
        )
        .unwrap();
    }

    #[test]
    fn session_reasoning_selection_uses_advertised_id_and_confirms_value() {
        use agent_client_protocol::schema::v1::SetSessionConfigOptionResponse;
        let calls = Arc::new(Mutex::new(Vec::new()));
        let captured = calls.clone();
        let agent = Agent.builder().on_receive_request(
            async move |request: SetSessionConfigOptionRequest,
                        responder: Responder<SetSessionConfigOptionResponse>,
                        _connection| {
                let value = request.value.as_value_id().unwrap().to_string();
                captured
                    .lock()
                    .push((request.config_id.to_string(), value.clone()));
                let options = if request.config_id.to_string() == "model" {
                    session_reasoning_options(&value, "default")
                } else {
                    // A rejection by returning the old value must not look successful.
                    session_reasoning_options("b", if value == "low" { "high" } else { &value })
                };
                responder.respond(SetSessionConfigOptionResponse::new(options))
            },
            agent_client_protocol::on_receive_request!(),
        );
        smol::block_on(
            Client
                .builder()
                .connect_with(agent, async move |connection| {
                    let selected = apply_deerflow_model(
                        &connection,
                        &SessionId::new("s"),
                        Some(&session_reasoning_options("a", "default")),
                        Some("b"),
                        Some("high"),
                        None,
                    )
                    .await?;
                    assert_eq!(
                        session_config_current_value(deerflow_effort_option(&selected).unwrap()),
                        Some("high")
                    );
                    assert!(
                        apply_deerflow_model(
                            &connection,
                            &SessionId::new("s"),
                            Some(&selected),
                            Some("b"),
                            Some("xhigh"),
                            None
                        )
                        .await
                        .is_err()
                    );
                    assert!(
                        apply_deerflow_model(
                            &connection,
                            &SessionId::new("s"),
                            Some(&selected),
                            Some("b"),
                            Some("low"),
                            None
                        )
                        .await
                        .is_err()
                    );
                    let reset = apply_deerflow_model(
                        &connection,
                        &SessionId::new("s"),
                        Some(&selected),
                        Some("b"),
                        Some("default"),
                        None,
                    )
                    .await?;
                    assert_eq!(
                        session_config_current_value(deerflow_effort_option(&reset).unwrap()),
                        Some("default")
                    );
                    Ok(())
                }),
        )
        .unwrap();
        assert_eq!(
            *calls.lock(),
            [
                ("model".into(), "b".into()),
                ("effort-id".into(), "high".into()),
                ("effort-id".into(), "low".into()),
                ("effort-id".into(), "default".into())
            ]
        );
    }

    #[test]
    fn session_reasoning_updates_report_authoritative_selection() {
        let (events, rx) = crossbeam_channel::unbounded();
        let mut state = AcpStreamState::default();
        let options = session_reasoning_options("a", "high");
        for applying in [true, false] {
            state.deerflow_applying_model = applying;
            let update = serde_json::from_value(
                json!({"sessionUpdate":"config_option_update","configOptions":options}),
            )
            .unwrap();
            handle_session_update(
                ProviderKind::DeerFlow,
                SessionNotification::new("s", update),
                &events,
                &mut state,
            )
            .unwrap();
            if applying {
                assert!(rx.is_empty());
            }
        }
        assert!(
            matches!(rx.recv().unwrap(), DriverEvent::DeerFlowModelsChanged { model, reasoning_effort, .. }
            if model.as_deref() == Some("a") && reasoning_effort.as_deref() == Some("high"))
        );
        assert!(state.deerflow_config_options.is_some());
    }

    #[test]
    fn deerflow_skips_thinking_when_agent_does_not_advertise_it() {
        use agent_client_protocol::schema::v1::SetSessionConfigOptionResponse;
        let calls = Arc::new(Mutex::new(Vec::new()));
        let captured = calls.clone();
        let agent = Agent.builder().on_receive_request(
            async move |request: SetSessionConfigOptionRequest,
                        responder: Responder<SetSessionConfigOptionResponse>,
                        _connection| {
                captured.lock().push(request.config_id.to_string());
                responder.respond(SetSessionConfigOptionResponse::new(
                    approval_options_with_id("approval", "ask"),
                ))
            },
            agent_client_protocol::on_receive_request!(),
        );
        smol::block_on(
            Client
                .builder()
                .connect_with(agent, async move |connection| {
                    apply_deerflow_model(
                        &connection,
                        &SessionId::new("s"),
                        Some(&approval_options_with_id("approval", "ask")),
                        Some("smoke-local"),
                        Some("off"),
                        None,
                    )
                    .await
                }),
        )
        .unwrap();
        assert_eq!(*calls.lock(), ["model"]);
    }

    #[test]
    fn deerflow_legacy_thinking_hint_does_not_block_an_unchanged_model() {
        use agent_client_protocol::schema::v1::SetSessionConfigOptionResponse;
        let calls = Arc::new(Mutex::new(Vec::new()));
        let captured = calls.clone();
        let agent = Agent.builder().on_receive_request(
            async move |request: SetSessionConfigOptionRequest,
                        responder: Responder<SetSessionConfigOptionResponse>,
                        _connection| {
                captured.lock().push(request.config_id.to_string());
                responder.respond_with_internal_error("No configuration change expected")
            },
            agent_client_protocol::on_receive_request!(),
        );
        smol::block_on(
            Client
                .builder()
                .connect_with(agent, async move |connection| {
                    let options = session_reasoning_options("a", "default")[..1].to_vec();
                    for hint in ["on", "off"] {
                        for model in [None, Some("a")] {
                            apply_deerflow_model(
                                &connection,
                                &SessionId::new("s"),
                                Some(&options),
                                model,
                                Some(hint),
                                None,
                            )
                            .await?;
                            assert!(
                                apply_deerflow_model(
                                    &connection,
                                    &SessionId::new("s"),
                                    Some(&options),
                                    model,
                                    None,
                                    Some(hint),
                                )
                                .await
                                .is_err()
                            );
                        }
                    }
                    Ok(())
                }),
        )
        .unwrap();
        assert!(calls.lock().is_empty());
    }

    #[test]
    fn deerflow_thinking_uses_advertised_boolean_and_rejects_effort_levels() {
        let options: Vec<SessionConfigOption> = serde_json::from_value(json!([
            {"id":"thinking_enabled","name":"Thinking","type":"boolean","currentValue":true}
        ]))
        .unwrap();
        assert_eq!(
            deerflow_thinking_value(&options, "off").unwrap().as_bool(),
            Some(false)
        );
        assert!(deerflow_thinking_value(&options, "high").is_err());
        assert_eq!(
            deerflow_thinking_value(&[], "on")
                .unwrap()
                .as_value_id()
                .unwrap()
                .to_string(),
            "on"
        );
    }

    #[test]
    fn deerflow_plan_updates_keep_entries_and_completion() {
        let (events, event_rx) = crossbeam_channel::unbounded();
        let mut state = AcpStreamState::default();
        for status in ["in_progress", "completed"] {
            let update = serde_json::from_value(json!({"sessionUpdate":"plan", "entries":[
                {"content":"Read the input","status":"completed","priority":"high"},
                {"content":"Write the report","status":status,"priority":"medium"}
            ]}))
            .unwrap();
            handle_session_update(
                ProviderKind::DeerFlow,
                SessionNotification::new("s", update),
                &events,
                &mut state,
            )
            .unwrap();
        }
        let seen = event_rx.try_iter().collect::<Vec<_>>();
        assert!(matches!(&seen[0], DriverEvent::RichActivity(item)
            if !item.complete && item.kind == ActivityKind::Plan && item.output.as_deref().unwrap().contains("Write the report (in progress)")));
        assert!(matches!(&seen[1], DriverEvent::RichActivity(item)
            if item.complete && item.detail.as_deref() == Some("2/2 completed")));
        let DriverEvent::RichActivity(item) = &seen[0] else {
            panic!("missing plan");
        };
        let plan: Value = serde_json::from_str(item.arguments.as_deref().unwrap()).unwrap();
        assert_eq!(plan["entries"][1]["status"], "in_progress");
        assert_eq!(plan["entries"][0]["priority"], "high");
    }

    fn stored_image_attachment(
        root: &Path,
        name: &str,
        bytes: &[u8],
    ) -> crate::model::MessageAttachment {
        let directory = root.join("attachments");
        std::fs::create_dir_all(&directory).unwrap();
        let path = directory.join(name);
        std::fs::write(&path, bytes).unwrap();
        crate::model::MessageAttachment {
            path,
            mention: name.to_owned(),
            name: name.to_owned(),
            is_dir: false,
            is_image: true,
            blob_reference: Some(format!("waku-attachment:{name}")),
        }
    }

    #[test]
    fn deerflow_text_resources_are_explicit_bounded_utf8_snapshots() {
        let root = std::env::temp_dir().join(format!("deerflow-acp-text-{}", uuid::Uuid::new_v4()));
        let mut file =
            stored_image_attachment(&root, "notes #1%25.csv", "key,value\n你好,42\n".as_bytes());
        file.is_image = false;
        let content =
            deerflow_prompt_content("Read the CSV".into(), &[file.clone()], &root).unwrap();
        let block = serde_json::to_value(&content[1]).unwrap();
        assert_eq!(block["type"], "resource");
        assert_eq!(block["resource"]["text"], "key,value\n你好,42\n");
        assert_eq!(
            block["resource"]["uri"],
            "attachment:///notes%20%231%2525.csv"
        );
        assert_eq!(block["resource"]["mimeType"], "text/plain");
        assert_eq!(
            deerflow_prompt_content(format!("@{}", file.path.display()), &[], &root)
                .unwrap()
                .len(),
            1
        );
        let original = file.clone();
        file.blob_reference = None;
        assert!(deerflow_prompt_content(String::new(), &[file], &root).is_err());
        file = original.clone();
        file.path = root.join("outside.txt");
        std::fs::write(&file.path, "outside").unwrap();
        assert!(deerflow_prompt_content(String::new(), &[file], &root).is_err());
        for (name, data) in [
            ("bad.exe", b"MZ".as_slice()),
            ("bad.txt", b"\xff"),
            ("nul.txt", b"a\0b"),
        ] {
            let mut file = stored_image_attachment(&root, name, data);
            file.is_image = false;
            assert!(deerflow_prompt_content(String::new(), &[file], &root).is_err());
        }
        assert!(deerflow_prompt_content(String::new(), &vec![original.clone(); 9], &root).is_err());
        std::fs::write(&original.path, vec![b'x'; 1024 * 1024]).unwrap();
        assert!(deerflow_prompt_content(String::new(), &vec![original.clone(); 5], &root).is_err());
        std::fs::File::create(&original.path)
            .unwrap()
            .set_len(1024 * 1024 + 1)
            .unwrap();
        assert!(deerflow_prompt_content(String::new(), &[original], &root).is_err());
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn deerflow_document_resources_use_bounded_base64_blobs() {
        let root =
            std::env::temp_dir().join(format!("deerflow-acp-documents-{}", uuid::Uuid::new_v4()));
        for name in ["report.pdf", "report.docx", "report.xlsx"] {
            let mut file = stored_image_attachment(&root, name, b"fixture bytes");
            file.is_image = false;
            let blocks = deerflow_prompt_content(String::new(), &[file.clone()], &root).unwrap();
            let wire = serde_json::to_value(&blocks[1]).unwrap();
            assert!(wire["resource"]["text"].is_null());
            assert_eq!(
                base64::engine::general_purpose::STANDARD
                    .decode(wire["resource"]["blob"].as_str().unwrap())
                    .unwrap(),
                b"fixture bytes"
            );
            std::fs::File::create(&file.path)
                .unwrap()
                .set_len(8 * 1024 * 1024 + 1)
                .unwrap();
            assert!(deerflow_prompt_content(String::new(), &[file], &root).is_err());
        }
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn deerflow_sends_only_explicit_stored_images_as_acp_blocks() {
        let root =
            std::env::temp_dir().join(format!("deerflow-acp-images-{}", uuid::Uuid::new_v4()));
        let image = stored_image_attachment(&root, "sample.png", b"\x89PNG\r\n\x1a\nfixture");
        let content =
            deerflow_prompt_content("Compare this".into(), &[image.clone()], &root).unwrap();
        assert_eq!(content.len(), 2);
        let value = serde_json::to_value(&content[1]).unwrap();
        assert_eq!(value["type"], "image");
        assert_eq!(value["mimeType"], "image/png");
        assert_eq!(
            base64::engine::general_purpose::STANDARD
                .decode(value["data"].as_str().unwrap())
                .unwrap(),
            b"\x89PNG\r\n\x1a\nfixture"
        );
        let text_only =
            deerflow_prompt_content(format!("Read @{}", image.path.display()), &[], &root).unwrap();
        assert_eq!(text_only.len(), 1);
        let mut unsigned = image.clone();
        unsigned.blob_reference = None;
        assert!(deerflow_prompt_content(String::new(), &[unsigned], &root).is_err());
        assert!(deerflow_prompt_content(String::new(), &vec![image; 9], &root).is_err());
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn deerflow_sends_windows_clipboard_bmp_as_png() {
        // A 32-bit BI_RGB bitmap from Windows, with two rows in bottom-up order.
        let bmp = base64::engine::general_purpose::STANDARD
            .decode("Qk1GAAAAAAAAADYAAAAoAAAAAgAAAAIAAAABACAAAAAAAAAAAADEDgAAxA4AAAAAAAAAAAAA/wAA//////8AAP//AIAA/w==")
            .unwrap();
        let root =
            std::env::temp_dir().join(format!("deerflow-acp-bmp-{}", uuid::Uuid::new_v4()));
        let store = crate::blob_store::BlobStore::new(root.join("blobs"));
        let pasted = store.store_clipboard_image("image/bmp", &bmp).unwrap();
        let draft = store.store_image_bytes("image/bmp", &bmp).unwrap();
        assert!(pasted.ends_with(".png"));
        let png = std::fs::read(store.path_for(&pasted).unwrap()).unwrap();
        let rgb = image::load_from_memory_with_format(&png, image::ImageFormat::Png)
            .unwrap()
            .into_rgb8();
        assert_eq!(rgb.dimensions(), (2, 2));
        assert_eq!(
            rgb.into_raw(),
            [255, 0, 0, 0, 128, 0, 0, 0, 255, 255, 255, 255]
        );

        let attachments = [(pasted, "image.png"), (draft, "image.bmp")].map(|(reference, name)| {
            crate::model::MessageAttachment {
                path: store.path_for(&reference).unwrap(),
                mention: name.into(),
                name: name.into(),
                is_dir: false,
                is_image: true,
                blob_reference: Some(reference),
            }
        });
        let blocks = deerflow_prompt_content(String::new(), &attachments, &root).unwrap();
        assert_eq!(blocks.len(), 3);
        for block in &blocks[1..] {
            let ContentBlock::Image(image) = block else {
                panic!("expected an image block");
            };
            assert_eq!(image.mime_type, "image/png");
            assert_eq!(
                base64::engine::general_purpose::STANDARD
                    .decode(&image.data)
                    .unwrap(),
                png
            );
        }
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn deerflow_applies_image_budget_to_encoded_screenshot() {
        let root =
            std::env::temp_dir().join(format!("deerflow-acp-bmp-{}", uuid::Uuid::new_v4()));
        let mut bmp = std::io::Cursor::new(Vec::new());
        image::RgbImage::new(3840, 2160)
            .write_to(&mut bmp, image::ImageFormat::Bmp)
            .unwrap();
        assert!(bmp.get_ref().len() as u64 > MAX_DEERFLOW_IMAGE_BYTES);
        let attachment = stored_image_attachment(&root, "screenshot.bmp", bmp.get_ref());
        let blocks = deerflow_prompt_content(String::new(), &[attachment], &root).unwrap();
        assert!(matches!(&blocks[1], ContentBlock::Image(image) if image.mime_type == "image/png"));
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn deerflow_rejects_images_outside_store_and_invalid_or_oversized_data() {
        let root =
            std::env::temp_dir().join(format!("deerflow-acp-images-{}", uuid::Uuid::new_v4()));
        let mut image = stored_image_attachment(&root, "not-image.png", b"plain text");
        assert!(deerflow_prompt_content(String::new(), &[image.clone()], &root).is_err());
        let broken = stored_image_attachment(&root, "broken.bmp", b"BMtruncated");
        assert!(deerflow_prompt_content(String::new(), &[broken], &root).is_err());
        let outside = root.join("outside.png");
        std::fs::write(&outside, b"\x89PNG\r\n\x1a\n").unwrap();
        image.path = outside;
        assert!(deerflow_prompt_content(String::new(), &[image.clone()], &root).is_err());
        image.path = root.join("attachments").join("large.png");
        std::fs::File::create(&image.path)
            .unwrap()
            .set_len(MAX_DEERFLOW_IMAGE_BYTES + 1)
            .unwrap();
        assert!(deerflow_prompt_content(String::new(), &[image], &root).is_err());
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn a_prompt_after_transport_exit_fails_instead_of_hanging() {
        let (commands, command_rx) = smol::channel::unbounded();
        close_command_loop(&command_rx);
        let (events, event_rx) = crate::driver::test_event_channel();
        let driver = AcpDriver {
            commands,
            events,
            steering_availability: Arc::default(),
            provider: ProviderKind::DeerFlow,
            access: Arc::new(Mutex::new(AcpAccessState::new(RuntimeMode::Ask))),
        };
        driver.prompt_with_attachments("Do not lose this message".into(), Vec::new());
        let seen = event_rx.try_iter().collect::<Vec<_>>();
        assert!(
            matches!(&seen[0], DriverEvent::Error(message) if message.contains("before this message was sent"))
        );
        assert!(matches!(
            &seen[1],
            DriverEvent::TurnFinished { success: false, .. }
        ));
        assert!(matches!(&seen[2], DriverEvent::ProcessExited));
    }

    #[test]
    fn closing_transport_wakes_an_idle_command_loop() {
        let (commands, command_rx) = smol::channel::bounded(1);

        close_command_loop(&command_rx);

        assert!(smol::block_on(command_rx.recv()).is_err());
        assert!(commands.try_send(CommandMessage::Cancel).is_err());
    }

    fn select_config_option(
        id: &str,
        category: SessionConfigOptionCategory,
        current: &str,
        values: &[&str],
    ) -> SessionConfigOption {
        SessionConfigOption::select(
            id.to_owned(),
            id.to_owned(),
            current.to_owned(),
            values
                .iter()
                .map(|value| SessionConfigSelectOption::new((*value).to_owned(), *value))
                .collect::<Vec<_>>(),
        )
        .category(category)
    }

    #[test]
    fn a_steer_only_settles_when_the_last_sdk_request_finishes() {
        let requests = Mutex::new(PendingPrompts::default());
        requests
            .lock()
            .insert(RequestId::Str("first".into()), "session".into());
        requests
            .lock()
            .insert(RequestId::Str("steer".into()), "session".into());
        assert!(!settle_prompt_request(
            &requests,
            &RequestId::Str("first".into())
        ));
        assert!(settle_prompt_request(
            &requests,
            &RequestId::Str("steer".into())
        ));
        assert!(!settle_prompt_request(
            &requests,
            &RequestId::Str("steer".into())
        ));
    }

    fn a_recovered_provider_failure_overrides_a_clean_stop_reason() {
        let (events, event_rx) = crossbeam_channel::unbounded();

        assert!(!finish_prompt(
            ProviderKind::DeerFlow,
            Ok(PromptResponse::new(StopReason::EndTurn)),
            Some("402 membership inactive".to_owned()),
            &events
        ));

        assert!(matches!(
            event_rx.try_recv().unwrap(),
            DriverEvent::Error(message) if message == "402 membership inactive"
        ));
        assert!(matches!(
            event_rx.try_recv().unwrap(),
            DriverEvent::TurnFinished {
                success: false,
                summary: None
            }
        ));
    }

    #[test]
    fn legacy_receipt_error_has_no_manual_review_instructions() {
        let (events, event_rx) = crossbeam_channel::unbounded();
        let error =
            agent_client_protocol::Error::new(-32010, "Uncertain tool effects require review")
                .data(json!({
                    "kind": "reconciliation_required",
                    "automaticReplay": false,
                    "listMethod": "_deerflow/tool_receipts/list"
                }));
        assert!(!finish_prompt(
            ProviderKind::DeerFlow,
            Err(error),
            None,
            &events
        ));
        let DriverEvent::Error(message) = event_rx.try_recv().unwrap() else {
            panic!("expected recovery message");
        };
        assert_eq!(
            message,
            "上一次执行未能正常结束，已保留执行记录。系统不会自动重放旧操作。"
        );
        assert!(!message.contains("listMethod"));
        assert!(matches!(
            event_rx.try_recv().unwrap(),
            DriverEvent::TurnFinished { success: false, .. }
        ));
        assert!(event_rx.try_recv().is_err());
    }

    #[test]
    fn waiting_execution_errors_explain_recovery_without_raw_metadata() {
        for error in [
            agent_client_protocol::Error::new(-32012, "The session has a waiting execution"),
            agent_client_protocol::Error::internal_error().data(json!({
                "kind": "execution_waiting_input",
                "automaticReplay": false,
                "getMethod": "_deerflow/executions/get",
                "resumeMethod": "_deerflow/executions/resume",
                "cancelMethod": "_deerflow/executions/cancel"
            })),
        ] {
            let (events, event_rx) = crossbeam_channel::unbounded();
            assert!(!finish_prompt(
                ProviderKind::DeerFlow,
                Err(error),
                None,
                &events
            ));
            assert!(matches!(
                event_rx.try_recv().unwrap(),
                DriverEvent::Error(message) if message == "上一轮执行仍在等待处理。可在此对话继续或结束上一轮。"
            ));
            assert!(matches!(
                event_rx.try_recv().unwrap(),
                DriverEvent::TurnFinished { success: false, .. }
            ));
            assert!(event_rx.try_recv().is_err());
        }
    }

    #[test]
    fn pinned_search_error_only_reports_a_stopped_turn_after_confirmed_failure() {
        for status in [None, Some("waiting_input"), Some("failed")] {
            let error = agent_client_protocol::Error::new(-32014, "private-snapshot").data(json!({
                "kind": "pinned_search_config_invalid",
                "execution": { "status": status, "blockedReason": "fixture-secret" }
            }));
            let message = deerflow_failure_message(&error).unwrap();
            assert!(message.contains("搜索配置"));
            assert_eq!(message.contains("本轮已停止"), status == Some("failed"));
            assert!(!message.contains("fixture-secret") && !message.contains("private-snapshot"));
        }
    }

    #[test]
    fn checkpoint_errors_show_safe_messages_without_execution_metadata() {
        for (code, kind, expected) in [
            (
                -32028,
                "checkpoint_too_large",
                "执行状态过大，无法保存本轮进度。请减少并行子任务后发送新消息继续；已保存的工具结果会保留。",
            ),
            (
                -32029,
                "checkpoint_invalid",
                "执行状态校验失败，暂时无法恢复。本轮已停止，已保存的记录会保留；请发送新消息继续。",
            ),
            (
                -32030,
                "checkpoint_persist_failed",
                "执行进度保存失败，本轮已停止。请检查本机存储后重试；已保存的记录会保留。",
            ),
        ] {
            for error in [
                agent_client_protocol::Error::new(code, "private-checkpoint"),
                agent_client_protocol::Error::internal_error().data(json!({
                    "kind": kind,
                    "execution": {"runId": "run", "resumable": false},
                    "automaticReplay": false,
                    "privateDetail": "fixture-secret"
                })),
            ] {
                let (events, event_rx) = crossbeam_channel::unbounded();
                assert!(!finish_prompt(
                    ProviderKind::DeerFlow,
                    Err(error),
                    None,
                    &events
                ));
                let DriverEvent::Error(message) = event_rx.try_recv().unwrap() else {
                    panic!("expected checkpoint message");
                };
                assert_eq!(message, expected);
                assert!(matches!(
                    event_rx.try_recv().unwrap(),
                    DriverEvent::TurnFinished { success: false, .. }
                ));
                assert!(event_rx.try_recv().is_err());
            }
        }
    }

    #[test]
    fn malformed_model_stream_errors_hide_execution_metadata() {
        for error in [
            agent_client_protocol::Error::new(-32026, "upstream private detail"),
            agent_client_protocol::Error::internal_error().data(json!({
                "kind": "model_response_invalid",
                "automaticReplay": false,
                "execution": {"runId": "run", "resumable": false},
                "privateDetail": "fixture-secret"
            })),
        ] {
            let (events, event_rx) = crossbeam_channel::unbounded();
            assert!(!finish_prompt(
                ProviderKind::DeerFlow,
                Err(error),
                None,
                &events
            ));
            let DriverEvent::Error(message) = event_rx.try_recv().unwrap() else {
                panic!("expected stream failure message");
            };
            assert_eq!(
                message,
                "模型流式响应解析失败，本轮已停止。已完成的工具结果已保留，请发送新消息继续。"
            );
            assert!(matches!(
                event_rx.try_recv().unwrap(),
                DriverEvent::TurnFinished { success: false, .. }
            ));
            assert!(event_rx.try_recv().is_err());
        }
    }

    #[test]
    fn rate_limited_prompt_errors_show_safe_message_without_execution_metadata() {
        for error in [
            agent_client_protocol::Error::new(-32027, "provider detail"),
            agent_client_protocol::Error::internal_error().data(json!({
                "kind": "model_rate_limited",
                "automaticReplay": false,
                "execution": {"runId": "run", "resumable": false},
                "providerDetail": "must not appear in the transcript"
            })),
        ] {
            let (events, event_rx) = crossbeam_channel::unbounded();
            assert!(!finish_prompt(
                ProviderKind::DeerFlow,
                Err(error),
                None,
                &events
            ));
            let DriverEvent::Error(message) = event_rx.try_recv().unwrap() else {
                panic!("expected rate limit message");
            };
            assert_eq!(
                message,
                "模型服务请求受到限流，本轮已停止。请稍后重试或切换模型；已完成的工具结果已保留，请检查结果后再继续。"
            );
            assert!(matches!(
                event_rx.try_recv().unwrap(),
                DriverEvent::TurnFinished { success: false, .. }
            ));
            assert!(event_rx.try_recv().is_err());
        }
    }

    #[test]
    fn transport_errors_show_safe_continuation_without_execution_metadata() {
        for error in [
            agent_client_protocol::Error::new(-32033, "private proxy detail"),
            agent_client_protocol::Error::internal_error().data(json!({
                "kind": "model_transport_failed",
                "automaticReplay": false,
                "execution": {"runId": "run", "resumable": false},
                "detail": "private proxy credential"
            })),
        ] {
            let (events, event_rx) = crossbeam_channel::unbounded();
            assert!(!finish_prompt(
                ProviderKind::DeerFlow,
                Err(error),
                None,
                &events
            ));
            let DriverEvent::Error(message) = event_rx.try_recv().unwrap() else {
                panic!("expected transport failure");
            };
            assert!(message.contains("本地代理连接中断"));
            assert!(message.contains("原会话"));
            assert!(!message.contains("private"));
            assert!(!message.contains("resumable"));
            assert!(matches!(
                event_rx.try_recv().unwrap(),
                DriverEvent::TurnFinished { success: false, .. }
            ));
            assert!(event_rx.try_recv().is_err());
        }
    }

    #[test]
    fn rate_limit_text_alone_does_not_reclassify_other_prompt_errors() {
        let (events, event_rx) = crossbeam_channel::unbounded();
        let error = agent_client_protocol::Error::internal_error().data(json!({
            "kind": "different_failure",
            "detail": "model_rate_limited"
        }));
        let expected = error.to_string();
        assert!(!finish_prompt(
            ProviderKind::DeerFlow,
            Err(error),
            None,
            &events
        ));
        assert!(matches!(
            event_rx.try_recv().unwrap(),
            DriverEvent::Error(message) if message == expected
        ));
        assert!(matches!(
            event_rx.try_recv().unwrap(),
            DriverEvent::TurnFinished { success: false, .. }
        ));
        assert!(event_rx.try_recv().is_err());
    }

    #[test]
    fn token_limits_do_not_claim_context_exhaustion() {
        for (limit, expected) in [
            (Some("output_tokens"), tr!("session.model_output_limit")),
            (Some("tokens"), tr!("session.run_token_budget_reached")),
            (None, tr!("session.token_limit_reached")),
        ] {
            let (events, event_rx) = crossbeam_channel::unbounded();
            let response: PromptResponse = serde_json::from_value(json!({
                "stopReason":"max_tokens", "_meta":{"deerflow":{"limit":limit}}
            }))
            .unwrap();
            assert!(!finish_prompt(
                ProviderKind::DeerFlow,
                Ok(response),
                None,
                &events
            ));
            assert!(matches!(event_rx.try_recv().unwrap(),
                DriverEvent::TurnFinished {success: false, summary: Some(summary)} if summary == expected
            ));
            assert!(event_rx.try_recv().is_err());
        }
    }

    #[test]
    fn typed_prompt_response_settles_the_turn() {
        let (events, event_rx) = crossbeam_channel::unbounded();
        assert!(finish_prompt(
            ProviderKind::DeerFlow,
            Ok(PromptResponse::new(StopReason::EndTurn)),
            None,
            &events
        ));
        assert!(matches!(
            event_rx.try_recv().unwrap(),
            DriverEvent::TurnFinished {
                success: true,
                summary: None
            }
        ));
    }

    #[test]
    fn typed_updates_preserve_text_reasoning_and_correlated_tools() {
        let (events, event_rx) = crossbeam_channel::unbounded();
        let mut state = AcpStreamState::default();
        let updates = [
            json!({"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking"}}),
            json!({"sessionUpdate":"tool_call","toolCallId":"call_1","title":"read","kind":"read","status":"pending","rawInput":{}}),
            json!({"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed","title":"fixture.txt","content":[{"type":"content","content":{"type":"text","text":"waku probe fixture"}}]}),
            json!({"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"OK"}}),
            json!({"sessionUpdate":"usage_update","used":9677,"size":500000}),
        ];
        for update in updates {
            let update = serde_json::from_value(update).unwrap();
            handle_session_update(
                ProviderKind::Cursor,
                SessionNotification::new("s", update),
                &events,
                &mut state,
            )
            .unwrap();
        }

        let seen = event_rx.try_iter().collect::<Vec<_>>();
        assert!(matches!(&seen[0], DriverEvent::ReasoningDelta(text) if text == "thinking"));
        assert!(matches!(&seen[1], DriverEvent::RichActivity(item)
                if item.kind == ActivityKind::FileRead && !item.complete));
        assert!(matches!(&seen[2], DriverEvent::RichActivity(item)
                if item.complete
                    && item.title == "fixture.txt"
                    && item.output.as_deref().is_some_and(|output| output.contains("waku probe fixture"))));
        assert!(matches!(&seen[3], DriverEvent::TextDelta(text) if text == "OK"));
        assert!(matches!(
            &seen[4],
            DriverEvent::UsageUpdated {
                context_tokens: Some(9677),
                context_window: Some(500000),
            }
        ));
    }

    #[test]
    fn acp_image_chunks_reach_the_desktop_as_ordered_message_content() {
        let root =
            std::env::temp_dir().join(format!("waku-acp-live-image-{}", uuid::Uuid::new_v4()));
        let blobs = Arc::new(crate::blob_store::BlobStore::new(root.clone()));
        let (events, received) = crossbeam_channel::unbounded();
        let mut state = AcpStreamState {
            image_store: Some(blobs.clone()),
            ..AcpStreamState::default()
        };
        let bytes = b"\x89PNG\r\n\x1a\nfixture";
        let content = [
            json!({"type":"text","text":"before"}),
            json!({"type":"image","mimeType":"image/png","data":base64::engine::general_purpose::STANDARD.encode(bytes)}),
            json!({"type":"text","text":"after"}),
        ];
        for content in content {
            let notification: SessionNotification = serde_json::from_value(json!({
                "sessionId":"s", "update":{"sessionUpdate":"agent_message_chunk","content":content}
            }))
            .unwrap();
            handle_session_update(ProviderKind::DeerFlow, notification, &events, &mut state)
                .unwrap();
        }
        let seen = received.try_iter().collect::<Vec<_>>();
        assert_eq!(seen.len(), 3);
        assert!(matches!(&seen[0], DriverEvent::TextDelta(text) if text == "before"));
        assert!(matches!(&seen[2], DriverEvent::TextDelta(text) if text == "after"));
        let DriverEvent::TextDelta(image) = &seen[1] else {
            panic!("image chunk was discarded")
        };
        let reference = waku_protocol::blob::references(image).next().unwrap();
        assert_eq!(
            std::fs::read(blobs.path_for(reference).unwrap()).unwrap(),
            bytes
        );
        assert!(state.produced_content);
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn deerflow_acp_config_models_are_parsed_correctly() {
        let option = select_config_option(
            "model",
            SessionConfigOptionCategory::Model,
            "deepseek-v4-flash",
            &[
                "deepseek-v4-flash",
                "gemini-3.6-flash-high",
                "claude-sonnet-4-6",
            ],
        );
        let models = parse_acp_config_models(&option);
        assert_eq!(models.len(), 3);
        assert_eq!(models[0].id, "deepseek-v4-flash");
        assert!(models[0].is_default);
        assert_eq!(models[1].id, "gemini-3.6-flash-high");
        assert!(!models[1].is_default);
        assert_eq!(models[2].id, "claude-sonnet-4-6");
        assert!(!models[2].is_default);
    }

    #[test]
    fn permission_reason_preserves_the_agents_explanation() {
        let tool_call = ToolCallUpdate::new(
            "tool-1",
            serde_json::from_value::<ToolCallUpdateFields>(json!({
                "title": "rm -rf build",
                "kind": "execute",
                "content": [
                    {"type":"content","content":{"type":"text","text":"Not in allowlist: rm"}}
                ]
            }))
            .unwrap(),
        );
        let request = RequestPermissionRequest::new("s", tool_call, Vec::new());
        let params = serde_json::to_value(request).unwrap();
        assert_eq!(
            permission_reason(&params).as_deref(),
            Some("Not in allowlist: rm")
        );
    }

}
