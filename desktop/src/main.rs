#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod portable;
mod preferences;
use preferences::{model_choices, model_label, selected_model_choice};
mod ui;

use iced::widget::{
    button, checkbox, column, container, pick_list, row, rule, scrollable, text, text_editor,
    text_input,
};
use iced::{Element, Fill, Length, Size, Subscription, Task, window};
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};
use std::env;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

const PORTABLE_LOCAL_SANDBOX_PROVIDER: &str = "deerflow.sandbox.local:LocalSandboxProvider";
const PORTABLE_LOCAL_SANDBOX_OPTIONS: &[&str] = &[
    "mounts",
    "bash_output_max_chars",
    "read_file_output_max_chars",
    "ls_output_max_chars",
];

fn main() -> iced::Result {
    configure_graphics_backend();

    iced::application(App::new, App::update, App::view)
        .title("DeerFlow Config")
        .theme(ui::app_theme())
        .subscription(App::subscription)
        .window(window::Settings {
            size: Size::new(1180.0, 760.0),
            min_size: Some(Size::new(860.0, 600.0)),
            exit_on_close_request: false,
            icon: Some(app_icon()),
            ..window::Settings::default()
        })
        .run()
}

fn app_icon() -> window::Icon {
    const ICON_SIZE: u32 = 256;
    const ICON_RGBA: &[u8] = include_bytes!("../assets/deerflow_icon_256.rgba");

    window::icon::from_rgba(ICON_RGBA.to_vec(), ICON_SIZE, ICON_SIZE)
        .expect("embedded DeerFlow icon must be valid 256x256 RGBA data")
}

fn configure_graphics_backend() {
    #[cfg(target_os = "windows")]
    if env::var_os("WGPU_BACKEND").is_none() {
        // SAFETY: This runs at the very beginning of `main`, before Iced starts
        // its event loop or worker threads, so no other thread can read the
        // process environment concurrently.
        unsafe {
            env::set_var("WGPU_BACKEND", "dx12");
        }
    }
}

#[derive(Debug, Clone, Copy, Default, Eq, PartialEq)]
enum Page {
    #[default]
    Dashboard,
    Models,
    Agents,
    Memory,
    Skills,
    SandboxTools,
    Runtime,
    Diagnostics,
    Data,
}

impl Page {
    const ALL: [(Self, &'static str); 9] = [
        (Self::Dashboard, "概览"),
        (Self::Models, "模型"),
        (Self::Agents, "智能体"),
        (Self::Memory, "长期记忆"),
        (Self::Skills, "技能"),
        (Self::SandboxTools, "工具与权限"),
        (Self::Runtime, "ACP 运行设置"),
        (Self::Diagnostics, "诊断"),
        (Self::Data, "数据与恢复"),
    ];

    fn title(self) -> &'static str {
        match self {
            Self::Dashboard => "概览",
            Self::Models => "模型配置",
            Self::Agents => "Agent 配置",
            Self::Memory => "长期记忆",
            Self::Skills => "技能与自进化",
            Self::SandboxTools => "工具与权限",
            Self::Runtime => "ACP 运行设置",
            Self::Diagnostics => "诊断",
            Self::Data => "数据与恢复",
        }
    }

    fn description(self) -> &'static str {
        match self {
            Self::Dashboard => "运行状态、资源概览与 Daemon 生命周期管理",
            Self::Models => "管理模型 Provider、访问凭据与能力声明",
            Self::Agents => "配置任务型 Subagent 与可选的主 Agent",
            Self::Memory => "配置本地 DeerMem 的提取、召回、检索与持久化",
            Self::Skills => "管理 Skill、改进候选和自进化安全边界",
            Self::SandboxTools => "控制执行边界、本地工具与 ACP 工具策略",
            Self::Runtime => "设置 ACP 会话的模型、权限与并发限制",
            Self::Diagnostics => "检查便携目录、运行时和关键文件位置",
            Self::Data => "备份恢复、会话清理、记忆与自进化历史",
        }
    }

    fn icon(self) -> ui::Icon {
        match self {
            Self::Dashboard => ui::Icon::Dashboard,
            Self::Models => ui::Icon::Models,
            Self::Agents => ui::Icon::Agents,
            Self::Memory => ui::Icon::Memory,
            Self::Skills => ui::Icon::Skills,
            Self::SandboxTools => ui::Icon::Tools,
            Self::Runtime => ui::Icon::Runtime,
            Self::Data => ui::Icon::Folder,
            Self::Diagnostics => ui::Icon::Diagnostics,
        }
    }
}

#[derive(Debug, Clone, Copy, Default, Eq, PartialEq)]
enum AgentSection {
    #[default]
    Subagents,
    CustomAgents,
}

#[derive(Debug, Clone, Copy, Default, Eq, PartialEq)]
enum SkillsSection {
    #[default]
    Proposals,
    Evolution,
    Catalog,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
struct ConfigDocument {
    config_revision: String,
    extensions_revision: String,
    default_model: String,
    models: Vec<ModelDocument>,
    runtime: RuntimeDocument,
    memory: MemoryDocument,
    agents: Vec<AgentDocument>,
    subagents: SubagentsDocument,
    sandbox: SandboxDocument,
    tool_groups: Vec<Map<String, Value>>,
    tools: Vec<Map<String, Value>>,
    skills_enabled: bool,
    skills: Vec<SkillDocument>,
    skill_evolution: SkillEvolutionDocument,
    paths: ProductDataPaths,
    #[serde(skip)]
    tool_groups_editor: text_editor::Content,
    #[serde(skip)]
    tools_editor: text_editor::Content,
    #[serde(skip)]
    portable_sandbox_migration_pending: bool,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
struct ModelDocument {
    original_name: String,
    name: String,
    display_name: String,
    description: String,
    use_path: String,
    model: String,
    api_key: String,
    api_key_configured: bool,
    api_key_literal: bool,
    clear_api_key: bool,
    base_url: String,
    supports_thinking: bool,
    supports_reasoning_effort: bool,
    supports_vision: bool,
    advanced: Map<String, Value>,
}

impl Default for ModelDocument {
    fn default() -> Self {
        Self {
            original_name: String::new(),
            name: "new-model".into(),
            display_name: String::new(),
            description: String::new(),
            use_path: "langchain_openai:ChatOpenAI".into(),
            model: String::new(),
            api_key: String::new(),
            api_key_configured: false,
            api_key_literal: false,
            clear_api_key: false,
            base_url: String::new(),
            supports_thinking: false,
            supports_reasoning_effort: false,
            supports_vision: false,
            advanced: Map::new(),
        }
    }
}

#[derive(Debug, Clone, Deserialize, Serialize)]
struct RuntimeDocument {
    model_name: Option<String>,
    agent_name: Option<String>,
    thinking_enabled: bool,
    plan_mode: bool,
    subagent_enabled: bool,
    max_concurrent_subagents: u32,
    max_active_connections: u32,
    max_active_runs: u32,
    run_timeout_seconds: f64,
    permission_mode: String,
    memory_scope: String,
    enable_bash: bool,
    tool_allowlist: Option<Vec<String>>,
    tool_denylist: Vec<String>,
    prompt_overlay: String,
    #[serde(default, flatten)]
    extra: Map<String, Value>,
    #[serde(skip)]
    extra_editor: text_editor::Content,
    #[serde(skip)]
    overlay_editor: text_editor::Content,
    #[serde(skip)]
    connections_input: String,
    #[serde(skip)]
    runs_input: String,
    #[serde(skip)]
    subagent_count_input: String,
    #[serde(skip)]
    timeout_input: String,
    #[serde(skip)]
    tool_allowlist_input: String,
    #[serde(skip)]
    tool_denylist_input: String,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
struct MemoryDocument {
    enabled: bool,
    manager_class: String,
    mode: String,
    injection_enabled: bool,
    shutdown_flush_timeout_seconds: f64,
    storage_path: String,
    storage_class: String,
    debounce_seconds: u32,
    model_name: Option<String>,
    max_facts: u32,
    fact_confidence_threshold: f64,
    max_injection_tokens: u32,
    retrieval_enabled: bool,
    retrieval_top_k: u32,
    retrieval_index_path: String,
    advanced: Map<String, Value>,
    backend_advanced: Map<String, Value>,
    #[serde(skip)]
    shutdown_flush_input: String,
    #[serde(skip)]
    debounce_input: String,
    #[serde(skip)]
    max_facts_input: String,
    #[serde(skip)]
    confidence_input: String,
    #[serde(skip)]
    max_tokens_input: String,
    #[serde(skip)]
    retrieval_top_k_input: String,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
struct AgentDocument {
    original_name: String,
    name: String,
    description: String,
    model: Option<String>,
    tool_groups: Vec<String>,
    skills: Option<Vec<String>>,
    soul: String,
    #[serde(default)]
    invalid: bool,
    #[serde(skip)]
    tool_groups_input: String,
    #[serde(skip)]
    skills_input: String,
}

impl Default for AgentDocument {
    fn default() -> Self {
        Self {
            original_name: String::new(),
            name: "new-agent".into(),
            description: String::new(),
            model: None,
            tool_groups: Vec::new(),
            skills: None,
            soul: String::new(),
            invalid: false,
            tool_groups_input: String::new(),
            skills_input: "*".into(),
        }
    }
}

#[derive(Debug, Clone, Deserialize, Serialize)]
struct SubagentsDocument {
    enabled: bool,
    timeout_seconds: u32,
    max_turns: Option<u32>,
    agents: Map<String, Value>,
    custom_agents: Map<String, Value>,
    #[serde(default)]
    builtin_agents: Vec<BuiltinSubagentDocument>,
    #[serde(default)]
    advanced: Map<String, Value>,
    #[serde(skip)]
    timeout_input: String,
    #[serde(skip)]
    max_turns_input: String,
    #[serde(skip)]
    agents_editor: text_editor::Content,
    #[serde(skip)]
    custom_agents_editor: text_editor::Content,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
struct BuiltinSubagentDocument {
    name: String,
    description: String,
    default_model: String,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
struct SandboxDocument {
    #[serde(rename = "use")]
    use_path: String,
    allow_host_bash: bool,
    allow_host_tools: bool,
    advanced: Map<String, Value>,
    #[serde(skip)]
    advanced_editor: text_editor::Content,
}

impl SandboxDocument {
    fn enforce_portable_local_mode(&mut self) -> bool {
        let original_provider = self.use_path.clone();
        let original_option_count = self.advanced.len();
        self.use_path = PORTABLE_LOCAL_SANDBOX_PROVIDER.into();
        self.advanced
            .retain(|key, _| PORTABLE_LOCAL_SANDBOX_OPTIONS.contains(&key.as_str()));
        original_provider != self.use_path || original_option_count != self.advanced.len()
    }
}

#[cfg(test)]
mod portable_sandbox_tests {
    use super::{PORTABLE_LOCAL_SANDBOX_PROVIDER, SandboxDocument};
    use iced::widget::text_editor;
    use serde_json::{Map, Value, json};

    #[test]
    fn portable_mode_forces_local_and_removes_container_options() {
        let mut advanced = Map::new();
        advanced.insert("mounts".into(), json!([]));
        advanced.insert("bash_output_max_chars".into(), json!(12_000));
        advanced.insert("image".into(), json!("sandbox:latest"));
        advanced.insert("wsl_distro".into(), json!("Ubuntu"));
        advanced.insert("environment".into(), json!({"TOKEN": "secret"}));
        let mut sandbox = SandboxDocument {
            use_path: "wsl".into(),
            allow_host_bash: false,
            allow_host_tools: false,
            advanced,
            advanced_editor: text_editor::Content::new(),
        };

        assert!(sandbox.enforce_portable_local_mode());

        assert_eq!(sandbox.use_path, PORTABLE_LOCAL_SANDBOX_PROVIDER);
        assert_eq!(
            sandbox.advanced.keys().collect::<Vec<_>>(),
            vec!["bash_output_max_chars", "mounts"]
        );
        assert_eq!(
            sandbox.advanced.get("bash_output_max_chars"),
            Some(&Value::from(12_000))
        );
    }
}

impl ConfigDocument {
    fn prepare_editor_state(&mut self) {
        self.runtime.extra_editor =
            text_editor::Content::with_text(&pretty_json_object(&self.runtime.extra));
        self.runtime.overlay_editor = text_editor::Content::with_text(&self.runtime.prompt_overlay);
        self.runtime.connections_input = self.runtime.max_active_connections.to_string();
        self.runtime.runs_input = self.runtime.max_active_runs.to_string();
        self.runtime.subagent_count_input = self.runtime.max_concurrent_subagents.to_string();
        self.runtime.timeout_input = self.runtime.run_timeout_seconds.to_string();
        self.runtime.tool_allowlist_input = self
            .runtime
            .tool_allowlist
            .as_ref()
            .map(|items| items.join(", "))
            .unwrap_or_else(|| "*".into());
        self.runtime.tool_denylist_input = self.runtime.tool_denylist.join(", ");
        self.memory.shutdown_flush_input = self.memory.shutdown_flush_timeout_seconds.to_string();
        self.memory.debounce_input = self.memory.debounce_seconds.to_string();
        self.memory.max_facts_input = self.memory.max_facts.to_string();
        self.memory.confidence_input = self.memory.fact_confidence_threshold.to_string();
        self.memory.max_tokens_input = self.memory.max_injection_tokens.to_string();
        self.memory.retrieval_top_k_input = self.memory.retrieval_top_k.to_string();
        self.subagents.timeout_input = self.subagents.timeout_seconds.to_string();
        self.subagents.max_turns_input = self
            .subagents
            .max_turns
            .map(|value| value.to_string())
            .unwrap_or_default();
        self.subagents.agents_editor =
            text_editor::Content::with_text(&pretty_json_object(&self.subagents.agents));
        self.subagents.custom_agents_editor =
            text_editor::Content::with_text(&pretty_json_object(&self.subagents.custom_agents));
        self.portable_sandbox_migration_pending = self.sandbox.enforce_portable_local_mode();
        self.sandbox.advanced_editor =
            text_editor::Content::with_text(&pretty_json_object(&self.sandbox.advanced));
        self.tool_groups_editor =
            text_editor::Content::with_text(&pretty_json_objects(&self.tool_groups));
        self.tools_editor = text_editor::Content::with_text(&pretty_json_objects(&self.tools));
        let discovery = &mut self.skill_evolution.discovery;
        discovery.min_tool_calls_input = discovery.min_tool_calls.to_string();
        discovery.repeat_threshold_input = discovery.repeat_threshold.to_string();
        discovery.repeat_window_days_input = discovery.repeat_window_days.to_string();
        discovery.cooldown_hours_input = discovery.cooldown_hours.to_string();
        discovery.max_daily_proposals_input = discovery.max_daily_proposals.to_string();
        discovery.max_pending_proposals_input = discovery.max_pending_proposals.to_string();
        let limits = &mut self.skill_evolution.candidate_limits;
        limits.max_files_input = limits.max_files.to_string();
        limits.max_total_bytes_input = limits.max_total_bytes.to_string();
        limits.max_file_bytes_input = limits.max_file_bytes.to_string();
        self.skill_evolution.auto_patch.max_changed_lines_input = self
            .skill_evolution
            .auto_patch
            .max_changed_lines
            .to_string();
        let monitoring = &mut self.skill_evolution.monitoring;
        monitoring.probation_uses_input = monitoring.probation_uses.to_string();
        monitoring.rollback_failures_input =
            monitoring.auto_rollback_consecutive_failures.to_string();
        for agent in &mut self.agents {
            agent.tool_groups_input = agent.tool_groups.join(", ");
            agent.skills_input = agent
                .skills
                .as_ref()
                .map(|items| items.join(", "))
                .unwrap_or_else(|| "*".into());
        }
    }

    fn sync_numeric_inputs(&mut self) -> Result<(), String> {
        self.sync_runtime_inputs()?;
        self.sync_evolution_inputs()
    }

    fn sync_runtime_inputs(&mut self) -> Result<(), String> {
        let r = &mut self.runtime;
        r.max_active_connections = parse_bounded_u32(&r.connections_input, "最大连接数", 1, 128)?;
        r.max_active_runs = parse_bounded_u32(&r.runs_input, "最大并发运行", 1, 128)?;
        r.max_concurrent_subagents =
            parse_bounded_u32(&r.subagent_count_input, "Subagent 并发", 1, 4)?;
        r.run_timeout_seconds = parse_bounded_f64(&r.timeout_input, "执行超时", 0.001, 86400.0)?;
        let extra = parse_json_object(&r.extra_editor.text(), "长任务与保留策略")?;
        for key in [
            "model_name",
            "agent_name",
            "thinking_enabled",
            "plan_mode",
            "subagent_enabled",
            "max_concurrent_subagents",
            "max_active_connections",
            "max_active_runs",
            "run_timeout_seconds",
            "permission_mode",
            "memory_scope",
            "enable_bash",
            "tool_allowlist",
            "tool_denylist",
            "prompt_overlay",
        ] {
            if extra.contains_key(key) {
                return Err(format!(
                    "{key} 请使用普通表单编辑，不能在高级 JSON 中重复定义"
                ));
            }
        }
        r.extra = extra;
        r.prompt_overlay = r.overlay_editor.text();
        Ok(())
    }

    fn sync_evolution_inputs(&mut self) -> Result<(), String> {
        let e = &mut self.skill_evolution;
        macro_rules! number {
            ($obj:expr, $field:ident, $input:ident, $label:expr, $min:expr, $max:expr) => {
                $obj.$field = parse_bounded_u32(&$obj.$input, $label, $min, $max)?;
            };
        }
        number!(
            e.discovery,
            min_tool_calls,
            min_tool_calls_input,
            "最少工具调用",
            1,
            1000
        );
        number!(
            e.discovery,
            repeat_threshold,
            repeat_threshold_input,
            "重复阈值",
            2,
            100
        );
        number!(
            e.discovery,
            repeat_window_days,
            repeat_window_days_input,
            "重复窗口天数",
            1,
            3650
        );
        number!(
            e.discovery,
            cooldown_hours,
            cooldown_hours_input,
            "冷却小时",
            0,
            8760
        );
        number!(
            e.discovery,
            max_daily_proposals,
            max_daily_proposals_input,
            "每日候选上限",
            1,
            1000
        );
        number!(
            e.discovery,
            max_pending_proposals,
            max_pending_proposals_input,
            "待审候选上限",
            1,
            10000
        );
        number!(
            e.candidate_limits,
            max_files,
            max_files_input,
            "候选文件数",
            1,
            1000
        );
        number!(
            e.candidate_limits,
            max_total_bytes,
            max_total_bytes_input,
            "候选总字节",
            1024,
            100000000
        );
        number!(
            e.candidate_limits,
            max_file_bytes,
            max_file_bytes_input,
            "单文件字节",
            256,
            10000000
        );
        number!(
            e.auto_patch,
            max_changed_lines,
            max_changed_lines_input,
            "变更行数",
            1,
            10000
        );
        number!(
            e.monitoring,
            probation_uses,
            probation_uses_input,
            "观察次数",
            1,
            100
        );
        number!(
            e.monitoring,
            auto_rollback_consecutive_failures,
            rollback_failures_input,
            "回滚失败阈值",
            1,
            100
        );
        Ok(())
    }

