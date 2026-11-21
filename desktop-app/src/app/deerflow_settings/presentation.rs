//! Presentation only: navigation, disclosure and search never define host grants.
use super::*;

#[derive(Clone, Copy, Default, PartialEq, Eq)]
pub(super) enum SkillCenterTab {
    #[default]
    Catalog,
    Review,
    History,
}

pub(super) const ADVANCED_GROUPS: &[(Section, &str, &str)] = &[
    (
        Section::Agents,
        "子代理与角色",
        "角色模型、指令、并发与委派限制",
    ),
    (
        Section::Memory,
        "记忆与会话管理",
        "注入、检索、作用域及已保存内容",
    ),
    (Section::Sources, "技能来源", "托管目录、作用域与导出目录"),
    (
        Section::Runtime,
        "运行与数据",
        "连接、超时、保留规则及附加指令",
    ),
    (
        Section::Overview,
        "服务与诊断",
        "实际运行值、日志、启动与配置重读",
    ),
];

// Only the outer page mounts form fields. Child pages supply their non-form
// content so GPUI never shares interaction state between duplicate field IDs.
pub(super) fn compose_settings_content(
    section: Section,
    content: Div,
    form_fields: impl FnOnce() -> AnyElement,
) -> Div {
    match section {
        Section::Common => div().child(form_fields()).child(content),
        Section::SkillCenter => content,
        _ => content.child(form_fields()),
    }
}

pub(super) fn common_fields(doc: &Value) -> Vec<Field> {
    let mut fields = vec![
        Field::new(
            "/default_model",
            "默认聊天模型",
            "新会话使用此模型；当前会话可另行选择",
            Kind::ConfiguredModelReference,
        ),
        Field::new(
            "/subagents/enabled",
            "允许子代理",
            "将部分任务委派给其他角色",
            Kind::Bool,
        ),
    ];
    if doc["network"]["proxy"].is_object() {
        fields.splice(1..1, network::fields());
    }
    if doc["subagents"]["enabled"] == true {
        fields.push(Field::new(
            "/subagents/max_concurrent",
            "子代理并发数",
            "同一个主任务共享此上限",
            Kind::Integer,
        ));
    }
    fields.extend([
        Field::new(
            "/memory/enabled",
            "长期记忆",
            "保存并使用当前记忆范围内的信息",
            Kind::Bool,
        ),
        Field::new(
            "/skills_enabled",
            "加载技能",
            "使用已启用的技能指导任务",
            Kind::Bool,
        ),
    ]);

    fields
}

fn model_field_visible(key: &str, advanced: bool) -> bool {
    advanced
        || matches!(
            key,
            "name"
                | "display_name"
                | "provider"
                | "api"
                | "model"
                | "base_url"
                | "api_key"
                | "clear_api_key"
        )
}

pub(super) fn visible_field(state: &DeerFlowSettings, field: &Field) -> bool {
    if state.search_target.as_deref() == Some(&field.path) || state.errors.contains_key(&field.path)
    {
        return true;
    }
    let key = field.path.rsplit('/').next().unwrap_or("");
    match state.section {
        Section::Models => model_field_visible(key, state.advanced),
        Section::Tools => {
            state.advanced
                || matches!(
                    field.path.as_str(),
                    "/runtime/permission_mode"
                        | "/runtime/enable_bash"
                        | "/sandbox/allow_host_bash"
                        | "/sandbox/allow_host_tools"
                )
                || matches!(key, "api_key" | "executable")
        }
        _ => true,
    }
}

