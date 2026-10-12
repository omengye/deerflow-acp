//! Operator reports about a real root task, including tasks without skill reads.
//! Every identifier comes from the service. Rendering uses bounded cached pages.
use super::skills::{PAGE_SIZE, PageNav, digest};
use super::*;
use serde::Deserialize;

fn identity(value: &str) -> bool {
    !value.is_empty() && value.len() <= 256 && !value.contains('\0')
}

fn status_label(status: &str) -> Option<&'static str> {
    Some(match status {
        "pending" => "等待执行",
        "running" => "执行中",
        "suspending" => "暂停中",
        "waiting_input" => "等待输入",
        "resuming" => "恢复中",
        "completed" => "已完成",
        "failed" => "失败",
        "cancelled" => "已取消",
        "needs_reconciliation" => "待核对工具结果",
        _ => return None,
    })
}

#[derive(Clone, Debug, Deserialize)]
struct TaskTarget {
    root_id: String,
    root_session_id: String,
    root_run_id: String,
    created_at: String,
    status: String,
    report_revision: u64,
}

impl TaskTarget {
    fn validate(&self, session_id: &str) -> anyhow::Result<()> {
        anyhow::ensure!(
            identity(&self.root_id)
                && identity(&self.root_run_id)
                && identity(&self.root_session_id)
                && self.root_session_id == session_id
                && self.report_revision <= 1000
                && chrono::DateTime::parse_from_rfc3339(&self.created_at).is_ok()
                && status_label(&self.status).is_some(),
            "缺失能力报告的任务身份、状态或时间无效"
        );
        Ok(())
    }

    fn same_identity(&self, other: &Self) -> bool {
        self.root_id == other.root_id
            && self.root_session_id == other.root_session_id
            && self.root_run_id == other.root_run_id
            && self.created_at == other.created_at
    }

    fn matches(&self, request: &Value) -> bool {
        request["session_id"] == self.root_session_id && request["run_id"] == self.root_run_id
    }

    fn request(&self, operation: &str) -> Value {
        json!({"operation":operation, "session_id":self.root_session_id, "run_id":self.root_run_id})
    }
}

#[derive(Clone, Debug, Deserialize)]
struct MissingReport {
    id: String,
    root_id: String,
    root_session_id: String,
    run_id: String,
    revision: u64,
    origin: String,
    kind: String,
    summary: String,
    summary_redacted: bool,
    attribution: String,
    created_at: String,
}

impl MissingReport {
    fn validate(&self, target: &TaskTarget) -> anyhow::Result<()> {
        anyhow::ensure!(
            self.id.strip_prefix("missing_").is_some_and(digest)
                && self.root_id == target.root_id
                && self.root_session_id == target.root_session_id
                // An operator using the SDK may report a descendant. Its real
                // membership is checked by the service, not inferred by Waku.
                && identity(&self.run_id)
                && (1..=1000).contains(&self.revision)
                && self.origin == "operator"
                && self.attribution == "unassessed"
                && self.summary.len() <= 2000
                && !self.summary.contains('\0')
                && match self.kind.as_str() {
                    "missing_skill" => !self.summary.trim().is_empty() && self.revision < 1000,
                    "withdrawn" => self.summary.is_empty(),
                    _ => false,
                }
                && chrono::DateTime::parse_from_rfc3339(&self.created_at).is_ok(),
            "缺失能力报告的身份、修订或内容无效"
        );
        Ok(())
    }
}

#[derive(Clone)]
struct PendingSubmit {
    target: TaskTarget,
    request: Value,
}

#[derive(Default)]
pub(super) struct MissingBrowser {
    session_id: Option<String>,
    targets: Vec<TaskTarget>,
    targets_nav: PageNav,
    targets_loaded: bool,
    targets_stale: bool,
    selected: Option<TaskTarget>,
    current: Option<MissingReport>,
    snapshot_ready: bool,
    history: Vec<MissingReport>,
    history_nav: PageNav,
    history_loaded: bool,
    history_revision: u64,
    summary: String,
    selection_generation: u64,
    requested_generation: Option<u64>,
    // Receipt identity survives service/session changes. Retrying the exact
    // operation must never attach its receipt to a subsequently selected task.
    pending_submit: Option<PendingSubmit>,
    last_result: Option<String>,
}

