//! Format the bounded, non-secret daemon projection once on status arrival.
use super::*;

pub(super) struct Row {
    pub(super) section: Section,
    pub(super) overview: bool,
    pub(super) label: SharedString,
    pub(super) value: SharedString,
}

pub(super) fn read(status: &Value) -> Vec<Row> {
    let settings = &status["applied_settings"];
    if status["running"] != true
        || settings["version"] != "deerflow-applied-settings-v1"
        || settings["applies_to"] != "new_sessions_and_tasks"
        || settings["fields"]
            .as_object()
            .is_none_or(|fields| fields.len() > 96)
    {
        return Vec::new();
    }
    let mut rows = Vec::new();
    for (path, label, section, overview) in [
        ("/runtime/model_name", "会话默认模型", Section::Models, true),
        (
            "/runtime/provider",
            "默认模型提供方",
            Section::Models,
            false,
        ),
        (
            "/runtime/max_active_connections",
            "连接上限",
            Section::Runtime,
            false,
        ),
        (
            "/runtime/max_active_runs",
            "前台并发上限",
            Section::Runtime,
            true,
        ),
        (
            "/runtime/run_timeout_seconds",
            "任务总执行时限（秒）",
            Section::Runtime,
            true,
        ),
        (
            "/runtime/queue_timeout_seconds",
            "排队超时（秒）",
            Section::Runtime,
            false,
        ),
        (
            "/runtime/permission_mode",
            "工具审批模式",
            Section::Tools,
            true,
        ),
        (
            "/runtime/tool_allowlist",
            "工具允许列表",
            Section::Tools,
            false,
        ),
        (
            "/runtime/tool_denylist",
            "工具拒绝列表",
            Section::Tools,
            false,
        ),
        (
            "/runtime/session_cleanup_enabled",
            "会话自动清理",
            Section::Runtime,
            false,
        ),
        (
            "/runtime/inactive_session_retention_days",
            "未关闭会话保留天数",
            Section::Runtime,
            false,
        ),
        (
            "/runtime/closed_session_retention_days",
            "已关闭会话保留天数",
            Section::Runtime,
            false,
        ),
        (
            "/runtime/session_cleanup_interval_seconds",
            "清理间隔（秒）",
            Section::Runtime,
            false,
        ),
        ("/memory/enabled", "记忆访问", Section::Memory, true),
        (
            "/memory/extraction_enabled",
            "自动提取记忆",
            Section::Memory,
            false,
        ),
        ("/memory/mode", "记忆模式", Section::Memory, false),
        (
            "/memory/injection_enabled",
            "记忆注入",
            Section::Memory,
            false,
        ),
        (
            "/memory/retrieval_enabled",
            "记忆检索",
            Section::Memory,
            false,
        ),
        (
            "/runtime/memory_scope",
            "记忆作用域",
            Section::Memory,
            false,
        ),
        (
            "/sandbox/provider",
            "命令工具运行环境",
            Section::Tools,
            true,
        ),
        (
            "/sandbox/enabled",
            "命令工具环境已启用",
            Section::Tools,
            false,
        ),
        (
            "/sandbox/allow_shell",
            "允许脚本命令",
            Section::Tools,
            false,
        ),
        (
            "/sandbox/allow_host_tools",
            "宿主工具访问",
            Section::Tools,
            false,
        ),
        (
            "/mcp/allowed_command_count",
            "MCP 允许的命令数量",
            Section::Tools,
            false,
        ),
        (
            "/mcp/allow_http",
            "MCP HTTP 出站连接",
            Section::Tools,
            false,
        ),
        ("/mcp/allow_sse", "MCP SSE 出站连接", Section::Tools, false),
        (
            "/skills/include_global",
            "加载全局技能",
            Section::Skills,
            false,
        ),
        (
            "/skills/source_count",
            "技能来源数量",
            Section::Skills,
            false,
        ),
        (
            "/skills/managed_source_count",
            "托管技能来源数量",
            Section::Skills,
            false,
        ),
        (
            "/skills/model_submission_enabled",
            "模型提交技能候选",
            Section::Skills,
            false,
        ),
        (
            "/skill_evolution/mode",
            "技能发布方式",
            Section::Skills,
            true,
        ),
        (
            "/skill_evolution/generation/enabled",
            "技能候选生成",
            Section::Skills,
            true,
        ),
        (
            "/skill_evolution/discovery/enabled",
            "技能自动发现",
            Section::Skills,
            false,
        ),
        (
            "/skill_evolution/learning/enabled",
            "会话经验提取（仅积累）",
            Section::Skills,
            false,
        ),
        (
            "/skill_evolution/learning/consolidation_enabled",
            "经验语义归并（仅积累）",
            Section::Skills,
            false,
        ),
        (
            "/skill_evolution/learning/cross_session_enabled",
            "工作区跨会话归并（需会话授权）",
            Section::Skills,
            false,
        ),
        (
            "/skill_evolution/learning/matching_enabled",
            "经验技能匹配（仅积累）",
            Section::Skills,
            false,
        ),
        (
            "/skill_evolution/behavior/enabled",
            "技能行为评测",
            Section::Skills,
            false,
        ),
        ("/subagents/enabled", "子代理", Section::Agents, true),
        (
            "/subagents/max_concurrent",
            "根任务子代理并发",
            Section::Agents,
            false,
        ),
        (
            "/subagents/background_workers",
            "后台执行并发",
            Section::Agents,
            false,
        ),
    ] {
        push_row(
            &mut rows,
            &settings["fields"][path],
            path,
            label,
            section,
            overview,
        );
    }
    for (key, label) in [
        ("validation/timeout_seconds", "审查超时（秒）"),
        ("validation/max_input_bytes", "审查输入上限（字节）"),
        ("validation/max_output_tokens", "审查输出 Token 上限"),
        ("generation/timeout_seconds", "生成超时（秒）"),
        ("generation/max_input_bytes", "生成输入上限（字节）"),
        ("generation/max_output_tokens", "生成输出 Token 上限"),
        ("learning/idle_seconds", "经验提取空闲窗口（秒）"),
        ("learning/timeout_seconds", "经验提取超时（秒）"),
        ("learning/max_input_bytes", "经验输入上限（字节）"),
        ("learning/max_output_tokens", "经验输出 Token 上限"),
        ("learning/max_daily_model_calls", "每日学习调用子额度"),
        ("learning/max_daily_tokens", "每日学习 Token 子额度"),
        ("learning/external_context_policy", "外部上下文学习策略"),
        ("budget/max_daily_model_calls", "每日维护调用上限"),
        ("budget/max_daily_tokens", "每日维护 Token 上限"),
        ("budget/max_job_seconds", "维护作业时限（秒）"),
        ("budget/workers", "维护并发数"),
        ("discovery/operator_corrections_enabled", "重复纠正发现"),
        ("discovery/missing_reports_enabled", "缺失技能发现"),
        ("discovery/successful_workflows_enabled", "成功流程发现"),
        ("discovery/experience_patterns_enabled", "会话经验改进提案"),
        (
            "discovery/experience_new_skills_enabled",
            "会话经验新技能提案",
        ),
        (
            "discovery/experience_review_drafts_enabled",
            "单任务自动起草待审核提案",
        ),
        ("discovery/new_skill_source_id", "新技能源 ID"),
        ("discovery/repeat_threshold", "发现所需独立任务数"),
        ("discovery/repeat_window_days", "发现统计范围（天）"),
        ("discovery/cooldown_hours", "发现冷却时间（小时）"),
        ("discovery/max_daily_proposals", "每日自动候选上限"),
        ("discovery/max_pending_proposals", "待处理候选上限"),
        ("behavior/timeout_seconds", "行为评测超时（秒）"),
        ("behavior/max_steps", "行为样例步数上限"),
        ("behavior/max_input_bytes", "行为输入上限（字节）"),
        ("behavior/max_output_tokens", "行为输出 Token 上限"),
        (
            "auto_patch/experience_patterns_enabled",
            "会话经验受限自动补丁",
        ),
        ("auto_patch/max_changed_lines", "自动补丁修改行数上限"),
        ("auto_patch/max_daily_publications", "每日自动发布上限"),
        ("auto_patch/observation_hours", "发布观察期（小时）"),
        ("auto_patch/min_observations", "观察期最少质量反馈数"),
        (
            "auto_patch/rollback_failure_threshold",
            "回滚所需连续质量失败数",
        ),
    ] {
        let path = format!("/skill_evolution/{key}");
        push_row(
            &mut rows,
            &settings["fields"][&path],
            &path,
            label,
            Section::Skills,
            false,
        );
    }
    // Only the explicit non-secret allowlist is formatted. This runs when a
    // status response arrives, not during render. Omitted role keys inherit
    // the host/parent defaults; these are loaded overrides, not task grants.
    read_groups(&mut rows, &settings["roles"], true);
    read_groups(&mut rows, &settings["tools"], false);
    rows
}

