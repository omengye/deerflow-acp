//! DeerFlow configuration uses the same native controls and theme as the chat
//! workspace. The Go configuration service validates and persists the YAML.
use super::*;
use crate::ui::ActivationExt;
use serde_json::{Value, json};

mod auto_patch;
mod behavior;
mod behavior_inspector;
mod capability_probe;
mod diagnostics;
mod discovery;
mod effective;
mod evolution;
mod feedback;
mod generation;
mod legacy;
mod mcp_editor;
mod memory_management;
mod missing;
pub(super) mod presentation;
mod roles;
mod settings_contract;
mod skill_sources;
mod skills;
mod tools;
use skills::SkillBrowser;

const SINGLE_FIELDS: usize = 40;
const FIELD_COUNT: usize = 56;

// Local transcript IDs cannot identify native Go tasks. Only the provider
// cursor establishes the session used for missing-capability discovery.
fn missing_session_context(session: Option<&AgentSession>) -> Option<(Uuid, String, String)> {
    let session = session.filter(|session| session.provider == ProviderKind::DeerFlow)?;
    let Some(ProviderResumeCursor::DeerFlow { session_id }) = &session.provider_cursor else {
        return None;
    };
    if session_id.is_empty() || session_id.len() > 256 || session_id.contains('\0') {
        return None;
    }
    Some((
        session.id,
        session_id.clone(),
        session.display_title().to_owned(),
    ))
}