    fn sync_subagent_editors(&mut self) -> Result<(), String> {
        let timeout_seconds: u32 = self
            .subagents
            .timeout_input
            .trim()
            .parse()
            .map_err(|_| "Subagents 默认超时必须是正整数".to_owned())?;
        if timeout_seconds == 0 {
            return Err("Subagents 默认超时必须大于 0".into());
        }
        self.subagents.timeout_seconds = timeout_seconds;
        self.subagents.max_turns = if self.subagents.max_turns_input.trim().is_empty() {
            None
        } else {
            let max_turns: u32 = self
                .subagents
                .max_turns_input
                .trim()
                .parse()
                .map_err(|_| "Subagents 默认最大轮次必须留空或填写正整数".to_owned())?;
            if max_turns == 0 {
                return Err("Subagents 默认最大轮次必须大于 0".into());
            }
            Some(max_turns)
        };
        self.subagents.agents =
            parse_json_object(&self.subagents.agents_editor.text(), "Subagents agents")?;
        self.subagents.custom_agents = parse_json_object(
            &self.subagents.custom_agents_editor.text(),
            "Subagents custom_agents",
        )?;
        Ok(())
    }

    fn sync_sandbox_tool_editors(&mut self) -> Result<(), String> {
        self.sandbox.advanced =
            parse_json_object(&self.sandbox.advanced_editor.text(), "Sandbox 高级配置")?;
        self.sandbox.enforce_portable_local_mode();
        self.tool_groups = parse_json_object_array(&self.tool_groups_editor.text(), "Tool Groups")?;
        self.tools = parse_json_object_array(&self.tools_editor.text(), "Tools")?;
        Ok(())
    }

    fn sync_memory_inputs(&mut self) -> Result<(), String> {
        self.memory.shutdown_flush_timeout_seconds = parse_bounded_f64(
            &self.memory.shutdown_flush_input,
            "关闭刷新超时",
            0.1,
            300.0,
        )?;
        self.memory.debounce_seconds =
            parse_bounded_u32(&self.memory.debounce_input, "记忆写入延迟", 1, 300)?;
        self.memory.max_facts =
            parse_bounded_u32(&self.memory.max_facts_input, "最大事实数", 10, 500)?;
        self.memory.fact_confidence_threshold =
            parse_bounded_f64(&self.memory.confidence_input, "事实置信度阈值", 0.0, 1.0)?;
        self.memory.max_injection_tokens =
            parse_bounded_u32(&self.memory.max_tokens_input, "最大注入 Token", 100, 8000)?;
        self.memory.retrieval_top_k =
            parse_bounded_u32(&self.memory.retrieval_top_k_input, "检索 Top K", 1, 100)?;
        Ok(())
    }

    fn refresh_subagent_editors(&mut self) {
        self.subagents.agents_editor =
            text_editor::Content::with_text(&pretty_json_object(&self.subagents.agents));
        self.subagents.custom_agents_editor =
            text_editor::Content::with_text(&pretty_json_object(&self.subagents.custom_agents));
    }
}

#[derive(Debug, Clone, Deserialize, Serialize)]
struct SkillDocument {
    name: String,
    description: String,
    category: String,
    enabled: bool,
    path: String,
}

#[derive(Debug, Clone, Deserialize)]
struct EvolutionProposal {
    id: String,
    status: String,
    action: String,
    skill_name: String,
    #[serde(default)]
    reason: String,
    #[serde(default)]
    trigger: Value,
    #[serde(default)]
    author: String,
    #[serde(default)]
    origin: String,
    #[serde(default)]
    base_revision: Option<u64>,
    #[serde(default)]
    base_sha256: Option<String>,
    #[serde(default)]
    candidate_sha256: Option<String>,
    #[serde(default)]
    risk: String,
    #[serde(default)]
    changed_files: Vec<String>,
    #[serde(default)]
    scans: Vec<Value>,
    #[serde(default)]
    evaluation: Value,
    created_at: String,
    updated_at: String,
    #[serde(default)]
    error: Option<String>,
    #[serde(default)]
    diff: String,
}

#[derive(Debug, Clone, Deserialize)]
struct ProposalListData {
    proposals: Vec<EvolutionProposal>,
    catalog_version: u64,
}

#[derive(Debug, Clone, Deserialize)]
struct ProposalMutationData {
    proposal: EvolutionProposal,
    catalog_version: u64,
}

#[derive(Debug, Clone, Copy, Eq, PartialEq)]
enum ProposalReviewAction {
    Approve,
    Reject,
}

impl ProposalReviewAction {
    fn label(self) -> &'static str {
        match self {
            Self::Approve => "批准并发布",
            Self::Reject => "拒绝 Proposal",
        }
    }

    fn operation(self) -> &'static str {
        match self {
            Self::Approve => "proposal.approve",
            Self::Reject => "proposal.reject",
        }
    }
}

#[derive(Debug, Clone, Deserialize, Serialize)]
struct SkillEvolutionDocument {
    enabled: bool,
    mode: String,
    storage_path: String,
    generation_model_name: Option<String>,
    moderation_model_name: Option<String>,
    evaluation_model_name: Option<String>,
    security_fail_closed: bool,
    discovery: SkillEvolutionDiscoveryDocument,
    candidate_limits: SkillEvolutionCandidateLimitsDocument,
    auto_patch: SkillEvolutionAutoPatchDocument,
    monitoring: SkillEvolutionMonitoringDocument,
    #[serde(default)]
    advanced: Map<String, Value>,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
struct SkillEvolutionDiscoveryDocument {
    enabled: bool,
    min_tool_calls: u32,
    repeat_threshold: u32,
    repeat_window_days: u32,
    cooldown_hours: u32,
    max_daily_proposals: u32,
    max_pending_proposals: u32,
    #[serde(skip)]
    min_tool_calls_input: String,
    #[serde(skip)]
    repeat_threshold_input: String,
    #[serde(skip)]
    repeat_window_days_input: String,
    #[serde(skip)]
    cooldown_hours_input: String,
    #[serde(skip)]
    max_daily_proposals_input: String,
    #[serde(skip)]
    max_pending_proposals_input: String,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
struct SkillEvolutionCandidateLimitsDocument {
    max_files: u32,
    max_total_bytes: u32,
    max_file_bytes: u32,
    #[serde(skip)]
    max_files_input: String,
    #[serde(skip)]
    max_total_bytes_input: String,
    #[serde(skip)]
    max_file_bytes_input: String,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
struct SkillEvolutionAutoPatchDocument {
    max_changed_lines: u32,
    allow_create: bool,
    allow_support_files: bool,
    allow_scripts: bool,
    allow_delete: bool,
    #[serde(skip)]
    max_changed_lines_input: String,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
struct SkillEvolutionMonitoringDocument {
    probation_uses: u32,
    auto_rollback_consecutive_failures: u32,
    #[serde(skip)]
    probation_uses_input: String,
    #[serde(skip)]
    rollback_failures_input: String,
}

#[derive(Debug, Clone, Default, Deserialize, Serialize)]
struct ProductDataPaths {
    config: String,
    extensions: String,
    skills: String,
    agents: String,
    user_data: String,
    memory: String,
    memory_index: String,
}

#[derive(Debug, Clone)]
struct ProductPaths {
    root: PathBuf,
    user_data: PathBuf,
    config: PathBuf,
    resources: PathBuf,
    python: PathBuf,
    bridge: PathBuf,
    runtime: PathBuf,
}

impl ProductPaths {
    fn discover() -> Self {
        let executable_root = env::current_exe()
            .ok()
            .and_then(|path| path.parent().map(Path::to_path_buf))
            .unwrap_or_else(|| PathBuf::from("."));
        let portable = executable_root.join("runtime").join("python.exe").is_file()
            || executable_root
                .join("resources")
                .join("default-config.yaml")
                .is_file();
        let root = if portable {
            executable_root
        } else {
            PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                .parent()
                .unwrap_or(Path::new("."))
                .to_path_buf()
        };
        let user_data = if portable {
            root.join("user-data")
        } else {
            root.join(".deerflow-config")
        };
        let python = if portable {
            root.join("runtime").join("python.exe")
        } else {
            root.join(".venv").join("Scripts").join("python.exe")
        };
        let bridge = if portable {
            root.join("deerflow-acp.exe")
        } else {
            let release = root
                .join("bridge")
                .join("target")
                .join("release")
                .join("deerflow-acp.exe");
            if release.is_file() {
                release
            } else {
                root.join("bridge")
                    .join("target")
                    .join("debug")
                    .join("deerflow-acp.exe")
            }
        };
        let config = if portable {
            user_data.join("config").join("config.yaml")
        } else {
            root.join("config.yaml")
        };
        let resources = root.join("resources");
        let runtime = user_data.join("runtime").join("acp");
        Self {
            root,
            user_data,
            config,
            resources,
            python,
            bridge,
            runtime,
        }
    }
}

#[derive(Debug, Clone, Default)]
enum DaemonStatus {
    #[default]
    Checking,
    Running(String),
    Stopped,
    Error(String),
}

impl DaemonStatus {
    fn is_running(&self) -> bool {
        matches!(self, Self::Running(_))
    }

    fn summary_label(&self) -> &'static str {
        match self {
            Self::Checking => "正在检查",
            Self::Running(details) if details.contains("warmup=warming") => "预热中",
            Self::Running(details) if details.contains("warmup=failed") => "预热失败",
            Self::Running(_) => "运行中",
            Self::Stopped => "已停止",
            Self::Error(_) => "检查失败",
        }
    }

    fn detail_label(&self) -> String {
        match self {
            Self::Checking => "正在读取本地 Daemon 状态".into(),
            Self::Running(details) => compact_daemon_details(details),
            Self::Stopped => "Daemon 未启动".into(),
            Self::Error(error) => format!("错误：{}", truncate_text(error, 30)),
        }
    }
}

fn compact_daemon_details(details: &str) -> String {
    let field = |name: &str| {
        details
            .split_whitespace()
            .find_map(|part| part.strip_prefix(name))
    };
    match (field("pid="), field("endpoint=")) {
        (Some(pid), Some(endpoint)) => format!("PID {pid} · {endpoint}"),
        (Some(pid), None) => format!("PID {pid}"),
        (None, Some(endpoint)) => endpoint.to_owned(),
        (None, None) => truncate_text(details, 36),
    }
}

fn truncate_text(value: &str, max_chars: usize) -> String {
    if value.chars().count() <= max_chars {
        value.to_owned()
    } else {
        let mut shortened = value
            .chars()
            .take(max_chars.saturating_sub(1))
            .collect::<String>();
        shortened.push('…');
        shortened
    }
}

#[cfg(test)]
mod daemon_status_tests {
    use super::{compact_daemon_details, truncate_text};

    #[test]
    fn running_status_keeps_only_pid_and_endpoint() {
        let details = "running pid=24412 build=dev-012345 endpoint=127.0.0.1:54283 (OK)";
        assert_eq!(
            compact_daemon_details(details),
            "PID 24412 · 127.0.0.1:54283"
        );
    }

    #[test]
    fn truncation_is_unicode_safe() {
        assert_eq!(
            truncate_text("检查失败：连接端点不可用", 8),
            "检查失败：连接…"
        );
    }
}

#[derive(Debug)]
struct App {
    paths: ProductPaths,
    page: Page,
    document: Option<ConfigDocument>,
    selected_model: usize,
    selected_agent: usize,
    agent_section: AgentSection,
    skills_section: SkillsSection,
    evolution_proposals: Vec<EvolutionProposal>,
    selected_evolution_proposal: Option<EvolutionProposal>,
    proposal_diff_editor: text_editor::Content,
    proposal_review_note: String,
    proposal_catalog_version: u64,
    proposal_busy: bool,
    proposal_error: Option<String>,
    proposal_notice: Option<String>,
    proposal_confirmation: Option<ProposalReviewAction>,
    daemon: DaemonStatus,
    busy: bool,
    dirty: bool,
    notice: Option<String>,
    error: Option<String>,
    status_checking: bool,
    last_checked: Option<Instant>,
    pending_apply: bool,
    apply_after_save: bool,
    waiting_apply: bool,
    pending_action: Option<Message>,
    advanced_pages: Vec<Page>,
    skill_search: String,
    portable: portable::State,
}

#[derive(Debug, Clone)]
enum Message {
    Navigate(Page),
    AgentSection(AgentSection),
    SkillsSection(SkillsSection),
    Reload,
    RefreshStatus,
    Tick,
    WindowClosed(window::Id),
    ConfirmAction,
    CancelAction,
    SaveApply,
    ApplyWhenIdle,
    ApplyNow,
    CancelApply,
    DrainChecked(Result<Value, String>),
    ToggleAdvanced(Page),
    LocateValidation(Page),
    ScrollToValidation(Page, f32),
    ReplacementModel(String),
    DeleteModel(String, Option<String>),
    SkillSearch(String),
    RuntimeExtra(text_editor::Action),
    RuntimeOverlayEdit(text_editor::Action),
    Portable(portable::Message),
    Loaded(Result<ConfigDocument, String>),
    DaemonChecked(DaemonStatus),
    StartDaemon,
    StopDaemon,
    DaemonActionFinished(Result<DaemonStatus, String>),
    Save,
    Saved(Result<(ConfigDocument, DaemonStatus, bool), String>),
    SelectModel(usize),
    AddModel,
    RemoveModel,
    ModelName(String),
    ModelDisplayName(String),
    ModelDescription(String),
    ModelUse(String),
    ModelId(String),
    ModelApiKey(String),
    ModelClearKey(bool),
    ModelBaseUrl(String),
    ModelThinking(bool),
    ModelReasoning(bool),
    ModelVision(bool),
    DefaultModel(String),
    MemoryEnabled(bool),
    MemoryMode(String),
    MemoryInjection(bool),
    MemoryModel(String),
    MemoryShutdownFlush(String),
    MemoryStoragePath(String),
    MemoryDebounce(String),
    MemoryMaxFacts(String),
    MemoryConfidence(String),
    MemoryMaxTokens(String),
    MemoryRetrieval(bool),
    MemoryRetrievalTopK(String),
    MemoryRetrievalIndexPath(String),
    SelectAgent(usize),
    AddAgent,
    RemoveAgent,
    AgentName(String),
    AgentDescription(String),
    AgentModel(String),
    AgentToolGroups(String),
    AgentSkills(String),
    SubagentsEnabled(bool),
    SubagentsTimeout(String),
    SubagentsMaxTurns(String),
    SubagentsAgentsEdit(text_editor::Action),
    SubagentsCustomEdit(text_editor::Action),
    SubagentModel(String, String, String),
    SandboxAllowHostBash(bool),
    SandboxAllowHostTools(bool),
    SandboxAdvancedEdit(text_editor::Action),
    ToolGroupsEdit(text_editor::Action),
    ToolsEdit(text_editor::Action),
    RuntimeToolAllowlist(String),
    RuntimeToolDenylist(String),
    SkillsEnabled(bool),
    SkillEnabled(usize, bool),
    EvolutionEnabled(bool),
    EvolutionMode(String),
    EvolutionStoragePath(String),
    EvolutionGenerationModel(String),
    EvolutionModerationModel(String),
    EvolutionEvaluationModel(String),
    EvolutionSecurityFailClosed(bool),
    EvolutionDiscoveryEnabled(bool),
    EvolutionMinToolCalls(String),
    EvolutionRepeatThreshold(String),
    EvolutionRepeatWindowDays(String),
    EvolutionCooldownHours(String),
    EvolutionMaxDailyProposals(String),
    EvolutionMaxPendingProposals(String),
    EvolutionMaxFiles(String),
    EvolutionMaxTotalBytes(String),
    EvolutionMaxFileBytes(String),
    EvolutionMaxChangedLines(String),
    EvolutionProbationUses(String),
    EvolutionRollbackFailures(String),
    RefreshEvolutionProposals,
    EvolutionProposalsLoaded(Result<ProposalListData, String>),
    SelectEvolutionProposal(String),
    EvolutionProposalLoaded(Result<EvolutionProposal, String>),
    ProposalReviewNote(String),
    RequestProposalReview(ProposalReviewAction),
    CancelProposalReview,
    ConfirmProposalReview,
    EvolutionProposalReviewed(Result<ProposalMutationData, String>),
    RuntimeModel(String),
    RuntimeAgent(String),
    RuntimeThinking(bool),
    RuntimePlan(bool),
    RuntimeSubagents(bool),
    RuntimeBash(bool),
    RuntimePermission(String),
    RuntimeMemory(String),
    RuntimeConnections(String),
    RuntimeRuns(String),
    RuntimeSubagentCount(String),
    RuntimeTimeout(String),
    OpenFolder(PathBuf),
}

impl Message {
    fn changes_config(&self) -> bool {
        match self {
            Self::SubagentsAgentsEdit(action)
            | Self::SubagentsCustomEdit(action)
            | Self::SandboxAdvancedEdit(action)
            | Self::ToolGroupsEdit(action)
            | Self::ToolsEdit(action) => return action.is_edit(),
            _ => {}
        }
        matches!(
            self,
            Self::AddModel
                | Self::RemoveModel
                | Self::ModelName(_)
                | Self::ModelDisplayName(_)
                | Self::ModelDescription(_)
                | Self::ModelUse(_)
                | Self::ModelId(_)
                | Self::ModelApiKey(_)
                | Self::ModelClearKey(_)
                | Self::ModelBaseUrl(_)
                | Self::ModelThinking(_)
                | Self::ModelReasoning(_)
                | Self::ModelVision(_)
                | Self::DefaultModel(_)
                | Self::MemoryEnabled(_)
                | Self::MemoryMode(_)
                | Self::MemoryInjection(_)
                | Self::MemoryModel(_)
                | Self::MemoryShutdownFlush(_)
                | Self::MemoryStoragePath(_)
                | Self::MemoryDebounce(_)
                | Self::MemoryMaxFacts(_)
                | Self::MemoryConfidence(_)
                | Self::MemoryMaxTokens(_)
                | Self::MemoryRetrieval(_)
                | Self::MemoryRetrievalTopK(_)
                | Self::MemoryRetrievalIndexPath(_)
                | Self::AddAgent
                | Self::RemoveAgent
                | Self::AgentName(_)
                | Self::AgentDescription(_)
                | Self::AgentModel(_)
                | Self::AgentToolGroups(_)
                | Self::AgentSkills(_)
                | Self::SubagentsEnabled(_)
                | Self::SubagentsTimeout(_)
                | Self::SubagentsMaxTurns(_)
                | Self::SubagentsAgentsEdit(_)
                | Self::SubagentsCustomEdit(_)
                | Self::SubagentModel(_, _, _)
                | Self::SandboxAllowHostBash(_)
                | Self::SandboxAllowHostTools(_)
                | Self::SandboxAdvancedEdit(_)
                | Self::ToolGroupsEdit(_)
                | Self::ToolsEdit(_)
                | Self::RuntimeToolAllowlist(_)
                | Self::RuntimeToolDenylist(_)
                | Self::SkillsEnabled(_)
                | Self::SkillEnabled(_, _)
                | Self::EvolutionEnabled(_)
                | Self::EvolutionMode(_)
                | Self::EvolutionStoragePath(_)
                | Self::EvolutionGenerationModel(_)
                | Self::EvolutionModerationModel(_)
                | Self::EvolutionEvaluationModel(_)
                | Self::EvolutionSecurityFailClosed(_)
                | Self::EvolutionDiscoveryEnabled(_)
                | Self::EvolutionMinToolCalls(_)
                | Self::EvolutionRepeatThreshold(_)
                | Self::EvolutionRepeatWindowDays(_)
                | Self::EvolutionCooldownHours(_)
                | Self::EvolutionMaxDailyProposals(_)
                | Self::EvolutionMaxPendingProposals(_)
                | Self::EvolutionMaxFiles(_)
                | Self::EvolutionMaxTotalBytes(_)
                | Self::EvolutionMaxFileBytes(_)
                | Self::EvolutionMaxChangedLines(_)
                | Self::EvolutionProbationUses(_)
                | Self::EvolutionRollbackFailures(_)
                | Self::RuntimeModel(_)
                | Self::RuntimeAgent(_)
                | Self::RuntimeThinking(_)
                | Self::RuntimePlan(_)
                | Self::RuntimeSubagents(_)
                | Self::RuntimeBash(_)
                | Self::RuntimePermission(_)
                | Self::RuntimeMemory(_)
                | Self::RuntimeConnections(_)
                | Self::RuntimeRuns(_)
                | Self::RuntimeSubagentCount(_)
                | Self::RuntimeTimeout(_)
        )
    }
}

impl App {
    fn new() -> (Self, Task<Message>) {
        let paths = ProductPaths::discover();
        let load_paths = paths.clone();
        let daemon_paths = paths.clone();
        (
            Self {
                paths,
                page: Page::Dashboard,
                document: None,
                selected_model: 0,
                selected_agent: 0,
                agent_section: AgentSection::Subagents,
                skills_section: SkillsSection::Proposals,
                evolution_proposals: Vec::new(),
                selected_evolution_proposal: None,
                proposal_diff_editor: text_editor::Content::new(),
                proposal_review_note: String::new(),
                proposal_catalog_version: 0,
                proposal_busy: false,
                proposal_error: None,
                proposal_notice: None,
                proposal_confirmation: None,
                daemon: DaemonStatus::Checking,
                busy: true,
                dirty: false,
                notice: None,
                error: None,
                status_checking: true,
                last_checked: None,
                pending_apply: false,
                apply_after_save: false,
                waiting_apply: false,
                pending_action: None,
                advanced_pages: Vec::new(),
                skill_search: String::new(),
                portable: portable::State::default(),
            },
            Task::batch([
                Task::perform(async move { load_config(&load_paths) }, Message::Loaded),
                Task::perform(
                    async move { query_daemon(&daemon_paths) },
                    Message::DaemonChecked,
                ),
            ]),
        )
    }

