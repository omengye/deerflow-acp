//! Bounded learning projections and session-bound action authority, without GPUI or I/O.
use super::skills_contract::{Head, PageNav, Target, digest};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::collections::HashSet;

#[path = "learning_source_state.rs"]
mod source_state;
use source_state::{SourceDetail, SourceSelection};

pub(super) const PAGE_SIZE: usize = 20;

pub(super) fn is_operation(request: &Value) -> bool {
    request["operation"]
        .as_str()
        .is_some_and(|op| op.starts_with("skill.learning."))
}

pub(super) fn session_bound(request: &Value) -> bool {
    is_operation(request) && request["operation"] != "skill.learning.status"
}

pub(super) fn mutation(request: &Value) -> bool {
    matches!(
        request["operation"].as_str(),
        Some(
            "skill.learning.control.set"
                | "skill.learning.episode.start"
                | "skill.learning.cancel"
                | "skill.learning.pattern.ignore"
                | "skill.learning.pattern.adjudicate"
                | "skill.learning.creation_retry.enqueue"
        )
    )
}

pub(super) fn review_mutation(request: &Value) -> bool {
    matches!(
        request["operation"].as_str(),
        Some(
            "skill.learning.pattern.ignore"
                | "skill.learning.pattern.adjudicate"
                | "skill.learning.creation_retry.enqueue"
        )
    )
}

pub(super) fn sources_changed(request: &Value) -> bool {
    request["operation"] == "skill.learning.control.set" || review_mutation(request)
}

fn identity(value: &str) -> bool {
    !value.is_empty() && value.len() <= 256 && !value.contains('\0')
}

fn text(value: &str, max: usize, required: bool) -> bool {
    value.len() <= max && !value.contains('\0') && (!required || !value.trim().is_empty())
}

fn named_digest(value: &str, prefix: &str) -> bool {
    value.strip_prefix(prefix).is_some_and(digest)
}

fn time(value: &str) -> bool {
    chrono::DateTime::parse_from_rfc3339(value).is_ok()
}

fn version(value: u64) -> bool {
    value > 0 && value < i64::MAX as u64
}

fn row_title(value: &str) -> String {
    let mut chars = value
        .chars()
        .map(|c| if c.is_whitespace() { ' ' } else { c });
    let mut result = chars.by_ref().take(80).collect::<String>();
    if chars.next().is_some() {
        result.push_str("...");
    }
    result
}

fn evidence_label(value: &str) -> Option<&'static str> {
    Some(match value {
        "observed_execution" => "已有本地执行回执（不等于质量通过）",
        "insufficient_evidence" => "独立任务证据不足",
        "unresolved" => "结果未解决",
        "ambiguous" => "归属有歧义",
        "conflicting" => "存在冲突",
        _ => return None,
    })
}

#[derive(Clone, Copy, Default, Debug, PartialEq, Eq)]
pub(super) enum Pane {
    #[default]
    Experiences,
    Patterns,
    Matches,
    Jobs,
}

impl Pane {
    pub(super) fn label(self) -> &'static str {
        match self {
            Self::Experiences => "过程经验",
            Self::Patterns => "归并结果",
            Self::Matches => "技能匹配",
            Self::Jobs => "学习作业",
        }
    }

    fn suffix(self) -> &'static str {
        match self {
            Self::Experiences => "experiences",
            Self::Patterns => "patterns",
            Self::Matches => "matches",
            Self::Jobs => "jobs",
        }
    }

    pub(super) fn supports_workspace(self) -> bool {
        matches!(self, Self::Patterns | Self::Matches)
    }
}

#[derive(Clone, Copy, Default, Debug, PartialEq, Eq)]
pub(super) enum Scope {
    #[default]
    Session,
    Workspace,
}

#[derive(Deserialize)]
pub(super) struct Status {
    version: String,
    pub(super) enabled: bool,
    mode: String,
    idle_seconds: u64,
    pending_inputs: u64,
    experiences: u64,
    pub(super) consolidation_enabled: bool,
    patterns: u64,
    pub(super) matching_enabled: bool,
    pub(super) cross_session_enabled: bool,
    matches: u64,
    proposal_discovery_enabled: bool,
    #[serde(default)]
    auto_patch_enabled: bool,
}

impl Status {
    fn validate(&self) -> anyhow::Result<()> {
        anyhow::ensure!(
            self.version == "procedural-experience-v1"
                && matches!(
                    self.mode.as_str(),
                    "shadow" | "review_candidates" | "guarded_auto_patch"
                )
                && (self.mode != "shadow") == self.proposal_discovery_enabled
                && (self.mode == "guarded_auto_patch") == self.auto_patch_enabled
                && (!self.proposal_discovery_enabled || self.matching_enabled)
                && (!self.enabled || self.idle_seconds > 0)
                && (!self.consolidation_enabled || self.enabled)
                && (!self.matching_enabled || self.consolidation_enabled)
                && (!self.cross_session_enabled || self.consolidation_enabled)
                && [
                    self.idle_seconds,
                    self.pending_inputs,
                    self.experiences,
                    self.patterns,
                    self.matches
                ]
                .iter()
                .all(|n| *n <= i64::MAX as u64),
            "学习状态版本或阶段关系无效"
        );
        Ok(())
    }

    fn summary(&self) -> String {
        let on = |enabled| if enabled { "已启用" } else { "关闭" };
        format!(
            "提取 {} · 归并 {} · 匹配 {} · 跨会话 {}\n模式 {} · 空闲等待 {} 秒\n此数据目录合计：待处理 {} · 经验 {} · 归并 {} · 匹配 {}\n排查顺序：确认聊天贡献 → 等待空闲提取 → 归并与匹配 → 生成待审核草稿 → 人工发布。数量是全目录累计值，不代表当前聊天已完成；具体等待、失败和额度原因请查看学习作业。",
            on(self.enabled),
            on(self.consolidation_enabled),
            on(self.matching_enabled),
            on(self.cross_session_enabled),
            if self.auto_patch_enabled {
                "受限自动补丁；描述修改另需正负触发评测，新技能/其他元数据/未合格候选仍需人工"
            } else if self.proposal_discovery_enabled {
                "可生成待审核提案，仍需人工发布"
            } else {
                "仅积累经验，不生成技能"
            },
            self.idle_seconds,
            self.pending_inputs,
            self.experiences,
            self.patterns,
            self.matches
        )
    }
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub(super) struct Control {
    pub(super) session_id: String,
    pub(super) contribute: bool,
    pub(super) workspace_shared: bool,
    pub(super) consent_epoch: u64,
}

impl Control {
    fn validate(&self, session: &str) -> anyhow::Result<()> {
        anyhow::ensure!(
            self.session_id == session
                && identity(session)
                && self.consent_epoch < i64::MAX as u64
                && (!self.workspace_shared || self.contribute),
            "贡献设置的会话、授权版本或共享状态无效"
        );
        Ok(())
    }
}

#[derive(Deserialize)]
struct Recipe {
    kind: String,
    goal: String,
    applicability: String,
    #[serde(default)]
    problem: String,
    resolution: String,
    steps: Vec<String>,
    verification: String,
    outcome: String,
    references: Vec<String>,
}

impl Recipe {
    fn validate(&self) -> anyhow::Result<()> {
        anyhow::ensure!(
            matches!(
                self.kind.as_str(),
                "correction_chain" | "proven_recipe" | "capability_gap" | "trigger_mismatch"
            ) && text(&self.goal, 1000, true)
                && text(&self.applicability, 2000, true)
                && text(&self.problem, 2000, self.kind == "correction_chain")
                && text(&self.resolution, 4000, true)
                && text(&self.verification, 2000, true)
                && (1..=12).contains(&self.steps.len())
                && self.steps.iter().all(|s| text(s, 1000, true))
                && (1..=32).contains(&self.references.len())
                && self.references.iter().all(|r| text(r, 2048, true))
                && self.references.iter().collect::<HashSet<_>>().len() == self.references.len()
                && matches!(
                    self.outcome.as_str(),
                    "observed_execution" | "user_reported" | "unresolved"
                )
                && (self.kind != "proven_recipe" || self.outcome == "observed_execution"),
            "经验类型、内容边界或引用无效"
        );
        Ok(())
    }

