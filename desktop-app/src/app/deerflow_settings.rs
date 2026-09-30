//! DeerFlow configuration uses the same native controls and theme as the chat
//! workspace. The Go configuration service validates and persists the YAML.
use super::*;
use crate::ui::ActivationExt;
use serde_json::{Value, json};

const SINGLE_FIELDS: usize = 40;
const FIELD_COUNT: usize = 56;

#[derive(Clone, Copy, Default, PartialEq, Eq)]
enum Section {
    #[default]
    Overview,
    Models,
    Agents,
    Memory,
    Skills,
    Tools,
    Runtime,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum DeerFlowServicePhase {
    Unknown,
    Starting,
    Warming,
    Ready,
    Applying,
    Stopping,
    Failed,
    Stopped,
}

fn deerflow_service_phase(
    status: &Value,
    request_pending: bool,
    startup_pending: bool,
    startup_error: bool,
    pending_operation: Option<&str>,
) -> DeerFlowServicePhase {
    if request_pending || pending_operation == Some("start") {
        return DeerFlowServicePhase::Starting;
    }
    if matches!(
        pending_operation,
        Some("apply" | "save-and-apply" | "cancel-apply")
    ) {
        return DeerFlowServicePhase::Applying;
    }
    if pending_operation == Some("stop") {
        return DeerFlowServicePhase::Stopping;
    }
    if startup_error {
        return DeerFlowServicePhase::Failed;
    }
    let applying = status["applying"].as_bool() == Some(true);
    if !startup_pending && !applying && status["running"].as_bool() == Some(false) {
        return DeerFlowServicePhase::Stopped;
    }
    if status["apply_error"].as_str().is_some() || status["warmup"] == "failed" {
        return DeerFlowServicePhase::Failed;
    }
    if applying {
        return if startup_pending {
            DeerFlowServicePhase::Starting
        } else {
            DeerFlowServicePhase::Applying
        };
    }
    if status["running"].as_bool() == Some(true) {
        return if status["warmup"] == "warming" {
            DeerFlowServicePhase::Warming
        } else {
            DeerFlowServicePhase::Ready
        };
    }
    if startup_pending && status["status_error"].as_str().is_some() {
        return DeerFlowServicePhase::Failed;
    }
    if startup_pending {
        return DeerFlowServicePhase::Starting;
    }
    if status.is_null() {
        DeerFlowServicePhase::Unknown
    } else {
        DeerFlowServicePhase::Stopped
    }
}
impl Section {
    const ALL: [(Self, &'static str); 7] = [
        (Self::Overview, "概览"),
        (Self::Models, "模型"),
        (Self::Agents, "智能体"),
        (Self::Memory, "长期记忆"),
        (Self::Skills, "技能"),
        (Self::Tools, "工具与权限"),
        (Self::Runtime, "ACP 运行"),
    ];
}
#[derive(Clone, Copy)]
enum Kind {
    Text,
    Optional,
    Integer,
    Number,
    Multiline,
    Json,
    Bool,
    Choice(&'static [(&'static str, &'static str)]),
}
impl Kind {
    fn multiline(self) -> bool {
        matches!(self, Self::Multiline | Self::Json)
    }
    fn input(self) -> bool {
        !matches!(self, Self::Bool | Self::Choice(_))
    }
}
#[derive(Clone)]
struct Field {
    path: String,
    label: String,
    hint: String,
    kind: Kind,
    slot: Option<usize>,
}
impl Field {
    fn new(
        path: impl Into<String>,
        label: impl Into<String>,
        hint: impl Into<String>,
        kind: Kind,
    ) -> Self {
        Self {
            path: path.into(),
            label: label.into(),
            hint: hint.into(),
            kind,
            slot: None,
        }
    }
}

pub(super) struct DeerFlowSettings {
    pub(super) inputs: Vec<Entity<TextInput>>,
    draft: Option<Value>,
    fields: Vec<Field>,
    input_text: Vec<String>,
    errors: HashMap<String, String>,
    section: Section,
    selected_model: usize,
    pending_model_names: HashMap<usize, String>,
    pending_agent_names: HashMap<usize, String>,
    selected_agent: usize,
    advanced: bool,
    dirty: bool,
    needs_apply: bool,
    request_generation: u64,
    edit_generation: u64,
    pending: Option<String>,
    status_pending: bool,
    status_generation: u64,
    observed_applied_generation: Option<u64>,
    refresh_after_start: bool,
    startup_request_pending: bool,
    startup_completion_pending: bool,
    startup_error: Option<String>,
    model_refresh_queued: bool,
    model_refresh_retry_pending: bool,
    status: Value,
    error: Option<String>,
    notice: Option<String>,
    confirm_reload: bool,
    confirm_portable: bool,
    exit_requested: bool,
    exit_after_save: bool,
    exit_focus: FocusHandle,
    polling: bool,
    sessions: Vec<Value>,
    session_page: usize,
    memory_page: usize,
    selected_session: Option<String>,
    memory_data: Value,
    proposals: Vec<Value>,
    proposal: Option<Value>,
    revisions: Vec<Value>,
    proposal_diff: Entity<TextInput>,
    confirm_manage: Option<(String, Value)>,
}
impl DeerFlowSettings {
    pub(super) fn new(window: &mut Window, cx: &mut App) -> Self {
        let inputs = (0..FIELD_COUNT)
            .map(|index| {
                cx.new(|cx| {
                    let input = TextInput::new(window, cx);
                    if index >= SINGLE_FIELDS {
                        input.multi_line()
                    } else {
                        input
                    }
                })
            })
            .collect();
        let proposal_diff = cx.new(|cx| TextInput::new(window, cx).multi_line().read_only(true));
        Self {
            inputs,
            draft: None,
            fields: Vec::new(),
            input_text: vec![String::new(); FIELD_COUNT],
            errors: HashMap::new(),
            section: Section::Overview,
            selected_model: 0,
            pending_model_names: HashMap::new(),
            pending_agent_names: HashMap::new(),
            selected_agent: 0,
            advanced: false,
            dirty: false,
            needs_apply: false,
            request_generation: 0,
            edit_generation: 0,
            pending: None,
            status_pending: false,
            status_generation: 0,
            observed_applied_generation: None,
            refresh_after_start: false,
            startup_request_pending: false,
            startup_completion_pending: false,
            startup_error: None,
            model_refresh_queued: false,
            model_refresh_retry_pending: false,
            status: Value::Null,
            error: None,
            notice: None,
            confirm_reload: false,
            confirm_portable: false,
            exit_requested: false,
            exit_after_save: false,
            exit_focus: cx.focus_handle(),
            polling: false,
            sessions: Vec::new(),
            session_page: 0,
            memory_page: 0,
            selected_session: None,
            memory_data: Value::Null,
            proposals: Vec::new(),
            proposal: None,
            revisions: Vec::new(),
            proposal_diff,
            confirm_manage: None,
        }
    }
    fn busy(&self) -> bool {
        self.pending.is_some()
    }
    fn applying(&self) -> bool {
        self.pending.as_deref() == Some("apply")
            || self.status["applying"].as_bool().unwrap_or(false)
    }
    fn can_edit(&self) -> bool {
        self.draft.is_some() && !self.busy() && !self.applying()
    }

    pub(super) fn service_phase(&self) -> DeerFlowServicePhase {
        deerflow_service_phase(
            &self.status,
            self.startup_request_pending,
            self.startup_completion_pending,
            self.startup_error.is_some(),
            self.pending.as_deref(),
        )
    }

    pub(super) fn startup_failed(&self) -> bool {
        self.startup_error.is_some() || self.status["warmup"] == "failed"
    }

    fn observe_saved_revision(&mut self) {
        if let (Some(saved), Some(running)) = (
            self.draft
                .as_ref()
                .and_then(|doc| doc["config_revision"].as_str()),
            self.status["config_revision"].as_str(),
        ) {
            if saved != running {
                self.needs_apply = true;
            }
        }
    }
}

fn field_text(value: &Value, kind: Kind) -> String {
    match kind {
        Kind::Json => serde_json::to_string_pretty(value).unwrap_or_default(),
        _ => value.as_str().map(str::to_owned).unwrap_or_else(|| {
            if value.is_null() {
                String::new()
            } else {
                value.to_string()
            }
        }),
    }
}
fn parse_field(text: &str, kind: Kind) -> Result<Value, String> {
    match kind {
        Kind::Text | Kind::Multiline => Ok(Value::String(text.to_owned())),
        Kind::Optional => Ok(if text.trim().is_empty() {
            Value::Null
        } else {
            json!(text.trim())
        }),
        Kind::Integer => text
            .trim()
            .parse::<i64>()
            .map(Value::from)
            .map_err(|_| "请输入整数".into()),
        Kind::Number => text
            .trim()
            .parse::<f64>()
            .ok()
            .filter(|v| v.is_finite())
            .map(Value::from)
            .ok_or_else(|| "请输入有效数字".into()),
        Kind::Json => serde_json::from_str(text).map_err(|e| format!("JSON 格式错误：{e}")),
        _ => Err("该字段不接受文本输入".into()),
    }
}
fn set_value(document: &mut Value, path: &str, value: Value) {
    if let Some((parent, key)) = path.rsplit_once('/') {
        if let Some(object) = document.pointer_mut(parent).and_then(Value::as_object_mut) {
            object.insert(key.to_owned(), value);
        }
    }
}
fn remap_model_references(document: &mut Value, names: &HashMap<String, String>) {
    fn replace(value: &mut Value, names: &HashMap<String, String>, allow_inherit: bool) {
        if let Some(current) = value.as_str() {
            if allow_inherit && current == "inherit" {
                return;
            }
            if let Some(replacement) = names.get(current) {
                *value = json!(replacement);
            }
        }
    }
    for path in [
        "/default_model",
        "/runtime/model_name",
        "/memory/model_name",
        "/skill_evolution/generation_model_name",
        "/skill_evolution/moderation_model_name",
        "/skill_evolution/evaluation_model_name",
    ] {
        if let Some(value) = document.pointer_mut(path) {
            replace(value, names, false);
        }
    }
    if let Some(agents) = document["agents"].as_array_mut() {
        for agent in agents {
            if let Some(model) = agent.get_mut("model") {
                replace(model, names, false);
            }
        }
    }
    for path in [
        "/subagents/agents",
        "/subagents/custom_agents",
        "/subagents/advanced/agents",
        "/subagents/advanced/custom_agents",
    ] {
        if let Some(agents) = document.pointer_mut(path).and_then(Value::as_object_mut) {
            for agent in agents.values_mut() {
                if let Some(model) = agent.get_mut("model") {
                    replace(model, names, true);
                }
            }
        }
    }
}
fn rename_model_references(document: &mut Value, old: &str, new: &str) {
    remap_model_references(document, &HashMap::from([(old.to_owned(), new.to_owned())]));
}
fn model_name_index(path: &str) -> Option<usize> {
    let pieces: Vec<_> = path.split('/').collect();
    if pieces.len() == 4 && pieces[1] == "models" && pieces[3] == "name" {
        pieces[2].parse().ok()
    } else {
        None
    }
}
fn document_with_model_names(
    document: &Value,
    pending: &HashMap<usize, String>,
) -> Result<Value, String> {
    let models = document["models"].as_array().ok_or("模型列表格式错误")?;
    if pending.keys().any(|index| *index >= models.len()) {
        return Err("模型列表已变化，请重新加载。".into());
    }
    let mut unique = HashSet::new();
    let mut names = Vec::new();
    let mut remap = HashMap::new();
    for (index, model) in models.iter().enumerate() {
        let old = model["name"].as_str().unwrap_or("");
        let new = pending
            .get(&index)
            .map(String::as_str)
            .unwrap_or(old)
            .trim();
        if new.is_empty() {
            return Err("模型配置名称不能为空。".into());
        }
        if !unique.insert(new.to_owned()) {
            return Err(format!("模型配置名称“{new}”重复，请使用唯一名称。"));
        }
        names.push(new.to_owned());
        if old != new {
            remap.insert(old.to_owned(), new.to_owned());
        }
    }
    let mut result = document.clone();
    for (index, name) in names.into_iter().enumerate() {
        result["models"][index]["name"] = json!(name);
    }
    // One lookup per reference keeps renaming chains/swaps from cascading.
    remap_model_references(&mut result, &remap);
    Ok(result)
}

impl Waku {
    pub(super) fn auto_start_deerflow(&mut self, cx: &mut Context<Self>) {
        // A remote Waku daemon is managed outside this desktop process.
        if self.daemon.is_remote() {
            return;
        }
        self.deerflow_settings.startup_request_pending = true;
        self.deerflow_settings.startup_completion_pending = true;
        self.deerflow_settings.startup_error = None;
        cx.notify();
        let daemon = self.daemon.client();
        cx.spawn(async move |this, cx| {
            let result: anyhow::Result<Value> = cx
                .background_executor()
                .spawn(async move {
                    match daemon.request(
                        Uuid::nil(),
                        Uuid::nil(),
                        waku_client::Command::DeerFlow {
                            operation: "start".into(),
                            input: json!({}),
                        },
                    )? {
                        waku_client::ResponsePayload::DeerFlow { data } => Ok(data),
                        _ => anyhow::bail!("DeerFlow 返回了无法识别的响应"),
                    }
                })
                .await;
            let _ = this.update(cx, |this, cx| {
                this.deerflow_settings.startup_request_pending = false;
                match result {
                    Ok(status) => {
                        // A status read started before the start response must not
                        // overwrite the newer startup state when it returns.
                        this.deerflow_settings.status_generation += 1;
                        this.deerflow_settings.status_pending = false;
                        this.deerflow_settings.status = status;
                        this.deerflow_settings.error = None;
                        this.deerflow_settings.refresh_after_start = true;
                        this.deerflow_status(cx);
                        this.deerflow_poll_status(cx);
                    }
                    Err(error) => {
                        let message = redact_deerflow_message(
                            &format!("自动启动 DeerFlow 服务失败：{error}"),
                            this.deerflow_settings.draft.as_ref(),
                        );
                        this.deerflow_settings.startup_completion_pending = false;
                        this.deerflow_settings.startup_error = Some(message.clone());
                        this.deerflow_settings.error = Some(message);
                        this.show_toast("DeerFlow ACP 启动失败，请在设置中查看详情");
                    }
                }
                cx.notify();
            });
        })
        .detach();
    }

    pub(crate) fn has_unsaved_deerflow_settings(&self) -> bool {
        self.deerflow_settings.dirty || self.deerflow_settings.busy()
    }

    pub(crate) fn request_deerflow_exit(
        &mut self,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        if !self.has_unsaved_deerflow_settings() {
            return false;
        }
        self.settings_page = Some(SettingsPage::DeerFlow);
        self.deerflow_settings.exit_requested = true;
        window.focus(&self.deerflow_settings.exit_focus, cx);
        window.activate_window();
        cx.notify();
        true
    }