impl MissingBrowser {
    fn revoke_snapshot(&mut self) {
        self.current = None;
        self.snapshot_ready = false;
        self.history.clear();
        self.history_nav = PageNav::default();
        self.history_loaded = false;
        self.history_revision = 0;
        self.requested_generation = None;
    }

    pub(super) fn invalidate(&mut self) {
        self.targets_stale = true;
        self.targets.clear();
        self.targets_loaded = false;
        self.targets_nav = PageNav::default();
        self.selected = None;
        self.revoke_snapshot();
        self.selection_generation += 1;
    }

    pub(super) fn set_session(&mut self, session_id: Option<&str>) {
        let session_id = session_id.filter(|id| identity(id));
        if self.session_id.as_deref() != session_id {
            self.invalidate();
            self.session_id = session_id.map(str::to_owned);
        }
    }

    fn choose(&mut self, target: &TaskTarget) -> bool {
        if self.targets_stale
            || self.session_id.as_deref() != Some(&target.root_session_id)
            || !self.targets.iter().any(|row| {
                row.same_identity(target) && row.report_revision == target.report_revision
            })
        {
            return false;
        }
        self.revoke_snapshot();
        self.selected = Some(target.clone());
        self.selection_generation += 1;
        true
    }

    pub(super) fn begin(&mut self, request: &Value) {
        if matches!(
            request["operation"].as_str(),
            Some("skill.missing.get" | "skill.missing.history" | "skill.missing.targets")
        ) {
            self.requested_generation = Some(self.selection_generation);
        }
        if request["operation"] == "skill.missing.submit" && self.pending_submit.is_none() {
            if let Some(target) = self
                .selected
                .as_ref()
                .filter(|target| target.matches(request))
            {
                self.pending_submit = Some(PendingSubmit {
                    target: target.clone(),
                    request: request.clone(),
                });
            }
        }
    }

    pub(super) fn can_begin(&self, request: &Value) -> bool {
        if request["operation"] != "skill.missing.submit" {
            return true;
        }
        if let Some(pending) = &self.pending_submit {
            return pending.request == *request;
        }
        let Some(key) = request["request_key"].as_str() else {
            return false;
        };
        let withdrawn = request["kind"] == "withdrawn";
        self.submit_request(withdrawn, key).as_ref() == Some(request)
    }

    pub(super) fn definitive_failure(&mut self, request: &Value) {
        if self
            .pending_submit
            .as_ref()
            .is_some_and(|pending| pending.request == *request)
        {
            self.pending_submit = None;
        }
    }

    fn can_submit(&self, withdrawn: bool) -> bool {
        let Some(target) = &self.selected else {
            return false;
        };
        if !self.snapshot_ready || self.session_id.as_deref() != Some(&target.root_session_id) {
            return false;
        }
        if withdrawn {
            target.report_revision < 1000
                && self
                    .current
                    .as_ref()
                    .is_some_and(|report| report.kind != "withdrawn")
        } else {
            target.report_revision < 999
                && !self.summary.trim().is_empty()
                && self.summary.len() <= 2000
                && !self.summary.contains('\0')
        }
    }

    fn submit_request(&self, withdrawn: bool, request_key: &str) -> Option<Value> {
        if !self.can_submit(withdrawn) {
            return None;
        }
        let target = self.selected.as_ref()?;
        Some(json!({"operation":"skill.missing.submit",
            "session_id":target.root_session_id, "run_id":target.root_run_id,
            "expected_report_version":target.report_revision, "request_key":request_key,
            "kind":if withdrawn { "withdrawn" } else { "missing_skill" },
            "summary":if withdrawn { "" } else { self.summary.trim() }}))
    }

    fn check_selection(&self, request: &Value) -> anyhow::Result<&TaskTarget> {
        let target = self
            .selected
            .as_ref()
            .ok_or_else(|| anyhow::anyhow!("请先选择真实任务"))?;
        anyhow::ensure!(
            target.matches(request)
                && self.session_id.as_deref() == Some(&target.root_session_id)
                && self.requested_generation == Some(self.selection_generation),
            "会话或任务已变化，请重新选择并刷新"
        );
        Ok(target)
    }