fn read_groups(rows: &mut Vec<Row>, value: &Value, roles: bool) {
    let Some(groups) = value
        .as_array()
        .filter(|groups| groups.len() <= if roles { 64 } else { 13 })
    else {
        return;
    };
    let role_fields = [
        ("model", "模型"),
        ("thinking_enabled", "思考"),
        ("reasoning_effort", "推理强度"),
        ("timeout_seconds", "超时（秒）"),
        ("max_turns", "轮次上限"),
        ("model_settings/temperature", "温度"),
        ("model_settings/max_tokens", "输出 Token 上限"),
        ("model_settings/thinking_budget_tokens", "思考 Token 预算"),
        ("tools", "工具允许列表"),
        ("disallowed_tools", "工具拒绝列表"),
        ("skills", "技能列表"),
        ("delegate_to", "可委派角色"),
        ("system_prompt_sha256", "指令摘要"),
        ("description_sha256", "说明摘要"),
    ];
    let tool_fields = [
        ("timeout", "超时（秒）"),
        ("max_results", "默认结果数（无调用覆盖时）"),
        ("max_output_chars", "输出字符上限"),
        ("allowed_sites", "允许站点"),
        ("executable_sha256", "配置命令入口摘要"),
    ];
    for group in groups {
        let Some(name) = group["name"].as_str().filter(|name| {
            !name.is_empty()
                && name.len() <= 64
                && name
                    .bytes()
                    .all(|c| c.is_ascii_alphanumeric() || matches!(c, b'_' | b'-'))
        }) else {
            continue;
        };
        if group["fields"]
            .as_object()
            .is_none_or(|fields| fields.len() > 16)
        {
            continue;
        }
        let (prefix, section, fields): (String, Section, &[(&str, &str)]) = if roles {
            let kind = match group["kind"].as_str() {
                Some("agents") => "内置角色覆盖",
                Some("custom_agents") => "自定义角色",
                _ => continue,
            };
            (format!("{kind} {name}"), Section::Agents, &role_fields)
        } else {
            if !matches!(
                name,
                "web_search"
                    | "web_fetch"
                    | "image_search"
                    | "host_opencli"
                    | "glob"
                    | "grep"
                    | "ls"
                    | "read_file"
                    | "write_file"
                    | "str_replace"
                    | "move_path"
                    | "delete_path"
                    | "bash"
            ) {
                continue;
            }
            (format!("工具 {name}"), Section::Tools, &tool_fields)
        };
        for (key, label) in fields {
            push_row(
                rows,
                &group["fields"][*key],
                key,
                &format!("{prefix} · {label}"),
                section,
                false,
            );
        }
    }
}

