//! Cached projections and bounded pages for the Go skill management service.
//! All requests use the parent's background executor and generation guard.
pub(super) use super::skills_contract::{Head, PageNav, Target, digest};
use super::*;
use serde::Deserialize;

pub(super) const PAGE_SIZE: usize = 20;

#[derive(Clone, Debug, Deserialize)]
struct CatalogSkill {
    target: Target,
    head: Head,
    builtin: bool,
    managed_source: bool,
    can_refresh_import: bool,
    #[serde(default)]
    record: Value,
    #[serde(default)]
    overlay_base: Value,
    #[serde(default)]
    import_observation: Value,
    #[serde(default)]
    export: Value,
}

impl CatalogSkill {
    fn validate(&self) -> anyhow::Result<()> {
        anyhow::ensure!(
            !self.target.source_id.is_empty()
                && !self.target.name.is_empty()
                && digest(&self.target.source_identity)
                && matches!(self.target.scope.as_str(), "global" | "workspace")
                && (self.target.scope != "workspace" || !self.target.workspace.is_empty())
                && self.head.state_version > 0
                && self.head.state_version <= i64::MAX as u64
                && (if self.head.exists {
                    digest(&self.head.version)
                } else {
                    self.head.version.is_empty() && !self.head.enabled
                }),
            "技能响应缺少有效来源或版本"
        );
        anyhow::ensure!(
            self.record.is_object() == self.head.exists,
            "技能条目状态不一致"
        );
        if !self.export.is_null() {
            let export_head: Head = serde_json::from_value(self.export["head"].clone())?;
            let export_target: Target = serde_json::from_value(self.export["target"].clone())?;
            anyhow::ensure!(
                export_head == self.head
                    && export_target == self.target
                    && self.export["stateToken"].as_str().is_some_and(digest),
                "导出状态与技能版本不一致"
            );
        }
        Ok(())
    }

    fn request(&self, operation: &str) -> Value {
        json!({"operation":operation,"target":self.target,"expected_head":self.head})
    }

    fn enable_request(&self) -> Value {
        let mut request = self.request("skill.set_enabled");
        request["enabled"] = json!(!self.head.enabled);
        request["request_key"] = json!(Uuid::new_v4().to_string());
        request
    }

    fn export_request(&self) -> Value {
        let mut request = self.request("skill.export.retry");
        request["expected_export_token"] = self.export["stateToken"].clone();
        request["request_key"] = json!(Uuid::new_v4().to_string());
        request
    }
}

#[derive(Clone, Debug, Deserialize)]
struct CatalogPage {
    skills: Vec<CatalogSkill>,
    sources: Vec<Value>,
    catalog_version: u64,
    #[serde(default)]
    next_cursor: String,
}

#[derive(Clone, Debug, Deserialize)]
struct CatalogDetail {
    skill: CatalogSkill,
    catalog_version: u64,
}

#[derive(Default)]
pub(super) struct SkillBrowser {
    pub(super) behavior: Option<behavior::BehaviorView>,
    pub(super) behavior_inspector: behavior_inspector::BehaviorInspector,
    pub(super) generation: generation::GenerationBrowser,
    pub(super) auto_patch: auto_patch::AutoPatchBrowser,
    pub(super) discovery: discovery::DiscoveryBrowser,
    pub(super) feedback: feedback::FeedbackBrowser,
    pub(super) missing: missing::MissingBrowser,
    pub(super) learning: learning_state::LearningBrowser,
    catalog: Option<CatalogPage>,
    catalog_nav: PageNav,
    selected: Option<CatalogDetail>,
    pub(super) catalog_stale: bool,
    pub(super) review_stale: bool,
    pub(super) proposal_nav: PageNav,
    pub(super) history_nav: PageNav,
    retry: Option<Value>,
    retry_service: Option<Value>,
    rollback: Option<Value>,
    learning_acknowledgment: bool,
}

pub(super) fn is_skill_operation(request: &Value) -> bool {
    request["operation"]
        .as_str()
        .is_some_and(|op| op.starts_with("skill.") || op.starts_with("proposal."))
}

// Applied generation may change on restart, but request-key reconciliation
// belongs to the original persistent service data. Never transfer a pending
// mutation to another configured store just because its counters match.
pub(super) fn service_identity(status: &Value) -> Value {
    json!({"config_path": status["config_path"], "user_data": status["user_data"]})
}

fn mutation(request: &Value) -> bool {
    learning_state::mutation(request)
        || matches!(
            request["operation"].as_str(),
            Some(
                "skill.set_enabled"
                    | "skill.generation.enqueue"
                    | "skill.generation.cancel"
                    | "skill.feedback.submit"
                    | "skill.missing.submit"
                    | "skill.export.retry"
                    | "proposal.approve"
                    | "proposal.reject"
                    | "proposal.rollback"
            )
        )
}

impl SkillBrowser {
    pub(super) fn set_learning_session(&mut self, session: Option<&str>) {
        self.learning.set_session(session);
        self.learning.invalidate();
        self.learning_acknowledgment = false;
    }

    pub(super) fn can_begin_at(&self, request: &Value, status: &Value) -> bool {
        self.can_begin(request)
            && (!(mutation(request) || self.learning.is_reconciliation(request))
                || self
                    .retry_service
                    .as_ref()
                    .is_none_or(|original| *original == service_identity(status)))
    }

    pub(super) fn begin_at(&mut self, request: &Value, status: &Value) {
        if mutation(request) && self.retry.is_none() {
            self.retry_service = Some(service_identity(status));
        }
        self.begin(request);
    }

    pub(super) fn is_original_retry(&self, request: &Value) -> bool {
        self.retry.as_ref() == Some(request)
    }

    pub(super) fn can_begin(&self, request: &Value) -> bool {
        self.learning.can_begin(request)
            && (request["operation"] != "skill.learning.creation_retry.prepare"
                || self
                    .generation
                    .creation_retry_previous()
                    .is_some_and(|(id, _)| request["retry"]["previous_generation_id"] == id))
            && (self.is_original_retry(request)
                || request["operation"] != "skill.learning.creation_retry.enqueue"
                || self
                    .generation
                    .creation_retry_previous()
                    .is_some_and(|(id, _)| request["retry"]["previous_generation_id"] == id))
            && (self.is_original_retry(request)
                || !learning_state::review_mutation(request)
                || request["operation"] != "skill.learning.pattern.adjudicate"
                || self.selected.as_ref().is_some_and(|detail| {
                    !self.catalog_stale
                        && request["review"]["target"] == json!(detail.skill.target)
                        && request["review"]["expected_head"] == json!(detail.skill.head)
                        && request["review"]["overlay_base"] == detail.skill.overlay_base
                }))
            && (!mutation(request)
                || self.retry.as_ref().map_or_else(
                    || self.missing.can_begin(request),
                    |original| original == request,
                ))
    }

    pub(super) fn begin(&mut self, request: &Value) {
        self.behavior_inspector.begin(request);
        self.generation.begin(request);
        self.feedback.begin(request);
        self.missing.begin(request);
        self.learning.begin(request);
        if learning_state::sources_changed(request) {
            self.revoke_learning_sources();
        }
        if mutation(request) {
            self.retry = Some(request.clone());
        }
    }

