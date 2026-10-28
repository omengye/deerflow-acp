//! Presentation only: navigation, disclosure and search never define host grants.
use super::*;

#[derive(Clone, Copy, Default, PartialEq, Eq)]
pub(super) enum SkillCenterTab {
    #[default]
    Catalog,
    Review,
    Generation,
    History,
    Automation,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum EvolutionMode {
    Off,
    Manual,
    Discover,
    ReviewLearning,
    Automatic,
}

impl EvolutionMode {
    fn label(self) -> &'static str {
        match self {
            Self::Off => "关闭新维护",
            Self::Manual => "手动改进，审核发布",
            Self::Discover => "自动发现，审核发布",
            Self::ReviewLearning => "审核后采用",
            Self::Automatic => "自动维护（推荐）",
        }
    }

    fn description(self) -> &'static str {
        match self {
            Self::Off => {
                "关闭新的生成、学习、发现、评测和经验自动补丁工作；保留模型、来源、额度和历史。选择仅修改草稿，保存并应用后生效。"
            }
            Self::Manual => {
                "手动提交和生成候选，发布需人工审核；关闭自动发现、学习和经验自动补丁。模型、来源、额度和行为评测参数保留，保存并应用后生效。"
            }
            Self::Discover => {
                "通过纠正、缺失报告和成功流程自动发现候选，发布需人工审核；关闭经验学习和经验自动补丁。模型、来源、额度和行为评测参数保留，保存并应用后生效。"
            }
            Self::ReviewLearning => {
                "提取、归并和匹配经验，为已有技能自动起草；发布需人工审核，可能消耗维护模型额度。不回填旧聊天。另行配置的发现规则以高级设置为准；切换此方式时不启用跨会话共享、新技能发现或自动发布。选择仅修改草稿，保存并应用后生效。"
            }
            Self::Automatic => {
                "授权已配置来源中符合条件的已有受管技能自动采用小补丁，并执行安全、质量和可信行为评测。首次使用已预置维护模型继承、额度和宿主测试套件，无需逐项设置；没有对应覆盖的技能不会自动发布。单任务草案等待新证据，新技能、内置覆盖和超范围改动仍需确认。不扩大来源、跨会话共享或维护额度；保存并应用后生效。"
            }
        }
    }
}

const EVOLUTION_PATHS: &[&str] = &[
    "/skill_evolution/enabled",
    "/skill_evolution/mode",
    "/skill_evolution/generation/enabled",
    "/skill_evolution/discovery/operator_corrections_enabled",
    "/skill_evolution/discovery/missing_reports_enabled",
    "/skill_evolution/discovery/successful_workflows_enabled",
    "/skill_evolution/learning/enabled",
    "/skill_evolution/learning/consolidation_enabled",
    "/skill_evolution/learning/matching_enabled",
    "/skill_evolution/discovery/experience_patterns_enabled",
    "/skill_evolution/discovery/experience_review_drafts_enabled",
    "/skill_evolution/discovery/experience_new_skills_enabled",
    "/skill_evolution/learning/cross_session_enabled",
    "/skill_evolution/auto_patch/experience_patterns_enabled",
    "/skill_evolution/behavior/enabled",
];

fn mode_paths(mode: EvolutionMode) -> &'static [&'static str] {
    match mode {
        EvolutionMode::Off | EvolutionMode::Automatic => EVOLUTION_PATHS,
        EvolutionMode::Manual | EvolutionMode::Discover | EvolutionMode::ReviewLearning => {
            &EVOLUTION_PATHS[..14]
        }
    }
}