fn push_row(
    rows: &mut Vec<Row>,
    field: &Value,
    path: &str,
    label: &str,
    section: Section,
    overview: bool,
) {
    let Some(value) = field.get("value") else {
        return;
    };
    let source = match field["source"].as_str() {
        Some("yaml") => "已加载配置",
        Some("cli") => "启动参数覆盖",
        Some("default") => "宿主默认",
        Some("legacy") => "兼容配置",
        Some("legacy_host_gate") => "旧宿主限制",
        Some("yaml_and_cli") => "配置与启动限制",
        _ => return,
    };
    let display = match value {
        Value::String(text) if path == "/skill_evolution/mode" && text == "review" => {
            "人工审核".to_owned()
        }
        Value::String(text) if path == "/skill_evolution/mode" && text == "auto_patch" => {
            "受限自动补丁".to_owned()
        }
        Value::Bool(true) => "开启".to_owned(),
        Value::Bool(false) => "关闭".to_owned(),
        Value::Null => "继承".to_owned(),
        Value::String(text) if text.is_empty() => "宿主默认".to_owned(),
        Value::String(text) if text.len() <= 512 => text.clone(),
        Value::Number(number)
            if number.as_f64() == Some(0.0)
                && matches!(path, "/runtime/run_timeout_seconds" | "/runtime/queue_timeout_seconds" | "/subagents/timeout_seconds") => "不限时（0）".to_owned(),
        Value::Number(number) => number.to_string(),
        Value::Array(items) if items.len() <= 128 => {
            let Some(items) = items.iter().map(Value::as_str).collect::<Option<Vec<_>>>() else {
                return;
            };
            let display = items.join("、");
            if display.len() > 2048 {
                return;
            }
            if items.is_empty() {
                "空列表".into()
            } else {
                display
            }
        }
        _ => return,
    };
    rows.push(Row {
        section,
        overview,
        label: label.to_owned().into(),
        value: format!("{display} · {source}").into(),
    });
}

impl DeerFlowSettings {
    pub(super) fn set_status(&mut self, status: Value) {
        if skills::service_identity(&self.status) != skills::service_identity(&status)
            || self.status["applied_generation"] != status["applied_generation"]
            || self.status["config_revision"] != status["config_revision"]
        {
            self.readiness = None;
        }
        self.applied_values = read(&status);
        self.status = status;
    }
}
