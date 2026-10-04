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
        _ => None,
    }
}

impl DiscoveryBrowser {
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
                        && status.triggers.len() <= 3
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
                    "自动发现{} · worker {}\n支持 {}\nUTC {} 已预留 {} 次 · 待处理 {} · 无效信号 {}{}",
                    if status.enabled {
                        "已启用"
                    } else {
                        "未启用"
                    },
                    if status.running {
                        "运行中"
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
                    let mut evidence = Vec::with_capacity(record.evidence.len());
                    let mut roots = std::collections::HashSet::new();
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
                        details: format!("发现 {} · {}\n生成作业 {} · {}\n提案 {} · 基础状态版本 {} · {} 个独立根任务",
                            record.id, record.created_at, record.generation_id, record.state,
                            if record.proposal_id.is_empty() { "尚未生成" } else { &record.proposal_id }, record.base.state_version, evidence.len()),
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
            "显示真实触发类型、独立根任务及回执引用。发现和生成结果需要经过发布审核。",
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