    pub(super) fn accept(&mut self, request: &Value, data: Value) -> anyhow::Result<()> {
        match request["operation"].as_str().unwrap_or("") {
            "skill.missing.targets" => {
                #[derive(Deserialize)]
                struct Page {
                    targets: Vec<TaskTarget>,
                    #[serde(default)]
                    next_cursor: String,
                }
                let session = self
                    .session_id
                    .as_deref()
                    .ok_or_else(|| anyhow::anyhow!("请选择 DeerFlow 会话"))?;
                anyhow::ensure!(
                    request["session_id"] == session
                        && self.requested_generation == Some(self.selection_generation),
                    "任务列表所属会话已变化"
                );
                let page: Page = serde_json::from_value(data)?;
                anyhow::ensure!(page.targets.len() <= PAGE_SIZE, "任务列表超过分页限制");
                for target in &page.targets {
                    target.validate(session)?;
                }
                // RFC3339Nano fractions have variable widths, so textual
                // sorting would reverse .1Z and .12Z.
                anyhow::ensure!(
                    page.targets.windows(2).all(|rows| {
                        let left =
                            chrono::DateTime::parse_from_rfc3339(&rows[0].created_at).unwrap();
                        let right =
                            chrono::DateTime::parse_from_rfc3339(&rows[1].created_at).unwrap();
                        left > right || (left == right && rows[0].root_id > rows[1].root_id)
                    }),
                    "任务列表顺序无效"
                );
                let selected_update = if let Some(selected) = &self.selected {
                    if let Some(updated) = page
                        .targets
                        .iter()
                        .find(|row| row.root_id == selected.root_id)
                    {
                        anyhow::ensure!(
                            selected.same_identity(updated)
                                && updated.report_revision >= selected.report_revision,
                            "任务身份或报告修订已变化"
                        );
                        Some(updated.clone())
                    } else {
                        None
                    }
                } else {
                    None
                };
                self.targets_nav
                    .accept(request, &page.next_cursor, "cursor")?;
                if let Some(updated) = selected_update {
                    if self
                        .selected
                        .as_ref()
                        .is_some_and(|old| old.report_revision != updated.report_revision)
                    {
                        self.revoke_snapshot();
                    }
                    self.selected = Some(updated);
                }
                self.targets = page.targets;
                self.targets_loaded = true;
                self.targets_stale = false;
            }
            "skill.missing.get" => {
                #[derive(Deserialize)]
                struct Snapshot {
                    root_id: String,
                    root_session_id: String,
                    report: Option<MissingReport>,
                }
                let snapshot: Snapshot = serde_json::from_value(data)?;
                let target = self.check_selection(request)?;
                anyhow::ensure!(
                    snapshot.root_id == target.root_id
                        && snapshot.root_session_id == target.root_session_id,
                    "当前报告不属于所选任务"
                );
                let revision = if let Some(report) = &snapshot.report {
                    report.validate(target)?;
                    report.revision
                } else {
                    0
                };
                anyhow::ensure!(
                    revision >= target.report_revision,
                    "报告修订已变化，请刷新任务列表"
                );
                if revision != target.report_revision {
                    self.history.clear();
                    self.history_nav = PageNav::default();
                    self.history_loaded = false;
                }
                self.selected.as_mut().unwrap().report_revision = revision;
                self.current = snapshot.report;
                self.snapshot_ready = true;
            }
            "skill.missing.history" => {
                #[derive(Deserialize)]
                struct Page {
                    revisions: Vec<MissingReport>,
                    current_revision: u64,
                    #[serde(default)]
                    next_cursor: String,
                }
                let page: Page = serde_json::from_value(data)?;
                let target = self.check_selection(request)?;
                anyhow::ensure!(
                    page.revisions.len() <= PAGE_SIZE
                        && page.current_revision <= 1000
                        && page.current_revision >= target.report_revision,
                    "报告历史修订无效"
                );
                for report in &page.revisions {
                    report.validate(target)?;
                    anyhow::ensure!(
                        report.revision <= page.current_revision,
                        "报告修订超出当前版本"
                    );
                }
                anyhow::ensure!(
                    page.revisions
                        .windows(2)
                        .all(|rows| rows[0].revision < rows[1].revision),
                    "报告历史顺序无效"
                );
                let changed = page.current_revision != target.report_revision;
                self.history_nav
                    .accept(request, &page.next_cursor, "cursor")?;
                if changed {
                    self.snapshot_ready = false;
                    self.selected.as_mut().unwrap().report_revision = page.current_revision;
                }
                self.history = page.revisions;
                self.history_revision = page.current_revision;
                self.history_loaded = true;
            }
            "skill.missing.submit" => {
                #[derive(Deserialize)]
                struct Receipt {
                    report: MissingReport,
                    current_revision: u64,
                    replayed: bool,
                }
                let receipt: Receipt = serde_json::from_value(data)?;
                let pending = self
                    .pending_submit
                    .as_ref()
                    .ok_or_else(|| anyhow::anyhow!("报告回执缺少原请求身份"))?;
                receipt.report.validate(&pending.target)?;
                anyhow::ensure!(
                    pending.request == *request
                        && pending.target.matches(request)
                        && request["run_id"] == receipt.report.run_id
                        && request["expected_report_version"]
                            .as_u64()
                            .and_then(|value| value.checked_add(1))
                            == Some(receipt.report.revision)
                        && request["kind"] == receipt.report.kind
                        && (receipt.report.summary_redacted
                            || request["summary"] == receipt.report.summary)
                        && receipt.current_revision >= receipt.report.revision
                        && receipt.current_revision <= 1000
                        && (receipt.replayed
                            || receipt.current_revision == receipt.report.revision),
                    "报告回执与原请求不一致"
                );
                self.last_result = Some(format!(
                    "{}修订 {} 已确认；服务当前修订 {}。\n会话 {} · 任务 {} · 运行 {}。刷新任务及当前报告后继续。",
                    if receipt.replayed {
                        "原请求重放："
                    } else {
                        "报告"
                    },
                    receipt.report.revision,
                    receipt.current_revision,
                    receipt.report.root_session_id,
                    receipt.report.root_id,
                    receipt.report.run_id
                ));
                self.pending_submit = None;
                // A replay can return an older revision. It is a receipt, never
                // the current report, even if the same task is still selected.
                self.invalidate();
            }
            _ => anyhow::bail!("无法识别缺失能力报告操作"),
        }
        Ok(())
    }
}