    fn subscription(&self) -> Subscription<Message> {
        Subscription::batch([
            iced::time::every(Duration::from_secs(5)).map(|_| Message::Tick),
            window::close_requests().map(Message::WindowClosed),
        ])
    }

    fn update(&mut self, message: Message) -> Task<Message> {
        if let Some((page, _)) = self.validation_issue() {
            if !self.advanced_pages.contains(&page) {
                self.advanced_pages.push(page);
            }
        }
        if self.pending_action.is_some()
            && !matches!(
                &message,
                Message::ConfirmAction
                    | Message::CancelAction
                    | Message::ReplacementModel(_)
                    | Message::ScrollToValidation(_, _)
                    | Message::Tick
                    | Message::RefreshStatus
                    | Message::Loaded(_)
                    | Message::Saved(_)
                    | Message::DaemonChecked(_)
                    | Message::DaemonActionFinished(_)
                    | Message::DrainChecked(_)
                    | Message::EvolutionProposalsLoaded(_)
                    | Message::EvolutionProposalLoaded(_)
                    | Message::EvolutionProposalReviewed(_)
                    | Message::Portable(
                        portable::Message::Inspected(_)
                            | portable::Message::ModelTested(_)
                            | portable::Message::SessionsLoaded(_)
                            | portable::Message::MemoryLoaded(_)
                            | portable::Message::Mutated(_)
                            | portable::Message::Restored(_)
                            | portable::Message::HistoryLoaded(_)
                    )
            )
        {
            return Task::none();
        }
        // A pending save owns a snapshot; never accept edits it would overwrite.
        if self.busy
            && (message.changes_config()
                || matches!(
                    message,
                    Message::RuntimeExtra(_)
                        | Message::RuntimeOverlayEdit(_)
                        | Message::Reload
                        | Message::Save
                        | Message::SaveApply
                        | Message::SelectModel(_)
                        | Message::SelectAgent(_)
                ))
        {
            return Task::none();
        }
        match message {
            Message::Portable(msg) => return self.update_portable(msg),
            Message::ToggleAdvanced(page) => {
                if self.advanced_open(page) {
                    self.advanced_pages.retain(|p| *p != page);
                } else {
                    self.advanced_pages.push(page);
                }
                return Task::none();
            }
            Message::LocateValidation(page) => {
                self.page = page;
                if page == Page::Models {
                    self.select_invalid_model();
                }
                if !self.advanced_pages.contains(&page) {
                    self.advanced_pages.push(page);
                }
                if page == Page::Skills {
                    self.skills_section = SkillsSection::Evolution;
                }
                let target = self
                    .validation_issue()
                    .map(|(_, e)| preferences::validation_target(page, &e))
                    .unwrap_or_else(|| format!("advanced-{page:?}"));
                return preferences::locate_field(page, target);
            }
            Message::ScrollToValidation(page, offset) => {
                return iced::widget::operation::scroll_to(
                    format!("page-{page:?}"),
                    iced::widget::operation::AbsoluteOffset {
                        x: None,
                        y: Some(offset),
                    },
                );
            }
            Message::ReplacementModel(value) => {
                if let Some(Message::DeleteModel(_, replacement)) = &mut self.pending_action {
                    *replacement = Some(value);
                }
                return Task::none();
            }
            Message::RemoveModel => {
                if let Some(doc) = &self.document {
                    if let Some(model) = doc.models.get(self.selected_model) {
                        let refs = model_references(doc, &model.name);
                        if !refs.is_empty() {
                            self.error = Some(format!(
                                "模型仍被引用：{}。请先修改这些配置。",
                                refs.join("、")
                            ));
                        } else if doc.models.len() > 1 {
                            self.pending_action =
                                Some(Message::DeleteModel(model.name.clone(), None));
                        }
                    }
                }
                return Task::none();
            }
            Message::DeleteModel(name, replacement) => {
                if self.busy {
                    return Task::none();
                }
                if let Some(doc) = &mut self.document {
                    match preferences::delete_model(doc, &name, replacement.as_deref()) {
                        Ok(()) => {
                            self.selected_model =
                                self.selected_model.min(doc.models.len().saturating_sub(1));
                            self.dirty = true;
                            self.error = None;
                            self.notice = Some("模型已从草稿移除，请保存配置。".into());
                        }
                        Err(error) => self.error = Some(error),
                    }
                }
                return Task::none();
            }
            Message::SkillSearch(value) => {
                self.skill_search = value;
                return Task::none();
            }
            Message::RuntimeExtra(action) => {
                let edit = action.is_edit();
                if let Some(doc) = &mut self.document {
                    doc.runtime.extra_editor.perform(action);
                }
                self.dirty |= edit;
                return Task::none();
            }
            Message::RuntimeOverlayEdit(action) => {
                let edit = action.is_edit();
                if let Some(doc) = &mut self.document {
                    doc.runtime.overlay_editor.perform(action);
                }
                self.dirty |= edit;
                return Task::none();
            }
            Message::WindowClosed(id) => {
                if self.busy || self.dirty || self.waiting_apply {
                    self.pending_action = Some(Message::WindowClosed(id));
                    return Task::none();
                }
                return window::close(id);
            }
            Message::ConfirmAction => {
                if !self.confirmation_ready() {
                    return Task::none();
                }
                if let Some(action) = self.pending_action.take() {
                    if let Message::WindowClosed(id) = action {
                        if self.busy || self.waiting_apply {
                            self.error = Some("请等待当前操作完成，或取消等待应用后再关闭".into());
                            return Task::none();
                        }
                        return window::close(id);
                    }
                    if matches!(action, Message::Reload) {
                        self.dirty = false;
                    }
                    return self.update(action);
                }
                return Task::none();
            }
            Message::CancelAction => {
                self.pending_action = None;
                return Task::none();
            }
            Message::SaveApply => {
                self.apply_after_save = true;
                return self.update(Message::Save);
            }
            Message::ApplyWhenIdle => {
                self.waiting_apply = true;
                let paths = self.paths.clone();
                return Task::perform(
                    async move { management_service(&paths, &json!({"operation":"daemon.drain"})) },
                    Message::DrainChecked,
                );
            }
            Message::CancelApply => {
                self.waiting_apply = false;
                let paths = self.paths.clone();
                return Task::perform(
                    async move { management_service(&paths, &json!({"operation":"daemon.resume"})) },
                    Message::DrainChecked,
                );
            }
            Message::ApplyNow => {
                self.waiting_apply = false;
                return self.daemon_task(DaemonCommand::Restart);
            }
            Message::DrainChecked(result) => {
                match result {
                    Ok(value) => {
                        self.portable.live = value.clone();
                        if self.waiting_apply && value["active_operations"].as_u64() == Some(0) {
                            self.waiting_apply = false;
                            return self.daemon_task(DaemonCommand::Restart);
                        }
                    }
                    Err(error) => {
                        self.error = Some(error);
                        self.waiting_apply = false;
                    }
                }
                return Task::none();
            }
            Message::Tick | Message::RefreshStatus => {
                if self.busy || self.status_checking {
                    return Task::none();
                }
                if self.waiting_apply {
                    let paths = self.paths.clone();
                    return Task::perform(
                        async move { management_service(&paths, &json!({"operation":"daemon.status"})) },
                        Message::DrainChecked,
                    );
                }
                self.status_checking = true;
                let paths = self.paths.clone();
                return Task::perform(async move { query_daemon(&paths) }, Message::DaemonChecked);
            }
            Message::Reload if self.dirty => {
                self.pending_action = Some(Message::Reload);
                return Task::none();
            }
            _ => {}
        }

        if message.changes_config() {
            self.dirty = true;
            self.notice = None;
        }
        match message {
            Message::Navigate(page) => {
                self.page = page;
                if page == Page::Skills {
                    self.proposal_error = None;
                    self.proposal_notice = None;
                    return self.proposal_list_task();
                }
            }
            Message::AgentSection(section) => self.agent_section = section,
            Message::SkillsSection(section) => self.skills_section = section,
            Message::Reload => {
                self.busy = true;
                self.error = None;
                let paths = self.paths.clone();
                return Task::perform(async move { load_config(&paths) }, Message::Loaded);
            }
            Message::Loaded(result) => {
                self.busy = false;
                match result {
                    Ok(document) => {
                        let migration_pending = document.portable_sandbox_migration_pending;
                        self.portable.soul = text_editor::Content::with_text(
                            document
                                .agents
                                .first()
                                .map(|a| a.soul.as_str())
                                .unwrap_or(""),
                        );
                        self.document = Some(document);
                        self.dirty = migration_pending;
                        self.selected_model = 0;
                        self.selected_agent = 0;
                        self.notice = Some(if migration_pending {
                            "检测到非 Local Sandbox 配置；保存后将迁移为便携 Local 模式".into()
                        } else {
                            "配置已加载".into()
                        });
                        self.error = None;
                    }
                    Err(error) => self.error = Some(error),
                }
            }
            Message::DaemonChecked(status) => {
                if let (DaemonStatus::Running(details), Some(doc)) = (&status, &self.document) {
                    if let Some(revision) = details
                        .split_whitespace()
                        .find_map(|p| p.strip_prefix("revision="))
                    {
                        self.pending_apply = revision != doc.config_revision;
                    }
                }
                self.daemon = status;
                self.status_checking = false;
                self.last_checked = Some(Instant::now());
            }
            Message::StartDaemon => return self.daemon_task(DaemonCommand::Start),
            Message::StopDaemon => return self.daemon_task(DaemonCommand::Stop),
            Message::DaemonActionFinished(result) => {
                self.busy = false;
                match result {
                    Ok(status) => {
                        self.pending_apply = false;
                        self.daemon = status;
                        self.last_checked = Some(Instant::now());
                        self.notice = Some("Daemon 状态已更新".into());
                        self.error = None;
                    }
                    Err(error) => {
                        self.daemon = DaemonStatus::Error(error.clone());
                        self.error = Some(error);
                    }
                }
            }
            Message::Save => {
                let Some(document) = &mut self.document else {
                    return Task::none();
                };
                if let Err(error) = document
                    .sync_numeric_inputs()
                    .and_then(|()| document.sync_subagent_editors())
                    .and_then(|()| document.sync_sandbox_tool_editors())
                    .and_then(|()| document.sync_memory_inputs())
                {
                    self.error = Some(error);
                    self.notice = None;
                    return Task::none();
                }
                let document = document.clone();
                self.busy = true;
                self.notice = None;
                self.error = None;
                let paths = self.paths.clone();
                return Task::perform(
                    async move { save_without_restart(&paths, &document) },
                    Message::Saved,
                );
            }
            Message::Saved(result) => {
                self.busy = false;
                match result {
                    Ok((document, status, restarted)) => {
                        self.document = Some(document);
                        self.dirty = false;
                        self.pending_apply = status.is_running();
                        self.daemon = status;
                        self.notice = Some(if restarted {
                            "配置已保存，Daemon 已安全重启".into()
                        } else {
                            "配置已保存；运行中的服务需要应用配置后生效".into()
                        });
                        self.error = None;
                        if self.apply_after_save {
                            self.apply_after_save = false;
                            if self.daemon.is_running() {
                                return self.update(Message::ApplyWhenIdle);
                            }
                            return self.daemon_task(DaemonCommand::Start);
                        }
                    }
                    Err(error) => {
                        self.error = Some(error);
                        self.apply_after_save = false;
                    }
                }
            }
            Message::SelectModel(index) => self.selected_model = index,
            Message::AddModel => {
                if let Some(document) = &mut self.document {
                    document.models.push(ModelDocument::default());
                    self.selected_model = document.models.len() - 1;
                }
            }
            Message::ModelName(value) => {
                if let Some(doc) = &mut self.document {
                    if let Some(model) = doc.models.get_mut(self.selected_model) {
                        let old = std::mem::replace(&mut model.name, value.clone());
                        if doc.default_model == old {
                            doc.default_model = value.clone();
                        }
                        for selected in [
                            &mut doc.runtime.model_name,
                            &mut doc.memory.model_name,
                            &mut doc.skill_evolution.generation_model_name,
                            &mut doc.skill_evolution.moderation_model_name,
                            &mut doc.skill_evolution.evaluation_model_name,
                        ] {
                            if selected.as_deref() == Some(&old) {
                                *selected = Some(value.clone());
                            }
                        }
                        for agent in &mut doc.agents {
                            if agent.model.as_deref() == Some(&old) {
                                agent.model = Some(value.clone());
                            }
                        }
                        for map in [&mut doc.subagents.agents, &mut doc.subagents.custom_agents] {
                            for item in map.values_mut() {
                                if item.get("model").and_then(Value::as_str) == Some(&old) {
                                    item["model"] = Value::String(value.clone());
                                }
                            }
                        }
                        doc.refresh_subagent_editors();
                    }
                }
            }
            Message::ModelDisplayName(value) => self.model_mut(|item| item.display_name = value),
            Message::ModelDescription(value) => self.model_mut(|item| item.description = value),
            Message::ModelUse(value) => self.model_mut(|item| item.use_path = value),
            Message::ModelId(value) => self.model_mut(|item| item.model = value),
            Message::ModelApiKey(value) => self.model_mut(|item| {
                item.api_key = value;
                item.clear_api_key = false;
            }),
            Message::ModelClearKey(value) => self.model_mut(|item| {
                item.clear_api_key = value;
                if value {
                    item.api_key.clear();
                }
            }),
            Message::ModelBaseUrl(value) => self.model_mut(|item| item.base_url = value),
            Message::ModelThinking(value) => self.model_mut(|item| item.supports_thinking = value),
            Message::ModelReasoning(value) => {
                self.model_mut(|item| item.supports_reasoning_effort = value)
            }
            Message::ModelVision(value) => self.model_mut(|item| item.supports_vision = value),
            Message::DefaultModel(value) => {
                if let Some(document) = &mut self.document {
                    if document.models.iter().any(|m| m.name == value) {
                        document.default_model = value.clone();
                        self.notice = Some(format!(
                            "默认模型已改为 {}，保存并应用后生效。",
                            model_label(document, &value)
                        ));
                    }
                }
            }
            Message::MemoryEnabled(value) => self.memory_mut(|item| item.enabled = value),
            Message::MemoryMode(value) => self.memory_mut(|item| item.mode = value),
            Message::MemoryInjection(value) => {
                self.memory_mut(|item| item.injection_enabled = value)
            }
            Message::MemoryModel(value) => {
                self.memory_mut(|item| item.model_name = non_empty(value))
            }
            Message::MemoryShutdownFlush(value) => {
                self.memory_mut(|item| item.shutdown_flush_input = value)
            }
            Message::MemoryStoragePath(value) => self.memory_mut(|item| item.storage_path = value),
            Message::MemoryDebounce(value) => self.memory_mut(|item| item.debounce_input = value),
            Message::MemoryMaxFacts(value) => self.memory_mut(|item| item.max_facts_input = value),
            Message::MemoryConfidence(value) => {
                self.memory_mut(|item| item.confidence_input = value)
            }
            Message::MemoryMaxTokens(value) => {
                self.memory_mut(|item| item.max_tokens_input = value)
            }
            Message::MemoryRetrieval(value) => {
                self.memory_mut(|item| item.retrieval_enabled = value)
            }
            Message::MemoryRetrievalTopK(value) => {
                self.memory_mut(|item| item.retrieval_top_k_input = value)
            }
            Message::MemoryRetrievalIndexPath(value) => {
                self.memory_mut(|item| item.retrieval_index_path = value)
            }
            Message::SelectAgent(index) => {
                self.selected_agent = index;
                if let Some(agent) = self.document.as_ref().and_then(|d| d.agents.get(index)) {
                    self.portable.soul = text_editor::Content::with_text(&agent.soul);
                }
            }
            Message::AddAgent => {
                if let Some(document) = &mut self.document {
                    document.agents.push(AgentDocument::default());
                    self.selected_agent = document.agents.len() - 1;
                    self.portable.soul =
                        text_editor::Content::with_text(&document.agents[self.selected_agent].soul);
                }
            }
            Message::RemoveAgent => {
                if let Some(document) = &mut self.document
                    && !document.agents.is_empty()
                {
                    let removed = document.agents.remove(self.selected_agent);
                    self.selected_agent = self
                        .selected_agent
                        .min(document.agents.len().saturating_sub(1));
                    if document.runtime.agent_name.as_deref() == Some(&removed.name) {
                        document.runtime.agent_name = None;
                    }
                    self.portable.soul = text_editor::Content::with_text(
                        document
                            .agents
                            .get(self.selected_agent)
                            .map(|a| a.soul.as_str())
                            .unwrap_or(""),
                    );
                }
            }
            Message::AgentName(value) => self.agent_mut(|item| item.name = value),
            Message::AgentDescription(value) => self.agent_mut(|item| item.description = value),
            Message::AgentModel(value) => self.agent_mut(|item| item.model = non_empty(value)),
            Message::AgentToolGroups(value) => self.agent_mut(|item| {
                item.tool_groups = comma_list(&value);
                item.tool_groups_input = value;
            }),
            Message::AgentSkills(value) => self.agent_mut(|item| {
                item.skills = if value.trim() == "*" {
                    None
                } else {
                    Some(comma_list(&value))
                };
                item.skills_input = value;
            }),
            Message::SubagentsEnabled(value) => self.subagents_mut(|item| item.enabled = value),
            Message::SubagentsTimeout(value) => self.subagents_mut(|item| {
                if let Ok(parsed) = value.parse() {
                    item.timeout_seconds = parsed;
                }
                item.timeout_input = value;
            }),
            Message::SubagentsMaxTurns(value) => self.subagents_mut(|item| {
                if value.trim().is_empty() {
                    item.max_turns = None;
                } else if let Ok(parsed) = value.parse() {
                    item.max_turns = Some(parsed);
                }
                item.max_turns_input = value;
            }),
            Message::SubagentsAgentsEdit(action) => self.subagents_mut(|item| {
                item.agents_editor.perform(action);
                if let Ok(value) = parse_json_object(&item.agents_editor.text(), "Subagents agents")
                {
                    item.agents = value;
                }
            }),
            Message::SubagentsCustomEdit(action) => self.subagents_mut(|item| {
                item.custom_agents_editor.perform(action);
                if let Ok(value) =
                    parse_json_object(&item.custom_agents_editor.text(), "Subagents custom_agents")
                {
                    item.custom_agents = value;
                }
            }),
            Message::SubagentModel(kind, name, model) => {
                let result = if let Some(document) = &mut self.document {
                    document.sync_subagent_editors().and_then(|()| {
                        assign_subagent_model(&mut document.subagents, &kind, &name, &model)?;
                        document.refresh_subagent_editors();
                        Ok(())
                    })
                } else {
                    Ok(())
                };
                if let Err(error) = result {
                    self.error = Some(error);
                } else {
                    self.error = None;
                    self.notice = Some("Subagent 模型分配已更新，保存配置后生效".into());
                }
            }
            Message::SandboxAllowHostBash(value) => {
                self.sandbox_mut(|item| item.allow_host_bash = value)
            }
            Message::SandboxAllowHostTools(value) => {
                self.sandbox_mut(|item| item.allow_host_tools = value)
            }
            Message::SandboxAdvancedEdit(action) => self.sandbox_mut(|item| {
                item.advanced_editor.perform(action);
                if let Ok(value) =
                    parse_json_object(&item.advanced_editor.text(), "Sandbox 高级配置")
                {
                    item.advanced = value;
                }
            }),
            Message::ToolGroupsEdit(action) => {
                if let Some(document) = &mut self.document {
                    document.tool_groups_editor.perform(action);
                    if let Ok(value) =
                        parse_json_object_array(&document.tool_groups_editor.text(), "Tool Groups")
                    {
                        document.tool_groups = value;
                    }
                }
            }
            Message::ToolsEdit(action) => {
                if let Some(document) = &mut self.document {
                    document.tools_editor.perform(action);
                    if let Ok(value) =
                        parse_json_object_array(&document.tools_editor.text(), "Tools")
                    {
                        document.tools = value;
                    }
                }
            }
            Message::RuntimeToolAllowlist(value) => self.runtime_mut(|item| {
                item.tool_allowlist = if value.trim() == "*" {
                    None
                } else {
                    Some(comma_list(&value))
                };
                item.tool_allowlist_input = value;
            }),
            Message::RuntimeToolDenylist(value) => self.runtime_mut(|item| {
                item.tool_denylist = comma_list(&value);
                item.tool_denylist_input = value;
            }),
            Message::SkillsEnabled(value) => {
                if let Some(document) = &mut self.document {
                    document.skills_enabled = value;
                }
            }
            Message::SkillEnabled(index, value) => {
                if let Some(document) = &mut self.document
                    && let Some(skill) = document.skills.get_mut(index)
                {
                    skill.enabled = value;
                }
            }
            Message::EvolutionEnabled(value) => self.evolution_mut(|item| item.enabled = value),
            Message::EvolutionMode(value) => self.evolution_mut(|item| item.mode = value),
            Message::EvolutionStoragePath(value) => {
                self.evolution_mut(|item| item.storage_path = value)
            }
            Message::EvolutionGenerationModel(value) => {
                self.evolution_mut(|item| item.generation_model_name = non_empty(value))
            }
            Message::EvolutionModerationModel(value) => {
                self.evolution_mut(|item| item.moderation_model_name = non_empty(value))
            }
            Message::EvolutionEvaluationModel(value) => {
                self.evolution_mut(|item| item.evaluation_model_name = non_empty(value))
            }
            Message::EvolutionSecurityFailClosed(value) => {
                self.evolution_mut(|item| item.security_fail_closed = value)
            }
            Message::EvolutionDiscoveryEnabled(value) => {
                self.evolution_mut(|item| item.discovery.enabled = value)
            }
            Message::EvolutionMinToolCalls(value) => self.evolution_mut(|item| {
                if let Ok(parsed) = value.parse() {
                    item.discovery.min_tool_calls = parsed;
                }
                item.discovery.min_tool_calls_input = value;
            }),
            Message::EvolutionRepeatThreshold(value) => self.evolution_mut(|item| {
                if let Ok(parsed) = value.parse() {
                    item.discovery.repeat_threshold = parsed;
                }
                item.discovery.repeat_threshold_input = value;
            }),
            Message::EvolutionRepeatWindowDays(value) => self.evolution_mut(|item| {
                if let Ok(parsed) = value.parse() {
                    item.discovery.repeat_window_days = parsed;
                }
                item.discovery.repeat_window_days_input = value;
            }),
            Message::EvolutionCooldownHours(value) => self.evolution_mut(|item| {
                if let Ok(parsed) = value.parse() {
                    item.discovery.cooldown_hours = parsed;
                }
                item.discovery.cooldown_hours_input = value;
            }),
            Message::EvolutionMaxDailyProposals(value) => self.evolution_mut(|item| {
                if let Ok(parsed) = value.parse() {
                    item.discovery.max_daily_proposals = parsed;
                }
                item.discovery.max_daily_proposals_input = value;
            }),
            Message::EvolutionMaxPendingProposals(value) => self.evolution_mut(|item| {
                if let Ok(parsed) = value.parse() {
                    item.discovery.max_pending_proposals = parsed;
                }
                item.discovery.max_pending_proposals_input = value;
            }),
            Message::EvolutionMaxFiles(value) => self.evolution_mut(|item| {
                if let Ok(parsed) = value.parse() {
                    item.candidate_limits.max_files = parsed;
                }
                item.candidate_limits.max_files_input = value;
            }),
            Message::EvolutionMaxTotalBytes(value) => self.evolution_mut(|item| {
                if let Ok(parsed) = value.parse() {
                    item.candidate_limits.max_total_bytes = parsed;
                }
                item.candidate_limits.max_total_bytes_input = value;
            }),
            Message::EvolutionMaxFileBytes(value) => self.evolution_mut(|item| {
                if let Ok(parsed) = value.parse() {
                    item.candidate_limits.max_file_bytes = parsed;
                }
                item.candidate_limits.max_file_bytes_input = value;
            }),
            Message::EvolutionMaxChangedLines(value) => self.evolution_mut(|item| {
                if let Ok(parsed) = value.parse() {
                    item.auto_patch.max_changed_lines = parsed;
                }
                item.auto_patch.max_changed_lines_input = value;
            }),
            Message::EvolutionProbationUses(value) => self.evolution_mut(|item| {
                if let Ok(parsed) = value.parse() {
                    item.monitoring.probation_uses = parsed;
                }
                item.monitoring.probation_uses_input = value;
            }),
            Message::EvolutionRollbackFailures(value) => self.evolution_mut(|item| {
                if let Ok(parsed) = value.parse() {
                    item.monitoring.auto_rollback_consecutive_failures = parsed;
                }
                item.monitoring.rollback_failures_input = value;
            }),
            Message::RefreshEvolutionProposals => {
                self.proposal_error = None;
                self.proposal_notice = None;
                return self.proposal_list_task();
            }
            Message::EvolutionProposalsLoaded(result) => {
                self.proposal_busy = false;
                match result {
                    Ok(data) => {
                        self.proposal_catalog_version = data.catalog_version;
                        self.evolution_proposals = data.proposals;
                        if let Some(selected) = &self.selected_evolution_proposal
                            && !self
                                .evolution_proposals
                                .iter()
                                .any(|proposal| proposal.id == selected.id)
                        {
                            self.selected_evolution_proposal = None;
                            self.proposal_diff_editor = text_editor::Content::new();
                            self.proposal_confirmation = None;
                        }
                    }
                    Err(error) => {
                        self.evolution_proposals.clear();
                        self.selected_evolution_proposal = None;
                        self.proposal_diff_editor = text_editor::Content::new();
                        self.proposal_confirmation = None;
                        self.proposal_error = Some(friendly_management_error(error));
                    }
                }
            }
            Message::SelectEvolutionProposal(proposal_id) => {
                let Some(proposal) = self
                    .evolution_proposals
                    .iter()
                    .find(|proposal| proposal.id == proposal_id)
                    .cloned()
                else {
                    return Task::none();
                };
                self.selected_evolution_proposal = Some(proposal);
                self.proposal_diff_editor = text_editor::Content::new();
                self.proposal_review_note.clear();
                self.proposal_confirmation = None;
                self.proposal_error = None;
                self.proposal_notice = None;
                self.proposal_busy = true;
                let paths = self.paths.clone();
                return Task::perform(
                    async move { load_evolution_proposal(&paths, &proposal_id) },
                    Message::EvolutionProposalLoaded,
                );
            }
            Message::EvolutionProposalLoaded(result) => {
                self.proposal_busy = false;
                match result {
                    Ok(proposal) => {
                        self.proposal_diff_editor =
                            text_editor::Content::with_text(if proposal.diff.is_empty() {
                                "（此 Proposal 没有文本差异）"
                            } else {
                                &proposal.diff
                            });
                        self.selected_evolution_proposal = Some(proposal);
                    }
                    Err(error) => {
                        self.proposal_error = Some(friendly_management_error(error));
                    }
                }
            }
            Message::ProposalReviewNote(value) => self.proposal_review_note = value,
            Message::RequestProposalReview(action) => {
                if self
                    .selected_evolution_proposal
                    .as_ref()
                    .is_some_and(|proposal| proposal.status == "pending_review")
                    && !self.proposal_busy
                {
                    self.proposal_confirmation = Some(action);
                    self.proposal_error = None;
                    self.proposal_notice = None;
                }
            }
            Message::CancelProposalReview => self.proposal_confirmation = None,
            Message::ConfirmProposalReview => {
                let (Some(action), Some(proposal)) = (
                    self.proposal_confirmation,
                    self.selected_evolution_proposal.as_ref(),
                ) else {
                    return Task::none();
                };
                if proposal.status != "pending_review" {
                    self.proposal_confirmation = None;
                    return Task::none();
                }
                let paths = self.paths.clone();
                let proposal_id = proposal.id.clone();
                let expected_base_sha256 = proposal.base_sha256.clone();
                let note = self.proposal_review_note.clone();
                self.proposal_busy = true;
                self.proposal_confirmation = None;
                self.proposal_error = None;
                self.proposal_notice = Some(format!("正在{}…", action.label()));
                return Task::perform(
                    async move {
                        review_evolution_proposal(
                            &paths,
                            action,
                            &proposal_id,
                            expected_base_sha256.as_deref(),
                            &note,
                        )
                    },
                    Message::EvolutionProposalReviewed,
                );
            }
            Message::EvolutionProposalReviewed(result) => {
                self.proposal_busy = false;
                self.selected_evolution_proposal = None;
                self.proposal_diff_editor = text_editor::Content::new();
                self.proposal_review_note.clear();
                self.proposal_confirmation = None;
                match result {
                    Ok(data) => {
                        self.proposal_catalog_version = data.catalog_version;
                        self.proposal_error = None;
                        self.proposal_notice = Some(format!(
                            "{} · {} 已完成（状态：{}）",
                            data.proposal.skill_name, data.proposal.action, data.proposal.status
                        ));
                    }
                    Err(error) => {
                        self.proposal_notice = None;
                        self.proposal_error = Some(friendly_management_error(error));
                    }
                }
                return self.proposal_list_task();
            }
            Message::RuntimeModel(value) => {
                self.runtime_mut(|item| item.model_name = non_empty(value))
            }
            Message::RuntimeAgent(value) => {
                self.runtime_mut(|item| item.agent_name = non_empty(value))
            }
            Message::RuntimeThinking(value) => {
                self.runtime_mut(|item| item.thinking_enabled = value)
            }
            Message::RuntimePlan(value) => self.runtime_mut(|item| item.plan_mode = value),
            Message::RuntimeSubagents(value) => {
                self.runtime_mut(|item| item.subagent_enabled = value)
            }
            Message::RuntimeBash(value) => self.runtime_mut(|item| item.enable_bash = value),
            Message::RuntimePermission(value) => self.runtime_mut(|item| {
                item.permission_mode = match value.as_str() {
                    "不请求审批" => "off".into(),
                    "所有操作请求审批" => "all".into(),
                    "危险操作请求审批" => "dangerous".into(),
                    _ => value,
                }
            }),
            Message::RuntimeMemory(value) => self.runtime_mut(|item| item.memory_scope = value),
            Message::RuntimeConnections(value) => self.runtime_mut(|item| {
                if let Ok(parsed) = value.parse() {
                    item.max_active_connections = parsed;
                }
                item.connections_input = value;
            }),
            Message::RuntimeRuns(value) => self.runtime_mut(|item| {
                if let Ok(parsed) = value.parse() {
                    item.max_active_runs = parsed;
                }
                item.runs_input = value;
            }),
            Message::RuntimeSubagentCount(value) => self.runtime_mut(|item| {
                if let Ok(parsed) = value.parse() {
                    item.max_concurrent_subagents = parsed;
                }
                item.subagent_count_input = value;
            }),
            Message::RuntimeTimeout(value) => self.runtime_mut(|item| {
                if let Ok(parsed) = value.parse() {
                    item.run_timeout_seconds = parsed;
                }
                item.timeout_input = value;
            }),
            Message::Tick
            | Message::RefreshStatus
            | Message::WindowClosed(_)
            | Message::ConfirmAction
            | Message::CancelAction
            | Message::SaveApply
            | Message::ApplyWhenIdle
            | Message::ApplyNow
            | Message::CancelApply
            | Message::DrainChecked(_)
            | Message::ToggleAdvanced(_)
            | Message::LocateValidation(_)
            | Message::ScrollToValidation(_, _)
            | Message::ReplacementModel(_)
            | Message::DeleteModel(_, _)
            | Message::RemoveModel
            | Message::SkillSearch(_)
            | Message::RuntimeExtra(_)
            | Message::RuntimeOverlayEdit(_)
            | Message::Portable(_) => {}
            Message::OpenFolder(path) => {
                if let Err(error) = open_folder(&path) {
                    self.error = Some(error);
                }
            }
        }
        Task::none()
    }

