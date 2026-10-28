//! Read-only discovery inventory. Keep one bounded page and format evidence
//! when replies arrive, so render only reads cached rows and selection.
use super::skills::{Head, PAGE_SIZE, PageNav, Target, digest};
use super::*;
use serde::Deserialize;

#[derive(Deserialize)]
struct Record {
    id: String,
    target: Target,
    base: Head,
    generation_id: String,
    created_at: String,
    kind: String,
    evidence: Vec<Evidence>,
    state: String,
    #[serde(default)]
    proposal_id: String,
    #[serde(default)]
    host_adjudicated: bool,
    #[serde(default)]
    review_draft: bool,
    #[serde(default)]
    creation_retry: Option<CreationRetry>,
    #[serde(default)]
    previous_proposal_id: String,
}

#[derive(Deserialize)]
struct CreationRetry {
    previous_generation_id: String,
    evidence_reservation_id: String,
    retry_number: u64,
}

#[derive(Deserialize)]
struct Evidence {
    root_id: String,
    #[serde(default)]
    signal_id: String,
    #[serde(default)]
    signal_state_version: u64,
    #[serde(default)]
    feedback_revision: u64,
    read_ids: Option<Vec<u64>>,
    #[serde(default)]
    missing_report_id: String,
    #[serde(default)]
    report_revision: u64,
    #[serde(default)]
    workflow_digest: String,
    #[serde(default)]
    receipts: Vec<Receipt>,
    #[serde(default)]
    episode_key: String,
    #[serde(default)]
    experience: Option<Experience>,
}

#[derive(Deserialize)]
struct Experience {
    match_id: String,
    pattern_id: String,
    pattern_revision: u64,
    match_input_sha256: String,
    match_result_sha256: String,
    pattern_sha256: String,
    policy_identity: String,
    episode_id: String,
    independence_key: String,
}

#[derive(Deserialize)]
struct Receipt {
    run_id: String,
    tool_call_id: String,
    version: u64,
}

struct Row {
    id: String,
    title: String,
    details: String,
    generation_id: String,
    evidence: Vec<(String, String)>,
}

#[derive(Default)]
pub(super) struct DiscoveryBrowser {
    status: Option<String>,
    rows: Vec<Row>,
    loaded: bool,
    selected: Option<String>,
    selected_evidence: usize,
    nav: PageNav,
}

fn bounded(value: &str, max: usize) -> bool {
    !value.is_empty() && value.len() <= max && !value.contains('\0')
}

fn label(kind: &str) -> Option<&'static str> {
    match kind {
        "operator_corrections" => Some("重复纠正"),
        "missing_skill" => Some("缺失技能报告"),
        "repeated_workflow" => Some("重复成功流程"),
        "experience_pattern" => Some("多轮会话经验"),
        _ => None,
    }
}

impl DiscoveryBrowser {
    pub(super) fn status_summary(&self) -> Option<&str> {
        self.status.as_deref()
    }

    pub(super) fn invalidate(&mut self) {
        *self = Self::default();
    }