    pub(super) fn transport_failed(&mut self, request: &Value) {
        if mutation(request) {
            self.retry = Some(request.clone());
        }
        self.invalidate();
    }

    fn revoke_learning_sources(&mut self) {
        self.generation.invalidate_learning_sources();
        self.discovery.invalidate();
        self.behavior = None;
        self.behavior_inspector.invalidate();
        self.review_stale = true;
    }

    pub(super) fn invalidate(&mut self) {
        self.behavior = None;
        self.behavior_inspector.invalidate();
        self.generation.invalidate();
        self.auto_patch.invalidate();
        self.discovery.invalidate();
        self.feedback.invalidate();
        self.missing.invalidate();
        self.learning.invalidate();
        self.learning_acknowledgment = false;
        self.catalog_stale = true;
        self.review_stale = true;
        self.selected = None;
        self.rollback = None;
        if self.learning.pending_sources_changed() {
            self.revoke_learning_sources();
        }
    }

    fn backend_failed(&mut self, request: &Value, code: &str) {
        self.invalidate();
        if self.retry.as_ref() == Some(request)
            && matches!(
                code,
                "conflict"
                    | "permission_denied"
                    | "not_found"
                    | "invalid_request"
                    | "unsupported"
                    | "unsupported_operation"
                    | "validation_failed"
                    | "resource_limit"
            )
        {
            self.retry = None;
            self.retry_service = None;
            self.feedback.definitive_failure(request);
            self.missing.definitive_failure(request);
            self.learning.definitive_failure(request);
        }
    }

    fn accept_catalog(&mut self, request: &Value, data: Value) -> anyhow::Result<()> {
        let page: CatalogPage = serde_json::from_value(data)?;
        anyhow::ensure!(
            page.skills.len() <= PAGE_SIZE && page.sources.len() <= 32,
            "技能目录超出分页限制"
        );
        for skill in &page.skills {
            skill.validate()?;
        }
        self.catalog_nav
            .accept(request, &page.next_cursor, "cursor")?;
        if let Some(selected) = self.selected.as_mut() {
            if let Some(skill) = page
                .skills
                .iter()
                .find(|skill| skill.target == selected.skill.target)
            {
                selected.skill = skill.clone();
                selected.catalog_version = page.catalog_version;
            } else {
                self.selected = None;
            }
        }
        self.catalog = Some(page);
        self.catalog_stale = false;
        Ok(())
    }

    fn accept_detail(&mut self, request: &Value, data: Value) -> anyhow::Result<()> {
        if mutation(request) {
            let mut expected: Head = serde_json::from_value(request["expected_head"].clone())?;
            if request["operation"] == "skill.set_enabled" {
                let enabled = request["enabled"]
                    .as_bool()
                    .ok_or_else(|| anyhow::anyhow!("启停请求无效"))?;
                if !expected.managed || expected.enabled != enabled {
                    expected.state_version += 1;
                    expected.managed = true;
                    expected.enabled = enabled;
                }
            }
            let applied: Head = serde_json::from_value(data["applied_head"].clone())?;
            anyhow::ensure!(
                applied == expected && data["replayed"].is_boolean(),
                "技能操作响应缺少原请求结果"
            );
        }
        let detail: CatalogDetail = serde_json::from_value(data)?;
        detail.skill.validate()?;
        let target: Target = serde_json::from_value(request["target"].clone())?;
        anyhow::ensure!(target == detail.skill.target, "技能响应与请求来源不一致");
        if let Some(page) = self.catalog.as_mut() {
            if page.catalog_version != detail.catalog_version {
                self.catalog_stale = true;
            }
            if let Some(skill) = page
                .skills
                .iter_mut()
                .find(|skill| skill.target == detail.skill.target)
            {
                *skill = detail.skill.clone();
            }
        }
        self.selected = Some(detail);
        if self.retry.as_ref() == Some(request) {
            self.retry = None;
            self.retry_service = None;
        }
        Ok(())
    }

    pub(super) fn mutations_pending(&self) -> bool {
        self.retry.is_some()
    }

    pub(super) fn abandon_retry(&mut self) {
        self.retry_service = None;
        if let Some(request) = self.retry.take() {
            self.feedback.definitive_failure(&request);
            self.missing.definitive_failure(&request);
            self.learning.definitive_failure(&request);
        }
        self.invalidate();
    }

    pub(super) fn accept_review_page(
        &mut self,
        request: &Value,
        data: &Value,
    ) -> anyhow::Result<Vec<Value>> {
        let history = request["operation"] == "proposal.history";
        let rows = data[if history { "revisions" } else { "proposals" }]
            .as_array()
            .ok_or_else(|| anyhow::anyhow!("技能列表响应缺少条目"))?;
        anyhow::ensure!(rows.len() <= PAGE_SIZE, "技能列表超出分页限制");
        for row in rows {
            let target: Target = serde_json::from_value(row["target"].clone())?;
            anyhow::ensure!(
                !target.name.is_empty() && digest(&target.source_identity),
                "技能列表来源无效"
            );
            if history {
                anyhow::ensure!(
                    row["version"].as_u64().is_some_and(|v| v > 0),
                    "发布修订无效"
                );
            } else {
                anyhow::ensure!(
                    row["id"].as_str().is_some_and(|v| !v.is_empty())
                        && row["state_version"].as_u64().is_some_and(|v| v > 0),
                    "提案版本无效"
                );
            }
        }
        let (nav, next, field) = if history {
            (
                &mut self.history_nav,
                &data["revision_next_cursor"],
                "revision_cursor",
            )
        } else {
            (&mut self.proposal_nav, &data["next_cursor"], "cursor")
        };
        nav.accept(request, next.as_str().unwrap_or(""), field)?;
        self.review_stale = false;
        Ok(rows.clone())
    }

    pub(super) fn complete_mutation(&mut self, request: &Value) {
        self.invalidate();
        if self.retry.as_ref() == Some(request) {
            self.retry = None;
            self.retry_service = None;
        }
    }

    pub(super) fn generation_mutation_completed(&mut self, request: &Value) {
        if self.retry.as_ref() == Some(request) {
            self.retry = None;
            self.retry_service = None;
        }
    }

    pub(super) fn accept_learning(&mut self, request: &Value, data: Value) -> anyhow::Result<bool> {
        let before = self.learning.control.clone();
        self.learning.accept(request, data)?;
        if learning_state::mutation(request) {
            self.generation_mutation_completed(request);
            self.learning_acknowledgment = false;
        }
        let sources_changed =
            learning_state::sources_changed(request) || before != self.learning.control;
        if sources_changed {
            self.revoke_learning_sources();
        }
        Ok(sources_changed)
    }