    fn daemon_task(&mut self, command: DaemonCommand) -> Task<Message> {
        self.daemon = DaemonStatus::Checking;
        self.busy = true;
        self.error = None;
        let paths = self.paths.clone();
        Task::perform(
            async move { daemon_action(&paths, command) },
            Message::DaemonActionFinished,
        )
    }

    fn proposal_list_task(&mut self) -> Task<Message> {
        self.proposal_busy = true;
        let paths = self.paths.clone();
        Task::perform(
            async move { load_pending_evolution_proposals(&paths) },
            Message::EvolutionProposalsLoaded,
        )
    }

    fn model_mut(&mut self, update: impl FnOnce(&mut ModelDocument)) {
        if let Some(document) = &mut self.document
            && let Some(model) = document.models.get_mut(self.selected_model)
        {
            update(model);
        }
    }

    fn agent_mut(&mut self, update: impl FnOnce(&mut AgentDocument)) {
        if let Some(document) = &mut self.document
            && let Some(agent) = document.agents.get_mut(self.selected_agent)
        {
            update(agent);
        }
    }

    fn subagents_mut(&mut self, update: impl FnOnce(&mut SubagentsDocument)) {
        if let Some(document) = &mut self.document {
            update(&mut document.subagents);
        }
    }

    fn sandbox_mut(&mut self, update: impl FnOnce(&mut SandboxDocument)) {
        if let Some(document) = &mut self.document {
            update(&mut document.sandbox);
        }
    }

    fn runtime_mut(&mut self, update: impl FnOnce(&mut RuntimeDocument)) {
        if let Some(document) = &mut self.document {
            update(&mut document.runtime);
        }
    }

    fn memory_mut(&mut self, update: impl FnOnce(&mut MemoryDocument)) {
        if let Some(document) = &mut self.document {
            update(&mut document.memory);
        }
    }

    fn evolution_mut(&mut self, update: impl FnOnce(&mut SkillEvolutionDocument)) {
        if let Some(document) = &mut self.document {
            update(&mut document.skill_evolution);
        }
    }

    fn view(&self) -> Element<'_, Message> {
        let draft_error = self.validation_issue();
        let sidebar =
            Page::ALL
                .into_iter()
                .fold(column![].spacing(4), |mut menu, (page, label)| {
                    let section = match page {
                        Page::Models => Some("配置与能力"),
                        Page::Runtime => Some("运行与维护"),
                        _ => None,
                    };
                    if let Some(label) = section {
                        menu = menu.push(
                            container(text(label).size(11).color(ui::TEXT_MUTED)).padding(
                                iced::Padding {
                                    top: 16.0,
                                    right: 12.0,
                                    bottom: 6.0,
                                    left: 12.0,
                                },
                            ),
                        );
                    }
                    menu.push(ui::sidebar_item(
                        page.icon(),
                        label,
                        self.page == page,
                        Message::Navigate(page),
                    ))
                });
        let sidebar = container(
            column![
                container(
                    column![
                        text("DeerFlow").size(25).color(ui::TEXT_PRIMARY),
                        text("便携 ACP 工作台").size(12).color(ui::TEXT_SECONDARY),
                    ]
                    .spacing(5)
                )
                .padding([12, 12]),
                scrollable(sidebar).height(Fill),
                rule::horizontal(1),
                container(
                    row![
                        ui::icon_view(ui::Icon::Folder, 13.0, ui::TEXT_MUTED),
                        text("配置保存在便携目录").size(11).color(ui::TEXT_MUTED),
                    ]
                    .spacing(7)
                    .align_y(iced::Alignment::Center)
                )
                .padding([8, 12]),
            ]
            .spacing(10)
            .padding(12),
        )
        .style(ui::sidebar)
        .width(Length::Fixed(208.0))
        .height(Fill);