    pub(super) fn accept(&mut self, request: &Value, data: Value) -> anyhow::Result<()> {
        match request["operation"].as_str() {
            Some("skill.discovery.status") => {
                #[derive(Deserialize)]
                struct Status {
                    version: String,
                    enabled: bool,
                    running: bool,
                    triggers: Vec<String>,
                    today: String,
                    daily_reservations: u64,
                    pending: u64,
                    failed_signals: u64,
                    #[serde(default)]
                    last_error_code: String,
                }
                let status: Status = serde_json::from_value(data)?;
                anyhow::ensure!(
                    status.version == "skill-discovery-v1"
                        && status.triggers.len() <= 4
                        && status.triggers.iter().all(|kind| label(kind).is_some())
                        && chrono::NaiveDate::parse_from_str(&status.today, "%Y-%m-%d").is_ok()
                        && [
                            status.daily_reservations,
                            status.pending,
                            status.failed_signals
                        ]
                        .iter()
                        .all(|v| *v <= i64::MAX as u64)
                        && status.last_error_code.len() <= 128,
                    "自动发现状态无效"
                );
                self.status = Some(format!(
                    "自动发现{} · 后台执行器{}（在线不代表已有提案）\n支持 {}\nUTC {} 已预留 {} 次 · 待处理 {} · 无效信号 {}{}",
                    if status.enabled {
                        "已启用"
                    } else {
                        "未启用"
                    },
                    if status.running {
                        "在线"
                    } else {
                        "未运行"
                    },
                    status
                        .triggers
                        .iter()
                        .filter_map(|kind| label(kind))
                        .collect::<Vec<_>>()
                        .join("、"),
                    status.today,
                    status.daily_reservations,
                    status.pending,
                    status.failed_signals,
                    if status.last_error_code.is_empty() {
                        String::new()
                    } else {
                        format!("\n最近错误 {}", status.last_error_code)
                    }
                ));
            }
            Some("skill.discovery.list") => {
                #[derive(Deserialize)]
                struct Page {
                    discoveries: Vec<Record>,
                    #[serde(default)]
                    next_cursor: String,
                }
                let page: Page = serde_json::from_value(data)?;
                anyhow::ensure!(page.discoveries.len() <= PAGE_SIZE, "自动发现页面超出限制");
                let mut rows = Vec::with_capacity(page.discoveries.len());
                let mut seen = std::collections::HashSet::new();
                for record in page.discoveries {
                    let kind =
                        label(&record.kind).ok_or_else(|| anyhow::anyhow!("未知自动发现类型"))?;
                    anyhow::ensure!(
                        bounded(&record.id, 128)
                            && seen.insert(record.id.clone())
                            && bounded(&record.generation_id, 128)
                            && record.generation_id.starts_with("smj_")
                            && bounded(&record.target.source_id, 64)
                            && bounded(&record.target.name, 64)
                            && digest(&record.target.source_identity)
                            && matches!(record.target.scope.as_str(), "global" | "workspace")
                            && (record.proposal_id.is_empty() || bounded(&record.proposal_id, 128))
                            && matches!(
                                record.state.as_str(),
                                "queued"
                                    | "running"
                                    | "retry"
                                    | "completed"
                                    | "failed"
                                    | "cancelled"
                            )
                            && chrono::DateTime::parse_from_rfc3339(&record.created_at).is_ok()
                            && !record.evidence.is_empty()
                            && record.evidence.len() <= 16
                            && record.base.state_version <= i64::MAX as u64
                            && if record.base.exists {
                                digest(&record.base.version)
                            } else {
                                record.base.version.is_empty()
                            },
                        "自动发现记录的身份、状态或证据无效"
                    );
                    let independent_count = record.evidence.len();
                    anyhow::ensure!(
                        !record.review_draft
                            || record.kind == "experience_pattern"
                                && record.creation_retry.is_none(),
                        "待审经验草案不能伪称其他触发或人工创建重试"
                    );
                    let mut evidence = Vec::with_capacity(
                        independent_count + usize::from(record.creation_retry.is_some()),
                    );
                    if let Some(retry) = &record.creation_retry {
                        anyhow::ensure!(
                            record.kind == "experience_pattern"
                                && !record.host_adjudicated
                                && !record.base.exists
                                && record.base.state_version == 0
                                && record.base.version.is_empty()
                                && bounded(&retry.previous_generation_id, 128)
                                && retry.previous_generation_id.starts_with("smj_")
                                && bounded(&retry.evidence_reservation_id, 128)
                                && retry.evidence_reservation_id.starts_with("sd_")
                                && retry.evidence_reservation_id != record.id
                                && (1..=3).contains(&retry.retry_number),
                            "人工创建重试来源无效"
                        );
                        evidence.push(("人工创建重试授权".into(), format!("人工重试 {} / 3\n原作业 {}\n原独立 episode 消费预留 {}\n复用证据不新增独立票、旧费用不退；新技能必须人工发布。",
                            retry.retry_number, retry.previous_generation_id, retry.evidence_reservation_id)));
                    }
                    let mut roots = std::collections::HashSet::new();
                    if !record.previous_proposal_id.is_empty() {
                        anyhow::ensure!(
                            bounded(&record.previous_proposal_id, 131)
                                && record.previous_proposal_id.starts_with("p_")
                                && !record.review_draft
                                && record.base.exists,
                            "草案后继引用无效"
                        );
                        evidence.push(("证据成熟后继候选".into(), format!("替代旧草案 {}。使用新的未消费独立证据；旧草案不直接发布，旧费用及消费记录保留。", record.previous_proposal_id)));
                    }
                    for item in record.evidence {
                        anyhow::ensure!(
                            bounded(&item.root_id, 256) && roots.insert(item.root_id.clone()),
                            "自动发现根任务重复或无效"
                        );
                        let detail = match record.kind.as_str() {
                            "operator_corrections" => {
                                let reads = item.read_ids.unwrap_or_default();
                                anyhow::ensure!(
                                    bounded(&item.signal_id, 128)
                                        && item.signal_state_version > 0
                                        && item.feedback_revision > 0
                                        && !reads.is_empty()
                                        && reads.len() <= 16
                                        && reads.iter().all(|v| *v > 0 && *v <= i64::MAX as u64),
                                    "纠正证据无效"
                                );
                                format!(
                                    "信号 {} · 状态版本 {} · 反馈修订 {}\n读取 ID {:?}",
                                    item.signal_id,
                                    item.signal_state_version,
                                    item.feedback_revision,
                                    reads
                                )
                            }
                            "missing_skill" => {
                                anyhow::ensure!(
                                    bounded(&item.missing_report_id, 128)
                                        && item.report_revision > 0,
                                    "缺失报告引用无效"
                                );
                                format!(
                                    "缺失报告 {} · 修订 {}",
                                    item.missing_report_id, item.report_revision
                                )
                            }
                            "experience_pattern" => {
                                let experience = item
                                    .experience
                                    .ok_or_else(|| anyhow::anyhow!("经验来源引用缺失"))?;
                                anyhow::ensure!(
                                    bounded(&experience.match_id, 128)
                                        && bounded(&experience.pattern_id, 128)
                                        && experience.pattern_revision > 0
                                        && experience.pattern_revision <= i64::MAX as u64
                                        && digest(&experience.match_input_sha256)
                                        && digest(&experience.match_result_sha256)
                                        && digest(&experience.pattern_sha256)
                                        && digest(&experience.policy_identity)
                                        && bounded(&experience.episode_id, 256)
                                        && digest(&experience.independence_key)
                                        && item.episode_key == experience.independence_key,
                                    "经验来源身份或摘要无效"
                                );
                                format!(
                                    "Pattern {} · 修订 {}\n匹配 {}\nEpisode {} · 独立键 {}\n输入 {}\n结果 {}\nPattern 摘要 {}\n策略 {}\n质量尚未评估；本地完成回执不证明有效性或技能因果贡献。{}",
                                    experience.pattern_id,
                                    experience.pattern_revision,
                                    experience.match_id,
                                    experience.episode_id,
                                    experience.independence_key,
                                    experience.match_input_sha256,
                                    experience.match_result_sha256,
                                    experience.pattern_sha256,
                                    experience.policy_identity,
                                    if record.host_adjudicated {
                                        "目标经人工裁决，此候选必须人工发布，不借用自动补丁授权"
                                    } else {
                                        "发布遵循独立宿主准入或人工审核"
                                    }
                                )
                            }
                            _ => {
                                anyhow::ensure!(
                                    digest(&item.workflow_digest)
                                        && (3..=32).contains(&item.receipts.len()),
                                    "成功流程证据无效"
                                );
                                let mut receipt_lines = Vec::with_capacity(item.receipts.len());
                                for receipt in item.receipts {
                                    anyhow::ensure!(
                                        bounded(&receipt.run_id, 256)
                                            && bounded(&receipt.tool_call_id, 256)
                                            && receipt.version > 0
                                            && receipt.version <= i64::MAX as u64,
                                        "流程工具回执引用无效"
                                    );
                                    receipt_lines.push(format!(
                                        "{} / {} · v{}",
                                        receipt.run_id, receipt.tool_call_id, receipt.version
                                    ));
                                }
                                format!(
                                    "流程摘要 {}\n工具回执（run / tool call / version）\n{}",
                                    item.workflow_digest,
                                    receipt_lines.join("\n")
                                )
                            }
                        };
                        evidence.push((format!("根任务 {}", item.root_id), detail));
                    }
                    rows.push(Row { title: format!("{kind} · {} / {}", record.target.source_id, record.target.name),
                        details: format!("发现 {} · {}\n生成作业 {} · {}\n提案 {} · 基础状态版本 {} · {} 个独立任务来源\n{}",
                            record.id, record.created_at, record.generation_id, record.state,
                            if record.proposal_id.is_empty() { "尚未生成" } else { &record.proposal_id }, record.base.state_version, independent_count,
                            if record.review_draft { "系统自动起草；独立任务证据不足，仅可人工审核发布，不因后来证据增加而升级自动发布授权" } else { "系统自动生成；是否自动发布由独立授权和可信评测决定，新技能仍人工审核" }),
                        id: record.id, generation_id: record.generation_id, evidence });
                }
                self.nav.accept(request, &page.next_cursor, "cursor")?;
                self.rows = rows;
                self.loaded = true;
                self.selected = None;
                self.selected_evidence = 0;
            }
            _ => anyhow::bail!("未知自动发现查询"),
        }
        Ok(())
    }
}

