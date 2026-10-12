//! Operator judgments refer to observed reads. Cached pages never infer skill
//! quality from run outcomes and never authorize generation or publication.
use super::skills::{PAGE_SIZE, PageNav, Target, digest};
use super::*;
use serde::Deserialize;

#[derive(Clone, Debug, Deserialize)]
struct Signal {
    id: String,
    target: Target,
    version: String,
    root_id: String,
    root_session_id: String,
    state_version: u64,
    feedback_revision: u64,
    attribution: String,
    read_count: u64,
    run_count: u64,
    role_count: u64,
    completed_runs: u64,
    failed_runs: u64,
    cancelled_runs: u64,
    uncertain_runs: u64,
    collection_pending: bool,
    #[serde(default)]
    quality: Option<Quality>,
}

#[derive(Clone, Debug, Deserialize)]
struct Quality {
    kind: String,
    confidence: String,
    source: String,
    eligible: bool,
    #[serde(default)]
    feedback_revision: u64,
    #[serde(default)]
    evidence_read_ids: Vec<u64>,
}

fn bounded(value: &str, limit: usize) -> bool {
    !value.is_empty() && value.len() <= limit && !value.contains('\0')
}

impl Signal {
    fn validate(&self) -> anyhow::Result<()> {
        anyhow::ensure!(
            self.id.strip_prefix("sig_").is_some_and(digest)
                && bounded(&self.target.source_id, 64)
                && bounded(&self.target.name, 64)
                && digest(&self.target.source_identity)
                && digest(&self.version)
                && bounded(&self.root_id, 256)
                && bounded(&self.root_session_id, 256)
                && self.state_version > 0
                && self.state_version <= i64::MAX as u64
                && self.feedback_revision <= 1000
                && self.attribution == "unassessed"
                && matches!(self.target.scope.as_str(), "global" | "workspace")
                && if self.target.scope == "global" {
                    self.target.workspace.is_empty()
                } else {
                    bounded(&self.target.workspace, 32768)
                },
            "技能信号身份或版本无效"
        );
        anyhow::ensure!(
            self.read_count <= i64::MAX as u64
                && self.run_count <= self.read_count
                && self.role_count <= self.read_count
                && self.completed_runs <= self.run_count
                && self.failed_runs <= self.run_count
                && self.cancelled_runs <= self.run_count
                && self.uncertain_runs <= self.run_count,
            "技能信号计数无效"
        );
        if let Some(quality) = &self.quality {
            anyhow::ensure!(
                bounded(&quality.kind, 64)
                    && bounded(&quality.confidence, 32)
                    && bounded(&quality.source, 64)
                    && quality.feedback_revision <= self.feedback_revision
                    && quality.evidence_read_ids.len() <= 16
                    && quality
                        .evidence_read_ids
                        .iter()
                        .all(|id| *id > 0 && *id <= i64::MAX as u64)
                    && quality
                        .evidence_read_ids
                        .windows(2)
                        .all(|pair| pair[0] < pair[1]),
                "技能质量归因格式无效"
            );
            anyhow::ensure!(
                !quality.eligible
                    || (matches!(
                        quality.kind.as_str(),
                        "skill_quality_success" | "skill_quality_failure"
                    ) && quality.confidence == "high"
                        && quality.source == "operator_confirmed"
                        && quality.feedback_revision == self.feedback_revision
                        && quality.feedback_revision > 0
                        && !quality.evidence_read_ids.is_empty()),
                "技能质量归因缺少操作者判断或证据"
            );
        }
        Ok(())
    }

    fn same_identity(&self, other: &Self) -> bool {
        self.id == other.id
            && self.target == other.target
            && self.version == other.version
            && self.root_id == other.root_id
            && self.root_session_id == other.root_session_id
    }
}

#[derive(Clone, Deserialize)]
struct Read {
    id: u64,
    run_id: String,
    session_id: String,
    attempt_id: String,
    #[serde(default)]
    role_id: String,
    kind: String,
    path: String,
    hash: String,
    bytes: u64,
    observed_at: String,
    outcome: String,
    effects_uncertain: bool,
}

