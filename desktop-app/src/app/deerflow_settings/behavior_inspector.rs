//! Cached, read-only maintenance details. Proposal refresh remains the authority
//! for publication; completed rows in a partial run never imply a passing review.
use super::skills::{Head, PAGE_SIZE, Target, digest};
use super::*;
use serde::Deserialize;
use std::collections::HashSet;

fn id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || c == b'_' || c == b'-')
}

fn timestamp(value: &str) -> bool {
    value.is_empty() || chrono::DateTime::parse_from_rfc3339(value).is_ok()
}

fn bounded_code(value: &str) -> bool {
    value.len() <= 128
        && value
            .bytes()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == b'_')
}

#[derive(Deserialize)]
struct Check {
    id: String,
    passed: bool,
    actual_digest: String,
}

#[derive(Deserialize)]
struct ResultRow {
    case_id: String,
    variant: String,
    version: String,
    repetition: u64,
    passed: bool,
    stop_reason: String,
    #[serde(default)]
    error_code: String,
    output_digest: String,
    trace_digest: String,
    checks: Vec<Check>,
    model_calls: u64,
    tool_calls: u64,
    input_tokens: u64,
    output_tokens: u64,
    usage_estimated: bool,
    elapsed_millis: u64,
    #[serde(default)]
    selection: Option<behavior::Selection>,
}

#[derive(Deserialize)]
struct Evaluation {
    id: String,
    proposal_id: String,
    job_id: String,
    target: Target,
    base: Head,
    candidate_version: String,
    #[serde(default)]
    suite_id: String,
    #[serde(default)]
    suite_digest: String,
    evaluator_identity: String,
    state: String,
    state_version: u64,
    attempts: u64,
    #[serde(default)]
    error_code: String,
    #[serde(default)]
    deadline: String,
    #[serde(default)]
    retry_at: String,
    content_reviewed: bool,
    #[serde(default)]
    report: Value,
}

#[derive(Deserialize)]
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

struct Detail {
    evaluation: Evaluation,
    title: String,
    identity: String,
    progress: String,
    rows: Vec<(String, String)>,
}

#[derive(Default)]
pub(super) struct BehaviorInspector {
    proposal: Option<Value>,
    generation: u64,
    pending: Option<(u64, Value)>,
    detail: Option<Detail>,
    absent: bool,
    result_page: usize,
    receipt_rows: Vec<(String, String)>,
    receipts_loaded: bool,
    receipt_after: u64,
    receipt_previous: Vec<u64>,
    receipt_next: Option<u64>,
}

impl BehaviorInspector {
    pub(super) fn invalidate(&mut self) {
        self.generation = self.generation.wrapping_add(1);
        self.pending = None;
        self.proposal = None;
        self.detail = None;
        self.absent = false;
        self.result_page = 0;
        self.clear_receipts();
    }

    pub(super) fn bind(&mut self, proposal: &Value) {
        self.invalidate();
        self.proposal = Some(
            json!({"id":proposal["id"],"state_version":proposal["state_version"],
            "target":proposal["target"],"base":proposal["base"],
            "candidate_sha256":proposal["candidate_sha256"],"behavior_job_id":proposal["behavior_job_id"],
            "evaluation":proposal["evaluation"]}),
        );
    }

    fn matches(&self, proposal: &Value) -> bool {
        self.proposal.as_ref().is_some_and(|bound| {
            bound["id"] == proposal["id"] && bound["state_version"] == proposal["state_version"]
        })
    }

    pub(super) fn begin(&mut self, request: &Value) {
        if request["operation"] == "proposal.get" {
            self.invalidate();
        } else if matches!(
            request["operation"].as_str(),
            Some("proposal.behavior" | "proposal.behavior.receipts")
        ) {
            self.pending = Some((self.generation, request.clone()));
        }
    }

    fn check_pending(&self, request: &Value) -> anyhow::Result<&Value> {
        let proposal = self
            .proposal
            .as_ref()
            .ok_or_else(|| anyhow::anyhow!("提案已变化，请刷新"))?;
        anyhow::ensure!(
            self.pending
                .as_ref()
                .is_some_and(
                    |(generation, pending)| *generation == self.generation && pending == request
                )
                && proposal["id"] == request["proposal_id"],
            "行为详情请求已过期"
        );
        Ok(proposal)
    }