        let (daemon_color, daemon_icon) = match &self.daemon {
            DaemonStatus::Running(details) if details.contains("warmup=failed") => {
                (ui::DANGER, ui::Icon::Alert)
            }
            DaemonStatus::Running(details) if details.contains("warmup=warming") => {
                (ui::ACCENT, ui::Icon::Activity)
            }
            DaemonStatus::Running(_) => (ui::SUCCESS, ui::Icon::Check),
            DaemonStatus::Stopped => (ui::TEXT_MUTED, ui::Icon::Stop),
            DaemonStatus::Checking => (ui::ACCENT, ui::Icon::Activity),
            DaemonStatus::Error(_) => (ui::DANGER, ui::Icon::Alert),
        };

        let status_actions = row![
            button(
                row![
                    ui::icon_view(ui::Icon::Refresh, 15.0, ui::TEXT_SECONDARY),
                    text("刷新状态"),
                ]
                .spacing(7)
                .align_y(iced::Alignment::Center),
            )
            .padding([8, 12])
            .style(ui::secondary_button)
            .on_press_maybe((!self.busy).then_some(Message::RefreshStatus)),
            button(
                row![
                    ui::icon_view(ui::Icon::Save, 15.0, ui::ACCENT_TEXT),
                    text("保存配置"),
                ]
                .spacing(7)
                .align_y(iced::Alignment::Center),
            )
            .padding([8, 13])
            .style(ui::primary_button)
            .on_press_maybe(
                (!self.busy && self.document.is_some() && self.dirty && draft_error.is_none())
                    .then_some(Message::Save),
            ),
        ]
        .spacing(9);
        let daemon_detail = self.daemon.detail_label();
        let status_detail = if self.busy {
            "正在执行操作，请稍候…".to_owned()
        } else if self.dirty {
            "存在未保存修改".to_owned()
        } else {
            daemon_detail
        };
        let status = container(
            column![
                row![
                    container(ui::page_header(self.page.title(), self.page.description()))
                        .width(Fill),
                    ui::status_pill(self.daemon.summary_label(), daemon_color, daemon_icon),
                ]
                .spacing(16)
                .align_y(iced::Alignment::Start),
                row![
                    text(status_detail)
                        .size(12)
                        .width(Fill)
                        .wrapping(text::Wrapping::None)
                        .color(if self.busy {
                            ui::ACCENT
                        } else if self.dirty {
                            ui::WARNING
                        } else {
                            ui::TEXT_MUTED
                        }),
                    status_actions,
                ]
                .spacing(16)
                .align_y(iced::Alignment::Center),
            ]
            .spacing(16),
        )
        .padding(20)
        .style(ui::topbar);

        let mut body = column![status].spacing(14);
        if let Some((page, error)) = draft_error {
            body = body.push(
                container(
                    row![
                        text(format!("请修正输入：{error}")).width(Fill),
                        ui::action_button("查看错误配置").on_press(Message::LocateValidation(page))
                    ]
                    .spacing(12),
                )
                .padding(10)
                .style(ui::error_callout),
            );
        }
        if self.pending_action.is_some() {
            body = body.push(
                container(
                    column![
                        text("请确认操作").size(18),
                        text(self.confirmation_text()),
                        self.delete_model_options(),
                        row![
                            ui::action_button("确认")
                                .on_press_maybe(
                                    self.confirmation_ready().then_some(Message::ConfirmAction)
                                )
                                .style(ui::danger_button),
                            ui::action_button("取消").on_press(Message::CancelAction)
                        ]
                        .spacing(12)
                    ]
                    .spacing(8),
                )
                .padding(12)
                .style(ui::warning_callout),
            );
        }
        if self.pending_apply || self.waiting_apply {
            body = body.push(
                row![
                    text(if self.waiting_apply {
                        "等待活动任务结束；新的任务已暂停接收"
                    } else {
                        "已保存，有待应用的配置"
                    })
                    .width(Fill),
                    ui::action_button("空闲后应用").on_press_maybe(
                        (!self.busy && !self.waiting_apply && !self.dirty)
                            .then_some(Message::ApplyWhenIdle)
                    ),
                    ui::action_button("立即重启…").on_press_maybe(
                        (!self.busy && !self.dirty)
                            .then_some(Message::Portable(portable::Message::ConfirmRestart))
                    ),
                    ui::action_button("取消等待")
                        .on_press_maybe(self.waiting_apply.then_some(Message::CancelApply)),
                ]
                .spacing(8),
            );
        }

        if let Some(error) = &self.error {
            body = body.push(
                container(
                    row![
                        ui::icon_view(ui::Icon::Alert, 17.0, ui::DANGER),
                        text(format!("操作未完成：{error}"))
                            .width(Fill)
                            .color(ui::TEXT_PRIMARY)
                            .wrapping(text::Wrapping::WordOrGlyph),
                        ui::action_button("重新加载")
                            .style(ui::danger_button)
                            .on_press_maybe((!self.busy).then_some(Message::Reload)),
                    ]
                    .spacing(10)
                    .align_y(iced::Alignment::Center),
                )
                .padding(11)
                .style(ui::error_callout),
            );
        } else if let Some(notice) = &self.notice {
            body = body.push(
                container(
                    row![
                        ui::icon_view(ui::Icon::Check, 17.0, ui::SUCCESS),
                        text(notice).color(ui::TEXT_PRIMARY),
                    ]
                    .spacing(9)
                    .align_y(iced::Alignment::Center),
                )
                .padding(11)
                .style(ui::success_callout),
            );
        }
        let page = match self.page {
            Page::Dashboard => self.dashboard_view(),
            Page::Models => self.models_view(),
            Page::Agents => self.agents_view(),
            Page::Memory => self.memory_view(),
            Page::Skills => self.skills_view(),
            Page::SandboxTools => self.sandbox_tools_view(),
            Page::Runtime => self.runtime_view(),
            Page::Diagnostics => self.diagnostics_view(),
            Page::Data => self.data_view(),
        };
        body = body.push(page);
        container(row![
            sidebar,
            container(body.padding(18)).width(Fill).height(Fill)
        ])
        .width(Fill)
        .height(Fill)
        .style(ui::app_background)
        .into()
    }

    fn confirmation_text(&self) -> String {
        match self.pending_action.as_ref() {
            Some(Message::DeleteModel(name, _)) => format!("删除模型 {}？", name),
            Some(Message::Reload) => "重新加载会丢弃所有未保存修改。".into(),
            Some(Message::WindowClosed(_)) => {
                "关闭会丢弃未保存修改。服务继续在后台运行；正在应用配置时请先等待或取消。".into()
            }
            Some(Message::ApplyNow) => "立即重启将中断此便携实例的所有活动 ACP 任务。".into(),
            Some(Message::StopDaemon) => "停止服务将中断此便携实例的所有活动 ACP 任务。".into(),
            Some(Message::Portable(msg)) => self.portable.confirmation(msg),
            _ => "确认执行此操作？".into(),
        }
    }

    fn dashboard_view(&self) -> Element<'_, Message> {
        let Some(document) = &self.document else {
            return container(
                row![
                    ui::icon_view(ui::Icon::Activity, 20.0, ui::ACCENT),
                    text("正在初始化便携配置…")
                        .size(18)
                        .color(ui::TEXT_SECONDARY),
                ]
                .spacing(10)
                .align_y(iced::Alignment::Center),
            )
            .padding(20)
            .style(ui::accent_card)
            .into();
        };
        let activity_label = match &self.daemon {
            DaemonStatus::Running(details) => {
                let field = |key: &str| {
                    details
                        .split_whitespace()
                        .find_map(|p| p.strip_prefix(key))
                        .unwrap_or("?")
                };
                format!(
                    "当前执行 {} · 排队 {} · 状态每 5 秒刷新",
                    field("active="),
                    field("queued=")
                )
            }
            _ => "服务未就绪；配置可先保存".into(),
        };
        let enabled_skills = if document.skills_enabled {
            document.skills.iter().filter(|skill| skill.enabled).count()
        } else {
            0
        };
        let cards = row![
            ui::metric(
                ui::Icon::Models,
                "模型",
                document.models.len().to_string(),
                "已配置 Provider"
            ),
            ui::metric(
                ui::Icon::Agents,
                "Agents",
                document.agents.len().to_string(),
                "Custom Agents"
            ),
            ui::metric(
                ui::Icon::Skills,
                "Skills",
                format!("{enabled_skills}/{}", document.skills.len()),
                "启用 / 总数"
            ),
            ui::metric(
                ui::Icon::Tools,
                "Tools",
                document.tools.len().to_string(),
                "本地工具定义"
            ),
        ]
        .spacing(14);
        let daemon_running = self.daemon.is_running();
        let daemon_checking = matches!(self.daemon, DaemonStatus::Checking);
        let controls = row![
            button(
                row![
                    ui::icon_view(ui::Icon::Play, 16.0, ui::ACCENT_TEXT),
                    text("启动 Daemon"),
                ]
                .spacing(8)
                .align_y(iced::Alignment::Center),
            )
            .padding([9, 14])
            .style(ui::success_button)
            .on_press_maybe(
                (!self.busy && !self.dirty && !daemon_running && !daemon_checking)
                    .then_some(Message::StartDaemon),
            ),
            button(
                row![
                    ui::icon_view(ui::Icon::Stop, 16.0, ui::DANGER),
                    text("停止 Daemon"),
                ]
                .spacing(8)
                .align_y(iced::Alignment::Center),
            )
            .padding([9, 14])
            .style(ui::danger_button)
            .on_press_maybe(
                (!self.busy && daemon_running)
                    .then_some(Message::Portable(portable::Message::ConfirmStop))
            ),
            button(
                row![
                    ui::icon_view(ui::Icon::Restart, 16.0, ui::WARNING),
                    text("重启 Daemon"),
                ]
                .spacing(8)
                .align_y(iced::Alignment::Center),
            )
            .padding([9, 14])
            .style(ui::warning_button)
            .on_press_maybe(
                (!self.busy && !self.dirty && daemon_running).then_some(Message::ApplyWhenIdle),
            ),
        ]
        .spacing(12);
        let daemon_control = ui::section(
            "Daemon 控制",
            "保存不中断服务。应用配置时可等待活动任务结束；客户端连接由你在客户端验收。",
            column![
                controls,
                container(
                    row![
                        ui::icon_view(ui::Icon::Alert, 16.0, ui::WARNING),
                        text("停止或重启会影响当前正在使用此便携实例的 ACP 会话。")
                            .size(12)
                            .color(ui::TEXT_SECONDARY),
                    ]
                    .spacing(8)
                    .align_y(iced::Alignment::Center),
                )
                .padding(10)
                .style(ui::warning_callout),
            ]
            .spacing(12),
        );
        scrollable(
            column![
                self.onboarding_view(),
                text(activity_label),
                cards,
                daemon_control,
                self.effective_view()
            ]
            .spacing(16),
        )
        .height(Fill)
        .into()
    }

    fn models_view(&self) -> Element<'_, Message> {
        let Some(document) = &self.document else {
            return text("尚未加载配置").into();
        };
        let choices = document
            .models
            .iter()
            .enumerate()
            .fold(
                column![text("模型列表").size(17).color(ui::TEXT_PRIMARY)].spacing(8),
                |list, (index, model)| {
                    list.push(
                        button(
                            row![
                                column![
                                    text(if model.display_name.trim().is_empty() {
                                        &model.name
                                    } else {
                                        &model.display_name
                                    })
                                    .size(14),
                                    text(&model.name).size(11).color(ui::TEXT_SECONDARY),
                                ]
                                .spacing(4)
                                .width(Fill),
                                text(if document.default_model == model.name {
                                    "默认"
                                } else {
                                    ""
                                })
                                .size(11)
                                .color(ui::ACCENT),
                            ]
                            .spacing(8)
                            .align_y(iced::Alignment::Center),
                        )
                        .on_press(Message::SelectModel(index))
                        .padding([9, 11])
                        .style(ui::list_button(index == self.selected_model))
                        .width(Fill),
                    )
                },
            )
            .push(
                ui::action_button("＋ 新增模型")
                    .padding([8, 11])
                    .style(ui::secondary_button)
                    .on_press(Message::AddModel),
            );
        let Some(model) = document.models.get(self.selected_model) else {
            return row![
                container(choices).padding(14).width(230).style(ui::card),
                container(text("请选择模型").color(ui::TEXT_SECONDARY))
                    .padding(18)
                    .width(Fill)
                    .style(ui::card),
            ]
            .spacing(18)
            .into();
        };
        let key_hint = if model.api_key_literal && model.api_key.is_empty() {
            "已保存密钥（留空则保留）"
        } else {
            "$ENV、${ENV:-default} 或新密钥"
        };
        let form = column![
            row![
                text(model_label(document, &model.name))
                    .size(20)
                    .width(Fill),
                ui::action_button(if document.default_model == model.name {
                    "已是默认"
                } else {
                    "设为默认"
                })
                .on_press_maybe(
                    (!self.busy && document.default_model != model.name)
                        .then(|| Message::DefaultModel(model.name.clone()))
                ),
            ]
            .spacing(12)
            .align_y(iced::Alignment::Center),
            text(format!(
                "引用位置：{}；重命名将同步更新这些引用",
                model_references(document, &model.name).join("、")
            ))
            .size(12),
            row![
                pick_list(
                    vec![
                        "OpenAI / 兼容服务".to_owned(),
                        "Anthropic".to_owned(),
                        "DeepSeek".to_owned()
                    ],
                    self.portable.provider.clone(),
                    |v| Message::Portable(portable::Message::Provider(v))
                )
                .placeholder("服务商预设"),
                ui::action_button("测试模型").on_press_maybe(
                    (!self.busy).then_some(Message::Portable(portable::Message::TestModel))
                ),
            ]
            .spacing(10),
            text("点击测试将发送一次最小文本请求，可能产生少量费用；不会保存或启动服务。").size(12),
            labeled_input("名称", &model.name, "唯一名称", Message::ModelName),
            labeled_input(
                "显示名称",
                &model.display_name,
                "可选",
                Message::ModelDisplayName
            ),
            labeled_input("模型 ID", &model.model, "模型服务端 ID", Message::ModelId),
            labeled_input(
                "Base URL",
                &model.base_url,
                "https://…/v1",
                Message::ModelBaseUrl
            ),
            column![
                text("API Key").size(13),
                text_input(key_hint, &model.api_key)
                    .on_input(Message::ModelApiKey)
                    .style(ui::input_style)
                    .secure(true),
                checkbox(model.clear_api_key)
                    .label("清除已保存的 API Key")
                    .style(ui::checkbox_style)
                    .on_toggle(Message::ModelClearKey),
            ]
            .spacing(6),
            labeled_input(
                "说明",
                &model.description,
                "可选",
                Message::ModelDescription
            ),
            row![
                checkbox(model.supports_thinking)
                    .label("Thinking")
                    .style(ui::checkbox_style)
                    .on_toggle(Message::ModelThinking),
                checkbox(model.supports_reasoning_effort)
                    .label("Reasoning effort")
                    .style(ui::checkbox_style)
                    .on_toggle(Message::ModelReasoning),
                checkbox(model.supports_vision)
                    .label("Vision")
                    .style(ui::checkbox_style)
                    .on_toggle(Message::ModelVision),
            ]
            .spacing(16),
            self.advanced_panel(
                Page::Models,
                labeled_input(
                    "Provider 类",
                    &model.use_path,
                    "langchain_openai:ChatOpenAI",
                    Message::ModelUse
                )
            ),
            rule::horizontal(1),
            ui::action_button("删除当前模型")
                .style(ui::danger_button)
                .on_press_maybe(
                    (!self.busy && document.models.len() > 1).then_some(Message::RemoveModel)
                ),
        ]
        .spacing(12);
        column![
            self.default_model_summary(document),
            row![
                container(scrollable(choices).spacing(6))
                    .padding(14)
                    .style(ui::card)
                    .width(230)
                    .height(Fill),
                scrollable(container(form).padding(18).style(ui::card))
                    .id("page-Models")
                    .spacing(8)
                    .width(Fill)
                    .height(Fill),
            ]
            .spacing(20)
            .height(Fill)
        ]
        .spacing(14)
        .height(Fill)
        .into()
    }

    fn agents_view(&self) -> Element<'_, Message> {
        let Some(document) = &self.document else {
            return text("尚未加载配置").into();
        };
        let subagents = &document.subagents;
        let assignments = subagent_assignments(subagents).into_iter().fold(
            column![].spacing(10),
            |list, assignment| {
                let selected = assignment.model.clone();
                let choices = subagent_model_choices(document, &selected);
                let kind = assignment.kind.clone();
                let name = assignment.name.clone();
                list.push(
                    container(
                        row![
                            column![
                                row![
                                    text(assignment.name.clone())
                                        .size(16)
                                        .color(ui::TEXT_PRIMARY),
                                    text(assignment.kind_label.clone())
                                        .size(12)
                                        .color(ui::ACCENT),
                                ]
                                .spacing(10)
                                .align_y(iced::Alignment::Center),
                                text(assignment.description.clone())
                                    .size(12)
                                    .color(ui::TEXT_SECONDARY)
                                    .width(Fill),
                            ]
                            .spacing(4)
                            .width(Fill),
                            column![
                                text("执行模型").size(12).color(ui::TEXT_SECONDARY),
                                pick_list(choices, Some(selected), move |model| {
                                    Message::SubagentModel(kind.clone(), name.clone(), model)
                                })
                                .style(ui::pick_list_style),
                            ]
                            .spacing(5)
                            .width(Length::Fixed(260.0)),
                        ]
                        .spacing(16)
                        .align_y(iced::Alignment::Center),
                    )
                    .padding(12)
                    .style(ui::inset_card),
                )
            },
        );
        let assignments = if subagent_assignments(subagents).is_empty() {
            column![text("暂无可配置的 Subagent。").size(13)]
        } else {
            assignments
        };

        let subagent_settings = column![
            text("Subagents 配置").size(21).color(ui::TEXT_PRIMARY),
            text("对应 Admin 的 Subagents 全局配置、模型分配和高级 JSON。全局开关与 Runtime / ACP 页的会话级 Subagents 开关需同时启用。")
                .size(13)
                .color(ui::TEXT_SECONDARY),
            row![
                checkbox(subagents.enabled)
                    .label("启用 Subagent 系统")
                    .style(ui::checkbox_style)
                    .on_toggle(Message::SubagentsEnabled),
                labeled_input(
                    "默认超时（秒）",
                    &subagents.timeout_input,
                    "900",
                    Message::SubagentsTimeout
                ),
                labeled_input(
                    "默认最大轮次（留空使用内置值）",
                    &subagents.max_turns_input,
                    "",
                    Message::SubagentsMaxTurns
                ),
            ]
            .spacing(14)
            .align_y(iced::Alignment::Center),
            text("Subagent 模型分配").size(20),
            assignments,
            text("高级 JSON 配置").size(20),
            text("可编辑内置 Agent 覆盖和自定义 Subagent；JSON 必须是对象，保存时会使用 DeerFlow 原生配置模型校验。")
                .size(13),
            row![
                column![
                    text("内置 Agent 覆盖（agents）").size(13),
                    text_editor(&subagents.agents_editor)
                        .placeholder("{}")
                        .on_action(Message::SubagentsAgentsEdit)
                        .height(Length::Fixed(180.0)),
                ]
                .spacing(6)
                .width(Fill),
                column![
                    text("自定义 Subagent（custom_agents）").size(13),
                    text_editor(&subagents.custom_agents_editor)
                        .placeholder("{}")
                        .on_action(Message::SubagentsCustomEdit)
                        .height(Length::Fixed(180.0)),
                ]
                .spacing(6)
                .width(Fill),
            ]
            .spacing(14),
        ]
        .spacing(14);