impl Read {
    fn validate(&self) -> anyhow::Result<()> {
        anyhow::ensure!(
            self.id > 0
                && self.id <= i64::MAX as u64
                && bounded(&self.run_id, 256)
                && bounded(&self.session_id, 256)
                && bounded(&self.attempt_id, 256)
                && self.role_id.len() <= 256
                && bounded(&self.kind, 64)
                && bounded(&self.path, 32768)
                && digest(&self.hash)
                && self.bytes <= i64::MAX as u64
                && chrono::DateTime::parse_from_rfc3339(&self.observed_at).is_ok()
                && matches!(self.outcome.as_str(), "completed" | "failed" | "cancelled"),
            "技能读取证据无效"
        );
        Ok(())
    }
}

#[derive(Clone, Deserialize)]
struct Feedback {
    signal_id: String,
    revision: u64,
    target: Target,
    skill_version: String,
    root_id: String,
    root_session_id: String,
    signal_state_version: u64,
    origin: String,
    kind: String,
    summary: String,
    summary_redacted: bool,
    read_ids: Vec<u64>,
    created_at: String,
}

const KINDS: [(&str, &str); 8] = [
    ("helpful", "有帮助"),
    ("correction", "需要改进"),
    ("quality_failure", "确认技能质量导致失败"),
    ("network", "外部：网络故障"),
    ("quota", "外部：额度不足"),
    ("permission", "外部：权限问题"),
    ("cancelled", "任务被取消"),
    ("other", "其他"),
];

fn kind_label(kind: &str) -> &str {
    if kind == "withdrawn" {
        "已撤回"
    } else {
        KINDS
            .iter()
            .find(|(key, _)| *key == kind)
            .map_or("未知", |(_, label)| *label)
    }
}

impl Feedback {
    fn validate(&self, signal: &Signal) -> anyhow::Result<()> {
        let withdrawn = self.kind == "withdrawn";
        anyhow::ensure!(
            self.signal_id == signal.id
                && self.target == signal.target
                && self.skill_version == signal.version
                && self.root_id == signal.root_id
                && self.root_session_id == signal.root_session_id
                && self.revision > 0
                && self.revision <= 1000
                && self.signal_state_version > 0
                && self.signal_state_version <= i64::MAX as u64
                && self.origin == "operator"
                && (withdrawn || KINDS.iter().any(|(kind, _)| *kind == self.kind))
                && self.summary.len() <= 2000
                && !self.summary.contains('\0')
                && if withdrawn {
                    self.summary.is_empty() && self.read_ids.is_empty()
                } else {
                    !self.summary.trim().is_empty()
                        && !self.read_ids.is_empty()
                        && self.read_ids.len() <= 16
                },
            "反馈身份或内容无效"
        );
        anyhow::ensure!(
            self.read_ids
                .iter()
                .all(|id| *id > 0 && *id <= i64::MAX as u64)
                && self.read_ids.windows(2).all(|pair| pair[0] < pair[1])
                && chrono::DateTime::parse_from_rfc3339(&self.created_at).is_ok(),
            "反馈读取引用或时间无效"
        );
        Ok(())
    }
}

#[derive(Default)]
pub(super) struct FeedbackBrowser {
    signals: Vec<Signal>,
    signals_loaded: bool,
    signals_stale: bool,
    signals_nav: PageNav,
    selected: Option<Signal>,
    reads: Vec<Read>,
    reads_nav: PageNav,
    evidence_ready: bool,
    selected_reads: Vec<u64>,
    current: Option<Feedback>,
    snapshot_ready: bool,
    evidence_current: bool,
    history: Vec<Feedback>,
    history_nav: PageNav,
    history_loaded: bool,
    history_revision: u64,
    summary: String,
    kind: String,
    edit_generation: u64,
    requested_generation: Option<u64>,
    // Survives invalidation: the exact original operation may need replay even
    // after the operator has queried another signal.
    pending_submit: Option<Signal>,
    last_result: Option<String>,
}

impl FeedbackBrowser {
    fn revoke_evidence(&mut self) {
        self.reads.clear();
        self.reads_nav = PageNav::default();
        self.evidence_ready = false;
        self.selected_reads.clear();
        self.current = None;
        self.snapshot_ready = false;
        self.evidence_current = false;
        self.history.clear();
        self.history_nav = PageNav::default();
        self.history_loaded = false;
    }

    pub(super) fn invalidate(&mut self) {
        self.signals_stale = true;
        self.revoke_evidence();
        self.requested_generation = None;
        self.edit_generation += 1;
    }

