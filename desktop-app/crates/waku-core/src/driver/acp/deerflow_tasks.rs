//! Read/control extensions use the existing attached ACP connection. They never
//! produce chat errors or submit model prompts, including on timeout. Explicit
//! execution recovery has the same turn/permission lifecycle as a prompt.
use super::*;
use waku_protocol::deerflow_tasks::{Action, Capabilities, Command, Event, Response};

pub(super) struct TaskConnection {
    id: String,
    session: String,
    capabilities: Capabilities,
    pending: Arc<AtomicBool>,
}

pub(super) struct NotificationState {
    connection: String,
    session: String,
    active: Option<(String, String, u64)>,
    parked: Option<(String, u64)>,
    announced: bool,
    recent: std::collections::VecDeque<(String, String)>,
}

pub(super) enum EarlyUpdate {
    Session(SessionNotification),
    Lifecycle(Value),
    Driver(DriverEvent),
}
#[derive(Default)]
pub(super) struct EarlyUpdates {
    pub(super) items: Vec<EarlyUpdate>,
    bytes: usize,
    pub(super) overflow: bool,
}
impl EarlyUpdates {
    pub(super) fn push(&mut self, update: EarlyUpdate) {
        let bytes = match &update {
            EarlyUpdate::Session(update) => serde_json::to_vec(update),
            EarlyUpdate::Lifecycle(update) => serde_json::to_vec(update),
            EarlyUpdate::Driver(update) => Ok(format!("{update:?}").into_bytes()),
        }
        .map(|bytes| bytes.len())
        .unwrap_or(4 * 1024 * 1024);
        if self.items.len() >= 128 || self.bytes.saturating_add(bytes) > 4 * 1024 * 1024 {
            self.overflow = true;
            return;
        }
        self.bytes += bytes;
        self.items.push(update);
    }
}

#[derive(Default)]
pub(super) struct CapturedEvents(std::cell::RefCell<Vec<DriverEvent>>);
impl DriverEventSink for CapturedEvents {
    fn send(&self, event: DriverEvent) -> Result<(), crossbeam_channel::SendError<DriverEvent>> {
        self.0.borrow_mut().push(event);
        Ok(())
    }
}
impl CapturedEvents {
    pub(super) fn append(
        self,
        stream: &mut AcpStreamState,
        events: &impl DriverEventSink,
    ) -> agent_client_protocol::Result<()> {
        for event in self.0.into_inner() {
            receive_update(stream, EarlyUpdate::Driver(event), true, events)?;
        }
        Ok(())
    }
}

pub(super) fn receive_permission(
    stream: &mut AcpStreamState,
    request: RequestPermissionRequest,
    responder: Responder<RequestPermissionResponse>,
    mode: RuntimeMode,
    pending: &PendingPermissions,
    events: &impl DriverEventSink,
) -> agent_client_protocol::Result<()> {
    if stream.deerflow_tasks.is_none() || stream.deerflow_buffering {
        let captured = CapturedEvents::default();
        handle_permission_request(
            request,
            responder,
            ProviderKind::DeerFlow,
            mode,
            pending,
            &captured,
        )?;
        captured.append(stream, events)
    } else {
        handle_permission_request(
            request,
            responder,
            ProviderKind::DeerFlow,
            mode,
            pending,
            events,
        )
    }
}

pub(super) fn receive_update(
    stream: &mut AcpStreamState,
    update: EarlyUpdate,
    has_prompt: bool,
    events: &impl DriverEventSink,
) -> agent_client_protocol::Result<()> {
    let deferred_lifecycle = has_prompt
        && matches!(&update,EarlyUpdate::Lifecycle(params) if stream.deerflow_tasks.as_ref().is_some_and(|state| state.valid_new_start(params) || state.valid_terminal_notice(params)));
    if stream.deerflow_tasks.is_none() || stream.deerflow_buffering || deferred_lifecycle {
        stream.deerflow_buffering |= deferred_lifecycle;
        stream.deerflow_early.push(update);
        if stream.deerflow_early.overflow {
            return Err(approval_error(
                "DeerFlow automatic output exceeded the pending conversation buffer. Reconnect to load saved output.",
            ));
        }
        return Ok(());
    }
    match update {
        EarlyUpdate::Lifecycle(params) => {
            if let Some(state) = &mut stream.deerflow_tasks {
                state.update(&params, has_prompt, events);
            }
        }
        EarlyUpdate::Session(notification) => {
            handle_session_update(ProviderKind::DeerFlow, notification, events, stream)?
        }
        EarlyUpdate::Driver(event) => {
            let _ = events.send(event);
        }
    }
    Ok(())
}

pub(super) fn flush_early_updates(
    stream: &mut AcpStreamState,
    has_prompt: bool,
    events: &impl DriverEventSink,
) -> agent_client_protocol::Result<()> {
    let early = std::mem::take(&mut stream.deerflow_early);
    stream.deerflow_buffering = false;
    if early.overflow {
        return Err(approval_error(
            "Too many DeerFlow updates arrived while attaching. Reconnect to load the saved conversation.",
        ));
    }
    for update in early.items {
        receive_update(stream, update, has_prompt, events)?;
    }
    Ok(())
}

/// ACP ends the request even when the durable execution is waiting for a child.
/// Keep that execution's UI turn open until its automatic continuation settles.
pub(super) fn finish_prompt(
    result: agent_client_protocol::Result<PromptResponse>,
    stream: &mut AcpStreamState,
    events: &impl DriverEventSink,
) -> bool {
    if let Some(state) = &mut stream.deerflow_tasks {
        state.parked = None;
        if let Ok(response) = &result
            && response.stop_reason == StopReason::EndTurn
            && let Some(execution) = response
                .meta
                .as_ref()
                .and_then(|meta| meta.get("deerflow"))
                .and_then(|meta| meta.get("execution"))
            && state.park_execution(execution, events)
        {
            return true;
        }
    }
    super::finish_prompt(result, None, events)
}

impl NotificationState {
    fn park_execution(&mut self, execution: &Value, events: &impl DriverEventSink) -> bool {
        let (Some(run), Some(version)) =
            (execution["runId"].as_str(), execution["version"].as_u64())
        else {
            return false;
        };
        if execution["sessionId"] != self.session
            || execution["status"] != "waiting_input"
            || run.is_empty()
            || run.len() > 256
            || version == 0
            || version > i64::MAX as u64
        {
            return false;
        }
        self.parked = Some((run.into(), version));
        let _ = events.send(DriverEvent::TurnParked);
        true
    }

    fn finish_automatic(&mut self, params: &Value, events: &impl DriverEventSink) {
        let execution = &params["result"]["execution"];
        self.parked = None;
        let success = !execution.is_null()
            && params.get("error").is_none_or(Value::is_null)
            && !matches!(
                execution["status"].as_str(),
                Some("failed" | "needs_reconciliation")
            )
            && matches!(
                params["result"]["stopReason"].as_str(),
                Some("end_turn" | "cancelled")
            );
        if success
            && params["result"]["stopReason"] == "end_turn"
            && self.park_execution(execution, events)
        {
            return;
        }
        let _ = events.send(DriverEvent::TurnFinished {
            success,
            summary: (!success).then(|| "自动执行已停止，请刷新任务状态查看原因。".into()),
        });
    }