impl DeerFlowSettings {
    pub(super) fn missing_editable(&self) -> bool {
        !self.busy()
            && !self.applying()
            && !self.dirty
            && !self.needs_apply
            && self.status["running"] == true
            && !self.skill_browser.mutations_pending()
            && self.skills_session_id.as_deref() == self.skill_browser.missing.session_id.as_deref()
            && self.skill_browser.missing.selected.is_some()
    }
}

impl Waku {
    pub(in crate::app) fn deerflow_missing_edited(&mut self, cx: &mut Context<Self>) {
        let value = self
            .deerflow_settings
            .missing_input
            .read(cx)
            .content()
            .to_owned();
        let browser = &mut self.deerflow_settings.skill_browser.missing;
        if browser.summary != value {
            browser.summary = value;
            self.deerflow_settings.confirm_manage = None;
            cx.notify();
        }
    }

    pub(super) fn render_deerflow_missing(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let browser = &state.skill_browser.missing;
        let unavailable = state.busy() || state.applying() || state.status["running"] != true;
        let editable = state.missing_editable();
        let mut group = df_group(theme).child(df_label("报告缺失的技能能力",
            "针对真实任务记录缺少的可复用能力，无需该任务读取过技能。报告尚未评估归因，提交后不会自动生成或发布技能。", theme));
        if let Some(result) = &browser.last_result {
            group = group.child(df_label("提交回执", result.clone(), theme));
        }
        group = group.child(df_button("df-missing-targets-refresh", "刷新当前聊天的任务", unavailable, false, theme, cx,
            |this, _, cx| {
                this.deerflow_sync_missing_session(cx);
                if let Some(session_id) = this.deerflow_settings.skills_session_id.clone() {
                    this.deerflow_request("manage", json!({"operation":"skill.missing.targets", "session_id":session_id, "limit":PAGE_SIZE}), cx);
                }
            }));
        let Some(session_id) = state
            .skills_session_id
            .as_ref()
            .filter(|session| browser.session_id.as_ref() == Some(*session))
        else {
            return group
                .child(df_label(
                    "尚未选定 DeerFlow 会话",
                    "打开一个已建立 DeerFlow 原生会话的聊天，再刷新任务。草稿会保留。",
                    theme,
                ))
                .into_any_element();
        };
        group = group.child(df_label(
            format!("报告所属聊天：{}", state.skills_session_title),
            format!("原生会话 {session_id}"),
            theme,
        ));
        if browser.targets_stale {
            group = group.child(df_label(
                "任务列表需要刷新",
                "会话或服务已变化，已撤销原任务的提交条件；说明草稿保留。",
                theme,
            ));
        }
        if browser.targets_loaded {
            if browser.targets.is_empty() {
                group = group.child(df_label(
                    "暂无可报告任务",
                    "此会话尚无已被服务接受的任务；执行任务后刷新。",
                    theme,
                ));
            }
            for target in &browser.targets {
                let target = target.clone();
                group = group.child(df_button(
                    SharedString::from(format!("df-missing-target-{}", target.root_id)),
                    format!(
                        "{} · {} · {}",
                        target.created_at,
                        status_label(&target.status).unwrap_or("未知"),
                        target.root_id
                    ),
                    unavailable || browser.targets_stale,
                    browser
                        .selected
                        .as_ref()
                        .is_some_and(|selected| selected.same_identity(&target)),
                    theme,
                    cx,
                    move |this, _, cx| {
                        if this.deerflow_settings.busy()
                            || !this.deerflow_settings.skill_browser.missing.choose(&target)
                        {
                            return;
                        }
                        this.deerflow_settings.confirm_manage = None;
                        this.deerflow_request("manage", target.request("skill.missing.get"), cx);
                    },
                ));
            }
            group = group.child(self.render_skill_page_nav(
                "missing-targets",
                &browser.targets_nav,
                unavailable || browser.targets_stale,
                theme,
                cx,
            ));
            group = group.child(df_label(
                "按任务创建时间显示",
                "状态指原始根任务，不代表全部子任务结果。新任务请从第一页刷新。",
                theme,
            ));
        }
        let Some(target) = &browser.selected else {
            return group.into_any_element();
        };
        group = group.child(df_label(
            "当前报告对应的任务",
            format!(
                "{} · {}\n任务 {} · 运行 {}\n已观察报告修订 {} · 尚未评估归因",
                target.created_at,
                status_label(&target.status).unwrap_or("未知"),
                target.root_id,
                target.root_run_id,
                target.report_revision
            ),
            theme,
        ));
        let mut actions = div().flex().flex_wrap().gap(px(6.0));
        for (suffix, label, operation) in [
            ("current", "刷新当前报告", "skill.missing.get"),
            ("history", "刷新报告历史", "skill.missing.history"),
        ] {
            let mut request = target.request(operation);
            if operation == "skill.missing.history" {
                request["limit"] = json!(PAGE_SIZE);
            }
            actions = actions.child(df_button(
                SharedString::from(format!("df-missing-{suffix}")),
                label,
                unavailable,
                false,
                theme,
                cx,
                move |this, _, cx| this.deerflow_request("manage", request.clone(), cx),
            ));
        }
        group = group.child(actions);
        if browser.snapshot_ready {
            if let Some(report) = &browser.current {
                group = group.child(render_report(report, "当前报告", theme));
            } else {
                group = group.child(df_label(
                    "此任务尚无报告",
                    "提交后记录第一版缺失能力说明。",
                    theme,
                ));
            }
        } else {
            group = group.child(df_label(
                "提交前刷新当前报告",
                "核对服务上的当前修订后再提交；说明草稿已保留。",
                theme,
            ));
        }
        group = group
            .child(df_label(
                "缺失能力说明",
                "最多 2000 UTF-8 字节。描述任务中缺少的可复用方法、知识或工具用法。",
                theme,
            ))
            .child(
                div()
                    .id("df-missing-summary")
                    .h(px(100.0))
                    .w_full()
                    .overflow_hidden()
                    .overflow_y_scroll()
                    .p(px(10.0))
                    .rounded(px(6.0))
                    .border_1()
                    .border_color(theme.border_strong)
                    .bg(theme.inset)
                    .child(state.missing_input.clone()),
            );
        for (withdrawn, label) in [(false, "提交缺失能力报告"), (true, "撤回当前报告")]
        {
            group = group.child(df_button(
                SharedString::from(format!("df-missing-submit-{withdrawn}")),
                label,
                !editable || !browser.can_submit(withdrawn),
                false,
                theme,
                cx,
                move |this, _, cx| {
                    if this.deerflow_settings.missing_editable() {
                        if let Some(request) = this
                            .deerflow_settings
                            .skill_browser
                            .missing
                            .submit_request(withdrawn, &Uuid::new_v4().to_string())
                        {
                            this.deerflow_settings.confirm_manage = None;
                            this.deerflow_request("manage", request, cx);
                        }
                    }
                },
            ));
        }
        if browser.summary.len() > 2000 {
            group = group.child(df_label(
                "说明过长",
                format!(
                    "当前 {} UTF-8 字节，上限 2000。草稿已保留。",
                    browser.summary.len()
                ),
                theme,
            ));
        }
        if target.report_revision >= 999 {
            group = group.child(df_label(
                "已达到报告修订上限",
                "最后一条修订仅用于撤回；全部历史仍可查看。",
                theme,
            ));
        }
        if browser.history_loaded {
            group = group.child(df_label(
                "报告历史",
                format!(
                    "查询时当前修订 {}；同一任务的修订替换原判断。",
                    browser.history_revision
                ),
                theme,
            ));
            if browser.history.is_empty() {
                group = group.child(df_label("暂无报告历史", "此任务还没有提交过报告。", theme));
            }
            for report in &browser.history {
                group = group.child(render_report(report, "历史报告", theme));
            }
            group = group.child(self.render_skill_page_nav(
                "missing-history",
                &browser.history_nav,
                unavailable,
                theme,
                cx,
            ));
        }
        group.into_any_element()
    }
}