pub(super) fn evolution_mode(doc: &Value) -> Option<EvolutionMode> {
    let e = &doc["skill_evolution"];
    if e["enabled"] == false {
        return Some(EvolutionMode::Off);
    }
    if e["enabled"] != true || e["generation"]["enabled"] != true {
        return None;
    }
    if e["mode"] == "auto_patch" {
        return (EVOLUTION_PATHS[6..11]
            .iter()
            .all(|path| doc.pointer(path) == Some(&json!(true)))
            && e["auto_patch"]["experience_patterns_enabled"] == true
            && e["behavior"]["enabled"] == true)
            .then_some(EvolutionMode::Automatic);
    }
    if e["mode"] != "review" {
        return None;
    }
    // Extra correction/report/workflow triggers do not change the review-draft
    // workflow. Recognize the current mode without rewriting those settings.
    if EVOLUTION_PATHS[6..11]
        .iter()
        .all(|path| doc.pointer(path).and_then(Value::as_bool) == Some(true))
        && EVOLUTION_PATHS[11..14]
            .iter()
            .all(|path| doc.pointer(path).and_then(Value::as_bool) == Some(false))
    {
        return Some(EvolutionMode::ReviewLearning);
    }
    if EVOLUTION_PATHS[6..14]
        .iter()
        .any(|path| doc.pointer(path).and_then(Value::as_bool) == Some(true))
    {
        return None;
    }
    let flags = [
        "operator_corrections_enabled",
        "missing_reports_enabled",
        "successful_workflows_enabled",
    ]
    .map(|key| e["discovery"][key].as_bool());
    if flags == [Some(false); 3] {
        Some(EvolutionMode::Manual)
    } else if flags == [Some(true); 3] {
        Some(EvolutionMode::Discover)
    } else {
        None
    }
}

fn set_evolution_mode(doc: &mut Value, mode: EvolutionMode) {
    // Only the selected preset's flags change. Models, sources, limits and
    // custom behavior settings remain in the draft for the host to validate.
    set_value(doc, EVOLUTION_PATHS[0], json!(mode != EvolutionMode::Off));
    if mode == EvolutionMode::Off {
        for path in &EVOLUTION_PATHS[2..] {
            set_value(doc, path, json!(false));
        }
        return;
    }
    if mode == EvolutionMode::Automatic {
        set_value(doc, EVOLUTION_PATHS[1], json!("auto_patch"));
        for index in [2, 6, 7, 8, 9, 10, 13, 14] {
            set_value(doc, EVOLUTION_PATHS[index], json!(true));
        }
        return;
    }
    set_value(doc, EVOLUTION_PATHS[1], json!("review"));
    set_value(doc, EVOLUTION_PATHS[2], json!(true));
    for path in &EVOLUTION_PATHS[3..6] {
        set_value(doc, path, json!(mode == EvolutionMode::Discover));
    }
    for path in &EVOLUTION_PATHS[6..11] {
        set_value(doc, path, json!(mode == EvolutionMode::ReviewLearning));
    }
    for path in &EVOLUTION_PATHS[11..14] {
        set_value(doc, path, json!(false));
    }
}