    fn detail(&self) -> String {
        let kind = match self.kind.as_str() {
            "correction_chain" => "纠正链",
            "proven_recipe" => "执行配方",
            "capability_gap" => "能力缺口",
            _ => "触发不匹配",
        };
        let outcome = match self.outcome.as_str() {
            "observed_execution" => "存在宿主保存的本地完成回执；方法质量尚未评估",
            "user_reported" => "用户陈述，未证明执行成功",
            _ => "未解决，不能视为成功样本",
        };
        let steps = self
            .steps
            .iter()
            .enumerate()
            .map(|(i, step)| format!("{}. {step}", i + 1))
            .collect::<Vec<_>>()
            .join("\n");
        format!(
            "类型 {kind} · 模型提取 / 质量未评估\n目标\n{}\n适用条件\n{}\n问题\n{}\n方法\n{}\n步骤\n{steps}\n验证说明（模型文本）\n{}\n结果\n{outcome}\n引用 ID（不替代完整回执核验）\n{}",
            self.goal,
            self.applicability,
            self.problem,
            self.resolution,
            self.verification,
            self.references.join("\n")
        )
    }
}

#[derive(Deserialize)]
struct Experience {
    id: String,
    session_id: String,
    episode_id: String,
    revision: u64,
    origin: String,
    quality: String,
    experience: Recipe,
    created_at: String,
}

#[derive(Deserialize)]
struct Pattern {
    id: String,
    session_id: String,
    revision: u64,
    origin: String,
    quality: String,
    representative_id: String,
    experience: Recipe,
    experience_ids: Vec<String>,
    episode_ids: Vec<String>,
    support_episodes: usize,
    observed_episodes: usize,
    evidence_state: String,
    source_state: String,
    conflicts_with: Vec<String>,
    updated_at: String,
    #[serde(default)]
    review: Option<PatternReview>,
}

#[derive(Clone, Deserialize)]
pub(super) struct PatternReview {
    pattern_id: String,
    pattern_revision: u64,
    state_version: u64,
    ignored: bool,
    adjudication_revision: u64,
    adjudication_id: String,
}

impl PatternReview {
    fn validate(&self, id: &str) -> anyhow::Result<()> {
        anyhow::ensure!(
            self.pattern_id == id
                && named_digest(id, "lpat_")
                && version(self.pattern_revision)
                && self.state_version < i64::MAX as u64
                && self.adjudication_revision <= self.pattern_revision
                && (self.adjudication_revision == 0) == self.adjudication_id.is_empty()
                && (self.adjudication_id.is_empty()
                    || named_digest(&self.adjudication_id, "ladj_")),
            "学习建议控制版本或人工裁决身份无效"
        );
        Ok(())
    }

    fn detail(&self) -> String {
        format!(
            "建议控制版本 {} · {}\n人工裁决 {} · 对应修订 {}\n忽略不删除来源经验、不退款、不撤销已采用技能；恢复不会重试旧作业或重置消费记录。",
            self.state_version,
            if self.ignored {
                "已忽略后续建议"
            } else {
                "允许建议"
            },
            self.adjudication_id,
            self.adjudication_revision
        )
    }
}

#[derive(Deserialize, Serialize)]
struct SkillRef {
    #[serde(rename = "sourceId")]
    source_id: String,
    #[serde(rename = "sourceIdentity")]
    source_identity: String,
    name: String,
    version: String,
    hash: String,
}

impl SkillRef {
    fn validate(&self) -> bool {
        identity(&self.source_id)
            && identity(&self.name)
            && digest(&self.source_identity)
            && digest(&self.version)
            && self.hash == self.version
    }

