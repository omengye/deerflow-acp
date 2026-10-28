//! Operator generation uses cached, bounded projections and the existing MANAGE
//! executor. Form inputs are transient and never become configuration edits.
use super::skills::{Head, PAGE_SIZE, PageNav, Target, digest};
use super::*;
use serde::Deserialize;

#[derive(Clone, Deserialize)]
struct Budget {
    max_daily_model_calls: u64,
    max_daily_tokens: u64,
    max_job_seconds: u64,
    workers: u64,
}

#[derive(Clone, Deserialize)]
struct Status {
    version: String,
    enabled: bool,
    sources: Vec<Target>,
    budget: Option<Budget>,
}

#[derive(Clone, Deserialize)]
struct Prepared {
    target: Target,
    base: Head,
    #[serde(default)]
    overlay_base: Value,
}

#[derive(Clone, Deserialize)]
struct Job {
    id: String,
    target: Target,
    base: Head,
    state: String,
    phase: String,
    state_version: u64,
    attempts: u64,
    #[serde(default)]
    deadline: String,
    #[serde(default)]
    retry_at: String,
    #[serde(default)]
    error_code: String,
    #[serde(default)]
    proposal_id: String,
    #[serde(default)]
    discovery_id: String,
    #[serde(default)]
    candidate_sha256: String,
    created_at: String,
}

impl Job {
    fn validate(&self) -> anyhow::Result<()> {
        anyhow::ensure!(
            valid_id(&self.id)
                && self.id.starts_with("smj_")
                && valid_target(&self.target, true)
                && valid_head(&self.base)
                && self.state_version > 0
                && self.state_version <= i64::MAX as u64
                && self.attempts <= 3
                && matches!(
                    self.state.as_str(),
                    "queued" | "running" | "retry" | "completed" | "failed" | "cancelled"
                )
                && matches!(
                    self.phase.as_str(),
                    "generating" | "validating" | "review_complete"
                )
                && (self.proposal_id.is_empty() || valid_id(&self.proposal_id))
                && (self.discovery_id.is_empty() || valid_id(&self.discovery_id))
                && (self.candidate_sha256.is_empty() || digest(&self.candidate_sha256))
                && self.error_code.len() <= 128
                && timestamp(&self.created_at)
                && (self.deadline.is_empty() || timestamp(&self.deadline))
                && (self.retry_at.is_empty() || timestamp(&self.retry_at)),
            "生成作业响应缺少有效身份、版本或状态"
        );
        Ok(())
    }

    fn cancellable(&self) -> bool {
        matches!(self.state.as_str(), "queued" | "running" | "retry")
    }
}

#[derive(Clone, Deserialize)]
struct Receipt {
    sequence: u64,
    id: String,
    job_id: String,
    attempt: u64,
    day: String,
    stage: String,
    model_identity: String,
    input_digest: String,
    reserved_tokens: u64,
    charged_tokens: u64,
    input_tokens: u64,
    output_tokens: u64,
    state: String,
    estimated: bool,
}

#[derive(Default)]
pub(super) struct GenerationBrowser {
    status: Option<Status>,
    source: Option<Target>,
    name: String,
    reason: String,
    edit_generation: u64,
    preparing_generation: Option<u64>,
    prepared: Option<Prepared>,
    jobs: Vec<Job>,
    jobs_nav: PageNav,
    jobs_loaded: bool,
    jobs_stale: bool,
    selected: Option<Job>,
    selected_stale: bool,
    receipts: Vec<Receipt>,
    receipts_loaded: bool,
    receipt_after: u64,
    receipt_previous: Vec<u64>,
    receipt_next: Option<u64>,
}

fn valid_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 64
        && !name.starts_with('-')
        && !name.ends_with('-')
        && !name.contains("--")
        && name
            .bytes()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == b'-')
}

fn valid_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 128
        && id
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || c == b'-' || c == b'_')
}

fn timestamp(value: &str) -> bool {
    chrono::DateTime::parse_from_rfc3339(value).is_ok()
}

fn valid_target(target: &Target, named: bool) -> bool {
    valid_name(&target.source_id)
        && digest(&target.source_identity)
        && (if named {
            valid_name(&target.name)
        } else {
            target.name.is_empty()
        })
        && match target.scope.as_str() {
            "global" => target.workspace.is_empty(),
            "workspace" => {
                !target.workspace.is_empty()
                    && target.workspace.len() <= 32768
                    && !target.workspace.contains('\0')
            }
            _ => false,
        }
}

fn valid_head(head: &Head) -> bool {
    head.state_version <= i64::MAX as u64
        && if head.exists {
            head.state_version > 0 && digest(&head.version)
        } else {
            head.version.is_empty() && !head.enabled && (head.state_version > 0 || !head.managed)
        }
}

impl GenerationBrowser {
    pub(super) fn status_summary(&self) -> Option<String> {
        self.status.as_ref().map(|status| format!(
            "草稿生成{} · 可授权目标来源 {} 个。生成产出请展开学习与生成详情查看作业；正式采用还需审核或满足自动补丁条件。",
            if status.enabled { "已启用" } else { "关闭" }, status.sources.len()
        ))
    }