fn select_evolution_mode(doc: &mut Value, mode: EvolutionMode) -> bool {
    if evolution_mode(doc) == Some(mode) {
        return false;
    }
    set_evolution_mode(doc, mode);
    true
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
    (
        Section::Skills,
        "技能维护",
        "发现、生成、评测、额度与发布规则",
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

const SKILL_GROUPS: &[(&str, &str)] = &[
    ("general", "模型与发布方式"),
    ("generation", "候选生成"),
    ("discovery", "自动发现"),
    ("budget", "维护额度"),
    ("validation", "安全与质量审查"),
    ("behavior", "行为评测"),
    ("auto_patch", "受限自动补丁"),
];

// Only the outer page mounts form fields. Child pages supply their non-form
// content so GPUI never shares interaction state between duplicate field IDs.
pub(super) fn compose_settings_content(
    section: Section,
    tab: SkillCenterTab,
    content: Div,
    form_fields: impl FnOnce() -> AnyElement,
) -> Div {
    match (section, tab) {
        (Section::Common, _) => div().child(form_fields()).child(content),
        (Section::SkillCenter, tab) if tab != SkillCenterTab::Automation => content,
        _ => content.child(form_fields()),
    }
}

impl DeerFlowSettings {
    fn skill_group_selected(&self, key: &str) -> bool {
        self.search_target
            .as_deref()
            .is_some_and(|path| skill_group(path) == key)
    }

    fn toggle_skill_group(&mut self, key: &str) {
        self.search_target = if self.skill_group_selected(key) {
            None
        } else {
            Some(format!("/skill_evolution/{key}"))
        };
    }
}

fn render_skill_groups<E: 'static>(
    state: &DeerFlowSettings,
    theme: Theme,
    cx: &mut Context<E>,
    toggle: impl Fn(&mut E, &'static str, &mut Context<E>) + Copy + 'static,
    render_field: impl Fn(&Field, &mut Context<E>) -> AnyElement,
) -> Div {
    let mut body = div();
    for &(key, label) in SKILL_GROUPS {
        let selected = state.skill_group_selected(key);
        let mut group = df_group(theme).child(
            df_button(
                SharedString::from(format!("df-skill-options-{key}")),
                format!("{} {label}", if selected { "▾" } else { "▸" }),
                // Disclosure is read-only; field editors retain their own gates.
                false,
                selected,
                theme,
                cx,
                move |this, _, cx| toggle(this, key, cx),
            )
            .debug_selector(|| format!("df-skill-options-{key}"))
            .w_full()
            .justify_start(),
        );
        for field in &state.fields {
            if skill_group(&field.path) == key
                && (selected || state.errors.contains_key(&field.path))
            {
                group = group.child(render_field(field, cx));
            }
        }
        body = body.child(group);
    }
    body
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
    if doc["skill_evolution"]["enabled"] == true {
        fields.push(Field::new(
            "/skill_evolution/budget/max_daily_model_calls",
            "每日维护模型调用上限",
            "按 UTC 日计入生成与审查；这是上限，不是剩余额度",
            Kind::Integer,
        ));
    }
    fields
}

fn model_field_visible(key: &str, advanced: bool) -> bool {
    advanced
        || matches!(
            key,
            "name"
                | "display_name"
                | "provider"
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
        "技能改进方式",
        "/skill_evolution/enabled",
        Common,
        "evolution 自进化 改进 审核"
    ),
    target!(
        "每日维护调用额度",
        "/skill_evolution/budget/max_daily_model_calls",
        Skills,
        "budget 费用 额度 预算 每日"
    ),
    target!(
        "模型 API 地址",
        "/models/*/base_url",
        Models,
        "endpoint api base url 模型 地址"
    ),
    target!("模型密钥", "/models/*/api_key", Models, "api key 密钥 模型"),
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
        "技能维护模型",
        "/skill_evolution/generation_model_name",
        Skills,
        "generation model 技能 生成 模型"
    ),
    target!(
        "自动发现规则",
        "/skill_evolution/discovery/repeat_threshold",
        Skills,
        "discovery 自动 发现 纠正 流程"
    ),
    target!(
        "维护 Token 预算",
        "/skill_evolution/budget/max_daily_tokens",
        Skills,
        "token budget 维护 预算"
    ),
    target!(
        "行为评测套件",
        "/skill_evolution/behavior/suite_path",
        Skills,
        "behavior suite 行为 评测 测试 套件"
    ),
    target!(
        "自动补丁发布",
        "/skill_evolution/mode",
        Skills,
        "auto patch 自动 补丁 发布"
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
        "会话保留天数",
        "/runtime/inactive_session_retention_days",
        Runtime,
        "retention cleanup 会话 保留 清理"
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
        Section::Skills,
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
    fn deerflow_select_evolution_mode(&mut self, mode: EvolutionMode, cx: &mut Context<Self>) {
        let state = &mut self.deerflow_settings;
        if !state.can_edit()
            || !mode_paths(mode)
                .iter()
                .all(|path| state.can_edit_field(path))
        {
            return;
        }
        let Some(doc) = state.draft.as_mut() else {
            return;
        };
        if !select_evolution_mode(doc, mode) {
            return;
        }
        state.dirty = true;
        state.edit_generation += 1;
        state.confirm_reload = false;
        state.notice = Some("方式已加入草稿，保存并应用后生效；可撤销更改。".into());
        for path in mode_paths(mode) {
            state.errors.remove(*path);
            state.invalid_text.remove(*path);
        }
        self.deerflow_rebind(cx);
        cx.notify();
    }

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

    pub(in crate::app) fn deerflow_find_setting(
        &mut self,
        target: SearchTarget,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
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
        let doc = state.draft.as_ref().unwrap_or(&Value::Null);
        let current = evolution_mode(doc);
        let mut group = df_group(theme)
            .id("df-common-evolution")
            .anchor_scroll(state.field_anchors.get("/skill_evolution/enabled").cloned())
            .child(df_label(
                "技能改进方式",
                "切换仅修改草稿，保存并应用后授权生效。自动维护只采用通过可信评测的已有技能小改动，不增加额度或权限。",
                theme,
            ));
        let mut choices = div().flex().flex_wrap().gap(px(6.0));
        for mode in [
            EvolutionMode::Automatic,
            EvolutionMode::ReviewLearning,
            EvolutionMode::Off,
        ] {
            let selected = current == Some(mode);
            let editable = state.can_edit()
                && mode_paths(mode)
                    .iter()
                    .all(|path| state.can_edit_field(path));
            choices = choices.child(df_button(
                SharedString::from(format!("df-evolution-mode-{}", mode.label())),
                mode.label(),
                !editable,
                selected,
                theme,
                cx,
                move |this, _, cx| this.deerflow_select_evolution_mode(mode, cx),
            ));
        }
        group = group.child(choices);
        if current.is_none() {
            group = group.child(df_label(
                "当前为自定义方式",
                "现有开关保持不变，可在高级设置中分别调整。",
                theme,
            ));
        }
        if let Some(mode) = current {
            group = group.child(df_label("方式说明", mode.description(), theme));
        }
        let mut links = div().flex().flex_wrap().gap(px(6.0));
        for (section, label) in [
            (Section::Agents, "管理角色"),
            (Section::Memory, "管理记忆"),
            (Section::Skills, "维护规则与预算"),
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
            "打开技能中心",
            state.busy(),
            false,
            theme,
            cx,
            |this, _, cx| this.open_deerflow_skill_center(cx),
        ));
        div()
            .child(df_group(theme).child(df_label(
                "首次使用只需配置模型",
                "在“模型”中填写服务地址、模型 ID 和密钥并保存应用，即可开始使用。新配置已预置记忆、子代理、技能学习、自动维护及预算；维护调用可能产生费用，可随时暂停。可信评测覆盖外的技能和新技能仍需审核；搜索密钥及浏览器依赖为可选配置，不影响基本聊天。已有配置保持原样。",
                theme,
            )))
            .child(self.render_deerflow_readiness(theme, cx))
            .child(
                group
                    .child(df_label(
                        "学习前置条件与排查",
                        learning_setup_hint(doc),
                        theme,
                    ))
                    .child(links),
            )
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
        if state.section == Section::Skills
            || state.section == Section::SkillCenter
                && state.skill_center_tab == SkillCenterTab::Automation
        {
            body = render_skill_groups(
                state,
                theme,
                cx,
                |this: &mut Self, key, cx| {
                    this.deerflow_settings.toggle_skill_group(key);
                    cx.notify();
                },
                |field, cx| self.render_deerflow_field(field, theme, cx),
            );
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
                .unwrap_or("维护状态尚未加载"),
            if state.dirty || state.needs_apply {
                "配置有尚未应用的修改；实际维护状态以后台回执为准。"
            } else {
                "查看当前技能与进化结果。新技能和超范围改动仍需确认；正常等待不作为故障。"
            },
            theme,
        ));
        let mut tabs = div().mt(px(12.0)).flex().flex_wrap().gap(px(6.0));
        for (tab, label) in [
            (SkillCenterTab::Catalog, "我的技能"),
            (SkillCenterTab::History, "进化记录"),
            (SkillCenterTab::Review, "需要处理"),
            (SkillCenterTab::Automation, "自动化设置"),
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
        body = body.child(tabs).child(
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
                ))
                .child(df_button(
                    "df-center-rules",
                    "自动化设置",
                    state.busy(),
                    false,
                    theme,
                    cx,
                    |this, _, cx| {
                        this.deerflow_settings.skill_center_tab = SkillCenterTab::Automation;
                        this.deerflow_rebind(cx);
                        cx.notify();
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
            SkillCenterTab::Generation => {
                body = body
                    .child(self.render_deerflow_generation(theme, cx))
                    .child(df_button(
                        "df-center-discovery",
                        if state.show_more {
                            "收起自动发现记录"
                        } else {
                            "查看自动发现记录"
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
                    body = body.child(self.render_deerflow_discovery(theme, cx));
                }
            }
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
            SkillCenterTab::Automation => {
                let doc = state.draft.as_ref().unwrap_or(&Value::Null);
                let e = &doc["skill_evolution"];
                body = body.child(self.render_deerflow_common(theme, cx))
                    .child(df_group(theme).child(df_label("额度（编辑草稿）", format!("维护每日 {} 次 / {} Token · 学习每日 {} 次 / {} Token（包含于总额度）\n并发上限 {} · 单作业 {} 秒。实际生效配置以保存并应用回执为准；额度按 UTC 日重置。",
                        e["budget"]["max_daily_model_calls"], e["budget"]["max_daily_tokens"], e["learning"]["max_daily_model_calls"], e["learning"]["max_daily_tokens"], e["budget"]["workers"], e["budget"]["max_job_seconds"]), theme)))
                    .child(self.render_deerflow_auto_patch(theme, cx));
            }
        }
        body.into_any_element()
    }
}

fn learning_setup_hint(doc: &Value) -> String {
    let e = &doc["skill_evolution"];
    let budget_ok = [
        "max_daily_model_calls",
        "max_daily_tokens",
        "max_job_seconds",
        "workers",
    ]
    .iter()
    .all(|key| e["budget"][key].as_u64().is_some_and(|n| n > 0))
        && ["max_daily_model_calls", "max_daily_tokens"]
            .iter()
            .all(|key| e["learning"][key].as_u64().is_some_and(|n| n > 0));
    format!(
        "草稿检查：技能系统 {}；维护与学习额度 {}。\n先确认模型和可维护的技能来源，再保存并应用。此方式只改进已匹配的现有技能；只有内置技能、来源不可维护或没有匹配时，不保证产生草稿。\n在“进化记录 > 学习与生成详情”核对运行阶段、聊天贡献、等待或失败原因；需要人工决策的候选进入“需要处理”。未出现提案不等于故障，也不代表任务已完成。",
        if doc["skills_enabled"] == true {
            "已启用"
        } else {
            "未启用，请在维护规则中开启"
        },
        if budget_ok {
            "已填写正数，剩余额度以运行服务为准"
        } else {
            "不完整，请检查预算和学习额度；不会自动增加额度"
        },
    )
}

fn skill_group(path: &str) -> &str {
    let key = path
        .strip_prefix("/skill_evolution/")
        .unwrap_or("general")
        .split('/')
        .next()
        .unwrap_or("general");
    if SKILL_GROUPS.iter().any(|(group, _)| *group == key) {
        key
    } else {
        "general"
    }
}

#[cfg(test)]
#[path = "presentation_interaction_tests.rs"]
mod interaction_tests;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn automatic_maintenance_preserves_budget_sources_and_consent() {
        let mut doc = json!({"skill_sources":{"managed":[{"id":"chosen"}]}, "skill_evolution":{
            "enabled":true,"mode":"review","generation":{},"learning":{"max_daily_model_calls":50,"cross_session_enabled":false},
            "discovery":{"experience_new_skills_enabled":false,"repeat_threshold":2},
            "auto_patch":{},"behavior":{"suite_path":"trusted-registry.json"},"budget":{"max_daily_model_calls":1000}
        }});
        let old = doc.clone();
        set_evolution_mode(&mut doc, EvolutionMode::Automatic);
        assert_eq!(evolution_mode(&doc), Some(EvolutionMode::Automatic));
        assert_eq!(doc["skill_sources"], old["skill_sources"]);
        assert_eq!(
            doc["skill_evolution"]["budget"],
            old["skill_evolution"]["budget"]
        );
        assert_eq!(
            doc["skill_evolution"]["learning"]["max_daily_model_calls"],
            50
        );
        assert_eq!(
            doc["skill_evolution"]["learning"]["cross_session_enabled"],
            false
        );
        assert_eq!(
            doc["skill_evolution"]["discovery"]["experience_new_skills_enabled"],
            false
        );
        assert_eq!(
            doc["skill_evolution"]["behavior"]["suite_path"],
            "trusted-registry.json"
        );
        let same = doc.clone();
        assert!(!select_evolution_mode(&mut doc, EvolutionMode::Automatic));
        assert_eq!(doc, same);
        set_evolution_mode(&mut doc, EvolutionMode::ReviewLearning);
        assert_eq!(evolution_mode(&doc), Some(EvolutionMode::ReviewLearning));
        assert_eq!(
            doc["skill_evolution"]["auto_patch"]["experience_patterns_enabled"],
            false
        );
    }

    #[test]
    fn model_config_name_is_visible_without_more_parameters() {
        for key in [
            "name",
            "display_name",
            "provider",
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

    fn evolution_document() -> Value {
        json!({"skill_evolution": {
            "generation": {}, "learning": {}, "discovery": {},
            "auto_patch": {}, "behavior": {}
        }})
    }

    #[test]
    fn selecting_each_mode_updates_the_draft_and_selected_highlight_together() {
        let modes = [
            EvolutionMode::Off,
            EvolutionMode::Manual,
            EvolutionMode::Discover,
            EvolutionMode::ReviewLearning,
        ];
        for from in modes {
            for to in modes {
                let mut saved = evolution_document();
                set_evolution_mode(&mut saved, from);
                let mut draft = saved.clone();
                assert_eq!(select_evolution_mode(&mut draft, to), from != to);
                assert_eq!(evolution_mode(&draft), Some(to), "{from:?} -> {to:?}");
                assert_eq!(evolution_mode(&saved), Some(from));
                if from != to {
                    assert_ne!(draft, saved);
                }
            }
        }
    }

    #[test]
    fn empty_and_custom_drafts_do_not_highlight_a_default_mode() {
        for doc in [Value::Null, json!({}), evolution_document()] {
            assert_eq!(evolution_mode(&doc), None);
        }

        let mut doc = evolution_document();
        set_evolution_mode(&mut doc, EvolutionMode::Manual);
        set_value(&mut doc, EVOLUTION_PATHS[3], json!(true));
        let original = doc.clone();
        assert_eq!(evolution_mode(&doc), None);
        assert_eq!(doc, original);

        assert!(select_evolution_mode(&mut doc, EvolutionMode::Discover));
        assert_eq!(evolution_mode(&doc), Some(EvolutionMode::Discover));
    }

    #[test]
    fn restoring_saved_draft_restores_mode_selection_without_confirmation() {
        let modes = [
            EvolutionMode::Off,
            EvolutionMode::Manual,
            EvolutionMode::Discover,
            EvolutionMode::ReviewLearning,
        ];
        for saved_mode in modes {
            let mut saved = evolution_document();
            set_evolution_mode(&mut saved, saved_mode);
            for selected_mode in modes {
                let mut draft = saved.clone();
                select_evolution_mode(&mut draft, selected_mode);
                assert_eq!(evolution_mode(&draft), Some(selected_mode));

                draft = saved.clone();
                assert_eq!(evolution_mode(&draft), Some(saved_mode));
                assert!(!select_evolution_mode(&mut draft, saved_mode));
                assert_eq!(draft, saved);
            }
        }
    }

    #[test]
    fn selecting_the_current_mode_keeps_advanced_choices_and_draft_unchanged() {
        let mut doc = evolution_document();
        set_evolution_mode(&mut doc, EvolutionMode::ReviewLearning);
        for enabled in 0..8 {
            for (index, path) in EVOLUTION_PATHS[3..6].iter().enumerate() {
                set_value(&mut doc, path, json!(enabled & (1 << index) != 0));
            }
            let original = doc.clone();
            assert!(!select_evolution_mode(
                &mut doc,
                EvolutionMode::ReviewLearning
            ));
            assert_eq!(doc, original);
        }
        set_evolution_mode(&mut doc, EvolutionMode::Off);
        set_value(&mut doc, EVOLUTION_PATHS[3], json!(true));
        let original = doc.clone();
        assert!(!select_evolution_mode(&mut doc, EvolutionMode::Off));
        assert_eq!(doc, original);
    }

    #[test]
    fn selecting_manual_or_discover_resolves_partial_learning_settings() {
        for mode in [EvolutionMode::Manual, EvolutionMode::Discover] {
            for path in &EVOLUTION_PATHS[6..14] {
                let mut doc = evolution_document();
                set_evolution_mode(&mut doc, mode);
                set_value(&mut doc, path, json!(true));
                assert_eq!(evolution_mode(&doc), None);
                assert!(select_evolution_mode(&mut doc, mode));
                assert_eq!(evolution_mode(&doc), Some(mode));
                for path in &EVOLUTION_PATHS[6..14] {
                    assert_eq!(doc.pointer(path), Some(&json!(false)));
                }
            }
        }
    }

    #[test]
    fn mode_selection_changes_only_capability_checked_flags() {
        let mut original = evolution_document();
        set_evolution_mode(&mut original, EvolutionMode::ReviewLearning);
        original["models"] = json!([{"name":"keep-model","api_key":"$MODEL_KEY"}]);
        original["skill_sources"] = json!({"managed":[{"id":"keep-source"}]});
        original["skill_evolution"]["mode"] = json!("auto_patch");
        original["skill_evolution"]["budget"] = json!({"max_daily_model_calls":3});
        original["skill_evolution"]["learning"]["max_daily_tokens"] = json!(123);
        original["skill_evolution"]["discovery"]["new_skill_source_id"] = json!("keep-source");
        original["skill_evolution"]["behavior"] = json!({"enabled":true,"suite_path":"keep-suite"});
        for mode in [
            EvolutionMode::Off,
            EvolutionMode::Manual,
            EvolutionMode::Discover,
            EvolutionMode::ReviewLearning,
        ] {
            let mut draft = original.clone();
            assert!(select_evolution_mode(&mut draft, mode));
            assert_eq!(evolution_mode(&draft), Some(mode));
            for path in mode_paths(mode) {
                set_value(&mut draft, path, original.pointer(path).unwrap().clone());
            }
            assert_eq!(draft, original, "{mode:?} changed an unrelated setting");
        }
    }

    #[test]
    fn review_learning_mode_accepts_additional_discovery_triggers_without_mutation() {
        let mut doc = evolution_document();
        set_evolution_mode(&mut doc, EvolutionMode::ReviewLearning);
        for enabled in 0..8 {
            for (index, path) in EVOLUTION_PATHS[3..6].iter().enumerate() {
                set_value(&mut doc, path, json!(enabled & (1 << index) != 0));
            }
            let original = doc.clone();
            assert_eq!(evolution_mode(&doc), Some(EvolutionMode::ReviewLearning));
            assert_eq!(doc, original);
            let reloaded: Value = serde_json::from_str(&doc.to_string()).unwrap();
            assert_eq!(
                evolution_mode(&reloaded),
                Some(EvolutionMode::ReviewLearning)
            );
        }
    }

    #[test]
    fn review_learning_mode_keeps_other_workflows_distinct() {
        let mut doc = evolution_document();
        set_evolution_mode(&mut doc, EvolutionMode::ReviewLearning);
        for path in &EVOLUTION_PATHS[6..11] {
            let mut changed = doc.clone();
            set_value(&mut changed, path, json!(false));
            assert_ne!(
                evolution_mode(&changed),
                Some(EvolutionMode::ReviewLearning)
            );
        }
        for path in &EVOLUTION_PATHS[11..14] {
            let mut changed = doc.clone();
            set_value(&mut changed, path, json!(true));
            assert_ne!(
                evolution_mode(&changed),
                Some(EvolutionMode::ReviewLearning)
            );
        }
        doc["skill_evolution"]["mode"] = json!("auto_patch");
        assert_eq!(evolution_mode(&doc), None);
        for mode in [
            EvolutionMode::Off,
            EvolutionMode::Manual,
            EvolutionMode::Discover,
        ] {
            let mut doc = evolution_document();
            set_evolution_mode(&mut doc, mode);
            assert_eq!(evolution_mode(&doc), Some(mode));
        }
    }

    #[test]
    fn legacy_import_is_absent_from_settings_navigation_and_search() {
        assert_eq!(ADVANCED_GROUPS.len(), 6);
        assert!(
            ADVANCED_GROUPS
                .iter()
                .all(|(_, label, _)| !label.contains("旧数据"))
        );
        assert!(SEARCH_TARGETS.iter().all(|seed| seed.path != "/legacy"));
    }

    #[test]
    fn review_learning_preset_is_bounded_and_reversible() {
        let mut doc = json!({"skills_enabled":true,"skill_sources":{"managed":[{"id":"keep"}]},"skill_evolution":{"mode":"auto_patch","generation":{"enabled":false},"behavior":{"enabled":true},"generation_model_name":"keep-model","budget":{"max_daily_model_calls":3},"learning":{"cross_session_enabled":true,"max_daily_tokens":123},"discovery":{"new_skill_source_id":"keep-source","experience_new_skills_enabled":true},"auto_patch":{"experience_patterns_enabled":true}}});
        let sources = doc["skill_sources"].clone();
        let budget = doc["skill_evolution"]["budget"].clone();
        set_evolution_mode(&mut doc, EvolutionMode::ReviewLearning);
        assert!(evolution_mode(&doc) == Some(EvolutionMode::ReviewLearning));
        assert_eq!(doc["skill_sources"], sources);
        assert_eq!(doc["skill_evolution"]["budget"], budget);
        assert_eq!(doc["skill_evolution"]["learning"]["max_daily_tokens"], 123);
        assert_eq!(
            doc["skill_evolution"]["generation_model_name"],
            "keep-model"
        );
        assert_eq!(doc["skill_evolution"]["behavior"]["enabled"], true);
        assert_eq!(
            doc["skill_evolution"]["discovery"]["new_skill_source_id"],
            "keep-source"
        );
        assert!(learning_setup_hint(&doc).contains("不会自动增加额度"));
        set_evolution_mode(&mut doc, EvolutionMode::Off);
        for path in &EVOLUTION_PATHS[2..] {
            assert_eq!(doc.pointer(path), Some(&json!(false)));
        }
        assert_eq!(doc["skill_evolution"]["budget"], budget);
    }
    #[test]
    fn settings_mode_changes_preserve_limits_models_and_custom_discovery() {
        let mut doc = json!({"skill_evolution":{"enabled":true,"mode":"review","generation":{"enabled":true,"max_output_tokens":4096},"discovery":{"operator_corrections_enabled":true,"missing_reports_enabled":false,"successful_workflows_enabled":true,"new_skill_source_id":"managed"},"budget":{"max_daily_model_calls":8},"generation_model_name":"model","behavior":{"enabled":false}}});
        assert_eq!(evolution_mode(&doc).map(|m| m.label()), None);
        let budget = doc["skill_evolution"]["budget"].clone();
        set_evolution_mode(&mut doc, EvolutionMode::Manual);
        assert!(evolution_mode(&doc) == Some(EvolutionMode::Manual));
        assert_eq!(doc["skill_evolution"]["budget"], budget);
        assert_eq!(doc["skill_evolution"]["generation_model_name"], "model");
        assert_eq!(
            doc["skill_evolution"]["discovery"]["new_skill_source_id"],
            "managed"
        );
        set_evolution_mode(&mut doc, EvolutionMode::Off);
        assert_eq!(
            doc["skill_evolution"]["generation"]["max_output_tokens"],
            4096
        );
        assert_eq!(doc["skill_evolution"]["budget"], budget);
    }
}