    fn choose(&mut self, signal: Signal) {
        self.revoke_evidence();
        self.selected = Some(signal);
        self.edit_generation += 1;
        self.requested_generation = None;
    }

    pub(super) fn begin(&mut self, request: &Value) {
        if matches!(
            request["operation"].as_str(),
            Some("skill.signals.get" | "skill.feedback.get")
        ) {
            self.requested_generation = Some(self.edit_generation);
        }
        if request["operation"] == "skill.feedback.submit" && self.pending_submit.is_none() {
            if let Some(signal) = &self.selected {
                if request["signal_id"] == signal.id {
                    self.pending_submit = Some(signal.clone());
                }
            }
        }
    }

    pub(super) fn definitive_failure(&mut self, request: &Value) {
        if request["operation"] == "skill.feedback.submit" {
            self.pending_submit = None;
        }
    }

    fn observe(&mut self, signal: Signal, request: &Value) -> anyhow::Result<()> {
        signal.validate()?;
        let old = self
            .selected
            .as_ref()
            .ok_or_else(|| anyhow::anyhow!("请先选择技能信号"))?;
        anyhow::ensure!(
            request["signal_id"] == signal.id
                && old.same_identity(&signal)
                && signal.state_version >= old.state_version
                && signal.feedback_revision >= old.feedback_revision,
            "技能信号响应与当前选择不一致"
        );
        if old.state_version != signal.state_version
            || old.feedback_revision != signal.feedback_revision
        {
            self.revoke_evidence();
        }
        self.selected = Some(signal);
        Ok(())
    }

    fn toggle_read(&mut self, id: u64) {
        if !self.evidence_ready || !self.reads.iter().any(|read| read.id == id) {
            return;
        }
        if let Some(index) = self.selected_reads.iter().position(|value| *value == id) {
            self.selected_reads.remove(index);
        } else if self.selected_reads.len() < 16 {
            self.selected_reads.push(id);
            self.selected_reads.sort_unstable();
        }
        self.edit_generation += 1;
    }

    fn can_submit(&self, withdrawn: bool) -> bool {
        if !self.snapshot_ready || self.selected.is_none() {
            return false;
        }
        if withdrawn {
            self.current
                .as_ref()
                .is_some_and(|current| current.kind != "withdrawn")
        } else {
            self.evidence_ready
                && !self.selected_reads.is_empty()
                && self.selected_reads.len() <= 16
                && KINDS.iter().any(|(kind, _)| *kind == self.kind)
                && !self.summary.trim().is_empty()
                && self.summary.len() <= 2000
                && !self.summary.contains('\0')
        }
    }

    fn submit_request(&self, withdrawn: bool, request_key: &str) -> Option<Value> {
        if !self.can_submit(withdrawn) {
            return None;
        }
        let signal = self.selected.as_ref()?;
        let (kind, summary, reads) = if withdrawn {
            ("withdrawn", "", Vec::new())
        } else {
            (
                self.kind.as_str(),
                self.summary.trim(),
                self.selected_reads.clone(),
            )
        };
        Some(
            json!({"operation":"skill.feedback.submit", "signal_id":signal.id,
            "expected_signal_state_version":signal.state_version,
            "expected_feedback_version":signal.feedback_revision,
            "request_key":request_key, "kind":kind, "summary":summary, "read_ids":reads}),
        )
    }