    pub(super) fn creation_retry_previous(&self) -> Option<(String, String)> {
        let job = self.selected.as_ref()?;
        (!self.selected_stale
            && !job.discovery_id.is_empty()
            && matches!(job.state.as_str(), "failed" | "cancelled" | "completed"))
        .then(|| {
            (
                job.id.clone(),
                format!(
                    "{} / {} · 作业 {} · {}",
                    job.target.source_id, job.target.name, job.id, job.state
                ),
            )
        })
    }

    pub(super) fn invalidate_learning_sources(&mut self) {
        self.invalidate();
        self.jobs.clear();
        self.jobs_loaded = false;
        self.jobs_nav = PageNav::default();
        self.selected = None;
    }

    pub(super) fn invalidate(&mut self) {
        self.status = None;
        self.prepared = None;
        self.preparing_generation = None;
        self.jobs_stale = true;
        self.selected_stale = true;
        self.clear_receipts();
    }

    fn clear_receipts(&mut self) {
        self.receipts.clear();
        self.receipts_loaded = false;
        self.receipt_after = 0;
        self.receipt_previous.clear();
        self.receipt_next = None;
    }

    fn enabled(&self) -> bool {
        self.status.as_ref().is_some_and(|status| status.enabled)
    }

    fn choose_source(&mut self, source: &Target) {
        if self
            .status
            .as_ref()
            .is_some_and(|status| status.sources.contains(source))
        {
            self.source = Some(source.clone());
            self.prepared = None;
            self.edit_generation += 1;
        }
    }

    pub(super) fn begin(&mut self, request: &Value) {
        if request["operation"] == "skill.generation.prepare" {
            self.preparing_generation = Some(self.edit_generation);
        }
    }

    fn target(&self) -> Option<Target> {
        let source = self.source.as_ref()?;
        if !self.enabled()
            || !self.status.as_ref()?.sources.contains(source)
            || !valid_name(&self.name)
        {
            return None;
        }
        let mut target = source.clone();
        target.name = self.name.clone();
        Some(target)
    }

    fn valid_reason(&self) -> bool {
        !self.reason.trim().is_empty() && self.reason.len() <= 4000 && !self.reason.contains('\0')
    }

    fn prepare_request(&self) -> Option<Value> {
        if !self.valid_reason() {
            return None;
        }
        Some(json!({"operation":"skill.generation.prepare", "target":self.target()?}))
    }

    fn enqueue_request(&self) -> Option<Value> {
        let prepared = self.prepared.as_ref()?;
        if !self.valid_reason() || self.target().as_ref() != Some(&prepared.target) {
            return None;
        }
        Some(
            json!({"operation":"skill.generation.enqueue","target":prepared.target,"expected_head":prepared.base,
            "request_key":Uuid::new_v4().to_string(),"reason":self.reason}),
        )
    }

