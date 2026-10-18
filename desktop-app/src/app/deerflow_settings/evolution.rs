//! Native controls for candidate generation, correction discovery and review.
use super::*;

const CONVERSION_FIELDS: &[&str] = &[
    "/skill_evolution_migration_ack",
    "/skill_evolution/mode",
    "/skill_evolution/security_fail_closed",
];

pub(super) fn fields() -> Vec<Field> {
    [
        (
            "enabled",
            "允许提交技能改进候选",
            "启用后可通过 skill_manage 提交候选；按照下方发布方式处理",
            Kind::Bool,
        ),
        (
            "mode",
            "发布方式",
            "人工审核默认开启；自动补丁仅适用于通过安全、质量和行为评测的现有技能小改动，新技能仍需人工审核",
            Kind::Choice(&[("review", "人工审核"), ("auto_patch", "受限自动补丁")]),
        ),
        (
            "allow_builtin_overlays",
            "允许改进内置技能",
            "改动作为独立覆盖版本审查；不修改内置源文件",
            Kind::Bool,
        ),
        (
            "moderation_model_name",
            "安全审查模型",
            "默认继承全局默认模型，也可单独指定；检查越权、凭据泄漏及可执行内容",
            Kind::InheritableConfiguredModelReference,
        ),
        (
            "generation/enabled",
            "允许后台生成技能候选",
            "需要先启用技能改进；处理手动请求及另行启用的发现信号，发布遵循审核和自动补丁规则",
            Kind::Bool,
        ),
        (
            "generation_model_name",
            "候选生成模型",
            "默认继承全局默认模型，也可单独指定；与安全和质量审查分别调用",
            Kind::InheritableConfiguredModelReference,
        ),
        (
            "generation/timeout_seconds",
            "单次生成超时（秒）",
            "0 使用 60；可设置 1–300，实际还受作业剩余时间限制",
            Kind::Integer,
        ),
        (
            "generation/max_input_bytes",
            "生成输入上限（字节）",
            "0 使用 131072；可设置 1024–2097152，包含基础技能和改进理由",
            Kind::Integer,
        ),
        (
            "generation/max_output_tokens",
            "生成输出 Token 上限",
            "0 使用 8192；可设置 128–32768",
            Kind::Integer,
        ),
        (
            "discovery/operator_corrections_enabled",
            "根据重复纠正自动生成候选",
            "默认关闭；需要后台生成已启用。独立任务对同一技能的明确纠正可触发模型调用，费用计入维护额度",
            Kind::Bool,
        ),
        (
            "learning/enabled",
            "从多轮会话自动提取过程经验",
            "默认关闭；启用技能改进并配置维护预算后，空闲时提取脱敏经验。生成候选需另开经验发现，不作为人工质量反馈",
            Kind::Bool,
        ),
        (
            "learning/extraction_model_name",
            "经验提取模型",
            "继承全局默认模型或单独指定；无工具调用，与聊天预算隔离",
            Kind::InheritableConfiguredModelReference,
        ),
        (
            "learning/consolidation_enabled",
            "归并已提取的过程经验",
            "默认关闭；单独授权后台模型比较同会话经验，保留冲突和任务来源，不自动生成或发布技能",
            Kind::Bool,
        ),
        (
            "learning/consolidation_model_name",
            "经验归并模型",
            "继承全局默认模型或单独指定；无工具调用，提取和归并共用学习子额度",
            Kind::InheritableConfiguredModelReference,
        ),
        (
            "learning/cross_session_enabled",
            "允许工作区内跨会话归并",
            "默认关闭；还需各会话显式同意共享。仅同一工作区内归并，普通会话查询保持隔离，撤销共享会清除依赖摘要",
            Kind::Bool,
        ),
        (
            "learning/matching_enabled",
            "匹配经验与已有技能",
            "默认关闭；需要经验提取和归并已启用。比较当前会话可见的技能并固定目标，生成候选需另开经验发现，发布仍需人工审核",
            Kind::Bool,
        ),
        (
            "learning/matching_model_name",
            "经验技能匹配模型",
            "继承全局默认模型或单独指定；无工具调用，与提取和归并共用学习子额度",
            Kind::InheritableConfiguredModelReference,
        ),
        (
            "learning/idle_seconds",
            "经验提取空闲窗口（秒）",
            "1–86400，默认 300；任务树未结束或副作用不确定时不进入学习",
            Kind::Integer,
        ),
        (
            "learning/timeout_seconds",
            "单次经验模型超时（秒）",
            "1–300，默认 60；提取、归并与匹配共用，同时受维护作业时限约束",
            Kind::Integer,
        ),
        (
            "learning/max_input_bytes",
            "经验模型输入上限（字节）",
            "1024–2097152，默认 131072；提取、归并与匹配共用，不发送隐藏推理或完整附件",
            Kind::Integer,
        ),
        (
            "learning/max_output_tokens",
            "经验模型输出 Token 上限",
            "128–32768，默认 4096；提取、归并与匹配共用，经验引用仍需宿主核验",
            Kind::Integer,
        ),
        (
            "learning/max_daily_model_calls",
            "每日学习模型调用子额度",
            "新桌面配置预设 50；必须小于或等于共享维护调用额度，不额外扩充总预算",
            Kind::Integer,
        ),
        (
            "learning/max_daily_tokens",
            "每日学习 Token 子额度",
            "新桌面配置预设 1000000；必须在共享维护 Token 额度内，未知用量保守计费",
            Kind::Integer,
        ),
        (
            "learning/external_context_policy",
            "外部上下文学习策略",
            "仅允许元数据提示；网络、MCP 及未知工具的结果正文不进入学习视图",
            Kind::Choice(&[("hints_only", "仅元数据提示")]),
        ),
        (
            "discovery/missing_reports_enabled",
            "根据缺失报告生成技能",
            "默认关闭；需要技能改进和后台生成已启用，并选择可写的新技能源。仅使用当前工作区内有效报告",
            Kind::Bool,
        ),
        (
            "discovery/successful_workflows_enabled",
            "将重复成功流程整理为技能",
            "默认关闭；需要技能改进和后台生成已启用，并选择可写的新技能源。独立根任务的真实工具回执作为证据",
            Kind::Bool,
        ),
        (
            "discovery/experience_patterns_enabled",
            "根据多轮经验提出已有技能改进",
            "默认关闭；需提取、归并、匹配与后台生成均已启用。通常要求至少两个独立任务有完整本地回执；可另启单任务自动起草待审草案。候选仅改 SKILL.md，自动补丁须完整独立证据与可信评测",
            Kind::Bool,
        ),
        (
            "discovery/experience_new_skills_enabled",
            "将确认缺少的过程能力整理为新技能",
            "默认关闭；还需多轮经验发现已启用及显式非内置新技能源。完整匹配目录、可读名称和来源由宿主复核，仅创建 SKILL.md 并等待人工发布",
            Kind::Bool,
        ),
        (
            "discovery/experience_review_drafts_enabled",
            "单任务自动起草待审核提案",
            "默认关闭；启用后，一个已有完整本地回执的任务即可自动起草，不需手工触发进化。独立证据不足的草案始终人工审核，即使开启自动补丁也不会自动发布",
            Kind::Bool,
        ),
        (
            "discovery/new_skill_source_id",
            "自动创建技能的来源 ID",
            "填写后台生成允许的非内置来源 ID，建议选择托管来源；宿主在使用时重新核对工作区授权",
            Kind::Text,
        ),
        (
            "discovery/repeat_threshold",
            "自动发现所需独立任务数",
            "2–16；重复纠正、成功流程和多轮经验共用。宿主已关联的同一任务、复制、子任务或重试不重复计数",
            Kind::Integer,
        ),
        (
            "discovery/repeat_window_days",
            "自动发现统计时间范围（天）",
            "1–365；所有发现入口共用，仅选择时间范围内仍有效的证据",
            Kind::Integer,
        ),
        (
            "discovery/cooldown_hours",
            "同技能自动生成间隔（小时）",
            "1–8760；失败或取消仍保留本次计数和冷却时间",
            Kind::Integer,
        ),
        (
            "discovery/max_daily_proposals",
            "每日自动候选上限",
            "1–100；按 UTC 日计入已入队的自动候选，模型调用还受维护预算限制",
            Kind::Integer,
        ),
        (
            "discovery/max_pending_proposals",
            "待处理候选上限",
            "1–1000；已有生成作业和待审核候选达到此数时，暂停新的自动选择",
            Kind::Integer,
        ),
        (
            "budget/max_daily_model_calls",
            "每日维护模型调用上限",
            "1–100000；按 UTC 日汇总所有技能源的生成、审查及行为评测，重试也计入",
            Kind::Integer,
        ),
        (
            "budget/max_daily_tokens",
            "每日维护 Token 上限",
            "1–1000000000；请求前预留输入和输出额度，未知用量保守计入",
            Kind::Integer,
        ),
        (
            "budget/max_job_seconds",
            "生成作业时限（秒）",
            "1–3600；从首次执行起包含生成、审查、重试和等待",
            Kind::Integer,
        ),
        (
            "budget/workers",
            "维护作业并发数",
            "1–16；与聊天和子代理的预算及并发分别限制",
            Kind::Integer,
        ),
        (
            "evaluation_model_name",
            "内容质量与行为评测模型",
            "默认继承全局默认模型，也可单独指定；内容审查与行为评测使用独立上下文，内容审查本身不代表通过行为测试",
            Kind::InheritableConfiguredModelReference,
        ),
        (
            "behavior/enabled",
            "启用基础与候选行为评测",
            "新桌面配置已启用；需要技能改进及后台生成已启用。使用宿主配置的可信样例比较两个版本，调用计入维护额度；自动补丁必须通过此评测",
            Kind::Bool,
        ),
        (
            "behavior/suite_path",
            "可信行为样例套件路径",
            "新桌面配置已预置路径和宿主套件，无需手填。自定义时支持单套件或 skill-behavior-registry-v1 注册表（最多 32 个目标）；不接受候选修改评分规则。保存并应用后生效",
            Kind::Text,
        ),
        (
            "behavior/timeout_seconds",
            "行为评测超时（秒）",
            "1–300，默认 60；评测还受维护作业剩余时间限制",
            Kind::Integer,
        ),
        (
            "behavior/max_steps",
            "单个样例执行步数上限",
            "1–32，默认 6；基础和候选分别在隔离的 fixture 工具上下文中执行",
            Kind::Integer,
        ),
        (
            "behavior/max_input_bytes",
            "行为评测输入上限（字节）",
            "1024–2097152，默认 131072；限制技能及样例输入的大小",
            Kind::Integer,
        ),
        (
            "behavior/max_output_tokens",
            "单次行为评测输出 Token 上限",
            "128–8192，默认 1024；基础与候选执行各自计入维护预算",
            Kind::Integer,
        ),
        (
            "auto_patch/experience_patterns_enabled",
            "允许会话经验受限自动补丁",
            "默认关闭；完整学习/发现链和可信套件下可受限发布既有 managed 技能的正文改善。描述修改另需 v2 正负触发选择通过且有改善；同改正文还需正文改善，其他元数据、新技能与首次采用仍人工审核",
            Kind::Bool,
        ),
        (
            "auto_patch/max_changed_lines",
            "自动补丁最多修改行数",
            "1–200，默认 40；仅现有技能 SKILL.md 的小改动可自动发布，其他候选保留人工审核",
            Kind::Integer,
        ),
        (
            "auto_patch/max_daily_publications",
            "每日自动发布上限",
            "1–20，默认 2；需要后台生成、行为评测和可信样例套件已启用",
            Kind::Integer,
        ),
        (
            "auto_patch/observation_hours",
            "自动发布后观察时间（小时）",
            "1–720，默认 72；观察新版本的独立根任务及明确质量反馈",
            Kind::Integer,
        ),
        (
            "auto_patch/min_observations",
            "观察期所需最少质量反馈",
            "1–100，默认 3；同一根任务只计一次，证据不足会单独标记",
            Kind::Integer,
        ),
        (
            "auto_patch/rollback_failure_threshold",
            "触发自动回滚的连续质量失败数",
            "1–100，默认 2；只计算明确归因于当前版本的连续失败，质量成功会清零，外部故障不计入；回滚保留历史修订",
            Kind::Integer,
        ),
        (
            "security_fail_closed",
            "审查失败时阻止发布",
            "必须开启；安全审查不可用或不通过时不能发布",
            Kind::TrueOnly,
        ),
        (
            "validation/response_format",
            "审核响应格式",
            "默认文本兼容模式；仅在 OpenAI 兼容服务确认支持时选择 JSON Schema。格式校验失败不会视为内容通过",
            Kind::Choice(&[("text", "文本兼容"), ("json_schema", "严格 JSON Schema")]),
        ),
        (
            "validation/timeout_seconds",
            "候选审查超时（秒）",
            "0 使用 60；可设置 1–300，包含排队和两次审查",
            Kind::Integer,
        ),
        (
            "validation/max_input_bytes",
            "审查输入上限（字节）",
            "0 使用 131072；可设置 1024–2097152，包含基础版本与候选",
            Kind::Integer,
        ),
        (
            "validation/max_output_tokens",
            "单次审查输出 Token 上限",
            "0 使用 1024；可设置 128–8192",
            Kind::Integer,
        ),
    ]
    .into_iter()
    .map(|(key, label, hint, kind)| {
        Field::new(format!("/skill_evolution/{key}"), label, hint, kind)
    })
    .collect()
}