    pub(super) fn accept(&mut self, request: &Value, data: Value) -> anyhow::Result<()> {
        match request["operation"].as_str().unwrap_or("") {
            "skill.signals.list" => {
                #[derive(Deserialize)]
                struct Page {
                    signals: Vec<Signal>,
                    catalog_version: u64,
                    #[serde(default)]
                    next_cursor: String,
                }
                let page: Page = serde_json::from_value(data)?;
                anyhow::ensure!(
                    page.signals.len() <= PAGE_SIZE && page.catalog_version <= i64::MAX as u64,
                    "技能信号列表超过限制"
                );
                for signal in &page.signals {
                    signal.validate()?;
                }
                anyhow::ensure!(
                    page.signals.windows(2).all(|rows| rows[0].id < rows[1].id),
                    "信号列表顺序无效"
                );
                self.signals_nav
                    .accept(request, &page.next_cursor, "cursor")?;
                if let Some(selected) = &self.selected {
                    if let Some(updated) = page.signals.iter().find(|row| row.id == selected.id) {
                        anyhow::ensure!(
                            selected.same_identity(updated)
                                && updated.state_version >= selected.state_version
                                && updated.feedback_revision >= selected.feedback_revision,
                            "技能信号身份或版本已变化"
                        );
                        if updated.state_version != selected.state_version
                            || updated.feedback_revision != selected.feedback_revision
                        {
                            self.revoke_evidence();
                            self.selected = Some(updated.clone());
                        }
                    }
                }
                self.signals = page.signals;
                self.signals_loaded = true;
                self.signals_stale = false;
            }
            "skill.signals.get" => {
                #[derive(Deserialize)]
                struct Page {
                    signal: Signal,
                    reads: Vec<Read>,
                    #[serde(default)]
                    next_cursor: String,
                }
                let page: Page = serde_json::from_value(data)?;
                anyhow::ensure!(
                    page.reads.len() <= PAGE_SIZE
                        && page.reads.len() as u64 <= page.signal.read_count,
                    "读取证据超过分页限制"
                );
                for read in &page.reads {
                    read.validate()?;
                }
                anyhow::ensure!(
                    page.reads.windows(2).all(|rows| rows[0].id < rows[1].id),
                    "读取证据顺序无效"
                );
                self.observe(page.signal, request)?;
                self.reads_nav
                    .accept(request, &page.next_cursor, "cursor")?;
                self.reads = page.reads;
                self.evidence_ready = self.requested_generation == Some(self.edit_generation);
                if !self.evidence_ready {
                    self.selected_reads.clear();
                }
            }
            "skill.feedback.get" => {
                #[derive(Deserialize)]
                struct Snapshot {
                    signal: Signal,
                    feedback: Option<Feedback>,
                    evidence_current: bool,
                }
                let snapshot: Snapshot = serde_json::from_value(data)?;
                if let Some(feedback) = &snapshot.feedback {
                    feedback.validate(&snapshot.signal)?;
                    anyhow::ensure!(
                        feedback.revision == snapshot.signal.feedback_revision
                            && feedback.signal_state_version <= snapshot.signal.state_version
                            && snapshot.evidence_current
                                == (feedback.kind != "withdrawn"
                                    && feedback.signal_state_version
                                        == snapshot.signal.state_version),
                        "当前反馈与证据版本不一致"
                    );
                } else {
                    anyhow::ensure!(
                        snapshot.signal.feedback_revision == 0 && !snapshot.evidence_current,
                        "当前反馈响应缺少修订"
                    );
                }
                self.observe(snapshot.signal, request)?;
                self.current = snapshot.feedback;
                self.evidence_current = snapshot.evidence_current;
                self.snapshot_ready = self.requested_generation == Some(self.edit_generation);
            }
            "skill.feedback.list" => {
                #[derive(Deserialize)]
                struct Page {
                    revisions: Vec<Feedback>,
                    current_revision: u64,
                    #[serde(default)]
                    next_cursor: String,
                }
                let page: Page = serde_json::from_value(data)?;
                let signal = self
                    .selected
                    .as_ref()
                    .ok_or_else(|| anyhow::anyhow!("请先选择信号"))?;
                anyhow::ensure!(
                    request["signal_id"] == signal.id
                        && page.revisions.len() <= PAGE_SIZE
                        && page.current_revision <= 1000,
                    "反馈历史响应无效"
                );
                for revision in &page.revisions {
                    revision.validate(signal)?;
                    anyhow::ensure!(
                        revision.revision <= page.current_revision,
                        "反馈修订超出当前版本"
                    );
                }
                anyhow::ensure!(
                    page.revisions
                        .windows(2)
                        .all(|rows| rows[0].revision < rows[1].revision),
                    "反馈修订顺序无效"
                );
                self.history_nav
                    .accept(request, &page.next_cursor, "cursor")?;
                if page.current_revision != signal.feedback_revision {
                    self.snapshot_ready = false;
                }
                self.history = page.revisions;
                self.history_revision = page.current_revision;
                self.history_loaded = true;
            }
            "skill.feedback.submit" => {
                #[derive(Deserialize)]
                struct Result {
                    feedback: Feedback,
                    current_revision: u64,
                    replayed: bool,
                }
                let result: Result = serde_json::from_value(data)?;
                let signal = self
                    .pending_submit
                    .as_ref()
                    .ok_or_else(|| anyhow::anyhow!("反馈响应缺少原请求身份"))?;
                result.feedback.validate(signal)?;
                anyhow::ensure!(
                    request["signal_id"] == result.feedback.signal_id
                        && request["expected_signal_state_version"]
                            == result.feedback.signal_state_version
                        && request["expected_feedback_version"]
                            .as_u64()
                            .and_then(|v| v.checked_add(1))
                            == Some(result.feedback.revision)
                        && request["kind"] == result.feedback.kind
                        && request["read_ids"] == json!(result.feedback.read_ids)
                        && (result.feedback.summary_redacted
                            || request["summary"] == result.feedback.summary)
                        && result.current_revision >= result.feedback.revision
                        && result.current_revision <= 1000
                        && (result.replayed || result.current_revision == result.feedback.revision),
                    "反馈回执与原请求不一致"
                );
                self.last_result = Some(format!(
                    "{}修订 {} 已确认；服务当前修订 {}。请刷新当前反馈核对。",
                    if result.replayed {
                        "原请求重放："
                    } else {
                        "反馈"
                    },
                    result.feedback.revision,
                    result.current_revision
                ));
                self.pending_submit = None;
                self.signals_stale = true;
                // A replay can return an older revision. Only feedback.get may
                // install an authoritative current judgment.
                self.revoke_evidence();
            }
            _ => anyhow::bail!("无法识别技能反馈操作"),
        }
        Ok(())
    }
}