    pub(super) fn accept(&mut self, request: &Value, data: Value) -> anyhow::Result<()> {
        match request["operation"].as_str().unwrap_or("") {
            "skill.generation.status" => {
                let status: Status = serde_json::from_value(data)?;
                anyhow::ensure!(
                    status.version == "skill-generation-v1"
                        && status.sources.len() <= 32
                        && status
                            .sources
                            .iter()
                            .all(|source| valid_target(source, false))
                        && status
                            .sources
                            .iter()
                            .map(|source| &source.source_id)
                            .collect::<HashSet<_>>()
                            .len()
                            == status.sources.len(),
                    "生成能力响应无效"
                );
                if status.enabled {
                    let b = status
                        .budget
                        .as_ref()
                        .ok_or_else(|| anyhow::anyhow!("缺少维护预算"))?;
                    anyhow::ensure!(
                        !status.sources.is_empty()
                            && (1..=100000).contains(&b.max_daily_model_calls)
                            && (1..=1000000000).contains(&b.max_daily_tokens)
                            && (1..=3600).contains(&b.max_job_seconds)
                            && (1..=16).contains(&b.workers),
                        "维护预算无效"
                    );
                } else {
                    anyhow::ensure!(
                        status.sources.is_empty() && status.budget.is_none(),
                        "关闭的生成能力仍声明授权"
                    );
                }
                if self
                    .source
                    .as_ref()
                    .is_some_and(|source| !status.sources.contains(source))
                {
                    self.source = None;
                }
                self.prepared = None;
                self.jobs_stale = true;
                self.selected_stale = true;
                self.clear_receipts();
                self.status = Some(status);
            }
            "skill.generation.prepare" => {
                let prepared: Prepared = serde_json::from_value(data)?;
                let requested: Target = serde_json::from_value(request["target"].clone())?;
                anyhow::ensure!(
                    prepared.target == requested
                        && valid_target(&prepared.target, true)
                        && valid_head(&prepared.base),
                    "准备结果与目标不一致"
                );
                if !prepared.overlay_base.is_null() {
                    let overlay = &prepared.overlay_base;
                    anyhow::ensure!(
                        overlay["sourceId"] == prepared.target.source_id
                            && overlay["sourceIdentity"] == prepared.target.source_identity
                            && overlay["name"] == prepared.target.name
                            && overlay["version"].as_str().is_some_and(digest)
                            && overlay["hash"] == overlay["version"],
                        "内置技能基础版本无效"
                    );
                }
                // A service or editor change must not authorize a different draft.
                anyhow::ensure!(
                    self.target().as_ref() == Some(&requested)
                        && self.preparing_generation == Some(self.edit_generation),
                    "目标已变化，请重新检查"
                );
                self.prepared = Some(prepared);
                self.preparing_generation = None;
            }
            "skill.generation.list" => {
                #[derive(Deserialize)]
                struct Page {
                    jobs: Vec<Job>,
                    #[serde(default)]
                    next_cursor: String,
                }
                let page: Page = serde_json::from_value(data)?;
                anyhow::ensure!(
                    page.jobs.len() <= PAGE_SIZE
                        && page
                            .jobs
                            .iter()
                            .map(|job| &job.id)
                            .collect::<HashSet<_>>()
                            .len()
                            == page.jobs.len(),
                    "作业列表超出限制或重复"
                );
                for job in &page.jobs {
                    job.validate()?;
                    for (key, actual) in [
                        ("source_id", &job.target.source_id),
                        ("name", &job.target.name),
                    ] {
                        anyhow::ensure!(
                            request[key]
                                .as_str()
                                .is_none_or(|expected| expected == actual),
                            "作业列表不符合筛选条件"
                        );
                    }
                }
                self.jobs_nav.accept(request, &page.next_cursor, "cursor")?;
                self.jobs = page.jobs;
                self.jobs_loaded = true;
                self.jobs_stale = false;
            }
            "skill.generation.get" | "skill.generation.enqueue" => {
                let job: Job = serde_json::from_value(data)?;
                job.validate()?;
                if request["operation"] == "skill.generation.get" {
                    anyhow::ensure!(request["job_id"] == job.id, "作业响应与请求不一致");
                } else {
                    let target: Target = serde_json::from_value(request["target"].clone())?;
                    let base: Head = serde_json::from_value(request["expected_head"].clone())?;
                    anyhow::ensure!(
                        job.target == target && job.base == base,
                        "入队响应与原请求不一致"
                    );
                    self.prepared = None;
                    self.jobs_stale = true;
                }
                if let Some(prior) = &self.selected {
                    if prior.id == job.id {
                        anyhow::ensure!(
                            prior.target == job.target
                                && prior.base == job.base
                                && prior.state_version <= job.state_version,
                            "作业来源或版本倒退"
                        );
                    }
                }
                self.clear_receipts();
                self.selected = Some(job);
                self.selected_stale = false;
            }
            "skill.generation.cancel" => {
                anyhow::ensure!(data["cancelled"] == true, "取消结果缺少确认");
                self.selected_stale = true;
                self.jobs_stale = true;
                self.prepared = None;
                self.clear_receipts();
            }
            "skill.generation.receipts" => self.accept_receipts(request, data)?,
            _ => anyhow::bail!("未知生成操作"),
        }
        Ok(())
    }

    fn accept_receipts(&mut self, request: &Value, data: Value) -> anyhow::Result<()> {
        let job = self
            .selected
            .as_ref()
            .ok_or_else(|| anyhow::anyhow!("请重新选择作业"))?;
        anyhow::ensure!(
            !self.selected_stale && request["job_id"] == job.id,
            "回执作业已变化"
        );
        let after = request["after_sequence"].as_u64().unwrap_or(0);
        anyhow::ensure!(data.get("receipts").is_some(), "响应缺少回执集合");
        // Older hosts serialize an empty slice as null.
        let receipts: Vec<Receipt> = if data["receipts"].is_null() {
            Vec::new()
        } else {
            serde_json::from_value(data["receipts"].clone())?
        };
        anyhow::ensure!(receipts.len() <= PAGE_SIZE, "回执页面超出限制");
        let mut last = after;
        for receipt in &receipts {
            anyhow::ensure!(
                receipt.sequence > last
                    && receipt.sequence <= i64::MAX as u64
                    && receipt.job_id == job.id
                    && valid_id(&receipt.id)
                    && (1..=3).contains(&receipt.attempt)
                    && digest(&receipt.input_digest)
                    && !receipt.model_identity.is_empty()
                    && receipt.model_identity.len() <= 128
                    && chrono::NaiveDate::parse_from_str(&receipt.day, "%Y-%m-%d").is_ok()
                    && matches!(
                        receipt.stage.as_str(),
                        "generation" | "security" | "quality" | "behavior"
                    )
                    && matches!(receipt.state.as_str(), "reserved" | "settled")
                    && [
                        receipt.reserved_tokens,
                        receipt.charged_tokens,
                        receipt.input_tokens,
                        receipt.output_tokens
                    ]
                    .iter()
                    .all(|value| *value <= i64::MAX as u64),
                "回执身份、阶段或序号无效"
            );
            last = receipt.sequence;
        }
        anyhow::ensure!(
            data["last_sequence"].as_u64() == Some(last),
            "回执游标不一致"
        );
        if after == 0 {
            self.receipt_previous.clear();
        } else if self.receipt_next == Some(after) && after != self.receipt_after {
            anyhow::ensure!(self.receipt_previous.len() < 1000, "分页数量达到上限");
            self.receipt_previous.push(self.receipt_after);
        } else if self.receipt_previous.last() == Some(&after) {
            self.receipt_previous.pop();
        } else {
            anyhow::ensure!(after == self.receipt_after, "回执游标已变化，请刷新");
        }
        self.receipt_after = after;
        self.receipt_next = (receipts.len() == PAGE_SIZE).then_some(last);
        self.receipts = receipts;
        self.receipts_loaded = true;
        Ok(())
    }
}