    fn detail(&self) -> String {
        format!(
            "{} / {}\nsource {}\nversion {}",
            self.source_id, self.name, self.source_identity, self.version
        )
    }
}

#[derive(Deserialize)]
struct Match {
    id: String,
    session_id: String,
    origin: String,
    quality: String,
    pattern_id: String,
    pattern_revision: u64,
    relation: String,
    reason: String,
    #[serde(default)]
    suggested_name: String,
    target: Option<Target>,
    head: Option<Head>,
    overlay_base: Option<SkillRef>,
    actual_reads: Vec<SkillRef>,
    evidence_state: String,
    source_state: String,
    created_at: String,
    #[serde(default)]
    review: Option<PatternReview>,
}

#[derive(Deserialize)]
struct Job {
    id: String,
    episode_id: String,
    #[serde(default)]
    pattern_id: String,
    #[serde(default)]
    pattern_revision: u64,
    kind: String,
    state: String,
    state_version: u64,
    attempts: u64,
    #[serde(default)]
    error_code: String,
    #[serde(default)]
    deadline: String,
}

pub(super) struct Row {
    pub(super) id: String,
    pub(super) title: String,
    pub(super) detail: String,
    pub(super) cancel: Option<Value>,
    review: Option<PatternReview>,
    adjudication_allowed: bool,
    source: Option<SourceSelection>,
}

impl Experience {
    fn row(self, session: &str) -> anyhow::Result<Row> {
        self.experience.validate()?;
        anyhow::ensure!(
            named_digest(&self.id, "lexp_")
                && self.session_id == session
                && identity(&self.episode_id)
                && self.episode_id.starts_with("lep_")
                && self.revision == 1
                && self.origin == "model_extracted"
                && self.quality == "unassessed"
                && time(&self.created_at),
            "过程经验的会话、身份或归因无效"
        );
        Ok(Row {
            title: row_title(&self.experience.goal),
            detail: format!(
                "经验 {} · 修订 {}\n会话 {}\n任务边界 {}\n记录时间 {}\n{}",
                self.id,
                self.revision,
                self.session_id,
                self.episode_id,
                self.created_at,
                self.experience.detail()
            ),
            source: Some(SourceSelection {
                kind: "experience",
                id: self.id.clone(),
                revision: self.revision,
                members: vec![self.id.clone()],
            }),
            id: self.id,
            cancel: None,
            review: None,
            adjudication_allowed: false,
        })
    }
}

impl Pattern {
    fn row(self, session: &str, shared: bool) -> anyhow::Result<Row> {
        self.experience.validate()?;
        anyhow::ensure!(
            named_digest(&self.id, "lpat_")
                && identity(&self.session_id)
                && (shared || self.session_id == session)
                && version(self.revision)
                && self.origin == "model_extracted"
                && self.quality == "unassessed"
                && self.source_state == "not_revalidated"
                && time(&self.updated_at)
                && (1..=32).contains(&self.experience_ids.len())
                && self
                    .experience_ids
                    .iter()
                    .all(|id| named_digest(id, "lexp_"))
                && self.experience_ids.windows(2).all(|ids| ids[0] < ids[1])
                && self.experience_ids.contains(&self.representative_id)
                && !self.episode_ids.is_empty()
                && self.episode_ids.len() <= self.experience_ids.len()
                && self
                    .episode_ids
                    .iter()
                    .all(|id| identity(id) && id.starts_with("lep_"))
                && self.episode_ids.windows(2).all(|ids| ids[0] < ids[1])
                && self.support_episodes > 0
                && self.support_episodes <= self.episode_ids.len()
                && self.observed_episodes <= self.support_episodes
                && self.conflicts_with.len() <= 16
                && self
                    .conflicts_with
                    .iter()
                    .all(|id| named_digest(id, "lpat_") && *id != self.id)
                && self.conflicts_with.windows(2).all(|ids| ids[0] < ids[1])
                && evidence_label(&self.evidence_state).is_some()
                && (self.evidence_state != "observed_execution" || self.observed_episodes >= 2)
                && (self.evidence_state == "conflicting") == !self.conflicts_with.is_empty(),
            "归并结果的成员、独立计数或来源状态无效"
        );
        if let Some(review) = &self.review {
            review.validate(&self.id)?;
            anyhow::ensure!(
                review.pattern_revision == self.revision,
                "建议控制与归并当前修订不一致"
            );
        }
        let mut row = Row {
            title: row_title(&format!(
                "{} · {} 个独立任务 · {}",
                self.experience.goal,
                self.support_episodes,
                evidence_label(&self.evidence_state).unwrap()
            )),
            detail: format!(
                "归并 {} · 修订 {}\n代表来源会话 {}\n更新于 {}\n独立支持 {} · 有本地回执 {}\n来源状态：历史，未重新核验\n代表经验 {}\n成员经验\n{}\n任务边界\n{}\n冲突对象\n{}\n{}",
                self.id,
                self.revision,
                self.session_id,
                self.updated_at,
                self.support_episodes,
                self.observed_episodes,
                self.representative_id,
                self.experience_ids.join("\n"),
                self.episode_ids.join("\n"),
                self.conflicts_with.join("\n"),
                self.experience.detail()
            ),
            source: Some(SourceSelection {
                kind: "pattern",
                id: self.id.clone(),
                revision: self.revision,
                members: self.experience_ids,
            }),
            id: self.id,
            cancel: None,
            review: self.review,
            adjudication_allowed: matches!(
                self.evidence_state.as_str(),
                "observed_execution" | "insufficient_evidence"
            ),
        };
        if let Some(review) = &row.review {
            row.detail.push_str(&format!("\n\n{}", review.detail()));
        }
        Ok(row)
    }
}

fn capability_name(name: &str) -> bool {
    if name.is_empty() || name.len() > 56 {
        return false;
    }
    let parts = name.split('-').collect::<Vec<_>>();
    parts.len() >= 2
        && parts.iter().all(|part| {
            !part.is_empty()
                && part
                    .bytes()
                    .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit())
                && part.bytes().any(|c| c.is_ascii_lowercase())
                && !(part.len() >= 12
                    && part
                        .bytes()
                        .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c)))
        })
}

impl Match {
    fn row(self, session: &str, shared: bool) -> anyhow::Result<Row> {
        let (caption, targeted) = match self.relation.as_str() {
            "body_gap" => ("已有技能正文缺口", true),
            "trigger_mismatch" => ("已有技能触发不匹配", true),
            "already_covered" => ("已有技能已覆盖", true),
            "new_capability" => ("建议新能力（仅分类）", false),
            "ambiguous" => ("归属未定", false),
            "irrelevant" => ("无相关技能", false),
            _ => anyhow::bail!("未知技能匹配关系"),
        };
        anyhow::ensure!(
            (named_digest(&self.id, "lmatch_") && self.origin == "model_matched"
                || named_digest(&self.id, "ladj_")
                    && self.origin == "host_adjudicated"
                    && targeted)
                && identity(&self.session_id)
                && (shared || self.session_id == session)
                && named_digest(&self.pattern_id, "lpat_")
                && version(self.pattern_revision)
                && self.quality == "unassessed"
                && text(&self.reason, 2000, true)
                && self.source_state == "not_revalidated"
                && evidence_label(&self.evidence_state).is_some()
                && time(&self.created_at)
                && self.target.is_some() == targeted
                && self.head.is_some() == targeted
                && (self.suggested_name.is_empty()
                    || self.relation == "new_capability" && capability_name(&self.suggested_name))
                && self.actual_reads.len() <= 32
                && self.actual_reads.iter().all(SkillRef::validate)
                && (targeted || self.actual_reads.is_empty() && self.overlay_base.is_none()),
            "技能匹配身份、目标或历史归因无效"
        );
        let mut target_text = "无固定目标；名称建议不授予来源、创建或发布权限".to_owned();
        if let (Some(target), Some(head)) = (&self.target, &self.head) {
            anyhow::ensure!(
                identity(&target.source_id)
                    && identity(&target.name)
                    && digest(&target.source_identity)
                    && matches!(target.scope.as_str(), "workspace" | "global")
                    && (target.scope != "workspace" || text(&target.workspace, 32768, true))
                    && head.exists
                    && head.enabled
                    && digest(&head.version)
                    && version(head.state_version)
                    && self
                        .actual_reads
                        .iter()
                        .all(|r| r.source_id == target.source_id
                            && r.source_identity == target.source_identity
                            && r.name == target.name),
                "匹配目标、固定 head 或实际读取归属无效"
            );
            target_text = format!(
                "{} / {} · {}\nsource {}\nworkspace {}\n固定 head {} · 状态版本 {} · {}",
                target.source_id,
                target.name,
                target.scope,
                target.source_identity,
                target.workspace,
                head.version,
                head.state_version,
                if head.managed { "托管" } else { "非托管" }
            );
            if let Some(base) = &self.overlay_base {
                anyhow::ensure!(
                    base.validate()
                        && head.managed
                        && base.source_id == target.source_id
                        && base.source_identity == target.source_identity
                        && base.name == target.name,
                    "内置覆盖来源无效"
                );
                target_text.push_str(&format!("\n覆盖基础 {}", base.version));
            }
        }
        let reads = if self.actual_reads.is_empty() {
            "无实际读取记录，模型相关性推断不能补造调用归因".to_owned()
        } else {
            self.actual_reads
                .iter()
                .map(SkillRef::detail)
                .collect::<Vec<_>>()
                .join("\n\n")
        };
        if let Some(review) = &self.review {
            review.validate(&self.pattern_id)?;
            anyhow::ensure!(
                review.pattern_revision >= self.pattern_revision,
                "历史裁决修订大于当前来源"
            );
        }
        let mut row = Row {
            title: row_title(&format!(
                "{caption} · {}",
                self.target
                    .as_ref()
                    .map(|t| t.name.as_str())
                    .unwrap_or(&self.suggested_name)
            )),
            detail: format!(
                "匹配 {}\n代表来源会话 {}\n归并 {} · 修订 {}\n记录于 {}\n{} / 质量未评估\n证据 {}\n来源状态：历史，未重新核验\n裁决理由\n{}\n固定目标（裁决时）\n{target_text}\n名称建议 {}\n实际读取版本（可与固定 head 不同）\n{reads}",
                self.id,
                self.session_id,
                self.pattern_id,
                self.pattern_revision,
                self.created_at,
                if self.origin == "host_adjudicated" {
                    "人工目标裁决；不是模型判断或质量确认，候选需人工发布"
                } else {
                    "模型匹配"
                },
                evidence_label(&self.evidence_state).unwrap(),
                self.reason,
                self.suggested_name
            ),
            id: self.id,
            cancel: None,
            review: self.review,
            adjudication_allowed: false,
            source: None,
        };
        if let Some(review) = &row.review {
            row.detail.push_str(&format!("\n\n{}", review.detail()));
        }
        if row
            .review
            .as_ref()
            .is_some_and(|review| review.pattern_revision != self.pattern_revision)
        {
            row.review = None;
        }
        Ok(row)
    }
}