impl DeerFlowSettings {
    pub(super) fn feedback_editable(&self) -> bool {
        !self.busy()
            && !self.applying()
            && !self.dirty
            && !self.needs_apply
            && self.status["running"] == true
            && !self.skill_browser.mutations_pending()
            && self.skill_browser.feedback.selected.is_some()
    }
}

impl Waku {
    pub(in crate::app) fn deerflow_feedback_edited(&mut self, cx: &mut Context<Self>) {
        let value = self
            .deerflow_settings
            .feedback_input
            .read(cx)
            .content()
            .to_owned();
        let browser = &mut self.deerflow_settings.skill_browser.feedback;
        if browser.summary != value {
            browser.summary = value;
            browser.edit_generation += 1;
            self.deerflow_settings.confirm_manage = None;
            cx.notify();
        }
    }

    pub(super) fn render_deerflow_feedback(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let browser = &state.skill_browser.feedback;
        let unavailable = state.busy() || state.applying() || state.status["running"] != true;
        let editable = state.feedback_editable();
        let mut group = df_group(theme).child(df_label("技能使用证据与反馈",
            "同一任务和技能版本的反馈修订替换原判断。“需要改进”可成为后台生成依据；明确的质量反馈用于发布后的质量观察。归因与证据由宿主核验。", theme));
        group = group.child(df_button(
            "df-signals-refresh",
            "刷新全部信号",
            unavailable,
            false,
            theme,
            cx,
            |this, _, cx| {
                this.deerflow_request(
                    "manage",
                    json!({"operation":"skill.signals.list", "limit":PAGE_SIZE}),
                    cx,
                );
            },
        ));
        if browser.signals_stale {
            group = group.child(df_label(
                "信号列表需要刷新",
                "服务或证据已变化，刷新后继续翻页。",
                theme,
            ));
        }
        if browser.signals_loaded {
            if browser.signals.is_empty() {
                group = group.child(df_label(
                    "暂无使用信号",
                    "当前可见来源尚无已归集的技能读取记录。",
                    theme,
                ));
            }
            for signal in &browser.signals {
                let signal = signal.clone();
                group = group.child(df_button(SharedString::from(format!("df-signal-{}", signal.id)),
                    format!("{} / {} · root {} · {} 次读取", signal.target.source_id, signal.target.name, signal.root_id, signal.read_count),
                    unavailable || browser.signals_stale, browser.selected.as_ref().is_some_and(|old| old.id == signal.id), theme, cx,
                    move |this, _, cx| {
                        if this.deerflow_settings.busy() { return; }
                        this.deerflow_settings.skill_browser.feedback.choose(signal.clone());
                        this.deerflow_settings.confirm_manage = None;
                        this.deerflow_request("manage", json!({"operation":"skill.signals.get", "signal_id":signal.id, "limit":PAGE_SIZE}), cx);
                    }));
            }
            group = group.child(self.render_skill_page_nav(
                "signals",
                &browser.signals_nav,
                unavailable || browser.signals_stale,
                theme,
                cx,
            ));
        }
        if let Some(result) = &browser.last_result {
            group = group.child(df_label("提交回执", result.clone(), theme));
        }
        let Some(signal) = &browser.selected else {
            return group.into_any_element();
        };
        group = group.child(df_label(format!("{} / {} · {}", signal.target.source_id, signal.target.name, signal.target.scope),
            format!("root {} · session {}\n技能版本 {}\n证据状态版本 {} · 反馈修订 {}\n读取 {} · 任务 {} · 角色 {} · 完成 {} / 失败 {} / 取消 {} / 副作用不确定 {}{}",
                signal.root_id, signal.root_session_id, signal.version, signal.state_version, signal.feedback_revision,
                signal.read_count, signal.run_count, signal.role_count, signal.completed_runs, signal.failed_runs, signal.cancelled_runs, signal.uncertain_runs,
                if signal.collection_pending { " · 仍有任务或证据待归集" } else { "" }), theme));
        if let Some(quality) = &signal.quality {
            let label = match quality.kind.as_str() {
                "skill_quality_success" => "操作者确认技能有帮助",
                "skill_quality_failure" => "操作者确认技能质量导致失败",
                "pending" => "仍在归集证据",
                "uncertain_effect" | "uncertain" => "工具副作用待核对",
                "cancelled" => "任务取消",
                "budget" | "quota" => "预算或额度限制",
                "network" => "网络故障",
                "permission" => "权限问题",
                "unknown_failure" => "失败原因未知",
                _ => "尚未确认技能质量归因",
            };
            group = group.child(df_label(
                "质量归因",
                format!(
                    "{label} · 置信度 {} · 来源 {}\n反馈修订 {} · 引用读取 {:?}\n{}",
                    quality.confidence,
                    quality.source,
                    quality.feedback_revision,
                    quality.evidence_read_ids,
                    if quality.eligible {
                        "可计入当前发布版本的质量观察；重复根任务只计一次。"
                    } else {
                        "此信号尚不计入技能质量观察。"
                    }
                ),
                theme,
            ));
        }
        let mut actions = div().flex().flex_wrap().gap(px(6.0));
        for (suffix, label, operation) in [
            ("reads", "刷新读取证据", "skill.signals.get"),
            ("current", "刷新当前反馈", "skill.feedback.get"),
            ("history", "刷新反馈历史", "skill.feedback.list"),
        ] {
            let id = signal.id.clone();
            actions = actions.child(df_button(
                SharedString::from(format!("df-feedback-{suffix}")),
                label,
                unavailable,
                false,
                theme,
                cx,
                move |this, _, cx| {
                    let mut request = json!({"operation":operation, "signal_id":id});
                    if operation != "skill.feedback.get" {
                        request["limit"] = json!(PAGE_SIZE);
                    }
                    this.deerflow_request("manage", request, cx);
                },
            ));
        }
        group = group.child(actions);
        for read in &browser.reads {
            let id = read.id;
            let selected = browser.selected_reads.contains(&id);
            group = group
                .child(df_label(
                    format!(
                        "读取 #{} · {} · {} 字节 · {}",
                        id, read.kind, read.bytes, read.path
                    ),
                    format!(
                        "任务 {} · session {} · attempt {} · role {}\n{} · hash {} · 原始结果 {}{}",
                        read.run_id,
                        read.session_id,
                        read.attempt_id,
                        if read.role_id.is_empty() {
                            "主代理"
                        } else {
                            &read.role_id
                        },
                        read.observed_at,
                        read.hash,
                        read.outcome,
                        if read.effects_uncertain {
                            " · 副作用不确定"
                        } else {
                            ""
                        }
                    ),
                    theme,
                ))
                .child(df_button(
                    SharedString::from(format!("df-feedback-read-{id}")),
                    if selected {
                        "取消引用此读取"
                    } else {
                        "引用此读取"
                    },
                    !editable
                        || !browser.evidence_ready
                        || !selected && browser.selected_reads.len() >= 16,
                    selected,
                    theme,
                    cx,
                    move |this, _, cx| {
                        if this.deerflow_settings.feedback_editable() {
                            this.deerflow_settings
                                .skill_browser
                                .feedback
                                .toggle_read(id);
                            this.deerflow_settings.confirm_manage = None;
                            cx.notify();
                        }
                    },
                ));
        }
        group = group.child(self.render_skill_page_nav(
            "signal-reads",
            &browser.reads_nav,
            unavailable || !browser.evidence_ready,
            theme,
            cx,
        ));
        group = group.child(df_label(
            format!("已选择 {} / 16 条真实读取", browser.selected_reads.len()),
            format!(
                "可跨页选择。引用 ID：{:?}。更换信号、证据版本或服务后清空选择。",
                browser.selected_reads
            ),
            theme,
        ));
        if browser.snapshot_ready {
            if let Some(feedback) = &browser.current {
                group = group.child(render_feedback(
                    feedback,
                    if browser.evidence_current {
                        "当前反馈 · 证据版本一致"
                    } else {
                        "当前反馈 · 已撤回或证据已变化"
                    },
                    theme,
                ));
            } else {
                group = group.child(df_label("当前无反馈", "此信号尚未提交人工判断。", theme));
            }
        } else {
            group = group.child(df_label(
                "提交前刷新当前反馈",
                "读取当前修订后，以完整证据版本和反馈版本提交。",
                theme,
            ));
        }
        let mut kinds = div().flex().flex_wrap().gap(px(6.0));
        for (kind, label) in KINDS {
            kinds = kinds.child(df_button(
                SharedString::from(format!("df-feedback-kind-{kind}")),
                label,
                !editable,
                browser.kind == kind,
                theme,
                cx,
                move |this, _, cx| {
                    if this.deerflow_settings.feedback_editable() {
                        let browser = &mut this.deerflow_settings.skill_browser.feedback;
                        browser.kind = kind.to_owned();
                        browser.edit_generation += 1;
                        this.deerflow_settings.confirm_manage = None;
                        cx.notify();
                    }
                },
            ));
        }
        group = group
            .child(kinds)
            .child(df_label(
                "反馈说明",
                "最多 2000 UTF-8 字节；引用 1–16 条实际读取。只有明确选择“确认技能质量导致失败”才提交质量失败判断；网络、额度及权限问题单独记录。",
                theme,
            ))
            .child(
                div()
                    .id("df-feedback-summary")
                    .h(px(100.0))
                    .w_full()
                    .overflow_hidden()
                    .overflow_y_scroll()
                    .p(px(10.0))
                    .rounded(px(6.0))
                    .border_1()
                    .border_color(theme.border_strong)
                    .bg(theme.inset)
                    .child(state.feedback_input.clone()),
            );
        for (withdrawn, label) in [(false, "提交反馈"), (true, "撤回当前反馈")] {
            group = group.child(df_button(
                SharedString::from(format!("df-feedback-submit-{withdrawn}")),
                label,
                !editable || !browser.can_submit(withdrawn),
                false,
                theme,
                cx,
                move |this, _, cx| {
                    if this.deerflow_settings.feedback_editable() {
                        if let Some(request) = this
                            .deerflow_settings
                            .skill_browser
                            .feedback
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
                "反馈过长",
                format!(
                    "当前 {} UTF-8 字节，上限 2000。草稿已保留。",
                    browser.summary.len()
                ),
                theme,
            ));
        }
        if browser.history_loaded {
            group = group.child(df_label(
                "反馈历史",
                format!(
                    "查询时当前修订 {}；历史修订不计为独立投票。",
                    browser.history_revision
                ),
                theme,
            ));
            if browser.history.is_empty() {
                group = group.child(df_label("暂无反馈修订", "此信号还没有历史反馈。", theme));
            }
            for feedback in &browser.history {
                group = group.child(render_feedback(feedback, "历史反馈", theme));
            }
            group = group.child(self.render_skill_page_nav(
                "feedback-history",
                &browser.history_nav,
                unavailable,
                theme,
                cx,
            ));
        }
        group.into_any_element()
    }
}

fn render_feedback(feedback: &Feedback, title: &str, theme: Theme) -> AnyElement {
    df_label(
        format!(
            "{title} · 修订 {} · {}",
            feedback.revision,
            kind_label(&feedback.kind)
        ),
        format!(
            "{}\n证据版本 {} · 引用 {:?} · {}{}",
            feedback.summary,
            feedback.signal_state_version,
            feedback.read_ids,
            feedback.created_at,
            if feedback.summary_redacted {
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
#[path = "feedback_tests.rs"]
mod tests;
