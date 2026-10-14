//! Read-only, bounded projections of automatic publication and observation.
//! Host workers own publication, quality attribution and rollback decisions.
use super::skills::{Head, PAGE_SIZE, PageNav, Target, digest};
use super::*;
use serde::Deserialize;

#[derive(Deserialize)]
struct Options {
    #[serde(default)]
    experience_patterns_enabled: bool,
    max_changed_lines: u64,
    max_daily_publications: u64,
    observation_hours: u64,
    min_observations: u64,
    rollback_failure_threshold: u64,
}

impl Options {
    fn validate(&self) -> anyhow::Result<()> {
        anyhow::ensure!(
            (1..=200).contains(&self.max_changed_lines)
                && (1..=20).contains(&self.max_daily_publications)
                && (1..=720).contains(&self.observation_hours)
                && (1..=100).contains(&self.min_observations)
                && (1..=100).contains(&self.rollback_failure_threshold),
            "自动补丁观察参数无效"
        );
        Ok(())
    }

    fn summary(&self) -> String {
        format!(
            "最多修改 {} 行 · 每日发布 {} 次 · 观察 {} 小时 · 最少 {} 个有效样本 · 连续 {} 次质量失败回滚 · 学习来源自动补丁{}",
            self.max_changed_lines,
            self.max_daily_publications,
            self.observation_hours,
            self.min_observations,
            self.rollback_failure_threshold,
            if self.experience_patterns_enabled {
                "已授权"
            } else {
                "未授权"
            },
        )
    }
}

#[derive(Deserialize)]
struct Status {
    enabled: bool,
    running: bool,
    options: Option<Options>,
    today: String,
    published_today: u64,
    observing: u64,
    #[serde(default)]
    last_error_code: String,
    #[serde(default)]
    readiness: Option<Readiness>,
}

#[derive(Deserialize)]
struct Readiness {
    state: String,
    catalog_version: u64,
    policy_identity: String,
    installed: u64,
    eligible_targets: u64,
    missing_suite: u64,
    pending_review: u64,
    #[serde(default)]
    needs_attention: Option<u64>,
    #[serde(default)]
    waiting_candidates: Option<u64>,
    new_skills_require_review: bool,
    reasons: Vec<String>,
}

impl Readiness {
    fn summary(&self) -> anyhow::Result<String> {
        anyhow::ensure!(
            matches!(self.state.as_str(), "disabled" | "partial" | "ready")
                && digest(&self.policy_identity)
                && self.eligible_targets <= self.installed
                && self.missing_suite <= self.installed
                && self.pending_review <= i64::MAX as u64
                && self.needs_attention.is_some() == self.waiting_candidates.is_some()
                && self
                    .needs_attention
                    .zip(self.waiting_candidates)
                    .is_none_or(|(a, w)| a <= self.pending_review && w == self.pending_review - a)
                && self.reasons.len() <= 8,
            "维护就绪回执无效"
        );
        let state = match self.state.as_str() {
            "ready" => "已就绪",
            "partial" => "部分可用",
            _ => "自动采用未启用",
        };
        Ok(format!(
            "后台状态：{state} · 目录版本 {}\n{} / {} 个安装目标具备自动补丁基础条件 · {} 个受管目标缺少可信覆盖\n当前授权来源共有 {} 个待审候选。{}{}候选仍须通过本次证据、评测、预算及版本检查。",
            self.catalog_version,
            self.eligible_targets,
            self.installed,
            self.missing_suite,
            self.pending_review,
            self.needs_attention
                .zip(self.waiting_candidates)
                .map(|(a, w)| format!("需处理 {a} 个，后台检查或等待 {w} 个。 "))
                .unwrap_or_default(),
            if self.new_skills_require_review {
                "新技能仍需确认；"
            } else {
                ""
            }
        ))
    }
}

#[derive(Deserialize)]
struct Observation {
    proposal_id: String,
    target: Target,
    publication_revision: u64,
    baseline_revision: u64,
    head: Head,
    options: Options,
    status: String,
    started_at: String,
    deadline: String,
    updated_at: String,
    successes: u64,
    failures: u64,
    consecutive_failures: u64,
    ignored: u64,
    #[serde(default)]
    rollback_revision: u64,
    #[serde(default)]
    reason: String,
    #[serde(default)]
    learning_source: Option<LearningSource>,
}

#[derive(Deserialize)]
struct LearningSource {
    generation_id: String,
    discovery_id: String,
    discovery_sha256: String,
    pattern_id: String,
    pattern_revision: u64,
    match_id: String,
    policy_identity: String,
    evidence_sha256: String,
    source_state: String,
}