impl Job {
    fn row(self, session: &str) -> anyhow::Result<Row> {
        let kind = match self.kind.as_str() {
            "experience_extract" => "经验提取",
            "experience_consolidate" => "经验归并",
            "skill_match" => "技能匹配",
            _ => anyhow::bail!("未知学习作业类型"),
        };
        let state = match self.state.as_str() {
            "queued" => "等待执行",
            "retry" => "等待后台重试",
            "running" => "执行中",
            "completed" => "已完成",
            "failed" => "失败",
            "cancelled" => "已取消",
            _ => anyhow::bail!("未知学习作业状态"),
        };
        let matching = self.kind == "skill_match";
        anyhow::ensure!(
            identity(&self.id)
                && self.id.starts_with("smj_")
                && version(self.state_version)
                && self.attempts <= i64::MAX as u64
                && text(&self.error_code, 128, false)
                && (self.deadline.is_empty() || time(&self.deadline))
                && if matching {
                    self.episode_id.is_empty()
                        && named_digest(&self.pattern_id, "lpat_")
                        && version(self.pattern_revision)
                } else {
                    identity(&self.episode_id)
                        && self.episode_id.starts_with("lep_")
                        && self.pattern_id.is_empty()
                        && self.pattern_revision == 0
                },
            "学习作业身份、版本或任务归属无效"
        );
        let cancel = matches!(self.state.as_str(), "queued" | "retry" | "running").then(|| json!({
            "operation":"skill.learning.cancel", "session_id":session, "job_id":self.id, "expected_state_version":self.state_version
        }));
        Ok(Row {
            title: format!("{kind} · {state} · {}", self.id),
            detail: format!(
                "作业 {}\n类型 {kind} · {state} · 状态版本 {}\n尝试 {} 次\n任务边界 {}\n归并 {} · 修订 {}\n错误代码 {}\n期限 {}\n匹配作业归属于归并修订，不伪称单一任务。取消不退款，也不删除已保存历史。",
                self.id,
                self.state_version,
                self.attempts,
                self.episode_id,
                self.pattern_id,
                self.pattern_revision,
                self.error_code,
                self.deadline
            ),
            id: self.id,
            cancel,
            review: None,
            adjudication_allowed: false,
            source: None,
        })
    }
}

#[derive(Default)]
pub(super) struct LearningBrowser {
    pub(super) session_id: Option<String>,
    pub(super) status: Option<Status>,
    pub(super) status_summary: Option<String>,
    pub(super) control: Option<Control>,
    pub(super) pane: Pane,
    pub(super) scope: Scope,
    pub(super) rows: Vec<Row>,
    pub(super) loaded: bool,
    pub(super) selected: Option<String>,
    pub(super) nav: PageNav,
    pub(super) last_result: Option<String>,
    pub(super) reconciliation: Option<String>,
    selection_generation: u64,
    requested_generation: Option<u64>,
    pending: Option<Value>,
    pending_generation: Option<u64>,
    creation_retry: Option<Value>,
    source_detail: Option<(Value, String)>,
    source_requested: Option<Value>,
}

impl LearningBrowser {
    fn clear_page(&mut self) {
        self.rows.clear();
        self.nav = PageNav::default();
        self.loaded = false;
        self.selected = None;
        self.requested_generation = None;
        self.creation_retry = None;
        self.source_detail = None;
        self.source_requested = None;
        self.selection_generation += 1;
    }

    pub(super) fn invalidate(&mut self) {
        self.status = None;
        self.status_summary = None;
        self.control = None;
        self.scope = Scope::Session;
        self.last_result = None;
        self.reconciliation = None;
        self.clear_page();
        // Preserve sent action identity even when its session or service is no
        // longer selected. A late receipt cannot authorize the new selection.
    }

    pub(super) fn set_session(&mut self, session: Option<&str>) {
        let session = session.filter(|s| identity(s));
        if self.session_id.as_deref() != session {
            self.invalidate();
            self.session_id = session.map(str::to_owned);
        }
    }

    pub(super) fn workspace_available(&self) -> bool {
        self.status
            .as_ref()
            .is_some_and(|s| s.cross_session_enabled)
            && self.control.as_ref().is_some_and(|c| {
                c.contribute
                    && c.workspace_shared
                    && self.session_id.as_deref() == Some(&c.session_id)
            })
    }

    pub(super) fn choose_pane(&mut self, pane: Pane) {
        if self.pane != pane {
            self.pane = pane;
            if !pane.supports_workspace() {
                self.scope = Scope::Session;
            }
            self.clear_page();
        }
    }

    pub(super) fn choose_scope(&mut self, scope: Scope) -> bool {
        if scope == Scope::Workspace
            && (!self.pane.supports_workspace() || !self.workspace_available())
        {
            return false;
        }
        if self.scope != scope {
            self.scope = scope;
            self.clear_page();
        }
        true
    }

    pub(super) fn page_request(&self) -> Option<Value> {
        let session = self.session_id.as_ref()?;
        let scope = if self.scope == Scope::Workspace {
            if !self.pane.supports_workspace() || !self.workspace_available() {
                return None;
            }
            "workspace."
        } else {
            ""
        };
        Some(
            json!({"operation":format!("skill.learning.{scope}{}", self.pane.suffix()), "session_id":session, "limit":PAGE_SIZE}),
        )
    }

    pub(super) fn select_row(&mut self, id: &str) {
        if self.selected.as_deref() != Some(id) && self.rows.iter().any(|row| row.id == id) {
            self.selected = Some(id.to_owned());
            self.source_detail = None;
            self.source_requested = None;
            self.creation_retry = None;
            self.selection_generation += 1;
        }
    }