impl DeerFlowSettings {
    pub(super) fn generation_editable(&self) -> bool {
        !self.busy()
            && !self.applying()
            && !self.dirty
            && !self.needs_apply
            && self.status["running"] == true
            && !self.skill_browser.mutations_pending()
            && self.skill_browser.generation.enabled()
    }
}

impl Waku {
    pub(in crate::app) fn deerflow_generation_edited(
        &mut self,
        index: usize,
        cx: &mut Context<Self>,
    ) {
        let value = self.deerflow_settings.generation_inputs[index]
            .read(cx)
            .content()
            .to_owned();
        let browser = &mut self.deerflow_settings.skill_browser.generation;
        let text = if index == 0 {
            &mut browser.name
        } else {
            &mut browser.reason
        };
        if *text == value {
            return;
        }
        *text = value;
        browser.edit_generation += 1;
        browser.prepared = None;
        self.deerflow_settings.confirm_manage = None;
        cx.notify();
    }

    pub(super) fn render_deerflow_generation(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let browser = &state.skill_browser.generation;
        let unavailable = state.busy() || state.applying() || state.status["running"] != true;
        let editable = state.generation_editable();
        let mut group = df_group(theme).child(df_label(
            "后台生成技能候选",
            "生成请求使用独立维护预算。新技能及超出自动补丁范围的改动需要人工审核。",
            theme,
        ));
        group = group.child(df_button(
            "df-generation-status",
            "刷新生成能力",
            unavailable,
            false,
            theme,
            cx,
            |this, _, cx| {
                this.deerflow_request("manage", json!({"operation":"skill.generation.status"}), cx);
            },
        ));
        let Some(status) = &browser.status else {
            return group
                .child(df_label(
                    "尚未确认当前服务能力",
                    "启动服务并刷新后，可提交请求和查询已有作业。输入草稿会保留。",
                    theme,
                ))
                .into_any_element();
        };
        if let Some(b) = &status.budget {
            group = group.child(df_label("当前维护限额",format!("每日 {} 次 / {} Token（UTC）· 作业 {} 秒 · 并发 {}。这些是配置上限，剩余额度以实际执行为准。",b.max_daily_model_calls,b.max_daily_tokens,b.max_job_seconds,b.workers),theme));
        } else {
            group = group.child(df_label(
                "生成已关闭",
                "仍可查询历史作业；启用需保存并应用生成设置。",
                theme,
            ));
        }
        if status.enabled {
            let mut sources = div().flex().flex_wrap().gap(px(6.0));
            for source in &status.sources {
                let selected = browser.source.as_ref() == Some(source);
                let source = source.clone();
                sources = sources.child(df_button(
                    SharedString::from(format!("df-generation-source-{}", source.source_identity)),
                    format!("{} · {}", source.source_id, source.scope),
                    !editable,
                    selected,
                    theme,
                    cx,
                    move |this, _, cx| {
                        if this.deerflow_settings.generation_editable() {
                            this.deerflow_settings
                                .skill_browser
                                .generation
                                .choose_source(&source);
                            this.deerflow_settings.confirm_manage = None;
                            cx.notify();
                        }
                    },
                ));
            }
            group = group
                .child(df_label(
                    "目标来源",
                    "选择当前服务授予生成权限的来源；同名技能按来源区分。",
                    theme,
                ))
                .child(sources)
                .child(df_label(
                    "技能名称",
                    "新建或改进现有技能；1–64 位小写字母、数字、单个连字符。",
                    theme,
                ))
                .child(
                    TextField::new("df-generation-name", state.generation_inputs[0].clone())
                        .w(px(300.0)),
                )
                .child(df_label(
                    "改进要求",
                    "说明需要解决的问题和预期行为，最多 4000 UTF-8 字节。",
                    theme,
                ))
                .child(
                    div()
                        .id("df-generation-reason")
                        .h(px(110.0))
                        .w_full()
                        .overflow_hidden()
                        .overflow_y_scroll()
                        .p(px(10.0))
                        .rounded(px(6.0))
                        .border_1()
                        .border_color(theme.border_strong)
                        .bg(theme.inset)
                        .child(state.generation_inputs[1].clone()),
                );
            let prepare = browser.prepare_request();
            group = group.child(df_button(
                "df-generation-prepare",
                "检查目标版本",
                !editable || prepare.is_none(),
                false,
                theme,
                cx,
                move |this, _, cx| {
                    if this.deerflow_settings.generation_editable() {
                        if let Some(request) = this
                            .deerflow_settings
                            .skill_browser
                            .generation
                            .prepare_request()
                        {
                            this.deerflow_request("manage", request, cx);
                        }
                    }
                },
            ));
            if let Some(prepared) = &browser.prepared {
                group = group.child(df_label(
                    format!("{} / {}", prepared.target.source_id, prepared.target.name),
                    format!(
                        "{} · 状态版本 {} · {}{}",
                        if prepared.base.exists {
                            "改进现有技能"
                        } else {
                            "创建技能"
                        },
                        prepared.base.state_version,
                        if prepared.base.version.is_empty() {
                            "无已发布内容"
                        } else {
                            &prepared.base.version
                        },
                        if prepared.overlay_base.is_null() {
                            ""
                        } else {
                            " · 内置覆盖"
                        }
                    ),
                    theme,
                ));
                group = group.child(df_button(
                    "df-generation-enqueue",
                    "提交候选生成",
                    !editable,
                    false,
                    theme,
                    cx,
                    |this, _, cx| {
                        if this.deerflow_settings.generation_editable() {
                            if let Some(request) = this
                                .deerflow_settings
                                .skill_browser
                                .generation
                                .enqueue_request()
                            {
                                this.deerflow_request("manage", request, cx);
                            }
                        }
                    },
                ));
            }
            if !browser.name.is_empty() && !valid_name(&browser.name)
                || !browser.reason.is_empty() && !browser.valid_reason()
            {
                group = group.child(df_label(
                    "输入尚不完整",
                    "检查技能名称和改进要求的格式与长度；草稿仍保留。",
                    theme,
                ));
            }
            if state.dirty || state.needs_apply {
                group = group.child(df_label(
                    "配置尚未应用",
                    "先保存并应用配置，或重新加载放弃草稿，再提交生成请求。",
                    theme,
                ));
            }
        }
        let mut filters = div().flex().flex_wrap().gap(px(6.0));
        for (suffix,label,query) in [
            ("all","刷新全部作业",Some(json!({"operation":"skill.generation.list","limit":PAGE_SIZE}))),
            ("target","查看此技能作业",browser.target().map(|t|json!({"operation":"skill.generation.list","source_id":t.source_id,"name":t.name,"limit":PAGE_SIZE}))),
        ] {
            filters = filters.child(df_button(SharedString::from(format!("df-generation-list-{suffix}")),label,unavailable || query.is_none(),false,theme,cx,move |this,_,cx|{
                if let Some(query) = &query { this.deerflow_request("manage",query.clone(),cx); }
            }));
        }
        group = group.child(filters);
        if browser.jobs_stale {
            group = group.child(df_label(
                "作业列表需要刷新",
                "后台进度、服务或来源已变化；刷新后再继续翻页。",
                theme,
            ));
        }
        if browser.jobs_loaded {
            if browser.jobs.is_empty() {
                group = group.child(df_label("暂无生成作业", "当前查询没有匹配记录。", theme));
            }
            for job in &browser.jobs {
                let id = job.id.clone();
                group = group.child(df_button(
                    SharedString::from(format!("df-generation-job-{id}")),
                    format!(
                        "{} / {} · {} · {}",
                        job.target.source_id,
                        job.target.name,
                        job_state(&job.state),
                        job.created_at
                    ),
                    unavailable,
                    false,
                    theme,
                    cx,
                    move |this, _, cx| {
                        this.deerflow_request(
                            "manage",
                            json!({"operation":"skill.generation.get","job_id":id}),
                            cx,
                        );
                    },
                ));
            }
            group = group.child(self.render_skill_page_nav(
                "generation",
                &browser.jobs_nav,
                unavailable || browser.jobs_stale,
                theme,
                cx,
            ));
        }
        if let Some(job) = &browser.selected {
            group = group.child(df_label(
                format!(
                    "{} / {} · {}",
                    job.target.source_id,
                    job.target.name,
                    job_state(&job.state)
                ),
                format!(
                    "作业 {} · 状态版本 {} · 尝试 {} 次 · {}",
                    job.id,
                    job.state_version,
                    job.attempts,
                    phase_label(&job.phase)
                ),
                theme,
            ));
            if !job.error_code.is_empty() {
                // Error details remain a bounded code; provider text is never
                // copied into this cached settings projection.
                group = group.child(df_label("作业结果", error_label(&job.error_code), theme));
            }
            if !job.discovery_id.is_empty() {
                group = group.child(df_label(
                    "自动选择来源",
                    format!("自动发现记录 {}。可在自动发现列表查看触发类型及证据；发布遵循审核和自动补丁规则。", job.discovery_id),
                    theme,
                ));
            }
            if job.deadline != "0001-01-01T00:00:00Z" && !job.deadline.is_empty() {
                group = group.child(df_label("作业截止时间", job.deadline.clone(), theme));
            }
            if job.state == "retry" {
                group = group.child(df_label("下次尝试时间", job.retry_at.clone(), theme));
            }
            if browser.selected_stale {
                group = group.child(df_label(
                    "详情需要刷新",
                    "取消或服务变化后需重新读取作业，旧状态不用于操作。",
                    theme,
                ));
            }
            let id = job.id.clone();
            group = group.child(df_button(
                "df-generation-get",
                "刷新作业详情",
                unavailable,
                false,
                theme,
                cx,
                move |this, _, cx| {
                    this.deerflow_request(
                        "manage",
                        json!({"operation":"skill.generation.get","job_id":id}),
                        cx,
                    );
                },
            ));
            if job.cancellable() {
                let id = job.id.clone();
                let version = job.state_version;
                group = group.child(df_button("df-generation-cancel","取消生成作业",unavailable || browser.selected_stale || state.dirty || state.skill_browser.mutations_pending(),false,theme,cx,move |this,_,cx| {
                    this.deerflow_settings.confirm_manage = None;
                    this.deerflow_request("manage", json!({"operation":"skill.generation.cancel","job_id":id,"expected_state_version":version}), cx);
                }));
                group = group.child(df_label(
                    "取消说明",
                    "停止该作业及尚未完成的候选审核；已发生的维护用量和已发布版本保留。",
                    theme,
                ));
            }
            if !job.proposal_id.is_empty() {
                let id = job.proposal_id.clone();
                group = group.child(df_button(
                    "df-generation-proposal",
                    "查看候选与审核",
                    unavailable || browser.selected_stale,
                    false,
                    theme,
                    cx,
                    move |this, _, cx| {
                        this.deerflow_request(
                            "manage",
                            json!({"operation":"proposal.get","proposal_id":id}),
                            cx,
                        );
                    },
                ));
            }
            for (key, label, after) in [
                ("refresh", "刷新用量回执", Some(0)),
                (
                    "previous",
                    "回执上一页",
                    browser.receipt_previous.last().copied(),
                ),
                ("next", "回执下一页", browser.receipt_next),
            ] {
                let id = job.id.clone();
                group = group.child(df_button(SharedString::from(format!("df-generation-receipts-{key}")),label,unavailable || browser.selected_stale || after.is_none(),false,theme,cx,move |this,_,cx| {
                    if let Some(after) = after {
                        let mut request = json!({"operation":"skill.generation.receipts","job_id":id,"limit":PAGE_SIZE});
                        if after > 0 { request["after_sequence"] = json!(after); }
                        this.deerflow_request("manage",request,cx);
                    }
                }));
            }
            if browser.receipts_loaded && browser.receipts.is_empty() {
                group = group.child(df_label(
                    "本页没有调用回执",
                    "可能尚未调用模型，或已到达回执末页。",
                    theme,
                ));
            }
            for receipt in &browser.receipts {
                group = group.child(df_label(
                    format!(
                        "#{} · {} · 尝试 {} · {}",
                        receipt.sequence,
                        stage_label(&receipt.stage),
                        receipt.attempt,
                        receipt.day
                    ),
                    format!(
                        "计入 {} Token · 预留 {} · 输入 {} / 输出 {} · {}{}",
                        receipt.charged_tokens,
                        receipt.reserved_tokens,
                        receipt.input_tokens,
                        receipt.output_tokens,
                        if receipt.state == "reserved" {
                            "已预留"
                        } else {
                            "已结算"
                        },
                        if receipt.estimated {
                            "（保守估算）"
                        } else {
                            "（提供方用量）"
                        }
                    ),
                    theme,
                ));
            }
        }
        group.into_any_element()
    }
}