pub(super) fn field_status(state: &DeerFlowSettings, field: &Field) -> Option<&'static str> {
    let doc = state.draft.as_ref()?;
    let current = doc.pointer(&field.path)?;
    // The daemon only projects explicitly supported non-secret fields. Model
    // aliases and provider IDs are different identities, so never compare them.
    let applied = state.status["applied_settings"]["fields"]
        .get(&field.path)
        .and_then(|entry| entry.get("value"));
    if state.status["running"] == true {
        if let Some(applied) = applied {
            if current != applied
                && !(current.is_number()
                    && applied.is_number()
                    && current.as_f64() == applied.as_f64())
            {
                return Some("草稿与当前运行值不同；实际值及来源见高级诊断");
            }
        }
    }
    if state
        .saved_draft
        .as_ref()
        .and_then(|saved| saved.pointer(&field.path))
        .is_some_and(|saved| saved != current)
    {
        Some("已修改，保存后应用")
    } else {
        None
    }
}

#[derive(Clone, Copy)]
pub(in crate::app) struct SearchSeed {
    pub(in crate::app) label: &'static str,
    pub(in crate::app) path: &'static str,
    section: Section,
    keywords: &'static str,
    tool: Option<&'static str>,
}

macro_rules! target {
    ($label:literal, $path:literal, $section:ident, $words:literal) => {
        SearchSeed {
            label: $label,
            path: $path,
            section: Section::$section,
            keywords: $words,
            tool: None,
        }
    };
}