impl LearningSource {
    fn summary(&self, options: &Options) -> anyhow::Result<String> {
        anyhow::ensure!(
            options.experience_patterns_enabled
                && bounded(&self.generation_id, 128)
                && self.generation_id.starts_with("smj_")
                && bounded(&self.discovery_id, 128)
                && self.discovery_id.starts_with("sd_")
                && bounded(&self.pattern_id, 128)
                && bounded(&self.match_id, 128)
                && (1..=i64::MAX as u64).contains(&self.pattern_revision)
                && digest(&self.discovery_sha256)
                && digest(&self.policy_identity)
                && digest(&self.evidence_sha256),
            "自动补丁学习来源引用无效"
        );
        let state = match self.source_state.as_str() {
            "valid_at_publication" => "仅发布时已核验；不是当前来源授权",
            "withdrawn" => "来源已撤销；已采用版本保留",
            "unavailable" => "来源链接不可用；不作为质量判断",
            _ => anyhow::bail!("未知自动补丁学习来源状态"),
        };
        Ok(format!(
            "\n学习来源：{state}\n发现 {} · 生成 {}\n归并 {} 修订 {} · 匹配 {}\n来源证据摘要 {}\n观察仍仅计算操作者确认的质量反馈，不把经验摘要当质量票。",
            self.discovery_id,
            self.generation_id,
            self.pattern_id,
            self.pattern_revision,
            self.match_id,
            self.evidence_sha256,
        ))
    }
}

#[derive(Default)]
pub(super) struct AutoPatchBrowser {
    status: Option<(String, String)>,
    pub(super) readiness_label: Option<String>,
    pub(super) needs_attention: Option<u64>,
    rows: Vec<(String, String)>,
    loaded: bool,
    nav: PageNav,
}

fn bounded(value: &str, max: usize) -> bool {
    !value.is_empty() && value.len() <= max && !value.contains('\0')
}

impl AutoPatchBrowser {
    pub(super) fn invalidate(&mut self) {
        *self = Self::default();
    }

    pub(super) fn accept(&mut self, request: &Value, data: Value) -> anyhow::Result<()> {
        match request["operation"].as_str() {
            Some("skill.auto_patch.status") => {
                let status: Status = serde_json::from_value(data)?;
                anyhow::ensure!(
                    chrono::NaiveDate::parse_from_str(&status.today, "%Y-%m-%d").is_ok()
                        && status.published_today <= i64::MAX as u64
                        && status.observing <= i64::MAX as u64
                        && status.last_error_code.len() <= 128
                        && (!status.enabled || status.options.is_some()),
                    "自动补丁状态无效"
                );
                if let Some(options) = &status.options {
                    options.validate()?;
                }
                let readiness = status
                    .readiness
                    .as_ref()
                    .map(Readiness::summary)
                    .transpose()?;
                self.needs_attention = status.readiness.as_ref().and_then(|r| r.needs_attention);
                self.readiness_label = status.readiness.as_ref().map(|r| {
                    match r.state.as_str() {
                        "ready" => "自动维护 · 已就绪",
                        "partial" => "自动维护 · 部分可用",
                        _ => "自动采用未启用",
                    }
                    .to_owned()
                });
                self.status = Some((
                    format!(
                        "自动发布{} · 维护任务{}",
                        if status.enabled {
                            "已启用"
                        } else {
                            "未启用"
                        },
                        if status.running {
                            "运行中"
                        } else {
                            "未运行"
                        }
                    ),
                    format!(
                        "UTC {} 已发布 {} 次 · 观察记录 {} 项\n{}{}{}\n{}",
                        status.today,
                        status.published_today,
                        status.observing,
                        status
                            .options
                            .as_ref()
                            .map(Options::summary)
                            .unwrap_or_else(|| "当前未配置自动补丁参数".into()),
                        if !status.running && status.observing > 0 {
                            "\n当前后台未运行观察维护；历史记录保留，请检查服务状态。"
                        } else {
                            ""
                        },
                        if status.last_error_code.is_empty() {
                            String::new()
                        } else {
                            format!("\n最近错误：{}", status.last_error_code)
                        },
                        readiness.unwrap_or_else(|| {
                            "当前服务尚未提供目标覆盖回执；不能确认自动维护就绪。".into()
                        })
                    ),
                ));
            }
            Some("skill.auto_patch.list") => {
                #[derive(Deserialize)]
                struct Page {
                    observations: Vec<Observation>,
                    #[serde(default)]
                    next_cursor: String,
                }
                let page: Page = serde_json::from_value(data)?;
                anyhow::ensure!(page.observations.len() <= PAGE_SIZE, "观察记录超出分页限制");
                let mut rows = Vec::with_capacity(page.observations.len());
                let mut seen = std::collections::HashSet::new();
                for observation in page.observations {
                    observation.options.validate()?;
                    let learning = observation
                        .learning_source
                        .as_ref()
                        .map(|source| source.summary(&observation.options))
                        .transpose()?
                        .unwrap_or_default();
                    anyhow::ensure!(
                        bounded(&observation.proposal_id, 128)
                            && seen.insert(observation.proposal_id.clone())
                            && bounded(&observation.target.source_id, 64)
                            && bounded(&observation.target.name, 64)
                            && digest(&observation.target.source_identity)
                            && matches!(observation.target.scope.as_str(), "global" | "workspace")
                            && if observation.target.scope == "workspace" {
                                bounded(&observation.target.workspace, 32768)
                            } else {
                                observation.target.workspace.is_empty()
                            },
                        "观察记录的提案或来源无效"
                    );
                    anyhow::ensure!(
                        observation.publication_revision > observation.baseline_revision
                            && observation.baseline_revision > 0
                            && observation.head.exists
                            && observation.head.managed
                            && digest(&observation.head.version)
                            && observation.head.state_version > 0
                            && [
                                observation.publication_revision,
                                observation.head.state_version,
                                observation.rollback_revision,
                                observation.successes,
                                observation.failures,
                                observation.ignored
                            ]
                            .iter()
                            .all(|value| *value <= i64::MAX as u64)
                            && observation.consecutive_failures <= observation.failures
                            && observation.reason.len() <= 512
                            && [
                                &observation.started_at,
                                &observation.deadline,
                                &observation.updated_at
                            ]
                            .iter()
                            .all(|time| chrono::DateTime::parse_from_rfc3339(time).is_ok()),
                        "观察记录的版本、时间或计数无效"
                    );
                    let state = match observation.status.as_str() {
                        "observing" => "观察中",
                        "passed" => "观察通过",
                        "insufficient_evidence" => "观察结束，证据不足",
                        "rolled_back" => "已自动回滚",
                        "superseded" => "已被后续版本取代",
                        _ => anyhow::bail!("未知自动补丁观察状态"),
                    };
                    rows.push((
                        format!("{} / {} · {state}", observation.target.source_id, observation.target.name),
                        format!("提案 {} · 发布修订 {} · 基线修订 {}\n发布版本 {} · 状态版本 {}\n开始 {} · 截止 {} · 更新 {}\n有效成功 {} · 累计质量失败 {} · 当前连续失败 {} · 未计入 {}\n{}{}{}{}",
                            observation.proposal_id, observation.publication_revision, observation.baseline_revision,
                            observation.head.version, observation.head.state_version,
                            observation.started_at, observation.deadline, observation.updated_at,
                            observation.successes, observation.failures, observation.consecutive_failures, observation.ignored, observation.options.summary(),
                            if observation.rollback_revision > 0 { format!("\n回滚修订 {}", observation.rollback_revision) } else { String::new() },
                            if observation.reason.is_empty() { String::new() } else { format!("\n原因：{}", observation.reason) }, learning),
                    ));
                }
                self.nav.accept(request, &page.next_cursor, "cursor")?;
                self.rows = rows;
                self.loaded = true;
            }
            _ => anyhow::bail!("未知自动补丁查询"),
        }
        Ok(())
    }
}

