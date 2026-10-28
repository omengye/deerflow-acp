//! Bounded metadata for explaining learning progress; never contains transcript text.
use super::learning_state::Job;
use serde::Deserialize;
use serde_json::{Value, json};
use std::collections::HashSet;

pub(super) const LIMIT: usize = 50;

fn bounded(value: &str, max: usize, required: bool) -> bool {
    value.len() <= max && !value.chars().any(char::is_control) && (!required || !value.is_empty())
}

#[derive(Clone, Default, Deserialize)]
pub(super) struct Queue {
    pub(super) pending: u64,
    pub(super) queued: u64,
    pub(super) ready: u64,
    pub(super) blocked: u64,
    pub(super) skipped: u64,
    pub(super) failed: u64,
    pub(super) failed_jobs: u64,
    pub(super) capture_limited_sessions: u64,
    pub(super) reasons: Vec<Reason>,
}

#[derive(Clone, Deserialize)]
pub(super) struct Reason {
    pub(super) code: String,
    pub(super) count: u64,
}

impl Queue {
    pub(super) fn validate(&self) -> anyhow::Result<()> {
        let mut seen = HashSet::new();
        anyhow::ensure!(
            [
                self.pending,
                self.queued,
                self.ready,
                self.blocked,
                self.skipped,
                self.failed,
                self.failed_jobs,
                self.capture_limited_sessions
            ]
            .iter()
            .all(|n| *n <= i64::MAX as u64)
                && self.reasons.len() <= 64
                && self.reasons.iter().all(|r| bounded(&r.code, 128, true)
                    && r.count <= i64::MAX as u64
                    && seen.insert(&r.code)),
            "学习队列统计无效"
        );
        Ok(())
    }

    pub(super) fn summary(&self) -> String {
        format!(
            "输入：待处理 {} · 已入队 {} · 可调度 {} · 阻塞 {} · 跳过 {} · 失败 {}\n失败作业 {} 个 · 捕获达到上限的会话 {} 个。输入数与作业数分别计数。",
            self.pending,
            self.queued,
            self.ready,
            self.blocked,
            self.skipped,
            self.failed,
            self.failed_jobs,
            self.capture_limited_sessions
        )
    }

    pub(super) fn reasons_summary(&self) -> String {
        self.reasons
            .iter()
            .map(|r| format!("{} 条：{}", r.count, reason(&r.code)))
            .collect::<Vec<_>>()
            .join("\n")
    }
}

pub(super) fn reason(code: &str) -> String {
    let label = match code {
        "host_disabled" => "全局学习关闭；在自动化设置启用并保存应用",
        "contribution_disabled" => "此聊天未允许贡献；在学习详情检查贡献授权",
        "consent_changed" => "来源授权已变化；旧来源不可复用，仅捕获授权后的输入",
        "tree_unsettled" => "原任务或子任务尚未结算；打开原任务核对运行状态和待确认回执",
        "source_active" => "当前任务边界仍有活跃输入；等待完成，卡住时核对原任务",
        "waiting_input" => "原任务正在等待用户输入或审批；打开原任务查看需要补充的内容并继续",
        "needs_reconciliation" => {
            "原任务执行结果尚待核对；打开原任务和待确认回执，根据实际结果处理，不自动重放工具"
        }
        "idle_window" => "尚未达到空闲等待时间；停止追加输入后等待自动提取",
        "extraction_active" => "此来源已在提取中；等待作业完成后刷新",
        "extracting" => "经验提取正在执行；完成后可在过程经验中查看结果",
        "retry_backoff" => "暂态失败后正在等待后台重试；可在学习作业中核对错误和尝试次数",
        "awaiting_worker" => "已排队等待后台执行器；检查后台在线状态后等待调度",
        "awaiting_cleanup" => "作业已结束，正在等待后台整理输入状态；稍后刷新",
        "job_missing" => "输入指向的学习作业未找到；请记录作业编号排查服务数据",
        "other" => "还有其他服务原因；请查看此聊天诊断或学习作业明细",
        "queue_backpressure" => "后台队列已满；等待现有作业完成后刷新",
        "ready" => "已满足调度条件；提取前仍会校验来源完整性和大小",
        "unsafe_outcome" => "执行结果不适合学习；核对任务和回执，不以新目标绕过历史阻塞",
        "source_invalid" | "unsafe_root_evidence" => {
            "来源、工具回执或任务树证据校验失败；核对原任务记录，不能安全重放或补写证据"
        }
        "source_bounds" | "capture_limited" => {
            "来源达到捕获边界；本段无法完整提取，开始新目标不会回填旧输入"
        }
        "budget_exhausted" => "学习或共享维护额度不足；检查自动化设置中的额度，按 UTC 日重置",
        "source_unavailable" => "完整来源已不可用；无法复用这次提取快照",
        "attempts_exhausted" => "已达到重试次数上限；不能重置尝试次数",
        "deadline_expired" => "原作业期限已到；不能通过重试延长期限",
        "non_retryable_failure" => "此失败不可安全重试；保留记录供排查",
        "result_committed" => "已有结果提交；不可重复提取",
        "not_failed" => "作业并非失败状态；不需要手动重试",
        "unsupported_kind" => "仅失败的经验提取作业支持此重试入口",
        "cancelled" => "作业已取消；已发生的用量保留",
        "" => "服务未提供原因；请刷新状态核对",
        _ => return format!("服务原因 {code}；请按该代码排查"),
    };
    label.to_owned()
}