const SEARCH_TARGETS: &[SearchSeed] = &[
    target!(
        "网络代理",
        "/network/proxy/enabled",
        Common,
        "network proxy 网络 代理 socks http"
    ),
    target!(
        "代理地址",
        "/network/proxy/url",
        Common,
        "proxy url 代理 地址"
    ),
    target!(
        "绕过代理",
        "/network/proxy/no_proxy",
        Common,
        "no_proxy bypass 内网 直连 绕过"
    ),
    target!(
        "默认聊天模型",
        "/default_model",
        Common,
        "default model 默认 模型"
    ),
    target!(
        "子代理开关",
        "/subagents/enabled",
        Common,
        "subagent agent 子代理 委派"
    ),
    target!(
        "子代理并发数",
        "/subagents/max_concurrent",
        Agents,
        "concurrency 并发 子代理"
    ),
    target!("长期记忆", "/memory/enabled", Common, "memory 记忆"),
    target!("加载技能", "/skills_enabled", Common, "skills 技能 启用"),
    target!(
        "技能自动进化与记录",
        "/skills/history",
        SkillCenter,
        "skill evolution maintenance 技能维护 自动维护 自动化设置 学习 发现 生成 评测 采用 撤回 记录 状态"
    ),
    target!(
        "模型 API 地址",
        "/models/*/base_url",
        Models,
        "endpoint api base url 模型 地址"
    ),
    target!("模型密钥", "/models/*/api_key", Models, "api key 密钥 模型"),
    target!(
        "模型接口协议",
        "/models/*/api",
        Models,
        "responses chat completions api 模型 接口 协议"
    ),
    target!(
        "输出上限参数",
        "/models/*/output_token_parameter",
        Models,
        "max_tokens max_completion_tokens 输出 兼容 参数"
    ),
    target!(
        "推理强度",
        "/models/*/reasoning_efforts",
        Models,
        "reasoning effort 思考 推理 强度"
    ),
    target!(
        "思考模式接口",
        "/models/*/thinking_parameter",
        Models,
        "thinking 思考 开启 关闭 接口"
    ),
    target!(
        "思考预算",
        "/models/*/thinking_budget_tokens",
        Models,
        "thinking Claude 思考 预算"
    ),
    target!(
        "上下文窗口",
        "/models/*/context_window",
        Models,
        "context 上下文 token"
    ),
    target!(
        "图片输入",
        "/models/*/supports_vision",
        Models,
        "vision 图片 视觉"
    ),
    target!(
        "图片生成",
        "/models/*/supports_image_generation",
        Models,
        "image generation 图片 生图 画图"
    ),
    target!(
        "工具审批",
        "/runtime/permission_mode",
        Tools,
        "permission approval 工具 权限 审批"
    ),
    target!(
        "命令工具",
        "/runtime/enable_bash",
        Tools,
        "bash powershell shell 命令 终端"
    ),
    target!(
        "主机命令授权",
        "/sandbox/allow_host_bash",
        Tools,
        "host sandbox 主机 命令 权限 沙箱"
    ),
    target!(
        "工具允许列表",
        "/runtime/tool_allowlist",
        Tools,
        "allowlist 白名单 允许 工具 权限"
    ),
    target!(
        "工具拒绝列表",
        "/runtime/tool_denylist",
        Tools,
        "denylist 黑名单 拒绝 工具 权限"
    ),
    SearchSeed {
        label: "OpenCLI 程序路径",
        path: "/tools/*/executable",
        section: Section::Tools,
        keywords: "opencli executable path 浏览器 路径",
        tool: Some("host_opencli"),
    },
    SearchSeed {
        label: "网页工具代理",
        path: "/tools/*/https_proxy",
        section: Section::Tools,
        keywords: "proxy 代理 网页 网络",
        tool: Some("web_search"),
    },
    target!(
        "MCP 服务器",
        "/client_mcp_servers",
        MCP,
        "mcp stdio 服务器 集成"
    ),
    target!(
        "子代理超时",
        "/subagents/timeout_seconds",
        Agents,
        "timeout 子代理 超时"
    ),
    target!(
        "委派层数",
        "/subagents/max_depth",
        Agents,
        "depth 嵌套 委派 层数"
    ),
    target!(
        "角色指令",
        "/subagents/agents/general-purpose/system_prompt",
        Agents,
        "role prompt 角色 指令"
    ),
    target!(
        "自动续跑",
        "/subagents/auto_continue",
        Agents,
        "continue 自动 续跑 后台"
    ),
    target!(
        "记忆检索",
        "/memory/retrieval_enabled",
        Memory,
        "retrieval 检索 记忆"
    ),
    target!(
        "记忆接入方式",
        "/memory/mode",
        Memory,
        "mode middleware tool 记忆 接入"
    ),
    target!(
        "记忆作用域",
        "/runtime/memory_scope",
        Memory,
        "scope 工作区 会话 范围"
    ),
    target!(
        "技能来源",
        "/skill_sources/import_path",
        Sources,
        "source path 技能 来源 目录 导入"
    ),
    target!(
        "技能导出目录",
        "/skill_sources/export_root",
        Sources,
        "export 技能 导出 目录"
    ),
    target!(
        "同时运行任务数",
        "/runtime/max_active_runs",
        Runtime,
        "parallel 并发 同时 任务"
    ),
    target!(
        "运行超时",
        "/runtime/run_timeout_seconds",
        Runtime,
        "runtime timeout 运行 超时"
    ),
    target!(
        "单次模型输出上限",
        "/runtime/max_output_tokens",
        Runtime,
        "output tokens 输出 推理 预算 上限"
    ),
    target!(
        "排队超时",
        "/runtime/queue_timeout_seconds",
        Runtime,
        "queue 排队 超时"
    ),
    target!(
        "任务累计 Token 上限",
        "/runtime/max_run_tokens",
        Runtime,
        "token budget 任务 预算 累计 限额"
    ),
    target!(
        "任务模型调用上限",
        "/runtime/max_model_calls",
        Runtime,
        "model calls budget 模型 调用 预算 次数"
    ),
    target!(
        "任务工具调用上限",
        "/runtime/max_tool_calls",
        Runtime,
        "tool calls budget 工具 调用 预算 次数"
    ),
    target!(
        "附加系统指令",
        "/runtime/prompt_overlay",
        Runtime,
        "system prompt 系统 附加 指令"
    ),
    target!(
        "服务状态与日志",
        "/diagnostics",
        Overview,
        "acp service log 日志 服务 诊断 运行值"
    ),
];