#[cfg(test)]
#[path = "auto_patch_learning_tests.rs"]
mod learning_tests;

impl Waku {
    pub(super) fn render_deerflow_auto_patch(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let browser = &state.skill_browser.auto_patch;
        let supported = [
            "max_changed_lines",
            "max_daily_publications",
            "observation_hours",
            "min_observations",
            "rollback_failure_threshold",
        ]
        .iter()
        .all(|field| {
            state
                .settings_contract
                .allows(&format!("/skill_evolution/auto_patch/{field}"))
        });
        let unavailable =
            !supported || state.busy() || state.applying() || state.status["running"] != true;
        let mut group = df_group(theme).child(df_label("自动补丁发布与观察", "宿主自动执行审核、发布和回滚。观察只计算明确的技能质量判断，外部故障、取消和不确定副作用单独记录。", theme));
        if !supported {
            return group
                .child(df_label(
                    "当前服务未提供此能力",
                    "更新配套 Go 服务并重新加载配置后，可查看自动补丁状态。",
                    theme,
                ))
                .into_any_element();
        }
        let mut actions = div().flex().flex_wrap().gap(px(6.0));
        for (suffix, label, operation) in [
            ("status", "刷新自动补丁状态", "skill.auto_patch.status"),
            ("list", "刷新发布观察记录", "skill.auto_patch.list"),
        ] {
            actions = actions.child(df_button(
                SharedString::from(format!("df-auto-patch-{suffix}")),
                label,
                unavailable,
                false,
                theme,
                cx,
                move |this, _, cx| {
                    let mut request = json!({"operation":operation});
                    if suffix == "list" {
                        request["limit"] = json!(PAGE_SIZE);
                    }
                    this.deerflow_request("manage", request, cx);
                },
            ));
        }
        group = group.child(actions);
        if let Some((title, details)) = &browser.status {
            group = group.child(df_label(title.clone(), details.clone(), theme));
        }
        if browser.loaded && browser.rows.is_empty() {
            group = group.child(df_label(
                "暂无自动发布记录",
                "符合条件的补丁发布后将在此显示观察进度。",
                theme,
            ));
        }
        for (title, details) in &browser.rows {
            group = group.child(df_label(title.clone(), details.clone(), theme));
        }
        group
            .child(self.render_skill_page_nav("auto-patch", &browser.nav, unavailable, theme, cx))
            .into_any_element()
    }
}