    pub(super) fn close_window_with_deerflow_guard(
        &mut self,
        _: &CloseWindow,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if !self.request_deerflow_exit(window, cx) {
            crate::platform::hide_window(window);
        }
    }

    pub(super) fn ensure_deerflow_settings(&mut self, cx: &mut Context<Self>) {
        if self.deerflow_settings.draft.is_none() && !self.deerflow_settings.busy() {
            self.deerflow_request("snapshot", json!({}), cx);
        }
        self.deerflow_status(cx);
    }

    fn deerflow_freeze_inputs(&mut self, cx: &mut Context<Self>) {
        let read_only = !self.deerflow_settings.can_edit();
        for input in &self.deerflow_settings.inputs {
            input.update(cx, |input, cx| {
                input.set_read_only(read_only);
                cx.notify();
            });
        }
    }

    fn deerflow_request(&mut self, operation: &str, input: Value, cx: &mut Context<Self>) {
        if self.deerflow_settings.busy() {
            return;
        }
        match operation {
            "start" => {
                self.deerflow_settings.startup_completion_pending = true;
                self.deerflow_settings.startup_error = None;
            }
            "stop" | "cancel-apply" | "apply" | "save-and-apply" => {
                self.deerflow_settings.startup_completion_pending = false;
                self.deerflow_settings.startup_error = None;
            }
            _ => {}
        }
        self.deerflow_settings.request_generation += 1;
        let generation = self.deerflow_settings.request_generation;
        let edits = self.deerflow_settings.edit_generation;
        self.deerflow_settings.pending = Some(operation.into());
        if matches!(
            operation,
            "apply" | "save-and-apply" | "start" | "stop" | "cancel-apply"
        ) {
            self.deerflow_settings.model_refresh_queued = false;
        }
        if matches!(operation, "apply" | "save-and-apply")
            && self.deerflow_settings.observed_applied_generation.is_none()
        {
            self.deerflow_settings.observed_applied_generation = Some(
                self.deerflow_settings.status["applied_generation"]
                    .as_u64()
                    .unwrap_or(0),
            );
        }
        if matches!(operation, "stop" | "cancel-apply") {
            self.deerflow_settings.refresh_after_start = false;
        }
        if matches!(
            operation,
            "apply" | "save-and-apply" | "start" | "stop" | "cancel-apply"
        ) {
            self.deerflow_settings.status_generation += 1;
            self.deerflow_settings.status_pending = false;
        }
        self.deerflow_settings.error = None;
        self.deerflow_settings.notice = None;
        self.deerflow_freeze_inputs(cx);
        let management_operation = input["operation"].as_str().unwrap_or("").to_owned();
        let operation = operation.to_owned();
        let daemon = self.daemon.client();
        cx.spawn(async move |this, cx| {
            let request_operation = operation.clone();
            let result: anyhow::Result<Value> = cx
                .background_executor()
                .spawn(async move {
                    match daemon.request(
                        Uuid::nil(),
                        Uuid::nil(),
                        waku_client::Command::DeerFlow {
                            operation: request_operation,
                            input,
                        },
                    )? {
                        waku_client::ResponsePayload::DeerFlow { data } => Ok(data),
                        _ => anyhow::bail!("DeerFlow 返回了无法识别的响应"),
                    }
                })
                .await;
            let _ = this.update(cx, |this, cx| {
                if this.deerflow_settings.request_generation != generation {
                    return;
                }
                this.deerflow_settings.pending = None;
                if matches!(
                    operation.as_str(),
                    "apply" | "save-and-apply" | "start" | "stop" | "cancel-apply"
                ) {
                    // A poll launched while this command was in flight may
                    // return afterward with an older service state.
                    this.deerflow_settings.status_generation += 1;
                    this.deerflow_settings.status_pending = false;
                }
                if result.is_err() {
                    this.deerflow_settings.exit_after_save = false;
                }
                let saved_for_exit = result.is_ok()
                    && operation == "save"
                    && this.deerflow_settings.exit_after_save
                    && this.deerflow_settings.edit_generation == edits;
                match result {
                    Err(error) => {
                        let message = if operation == "apply" && this.deerflow_settings.needs_apply
                        {
                            format!("配置已保存，但应用失败：{error}。可点击“重试应用”。")
                        } else {
                            error.to_string()
                        };
                        if operation == "start" {
                            this.deerflow_settings.startup_completion_pending = false;
                            this.deerflow_settings.startup_error = Some(redact_deerflow_message(
                                &message,
                                this.deerflow_settings.draft.as_ref(),
                            ));
                        }
                        this.deerflow_service_error(&message, cx);
                    }
                    Ok(data) => match operation.as_str() {
                        "snapshot" | "save" | "save-and-apply" => {
                            let (document, application) = if operation == "save-and-apply" {
                                (data["document"].clone(), Some(data["status"].clone()))
                            } else {
                                (data, None)
                            };
                            if this.deerflow_settings.edit_generation == edits {
                                this.deerflow_settings.draft = Some(document);
                                this.deerflow_settings.observe_saved_revision();
                                this.deerflow_settings.pending_model_names.clear();
                                this.deerflow_settings.pending_agent_names.clear();
                                this.deerflow_settings.dirty = false;
                                this.deerflow_settings.errors.clear();
                                this.deerflow_settings.confirm_reload = false;
                                this.deerflow_rebind(cx);
                                if matches!(operation.as_str(), "save" | "save-and-apply") {
                                    this.deerflow_settings.needs_apply = true;
                                    this.deerflow_settings.notice =
                                        Some("配置已保存，正在等待任务完成并应用…".into());
                                } else {
                                    this.deerflow_settings.notice = Some("配置已加载。".into());
                                }
                            } else {
                                this.deerflow_settings.notice = Some(
                                    "请求已完成；保留了请求期间的新草稿，请重新加载核对。".into(),
                                );
                            }
                            if let Some(status) = application {
                                this.deerflow_settings.status = status;
                                this.deerflow_status(cx);
                                this.deerflow_poll_status(cx);
                            }
                        }
                        "manage" => this.deerflow_managed(&management_operation, data, cx),
                        "validate" => this.deerflow_settings.notice = Some("配置校验通过。".into()),
                        "test-model" => {
                            if data["ok"].as_bool() == Some(true) {
                                this.deerflow_settings.notice =
                                    Some(format!("模型连接成功，耗时 {} ms。", data["latency_ms"]));
                            } else {
                                this.deerflow_settings.error = Some(format!(
                                    "模型连接失败：{}",
                                    data["error_type"]
                                        .as_str()
                                        .unwrap_or("请检查模型地址和密钥")
                                ));
                            }
                        }
                        "apply" | "start" | "stop" | "cancel-apply" => {
                            this.deerflow_settings.status = data;
                            if operation == "start" {
                                this.deerflow_settings.refresh_after_start = true;
                            }
                            this.deerflow_settings.notice = Some(
                                match operation.as_str() {
                                    "apply" => "配置已保存，正在等待任务完成并应用…",
                                    "cancel-apply" => "已请求取消等待；保存的配置会保留。",
                                    "start" => "已请求启动服务。",
                                    _ => "已请求停止服务。",
                                }
                                .into(),
                            );
                            this.deerflow_status(cx);
                            this.deerflow_poll_status(cx);
                        }
                        _ => {}
                    },
                }
                this.deerflow_freeze_inputs(cx);
                if saved_for_exit {
                    this.deerflow_settings.exit_requested = false;
                    this.deerflow_settings.exit_after_save = false;
                    cx.quit();
                }
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }

    fn deerflow_status(&mut self, cx: &mut Context<Self>) {
        if self.deerflow_settings.status_pending {
            return;
        }
        self.deerflow_settings.status_pending = true;
        self.deerflow_settings.status_generation += 1;
        let generation = self.deerflow_settings.status_generation;
        let daemon = self.daemon.client();
        cx.spawn(async move |this, cx| {
            let result: anyhow::Result<Value> = cx
                .background_executor()
                .spawn(async move {
                    match daemon.request(
                        Uuid::nil(),
                        Uuid::nil(),
                        waku_client::Command::DeerFlow {
                            operation: "status".into(),
                            input: json!({}),
                        },
                    )? {
                        waku_client::ResponsePayload::DeerFlow { data } => Ok(data),
                        _ => anyhow::bail!("无法读取服务状态"),
                    }
                })
                .await;
            let _ = this.update(cx, |this, cx| {
                if this.deerflow_settings.status_generation != generation {
                    return;
                }
                this.deerflow_settings.status_pending = false;
                match result {
                    Ok(status) => {
                        let was_applying = this.deerflow_settings.applying();
                        let applied_generation = status["applied_generation"].as_u64().unwrap_or(0);
                        let applied = this
                            .deerflow_settings
                            .observed_applied_generation
                            .is_some_and(|previous| applied_generation > previous);
                        let ready = model_refresh_ready(&status);
                        this.deerflow_settings.status = status;
                        this.deerflow_settings.observe_saved_revision();
                        if ready
                            && this.deerflow_settings.status["warmup"] != "warming"
                            && this.deerflow_settings.startup_error.take().is_some()
                        {
                            this.deerflow_settings.error = None;
                        }
                        if let Some(error) = this.deerflow_settings.status["apply_error"].as_str() {
                            let message = if this.deerflow_settings.needs_apply {
                                format!("配置已保存，但应用失败：{error}。可点击“重试应用”。")
                            } else {
                                error.to_owned()
                            };
                            this.deerflow_settings.error = Some(redact_deerflow_message(
                                &message,
                                this.deerflow_settings.draft.as_ref(),
                            ));
                            this.deerflow_settings.notice = None;
                        }
                        if ready && (applied || this.deerflow_settings.refresh_after_start) {
                            this.deerflow_settings.needs_apply = false;
                            this.deerflow_settings.error = None;
                            this.deerflow_settings.refresh_after_start = false;
                            this.deerflow_settings.notice = Some(
                                if applied {
                                    "配置已保存并应用。模型列表已请求刷新。"
                                } else if this.deerflow_settings.status["warmup"] == "warming" {
                                    "ACP 已连接，正在预热 Agent。"
                                } else {
                                    "服务已启动。"
                                }
                                .into(),
                            );
                            this.deerflow_settings.model_refresh_queued = true;
                            this.deerflow_flush_model_refresh(cx);
                        } else if was_applying
                            && !this.deerflow_settings.applying()
                            && this.deerflow_settings.needs_apply
                            && this.deerflow_settings.status["apply_error"].is_null()
                        {
                            this.deerflow_settings.notice =
                                Some("配置已保存，尚未应用。可点击“重试应用”。".into());
                        }
                        if !this.deerflow_settings.applying() {
                            this.deerflow_settings.observed_applied_generation =
                                Some(applied_generation);
                        }
                        let phase = this.deerflow_settings.service_phase();
                        if this.deerflow_settings.startup_completion_pending {
                            match phase {
                                DeerFlowServicePhase::Ready => {
                                    this.deerflow_settings.startup_completion_pending = false;
                                    this.deerflow_settings.notice =
                                        Some("DeerFlow ACP 已就绪。".into());
                                    this.show_success_toast("DeerFlow ACP 已就绪");
                                }
                                DeerFlowServicePhase::Failed => {
                                    this.deerflow_settings.startup_completion_pending = false;
                                    if this.deerflow_settings.error.is_none() {
                                        this.deerflow_settings.error =
                                            Some("DeerFlow ACP 启动失败，请检查服务日志。".into());
                                    }
                                    this.deerflow_settings.startup_error =
                                        this.deerflow_settings.error.clone();
                                    this.show_toast("DeerFlow ACP 启动失败，请在设置中查看详情");
                                }
                                _ => {}
                            }
                        }
                        if this.deerflow_settings.applying()
                            || phase == DeerFlowServicePhase::Warming
                            || this.deerflow_settings.startup_completion_pending
                        {
                            this.deerflow_poll_status(cx);
                        }
                    }
                    Err(error) => {
                        let message = redact_deerflow_message(
                            &error.to_string(),
                            this.deerflow_settings.draft.as_ref(),
                        );
                        this.deerflow_settings.error = Some(message.clone());
                        if this.deerflow_settings.startup_completion_pending
                            && !this.deerflow_settings.startup_request_pending
                        {
                            this.deerflow_settings.startup_completion_pending = false;
                            this.deerflow_settings.startup_error = Some(message);
                            this.show_toast("DeerFlow ACP 状态查询失败，请在设置中查看详情");
                        } else if this.deerflow_settings.applying() {
                            this.deerflow_poll_status(cx);
                        }
                    }
                }
                this.deerflow_freeze_inputs(cx);
                cx.notify();
            });
        })
        .detach();
    }

    fn deerflow_poll_status(&mut self, cx: &mut Context<Self>) {
        if self.deerflow_settings.polling {
            return;
        }
        self.deerflow_settings.polling = true;
        cx.spawn(async move |this, cx| {
            cx.background_executor().timer(Duration::from_secs(1)).await;
            let _ = this.update(cx, |this, cx| {
                this.deerflow_settings.polling = false;
                this.deerflow_status(cx);
            });
        })
        .detach();
    }

    pub(super) fn deerflow_field_edited(&mut self, slot: usize, cx: &mut Context<Self>) {
        let text = self.deerflow_settings.inputs[slot]
            .read(cx)
            .content()
            .to_owned();
        // Programmatic field hydration emits Edited too. Comparing the exact
        // hydrated text avoids dirtying a loaded draft or recursively rebinding.
        if self.deerflow_settings.input_text[slot] == text || !self.deerflow_settings.can_edit() {
            return;
        }
        let Some(field) = self
            .deerflow_settings
            .fields
            .iter()
            .find(|f| f.slot == Some(slot))
            .cloned()
        else {
            return;
        };
        self.deerflow_settings.input_text[slot] = text.clone();
        self.deerflow_settings.dirty = true;
        self.deerflow_settings.edit_generation += 1;
        self.deerflow_settings.notice = None;
        self.deerflow_settings.confirm_reload = false;
        if let Some(index) = model_name_index(&field.path) {
            self.deerflow_settings
                .pending_model_names
                .insert(index, text);
            self.deerflow_settings.errors.remove(&field.path);
            if let Some(document) = &self.deerflow_settings.draft {
                if let Err(error) =
                    document_with_model_names(document, &self.deerflow_settings.pending_model_names)
                {
                    self.deerflow_settings.errors.insert(field.path, error);
                }
            }
            cx.notify();
            return;
        }
        if let Some(index) = agent_name_index(&field.path) {
            self.deerflow_settings
                .pending_agent_names
                .insert(index, text);
            self.deerflow_settings.errors.remove(&field.path);
            if let Some(document) = &self.deerflow_settings.draft {
                if let Err(error) =
                    document_with_agent_names(document, &self.deerflow_settings.pending_agent_names)
                {
                    self.deerflow_settings.errors.insert(field.path, error);
                }
            }
            cx.notify();
            return;
        }
        let parsed = parse_field(&text, field.kind).and_then(|value| {
            if field.path == "/sandbox/advanced" {
                let options = value.as_object().ok_or_else(|| "本地执行环境高级参数必须为 JSON 对象".to_owned())?;
                if options.keys().any(|key| !LOCAL_SANDBOX_OPTIONS.contains(&key.as_str())) {
                    return Err("便携版仅支持 mounts、bash_output_max_chars、read_file_output_max_chars、ls_output_max_chars".into());
                }
            }
            Ok(value)
        });
        match parsed {
            Ok(value) => {
                self.deerflow_settings.errors.retain(|path, _| {
                    path != &field.path && !path.starts_with(&(field.path.clone() + "/"))
                });
                if let Some(document) = self.deerflow_settings.draft.as_mut() {
                    if field.path.starts_with("/models/")
                        && field.path.ends_with("/api_key")
                        && !text.trim().is_empty()
                    {
                        let clear_path =
                            field.path.trim_end_matches("api_key").to_owned() + "clear_api_key";
                        set_value(document, &clear_path, json!(false));
                    }
                    set_value(document, &field.path, value);
                }
            }
            Err(error) => {
                self.deerflow_settings.errors.insert(field.path, error);
            }
        }
        cx.notify();
    }

    fn deerflow_change(&mut self, path: &str, value: Value, cx: &mut Context<Self>) {
        if !self.deerflow_settings.can_edit() {
            return;
        }
        self.deerflow_settings.errors.remove(path);
        if let Some(document) = self.deerflow_settings.draft.as_mut() {
            set_value(document, path, value);
        }
        self.deerflow_settings.dirty = true;
        self.deerflow_settings.edit_generation += 1;
        self.deerflow_settings.confirm_reload = false;
        self.deerflow_settings.notice = None;
        cx.notify();
    }

    fn deerflow_rebind(&mut self, cx: &mut Context<Self>) {
        let state = &mut self.deerflow_settings;
        let Some(doc) = state.draft.as_ref() else {
            return;
        };
        state.selected_model = state.selected_model.min(
            doc["models"]
                .as_array()
                .map_or(0, |a| a.len().saturating_sub(1)),
        );
        state.selected_agent = state.selected_agent.min(
            doc["agents"]
                .as_array()
                .map_or(0, |a| a.len().saturating_sub(1)),
        );
        let mut fields = fields_for(state);
        let (mut single, mut multi) = (0, SINGLE_FIELDS);
        for field in &mut fields {
            if !field.kind.input() {
                continue;
            }
            let slot = if field.kind.multiline() {
                let n = multi;
                multi += 1;
                n
            } else {
                let n = single;
                single += 1;
                n
            };
            if slot >= FIELD_COUNT || (!field.kind.multiline() && slot >= SINGLE_FIELDS) {
                continue;
            }
            field.slot = Some(slot);
            let text = model_name_index(&field.path)
                .and_then(|index| state.pending_model_names.get(&index))
                .or_else(|| {
                    agent_name_index(&field.path)
                        .and_then(|index| state.pending_agent_names.get(&index))
                })
                .cloned()
                .unwrap_or_else(|| {
                    field_text(doc.pointer(&field.path).unwrap_or(&Value::Null), field.kind)
                });
            state.input_text[slot] = text.clone();
            state.inputs[slot].update(cx, |input, cx| {
                input.set_masked(field.path.ends_with("/api_key"), cx);
                input.set_placeholder(field.hint.clone(), cx);
                input.set_content(text, cx);
            });
        }
        state.fields = fields;
        self.deerflow_freeze_inputs(cx);
    }

    pub(super) fn open_deerflow_tools_settings(&mut self, cx: &mut Context<Self>) {
        self.open_settings_page(SettingsPage::DeerFlow, cx);
        self.deerflow_select_section(Section::Tools, cx);
    }

    pub(super) fn open_deerflow_models_settings(&mut self, cx: &mut Context<Self>) {
        self.open_settings_page(SettingsPage::DeerFlow, cx);
        self.deerflow_select_section(Section::Models, cx);
    }

    fn deerflow_select_section(&mut self, section: Section, cx: &mut Context<Self>) {
        if self.deerflow_settings.busy() {
            return;
        }
        if !self.deerflow_settings.errors.is_empty() {
            self.deerflow_settings.error = Some("请先修正当前页面的字段格式；草稿已保留。".into());
        } else {
            self.deerflow_settings.section = section;
            self.deerflow_settings.advanced = false;
            self.deerflow_rebind(cx);
        }
        cx.notify();
    }

    fn deerflow_save_or_validate(&mut self, operation: &str, cx: &mut Context<Self>) {
        if let Some(issue) = self
            .deerflow_settings
            .draft
            .as_ref()
            .and_then(portable_issue)
        {
            self.deerflow_settings.error = Some(issue);
            cx.notify();
            return;
        }
        if !self.deerflow_settings.errors.is_empty() {
            self.deerflow_settings.error = Some("请先修正页面中的字段错误。".into());
            cx.notify();
            return;
        }
        if let Some(document) = &self.deerflow_settings.draft {
            match document_with_pending_names(
                document,
                &self.deerflow_settings.pending_model_names,
                &self.deerflow_settings.pending_agent_names,
            ) {
                Ok(document) => {
                    let operation =
                        if operation == "save" && !self.deerflow_settings.exit_after_save {
                            "save-and-apply"
                        } else {
                            operation
                        };
                    self.deerflow_request(operation, document, cx);
                }
                Err(error) => {
                    self.deerflow_settings.error = Some(error);
                    cx.notify();
                }
            }
        }
    }

    fn deerflow_add_item(&mut self, models: bool, cx: &mut Context<Self>) {
        if !self.deerflow_settings.can_edit() || !self.deerflow_settings.errors.is_empty() {
            return;
        }
        let Some(doc) = self.deerflow_settings.draft.as_mut() else {
            return;
        };
        let key = if models { "models" } else { "agents" };
        let Some(items) = doc[key].as_array_mut() else {
            return;
        };
        let prefix = if models { "model" } else { "agent" };
        let mut number = items.len() + 1;
        while items
            .iter()
            .any(|item| item["name"].as_str() == Some(&format!("{prefix}-{number}")))
        {
            number += 1;
        }
        let name = format!("{prefix}-{number}");
        let value = if models {
            json!({"original_name":"", "name":name, "display_name":"", "description":"", "use_path":"langchain_openai:ChatOpenAI", "model":"", "base_url":"", "api_key":"", "api_key_configured":false, "clear_api_key":false, "supports_thinking":false, "supports_reasoning_effort":false, "supports_vision":false, "advanced":{}})
        } else {
            json!({"original_name":"", "name":name, "display_name":"", "description":"", "model":null, "tool_groups":[], "skills":null, "memory_enabled":true, "soul":""})
        };
        items.push(value);
        let index = items.len() - 1;
        if models {
            self.deerflow_settings.selected_model = index;
        } else {
            self.deerflow_settings.selected_agent = index;
        }
        self.deerflow_settings.dirty = true;
        self.deerflow_settings.edit_generation += 1;
        self.deerflow_rebind(cx);
        cx.notify();
    }

    fn deerflow_remove_item(&mut self, models: bool, cx: &mut Context<Self>) {
        if !self.deerflow_settings.can_edit() || !self.deerflow_settings.errors.is_empty() {
            return;
        }
        if !self.deerflow_commit_names(cx) {
            return;
        }
        let state = &mut self.deerflow_settings;
        let Some(doc) = state.draft.as_mut() else {
            return;
        };
        let key = if models { "models" } else { "agents" };
        let index = if models {
            state.selected_model
        } else {
            state.selected_agent
        };
        let Some(items) = doc[key].as_array_mut() else {
            return;
        };
        if index >= items.len() || (models && items.len() <= 1) {
            return;
        }
        let removed = items.remove(index);
        let old = removed["name"].as_str().unwrap_or("");
        if models {
            let next = items
                .first()
                .and_then(|item| item["name"].as_str())
                .unwrap_or("")
                .to_owned();
            rename_model_references(doc, old, &next);
        } else if doc["runtime"]["agent_name"].as_str() == Some(old) {
            doc["runtime"]["agent_name"] = Value::Null;
        }
        state.dirty = true;
        state.edit_generation += 1;
        self.deerflow_rebind(cx);
        cx.notify();
    }
}

fn fields_for(state: &DeerFlowSettings) -> Vec<Field> {
    let mut fields = Vec::new();
    let mut add = |path: String, label: &str, hint: &str, kind| {
        fields.push(Field::new(path, label, hint, kind))
    };
    let doc = state.draft.as_ref().unwrap_or(&Value::Null);
    match state.section {
        Section::Models if doc["models"].as_array().is_some_and(|a| !a.is_empty()) => {
            let root = format!("/models/{}", state.selected_model);
            for (key, label, hint) in [
                ("name", "配置名称", "供默认模型、智能体和会话选择引用"),
                ("display_name", "显示名称", "在模型列表中显示"),
                ("description", "模型说明", "可选说明"),
                ("model", "模型 ID", "服务商提供的模型标识"),
                ("base_url", "API 地址", "例如 https://api.example.com/v1"),
                (
                    "api_key",
                    "API Key",
                    "留空保留已保存的密钥；也可填写 $环境变量名",
                ),
                ("use_path", "模型适配器", "例如 langchain_openai:ChatOpenAI"),
            ] {
                add(format!("{root}/{key}"), label, hint, Kind::Text);
            }
            add(
                format!("{root}/clear_api_key"),
                "清除已保存的密钥",
                "仅在确实需要移除密钥时开启",
                Kind::Bool,
            );
            for (key, label) in [
                ("supports_thinking", "支持思考"),
                ("supports_reasoning_effort", "支持思考强度"),
                ("supports_vision", "支持图片输入"),
            ] {
                add(
                    format!("{root}/{key}"),
                    label,
                    "请按模型服务商的能力说明设置",
                    Kind::Bool,
                );
            }
            if state.advanced {
                add(
                    format!("{root}/advanced"),
                    "模型高级参数",
                    "JSON 对象，保留额外服务商参数",
                    Kind::Json,
                );
            }
        }
        Section::Agents if doc["agents"].as_array().is_some_and(|a| !a.is_empty()) => {
            let root = format!("/agents/{}", state.selected_agent);
            for (key, label, hint, kind) in [
                ("name", "智能体名称", "英文字母、数字和连字符", Kind::Text),
                (
                    "display_name",
                    "显示名称",
                    "可选的人类可读名称",
                    Kind::Optional,
                ),
                ("description", "说明", "智能体用途", Kind::Text),
                ("model", "模型配置名称", "留空继承默认模型", Kind::Optional),
                (
                    "memory_enabled",
                    "读写长期记忆",
                    "关闭后此智能体不读取或写入记忆",
                    Kind::Bool,
                ),
                ("soul", "角色与行为指令", "保存到 SOUL.md", Kind::Multiline),
            ] {
                add(format!("{root}/{key}"), label, hint, kind);
            }
            if state.advanced {
                add(
                    format!("{root}/tool_groups"),
                    "工具组",
                    "JSON 字符串数组",
                    Kind::Json,
                );
                add(
                    format!("{root}/skills"),
                    "可用技能",
                    "null 继承全部已启用技能；[] 禁用技能",
                    Kind::Json,
                );
                add(
                    "/subagents".into(),
                    "子智能体配置",
                    "JSON 对象，包含内置子智能体及并发配置",
                    Kind::Json,
                );
            }
        }
        Section::Memory => {
            for (key, label, hint, kind) in [
                (
                    "enabled",
                    "启用长期记忆",
                    "使用便携目录中的本地 DeerMem",
                    Kind::Bool,
                ),
                (
                    "injection_enabled",
                    "自动注入记忆",
                    "在会话上下文中使用相关事实",
                    Kind::Bool,
                ),
                (
                    "retrieval_enabled",
                    "检索记忆",
                    "按相关度检索已有记忆",
                    Kind::Bool,
                ),
                (
                    "mode",
                    "记忆接入方式",
                    "",
                    Kind::Choice(&[("middleware", "自动中间件"), ("tool", "工具调用")]),
                ),
                (
                    "model_name",
                    "记忆处理模型",
                    "留空继承默认模型",
                    Kind::Optional,
                ),
                ("max_facts", "最多事实数", "10–500", Kind::Integer),
                (
                    "max_injection_tokens",
                    "记忆注入 Token 上限",
                    "100–8000",
                    Kind::Integer,
                ),
                ("retrieval_top_k", "检索结果数", "1–100", Kind::Integer),
                (
                    "fact_confidence_threshold",
                    "事实置信度阈值",
                    "0–1",
                    Kind::Number,
                ),
                (
                    "fact_dedup_enabled",
                    "事实去重",
                    "合并高度相似的事实",
                    Kind::Bool,
                ),
                (
                    "debounce_seconds",
                    "写入防抖时间（秒）",
                    "1–300",
                    Kind::Integer,
                ),
                ("storage_path", "记忆文件", "空白使用默认路径", Kind::Text),
                (
                    "retrieval_index_path",
                    "检索索引",
                    "空白使用默认路径",
                    Kind::Text,
                ),
            ] {
                add(format!("/memory/{key}"), label, hint, kind);
            }
            if state.advanced {
                add(
                    "/memory/shutdown_flush_timeout_seconds".into(),
                    "关闭时等待记忆写入（秒）",
                    "0.1–300",
                    Kind::Number,
                );
                add(
                    "/memory/fact_dedup_similarity_threshold".into(),
                    "去重相似度阈值",
                    "0.5–1",
                    Kind::Number,
                );
                add(
                    "/memory/storage_class".into(),
                    "记忆存储实现",
                    "本地记忆存储类路径",
                    Kind::Text,
                );
                add(
                    "/memory/advanced".into(),
                    "记忆高级参数",
                    "JSON 对象",
                    Kind::Json,
                );
                add(
                    "/memory/backend_advanced".into(),
                    "本地存储高级参数",
                    "JSON 对象",
                    Kind::Json,
                );
            }
        }
        Section::Skills => {
            add(
                "/skills_enabled".into(),
                "启用技能",
                "控制 DeerFlow 是否加载技能",
                Kind::Bool,
            );
            if let Some(skills) = doc["skills"].as_array() {
                for (index, skill) in skills.iter().enumerate() {
                    add(
                        format!("/skills/{index}/enabled"),
                        skill["name"].as_str().unwrap_or("技能"),
                        skill["description"].as_str().unwrap_or(""),
                        Kind::Bool,
                    );
                }
            }
            if state.advanced {
                add(
                    "/skill_evolution".into(),
                    "技能自进化配置",
                    "JSON 对象，保留审核与自动修订规则",
                    Kind::Json,
                );
            }
        }
        Section::Tools => {
            add(
                "/runtime/permission_mode".into(),
                "工具审批",
                "选择需要请求用户批准的范围",
                Kind::Choice(&[
                    ("all", "每次调用"),
                    ("dangerous", "危险操作"),
                    ("off", "不请求审批"),
                ]),
            );
            for (path, label, hint) in [
                (
                    "/runtime/enable_bash",
                    "启用命令工具",
                    "允许智能体调用命令工具；Go 后端在 Windows 使用 PowerShell",
                ),
                (
                    "/sandbox/allow_host_bash",
                    "允许主机命令",
                    "允许在本机运行命令",
                ),
                (
                    "/sandbox/allow_host_tools",
                    "允许主机工具",
                    "允许访问本机工具能力",
                ),
            ] {
                add(path.into(), label, hint, Kind::Bool);
            }

            add(
                "/runtime/tool_allowlist".into(),
                "工具允许列表",
                "JSON 数组；null 表示不限制",
                Kind::Json,
            );
            add(
                "/runtime/tool_denylist".into(),
                "工具拒绝列表",
                "JSON 字符串数组",
                Kind::Json,
            );
            add(
                "/client_mcp_servers".into(),
                "会话 MCP 服务器（Go ACP）",
                "JSON 数组；仅支持 stdio。每项填写 name、绝对 command、args 数组及 env 名值数组；敏感值请放在 env。须先在宿主 config.yaml 启用 local_acp.accept_client_mcp_servers，并将命令加入 client_mcp_allowed_commands。环境变量值保存后会脱敏。",
                Kind::Json,
            );
            if state.advanced {
                add(
                    "/tools".into(),
                    "工具定义",
                    "JSON 数组，保留未知工具参数",
                    Kind::Json,
                );
                add("/tool_groups".into(), "工具组定义", "JSON 数组", Kind::Json);
                add(
                    "/sandbox/advanced".into(),
                    "执行环境高级参数",
                    "JSON 对象；脱敏值由服务安全还原",
                    Kind::Json,
                );
            }
        }
        Section::Runtime => {
            let go_backend = state.status["acp_backend"] == "go";
            for (key, label, hint, kind) in [
                (
                    "model_name",
                    "默认会话模型",
                    "留空继承全局默认模型",
                    Kind::Optional,
                ),
                (
                    "agent_name",
                    "默认智能体",
                    if go_backend {
                        "Go ACP 当前仅运行默认智能体；此项暂不生效"
                    } else {
                        "留空使用默认智能体"
                    },
                    Kind::Optional,
                ),
                (
                    "thinking_enabled",
                    "默认开启思考",
                    "新会话默认设置",
                    Kind::Bool,
                ),
                ("plan_mode", "默认开启计划", "新会话默认设置", Kind::Bool),
                (
                    "subagent_enabled",
                    "启用子智能体",
                    if go_backend {
                        "Go ACP 当前提供通用 task 委派；自定义子智能体配置尚未接入"
                    } else {
                        "允许委派独立工作"
                    },
                    Kind::Bool,
                ),
                (
                    "max_concurrent_subagents",
                    "子智能体并发数",
                    "1–4",
                    Kind::Integer,
                ),
                (
                    "max_active_connections",
                    "最大连接数",
                    "1–128",
                    Kind::Integer,
                ),
                ("max_active_runs", "同时运行任务数", "1–128", Kind::Integer),
                (
                    "run_timeout_seconds",
                    "运行超时（秒）",
                    "大于 0",
                    Kind::Number,
                ),
                (
                    "queue_timeout_seconds",
                    "排队超时（秒）",
                    "1–86400",
                    Kind::Number,
                ),
                (
                    "memory_scope",
                    "记忆范围",
                    if go_backend {
                        "Go ACP 支持工作区和会话范围；全局范围暂不支持"
                    } else {
                        ""
                    },
                    Kind::Choice(&[
                        ("workspace", "工作区"),
                        ("session", "单个会话"),
                        ("global", "全局"),
                    ]),
                ),
                (
                    "session_cleanup_enabled",
                    "清理过期会话",
                    "清理规则同时影响会话恢复",
                    Kind::Bool,
                ),
                (
                    "inactive_session_retention_days",
                    "不活跃会话保留天数",
                    "1–3650",
                    Kind::Integer,
                ),
                (
                    "closed_session_retention_days",
                    "已关闭会话保留天数",
                    "0–3650",
                    Kind::Integer,
                ),
                (
                    "goal_auto_continue",
                    "自动继续目标",
                    "按配置的次数限制继续执行",
                    Kind::Bool,
                ),
                (
                    "goal_max_continuations",
                    "最多自动继续次数",
                    "0–8",
                    Kind::Integer,
                ),
            ] {
                add(format!("/runtime/{key}"), label, hint, kind);
            }
            if state.advanced {
                add(
                    "/runtime/prompt_overlay".into(),
                    "附加系统指令",
                    "在默认指令上追加",
                    Kind::Multiline,
                );
                add(
                    "/runtime/session_cleanup_interval_seconds".into(),
                    "会话清理间隔（秒）",
                    "60–86400",
                    Kind::Number,
                );
                add(
                    "/runtime/goal_max_no_progress_continuations".into(),
                    "无进展时最多继续次数",
                    "0–8",
                    Kind::Integer,
                );
            }
        }
        _ => {}
    }
    if state.section == Section::Agents
        && state.advanced
        && !fields.iter().any(|field| field.path == "/subagents")
    {
        fields.push(Field::new(
            "/subagents",
            "子智能体配置",
            "JSON 对象，包含内置子智能体及并发配置",
            Kind::Json,
        ));
    }
    fields
}

fn df_group(theme: Theme) -> Div {
    div()
        .mt(px(15.0))
        .w_full()
        .px(px(20.0))
        .py(px(16.0))
        .rounded(px(13.0))
        .bg(theme.raised)
        .flex()
        .flex_col()
        .gap(px(16.0))
}
fn df_label(label: impl Into<SharedString>, hint: impl Into<SharedString>, theme: Theme) -> Div {
    let label: SharedString = label.into();
    let hint: SharedString = hint.into();
    div()
        .flex_1()
        .min_w_0()
        .child(
            div()
                .text_size(sp(13.5))
                .font_weight(FontWeight::MEDIUM)
                .text_color(theme.text)
                .child(label),
        )
        .child(
            div()
                .mt(px(4.0))
                .text_size(sp(12.5))
                .line_height(sp(18.0))
                .text_color(theme.text_secondary)
                .child(hint),
        )
}
fn df_button(
    id: impl Into<gpui::ElementId>,
    label: impl Into<SharedString>,
    disabled: bool,
    selected: bool,
    theme: Theme,
    cx: &mut Context<Waku>,
    action: impl Fn(&mut Waku, &mut Window, &mut Context<Waku>) + 'static,
) -> Stateful<Div> {
    let label: SharedString = label.into();
    let button = div()
        .id(id)
        .tab_index(0)
        .focus_visible(|style| style.border_color(theme.accent))
        .h(px(29.0))
        .px(px(10.0))
        .rounded(px(7.0))
        .border_1()
        .border_color(if selected {
            theme.accent
        } else {
            theme.border_strong
        })
        .bg(if selected {
            theme.overlay
        } else {
            theme.raised
        })
        .flex()
        .flex_none()
        .items_center()
        .cursor_default()
        .text_size(sp(12.5))
        .text_color(if selected {
            theme.text
        } else {
            theme.text_secondary
        })
        .when(disabled, |el| el.opacity(0.5))
        .when(!disabled, |el| el.hover(|el| el.bg(theme.overlay)))
        .child(label);
    if disabled {
        button
    } else {
        button.on_activation(cx, action)
    }
}

impl Waku {
    pub(super) fn render_deerflow_settings(&self, cx: &mut Context<Self>) -> AnyElement {
        let state = &self.deerflow_settings;
        let theme = Theme::current(cx);
        let busy = state.busy();
        let editable = state.can_edit();
        let invalid = !state.errors.is_empty();
        let loaded = state.draft.is_some();
        let unsupported = state.draft.as_ref().and_then(portable_issue).is_some();
        let mut tabs = div().mt(px(15.0)).flex().flex_wrap().gap(px(6.0));
        for (section, label) in Section::ALL {
            tabs = tabs.child(df_button(
                SharedString::from(format!("df-tab-{label}")),
                label,
                busy,
                state.section == section,
                theme,
                cx,
                move |this, _, cx| this.deerflow_select_section(section, cx),
            ));
        }
        let mut toolbar = div()
            .flex()
            .flex_wrap()
            .gap(px(6.0))
            .child(df_button(
                "df-save",
                "保存并应用",
                !editable || invalid || !state.dirty,
                false,
                theme,
                cx,
                |this, _, cx| this.deerflow_save_or_validate("save", cx),
            ))
            .child(df_button(
                "df-validate",
                "校验",
                !editable || invalid,
                false,
                theme,
                cx,
                |this, _, cx| this.deerflow_save_or_validate("validate", cx),
            ))
            .when(state.needs_apply, |toolbar| {
                toolbar.child(df_button(
                    "df-apply",
                    "重试应用",
                    busy || !loaded || state.dirty || invalid || unsupported || state.applying(),
                    false,
                    theme,
                    cx,
                    |this, _, cx| this.deerflow_request("apply", json!({}), cx),
                ))
            })
            .child(df_button(
                "df-reload",
                "重新加载",
                busy || state.applying(),
                false,
                theme,
                cx,
                |this, _, cx| {
                    if this.deerflow_settings.dirty {
                        this.deerflow_settings.confirm_reload = true;
                        cx.notify();
                    } else {
                        this.deerflow_request("snapshot", json!({}), cx);
                    }
                },
            ));
        if state.applying() {
            toolbar = toolbar.child(df_button(
                "df-cancel-apply",
                "取消等待",
                busy,
                false,
                theme,
                cx,
                |this, _, cx| this.deerflow_request("cancel-apply", json!({}), cx),
            ));
        }
        let running_text = match state.service_phase() {
            DeerFlowServicePhase::Unknown => "正在读取服务状态",
            DeerFlowServicePhase::Starting => "正在启动 ACP 服务",
            DeerFlowServicePhase::Warming => "ACP 已连接，正在预热 Agent",
            DeerFlowServicePhase::Ready => "ACP 服务已就绪",
            DeerFlowServicePhase::Applying => "等待任务完成后应用配置",
            DeerFlowServicePhase::Stopping => "正在停止 ACP 服务",
            DeerFlowServicePhase::Failed => {
                if state.startup_failed() {
                    "ACP 服务启动失败"
                } else {
                    "ACP 配置应用失败"
                }
            }
            DeerFlowServicePhase::Stopped => "服务未启动",
        };
        let edit_text = if let Some(operation) = state.pending.as_deref() {
            match operation {
                "save" | "save-and-apply" => "正在保存…",
                "snapshot" => "正在加载…",
                "validate" => "正在校验…",
                "test-model" => "正在测试模型…",
                _ => "正在处理…",
            }
        } else if state.dirty {
            "有未保存的更改"
        } else if state.needs_apply {
            "配置已保存，尚未应用"
        } else {
            "更改已保存"
        };
        let mut body = div().child(tabs).child(
            df_group(theme)
                .child(df_label(
                    "DeerFlow ACP",
                    format!("{running_text} · {edit_text}"),
                    theme,
                ))
                .child(toolbar)
                .child(
                    div()
                        .text_size(sp(12.5))
                        .line_height(sp(18.0))
                        .text_color(theme.text_tertiary)
                        .child("保存后自动应用配置；有任务运行时会等待任务结束。"),
                ),
        );
        if let Some(issue) = state.draft.as_ref().and_then(portable_issue) {
            let mut migration = df_group(theme).child(df_label("已有配置需要确认迁移", issue, theme))
                .child(df_label("原配置尚未修改", "转换会在草稿中固定本地执行环境和 DeerMem，并移除非本地沙箱参数；检查后再保存。", theme));
            if state.confirm_portable {
                migration = migration.child(
                    div()
                        .flex()
                        .gap(px(6.0))
                        .child(df_button(
                            "df-portable-confirm",
                            "确认转换草稿",
                            busy,
                            false,
                            theme,
                            cx,
                            |this, _, cx| {
                                if let Some(doc) = this.deerflow_settings.draft.as_mut() {
                                    normalize_portable_document(doc);
                                }
                                this.deerflow_settings.confirm_portable = false;
                                this.deerflow_settings.dirty = true;
                                this.deerflow_settings.edit_generation += 1;
                                this.deerflow_settings.errors.clear();
                                this.deerflow_rebind(cx);
                                cx.notify();
                            },
                        ))
                        .child(df_button(
                            "df-portable-cancel",
                            "取消",
                            busy,
                            false,
                            theme,
                            cx,
                            |this, _, cx| {
                                this.deerflow_settings.confirm_portable = false;
                                cx.notify();
                            },
                        )),
                );
            } else {
                migration = migration.child(df_button(
                    "df-portable-review",
                    "转换为本地便携配置…",
                    busy,
                    false,
                    theme,
                    cx,
                    |this, _, cx| {
                        this.deerflow_settings.confirm_portable = true;
                        cx.notify();
                    },
                ));
            }
            body = body.child(migration);
        }
        if state.confirm_reload {
            body = body.child(
                df_group(theme)
                    .child(df_label(
                        "放弃未保存的更改？",
                        "重新加载会以磁盘配置替换当前草稿。",
                        theme,
                    ))
                    .child(
                        div()
                            .flex()
                            .gap(px(8.0))
                            .child(df_button(
                                "df-discard-confirm",
                                "放弃并重新加载",
                                busy,
                                false,
                                theme,
                                cx,
                                |this, _, cx| this.deerflow_request("snapshot", json!({}), cx),
                            ))
                            .child(df_button(
                                "df-discard-cancel",
                                "继续编辑",
                                busy,
                                false,
                                theme,
                                cx,
                                |this, _, cx| {
                                    this.deerflow_settings.confirm_reload = false;
                                    cx.notify();
                                },
                            )),
                    ),
            );
        }
        if let Some((description, request)) = &state.confirm_manage {
            let request = request.clone();
            body = body.child(
                df_group(theme)
                    .child(df_label("确认执行操作？", description.clone(), theme))
                    .child(
                        div()
                            .flex()
                            .gap(px(6.0))
                            .child(df_button(
                                "df-manage-confirm",
                                "确认",
                                busy || state.applying(),
                                false,
                                theme,
                                cx,
                                move |this, _, cx| {
                                    if request["operation"] == "session.delete" {
                                        this.deerflow_delete_session(
                                            request["session_id"].as_str().unwrap_or("").to_owned(),
                                            cx,
                                        );
                                    } else {
                                        this.deerflow_request("manage", request.clone(), cx);
                                    }
                                },
                            ))
                            .child(df_button(
                                "df-manage-cancel",
                                "取消",
                                busy,
                                false,
                                theme,
                                cx,
                                |this, _, cx| {
                                    this.deerflow_settings.confirm_manage = None;
                                    cx.notify();
                                },
                            )),
                    ),
            );
        }
        if let Some(error) = &state.error {
            body = body.child(
                div()
                    .mt(px(12.0))
                    .p(px(12.0))
                    .rounded(px(8.0))
                    .bg(theme.danger_soft)
                    .text_size(sp(12.5))
                    .line_height(sp(18.0))
                    .text_color(theme.danger)
                    .child(error.clone()),
            );
        }
        if let Some(notice) = &state.notice {
            body = body.child(
                div()
                    .mt(px(12.0))
                    .text_size(sp(12.5))
                    .line_height(sp(18.0))
                    .text_color(theme.text_secondary)
                    .child(notice.clone()),
            );
        }
        if !loaded {
            return body.into_any_element();
        }
        let doc = state.draft.as_ref().unwrap();
        match state.section {
            Section::Overview => {
                let mut group = df_group(theme)
                    .child(df_label(
                        "运行服务",
                        "打开桌面程序时会自动启动；也可在这里手动停止或重试。",
                        theme,
                    ))
                    .child(
                        div()
                            .flex()
                            .flex_wrap()
                            .gap(px(6.0))
                            .child(df_button(
                                "df-start",
                                "启动服务",
                                busy || state.applying()
                                    || state.status["running"].as_bool() == Some(true),
                                false,
                                theme,
                                cx,
                                |this, _, cx| this.deerflow_request("start", json!({}), cx),
                            ))
                            .child(df_button(
                                "df-stop",
                                "停止服务",
                                busy || state.applying()
                                    || state.status["running"].as_bool() != Some(true)
                                    || state.status["active_operations"].as_u64().unwrap_or(0) > 0,
                                false,
                                theme,
                                cx,
                                |this, _, cx| this.deerflow_request("stop", json!({}), cx),
                            ))
                            .child(df_button(
                                "df-status",
                                "刷新状态",
                                state.status_pending,
                                false,
                                theme,
                                cx,
                                |this, _, cx| this.deerflow_status(cx),
                            )),
                    );
                for (label, value) in [
                    (
                        "默认模型",
                        doc["default_model"].as_str().unwrap_or("未配置").to_owned(),
                    ),
                    (
                        "模型",
                        doc["models"].as_array().map_or(0, Vec::len).to_string(),
                    ),
                    (
                        "智能体",
                        doc["agents"].as_array().map_or(0, Vec::len).to_string(),
                    ),
                    (
                        "活跃任务",
                        state.status["active_runs"]
                            .as_u64()
                            .unwrap_or(0)
                            .to_string(),
                    ),
                    (
                        "等待任务",
                        state.status["queued_runs"]
                            .as_u64()
                            .unwrap_or(0)
                            .to_string(),
                    ),
                ] {
                    group = group.child(df_label(label, value, theme));
                }
                body = body.child(group);
                let mut paths = df_group(theme).child(df_label(
                    "便携数据",
                    "配置、历史、记忆与产物保存在用户数据目录。",
                    theme,
                ));
                if let Some(map) = doc["paths"].as_object() {
                    for key in ["user_data", "config", "skills", "agents", "memory"] {
                        if let Some(value) = map.get(key).and_then(Value::as_str) {
                            paths = paths.child(df_label(key, value.to_owned(), theme));
                        }
                    }
                }
                body = body.child(paths);
            }
            Section::Models | Section::Agents => {
                let models = state.section == Section::Models;
                let key = if models { "models" } else { "agents" };
                let items = doc[key].as_array().map(Vec::as_slice).unwrap_or(&[]);
                let selected = if models {
                    state.selected_model
                } else {
                    state.selected_agent
                };
                let mut picker = div().flex().flex_wrap().gap(px(6.0));
                for (index, item) in items.iter().enumerate() {
                    let name = item["name"].as_str().unwrap_or("未命名");
                    let label = if models && doc["default_model"].as_str() == Some(name) {
                        format!("{name} · 默认")
                    } else {
                        name.to_owned()
                    };
                    picker = picker.child(df_button(
                        SharedString::from(format!("df-item-{index}")),
                        label,
                        busy || invalid,
                        index == selected,
                        theme,
                        cx,
                        move |this, _, cx| {
                            if models {
                                this.deerflow_settings.selected_model = index;
                            } else {
                                this.deerflow_settings.selected_agent = index;
                            }
                            this.deerflow_rebind(cx);
                            cx.notify();
                        },
                    ));
                }
                let mut actions = div()
                    .flex()
                    .flex_wrap()
                    .gap(px(6.0))
                    .child(df_button(
                        "df-add-item",
                        if models {
                            "添加模型"
                        } else {
                            "添加智能体"
                        },
                        !editable || invalid,
                        false,
                        theme,
                        cx,
                        move |this, _, cx| this.deerflow_add_item(models, cx),
                    ))
                    .child(df_button(
                        "df-remove-item",
                        "从草稿移除",
                        !editable || invalid || items.is_empty() || (models && items.len() <= 1),
                        false,
                        theme,
                        cx,
                        move |this, _, cx| this.deerflow_remove_item(models, cx),
                    ));
                if models && let Some(model) = items.get(selected) {
                    let name = model["name"].as_str().unwrap_or("").to_owned();
                    actions = actions
                        .child(df_button(
                            "df-model-default",
                            "设为默认",
                            !editable
                                || invalid
                                || doc["default_model"].as_str() == Some(name.as_str()),
                            false,
                            theme,
                            cx,
                            move |this, _, cx| {
                                this.deerflow_change("/default_model", json!(name), cx)
                            },
                        ))
                        .child(df_button(
                            "df-model-test",
                            "测试连接",
                            !editable || invalid,
                            false,
                            theme,
                            cx,
                            |this, _, cx| {
                                let index = this.deerflow_settings.selected_model;
                                if let Some(document) = &this.deerflow_settings.draft {
                                    match document_with_pending_names(
                                        document,
                                        &this.deerflow_settings.pending_model_names,
                                        &this.deerflow_settings.pending_agent_names,
                                    ) {
                                        Ok(document) => this.deerflow_request(
                                            "test-model",
                                            json!({"model":document["models"][index]}),
                                            cx,
                                        ),
                                        Err(error) => {
                                            this.deerflow_settings.error = Some(error);
                                            cx.notify();
                                        }
                                    }
                                }
                            },
                        ));
                }
                if (models && !state.pending_model_names.is_empty())
                    || (!models && !state.pending_agent_names.is_empty())
                {
                    actions = actions.child(df_button(
                        "df-model-name-commit",
                        "确认名称",
                        !editable || invalid,
                        false,
                        theme,
                        cx,
                        |this, _, cx| {
                            this.deerflow_commit_names(cx);
                        },
                    ));
                }
                body = body.child(
                    df_group(theme).child(picker).child(actions).child(
                        div()
                            .text_size(sp(12.5))
                            .text_color(theme.text_tertiary)
                            .child(if models {
                                "名称在确认或保存时统一更新引用；密钥留空保留已有值。测试连接会发送一条简短的真实模型请求。"
                            } else {
                                "所有修改先保留在草稿中，保存后自动应用智能体配置。"
                            }),
                    ),
                );
            }
            Section::Skills => {
                body = body.child(self.render_deerflow_proposals(theme, cx));
            }
            Section::Memory => {
                body = body.child(df_group(theme).child(df_label(
                    "本地 DeerMem",
                    "记忆管理器固定为 DeerMem，不依赖外部记忆服务。",
                    theme,
                )));
                body = body.child(self.render_deerflow_data(theme, cx));
            }
            Section::Tools => {
                body = body.child(df_group(theme).child(df_label(
                    "本地工具执行环境",
                    "便携版固定使用 LocalSandboxProvider；命令与主机工具访问由以下权限控制。",
                    theme,
                )));
            }
            _ => {}
        }
        if !state.fields.is_empty() {
            let mut group = df_group(theme);
            for field in &state.fields {
                group = group.child(self.render_deerflow_field(field, theme, cx));
            }
            body = body.child(group);
        }
        if state.section != Section::Overview {
            body = body.child(div().mt(px(15.0)).child(df_button(
                "df-advanced",
                if state.advanced {
                    "收起高级设置"
                } else {
                    "展开高级设置"
                },
                busy || invalid,
                state.advanced,
                theme,
                cx,
                |this, _, cx| {
                    this.deerflow_settings.advanced = !this.deerflow_settings.advanced;
                    this.deerflow_rebind(cx);
                    cx.notify();
                },
            )));
        }
        body.into_any_element()
    }

    fn render_deerflow_field(
        &self,
        field: &Field,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let current = state
            .draft
            .as_ref()
            .and_then(|doc| doc.pointer(&field.path))
            .unwrap_or(&Value::Null);
        let label = df_label(field.label.clone(), field.hint.clone(), theme);
        let path = field.path.clone();
        let mut row = div().w_full().flex().gap(px(24.0));
        match field.kind {
            Kind::Bool => {
                let checked = current.as_bool().unwrap_or(false);
                row = row.items_center().child(label).child(toggle_switch(
                    SharedString::from(format!("df-toggle-{path}")),
                    checked,
                    !state.can_edit(),
                    theme,
                    cx,
                    move |this, _, cx| this.deerflow_change(&path, json!(!checked), cx),
                ));
            }
            Kind::Choice(choices) => {
                let mut options = div().flex().flex_wrap().gap(px(4.0));
                for (value, caption) in choices {
                    let selected = current.as_str() == Some(*value);
                    let path = path.clone();
                    let value = *value;
                    options = options.child(df_button(
                        SharedString::from(format!("df-choice-{path}-{value}")),
                        *caption,
                        !state.can_edit(),
                        selected,
                        theme,
                        cx,
                        move |this, _, cx| this.deerflow_change(&path, json!(value), cx),
                    ));
                }
                row = row.flex_col().gap(px(8.0)).child(label).child(options);
            }
            kind => {
                if let Some(slot) = field.slot {
                    if kind.multiline() {
                        row = row.flex_col().gap(px(8.0)).child(label).child(
                            div()
                                .id(SharedString::from(format!("df-multiline-{path}")))
                                .h(px(if matches!(kind, Kind::Json) {
                                    160.0
                                } else {
                                    200.0
                                }))
                                .w_full()
                                .flex_none()
                                .overflow_hidden()
                                .overflow_y_scroll()
                                .p(px(10.0))
                                .rounded(px(6.0))
                                .border_1()
                                .border_color(theme.border_strong)
                                .bg(theme.inset)
                                .text_size(sp(12.5))
                                .line_height(sp(18.0))
                                .child(state.inputs[slot].clone()),
                        );
                    } else {
                        row = row.items_start().child(label).child(
                            TextField::new(
                                SharedString::from(format!("df-field-{path}")),
                                state.inputs[slot].clone(),
                            )
                            .w(px(300.0))
                            .flex_none(),
                        );
                    }
                }
            }
        }
        let mut wrapper = div().child(row);
        if let Some(error) = state.errors.get(&field.path) {
            wrapper = wrapper.child(
                div()
                    .mt(px(4.0))
                    .text_size(sp(12.5))
                    .text_color(theme.danger)
                    .child(error.clone()),
            );
        }
        wrapper.into_any_element()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn parsing_keeps_null_inheritance_and_rejects_non_finite_numbers() {
        assert_eq!(parse_field("  ", Kind::Optional).unwrap(), Value::Null);
        assert!(parse_field("NaN", Kind::Number).is_err());
        assert!(parse_field("1.5", Kind::Integer).is_err());
        assert_eq!(parse_field("[]", Kind::Json).unwrap(), json!([]));
    }
    #[test]
    fn model_rename_updates_references_without_changing_remote_model_ids() {
        let mut doc = json!({"default_model":"old","runtime":{"model_name":"old"},"memory":{"model_name":null},"agents":[{"model":"old"}],"models":[{"name":"old","model":"old"}]});
        rename_model_references(&mut doc, "old", "new");
        assert_eq!(doc["default_model"], "new");
        assert_eq!(doc["runtime"]["model_name"], "new");
        assert_eq!(doc["agents"][0]["model"], "new");
        assert_eq!(doc["models"][0]["model"], "old");
        assert!(doc["memory"]["model_name"].is_null());
    }
}

const LOCAL_SANDBOX: &str = "deerflow.sandbox.local:LocalSandboxProvider";

fn deletable_phase(phase: &Value) -> bool {
    phase.is_null() || phase.as_str() == Some("idle")
}

/// Runs entirely on a worker. Close only this desktop's matching drivers,
/// wait for the ACP coordinator to release ownership, and verify both
/// persistent stores before reporting success to the UI.
fn delete_deerflow_history(
    native_id: &str,
    local_ids: &[Uuid],
    owns_connection: bool,
    mut request: impl FnMut(Uuid, waku_client::Command) -> anyhow::Result<waku_client::ResponsePayload>,
    mut close: impl FnMut(),
    mut wait: impl FnMut(),
) -> anyhow::Result<()> {
    use waku_client::{Command, ResponsePayload};
    let manage = |request: &mut dyn FnMut(Uuid, Command) -> anyhow::Result<ResponsePayload>,
                  input| {
        match request(
            Uuid::nil(),
            Command::DeerFlow {
                operation: "manage".into(),
                input,
            },
        )? {
            ResponsePayload::DeerFlow { data } => Ok(data),
            _ => anyhow::bail!("会话管理返回了无法识别的响应"),
        }
    };
    let phase = |data: &Value| -> anyhow::Result<Value> {
        let sessions = data["sessions"]
            .as_array()
            .ok_or_else(|| anyhow::anyhow!("会话列表响应不完整，请重试"))?;
        Ok(sessions
            .iter()
            .find(|session| session["session_id"] == native_id)
            .map(|session| session["phase"].clone())
            .unwrap_or(Value::Null))
    };
    let current = phase(&manage(&mut request, json!({"operation":"session.list"}))?)?;
    if !deletable_phase(&current) {
        anyhow::bail!("会话正在执行或切换连接，请先停止任务后再删除");
    }
    if !current.is_null() {
        if !owns_connection {
            anyhow::bail!("会话仍连接其他客户端，请先在该客户端关闭会话后重试");
        }
        close();
        let mut released = false;
        for _ in 0..40 {
            let current = phase(&manage(&mut request, json!({"operation":"session.list"}))?)?;
            if current.is_null() {
                released = true;
                break;
            }
            if !matches!(current.as_str(), Some("idle" | "disconnecting")) {
                anyhow::bail!("会话状态已变化，请刷新列表后重试");
            }
            wait();
        }
        if !released {
            anyhow::bail!("会话连接尚未释放，历史记录已保留，请稍后重试");
        }
    }
    let deleted = manage(
        &mut request,
        json!({"operation":"session.delete","session_id":native_id}),
    )?;
    if !deleted["deleted"]
        .as_array()
        .is_some_and(|ids| ids.iter().any(|id| id == native_id))
    {
        anyhow::bail!("服务未确认删除该会话，列表记录已保留，请重试");
    }
    for id in local_ids {
        match request(*id, Command::RemoveSession) {
            Ok(ResponsePayload::Ack) => {}
            Ok(_) => anyhow::bail!("历史已删除，但桌面列表清理未确认，请重试"),
            Err(error) => anyhow::bail!("历史已删除，但桌面列表清理失败，请重试：{error}"),
        }
    }
    Ok(())
}

const LOCAL_SANDBOX_OPTIONS: &[&str] = &[
    "mounts",
    "bash_output_max_chars",
    "read_file_output_max_chars",
    "ls_output_max_chars",
];

#[cfg(test)]
mod deletion_tests {
    use super::*;
    use std::cell::RefCell;
    use waku_client::{Command, ResponsePayload};

    fn reply(data: Value) -> anyhow::Result<ResponsePayload> {
        Ok(ResponsePayload::DeerFlow { data })
    }

    #[test]
    fn idle_connection_is_released_before_both_stores_are_deleted() {
        let local = Uuid::new_v4();
        let events = RefCell::new(Vec::new());
        let mut phases = [json!("idle"), json!("disconnecting"), Value::Null].into_iter();
        delete_deerflow_history("native", &[local], true, |id, command| {
            match command {
                Command::DeerFlow { input, .. } if input["operation"] == "session.list" => {
                    events.borrow_mut().push("list");
                    reply(json!({"sessions":[{"session_id":"native","phase":phases.next().unwrap()}]}))
                }
                Command::DeerFlow { input, .. } => {
                    assert_eq!(input, json!({"operation":"session.delete","session_id":"native"}));
                    events.borrow_mut().push("delete-history");
                    reply(json!({"deleted":["native"]}))
                }
                Command::RemoveSession => {
                    assert_eq!(id, local);
                    events.borrow_mut().push("delete-desktop");
                    Ok(ResponsePayload::Ack)
                }
                _ => panic!("unexpected command"),
            }
        }, || events.borrow_mut().push("close"), || events.borrow_mut().push("wait")).unwrap();
        assert_eq!(
            *events.borrow(),
            [
                "list",
                "close",
                "list",
                "wait",
                "list",
                "delete-history",
                "delete-desktop"
            ]
        );
    }

    #[test]
    fn busy_or_foreign_connections_never_close_or_delete() {
        for (phase, owned) in [
            ("running", true),
            ("loading", true),
            ("mutating", true),
            ("disconnecting", true),
            ("idle", false),
        ] {
            let result = delete_deerflow_history(
                "native",
                &[],
                owned,
                |_, command| {
                    assert!(
                        matches!(command, Command::DeerFlow { ref input, .. } if input["operation"] == "session.list")
                    );
                    reply(json!({"sessions":[{"session_id":"native","phase":phase}]}))
                },
                || panic!("must not close"),
                || panic!("must not wait"),
            );
            assert!(result.is_err(), "{phase}");
        }
    }

    #[test]
    fn unconfirmed_purge_does_not_remove_desktop_record() {
        for deleted in [json!([]), json!(["another-session"]), Value::Null] {
            let result = delete_deerflow_history(
                "native",
                &[Uuid::new_v4()],
                false,
                |_, command| match command {
                    Command::DeerFlow { input, .. } if input["operation"] == "session.list" => {
                        reply(json!({"sessions":[]}))
                    }
                    Command::DeerFlow { .. } => reply(json!({"deleted":deleted})),
                    _ => panic!("must retain desktop row"),
                },
                || panic!("must not close"),
                || panic!("must not wait"),
            );
            assert!(result.is_err());
        }
    }

    #[test]
    fn connection_release_timeout_retains_history() {
        let waits = RefCell::new(0);
        let mut closes = 0;
        let result = delete_deerflow_history(
            "native",
            &[],
            true,
            |_, command| {
                assert!(
                    matches!(command, Command::DeerFlow { ref input, .. } if input["operation"] == "session.list")
                );
                reply(json!({"sessions":[{"session_id":"native","phase":"idle"}]}))
            },
            || closes += 1,
            || *waits.borrow_mut() += 1,
        );
        assert!(result.unwrap_err().to_string().contains("尚未释放"));
        assert_eq!(closes, 1);
        assert_eq!(*waits.borrow(), 40);
    }

    #[test]
    fn desktop_cleanup_failure_is_reported_for_retry() {
        let result = delete_deerflow_history(
            "native",
            &[Uuid::new_v4()],
            false,
            |_, command| match command {
                Command::DeerFlow { input, .. } if input["operation"] == "session.list" => {
                    reply(json!({"sessions":[]}))
                }
                Command::DeerFlow { .. } => {
                    reply(json!({"deleted":["native"],"already_deleted":true}))
                }
                Command::RemoveSession => anyhow::bail!("disk full"),
                _ => panic!("unexpected command"),
            },
            || panic!("must not close"),
            || panic!("must not wait"),
        );
        let error = result.unwrap_err().to_string();
        assert!(error.contains("桌面列表清理失败"));
        assert!(error.contains("disk full"));
    }
}

fn normalize_portable_document(document: &mut Value) -> bool {
    let previous = document.clone();
    document["sandbox"]["use"] = json!(LOCAL_SANDBOX);
    if let Some(options) = document["sandbox"]["advanced"].as_object_mut() {
        options.retain(|key, _| LOCAL_SANDBOX_OPTIONS.contains(&key.as_str()));
    }
    document["memory"]["manager_class"] = json!("deermem");
    previous != *document
}

impl Waku {
    fn deerflow_delete_session(&mut self, session_id: String, cx: &mut Context<Self>) {
        if self.deerflow_settings.busy() || self.deerflow_settings.applying() {
            return;
        }
        let sessions: Vec<_> = self.state.sessions.iter().filter(|session| {
            matches!(&session.provider_cursor, Some(ProviderResumeCursor::DeerFlow { session_id: native }) if native == &session_id)
        }).collect();
        if sessions.iter().any(|session| {
            session.is_busy()
                || !session.queued_messages.is_empty()
                || self.submission_preparations.contains(&session.id)
                || self.goal_runtime_starts.contains(&session.id)
                || self.pending_goal_operations.contains_key(&session.id)
                || self.response_fork_preparations.contains_key(&session.id)
        }) {
            self.deerflow_settings.error =
                Some("会话正在执行或有排队任务，请先停止任务后再删除。".into());
            cx.notify();
            return;
        }
        let local_ids: Vec<_> = sessions.iter().map(|session| session.id).collect();
        let drivers: Vec<_> = local_ids
            .iter()
            .filter_map(|id| self.runtimes.get(id).map(|runtime| runtime.driver.clone()))
            .collect();
        self.deerflow_settings.pending = Some("delete-session".into());
        self.deerflow_settings.confirm_manage = None;
        self.deerflow_settings.error = None;
        self.deerflow_settings.notice = Some("正在释放会话连接并删除历史…".into());
        self.deerflow_freeze_inputs(cx);
        let daemon = self.daemon.client();
        cx.spawn(async move |this, cx| {
            let ids = local_ids.clone();
            let result = cx
                .background_executor()
                .spawn(async move {
                    delete_deerflow_history(
                        &session_id,
                        &ids,
                        !drivers.is_empty(),
                        |id, command| daemon.request(id, Uuid::nil(), command),
                        || {
                            for driver in &drivers {
                                driver.close();
                            }
                        },
                        || std::thread::sleep(Duration::from_millis(200)),
                    )
                })
                .await;
            let _ = this.update(cx, |this, cx| {
                this.deerflow_settings.pending = None;
                match result {
                    Ok(()) => {
                        // The daemon acknowledged persistent removal before we
                        // remove the rows, so a failed RPC never looks successful.
                        for id in local_ids {
                            this.forget_session(id, cx);
                        }
                        this.deerflow_settings.selected_session = None;
                        this.deerflow_settings.memory_data = Value::Null;
                        this.deerflow_request("manage", json!({"operation":"session.list"}), cx);
                        this.deerflow_settings.notice =
                            Some("会话及历史已删除，产物文件保留。".into());
                    }
                    Err(error) => {
                        this.deerflow_settings.notice = None;
                        this.deerflow_service_error(&format!("{error:#}"), cx);
                    }
                }
                this.deerflow_freeze_inputs(cx);
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }

    fn deerflow_managed(&mut self, operation: &str, data: Value, cx: &mut Context<Self>) {
        self.deerflow_settings.confirm_manage = None;
        match operation {
            "session.list" => {
                self.deerflow_settings.sessions =
                    data["sessions"].as_array().cloned().unwrap_or_default();
                self.deerflow_settings.session_page = 0;
            }
            "memory.get" | "memory.delete" => {
                self.deerflow_settings.memory_data = data;
                self.deerflow_settings.memory_page = 0;
                if operation == "memory.delete" {
                    self.deerflow_settings.notice = Some("所选记忆事实已删除。".into());
                }
            }
            "proposal.list" | "proposal.history" => {
                self.deerflow_settings.proposals =
                    data["proposals"].as_array().cloned().unwrap_or_default();
                self.deerflow_settings.revisions =
                    data["revisions"].as_array().cloned().unwrap_or_default();
            }
            "proposal.get" => {
                let diff = data["diff"].as_str().unwrap_or("暂无差异").to_owned();
                self.deerflow_settings
                    .proposal_diff
                    .update(cx, |input, cx| input.set_content(diff, cx));
                self.deerflow_settings.proposal = Some(data);
            }
            "proposal.approve" | "proposal.reject" | "proposal.rollback" => {
                self.deerflow_settings.proposal = None;
                self.deerflow_request("manage", json!({"operation":"proposal.history"}), cx);
                self.deerflow_settings.notice = Some(
                    match operation {
                        "proposal.approve" => "提案已批准并发布。",
                        "proposal.reject" => "提案已拒绝。",
                        _ => "技能版本已回滚，并创建了新修订记录。",
                    }
                    .into(),
                );
            }
            _ => self.deerflow_settings.notice = Some("操作已完成。".into()),
        }
    }

    fn deerflow_select_memory(&mut self, session: String, cx: &mut Context<Self>) {
        if self.deerflow_settings.busy() {
            return;
        }
        self.deerflow_settings.selected_session = Some(session.clone());
        self.deerflow_settings.memory_data = Value::Null;
        self.deerflow_settings.confirm_manage = None;
        self.deerflow_request(
            "manage",
            json!({"operation":"memory.get","session_id":session}),
            cx,
        );
    }

    fn render_deerflow_data(&self, theme: Theme, cx: &mut Context<Self>) -> AnyElement {
        let state = &self.deerflow_settings;
        let unavailable =
            state.busy() || state.applying() || state.status["running"].as_bool() != Some(true);
        let mut group = df_group(theme)
            .child(df_label(
                "会话与记忆数据",
                "运行服务后，可查看会话对应的记忆作用域并管理历史数据。",
                theme,
            ))
            .child(
                div()
                    .flex()
                    .flex_wrap()
                    .gap(px(6.0))
                    .child(df_button(
                        "df-sessions-load",
                        "加载会话列表",
                        unavailable,
                        false,
                        theme,
                        cx,
                        |this, _, cx| {
                            this.deerflow_request("manage", json!({"operation":"session.list"}), cx)
                        },
                    ))
                    .child(df_button(
                        "df-memory-legacy",
                        "查看旧版共享记忆",
                        unavailable,
                        false,
                        theme,
                        cx,
                        |this, _, cx| this.deerflow_select_memory("__legacy__".into(), cx),
                    )),
            );
        let page_size = 12;
        let count = state.sessions.len();
        let offset = state.session_page * page_size;
        if count > 0 {
            group = group.child(df_label(
                "历史会话",
                format!("共 {count} 个 · 第 {} 页", state.session_page + 1),
                theme,
            ));
            for session in state.sessions.iter().skip(offset).take(page_size) {
                let id = session["session_id"].as_str().unwrap_or("").to_owned();
                let title = session["title"].as_str().unwrap_or("未命名会话");
                let cwd = session["cwd"].as_str().unwrap_or("");
                let status = if session["cleanup_eligible"].as_bool() == Some(true) {
                    "符合过期清理条件"
                } else if session["phase"] == "idle" {
                    "空闲，可删除；本桌面的连接会自动释放"
                } else if !session["phase"].is_null() {
                    "正在执行或切换连接，请结束任务后再删除"
                } else {
                    "保留"
                };
                let memory_id = id.clone();
                let delete_id = id.clone();
                group = group.child(div().flex().flex_col().gap(px(8.0))
                    .child(df_label(title.to_owned(), format!("{cwd}\n{status}"), theme))
                    .child(div().flex().flex_wrap().gap(px(6.0))
                        .child(df_button(SharedString::from(format!("df-memory-{id}")), "查看记忆", unavailable,
                            state.selected_session.as_deref() == Some(id.as_str()), theme, cx,
                            move |this, _, cx| this.deerflow_select_memory(memory_id.clone(), cx)))
                        .child(df_button(SharedString::from(format!("df-session-delete-{id}")), "删除会话…", unavailable || !deletable_phase(&session["phase"]), false, theme, cx,
                            move |this, _, cx| {
                                this.deerflow_settings.confirm_manage = Some((
                                    format!("永久删除会话 {delete_id} 及其历史。本桌面的空闲连接会自动释放，产物文件保留。"),
                                    json!({"operation":"session.delete","session_id":delete_id}),
                                ));
                                this.settings_scroll.set_offset(gpui::point(px(0.0), px(0.0)));
                                cx.notify();
                            }))));
            }
            group = group.child(
                div()
                    .flex()
                    .gap(px(6.0))
                    .child(df_button(
                        "df-session-prev",
                        "上一页",
                        state.session_page == 0 || state.busy(),
                        false,
                        theme,
                        cx,
                        |this, _, cx| {
                            this.deerflow_settings.session_page =
                                this.deerflow_settings.session_page.saturating_sub(1);
                            cx.notify();
                        },
                    ))
                    .child(df_button(
                        "df-session-next",
                        "下一页",
                        offset + page_size >= count || state.busy(),
                        false,
                        theme,
                        cx,
                        |this, _, cx| {
                            this.deerflow_settings.session_page += 1;
                            cx.notify();
                        },
                    )),
            );
        }
        if !state.memory_data.is_null() {
            group = group.child(df_label(
                "所选作用域的记忆",
                format!(
                    "{} · {}",
                    state.memory_data["workspace"].as_str().unwrap_or(""),
                    state.memory_data["scope"].as_str().unwrap_or("")
                ),
                theme,
            ));
            if let Some(facts) = state.memory_data["memory"]["facts"].as_array() {
                let offset = state.memory_page * page_size;
                if facts.is_empty() {
                    group = group.child(df_label("暂无记忆事实", "此作用域尚未保存事实。", theme));
                }
                for fact in facts.iter().skip(offset).take(page_size) {
                    let id = fact["id"].as_str().unwrap_or("").to_owned();
                    let session_id = state.selected_session.clone().unwrap_or_default();
                    group = group.child(div().flex().items_start().gap(px(16.0))
                        .child(df_label(fact["content"].as_str().unwrap_or("").to_owned(), id.clone(), theme))
                        .child(df_button(SharedString::from(format!("df-fact-delete-{id}")), "删除…", unavailable || state.status["active_operations"].as_u64().unwrap_or(0) > 0, false, theme, cx,
                            move |this, _, cx| {
                                this.deerflow_settings.confirm_manage = Some((
                                    format!("删除当前作用域中的记忆事实 {id}。其他工作区的记忆不受影响。"),
                                    json!({"operation":"memory.delete","session_id":session_id,"fact_id":id}),
                                )); cx.notify();
                            })));
                }
                if facts.len() > page_size {
                    group = group.child(
                        div()
                            .flex()
                            .gap(px(6.0))
                            .child(df_button(
                                "df-memory-prev",
                                "上一页记忆",
                                state.memory_page == 0 || state.busy(),
                                false,
                                theme,
                                cx,
                                |this, _, cx| {
                                    this.deerflow_settings.memory_page =
                                        this.deerflow_settings.memory_page.saturating_sub(1);
                                    cx.notify();
                                },
                            ))
                            .child(df_button(
                                "df-memory-next",
                                "下一页记忆",
                                offset + page_size >= facts.len() || state.busy(),
                                false,
                                theme,
                                cx,
                                |this, _, cx| {
                                    this.deerflow_settings.memory_page += 1;
                                    cx.notify();
                                },
                            )),
                    );
                }
            }
        }
        group.into_any_element()
    }

    fn render_deerflow_proposals(&self, theme: Theme, cx: &mut Context<Self>) -> AnyElement {
        let state = &self.deerflow_settings;
        let unavailable =
            state.busy() || state.applying() || state.status["running"].as_bool() != Some(true);
        let mut group = df_group(theme)
            .child(df_label(
                "技能自进化审查",
                "查看候选技能的变更，批准发布、拒绝或恢复历史版本。",
                theme,
            ))
            .child(
                div()
                    .flex()
                    .flex_wrap()
                    .gap(px(6.0))
                    .child(df_button(
                        "df-proposal-pending",
                        "待审查提案",
                        unavailable,
                        false,
                        theme,
                        cx,
                        |this, _, cx| {
                            this.deerflow_request(
                                "manage",
                                json!({"operation":"proposal.list","status":"pending_review"}),
                                cx,
                            )
                        },
                    ))
                    .child(df_button(
                        "df-proposal-history",
                        "自进化历史",
                        unavailable,
                        false,
                        theme,
                        cx,
                        |this, _, cx| {
                            this.deerflow_request(
                                "manage",
                                json!({"operation":"proposal.history"}),
                                cx,
                            )
                        },
                    )),
            );
        for proposal in &state.proposals {
            let id = proposal["id"].as_str().unwrap_or("").to_owned();
            let title = format!(
                "{} · {} · {}",
                proposal["skill_name"].as_str().unwrap_or("技能"),
                proposal["action"].as_str().unwrap_or(""),
                proposal["status"].as_str().unwrap_or("")
            );
            group = group.child(df_button(
                SharedString::from(format!("df-proposal-{id}")),
                title,
                unavailable,
                state
                    .proposal
                    .as_ref()
                    .is_some_and(|p| p["id"].as_str() == Some(id.as_str())),
                theme,
                cx,
                move |this, _, cx| {
                    this.deerflow_settings.proposal = None;
                    this.deerflow_settings.confirm_manage = None;
                    this.deerflow_request(
                        "manage",
                        json!({"operation":"proposal.get","proposal_id":id}),
                        cx,
                    );
                },
            ));
        }
        if let Some(proposal) = &state.proposal {
            let id = proposal["id"].as_str().unwrap_or("").to_owned();
            let approve_id = id.clone();
            let base = proposal["base_sha256"].clone();
            group = group
                .child(df_label(
                    proposal["skill_name"].as_str().unwrap_or("提案").to_owned(),
                    proposal["reason"].as_str().unwrap_or("").to_owned(),
                    theme,
                ))
                .child(
                    div()
                        .id(SharedString::from(format!("df-proposal-diff-{id}")))
                        .h(px(300.0))
                        .w_full()
                        .flex_none()
                        // Keep long diff text and its mouse hit area inside the
                        // viewport, above the separate review action row.
                        .overflow_hidden()
                        .overflow_y_scroll()
                        .p(px(10.0))
                        .rounded(px(6.0))
                        .border_1()
                        .border_color(theme.border_strong)
                        .bg(theme.inset)
                        .text_size(sp(12.5))
                        .line_height(sp(18.0))
                        .child(state.proposal_diff.clone()),
                );
            let disabled =
                unavailable || state.dirty || proposal["status"].as_str() != Some("pending_review");
            group = group.child(div().flex().flex_none().flex_wrap().gap(px(6.0))
                .child(df_button("df-proposal-approve", "确认发布", disabled, false, theme, cx,
                    move |this, _, cx| {
                        this.deerflow_settings.confirm_manage = None;
                        this.deerflow_request("manage", json!({"operation":"proposal.approve","proposal_id":approve_id,"expected_base_sha256":base}), cx);
                        this.settings_scroll.set_offset(gpui::point(px(0.0), px(0.0)));
                        cx.notify();
                    }))
                .child(df_button("df-proposal-reject", "拒绝提案…", disabled, false, theme, cx,
                    move |this, _, cx| {
                        this.deerflow_settings.confirm_manage = Some((format!("拒绝提案 {id}。"), json!({"operation":"proposal.reject","proposal_id":id})));
                        this.settings_scroll.set_offset(gpui::point(px(0.0), px(0.0)));
                        cx.notify();
                    })));
            if state.dirty {
                group = group.child(df_label(
                    "草稿尚未保存",
                    "先保存或放弃草稿，再审查发布技能。",
                    theme,
                ));
            }
        }
        for revision in &state.revisions {
            let name = revision["name"].as_str().unwrap_or("").to_owned();
            let version = revision["version"].as_u64().unwrap_or(0);
            group = group.child(div().flex().items_center().gap(px(16.0))
                .child(df_label(format!("{name} · 版本 {version}"), "历史技能修订", theme))
                .child(df_button(SharedString::from(format!("df-revision-{name}-{version}")), "恢复此版本…", unavailable || state.dirty || version == 0, false, theme, cx,
                    move |this, _, cx| {
                        this.deerflow_settings.confirm_manage = Some((
                            format!("将技能 {name} 恢复至版本 {version}。该操作会创建新的修订记录。"),
                            json!({"operation":"proposal.rollback","name":name,"version":version}),
                        )); cx.notify();
                    })));
        }
        group.into_any_element()
    }
}

fn portable_issue(document: &Value) -> Option<String> {
    if document["sandbox"]["use"].as_str() != Some(LOCAL_SANDBOX) {
        return Some(
            "此配置使用其他沙箱提供者。便携桌面仅支持 LocalSandboxProvider，请确认转换后再保存。"
                .into(),
        );
    }
    if document["memory"]["manager_class"].as_str() != Some("deermem") {
        return Some(
            "此配置使用其他记忆管理器。便携桌面仅支持本地 DeerMem，请确认转换后再保存。".into(),
        );
    }
    if document["sandbox"]["advanced"]
        .as_object()
        .is_some_and(|options| {
            options
                .keys()
                .any(|key| !LOCAL_SANDBOX_OPTIONS.contains(&key.as_str()))
        })
    {
        return Some("已有沙箱配置包含非本地参数。确认转换前会完整保留原配置。".into());
    }
    None
}

fn redact_deerflow_message(message: &str, document: Option<&Value>) -> String {
    fn collect(value: &Value, sensitive: bool, secrets: &mut Vec<String>) {
        match value {
            Value::Object(object) => {
                for (key, value) in object {
                    let key = key.to_ascii_lowercase();
                    let sensitive = sensitive
                        || [
                            "api_key",
                            "api-key",
                            "token",
                            "secret",
                            "password",
                            "authorization",
                            "credential",
                        ]
                        .iter()
                        .any(|part| key.contains(part));
                    collect(value, sensitive, secrets);
                }
            }
            Value::Array(items) => {
                for value in items {
                    collect(value, sensitive, secrets);
                }
            }
            Value::String(secret)
                if sensitive && !secret.is_empty() && secret != "__DEERFLOW_REDACTED__" =>
            {
                secrets.push(secret.clone())
            }
            _ => {}
        }
    }
    let mut secrets = Vec::new();
    if let Some(document) = document {
        collect(document, false, &mut secrets);
        for server in document["client_mcp_servers"]
            .as_array()
            .into_iter()
            .flatten()
        {
            for env in server["env"].as_array().into_iter().flatten() {
                if let Some(value) = env["value"].as_str()
                    && !value.is_empty()
                    && value != "__DEERFLOW_REDACTED__"
                {
                    secrets.push(value.to_owned());
                }
            }
        }
    }
    secrets.sort_by_key(|secret| std::cmp::Reverse(secret.len()));
    secrets.dedup();
    let mut redacted = message.to_owned();
    for secret in secrets {
        redacted = redacted.replace(&secret, "[REDACTED]");
    }
    redacted
}

impl Waku {
    pub(super) fn render_deerflow_exit_dialog(&self, cx: &mut Context<Self>) -> Option<AnyElement> {
        let state = &self.deerflow_settings;
        if !state.exit_requested {
            return None;
        }
        let theme = Theme::current(cx);
        let busy = state.busy();
        let mut card = div()
            .id("deerflow-exit-dialog-card")
            .track_focus(&state.exit_focus)
            .tab_group()
            .tab_stop(false)
            .w_full()
            .max_w(px(500.0))
            .p(px(24.0))
            .rounded(px(18.0))
            .bg(theme.composer)
            .shadow_xl()
            .flex()
            .flex_col()
            .gap(px(16.0))
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .on_key_down(cx.listener(|this, event: &KeyDownEvent, window, cx| {
                if event.keystroke.key == "escape" {
                    this.deerflow_settings.exit_requested = false;
                    this.deerflow_settings.exit_after_save = false;
                    window.focus(&this.settings_focus, cx);
                    cx.stop_propagation();
                    cx.notify();
                }
            }))
            .child(df_label(
                "退出前处理 DeerFlow 配置",
                if busy {
                    "配置操作仍在后台进行，可以等待完成，也可以明确选择不等待并退出。"
                } else {
                    "当前配置草稿尚未保存。保存成功后会退出；保存失败时保留窗口与草稿。"
                },
                theme,
            ));
        if let Some(error) = &state.error {
            card = card.child(
                div()
                    .text_size(sp(12.5))
                    .line_height(sp(18.0))
                    .text_color(theme.danger)
                    .child(error.clone()),
            );
        }
        card = card.child(
            div()
                .flex()
                .flex_wrap()
                .gap(px(8.0))
                .child(df_button(
                    "df-exit-save",
                    "保存并退出",
                    busy || !state.dirty,
                    false,
                    theme,
                    cx,
                    |this, _, cx| {
                        if !this.deerflow_settings.errors.is_empty() {
                            this.deerflow_settings.error =
                                Some("请先继续编辑并修正字段错误。".into());
                        } else if let Some(issue) = this
                            .deerflow_settings
                            .draft
                            .as_ref()
                            .and_then(portable_issue)
                        {
                            this.deerflow_settings.error = Some(issue);
                        } else {
                            this.deerflow_settings.exit_after_save = true;
                            this.deerflow_save_or_validate("save", cx);
                        }
                        cx.notify();
                    },
                ))
                .child(df_button(
                    "df-exit-discard",
                    if busy {
                        "不等待并退出"
                    } else {
                        "放弃并退出"
                    },
                    false,
                    false,
                    theme,
                    cx,
                    |this, _, cx| {
                        this.deerflow_settings.exit_requested = false;
                        this.deerflow_settings.exit_after_save = false;
                        cx.quit();
                    },
                ))
                .child(df_button(
                    "df-exit-cancel",
                    "继续编辑",
                    false,
                    false,
                    theme,
                    cx,
                    |this, window, cx| {
                        this.deerflow_settings.exit_requested = false;
                        this.deerflow_settings.exit_after_save = false;
                        window.focus(&this.settings_focus, cx);
                        cx.notify();
                    },
                )),
        );
        let scrim = gpui::hsla(0.0, 0.0, 0.0, if theme.is_dark { 0.34 } else { 0.16 });
        let layer = div()
            .id("deerflow-exit-dialog-layer")
            .absolute()
            .inset_0()
            .occlude()
            .bg(scrim)
            .p(px(24.0))
            .flex()
            .items_center()
            .justify_center()
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .child(card);
        Some(gpui::deferred(layer).with_priority(5).into_any_element())
    }
}

fn model_refresh_ready(status: &Value) -> bool {
    status["running"].as_bool() == Some(true)
        && status["applying"].as_bool() != Some(true)
        && status["apply_error"].as_str().is_none()
        && status["warmup"] != "failed"
}

#[cfg(test)]
mod integrity_tests {
    use super::*;
    #[test]
    fn unsupported_configuration_is_detected_without_mutation() {
        let mut doc = json!({"sandbox":{"use":"docker:Provider","advanced":{"image":"example","mounts":[]}},"memory":{"manager_class":"remote"}});
        let original = doc.clone();
        assert!(portable_issue(&doc).is_some());
        assert_eq!(doc, original);
        assert!(normalize_portable_document(&mut doc));
        assert!(portable_issue(&doc).is_none());
        assert!(doc["sandbox"]["advanced"].get("image").is_none());
        assert_eq!(doc["sandbox"]["advanced"]["mounts"], json!([]));
    }
    #[test]
    fn service_errors_hide_unsaved_and_nested_secrets() {
        let doc = json!({"models":[{"api_key":"sk-test-private"}],"tools":[{"headers":{"authorization":"Bearer local-secret"}}],"client_mcp_servers":[{"name":"fixture","env":[{"name":"MCP_KEY","value":"mcp-private"}]}]});
        let text = redact_deerflow_message(
            "request sk-test-private failed: Bearer local-secret mcp-private",
            Some(&doc),
        );
        assert!(!text.contains("sk-test-private"));
        assert!(!text.contains("local-secret"));
        assert!(!text.contains("mcp-private"));
        assert!(text.contains("[REDACTED]"));
    }
    #[test]
    fn stopped_or_applying_service_never_refreshes_models() {
        assert!(!model_refresh_ready(
            &json!({"running":false,"applying":false})
        ));
        assert!(!model_refresh_ready(
            &json!({"running":true,"applying":true})
        ));
        assert!(!model_refresh_ready(
            &json!({"running":true,"applying":false,"apply_error":"failure"})
        ));
        assert!(!model_refresh_ready(
            &json!({"running":true,"applying":false,"warmup":"failed"})
        ));
        assert!(model_refresh_ready(
            &json!({"running":true,"applying":false,"apply_error":null})
        ));
    }
    #[test]
    fn service_phase_waits_for_agent_warmup() {
        let phase = |status: Value, request_pending, startup_pending| {
            deerflow_service_phase(&status, request_pending, startup_pending, false, None)
        };
        assert_eq!(
            phase(Value::Null, true, true),
            DeerFlowServicePhase::Starting
        );
        assert_eq!(
            phase(Value::Null, false, true),
            DeerFlowServicePhase::Starting
        );
        assert_eq!(
            phase(json!({"running":false,"applying":true}), false, true),
            DeerFlowServicePhase::Starting
        );
        assert_eq!(
            phase(json!({"running":true,"warmup":"warming"}), false, true),
            DeerFlowServicePhase::Warming
        );
        assert_eq!(
            phase(json!({"running":true,"warmup":"ready"}), false, true),
            DeerFlowServicePhase::Ready
        );
    }
    #[test]
    fn service_phase_reports_startup_failures() {
        assert_eq!(
            deerflow_service_phase(
                &json!({"running":true,"warmup":"failed"}),
                false,
                true,
                false,
                None
            ),
            DeerFlowServicePhase::Failed
        );
        assert_eq!(
            deerflow_service_phase(
                &json!({"running":false,"apply_error":"failed"}),
                false,
                true,
                false,
                None
            ),
            DeerFlowServicePhase::Failed
        );
        assert_eq!(
            deerflow_service_phase(
                &json!({"running":false,"status_error":"disconnected"}),
                false,
                true,
                false,
                None
            ),
            DeerFlowServicePhase::Failed
        );
        assert_eq!(
            deerflow_service_phase(
                &json!({"running":false,"status_error":"not running"}),
                false,
                false,
                false,
                None
            ),
            DeerFlowServicePhase::Stopped
        );
        assert_eq!(
            deerflow_service_phase(&Value::Null, false, false, true, None),
            DeerFlowServicePhase::Failed
        );
        assert_eq!(
            deerflow_service_phase(&json!({"running":true}), false, false, false, Some("apply")),
            DeerFlowServicePhase::Applying
        );
        assert_eq!(
            deerflow_service_phase(&json!({"running":true}), false, false, false, Some("stop")),
            DeerFlowServicePhase::Stopping
        );
    }
}

impl Waku {
    fn deerflow_commit_names(&mut self, cx: &mut Context<Self>) -> bool {
        if self.deerflow_settings.pending_model_names.is_empty()
            && self.deerflow_settings.pending_agent_names.is_empty()
        {
            return true;
        }
        let Some(document) = &self.deerflow_settings.draft else {
            return false;
        };
        match document_with_pending_names(
            document,
            &self.deerflow_settings.pending_model_names,
            &self.deerflow_settings.pending_agent_names,
        ) {
            Ok(document) => {
                self.deerflow_settings.draft = Some(document);
                self.deerflow_settings.pending_model_names.clear();
                self.deerflow_settings.pending_agent_names.clear();
                self.deerflow_rebind(cx);
                cx.notify();
                true
            }
            Err(error) => {
                self.deerflow_settings.error = Some(error);
                cx.notify();
                false
            }
        }
    }

    fn deerflow_flush_model_refresh(&mut self, cx: &mut Context<Self>) {
        if !self.deerflow_settings.model_refresh_queued
            || !model_refresh_ready(&self.deerflow_settings.status)
        {
            return;
        }
        // Let the previous result be consumed before requesting the new catalog;
        // otherwise refresh_provider_model_discovery intentionally skips it.
        if self
            .provider_model_discoveries_pending
            .contains(&ProviderKind::DeerFlow)
            || self.provider_detection_remaining > 0
        {
            if self.deerflow_settings.model_refresh_retry_pending {
                return;
            }
            self.deerflow_settings.model_refresh_retry_pending = true;
            cx.spawn(async move |this, cx| {
                cx.background_executor()
                    .timer(Duration::from_millis(250))
                    .await;
                let _ = this.update(cx, |this, cx| {
                    this.deerflow_settings.model_refresh_retry_pending = false;
                    this.deerflow_flush_model_refresh(cx);
                });
            })
            .detach();
            return;
        }
        self.deerflow_settings.model_refresh_queued = false;
        // The restarted Go daemon may expose a different model set. Do not
        // keep rendering choices from the daemon that was just replaced.
        if let Some(probe) = self
            .probes
            .iter_mut()
            .find(|probe| probe.provider == ProviderKind::DeerFlow)
        {
            probe.models.clear();
        }
        self.refresh_provider_model_discovery(ProviderKind::DeerFlow);
        self.refresh_composer_sources(cx);
    }
}

#[cfg(test)]
mod model_name_tests {
    use super::*;
    fn document() -> Value {
        json!({"models":[{"name":"gpt","original_name":"gpt","model":"remote-a"},{"name":"gpt4","original_name":"gpt4","model":"remote-b"}],"default_model":"gpt","runtime":{"model_name":"gpt4"},"memory":{"model_name":"gpt"},"agents":[{"model":"gpt4"}],"subagents":{"agents":{"general":{"model":"gpt4"}},"custom_agents":{"writer":{"model":"gpt4"},"reader":{"model":"inherit"}},"advanced":{"agents":{"general":{"model":"gpt4"}}}}})
    }
    #[test]
    fn temporary_name_collision_cannot_steal_another_models_references() {
        let draft = document();
        assert!(document_with_model_names(&draft, &HashMap::from([(1, "gpt".into())])).is_err());
        assert_eq!(draft["default_model"], "gpt");
        let saved =
            document_with_model_names(&draft, &HashMap::from([(1, "gpt5".into())])).unwrap();
        assert_eq!(saved["default_model"], "gpt");
        assert_eq!(saved["memory"]["model_name"], "gpt");
        assert_eq!(saved["runtime"]["model_name"], "gpt5");
        assert_eq!(saved["models"][1]["original_name"], "gpt4");
        assert_eq!(saved["models"][1]["model"], "remote-b");
    }
    #[test]
    fn simultaneous_name_mapping_does_not_cascade() {
        let saved = document_with_model_names(
            &document(),
            &HashMap::from([(0, "gpt4".into()), (1, "gpt5".into())]),
        )
        .unwrap();
        assert_eq!(saved["default_model"], "gpt4");
        assert_eq!(saved["runtime"]["model_name"], "gpt5");
        let swapped = document_with_model_names(
            &document(),
            &HashMap::from([(0, "gpt4".into()), (1, "gpt".into())]),
        )
        .unwrap();
        assert_eq!(swapped["default_model"], "gpt4");
        assert_eq!(swapped["runtime"]["model_name"], "gpt");
    }
    #[test]
    fn renaming_and_deleting_cover_builtin_custom_and_source_subagents() {
        let mut saved =
            document_with_model_names(&document(), &HashMap::from([(1, "gpt5".into())])).unwrap();
        assert_eq!(saved["subagents"]["agents"]["general"]["model"], "gpt5");
        assert_eq!(
            saved["subagents"]["custom_agents"]["writer"]["model"],
            "gpt5"
        );
        assert_eq!(
            saved["subagents"]["advanced"]["agents"]["general"]["model"],
            "gpt5"
        );
        rename_model_references(&mut saved, "gpt5", "gpt");
        assert_eq!(saved["subagents"]["agents"]["general"]["model"], "gpt");
        assert_eq!(
            saved["subagents"]["custom_agents"]["writer"]["model"],
            "gpt"
        );
        assert_eq!(
            saved["subagents"]["custom_agents"]["reader"]["model"],
            "inherit"
        );
    }
}

fn agent_name_index(path: &str) -> Option<usize> {
    let pieces: Vec<_> = path.split('/').collect();
    if pieces.len() == 4 && pieces[1] == "agents" && pieces[3] == "name" {
        pieces[2].parse().ok()
    } else {
        None
    }
}
fn document_with_agent_names(
    document: &Value,
    pending: &HashMap<usize, String>,
) -> Result<Value, String> {
    if pending.is_empty() {
        return Ok(document.clone());
    }
    let agents = document["agents"].as_array().ok_or("智能体列表格式错误")?;
    if pending.keys().any(|index| *index >= agents.len()) {
        return Err("智能体列表已变化，请重新加载。".into());
    }
    let mut unique = HashSet::new();
    let mut names = Vec::new();
    let mut remap = HashMap::new();
    for (index, agent) in agents.iter().enumerate() {
        let old = agent["name"].as_str().unwrap_or("");
        let new = pending
            .get(&index)
            .map(String::as_str)
            .unwrap_or(old)
            .trim();
        if new.is_empty()
            || !new
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
        {
            return Err("智能体名称仅支持英文字母、数字和连字符，且不能为空。".into());
        }
        if !unique.insert(new.to_owned()) {
            return Err(format!("智能体名称“{new}”重复，请使用唯一名称。"));
        }
        names.push(new.to_owned());
        if old != new {
            remap.insert(old.to_owned(), new.to_owned());
        }
    }
    let mut result = document.clone();
    for (index, name) in names.into_iter().enumerate() {
        result["agents"][index]["name"] = json!(name);
    }
    if let Some(current) = document
        .pointer("/runtime/agent_name")
        .and_then(Value::as_str)
    {
        if let Some(next) = remap.get(current) {
            set_value(&mut result, "/runtime/agent_name", json!(next));
        }
    }
    Ok(result)
}
fn document_with_pending_names(
    document: &Value,
    models: &HashMap<usize, String>,
    agents: &HashMap<usize, String>,
) -> Result<Value, String> {
    document_with_agent_names(&document_with_model_names(document, models)?, agents)
}

#[cfg(test)]
mod agent_name_tests {
    use super::*;
    #[test]
    fn temporary_agent_collision_does_not_steal_default_profile() {
        let original = json!({"runtime":{"agent_name":"writer"},"agents":[{"name":"writer","original_name":"writer"},{"name":"writer2","original_name":"writer2"}]});
        assert!(
            document_with_agent_names(&original, &HashMap::from([(1, "writer".into())])).is_err()
        );
        let renamed =
            document_with_agent_names(&original, &HashMap::from([(1, "writer3".into())])).unwrap();
        assert_eq!(renamed["runtime"]["agent_name"], "writer");
        assert_eq!(renamed["agents"][1]["name"], "writer3");
        assert_eq!(renamed["agents"][1]["original_name"], "writer2");
        let default_renamed =
            document_with_agent_names(&original, &HashMap::from([(0, "author".into())])).unwrap();
        assert_eq!(default_renamed["runtime"]["agent_name"], "author");
    }
}

fn validation_error_items(message: &str) -> Vec<(String, String)> {
    let Some(start) = message.find('[') else {
        return Vec::new();
    };
    let Some(end) = message.rfind(']') else {
        return Vec::new();
    };
    if end < start {
        return Vec::new();
    }
    let Ok(Value::Array(errors)) = serde_json::from_str::<Value>(&message[start..=end]) else {
        return Vec::new();
    };
    errors
        .iter()
        .filter_map(|error| {
            let loc = error["loc"].as_array()?;
            let parts: Vec<_> = loc
                .iter()
                .map(|part| {
                    part.as_str()
                        .map(str::to_owned)
                        .unwrap_or_else(|| part.to_string())
                })
                .collect();
            let path = format!("/{}", parts.join("/"));
            let message = error["msg"]
                .as_str()?
                .to_owned()
                .replace("Input should be less than or equal to ", "应不大于 ")
                .replace("Input should be greater than or equal to ", "应不小于 ")
                .replace("Input should be greater than ", "应大于 ")
                .replace("Field required", "必填字段")
                .replace("Value error, ", "");
            Some((path, message))
        })
        .collect()
}
impl Waku {
    fn deerflow_service_error(&mut self, message: &str, cx: &mut Context<Self>) {
        let safe = redact_deerflow_message(message, self.deerflow_settings.draft.as_ref());
        let items = validation_error_items(&safe);
        let Some((path, first_message)) = items.first() else {
            self.deerflow_settings.error = Some(safe);
            return;
        };
        self.deerflow_settings.error = Some(format!(
            "配置校验未通过：\n{}",
            items
                .iter()
                .map(|(path, message)| format!(
                    "{}：{message}",
                    path.trim_start_matches('/').replace('/', " · ")
                ))
                .collect::<Vec<_>>()
                .join("\n")
        ));
        // Focus the first editable failure. Additional failures stay in the
        // summary and are revalidated after the current field is corrected.
        let top = path.split('/').nth(1).unwrap_or("");
        let section = match top {
            "models" | "default_model" => Section::Models,
            "agents" | "subagents" => Section::Agents,
            "memory" => Section::Memory,
            "skills" | "skills_enabled" | "skill_evolution" => Section::Skills,
            "sandbox" | "tools" | "tool_groups" => Section::Tools,
            "runtime"
                if [
                    "/runtime/permission_mode",
                    "/runtime/tool_allowlist",
                    "/runtime/tool_denylist",
                    "/runtime/enable_bash",
                ]
                .iter()
                .any(|prefix| path.starts_with(*prefix)) =>
            {
                Section::Tools
            }
            "runtime" => Section::Runtime,
            _ => return,
        };
        self.deerflow_settings.section = section;
        self.deerflow_settings.advanced = true;
        let index = path
            .split('/')
            .nth(2)
            .and_then(|part| part.parse::<usize>().ok());
        if let Some(index) = index {
            if top == "models" {
                self.deerflow_settings.selected_model = index;
            }
            if top == "agents" {
                self.deerflow_settings.selected_agent = index;
            }
        }
        self.deerflow_rebind(cx);
        if let Some(field) = self
            .deerflow_settings
            .fields
            .iter()
            .filter(|field| path == &field.path || path.starts_with(&(field.path.clone() + "/")))
            .max_by_key(|field| field.path.len())
        {
            self.deerflow_settings
                .errors
                .insert(field.path.clone(), first_message.clone());
        }
    }
}
#[cfg(test)]
mod validation_error_tests {
    use super::*;
    #[test]
    fn validation_details_display_paths_without_schema_dump() {
        let items = validation_error_items(
            r#"service failed: [{"type":"less_than_equal","loc":["runtime","max_active_runs"],"msg":"Input should be less than or equal to 128","ctx":{"le":128}}]"#,
        );
        assert_eq!(
            items,
            vec![("/runtime/max_active_runs".into(), "应不大于 128".into())]
        );
        assert!(validation_error_items("connection closed").is_empty());
    }
}