#[derive(Clone)]
pub(in crate::app) struct SearchTarget {
    pub(in crate::app) label: String,
    pub(in crate::app) path: String,
    section: Section,
    keywords: String,
    tool: Option<&'static str>,
}

impl SearchTarget {
    pub(in crate::app) fn key(&self) -> String {
        format!("{}:{}", self.tool.unwrap_or(""), self.path)
    }
}

pub(super) fn search_catalogue(state: &DeerFlowSettings) -> Vec<SearchTarget> {
    let mut targets: Vec<_> = SEARCH_TARGETS
        .iter()
        .map(|seed| SearchTarget {
            label: seed.label.into(),
            path: seed.path.into(),
            section: seed.section,
            keywords: format!("{} {}", seed.label, seed.keywords).to_lowercase(),
            tool: seed.tool,
        })
        .collect();
    let mut add = |field: Field, section: Section, tool: Option<&'static str>| {
        let path = if tool.is_some() {
            format!("/tools/*/{}", field.path.rsplit('/').next().unwrap_or(""))
        } else if let Some(model_field) = field
            .path
            .strip_prefix("/models/")
            .and_then(|path| path.split_once('/').map(|(_, field)| field))
        {
            format!("/models/*/{model_field}")
        } else {
            field.path.clone()
        };
        let keywords =
            format!("{} {} {}", field.label, field.hint, tool.unwrap_or("")).to_lowercase();
        if let Some(existing) = targets
            .iter_mut()
            .find(|target| target.path == path && target.tool == tool)
        {
            existing.keywords.push(' ');
            existing.keywords.push_str(&keywords);
        } else {
            targets.push(SearchTarget {
                label: tool.map_or_else(
                    || field.label.clone(),
                    |name| format!("{name} · {}", field.label),
                ),
                path,
                section,
                keywords,
                tool,
            });
        }
    };
    // Build once on draft hydration/selection. Only labels and explanations
    // enter the index; credentials, user values and file contents never do.
    for section in [
        Section::Common,
        Section::Models,
        Section::Agents,
        Section::Memory,
        Section::Sources,
        Section::Runtime,
        Section::MCP,
    ] {
        for field in fields_for_section(state, section) {
            add(field, section, None);
        }
    }
    for field in fields_for_section(state, Section::Tools)
        .into_iter()
        .filter(|field| !field.path.starts_with("/tools/"))
    {
        add(field, Section::Tools, None);
    }
    let doc = state.draft.as_ref().unwrap_or(&Value::Null);
    for (index, name) in tools::NAMES.iter().enumerate() {
        for field in tools::fields(doc, index) {
            add(field, Section::Tools, Some(name));
        }
    }
    targets
}

pub(in crate::app) fn search(query: &str, state: &DeerFlowSettings) -> Vec<SearchTarget> {
    if query.is_empty() {
        return vec![];
    }
    state
        .search_catalogue
        .iter()
        .filter(|item| item.keywords.contains(query))
        .take(10)
        .cloned()
        .collect()
}

pub(super) fn page_for_section(section: Section) -> SettingsPage {
    match section {
        Section::Common => SettingsPage::DeerFlow,
        Section::Models => SettingsPage::DeerFlowModels,
        Section::Tools | Section::MCP => SettingsPage::DeerFlowTools,
        Section::SkillCenter => SettingsPage::DeerFlowSkillCenter,
        _ => SettingsPage::DeerFlowAdvanced,
    }
}

impl Waku {
    pub(in crate::app) fn deerflow_route_page(
        &mut self,
        page: SettingsPage,
        cx: &mut Context<Self>,
    ) {
        let section = match page {
            SettingsPage::DeerFlowModels => Section::Models,
            SettingsPage::DeerFlowTools => Section::Tools,
            SettingsPage::DeerFlowAdvanced => Section::Advanced,
            SettingsPage::DeerFlowSkillCenter => Section::SkillCenter,
            _ => Section::Common,
        };
        self.deerflow_select_section(section, cx);
    }