    pub(super) fn learning_adjudication_request(&self, relation: &str) -> Option<Value> {
        let detail = self.selected.as_ref()?;
        if self.catalog_stale || !detail.skill.head.exists || !detail.skill.head.enabled {
            return None;
        }
        self.learning.adjudication_request(
            json!(detail.skill.target),
            json!(detail.skill.head),
            detail.skill.overlay_base.clone(),
            relation,
        )
    }

    pub(super) fn learning_target_caption(&self) -> Option<String> {
        let detail = self.selected.as_ref()?;
        (!self.catalog_stale).then(|| {
            format!(
                "{} / {} · head {} · 状态版本 {}",
                detail.skill.target.source_id,
                detail.skill.target.name,
                detail.skill.head.version,
                detail.skill.head.state_version
            )
        })
    }
}

pub(super) fn validate_proposal(
    request: &Value,
    proposal: &Value,
) -> anyhow::Result<Option<behavior::BehaviorView>> {
    let target: Target = serde_json::from_value(proposal["target"].clone())?;
    let _: Head = serde_json::from_value(proposal["base"].clone())?;
    let _: Head = serde_json::from_value(proposal["current_head"].clone())?;
    anyhow::ensure!(
        proposal["id"] == request["proposal_id"]
            && proposal["state_version"].as_u64().is_some_and(|v| v > 0)
            && digest(&target.source_identity),
        "提案响应缺少已观察版本或来源"
    );
    behavior::BehaviorView::parse(proposal)
}

impl Waku {
    pub(super) fn render_deerflow_skill_pending(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let browser = &state.skill_browser;
        let Some(request) = &browser.retry else {
            return div().into_any_element();
        };
        let unavailable = state.busy() || state.applying() || state.status["running"] != true;
        let learning = learning_state::mutation(request);
        let review = learning_state::review_mutation(request);
        let mut group = df_group(theme).child(df_label(
            "操作结果待确认",
            if review {
                format!("原会话 {}\n操作 {}\n原控制条件 {}\n人工控制与显式创建重试保存请求键回执。只可向原服务数据目录重发完整原请求；不能改目标、版本或请求键。回执是原操作结果，不代表当前状态。",
                    request["session_id"].as_str().unwrap_or(""), request["operation"].as_str().unwrap_or(""),
                    if request["operation"] == "skill.learning.creation_retry.enqueue" { &request["retry"] } else { &request["review"] })
            } else if learning {
                format!("原会话 {}\n操作 {}\n授权条件 {}\n学习操作没有请求键回执，不能把重发当作安全核对。原请求结果未明时，禁止新修改；当前状态核对仅限原服务数据目录。",
                    request["session_id"].as_str().unwrap_or(""), request["operation"].as_str().unwrap_or(""),
                    if request["operation"] == "skill.learning.control.set" { request["control"].to_string() } else { request["expected_state_version"].to_string() })
            } else {
                "保留了原请求和版本条件。重试使用同一请求键；状态冲突时请刷新核对结果。".to_owned()
            }, theme));
        if learning {
            if review {
                let retry = request.clone();
                group = group.child(df_button(
                    "df-learning-review-retry",
                    "核对 / 重试原人工操作",
                    unavailable || !browser.can_begin_at(&retry, &state.status),
                    false,
                    theme,
                    cx,
                    move |this, _, cx| this.deerflow_request("manage", retry.clone(), cx),
                ));
            } else if let Some(query) = browser.learning.reconciliation_request() {
                group = group.child(df_button(
                    "df-learning-reconcile",
                    "核对原会话当前状态",
                    unavailable || !browser.can_begin_at(&query, &state.status),
                    false,
                    theme,
                    cx,
                    move |this, _, cx| this.deerflow_request("manage", query.clone(), cx),
                ));
            } else {
                group = group.child(df_label("任务边界结果无法通过查询确认",
                    "当前服务没有 episode.start 的幂等回执或当前边界查询。不要重发同一操作；结束待确认状态只恢复界面操作，不证明原请求失败，也不新增边界。", theme));
            }
            if let Some(result) = &browser.learning.reconciliation {
                group = group.child(df_label("当前状态核对", result.clone(), theme));
            }
            group = group.child(df_button("df-learning-end-unresolved",
                if browser.learning_acknowledgment { "确认结束待确认状态（不重发）" } else { "我已核对，结束待确认状态" },
                unavailable, browser.learning_acknowledgment, theme, cx, |this, _, cx| {
                    if this.deerflow_settings.skill_browser.learning_acknowledgment {
                        this.deerflow_settings.skill_browser.abandon_retry();
                        this.deerflow_settings.confirm_manage = None;
                        this.deerflow_settings.notice = Some("已结束界面的待确认状态，没有重发、撤销或证明原操作失败。请刷新当前会话设置后继续。".into());
                    } else {
                        this.deerflow_settings.skill_browser.learning_acknowledgment = true;
                    }
                    cx.notify();
                }));
        } else {
            let retry = request.clone();
            group = group
                .child(df_button(
                    "df-skill-retry",
                    "重试原操作",
                    unavailable || !browser.can_begin_at(&retry, &state.status),
                    false,
                    theme,
                    cx,
                    move |this, _, cx| this.deerflow_request("manage", retry.clone(), cx),
                ))
                .child(df_button(
                    "df-skill-dismiss-retry",
                    "结束重试并刷新",
                    unavailable,
                    false,
                    theme,
                    cx,
                    |this, _, cx| {
                        this.deerflow_settings.skill_browser.abandon_retry();
                        this.deerflow_request(
                            "manage",
                            json!({"operation":"skill.list", "limit":PAGE_SIZE}),
                            cx,
                        );
                    },
                ));
        }
        group.into_any_element()
    }

    pub(super) fn deerflow_prepare_skill_rollback(
        &mut self,
        revision: Value,
        cx: &mut Context<Self>,
    ) {
        if self.deerflow_settings.busy() || self.deerflow_settings.skill_browser.mutations_pending()
        {
            return;
        }
        let target = revision["target"].clone();
        self.deerflow_settings.confirm_manage = None;
        self.deerflow_settings.skill_browser.rollback = Some(revision);
        self.deerflow_request(
            "manage",
            json!({"operation":"skill.get","target":target}),
            cx,
        );
    }