    pub(super) fn steering_target(&self) -> Option<(String, u64)> {
        self.active
            .as_ref()
            .map(|(run, _, version)| (run.clone(), *version))
    }
    fn valid_terminal_notice(&self, params: &Value) -> bool {
        let execution = &params["result"]["execution"];
        params["sessionId"] == self.session
            && params["phase"] == "finished"
            && params["runId"].as_str().is_none_or(str::is_empty)
            && params["operationId"].as_str().is_none_or(str::is_empty)
            && execution["sessionId"] == self.session
            && execution["runId"]
                .as_str()
                .is_some_and(|run| !run.is_empty() && run.len() <= 256)
            && execution["version"]
                .as_u64()
                .is_some_and(|version| version > 0 && version <= i64::MAX as u64)
            && matches!(
                execution["status"].as_str(),
                Some("completed" | "cancelled" | "failed" | "needs_reconciliation")
            )
    }
    fn valid_new_start(&self, params: &Value) -> bool {
        let execution = &params["execution"];
        let (Some(run), Some(operation)) =
            (params["runId"].as_str(), params["operationId"].as_str())
        else {
            return false;
        };
        params["sessionId"] == self.session
            && params["phase"] == "started"
            && execution["runId"] == run
            && !run.is_empty()
            && run.len() <= 256
            && !operation.is_empty()
            && operation.len() <= 256
            && execution["version"]
                .as_u64()
                .is_some_and(|version| version > 0 && version <= i64::MAX as u64)
            && self.active.is_none()
            && !self
                .recent
                .iter()
                .any(|(r, op)| r == run && op == operation)
    }
    #[cfg(test)]
    fn after_prompt(&mut self, events: &impl DriverEventSink) {
        if self.active.is_some() && !self.announced {
            self.announced = true;
            let _ = events.send(DriverEvent::TurnStarted);
        }
    }
    pub(super) fn invalidate(&self, events: &impl DriverEventSink) {
        let _ = events.send(DriverEvent::DeerFlowTasks(Event::Invalidated {
            connection_id: self.connection.clone(),
            session_id: self.session.clone(),
        }));
    }

    pub(super) fn update(
        &mut self,
        params: &Value,
        has_prompt: bool,
        events: &impl DriverEventSink,
    ) {
        if params["sessionId"] != self.session {
            return;
        }
        self.invalidate(events);
        let started = params["phase"] == "started";
        let execution = if started {
            &params["execution"]
        } else {
            &params["result"]["execution"]
        };
        // A failed dependency can end a parked execution without starting a
        // new attempt. Bind that terminal notice to the parked run/version;
        // unrelated or late notices must not finish another user's prompt.
        if !has_prompt
            && self.active.is_none()
            && self.valid_terminal_notice(params)
            && self.parked.as_ref().is_some_and(|(run, version)| {
                execution["runId"] == *run
                    && execution["version"]
                        .as_u64()
                        .is_some_and(|next| next >= *version && next <= i64::MAX as u64)
            })
        {
            self.finish_automatic(params, events);
            return;
        }
        let (Some(run), Some(operation)) =
            (params["runId"].as_str(), params["operationId"].as_str())
        else {
            return;
        };
        if run.is_empty() || run.len() > 256 || operation.is_empty() || operation.len() > 256 {
            return;
        }
        if started {
            let Some(version) = execution["version"]
                .as_u64()
                .filter(|v| *v > 0 && *v <= i64::MAX as u64)
            else {
                return;
            };
            if execution["runId"] != run
                || self.active.is_some()
                || self.recent.iter().any(|(r, a)| r == run && a == operation)
            {
                return;
            }
            self.parked = None;
            self.active = Some((run.into(), operation.into(), version));
            self.announced = !has_prompt;
            if self.announced {
                let _ = events.send(DriverEvent::TurnStarted);
            }
        } else if params["phase"] == "finished"
            && self.active.as_ref().is_some_and(|(r, a, v)| {
                r == run
                    && a == operation
                    && (execution.is_null()
                        || (execution["runId"] == run
                            && execution["version"].as_u64().is_some_and(|version| {
                                version >= *v && version <= i64::MAX as u64
                            })))
            })
        {
            self.active = None;
            self.recent.push_back((run.into(), operation.into()));
            if self.recent.len() > 32 {
                self.recent.pop_front();
            }
            // A late automatic completion never settles a new user prompt.
            if !has_prompt {
                self.finish_automatic(params, events);
            }
        }
    }
}

fn capabilities(value: &Value) -> Capabilities {
    let tree = &value["_meta"]["deerflow"]["taskTree"];
    let background = &value["_meta"]["deerflow"]["background"];
    let executions = &value["_meta"]["deerflow"]["executions"];
    let receipts = &value["_meta"]["deerflow"]["toolReceipts"];
    let supported = tree["version"] == 1
        && tree["listMethod"] == "_deerflow/tasks/tree"
        && tree["getMethod"] == "_deerflow/tasks/tree/get";
    Capabilities {
        session_budget: value["_meta"]["deerflow"]["sessionBudget"]["version"] == 1
            && value["_meta"]["deerflow"]["sessionBudget"]["getMethod"]
                == "_deerflow/session/budget/get"
            && value["_meta"]["deerflow"]["sessionBudget"]["setMethod"]
                == "_deerflow/session/budget/set",
        tree: supported,
        cancel: supported && tree["cancelMethod"] == "_deerflow/tasks/tree/cancel",
        permission: background["version"] == 1
            && background["permissionMethod"] == "_deerflow/tasks/permission/get"
            && background["approveMethod"] == "_deerflow/tasks/approve",
        resume: background["version"] == 1
            && background["resumeMethod"] == "_deerflow/tasks/resume",
        resource_authorize: background["version"] == 1
            && background["resourceAuthorizeMethod"] == "_deerflow/tasks/resources/authorize",
        executions: executions["version"] == 1
            && executions["getMethod"] == "_deerflow/executions/get"
            && executions["resumeMethod"] == "_deerflow/executions/resume",
        artifact_read: supported
            && tree["artifactReadMethod"] == "_deerflow/tasks/tree/artifacts/read",
        artifact_list: supported
            && tree["artifactListMethod"] == "_deerflow/tasks/tree/artifacts/list",
        tool_receipts: receipts["version"] == 1
            && receipts["listMethod"] == "_deerflow/tool_receipts/list"
            && receipts["pagination"] == "live_keyset",
    }
}

pub(super) fn failed(events: &DriverEventSender, command: Command, code: &str, message: &str) {
    let _ = events.send(DriverEvent::DeerFlowTasks(Event::Failed {
        command,
        code: code.into(),
        message: message.into(),
    }));
}

impl TaskConnection {
    pub(super) fn notifications(&self) -> NotificationState {
        NotificationState {
            connection: self.id.clone(),
            session: self.session.clone(),
            active: None,
            parked: None,
            announced: false,
            recent: Default::default(),
        }
    }
    pub(super) fn new(
        initialize: &InitializeResponse,
        session: &str,
        events: &DriverEventSender,
    ) -> Self {
        let state = Self {
            id: uuid::Uuid::new_v4().to_string(),
            session: session.into(),
            capabilities: capabilities(&serde_json::to_value(initialize).unwrap_or(Value::Null)),
            pending: Arc::new(AtomicBool::new(false)),
        };
        let _ = events.send(DriverEvent::DeerFlowTasks(Event::Ready {
            connection_id: state.id.clone(),
            session_id: state.session.clone(),
            capabilities: state.capabilities.clone(),
        }));
        state
    }

