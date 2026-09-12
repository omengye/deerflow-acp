//! ACP v2 facade for the stable DeerFlow ACP v1 daemon.
//!
//! DeerFlow's Python agent remains the owner of sessions, persistence, tools,
//! and model execution.  This facade owns the version boundary: it exposes the
//! draft ACP v2 lifecycle on stdio and translates application-owned events to
//! the daemon's stable v1 connection.

use std::{
    collections::{HashMap, HashSet},
    path::Path,
    sync::{
        Arc, Mutex,
        atomic::{AtomicU64, Ordering},
    },
};

use agent_client_protocol::schema::{ProtocolVersion, v1, v2 as schema_v2};
use agent_client_protocol::{Agent, ByteStreams, Client, ConnectionTo, Error, Responder, Stdio};
use tokio::sync::Notify;
use tokio_util::compat::{TokioAsyncReadCompatExt, TokioAsyncWriteCompatExt};

use crate::{Result, gateway};

#[derive(Clone, Default)]
struct FacadeState {
    inner: Arc<Mutex<FacadeStateInner>>,
    changed: Arc<Notify>,
    next_message_id: Arc<AtomicU64>,
}

#[derive(Default)]
struct FacadeStateInner {
    frontend: Option<ConnectionTo<Client>>,
    sessions: HashMap<String, SessionState>,
    suppressed_updates: HashSet<String>,
}

#[derive(Clone, Copy, Debug, Default)]
struct SessionState {
    foreground: bool,
    closing: bool,
}

impl FacadeState {
    fn set_frontend(&self, connection: ConnectionTo<Client>) {
        self.inner
            .lock()
            .expect("v2 facade state poisoned")
            .frontend = Some(connection);
    }

    fn frontend(&self) -> Option<ConnectionTo<Client>> {
        self.inner
            .lock()
            .expect("v2 facade state poisoned")
            .frontend
            .clone()
    }

    fn attach(&self, session_id: &str) {
        self.inner
            .lock()
            .expect("v2 facade state poisoned")
            .sessions
            .entry(session_id.to_owned())
            .or_default();
    }

    fn begin_prompt(&self, session_id: &str) -> agent_client_protocol::Result<()> {
        let mut inner = self.inner.lock().expect("v2 facade state poisoned");
        let session = inner.sessions.get_mut(session_id).ok_or_else(|| {
            Error::invalid_params().data(format!("session `{session_id}` is not attached"))
        })?;
        if session.closing {
            return Err(Error::invalid_params().data(format!("session `{session_id}` is closing")));
        }
        if session.foreground {
            return Err(Error::invalid_params().data(format!(
                "session `{session_id}` already has foreground work"
            )));
        }
        session.foreground = true;
        Ok(())
    }

    fn finish_prompt(&self, session_id: &str) {
        if let Some(session) = self
            .inner
            .lock()
            .expect("v2 facade state poisoned")
            .sessions
            .get_mut(session_id)
        {
            session.foreground = false;
        }
        self.changed.notify_waiters();
    }

    fn begin_close(&self, session_id: &str) -> agent_client_protocol::Result<bool> {
        let mut inner = self.inner.lock().expect("v2 facade state poisoned");
        let session = inner.sessions.get_mut(session_id).ok_or_else(|| {
            Error::invalid_params().data(format!("session `{session_id}` is not attached"))
        })?;
        session.closing = true;
        Ok(session.foreground)
    }

    fn remove(&self, session_id: &str) {
        let mut inner = self.inner.lock().expect("v2 facade state poisoned");
        inner.sessions.remove(session_id);
        inner.suppressed_updates.remove(session_id);
        self.changed.notify_waiters();
    }

    async fn wait_until_idle(&self, session_id: &str) {
        let notified = self.changed.notified();
        tokio::pin!(notified);
        loop {
            notified.as_mut().enable();
            let foreground = self
                .inner
                .lock()
                .expect("v2 facade state poisoned")
                .sessions
                .get(session_id)
                .is_some_and(|session| session.foreground);
            if !foreground {
                return;
            }
            notified.as_mut().await;
            notified.set(self.changed.notified());
        }
    }