    pub(super) fn render_deerflow_skill_evaluation(
        &self,
        proposal: &Value,
        theme: Theme,
    ) -> AnyElement {
        let evaluation = &proposal["evaluation"];
        let check = |value: &Value| match value.as_str().unwrap_or("") {
            "passed" => "通过",
            "failed" => "未通过",
            "not_run" => "未执行",
            "unavailable" => "未能完成",
            _ => "未完成",
        };
        let mut group = div()
            .flex()
            .flex_col()
            .gap(px(6.0))
            .child(df_label(
                "提案状态",
                format!(
                    "{} · 状态版本 {} · 更新于 {}",
                    text(&proposal["status"]),
                    proposal["state_version"],
                    text(&proposal["updated_at"])
                ),
                theme,
            ))
            .child(df_label(
                "评测报告",
                format!(
                    "内容检查：{} · 行为评测：{} · 规则版本：{}",
                    check(&evaluation["security"]),
                    check(&evaluation["behavior"]),
                    text(&evaluation["validator_version"])
                ),
                theme,
            ));
        if let Some(reviewer) = proposal["reviewed_by"].as_str().filter(|s| !s.is_empty()) {
            group = group.child(df_label(
                "审查记录",
                format!(
                    "{reviewer} · {}\n{}",
                    text(&proposal["reviewed_at"]),
                    text(&proposal["review_note"])
                ),
                theme,
            ));
        }
        if evaluation["behavior"] == "not_run" {
            group = group.child(df_label(
                "行为评测尚未执行",
                "内容审查不能证明真实任务效果；本次由用户审查决定是否发布。",
                theme,
            ));
        }
        group = group.child(df_label(
            "候选与基础版本",
            format!(
                "基础：{}\n候选：{}",
                text(&proposal["base"]["version"]),
                text(&proposal["candidate_sha256"])
            ),
            theme,
        ));
        if let Some(overlay) = proposal["overlay_base"]["version"].as_str() {
            group = group.child(df_label("内置覆盖来源", overlay.to_owned(), theme));
        }
        if let Some(checks) = evaluation["model_checks"].as_array() {
            for model in checks.iter().take(16) {
                group = group.child(df_label(
                    format!(
                        "{} · {} / {} · {}",
                        text(&model["stage"]),
                        text(&model["provider"]),
                        text(&model["model"]),
                        if model["passed"] == true {
                            "通过"
                        } else {
                            "未通过"
                        }
                    ),
                    format!(
                        "{}\n用量：输入 {} / 输出 {} tokens{}",
                        text(&model["reason"]),
                        model["input_tokens"],
                        model["output_tokens"],
                        if model["usage_estimated"] == true {
                            "（估算）"
                        } else {
                            ""
                        }
                    ),
                    theme,
                ));
            }
        }
        if let Some(evidence) = evaluation["evidence"].as_array() {
            for item in evidence.iter().take(32) {
                group = group.child(df_label("验证依据", text(item), theme));
            }
        }
        if let Some(behavior) = self
            .deerflow_settings
            .skill_browser
            .behavior
            .as_ref()
            .filter(|behavior| behavior.matches(proposal))
        {
            group = group.child(behavior.render(theme));
        } else if evaluation["behavior_report"].is_object() {
            group = group.child(df_label(
                "行为评测报告需要刷新",
                "服务或提案状态已变化；重新选择当前提案后查看样例对照。",
                theme,
            ));
        } else if evaluation["behavior"] == "passed" {
            group = group.child(df_label(
                "未提供结构化行为报告",
                "此历史报告未包含可核对的样例对照记录。",
                theme,
            ));
        } else if evaluation["behavior"] == "unavailable" {
            group = group.child(df_label(
                "行为评测未能完成",
                "没有完整的基础与候选对照结果。查看提案处理结果和维护作业错误状态。",
                theme,
            ));
        }
        if let Some(error) = proposal["error"].as_str().filter(|s| !s.is_empty()) {
            group = group.child(df_label("提案处理结果", error.to_owned(), theme));
        }
        group.into_any_element()
    }

    pub(super) fn deerflow_skill_response(
        &mut self,
        request: &Value,
        envelope: Value,
        cx: &mut Context<Self>,
    ) {
        if envelope["ok"] != true {
            self.deerflow_settings.confirm_manage = None;
            let code = envelope["code"].as_str().unwrap_or("");
            if request["operation"] == "proposal.behavior" && code == "not_found" {
                match self
                    .deerflow_settings
                    .skill_browser
                    .behavior_inspector
                    .accept_absent(request)
                {
                    Ok(()) => {
                        self.deerflow_settings.notice = Some("此提案没有行为评测记录。".into())
                    }
                    Err(error) => self.deerflow_service_error(&error.to_string(), cx),
                }
                return;
            }
            self.deerflow_settings
                .skill_browser
                .backend_failed(request, code);
            let missing = request["operation"]
                .as_str()
                .is_some_and(|op| op.starts_with("skill.missing."));
            let learning = learning_state::is_operation(request);
            let message = match code {
                "conflict" if learning => {
                    "学习授权或作业版本已变化，请刷新后核对；本次修改未覆盖新状态。"
                }
                "permission_denied" if learning => {
                    "当前会话、工作区共享或学习授权不可用，请刷新贡献设置。"
                }
                "not_found" if learning => "会话或学习来源已不存在，请刷新当前会话。",
                "conflict" if missing => {
                    "报告已被更新，请刷新当前报告后重试；本次修改未覆盖新状态。"
                }
                "permission_denied" if missing => "当前会话或任务不可访问，请刷新任务列表。",
                "not_found" if missing => "会话或任务已不存在，请刷新任务列表。",
                "conflict" => "技能或导出状态已变化，请刷新后重新检查；本次操作未覆盖新状态。",
                "permission_denied" => "技能源授权已变化，请刷新目录。",
                "unsupported" | "unsupported_operation" => {
                    "当前服务不支持此技能管理操作，请使用配套的 Eino Go 服务。"
                }
                "not_found" => "技能或提案已不存在，请刷新目录。",
                _ => envelope["error"]
                    .as_str()
                    .unwrap_or("技能管理失败，请刷新状态后重试。"),
            };
            self.deerflow_service_error(message, cx);
            return;
        }
        let data = envelope["data"].clone();
        let operation = request["operation"].as_str().unwrap_or("");
        let result = match operation {
            op if op.starts_with("skill.learning.") => {
                let result = self
                    .deerflow_settings
                    .skill_browser
                    .accept_learning(request, data);
                if result.as_ref().is_ok_and(|changed| *changed) {
                    self.deerflow_settings.proposals.clear();
                    self.deerflow_settings.proposal = None;
                    self.deerflow_settings
                        .proposal_diff
                        .update(cx, |input, cx| input.set_content(String::new(), cx));
                }
                result.map(|_| ())
            }
            "skill.discovery.status" | "skill.discovery.list" => self
                .deerflow_settings
                .skill_browser
                .discovery
                .accept(request, data),
            "skill.auto_patch.status" | "skill.auto_patch.list" => self
                .deerflow_settings
                .skill_browser
                .auto_patch
                .accept(request, data),
            "proposal.behavior" | "proposal.behavior.receipts" => self
                .deerflow_settings
                .skill_browser
                .behavior_inspector
                .accept(request, data),
            op if op.starts_with("skill.missing.") => {
                let result = self
                    .deerflow_settings
                    .skill_browser
                    .missing
                    .accept(request, data);
                if result.is_ok() {
                    self.deerflow_settings
                        .skill_browser
                        .generation_mutation_completed(request);
                }
                result
            }
            op if op.starts_with("skill.signals.") || op.starts_with("skill.feedback.") => {
                let result = self
                    .deerflow_settings
                    .skill_browser
                    .feedback
                    .accept(request, data);
                if result.is_ok() {
                    self.deerflow_settings
                        .skill_browser
                        .generation_mutation_completed(request);
                }
                result
            }
            op if op.starts_with("skill.generation.") => {
                let result = self
                    .deerflow_settings
                    .skill_browser
                    .generation
                    .accept(request, data);
                if result.is_ok() {
                    self.deerflow_settings
                        .skill_browser
                        .generation_mutation_completed(request);
                }
                result
            }
            "skill.list" => self
                .deerflow_settings
                .skill_browser
                .accept_catalog(request, data),
            "skill.get" | "skill.set_enabled" | "skill.imports.refresh" | "skill.export.retry" => {
                self.deerflow_settings
                    .skill_browser
                    .accept_detail(request, data)
            }
            _ => {
                if mutation(request) {
                    if let Err(error) = validate_review_result(request, &data) {
                        self.deerflow_settings
                            .skill_browser
                            .transport_failed(request);
                        self.deerflow_service_error(&error.to_string(), cx);
                        return;
                    }
                }
                self.deerflow_managed(request, data, cx);
                return;
            }
        };
        if let Err(error) = result {
            self.deerflow_settings
                .skill_browser
                .transport_failed(request);
            self.deerflow_service_error(&format!("技能管理响应无效：{error}"), cx);
            return;
        }
        self.deerflow_settings.confirm_manage = None;
        if operation == "skill.get" {
            if let Some(revision) = self.deerflow_settings.skill_browser.rollback.take() {
                let skill = &self
                    .deerflow_settings
                    .skill_browser
                    .selected
                    .as_ref()
                    .unwrap()
                    .skill;
                if revision["target"] == request["target"] {
                    let command = json!({"operation":"proposal.rollback", "target":skill.target,
                        "source_id":skill.target.source_id,"name":skill.target.name,"version":revision["version"],
                        "expected_head":skill.head,"request_key":Uuid::new_v4().to_string()});
                    self.deerflow_settings.confirm_manage = Some((
                        format!(
                            "将 {} / {} 从状态版本 {} 恢复到修订 {}，并创建新修订。历史任务保留原快照。",
                            skill.target.source_id,
                            skill.target.name,
                            skill.head.state_version,
                            revision["version"]
                        ),
                        command,
                    ));
                }
            }
        }
        self.deerflow_settings.notice = Some(
            match operation {
                "skill.set_enabled" => "技能启停状态已保存，对新任务生效。",
                "skill.imports.refresh" => "源变化检查已完成；已发布技能保持当前版本。",
                "skill.export.retry" => "已提交导出检查与重试；刷新详情可查看结果。",
                "skill.generation.enqueue" => {
                    "生成请求已入队。刷新作业查看进展；发布遵循审核和自动补丁规则。"
                }
                "skill.generation.cancel" => "生成作业已取消。已发生的模型用量仍计入维护额度。",
                "skill.feedback.submit" => {
                    "反馈操作已确认。刷新当前反馈核对修订及归因；已启用的自动发现和发布观察可使用有效证据。"
                }
                "skill.missing.submit" => "缺失技能报告操作已确认。刷新当前报告核对修订。",
                "skill.learning.control.set" => "学习贡献操作已确认；请刷新历史和待审核提案核对当前来源。",
                "skill.learning.episode.start" => "新任务边界操作已确认，只影响之后的真实输入。",
                "skill.learning.cancel" => "学习作业取消已确认；已发生的维护用量保留。",
                _ => "技能状态已刷新。",
            }
            .into(),
        );
    }