pub(super) fn model_options(document: &Value) -> Vec<(Value, String)> {
    let mut options = vec![(json!(""), "未选择".into())];
    if let Some(models) = document["models"].as_array() {
        options.extend(
            models
                .iter()
                .filter_map(|model| model["name"].as_str())
                .map(|name| (json!(name), name.to_owned())),
        );
    }
    options
}

pub(super) fn inheritable_model_options(document: &Value) -> Vec<(Value, String)> {
    let mut options = model_options(document);
    options[0] = (Value::Null, "继承默认模型".into());
    options
}

fn confirm_conversion(document: &mut Value) -> Result<(), String> {
    let revision = document["config_revision"]
        .as_str()
        .filter(|value| !value.is_empty())
        .ok_or("缺少配置版本，请重新加载")?;
    if document["skill_evolution_migration"]["source_revision"] != revision {
        return Err("旧设置转换信息已过期，请重新加载".into());
    }
    let revision = revision.to_owned();
    if !document["skill_evolution"].is_object() {
        return Err("技能改进配置格式无效".into());
    }
    document["skill_evolution"]["mode"] = json!("review");
    document["skill_evolution"]["security_fail_closed"] = json!(true);
    document["skill_evolution_migration_ack"] = json!(revision);
    Ok(())
}

impl Waku {
    pub(super) fn render_deerflow_evolution(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let Some(document) = &state.draft else {
            return div().into_any_element();
        };
        let mut group = df_group(theme).child(df_label(
            "技能改进与审核",
            "支持后台生成、重复纠正、缺失技能报告及成功流程的自动发现，候选经过独立内容审查和可信样例评测。默认人工审核，可启用受限自动补丁及发布后观察。后台额度独立于聊天；修改设置后需要保存并应用。",
            theme,
        ));
        let migration = &document["skill_evolution_migration"];
        if migration["required"] == true {
            let confirmed =
                document["skill_evolution_migration_ack"] == document["config_revision"];
            group = group.child(df_label(
                "旧进化设置",
                "未转换时，关闭状态下的旧数据会继续保留。转换将移除下面的未接通字段，改为人工审核并要求安全审查通过；启用状态不变，保存前可重新加载撤销。历史技能和提案不受影响。",
                theme,
            ));
            for path in migration["removed_paths"]
                .as_array()
                .into_iter()
                .flatten()
                .filter_map(Value::as_str)
            {
                group = group.child(
                    div()
                        .text_size(sp(12.0))
                        .text_color(theme.text_secondary)
                        .child(path.to_owned()),
                );
            }
            group = group.child(df_button(
                "df-evolution-convert",
                if confirmed {
                    "已确认转换，待保存"
                } else {
                    "转换为当前审核配置"
                },
                confirmed
                    || !state.errors.is_empty()
                    || !CONVERSION_FIELDS
                        .iter()
                        .all(|path| state.can_edit_field(path)),
                confirmed,
                theme,
                cx,
                |this, _, cx| {
                    if !this.deerflow_settings.errors.is_empty()
                        || !CONVERSION_FIELDS
                            .iter()
                            .all(|path| this.deerflow_settings.can_edit_field(path))
                    {
                        return;
                    }
                    if let Some(document) = this.deerflow_settings.draft.as_mut() {
                        match confirm_conversion(document) {
                            Ok(()) => {
                                this.deerflow_settings.dirty = true;
                                this.deerflow_settings.edit_generation += 1;
                                this.deerflow_settings.notice = None;
                                this.deerflow_settings.error = None;
                                this.deerflow_settings.confirm_reload = false;
                                this.deerflow_rebind(cx);
                            }
                            Err(error) => this.deerflow_settings.error = Some(error),
                        }
                    }
                    cx.notify();
                },
            ));
        }
        group.into_any_element()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn deerflow_evolution_conversion_preserves_enablement_and_requires_current_revision() {
        let mut doc = json!({"config_revision":"r1","skill_evolution_migration":{"source_revision":"r1"},"skill_evolution":{"enabled":false,"mode":"auto_patch","security_fail_closed":false,"moderation_model_name":"safety"}});
        confirm_conversion(&mut doc).unwrap();
        assert_eq!(doc["skill_evolution"]["enabled"], false);
        assert_eq!(doc["skill_evolution"]["moderation_model_name"], "safety");
        assert_eq!(doc["skill_evolution"]["mode"], "review");
        assert_eq!(doc["skill_evolution"]["security_fail_closed"], true);
        assert_eq!(doc["skill_evolution_migration_ack"], "r1");
        doc["config_revision"] = json!("r2");
        let before = doc.clone();
        assert!(confirm_conversion(&mut doc).is_err());
        assert_eq!(doc, before);
    }

    #[test]
    fn deerflow_evolution_form_exposes_operator_generation_and_configured_models() {
        assert!(
            fields()
                .iter()
                .all(|field| !matches!(field.kind, Kind::Json))
        );
        assert!(
            !fields()
                .iter()
                .any(|field| field.path == "/skill_evolution/discovery/enabled")
        );
        for path in [
            "generation/enabled",
            "generation_model_name",
            "learning/enabled",
            "learning/extraction_model_name",
            "learning/consolidation_enabled",
            "learning/consolidation_model_name",
            "learning/cross_session_enabled",
            "learning/matching_enabled",
            "learning/matching_model_name",
            "learning/idle_seconds",
            "learning/timeout_seconds",
            "learning/max_input_bytes",
            "learning/max_output_tokens",
            "learning/max_daily_model_calls",
            "learning/max_daily_tokens",
            "learning/external_context_policy",
            "budget/max_daily_tokens",
            "discovery/operator_corrections_enabled",
            "discovery/missing_reports_enabled",
            "discovery/successful_workflows_enabled",
            "discovery/experience_patterns_enabled",
            "discovery/experience_new_skills_enabled",
            "discovery/experience_review_drafts_enabled",
            "discovery/new_skill_source_id",
            "discovery/repeat_threshold",
            "discovery/repeat_window_days",
            "discovery/cooldown_hours",
            "discovery/max_daily_proposals",
            "discovery/max_pending_proposals",
            "behavior/enabled",
            "behavior/suite_path",
            "behavior/timeout_seconds",
            "behavior/max_steps",
            "behavior/max_input_bytes",
            "behavior/max_output_tokens",
            "auto_patch/experience_patterns_enabled",
            "auto_patch/max_changed_lines",
            "auto_patch/max_daily_publications",
            "auto_patch/observation_hours",
            "auto_patch/min_observations",
            "auto_patch/rollback_failure_threshold",
        ] {
            assert!(
                fields()
                    .iter()
                    .any(|field| field.path == format!("/skill_evolution/{path}"))
            );
        }
        let choices = model_options(&json!({"models":[{"name":"one"},{"name":"two"}]}));
        assert_eq!(
            choices
                .iter()
                .map(|(value, _)| value.clone())
                .collect::<Vec<_>>(),
            vec![json!(""), json!("one"), json!("two")]
        );
        let inherited =
            inheritable_model_options(&json!({"models":[{"name":"one"},{"name":"two"}]}));
        assert_eq!(inherited[0], (Value::Null, "继承默认模型".into()));
        assert_eq!(&inherited[1..], &choices[1..]);
        for path in [
            "moderation_model_name",
            "evaluation_model_name",
            "generation_model_name",
            "learning/extraction_model_name",
            "learning/consolidation_model_name",
            "learning/matching_model_name",
        ] {
            assert!(
                fields()
                    .iter()
                    .any(|field| field.path == format!("/skill_evolution/{path}")
                        && matches!(field.kind, Kind::InheritableConfiguredModelReference))
            );
        }
        assert!(
            fields()
                .iter()
                .any(|field| field.path == "/skill_evolution/behavior/suite_path"
                    && matches!(field.kind, Kind::Text))
        );
        assert!(
            !fields()
                .iter()
                .any(|field| field.path == "/skill_evolution/behavior/model_name")
        );
    }
}