    pub(in crate::app) fn open_deerflow_skill_center(&mut self, cx: &mut Context<Self>) {
        self.open_settings_page(SettingsPage::DeerFlowSkillCenter, cx);
    }

    fn open_deerflow_skill_history(&mut self, cx: &mut Context<Self>) {
        self.deerflow_settings.skill_center_tab = SkillCenterTab::History;
        self.deerflow_settings.show_more = false;
        self.open_deerflow_skill_center(cx);
    }

    pub(in crate::app) fn deerflow_find_setting(
        &mut self,
        target: SearchTarget,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if target.section == Section::SkillCenter {
            self.open_deerflow_skill_history(cx);
            self.deerflow_settings.search_key = Some(target.key());
            return;
        }
        self.open_settings_page(page_for_section(target.section), cx);
        self.deerflow_settings.section = target.section;
        self.deerflow_settings.advanced = target.section != Section::MCP;
        if let Some(name) = target.tool {
            if let Some(index) = tools::NAMES.iter().position(|item| *item == name) {
                self.deerflow_settings.selected_tool = index;
            }
        }
        let path = if target.path.starts_with("/models/*/") {
            target
                .path
                .replacen('*', &self.deerflow_settings.selected_model.to_string(), 1)
        } else if target.path.starts_with("/tools/*/") {
            tools::index(
                self.deerflow_settings
                    .draft
                    .as_ref()
                    .unwrap_or(&Value::Null),
                self.deerflow_settings.selected_tool,
            )
            .map(|index| target.path.replacen('*', &index.to_string(), 1))
            .unwrap_or_else(|| target.path.clone())
        } else if target
            .path
            .starts_with("/subagents/agents/general-purpose/")
        {
            format!(
                "{}/{}",
                self.deerflow_settings.roles.selected.root(),
                target
                    .path
                    .strip_prefix("/subagents/agents/general-purpose/")
                    .unwrap_or("system_prompt")
            )
        } else {
            target.path.clone()
        };
        self.deerflow_settings.search_target = Some(path.clone());
        self.deerflow_settings.search_key = Some(target.key());
        self.deerflow_rebind(cx);
        self.deerflow_focus_field(&path, window, cx);
        cx.notify();
    }

    pub(in crate::app) fn deerflow_focus_pending(
        &mut self,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Some(path) = self.deerflow_settings.focus_pending.take() else {
            return;
        };
        if self.deerflow_settings.search_target.as_deref() == Some(&path) {
            self.deerflow_focus_field(&path, window, cx);
        }
    }

    pub(super) fn deerflow_focus_field(
        &self,
        path: &str,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if let Some(anchor) = self.deerflow_settings.field_anchors.get(path) {
            anchor.scroll_to(window, cx);
        }
        if let Some(slot) = self
            .deerflow_settings
            .fields
            .iter()
            .find(|field| field.path == path)
            .and_then(|field| field.slot)
        {
            window.focus(&self.deerflow_settings.inputs[slot].read(cx).focus(), cx);
        }
    }

    fn deerflow_undo_draft(&mut self, cx: &mut Context<Self>) {
        if self.deerflow_settings.busy() || self.deerflow_settings.editing_locked() {
            return;
        }
        let Some(saved) = self.deerflow_settings.saved_draft.clone() else {
            return;
        };
        let state = &mut self.deerflow_settings;
        state.draft = Some(saved);
        state.network_probe = None;
        state.invalid_text.clear();
        state.pending_model_names.clear();
        state.roles.pending_name = None;
        state.confirm_reload = false;
        state.errors.clear();
        state.error = None;
        state.dirty = false;
        state.edit_generation += 1;
        state.notice = Some("未保存的修改已撤销。".into());
        self.deerflow_rebind(cx);
        cx.notify();
    }