    fn suppress_updates(&self, session_id: &str, suppress: bool) {
        let mut inner = self.inner.lock().expect("v2 facade state poisoned");
        if suppress {
            inner.suppressed_updates.insert(session_id.to_owned());
        } else {
            inner.suppressed_updates.remove(session_id);
        }
    }

    fn updates_suppressed(&self, session_id: &str) -> bool {
        self.inner
            .lock()
            .expect("v2 facade state poisoned")
            .suppressed_updates
            .contains(session_id)
    }

    fn next_message_id(&self, kind: &str) -> schema_v2::MessageId {
        let next = self.next_message_id.fetch_add(1, Ordering::Relaxed) + 1;
        schema_v2::MessageId::new(format!("deerflow-{kind}-{next}"))
    }
}

fn conversion_error(error: impl std::fmt::Display) -> Error {
    Error::invalid_params().data(error.to_string())
}

fn send_update(
    connection: &ConnectionTo<Client>,
    session_id: impl Into<schema_v2::SessionId>,
    update: schema_v2::SessionUpdate,
) -> agent_client_protocol::Result<()> {
    connection.send_notification(schema_v2::UpdateSessionNotification::new(
        session_id, update,
    ))
}

fn send_ready(state: &FacadeState, session_id: &str) -> agent_client_protocol::Result<()> {
    if let Some(frontend) = state.frontend() {
        send_update(
            &frontend,
            session_id,
            schema_v2::SessionUpdate::StateUpdate(schema_v2::StateUpdate::Idle(
                schema_v2::IdleStateUpdate::new(),
            )),
        )?;
    }
    Ok(())
}

fn send_finished(
    state: &FacadeState,
    session_id: &str,
    stop_reason: schema_v2::StopReason,
    error: Option<String>,
) -> agent_client_protocol::Result<()> {
    state.finish_prompt(session_id);
    let Some(frontend) = state.frontend() else {
        return Ok(());
    };
    let mut idle = schema_v2::IdleStateUpdate::new().stop_reason(stop_reason);
    if let Some(error) = error {
        idle = idle.meta(serde_json::Map::from_iter([(
            "deerflowError".to_owned(),
            serde_json::Value::String(error),
        )]));
    }
    send_update(
        &frontend,
        session_id,
        schema_v2::SessionUpdate::StateUpdate(schema_v2::StateUpdate::Idle(idle)),
    )
}

async fn run_prompt(
    state: FacadeState,
    backend: ConnectionTo<Agent>,
    request: schema_v2::PromptRequest,
    v1_request: v1::PromptRequest,
) -> agent_client_protocol::Result<()> {
    let session_id = request.session_id.to_string();
    let Some(frontend) = state.frontend() else {
        state.finish_prompt(&session_id);
        return Ok(());
    };

    if let Err(error) = send_update(
        &frontend,
        request.session_id.clone(),
        schema_v2::SessionUpdate::UserMessage(
            schema_v2::UserMessage::new(state.next_message_id("user")).content(request.prompt),
        ),
    ) {
        state.finish_prompt(&session_id);
        return Err(error);
    }
    if let Err(error) = send_update(
        &frontend,
        request.session_id,
        schema_v2::SessionUpdate::StateUpdate(schema_v2::StateUpdate::Running(
            schema_v2::RunningStateUpdate::new(),
        )),
    ) {
        state.finish_prompt(&session_id);
        return Err(error);
    }

    match backend.send_request(v1_request).block_task().await {
        Ok(response) => send_finished(
            &state,
            &session_id,
            schema_v2::StopReason::from(response.stop_reason),
            None,
        ),
        Err(error) => send_finished(
            &state,
            &session_id,
            schema_v2::StopReason::Other("_deerflow_error".to_owned()),
            Some(error.to_string()),
        ),
    }
}

