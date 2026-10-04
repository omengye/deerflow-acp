//! Validate behavior evidence when a MANAGE response arrives, then render only
//! the cached, bounded presentation. The host remains publication authority.
use super::skills::digest;
use super::*;
use serde::Deserialize;
use std::collections::BTreeMap;

const MAX_CASES: usize = 16;
const MAX_REPETITIONS: u64 = 3;
const MAX_REPORT_BYTES: usize = 64 << 10;

#[derive(Clone, Deserialize)]
struct Comparison {
    case_id: String,
    kind: String,
    repetition: u64,
    base_passed: bool,
    candidate_passed: bool,
    base_output_digest: String,
    candidate_output_digest: String,
    #[serde(default)]
    base_error_code: String,
    #[serde(default)]
    candidate_error_code: String,
}

#[derive(Deserialize)]
struct Report {
    evaluation_id: String,
    evaluator_identity: String,
    suite_id: String,
    suite_digest: String,
    rule_version: String,
    base_version: String,
    candidate_version: String,
    state: String,
    targeted_cases: u64,
    regression_cases: u64,
    regressions: u64,
    improvements: u64,
    results: Vec<Comparison>,
    model_calls: u64,
    tool_calls: u64,
    input_tokens: u64,
    output_tokens: u64,
    usage_estimated: bool,
    elapsed_millis: u64,
}

fn bounded(value: &str, limit: usize) -> bool {
    !value.is_empty() && value.len() <= limit && !value.chars().any(char::is_control)
}

fn code(value: &str) -> bool {
    value.len() <= 128
        && value
            .bytes()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == b'_')
}

fn outcome(passed: bool, error: &str) -> String {
    if passed {
        return "通过".into();
    }
    let label = match error {
        "" | "assertion_failed" => "断言未通过",
        "timeout" | "deadline_exceeded" => "执行超时",
        "budget_exhausted" => "维护预算不足",
        "cancelled" => "执行已取消",
        "model_error" => "模型调用失败",
        "step_limit" | "max_steps" => "达到执行步数上限",
        "fixture_mismatch" => "fixture 调用不匹配",
        "tool_error" => "fixture 工具执行失败",
        "isolation_unavailable" => "隔离环境不可用",
        _ => return format!("未通过（{error}）"),
    };
    label.into()
}

fn effect(comparison: &Comparison) -> &'static str {
    match (comparison.base_passed, comparison.candidate_passed) {
        (true, false) => "出现退化",
        (false, true) => "样例改善",
        (true, true) => "均通过",
        (false, false) => "均未通过",
    }
}

pub(super) struct BehaviorView {
    proposal_id: String,
    state_version: u64,
    title: String,
    summary: String,
    identity: String,
    usage: String,
    rows: Vec<(String, String)>,
}