    pub(in crate::app) fn render_deerflow_save_bar(&self, cx: &mut Context<Self>) -> AnyElement {
        let state = &self.deerflow_settings;
        if !state.dirty && !state.needs_apply && !state.applying() {
            return div().into_any_element();
        }
        let theme = Theme::current(cx);
        let mut bar = div()
            .px(px(32.0))
            .py(px(12.0))
            .flex_none()
            .flex()
            .items_center()
            .gap(px(8.0))
            .border_t_1()
            .border_color(theme.border)
            .bg(theme.surface)
            .child(
                div()
                    .flex_1()
                    .text_size(sp(12.5))
                    .text_color(theme.text_secondary)
                    .child(if state.editing_locked() {
                        "已保存，等待任务完成后生效".to_owned()
                    } else if !state.errors.is_empty() {
                        format!("{} 处需要修改；草稿已保留", state.errors.len())
                    } else if state.dirty {
                        "有未保存的更改".into()
                    } else if state.applying() && state.status["starting"] == true {
                        "ACP 正在启动；可修改配置".into()
                    } else {
                        "配置已保存，尚未生效".into()
                    }),
            );
        if state.dirty {
            bar = bar
                .child(df_button(
                    "df-undo",
                    "撤销更改",
                    !state.can_edit(),
                    false,
                    theme,
                    cx,
                    |this, _, cx| this.deerflow_undo_draft(cx),
                ))
                .child(df_button(
                    "df-save",
                    "保存更改",
                    !state.can_edit() || !state.errors.is_empty(),
                    true,
                    theme,
                    cx,
                    |this, _, cx| this.deerflow_save_or_validate("save", cx),
                ));
        } else if state.needs_apply && !state.applying() {
            bar = bar.child(df_button(
                "df-apply",
                "重试应用",
                state.busy(),
                true,
                theme,
                cx,
                |this, _, cx| this.deerflow_request("apply", json!({}), cx),
            ));
        }
        bar.into_any_element()
    }

    pub(super) fn render_deerflow_common(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let group = df_group(theme).id("df-common-evolution").child(df_label(
            "技能自动进化", "随服务启动，使用主模型和默认策略自动学习、验证并采用技能，无需额外配置。进度、等待原因和采用、撤回记录统一在技能管理中查看。", theme));
        let mut links = div().flex().flex_wrap().gap(px(6.0));
        for (section, label) in [
            (Section::Agents, "管理角色"),
            (Section::Memory, "管理记忆"),
            (Section::Sources, "配置技能来源"),
        ] {
            links = links.child(df_button(
                SharedString::from(format!("df-common-link-{label}")),
                label,
                state.busy(),
                false,
                theme,
                cx,
                move |this, _, cx| {
                    this.open_settings_page(SettingsPage::DeerFlowAdvanced, cx);
                    this.deerflow_select_section(section, cx);
                },
            ));
        }
        links = links.child(df_button(
            "df-common-skills",
            "查看技能进化记录",
            state.busy(),
            false,
            theme,
            cx,
            |this, _, cx| this.open_deerflow_skill_history(cx),
        ));
        div()
            .child(df_group(theme).child(df_label(
                "首次使用只需配置模型",
                "在“模型”中填写服务地址、模型 ID 和密钥并保存，即可使用默认配置开始任务。技能自动进化使用该模型自动运行；搜索等外部服务按需配置。",
                theme,
            )))
            .child(self.render_deerflow_readiness(theme, cx))
            .child(group.child(links))
            .into_any_element()
    }