async fn serve_frontend(
    state: FacadeState,
    backend: ConnectionTo<Agent>,
    prompt_capabilities: schema_v2::PromptCapabilities,
) -> agent_client_protocol::Result<()> {
    Agent
        .v2()
        .name("deerflow-acp-v2")
        .on_receive_request(
            {
                let state = state.clone();
                let prompt_capabilities = prompt_capabilities.clone();
                async move |request: schema_v2::InitializeRequest,
                            responder: Responder<schema_v2::InitializeResponse>,
                            connection: ConnectionTo<Client>| {
                    state.set_frontend(connection);
                    responder.respond(
                        schema_v2::InitializeResponse::new(
                            request.protocol_version,
                            schema_v2::Implementation::new(
                                "deerflow-acp-v2",
                                env!("CARGO_PKG_VERSION"),
                            )
                            .title("DeerFlow Portable"),
                        )
                        .capabilities(
                            schema_v2::AgentCapabilities::new().session(
                                schema_v2::SessionCapabilities::new()
                                    .prompt(prompt_capabilities.clone()),
                            ),
                        ),
                    )
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let state = state.clone();
                let backend = backend.clone();
                async move |request: schema_v2::NewSessionRequest,
                            responder: Responder<schema_v2::NewSessionResponse>,
                            _connection: ConnectionTo<Client>| {
                    let request =
                        v1::NewSessionRequest::try_from(request).map_err(conversion_error)?;
                    let response = backend.send_request(request).block_task().await?;
                    let session_id = response.session_id.to_string();
                    state.attach(&session_id);
                    responder.respond(schema_v2::NewSessionResponse::new(session_id.clone()))?;
                    send_ready(&state, &session_id)
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let backend = backend.clone();
                async move |request: schema_v2::ListSessionsRequest,
                            responder: Responder<schema_v2::ListSessionsResponse>,
                            _connection: ConnectionTo<Client>| {
                    let request =
                        v1::ListSessionsRequest::try_from(request).map_err(conversion_error)?;
                    let response = backend.send_request(request).block_task().await?;
                    let response = schema_v2::ListSessionsResponse::try_from(response)
                        .map_err(conversion_error)?;
                    responder.respond(response)
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let state = state.clone();
                let backend = backend.clone();
                async move |request: schema_v2::ResumeSessionRequest,
                            responder: Responder<schema_v2::ResumeSessionResponse>,
                            _connection: ConnectionTo<Client>| {
                    if request
                        .replay_from
                        .as_ref()
                        .is_some_and(|value| !matches!(value, schema_v2::ReplayFrom::Start(_)))
                    {
                        return Err(Error::invalid_params().data("unsupported replay cursor"));
                    }
                    let session_id = request.session_id.to_string();
                    let suppress = request.replay_from.is_none();
                    state.suppress_updates(&session_id, suppress);
                    let load =
                        v1::LoadSessionRequest::new(session_id.clone(), request.cwd.into_inner())
                            .additional_directories(
                                request
                                    .additional_directories
                                    .into_iter()
                                    .map(schema_v2::AbsolutePath::into_inner)
                                    .collect(),
                            );
                    let result = backend.send_request(load).block_task().await;
                    state.suppress_updates(&session_id, false);
                    let _response = result?;
                    state.attach(&session_id);
                    responder.respond(schema_v2::ResumeSessionResponse::new())?;
                    send_ready(&state, &session_id)
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let state = state.clone();
                let backend = backend.clone();
                async move |request: schema_v2::CloseSessionRequest,
                            responder: Responder<schema_v2::CloseSessionResponse>,
                            connection: ConnectionTo<Client>| {
                    let session_id = request.session_id.to_string();
                    let had_foreground = state.begin_close(&session_id)?;
                    if had_foreground {
                        backend
                            .send_notification(v1::CancelNotification::new(session_id.clone()))?;
                    }
                    let closing_state = state.clone();
                    let closing_backend = backend.clone();
                    connection.spawn(async move {
                        closing_state.wait_until_idle(&session_id).await;
                        let result = closing_backend
                            .send_request(v1::CloseSessionRequest::new(session_id.clone()))
                            .block_task()
                            .await;
                        match result {
                            Ok(_) => {
                                closing_state.remove(&session_id);
                                responder.respond(schema_v2::CloseSessionResponse::new())
                            }
                            Err(error) => responder.respond_with_error(error),
                        }
                    })
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let state = state.clone();
                let backend = backend.clone();
                async move |request: schema_v2::PromptRequest,
                            responder: Responder<schema_v2::PromptResponse>,
                            connection: ConnectionTo<Client>| {
                    let v1_request =
                        v1::PromptRequest::try_from(request.clone()).map_err(conversion_error)?;
                    let session_id = request.session_id.to_string();
                    state.begin_prompt(&session_id)?;
                    responder.respond(schema_v2::PromptResponse::new())?;
                    let prompt_state = state.clone();
                    let prompt_backend = backend.clone();
                    if let Err(error) = connection.spawn(async move {
                        run_prompt(prompt_state, prompt_backend, request, v1_request).await
                    }) {
                        state.finish_prompt(&session_id);
                        return Err(error);
                    }
                    Ok(())
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_notification(
            {
                let backend = backend.clone();
                async move |notification: schema_v2::CancelSessionNotification,
                            _connection: ConnectionTo<Client>| {
                    backend.send_notification(v1::CancelNotification::new(
                        notification.session_id.to_string(),
                    ))
                }
            },
            agent_client_protocol::on_receive_notification!(),
        )
        .connect_to(Stdio::new())
        .await
}

pub(crate) async fn run(endpoint_path: &Path) -> Result<()> {
    let stream = gateway::connect_daemon(endpoint_path).await?;
    let (reader, writer) = stream.into_split();
    let transport = ByteStreams::new(writer.compat_write(), reader.compat());
    let state = FacadeState::default();

    let updates_state = state.clone();
    let permissions_state = state.clone();
    Client
        .builder()
        .name("deerflow-acp-v2-backend")
        .on_receive_notification(
            async move |notification: v1::SessionNotification, _connection: ConnectionTo<Agent>| {
                let session_id = notification.session_id.to_string();
                if updates_state.updates_suppressed(&session_id) {
                    return Ok(());
                }
                let Some(frontend) = updates_state.frontend() else {
                    return Ok(());
                };
                match schema_v2::SessionUpdate::try_from(notification.update) {
                    Ok(update) => send_update(&frontend, session_id, update),
                    Err(error) => {
                        eprintln!("deerflow-acp: skipped unrepresentable v1 update: {error}");
                        Ok(())
                    }
                }
            },
            agent_client_protocol::on_receive_notification!(),
        )
        .on_receive_request(
            async move |request: v1::RequestPermissionRequest,
                        responder: Responder<v1::RequestPermissionResponse>,
                        _connection: ConnectionTo<Agent>| {
                let Some(frontend) = permissions_state.frontend() else {
                    return responder.respond(v1::RequestPermissionResponse::new(
                        v1::RequestPermissionOutcome::Cancelled,
                    ));
                };
                let request = match schema_v2::RequestPermissionRequest::try_from(request) {
                    Ok(request) => request,
                    Err(error) => {
                        eprintln!("deerflow-acp: could not convert permission request: {error}");
                        return responder.respond(v1::RequestPermissionResponse::new(
                            v1::RequestPermissionOutcome::Cancelled,
                        ));
                    }
                };
                let response = frontend.send_request(request).block_task().await?;
                let response =
                    v1::RequestPermissionResponse::try_from(response).map_err(conversion_error)?;
                responder.respond(response)
            },
            agent_client_protocol::on_receive_request!(),
        )
        .connect_with(transport, async move |backend| {
            let initialize = v1::InitializeRequest::new(ProtocolVersion::V1).client_info(
                v1::Implementation::new("deerflow-acp-v2-backend", env!("CARGO_PKG_VERSION")),
            );
            let initialized = backend.send_request(initialize).block_task().await?;
            if initialized.protocol_version != ProtocolVersion::V1 {
                return Err(Error::internal_error().data(format!(
                    "DeerFlow daemon selected unexpected protocol {}",
                    initialized.protocol_version
                )));
            }
            let prompt_capabilities = schema_v2::PromptCapabilities::try_from(
                initialized.agent_capabilities.prompt_capabilities,
            )
            .map_err(conversion_error)?;
            serve_frontend(state, backend, prompt_capabilities).await
        })
        .await?;
    Ok(())
}