#[derive(Clone, Copy, Default, PartialEq, Eq)]
pub(super) enum Section {
    #[default]
    Common,
    Advanced,
    SkillCenter,
    Sources,
    MCP,
    Legacy,
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
    if status["apply_error"].as_str().is_some() || status["warmup"] == "failed" {
        return DeerFlowServicePhase::Failed;
    }
    if !startup_pending && !applying && status["running"].as_bool() == Some(false) {
        return DeerFlowServicePhase::Stopped;
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

fn deerflow_editing_locked(status: &Value, pending_operation: Option<&str>) -> bool {
    pending_operation == Some("apply") || (status["applying"] == true && status["starting"] != true)
}
#[derive(Clone, Copy)]
enum Kind {
    Text,
    Optional,
    Integer,
    Number,
    OptionalInteger,
    OptionalNumber,
    Names,
    Arguments,
    OptionalBool,
    ModelReference,
    ConfiguredModelReference,
    OptionalChoice(&'static [(&'static str, &'static str)]),
    Multiline,
    Json,
    Bool,
    TrueOnly,
    Choice(&'static [(&'static str, &'static str)]),
}
impl Kind {
    fn multiline(self) -> bool {
        matches!(
            self,
            Self::Multiline | Self::Json | Self::Names | Self::Arguments
        )
    }
    fn input(self) -> bool {
        !matches!(
            self,
            Self::Bool
                | Self::TrueOnly
                | Self::OptionalBool
                | Self::Choice(_)
                | Self::OptionalChoice(_)
                | Self::ModelReference
                | Self::ConfiguredModelReference
        )
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
    pub(super) generation_inputs: [Entity<TextInput>; 2],
    pub(super) feedback_input: Entity<TextInput>,
    pub(super) missing_input: Entity<TextInput>,
    skills_session_id: Option<String>,
    skills_local_session_id: Option<Uuid>,
    skills_session_title: String,
    draft: Option<Value>,
    saved_draft: Option<Value>,
    invalid_text: HashMap<String, String>,
    field_anchors: HashMap<String, gpui::ScrollAnchor>,
    search_target: Option<String>,
    focus_pending: Option<String>,
    pub(super) search_key: Option<String>,
    pub(super) search_catalogue: Vec<presentation::SearchTarget>,
    skill_center_tab: presentation::SkillCenterTab,
    show_more: bool,
    pending_evolution_mode: Option<presentation::EvolutionMode>,
    selected_mcp: usize,
    selected_mcp_env: usize,
    fields: Vec<Field>,
    input_text: Vec<String>,
    errors: HashMap<String, String>,
    section: Section,
    selected_model: usize,
    selected_tool: usize,
    selected_skill_source: usize,
    pending_model_names: HashMap<usize, String>,
    roles: roles::RoleEditor,
    legacy: legacy::LegacyBrowser,
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
    applied_values: Vec<effective::Row>,
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
    memory_scope: String,
    selected_session: Option<String>,
    memory_data: Value,
    diagnostic_data: Value,
    proposals: Vec<Value>,
    proposal: Option<Value>,
    revisions: Vec<Value>,
    proposal_diff: Entity<TextInput>,
    confirm_manage: Option<(String, Value)>,
    skill_browser: SkillBrowser,
    settings_contract: settings_contract::Capabilities,
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
        let feedback_input = cx.new(|cx| TextInput::new(window, cx).multi_line().read_only(true));
        let missing_input = cx.new(|cx| TextInput::new(window, cx).multi_line().read_only(true));
        let generation_inputs = [
            cx.new(|cx| TextInput::new(window, cx).read_only(true)),
            cx.new(|cx| TextInput::new(window, cx).multi_line().read_only(true)),
        ];
        Self {
            inputs,
            generation_inputs,
            feedback_input,
            missing_input,
            skills_session_id: None,
            skills_local_session_id: None,
            skills_session_title: String::new(),
            draft: None,
            saved_draft: None,
            invalid_text: HashMap::new(),
            field_anchors: HashMap::new(),
            search_target: None,
            focus_pending: None,
            search_key: None,
            search_catalogue: Vec::new(),
            skill_center_tab: presentation::SkillCenterTab::default(),
            show_more: false,
            pending_evolution_mode: None,
            selected_mcp: 0,
            selected_mcp_env: 0,
            fields: Vec::new(),
            input_text: vec![String::new(); FIELD_COUNT],
            errors: HashMap::new(),
            section: Section::Common,
            selected_model: 0,
            selected_tool: 0,
            selected_skill_source: 0,
            pending_model_names: HashMap::new(),
            roles: roles::RoleEditor::default(),
            legacy: legacy::LegacyBrowser::default(),
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
            applied_values: Vec::new(),
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
            memory_scope: "workspace".into(),
            selected_session: None,
            memory_data: Value::Null,
            diagnostic_data: Value::Null,
            proposals: Vec::new(),
            proposal: None,
            revisions: Vec::new(),
            proposal_diff,
            confirm_manage: None,
            skill_browser: SkillBrowser::default(),
            settings_contract: settings_contract::Capabilities::default(),
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
        self.draft.is_some() && !self.busy() && !self.editing_locked()
    }

    fn editing_locked(&self) -> bool {
        deerflow_editing_locked(&self.status, self.pending.as_deref())
    }

    fn can_edit_field(&self, path: &str) -> bool {
        self.can_edit()
            && self.settings_contract.allows(path)
            && (path != roles::NAME_PATH || self.settings_contract.allows("/subagents/agents"))
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
        Kind::Names | Kind::Arguments => match value {
            Value::Null => String::new(),
            Value::Array(items) if items.is_empty() => {
                if matches!(kind, Kind::Names) {
                    "[]".into()
                } else {
                    String::new()
                }
            }
            Value::Array(items) if items.iter().all(Value::is_string) => items
                .iter()
                .filter_map(Value::as_str)
                .collect::<Vec<_>>()
                .join("\n"),
            _ => value.to_string(),
        },
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
        Kind::Arguments => Ok(json!(if text.is_empty() {
            vec![]
        } else {
            text.lines().map(str::to_owned).collect::<Vec<_>>()
        })),
        Kind::OptionalInteger | Kind::OptionalNumber if text.trim().is_empty() => Ok(Value::Null),
        Kind::OptionalInteger => parse_field(text, Kind::Integer),
        Kind::OptionalNumber => parse_field(text, Kind::Number),
        Kind::Names => {
            let text = text.trim();
            Ok(if text.is_empty() {
                Value::Null
            } else if text == "[]" {
                json!([])
            } else {
                json!(
                    text.lines()
                        .map(str::trim)
                        .filter(|line| !line.is_empty())
                        .collect::<Vec<_>>()
                )
            })
        }
        _ => Err("该字段不接受文本输入".into()),
    }
}
fn migration_acknowledged(document: &Value) -> bool {
    let migration = &document["config_migration"];
    migration["required"] != true
        || (migration["source_revision"]
            .as_str()
            .is_some_and(|revision| !revision.is_empty())
            && document["config_migration_ack"] == migration["source_revision"]
            && document["config_revision"] == migration["source_revision"])
}

fn set_value(document: &mut Value, path: &str, value: Value) {
    if let Some((parent, key)) = path.rsplit_once('/') {
        if let Some(object) = document.pointer_mut(parent).and_then(Value::as_object_mut) {
            object.insert(key.to_owned(), value);
        }
    }
}
fn set_edit_value(document: &mut Value, path: &str, value: Value) -> Result<(), String> {
    if path.starts_with("/subagents/agents/") || path.starts_with("/subagents/custom_agents/") {
        roles::set_field(document, path, value)
    } else {
        set_value(document, path, value);
        Ok(())
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
        "/skill_evolution/moderation_model_name",
        "/skill_evolution/evaluation_model_name",
        "/skill_evolution/generation_model_name",
    ] {
        if let Some(value) = document.pointer_mut(path) {
            replace(value, names, false);
        }
    }
    for path in ["/subagents/agents", "/subagents/custom_agents"] {
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
    pub(super) fn deerflow_sync_missing_session(&mut self, cx: &mut Context<Self>) {
        let context = missing_session_context(self.selected_session());
        let local = context.as_ref().map(|(local, _, _)| *local);
        let native = context.as_ref().map(|(_, native, _)| native.clone());
        let title = context.map(|(_, _, title)| title).unwrap_or_default();
        let state = &mut self.deerflow_settings;
        let changed = state.skills_local_session_id != local || state.skills_session_id != native;
        state.skills_session_title = title;
        if changed {
            state.skill_browser.missing.set_session(native.as_deref());
            // Local views may share a native session; switching views still
            // revokes the form's previously observed action state.
            state.skill_browser.missing.invalidate();
            state.skills_local_session_id = local;
            state.skills_session_id = native;
            if state.confirm_manage.as_ref().is_some_and(|(_, request)| {
                request["operation"]
                    .as_str()
                    .is_some_and(|op| op.starts_with("skill.missing."))
            }) {
                state.confirm_manage = None;
            }
            self.deerflow_freeze_inputs(cx);
            cx.notify();
        }
    }

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
                        this.deerflow_settings.set_status(status);
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
        self.deerflow_select_section(Section::Common, cx);
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
        self.deerflow_sync_missing_session(cx);
        if self.deerflow_settings.draft.is_none() && !self.deerflow_settings.busy() {
            self.deerflow_request("snapshot", json!({}), cx);
        }
        self.deerflow_status(cx);
    }

    fn deerflow_freeze_inputs(&mut self, cx: &mut Context<Self>) {
        let state = &mut self.deerflow_settings;
        state.settings_contract = settings_contract::Capabilities::read(
            state.draft.as_ref().unwrap_or(&Value::Null),
            &state.status,
        );
        for (slot, input) in state.inputs.iter().enumerate() {
            let read_only = state
                .fields
                .iter()
                .find(|field| field.slot == Some(slot))
                .is_none_or(|field| !state.can_edit_field(&field.path));
            input.update(cx, |input, cx| {
                input.set_read_only(read_only);
                cx.notify();
            });
        }
        let read_only = !state.generation_editable();
        for input in &state.generation_inputs {
            input.update(cx, |input, cx| {
                input.set_read_only(read_only);
                cx.notify();
            });
        }
        let read_only = !state.feedback_editable();
        state.feedback_input.update(cx, |input, cx| {
            input.set_read_only(read_only);
            cx.notify();
        });
        let read_only = !state.missing_editable();
        state.missing_input.update(cx, |input, cx| {
            input.set_read_only(read_only);
            cx.notify();
        });
    }

    fn deerflow_request(&mut self, operation: &str, input: Value, cx: &mut Context<Self>) {
        if self.deerflow_settings.busy() {
            return;
        }
        if operation == "manage"
            && input["operation"]
                .as_str()
                .is_some_and(|op| op.starts_with("skill.missing."))
        {
            self.deerflow_sync_missing_session(cx);
            // Only an exact unresolved mutation may still refer to the previous
            // session; ordinary reads and newly composed writes use this session.
            if input["session_id"].as_str() != self.deerflow_settings.skills_session_id.as_deref()
                && !self
                    .deerflow_settings
                    .skill_browser
                    .is_original_retry(&input)
            {
                self.deerflow_settings.error =
                    Some("会话已变化，请刷新当前会话的任务后继续。草稿已保留。".into());
                cx.notify();
                return;
            }
        }
        if operation == "manage"
            && skills::is_skill_operation(&input)
            && !self
                .deerflow_settings
                .skill_browser
                .can_begin_at(&input, &self.deerflow_settings.status)
        {
            self.deerflow_settings.error =
                Some("先核对或结束上一次技能操作的重试，再提交新的修改；原请求只能在原服务数据目录重试。".into());
            cx.notify();
            return;
        }
        match operation {
            "legacy-agent-preview" => self.deerflow_settings.legacy.clear_preview(),
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
            self.deerflow_settings.skill_browser.invalidate();
            self.deerflow_settings.confirm_manage = None;
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
        let management_request = input.clone();
        let service_generation = self.deerflow_settings.status["applied_generation"].as_u64();
        let service_identity = skills::service_identity(&self.deerflow_settings.status);
        let structured_manage =
            operation == "manage" && skills::is_skill_operation(&management_request);
        if structured_manage {
            self.deerflow_settings
                .skill_browser
                .begin_at(&management_request, &self.deerflow_settings.status);
        }
        let operation = operation.to_owned();
        let daemon = self.daemon.client();
        cx.spawn(async move |this, cx| {
            let request_operation = if structured_manage {
                "manage-envelope".to_owned()
            } else {
                operation.clone()
            };
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
                if structured_manage
                    && management_request["operation"]
                        .as_str()
                        .is_some_and(|op| op.starts_with("skill.missing."))
                {
                    this.deerflow_sync_missing_session(cx);
                }
                if structured_manage
                    && (this.deerflow_settings.status["applied_generation"].as_u64()
                        != service_generation
                        || skills::service_identity(&this.deerflow_settings.status)
                            != service_identity
                        || this.deerflow_settings.status["running"].as_bool() == Some(false))
                {
                    this.deerflow_settings
                        .skill_browser
                        .transport_failed(&management_request);
                    this.deerflow_service_error(
                        "服务已变化，请刷新相关状态。已发送的修改可在原服务数据目录使用原请求核对。",
                        cx,
                    );
                    this.deerflow_freeze_inputs(cx);
                    cx.notify();
                    return;
                }
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
                        if structured_manage {
                            this.deerflow_settings
                                .skill_browser
                                .transport_failed(&management_request);
                        }
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
                        "legacy-agent-preview" | "legacy-history-import" | "legacy-history-list" | "legacy-history-get" => {
                            this.deerflow_legacy_response(&operation, &management_request, data, edits, cx);
                        }
                        "snapshot" | "save" | "save-and-apply" => {
                            let (document, application) = if operation == "save-and-apply" {
                                (data["document"].clone(), Some(data["status"].clone()))
                            } else {
                                (data, None)
                            };
                            if this.deerflow_settings.edit_generation == edits {
                                this.deerflow_settings.saved_draft = Some(document.clone());
                                this.deerflow_settings.invalid_text.clear();
                                this.deerflow_settings.draft = Some(document);
                                this.deerflow_settings.observe_saved_revision();
                                this.deerflow_settings.pending_model_names.clear();
                                if operation != "snapshot" {
                                    if let Some(name) = &this.deerflow_settings.roles.pending_name {
                                        this.deerflow_settings.roles.selected.name =
                                            name.trim().to_owned();
                                    }
                                }
                                this.deerflow_settings.roles.pending_name = None;
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
                                this.deerflow_settings.set_status(status);
                                this.deerflow_status(cx);
                                this.deerflow_poll_status(cx);
                            }
                        }
                        "manage" => {
                            if structured_manage {
                                this.deerflow_skill_response(&management_request, data, cx);
                            } else {
                                this.deerflow_managed(&management_request, data, cx);
                            }
                        }
                        "validate" => this.deerflow_settings.notice = Some("配置校验通过。".into()),
                        "test-model" => {
                            if data["capabilities"].is_object() {
                                this.deerflow_settings.notice = Some(capability_probe::capability_notice(&data));
                            } else if data["ok"].as_bool() == Some(true) {
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
                            this.deerflow_settings.set_status(data);
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
                        if skills::service_identity(&status)
                            != skills::service_identity(&this.deerflow_settings.status)
                            || status["applied_generation"]
                                != this.deerflow_settings.status["applied_generation"]
                            || status["running"].as_bool() == Some(false)
                        {
                            this.deerflow_settings.skill_browser.invalidate();
                            this.deerflow_settings.confirm_manage = None;
                        }
                        this.deerflow_settings.set_status(status);
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
                            if this.deerflow_settings.observed_applied_generation
                                != Some(applied_generation)
                            {
                                this.deerflow_settings.diagnostic_data = Value::Null;
                            }
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
        if !self.deerflow_settings.can_edit_field(&field.path) {
            return;
        }
        self.deerflow_settings.input_text[slot] = text.clone();
        self.deerflow_settings.dirty = true;
        self.deerflow_settings.edit_generation += 1;
        self.deerflow_settings.notice = None;
        self.deerflow_settings.confirm_reload = false;
        if field.path == roles::NAME_PATH {
            self.deerflow_settings.roles.pending_name = Some(text);
            self.deerflow_settings.errors.remove(&field.path);
            if let Some(document) = &self.deerflow_settings.draft {
                if let Err(error) = self.deerflow_settings.roles.projected(document) {
                    self.deerflow_settings.errors.insert(field.path, error);
                }
            }
            cx.notify();
            return;
        }
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
        let parsed = parse_field(&text, field.kind);
        match parsed {
            Ok(value) => {
                self.deerflow_settings.invalid_text.remove(&field.path);
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
                    if field.path.starts_with("/tools/") && !text.is_empty() {
                        if let Some((root, key)) = field.path.rsplit_once('/') {
                            if matches!(key, "api_key" | "https_proxy") {
                                set_value(document, &format!("{root}/clear_{key}"), json!(false));
                            }
                        }
                    }
                    if let Err(error) = set_edit_value(document, &field.path, value) {
                        self.deerflow_settings.errors.insert(field.path, error);
                    }
                }
            }
            Err(error) => {
                self.deerflow_settings
                    .invalid_text
                    .insert(field.path.clone(), text);
                self.deerflow_settings.errors.insert(field.path, error);
            }
        }
        cx.notify();
    }

    fn deerflow_change(&mut self, path: &str, value: Value, cx: &mut Context<Self>) {
        if !self.deerflow_settings.can_edit_field(path) {
            return;
        }
        self.deerflow_settings.errors.remove(path);
        self.deerflow_settings.invalid_text.remove(path);
        if let Some(document) = self.deerflow_settings.draft.as_mut() {
            if let Err(error) = set_edit_value(document, path, value) {
                self.deerflow_settings.errors.insert(path.into(), error);
                cx.notify();
                return;
            }
        }
        self.deerflow_settings.dirty = true;
        self.deerflow_settings.edit_generation += 1;
        self.deerflow_settings.confirm_reload = false;
        self.deerflow_settings.notice = None;
        if self.deerflow_settings.section == Section::Common {
            self.deerflow_rebind(cx);
        }
        cx.notify();
    }

    fn deerflow_rebind(&mut self, cx: &mut Context<Self>) {
        let scroll = self.settings_scroll.clone();
        let state = &mut self.deerflow_settings;
        let Some(doc) = state.draft.as_ref() else {
            return;
        };
        state.roles.refresh(doc);
        state.selected_skill_source = state.selected_skill_source.min(
            doc["skill_sources"]["managed"]
                .as_array()
                .map_or(0, |items| items.len().saturating_sub(1)),
        );
        state.selected_model = state.selected_model.min(
            doc["models"]
                .as_array()
                .map_or(0, |a| a.len().saturating_sub(1)),
        );
        state.selected_mcp = state.selected_mcp.min(
            doc["client_mcp_servers"]
                .as_array()
                .map_or(0, |items| items.len().saturating_sub(1)),
        );
        state.selected_mcp_env = state.selected_mcp_env.min(
            doc["client_mcp_servers"][state.selected_mcp]["env"]
                .as_array()
                .map_or(0, |items| items.len().saturating_sub(1)),
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
            let text = if field.path == roles::NAME_PATH {
                state
                    .roles
                    .pending_name
                    .clone()
                    .unwrap_or_else(|| state.roles.selected.name.clone())
            } else {
                model_name_index(&field.path)
                    .and_then(|index| state.pending_model_names.get(&index))
                    .cloned()
                    .or_else(|| state.invalid_text.get(&field.path).cloned())
                    .unwrap_or_else(|| {
                        field_text(doc.pointer(&field.path).unwrap_or(&Value::Null), field.kind)
                    })
            };
            state.input_text[slot] = text.clone();
            state.inputs[slot].update(cx, |input, cx| {
                input.set_masked(
                    field.path.ends_with("/api_key")
                        || field.path.starts_with("/client_mcp_servers/")
                            && field.path.ends_with("/value"),
                    cx,
                );
                input.set_placeholder(field.hint.clone(), cx);
                input.set_content(text, cx);
            });
        }
        state
            .field_anchors
            .retain(|path, _| fields.iter().any(|field| &field.path == path));
        for field in &fields {
            state
                .field_anchors
                .entry(field.path.clone())
                .or_insert_with(|| gpui::ScrollAnchor::for_handle(scroll.clone()));
        }
        if state.section == Section::Common {
            state.field_anchors.insert(
                "/skill_evolution/enabled".into(),
                gpui::ScrollAnchor::for_handle(scroll),
            );
        }
        state.fields = fields;
        state.search_catalogue = presentation::search_catalogue(state);
        self.deerflow_freeze_inputs(cx);
    }

    pub(super) fn open_deerflow_tools_settings(&mut self, cx: &mut Context<Self>) {
        self.open_settings_page(SettingsPage::DeerFlowTools, cx);
    }

    pub(super) fn open_deerflow_models_settings(&mut self, cx: &mut Context<Self>) {
        self.open_settings_page(SettingsPage::DeerFlowModels, cx);
    }

    fn deerflow_select_section(&mut self, section: Section, cx: &mut Context<Self>) {
        self.deerflow_settings.section = section;
        self.deerflow_settings.advanced = false;
        self.deerflow_settings.search_target = None;
        self.deerflow_settings.search_key = None;
        self.deerflow_rebind(cx);
        cx.notify();
    }

    fn deerflow_save_or_validate(&mut self, operation: &str, cx: &mut Context<Self>) {
        if operation != "validate"
            && self
                .deerflow_settings
                .draft
                .as_ref()
                .is_some_and(|doc| !migration_acknowledged(doc))
        {
            self.deerflow_settings.error =
                Some("先查看并确认上方的子代理配置迁移说明；原文件尚未修改。".into());
            cx.notify();
            return;
        }
        if self.deerflow_settings.roles.pending_name.is_some()
            && !self.deerflow_settings.can_edit_field(roles::NAME_PATH)
        {
            self.deerflow_settings.error =
                Some("角色名称或委派字段暂不可编辑；草稿已保留。".into());
            cx.notify();
            return;
        }
        if let Some(document) = &self.deerflow_settings.draft {
            if !self.deerflow_settings.settings_contract.can_save(document) {
                self.deerflow_settings.error =
                    Some("部分字段定义尚未匹配，请更新配套服务后重新加载；草稿已保留。".into());
                cx.notify();
                return;
            }
        }
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
            match document_with_model_names(document, &self.deerflow_settings.pending_model_names)
                .and_then(|doc| self.deerflow_settings.roles.projected(&doc))
            {
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

    fn deerflow_add_model(&mut self, cx: &mut Context<Self>) {
        if !self.deerflow_settings.can_edit_field("/models")
            || !self.deerflow_settings.errors.is_empty()
        {
            return;
        }
        let Some(doc) = self.deerflow_settings.draft.as_mut() else {
            return;
        };
        let Some(items) = doc["models"].as_array_mut() else {
            return;
        };
        let mut number = items.len() + 1;
        while items
            .iter()
            .any(|item| item["name"].as_str() == Some(&format!("model-{number}")))
        {
            number += 1;
        }
        items.push(json!({"original_name":"", "name":format!("model-{number}"), "display_name":"", "description":"", "provider":"openai", "model":"", "base_url":"", "api_key":"", "api_key_configured":false, "api_key_literal":false, "clear_api_key":false, "supports_vision":false, "context_window":0}));
        self.deerflow_settings.selected_model = items.len() - 1;
        self.deerflow_settings.dirty = true;
        self.deerflow_settings.edit_generation += 1;
        self.deerflow_rebind(cx);
        cx.notify();
    }

    fn deerflow_remove_model(&mut self, cx: &mut Context<Self>) {
        if !self.deerflow_settings.can_edit_field("/models")
            || !self.deerflow_settings.errors.is_empty()
            || !self.deerflow_commit_names(cx)
        {
            return;
        }
        let state = &mut self.deerflow_settings;
        let Some(doc) = state.draft.as_mut() else {
            return;
        };
        let Some(items) = doc["models"].as_array_mut() else {
            return;
        };
        if items.len() <= 1 || state.selected_model >= items.len() {
            return;
        }
        let removed = items.remove(state.selected_model);
        let next = items[0]["name"].as_str().unwrap_or("").to_owned();
        rename_model_references(doc, removed["name"].as_str().unwrap_or(""), &next);
        state.dirty = true;
        state.edit_generation += 1;
        self.deerflow_rebind(cx);
        cx.notify();
    }
}

fn fields_for(state: &DeerFlowSettings) -> Vec<Field> {
    fields_for_section(state, state.section)
}

fn fields_for_section(state: &DeerFlowSettings, section: Section) -> Vec<Field> {
    let mut fields = Vec::new();
    let mut add = |path: String, label: &str, hint: &str, kind| {
        fields.push(Field::new(path, label, hint, kind))
    };
    let doc = state.draft.as_ref().unwrap_or(&Value::Null);
    match section {
        Section::Common => fields.extend(presentation::common_fields(doc)),
        Section::MCP => fields.extend(mcp_editor::fields(state, doc)),
        Section::Sources => fields.extend(skill_sources::fields(doc, state.selected_skill_source)),
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
            ] {
                add(format!("{root}/{key}"), label, hint, Kind::Text);
            }
            add(
                format!("{root}/clear_api_key"),
                "清除已保存的密钥",
                "清除操作加入草稿，保存后生效；输入新密钥会取消清除",
                Kind::Bool,
            );
            add(
                format!("{root}/provider"),
                "模型提供商",
                "使用已接入的 Go 模型适配器",
                Kind::Choice(&[
                    ("openai", "OpenAI 兼容"),
                    ("ark", "火山方舟 Ark"),
                    ("claude", "Claude"),
                ]),
            );
            add(
                format!("{root}/supports_vision"),
                "支持图片输入",
                "请按服务商实际能力设置",
                Kind::Bool,
            );
            add(
                format!("{root}/context_window"),
                "上下文窗口",
                "Token 数；0 使用模型/宿主默认",
                Kind::Integer,
            );
        }
        Section::Agents => {
            add(
                "/subagents/enabled".into(),
                "允许子代理",
                "主机默认开关；显式 CLI 参数优先。主机实际关闭后，会话无法开启",
                Kind::Bool,
            );
            add(
                "/subagents/max_concurrent".into(),
                "每根任务子代理并发数",
                "1–64；前后台与各层共享",
                Kind::Integer,
            );
            for (key, label, hint, kind) in [
                (
                    "timeout_seconds",
                    "默认角色超时（秒）",
                    "留空使用 900；0.001–86400，仍受父任务剩余预算限制",
                    Kind::OptionalNumber,
                ),
                (
                    "max_turns",
                    "默认角色轮次",
                    "留空使用 50；1–10000，不能扩大父预算",
                    Kind::OptionalInteger,
                ),
                (
                    "max_depth",
                    "委派层数",
                    "留空使用 1；支持 1–3 层",
                    Kind::OptionalInteger,
                ),
                (
                    "background_workers",
                    "后台执行槽数",
                    "留空使用宿主默认；1–64",
                    Kind::OptionalInteger,
                ),
                (
                    "max_tasks_per_root",
                    "每根任务子任务数上限",
                    "留空继承宿主默认；1–10000",
                    Kind::OptionalInteger,
                ),
                (
                    "auto_continue",
                    "自动处理子任务结果",
                    "有当前授权连接且父会话空闲时继续；不能自动批准工具",
                    Kind::Bool,
                ),
                (
                    "max_auto_continuations",
                    "最多自动续跑次数",
                    "留空使用 10；0–1000，0 表示不创建自动新运行",
                    Kind::OptionalInteger,
                ),
            ] {
                add(format!("/subagents/{key}"), label, hint, kind);
            }
            fields.extend(state.roles.fields(doc));
        }
        Section::Memory => {
            add(
                "/runtime/memory_scope".into(),
                "记忆作用域",
                "自动提取写入此本地范围；读取时可附加全局记忆",
                Kind::Choice(&[
                    ("workspace", "项目"),
                    ("session", "单个会话"),
                    ("global", "全局与项目"),
                ]),
            );
            for (key, label, hint, kind) in [
                (
                    "enabled",
                    "启用长期记忆",
                    "使用 Go Harness 的本地记忆存储",
                    Kind::Bool,
                ),
                (
                    "include_global",
                    "读取全局记忆",
                    "跨项目、会话和模型共享；自动提取仍写入本地范围",
                    Kind::Bool,
                ),
                (
                    "injection_enabled",
                    "自动注入记忆",
                    "在会话上下文中使用已有记忆",
                    Kind::Bool,
                ),
                (
                    "retrieval_enabled",
                    "检索记忆",
                    "按相关度检索；关闭后仍可注入有界的已有记忆",
                    Kind::Bool,
                ),
                (
                    "mode",
                    "记忆接入方式",
                    "工具模式要求启用检索",
                    Kind::Choice(&[("middleware", "自动中间件"), ("tool", "工具调用")]),
                ),
            ] {
                add(format!("/memory/{key}"), label, hint, kind);
            }
        }
        Section::Skills => fields.extend(evolution::fields()),
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
                "每行一个名称；留空表示不限制，[] 表示不允许任何工具",
                Kind::Names,
            );
            add(
                "/runtime/tool_denylist".into(),
                "工具拒绝列表",
                "每行一个名称；留空或 [] 表示不额外拒绝",
                Kind::Names,
            );
            fields.extend(tools::fields(doc, state.selected_tool));
        }
        Section::Runtime => {
            for (key, label, hint, kind) in [
                (
                    "model_name",
                    "覆盖默认聊天模型",
                    "留空使用常用页的默认模型；显式值优先",
                    Kind::Optional,
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
                    "0.001–86400",
                    Kind::Number,
                ),
                (
                    "queue_timeout_seconds",
                    "排队超时（秒）",
                    "0.001–86400",
                    Kind::Number,
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
            ] {
                add(format!("/runtime/{key}"), label, hint, kind);
            }
            {
                add(
                    "/runtime/prompt_overlay".into(),
                    "附加系统指令",
                    "在默认指令上追加，最多 65536 个字符",
                    Kind::Multiline,
                );
                add(
                    "/runtime/session_cleanup_interval_seconds".into(),
                    "会话清理间隔（秒）",
                    "60–86400",
                    Kind::Number,
                );
            }
        }
        _ => {}
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
        let mut body = div().child(
            div()
                .mt(px(12.0))
                .flex()
                .items_center()
                .gap(px(8.0))
                .child(
                    div()
                        .flex_1()
                        .text_size(sp(12.5))
                        .text_color(theme.text_secondary)
                        .child(format!("{running_text} · {edit_text}")),
                )
                .when(state.section == Section::Overview, |bar| {
                    bar.child(df_button(
                        "df-settings-more",
                        if state.show_more {
                            "收起更多操作"
                        } else {
                            "更多操作"
                        },
                        busy,
                        state.show_more,
                        theme,
                        cx,
                        |this, _, cx| {
                            this.deerflow_settings.show_more = !this.deerflow_settings.show_more;
                            cx.notify();
                        },
                    ))
                }),
        );
        if state.section == Section::Overview && state.show_more {
            body = body.child(
                df_group(theme).child(
                    div()
                        .flex()
                        .flex_wrap()
                        .gap(px(6.0))
                        .child(df_button(
                            "df-validate",
                            "校验草稿",
                            !editable || invalid,
                            false,
                            theme,
                            cx,
                            |this, _, cx| this.deerflow_save_or_validate("validate", cx),
                        ))
                        .child(df_button(
                            "df-reload",
                            "重新读取配置",
                            busy || state.editing_locked(),
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
                        ))
                        .when(state.applying(), |actions| {
                            actions.child(df_button(
                                "df-cancel-apply",
                                "取消等待",
                                busy,
                                false,
                                theme,
                                cx,
                                |this, _, cx| this.deerflow_request("cancel-apply", json!({}), cx),
                            ))
                        }),
                ),
            );
        }
        if let Some(issue) = &state.settings_contract.issue {
            body = body.child(df_group(theme).child(df_label(
                "部分配置暂为只读",
                issue.clone(),
                theme,
            )));
        }
        if let Some(doc) = state
            .draft
            .as_ref()
            .filter(|doc| doc["config_migration"]["required"] == true)
        {
            let migration = &doc["config_migration"];
            let confirmed = migration_acknowledged(doc);
            let revision = migration["source_revision"]
                .as_str()
                .unwrap_or("")
                .to_owned();
            let summary = format!(
                "原 ACP 开关：{}，原角色开关：{}；合并后的启用值：{}，并发上限：{}。保存时备份原配置，写入 eino-harness-v1 并删除两个旧 ACP 字段。下方草稿可继续调整。",
                migration["legacy_local_enabled"],
                migration["legacy_role_enabled"],
                migration["enabled"],
                migration["max_concurrent"]
            );
            body = body.child(
                df_group(theme)
                    .child(df_label("子代理配置格式迁移", summary, theme))
                    .child(df_label(
                        "命令行优先级",
                        migration["cli_note"]
                            .as_str()
                            .unwrap_or("请检查自定义启动参数。"),
                        theme,
                    ))
                    .child(df_button(
                        "df-config-migration-ack",
                        if confirmed {
                            "已确认，保存时执行迁移"
                        } else {
                            "确认转换及 CLI 优先级变化"
                        },
                        !state.can_edit_field("/config_migration_ack")
                            || confirmed
                            || revision.is_empty(),
                        confirmed,
                        theme,
                        cx,
                        move |this, _, cx| {
                            this.deerflow_change("/config_migration_ack", json!(revision), cx)
                        },
                    )),
            );
        }
        if let Some(issue) = state.draft.as_ref().and_then(portable_issue) {
            let mut migration = df_group(theme)
                .child(df_label("已有配置需要确认迁移", issue, theme))
                .child(df_label(
                    "原配置尚未修改",
                    "转换会在草稿中固定本地执行环境，并移除非本地沙箱参数；检查后再保存。",
                    theme,
                ));
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
        let mut content = div();
        if state.section == Section::Overview && !state.applied_values.is_empty() {
            let mut effective = df_group(theme).child(df_label(
                "当前运行值",
                "由运行服务报告；已有任务保留创建时快照。",
                theme,
            ));
            for row in &state.applied_values {
                effective = effective.child(df_label(row.label.clone(), row.value.clone(), theme));
            }
            content = content.child(effective);
        }
        match state.section {
            Section::Common => {
                content = content
                    .child(self.render_deerflow_form_fields(theme, cx))
                    .child(self.render_deerflow_common(theme, cx))
            }
            Section::SkillCenter => {
                content = content.child(self.render_deerflow_skill_center(theme, cx))
            }
            Section::MCP => content = content.child(self.render_deerflow_mcp_editor(theme, cx)),
            Section::Sources => {
                content = content.child(self.render_deerflow_skill_sources(theme, cx))
            }
            Section::Legacy => {
                content = content
                    .child(self.render_deerflow_legacy_agent(theme, cx))
                    .child(self.render_deerflow_legacy_history(theme, cx))
            }
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
                        "服务诊断日志（启动后生成）",
                        state.status["diagnostics_log"]["path"]
                            .as_str()
                            .or_else(|| state.status["diagnostics_directory"].as_str())
                            .unwrap_or("尚未取得目录")
                            .to_owned(),
                    ),
                    (
                        "当前配置的默认模型",
                        doc["default_model"].as_str().unwrap_or("未配置").to_owned(),
                    ),
                    (
                        "模型",
                        doc["models"].as_array().map_or(0, Vec::len).to_string(),
                    ),
                    (
                        "子代理角色",
                        doc["subagents"]["custom_agents"]
                            .as_object()
                            .map_or(2, |roles| 2 + roles.len())
                            .to_string(),
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
                content = content.child(group);
                let mut paths = df_group(theme).child(df_label(
                    "便携数据",
                    "配置、历史、记忆与产物保存在用户数据目录。",
                    theme,
                ));
                if let Some(map) = doc["paths"].as_object() {
                    for key in ["user_data", "config", "skills"] {
                        if let Some(value) = map.get(key).and_then(Value::as_str) {
                            paths = paths.child(df_label(key, value.to_owned(), theme));
                        }
                    }
                }
                content = content.child(paths);
            }
            Section::Models => {
                let key = "models";
                let items = doc[key].as_array().map(Vec::as_slice).unwrap_or(&[]);
                let selected = state.selected_model;
                let mut picker = div().flex().flex_col().gap(px(6.0));
                for (index, item) in items.iter().enumerate() {
                    let name = item["name"].as_str().unwrap_or("未命名");
                    let label = format!(
                        "{}{} · {} · {}",
                        item["display_name"]
                            .as_str()
                            .filter(|value| !value.is_empty())
                            .unwrap_or(name),
                        if doc["default_model"].as_str() == Some(name) {
                            " · 默认"
                        } else {
                            ""
                        },
                        item["provider"].as_str().unwrap_or(""),
                        item["model"].as_str().unwrap_or("")
                    );
                    picker = picker.child(
                        df_button(
                            SharedString::from(format!("df-item-{index}")),
                            label,
                            busy,
                            index == selected,
                            theme,
                            cx,
                            move |this, _, cx| {
                                this.deerflow_settings.selected_model = index;
                                this.deerflow_rebind(cx);
                                cx.notify();
                            },
                        )
                        .w_full()
                        .justify_start(),
                    );
                }
                let mut actions = div()
                    .flex()
                    .flex_wrap()
                    .gap(px(6.0))
                    .child(df_button(
                        "df-add-item",
                        "添加模型",
                        !state.can_edit_field("/models") || invalid,
                        false,
                        theme,
                        cx,
                        move |this, _, cx| this.deerflow_add_model(cx),
                    ))
                    .child(df_button(
                        "df-remove-item",
                        "从草稿移除",
                        !state.can_edit_field("/models") || invalid || items.len() <= 1,
                        false,
                        theme,
                        cx,
                        move |this, _, cx| this.deerflow_remove_model(cx),
                    ));
                if items.get(selected).is_some() {
                    actions = actions
                        .child(df_button(
                            "df-model-default",
                            "修改默认模型",
                            !state.can_edit_field("/default_model") || invalid,
                            false,
                            theme,
                            cx,
                            move |this, _, cx| this.open_settings_page(SettingsPage::DeerFlow, cx),
                        ))
                        .child(df_button(
                            "df-model-test",
                            "测试连接",
                            !state.can_edit_field("/models") || invalid,
                            false,
                            theme,
                            cx,
                            |this, _, cx| {
                                if !this.deerflow_settings.can_edit_field("/models") {
                                    return;
                                }
                                let index = this.deerflow_settings.selected_model;
                                if let Some(document) = &this.deerflow_settings.draft {
                                    match document_with_model_names(
                                        document,
                                        &this.deerflow_settings.pending_model_names,
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
                    actions = actions.child(df_button(
                        "df-model-agent-test", "测试 Agent 能力", !state.can_edit_field("/models") || invalid, false, theme, cx,
                        |this, _, cx| {
                            let index = this.deerflow_settings.selected_model;
                            if let Some(document) = &this.deerflow_settings.draft {
                                match document_with_model_names(document, &this.deerflow_settings.pending_model_names) {
                                    Ok(document) => this.deerflow_request("test-model", json!({"model":document["models"][index],"profile":"agent"}), cx),
                                    Err(error) => { this.deerflow_settings.error = Some(error); cx.notify(); }
                                }
                            }
                        },
                    ));
                }
                if !state.pending_model_names.is_empty() {
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
                content = content.child(
                    df_group(theme).child(picker).child(actions).child(
                        div()
                            .text_size(sp(12.5))
                            .text_color(theme.text_tertiary)
                            .child("名称在确认或保存时统一更新引用；密钥留空保留已有值。更换 provider 或 API 地址会清除旧凭据及原 provider 扩展。测试连接发送一次文本请求；Agent 能力测试最多两次请求，检查流式、工具调用及 usage，不执行工具。"),
                    ),
                );
            }
            Section::Agents => {
                content = content.child(self.render_deerflow_roles(theme, cx));
            }
            Section::Skills => {
                if doc["skill_evolution_migration"]["required"] == true {
                    content = content.child(self.render_deerflow_evolution(theme, cx));
                }
            }
            Section::Memory => {
                content = content.child(df_group(theme).child(df_label(
                    "本地 DeerMem",
                    "记忆管理器固定为 DeerMem，不依赖外部记忆服务。",
                    theme,
                )));
                content = content.child(self.render_deerflow_data(theme, cx));
            }
            Section::Tools => {
                content = content
                    .child(self.render_deerflow_tools(theme, cx))
                    .child(df_button(
                        "df-tools-mcp",
                        "管理 MCP 服务器",
                        busy,
                        false,
                        theme,
                        cx,
                        |this, _, cx| this.deerflow_select_section(Section::MCP, cx),
                    ));
            }
            _ => {}
        }
        if state.section != Section::Common {
            content = content.child(self.render_deerflow_form_fields(theme, cx));
        }
        body = body.child(self.render_deerflow_panel(content, theme, cx));
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
        let caption = if field.path.starts_with("/models/") && field.path.ends_with("/api_key") {
            let model = field
                .path
                .rsplit_once('/')
                .and_then(|(root, _)| state.draft.as_ref().and_then(|doc| doc.pointer(root)));
            let credential_state = if model.is_some_and(|model| model["clear_api_key"] == true) {
                "待清除"
            } else if current.as_str().is_some_and(|key| !key.is_empty()) {
                "已填写"
            } else if model.is_some_and(|model| model["api_key_configured"] == true) {
                "已配置"
            } else {
                "未配置"
            };
            format!("{} · {credential_state}", field.label)
        } else {
            field.label.clone()
        };
        let label = df_label(caption, field.hint.clone(), theme);
        let path = field.path.clone();
        let mut row = div().w_full().flex().gap(px(24.0));
        match field.kind {
            Kind::Bool if path.starts_with("/models/") && path.ends_with("/clear_api_key") => {
                let clearing = current.as_bool().unwrap_or(false);
                row = row.items_center().child(label).child(df_button(
                    SharedString::from(format!("df-clear-key-{path}")),
                    if clearing {
                        "取消清除"
                    } else {
                        "清除密钥"
                    },
                    !state.can_edit_field(&path),
                    clearing,
                    theme,
                    cx,
                    move |this, _, cx| this.deerflow_change(&path, json!(!clearing), cx),
                ));
                if clearing {
                    row = row.child(
                        div()
                            .text_size(sp(12.0))
                            .text_color(theme.text_secondary)
                            .child("保存后清除"),
                    );
                }
            }
            Kind::Bool | Kind::TrueOnly => {
                let checked = current.as_bool().unwrap_or(false);
                row = row.items_center().child(label).child(toggle_switch(
                    SharedString::from(format!("df-toggle-{path}")),
                    checked,
                    !state.can_edit_field(&path)
                        || (matches!(field.kind, Kind::TrueOnly) && checked),
                    theme,
                    cx,
                    move |this, _, cx| this.deerflow_change(&path, json!(!checked), cx),
                ));
            }
            Kind::Choice(choices) | Kind::OptionalChoice(choices) => {
                let mut options = div().flex().flex_wrap().gap(px(4.0));
                for (value, caption) in choices {
                    let optional = matches!(field.kind, Kind::OptionalChoice(_));
                    let selected = current.as_str() == Some(*value)
                        || (optional && value.is_empty() && current.is_null());
                    let path = path.clone();
                    let value = *value;
                    options = options.child(df_button(
                        SharedString::from(format!("df-choice-{path}-{value}")),
                        *caption,
                        !state.can_edit_field(&path),
                        selected,
                        theme,
                        cx,
                        move |this, _, cx| {
                            this.deerflow_change(
                                &path,
                                if optional && value.is_empty() {
                                    Value::Null
                                } else {
                                    json!(value)
                                },
                                cx,
                            )
                        },
                    ));
                }
                row = row.flex_col().gap(px(8.0)).child(label).child(options);
            }
            Kind::OptionalBool | Kind::ModelReference | Kind::ConfiguredModelReference => {
                let options: Vec<(Value, String)> = if matches!(field.kind, Kind::OptionalBool) {
                    vec![
                        (Value::Null, "继承".into()),
                        (json!(true), "开启".into()),
                        (json!(false), "关闭".into()),
                    ]
                } else if matches!(field.kind, Kind::ConfiguredModelReference) {
                    evolution::model_options(state.draft.as_ref().unwrap_or(&Value::Null))
                } else {
                    let mut options = vec![(Value::Null, "继承父模型".into())];
                    if let Some(models) = state
                        .draft
                        .as_ref()
                        .and_then(|doc| doc["models"].as_array())
                    {
                        options.extend(
                            models
                                .iter()
                                .filter_map(|model| model["name"].as_str())
                                .map(|name| (json!(name), name.to_owned())),
                        );
                    }
                    options
                };
                let mut choices = div().flex().flex_wrap().gap(px(4.0));
                for (index, (value, caption)) in options.into_iter().enumerate() {
                    let selected = &value == current
                        || (matches!(field.kind, Kind::ModelReference)
                            && value.is_null()
                            && matches!(current.as_str(), Some("" | "inherit")));
                    let path = path.clone();
                    choices = choices.child(df_button(
                        SharedString::from(format!("df-option-{path}-{index}")),
                        caption,
                        !state.can_edit_field(&path),
                        selected,
                        theme,
                        cx,
                        move |this, _, cx| this.deerflow_change(&path, value.clone(), cx),
                    ));
                }
                row = row.flex_col().gap(px(8.0)).child(label).child(choices);
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
        let mut wrapper = div()
            .id(SharedString::from(format!("df-anchor-{}", field.path)))
            .anchor_scroll(state.field_anchors.get(&field.path).cloned())
            .when(state.search_target.as_deref() == Some(&field.path), |el| {
                el.p(px(8.0))
                    .rounded(px(7.0))
                    .border_1()
                    .border_color(theme.accent)
            })
            .child(row);
        if let Some(status) = presentation::field_status(state, field) {
            wrapper = wrapper.child(
                div()
                    .mt(px(4.0))
                    .text_size(sp(12.0))
                    .text_color(theme.text_tertiary)
                    .child(status),
            );
        }
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
    fn deerflow_missing_context_requires_a_native_cursor_and_keeps_local_identity() {
        assert!(missing_session_context(None).is_none());
        let mut session = AgentSession::new(Uuid::new_v4(), ProviderKind::DeerFlow);
        session.title = "Task with no skill reads".into();
        assert!(missing_session_context(Some(&session)).is_none());
        session.provider_cursor = Some(ProviderResumeCursor::DeerFlow {
            session_id: "native-go-session".into(),
        });
        let (local, native, title) = missing_session_context(Some(&session)).unwrap();
        assert_eq!(local, session.id);
        assert_eq!(native, "native-go-session");
        assert_eq!(title, session.title);
        assert_ne!(native, local.to_string());
        let mut other_view = session.clone();
        other_view.id = Uuid::new_v4();
        assert_ne!(
            missing_session_context(Some(&session)),
            missing_session_context(Some(&other_view))
        );
        session.provider = ProviderKind::Codex;
        assert!(missing_session_context(Some(&session)).is_none());
        session.provider = ProviderKind::DeerFlow;
        session.provider_cursor = Some(ProviderResumeCursor::from_session_id(
            ProviderKind::Codex,
            "foreign".into(),
        ));
        assert!(missing_session_context(Some(&session)).is_none());
        for invalid in [String::new(), "x".repeat(257), "native\0invalid".into()] {
            session.provider_cursor = Some(ProviderResumeCursor::DeerFlow {
                session_id: invalid,
            });
            assert!(missing_session_context(Some(&session)).is_none());
        }
    }
    #[test]
    fn deerflow_migration_confirmation_is_bound_to_the_reviewed_revision() {
        let mut doc = json!({"config_revision":"original", "config_migration":{"required":true,"source_revision":"original"},"config_migration_ack":""});
        assert!(!migration_acknowledged(&doc));
        set_value(&mut doc, "/config_migration_ack", json!("original"));
        assert!(migration_acknowledged(&doc));
        doc["config_revision"] = json!("changed");
        assert!(!migration_acknowledged(&doc));
        doc["config_migration"]["source_revision"] = json!("changed");
        assert!(!migration_acknowledged(&doc));
        doc["config_migration"]["required"] = json!(false);
        assert!(migration_acknowledged(&doc));
    }
    #[test]
    fn parsing_keeps_null_inheritance_and_rejects_non_finite_numbers() {
        assert_eq!(parse_field("", Kind::OptionalInteger).unwrap(), Value::Null);
        assert_eq!(parse_field("0", Kind::OptionalInteger).unwrap(), json!(0));
        assert_eq!(parse_field(" ", Kind::OptionalNumber).unwrap(), Value::Null);
        assert_eq!(
            parse_field("0.5", Kind::OptionalNumber).unwrap(),
            json!(0.5)
        );
        assert!(parse_field("1.5", Kind::OptionalInteger).is_err());
        assert_eq!(parse_field("  ", Kind::Optional).unwrap(), Value::Null);
        assert!(parse_field("NaN", Kind::Number).is_err());
        assert!(parse_field("1.5", Kind::Integer).is_err());
        assert_eq!(parse_field("[]", Kind::Json).unwrap(), json!([]));
    }
    #[test]
    fn model_rename_updates_references_without_changing_remote_model_ids() {
        let mut doc = json!({"default_model":"old","runtime":{"model_name":"old"},"memory":{"model_name":null},"models":[{"name":"old","model":"old"}],"skill_evolution":{"generation_model_name":"old","moderation_model_name":"old","evaluation_model_name":"quality"}});
        rename_model_references(&mut doc, "old", "new");
        assert_eq!(doc["default_model"], "new");
        assert_eq!(doc["runtime"]["model_name"], "new");
        assert_eq!(doc["models"][0]["model"], "old");
        assert!(doc["memory"]["model_name"].is_null());
        assert_eq!(doc["skill_evolution"]["generation_model_name"], "new");
        assert_eq!(doc["skill_evolution"]["moderation_model_name"], "new");
        assert_eq!(doc["skill_evolution"]["evaluation_model_name"], "quality");
    }
}

const LOCAL_SANDBOX: &str = "local";

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
    document["sandbox"]["provider"] = json!(LOCAL_SANDBOX);

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

    fn deerflow_managed(&mut self, request: &Value, data: Value, cx: &mut Context<Self>) {
        let operation = request["operation"].as_str().unwrap_or("");
        self.deerflow_settings.confirm_manage = None;
        match operation {
            "session.diagnostics" => {
                if data["session_id"] == request["session_id"] {
                    self.deerflow_settings.diagnostic_data = data;
                } else {
                    self.deerflow_settings.error = Some("诊断会话发生变化，请重新查询。".into());
                }
            }
            "session.list" => {
                self.deerflow_settings.sessions =
                    data["sessions"].as_array().cloned().unwrap_or_default();
                if !self.deerflow_settings.sessions.iter().any(|session| {
                    session["session_id"] == self.deerflow_settings.diagnostic_data["session_id"]
                }) {
                    self.deerflow_settings.diagnostic_data = Value::Null;
                }
                self.deerflow_settings.session_page = 0;
            }
            "memory.get" | "memory.delete" | "memory.promote" => {
                self.deerflow_settings.memory_data = data;
                self.deerflow_settings.memory_page = 0;
                if operation == "memory.delete" {
                    self.deerflow_settings.notice = Some("所选记忆事实已删除。".into());
                } else if operation == "memory.promote" {
                    self.deerflow_settings.notice =
                        Some("已复制到全局记忆，原事实保留。切换到全局可查看。".into());
                }
            }
            "proposal.list" | "proposal.history" => {
                match self
                    .deerflow_settings
                    .skill_browser
                    .accept_review_page(request, &data)
                {
                    Ok(rows) => {
                        self.deerflow_settings.proposal = None;
                        if operation == "proposal.list" {
                            self.deerflow_settings.proposals = rows;
                        } else {
                            self.deerflow_settings.revisions = rows;
                        }
                    }
                    Err(error) => {
                        self.deerflow_settings.skill_browser.review_stale = true;
                        self.deerflow_service_error(&error.to_string(), cx);
                    }
                }
            }
            "proposal.get" => {
                let behavior = match skills::validate_proposal(request, &data) {
                    Ok(behavior) => behavior,
                    Err(error) => {
                        self.deerflow_settings.skill_browser.review_stale = true;
                        self.deerflow_service_error(&error.to_string(), cx);
                        return;
                    }
                };
                let diff = data["diff"].as_str().unwrap_or("暂无差异").to_owned();
                self.deerflow_settings
                    .proposal_diff
                    .update(cx, |input, cx| input.set_content(diff, cx));
                self.deerflow_settings
                    .skill_browser
                    .behavior_inspector
                    .bind(&data);
                self.deerflow_settings.proposal = Some(data);
                self.deerflow_settings.skill_browser.behavior = behavior;
                self.deerflow_settings.skill_browser.review_stale = false;
            }
            "proposal.approve" | "proposal.reject" | "proposal.rollback" => {
                self.deerflow_settings
                    .skill_browser
                    .complete_mutation(request);
                self.deerflow_settings.proposal = None;
                self.deerflow_request(
                    "manage",
                    json!({"operation":"proposal.history","limit":20}),
                    cx,
                );
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

    fn deerflow_load_memory(&mut self, cx: &mut Context<Self>) {
        if self.deerflow_settings.busy() {
            return;
        }
        self.deerflow_settings.memory_data = Value::Null;
        self.deerflow_settings.memory_page = 0;
        self.deerflow_settings.confirm_manage = None;
        if let Some(request) = memory_management::get_request(
            &self.deerflow_settings.memory_scope,
            self.deerflow_settings.selected_session.as_deref(),
        ) {
            self.deerflow_request("manage", request, cx);
        } else {
            self.deerflow_settings.notice =
                Some("请选择一个历史会话，以确定项目或会话范围。".into());
            cx.notify();
        }
    }

    fn deerflow_select_memory(&mut self, session: String, cx: &mut Context<Self>) {
        if self.deerflow_settings.busy() {
            return;
        }
        self.deerflow_settings.selected_session = Some(session);
        if self.deerflow_settings.memory_scope == "global" {
            self.deerflow_settings.memory_scope = "workspace".into();
        }
        self.deerflow_load_memory(cx);
    }

    fn deerflow_select_memory_scope(&mut self, scope: &str, cx: &mut Context<Self>) {
        if self.deerflow_settings.busy() {
            return;
        }
        self.deerflow_settings.memory_scope = scope.into();
        self.deerflow_load_memory(cx);
    }

    fn render_deerflow_data(&self, theme: Theme, cx: &mut Context<Self>) -> AnyElement {
        let state = &self.deerflow_settings;
        let unavailable =
            state.busy() || state.applying() || state.status["running"].as_bool() != Some(true);
        let mut group = df_group(theme)
            .child(df_label(
                "会话与记忆数据",
                "全局记忆跨项目共享；项目和会话记忆通过下方历史会话定位。",
                theme,
            ))
            .child(div().flex().flex_wrap().gap(px(6.0)).child(df_button(
                "df-sessions-load",
                "加载会话列表",
                unavailable,
                false,
                theme,
                cx,
                |this, _, cx| {
                    this.deerflow_request("manage", json!({"operation":"session.list"}), cx)
                },
            )));
        let mut scope_controls = div().flex().flex_wrap().gap(px(6.0));
        for (scope, label) in [
            ("global", "全局"),
            ("workspace", "项目"),
            ("session", "会话"),
        ] {
            scope_controls = scope_controls.child(df_button(
                SharedString::from(format!("df-memory-scope-{scope}")),
                label,
                unavailable,
                state.memory_scope == scope,
                theme,
                cx,
                move |this, _, cx| this.deerflow_select_memory_scope(scope, cx),
            ));
        }
        scope_controls = scope_controls.child(df_button(
            "df-memory-refresh",
            "刷新记忆",
            unavailable,
            false,
            theme,
            cx,
            |this, _, cx| this.deerflow_load_memory(cx),
        ));
        group = group.child(scope_controls);
        if state.diagnostic_data.is_object() {
            group = group.child(diagnostics::render(&state.diagnostic_data, theme));
        }
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
                let diagnostic_id = id.clone();
                let delete_id = id.clone();
                group = group.child(div().flex().flex_col().gap(px(8.0))
                    .child(df_label(title.to_owned(), format!("{cwd}\n{status}"), theme))
                    .child(div().flex().flex_wrap().gap(px(6.0))
                        .child(df_button(SharedString::from(format!("df-memory-{id}")), "查看记忆", unavailable,
                            state.selected_session.as_deref() == Some(id.as_str()), theme, cx,
                            move |this, _, cx| this.deerflow_select_memory(memory_id.clone(), cx)))
                        .child(df_button(SharedString::from(format!("df-diagnostics-{id}")), "查看诊断", unavailable, false, theme, cx,
                            move |this, _, cx| this.deerflow_request("manage", json!({"operation":"session.diagnostics","session_id":diagnostic_id}), cx)))
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
                    memory_management::scope_label(
                        state.memory_data["scope"].as_str().unwrap_or("")
                    )
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
                    let scope = fact["scope"].as_str().unwrap_or("");
                    let revision = fact["revision"].as_i64().unwrap_or(0);
                    let disabled =
                        unavailable || state.status["active_operations"].as_u64().unwrap_or(0) > 0;
                    let mut actions = div().flex().flex_wrap().gap(px(6.0));
                    if let Some(request) = memory_management::mutation_request(
                        "memory.delete",
                        scope,
                        state.selected_session.as_deref(),
                        &id,
                        revision,
                    ) {
                        let confirmation = if scope == "global" {
                            format!(
                                "删除全局记忆事实 {id}。所有项目后续读取都将不再使用此事实，已有项目副本保留。"
                            )
                        } else {
                            format!("删除当前范围中的记忆事实 {id}。其他范围的事实保留。")
                        };
                        actions = actions.child(df_button(
                            SharedString::from(format!("df-fact-delete-{id}")),
                            "删除…",
                            disabled,
                            false,
                            theme,
                            cx,
                            move |this, _, cx| {
                                this.deerflow_settings.confirm_manage =
                                    Some((confirmation.clone(), request.clone()));
                                cx.notify();
                            },
                        ));
                    }
                    if let Some(request) = memory_management::mutation_request(
                        "memory.promote",
                        scope,
                        state.selected_session.as_deref(),
                        &id,
                        revision,
                    ) {
                        actions = actions.child(df_button(
                            SharedString::from(format!("df-fact-promote-{id}")), "提升为全局…", disabled, false, theme, cx,
                            move |this, _, cx| {
                                this.deerflow_settings.confirm_manage = Some((
                                    "将这条记忆复制到全局，其他项目和模型将能读取。原事实保留，相同全局事实不会重复创建。".into(), request.clone(),
                                ));
                                cx.notify();
                            },
                        ));
                    }
                    group = group.child(
                        div()
                            .flex()
                            .flex_col()
                            .gap(px(8.0))
                            .child(df_label(
                                fact["content"].as_str().unwrap_or("").to_owned(),
                                format!(
                                    "{} · v{revision} · {id}",
                                    memory_management::scope_label(scope)
                                ),
                                theme,
                            ))
                            .child(actions),
                    );
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
        let history_only = state.skill_center_tab == presentation::SkillCenterTab::History;
        let mut group = df_group(theme).child(df_label(
            if history_only {
                "发布历史"
            } else {
                "待审核候选"
            },
            if history_only {
                "查看已发布修订，核对后可恢复历史版本。"
            } else {
                "查看候选差异和评测报告，再批准或拒绝。"
            },
            theme,
        ));
        let query = if history_only {
            json!({"operation":"proposal.history","limit":20})
        } else {
            json!({"operation":"proposal.list","status":"pending_review","limit":20})
        };
        group = group.child(df_button(
            "df-review-refresh",
            "刷新列表",
            unavailable,
            false,
            theme,
            cx,
            move |this, _, cx| this.deerflow_request("manage", query.clone(), cx),
        ));
        if !history_only {
            for proposal in &state.proposals {
                let id = proposal["id"].as_str().unwrap_or("").to_owned();
                let title = format!(
                    "{} / {} · {} · {}",
                    proposal["target"]["source_id"]
                        .as_str()
                        .unwrap_or("未知来源"),
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
            group = group.child(self.render_skill_page_nav(
                "proposals",
                &state.skill_browser.proposal_nav,
                unavailable || state.skill_browser.review_stale,
                theme,
                cx,
            ));
            if let Some(proposal) = &state.proposal {
                let id = proposal["id"].as_str().unwrap_or("").to_owned();
                let approve_id = id.clone();
                let base = proposal["base_sha256"].clone();
                let observed_state = proposal["state_version"].clone();
                let reject_state = observed_state.clone();
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
                group = group.child(self.render_deerflow_skill_evaluation(proposal, theme));
                group = group.child(self.render_deerflow_behavior_inspector(proposal, theme, cx));
                let disabled = unavailable
                    || state.dirty
                    || state.skill_browser.review_stale
                    || state.skill_browser.mutations_pending()
                    || observed_state.as_u64().is_none_or(|v| v == 0);
                let approve_disabled =
                    disabled || proposal["status"].as_str() != Some("pending_review");
                let reject_disabled = disabled
                    || !matches!(
                        proposal["status"].as_str(),
                        Some("pending_review" | "validating")
                    );
                group = group.child(div().flex().flex_none().flex_wrap().gap(px(6.0))
                .child(df_button("df-proposal-approve", "确认发布", approve_disabled, false, theme, cx,
                    move |this, _, cx| {
                        this.deerflow_settings.confirm_manage = None;
                        this.deerflow_request("manage", json!({"operation":"proposal.approve","proposal_id":approve_id,"expected_base_sha256":base,"expected_state_version":observed_state}), cx);
                        this.settings_scroll.set_offset(gpui::point(px(0.0), px(0.0)));
                        cx.notify();
                    }))
                .child(df_button("df-proposal-reject", "拒绝提案…", reject_disabled, false, theme, cx,
                    move |this, _, cx| {
                        this.deerflow_settings.confirm_manage = Some((format!("拒绝提案 {id}。"), json!({"operation":"proposal.reject","proposal_id":id,"expected_state_version":reject_state})));
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
        }
        if history_only {
            for revision in &state.revisions {
                let name = revision["name"].as_str().unwrap_or("").to_owned();
                let version = revision["version"].as_u64().unwrap_or(0);
                let source = revision["target"]["source_id"]
                    .as_str()
                    .unwrap_or("未知来源");
                let identity = revision["target"]["source_identity"].as_str().unwrap_or("");
                let revision = revision.clone();
                group = group.child(
                    div()
                        .flex()
                        .items_center()
                        .gap(px(16.0))
                        .child(df_label(
                            format!("{source} / {name} · 修订 {version}"),
                            "历史技能修订；确认前重新读取当前发布状态",
                            theme,
                        ))
                        .child(df_button(
                            SharedString::from(format!("df-revision-{identity}-{name}-{version}")),
                            "恢复此版本…",
                            unavailable
                                || state.dirty
                                || state.skill_browser.mutations_pending()
                                || version == 0,
                            false,
                            theme,
                            cx,
                            move |this, _, cx| {
                                this.deerflow_prepare_skill_rollback(revision.clone(), cx);
                            },
                        )),
                );
            }
            group = group.child(self.render_skill_page_nav(
                "history",
                &state.skill_browser.history_nav,
                unavailable || state.skill_browser.review_stale,
                theme,
                cx,
            ));
        }
        group.into_any_element()
    }
}

fn portable_issue(document: &Value) -> Option<String> {
    if document["sandbox"]["provider"].as_str() != Some(LOCAL_SANDBOX) {
        return Some(
            "此配置使用其他执行器。便携桌面仅支持 Go 本地执行器，请确认转换后再保存。".into(),
        );
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
                            "https_proxy",
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
        let mut doc = json!({"sandbox":{"provider":"docker:Provider","allow_host_bash":false,"allow_host_tools":false},"tool_diagnostics":["sandbox.image：原数据保留"],"memory":{"manager_class":"remote"}});
        let original = doc.clone();
        assert!(portable_issue(&doc).is_some());
        assert_eq!(doc, original);
        assert!(normalize_portable_document(&mut doc));
        assert!(portable_issue(&doc).is_none());
        assert_eq!(doc["sandbox"]["provider"], "local");
        assert_eq!(doc["tool_diagnostics"], original["tool_diagnostics"]);
    }
    #[test]
    fn service_errors_hide_unsaved_and_nested_secrets() {
        let doc = json!({"models":[{"api_key":"sk-test-private"}],"tools":[{"https_proxy":"http://fixture:proxy-secret@localhost:11808","headers":{"authorization":"Bearer local-secret"}}],"client_mcp_servers":[{"name":"fixture","env":[{"name":"MCP_KEY","value":"mcp-private"}]}]});
        let text = redact_deerflow_message(
            "request sk-test-private failed: Bearer local-secret mcp-private http://fixture:proxy-secret@localhost:11808",
            Some(&doc),
        );
        assert!(!text.contains("sk-test-private"));
        assert!(!text.contains("local-secret"));
        assert!(!text.contains("mcp-private"));
        assert!(!text.contains("proxy-secret"));
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
                &json!({"running":false,"applying":false,"apply_error":"configuration failed"}),
                false,
                false,
                false,
                None
            ),
            DeerFlowServicePhase::Failed
        );
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

    #[test]
    fn startup_keeps_configuration_editable_and_failure_releases_it() {
        assert!(!deerflow_editing_locked(
            &json!({"running":false,"applying":true,"starting":true}),
            None
        ));
        assert!(!deerflow_editing_locked(
            &json!({"running":false,"applying":false,"starting":false,"apply_error":"failed"}),
            None
        ));
        assert!(deerflow_editing_locked(
            &json!({"applying":true,"starting":false}),
            None
        ));
        assert!(deerflow_editing_locked(&json!({"applying":true}), None));
        assert!(deerflow_editing_locked(
            &json!({"applying":true,"starting":true}),
            Some("apply")
        ));
    }
}

impl Waku {
    fn deerflow_commit_names(&mut self, cx: &mut Context<Self>) -> bool {
        if self.deerflow_settings.pending_model_names.is_empty()
            && self.deerflow_settings.roles.pending_name.is_none()
        {
            return true;
        }
        if !self.deerflow_settings.can_edit()
            || (self.deerflow_settings.roles.pending_name.is_some()
                && !self.deerflow_settings.can_edit_field(roles::NAME_PATH))
            || self
                .deerflow_settings
                .pending_model_names
                .keys()
                .any(|index| {
                    !self
                        .deerflow_settings
                        .can_edit_field(&format!("/models/{index}/name"))
                })
        {
            self.deerflow_settings.error = Some("名称或引用字段暂不可编辑；草稿已保留。".into());
            cx.notify();
            return false;
        }
        let Some(document) = &self.deerflow_settings.draft else {
            return false;
        };
        match document_with_model_names(document, &self.deerflow_settings.pending_model_names)
            .and_then(|doc| self.deerflow_settings.roles.projected(&doc))
        {
            Ok(document) => {
                self.deerflow_settings.draft = Some(document);
                self.deerflow_settings.pending_model_names.clear();
                if let Some(name) = self.deerflow_settings.roles.pending_name.take() {
                    self.deerflow_settings.roles.selected.name = name.trim().to_owned();
                }
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
        json!({"models":[{"name":"gpt","original_name":"gpt","model":"remote-a"},{"name":"gpt4","original_name":"gpt4","model":"remote-b"}],"default_model":"gpt","runtime":{"model_name":"gpt4"},"memory":{"model_name":"gpt"},"subagents":{"agents":{"general":{"model":"gpt4"}},"custom_agents":{"writer":{"model":"gpt4"},"reader":{"model":"inherit"}}}})
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
    fn renaming_and_deleting_cover_builtin_and_custom_subagents() {
        let mut saved =
            document_with_model_names(&document(), &HashMap::from([(1, "gpt5".into())])).unwrap();
        assert_eq!(saved["subagents"]["agents"]["general"]["model"], "gpt5");
        assert_eq!(
            saved["subagents"]["custom_agents"]["writer"]["model"],
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
            "models" => Section::Models,
            "default_model" => Section::Common,
            "subagents" => Section::Agents,
            "memory" => Section::Memory,
            "skills_enabled" => Section::Common,
            "skill_sources" => Section::Sources,
            "skills" | "skill_evolution" => Section::Skills,
            "client_mcp_servers" => Section::MCP,
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
            "runtime" if path == "/runtime/memory_scope" => Section::Memory,
            "runtime" => Section::Runtime,
            _ => return,
        };
        self.settings_page = Some(presentation::page_for_section(section));
        self.deerflow_settings.section = section;
        self.deerflow_settings.search_target = Some(path.clone());
        self.deerflow_settings.focus_pending = Some(path.clone());
        self.deerflow_settings.advanced = section != Section::MCP;
        if let Some(index) = path
            .strip_prefix("/skill_sources/managed/")
            .and_then(|path| path.split('/').next())
            .and_then(|index| index.parse::<usize>().ok())
        {
            self.deerflow_settings.selected_skill_source = index;
        }
        if path.starts_with("/subagents/agents/") || path.starts_with("/subagents/custom_agents/") {
            if let Some(key) = self
                .deerflow_settings
                .roles
                .choices
                .iter()
                .find(|key| path.starts_with(&(key.root() + "/")))
                .cloned()
            {
                if self.deerflow_settings.roles.pending_name.is_none() {
                    self.deerflow_settings.roles.selected = key;
                }
            }
        }
        let index = path
            .split('/')
            .nth(2)
            .and_then(|part| part.parse::<usize>().ok());
        if let Some(index) = index {
            if top == "models" {
                self.deerflow_settings.selected_model = index;
            } else if top == "tools" {
                if let Some(name) = self
                    .deerflow_settings
                    .draft
                    .as_ref()
                    .and_then(|doc| doc["tools"][index]["name"].as_str())
                {
                    if let Some(selected) = tools::NAMES.iter().position(|tool| *tool == name) {
                        self.deerflow_settings.selected_tool = selected;
                    }
                }
            } else if top == "client_mcp_servers" {
                self.deerflow_settings.selected_mcp = index;
                if let Some(env_index) = path
                    .split('/')
                    .nth(4)
                    .and_then(|item| item.parse::<usize>().ok())
                {
                    self.deerflow_settings.selected_mcp_env = env_index;
                }
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
            self.deerflow_settings.search_target = Some(field.path.clone());
            self.deerflow_settings.focus_pending = Some(field.path.clone());
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