    pub(super) fn render_deerflow_skill_catalog(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let browser = &state.skill_browser;
        let unavailable =
            state.busy() || state.applying() || state.status["running"].as_bool() != Some(true);
        let mut group = df_group(theme).child(df_label(
            "已安装技能",
            "由 Eino Go 服务管理；启停立即保存，对新任务生效。",
            theme,
        ));
        let mut filters = div().flex().flex_wrap().gap(px(6.0));
        for (status, label) in [
            ("", "刷新全部"),
            ("enabled", "已启用"),
            ("disabled", "已停用"),
            ("deleted", "已删除"),
        ] {
            filters = filters.child(df_button(
                SharedString::from(format!("df-skills-{status}")),
                label,
                unavailable,
                false,
                theme,
                cx,
                move |this, _, cx| {
                    let mut query = json!({"operation":"skill.list","limit":PAGE_SIZE});
                    if !status.is_empty() {
                        query["status"] = json!(status);
                    }
                    this.deerflow_settings.skill_browser.rollback = None;
                    this.deerflow_request("manage", query, cx);
                },
            ));
        }
        group = group.child(filters);
        group = group
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(8.0))
                    .child(
                        TextField::new("df-catalog-search", self.skills_search.clone())
                            .w(px(260.0)),
                    )
                    .child(df_button(
                        "df-catalog-search-submit",
                        "按技能名称查询",
                        unavailable,
                        false,
                        theme,
                        cx,
                        |this, _, cx| {
                            let name = this.skills_search.read(cx).content().trim().to_owned();
                            let mut request = json!({"operation":"skill.list","limit":PAGE_SIZE});
                            if !name.is_empty() {
                                request["name"] = json!(name);
                            }
                            this.deerflow_request("manage", request, cx);
                        },
                    )),
            )
            .child(
                div()
                    .text_size(sp(12.0))
                    .text_color(theme.text_tertiary)
                    .child("名称精确匹配；留空查询全部。"),
            );
        if browser.catalog_stale {
            group = group.child(df_label(
                "目录需要刷新",
                "服务或发布状态已变化，刷新目录后继续翻页。",
                theme,
            ));
        }
        if let Some(page) = &browser.catalog {
            group = group.child(df_label(
                format!(
                    "目录版本 {} · 第 {} 页",
                    page.catalog_version,
                    browser.catalog_nav.page_number()
                ),
                format!(
                    "本页 {} 项 · {} 个已配置来源",
                    page.skills.len(),
                    page.sources.len()
                ),
                theme,
            ));
            for skill in &page.skills {
                let target = serde_json::to_value(&skill.target).unwrap();
                let key = format!("{}-{}", skill.target.source_identity, skill.target.name);
                let status = if !skill.head.exists {
                    "已删除"
                } else if skill.head.enabled {
                    "已启用"
                } else {
                    "已停用"
                };
                group = group.child(df_button(
                    SharedString::from(format!("df-skill-{key}")),
                    format!(
                        "{} / {} · {status}",
                        skill.target.source_id, skill.target.name
                    ),
                    unavailable,
                    browser
                        .selected
                        .as_ref()
                        .is_some_and(|detail| detail.skill.target == skill.target),
                    theme,
                    cx,
                    move |this, _, cx| {
                        this.deerflow_settings.skill_browser.rollback = None;
                        this.deerflow_request(
                            "manage",
                            json!({"operation":"skill.get","target":target}),
                            cx,
                        );
                    },
                ));
            }
            group = group.child(self.render_skill_page_nav(
                "catalog",
                &browser.catalog_nav,
                unavailable || browser.catalog_stale,
                theme,
                cx,
            ));
        } else {
            group = group.child(df_label(
                "尚未加载技能目录",
                "启动服务后点击刷新，读取当前服务的已发布技能。",
                theme,
            ));
        }
        if let Some(detail) = &browser.selected {
            let skill = &detail.skill;
            let disabled = unavailable || state.dirty || browser.mutations_pending();
            group = group.child(df_label(
                format!("{} / {}", skill.target.source_id, skill.target.name),
                format!(
                    "{} · 状态版本 {} · {}{}",
                    skill.target.scope,
                    skill.head.state_version,
                    if skill.builtin {
                        "内置技能"
                    } else {
                        "用户技能"
                    },
                    if skill.managed_source {
                        " · 托管来源"
                    } else {
                        ""
                    }
                ),
                theme,
            ));
            if let Some(description) = skill.record["description"].as_str() {
                group = group.child(df_label("说明", description.to_owned(), theme));
            }
            if skill.head.exists {
                group = group.child(df_label("当前内容版本", skill.head.version.clone(), theme));
            }
            if let Some(base) = skill.overlay_base["version"].as_str() {
                group = group.child(df_label(
                    "内置覆盖版本",
                    format!("基于原始版本 {base}；分发源文件保持只读。"),
                    theme,
                ));
            }
            let mut actions = div().flex().flex_wrap().gap(px(6.0));
            let target = serde_json::to_value(&skill.target).unwrap();
            actions = actions.child(df_button(
                "df-skill-refresh",
                "刷新详情",
                unavailable,
                false,
                theme,
                cx,
                move |this, _, cx| {
                    this.deerflow_request(
                        "manage",
                        json!({"operation":"skill.get","target":target}),
                        cx,
                    );
                },
            ));
            if skill.head.exists {
                let observed = skill.clone();
                actions = actions.child(df_button(
                    "df-skill-toggle",
                    if skill.head.enabled {
                        "停用技能"
                    } else {
                        "启用技能"
                    },
                    disabled,
                    false,
                    theme,
                    cx,
                    move |this, _, cx| {
                        this.deerflow_request("manage", observed.enable_request(), cx);
                    },
                ));
            }
            if skill.can_refresh_import {
                let request = skill.request("skill.imports.refresh");
                actions = actions.child(df_button(
                    "df-skill-import-refresh",
                    "检查源变化",
                    unavailable,
                    false,
                    theme,
                    cx,
                    move |this, _, cx| {
                        this.deerflow_request("manage", request.clone(), cx);
                    },
                ));
            }
            if !skill.export.is_null() {
                let observed = skill.clone();
                actions = actions.child(df_button(
                    "df-skill-export-retry",
                    "重试导出",
                    disabled,
                    false,
                    theme,
                    cx,
                    move |this, _, cx| {
                        this.deerflow_request("manage", observed.export_request(), cx);
                    },
                ));
            }
            group = group.child(actions);
            if state.dirty {
                group = group.child(df_label(
                    "草稿尚未保存",
                    "先处理配置草稿，再修改技能状态。",
                    theme,
                ));
            }
            if !skill.import_observation.is_null() {
                group = group.child(df_label(
                    "源变化检查",
                    format!(
                        "{} · {}",
                        status_label(&skill.import_observation["status"]),
                        text(&skill.import_observation["checkedAt"])
                    ),
                    theme,
                ));
                if let Some(changes) = skill.import_observation["changes"].as_array() {
                    for change in changes.iter().take(100) {
                        group = group.child(df_label(
                            text(&change["path"]),
                            text(&change["change"]),
                            theme,
                        ));
                    }
                }
                if skill.import_observation["truncated"] == true {
                    group = group.child(df_label(
                        "差异已截断",
                        "报告最多包含 100 条文件差异。",
                        theme,
                    ));
                }
            }
            if !skill.export.is_null() {
                group = group.child(df_label(
                    "导出状态",
                    format!(
                        "{} · 尝试 {} 次 · {}",
                        status_label(&skill.export["status"]),
                        skill.export["attempts"],
                        text(&skill.export["updatedAt"])
                    ),
                    theme,
                ));
                if let Some(directory) = skill.export["directory"].as_str() {
                    group = group.child(df_label("导出视图", directory.to_owned(), theme));
                }
                if skill.export["errorCode"] == "filesystem_unavailable" {
                    group = group.child(df_label(
                        "导出目录暂不可写",
                        "已发布版本仍可使用。检查目录后重试导出。",
                        theme,
                    ));
                }
            }
        }
        group.into_any_element()
    }

    pub(super) fn render_skill_page_nav(
        &self,
        kind: &str,
        nav: &PageNav,
        disabled: bool,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let mut row = div().flex().flex_wrap().gap(px(6.0));
        for (next, label) in [(false, "上一页"), (true, "下一页")] {
            let request = nav.request(next);
            row = row.child(df_button(
                SharedString::from(format!("df-page-{kind}-{next}")),
                label,
                disabled || request.is_none(),
                false,
                theme,
                cx,
                move |this, _, cx| {
                    if let Some(request) = &request {
                        this.deerflow_request("manage", request.clone(), cx);
                    }
                },
            ));
        }
        row.into_any_element()
    }
}