        let choices = document
            .agents
            .iter()
            .enumerate()
            .fold(
                column![text("Custom Agents").size(18)].spacing(8),
                |list, (index, agent)| {
                    list.push(
                        button(text(&agent.name))
                            .on_press(Message::SelectAgent(index))
                            .padding([9, 11])
                            .style(ui::list_button(index == self.selected_agent))
                            .width(Fill),
                    )
                },
            )
            .push(
                ui::action_button("＋ 新增 Agent")
                    .padding([8, 11])
                    .style(ui::secondary_button)
                    .on_press(Message::AddAgent),
            );
        let custom_agent_editor: Element<'_, Message> =
            if let Some(agent) = document.agents.get(self.selected_agent) {
                let model_names = model_choices(document, "继承全局默认");
                let form = column![
                    text("Custom Agent 配置").size(24),
                    labeled_input(
                        "名称",
                        &agent.name,
                        "letters, numbers, hyphen",
                        Message::AgentName
                    ),
                    labeled_input(
                        "说明",
                        &agent.description,
                        "Agent 用途",
                        Message::AgentDescription
                    ),
                    column![
                        text("执行模型").size(13),
                        pick_list(
                            model_names,
                            Some(selected_model_choice(
                                document,
                                &agent.model,
                                "继承全局默认"
                            )),
                            |choice: preferences::ModelChoice| Message::AgentModel(
                                choice.name.unwrap_or_default()
                            )
                        )
                        .style(ui::pick_list_style),
                    ]
                    .spacing(6),
                    labeled_input(
                        "Tool Groups",
                        &agent.tool_groups_input,
                        "逗号分隔",
                        Message::AgentToolGroups
                    ),
                    labeled_input(
                        "Skills",
                        &agent.skills_input,
                        "* 表示全部；空表示禁用全部",
                        Message::AgentSkills
                    ),
                    column![
                        text("SOUL.md").size(13),
                        text_editor(&self.portable.soul)
                            .on_action(|a| Message::Portable(portable::Message::Soul(a)))
                            .height(180),
                    ]
                    .spacing(6),
                    ui::action_button("删除当前 Agent")
                        .style(ui::danger_button)
                        .on_press(Message::RemoveAgent),
                ]
                .spacing(12);
                row![
                    container(choices)
                        .padding(14)
                        .width(230)
                        .style(ui::inset_card),
                    container(form).padding(4).width(Fill),
                ]
                .spacing(20)
                .into()
            } else {
                row![
                    container(choices)
                        .padding(14)
                        .width(230)
                        .style(ui::inset_card),
                    column![
                        text("还没有 Custom Agent").size(24),
                        text("点击左侧“新增 Agent”创建一个；ACP 也可以不指定 Agent。")
                    ]
                    .spacing(10)
                ]
                .spacing(20)
                .into()
            };

        let tabs = row![
            ui::action_button("Subagents")
                .padding([8, 13])
                .style(ui::tab_button(
                    self.agent_section == AgentSection::Subagents
                ))
                .on_press(Message::AgentSection(AgentSection::Subagents)),
            ui::action_button("Custom Agents")
                .padding([8, 13])
                .style(ui::tab_button(
                    self.agent_section == AgentSection::CustomAgents
                ))
                .on_press(Message::AgentSection(AgentSection::CustomAgents)),
        ]
        .spacing(8);
        let active: Element<'_, Message> = match self.agent_section {
            AgentSection::Subagents => container(subagent_settings)
                .padding(18)
                .style(ui::card)
                .width(Fill)
                .into(),
            AgentSection::CustomAgents => container(
                column![
                    text("Custom Agents").size(21).color(ui::TEXT_PRIMARY),
                    text("这些 Agent 以独立目录保存，可直接作为 ACP 的主 Agent；它们与任务型 Subagent 配置相互独立。")
                        .size(13)
                        .color(ui::TEXT_SECONDARY),
                    custom_agent_editor,
                ]
                .spacing(16),
            )
            .padding(18)
            .style(ui::card)
            .width(Fill)
            .into(),
        };
        column![tabs, scrollable(active).spacing(8).height(Fill)]
            .spacing(12)
            .into()
    }

    fn proposal_review_view(&self) -> Element<'_, Message> {
        let refresh = button(if self.proposal_busy {
            "正在读取…"
        } else {
            "刷新待审批"
        })
        .style(ui::secondary_button)
        .on_press_maybe((!self.proposal_busy).then_some(Message::RefreshEvolutionProposals));

        let proposal_list = if self.evolution_proposals.is_empty() {
            column![
                text(if self.proposal_busy {
                    "正在从 Daemon 读取 Proposal…"
                } else {
                    "当前没有待审批 Proposal"
                })
                .size(13)
            ]
        } else {
            self.evolution_proposals
                .iter()
                .fold(column![], |list, proposal| {
                    let selected = self
                        .selected_evolution_proposal
                        .as_ref()
                        .is_some_and(|item| item.id == proposal.id);
                    list.push(
                        button(
                            text(format!(
                                "{}{} · {} · {} · {}",
                                if selected { "● " } else { "" },
                                proposal.skill_name,
                                proposal.action,
                                proposal.risk,
                                proposal.created_at
                            ))
                            .width(Fill)
                            .wrapping(text::Wrapping::WordOrGlyph),
                        )
                        .width(Fill)
                        .padding([8, 10])
                        .style(ui::list_button(selected))
                        .on_press_maybe(
                            (!self.proposal_busy)
                                .then_some(Message::SelectEvolutionProposal(proposal.id.clone())),
                        ),
                    )
                })
                .spacing(7)
        };

        let detail: Element<'_, Message> = if let Some(proposal) = &self.selected_evolution_proposal
        {
            let scans =
                serde_json::to_string_pretty(&proposal.scans).unwrap_or_else(|_| "[]".into());
            let evaluation =
                serde_json::to_string_pretty(&proposal.evaluation).unwrap_or_else(|_| "{}".into());
            let trigger =
                serde_json::to_string_pretty(&proposal.trigger).unwrap_or_else(|_| "{}".into());
            let changed_files = if proposal.changed_files.is_empty() {
                "--".into()
            } else {
                proposal.changed_files.join(", ")
            };
            let confirmation: Element<'_, Message> =
                if let Some(action) = self.proposal_confirmation {
                    container(
                        column![
                            text(format!(
                                "确认{} {} / {}？",
                                action.label(),
                                proposal.skill_name,
                                proposal.id
                            ))
                            .size(16),
                            text(if action == ProposalReviewAction::Approve {
                                "批准会重新执行安全扫描，并在基线未变化时原子发布 Skill。"
                            } else {
                                "拒绝会保留 Proposal 和审计记录，但不会修改当前 Skill。"
                            })
                            .size(13),
                            row![
                                button(text(format!("确认{}", action.label())))
                                    .style(if action == ProposalReviewAction::Approve {
                                        ui::success_button
                                    } else {
                                        ui::danger_button
                                    })
                                    .on_press(Message::ConfirmProposalReview),
                                ui::action_button("取消")
                                    .style(ui::secondary_button)
                                    .on_press(Message::CancelProposalReview),
                            ]
                            .spacing(10),
                        ]
                        .spacing(8),
                    )
                    .padding(12)
                    .style(ui::warning_callout)
                    .into()
                } else {
                    container(column![]).into()
                };

            column![
                rule::horizontal(1),
                text(format!("{} / {}", proposal.skill_name, proposal.id))
                    .size(20)
                    .width(Fill)
                    .wrapping(text::Wrapping::WordOrGlyph),
                text(format!(
                    "状态 {} · 操作 {} · 风险 {} · 来源 {} · 作者 {}",
                    proposal.status,
                    proposal.action,
                    proposal.risk,
                    proposal.origin,
                    proposal.author
                ))
                .size(13)
                .width(Fill)
                .wrapping(text::Wrapping::WordOrGlyph),
                text(format!(
                    "创建 {} · 更新 {} · 基线 revision {}",
                    proposal.created_at,
                    proposal.updated_at,
                    proposal
                        .base_revision
                        .map(|value| value.to_string())
                        .unwrap_or_else(|| "--".into())
                ))
                .size(13)
                .width(Fill)
                .wrapping(text::Wrapping::WordOrGlyph),
                text(format!("原因：{}", proposal.reason))
                    .size(13)
                    .width(Fill)
                    .wrapping(text::Wrapping::WordOrGlyph),
                text(format!("改动文件：{changed_files}"))
                    .size(13)
                    .width(Fill)
                    .wrapping(text::Wrapping::WordOrGlyph),
                text(format!(
                    "Base SHA-256: {}\nCandidate SHA-256: {}",
                    proposal.base_sha256.as_deref().unwrap_or("--"),
                    proposal.candidate_sha256.as_deref().unwrap_or("--")
                ))
                .size(12)
                .width(Fill)
                .wrapping(text::Wrapping::WordOrGlyph),
                text(format!("触发信息：\n{trigger}"))
                    .size(12)
                    .width(Fill)
                    .wrapping(text::Wrapping::WordOrGlyph),
                text(format!("安全扫描：\n{scans}"))
                    .size(12)
                    .width(Fill)
                    .wrapping(text::Wrapping::WordOrGlyph),
                text(format!("质量评估：\n{evaluation}"))
                    .size(12)
                    .width(Fill)
                    .wrapping(text::Wrapping::WordOrGlyph),
                text("Diff").size(16),
                text_editor(&self.proposal_diff_editor).height(Length::Fixed(260.0)),
                if let Some(error) = &proposal.error {
                    text(format!("Proposal 错误：{error}"))
                        .size(13)
                        .width(Fill)
                        .wrapping(text::Wrapping::WordOrGlyph)
                } else {
                    text("").size(1)
                },
                column![
                    text("审批备注（可选，最多 4000 字符）").size(13),
                    text_input("说明批准或拒绝原因", &self.proposal_review_note)
                        .on_input(Message::ProposalReviewNote)
                        .style(ui::input_style),
                ]
                .spacing(6),
                row![
                    ui::action_button("批准并发布")
                        .style(ui::success_button)
                        .on_press_maybe(
                            (!self.proposal_busy && proposal.status == "pending_review").then_some(
                                Message::RequestProposalReview(ProposalReviewAction::Approve,)
                            ),
                        ),
                    ui::action_button("拒绝")
                        .style(ui::danger_button)
                        .on_press_maybe(
                            (!self.proposal_busy && proposal.status == "pending_review").then_some(
                                Message::RequestProposalReview(ProposalReviewAction::Reject,)
                            ),
                        ),
                ]
                .spacing(10),
                confirmation,
            ]
            .spacing(9)
            .into()
        } else {
            text("选择一条 Proposal 查看完整 diff、安全扫描和评估结果。")
                .size(13)
                .into()
        };

        let mut content = column![
            row![
                text("Proposal 审批").size(22),
                text(format!(
                    "待审批 {} · Skill catalog v{}",
                    self.evolution_proposals.len(),
                    self.proposal_catalog_version
                ))
                .size(13),
                refresh,
            ]
            .spacing(12)
            .align_y(iced::Alignment::Center),
            text("审批通过后 Daemon 会重新安全扫描候选并刷新当前进程的 Skill 缓存。Daemon 未运行时这里只显示连接提示，不会自动启动。")
                .size(13),
        ]
        .spacing(10);
        if let Some(error) = &self.proposal_error {
            content = content.push(
                text(format!("审批服务：{error}"))
                    .size(13)
                    .width(Fill)
                    .wrapping(text::Wrapping::WordOrGlyph),
            );
        }
        if let Some(notice) = &self.proposal_notice {
            content = content.push(text(notice).size(13));
        }
        content.push(proposal_list).push(detail).spacing(10).into()
    }

    fn skills_view(&self) -> Element<'_, Message> {
        let Some(document) = &self.document else {
            return text("尚未加载配置").into();
        };
        let evolution = &document.skill_evolution;
        let model_names = model_choices(document, "继承全局默认");
        let publication_notice = if evolution.mode == "auto_patch" {
            "Auto Patch 仅允许发布低风险的 SKILL.md 小范围修改；创建、脚本、支持文件和删除始终被安全锁禁止。"
        } else {
            "Review 模式下 Agent 只生成候选，可在本页 Proposal 审批区审查并批准后发布。"
        };

        let evolution_settings = column![
            labeled_input(
                "状态目录（相对 DEER_FLOW_HOME）",
                &evolution.storage_path,
                "skill-evolution",
                Message::EvolutionStoragePath
            ),
            text("模型分工").size(16),
            row![
                column![
                    text("候选生成模型").size(13),
                    pick_list(
                        model_names.clone(),
                        Some(selected_model_choice(
                            document,
                            &evolution.generation_model_name,
                            "继承全局默认"
                        )),
                        |choice: preferences::ModelChoice| Message::EvolutionGenerationModel(
                            choice.name.unwrap_or_default()
                        )
                    )
                    .style(ui::pick_list_style),
                ]
                .spacing(6)
                .width(Fill),
                column![
                    text("安全审核模型").size(13),
                    pick_list(
                        model_names.clone(),
                        Some(selected_model_choice(
                            document,
                            &evolution.moderation_model_name,
                            "继承全局默认"
                        )),
                        |choice: preferences::ModelChoice| Message::EvolutionModerationModel(
                            choice.name.unwrap_or_default()
                        )
                    )
                    .style(ui::pick_list_style),
                ]
                .spacing(6)
                .width(Fill),
                column![
                    text("质量评估模型").size(13),
                    pick_list(
                        model_names,
                        Some(selected_model_choice(
                            document,
                            &evolution.evaluation_model_name,
                            "继承全局默认"
                        )),
                        |choice: preferences::ModelChoice| Message::EvolutionEvaluationModel(
                            choice.name.unwrap_or_default()
                        )
                    )
                    .style(ui::pick_list_style),
                ]
                .spacing(6)
                .width(Fill),
            ]
            .spacing(12),
            rule::horizontal(1),
            row![
                text("自动发现").size(18),
                checkbox(evolution.discovery.enabled)
                    .label("启用自动发现")
                    .style(ui::checkbox_style)
                    .on_toggle(Message::EvolutionDiscoveryEnabled),
            ]
            .spacing(16)
            .align_y(iced::Alignment::Center),
            row![
                labeled_input(
                    "最少工具调用",
                    &evolution.discovery.min_tool_calls_input,
                    "5",
                    Message::EvolutionMinToolCalls
                ),
                labeled_input(
                    "重复阈值",
                    &evolution.discovery.repeat_threshold_input,
                    "2",
                    Message::EvolutionRepeatThreshold
                ),
                labeled_input(
                    "重复窗口（天）",
                    &evolution.discovery.repeat_window_days_input,
                    "30",
                    Message::EvolutionRepeatWindowDays
                ),
            ]
            .spacing(12),
            row![
                labeled_input(
                    "冷却时间（小时）",
                    &evolution.discovery.cooldown_hours_input,
                    "24",
                    Message::EvolutionCooldownHours
                ),
                labeled_input(
                    "每日候选上限",
                    &evolution.discovery.max_daily_proposals_input,
                    "5",
                    Message::EvolutionMaxDailyProposals
                ),
                labeled_input(
                    "待审候选上限",
                    &evolution.discovery.max_pending_proposals_input,
                    "20",
                    Message::EvolutionMaxPendingProposals
                ),
            ]
            .spacing(12),
            rule::horizontal(1),
            text("候选安全边界").size(18),
            row![
                labeled_input(
                    "最大文件数",
                    &evolution.candidate_limits.max_files_input,
                    "20",
                    Message::EvolutionMaxFiles
                ),
                labeled_input(
                    "总大小上限（bytes）",
                    &evolution.candidate_limits.max_total_bytes_input,
                    "524288",
                    Message::EvolutionMaxTotalBytes
                ),
                labeled_input(
                    "单文件上限（bytes）",
                    &evolution.candidate_limits.max_file_bytes_input,
                    "131072",
                    Message::EvolutionMaxFileBytes
                ),
                labeled_input(
                    "Auto Patch 最大改动行",
                    &evolution.auto_patch.max_changed_lines_input,
                    "40",
                    Message::EvolutionMaxChangedLines
                ),
            ]
            .spacing(12),
            text(format!(
                "安全锁：create={} · support_files={} · scripts={} · delete={}",
                evolution.auto_patch.allow_create,
                evolution.auto_patch.allow_support_files,
                evolution.auto_patch.allow_scripts,
                evolution.auto_patch.allow_delete
            ))
            .size(13),
            rule::horizontal(1),
            text("发布后观察与回滚").size(18),
            row![
                labeled_input(
                    "Probation 使用次数",
                    &evolution.monitoring.probation_uses_input,
                    "3",
                    Message::EvolutionProbationUses
                ),
                labeled_input(
                    "连续失败自动回滚阈值",
                    &evolution.monitoring.rollback_failures_input,
                    "2",
                    Message::EvolutionRollbackFailures
                ),
            ]
            .spacing(12),
        ]
        .spacing(12);

