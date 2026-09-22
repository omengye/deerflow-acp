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
    InitializeResponse, LoadSessionRequest, NewSessionRequest, PermissionOptionKind, PromptRequest,
    PromptResponse, RequestId, RequestPermissionOutcome, RequestPermissionRequest,
    RequestPermissionResponse, ResumeSessionRequest, SelectedPermissionOutcome, SessionConfigKind,
    SessionConfigOption, SessionConfigOptionCategory, SessionConfigOptionValue,
    SessionConfigSelectOptions, SessionId, SessionModeId, SessionModeState, SessionNotification,
    SetSessionConfigOptionRequest, SetSessionModeRequest, StopReason, TextContent,
};
use agent_client_protocol::{
    AcpAgent, AcpAgentConfig, Agent, Client, ConnectionTo, Handled, LineDirection, Responder,
    UntypedMessage,
};
use anyhow::{Context as _, anyhow};
use base64::Engine as _;
use parking_lot::Mutex;
use serde_json::{Map, Value, json};

use super::activity;
use crate::driver::{
    DriverControl, DriverEventSender, DriverEventSink, DriverStartOptions, SessionOptions,
};
use crate::model::{
    ActivityKind, DriverEvent, PermissionOption, ProviderKind, ProviderModel, ProviderResumeCursor,
    RuntimeMode, UserInputAnswer, UserInputOption, UserInputQuestion,
};
use waku_protocol::model_catalog::{
    CursorModelSelection, cursor_suffix_has, normalize_cursor_reasoning_effort,
    resolve_cursor_model,
};

enum CommandMessage {
    Prompt(String, Vec<crate::model::MessageAttachment>),
    Steer(String),
    Cancel,
    Respond {
        request_id: String,
        option_id: String,
    },
    RespondUserInput {
        request_id: String,
        answers: Vec<UserInputAnswer>,
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
    supports_steer: bool,
    provider: ProviderKind,
    access: Arc<Mutex<AcpAccessState>>,
    computer_use: Option<super::support::HeadlessComputerUseRuntime>,
}

/// Per-provider launch details. Everything after process launch is ACP.
struct AcpLaunch {
    args: Vec<String>,
    env: Vec<(String, String)>,
}

fn launch_for(provider: ProviderKind, reasoning_effort: Option<&str>) -> anyhow::Result<AcpLaunch> {
    match provider {
        ProviderKind::Cursor => Ok(AcpLaunch {
            args: vec!["acp".into()],
            env: Vec::new(),
        }),
        ProviderKind::Grok => {
            let mut args = vec!["agent".into()];
            if let Some(effort) = reasoning_effort.filter(|effort| !effort.is_empty()) {
                args.push("--reasoning-effort".into());
                args.push(effort.to_owned());
            }
            args.push("stdio".into());
            Ok(AcpLaunch {
                args,
                env: vec![("GROK_OAUTH2_REFERRER".into(), "waku".into())],
            })
        }
        ProviderKind::Fx => Ok(AcpLaunch {
            args: vec!["acp".into()],
            env: Vec::new(),
        }),
        ProviderKind::Kimi => Ok(AcpLaunch {
            args: vec!["acp".into()],
            env: Vec::new(),
        }),
        ProviderKind::OpenCode => Ok(AcpLaunch {
            args: vec!["acp".into()],
            env: Vec::new(),
        }),
        ProviderKind::DeerFlow => Ok(AcpLaunch {
            args: crate::deerflow_config::launch_arguments()?,
            env: crate::deerflow_config::launch_environment()?,
        }),
        _ => Err(anyhow!(
            "{} does not speak the Agent Client Protocol",
            provider.display_name()
        )),
    }
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
            service_tier,
            context_window,
            agent_preset: _,
            computer_use_enabled,
            provider_cursor,
        } = options;
        let fork_context = match &provider_cursor {
            Some(ProviderResumeCursor::Cursor { fork_context, .. }) => fork_context.clone(),
            _ => None,
        };
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

        let launch = launch_for(provider, reasoning_effort.as_deref())?;
        let computer_use = (provider == ProviderKind::Grok && computer_use_enabled)
            .then(|| super::support::HeadlessComputerUseRuntime::start(provider, events.clone()))
            .transpose()?;
        let grok_title_home = computer_use
            .as_ref()
            .and_then(super::support::HeadlessComputerUseRuntime::grok_home)
            .map(ToOwned::to_owned);
        let stderr_lines = Arc::new(Mutex::new(Vec::<String>::new()));
        let agent = sdk_agent(
            &binary,
            &cwd,
            launch,
            computer_use.as_ref().map(|runtime| &runtime.config),
            stderr_lines.clone(),
        )?;
        let (commands, command_rx) = smol::channel::unbounded();
        let access = Arc::new(Mutex::new(AcpAccessState::new(mode)));
        let connection_access = access.clone();
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
                    model,
                    reasoning_effort,
                    service_tier,
                    context_window,
                    resume_session_id,
                    fork_context,
                    grok_title_home,
                    command_rx,
                    thread_events.clone(),
                ));
                if let Err(error) = result {
                    let stderr = super::support::provider_stderr_error(stderr_lines.lock().clone());
                    let detail = if provider == ProviderKind::DeerFlow {
                        match stderr {
                            Some(stderr) => format!("{error}\n{stderr}"),
                            None => error.to_string(),
                        }
                    } else {
                        stderr.unwrap_or_else(|| error.to_string())
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
            supports_steer: !matches!(provider, ProviderKind::Fx | ProviderKind::DeerFlow),
            provider,
            access,
            computer_use,
        })
    }
}

fn sdk_agent(
    binary: &Path,
    _cwd: &Path,
    mut launch: AcpLaunch,
    computer_use: Option<&super::support::HeadlessComputerUseConfig>,
    stderr_lines: Arc<Mutex<Vec<String>>>,
) -> anyhow::Result<AcpAgent> {
    let binary = binary
        .to_str()
        .ok_or_else(|| anyhow!("the ACP executable path is not valid UTF-8"))?;
    #[cfg(unix)]
    let cwd = _cwd
        .to_str()
        .ok_or_else(|| anyhow!("the ACP working directory is not valid UTF-8"))?;
    let (computer_args, computer_env) =
        super::support::grok_computer_use_launch_configuration(computer_use);
    launch.args.extend(computer_args);
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
    environment.extend(computer_env);

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
    let mut launch = launch_for(provider, None)?;
    if provider == ProviderKind::DeerFlow && !auto_start {
        launch.args.push("--no-auto-start".into());
    }
    sdk_agent(binary, cwd, launch, None, Arc::new(Mutex::new(Vec::new())))
}

type PermissionResponder = Responder<RequestPermissionResponse>;
type PendingPermissions = Arc<Mutex<HashMap<String, PermissionResponder>>>;

#[derive(Clone, Copy)]
enum AcpUserInputKind {
    Cursor,
    Xai,
}

struct PendingAcpUserInput {
    kind: AcpUserInputKind,
    params: Value,
    responder: Responder<Value>,
}

type PendingAcpUserInputs = Arc<Mutex<HashMap<String, PendingAcpUserInput>>>;

#[derive(Default)]
struct PendingPrompts(Vec<PendingPrompt>);

struct PendingPrompt {
    request_id: RequestId,
    extension_id: Option<String>,
    session_id: String,
}

impl PendingPrompts {
    fn insert(&mut self, request_id: RequestId, extension_id: Option<String>, session_id: String) {
        self.0.push(PendingPrompt {
            request_id,
            extension_id,
            session_id,
        });
    }

    fn is_empty(&self) -> bool {
        self.0.is_empty()
    }

    fn settle_request(&mut self, request_id: &RequestId) -> bool {
        let Some(index) = self
            .0
            .iter()
            .position(|prompt| &prompt.request_id == request_id)
        else {
            return false;
        };
        self.0.remove(index);
        self.0.is_empty()
    }