    pub(super) fn source_members(&self) -> &[String] {
        if !self.loaded || self.scope == Scope::Workspace && !self.workspace_available() {
            return &[];
        }
        self.rows
            .iter()
            .find(|row| self.selected.as_ref() == Some(&row.id))
            .and_then(|row| row.source.as_ref())
            .map_or(&[], |source| source.members.as_slice())
    }

    pub(super) fn source_request(&self, member: &str) -> Option<Value> {
        if self.pending.is_some() || !self.source_members().iter().any(|id| id == member) {
            return None;
        }
        let source = self
            .rows
            .iter()
            .find(|row| self.selected.as_ref() == Some(&row.id))?
            .source
            .as_ref()?;
        Some(
            json!({"operation":"skill.learning.source.get", "session_id":self.session_id.as_ref()?,
            "source":{"kind":source.kind, "id":source.id, "expected_revision":source.revision,
                "scope":if self.scope == Scope::Workspace { "workspace" } else { "session" }, "experience_id":member}}),
        )
    }

    pub(super) fn source_detail(&self) -> Option<&str> {
        let (request, detail) = self.source_detail.as_ref()?;
        let member = request["source"]["experience_id"].as_str()?;
        (self.source_request(member).as_ref() == Some(request)).then_some(detail.as_str())
    }

    pub(super) fn source_selected(&self, member: &str) -> bool {
        self.source_detail
            .as_ref()
            .is_some_and(|(request, _)| request["source"]["experience_id"] == member)
    }

    pub(super) fn control_request(&self, contribute: bool, shared: bool) -> Option<Value> {
        let control = self.control.as_ref()?;
        if self.pending.is_some()
            || self.session_id.as_deref() != Some(&control.session_id)
            || control.consent_epoch >= i64::MAX as u64 - 1
            || shared && !contribute
            || contribute && !control.contribute && !self.status.as_ref().is_some_and(|s| s.enabled)
            || shared
                && !self
                    .status
                    .as_ref()
                    .is_some_and(|s| s.cross_session_enabled)
            || contribute == control.contribute && shared == control.workspace_shared
        {
            return None;
        }
        Some(
            json!({"operation":"skill.learning.control.set", "session_id":control.session_id,
            "control":{"contribute":contribute, "workspace_shared":shared, "expected_epoch":control.consent_epoch}}),
        )
    }

    pub(super) fn episode_request(&self) -> Option<Value> {
        let control = self.control.as_ref()?;
        if self.pending.is_some()
            || !control.contribute
            || self.session_id.as_deref() != Some(&control.session_id)
            || !self.status.as_ref().is_some_and(|s| s.enabled)
        {
            return None;
        }
        Some(json!({"operation":"skill.learning.episode.start", "session_id":control.session_id}))
    }

    fn selected_review(&self) -> Option<(&Row, &PatternReview)> {
        if self.pending.is_some()
            || !self.loaded
            || !matches!(self.pane, Pane::Patterns | Pane::Matches)
            || self.scope == Scope::Workspace && !self.workspace_available()
        {
            return None;
        }
        let row = self
            .rows
            .iter()
            .find(|row| self.selected.as_ref() == Some(&row.id))?;
        let review = row.review.as_ref()?;
        (review.state_version < i64::MAX as u64 - 1).then_some((row, review))
    }

    fn review_fields(&self) -> Option<Value> {
        let (_, review) = self.selected_review()?;
        Some(
            json!({"pattern_id":review.pattern_id, "expected_pattern_revision":review.pattern_revision,
            "expected_state_version":review.state_version, "scope":if self.scope == Scope::Workspace { "workspace" } else { "session" }}),
        )
    }

    pub(super) fn creation_retry_prepare_request(&self, previous: &str) -> Option<Value> {
        let (row, review) = self.selected_review()?;
        if self.pane != Pane::Patterns
            || !row.adjudication_allowed
            || review.ignored
            || !self
                .status
                .as_ref()
                .is_some_and(|s| s.matching_enabled && s.proposal_discovery_enabled)
            || !identity(previous)
            || !previous.starts_with("smj_")
        {
            return None;
        }
        let mut fields = self.review_fields()?;
        fields["previous_generation_id"] = json!(previous);
        Some(
            json!({"operation":"skill.learning.creation_retry.prepare", "session_id":self.session_id.as_ref()?, "retry":fields}),
        )
    }

    fn creation_retry_request_with_key(&self, key: &str) -> Option<Value> {
        let prepared = self.creation_retry.as_ref()?;
        let mut fields = prepared["request"].clone();
        let previous = fields["previous_generation_id"].as_str()?;
        let observation = self.creation_retry_prepare_request(previous)?;
        for key in [
            "pattern_id",
            "expected_pattern_revision",
            "expected_state_version",
            "scope",
            "previous_generation_id",
        ] {
            if observation["retry"][key] != fields[key] {
                return None;
            }
        }
        fields["request_key"] = json!(key);
        Some(
            json!({"operation":"skill.learning.creation_retry.enqueue", "session_id":self.session_id.as_ref()?, "retry":fields}),
        )
    }

    pub(super) fn creation_retry_request(&self) -> Option<Value> {
        self.creation_retry_request_with_key(&uuid::Uuid::new_v4().to_string())
    }

    pub(super) fn creation_retry_caption(&self) -> Option<String> {
        let prepared = self.creation_retry.as_ref()?;
        Some(format!(
            "原作业 {} · {}\n原提案 {} · {}\n目标 {} / {} · source {}\n人工重试 {} / {} · 原证据预留 {}\n新入队仍受 cooldown、提案数量和维护额度限制。",
            prepared["request"]["previous_generation_id"],
            prepared["previous_generation_state"],
            prepared["request"]["previous_proposal_id"],
            prepared["previous_proposal_status"],
            prepared["request"]["target"]["source_id"],
            prepared["request"]["target"]["name"],
            prepared["request"]["target"]["source_identity"],
            prepared["retry"]["retry_number"],
            prepared["max_retries"],
            prepared["retry"]["evidence_reservation_id"]
        ))
    }

    fn can_creation_retry(&self, request: &Value) -> bool {
        let Some(key) = request["retry"]["request_key"].as_str() else {
            return false;
        };
        if uuid::Uuid::parse_str(key).is_err() {
            return false;
        }
        self.creation_retry_request_with_key(key).as_ref() == Some(request)
    }

    pub(super) fn ignore_request(&self) -> Option<Value> {
        let (_, review) = self.selected_review()?;
        let mut fields = self.review_fields()?;
        fields["ignored"] = json!(!review.ignored);
        fields["request_key"] = json!(uuid::Uuid::new_v4().to_string());
        Some(
            json!({"operation":"skill.learning.pattern.ignore", "session_id":self.session_id.as_ref()?, "review":fields}),
        )
    }