#[derive(Deserialize)]
pub(super) struct Input {
    pub(super) run_id: String,
    root_id: String,
    episode_id: String,
    pub(super) state: String,
    #[serde(default)]
    pub(super) reason: String,
    #[serde(default)]
    ended_at: String,
    #[serde(default)]
    job_id: String,
    #[serde(default)]
    run_status: String,
}

impl Input {
    pub(super) fn summary(&self) -> String {
        format!(
            "运行 {} · {} · 学习状态 {}\n{}\n任务树 {} · 任务边界 {} · 作业 {}",
            self.run_id,
            self.run_status,
            self.state,
            reason(&self.reason),
            self.root_id,
            self.episode_id,
            self.job_id
        )
    }
}

#[derive(Deserialize)]
pub(super) struct DiagnosticJob {
    #[serde(flatten)]
    pub(super) job: Job,
    pub(super) retryable: bool,
    pub(super) retry_reason: String,
}

#[derive(Deserialize)]
pub(super) struct Diagnostics {
    pub(super) session_id: String,
    pub(super) consent_epoch: u64,
    pub(super) contribute: bool,
    pub(super) running: bool,
    pub(super) queue: Queue,
    pub(super) inputs: Vec<Input>,
    pub(super) jobs: Vec<DiagnosticJob>,
    pub(super) truncated: bool,
}

impl Diagnostics {
    pub(super) fn read(data: Value, session: &str) -> anyhow::Result<Self> {
        anyhow::ensure!(
            serde_json::to_vec(&data)?.len() <= 256 << 10,
            "学习诊断响应过大"
        );
        let parsed: Self = serde_json::from_value(data)?;
        parsed.queue.validate()?;
        let mut runs = HashSet::new();
        let mut jobs = HashSet::new();
        anyhow::ensure!(
            parsed.session_id == session
                && bounded(session, 256, true)
                && parsed.consent_epoch < i64::MAX as u64
                && parsed.inputs.len() <= LIMIT
                && parsed.jobs.len() <= LIMIT
                && parsed.inputs.iter().all(|i| bounded(&i.run_id, 256, true)
                    && runs.insert(&i.run_id)
                    && bounded(&i.root_id, 256, false)
                    && bounded(&i.episode_id, 256, false)
                    && bounded(&i.job_id, 256, false)
                    && bounded(&i.reason, 128, false)
                    && bounded(&i.run_status, 64, false)
                    && matches!(
                        i.state.as_str(),
                        "pending"
                            | "queued"
                            | "processed"
                            | "skipped"
                            | "cancelled"
                            | "awaiting_completion"
                    )
                    && (i.ended_at.is_empty()
                        || chrono::DateTime::parse_from_rfc3339(&i.ended_at).is_ok())),
            "学习诊断的会话、授权或输入边界无效"
        );
        for item in &parsed.jobs {
            item.job.clone().row(session)?;
            anyhow::ensure!(
                jobs.insert(&item.job.id)
                    && bounded(&item.retry_reason, 128, false)
                    && (!item.retryable
                        || item.retry_reason.is_empty()
                            && item.job.kind == "experience_extract"
                            && item.job.state == "failed"
                            && parsed.contribute
                            && parsed.consent_epoch > 0),
                "学习重试条件无效"
            );
        }
        Ok(parsed)
    }

    pub(super) fn retry_request(&self, job_id: &str) -> Option<Value> {
        let item = self.jobs.iter().find(|j| j.job.id == job_id)?;
        (self.contribute && self.consent_epoch > 0 && item.retryable).then(|| {
            json!({
                "operation":"skill.learning.extraction.retry", "session_id":self.session_id,
                "job_id":job_id, "expected_epoch":self.consent_epoch,
                "expected_state_version":item.job.state_version
            })
        })
    }
}

pub(super) fn request(session: &str) -> Value {
    json!({"operation":"skill.learning.diagnostics", "session_id":session, "limit":LIMIT})
}