    pub(super) fn accept_absent(&mut self, request: &Value) -> anyhow::Result<()> {
        self.check_pending(request)?;
        anyhow::ensure!(
            request["operation"] == "proposal.behavior",
            "无效的详情操作"
        );
        self.pending = None;
        self.detail = None;
        self.absent = true;
        self.clear_receipts();
        Ok(())
    }

    fn clear_receipts(&mut self) {
        self.receipt_rows.clear();
        self.receipts_loaded = false;
        self.receipt_after = 0;
        self.receipt_previous.clear();
        self.receipt_next = None;
    }

    pub(super) fn accept(&mut self, request: &Value, data: Value) -> anyhow::Result<()> {
        self.check_pending(request)?;
        match request["operation"].as_str() {
            Some("proposal.behavior") => self.accept_detail(data)?,
            Some("proposal.behavior.receipts") => self.accept_receipts(request, data)?,
            _ => anyhow::bail!("未知行为详情操作"),
        }
        self.pending = None;
        Ok(())
    }

    fn accept_detail(&mut self, data: Value) -> anyhow::Result<()> {
        anyhow::ensure!(
            serde_json::to_vec(&data)?.len() <= 1 << 20,
            "行为详情超过大小限制"
        );
        let evaluation: Evaluation = serde_json::from_value(data.clone())?;
        let proposal = self.proposal.as_ref().unwrap();
        let target: Target = serde_json::from_value(proposal["target"].clone())?;
        let base: Head = serde_json::from_value(proposal["base"].clone())?;
        anyhow::ensure!(
            id(&evaluation.id)
                && evaluation.id.starts_with("sbe_")
                && id(&evaluation.job_id)
                && evaluation.job_id.starts_with("smj_")
                && proposal["id"] == evaluation.proposal_id
                && evaluation.target == target
                && evaluation.base == base
                && proposal["candidate_sha256"].as_str().unwrap_or("")
                    == evaluation.candidate_version
                && (evaluation.candidate_version.is_empty()
                    || digest(&evaluation.candidate_version))
                && proposal["behavior_job_id"]
                    .as_str()
                    .filter(|v| !v.is_empty())
                    .is_none_or(|v| v == evaluation.job_id)
                && (1..=i64::MAX as u64).contains(&evaluation.state_version)
                && evaluation.attempts <= 3
                && matches!(
                    evaluation.state.as_str(),
                    "queued" | "running" | "retry" | "completed" | "failed" | "cancelled"
                )
                && bounded_code(&evaluation.error_code)
                && timestamp(&evaluation.deadline)
                && timestamp(&evaluation.retry_at)
                && !evaluation.evaluator_identity.is_empty()
                && evaluation.evaluator_identity.len() <= 128
                && !evaluation.evaluator_identity.chars().any(char::is_control)
                && ((evaluation.suite_id.is_empty() && evaluation.suite_digest.is_empty())
                    || (id(&evaluation.suite_id) && digest(&evaluation.suite_digest))),
            "行为详情身份或状态无效"
        );
        if let Some(prior) = &self.detail {
            anyhow::ensure!(
                prior.evaluation.id == evaluation.id
                    && prior.evaluation.job_id == evaluation.job_id
                    && prior.evaluation.state_version <= evaluation.state_version,
                "行为作业身份变化或状态倒退"
            );
        }
        let results: Vec<ResultRow> = if data["results"].is_null() {
            Vec::new()
        } else {
            serde_json::from_value(data["results"].clone())?
        };
        anyhow::ensure!(results.len() <= 96, "行为样例超过数量限制");
        let mut unique = HashSet::new();
        let mut cases = HashSet::new();
        let mut rows = Vec::with_capacity(results.len());
        for result in results {
            let expected_version = match result.variant.as_str() {
                "base" => &evaluation.base.version,
                "candidate" => &evaluation.candidate_version,
                _ => anyhow::bail!("行为样例版本类型无效"),
            };
            anyhow::ensure!(
                id(&result.case_id)
                    && (1..=3).contains(&result.repetition)
                    && result.version == *expected_version
                    && digest(&result.output_digest)
                    && digest(&result.trace_digest)
                    && bounded_code(&result.error_code)
                    && matches!(result.stop_reason.as_str(), "completed" | "failed")
                    && !result.checks.is_empty()
                    && result.checks.len() <= 32
                    && result
                        .checks
                        .iter()
                        .all(|check| id(&check.id) && digest(&check.actual_digest))
                    && result
                        .checks
                        .iter()
                        .map(|check| &check.id)
                        .collect::<HashSet<_>>()
                        .len()
                        == result.checks.len()
                    && (!result.passed
                        || (result.error_code.is_empty()
                            && result.stop_reason == "completed"
                            && result.checks.iter().all(|check| check.passed)))
                    && [
                        result.model_calls,
                        result.tool_calls,
                        result.input_tokens,
                        result.output_tokens,
                        result.elapsed_millis
                    ]
                    .iter()
                    .all(|value| *value <= i64::MAX as u64)
                    && unique.insert((
                        result.case_id.clone(),
                        result.variant.clone(),
                        result.repetition
                    )),
                "行为样例结果无效或重复"
            );
            cases.insert(result.case_id.clone());
            if let Some(selection) = &result.selection {
                anyhow::ensure!(
                    selection.validate()
                        && selection.target_name == evaluation.target.name
                        && result.tool_calls == 0
                        && result.model_calls > 0
                        && result.stop_reason == "completed"
                        && matches!(result.error_code.as_str(), "" | "assertion_failed"),
                    "元数据触发证据不得宣称工具执行或伪造目标"
                );
            }
            let checks = result
                .checks
                .iter()
                .map(|check| {
                    format!(
                        "{}：{}",
                        check.id,
                        if check.passed { "通过" } else { "未通过" }
                    )
                })
                .collect::<Vec<_>>()
                .join(" · ");
            rows.push((format!("{} · {} · 第 {} 次 · {}",result.case_id,if result.variant == "base" {"基础版本"} else {"候选版本"},result.repetition,if result.passed {"通过"} else {"未通过"}),
                format!("结束状态 {}{}\n断言：{}\n模型 {} 次 · fixture 工具 {} 次 · 输入 {} / 输出 {} tokens{} · {} ms\n输出摘要 {} · 轨迹摘要 {}（SHA-256 前 16 位）{}",
                result.stop_reason,if result.error_code.is_empty() {String::new()} else {format!(" · {}",result.error_code)},checks,result.model_calls,result.tool_calls,result.input_tokens,result.output_tokens,
                if result.usage_estimated {"（估算）"} else {""},result.elapsed_millis,&result.output_digest[..16],&result.trace_digest[..16],
                result.selection.as_ref().map_or(String::new(), |selection| format!("\n{}", selection.detail())))));
        }
        anyhow::ensure!(cases.len() <= 16, "行为样例种类超过限制");
        if !evaluation.report.is_null() {
            let mut checked = proposal.clone();
            checked["evaluation"] = json!({"base_version":evaluation.base.version,"candidate_version":evaluation.candidate_version,
                "behavior":evaluation.report["state"],"behavior_report":evaluation.report,"behavior_digest":"a".repeat(64)});
            behavior::BehaviorView::parse(&checked)?;
            anyhow::ensure!(
                evaluation.report["evaluation_id"] == evaluation.id
                    && evaluation.report["evaluator_identity"] == evaluation.evaluator_identity
                    && evaluation.report["suite_id"] == evaluation.suite_id
                    && evaluation.report["suite_digest"] == evaluation.suite_digest,
                "行为报告与作业身份不一致"
            );
        }
        let title = format!(
            "行为维护作业 · {} · 尝试 {} · 状态版本 {}",
            match evaluation.state.as_str() {
                "queued" => "等待执行",
                "running" => "执行中",
                "retry" => "等待重试",
                "completed" => "已完成",
                "failed" => "失败",
                "cancelled" => "已取消",
                _ => unreachable!(),
            },
            evaluation.attempts,
            evaluation.state_version
        );
        let identity = format!(
            "评测 {}\n维护作业 {}\n套件 {}\n执行器身份 {}",
            evaluation.id,
            evaluation.job_id,
            if evaluation.suite_id.is_empty() {
                "当前目标没有绑定样例套件"
            } else {
                &evaluation.suite_id
            },
            evaluation.evaluator_identity
        );
        let progress = format!(
            "内容审查{} · 已保存 {} 个版本样例结果。{}{}{}",
            if evaluation.content_reviewed {
                "已完成"
            } else {
                "尚未完成"
            },
            rows.len(),
            if evaluation.report.is_null() {
                "完整行为报告尚未生成；部分结果不代表评测通过。"
            } else {
                "完整报告已生成；刷新提案查看最新审核状态。"
            },
            if evaluation.error_code.is_empty() {
                String::new()
            } else {
                format!("\n处理结果：{}", evaluation.error_code)
            },
            if evaluation.state == "retry" {
                format!("\n下次尝试：{}", evaluation.retry_at)
            } else {
                String::new()
            }
        );
        self.detail = Some(Detail {
            evaluation,
            title,
            identity,
            progress,
            rows,
        });
        self.absent = false;
        self.result_page = 0;
        self.clear_receipts();
        Ok(())
    }