    pub(super) fn adjudication_request(
        &self,
        target: Value,
        head: Value,
        overlay: Value,
        relation: &str,
    ) -> Option<Value> {
        let (row, review) = self.selected_review()?;
        if self.pane != Pane::Patterns
            || !row.adjudication_allowed
            || review.ignored
            || !self.status.as_ref().is_some_and(|s| s.matching_enabled)
            || !matches!(
                relation,
                "body_gap" | "trigger_mismatch" | "already_covered"
            )
        {
            return None;
        }
        let mut fields = self.review_fields()?;
        fields["request_key"] = json!(uuid::Uuid::new_v4().to_string());
        fields["target"] = target;
        fields["expected_head"] = head;
        fields["overlay_base"] = overlay;
        fields["relation"] = json!(relation);
        fields["reason"] = json!(
            "User explicitly selects this existing skill and relation in the learning review UI. Quality remains unassessed."
        );
        Some(
            json!({"operation":"skill.learning.pattern.adjudicate", "session_id":self.session_id.as_ref()?, "review":fields}),
        )
    }

    fn can_review(&self, request: &Value) -> bool {
        if request["session_id"].as_str() != self.session_id.as_deref() {
            return false;
        }
        let (Some(fields), Some((row, review))) = (self.review_fields(), self.selected_review())
        else {
            return false;
        };
        if [
            "pattern_id",
            "expected_pattern_revision",
            "expected_state_version",
            "scope",
        ]
        .iter()
        .any(|key| request["review"][key] != fields[key])
            || !request["review"]["request_key"]
                .as_str()
                .is_some_and(|k| text(k, 128, true))
        {
            return false;
        }
        if request["operation"] == "skill.learning.pattern.ignore" {
            return request["review"]["ignored"].as_bool() == Some(!review.ignored);
        }
        self.pane == Pane::Patterns
            && row.adjudication_allowed
            && !review.ignored
            && self.status.as_ref().is_some_and(|s| s.matching_enabled)
            && matches!(
                request["review"]["relation"].as_str(),
                Some("body_gap" | "trigger_mismatch" | "already_covered")
            )
            && request["review"]["target"].is_object()
            && request["review"]["expected_head"]["exists"] == true
            && request["review"]["expected_head"]["enabled"] == true
    }

    pub(super) fn can_begin(&self, request: &Value) -> bool {
        if !is_operation(request) {
            return true;
        }
        if mutation(request) {
            if self.pending.is_some() {
                return review_mutation(request) && self.pending.as_ref() == Some(request);
            }
            return match request["operation"].as_str() {
                Some("skill.learning.control.set") => {
                    let (Some(contribute), Some(shared)) = (
                        request["control"]["contribute"].as_bool(),
                        request["control"]["workspace_shared"].as_bool(),
                    ) else {
                        return false;
                    };
                    self.control_request(contribute, shared).as_ref() == Some(request)
                }
                Some("skill.learning.episode.start") => {
                    self.episode_request().as_ref() == Some(request)
                }
                Some("skill.learning.cancel") => {
                    self.loaded
                        && self.scope == Scope::Session
                        && self.pane == Pane::Jobs
                        && self
                            .rows
                            .iter()
                            .any(|row| row.cancel.as_ref() == Some(request))
                }
                Some("skill.learning.pattern.ignore" | "skill.learning.pattern.adjudicate") => {
                    self.can_review(request)
                }
                Some("skill.learning.creation_retry.enqueue") => self.can_creation_retry(request),
                _ => false,
            };
        }
        if self.is_reconciliation(request) {
            return true;
        }
        if request["operation"] == "skill.learning.status" {
            return true;
        }
        if request["session_id"].as_str() != self.session_id.as_deref() || self.session_id.is_none()
        {
            return false;
        }
        if request["operation"] == "skill.learning.control.get" {
            return true;
        }
        if request["operation"] == "skill.learning.creation_retry.prepare" {
            return request["retry"]["previous_generation_id"]
                .as_str()
                .and_then(|id| self.creation_retry_prepare_request(id))
                .as_ref()
                == Some(request);
        }
        if request["operation"] == "skill.learning.source.get" {
            return request["source"]["experience_id"]
                .as_str()
                .and_then(|id| self.source_request(id))
                .as_ref()
                == Some(request);
        }
        self.page_request().is_some_and(|page| {
            page["operation"] == request["operation"]
                && request["limit"].as_u64() == Some(PAGE_SIZE as u64)
        })
    }

    pub(super) fn begin(&mut self, request: &Value) {
        if is_operation(request) {
            if sources_changed(request)
                || self
                    .page_request()
                    .is_some_and(|page| page["operation"] == request["operation"])
            {
                self.source_detail = None;
                self.source_requested = None;
            }
            if request["operation"] == "skill.learning.creation_retry.prepare" {
                self.creation_retry = None;
            }
            if request["operation"] == "skill.learning.source.get" {
                self.source_detail = None;
                self.source_requested = Some(request.clone());
            }
            self.requested_generation = Some(self.selection_generation);
            if mutation(request) && self.pending.is_none() {
                self.pending = Some(request.clone());
                self.pending_generation = Some(self.selection_generation);
            }
        }
    }

    pub(super) fn definitive_failure(&mut self, request: &Value) {
        if self.pending.as_ref() == Some(request) {
            self.pending = None;
            self.pending_generation = None;
            self.reconciliation = None;
        }
    }

    pub(super) fn pending_request(&self) -> Option<&Value> {
        self.pending.as_ref()
    }

    pub(super) fn pending_sources_changed(&self) -> bool {
        self.pending.as_ref().is_some_and(sources_changed)
    }

    pub(super) fn reconciliation_request(&self) -> Option<Value> {
        let pending = self.pending.as_ref()?;
        let operation = match pending["operation"].as_str()? {
            "skill.learning.control.set" => "skill.learning.control.get",
            "skill.learning.cancel" => "skill.learning.jobs",
            // There is no current-boundary query or replay receipt. Never
            // claim that a different read proves episode.start did not run.
            _ => return None,
        };
        let mut request = json!({"operation":operation, "session_id":pending["session_id"]});
        if operation == "skill.learning.jobs" {
            request["limit"] = json!(PAGE_SIZE);
        }
        Some(request)
    }

    pub(super) fn is_reconciliation(&self, request: &Value) -> bool {
        self.reconciliation_request().as_ref() == Some(request)
    }

    fn current_response(&self, request: &Value) -> bool {
        self.requested_generation == Some(self.selection_generation)
            && (!session_bound(request)
                || request["session_id"].as_str() == self.session_id.as_deref())
    }