    fn request(&self, command: &Command) -> anyhow::Result<(&'static str, Value)> {
        anyhow::ensure!(
            command.connection_id == self.id
                && command.session_id == self.session
                && uuid::Uuid::parse_str(&command.request_id).is_ok(),
            "任务连接已变化，请刷新"
        );
        let receipt_action = matches!(command.action, Action::ReceiptList { .. });
        anyhow::ensure!(
            if matches!(command.action, Action::BudgetGet | Action::BudgetSet { .. }) {
                self.capabilities.session_budget
            } else if receipt_action {
                self.capabilities.tool_receipts
            } else {
                self.capabilities.tree
            },
            "当前服务未声明所需控制能力"
        );
        let (method, mut params) = match &command.action {
            Action::BudgetGet => ("_deerflow/session/budget/get", json!({})),
            Action::BudgetSet {
                expected_version,
                operation_id,
                limits,
            } => {
                anyhow::ensure!(
                    *expected_version > 0
                        && *expected_version <= i64::MAX as u64
                        && uuid::Uuid::parse_str(operation_id).is_ok()
                        && limits.max_tokens <= i64::MAX as u64
                        && limits.max_model_calls <= i32::MAX as u64
                        && limits.max_tool_calls <= i32::MAX as u64,
                    "任务预算参数无效"
                );
                (
                    "_deerflow/session/budget/set",
                    json!({"expectedVersion":expected_version,"operationId":operation_id,"limits":limits}),
                )
            }
            Action::ReceiptList {
                cursor,
                limit,
                uncertain_only,
            } => {
                anyhow::ensure!(
                    (1..=20).contains(limit) && cursor.as_ref().is_none_or(|c| c.len() <= 8192),
                    "回执分页参数无效"
                );
                let mut params = json!({"limit":limit,"uncertainOnly":uncertain_only});
                if let Some(cursor) = cursor {
                    params["cursor"] = json!(cursor);
                }
                ("_deerflow/tool_receipts/list", params)
            }
            Action::List {
                root_id,
                cursor,
                limit,
            } => {
                anyhow::ensure!(
                    (1..=100).contains(limit)
                        && root_id
                            .as_ref()
                            .is_none_or(|id| !id.is_empty() && id.len() <= 4096)
                        && cursor.as_ref().is_none_or(|c| c.len() <= 16384),
                    "任务分页参数无效"
                );
                let mut value = json!({"limit":limit});
                if let Some(root) = root_id {
                    value["rootId"] = json!(root);
                }
                if let Some(cursor) = cursor {
                    value["cursor"] = json!(cursor);
                }
                ("_deerflow/tasks/tree", value)
            }
            Action::Get { root_id, node_id } => (
                "_deerflow/tasks/tree/get",
                json!({"rootId":root_id,"nodeId":node_id}),
            ),
            Action::Cancel {
                root_id,
                node_id,
                scope,
                expected_version,
            } => {
                anyhow::ensure!(
                    self.capabilities.cancel && *expected_version > 0,
                    "此节点暂不可取消"
                );
                anyhow::ensure!(
                    (matches!(scope, waku_protocol::deerflow_tasks::CancelScope::Root))
                        == (*node_id == format!("root/{root_id}")),
                    "取消范围与节点不一致"
                );
                (
                    "_deerflow/tasks/tree/cancel",
                    json!({"rootId":root_id,"nodeId":node_id,"expectedVersion":expected_version}),
                )
            }
            Action::BackgroundGet { task_id } => ("_deerflow/tasks/get", json!({"taskId":task_id})),
            Action::Permission {
                task_id,
                interaction_id,
                task_version,
                intent_id,
            } => {
                anyhow::ensure!(
                    self.capabilities.permission && *task_version > 0,
                    "当前服务未提供工具审批详情"
                );
                (
                    "_deerflow/tasks/permission/get",
                    json!({"taskId":task_id,"interactionId":interaction_id,"taskVersion":task_version,"intentId":intent_id}),
                )
            }
            Action::Approve {
                task_id,
                interaction_id,
                task_version,
                decisions,
            } => {
                anyhow::ensure!(
                    self.capabilities.permission
                        && *task_version > 0
                        && !decisions.is_empty()
                        && decisions.len() <= 128,
                    "工具审批参数无效"
                );
                (
                    "_deerflow/tasks/approve",
                    json!({"taskId":task_id,"approval":{"id":interaction_id,"taskVersion":task_version,"decisions":decisions}}),
                )
            }
            Action::Resume { task_id, version } => {
                anyhow::ensure!(
                    self.capabilities.resume && *version > 0,
                    "当前任务暂不可恢复"
                );
                (
                    "_deerflow/tasks/resume",
                    json!({"taskId":task_id,"version":version}),
                )
            }
            Action::AuthorizeResources {
                task_id,
                task_version,
                resource_version,
            } => {
                anyhow::ensure!(
                    self.capabilities.resource_authorize
                        && *task_version > 0
                        && *resource_version > 0,
                    "当前任务资源暂不可重新授权"
                );
                (
                    "_deerflow/tasks/resources/authorize",
                    json!({"taskId":task_id,"taskVersion":task_version,"resourceVersion":resource_version}),
                )
            }
            Action::ExecutionGet { run_id } => {
                anyhow::ensure!(
                    self.capabilities.executions && !run_id.is_empty() && run_id.len() <= 256,
                    "当前服务未提供执行恢复状态"
                );
                ("_deerflow/executions/get", json!({"runId":run_id}))
            }
            Action::ArtifactList {
                root_id,
                node_id,
                cursor,
                limit,
            } => {
                anyhow::ensure!(
                    self.capabilities.artifact_list
                        && !root_id.is_empty()
                        && root_id.len() <= 4096
                        && !node_id.is_empty()
                        && node_id.len() <= 4096
                        && cursor.as_ref().is_none_or(|c| c.len() <= 16384)
                        && (1..=100).contains(limit),
                    "产物分页参数无效"
                );
                let mut params = json!({"rootId":root_id,"nodeId":node_id,"limit":limit});
                if let Some(cursor) = cursor {
                    params["cursor"] = json!(cursor);
                }
                ("_deerflow/tasks/tree/artifacts/list", params)
            }
            Action::ArtifactRead {
                root_id,
                node_id,
                artifact_id,
                offset,
                limit,
                expected_sha256,
            } => {
                anyhow::ensure!(
                    self.capabilities.artifact_read
                        && !root_id.is_empty()
                        && root_id.len() <= 4096
                        && !node_id.is_empty()
                        && node_id.len() <= 4096
                        && !artifact_id.is_empty()
                        && artifact_id.len() <= 4096
                        && *offset <= i64::MAX as u64
                        && (1..=1_048_576).contains(limit)
                        && (*offset == 0 || expected_sha256.is_some())
                        && expected_sha256.as_ref().is_none_or(|hash| hash.len() == 64
                            && hash
                                .bytes()
                                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))),
                    "产物读取请求无效"
                );
                let mut params = json!({"rootId":root_id,"nodeId":node_id,"artifactId":artifact_id,"offset":offset,"limit":limit});
                if let Some(hash) = expected_sha256 {
                    params["expectedSha256"] = json!(hash);
                }
                ("_deerflow/tasks/tree/artifacts/read", params)
            }
            Action::ExecutionResume {
                run_id,
                expected_version,
            } => {
                anyhow::ensure!(
                    self.capabilities.executions
                        && !run_id.is_empty()
                        && run_id.len() <= 256
                        && *expected_version > 0
                        && *expected_version <= i64::MAX as u64,
                    "当前执行暂不可恢复"
                );
                (
                    "_deerflow/executions/resume",
                    json!({"runId":run_id,"expectedVersion":expected_version}),
                )
            }
        };
        params["sessionId"] = json!(self.session);
        anyhow::ensure!(
            serde_json::to_vec(&params)?.len() <= 128 * 1024,
            "任务请求超过大小限制"
        );
        Ok((method, params))
    }

    pub(super) fn dispatch(
        &self,
        connection: &ConnectionTo<Agent>,
        command: Command,
        events: &DriverEventSender,
    ) {
        if matches!(command.action, Action::ExecutionResume { .. }) {
            failed(
                events,
                command,
                "invalid_request",
                "恢复执行必须通过会话执行通道。",
            );
            return;
        }
        let (method, params) = match self.request(&command) {
            Ok(request) => request,
            Err(error) => {
                failed(events, command, "invalid_request", &error.to_string());
                return;
            }
        };
        if self.pending.swap(true, Ordering::AcqRel) {
            failed(
                events,
                command,
                "busy",
                "已有任务操作正在处理，请稍后刷新。",
            );
            return;
        }
        let request = match UntypedMessage::new(method, params) {
            Ok(request) => request,
            Err(_) => {
                self.pending.store(false, Ordering::Release);
                failed(events, command, "invalid_request", "任务请求无法编码。");
                return;
            }
        };
        let complete = Arc::new(AtomicBool::new(false));
        let callback_complete = complete.clone();
        let callback_pending = self.pending.clone();
        let callback_events = events.clone();
        let callback_command = command.clone();
        let registered = connection
            .send_request(request)
            .on_receiving_result(async move |reply| {
                if callback_complete.swap(true, Ordering::AcqRel) {
                    return Ok(());
                }
                callback_pending.store(false, Ordering::Release);
                match reply {
                    Ok(data) => match response(&callback_command.action, data) {
                        Ok(response) => {
                            let _ =
                                callback_events.send(DriverEvent::DeerFlowTasks(Event::Reply {
                                    command: callback_command,
                                    response,
                                }));
                        }
                        Err(_) => failed(
                            &callback_events,
                            callback_command,
                            "invalid_response",
                            "任务响应无效，请刷新当前任务。",
                        ),
                    },
                    Err(error) => failed(
                        &callback_events,
                        callback_command,
                        &error.code.to_string(),
                        "任务操作未完成。状态可能已变化，请刷新后重试；聊天可以继续。",
                    ),
                }
                Ok(())
            });
        if registered.is_err() {
            complete.store(true, Ordering::Release);
            self.pending.store(false, Ordering::Release);
            failed(
                events,
                command,
                "connection_closed",
                "任务连接已断开，请重新连接后刷新。",
            );
            return;
        }
        let events = events.clone();
        let pending = self.pending.clone();
        smol::spawn(async move {
            smol::Timer::after(Duration::from_secs(15)).await;
            if !complete.swap(true, Ordering::AcqRel) {
                pending.store(false, Ordering::Release);
                failed(
                    &events,
                    command,
                    "timeout",
                    "任务操作等待超时，实际结果尚未确认。请刷新状态后再操作。",
                );
            }
        })
        .detach();
    }

    pub(super) fn resume_execution(
        &self,
        connection: &ConnectionTo<Agent>,
        command: Command,
        prompt_requests: &PendingPromptRequests,
        events: &DriverEventSender,
        stream_state: &Arc<Mutex<AcpStreamState>>,
    ) {
        let request = self.request(&command).and_then(|(method, params)| {
            anyhow::ensure!(
                matches!(command.action, Action::ExecutionResume { .. }),
                "invalid recovery action"
            );
            Ok(UntypedMessage::new(method, params)?)
        });
        let request = match request {
            Ok(request) => request,
            Err(error) => {
                failed(events, command, "invalid_request", &error.to_string());
                return;
            }
        };
        if !prompt_requests.lock().is_empty() || self.pending.swap(true, Ordering::AcqRel) {
            failed(
                events,
                command,
                "busy",
                "当前会话仍在执行，请先处理已有审批或等待执行结束。",
            );
            return;
        }
        stream_state.lock().produced_content = false;
        let _ = events.send(DriverEvent::TurnStarted);
        let sent = connection.send_request(request);
        let request_id = sent.id().clone();
        prompt_requests
            .lock()
            .insert(request_id.clone(), None, self.session.clone());
        let callback_requests = prompt_requests.clone();
        let callback_id = request_id.clone();
        let callback_pending = self.pending.clone();
        let callback_events = events.clone();
        let callback_command = command.clone();
        // No management timeout: the saved permission may wait for a human.
        let callback_stream = stream_state.clone();
        // session/update and session/request_permission use existing handlers.
        let registered = sent.on_receiving_result(async move |result| {
            callback_pending.store(false, Ordering::Release);
            if settle_prompt_request(&callback_requests, &callback_id) {
                let result = result.and_then(|value| {
                    serde_json::from_value::<PromptResponse>(value)
                        .map_err(|_| approval_error("Invalid execution resume response"))
                });
                match result {
                    Ok(response) => {
                        finish_prompt(Ok(response), &mut callback_stream.lock(), &callback_events);
                        let _ = callback_events.send(DriverEvent::DeerFlowTasks(Event::Reply {
                            command: callback_command,
                            response: Response::ExecutionFinished,
                        }));
                    }
                    Err(error) => {
                        failed(
                            &callback_events,
                            callback_command,
                            &error.code.to_string(),
                            "执行未能恢复。请刷新执行状态；待审批工具不会被自动允许。",
                        );
                        let _ = callback_events.send(DriverEvent::TurnFinished {
                            success: false,
                            summary: None,
                        });
                    }
                }
                flush_early_updates(&mut callback_stream.lock(), false, &callback_events)?;
            }
            Ok(())
        });
        if registered.is_err() {
            prompt_requests.lock().settle_request(&request_id);
            self.pending.store(false, Ordering::Release);
            failed(
                events,
                command,
                "connection_closed",
                "执行恢复连接已断开，请重连后读取实际状态。",
            );
            let _ = events.send(DriverEvent::TurnFinished {
                success: false,
                summary: None,
            });
        }
    }
}