    pub(super) fn render_deerflow_panel(
        &self,
        content: Div,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        if self.settings_page != Some(SettingsPage::DeerFlowAdvanced) {
            return content.into_any_element();
        }
        let state = &self.deerflow_settings;
        let mut body = div();
        let mut content = Some(content.into_any_element());
        for &(section, label, hint) in ADVANCED_GROUPS {
            let selected = state.section == section;
            let mut group = df_group(theme)
                .child(
                    df_button(
                        SharedString::from(format!("df-advanced-{label}")),
                        format!("{} {label}", if selected { "▾" } else { "▸" }),
                        state.busy(),
                        selected,
                        theme,
                        cx,
                        move |this, _, cx| {
                            this.deerflow_select_section(
                                if selected { Section::Advanced } else { section },
                                cx,
                            );
                            this.settings_scroll.set_offset(gpui::Point::default());
                        },
                    )
                    .w_full()
                    .justify_start(),
                )
                .child(
                    div()
                        .text_size(sp(12.5))
                        .text_color(theme.text_secondary)
                        .child(hint),
                );
            if selected {
                if let Some(content) = content.take() {
                    group = group.child(content);
                }
            }
            body = body.child(group);
        }
        body.into_any_element()
    }

    pub(super) fn render_deerflow_form_fields(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let mut body = div();
        if state.section == Section::Common {
            let mut group = df_group(theme);
            for field in &state.fields {
                if field.path.starts_with("/network/") {
                    continue;
                }
                group = group.child(self.render_deerflow_field(field, theme, cx));
                if field.path == "/default_model"
                    && state
                        .draft
                        .as_ref()
                        .is_some_and(|doc| doc["network"].is_object())
                {
                    body = body
                        .child(group)
                        .child(self.render_deerflow_network(theme, cx));
                    group = df_group(theme);
                }
            }
            body = body.child(group);
        } else if !state.fields.is_empty() {
            let mut group = df_group(theme);
            for field in &state.fields {
                if visible_field(state, field) {
                    group = group.child(self.render_deerflow_field(field, theme, cx));
                }
            }
            body = body.child(group);
        }
        if matches!(
            state.section,
            Section::Models | Section::Tools | Section::MCP
        ) {
            body = body.child(div().mt(px(12.0)).child(df_button(
                "df-more-parameters",
                if state.advanced {
                    "收起更多参数"
                } else {
                    "更多参数"
                },
                state.busy(),
                state.advanced,
                theme,
                cx,
                |this, _, cx| {
                    this.deerflow_settings.advanced = !this.deerflow_settings.advanced;
                    this.deerflow_settings.search_target = None;
                    this.deerflow_rebind(cx);
                    cx.notify();
                },
            )));
        }
        body.into_any_element()
    }