    pub(super) fn accept(&mut self, request: &Value, data: Value) -> anyhow::Result<()> {
        if mutation(request) {
            return self.accept_mutation(request, data);
        }
        if self.is_reconciliation(request) {
            self.accept_reconciliation(request, &data)?;
            if request["operation"] == "skill.learning.jobs" {
                return Ok(());
            }
        }
        if !self.current_response(request) {
            return Ok(());
        }
        match request["operation"].as_str() {
            Some("skill.learning.source.get") => {
                if self.source_requested.as_ref() != Some(request) || !self.can_begin(request) {
                    return Ok(());
                }
                anyhow::ensure!(
                    serde_json::to_vec(&data)?.len() <= 512 << 10,
                    "学习来源响应超出边界"
                );
                let source: SourceDetail = serde_json::from_value(data)?;
                let detail = source.detail(request)?;
                self.source_detail = Some((request.clone(), detail));
                self.source_requested = None;
            }
            Some("skill.learning.status") => {
                let status: Status = serde_json::from_value(data)?;
                status.validate()?;
                self.status_summary = Some(status.summary());
                self.status = Some(status);
                if self.scope == Scope::Workspace && !self.workspace_available() {
                    self.scope = Scope::Session;
                    self.clear_page();
                }
            }
            Some("skill.learning.control.get") => {
                let session = self
                    .session_id
                    .as_deref()
                    .ok_or_else(|| anyhow::anyhow!("请选择原生会话"))?;
                let control: Control = serde_json::from_value(data)?;
                control.validate(session)?;
                if self.control.as_ref() != Some(&control) {
                    self.clear_page();
                }
                self.control = Some(control);
                if self.scope == Scope::Workspace && !self.workspace_available() {
                    self.scope = Scope::Session;
                    self.clear_page();
                }
            }
            Some("skill.learning.creation_retry.prepare") => {
                anyhow::ensure!(self.can_begin(request), "新技能重试观察已过期");
                let fixed = &data["request"];
                for key in [
                    "pattern_id",
                    "expected_pattern_revision",
                    "expected_state_version",
                    "scope",
                    "previous_generation_id",
                ] {
                    anyhow::ensure!(
                        fixed[key] == request["retry"][key],
                        "新技能重试观察与原条件不一致"
                    );
                }
                let target: Target = serde_json::from_value(fixed["target"].clone())?;
                let head: Head = serde_json::from_value(fixed["expected_head"].clone())?;
                let proposal = fixed["previous_proposal_id"]
                    .as_str()
                    .ok_or_else(|| anyhow::anyhow!("缺少原提案身份"))?;
                anyhow::ensure!(
                    identity(&target.source_id)
                        && digest(&target.source_identity)
                        && capability_name(&target.name)
                        && matches!(target.scope.as_str(), "global" | "workspace")
                        && (if target.scope == "workspace" {
                            !target.workspace.is_empty()
                                && target.workspace.len() <= 32768
                                && !target.workspace.contains('\0')
                        } else {
                            target.workspace.is_empty()
                        })
                        && head.version.is_empty()
                        && head.state_version == 0
                        && !head.exists
                        && !head.enabled
                        && !head.managed
                        && fixed["overlay_base"].is_null()
                        && fixed["request_key"] == ""
                        && fixed["expected_generation_state_version"]
                            .as_u64()
                            .is_some_and(version)
                        && fixed["expected_proposal_state_version"]
                            .as_u64()
                            .is_some_and(|v| v < i64::MAX as u64 && (v == 0) == proposal.is_empty())
                        && (proposal.is_empty() || identity(proposal))
                        && matches!(
                            data["previous_generation_state"].as_str(),
                            Some("failed" | "cancelled" | "completed")
                        )
                        && matches!(
                            data["previous_proposal_status"].as_str(),
                            Some("" | "failed" | "rejected")
                        )
                        && (proposal.is_empty() == (data["previous_proposal_status"] == ""))
                        && (data["previous_generation_state"] != "completed"
                            || !proposal.is_empty())
                        && data["retry"]["previous_generation_id"]
                            == fixed["previous_generation_id"]
                        && data["retry"]["evidence_reservation_id"]
                            == fixed["evidence_reservation_id"]
                        && data["retry"]["retry_number"] == fixed["expected_retry_number"]
                        && data["retry"]["evidence_reservation_id"]
                            .as_str()
                            .is_some_and(|id| identity(id) && id.starts_with("sd_"))
                        && data["retry"]["retry_number"]
                            .as_u64()
                            .is_some_and(|v| (1..=3).contains(&v))
                        && data["max_retries"] == 3,
                    "新技能重试条件、目标或上限无效"
                );
                self.creation_retry = Some(data);
            }
            _ => self.accept_page(request, data)?,
        }
        Ok(())
    }

    fn accept_reconciliation(&mut self, request: &Value, data: &Value) -> anyhow::Result<()> {
        let pending = self
            .pending
            .as_ref()
            .ok_or_else(|| anyhow::anyhow!("原学习请求已结束"))?;
        let session = pending["session_id"]
            .as_str()
            .ok_or_else(|| anyhow::anyhow!("原会话无效"))?;
        let summary = if request["operation"] == "skill.learning.control.get" {
            let control: Control = serde_json::from_value(data.clone())?;
            control.validate(session)?;
            format!(
                "原会话 {session}\n当前授权版本 {} · 贡献 {} · 工作区共享 {}\n原请求授权版本 {}\n这是当前状态，不是原操作的幂等回执；核对后再结束待确认状态。",
                control.consent_epoch,
                control.contribute,
                control.workspace_shared,
                pending["control"]["expected_epoch"]
            )
        } else {
            let values = data["jobs"]
                .as_array()
                .ok_or_else(|| anyhow::anyhow!("学习作业核对缺少条目"))?;
            anyhow::ensure!(values.len() <= PAGE_SIZE, "学习作业核对超出分页限制");
            let mut found = None;
            for value in values {
                let job: Job = serde_json::from_value(value.clone())?;
                let row = job.row(session)?;
                if value["id"] == pending["job_id"] {
                    found = Some(row.detail);
                }
            }
            format!(
                "原会话 {session}\n{}\n这是当前作业状态，不证明原取消操作被执行。第一页未找到也不代表作业不存在。",
                found.unwrap_or_else(|| {
                    "第一页未包含原作业；回到原会话翻页核对，不能据此重发取消请求。".into()
                })
            )
        };
        self.reconciliation = Some(summary);
        Ok(())
    }