impl BehaviorView {
    pub(super) fn parse(proposal: &Value) -> anyhow::Result<Option<Self>> {
        let evaluation = &proposal["evaluation"];
        let value = &evaluation["behavior_report"];
        if value.is_null() {
            anyhow::ensure!(
                evaluation["behavior_digest"].is_null() || evaluation["behavior_digest"] == "",
                "行为报告摘要缺少对应的报告"
            );
            return Ok(None);
        }
        // This is the same envelope budget as the persisted Go validation
        // report. Parse once on response, never while building a frame.
        anyhow::ensure!(
            serde_json::to_vec(evaluation)?.len() <= MAX_REPORT_BYTES,
            "行为评测报告超过大小限制"
        );
        let report: Report = serde_json::from_value(value.clone())?;
        anyhow::ensure!(
            bounded(&report.evaluation_id, 256)
                && bounded(&report.evaluator_identity, 256)
                && bounded(&report.suite_id, 128)
                && bounded(&report.rule_version, 128)
                && digest(&report.suite_digest)
                && evaluation["behavior_digest"].as_str().is_some_and(digest)
                && evaluation["base_version"] == report.base_version
                && evaluation["candidate_version"] == report.candidate_version
                && proposal["base"]["version"] == report.base_version
                && proposal["candidate_sha256"].as_str().unwrap_or("") == report.candidate_version
                && (report.base_version.is_empty() || digest(&report.base_version))
                && (report.candidate_version.is_empty() || digest(&report.candidate_version))
                && (!report.base_version.is_empty() || !report.candidate_version.is_empty())
                && matches!(report.state.as_str(), "passed" | "failed")
                && evaluation["behavior"] == report.state,
            "行为评测报告与提案版本或评测身份不一致"
        );
        anyhow::ensure!(
            report.targeted_cases >= 1
                && report.targeted_cases <= MAX_CASES as u64
                && report.regression_cases <= MAX_CASES as u64
                && report.targeted_cases + report.regression_cases <= MAX_CASES as u64
                && !report.results.is_empty()
                && report.results.len() <= MAX_CASES * MAX_REPETITIONS as usize
                && [
                    report.model_calls,
                    report.tool_calls,
                    report.input_tokens,
                    report.output_tokens,
                    report.elapsed_millis
                ]
                .iter()
                .all(|value| *value <= i64::MAX as u64),
            "行为评测报告的样例数量或用量无效"
        );
        let mut cases: BTreeMap<&str, (&str, u64)> = BTreeMap::new();
        let mut regressions = 0;
        let mut improvements = 0;
        let mut all_targeted_passed = true;
        for result in &report.results {
            anyhow::ensure!(
                bounded(&result.case_id, 128)
                    && matches!(result.kind.as_str(), "targeted" | "regression")
                    && (1..=MAX_REPETITIONS).contains(&result.repetition)
                    && digest(&result.base_output_digest)
                    && digest(&result.candidate_output_digest)
                    && code(&result.base_error_code)
                    && code(&result.candidate_error_code)
                    && (!result.base_passed || result.base_error_code.is_empty())
                    && (!result.candidate_passed || result.candidate_error_code.is_empty()),
                "行为样例缺少有效结果或输出摘要"
            );
            let previous = cases.entry(&result.case_id).or_insert((&result.kind, 0));
            anyhow::ensure!(
                previous.0 == result.kind && result.repetition == previous.1 + 1,
                "行为样例重复次数或类型不一致"
            );
            previous.1 = result.repetition;
            regressions += u64::from(result.base_passed && !result.candidate_passed);
            improvements += u64::from(!result.base_passed && result.candidate_passed);
            if result.kind == "targeted" {
                all_targeted_passed &= result.candidate_passed;
            }
        }
        let targeted = cases
            .values()
            .filter(|(kind, _)| *kind == "targeted")
            .count() as u64;
        let regression = cases.len() as u64 - targeted;
        let passed = targeted > 0 && all_targeted_passed && regressions == 0;
        anyhow::ensure!(
            targeted == report.targeted_cases
                && regression == report.regression_cases
                && regressions == report.regressions
                && improvements == report.improvements
                && passed == (report.state == "passed"),
            "行为评测汇总与逐样例结果不一致"
        );
        let rows = report
            .results
            .iter()
            .map(|result| {
                (
                    format!(
                        "{} · {}样例 · 第 {} 次 · {}",
                        result.case_id,
                        if result.kind == "targeted" {
                            "针对性"
                        } else {
                            "回归"
                        },
                        result.repetition,
                        effect(result)
                    ),
                    format!(
                        "基础：{} · 候选：{}\n输出 SHA-256 摘要：基础 {} · 候选 {}",
                        outcome(result.base_passed, &result.base_error_code),
                        outcome(result.candidate_passed, &result.candidate_error_code),
                        &result.base_output_digest[..16],
                        &result.candidate_output_digest[..16]
                    ),
                )
            })
            .collect();
        Ok(Some(Self {
            proposal_id: proposal["id"].as_str().unwrap_or("").to_owned(),
            state_version: proposal["state_version"].as_u64().unwrap_or(0),
            title: format!(
                "可信样例行为评测 · {}",
                if passed { "通过" } else { "未通过" }
            ),
            summary: format!(
                "套件 {} · 针对性样例 {} · 回归样例 {}\n共 {} 次基础/候选对照 · 改善 {} · 退化 {}",
                report.suite_id,
                targeted,
                regression,
                report.results.len(),
                improvements,
                regressions
            ),
            identity: format!(
                "评测 {}\n套件摘要 {}\n评分规则 {}\n行为执行器身份 {}\n报告摘要 {}",
                report.evaluation_id,
                report.suite_digest,
                report.rule_version,
                report.evaluator_identity,
                evaluation["behavior_digest"].as_str().unwrap_or("")
            ),
            usage: format!(
                "模型调用 {} · fixture 工具调用 {} · 耗时 {} ms\n输入 {} / 输出 {} tokens{}；费用计入独立维护预算。",
                report.model_calls,
                report.tool_calls,
                report.elapsed_millis,
                report.input_tokens,
                report.output_tokens,
                if report.usage_estimated {
                    "（估算）"
                } else {
                    ""
                }
            ),
            rows,
        }))
    }