        let query = self.skill_search.to_lowercase();
        let skill_list = document
            .skills
            .iter()
            .enumerate()
            .filter(|(_, skill)| {
                format!("{} {} {}", skill.name, skill.category, skill.description)
                    .to_lowercase()
                    .contains(&query)
            })
            .fold(
                column![
                    text("Skill 状态").size(22),
                    text_input("搜索名称、分类或说明", &self.skill_search)
                        .on_input(Message::SkillSearch),
                    checkbox(document.skills_enabled)
                        .label("启用 Skill 系统")
                        .style(ui::checkbox_style)
                        .on_toggle(Message::SkillsEnabled),
                    text(
                        "这里只管理 Skill 启用状态；不管理 MCP。保存时会原样保留已有 mcpServers。"
                    )
                    .size(13),
                ]
                .spacing(12),
                |list, (index, skill)| {
                    list.push(
                        container(
                            column![
                                checkbox(skill.enabled)
                                    .label(format!("{}  [{}]", skill.name, skill.category))
                                    .style(ui::checkbox_style)
                                    .on_toggle(move |enabled| Message::SkillEnabled(
                                        index, enabled
                                    )),
                                text(&skill.description).size(13).color(ui::TEXT_SECONDARY),
                            ]
                            .spacing(4),
                        )
                        .padding(11)
                        .style(ui::inset_card),
                    )
                },
            );
        let tabs = row![
            ui::action_button("Proposal 审批")
                .padding([8, 13])
                .style(ui::tab_button(
                    self.skills_section == SkillsSection::Proposals
                ))
                .on_press(Message::SkillsSection(SkillsSection::Proposals)),
            ui::action_button("自进化设置")
                .padding([8, 13])
                .style(ui::tab_button(
                    self.skills_section == SkillsSection::Evolution
                ))
                .on_press(Message::SkillsSection(SkillsSection::Evolution)),
            ui::action_button("Skill 状态")
                .padding([8, 13])
                .style(ui::tab_button(
                    self.skills_section == SkillsSection::Catalog
                ))
                .on_press(Message::SkillsSection(SkillsSection::Catalog)),
        ]
        .spacing(8);
        let active: Element<'_, Message> = match self.skills_section {
            SkillsSection::Proposals => container(self.proposal_review_view())
                .padding(18)
                .style(ui::card)
                .width(Fill)
                .into(),
            SkillsSection::Evolution => container(
                column![
                    text("自进化设置").size(21),
                    checkbox(evolution.enabled)
                        .label("启用 Self Improving")
                        .style(ui::checkbox_style)
                        .on_toggle(Message::EvolutionEnabled),
                    text("发布模式"),
                    pick_list(
                        vec!["review".into(), "auto_patch".into()],
                        Some(evolution.mode.clone()),
                        Message::EvolutionMode
                    )
                    .style(ui::pick_list_style),
                    checkbox(evolution.security_fail_closed)
                        .label("安全扫描不可用时阻止候选")
                        .style(ui::checkbox_style)
                        .on_toggle(Message::EvolutionSecurityFailClosed),
                    text(publication_notice),
                    self.advanced_panel(Page::Skills, evolution_settings),
                ]
                .spacing(12),
            )
            .padding(18)
            .style(ui::card)
            .width(Fill)
            .into(),
            SkillsSection::Catalog => container(skill_list)
                .padding(18)
                .style(ui::card)
                .width(Fill)
                .into(),
        };
        column![
            tabs,
            scrollable(active).id("page-Skills").spacing(8).height(Fill)
        ]
        .spacing(12)
        .into()
    }

    fn sandbox_tools_view(&self) -> Element<'_, Message> {
        self.tools_simple_view()
    }

    fn tools_advanced_view(&self) -> Element<'_, Message> {
        let Some(document) = &self.document else {
            return text("配置未加载").into();
        };
        column![
            text("Local 挂载与输出限制").size(16),
            text("仅支持 mounts 和本地文件工具输出限制。"),
            text_editor(&document.sandbox.advanced_editor)
                .on_action(Message::SandboxAdvancedEdit)
                .height(220),
            text("工具组 JSON").size(16),
            text_editor(&document.tool_groups_editor)
                .on_action(Message::ToolGroupsEdit)
                .height(200),
            text("工具 JSON").size(16),
            text("敏感字段显示为 __DEERFLOW_REDACTED__；保留占位符会保留原值。"),
            text_editor(&document.tools_editor)
                .on_action(Message::ToolsEdit)
                .height(300),
        ]
        .spacing(12)
        .into()
    }

    fn memory_view(&self) -> Element<'_, Message> {
        let Some(document) = &self.document else {
            return text("尚未加载配置").into();
        };
        let memory = &document.memory;
        let models = model_choices(document, "继承全局默认");
        let overview = ui::section(
            "本地 DeerMem",
            "在对话完成后异步提取稳定事实，并在后续 ACP 对话中按作用域召回。当前便携版仅支持本地 DeerMem。",
            column![
                row![
                    checkbox(memory.enabled)
                        .label("启用长期记忆")
                        .style(ui::checkbox_style)
                        .on_toggle(Message::MemoryEnabled),
                    checkbox(memory.injection_enabled)
                        .label("允许提示词注入")
                        .style(ui::checkbox_style)
                        .on_toggle(Message::MemoryInjection),
                    checkbox(memory.retrieval_enabled)
                        .label("启用 FTS5 相关性检索")
                        .style(ui::checkbox_style)
                        .on_toggle(Message::MemoryRetrieval),
                ]
                .spacing(18),
                row![
                    column![
                        text("召回模式").size(13).color(ui::TEXT_SECONDARY),
                        pick_list(
                            vec!["middleware".into(), "tool".into()],
                            Some(memory.mode.clone()),
                            Message::MemoryMode
                        )
                        .style(ui::pick_list_style),
                        text(if memory.mode == "middleware" {
                            "自动向模型注入相关记忆"
                        } else {
                            "由模型主动调用 memory_search"
                        })
                        .size(12)
                        .color(ui::TEXT_MUTED),
                    ]
                    .spacing(6)
                    .width(Fill),
                    column![
                        text("ACP 记忆作用域").size(13).color(ui::TEXT_SECONDARY),
                        pick_list(
                            vec!["global".into(), "workspace".into(), "session".into()],
                            Some(document.runtime.memory_scope.clone()),
                            Message::RuntimeMemory
                        )
                        .style(ui::pick_list_style),
                    ]
                    .spacing(6)
                    .width(Fill),
                ]
                .spacing(14),
            ]
            .spacing(16),
        );
        let extraction = ui::section(
            "提取与注入",
            "数值限制与 DeerFlow 内核一致；保存前会再次校验。",
            column![
                column![
                    text("记忆提取模型").size(13).color(ui::TEXT_SECONDARY),
                    pick_list(
                        models,
                        Some(selected_model_choice(
                            document,
                            &memory.model_name,
                            "继承全局默认"
                        )),
                        |choice: preferences::ModelChoice| Message::MemoryModel(
                            choice.name.unwrap_or_default()
                        )
                    )
                    .style(ui::pick_list_style),
                ]
                .spacing(6)
                .width(Fill),
                row![
                    labeled_input(
                        "写入延迟（秒，1–300）",
                        &memory.debounce_input,
                        "30",
                        Message::MemoryDebounce
                    ),
                    labeled_input(
                        "最大事实数（10–500）",
                        &memory.max_facts_input,
                        "100",
                        Message::MemoryMaxFacts
                    ),
                    labeled_input(
                        "事实置信度（0–1）",
                        &memory.confidence_input,
                        "0.7",
                        Message::MemoryConfidence
                    ),
                ]
                .spacing(12),
                row![
                    labeled_input(
                        "最大注入 Token（100–8000）",
                        &memory.max_tokens_input,
                        "2000",
                        Message::MemoryMaxTokens
                    ),
                    labeled_input(
                        "检索 Top K（1–100）",
                        &memory.retrieval_top_k_input,
                        "12",
                        Message::MemoryRetrievalTopK
                    ),
                    labeled_input(
                        "关闭刷新超时（秒，0.1–300）",
                        &memory.shutdown_flush_input,
                        "30",
                        Message::MemoryShutdownFlush
                    ),
                ]
                .spacing(12),
            ]
            .spacing(14),
        );
        let storage = ui::section(
            "本地持久化",
            "相对路径以 user-data/data/deerflow 为基准。使用相对路径可保持整个目录可移动。",
            column![
                row![
                    labeled_input(
                        "记忆文件",
                        &memory.storage_path,
                        "memory.json",
                        Message::MemoryStoragePath
                    ),
                    labeled_input(
                        "FTS5 索引",
                        &memory.retrieval_index_path,
                        "memory-fts5.sqlite3",
                        Message::MemoryRetrievalIndexPath
                    ),
                ]
                .spacing(12),
                text(format!("当前记忆文件：{}", document.paths.memory))
                    .size(12)
                    .color(ui::TEXT_SECONDARY),
                text(format!("当前检索索引：{}", document.paths.memory_index))
                    .size(12)
                    .color(ui::TEXT_SECONDARY),
                text(format!("存储实现：{}", memory.storage_class))
                    .size(12)
                    .color(ui::TEXT_MUTED),
            ]
            .spacing(8),
        );
        let content = column![
            overview,
            self.advanced_panel(Page::Memory, column![extraction, storage].spacing(16))
        ];
        scrollable(content.spacing(16))
            .id("page-Memory")
            .spacing(8)
            .width(Fill)
            .height(Fill)
            .into()
    }

    fn runtime_view(&self) -> Element<'_, Message> {
        let Some(document) = &self.document else {
            return text("尚未加载配置").into();
        };
        let runtime = &document.runtime;
        let models = model_choices(document, "自动选择（Agent 优先，否则全局默认）");
        let agents = std::iter::once(String::new())
            .chain(document.agents.iter().map(|item| item.name.clone()))
            .collect::<Vec<_>>();
        let form = column![
            text("会话默认值").size(20).color(ui::TEXT_PRIMARY),
            text("runtime-dir 自动使用解压目录下的 user-data/runtime/acp，无需配置。")
                .size(13)
                .color(ui::TEXT_SECONDARY),
            row![
                column![
                    text("新会话模型").size(13),
                    pick_list(
                        models,
                        Some(selected_model_choice(
                            document,
                            &runtime.model_name,
                            "自动选择（Agent 优先，否则全局默认）"
                        )),
                        |choice: preferences::ModelChoice| Message::RuntimeModel(
                            choice.name.unwrap_or_default()
                        )
                    )
                    .style(ui::pick_list_style)
                ]
                .spacing(6)
                .width(Fill),
                column![
                    text("Custom Agent（可选）").size(13),
                    pick_list(agents, runtime.agent_name.clone(), Message::RuntimeAgent)
                        .style(ui::pick_list_style)
                ]
                .spacing(6)
                .width(Fill),
            ]
            .spacing(14),
            text(if let Some(name) = &runtime.model_name {
                format!(
                    "已指定 {}，不跟随全局默认模型变化。",
                    model_label(document, name)
                )
            } else {
                let (name, source) = preferences::effective_model(document);
                format!(
                    "自动选择：{} · 来自{}。",
                    model_label(document, name),
                    source
                )
            })
            .size(12)
            .color(ui::TEXT_SECONDARY),
            row![
                checkbox(runtime.thinking_enabled)
                    .label("Thinking")
                    .style(ui::checkbox_style)
                    .on_toggle(Message::RuntimeThinking),
                checkbox(runtime.plan_mode)
                    .label("Plan mode")
                    .style(ui::checkbox_style)
                    .on_toggle(Message::RuntimePlan),
                checkbox(runtime.subagent_enabled)
                    .label("Subagents")
                    .style(ui::checkbox_style)
                    .on_toggle(Message::RuntimeSubagents),
                checkbox(runtime.enable_bash)
                    .label("Bash（高风险）")
                    .style(ui::checkbox_style)
                    .on_toggle(Message::RuntimeBash),
            ]
            .spacing(16),
            row![
                column![
                    text("权限模式").size(13),
                    pick_list(
                        vec![
                            "不请求审批".into(),
                            "危险操作请求审批".into(),
                            "所有操作请求审批".into()
                        ],
                        Some(
                            match runtime.permission_mode.as_str() {
                                "off" => "不请求审批",
                                "all" => "所有操作请求审批",
                                _ => "危险操作请求审批",
                            }
                            .to_owned()
                        ),
                        Message::RuntimePermission
                    )
                    .style(ui::pick_list_style)
                ]
                .spacing(6)
                .width(Fill),
                column![
                    text("记忆作用域").size(13),
                    pick_list(
                        vec!["global".into(), "workspace".into(), "session".into()],
                        Some(runtime.memory_scope.clone()),
                        Message::RuntimeMemory
                    )
                    .style(ui::pick_list_style)
                ]
                .spacing(6)
                .width(Fill),
            ]
            .spacing(14),
            row![
                labeled_input(
                    "最大连接数",
                    &runtime.connections_input,
                    "16",
                    Message::RuntimeConnections
                ),
                labeled_input(
                    "最大并发运行",
                    &runtime.runs_input,
                    "2",
                    Message::RuntimeRuns
                ),
                labeled_input(
                    "Subagent 并发",
                    &runtime.subagent_count_input,
                    "2",
                    Message::RuntimeSubagentCount
                ),
            ]
            .spacing(12),
            column![
                text("Prompt Overlay").size(13),
                text_editor(&runtime.overlay_editor)
                    .on_action(Message::RuntimeOverlayEdit)
                    .height(140),
            ]
            .spacing(6),
        ]
        .spacing(16);
        let form = form.push(self.advanced_panel(Page::Runtime, column![
            labeled_input("运行超时（秒）", &runtime.timeout_input, "600", Message::RuntimeTimeout),
            text("长任务与保留策略（JSON）").size(16),
            text("goal_auto_continue 为自动续跑；queue_timeout_seconds 为排队超时；retention_days 为保留天数。未知扩展字段会保留。"),
            text_editor(&runtime.extra_editor).on_action(Message::RuntimeExtra).height(210),
        ].spacing(12)));
        scrollable(container(form).padding(18).style(ui::card))
            .id("page-Runtime")
            .spacing(8)
            .width(Fill)
            .height(Fill)
            .into()
    }

    fn diagnostics_view(&self) -> Element<'_, Message> {
        let mut path_rows = vec![
            ("产品目录", self.paths.root.clone()),
            ("配置文件", self.paths.config.clone()),
            ("用户数据", self.paths.user_data.clone()),
            ("Python", self.paths.python.clone()),
            ("ACP Bridge", self.paths.bridge.clone()),
            ("Runtime", self.paths.runtime.clone()),
            ("Daemon 日志", self.paths.runtime.join("daemon.log")),
        ];
        if let Some(document) = &self.document {
            path_rows.push(("记忆文件", PathBuf::from(&document.paths.memory)));
            path_rows.push(("记忆索引", PathBuf::from(&document.paths.memory_index)));
        }
        let path_rows = path_rows
            .into_iter()
            .fold(column![].spacing(9), |list, (label, path)| {
                let folder = if path.is_dir() {
                    path.clone()
                } else {
                    path.parent().unwrap_or(Path::new(".")).to_path_buf()
                };
                list.push(
                    container(
                        row![
                            text(label)
                                .size(13)
                                .color(ui::TEXT_SECONDARY)
                                .width(Length::Fixed(100.0)),
                            text(path.display().to_string())
                                .size(13)
                                .color(ui::TEXT_PRIMARY)
                                .width(Fill)
                                .wrapping(text::Wrapping::WordOrGlyph),
                            button(
                                row![
                                    ui::icon_view(ui::Icon::Folder, 15.0, ui::TEXT_SECONDARY),
                                    text("打开目录"),
                                ]
                                .spacing(7)
                                .align_y(iced::Alignment::Center),
                            )
                            .style(ui::secondary_button)
                            .on_press(Message::OpenFolder(folder)),
                        ]
                        .spacing(12)
                        .align_y(iced::Alignment::Center),
                    )
                    .padding(11)
                    .style(ui::inset_card),
                )
            });
        scrollable(
            container(
                column![
                    text("便携路径").size(20).color(ui::TEXT_PRIMARY),
                    row![ui::action_button("运行本地自检 / 查看日志").on_press_maybe((!self.busy).then_some(Message::Portable(portable::Message::Inspect))),
                         ui::action_button("复制脱敏报告").on_press(Message::Portable(portable::Message::CopyReport)),
                         ui::action_button("重新加载配置…").on_press_maybe((!self.busy).then_some(Message::Reload))].spacing(8),
                    text(format!("状态最后检查：{}", self.last_checked.map(|t| format!("{} 秒前", t.elapsed().as_secs())).unwrap_or("尚未完成".into()))),
                    text_editor(&self.portable.diagnostic_output).on_action(|a| Message::Portable(portable::Message::DiagnosticOutput(a))).height(240),
                    path_rows,
                    container(
                        row![
                            ui::icon_view(ui::Icon::Alert, 16.0, ui::ACCENT),
                            text("本地自检检查配置、存储与日志；模型测试在模型页。ACP 客户端连接由你在客户端验收。")
                                .size(13)
                                .color(ui::TEXT_SECONDARY),
                        ]
                        .spacing(8)
                        .align_y(iced::Alignment::Center),
                    )
                    .padding(10)
                    .style(ui::accent_card),
                ]
                .spacing(14),
            )
            .padding(18)
            .style(ui::card),
        )
        .spacing(8)
        .width(Fill)
        .height(Fill)
        .into()
    }
}

fn model_references(doc: &ConfigDocument, name: &str) -> Vec<String> {
    let mut refs = Vec::new();
    for (label, selected) in [
        ("ACP 默认模型", &doc.runtime.model_name),
        ("记忆提取", &doc.memory.model_name),
        ("Skill 生成", &doc.skill_evolution.generation_model_name),
        ("Skill 审核", &doc.skill_evolution.moderation_model_name),
        ("Skill 评估", &doc.skill_evolution.evaluation_model_name),
    ] {
        if selected.as_deref() == Some(name) {
            refs.push(label.into());
        }
    }
    for agent in &doc.agents {
        if agent.model.as_deref() == Some(name) {
            refs.push(format!("Agent {}", agent.name));
        }
    }
    for (label, map) in [
        ("Subagent", &doc.subagents.agents),
        ("Custom Subagent", &doc.subagents.custom_agents),
    ] {
        for (key, value) in map {
            if value.get("model").and_then(Value::as_str) == Some(name) {
                refs.push(format!("{label} {key}"));
            }
        }
    }
    refs
}

fn labeled_input<'a>(
    label: &'a str,
    value: &'a str,
    placeholder: &'a str,
    on_input: fn(String) -> Message,
) -> Element<'a, Message> {
    let mut field = column![
        text(label).size(13).color(ui::TEXT_SECONDARY),
        text_input(placeholder, value)
            .id(format!("field-{label}"))
            .on_input(on_input)
            .style(ui::input_style)
    ]
    .spacing(6)
    .width(Fill);
    let range = match label {
        "最大连接数" | "最大并发运行" => Some((1.0, 128.0, true)),
        "Subagent 并发" => Some((1.0, 4.0, true)),
        "运行超时（秒）" => Some((0.001, 86400.0, false)),
        "默认超时（秒）" => Some((1.0, u32::MAX as f64, true)),
        "默认最大轮次（留空使用内置值）" if !value.trim().is_empty() => {
            Some((1.0, u32::MAX as f64, true))
        }
        "最少工具调用" | "每日候选上限" | "最大文件数" => {
            Some((1.0, 1000.0, true))
        }
        "重复阈值" => Some((2.0, 100.0, true)),
        "重复窗口（天）" => Some((1.0, 3650.0, true)),
        "冷却时间（小时）" => Some((0.0, 8760.0, true)),
        "待审候选上限" | "Auto Patch 最大改动行" => Some((1.0, 10000.0, true)),
        "总大小上限（bytes）" => Some((1024.0, 100000000.0, true)),
        "单文件上限（bytes）" => Some((256.0, 10000000.0, true)),
        "Probation 使用次数" | "连续失败自动回滚阈值" | "检索 Top K（1–100）" => {
            Some((1.0, 100.0, true))
        }
        "写入延迟（秒，1–300）" => Some((1.0, 300.0, true)),
        "最大事实数（10–500）" => Some((10.0, 500.0, true)),
        "事实置信度（0–1）" => Some((0.0, 1.0, false)),
        "最大注入 Token（100–8000）" => Some((100.0, 8000.0, true)),
        "关闭刷新超时（秒，0.1–300）" => Some((0.1, 300.0, false)),
        _ => None,
    };
    if let Some((min, max, integer)) = range {
        let valid = value.trim().parse::<f64>().ok().is_some_and(|v| {
            v.is_finite()
                && v >= min
                && v <= max
                && (!integer || value.trim().parse::<u32>().is_ok())
        });
        if !valid {
            field = field.push(
                text(format!(
                    "请输入 {min}–{max} 范围内的{}",
                    if integer { "整数" } else { "数字" }
                ))
                .size(12)
                .color(ui::DANGER),
            );
        }
    }
    field.into()
}