    fn accept_mutation(&mut self, request: &Value, data: Value) -> anyhow::Result<()> {
        anyhow::ensure!(
            self.pending.as_ref() == Some(request),
            "学习操作响应与原请求不一致"
        );
        let current = self.current_response(request)
            && self.pending_generation == Some(self.selection_generation);
        let session = request["session_id"]
            .as_str()
            .ok_or_else(|| anyhow::anyhow!("学习操作缺少会话"))?;
        let (control, result) = match request["operation"].as_str() {
            Some("skill.learning.control.set") => {
                let control: Control = serde_json::from_value(data)?;
                control.validate(session)?;
                let expected = request["control"]["expected_epoch"]
                    .as_u64()
                    .ok_or_else(|| anyhow::anyhow!("缺少原授权版本"))?;
                anyhow::ensure!(
                    expected.checked_add(1) == Some(control.consent_epoch)
                        && request["control"]["contribute"] == control.contribute
                        && request["control"]["workspace_shared"] == control.workspace_shared,
                    "贡献操作响应与原授权条件不一致"
                );
                (Some(control), "贡献设置已确认。授权变更已清理旧的未发布学习来源及派生内容；费用、消费记录和已采用技能保持不变。".to_owned())
            }
            Some("skill.learning.episode.start") => {
                let episode = data["episode_id"]
                    .as_str()
                    .ok_or_else(|| anyhow::anyhow!("任务边界响应缺少 ID"))?;
                anyhow::ensure!(
                    identity(episode) && episode.starts_with("lep_"),
                    "任务边界 ID 无效"
                );
                (
                    None,
                    format!(
                        "新任务边界已确认 {episode}。只影响之后输入；不会回填聊天，也不代表新增成功证据。"
                    ),
                )
            }
            Some("skill.learning.cancel") => {
                anyhow::ensure!(data["cancelled"] == true, "取消操作未提供完成结果");
                (
                    None,
                    "学习作业已取消。已发生用量保留，不删除已保存经验或正式技能。".to_owned(),
                )
            }
            Some("skill.learning.pattern.ignore" | "skill.learning.pattern.adjudicate") => {
                let review: PatternReview = serde_json::from_value(data["review"].clone())?;
                let original = &request["review"];
                review.validate(
                    original["pattern_id"]
                        .as_str()
                        .ok_or_else(|| anyhow::anyhow!("缺少原归并身份"))?,
                )?;
                anyhow::ensure!(
                    original["expected_state_version"]
                        .as_u64()
                        .and_then(|v| v.checked_add(1))
                        == Some(review.state_version)
                        && original["expected_pattern_revision"].as_u64()
                            == Some(review.pattern_revision),
                    "人工控制回执与原版本不一致"
                );
                if request["operation"] == "skill.learning.pattern.ignore" {
                    anyhow::ensure!(
                        original["ignored"] == review.ignored && review.adjudication_id.is_empty(),
                        "忽略回执状态不一致"
                    );
                } else {
                    anyhow::ensure!(
                        !review.ignored
                            && data["match"]["id"] == review.adjudication_id
                            && data["match"]["origin"] == "host_adjudicated"
                            && data["match"]["quality"] == "unassessed"
                            && data["match"]["pattern_id"] == review.pattern_id
                            && data["match"]["pattern_revision"] == review.pattern_revision
                            && data["match"]["target"] == original["target"]
                            && data["match"]["head"] == original["expected_head"]
                            && data["match"]["overlay_base"] == original["overlay_base"]
                            && data["match"]["source_state"] == "checked_at_adjudication"
                            && review.adjudication_revision == review.pattern_revision
                            && data["match"]["relation"] == original["relation"],
                        "人工裁决回执与原目标不一致"
                    );
                }
                (
                    None,
                    format!(
                        "已确认原人工操作（控制版本 {}）。历史回执不代表当前状态；请刷新。来源经验、费用、消费记录和已采用技能保留。人工裁决候选仍需人工发布。",
                        review.state_version
                    ),
                )
            }
            Some("skill.learning.creation_retry.enqueue") => {
                let original = &request["retry"];
                let review: PatternReview = serde_json::from_value(data["review"].clone())?;
                review.validate(
                    original["pattern_id"]
                        .as_str()
                        .ok_or_else(|| anyhow::anyhow!("缺少原归并身份"))?,
                )?;
                anyhow::ensure!(
                    data["id"]
                        .as_str()
                        .is_some_and(|id| named_digest(id, "lretry_"))
                        && original["expected_state_version"]
                            .as_u64()
                            .and_then(|v| v.checked_add(1))
                            == Some(review.state_version)
                        && original["expected_pattern_revision"] == review.pattern_revision
                        && !review.ignored
                        && data["generation_id"]
                            .as_str()
                            .is_some_and(|id| identity(id) && id.starts_with("smj_"))
                        && data["generation_id"] != original["previous_generation_id"]
                        && data["discovery_id"]
                            .as_str()
                            .is_some_and(|id| identity(id) && id.starts_with("sd_"))
                        && data["discovery_id"] != original["evidence_reservation_id"]
                        && data["retry"]["previous_generation_id"]
                            == original["previous_generation_id"]
                        && data["retry"]["evidence_reservation_id"]
                            == original["evidence_reservation_id"]
                        && data["retry"]["retry_number"] == original["expected_retry_number"]
                        && data["retry"]["evidence_reservation_id"]
                            .as_str()
                            .is_some_and(|id| identity(id) && id.starts_with("sd_"))
                        && data["retry"]["retry_number"]
                            .as_u64()
                            .is_some_and(|v| (1..=3).contains(&v)),
                    "新技能重试回执与原条件不一致"
                );
                (
                    None,
                    format!(
                        "原人工重试已入队 {}。这是原请求结果，不代表当前作业或授权状态。原创建预留、费用和 episode 消费保留；此次复用原证据不新增票，新技能必须人工发布。",
                        data["generation_id"]
                    ),
                )
            }
            _ => anyhow::bail!("未知学习操作响应"),
        };
        self.pending = None;
        self.pending_generation = None;
        if current {
            self.clear_page();
            if let Some(control) = control {
                self.control = Some(control);
            }
            if self.scope == Scope::Workspace && !self.workspace_available() {
                self.scope = Scope::Session;
            }
            self.last_result = Some(result);
        }
        Ok(())
    }

    fn accept_page(&mut self, request: &Value, data: Value) -> anyhow::Result<()> {
        anyhow::ensure!(self.can_begin(request), "学习列表的会话或查询条件已变化");
        let session = self
            .session_id
            .as_deref()
            .ok_or_else(|| anyhow::anyhow!("请选择原生会话"))?;
        let shared = self.scope == Scope::Workspace;
        let values = data[self.pane.suffix()]
            .as_array()
            .ok_or_else(|| anyhow::anyhow!("学习列表缺少条目"))?;
        anyhow::ensure!(values.len() <= PAGE_SIZE, "学习列表超出分页限制");
        let mut rows = Vec::with_capacity(values.len());
        for value in values {
            let max_bytes = match self.pane {
                Pane::Experiences => 64 << 10,
                Pane::Patterns => 128 << 10,
                Pane::Matches => 32 << 10,
                Pane::Jobs => 4 << 10,
            };
            anyhow::ensure!(
                serde_json::to_vec(value)?.len() <= max_bytes,
                "学习条目超出内容限制"
            );
            rows.push(match self.pane {
                Pane::Experiences => {
                    serde_json::from_value::<Experience>(value.clone())?.row(session)?
                }
                Pane::Patterns => {
                    serde_json::from_value::<Pattern>(value.clone())?.row(session, shared)?
                }
                Pane::Matches => {
                    serde_json::from_value::<Match>(value.clone())?.row(session, shared)?
                }
                Pane::Jobs => serde_json::from_value::<Job>(value.clone())?.row(session)?,
            });
        }
        let cursor = request["cursor"].as_str().unwrap_or("");
        let next = data
            .get("next_cursor")
            .map(|v| {
                v.as_str()
                    .ok_or_else(|| anyhow::anyhow!("学习游标类型无效"))
            })
            .transpose()?
            .unwrap_or("");
        anyhow::ensure!(
            cursor.len() <= 128
                && next.len() <= 128
                && rows.windows(2).all(|rows| rows[0].id < rows[1].id)
                && rows.first().is_none_or(|row| row.id.as_str() > cursor)
                && (next.is_empty()
                    || rows.len() == PAGE_SIZE && rows.last().is_some_and(|row| row.id == next)),
            "学习列表顺序或分页边界无效"
        );
        self.nav.accept(request, next, "cursor")?;
        self.rows = rows;
        self.loaded = true;
        self.selected = None;
        self.source_detail = None;
        self.source_requested = None;
        self.creation_retry = None;
        Ok(())
    }
}

#[cfg(test)]
#[path = "learning_state_tests.rs"]
mod tests;