#[cfg(test)]
#[path = "skills_learning_tests.rs"]
mod learning_transport_tests;

fn text(value: &Value) -> String {
    value.as_str().unwrap_or("—").to_owned()
}

fn status_label(value: &Value) -> &str {
    match value.as_str().unwrap_or("") {
        "unchanged" => "未变化",
        "changed" => "源文件已变化",
        "missing" => "源文件缺失",
        "invalid" => "源内容无效",
        "unavailable" => "源暂不可读",
        "pending" => "等待同步",
        "synced" => "已同步",
        "failed" => "同步失败",
        _ => "状态未知",
    }
}

fn validate_review_result(request: &Value, data: &Value) -> anyhow::Result<()> {
    anyhow::ensure!(data["catalog_version"].is_u64(), "管理结果缺少目录版本");
    if request["operation"] == "proposal.rollback" {
        anyhow::ensure!(
            data["revision"]["target"] == request["target"]
                && data["revision"]["rollback_of"] == request["version"]
                && data["revision"]["version"].as_u64().is_some_and(|v| v > 0),
            "回滚结果与请求不一致"
        );
    } else {
        let expected_status = if request["operation"] == "proposal.approve" {
            "published"
        } else {
            "rejected"
        };
        anyhow::ensure!(
            data["proposal"]["id"] == request["proposal_id"]
                && data["proposal"]["status"] == expected_status
                && data["proposal"]["state_version"]
                    .as_u64()
                    .is_some_and(|v| v > 0),
            "审查结果与请求不一致"
        );
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn deerflow_generation_transport_failure_retains_original_mutation() {
        for request in [
            json!({"operation":"skill.generation.enqueue","target":{"source_id":"project"},"request_key":"stable-key","reason":"original intent","expected_head":{"state_version":0}}),
            json!({"operation":"skill.generation.cancel","job_id":"smj_one","expected_state_version":7}),
            json!({"operation":"skill.missing.submit","session_id":"native-session","run_id":"root-run","expected_report_version":0,"request_key":"stable-key","kind":"missing_skill","summary":"Need a reusable workflow"}),
        ] {
            let mut browser = SkillBrowser::default();
            browser.begin(&request);
            browser.transport_failed(&request);
            assert!(browser.mutations_pending());
            assert!(browser.can_begin(&request));
            let mut changed = request.clone();
            changed["request_key"] = json!("new-key");
            assert!(!browser.can_begin(&changed));
            assert!(
                browser.can_begin(&json!({"operation":"skill.generation.get","job_id":"smj_one"}))
            );
            browser.generation_mutation_completed(&json!({"operation":"skill.generation.status"}));
            assert_eq!(browser.retry.as_ref(), Some(&request));
            browser.generation_mutation_completed(&request);
            assert!(!browser.mutations_pending());
            browser.begin(&request);
            browser.backend_failed(&request, "resource_limit");
            assert!(!browser.mutations_pending());
        }
    }

    fn skill() -> Value {
        json!({
            "target":{"source_id":"project","source_identity":"a".repeat(64),"name":"research","scope":"global"},
            "head":{"version":"b".repeat(64),"state_version":2,"exists":true,"enabled":true,"managed":false},
            "record":{"description":"Research sources","enabled":true},
            "builtin":true,"managed_source":false,"can_refresh_import":false
        })
    }

    #[test]
    fn navigation_keeps_pages_bounded_and_filters_bound() {
        let first = json!({"operation":"skill.list","status":"disabled","limit":20});
        let mut nav = PageNav::default();
        nav.accept(&first, "next-one", "cursor").unwrap();
        assert!(nav.request(false).is_none());
        let second = nav.request(true).unwrap();
        assert_eq!(second["status"], "disabled");
        nav.accept(&second, "next-two", "cursor").unwrap();
        let third = nav.request(true).unwrap();
        nav.accept(&third, "", "cursor").unwrap();
        assert!(nav.request(true).is_none());
        assert_eq!(nav.request(false).unwrap(), second);
        nav.accept(&second, "next-two", "cursor").unwrap();
        assert_eq!(nav.request(false).unwrap(), first);
        let mut wrong_filter = third.clone();
        wrong_filter["status"] = json!("enabled");
        assert!(nav.accept(&wrong_filter, "", "cursor").is_err());
        assert_eq!(nav.request(false).unwrap(), first);
        nav.accept(&first, "next-one", "cursor").unwrap();
        assert!(nav.request(false).is_none());
    }

    #[test]
    fn catalog_requires_full_state_and_rejects_rebound_detail() {
        let mut browser = SkillBrowser::default();
        let entry = skill();
        let list = json!({"operation":"skill.list","limit":20});
        browser
            .accept_catalog(
                &list,
                json!({"skills":[entry.clone()],"sources":[],"catalog_version":3}),
            )
            .unwrap();
        let get = json!({"operation":"skill.get","target":entry["target"]});
        browser
            .accept_detail(&get, json!({"skill":entry.clone(),"catalog_version":3}))
            .unwrap();
        let mut rebound = entry.clone();
        rebound["target"]["source_identity"] = json!("c".repeat(64));
        assert!(
            browser
                .accept_detail(&get, json!({"skill":rebound,"catalog_version":3}))
                .is_err()
        );
        for key in ["state_version", "enabled", "managed"] {
            let mut incomplete = entry.clone();
            incomplete["head"].as_object_mut().unwrap().remove(key);
            assert!(
                browser
                    .accept_catalog(
                        &list,
                        json!({"skills":[incomplete],"sources":[],"catalog_version":3})
                    )
                    .is_err()
            );
        }
        let overflow = vec![entry; PAGE_SIZE + 1];
        assert!(
            browser
                .accept_catalog(
                    &list,
                    json!({"skills":overflow,"sources":[],"catalog_version":3})
                )
                .is_err()
        );
        assert_eq!(browser.catalog.as_ref().unwrap().skills.len(), 1);
        assert_eq!(browser.catalog.as_ref().unwrap().catalog_version, 3);
    }

    #[test]
    fn interrupted_mutation_keeps_original_key_and_applied_head() {
        let mut browser = SkillBrowser::default();
        let mut entry = skill();
        let parsed: CatalogSkill = serde_json::from_value(entry.clone()).unwrap();
        let request = parsed.enable_request();
        assert_eq!(request["target"], entry["target"]);
        assert_eq!(request["expected_head"], entry["head"]);
        browser.begin(&request);
        browser.transport_failed(&request);
        assert_eq!(browser.retry.as_ref(), Some(&request));
        assert!(browser.can_begin(&request));
        let mut different = request.clone();
        different["request_key"] = json!("different-request");
        assert!(!browser.can_begin(&different));
        let read = json!({"operation":"skill.list","limit":20});
        assert!(browser.can_begin(&read));
        browser.backend_failed(&read, "conflict");
        assert_eq!(browser.retry.as_ref(), Some(&request));
        entry["head"]["managed"] = json!(true);
        entry["head"]["enabled"] = json!(false);
        entry["head"]["state_version"] = json!(3);
        let applied = entry["head"].clone();
        // A later operator already changed the current skill. Replaying must
        // report that current state while retaining the original applied head.
        entry["head"]["enabled"] = json!(true);
        entry["head"]["state_version"] = json!(4);
        assert!(
            browser
                .accept_detail(&request, json!({"skill":entry,"catalog_version":5}))
                .is_err()
        );
        assert!(browser.mutations_pending());
        browser
            .accept_detail(
                &request,
                json!({"skill":entry,"catalog_version":5,"applied_head":applied,"replayed":true}),
            )
            .unwrap();
        assert!(!browser.mutations_pending());
        assert!(browser.selected.as_ref().unwrap().skill.head.enabled);
        assert_eq!(
            browser.selected.as_ref().unwrap().skill.head.state_version,
            4
        );
    }

    #[test]
    fn exporter_refresh_does_not_expire_catalog_but_head_change_does() {
        let mut browser = SkillBrowser::default();
        let mut entry = skill();
        entry["head"]["managed"] = json!(true);
        entry["export"] = json!({"target":entry["target"],"head":entry["head"],"status":"pending","stateToken":"c".repeat(64)});
        browser.accept_catalog(&json!({"operation":"skill.list","limit":20}), json!({"skills":[entry.clone()],"sources":[],"catalog_version":5,"next_cursor":"next"})).unwrap();
        entry["export"]["stateToken"] = json!("d".repeat(64));
        entry["export"]["status"] = json!("synced");
        let get = json!({"operation":"skill.get","target":entry["target"]});
        browser
            .accept_detail(&get, json!({"skill":entry.clone(),"catalog_version":5}))
            .unwrap();
        assert!(!browser.catalog_stale);
        let retry = browser.selected.as_ref().unwrap().skill.export_request();
        assert_eq!(
            retry["expected_export_token"],
            entry["export"]["stateToken"]
        );
        entry["head"]["state_version"] = json!(4);
        entry["export"]["head"] = entry["head"].clone();
        browser
            .accept_detail(&get, json!({"skill":entry,"catalog_version":6}))
            .unwrap();
        assert!(browser.catalog_stale);
        browser.invalidate();
        assert!(browser.selected.is_none());
    }

    #[test]
    fn proposal_and_history_pagination_are_independent() {
        let target = skill()["target"].clone();
        let proposal = json!({"id":"p1","target":target,"state_version":2});
        let revision = json!({"version":1,"target":target});
        let mut browser = SkillBrowser::default();
        let list = json!({"operation":"proposal.list","status":"pending_review","limit":20});
        browser
            .accept_review_page(
                &list,
                &json!({"proposals":[proposal],"next_cursor":"proposal-next"}),
            )
            .unwrap();
        let history = json!({"operation":"proposal.history","limit":20});
        browser.accept_review_page(&history, &json!({"revisions":[revision],"revision_next_cursor":"history-next","next_cursor":"must-not-use"})).unwrap();
        assert_eq!(
            browser.proposal_nav.request(true).unwrap()["cursor"],
            "proposal-next"
        );
        let next = browser.history_nav.request(true).unwrap();
        assert_eq!(next["revision_cursor"], "history-next");
        assert!(next.get("cursor").is_none());
        assert_eq!(
            browser.proposal_nav.request(true).unwrap()["status"],
            "pending_review"
        );
    }

    #[test]
    fn review_details_and_results_require_observed_identity_and_state() {
        let entry = skill();
        let request = json!({"operation":"proposal.get","proposal_id":"p1"});
        let mut detail = json!({"id":"p1","target":entry["target"],"base":entry["head"],"current_head":entry["head"],"state_version":2});
        validate_proposal(&request, &detail).unwrap();
        detail["id"] = json!("p2");
        assert!(validate_proposal(&request, &detail).is_err());
        let rollback =
            json!({"operation":"proposal.rollback","target":entry["target"],"version":1});
        let result = json!({"catalog_version":4,"revision":{"version":3,"target":entry["target"],"rollback_of":1}});
        validate_review_result(&rollback, &result).unwrap();
        let mut wrong = result.clone();
        wrong["revision"]["target"]["source_identity"] = json!("e".repeat(64));
        assert!(validate_review_result(&rollback, &wrong).is_err());
    }

    #[test]
    fn missing_retry_stays_bound_to_original_service_store() {
        let mut browser = SkillBrowser::default();
        let status = json!({"config_path":"C:/bundle-a/config.yaml", "user_data":"C:/bundle-a/user-data", "applied_generation":8});
        let original = json!({"operation":"skill.missing.submit", "session_id":"session-a", "run_id":"run-a", "expected_report_version":0, "request_key":"stable-key", "kind":"missing_skill", "summary":"Missing export capability"});
        // Install an already-sent operation to isolate shared transport state;
        // fresh report authority is covered by MissingBrowser's own tests.
        assert!(!browser.can_begin_at(&original, &status));
        browser.begin_at(&original, &status);
        browser.transport_failed(&original);
        assert!(browser.is_original_retry(&original));
        assert!(browser.can_begin_at(&original, &status));

        let mut new_request = original.clone();
        new_request["request_key"] = json!("new-key");
        assert!(!browser.can_begin_at(&new_request, &status));
        for field in ["config_path", "user_data"] {
            let mut another_store = status.clone();
            another_store[field] = json!("C:/bundle-b/another-path");
            assert!(!browser.can_begin_at(&original, &another_store));
            let read =
                json!({"operation":"skill.missing.targets", "session_id":"session-b", "limit":20});
            assert!(browser.can_begin_at(&read, &another_store));
            browser.begin_at(&read, &another_store);
            assert_eq!(browser.retry.as_ref(), Some(&original));
            assert_eq!(browser.retry_service, Some(service_identity(&status)));
        }
    }

    #[test]
    fn service_restart_preserves_original_retry_and_completion_clears_binding() {
        let mut browser = SkillBrowser::default();
        let original = json!({"operation":"skill.missing.submit", "session_id":"session-a", "run_id":"run-a", "expected_report_version":0, "request_key":"stable-key", "kind":"missing_skill", "summary":"Missing export capability"});
        let initial = json!({"config_path":"C:/bundle/config.yaml", "user_data":"C:/bundle/user-data", "applied_generation":8});
        browser.begin_at(&original, &initial);
        browser.transport_failed(&original);
        for generation in [0, 9] {
            let mut restarted = initial.clone();
            restarted["applied_generation"] = json!(generation);
            browser.invalidate();
            assert!(browser.can_begin_at(&original, &restarted));
            browser.begin_at(&original, &restarted);
            assert_eq!(browser.retry.as_ref(), Some(&original));
            assert_eq!(browser.retry_service, Some(service_identity(&initial)));
        }
        let read =
            json!({"operation":"skill.missing.get", "session_id":"session-a", "run_id":"run-a"});
        browser.generation_mutation_completed(&read);
        assert!(browser.mutations_pending());
        assert!(browser.retry_service.is_some());
        browser.generation_mutation_completed(&original);
        assert!(!browser.mutations_pending());
        assert!(browser.retry_service.is_none());
    }

    #[test]
    fn definitive_missing_failure_or_abandon_releases_service_binding() {
        let original = json!({"operation":"skill.missing.submit", "session_id":"session-a", "run_id":"run-a", "expected_report_version":0, "request_key":"stable-key", "kind":"missing_skill", "summary":"Missing export capability"});
        let next_mutation = json!({"operation":"skill.generation.cancel", "job_id":"smj-next", "expected_state_version":1});
        let first_store =
            json!({"config_path":"C:/first/config.yaml", "user_data":"C:/first/user-data"});
        let second_store =
            json!({"config_path":"C:/second/config.yaml", "user_data":"C:/second/user-data"});
        for code in [
            "conflict",
            "permission_denied",
            "not_found",
            "invalid_request",
            "unsupported",
            "unsupported_operation",
            "validation_failed",
            "resource_limit",
        ] {
            let mut browser = SkillBrowser::default();
            browser.begin_at(&original, &first_store);
            browser.backend_failed(&original, code);
            assert!(!browser.mutations_pending(), "{code}");
            assert!(browser.retry_service.is_none(), "{code}");
            assert!(!browser.can_begin_at(&original, &second_store), "{code}");
            assert!(
                browser.can_begin_at(&next_mutation, &second_store),
                "{code}"
            );
        }

        let mut browser = SkillBrowser::default();
        browser.begin_at(&original, &first_store);
        browser.backend_failed(&original, "internal_error");
        let read =
            json!({"operation":"skill.missing.get", "session_id":"session-a", "run_id":"run-a"});
        browser.backend_failed(&read, "conflict");
        assert_eq!(browser.retry.as_ref(), Some(&original));
        assert_eq!(browser.retry_service, Some(service_identity(&first_store)));
        assert!(!browser.can_begin_at(&original, &second_store));
        browser.abandon_retry();
        assert!(!browser.mutations_pending());
        assert!(browser.retry_service.is_none());
        assert!(!browser.can_begin_at(&original, &second_store));
        assert!(browser.can_begin_at(&next_mutation, &second_store));
    }
}