    pub(super) fn matches(&self, proposal: &Value) -> bool {
        proposal["id"] == self.proposal_id
            && proposal["state_version"].as_u64() == Some(self.state_version)
    }

    pub(super) fn render(&self, theme: Theme) -> AnyElement {
        let mut group = div().flex().flex_col().gap(px(6.0))
            .child(df_label(self.title.clone(), self.summary.clone(), theme))
            .child(df_label("评测身份与评分规则", self.identity.clone(), theme))
            .child(df_label("评测用量", self.usage.clone(), theme))
            .child(df_label("逐样例基础与候选对照", "样例使用可信 fixture 工具和独立上下文。输出摘要显示前 16 位；结果仅代表当前套件的覆盖范围，发布还需满足审核或自动补丁条件。", theme));
        for (title, detail) in &self.rows {
            group = group.child(df_label(title.clone(), detail.clone(), theme));
        }
        group.into_any_element()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn comparison(id: &str, kind: &str, repetition: u64, base: bool, candidate: bool) -> Value {
        json!({"case_id":id,"kind":kind,"repetition":repetition,"base_passed":base,"candidate_passed":candidate,
            "base_output_digest":"a".repeat(64),"candidate_output_digest":"b".repeat(64)})
    }

    fn proposal() -> Value {
        json!({"id":"p-one","state_version":4,"base":{"version":"c".repeat(64)},"candidate_sha256":"d".repeat(64),
            "evaluation":{"base_version":"c".repeat(64),"candidate_version":"d".repeat(64),"behavior":"passed","behavior_digest":"e".repeat(64),
            "behavior_report":{"evaluation_id":"sbe-one","evaluator_identity":format!("eino-skill-behavior-v1/{}", "f".repeat(64)),
                "suite_id":"source-verification","suite_digest":"a".repeat(64),"rule_version":"fixture-rules-v1",
                "base_version":"c".repeat(64),"candidate_version":"d".repeat(64),"state":"passed",
                "targeted_cases":1,"regression_cases":1,"regressions":0,"improvements":2,
                "results":[comparison("dates","targeted",1,false,true),comparison("dates","targeted",2,false,true),comparison("format","regression",1,true,true)],
                "model_calls":6,"tool_calls":4,"input_tokens":600,"output_tokens":100,"usage_estimated":false,"elapsed_millis":2300}}})
    }

    #[test]
    fn deerflow_behavior_report_caches_repeated_case_comparisons_and_bound_identity() {
        let proposal = proposal();
        let view = BehaviorView::parse(&proposal).unwrap().unwrap();
        assert!(view.title.contains("通过"));
        assert!(view.summary.contains("针对性样例 1 · 回归样例 1"));
        assert!(view.summary.contains("改善 2 · 退化 0"));
        assert!(view.identity.contains("fixture-rules-v1"));
        assert!(view.identity.contains("eino-skill-behavior-v1/"));
        assert!(view.usage.contains("模型调用 6"));
        assert_eq!(view.rows.len(), 3);
        assert!(view.rows[0].1.contains("基础：断言未通过 · 候选：通过"));
        assert!(view.matches(&proposal));
        let mut changed = proposal.clone();
        changed["state_version"] = json!(5);
        assert!(!view.matches(&changed));
        changed["state_version"] = json!(4);
        changed["id"] = json!("p-other");
        assert!(!view.matches(&changed));
    }

    #[test]
    fn deerflow_behavior_absent_report_preserves_legacy_not_run_and_unavailable() {
        for state in ["not_run", "unavailable", "passed", "failed"] {
            assert!(
                BehaviorView::parse(&json!({"evaluation":{"behavior":state}}))
                    .unwrap()
                    .is_none()
            );
        }
        assert!(
            BehaviorView::parse(
                &json!({"evaluation":{"behavior":"passed","behavior_digest":"e".repeat(64)}})
            )
            .is_err()
        );
    }

    #[test]
    fn deerflow_behavior_report_rejects_rebound_versions_and_forged_summary() {
        for path in [
            "/evaluation/base_version",
            "/evaluation/candidate_version",
            "/base/version",
            "/candidate_sha256",
            "/evaluation/behavior_report/base_version",
            "/evaluation/behavior_report/candidate_version",
            "/evaluation/behavior_digest",
        ] {
            let mut changed = proposal();
            *changed.pointer_mut(path).unwrap() = json!("changed");
            assert!(BehaviorView::parse(&changed).is_err(), "{path}");
        }
        for field in [
            "targeted_cases",
            "regression_cases",
            "regressions",
            "improvements",
        ] {
            let mut changed = proposal();
            changed["evaluation"]["behavior_report"][field] = json!(999);
            assert!(BehaviorView::parse(&changed).is_err(), "{field}");
        }
        let mut changed = proposal();
        changed["evaluation"]["behavior_report"]["results"][0]["candidate_passed"] = json!(false);
        assert!(BehaviorView::parse(&changed).is_err());
    }

    #[test]
    fn deerflow_behavior_report_exposes_failures_and_regression_without_passing_them() {
        let mut failed = proposal();
        failed["evaluation"]["behavior"] = json!("failed");
        let report = &mut failed["evaluation"]["behavior_report"];
        report["state"] = json!("failed");
        report["regressions"] = json!(1);
        report["results"][2]["candidate_passed"] = json!(false);
        report["results"][2]["candidate_error_code"] = json!("fixture_mismatch");
        report["usage_estimated"] = json!(true);
        let view = BehaviorView::parse(&failed).unwrap().unwrap();
        assert!(view.title.contains("未通过"));
        assert!(view.summary.contains("退化 1"));
        assert!(view.rows[2].0.contains("出现退化"));
        assert!(view.rows[2].1.contains("fixture 调用不匹配"));
        assert!(view.usage.contains("估算"));
        failed["evaluation"]["behavior"] = json!("passed");
        assert!(BehaviorView::parse(&failed).is_err());
    }

    #[test]
    fn deerflow_behavior_report_requires_contiguous_bounded_repetitions_and_counts() {
        for repetition in [0, 2, 4] {
            let mut changed = proposal();
            changed["evaluation"]["behavior_report"]["results"][0]["repetition"] =
                json!(repetition);
            assert!(BehaviorView::parse(&changed).is_err());
        }
        let mut changed = proposal();
        changed["evaluation"]["behavior_report"]["results"][1]["kind"] = json!("regression");
        assert!(BehaviorView::parse(&changed).is_err());
        changed = proposal();
        changed["evaluation"]["behavior_report"]["results"] = json!([]);
        assert!(BehaviorView::parse(&changed).is_err());
        changed["evaluation"]["behavior_report"]["results"] =
            json!(vec![comparison("repeat", "targeted", 1, false, true); 49]);
        assert!(BehaviorView::parse(&changed).is_err());
        changed = proposal();
        changed["evaluation"]["evidence"] = json!(["x".repeat(MAX_REPORT_BYTES)]);
        assert!(BehaviorView::parse(&changed).is_err());
    }

    #[test]
    fn deerflow_behavior_report_rejects_invalid_numbers_and_partial_digests() {
        for field in [
            "model_calls",
            "tool_calls",
            "input_tokens",
            "output_tokens",
            "elapsed_millis",
        ] {
            for invalid in [json!(-1), json!(1.5), json!(u64::MAX), Value::Null] {
                let mut changed = proposal();
                changed["evaluation"]["behavior_report"][field] = invalid;
                assert!(BehaviorView::parse(&changed).is_err(), "{field}");
            }
        }
        for field in ["base_output_digest", "candidate_output_digest"] {
            let mut changed = proposal();
            changed["evaluation"]["behavior_report"]["results"][0][field] = json!("short");
            assert!(BehaviorView::parse(&changed).is_err(), "{field}");
        }
        let mut changed = proposal();
        changed["evaluation"]["behavior_report"]["results"][0]["candidate_error_code"] =
            json!("timeout");
        assert!(BehaviorView::parse(&changed).is_err());
    }

    #[test]
    fn deerflow_behavior_report_supports_create_and_delete_no_skill_controls() {
        let mut created = proposal();
        created["base"]["version"] = json!("");
        created["evaluation"]["base_version"] = json!("");
        created["evaluation"]["behavior_report"]["base_version"] = json!("");
        assert!(BehaviorView::parse(&created).unwrap().is_some());
        let mut deleted = proposal();
        deleted.as_object_mut().unwrap().remove("candidate_sha256");
        deleted["evaluation"]["candidate_version"] = json!("");
        deleted["evaluation"]["behavior_report"]["candidate_version"] = json!("");
        assert!(BehaviorView::parse(&deleted).unwrap().is_some());
        deleted["base"]["version"] = json!("");
        deleted["evaluation"]["base_version"] = json!("");
        deleted["evaluation"]["behavior_report"]["base_version"] = json!("");
        assert!(BehaviorView::parse(&deleted).is_err());
    }
}