fn response(action: &Action, data: Value) -> anyhow::Result<Response> {
    anyhow::ensure!(
        serde_json::to_vec(&data)?.len() <= 4 * 1024 * 1024,
        "task response exceeds bounds"
    );
    Ok(match action {
        Action::BudgetGet | Action::BudgetSet { .. } => {
            Response::Budget(serde_json::from_value(data)?)
        }
        Action::ReceiptList { .. } => Response::Receipts(serde_json::from_value(data)?),
        Action::List { .. } => Response::Page(serde_json::from_value(data)?),
        Action::Get { .. } | Action::Cancel { .. } => Response::Node(serde_json::from_value(data)?),
        Action::Permission { .. } => Response::Permission(data),
        Action::ExecutionGet { .. } => Response::Execution(serde_json::from_value(data)?),
        Action::ArtifactRead { .. } => Response::Artifact(serde_json::from_value(data)?),
        Action::ArtifactList { .. } => Response::Artifacts(serde_json::from_value(data)?),
        Action::ExecutionResume { .. } => {
            anyhow::bail!("execution recovery must use turn lifecycle")
        }
        Action::BackgroundGet { .. }
        | Action::Approve { .. }
        | Action::Resume { .. }
        | Action::AuthorizeResources { .. } => Response::Background(serde_json::from_value(data)?),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn deerflow_receipts_are_read_only_and_bound_to_the_owning_connection() {
        let caps = capabilities(
            &json!({"_meta":{"deerflow":{"toolReceipts":{"version":1,"pagination":"live_keyset","listMethod":"_deerflow/tool_receipts/list"}}}}),
        );
        assert!(caps.tool_receipts && !caps.tree);
        let state = TaskConnection {
            id: "connection".into(),
            session: "session".into(),
            capabilities: caps,
            pending: Arc::new(AtomicBool::new(false)),
        };
        let mut command = Command {
            request_id: uuid::Uuid::new_v4().to_string(),
            connection_id: state.id.clone(),
            session_id: state.session.clone(),
            action: Action::ReceiptList {
                cursor: None,
                limit: 10,
                uncertain_only: true,
            },
        };
        let (method, params) = state.request(&command).unwrap();
        assert_eq!(method, "_deerflow/tool_receipts/list");
        assert_eq!(params["sessionId"], "session");
        command.session_id = "foreign".into();
        assert!(state.request(&command).is_err());
        command.session_id = state.session.clone();
    }
    #[test]
    fn deerflow_automatic_stream_and_permission_wait_for_the_old_prompt_response() {
        for (finish_before_reply, with_permission, waiting) in [
            (false, false, false),
            (true, false, false),
            (false, true, false),
            (false, false, true),
            (true, false, true),
            (false, true, true),
        ] {
            let start = json!({"sessionId":"session","phase":"started","runId":"automatic","operationId":"operation","execution":{"runId":"automatic","version":2}});
            let finish = json!({"sessionId":"session","phase":"finished","runId":"automatic","operationId":"operation","result":{"stopReason":"end_turn","execution":{"runId":"automatic","version":3}}});
            let agent=Agent.builder().on_receive_request(async move |_request:PromptRequest,responder:Responder<PromptResponse>,connection:ConnectionTo<Client>| {
                connection.send_notification(UntypedMessage::new("_deerflow/executions/updated",start.clone())?)?;
                connection.send_notification(UntypedMessage::new("session/update",json!({"sessionId":"session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"automatic text"}}}))?)?;
                connection.send_notification(UntypedMessage::new("session/update",json!({"sessionId":"session","update":{"sessionUpdate":"tool_call","toolCallId":"automatic-tool","title":"automatic tool","kind":"other","status":"in_progress"}}))?)?;
                if with_permission {
                    let permission:RequestPermissionRequest=serde_json::from_value(json!({"sessionId":"session","toolCall":{"toolCallId":"permission-tool","title":"write_file","kind":"edit","status":"pending"},"options":[{"optionId":"reject","name":"Reject","kind":"reject_once"}]}))?;
                    let after=connection.clone();let finish=finish.clone();
                    connection.send_request(permission).on_receiving_result(async move |result| {
                        assert_eq!(serde_json::to_value(result?)?["outcome"]["optionId"],"reject");
                        after.send_notification(UntypedMessage::new("_deerflow/executions/updated",finish)?)
                    })?;
                } else if finish_before_reply {connection.send_notification(UntypedMessage::new("_deerflow/executions/updated",finish.clone())?)?;}
                let response = if waiting {
                    serde_json::from_value(json!({
                        "stopReason": "end_turn",
                        "_meta": {"deerflow": {"execution": {
                            "sessionId": "session", "runId": "automatic", "version": 1,
                            "status": "waiting_input", "waitingTasks": ["research"], "resumable": true
                        }}}
                    }))?
                } else {
                    PromptResponse::new(StopReason::EndTurn)
                };
                responder.respond(response)?;
                if !with_permission && !finish_before_reply {connection.send_notification(UntypedMessage::new("_deerflow/executions/updated",finish.clone())?)?;}
                Ok(())
            },agent_client_protocol::on_receive_request!());
            let (events, received) = crate::driver::test_event_channel();
            let requests = Arc::new(Mutex::new(PendingPrompts::default()));
            let permissions: PendingPermissions = Arc::new(Mutex::new(HashMap::new()));
            let stream = Arc::new(Mutex::new(AcpStreamState::default()));
            stream.lock().deerflow_tasks = Some(NotificationState {
                connection: "connection".into(),
                session: "session".into(),
                active: None,
                parked: None,
                announced: false,
                recent: Default::default(),
            });
            let client = Client
                .builder()
                .on_receive_notification(
                    {
                        let stream = stream.clone();
                        let requests = requests.clone();
                        let events = events.clone();
                        async move |notification: SessionNotification, _| {
                            receive_update(
                                &mut stream.lock(),
                                EarlyUpdate::Session(notification),
                                !requests.lock().is_empty(),
                                &events,
                            )
                        }
                    },
                    agent_client_protocol::on_receive_notification!(),
                )
                .on_receive_notification(
                    {
                        let stream = stream.clone();
                        let requests = requests.clone();
                        let events = events.clone();
                        async move |notification: UntypedMessage, _| {
                            receive_update(
                                &mut stream.lock(),
                                EarlyUpdate::Lifecycle(notification.params().clone()),
                                !requests.lock().is_empty(),
                                &events,
                            )
                        }
                    },
                    agent_client_protocol::on_receive_notification!(),
                )
                .on_receive_request(
                    {
                        let stream = stream.clone();
                        let permissions = permissions.clone();
                        let events = events.clone();
                        async move |request: RequestPermissionRequest, responder, _| {
                            receive_permission(
                                &mut stream.lock(),
                                request,
                                responder,
                                RuntimeMode::FullAccess,
                                &permissions,
                                &events,
                            )
                        }
                    },
                    agent_client_protocol::on_receive_request!(),
                );
            smol::block_on(smol::future::or(
                client.connect_with(agent, async move |connection| {
                    events.send(DriverEvent::TurnStarted).unwrap();
                    send_prompt(
                        &connection,
                        &SessionId::new("session"),
                        "old prompt".into(),
                        &[],
                        &requests,
                        &events,
                        ProviderKind::DeerFlow,
                        "session",
                        None,
                        super::super::super::title_refresh::NativeTitleRefresh::default(),
                        stream.clone(),
                    )?;
                    let mut sequence = vec![];
                    let mut finishes = 0;
                    while finishes < if waiting { 1 } else { 2 } {
                        if let Ok(event) = received.try_recv() {
                            match &event {
                                DriverEvent::TurnStarted => sequence.push("started"),
                                DriverEvent::TurnParked => sequence.push("parked"),
                                DriverEvent::TurnFinished { .. } => {
                                    finishes += 1;
                                    sequence.push("finished");
                                }
                                DriverEvent::TextDelta(text) => {
                                    assert_eq!(text, "automatic text");
                                    sequence.push("text");
                                }
                                DriverEvent::RichActivity(_) => sequence.push("tool"),
                                DriverEvent::Permission { request_id, .. } => {
                                    sequence.push("permission");
                                    permissions.lock().remove(request_id).unwrap().respond(
                                        RequestPermissionResponse::new(
                                            RequestPermissionOutcome::Selected(
                                                SelectedPermissionOutcome::new("reject"),
                                            ),
                                        ),
                                    )?;
                                }
                                DriverEvent::Error(error) => {
                                    panic!("unexpected chat error {error}")
                                }
                                _ => {}
                            }
                        } else {
                            smol::Timer::after(Duration::from_millis(5)).await;
                        }
                    }
                    assert_eq!(
                        sequence,
                        if with_permission {
                            vec![
                                "started",
                                if waiting { "parked" } else { "finished" },
                                "started",
                                "text",
                                "tool",
                                "permission",
                                "finished",
                            ]
                        } else {
                            vec![
                                "started",
                                if waiting { "parked" } else { "finished" },
                                "started",
                                "text",
                                "tool",
                                "finished",
                            ]
                        }
                    );
                    assert!(permissions.lock().is_empty());
                    Ok(())
                }),
                async {
                    smol::Timer::after(Duration::from_secs(5)).await;
                    Err(approval_error("automatic interleaving test timed out"))
                },
            ))
            .unwrap();
        }
    }
    #[test]
    fn deerflow_attach_buffers_automatic_updates_in_order_and_rejects_overflow() {
        let (events, received) = crate::driver::test_event_channel();
        let mut stream = AcpStreamState::default();
        stream.deerflow_early.push(EarlyUpdate::Lifecycle(json!({"sessionId":"session","phase":"started","runId":"run","operationId":"operation","execution":{"runId":"run","version":2}})));
        stream.deerflow_early.push(EarlyUpdate::Session(serde_json::from_value(json!({"sessionId":"session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"automatic output"}}})).unwrap()));
        stream.deerflow_early.push(EarlyUpdate::Lifecycle(json!({"sessionId":"session","phase":"finished","runId":"run","operationId":"operation","result":{"stopReason":"end_turn","execution":{"runId":"run","version":3}}})));
        stream.deerflow_tasks = Some(NotificationState {
            connection: "connection".into(),
            session: "session".into(),
            active: None,
            parked: None,
            announced: false,
            recent: Default::default(),
        });
        flush_early_updates(&mut stream, false, &events).unwrap();
        let output: Vec<_> = received
            .try_iter()
            .filter(|event| !matches!(event, DriverEvent::DeerFlowTasks(_)))
            .collect();
        assert_eq!(output.len(), 3);
        assert!(matches!(output[0], DriverEvent::TurnStarted));
        assert!(matches!(&output[1], DriverEvent::TextDelta(text) if text == "automatic output"));
        assert!(matches!(
            output[2],
            DriverEvent::TurnFinished { success: true, .. }
        ));
        for _ in 0..129 {
            stream
                .deerflow_early
                .push(EarlyUpdate::Lifecycle(json!({})));
        }
        assert_eq!(stream.deerflow_early.items.len(), 128);
        assert!(flush_early_updates(&mut stream, false, &events).is_err());
        assert!(stream.deerflow_early.items.is_empty());
    }
    #[test]
    fn deerflow_automatic_updates_bind_operation_and_never_finish_a_new_prompt() {
        let (events, received) = crate::driver::test_event_channel();
        let mut state = NotificationState {
            connection: "connection".into(),
            session: "session".into(),
            active: None,
            parked: None,
            announced: false,
            recent: Default::default(),
        };
        let start = json!({"sessionId":"session","phase":"started","operationId":"operation","runId":"run","execution":{"runId":"run","attemptId":"attempt1","version":7}});
        let mut finish = json!({"sessionId":"session","phase":"finished","operationId":"operation","runId":"run","result":{"stopReason":"end_turn","execution":{"runId":"run","attemptId":"attempt2","version":11}}});
        let mut foreign = start.clone();
        foreign["sessionId"] = json!("foreign");
        state.update(&foreign, false, &events);
        assert!(received.try_recv().is_err());
        state.update(&start, false, &events);
        state.update(&start, false, &events);
        finish["operationId"] = json!("old");
        state.update(&finish, false, &events);
        assert!(state.active.is_some());
        finish["operationId"] = json!("operation");
        finish["result"]["execution"]["version"] = json!(6);
        state.update(&finish, false, &events);
        assert!(state.active.is_some());
        finish["result"]["execution"]["version"] = json!(11);
        state.update(&finish, false, &events);
        assert!(
            state.active.is_none(),
            "one automatic drive may legitimately advance attempts"
        );
        let output: Vec<_> = received.try_iter().collect();
        assert_eq!(
            output
                .iter()
                .filter(|event| matches!(event, DriverEvent::TurnStarted))
                .count(),
            1
        );
        assert_eq!(
            output
                .iter()
                .filter(|event| matches!(event, DriverEvent::TurnFinished { success: true, .. }))
                .count(),
            1
        );
        state.update(&start, false, &events);
        assert!(
            !received
                .try_iter()
                .any(|event| matches!(event, DriverEvent::TurnStarted))
        );
        let mut next = start.clone();
        next["operationId"] = json!("next");
        state.update(&next, false, &events);
        received.try_iter().for_each(drop);
        finish["operationId"] = json!("next");
        state.update(&finish, true, &events);
        assert!(
            !received
                .try_iter()
                .any(|event| matches!(event, DriverEvent::TurnFinished { .. })),
            "late automatic result cannot finish current prompt"
        );
        next["operationId"] = json!("failed");
        state.update(&next, false, &events);
        received.try_iter().for_each(drop);
        state.update(&json!({"sessionId":"session","phase":"finished","operationId":"failed","runId":"run","error":{"code":-32603},"result":{}}), false, &events);
        assert!(
            received
                .try_iter()
                .any(|event| matches!(event, DriverEvent::TurnFinished { success: false, .. })),
            "I/O completion with persistence failure must not invent successful task completion"
        );
        next["operationId"] = json!("racing-prompt-reply");
        state.update(&next, true, &events);
        assert!(
            !received
                .try_iter()
                .any(|event| matches!(event, DriverEvent::TurnStarted))
        );
        state.after_prompt(&events);
        state.after_prompt(&events);
        assert_eq!(
            received
                .try_iter()
                .filter(|event| matches!(event, DriverEvent::TurnStarted))
                .count(),
            1,
            "automatic start racing the earlier prompt reply is deferred, not lost"
        );
    }
    #[test]
    fn deerflow_automatic_continuation_can_wait_again_before_finishing() {
        let (events, received) = crate::driver::test_event_channel();
        let mut state = NotificationState {
            connection: "connection".into(),
            session: "session".into(),
            active: None,
            parked: None,
            announced: false,
            recent: Default::default(),
        };
        let start = json!({
            "sessionId": "session", "phase": "started", "operationId": "first", "runId": "run",
            "execution": {"sessionId": "session", "runId": "run", "version": 2, "status": "running"}
        });
        let waiting = json!({
            "sessionId": "session", "phase": "finished", "operationId": "first", "runId": "run",
            "result": {"stopReason": "end_turn", "execution": {
                "sessionId": "session", "runId": "run", "version": 3,
                "status": "waiting_input", "waitingTasks": ["second-child"], "resumable": true
            }}
        });
        state.update(&start, false, &events);
        state.update(&waiting, false, &events);
        let output: Vec<_> = received
            .try_iter()
            .filter(|event| !matches!(event, DriverEvent::DeerFlowTasks(_)))
            .collect();
        assert!(matches!(
            output.as_slice(),
            [DriverEvent::TurnStarted, DriverEvent::TurnParked]
        ));
        assert_eq!(state.parked, Some(("run".into(), 3)));

        let mut next = start;
        next["operationId"] = json!("second");
        next["execution"]["version"] = json!(4);
        state.update(&next, false, &events);
        // A repeated completion from the previous attempt must not park or
        // finish the continuation now producing the answer.
        state.update(&waiting, false, &events);
        let mut done = waiting;
        done["operationId"] = json!("second");
        done["result"]["execution"]["version"] = json!(5);
        done["result"]["execution"]["status"] = json!("completed");
        done["result"]["execution"]
            .as_object_mut()
            .unwrap()
            .remove("waitingTasks");
        state.update(&done, false, &events);
        state.update(&done, false, &events);
        let output: Vec<_> = received
            .try_iter()
            .filter(|event| !matches!(event, DriverEvent::DeerFlowTasks(_)))
            .collect();
        assert!(matches!(
            output.as_slice(),
            [
                DriverEvent::TurnStarted,
                DriverEvent::TurnFinished { success: true, .. }
            ]
        ));
        assert!(state.parked.is_none());
    }

    #[test]
    fn deerflow_parked_dependency_failure_only_finishes_its_own_turn() {
        let (events, received) = crate::driver::test_event_channel();
        let mut stream = AcpStreamState::default();
        stream.deerflow_tasks = Some(NotificationState {
            connection: "connection".into(),
            session: "session".into(),
            active: None,
            parked: None,
            announced: false,
            recent: Default::default(),
        });
        let execution = json!({
            "sessionId": "session", "runId": "run", "version": 7,
            "status": "waiting_input", "waitingTasks": ["child"], "resumable": true
        });
        let response: PromptResponse = serde_json::from_value(json!({
            "stopReason": "end_turn", "_meta": {"deerflow": {"execution": execution}}
        }))
        .unwrap();
        assert!(finish_prompt(Ok(response.clone()), &mut stream, &events));
        assert!(matches!(
            received.try_recv().unwrap(),
            DriverEvent::TurnParked
        ));

        // closeFailedTaskWait reports a terminal result without starting an
        // automatic attempt, so there is no operationId or top-level runId.
        let failed = json!({
            "sessionId": "session", "phase": "finished",
            "error": {"code": -32603, "message": "Internal error"},
            "result": {"stopReason": "failed", "execution": {
                "sessionId": "session", "runId": "run", "version": 8, "status": "failed"
            }}
        });
        let state = stream.deerflow_tasks.as_mut().unwrap();
        for (path, value) in [
            ("/sessionId", json!("other-session")),
            ("/result/execution/sessionId", json!("other-session")),
            ("/result/execution/runId", json!("other-run")),
            ("/result/execution/version", json!(6)),
            ("/result/execution", Value::Null),
        ] {
            let mut stale = failed.clone();
            *stale.pointer_mut(path).unwrap() = value;
            state.update(&stale, false, &events);
        }
        state.update(&failed, true, &events);
        assert!(
            !received
                .try_iter()
                .any(|event| matches!(event, DriverEvent::TurnFinished { .. }))
        );
        state.update(&failed, false, &events);
        state.update(&failed, false, &events);
        let output: Vec<_> = received
            .try_iter()
            .filter(|event| !matches!(event, DriverEvent::DeerFlowTasks(_)))
            .collect();
        assert!(matches!(
            output.as_slice(),
            [DriverEvent::TurnFinished { success: false, .. }]
        ));
        assert!(state.parked.is_none());

        // The dependency may fail before the waiting prompt's response reaches
        // the client. Its notice must survive until that response binds the run.
        receive_update(&mut stream, EarlyUpdate::Lifecycle(failed), true, &events).unwrap();
        assert!(received.try_recv().is_err());
        assert!(finish_prompt(Ok(response), &mut stream, &events));
        flush_early_updates(&mut stream, false, &events).unwrap();
        let output: Vec<_> = received
            .try_iter()
            .filter(|event| !matches!(event, DriverEvent::DeerFlowTasks(_)))
            .collect();
        assert!(matches!(
            output.as_slice(),
            [
                DriverEvent::TurnParked,
                DriverEvent::TurnFinished { success: false, .. }
            ]
        ));
    }

    #[test]
    fn deerflow_terminal_prompt_responses_do_not_leave_a_parked_turn() {
        for (stop_reason, status, session) in [
            ("end_turn", "completed", "session"),
            ("cancelled", "waiting_input", "session"),
            ("max_tokens", "waiting_input", "session"),
            ("end_turn", "waiting_input", "foreign-session"),
        ] {
            let (events, received) = crate::driver::test_event_channel();
            let mut stream = AcpStreamState::default();
            stream.deerflow_tasks = Some(NotificationState {
                connection: "connection".into(),
                session: "session".into(),
                active: None,
                parked: Some(("old-run".into(), 1)),
                announced: false,
                recent: Default::default(),
            });
            let response = serde_json::from_value(json!({
                "stopReason": stop_reason, "_meta": {"deerflow": {"execution": {
                    "sessionId": session, "runId": "run", "version": 2, "status": status
                }}}
            }))
            .unwrap();
            finish_prompt(Ok(response), &mut stream, &events);
            assert!(matches!(
                received.try_recv().unwrap(),
                DriverEvent::TurnFinished { .. }
            ));
            assert!(received.try_recv().is_err());
            assert!(stream.deerflow_tasks.as_ref().unwrap().parked.is_none());
        }
    }

    #[test]
    fn deerflow_execution_resume_preserves_turn_lifecycle_and_replays_real_permission() {
        for waiting in [false, true] {
            let agent = Agent.builder().on_receive_request(
            async move |request: UntypedMessage, responder, connection: ConnectionTo<Client>| {
                assert_eq!(request.method(), "_deerflow/executions/resume");
                assert_eq!(request.params(), &json!({"sessionId":"session","runId":"continuation-run","expectedVersion":7}));
                let permission: RequestPermissionRequest = serde_json::from_value(json!({
                    "sessionId":"session", "toolCall":{"toolCallId":"saved-call","title":"write_file","kind":"edit","status":"pending"},
                    "options":[{"optionId":"allow","name":"Allow once","kind":"allow_once"},{"optionId":"reject","name":"Reject once","kind":"reject_once"}]
                }))?;
                connection.send_request(permission).on_receiving_result(async move |result| {
                    assert_eq!(serde_json::to_value(result?)?["outcome"]["optionId"], "reject");
                    let mut response = json!({"stopReason":"end_turn"});
                    if waiting {
                        response["_meta"] = json!({"deerflow": {"execution": {
                            "sessionId": "session", "runId": "continuation-run", "version": 8,
                            "status": "waiting_input", "waitingTasks": ["child"], "resumable": true
                        }}});
                    }
                    responder.respond(response)
                })?;
                Ok(Handled::Yes)
            }, agent_client_protocol::on_receive_request!());
            let (events, received) = crate::driver::test_event_channel();
            let permissions: PendingPermissions = Arc::new(Mutex::new(HashMap::new()));
            let handler_permissions = permissions.clone();
            let handler_events = events.clone();
            let client = Client.builder().on_receive_request(
                async move |request: RequestPermissionRequest, responder, _connection| {
                    handle_permission_request(
                        request,
                        responder,
                        ProviderKind::DeerFlow,
                        RuntimeMode::FullAccess,
                        &handler_permissions,
                        &handler_events,
                    )
                },
                agent_client_protocol::on_receive_request!(),
            );
            smol::block_on(smol::future::or(
                client.connect_with(agent, async move |connection| {
                    let state = TaskConnection {
                        id: "connection".into(),
                        session: "session".into(),
                        capabilities: Capabilities {
                            tree: true,
                            executions: true,
                            ..Default::default()
                        },
                        pending: Arc::new(AtomicBool::new(false)),
                    };
                    let prompt_requests = Arc::new(Mutex::new(PendingPrompts::default()));
                    let mut stream = AcpStreamState::default();
                    stream.deerflow_tasks = Some(state.notifications());
                    state.resume_execution(
                        &connection,
                        Command {
                            request_id: uuid::Uuid::new_v4().to_string(),
                            connection_id: state.id.clone(),
                            session_id: state.session.clone(),
                            action: Action::ExecutionResume {
                                run_id: "continuation-run".into(),
                                expected_version: 7,
                            },
                        },
                        &prompt_requests,
                        &events,
                        &Arc::new(Mutex::new(stream)),
                    );
                    assert!(matches!(
                        received.try_recv().unwrap(),
                        DriverEvent::TurnStarted
                    ));
                    let mut permitted = false;
                    let mut finished = false;
                    loop {
                        match received.try_recv() {
                            Ok(DriverEvent::Permission { request_id, .. }) => {
                                assert!(!prompt_requests.lock().is_empty());
                                assert!(!finished);
                                permissions
                                    .lock()
                                    .remove(&request_id)
                                    .expect("saved permission waits for explicit decision")
                                    .respond(RequestPermissionResponse::new(
                                        RequestPermissionOutcome::Selected(
                                            SelectedPermissionOutcome::new("reject"),
                                        ),
                                    ))?;
                                permitted = true;
                            }
                            Ok(DriverEvent::TurnFinished { success: true, .. }) => {
                                assert!(permitted);
                                assert!(!waiting);
                                finished = true;
                            }
                            Ok(DriverEvent::TurnParked) => {
                                assert!(permitted);
                                assert!(waiting);
                                finished = true;
                            }
                            Ok(DriverEvent::DeerFlowTasks(Event::Reply {
                                response: Response::ExecutionFinished,
                                ..
                            })) => {
                                assert!(finished);
                                break;
                            }
                            Ok(other) => panic!("unexpected recovery event: {other:?}"),
                            Err(_) => {
                                smol::Timer::after(Duration::from_millis(5)).await;
                            }
                        }
                    }
                    assert!(prompt_requests.lock().is_empty());
                    assert!(!state.pending.load(Ordering::Acquire));
                    Ok(())
                }),
                async {
                    smol::Timer::after(Duration::from_secs(5)).await;
                    Err(approval_error("execution resume test timed out"))
                },
            ))
            .unwrap();
        }
    }
    #[test]
    fn deerflow_task_capabilities_require_exact_versioned_methods() {
        let mut value = json!({"_meta":{"deerflow":{"taskTree":{"version":1,"listMethod":"_deerflow/tasks/tree","getMethod":"_deerflow/tasks/tree/get","cancelMethod":"_deerflow/tasks/tree/cancel"}}}});
        assert!(capabilities(&value).tree);
        assert!(capabilities(&value).cancel);
        value["_meta"]["deerflow"]["taskTree"]["listMethod"] = json!("external/mutate");
        assert!(!capabilities(&value).tree);
        assert!(!capabilities(&Value::Null).permission);
    }
    #[test]
    fn deerflow_artifact_requests_require_advertisement_and_pin_later_chunk_hashes() {
        let mut state = TaskConnection {
            id: "connection".into(),
            session: "session".into(),
            capabilities: Capabilities {
                tree: true,
                ..Default::default()
            },
            pending: Arc::new(AtomicBool::new(false)),
        };
        let mut command = Command {
            request_id: uuid::Uuid::new_v4().to_string(),
            connection_id: state.id.clone(),
            session_id: state.session.clone(),
            action: Action::ArtifactRead {
                root_id: "root".into(),
                node_id: "node".into(),
                artifact_id: "artifact".into(),
                offset: 0,
                limit: 1_048_576,
                expected_sha256: None,
            },
        };
        assert!(state.request(&command).is_err());
        state.capabilities.artifact_read = true;
        let (method, params) = state.request(&command).unwrap();
        assert_eq!(method, "_deerflow/tasks/tree/artifacts/read");
        assert!(params.get("expectedSha256").is_none());
        if let Action::ArtifactRead { offset, .. } = &mut command.action {
            *offset = 5;
        }
        assert!(state.request(&command).is_err());
        if let Action::ArtifactRead {
            expected_sha256, ..
        } = &mut command.action
        {
            *expected_sha256 = Some("a".repeat(64));
        }
        assert_eq!(
            state.request(&command).unwrap().1["expectedSha256"],
            "a".repeat(64)
        );
    }
    #[test]
    fn deerflow_task_requests_use_attached_session_and_typed_cancel_scope() {
        let state = TaskConnection {
            id: "connection".into(),
            session: "session".into(),
            capabilities: Capabilities {
                tree: true,
                cancel: true,
                ..Default::default()
            },
            pending: Arc::new(AtomicBool::new(false)),
        };
        let mut command = Command {
            request_id: uuid::Uuid::new_v4().to_string(),
            connection_id: state.id.clone(),
            session_id: state.session.clone(),
            action: Action::Cancel {
                root_id: "run".into(),
                node_id: "root/run".into(),
                scope: waku_protocol::deerflow_tasks::CancelScope::Root,
                expected_version: 3,
            },
        };
        let (method, params) = state.request(&command).unwrap();
        assert_eq!(method, "_deerflow/tasks/tree/cancel");
        assert_eq!(
            params,
            json!({"sessionId":"session","rootId":"run","nodeId":"root/run","expectedVersion":3})
        );
        command.session_id = "other".into();
        assert!(state.request(&command).is_err());
        command.session_id = state.session.clone();
        command.connection_id = "old".into();
        assert!(state.request(&command).is_err());
    }

    #[test]
    fn session_budget_requests_require_capability_and_bound_session() {
        let mut state = TaskConnection {
            id: "connection".into(),
            session: "native".into(),
            capabilities: Capabilities::default(),
            pending: Arc::new(AtomicBool::new(false)),
        };
        let mut command = Command {
            request_id: uuid::Uuid::new_v4().to_string(),
            connection_id: state.id.clone(),
            session_id: state.session.clone(),
            action: Action::BudgetGet,
        };
        assert!(state.request(&command).is_err());
        state.capabilities.session_budget = true;
        let (method, params) = state.request(&command).unwrap();
        assert_eq!(method, "_deerflow/session/budget/get");
        assert_eq!(params["sessionId"], "native");
        command.action = Action::BudgetSet {
            expected_version: 2,
            operation_id: uuid::Uuid::new_v4().to_string(),
            limits: waku_protocol::deerflow_tasks::TaskBudget {
                max_tokens: 1000,
                max_model_calls: 2,
                max_tool_calls: 0,
            },
        };
        let (method, params) = state.request(&command).unwrap();
        assert_eq!(method, "_deerflow/session/budget/set");
        assert_eq!(params["limits"]["maxTokens"], 1000);
        command.session_id = "other".into();
        assert!(state.request(&command).is_err());
    }

    #[test]
    fn deerflow_task_dispatch_uses_live_acp_and_isolates_management_failure() {
        let captured = Arc::new(Mutex::new(Vec::new()));
        let requests = captured.clone();
        let agent=Agent.builder().on_receive_request(async move |request:UntypedMessage,responder,_connection| {
            requests.lock().push((request.method().to_owned(),request.params().clone()));
            if request.method()=="_deerflow/tasks/tree" {responder.respond(json!({"sessionId":"session","nodes":[],"hasMore":false,"consistency":"live_keyset"}))?;}
            else {responder.respond_with_internal_error("task version changed")?;}
            Ok(Handled::Yes)
        },agent_client_protocol::on_receive_request!());
        let (events, received) = crate::driver::test_event_channel();
        smol::block_on(
            Client
                .builder()
                .connect_with(agent, async move |connection| {
                    let state = TaskConnection {
                        id: "connection".into(),
                        session: "session".into(),
                        capabilities: Capabilities {
                            tree: true,
                            cancel: true,
                            ..Default::default()
                        },
                        pending: Arc::new(AtomicBool::new(false)),
                    };
                    for action in [
                        Action::List {
                            root_id: None,
                            cursor: None,
                            limit: 40,
                        },
                        Action::Cancel {
                            root_id: "run".into(),
                            node_id: "root/run".into(),
                            scope: waku_protocol::deerflow_tasks::CancelScope::Root,
                            expected_version: 3,
                        },
                    ] {
                        let is_list = matches!(action, Action::List { .. });
                        let command = Command {
                            request_id: uuid::Uuid::new_v4().to_string(),
                            connection_id: state.id.clone(),
                            session_id: state.session.clone(),
                            action,
                        };
                        state.dispatch(&connection, command, &events);
                        let event = smol::future::race(
                            async {
                                loop {
                                    if let Ok(event) = received.try_recv() {
                                        break Ok(event);
                                    }
                                    smol::Timer::after(Duration::from_millis(5)).await;
                                }
                            },
                            async {
                                smol::Timer::after(Duration::from_secs(2)).await;
                                Err(anyhow!("missing task response"))
                            },
                        )
                        .await?;
                        if is_list {
                            assert!(matches!(
                                event,
                                DriverEvent::DeerFlowTasks(Event::Reply {
                                    response: Response::Page(_),
                                    ..
                                })
                            ));
                        } else {
                            assert!(matches!(
                                event,
                                DriverEvent::DeerFlowTasks(Event::Failed { .. })
                            ));
                        }
                        assert!(!state.pending.load(Ordering::Acquire));
                    }
                    Ok(())
                }),
        )
        .unwrap();
        let calls = captured.lock();
        assert_eq!(calls.len(), 2);
        assert!(
            calls
                .iter()
                .all(|(_, params)| params["sessionId"] == "session")
        );
        assert!(calls.iter().all(|(method, _)| method != "session/prompt"));
    }
}