impl Waku {
    pub(super) fn render_deerflow_discovery(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let browser = &state.skill_browser.discovery;
        let supported = [
            "operator_corrections_enabled",
            "missing_reports_enabled",
            "successful_workflows_enabled",
            "new_skill_source_id",
        ]
        .iter()
        .all(|field| {
            state
                .settings_contract
                .allows(&format!("/skill_evolution/discovery/{field}"))
        });
        let unavailable =
            !supported || state.busy() || state.applying() || state.status["running"] != true;
        let mut group = df_group(theme).child(df_label(
            "自动发现记录",
            "显示真实触发类型、任务独立性及回执或经验来源引用。多轮经验候选仅改或新建 SKILL.md；默认人工发布，独立授权的正文补丁需行为改善，描述补丁另需正负触发选择改善。新技能和其他元数据仍人工处理。",
            theme,
        ));
        if !supported {
            return group
                .child(df_label(
                    "当前服务未提供此能力",
                    "更新配套 Go 服务并重新加载配置后可查看。",
                    theme,
                ))
                .into_any_element();
        }
        let mut actions = div().flex().flex_wrap().gap(px(6.0));
        for (suffix, caption, operation) in [
            ("status", "刷新发现状态", "skill.discovery.status"),
            ("list", "刷新发现记录", "skill.discovery.list"),
        ] {
            actions = actions.child(df_button(
                SharedString::from(format!("df-discovery-{suffix}")),
                caption,
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
        if let Some(status) = &browser.status {
            group = group.child(df_label("运行状态", status.clone(), theme));
        }
        if browser.loaded && browser.rows.is_empty() {
            group = group.child(df_label(
                "暂无发现记录",
                "启用所需触发器后，满足条件的证据会生成候选作业。",
                theme,
            ));
        }
        for row in &browser.rows {
            let id = row.id.clone();
            group = group.child(df_button(
                SharedString::from(format!("df-discovery-{id}")),
                row.title.clone(),
                unavailable,
                browser.selected.as_ref() == Some(&id),
                theme,
                cx,
                move |this, _, cx| {
                    this.deerflow_settings.skill_browser.discovery.selected = Some(id.clone());
                    this.deerflow_settings
                        .skill_browser
                        .discovery
                        .selected_evidence = 0;
                    cx.notify();
                },
            ));
        }
        group = group.child(self.render_skill_page_nav(
            "discovery",
            &browser.nav,
            unavailable,
            theme,
            cx,
        ));
        if let Some(row) = browser
            .rows
            .iter()
            .find(|row| Some(&row.id) == browser.selected.as_ref())
        {
            group = group.child(df_label(row.title.clone(), row.details.clone(), theme));
            let job = row.generation_id.clone();
            group = group.child(df_button(
                "df-discovery-job",
                "查看关联生成作业",
                unavailable,
                false,
                theme,
                cx,
                move |this, _, cx| {
                    this.deerflow_request(
                        "manage",
                        json!({"operation":"skill.generation.get","job_id":job}),
                        cx,
                    );
                },
            ));
            let mut choices = div().flex().flex_wrap().gap(px(6.0));
            for index in 0..row.evidence.len() {
                choices = choices.child(df_button(
                    SharedString::from(format!("df-discovery-evidence-{index}")),
                    format!("证据 {}", index + 1),
                    false,
                    browser.selected_evidence == index,
                    theme,
                    cx,
                    move |this, _, cx| {
                        this.deerflow_settings
                            .skill_browser
                            .discovery
                            .selected_evidence = index;
                        cx.notify();
                    },
                ));
            }
            group = group.child(choices);
            if let Some((title, detail)) = row.evidence.get(browser.selected_evidence) {
                group = group.child(df_label(title.clone(), detail.clone(), theme));
            }
        }
        group.into_any_element()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn retry_record() -> Value {
        let reference = json!({"match_id":"lmatch_original", "pattern_id":"lpat_original", "pattern_revision":2,
            "match_input_sha256":"a".repeat(64), "match_result_sha256":"b".repeat(64), "pattern_sha256":"c".repeat(64),
            "policy_identity":"d".repeat(64), "episode_id":"lep_first", "independence_key":"e".repeat(64)});
        let mut second = reference.clone();
        second["episode_id"] = json!("lep_second");
        second["independence_key"] = json!("f".repeat(64));
        json!({"id":"sd_retry","target":{"source_id":"project","source_identity":"a".repeat(64),"name":"json-audit","scope":"global"},
            "base":{"version":"","state_version":0,"exists":false,"enabled":false,"managed":false},"generation_id":"smj_retry",
            "created_at":"2026-10-09T08:20:00Z","kind":"experience_pattern","state":"queued","host_adjudicated":false,
            "creation_retry":{"previous_generation_id":"smj_previous","evidence_reservation_id":"sd_original","retry_number":1},
            "evidence":[{"root_id":"root_first","episode_key":"e".repeat(64),"experience":reference},
                {"root_id":"root_second","episode_key":"f".repeat(64),"experience":second}]})
    }

    #[test]
    fn learning_review_draft_is_system_generated_and_never_displayed_as_auto_publishable() {
        let mut record = retry_record();
        record.as_object_mut().unwrap().remove("creation_retry");
        record["review_draft"] = json!(true);
        record["evidence"].as_array_mut().unwrap().truncate(1);
        let mut browser = DiscoveryBrowser::default();
        let query = json!({"operation":"skill.discovery.list","limit":PAGE_SIZE});
        browser
            .accept(&query, json!({"discoveries":[record.clone()]}))
            .unwrap();
        assert!(browser.rows[0].details.contains("1 个独立任务来源"));
        assert!(browser.rows[0].details.contains("系统自动起草"));
        assert!(browser.rows[0].details.contains("仅可人工审核发布"));
        assert_eq!(browser.rows[0].evidence.len(), 1);
        record["creation_retry"] = retry_record()["creation_retry"].clone();
        assert!(
            browser
                .accept(&query, json!({"discoveries":[record]}))
                .is_err()
        );
    }

    #[test]
    fn learning_creation_retry_discovery_does_not_count_authorization_as_an_episode() {
        let mut browser = DiscoveryBrowser::default();
        let query = json!({"operation":"skill.discovery.list","limit":PAGE_SIZE});
        browser
            .accept(&query, json!({"discoveries":[retry_record()]}))
            .unwrap();
        assert!(browser.rows[0].details.contains("2 个独立任务来源"));
        assert_eq!(browser.rows[0].evidence.len(), 3);
        assert!(browser.rows[0].evidence[0].1.contains("不新增独立票"));
        for number in [0, 4] {
            let mut wrong = retry_record();
            wrong["creation_retry"]["retry_number"] = json!(number);
            assert!(
                browser
                    .accept(&query, json!({"discoveries":[wrong]}))
                    .is_err()
            );
        }
        let mut wrong = retry_record();
        wrong["creation_retry"]["evidence_reservation_id"] = json!("sd_retry");
        assert!(
            browser
                .accept(&query, json!({"discoveries":[wrong]}))
                .is_err()
        );
    }
}