    fn settle_extension(&mut self, session_id: &str, extension_id: Option<&str>) -> bool {
        let Some(index) = self.0.iter().position(|prompt| {
            prompt.session_id == session_id
                && extension_id
                    .is_none_or(|extension_id| prompt.extension_id.as_deref() == Some(extension_id))
        }) else {
            return false;
        };
        self.0.remove(index);
        self.0.is_empty()
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
    model: Option<String>,
    reasoning_effort: Option<String>,
    service_tier: Option<String>,
    context_window: Option<String>,
    resume_session_id: Option<String>,
    fork_context: Option<String>,
    grok_title_home: Option<std::path::PathBuf>,
    commands: smol::channel::Receiver<CommandMessage>,
    events: DriverEventSender,
) -> agent_client_protocol::Result<()> {
    let suppress_session_updates = Arc::new(AtomicBool::new(false));
    let stream_state = Arc::new(Mutex::new(AcpStreamState::default()));
    let pending_permissions: PendingPermissions = Arc::new(Mutex::new(HashMap::new()));
    let pending_user_inputs: PendingAcpUserInputs = Arc::new(Mutex::new(HashMap::new()));
    let prompt_requests = Arc::new(Mutex::new(PendingPrompts::default()));
    let title_refresh = super::title_refresh::NativeTitleRefresh::default();
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
                async move |notification: SessionNotification, _connection| {
                    if !suppress_session_updates.load(Ordering::Acquire) {
                        handle_session_update(
                            provider,
                            notification,
                            &events,
                            &mut stream_state.lock(),
                        )?;
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
                let grok_title_home = grok_title_home.clone();
                let title_refresh = title_refresh.clone();
                async move |notification: UntypedMessage, _connection| {
                    if notification.method() == "_x.ai/session/prompt_complete" {
                        if let Some(session_id) = finish_xai_prompt_complete(
                            notification.params(),
                            &prompt_requests,
                            &events,
                        ) {
                            start_grok_title_refresh(
                                grok_title_home.as_deref(),
                                &session_id,
                                &title_refresh,
                                events.clone(),
                            );
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
                async move |request: RequestPermissionRequest, responder, _connection| {
                    handle_permission_request(
                        request,
                        responder,
                        provider,
                        mode,
                        &pending_permissions,
                        &events,
                    )
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let events = events.clone();
                let pending = pending_user_inputs.clone();
                async move |request: UntypedMessage, responder, _connection| {
                    let kind = match request.method() {
                        "cursor/ask_question" => AcpUserInputKind::Cursor,
                        "_x.ai/ask_user_question" | "x.ai/ask_user_question" => {
                            AcpUserInputKind::Xai
                        }
                        _ => {
                            return Ok(Handled::No {
                                message: (request, responder),
                                retry: false,
                            });
                        }
                    };
                    let request_id = responder.id().to_string();
                    let params = match kind {
                        AcpUserInputKind::Cursor => request.params().clone(),
                        AcpUserInputKind::Xai => {
                            unwrap_xai_question_params(request.params()).clone()
                        }
                    };
                    let questions = match kind {
                        AcpUserInputKind::Cursor => cursor_user_input_questions(&params),
                        AcpUserInputKind::Xai => xai_user_input_questions(&params),
                    };
                    if questions.is_empty() {
                        responder.respond(cancelled_user_input_response(kind))?;
                        return Ok(Handled::Yes);
                    }
                    pending.lock().insert(
                        request_id.clone(),
                        PendingAcpUserInput {
                            kind,
                            params,
                            responder,
                        },
                    );
                    if events
                        .send(DriverEvent::UserInputRequested {
                            request_id: request_id.clone(),
                            questions,
                        })
                        .is_err()
                        && let Some(pending) = pending.lock().remove(&request_id)
                    {
                        let _ = pending
                            .responder
                            .respond(cancelled_user_input_response(pending.kind));
                    }
                    Ok(Handled::Yes)
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .connect_with(agent, async move |connection: ConnectionTo<Agent>| {
            let mut client_capabilities = ClientCapabilities::new().terminal(false);
            if provider == ProviderKind::Cursor {
                // Cursor only exposes its parameterized model controls to
                // clients that opt in. Waku applies the returned config option
                // ids rather than assuming Cursor's private ids stay stable.
                let mut meta = Map::new();
                meta.insert("parameterizedModelPicker".to_owned(), Value::Bool(true));
                client_capabilities = client_capabilities.meta(meta);
            }
            let initialize = connection
                .send_request(
                    InitializeRequest::new(ProtocolVersion::V1)
                        .client_capabilities(client_capabilities)
                        .client_info(Implementation::new("waku", env!("CARGO_PKG_VERSION"))),
                )
                .block_task()
                .await?;
            let (session_id, modes, config_options) = establish_session(
                &connection,
                &initialize,
                resume_session_id.as_deref(),
                &cwd,
                &suppress_session_updates,
            )
            .await?;

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

            if provider == ProviderKind::DeerFlow
                && let Some(options) = config_options.as_deref()
            {
                let models = crate::model_catalog::parse_deerflow_models(options);
                if !models.is_empty() {
                    crate::model_catalog::write_cached_models(provider, &models);
                }
            }

            if let Some(mode_id) = desired_access_mode(provider, modes.as_ref(), mode) {
                // Mode selection is opportunistic: an agent can advertise a
                // mode but reject a later transition without invalidating the
                // session itself.
                let _ = connection
                    .send_request(SetSessionModeRequest::new(session_id.clone(), mode_id))
                    .block_task()
                    .await;
            }
            let native_session_id = session_id.to_string();
            let _ = events.send(DriverEvent::Connected {
                provider_cursor: Some(ProviderResumeCursor::from_session_id(
                    provider,
                    native_session_id.clone(),
                )),
            });
            if let Some(approval) = approval {
                access.lock().confirm(&approval);
                let _ = events.send(DriverEvent::ToolApprovalChanged(approval));
            }

            let mut current_model = model;
            let mut current_effort = reasoning_effort;
            let mut current_tier = service_tier;
            let mut current_window = context_window;
            let mut model_options_ready = apply_model(
                &connection,
                &session_id,
                provider,
                config_options.as_deref(),
                current_model.as_deref(),
                current_effort.as_deref(),
                current_tier.as_deref(),
                current_window.as_deref(),
                &events,
            )
            .await;
            let mut fork_context = fork_context;

            while let Ok(command) = commands.recv().await {
                match command {
                    CommandMessage::Prompt(text, attachments) => {
                        if provider == ProviderKind::DeerFlow && !access.lock().ready {
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
                        let text = fork_context
                            .take()
                            .map(|context| {
                                crate::cursor_session::prompt_with_fork_context(&context, &text)
                            })
                            .unwrap_or(text);
                        let _ = events.send(DriverEvent::TurnStarted);
                        if let Err(error) = send_prompt(
                            &connection,
                            &session_id,
                            text,
                            &attachments,
                            &prompt_requests,
                            &events,
                            provider,
                            &native_session_id,
                            grok_title_home.clone(),
                            title_refresh.clone(),
                            stream_state.clone(),
                        ) {
                            let _ = events.send(DriverEvent::Error(error.to_string()));
                            let _ = events.send(DriverEvent::TurnFinished {
                                success: false,
                                summary: None,
                            });
                        }
                    }
                    CommandMessage::Steer(text) => {
                        if !model_options_ready {
                            let _ = events.send(DriverEvent::SteerRejected {
                                message: text,
                                reason: "DeerFlow model settings have not been applied. Correct the options and retry.".into(),
                            });
                            continue;
                        }
                        if prompt_requests.lock().is_empty() {
                            let _ = events.send(DriverEvent::SteerRejected {
                                message: text,
                                reason: format!(
                                    "{} has no active turn to steer.",
                                    provider.display_name()
                                ),
                            });
                            continue;
                        }
                        match send_prompt(
                            &connection,
                            &session_id,
                            text.clone(),
                            &[],
                            &prompt_requests,
                            &events,
                            provider,
                            &native_session_id,
                            grok_title_home.clone(),
                            title_refresh.clone(),
                            stream_state.clone(),
                        ) {
                            Ok(()) => {
                                let _ = events.send(DriverEvent::SteerAccepted { message: text });
                            }
                            Err(error) => {
                                let _ = events.send(DriverEvent::SteerRejected {
                                    message: text,
                                    reason: error.to_string(),
                                });
                            }
                        }
                    }
                    CommandMessage::Cancel => {
                        let _ = connection
                            .send_notification(CancelNotification::new(session_id.clone()));
                        cancel_pending_permissions(&pending_permissions);
                        cancel_pending_user_inputs(&pending_user_inputs);
                    }
                    CommandMessage::Respond {
                        request_id,
                        option_id,
                    } => {
                        if let Some(responder) = pending_permissions.lock().remove(&request_id) {
                            let _ = responder.respond(RequestPermissionResponse::new(
                                RequestPermissionOutcome::Selected(SelectedPermissionOutcome::new(
                                    option_id,
                                )),
                            ));
                        }
                    }
                    CommandMessage::RespondUserInput {
                        request_id,
                        answers,
                    } => {
                        if let Some(pending) = pending_user_inputs.lock().remove(&request_id) {
                            let response = match pending.kind {
                                AcpUserInputKind::Cursor => {
                                    cursor_user_input_response(&pending.params, &answers)
                                }
                                AcpUserInputKind::Xai => {
                                    xai_user_input_response(&pending.params, &answers)
                                }
                            };
                            let _ = pending.responder.respond(response);
                        }
                    }
                    CommandMessage::SetToolApproval { mode, deadline, reply } => {
                        if Instant::now() >= deadline {
                            let _ = reply.send(Err("Tool approval change expired before it could be sent".into()));
                            continue;
                        }
                        if provider != ProviderKind::DeerFlow || !prompt_requests.lock().is_empty() {
                            let _ = reply.send(Err("Tool approvals can only change while the DeerFlow session is idle".into()));
                            continue;
                        }
                        if let Err(error) = request_deerflow_approval(
                            &connection, &session_id, mode, &access, &events, reply.clone(),
                        ) {
                            let _ = reply.send(Err(error.to_string()));
                        }
                    }
                    CommandMessage::Options(options) => {
                        if !model_options_ready
                            || options.model != current_model
                            || options.reasoning_effort != current_effort
                            || options.service_tier != current_tier
                            || options.context_window != current_window
                        {
                            current_model = options.model;
                            current_effort = options.reasoning_effort;
                            current_tier = options.service_tier;
                            current_window = options.context_window;
                            model_options_ready = apply_model(
                                &connection,
                                &session_id,
                                provider,
                                config_options.as_deref(),
                                current_model.as_deref(),
                                current_effort.as_deref(),
                                current_tier.as_deref(),
                                current_window.as_deref(),
                                &events,
                            )
                            .await;
                        }
                    }
                    CommandMessage::Shutdown => break,
                }
            }
            cancel_pending_permissions(&pending_permissions);
            cancel_pending_user_inputs(&pending_user_inputs);
            Ok(())
        })
        .await
}

fn approval_error(message: impl Into<String>) -> agent_client_protocol::Error {
    agent_client_protocol::Error::new(
        agent_client_protocol::ErrorCode::InvalidParams.into(),
        message,
    )
}

fn deerflow_approval(
    options: Option<&[SessionConfigOption]>,
) -> agent_client_protocol::Result<String> {
    let Some(option) = options
        .unwrap_or_default()
        .iter()
        .find(|option| option.id.to_string() == "tool_approval")
    else {
        return Ok("off".into());
    };
    match session_config_current_value(option) {
        Some(mode @ ("ask" | "allow_always" | "reject_always")) => Ok(mode.to_owned()),
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
    smol::future::or(
        async {
            let response = connection
                .send_request(SetSessionConfigOptionRequest::new(
                    session_id.clone(),
                    "tool_approval",
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
            "tool_approval",
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
                .send_request(ResumeSessionRequest::new(existing.to_owned(), cwd))
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
                Err(error) => failures.push(format!("session/resume: {error}")),
            }
        }

        if initialize.agent_capabilities.load_session {
            suppress_session_updates.store(true, Ordering::Release);
            let response = connection
                .send_request(LoadSessionRequest::new(existing.to_owned(), cwd))
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
        .send_request(NewSessionRequest::new(cwd))
        .block_task()
        .await?;
    Ok((response.session_id, response.modes, response.config_options))
}

fn desired_access_mode(
    provider: ProviderKind,
    modes: Option<&SessionModeState>,
    mode: RuntimeMode,
) -> Option<SessionModeId> {
    // DeerFlow's plan mode enables its task planner. It is independent of
    // tool approval and is not the legacy read-only mode used by Cursor.
    if provider == ProviderKind::DeerFlow {
        return None;
    }
    let modes = modes?;
    let desired = if provider == ProviderKind::Fx {
        let desired = if mode == RuntimeMode::Ask {
            "ask"
        } else {
            "code"
        };
        modes
            .available_modes
            .iter()
            .find(|mode| mode.id.to_string().eq_ignore_ascii_case(desired))?
            .id
            .clone()
    } else {
        // Sessions created before the interaction toggle was removed may
        // retain the provider's read-only mode. Return only those sessions to
        // the provider's ordinary execution mode; otherwise leave externally
        // selected native modes untouched.
        if !modes
            .current_mode_id
            .to_string()
            .eq_ignore_ascii_case("plan")
        {
            return None;
        }
        modes
            .available_modes
            .iter()
            .find(|mode| {
                let id = mode.id.to_string();
                id.eq_ignore_ascii_case("agent") || id.eq_ignore_ascii_case("default")
            })?
            .id
            .clone()
    };
    (modes.current_mode_id != desired).then_some(desired)
}

/// Which session config option carries reasoning effort. ACP leaves the id to
/// the agent: Kimi Code exposes it as its `thinking` level, while the other
/// agents Waku drives keep it on `mode`. Grok does not use this path: its
/// effort rides on `session/set_model` as `_meta.reasoningEffort`.
fn reasoning_effort_config_id(provider: ProviderKind) -> &'static str {
    match provider {
        ProviderKind::Kimi => "thinking",
        ProviderKind::DeerFlow => "thinking_enabled",
        _ => "mode",
    }
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

fn cursor_model_selection(
    option: &SessionConfigOption,
    requested: &str,
) -> Option<CursorModelSelection> {
    resolve_cursor_model(session_config_select_values(option), requested)
}

fn cursor_option_id(option: &SessionConfigOption) -> String {
    option.id.to_string().to_ascii_lowercase()
}

fn cursor_option_name(option: &SessionConfigOption) -> String {
    option.name.to_ascii_lowercase()
}

fn is_cursor_thinking_option(option: &SessionConfigOption) -> bool {
    option.category == Some(SessionConfigOptionCategory::ModelConfig) && {
        let id = cursor_option_id(option);
        let name = cursor_option_name(option);
        id == "thinking" || name.contains("thinking")
    }
}

fn is_cursor_fast_option(option: &SessionConfigOption) -> bool {
    option.category == Some(SessionConfigOptionCategory::ModelConfig) && {
        let id = cursor_option_id(option);
        let name = cursor_option_name(option);
        id == "fast" || name == "fast" || name.contains("fast mode")
    }
}

fn is_cursor_context_option(option: &SessionConfigOption) -> bool {
    option.category == Some(SessionConfigOptionCategory::ModelConfig) && {
        let id = cursor_option_id(option);
        let name = cursor_option_name(option);
        id == "context" || id == "context_size" || name.contains("context")
    }
}

fn is_cursor_effort_option(option: &SessionConfigOption) -> bool {
    if !matches!(option.kind, SessionConfigKind::Select(_)) {
        return false;
    }
    let id = cursor_option_id(option);
    let name = cursor_option_name(option);
    id == "effort"
        || id == "reasoning"
        || name == "effort"
        || name == "reasoning"
        || name.contains("effort")
        || name.contains("reasoning")
}

fn find_cursor_effort_option(options: &[SessionConfigOption]) -> Option<&SessionConfigOption> {
    let candidates: Vec<&SessionConfigOption> = options
        .iter()
        .filter(|option| is_cursor_effort_option(option))
        .collect();
    candidates
        .iter()
        .copied()
        .find(|option| {
            matches!(
                option.category.as_ref(),
                Some(SessionConfigOptionCategory::Other(value))
                    if value.eq_ignore_ascii_case("model_option")
            )
        })
        .or_else(|| {
            candidates
                .iter()
                .copied()
                .find(|option| option.id.to_string().eq_ignore_ascii_case("effort"))
        })
        .or_else(|| {
            candidates
                .iter()
                .copied()
                .find(|option| option.category == Some(SessionConfigOptionCategory::ThoughtLevel))
        })
        .or_else(|| candidates.first().copied())
}

fn cursor_matching_select_value<'a>(values: &[&'a str], requested: &str) -> Option<&'a str> {
    let normalized = normalize_cursor_reasoning_effort(requested);
    values
        .iter()
        .find(|value| normalize_cursor_reasoning_effort(value) == normalized)
        .copied()
}

fn cursor_desired_effort_value(
    option: &SessionConfigOption,
    selection: &CursorModelSelection,
    reasoning_effort: Option<&str>,
) -> Option<String> {
    let values = session_config_select_values(option);
    if let Some(effort) = reasoning_effort.filter(|effort| !effort.is_empty())
        && let Some(value) = cursor_matching_select_value(&values, effort)
    {
        return Some(value.to_owned());
    }
    if selection.suffix.contains("extra-high")
        && let Some(value) = values
            .iter()
            .find(|value| normalize_cursor_reasoning_effort(value) == "xhigh")
    {
        return Some((*value).to_owned());
    }
    values
        .iter()
        .find(|value| cursor_suffix_has(&selection.suffix, value))
        .map(|value| (*value).to_owned())
}

fn cursor_desired_thinking(
    selection: &CursorModelSelection,
    reasoning_effort: Option<&str>,
) -> Option<bool> {
    if let Some(effort) = reasoning_effort {
        return Some(normalize_cursor_reasoning_effort(effort) != "none");
    }
    cursor_suffix_has(&selection.suffix, "thinking").then_some(true)
}

fn cursor_desired_fast(
    selection: &CursorModelSelection,
    service_tier: Option<&str>,
) -> Option<bool> {
    match service_tier {
        Some("fast") => Some(true),
        Some(_) => Some(false),
        None if cursor_suffix_has(&selection.suffix, "fast") => Some(true),
        None => None,
    }
}

fn cursor_desired_context_value(
    option: &SessionConfigOption,
    context_window: Option<&str>,
) -> Option<String> {
    let requested = context_window.filter(|value| !value.is_empty())?;
    let values = session_config_select_values(option);
    let normalized = requested.replace(['_', ' '], "-");
    values
        .iter()
        .find(|value| {
            value.eq_ignore_ascii_case(requested)
                || value
                    .replace(['_', ' '], "-")
                    .eq_ignore_ascii_case(&normalized)
        })
        .map(|value| (*value).to_owned())
}

fn cursor_flag_request_value(
    option: &SessionConfigOption,
    enabled: bool,
) -> Option<SessionConfigOptionValue> {
    match &option.kind {
        SessionConfigKind::Boolean(_) => Some(SessionConfigOptionValue::boolean(enabled)),
        SessionConfigKind::Select(_) => {
            let value = if enabled { "true" } else { "false" };
            session_config_select_values(option)
                .contains(&value)
                .then(|| SessionConfigOptionValue::value_id(value))
        }
        _ => None,
    }
}

fn cursor_flag_matches(option: &SessionConfigOption, enabled: bool) -> bool {
    match &option.kind {
        SessionConfigKind::Boolean(boolean) => boolean.current_value == enabled,
        SessionConfigKind::Select(_) => {
            let value = if enabled { "true" } else { "false" };
            session_config_current_value(option) == Some(value)
        }
        _ => false,
    }
}

fn session_config_current_value(option: &SessionConfigOption) -> Option<&str> {
    let SessionConfigKind::Select(select) = &option.kind else {
        return None;
    };
    Some(select.current_value.0.as_ref())
}

async fn apply_cursor_variant_configs(
    connection: &ConnectionTo<Agent>,
    session_id: &SessionId,
    mut options: Vec<SessionConfigOption>,
    selection: &CursorModelSelection,
    reasoning_effort: Option<&str>,
    service_tier: Option<&str>,
    context_window: Option<&str>,
) -> agent_client_protocol::Result<()> {
    // Thinking can reveal a thought-level option, so apply it first and use
    // each response's refreshed option set for the next selection.
    for target in ["thinking", "effort", "context", "fast"] {
        let Some(option) = (match target {
            "thinking" => options
                .iter()
                .find(|option| is_cursor_thinking_option(option)),
            "effort" => find_cursor_effort_option(&options),
            "context" => options
                .iter()
                .find(|option| is_cursor_context_option(option)),
            "fast" => options.iter().find(|option| is_cursor_fast_option(option)),
            _ => None,
        })
        .cloned() else {
            continue;
        };
        let value = match target {
            "thinking" => {
                let Some(enabled) = cursor_desired_thinking(selection, reasoning_effort) else {
                    continue;
                };
                if cursor_flag_matches(&option, enabled) {
                    continue;
                }
                let Some(value) = cursor_flag_request_value(&option, enabled) else {
                    continue;
                };
                value
            }
            "effort" => {
                let Some(value) = cursor_desired_effort_value(&option, selection, reasoning_effort)
                else {
                    continue;
                };
                if session_config_current_value(&option) == Some(value.as_str()) {
                    continue;
                }
                SessionConfigOptionValue::value_id(value)
            }
            "context" => {
                let Some(value) = cursor_desired_context_value(&option, context_window) else {
                    continue;
                };
                if session_config_current_value(&option) == Some(value.as_str()) {
                    continue;
                }
                SessionConfigOptionValue::value_id(value)
            }
            "fast" => {
                let Some(enabled) = cursor_desired_fast(selection, service_tier) else {
                    continue;
                };
                if cursor_flag_matches(&option, enabled) {
                    continue;
                }
                let Some(value) = cursor_flag_request_value(&option, enabled) else {
                    continue;
                };
                value
            }
            _ => continue,
        };
        options = connection
            .send_request(SetSessionConfigOptionRequest::new(
                session_id.clone(),
                option.id,
                value,
            ))
            .block_task()
            .await?
            .config_options;
    }
    Ok(())
}

fn find_config_option(
    config_options: &[SessionConfigOption],
    category: SessionConfigOptionCategory,
) -> Option<&SessionConfigOption> {
    config_options
        .iter()
        .find(|option| option.category.as_ref() == Some(&category))
}

fn fx_model_option(config_options: &[SessionConfigOption]) -> Option<&SessionConfigOption> {
    config_options.iter().find(|option| {
        option.category == Some(SessionConfigOptionCategory::Model)
            && option.id.to_string().eq_ignore_ascii_case("model")
    })
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

fn fx_model_provider_switch<'a>(
    config_options: &'a [SessionConfigOption],
    model: &str,
) -> Option<(&'a SessionConfigOption, &'static str)> {
    if fx_model_option(config_options)
        .is_some_and(|option| session_config_select_values(option).contains(&model))
    {
        return None;
    }
    // Fx scopes model options to the selected account route. AI Gateway IDs
    // are provider/model pairs, while subscription IDs are flat. Selecting the
    // Gateway route returns a refreshed model option that contains these IDs.
    if !model.contains('/') {
        return None;
    }
    let provider = config_options.iter().find(|option| {
        option.category == Some(SessionConfigOptionCategory::Model)
            && option.id.to_string().eq_ignore_ascii_case("provider")
    })?;
    (session_config_current_value(provider) != Some("gateway")
        && session_config_select_values(provider).contains(&"gateway"))
    .then_some((provider, "gateway"))
}

fn set_model_params(
    session_id: &SessionId,
    model: &str,
    reasoning_effort: Option<&str>,
    provider: ProviderKind,
) -> serde_json::Value {
    let mut params = json!({"sessionId": session_id, "modelId": model});
    if provider == ProviderKind::Grok
        && let Some(effort) = reasoning_effort.filter(|effort| !effort.is_empty())
    {
        params["_meta"] = json!({"reasoningEffort": effort});
    }
    params
}

#[allow(clippy::too_many_arguments)]
async fn apply_model(
    connection: &ConnectionTo<Agent>,
    session_id: &SessionId,
    provider: ProviderKind,
    config_options: Option<&[SessionConfigOption]>,
    model: Option<&str>,
    reasoning_effort: Option<&str>,
    service_tier: Option<&str>,
    context_window: Option<&str>,
    events: &DriverEventSender,
) -> bool {
    if provider == ProviderKind::DeerFlow {
        return match apply_deerflow_model(
            connection,
            session_id,
            config_options,
            model,
            reasoning_effort,
        )
        .await
        {
            Ok(()) => true,
            Err(error) => {
                let _ = events.send(DriverEvent::Error(tr!(
                    "errors.select_model",
                    error = error
                )));
                false
            }
        };
    }
    apply_other_model(
        connection,
        session_id,
        provider,
        config_options,
        model,
        reasoning_effort,
        service_tier,
        context_window,
        events,
    )
    .await;
    true
}

async fn apply_deerflow_model(
    connection: &ConnectionTo<Agent>,
    session_id: &SessionId,
    config_options: Option<&[SessionConfigOption]>,
    model: Option<&str>,
    thinking: Option<&str>,
) -> agent_client_protocol::Result<()> {
    let mut options = config_options.unwrap_or_default().to_vec();
    if let Some(model) = model.filter(|model| !model.is_empty()) {
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
    // Thinking can be changed while using the server's default model. It is
    // an on/off switch, never a low/medium/high reasoning-effort ladder.
    if let Some(thinking) = thinking.filter(|value| !value.is_empty()) {
        let value = deerflow_thinking_value(&options, thinking)?;
        let response = connection
            .send_request(SetSessionConfigOptionRequest::new(
                session_id.clone(),
                "thinking_enabled",
                value,
            ))
            .block_task()
            .await?;
        options = response.config_options;
    }
    let models = crate::model_catalog::parse_deerflow_models(&options);
    if !models.is_empty() {
        crate::model_catalog::write_cached_models(ProviderKind::DeerFlow, &models);
    }
    Ok(())
}

fn deerflow_thinking_value(
    options: &[SessionConfigOption],
    requested: &str,
) -> agent_client_protocol::Result<SessionConfigOptionValue> {
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
async fn apply_other_model(
    connection: &ConnectionTo<Agent>,
    session_id: &SessionId,
    provider: ProviderKind,
    config_options: Option<&[SessionConfigOption]>,
    model: Option<&str>,
    reasoning_effort: Option<&str>,
    service_tier: Option<&str>,
    context_window: Option<&str>,
    events: &DriverEventSender,
) {
    let Some(model) = model else {
        return;
    };
    let cursor_model_option = (provider == ProviderKind::Cursor)
        .then_some(config_options)
        .flatten()
        .and_then(|options| find_config_option(options, SessionConfigOptionCategory::Model));
    if let Some(option) = cursor_model_option
        && let Some(selection) = cursor_model_selection(option, model)
    {
        match connection
            .send_request(SetSessionConfigOptionRequest::new(
                session_id.clone(),
                option.id.clone(),
                selection.value.as_str(),
            ))
            .block_task()
            .await
        {
            Ok(response) => {
                if let Err(error) = apply_cursor_variant_configs(
                    connection,
                    session_id,
                    response.config_options,
                    &selection,
                    reasoning_effort,
                    service_tier,
                    context_window,
                )
                .await
                {
                    let _ = events.send(DriverEvent::Error(tr!(
                        "errors.select_model",
                        error = error
                    )));
                }
            }
            Err(error) => {
                let _ = events.send(DriverEvent::Error(tr!(
                    "errors.select_model",
                    error = error
                )));
            }
        }
        return;
    }

    if provider == ProviderKind::Fx {
        let mut options = config_options.unwrap_or_default().to_vec();
        if let Some((provider_option, value)) = fx_model_provider_switch(&options, model) {
            let config_id = provider_option.id.clone();
            match connection
                .send_request(SetSessionConfigOptionRequest::new(
                    session_id.clone(),
                    config_id,
                    value,
                ))
                .block_task()
                .await
            {
                Ok(response) => options = response.config_options,
                Err(error) => {
                    let _ = events.send(DriverEvent::Error(tr!(
                        "errors.select_model",
                        error = error
                    )));
                    return;
                }
            }
        }
        let Some(option) = fx_model_option(&options) else {
            let _ = events.send(DriverEvent::Error(tr!(
                "errors.select_model",
                error = "Fx did not advertise its model configuration"
            )));
            return;
        };
        if !session_config_select_values(option).contains(&model) {
            let _ = events.send(DriverEvent::Error(tr!(
                "errors.select_model",
                error = format!("Fx did not advertise model {model}")
            )));
            return;
        }
        if let Err(error) = connection
            .send_request(SetSessionConfigOptionRequest::new(
                session_id.clone(),
                option.id.clone(),
                model,
            ))
            .block_task()
            .await
        {
            let _ = events.send(DriverEvent::Error(tr!(
                "errors.select_model",
                error = error
            )));
        }
        return;
    }

    {
        // Grok, Kimi, OpenCode, and Cursor agents that do not advertise a model
        // config option retain the legacy request unchanged. Fx intentionally
        // stays on session/set_config_option, its documented model API.
        let request = match UntypedMessage::new(
            "session/set_model",
            set_model_params(session_id, model, reasoning_effort, provider),
        ) {
            Ok(request) => request,
            Err(error) => {
                let _ = events.send(DriverEvent::Error(tr!(
                    "errors.select_model",
                    error = error
                )));
                return;
            }
        };
        if let Err(error) = connection.send_request(request).block_task().await {
            let _ = events.send(DriverEvent::Error(tr!(
                "errors.select_model",
                error = error
            )));
            return;
        }
    }

    if provider != ProviderKind::Grok
        && let Some(effort) = reasoning_effort
    {
        // Reasoning effort is an optional config extension and is deliberately
        // non-fatal when an agent does not expose it.
        let _ = connection
            .send_request(SetSessionConfigOptionRequest::new(
                session_id.clone(),
                reasoning_effort_config_id(provider),
                effort,
            ))
            .block_task()
            .await;
    }
}

#[allow(clippy::too_many_arguments)]
fn send_prompt(
    connection: &ConnectionTo<Agent>,
    session_id: &SessionId,
    text: String,
    attachments: &[crate::model::MessageAttachment],
    prompt_requests: &PendingPromptRequests,
    events: &DriverEventSender,
    provider: ProviderKind,
    native_session_id: &str,
    grok_title_home: Option<std::path::PathBuf>,
    title_refresh: super::title_refresh::NativeTitleRefresh,
    stream_state: Arc<Mutex<AcpStreamState>>,
) -> agent_client_protocol::Result<()> {
    stream_state.lock().produced_content = false;
    // Read before the turn runs, so the failure lookup cannot mistake an
    // earlier turn's record for this one's.
    let wire_offset = (provider == ProviderKind::Kimi)
        .then(|| crate::kimi_session::wire_offset(native_session_id));
    let extension_id =
        (provider == ProviderKind::Grok).then(|| format!("waku-{}", uuid::Uuid::new_v4()));
    let content = if provider == ProviderKind::DeerFlow {
        deerflow_prompt_content(
            text,
            attachments,
            &waku_protocol::identity::desktop_data_directory(),
        )?
    } else {
        vec![ContentBlock::Text(TextContent::new(text))]
    };
    let mut request = PromptRequest::new(session_id.clone(), content);
    if let Some(extension_id) = extension_id.as_ref() {
        let mut meta = serde_json::Map::new();
        meta.insert("promptId".into(), Value::String(extension_id.clone()));
        meta.insert("requestId".into(), Value::String(extension_id.clone()));
        request = request.meta(meta);
    }
    let sent = connection.send_request(request);
    let request_id = sent.id().clone();
    prompt_requests.lock().insert(
        request_id.clone(),
        extension_id,
        native_session_id.to_owned(),
    );
    let callback_request_id = request_id.clone();
    let callback_requests = prompt_requests.clone();
    let callback_events = events.clone();
    let native_session_id = native_session_id.to_owned();
    let registered = sent.on_receiving_result(async move |result| {
        if settle_prompt_request(&callback_requests, &callback_request_id) {
            // Only an empty turn pays for this lookup, so a healthy turn never
            // waits on Kimi's records.
            let native_failure = wire_offset
                .filter(|_| !stream_state.lock().produced_content)
                .and_then(|offset| crate::kimi_session::turn_failure(&native_session_id, offset));
            let success = finish_prompt(result, native_failure, &callback_events);
            if provider == ProviderKind::Grok && success {
                start_grok_title_refresh(
                    grok_title_home.as_deref(),
                    &native_session_id,
                    &title_refresh,
                    callback_events,
                );
            }
        }
        Ok(())
    });
    if registered.is_err() {
        prompt_requests.lock().settle_request(&request_id);
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

/// Only explicit, daemon-managed composer attachments become image blocks.
/// Textual @paths never grant permission to read a local file. The canonical
/// containment check also excludes symlinks that escape the attachment store.
fn deerflow_prompt_content(
    text: String,
    attachments: &[crate::model::MessageAttachment],
    data_root: &Path,
) -> agent_client_protocol::Result<Vec<ContentBlock>> {
    let mut content = vec![ContentBlock::Text(TextContent::new(text))];
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
        if !metadata.is_file() || metadata.len() > MAX_DEERFLOW_IMAGE_BYTES {
            return Err(image_attachment_error(format!(
                "Image {} must be a file no larger than 20 MB.",
                attachment.name
            )));
        }
        let mut bytes = Vec::with_capacity(metadata.len() as usize);
        file.take(MAX_DEERFLOW_IMAGE_BYTES + 1)
            .read_to_end(&mut bytes)
            .map_err(|error| {
                image_attachment_error(format!("Could not read image {}: {error}", attachment.name))
            })?;
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
                "Image {} must contain PNG, JPEG, GIF, or WebP data.",
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

fn settle_prompt_request(prompt_requests: &Mutex<PendingPrompts>, request_id: &RequestId) -> bool {
    prompt_requests.lock().settle_request(request_id)
}

fn finish_xai_prompt_complete(
    params: &Value,
    prompt_requests: &Mutex<PendingPrompts>,
    events: &DriverEventSender,
) -> Option<String> {
    let Some(session_id) = params.get("sessionId").and_then(Value::as_str) else {
        return None;
    };
    let prompt_id = params.get("promptId").and_then(Value::as_str);
    if !prompt_requests
        .lock()
        .settle_extension(session_id, prompt_id)
    {
        return None;
    }

    let stop_reason = match params.get("stopReason").and_then(Value::as_str) {
        Some("cancelled") => StopReason::Cancelled,
        Some("max_tokens") => StopReason::MaxTokens,
        Some("max_turn_requests") => StopReason::MaxTurnRequests,
        Some("refusal") => StopReason::Refusal,
        _ => StopReason::EndTurn,
    };
    finish_prompt(Ok(PromptResponse::new(stop_reason)), None, events).then(|| session_id.to_owned())
}

fn start_grok_title_refresh(
    grok_title_home: Option<&Path>,
    native_session_id: &str,
    title_refresh: &super::title_refresh::NativeTitleRefresh,
    events: DriverEventSender,
) {
    let grok_title_home = grok_title_home.map(ToOwned::to_owned);
    let native_session_id = native_session_id.to_owned();
    title_refresh.start(
        "waku-grok-title",
        vec![
            Duration::ZERO,
            Duration::from_millis(250),
            Duration::from_millis(750),
            Duration::from_millis(1_500),
            Duration::from_secs(3),
            Duration::from_secs(5),
            Duration::from_millis(7_500),
            Duration::from_secs(10),
        ],
        events,
        move || match grok_title_home.as_deref() {
            Some(home) => crate::grok_session::generated_title_in(home, &native_session_id),
            None => crate::grok_session::generated_title(&native_session_id),
        },
    );
}

fn finish_prompt(
    result: agent_client_protocol::Result<PromptResponse>,
    native_failure: Option<String>,
    events: &impl DriverEventSink,
) -> bool {
    let response = match result {
        Ok(response) => response,
        Err(error) => {
            let _ = events.send(DriverEvent::Error(error.to_string()));
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
        StopReason::EndTurn | StopReason::Cancelled => (true, None),
        StopReason::MaxTokens => (false, Some(tr!("session.agent_ran_out_of_context"))),
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

fn cancel_pending_permissions(pending: &PendingPermissions) {
    for (_, responder) in pending.lock().drain() {
        let _ = responder.respond(RequestPermissionResponse::new(
            RequestPermissionOutcome::Cancelled,
        ));
    }
}

fn cancel_pending_user_inputs(pending: &PendingAcpUserInputs) {
    for (_, pending) in pending.lock().drain() {
        let _ = pending
            .responder
            .respond(cancelled_user_input_response(pending.kind));
    }
}

fn cancelled_user_input_response(kind: AcpUserInputKind) -> Value {
    match kind {
        AcpUserInputKind::Cursor => json!({"answers": {}}),
        AcpUserInputKind::Xai => json!({"outcome": "cancelled"}),
    }
}

fn unwrap_xai_question_params(params: &Value) -> &Value {
    if matches!(
        params.get("method").and_then(Value::as_str),
        Some("x.ai/ask_user_question" | "_x.ai/ask_user_question")
    ) {
        params.get("params").unwrap_or(params)
    } else {
        params
    }
}

fn cursor_user_input_questions(params: &Value) -> Vec<UserInputQuestion> {
    params
        .get("questions")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(|question| {
            let text = question.get("prompt").and_then(Value::as_str)?.trim();
            if text.is_empty() {
                return None;
            }
            let mut options = question
                .get("options")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
                .filter_map(|option| {
                    let label = option.get("label").and_then(Value::as_str)?.trim();
                    (!label.is_empty()).then(|| UserInputOption {
                        label: label.to_owned(),
                        description: Some(label.to_owned()),
                    })
                })
                .collect::<Vec<_>>();
            if options.is_empty() {
                options.push(UserInputOption {
                    label: "OK".into(),
                    description: Some("Continue".into()),
                });
            }
            Some(UserInputQuestion {
                id: question
                    .get("id")
                    .and_then(Value::as_str)
                    .filter(|id| !id.is_empty())
                    .unwrap_or(text)
                    .to_owned(),
                header: "Question".into(),
                question: text.to_owned(),
                options,
                multi_select: question
                    .get("allowMultiple")
                    .and_then(Value::as_bool)
                    .unwrap_or(false),
            })
        })
        .collect()
}

fn cursor_user_input_response(params: &Value, submitted: &[UserInputAnswer]) -> Value {
    let mut answers = serde_json::Map::new();
    for question in params
        .get("questions")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
    {
        let Some(id) = question.get("id").and_then(Value::as_str) else {
            continue;
        };
        let values = submitted
            .iter()
            .find(|answer| answer.question_id == id)
            .map(|answer| answer.answers.as_slice())
            .unwrap_or_default();
        let value = if question
            .get("allowMultiple")
            .and_then(Value::as_bool)
            .unwrap_or(false)
        {
            json!(values)
        } else {
            values
                .first()
                .map_or(Value::String(String::new()), |value| json!(value))
        };
        answers.insert(id.to_owned(), value);
    }
    json!({"answers": answers})
}

fn xai_user_input_questions(params: &Value) -> Vec<UserInputQuestion> {
    params
        .get("questions")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .enumerate()
        .filter_map(|(index, question)| {
            let text = question.get("question").and_then(Value::as_str)?.trim();
            if text.is_empty() {
                return None;
            }
            let mut options = question
                .get("options")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
                .filter_map(|option| {
                    let label = option.get("label").and_then(Value::as_str)?.trim();
                    (!label.is_empty()).then(|| UserInputOption {
                        label: label.to_owned(),
                        description: option
                            .get("description")
                            .and_then(Value::as_str)
                            .map(str::trim)
                            .filter(|description| !description.is_empty())
                            .map(str::to_owned),
                    })
                })
                .collect::<Vec<_>>();
            if options.is_empty() {
                options.push(UserInputOption {
                    label: "OK".into(),
                    description: Some("Continue".into()),
                });
            }
            Some(UserInputQuestion {
                id: question
                    .get("id")
                    .and_then(Value::as_str)
                    .filter(|id| !id.is_empty())
                    .unwrap_or(text)
                    .to_owned(),
                header: format!("Question {}", index + 1),
                question: text.to_owned(),
                options,
                multi_select: question
                    .get("multiSelect")
                    .and_then(Value::as_bool)
                    .unwrap_or(false),
            })
        })
        .collect()
}

fn xai_user_input_response(params: &Value, submitted: &[UserInputAnswer]) -> Value {
    let mut answers = serde_json::Map::new();
    let mut annotations = serde_json::Map::new();
    for question in params
        .get("questions")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
    {
        let Some(question_text) = question.get("question").and_then(Value::as_str) else {
            continue;
        };
        let id = question
            .get("id")
            .and_then(Value::as_str)
            .unwrap_or(question_text);
        let values = submitted
            .iter()
            .find(|answer| answer.question_id == id || answer.question_id == question_text)
            .map(|answer| answer.answers.as_slice())
            .unwrap_or_default();
        let options = question
            .get("options")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
            .collect::<Vec<_>>();
        let option_labels = options
            .iter()
            .filter_map(|option| option.get("label").and_then(Value::as_str))
            .collect::<Vec<_>>();
        let selected = values
            .iter()
            .filter(|value| option_labels.contains(&value.as_str()))
            .cloned()
            .collect::<Vec<_>>();
        let notes = values
            .iter()
            .filter(|value| !option_labels.contains(&value.as_str()))
            .cloned()
            .collect::<Vec<_>>()
            .join("\n");
        let preview = if question
            .get("multiSelect")
            .and_then(Value::as_bool)
            .unwrap_or(false)
        {
            None
        } else {
            selected.iter().find_map(|selected| {
                options.iter().find_map(|option| {
                    (option.get("label").and_then(Value::as_str) == Some(selected.as_str()))
                        .then(|| {
                            option
                                .get("preview")
                                .and_then(Value::as_str)
                                .map(str::trim)
                                .filter(|preview| !preview.is_empty())
                                .map(str::to_owned)
                        })
                        .flatten()
                })
            })
        };
        answers.insert(
            question_text.to_owned(),
            json!(if selected.is_empty() && !notes.is_empty() {
                vec!["Other".to_owned()]
            } else {
                selected
            }),
        );
        let mut annotation = serde_json::Map::new();
        if let Some(preview) = preview {
            annotation.insert("preview".into(), Value::String(preview));
        }
        if !notes.is_empty() {
            annotation.insert("notes".into(), Value::String(notes));
        }
        if !annotation.is_empty() {
            annotations.insert(question_text.to_owned(), Value::Object(annotation));
        }
    }
    let mut response = json!({"outcome": "accepted", "answers": answers});
    if !annotations.is_empty() {
        response["annotations"] = Value::Object(annotations);
    }
    response
}

fn handle_permission_request(
    request: RequestPermissionRequest,
    responder: PermissionResponder,
    provider: ProviderKind,
    mode: RuntimeMode,
    pending: &PendingPermissions,
    events: &impl DriverEventSink,
) -> agent_client_protocol::Result<()> {
    let request_id = responder.id().to_string();
    let params = serde_json::to_value(&request)?;
    let options = request
        .options
        .iter()
        .map(|option| PermissionOption {
            id: option.option_id.to_string(),
            label: option.name.clone(),
            allow: matches!(
                option.kind,
                PermissionOptionKind::AllowOnce | PermissionOptionKind::AllowAlways
            ),
        })
        .collect::<Vec<_>>();

    if provider != ProviderKind::DeerFlow && mode != RuntimeMode::Ask {
        let choice = request
            .options
            .iter()
            .find(|option| option.kind == PermissionOptionKind::AllowAlways)
            .or_else(|| {
                request
                    .options
                    .iter()
                    .find(|option| option.kind == PermissionOptionKind::AllowOnce)
            });
        return match choice {
            Some(choice) => responder.respond(RequestPermissionResponse::new(
                RequestPermissionOutcome::Selected(SelectedPermissionOutcome::new(
                    choice.option_id.clone(),
                )),
            )),
            None => responder.respond(RequestPermissionResponse::new(
                RequestPermissionOutcome::Cancelled,
            )),
        };
    }

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
    pending.lock().insert(request_id.clone(), responder);
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
    if provider == ProviderKind::Fx
        && !state.produced_content
        && kind == Some("agent_message_chunk")
        && update
            .pointer("/content/text")
            .and_then(Value::as_str)
            .is_some_and(fx_context_notice)
    {
        return Ok(());
    }
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
            if let Some(text) = update
                .get("content")
                .and_then(crate::acp_session::content_text)
            {
                let _ = events.send(DriverEvent::TextDelta(text));
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

fn fx_context_notice(text: &str) -> bool {
    text.starts_with("[context] ") || text.starts_with("skill discovery warning: ")
}

#[derive(Default)]
struct AcpStreamState {
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
    (!reason.is_empty()).then(|| truncate(&reason, 400))
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
        self.supports_steer
    }

    fn steer(&self, prompt: String) {
        let _ = self.commands.try_send(CommandMessage::Steer(prompt));
    }

    fn cancel(&self) {
        let _ = self.commands.try_send(CommandMessage::Cancel);
    }

    fn cancel_computer_use(&self) {
        if let Some(computer_use) = self.computer_use.as_ref() {
            computer_use.stop();
        }
    }

    fn respond(&self, request_id: String, option_id: String) {
        let _ = self.commands.try_send(CommandMessage::Respond {
            request_id,
            option_id,
        });
    }

    fn respond_user_input(&self, request_id: String, answers: Vec<UserInputAnswer>) {
        let _ = self.commands.try_send(CommandMessage::RespondUserInput {
            request_id,
            answers,
        });
    }

    fn apply_options(&self, options: SessionOptions) -> bool {
        // DeerFlow owns its policy through tool_approval. A stale client mode
        // accompanying a model change must never overwrite it or force a
        // restart that pretends the stale mode is authoritative.
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

    fn rollback(&self, _turns: usize) -> anyhow::Result<Option<ProviderResumeCursor>> {
        Err(anyhow!(
            "conversation rollback is not supported by this provider transport"
        ))
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
    fn deerflow_keeps_its_task_planner_independent_of_tool_approval() {
        let modes = SessionModeState::new(
            "plan",
            vec![
                SessionMode::new("default", "Default"),
                SessionMode::new("plan", "Plan"),
            ],
        );
        assert!(
            desired_access_mode(ProviderKind::DeerFlow, Some(&modes), RuntimeMode::Ask).is_none()
        );
        assert!(
            desired_access_mode(
                ProviderKind::DeerFlow,
                Some(&modes),
                RuntimeMode::FullAccess
            )
            .is_none()
        );
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
                    apply_deerflow_model(&connection, &SessionId::new("s"), None, None, Some("off"))
                        .await
                }),
        )
        .unwrap();
        let calls = calls.lock();
        assert_eq!(calls.len(), 1);
        assert_eq!(calls[0]["configId"], "thinking_enabled");
        assert_eq!(calls[0]["value"], "off");
    }

    fn approval_options(mode: &str) -> Vec<SessionConfigOption> {
        serde_json::from_value(json!([{
            "id":"tool_approval", "name":"Tool approvals", "type":"select",
            "currentValue":mode,
            "options":[
                {"value":"ask", "name":"Ask"},
                {"value":"allow_always", "name":"Allow"},
                {"value":"reject_always", "name":"Reject"}
            ]
        }]))
        .unwrap()
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
                    for stored in ["ask", "allow_always", "reject_always"] {
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
                responder.respond(SetSessionConfigOptionResponse::new(approval_options("ask")))
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
    fn deerflow_rejects_images_outside_store_and_invalid_or_oversized_data() {
        let root =
            std::env::temp_dir().join(format!("deerflow-acp-images-{}", uuid::Uuid::new_v4()));
        let mut image = stored_image_attachment(&root, "not-image.png", b"plain text");
        assert!(deerflow_prompt_content(String::new(), &[image.clone()], &root).is_err());
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
            supports_steer: false,
            provider: ProviderKind::DeerFlow,
            access: Arc::new(Mutex::new(AcpAccessState::new(RuntimeMode::Ask))),
            computer_use: None,
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
    fn cursor_question_response_uses_native_scalar_and_array_answers() {
        let params = json!({
            "toolCallId": "ask-1",
            "questions": [
                {
                    "id": "scope",
                    "prompt": "Which scope?",
                    "options": [{"id": "workspace", "label": "Workspace"}]
                },
                {
                    "id": "checks",
                    "prompt": "Which checks?",
                    "options": [
                        {"id": "tests", "label": "Tests"},
                        {"id": "lint", "label": "Lint"}
                    ],
                    "allowMultiple": true
                }
            ]
        });

        let questions = cursor_user_input_questions(&params);
        assert_eq!(questions.len(), 2);
        assert!(!questions[0].multi_select);
        assert!(questions[1].multi_select);

        let response = cursor_user_input_response(
            &params,
            &[
                UserInputAnswer {
                    question_id: "scope".into(),
                    answers: vec!["Workspace".into()],
                },
                UserInputAnswer {
                    question_id: "checks".into(),
                    answers: vec!["Tests".into(), "Lint".into()],
                },
            ],
        );
        assert_eq!(
            response.pointer("/answers/scope"),
            Some(&json!("Workspace"))
        );
        assert_eq!(
            response.pointer("/answers/checks"),
            Some(&json!(["Tests", "Lint"]))
        );
    }

    #[test]
    fn grok_question_response_keeps_native_labels_and_annotates_custom_text() {
        let params = json!({
            "sessionId": "session-1",
            "toolCallId": "tool-1",
            "mode": "default",
            "questions": [
                {
                    "id": "environment",
                    "question": "Where should this deploy?",
                    "options": [{"label": "Preview", "preview": "Deploy to preview"}],
                    "multiSelect": false
                },
                {
                    "id": "notes",
                    "question": "Anything else?",
                    "options": [{"label": "No"}],
                    "multiSelect": false
                }
            ]
        });
        let response = xai_user_input_response(
            &params,
            &[
                UserInputAnswer {
                    question_id: "environment".into(),
                    answers: vec!["Preview".into()],
                },
                UserInputAnswer {
                    question_id: "notes".into(),
                    answers: vec!["Use the EU region".into()],
                },
            ],
        );

        assert_eq!(response["outcome"], "accepted");
        assert_eq!(
            response.pointer("/answers/Where should this deploy?/0"),
            Some(&json!("Preview"))
        );
        assert_eq!(
            response.pointer("/answers/Anything else?/0"),
            Some(&json!("Other"))
        );
        assert_eq!(
            response.pointer("/annotations/Where should this deploy?/preview"),
            Some(&json!("Deploy to preview"))
        );
        assert_eq!(
            response.pointer("/annotations/Anything else?/notes"),
            Some(&json!("Use the EU region"))
        );
    }

    #[test]
    fn legacy_read_only_sessions_return_to_the_advertised_agent_mode() {
        let modes = SessionModeState::new(
            "plan",
            vec![
                SessionMode::new("agent", "Agent"),
                SessionMode::new("plan", "Plan"),
            ],
        );

        assert_eq!(
            desired_access_mode(ProviderKind::Cursor, Some(&modes), RuntimeMode::FullAccess)
                .map(|mode| mode.to_string()),
            Some("agent".to_owned())
        );
    }

    #[test]
    fn fx_access_mode_selects_ask_or_code() {
        let modes = SessionModeState::new(
            "code",
            vec![
                SessionMode::new("ask", "Ask before sensitive actions"),
                SessionMode::new("code", "Review sensitive actions automatically"),
            ],
        );
        assert_eq!(
            desired_access_mode(ProviderKind::Fx, Some(&modes), RuntimeMode::Ask)
                .map(|mode| mode.to_string()),
            Some("ask".to_owned())
        );
        assert!(
            desired_access_mode(ProviderKind::Fx, Some(&modes), RuntimeMode::FullAccess).is_none()
        );
    }

    #[test]
    fn fx_launches_its_documented_acp_subcommand() {
        let launch = launch_for(ProviderKind::Fx, None).unwrap();
        assert_eq!(launch.args, ["acp"]);
        assert!(launch.env.is_empty());
    }

    #[test]
    fn fx_model_option_ignores_provider_selector_in_same_category() {
        let provider = select_config_option(
            "provider",
            SessionConfigOptionCategory::Model,
            "gateway",
            &["gateway", "codex", "grok"],
        );
        let model = select_config_option(
            "model",
            SessionConfigOptionCategory::Model,
            "openai/gpt-5.6-sol",
            &["openai/gpt-5.6-sol", "anthropic/claude-sonnet-5"],
        );

        assert_eq!(
            fx_model_option(&[provider, model]).map(|option| option.id.to_string()),
            Some("model".to_owned())
        );
    }

    #[test]
    fn fx_gateway_model_selects_the_gateway_route_first() {
        let provider = select_config_option(
            "provider",
            SessionConfigOptionCategory::Model,
            "codex",
            &["gateway", "codex", "grok"],
        );
        let model = select_config_option(
            "model",
            SessionConfigOptionCategory::Model,
            "gpt-5.6-luna",
            &["gpt-5.6-sol", "gpt-5.6-luna"],
        );
        let options = [provider, model];

        let (option, value) =
            fx_model_provider_switch(&options, "openai/gpt-5.6-luna-fast").unwrap();
        assert_eq!(option.id.to_string(), "provider");
        assert_eq!(value, "gateway");
    }

    #[test]
    fn cursor_model_aliases_resolve_to_advertised_parameterized_picker_values() {
        let option = select_config_option(
            "model",
            SessionConfigOptionCategory::Model,
            "default",
            &["default", "grok-4.6", "composer-2.5", "claude-sonnet-4-6"],
        );

        assert_eq!(
            cursor_model_selection(&option, "auto"),
            Some(CursorModelSelection {
                value: "default".into(),
                suffix: String::new(),
            })
        );
        assert_eq!(
            cursor_model_selection(&option, "composer-2.5"),
            Some(CursorModelSelection {
                value: "composer-2.5".into(),
                suffix: String::new(),
            })
        );
        assert_eq!(
            cursor_model_selection(&option, "cursor-grok-4.6-xhigh-fast"),
            Some(CursorModelSelection {
                value: "grok-4.6".into(),
                suffix: "xhigh-fast".into(),
            })
        );
        assert_eq!(
            cursor_model_selection(&option, "claude-4.6-sonnet-medium-thinking"),
            Some(CursorModelSelection {
                value: "claude-sonnet-4-6".into(),
                suffix: "medium-thinking".into(),
            })
        );
    }

    #[test]
    fn cursor_model_suffix_selects_dynamic_effort_thinking_and_fast_options() {
        let selection = CursorModelSelection {
            value: "claude-opus-5".into(),
            suffix: "thinking-extra-high-fast".into(),
        };
        let effort = select_config_option(
            "effort",
            SessionConfigOptionCategory::ThoughtLevel,
            "high",
            &["low", "medium", "high", "xhigh"],
        );
        let extra_high = select_config_option(
            "reasoning",
            SessionConfigOptionCategory::ThoughtLevel,
            "high",
            &["low", "medium", "high", "extra-high"],
        );
        let context = select_config_option(
            "context",
            SessionConfigOptionCategory::ModelConfig,
            "272k",
            &["272k", "1m"],
        );

        assert_eq!(
            cursor_desired_effort_value(&effort, &selection, None).as_deref(),
            Some("xhigh")
        );
        assert_eq!(
            cursor_desired_effort_value(&extra_high, &selection, Some("xhigh")).as_deref(),
            Some("extra-high")
        );
        assert_eq!(cursor_desired_thinking(&selection, None), Some(true));
        assert_eq!(cursor_desired_fast(&selection, None), Some(true));
        assert_eq!(
            cursor_desired_effort_value(&effort, &selection, Some("low")).as_deref(),
            Some("low")
        );
        assert_eq!(
            cursor_desired_fast(&selection, Some("default")),
            Some(false)
        );
        assert_eq!(
            cursor_desired_context_value(&context, Some("1m")).as_deref(),
            Some("1m")
        );
        assert_eq!(
            cursor_desired_thinking(&selection, Some("none")),
            Some(false)
        );
    }

    #[test]
    fn cursor_prefers_model_option_effort_over_thought_level() {
        let thought = select_config_option(
            "reasoning",
            SessionConfigOptionCategory::ThoughtLevel,
            "high",
            &["low", "medium", "high"],
        );
        let effort = select_config_option(
            "effort",
            SessionConfigOptionCategory::Other("model_option".into()),
            "max",
            &["low", "medium", "high", "max"],
        );
        let options = [thought, effort];
        let selected = find_cursor_effort_option(&options).unwrap();
        assert_eq!(selected.id.to_string(), "effort");
    }

    #[test]
    fn a_steer_only_settles_when_the_last_sdk_request_finishes() {
        let requests = Mutex::new(PendingPrompts::default());
        requests
            .lock()
            .insert(RequestId::Str("first".into()), None, "session".into());
        requests
            .lock()
            .insert(RequestId::Str("steer".into()), None, "session".into());
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

    #[test]
    fn xai_prompt_complete_settles_a_missing_standard_response_once() {
        let requests = Mutex::new(PendingPrompts::default());
        let request_id = RequestId::Str("sdk-request".into());
        requests.lock().insert(
            request_id.clone(),
            Some("waku-prompt".into()),
            "grok-session".into(),
        );
        let (events, event_rx) = crate::driver::test_event_channel();

        assert_eq!(
            finish_xai_prompt_complete(
                &json!({
                    "sessionId": "grok-session",
                    "promptId": "waku-prompt",
                    "stopReason": "end_turn"
                }),
                &requests,
                &events,
            ),
            Some("grok-session".into())
        );
        assert!(matches!(
            event_rx.try_recv().unwrap(),
            DriverEvent::TurnFinished {
                success: true,
                summary: None
            }
        ));
        assert!(!settle_prompt_request(&requests, &request_id));
        assert!(event_rx.try_recv().is_err());
    }

    /// Kimi ends a failed turn with `end_turn` and no content at all, so the
    /// provider's own record is the only thing that can name the cause.
    #[test]
    fn a_recovered_provider_failure_overrides_a_clean_stop_reason() {
        let (events, event_rx) = crossbeam_channel::unbounded();

        assert!(!finish_prompt(
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
    fn typed_prompt_response_settles_the_turn() {
        let (events, event_rx) = crossbeam_channel::unbounded();
        assert!(finish_prompt(
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
    fn fx_context_notices_do_not_become_assistant_text() {
        let (events, event_rx) = crossbeam_channel::unbounded();
        let mut state = AcpStreamState::default();
        for text in [
            "[context] skill catalog omitted 19 entries",
            "skill discovery warning: candidate was skipped",
            "Hi! How can I help?",
            "[context] is ordinary text after the answer starts",
        ] {
            let update = serde_json::from_value(json!({
                "sessionUpdate": "agent_message_chunk",
                "content": {"type": "text", "text": text}
            }))
            .unwrap();
            handle_session_update(
                ProviderKind::Fx,
                SessionNotification::new("s", update),
                &events,
                &mut state,
            )
            .unwrap();
        }

        let seen = event_rx.try_iter().collect::<Vec<_>>();
        assert_eq!(seen.len(), 2);
        assert!(matches!(&seen[0], DriverEvent::TextDelta(text) if text == "Hi! How can I help?"));
        assert!(matches!(&seen[1], DriverEvent::TextDelta(text) if text.starts_with("[context]")));
        assert!(state.produced_content);
    }

    #[test]
    fn grok_launch_passes_reasoning_effort_before_stdio() {
        let launch = launch_for(ProviderKind::Grok, Some("xhigh")).unwrap();
        assert_eq!(
            launch.args,
            ["agent", "--reasoning-effort", "xhigh", "stdio"]
        );
        let bare = launch_for(ProviderKind::Grok, None).unwrap();
        assert_eq!(bare.args, ["agent", "stdio"]);
    }

    #[test]
    fn grok_set_model_includes_reasoning_effort_meta() {
        let params = set_model_params(
            &SessionId::new("sess"),
            "grok-4.6",
            Some("xhigh"),
            ProviderKind::Grok,
        );
        assert_eq!(params["modelId"], "grok-4.6");
        assert_eq!(params["_meta"]["reasoningEffort"], "xhigh");
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

    /// Drives a real agent through the SDK-backed driver. Ignored by default:
    /// it needs the CLI installed, credentials, and the network.
    #[test]
    #[ignore = "requires an installed, authenticated grok"]
    fn grok_prompt_response_from_the_sdk_finishes_the_turn() {
        let binary = crate::command_env::find_executable("grok").expect("grok is not installed");
        let (events, event_rx) = crate::driver::test_event_channel();
        let driver = AcpDriver::start(
            ProviderKind::Grok,
            DriverStartOptions {
                binary,
                cwd: std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR")),
                mode: RuntimeMode::FullAccess,
                model: Some("grok-4.5".into()),
                reasoning_effort: None,
                service_tier: None,
                context_window: None,
                agent_preset: None,
                computer_use_enabled: false,
                provider_cursor: None,
            },
            events,
        )
        .expect("the ACP session should open");

        loop {
            let event = event_rx
                .recv_timeout(Duration::from_secs(60))
                .expect("the agent should report its session");
            match event {
                DriverEvent::Connected {
                    provider_cursor: Some(ProviderResumeCursor::Grok { .. }),
                } => break,
                DriverEvent::Error(error) => panic!("the agent reported: {error}"),
                _ => {}
            }
        }
        driver.prompt("hi".into());
        let mut finished = None;
        while let Ok(event) = event_rx.recv_timeout(Duration::from_secs(120)) {
            match event {
                DriverEvent::TurnFinished { success, .. } => {
                    finished = Some(success);
                    break;
                }
                DriverEvent::Error(error) => panic!("the agent reported: {error}"),
                _ => {}
            }
        }
        assert_eq!(finished, Some(true));
    }

    /// Covers Cursor's provider-private parameterized picker with a model id
    /// whose CLI alias carries both effort and fast-mode values.
    #[test]
    #[ignore = "requires an installed, authenticated cursor-agent"]
    fn cursor_parameterized_model_selection_finishes_a_real_turn() {
        let binary = crate::command_env::find_executable("cursor-agent")
            .expect("cursor-agent is not installed");
        let (events, event_rx) = crate::driver::test_event_channel();
        let driver = AcpDriver::start(
            ProviderKind::Cursor,
            DriverStartOptions {
                binary,
                cwd: std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR")),
                mode: RuntimeMode::FullAccess,
                model: Some("cursor-grok-4.6-xhigh".into()),
                reasoning_effort: None,
                service_tier: None,
                context_window: None,
                agent_preset: None,
                computer_use_enabled: false,
                provider_cursor: None,
            },
            events,
        )
        .expect("the ACP session should open");

        loop {
            let event = event_rx
                .recv_timeout(Duration::from_secs(60))
                .expect("the agent should report its session");
            match event {
                DriverEvent::Connected {
                    provider_cursor: Some(ProviderResumeCursor::Cursor { .. }),
                } => break,
                DriverEvent::Error(error) => panic!("the agent reported: {error}"),
                _ => {}
            }
        }
        driver.prompt("Reply exactly OK.".into());

        let mut produced_text = false;
        let mut finished = None;
        while let Ok(event) = event_rx.recv_timeout(Duration::from_secs(120)) {
            match event {
                DriverEvent::TextDelta(text) => produced_text |= !text.is_empty(),
                DriverEvent::TurnFinished { success, .. } => {
                    finished = Some(success);
                    break;
                }
                DriverEvent::Error(error) => panic!("the agent reported: {error}"),
                _ => {}
            }
        }
        assert!(produced_text, "the Cursor turn produced no text");
        assert_eq!(finished, Some(true));
    }

    /// The invariant Kimi's silent failures break: a turn may finish
    /// successfully or report why it did not, but it must never claim success
    /// having produced nothing at all. Holds whether or not the account is
    /// currently able to serve the request.
    #[test]
    #[ignore = "requires an installed, authenticated kimi"]
    fn kimi_never_reports_an_empty_turn_as_a_success() {
        let binary = crate::command_env::find_executable("kimi").expect("kimi is not installed");
        let (events, event_rx) = crate::driver::test_event_channel();
        let driver = AcpDriver::start(
            ProviderKind::Kimi,
            DriverStartOptions {
                binary,
                cwd: std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR")),
                mode: RuntimeMode::FullAccess,
                model: None,
                reasoning_effort: None,
                service_tier: None,
                context_window: None,
                agent_preset: None,
                computer_use_enabled: false,
                provider_cursor: None,
            },
            events,
        )
        .expect("the ACP session should open");

        loop {
            let event = event_rx
                .recv_timeout(Duration::from_secs(60))
                .expect("the agent should report its session");
            match event {
                DriverEvent::Connected {
                    provider_cursor: Some(ProviderResumeCursor::Kimi { .. }),
                } => break,
                DriverEvent::Error(error) => panic!("the agent reported: {error}"),
                _ => {}
            }
        }
        driver.prompt("Say hi in three words.".into());

        let mut produced_content = false;
        let mut reported_error = None;
        let mut finished = None;
        while let Ok(event) = event_rx.recv_timeout(Duration::from_secs(120)) {
            match event {
                DriverEvent::TextDelta(_) | DriverEvent::ReasoningDelta(_) => {
                    produced_content = true;
                }
                DriverEvent::Error(error) => reported_error = Some(error),
                DriverEvent::TurnFinished { success, .. } => {
                    finished = Some(success);
                    break;
                }
                _ => {}
            }
        }

        match finished.expect("the turn should settle") {
            true => assert!(
                produced_content,
                "the turn was reported successful without producing anything"
            ),
            false => assert!(
                reported_error.is_some_and(|error| !error.trim().is_empty()),
                "the turn failed without naming a reason"
            ),
        }
    }
}