fn parse_bounded_u32(value: &str, label: &str, minimum: u32, maximum: u32) -> Result<u32, String> {
    let parsed = value
        .trim()
        .parse::<u32>()
        .map_err(|_| format!("{label}必须是整数"))?;
    if !(minimum..=maximum).contains(&parsed) {
        return Err(format!("{label}必须在 {minimum} 到 {maximum} 之间"));
    }
    Ok(parsed)
}

fn parse_bounded_f64(value: &str, label: &str, minimum: f64, maximum: f64) -> Result<f64, String> {
    let parsed = value
        .trim()
        .parse::<f64>()
        .map_err(|_| format!("{label}必须是数字"))?;
    if !parsed.is_finite() || parsed < minimum || parsed > maximum {
        return Err(format!("{label}必须在 {minimum} 到 {maximum} 之间"));
    }
    Ok(parsed)
}

#[cfg(test)]
mod memory_input_tests {
    use super::{parse_bounded_f64, parse_bounded_u32};

    #[test]
    fn accepts_memory_limits_at_their_boundaries() {
        assert_eq!(parse_bounded_u32("10", "facts", 10, 500), Ok(10));
        assert_eq!(parse_bounded_u32("500", "facts", 10, 500), Ok(500));
        assert_eq!(parse_bounded_f64("0", "confidence", 0.0, 1.0), Ok(0.0));
        assert_eq!(parse_bounded_f64("1", "confidence", 0.0, 1.0), Ok(1.0));
    }

    #[test]
    fn rejects_out_of_range_and_non_finite_memory_values() {
        assert!(parse_bounded_u32("9", "facts", 10, 500).is_err());
        assert!(parse_bounded_u32("abc", "facts", 10, 500).is_err());
        assert!(parse_bounded_f64("1.1", "confidence", 0.0, 1.0).is_err());
        assert!(parse_bounded_f64("NaN", "confidence", 0.0, 1.0).is_err());
    }
}

fn non_empty(value: String) -> Option<String> {
    (!value.trim().is_empty()).then(|| value.trim().to_owned())
}

fn comma_list(value: &str) -> Vec<String> {
    value
        .split(',')
        .map(str::trim)
        .filter(|item| !item.is_empty())
        .map(str::to_owned)
        .collect()
}

#[derive(Debug)]
struct SubagentAssignment {
    kind: String,
    kind_label: String,
    name: String,
    description: String,
    model: String,
}

fn subagent_assignments(document: &SubagentsDocument) -> Vec<SubagentAssignment> {
    let mut output = Vec::new();
    let mut builtin_names = Vec::new();
    for builtin in &document.builtin_agents {
        builtin_names.push(builtin.name.clone());
        let override_config = document
            .agents
            .get(&builtin.name)
            .and_then(Value::as_object);
        output.push(SubagentAssignment {
            kind: "builtin".into(),
            kind_label: "内置".into(),
            name: builtin.name.clone(),
            description: override_config
                .and_then(|value| value.get("description"))
                .and_then(Value::as_str)
                .unwrap_or(&builtin.description)
                .to_owned(),
            model: override_config
                .and_then(|value| value.get("model"))
                .and_then(Value::as_str)
                .unwrap_or(&builtin.default_model)
                .to_owned(),
        });
    }
    for (name, value) in &document.agents {
        if builtin_names.contains(name) || document.custom_agents.contains_key(name) {
            continue;
        }
        let config = value.as_object();
        output.push(SubagentAssignment {
            kind: "override".into(),
            kind_label: "覆盖".into(),
            name: name.clone(),
            description: config
                .and_then(|value| value.get("description"))
                .and_then(Value::as_str)
                .unwrap_or("额外的 Subagent 覆盖配置")
                .to_owned(),
            model: config
                .and_then(|value| value.get("model"))
                .and_then(Value::as_str)
                .unwrap_or("inherit")
                .to_owned(),
        });
    }
    for (name, value) in &document.custom_agents {
        let config = value.as_object();
        let override_config = document.agents.get(name).and_then(Value::as_object);
        output.push(SubagentAssignment {
            kind: "custom".into(),
            kind_label: "自定义".into(),
            name: name.clone(),
            description: config
                .and_then(|value| value.get("description"))
                .and_then(Value::as_str)
                .unwrap_or("自定义 Subagent")
                .to_owned(),
            model: override_config
                .and_then(|value| value.get("model"))
                .and_then(Value::as_str)
                .or_else(|| {
                    config
                        .and_then(|value| value.get("model"))
                        .and_then(Value::as_str)
                })
                .unwrap_or("inherit")
                .to_owned(),
        });
    }
    output
}

fn subagent_model_choices(document: &ConfigDocument, selected: &str) -> Vec<String> {
    let mut choices = vec!["inherit".into()];
    choices.extend(document.models.iter().map(|item| item.name.clone()));
    if selected != "inherit" && !choices.iter().any(|item| item == selected) {
        choices.push(selected.to_owned());
    }
    choices
}

fn parse_json_object(value: &str, label: &str) -> Result<Map<String, Value>, String> {
    if value.trim().is_empty() {
        return Ok(Map::new());
    }
    let parsed: Value =
        serde_json::from_str(value).map_err(|error| format!("{label} JSON 格式错误：{error}"))?;
    parsed
        .as_object()
        .cloned()
        .ok_or_else(|| format!("{label} 必须是 JSON 对象"))
}

fn parse_json_object_array(value: &str, label: &str) -> Result<Vec<Map<String, Value>>, String> {
    if value.trim().is_empty() {
        return Ok(Vec::new());
    }
    let parsed: Value =
        serde_json::from_str(value).map_err(|error| format!("{label} JSON 格式错误：{error}"))?;
    let items = parsed
        .as_array()
        .ok_or_else(|| format!("{label} 必须是 JSON 数组"))?;
    items
        .iter()
        .enumerate()
        .map(|(index, item)| {
            item.as_object()
                .cloned()
                .ok_or_else(|| format!("{label} 第 {} 项必须是 JSON 对象", index + 1))
        })
        .collect()
}

fn pretty_json_object(value: &Map<String, Value>) -> String {
    serde_json::to_string_pretty(value).unwrap_or_else(|_| "{}".into())
}

fn pretty_json_objects(value: &[Map<String, Value>]) -> String {
    serde_json::to_string_pretty(value).unwrap_or_else(|_| "[]".into())
}

fn assign_subagent_model(
    document: &mut SubagentsDocument,
    kind: &str,
    name: &str,
    model: &str,
) -> Result<(), String> {
    if kind == "custom" {
        let custom = document
            .custom_agents
            .get_mut(name)
            .and_then(Value::as_object_mut)
            .ok_or_else(|| format!("Custom Subagent {name:?} 配置不是 JSON 对象"))?;
        custom.insert("model".into(), Value::String(model.to_owned()));
        let remove_override = if let Some(override_config) =
            document.agents.get_mut(name).and_then(Value::as_object_mut)
        {
            override_config.remove("model");
            override_config.is_empty()
        } else {
            false
        };
        if remove_override {
            document.agents.remove(name);
        }
        return Ok(());
    }

    if model == "inherit" {
        let remove_override = if let Some(override_config) =
            document.agents.get_mut(name).and_then(Value::as_object_mut)
        {
            override_config.remove("model");
            override_config.is_empty()
        } else {
            false
        };
        if remove_override {
            document.agents.remove(name);
        }
        return Ok(());
    }

    let override_config = document
        .agents
        .entry(name.to_owned())
        .or_insert_with(|| Value::Object(Map::new()))
        .as_object_mut()
        .ok_or_else(|| format!("Subagent {name:?} 覆盖配置不是 JSON 对象"))?;
    override_config.insert("model".into(), Value::String(model.to_owned()));
    Ok(())
}

#[derive(Debug, Clone, Copy)]
enum DaemonCommand {
    Start,
    Stop,
    Restart,
}

#[derive(Deserialize)]
struct ServiceEnvelope<T> {
    ok: bool,
    data: Option<T>,
    error: Option<String>,
    #[serde(default)]
    code: Option<String>,
}

fn configure_no_window(command: &mut Command) {
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        command.creation_flags(CREATE_NO_WINDOW);
    }
}

fn config_service<T: DeserializeOwned>(
    paths: &ProductPaths,
    command_name: &str,
    input: Option<&impl Serialize>,
) -> Result<T, String> {
    if !paths.python.is_file() {
        return Err(format!(
            "Python runtime not found: {}",
            paths.python.display()
        ));
    }
    let mut command = Command::new(&paths.python);
    command
        .arg("-m")
        .arg("deerflow.config_tool")
        .arg("--config")
        .arg(&paths.config)
        .arg("--user-data")
        .arg(&paths.user_data)
        .arg("--resources")
        .arg(&paths.resources)
        .arg(command_name)
        .env("PYTHONUTF8", "1")
        .env("PYTHONIOENCODING", "utf-8")
        .stdin(if input.is_some() {
            Stdio::piped()
        } else {
            Stdio::null()
        })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    configure_no_window(&mut command);
    let mut child = command.spawn().map_err(|error| error.to_string())?;
    if let Some(value) = input {
        let bytes = serde_json::to_vec(value).map_err(|error| error.to_string())?;
        child
            .stdin
            .take()
            .ok_or_else(|| "Unable to open configuration service stdin".to_owned())?
            .write_all(&bytes)
            .map_err(|error| error.to_string())?;
    }
    let output = child
        .wait_with_output()
        .map_err(|error| error.to_string())?;
    let stdout = String::from_utf8_lossy(&output.stdout);
    let envelope: ServiceEnvelope<T> = serde_json::from_str(stdout.trim()).map_err(|error| {
        format!(
            "Invalid configuration service response: {error}; stderr: {}",
            String::from_utf8_lossy(&output.stderr).trim()
        )
    })?;
    if output.status.success() && envelope.ok {
        envelope
            .data
            .ok_or_else(|| "Configuration service returned no data".into())
    } else {
        Err(envelope
            .error
            .unwrap_or_else(|| String::from_utf8_lossy(&output.stderr).trim().to_owned()))
    }
}

fn management_service<T: DeserializeOwned>(
    paths: &ProductPaths,
    request: &impl Serialize,
) -> Result<T, String> {
    if !paths.bridge.is_file() {
        return Err(format!("ACP Bridge not found: {}", paths.bridge.display()));
    }
    let mut command = Command::new(&paths.bridge);
    command
        .arg("--manage")
        .arg("--config")
        .arg(&paths.config)
        .arg("--runtime-dir")
        .arg(&paths.runtime)
        .env("PYTHONUTF8", "1")
        .env("PYTHONIOENCODING", "utf-8")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    configure_no_window(&mut command);
    let mut child = command.spawn().map_err(|error| error.to_string())?;
    let bytes = serde_json::to_vec(request).map_err(|error| error.to_string())?;
    child
        .stdin
        .take()
        .ok_or_else(|| "Unable to open Proposal service stdin".to_owned())?
        .write_all(&bytes)
        .map_err(|error| error.to_string())?;
    let output = child
        .wait_with_output()
        .map_err(|error| error.to_string())?;
    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr).trim().to_owned();
        return Err(if stderr.is_empty() {
            "Proposal service exited without a response".into()
        } else {
            stderr
        });
    }

    let stdout = String::from_utf8_lossy(&output.stdout);
    let envelope: ServiceEnvelope<T> = serde_json::from_str(stdout.trim()).map_err(|error| {
        format!(
            "Invalid Proposal service response: {error}; stderr: {}",
            String::from_utf8_lossy(&output.stderr).trim()
        )
    })?;
    if envelope.ok {
        envelope
            .data
            .ok_or_else(|| "Proposal service returned no data".into())
    } else {
        let error = envelope
            .error
            .unwrap_or_else(|| "Proposal operation failed".into());
        Err(match envelope.code {
            Some(code) => format!("{code}: {error}"),
            None => error,
        })
    }
}

fn load_pending_evolution_proposals(paths: &ProductPaths) -> Result<ProposalListData, String> {
    management_service(
        paths,
        &json!({
            "operation": "proposal.list",
            "status": "pending_review",
        }),
    )
}

fn load_evolution_proposal(
    paths: &ProductPaths,
    proposal_id: &str,
) -> Result<EvolutionProposal, String> {
    management_service(
        paths,
        &json!({
            "operation": "proposal.get",
            "proposal_id": proposal_id,
        }),
    )
}

fn review_evolution_proposal(
    paths: &ProductPaths,
    action: ProposalReviewAction,
    proposal_id: &str,
    expected_base_sha256: Option<&str>,
    note: &str,
) -> Result<ProposalMutationData, String> {
    management_service(
        paths,
        &json!({
            "operation": action.operation(),
            "proposal_id": proposal_id,
            "expected_base_sha256": expected_base_sha256,
            "note": if note.trim().is_empty() {
                Value::Null
            } else {
                Value::String(note.trim().to_owned())
            },
        }),
    )
}

fn friendly_management_error(error: String) -> String {
    let normalized = error.to_ascii_lowercase();
    if normalized.contains("daemon is not running") {
        "Daemon 未运行。请先在概览页启动 Daemon，再刷新待审批列表。".into()
    } else if normalized.contains("uses a different config") {
        "当前 Daemon 使用了另一份 config.yaml；请先停止它，再从本工具启动。".into()
    } else {
        error
    }
}

fn load_config(paths: &ProductPaths) -> Result<ConfigDocument, String> {
    let mut document = config_service::<ConfigDocument>(paths, "snapshot", None::<&&str>)?;
    document.prepare_editor_state();
    Ok(document)
}

fn bridge_command(paths: &ProductPaths, mode: &str) -> Result<String, String> {
    if !paths.bridge.is_file() {
        return Err(format!("ACP Bridge not found: {}", paths.bridge.display()));
    }
    let mut command = Command::new(&paths.bridge);
    command
        .arg(mode)
        .arg("--config")
        .arg(&paths.config)
        .arg("--python")
        .arg(&paths.python)
        .arg("--runtime-dir")
        .arg(&paths.runtime)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    configure_no_window(&mut command);
    let output = command.output().map_err(|error| error.to_string())?;
    if output.status.success() {
        Ok(String::from_utf8_lossy(&output.stdout).trim().to_owned())
    } else {
        Err(String::from_utf8_lossy(&output.stderr).trim().to_owned())
    }
}

fn query_daemon(paths: &ProductPaths) -> DaemonStatus {
    match bridge_command(paths, "--status") {
        Ok(mut details) => {
            if let Ok(activity) =
                management_service::<Value>(paths, &json!({"operation":"daemon.status"}))
            {
                details.push_str(&format!(
                    " active={} queued={} revision={} warmup={}",
                    activity["active_runs"],
                    activity["queued_runs"],
                    activity["config_revision"].as_str().unwrap_or(""),
                    activity["warmup"].as_str().unwrap_or("unknown")
                ));
            }
            DaemonStatus::Running(details)
        }
        Err(error) if error.contains("not running") => DaemonStatus::Stopped,
        Err(error) => DaemonStatus::Error(error),
    }
}

fn daemon_action(paths: &ProductPaths, action: DaemonCommand) -> Result<DaemonStatus, String> {
    match action {
        DaemonCommand::Start => {
            let details = bridge_command(paths, "--start-daemon")?;
            Ok(DaemonStatus::Running(details))
        }
        DaemonCommand::Stop => {
            if matches!(query_daemon(paths), DaemonStatus::Running(_)) {
                bridge_command(paths, "--stop-daemon")?;
            }
            Ok(DaemonStatus::Stopped)
        }
        DaemonCommand::Restart => {
            if matches!(query_daemon(paths), DaemonStatus::Running(_)) {
                bridge_command(paths, "--stop-daemon")?;
            }
            let details = bridge_command(paths, "--start-daemon")?;
            Ok(DaemonStatus::Running(details))
        }
    }
}

fn save_without_restart(
    paths: &ProductPaths,
    document: &ConfigDocument,
) -> Result<(ConfigDocument, DaemonStatus, bool), String> {
    let mut saved = config_service::<ConfigDocument>(paths, "save", Some(document))?;
    saved.prepare_editor_state();
    // A status probe cannot turn a successful save into a failed save.
    Ok((saved, query_daemon(paths), false))
}

fn open_folder(path: &Path) -> Result<(), String> {
    #[cfg(windows)]
    {
        Command::new("explorer.exe")
            .arg(path)
            .spawn()
            .map(|_| ())
            .map_err(|error| error.to_string())
    }
    #[cfg(not(windows))]
    {
        Command::new("xdg-open")
            .arg(path)
            .spawn()
            .map(|_| ())
            .map_err(|error| error.to_string())
    }
}

#[cfg(test)]
mod portable_regression_tests {
    use super::*;

    fn runtime_json() -> Value {
        json!({"model_name":null,"agent_name":null,"thinking_enabled":true,"plan_mode":true,
            "subagent_enabled":false,"max_concurrent_subagents":2,"max_active_connections":16,
            "max_active_runs":2,"run_timeout_seconds":600.0,"permission_mode":"dangerous",
            "memory_scope":"workspace","enable_bash":false,"tool_allowlist":null,
            "tool_denylist":[],"prompt_overlay":"", "goal_auto_continue":true,
            "goal_max_continuations":7,"goal_max_no_progress_continuations":5,
            "queue_timeout_seconds":300,"future_runtime_field":{"keep":true}})
    }

    #[test]
    fn desktop_roundtrip_preserves_goal_and_future_fields() {
        let input = runtime_json();
        let document: RuntimeDocument = serde_json::from_value(input.clone()).unwrap();
        assert_eq!(serde_json::to_value(document).unwrap(), input);
    }

    #[test]
    fn reload_requires_confirmation_and_refresh_does_not_discard_draft() {
        let (mut app, _) = App::new();
        app.busy = false;
        app.dirty = true;
        let _ = app.update(Message::Reload);
        assert!(matches!(app.pending_action, Some(Message::Reload)));
        assert!(app.dirty);
        let _ = app.update(Message::CancelAction);
        let _ = app.update(Message::RefreshStatus);
        assert!(app.dirty);
        assert!(app.pending_action.is_none());
    }

    #[test]
    fn saving_ignores_late_config_edits() {
        let (mut app, _) = App::new();
        assert!(app.busy);
        assert!(!app.dirty);
        let _ = app.update(Message::ModelName("late edit".into()));
        assert!(!app.dirty);
    }

    #[test]
    fn invalid_numeric_values_cannot_be_accepted() {
        assert!(parse_bounded_u32("abc", "并发", 1, 128).is_err());
        assert!(parse_bounded_u32("0", "并发", 1, 128).is_err());
        assert!(parse_bounded_f64("NaN", "超时", 0.1, 86400.0).is_err());
    }
}