    fn accept_receipts(&mut self, request: &Value, data: Value) -> anyhow::Result<()> {
        let detail = self
            .detail
            .as_ref()
            .ok_or_else(|| anyhow::anyhow!("请先刷新行为详情"))?;
        anyhow::ensure!(
            data["proposal_id"] == detail.evaluation.proposal_id && data.get("receipts").is_some(),
            "回执提案身份不一致"
        );
        let receipts: Vec<Receipt> = if data["receipts"].is_null() {
            Vec::new()
        } else {
            serde_json::from_value(data["receipts"].clone())?
        };
        anyhow::ensure!(
            request["limit"].as_u64() == Some(PAGE_SIZE as u64) && receipts.len() <= PAGE_SIZE,
            "回执超过分页限制"
        );
        let after = request["after_sequence"].as_u64().unwrap_or(0);
        let mut last = after;
        let mut rows = Vec::with_capacity(receipts.len());
        let mut ids = HashSet::new();
        for receipt in &receipts {
            anyhow::ensure!(
                receipt.sequence > last
                    && receipt.sequence <= i64::MAX as u64
                    && receipt.job_id == detail.evaluation.job_id
                    && id(&receipt.id)
                    && ids.insert(&receipt.id)
                    && (1..=3).contains(&receipt.attempt)
                    && digest(&receipt.input_digest)
                    && !receipt.model_identity.is_empty()
                    && receipt.model_identity.len() <= 128
                    && !receipt.model_identity.chars().any(char::is_control)
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
                    .all(|v| *v <= i64::MAX as u64),
                "维护回执身份或用量无效"
            );
            last = receipt.sequence;
            rows.push((
                format!(
                    "#{} · {} · 尝试 {} · {}",
                    receipt.sequence,
                    match receipt.stage.as_str() {
                        "generation" => "生成",
                        "security" => "安全审查",
                        "quality" => "质量审查",
                        _ => "行为评测",
                    },
                    receipt.attempt,
                    receipt.day
                ),
                format!(
                    "计入 {} tokens · 预留 {} · 输入 {} / 输出 {} · {}{}",
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
            ));
        }
        anyhow::ensure!(
            data["last_sequence"].as_u64() == Some(last),
            "维护回执游标不一致"
        );
        if after == 0 {
            self.receipt_previous.clear();
        } else if self.receipt_next == Some(after) && after != self.receipt_after {
            anyhow::ensure!(self.receipt_previous.len() < 1000, "维护回执页数达到上限");
            self.receipt_previous.push(self.receipt_after);
        } else if self.receipt_previous.last() == Some(&after) {
            self.receipt_previous.pop();
        } else {
            anyhow::ensure!(after == self.receipt_after, "维护回执游标已变化，请刷新");
        }
        self.receipt_after = after;
        self.receipt_next = (receipts.len() == PAGE_SIZE).then_some(last);
        self.receipt_rows = rows;
        self.receipts_loaded = true;
        Ok(())
    }
}

impl Waku {
    pub(super) fn render_deerflow_behavior_inspector(
        &self,
        proposal: &Value,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let browser = &state.skill_browser.behavior_inspector;
        let bound = browser.matches(proposal);
        let unavailable = state.busy() || state.applying() || state.status["running"] != true;
        let mut group = div().flex().flex_col().gap(px(6.0));
        for (key, label, operation) in [
            ("detail", "刷新行为评测详情", "proposal.behavior"),
            ("proposal", "刷新提案审核状态", "proposal.get"),
        ] {
            let id = proposal["id"].clone();
            group = group.child(df_button(
                SharedString::from(format!("df-behavior-{key}")),
                label,
                unavailable
                    || (operation != "proposal.get"
                        && (!bound || state.skill_browser.review_stale)),
                false,
                theme,
                cx,
                move |this, _, cx| {
                    this.deerflow_request(
                        "manage",
                        json!({"operation":operation,"proposal_id":id}),
                        cx,
                    );
                },
            ));
        }
        if !bound {
            return group.into_any_element();
        }
        if browser.absent {
            group = group.child(df_label(
                "没有行为评测记录",
                "此提案未建立行为评测作业；已完成的内容审查仍可在上方查看。",
                theme,
            ));
        }
        if let Some(detail) = &browser.detail {
            group = group
                .child(df_label(
                    detail.title.clone(),
                    detail.progress.clone(),
                    theme,
                ))
                .child(df_label("评测作业身份", detail.identity.clone(), theme));
            let start = browser.result_page * PAGE_SIZE;
            for (title, text) in detail.rows.iter().skip(start).take(PAGE_SIZE) {
                group = group.child(df_label(title.clone(), text.clone(), theme));
            }
            for (key, label, page) in [
                ("previous", "样例上一页", browser.result_page.checked_sub(1)),
                (
                    "next",
                    "样例下一页",
                    (start + PAGE_SIZE < detail.rows.len()).then_some(browser.result_page + 1),
                ),
            ] {
                let generation = browser.generation;
                group = group.child(df_button(
                    SharedString::from(format!("df-behavior-results-{key}")),
                    label,
                    unavailable || page.is_none(),
                    false,
                    theme,
                    cx,
                    move |this, _, cx| {
                        let browser = &mut this.deerflow_settings.skill_browser.behavior_inspector;
                        if browser.generation == generation {
                            if let Some(page) = page {
                                browser.result_page = page;
                                cx.notify();
                            }
                        }
                    },
                ));
            }
            group = group.child(df_label(
                "维护作业用量回执",
                "同一作业可能包含生成、内容审查与行为评测调用，均计入维护额度。",
                theme,
            ));
            for (key, label, after) in [
                ("refresh", "刷新用量回执", Some(0)),
                (
                    "previous",
                    "回执上一页",
                    browser.receipt_previous.last().copied(),
                ),
                ("next", "回执下一页", browser.receipt_next),
            ] {
                let id = proposal["id"].clone();
                group = group.child(df_button(SharedString::from(format!("df-behavior-receipts-{key}")),label,unavailable || after.is_none() || state.skill_browser.review_stale,false,theme,cx,move |this,_,cx| {
                    if let Some(after) = after { this.deerflow_request("manage",json!({"operation":"proposal.behavior.receipts","proposal_id":id,"after_sequence":after,"limit":PAGE_SIZE}),cx); }
                }));
            }
            if browser.receipts_loaded && browser.receipt_rows.is_empty() {
                group = group.child(df_label(
                    "本页没有维护回执",
                    "可能尚未调用模型，或已到达回执末页。",
                    theme,
                ));
            }
            for (title, text) in &browser.receipt_rows {
                group = group.child(df_label(title.clone(), text.clone(), theme));
            }
        }
        group.into_any_element()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn proposal(name: &str) -> Value {
        json!({"id":name,"state_version":4,"target":{"source_id":"managed","source_identity":"a".repeat(64),"name":"source-check","scope":"global"},
            "base":{"version":"b".repeat(64),"state_version":2,"exists":true,"enabled":true,"managed":true},"candidate_sha256":"c".repeat(64),"behavior_job_id":"smj_manual"})
    }

    fn row() -> Value {
        json!({"case_id":"dates","variant":"candidate","version":"c".repeat(64),"repetition":1,"passed":false,"stop_reason":"completed","error_code":"assertion_failed",
            "output_digest":"d".repeat(64),"trace_digest":"e".repeat(64),"checks":[{"id":"references","passed":false,"actual_digest":"f".repeat(64)}],
            "model_calls":2,"tool_calls":1,"input_tokens":30,"output_tokens":10,"usage_estimated":true,"elapsed_millis":100})
    }

    fn detail(proposal: &Value) -> Value {
        json!({"id":"sbe_manual","proposal_id":proposal["id"],"job_id":"smj_manual","target":proposal["target"],"base":proposal["base"],
            "candidate_version":proposal["candidate_sha256"],"suite_id":"citations","suite_digest":"a".repeat(64),"evaluator_identity":"eino-skill-behavior-v1/fixture",
            "state":"failed","state_version":5,"attempts":1,"error_code":"budget_exhausted","content_reviewed":true,"results":[row()]})
    }

    fn request(proposal: &Value) -> Value {
        json!({"operation":"proposal.behavior","proposal_id":proposal["id"]})
    }

    fn loaded() -> (BehaviorInspector, Value) {
        let proposal = proposal("sp_a");
        let mut browser = BehaviorInspector::default();
        browser.bind(&proposal);
        let request = request(&proposal);
        browser.begin(&request);
        browser.accept(&request, detail(&proposal)).unwrap();
        (browser, proposal)
    }

    fn receipts(proposal: &Value, first: u64, count: usize) -> (Value, Value) {
        let request = json!({"operation":"proposal.behavior.receipts","proposal_id":proposal["id"],"after_sequence":first - 1,"limit":PAGE_SIZE});
        let rows: Vec<_> = (first..first + count as u64).map(|sequence| json!({"sequence":sequence,"id":format!("smr_{sequence}"),"job_id":"smj_manual","attempt":1,"day":"2026-10-02","stage":"behavior",
            "model_identity":"test-model","input_digest":"d".repeat(64),"reserved_tokens":100,"charged_tokens":40,"input_tokens":30,"output_tokens":10,"state":"settled","estimated":false})).collect();
        let data = json!({"proposal_id":proposal["id"],"receipts":rows,"last_sequence":first + count as u64 - 1});
        (request, data)
    }

    #[test]
    fn deerflow_behavior_inspector_partial_failure_keeps_evidence_without_passing_review() {
        let (browser, proposal) = loaded();
        let detail = browser.detail.as_ref().unwrap();
        assert!(browser.matches(&proposal));
        assert!(detail.title.contains("失败"));
        assert!(detail.progress.contains("部分结果不代表评测通过"));
        assert!(detail.progress.contains("budget_exhausted"));
        assert_eq!(detail.rows.len(), 1);
        assert!(detail.rows[0].0.contains("候选版本"));
        assert!(detail.rows[0].1.contains("references：未通过"));
        assert!(detail.rows[0].1.contains("（估算）"));
        assert!(browser.proposal.as_ref().unwrap()["evaluation"].is_null());
    }

    #[test]
    fn skill_trigger_inspector_keeps_selection_separate_from_execution() {
        let a = proposal("sp_a");
        let request = request(&a);
        for mode in [
            "valid",
            "tool-executed",
            "rebound-target",
            "quality-claim",
            "bad-digest",
        ] {
            let mut browser = BehaviorInspector::default();
            browser.bind(&a);
            browser.begin(&request);
            let mut data = detail(&a);
            data["results"][0]["tool_calls"] = json!(0);
            data["results"][0]["selection"] = json!({"rule_version":"skill-trigger-selection-v1","target_name":"source-check","selected_name":"","catalog_digest":"a".repeat(64)});
            match mode {
                "tool-executed" => data["results"][0]["tool_calls"] = json!(1),
                "rebound-target" => {
                    data["results"][0]["selection"]["target_name"] = json!("another-check")
                }
                "quality-claim" => {
                    data["results"][0]["selection"]["rule_version"] = json!("quality-confirmed")
                }
                "bad-digest" => data["results"][0]["selection"]["catalog_digest"] = json!("bad"),
                _ => {}
            }
            let outcome = browser.accept(&request, data);
            assert_eq!(outcome.is_ok(), mode == "valid", "{mode}");
            if mode == "valid" {
                let detail = browser.detail.as_ref().unwrap();
                assert!(detail.rows[0].1.contains("未加载正文、未执行工具"));
                assert!(detail.rows[0].1.contains("不是质量确认"));
            }
        }
    }

    #[test]
    fn deerflow_behavior_inspector_rejects_late_response_after_proposal_or_service_change() {
        let a = proposal("sp_a");
        let request = request(&a);
        for change in 0..3 {
            let mut browser = BehaviorInspector::default();
            browser.bind(&a);
            browser.begin(&request);
            match change {
                0 => browser.invalidate(),
                1 => browser.bind(&proposal("sp_b")),
                _ => {
                    browser.bind(&proposal("sp_b"));
                    browser.bind(&a);
                }
            }
            assert!(browser.accept(&request, detail(&a)).is_err());
            assert!(browser.detail.is_none());
        }
    }

    #[test]
    fn deerflow_behavior_inspector_rejects_rebinding_duplicate_and_unbounded_results() {
        for path in [
            "/proposal_id",
            "/job_id",
            "/target/source_identity",
            "/base/version",
            "/candidate_version",
            "/results/0/version",
            "/results/0/output_digest",
            "/results/0/checks/0/actual_digest",
        ] {
            let a = proposal("sp_a");
            let request = request(&a);
            let mut browser = BehaviorInspector::default();
            browser.bind(&a);
            browser.begin(&request);
            let mut data = detail(&a);
            *data.pointer_mut(path).unwrap() = json!("different");
            assert!(browser.accept(&request, data).is_err(), "{path}");
        }
        for rows in [vec![row(); 2], vec![row(); 97]] {
            let (mut browser, a) = loaded();
            let request = request(&a);
            browser.begin(&request);
            let mut data = detail(&a);
            data["results"] = json!(rows);
            assert!(browser.accept(&request, data).is_err());
        }
        let (mut browser, a) = loaded();
        let request = request(&a);
        browser.begin(&request);
        let mut data = detail(&a);
        data["results"][0]["passed"] = json!(true);
        assert!(browser.accept(&request, data).is_err());
    }

    #[test]
    fn deerflow_behavior_inspector_receipts_bind_job_proposal_and_cursor() {
        for path in [
            "/proposal_id",
            "/receipts/0/job_id",
            "/receipts/0/input_digest",
            "/last_sequence",
        ] {
            let (mut browser, a) = loaded();
            let (request, mut data) = receipts(&a, 1, 1);
            browser.begin(&request);
            *data.pointer_mut(path).unwrap() = json!("changed");
            assert!(browser.accept(&request, data).is_err(), "{path}");
            assert!(!browser.receipts_loaded);
        }
        let (mut browser, a) = loaded();
        let (receipt_request, data) = receipts(&a, 1, PAGE_SIZE);
        browser.begin(&receipt_request);
        browser.accept(&receipt_request, data).unwrap();
        assert_eq!(browser.receipt_next, Some(PAGE_SIZE as u64));
        let (next, data) = receipts(&a, PAGE_SIZE as u64 + 1, 1);
        browser.begin(&next);
        browser.accept(&next, data).unwrap();
        assert_eq!(browser.receipt_previous, vec![0]);
        assert_eq!(browser.receipt_rows.len(), 1);
        assert_eq!(browser.receipt_next, None);
        let (jump, data) = receipts(&a, 300, 1);
        browser.begin(&jump);
        assert!(browser.accept(&jump, data).is_err());
        assert_eq!(browser.receipt_after, PAGE_SIZE as u64);
        let request = request(&a);
        browser.begin(&request);
        browser.accept(&request, detail(&a)).unwrap();
        assert!(!browser.receipts_loaded);
        assert!(browser.receipt_previous.is_empty());
    }

    #[test]
    fn deerflow_behavior_inspector_empty_and_missing_records_are_read_only() {
        let (mut browser, a) = loaded();
        let request = request(&a);
        browser.begin(&request);
        browser.accept_absent(&request).unwrap();
        assert!(browser.absent);
        assert!(browser.detail.is_none());
        assert!(browser.matches(&a));
        browser.begin(&request);
        let mut data = detail(&a);
        data["state"] = json!("queued");
        data["results"] = Value::Null;
        data["error_code"] = json!("");
        data["attempts"] = json!(0);
        data["content_reviewed"] = json!(false);
        data["suite_id"] = json!("");
        data["suite_digest"] = json!("");
        browser.accept(&request, data).unwrap();
        assert!(browser.detail.as_ref().unwrap().rows.is_empty());
        assert!(!browser.absent);
    }
}