fn render_report(report: &MissingReport, title: &str, theme: Theme) -> AnyElement {
    df_label(
        format!(
            "{title} · 修订 {} · {}",
            report.revision,
            if report.kind == "withdrawn" {
                "已撤回"
            } else {
                "能力缺失"
            }
        ),
        format!(
            "{}\n{} · 操作者报告 · 尚未评估归因{}",
            report.summary,
            report.created_at,
            if report.summary_redacted {
                " · 敏感内容已脱敏"
            } else {
                ""
            }
        ),
        theme,
    )
    .into_any_element()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn target(root: &str, created: &str) -> Value {
        json!({"root_id":root, "root_session_id":"session-one", "root_run_id":format!("run-{root}"),
            "created_at":created, "status":"completed", "report_revision":0})
    }

    fn discover(browser: &mut MissingBrowser) {
        browser.set_session(Some("session-one"));
        let request = json!({"operation":"skill.missing.targets", "session_id":"session-one", "limit":PAGE_SIZE});
        browser.begin(&request);
        browser
            .accept(
                &request,
                json!({"targets":[target("one", "2026-10-02T12:00:00.12Z")]}),
            )
            .unwrap();
        assert!(browser.choose(&browser.targets[0].clone()));
    }

    fn refresh(browser: &mut MissingBrowser, report: Option<Value>) {
        let request = browser
            .selected
            .as_ref()
            .unwrap()
            .request("skill.missing.get");
        let selected = browser.selected.as_ref().unwrap();
        let data = json!({"root_id":selected.root_id, "root_session_id":selected.root_session_id, "report":report});
        browser.begin(&request);
        browser.accept(&request, data).unwrap();
    }

    fn ready() -> MissingBrowser {
        let mut browser = MissingBrowser::default();
        discover(&mut browser);
        browser.summary = "缺少可复用的发布验证流程".into();
        assert!(!browser.can_submit(false));
        refresh(&mut browser, None);
        browser
    }

    fn report(revision: u64, kind: &str, summary: &str) -> Value {
        json!({"id":format!("missing_{}", "a".repeat(64)), "root_id":"one", "root_session_id":"session-one", "run_id":"run-one",
            "revision":revision, "origin":"operator", "kind":kind, "summary":summary, "summary_redacted":false,
            "attribution":"unassessed", "created_at":"2026-10-02T12:00:01Z"})
    }

    #[test]
    fn deerflow_missing_zero_read_task_uses_discovered_identity_and_current_revision() {
        let browser = ready();
        let submit = browser.submit_request(false, "stable-key").unwrap();
        assert_eq!(
            submit,
            json!({"operation":"skill.missing.submit", "session_id":"session-one", "run_id":"run-one",
            "expected_report_version":0, "request_key":"stable-key", "kind":"missing_skill", "summary":"缺少可复用的发布验证流程"})
        );
        assert!(!browser.can_submit(true));
    }

    #[test]
    fn deerflow_missing_invalidation_preserves_draft_and_rejects_old_session_response() {
        let mut browser = ready();
        let original = browser.submit_request(false, "original-key").unwrap();
        browser.begin(&original);
        let get = browser
            .selected
            .as_ref()
            .unwrap()
            .request("skill.missing.get");
        browser.begin(&get);
        browser.set_session(Some("session-two"));
        assert!(!browser.can_submit(false));
        assert!(browser.can_begin(&original));
        assert!(!browser.targets_loaded);
        assert_eq!(browser.summary, "缺少可复用的发布验证流程");
        assert!(browser.pending_submit.is_some());
        assert!(
            browser
                .accept(
                    &get,
                    json!({"root_id":"one", "root_session_id":"session-one"})
                )
                .is_err()
        );
        let replay = json!({"report":report(1, "missing_skill", original["summary"].as_str().unwrap()), "current_revision":3, "replayed":true});
        browser.accept(&original, replay).unwrap();
        assert!(browser.current.is_none());
        assert!(!browser.snapshot_ready);
        assert!(browser.last_result.as_ref().unwrap().contains("原请求重放"));
        assert_eq!(browser.session_id.as_deref(), Some("session-two"));
        assert!(browser.pending_submit.is_none());
        assert!(!browser.can_begin(&original));
    }

    #[test]
    fn deerflow_missing_stale_confirmation_cannot_start_after_session_or_draft_changes() {
        let mut browser = ready();
        let original = browser.submit_request(false, "original-key").unwrap();
        assert!(browser.can_begin(&original));
        browser.summary = "新的草稿".into();
        assert!(!browser.can_begin(&original));
        browser.summary = original["summary"].as_str().unwrap().into();
        browser.set_session(Some("session-two"));
        assert!(!browser.can_begin(&original));
        assert!(browser.pending_submit.is_none());
        assert_eq!(browser.summary, original["summary"].as_str().unwrap());
    }

    #[test]
    fn deerflow_missing_receipt_requires_immutable_original_request_and_run() {
        let mut browser = ready();
        let original = browser.submit_request(false, "stable-key").unwrap();
        browser.begin(&original);
        let mut wrong = original.clone();
        wrong["request_key"] = json!("replacement-key");
        let receipt = json!({"report":report(1, "missing_skill", original["summary"].as_str().unwrap()), "current_revision":1, "replayed":false});
        assert!(browser.accept(&wrong, receipt.clone()).is_err());
        let mut wrong_run = receipt.clone();
        wrong_run["report"]["run_id"] = json!("child-run");
        assert!(browser.accept(&original, wrong_run).is_err());
        assert!(browser.pending_submit.is_some());
        browser.definitive_failure(&wrong);
        assert!(browser.pending_submit.is_some());
        browser.accept(&original, receipt).unwrap();
        assert!(browser.pending_submit.is_none());
    }

    #[test]
    fn deerflow_missing_targets_parse_fractional_time_and_reject_cross_session_or_unbounded_pages()
    {
        let mut browser = MissingBrowser::default();
        browser.set_session(Some("session-one"));
        let request = json!({"operation":"skill.missing.targets", "session_id":"session-one", "limit":PAGE_SIZE});
        browser.begin(&request);
        let newest = target("z", "2026-10-02T12:00:00.12Z");
        let earlier = target("a", "2026-10-02T12:00:00.1Z");
        browser
            .accept(
                &request,
                json!({"targets":[newest.clone(), earlier.clone()], "next_cursor":"next"}),
            )
            .unwrap();
        assert!(
            browser
                .accept(&request, json!({"targets":[earlier, newest.clone()]}))
                .is_err()
        );
        let mut wrong = newest.clone();
        wrong["root_session_id"] = json!("private-background-session");
        assert!(
            browser
                .accept(&request, json!({"targets":[wrong]}))
                .is_err()
        );
        assert!(
            browser
                .accept(&request, json!({"targets":vec![newest;PAGE_SIZE+1]}))
                .is_err()
        );
        let mut next = request.clone();
        next["cursor"] = json!("next");
        browser.begin(&next);
        browser
            .accept(
                &next,
                json!({"targets":[target("older", "2026-10-01T12:00:00Z")]}),
            )
            .unwrap();
        assert_eq!(browser.targets.len(), 1);
        let invented: TaskTarget =
            serde_json::from_value(target("invented", "2026-10-01T12:00:00Z")).unwrap();
        assert!(!browser.choose(&invented));
    }

    #[test]
    fn deerflow_missing_descendant_report_and_history_advance_cas_without_inventing_reads() {
        let mut browser = ready();
        let mut child = report(1, "missing_skill", "外部操作者对子任务的报告");
        child["run_id"] = json!("child-run");
        refresh(&mut browser, Some(child.clone()));
        assert_eq!(
            browser.submit_request(false, "next-key").unwrap()["expected_report_version"],
            1
        );
        let get = browser
            .selected
            .as_ref()
            .unwrap()
            .request("skill.missing.get");
        browser.begin(&get);
        assert!(
            browser
                .accept(
                    &get,
                    json!({"root_id":"one", "root_session_id":"session-one"})
                )
                .is_err()
        );
        let mut history = browser
            .selected
            .as_ref()
            .unwrap()
            .request("skill.missing.history");
        history["limit"] = json!(PAGE_SIZE);
        browser.begin(&history);
        browser
            .accept(
                &history,
                json!({"revisions":[child], "current_revision":2, "next_cursor":"history-next"}),
            )
            .unwrap();
        assert!(!browser.snapshot_ready);
        assert!(!browser.can_submit(false));
        let mut next = history.clone();
        next["cursor"] = json!("history-next");
        browser.begin(&next);
        browser
            .accept(
                &next,
                json!({"revisions":[report(2, "missing_skill", "当前报告")], "current_revision":2}),
            )
            .unwrap();
        assert_eq!(browser.history.len(), 1);
        assert_eq!(browser.history[0].revision, 2);
        refresh(&mut browser, Some(report(2, "missing_skill", "当前报告")));
        assert_eq!(
            browser.submit_request(false, "next-key").unwrap()["expected_report_version"],
            2
        );
        browser.begin(&history);
        let invalid = json!({"revisions":[report(2,"missing_skill","second"),report(1,"missing_skill","first")], "current_revision":2});
        assert!(browser.accept(&history, invalid).is_err());
    }

    #[test]
    fn deerflow_missing_utf8_bounds_and_reserved_withdrawal_revision() {
        let mut browser = ready();
        browser.summary = "中".repeat(667);
        assert!(!browser.can_submit(false));
        browser.summary = "中".repeat(666);
        assert!(browser.can_submit(false));
        refresh(&mut browser, Some(report(999, "missing_skill", "最终报告")));
        assert!(!browser.can_submit(false));
        let withdraw = browser.submit_request(true, "withdraw-key").unwrap();
        assert_eq!(withdraw["kind"], "withdrawn");
        assert_eq!(withdraw["summary"], "");
        assert_eq!(withdraw["expected_report_version"], 999);
        refresh(&mut browser, Some(report(1000, "withdrawn", "")));
        assert!(!browser.can_submit(true));
        assert!(!browser.can_submit(false));
    }
}