    pub(super) fn render_deerflow_skill_center(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let mut body = div();
        body = body.child(df_label(
            state
                .skill_browser
                .auto_patch
                .readiness_label
                .as_deref()
                .unwrap_or("技能自动进化 · 状态尚未加载"),
            if state.dirty || state.needs_apply {
                "配置有尚未应用的修改；实际维护状态以后台回执为准。"
            } else {
                "技能学习开启时，所有 DeerFlow 聊天自动贡献新的任务经验。后台使用主模型审查技能包的安全性，通过后自动发布。进度和结果可在进化记录中查看。"
            },
            theme,
        ));
        let mut tabs = div().mt(px(12.0)).flex().flex_wrap().gap(px(6.0));
        for (tab, label) in [
            (SkillCenterTab::Catalog, "我的技能"),
            (SkillCenterTab::History, "进化记录"),
            (SkillCenterTab::Review, "需要处理"),
        ] {
            tabs = tabs.child(df_button(
                SharedString::from(format!("df-center-{label}")),
                if tab == SkillCenterTab::Review {
                    state
                        .skill_browser
                        .auto_patch
                        .needs_attention
                        .map(|count| {
                            if count > 0 {
                                format!("需要处理 · {count}")
                            } else {
                                label.to_owned()
                            }
                        })
                        .unwrap_or_else(|| "需要处理 · 未加载".into())
                } else {
                    label.to_owned()
                },
                state.busy(),
                state.skill_center_tab == tab,
                theme,
                cx,
                move |this, _, cx| {
                    this.deerflow_settings.skill_center_tab = tab;
                    this.deerflow_settings.show_more = false;
                    this.settings_scroll.set_offset(gpui::Point::default());
                    this.deerflow_rebind(cx);
                    this.deerflow_queue_skill_center(cx);
                    cx.notify();
                },
            ));
        }
        body = body
            .child(tabs)
            .child(
                div()
                    .mt(px(12.0))
                    .flex()
                    .flex_wrap()
                    .gap(px(6.0))
                    .child(df_button(
                        "df-center-sources",
                        "管理来源",
                        state.busy(),
                        false,
                        theme,
                        cx,
                        |this, _, cx| {
                            this.open_settings_page(SettingsPage::DeerFlowAdvanced, cx);
                            this.deerflow_select_section(Section::Sources, cx);
                        },
                    )),
            );
        body = body.child(self.render_deerflow_skill_pending(theme, cx));
        match state.skill_center_tab {
            SkillCenterTab::Catalog => {
                body = body
                    .child(self.render_deerflow_skill_catalog(theme, cx))
                    .child(df_button(
                        "df-center-feedback",
                        if state.show_more {
                            "收起当前会话反馈"
                        } else {
                            "当前会话反馈与缺失报告"
                        },
                        state.busy(),
                        state.show_more,
                        theme,
                        cx,
                        |this, _, cx| {
                            this.deerflow_settings.show_more = !this.deerflow_settings.show_more;
                            cx.notify();
                        },
                    ));
                if state.show_more {
                    body = body
                        .child(self.render_deerflow_feedback(theme, cx))
                        .child(self.render_deerflow_missing(theme, cx));
                }
            }
            SkillCenterTab::Review => body = body.child(self.render_deerflow_proposals(theme, cx)),
            SkillCenterTab::History => {
                body = body
                    .child(self.render_deerflow_learning_overview(theme, cx))
                    .child(self.render_deerflow_proposals(theme, cx))
                    .child(self.render_deerflow_auto_patch(theme, cx))
                    .child(df_button(
                        "df-center-activity-details",
                        if state.show_more {
                            "收起学习与生成详情"
                        } else {
                            "学习与生成详情"
                        },
                        state.busy(),
                        state.show_more,
                        theme,
                        cx,
                        |this, _, cx| {
                            this.deerflow_settings.show_more = !this.deerflow_settings.show_more;
                            cx.notify();
                        },
                    ));
                if state.show_more {
                    body = body
                        .child(self.render_deerflow_generation(theme, cx))
                        .child(self.render_deerflow_discovery(theme, cx))
                        .child(self.render_deerflow_learning(theme, cx));
                }
            }
        }
        body.into_any_element()
    }
}

#[cfg(test)]
#[path = "presentation_interaction_tests.rs"]
mod interaction_tests;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn model_config_name_is_visible_without_more_parameters() {
        for key in [
            "name",
            "display_name",
            "provider",
            "api",
            "model",
            "base_url",
            "api_key",
            "clear_api_key",
        ] {
            assert!(
                model_field_visible(key, false),
                "{key} should be visible by default"
            );
            assert!(model_field_visible(key, true));
        }
        for key in [
            "description",
            "supports_vision",
            "supports_image_generation",
            "context_window",
            "reasoning_efforts",
            "thinking_parameter",
            "thinking_budget_tokens",
            "output_token_parameter",
        ] {
            assert!(
                !model_field_visible(key, false),
                "{key} should remain advanced"
            );
            assert!(model_field_visible(key, true));
        }
    }

    #[test]
    fn legacy_import_is_absent_from_settings_navigation_and_search() {
        assert_eq!(ADVANCED_GROUPS.len(), 5);
        assert!(
            ADVANCED_GROUPS
                .iter()
                .all(|(_, label, _)| !label.contains("旧数据"))
        );
        assert!(SEARCH_TARGETS.iter().all(|seed| seed.path != "/legacy"));
    }
}