fn job_state(state: &str) -> &'static str {
    match state {
        "queued" => "等待执行",
        "running" => "执行中",
        "retry" => "等待重试",
        "completed" => "审核已完成",
        "failed" => "失败",
        "cancelled" => "已取消",
        _ => "未知",
    }
}
fn phase_label(phase: &str) -> &'static str {
    match phase {
        "generating" => "生成阶段",
        "validating" => "审查阶段",
        "review_complete" => "已交给提案流程",
        _ => "未知阶段",
    }
}
fn stage_label(stage: &str) -> &'static str {
    match stage {
        "generation" => "生成",
        "security" => "安全审查",
        "quality" => "质量审查",
        "behavior" => "行为评测",
        _ => "未知阶段",
    }
}
fn error_label(code: &str) -> &'static str {
    match code {
        "budget_exhausted" => "维护额度已耗尽。",
        "base_changed" => "技能基础版本已变化，需要重新检查并提交。",
        "evidence_changed" => "自动选择依据已撤回、更新或不再可用，后续生成及审查已停止。",
        "configuration_changed" => "生成或审查模型配置已变化，旧作业不会自动切换模型。",
        "source_unavailable" => "技能源已不可用或授权已撤销。",
        "invalid_candidate" => "候选内容无效或未通过审查。",
        "model_failed" => "模型生成或审查失败，重试已结束。",
        "job_deadline" => "作业时限已到。",
        "cancelled" => "作业已请求取消。",
        "worker_interrupted" => "执行曾被中断，请查看当前状态。",
        _ => "作业暂时不可用，请查看状态或日志。",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn target() -> Value {
        json!({"source_id":"custom","source_identity":"a".repeat(64),"name":"research","scope":"global"})
    }
    fn head() -> Value {
        json!({"version":"","state_version":0,"exists":false,"enabled":false,"managed":false})
    }
    fn status(enabled: bool) -> Value {
        let mut source = target();
        source["name"] = json!("");
        if enabled {
            json!({"version":"skill-generation-v1","enabled":true,"sources":[source],"budget":{"max_daily_model_calls":30,"max_daily_tokens":100000,"max_job_seconds":300,"workers":1}})
        } else {
            json!({"version":"skill-generation-v1","enabled":false,"sources":[]})
        }
    }
    fn editor() -> GenerationBrowser {
        let mut browser = GenerationBrowser::default();
        browser
            .accept(
                &json!({"operation":"skill.generation.status"}),
                status(true),
            )
            .unwrap();
        let source = browser.status.as_ref().unwrap().sources[0].clone();
        browser.choose_source(&source);
        browser.name = "research".into();
        browser.reason = "Verify source evidence".into();
        browser
    }
    fn prepare(browser: &mut GenerationBrowser) -> Value {
        let request = browser.prepare_request().unwrap();
        browser.begin(&request);
        browser
            .accept(&request, json!({"target":target(),"base":head()}))
            .unwrap();
        browser.enqueue_request().unwrap()
    }
    fn job(id: &str) -> Value {
        json!({"id":id,"target":target(),"base":head(),"state":"queued","phase":"generating","state_version":1,"attempts":0,"created_at":"2026-10-02T00:00:00Z","deadline":"0001-01-01T00:00:00Z","retry_at":"0001-01-01T00:00:00Z"})
    }
    fn select(browser: &mut GenerationBrowser, id: &str) {
        browser
            .accept(
                &json!({"operation":"skill.generation.get","job_id":id}),
                job(id),
            )
            .unwrap();
    }
    fn receipt(sequence: u64, id: &str) -> Value {
        json!({"sequence":sequence,"id":format!("smc_{sequence}"),"job_id":id,"attempt":1,"day":"2026-10-02","stage":"generation","model_identity":"b".repeat(64),"input_digest":"c".repeat(64),"reserved_tokens":9000,"charged_tokens":9000,"input_tokens":0,"output_tokens":0,"state":"reserved","estimated":true})
    }

    #[test]
    fn deerflow_generation_prepare_pins_complete_absent_head_and_draft() {
        let mut browser = editor();
        assert!(browser.enqueue_request().is_none());
        let request = prepare(&mut browser);
        assert_eq!(request["target"], target());
        assert_eq!(request["expected_head"], head());
        assert_eq!(request["reason"], "Verify source evidence");
        assert!(Uuid::parse_str(request["request_key"].as_str().unwrap()).is_ok());
        let query = browser.prepare_request().unwrap();
        browser.begin(&query);
        browser.edit_generation += 1;
        browser.reason = "Updated intent".into();
        browser.prepared = None;
        assert!(
            browser
                .accept(&query, json!({"target":target(),"base":head()}))
                .is_err()
        );
        assert!(browser.enqueue_request().is_none());
        let request = prepare(&mut browser);
        let mut response = job("smj_one");
        response["base"]["managed"] = json!(true);
        assert!(browser.accept(&request, response).is_err());
        assert!(browser.selected.is_none());
        browser.accept(&request, job("smj_one")).unwrap();
        assert!(browser.prepared.is_none());
        assert!(!browser.selected_stale);
        assert!(browser.jobs_stale);
        assert_eq!(browser.selected.as_ref().unwrap().id, "smj_one");
    }

    #[test]
    fn deerflow_generation_learning_source_change_clears_cached_jobs_without_erasing_draft() {
        let mut browser = editor();
        let request = json!({"operation":"skill.generation.list", "limit":PAGE_SIZE});
        browser
            .accept(&request, json!({"jobs":[job("smj_one")]}))
            .unwrap();
        select(&mut browser, "smj_one");
        browser.receipts_loaded = true;
        let name = browser.name.clone();
        let reason = browser.reason.clone();
        browser.invalidate_learning_sources();
        assert_eq!(browser.name, name);
        assert_eq!(browser.reason, reason);
        assert!(browser.jobs.is_empty());
        assert!(!browser.jobs_loaded);
        assert!(browser.jobs_stale);
        assert!(browser.selected.is_none());
        assert!(browser.receipts.is_empty());
        assert!(!browser.receipts_loaded);
        assert!(browser.prepare_request().is_none());
        assert!(browser.jobs_nav.request(true).is_none());
    }

    #[test]
    fn deerflow_generation_service_changes_preserve_draft_and_revoke_old_authority() {
        let mut browser = editor();
        prepare(&mut browser);
        select(&mut browser, "smj_one");
        browser.invalidate();
        assert_eq!(browser.name, "research");
        assert_eq!(browser.reason, "Verify source evidence");
        assert!(browser.status.is_none());
        assert!(browser.selected_stale);
        assert!(browser.enqueue_request().is_none());
        let mut changed = status(true);
        changed["sources"][0]["source_identity"] = json!("d".repeat(64));
        browser
            .accept(&json!({"operation":"skill.generation.status"}), changed)
            .unwrap();
        assert!(browser.source.is_none());
        assert!(browser.prepare_request().is_none());
        browser
            .accept(
                &json!({"operation":"skill.generation.status"}),
                status(false),
            )
            .unwrap();
        assert!(!browser.enabled());
        browser
            .accept(
                &json!({"operation":"skill.generation.list","limit":PAGE_SIZE}),
                json!({"jobs":[job("smj_one")]}),
            )
            .unwrap();
        select(&mut browser, "smj_one");
        assert!(browser.selected.as_ref().unwrap().cancellable());
        browser.accept(&json!({"operation":"skill.generation.cancel","job_id":"smj_one","expected_state_version":1}),json!({"cancelled":true})).unwrap();
        assert!(browser.selected_stale);
    }

    #[test]
    fn deerflow_generation_rejects_malformed_capabilities_and_foreign_progress() {
        for (path, value) in [
            ("/version", json!("old")),
            ("/budget/workers", json!(0)),
            ("/sources/0/name", json!("not-a-source")),
            ("/sources/0/source_identity", json!("unknown")),
        ] {
            let mut response = status(true);
            *response.pointer_mut(path).unwrap() = value;
            assert!(
                GenerationBrowser::default()
                    .accept(&json!({"operation":"skill.generation.status"}), response)
                    .is_err()
            );
        }
        let mut browser = editor();
        let request = browser.prepare_request().unwrap();
        browser.begin(&request);
        let mut foreign = target();
        foreign["source_identity"] = json!("d".repeat(64));
        assert!(
            browser
                .accept(&request, json!({"target":foreign,"base":head()}))
                .is_err()
        );
        select(&mut browser, "smj_one");
        let query = json!({"operation":"skill.generation.get","job_id":"smj_one"});
        let mut newer = job("smj_one");
        newer["state_version"] = json!(2);
        newer["state"] = json!("running");
        browser.accept(&query, newer).unwrap();
        assert!(browser.accept(&query, job("smj_one")).is_err());
        assert!(browser.accept(&query, job("smj_foreign")).is_err());
        assert_eq!(browser.selected.as_ref().unwrap().state_version, 2);
        let mut completed = job("smj_one");
        completed["state_version"] = json!(3);
        completed["state"] = json!("completed");
        completed["phase"] = json!("review_complete");
        browser.accept(&query, completed).unwrap();
        assert!(!browser.selected.as_ref().unwrap().cancellable());
    }

    #[test]
    fn deerflow_generation_lists_replace_pages_and_bind_filters() {
        let mut browser = editor();
        let first = json!({"operation":"skill.generation.list","source_id":"custom","name":"research","limit":PAGE_SIZE});
        browser
            .accept(
                &first,
                json!({"jobs":[job("smj_one")],"next_cursor":"next"}),
            )
            .unwrap();
        let mut next = first.clone();
        next["cursor"] = json!("next");
        browser
            .accept(&next, json!({"jobs":[job("smj_two")]}))
            .unwrap();
        assert_eq!(browser.jobs.len(), 1);
        assert_eq!(browser.jobs[0].id, "smj_two");
        let mut foreign = job("smj_other");
        foreign["target"]["name"] = json!("other");
        assert!(browser.accept(&first, json!({"jobs":[foreign]})).is_err());
        let too_many: Vec<_> = (0..=PAGE_SIZE).map(|i| job(&format!("smj_{i}"))).collect();
        assert!(browser.accept(&first, json!({"jobs":too_many})).is_err());
        assert_eq!(browser.jobs[0].id, "smj_two");
    }

    #[test]
    fn deerflow_generation_receipts_page_by_sequence_and_never_cross_jobs() {
        let mut browser = editor();
        select(&mut browser, "smj_one");
        let first =
            json!({"operation":"skill.generation.receipts","job_id":"smj_one","limit":PAGE_SIZE});
        let rows: Vec<_> = (1..=PAGE_SIZE as u64)
            .map(|seq| receipt(seq, "smj_one"))
            .collect();
        browser
            .accept(&first, json!({"receipts":rows,"last_sequence":20}))
            .unwrap();
        assert_eq!(browser.receipt_next, Some(20));
        let next = json!({"operation":"skill.generation.receipts","job_id":"smj_one","after_sequence":20,"limit":PAGE_SIZE});
        browser
            .accept(
                &next,
                json!({"receipts":[receipt(21,"smj_one")],"last_sequence":21}),
            )
            .unwrap();
        assert_eq!(browser.receipts.len(), 1);
        assert_eq!(browser.receipt_previous, vec![0]);
        assert!(
            browser
                .accept(
                    &first,
                    json!({"receipts":[receipt(1,"smj_other")],"last_sequence":1})
                )
                .is_err()
        );
        assert!(browser.accept(&first,json!({"receipts":[receipt(1,"smj_one"),receipt(1,"smj_one")],"last_sequence":1})).is_err());
        assert!(browser.accept(&first, json!({"last_sequence":0})).is_err());
        assert_eq!(browser.receipts[0].sequence, 21);
        browser
            .accept(&first, json!({"receipts":null,"last_sequence":0}))
            .unwrap();
        assert!(browser.receipts.is_empty());
        assert!(browser.receipt_previous.is_empty());
        browser.invalidate();
        assert!(
            browser
                .accept(&first, json!({"receipts":[],"last_sequence":0}))
                .is_err()
        );
    }
}
