//! Receipt evidence is parsed once, bounded, and displayed without executing it.
use super::*;
use crate::ui::text_field::TextField;
use gpui::Subscription;
use waku_protocol::deerflow_tasks::{
    ReceiptOutcome, ReceiptPage, ReceiptReconciliation, ToolReceipt,
};

// Escaped evidence can be larger than its raw 64 KiB budget. Four receipts,
// including prior review evidence, fit the shared 4 MiB control response cap.
const RECEIPT_PAGE_SIZE: u32 = 4;

pub(super) fn list_action(cursor: Option<String>, uncertain_only: bool) -> Action {
    Action::ReceiptList {
        cursor,
        limit: RECEIPT_PAGE_SIZE,
        uncertain_only,
    }
}

struct CachedReceipt {
    receipt: ToolReceipt,
    target: ReviewTarget,
    evidence: SharedString,
    summary: SharedString,
    guidance: &'static str,
}
#[derive(Clone, Debug, Eq, PartialEq)]
struct ReviewTarget {
    session_id: String,
    run_id: String,
    tool_call_id: String,
    expected_version: u64,
}
impl From<&ToolReceipt> for ReviewTarget {
    fn from(receipt: &ToolReceipt) -> Self {
        Self {
            session_id: receipt.session_id.clone(),
            run_id: receipt.run_id.clone(),
            tool_call_id: receipt.tool_call_id.clone(),
            expected_version: receipt.version,
        }
    }
}
pub(super) struct ReviewInputs {
    target: ReviewTarget,
    reviewer: Entity<TextInput>,
    note: Entity<TextInput>,
    evidence: Entity<TextInput>,
    show_evidence: bool,
    _subscriptions: Vec<Subscription>,
}

#[derive(Default)]
pub(super) struct ReceiptBrowser {
    page: Option<ReceiptPage>,
    rows: Vec<CachedReceipt>,
    uncertain_only: bool,
    fresh: bool,
    pub(super) reviews: Vec<ReviewInputs>,
}

fn valid_review_fields(reviewer: &str, note: &str) -> bool {
    let reviewer = reviewer.trim();
    let note = note.trim();
    !reviewer.is_empty() && reviewer.len() <= 256 && !note.is_empty() && note.len() <= 16384
}

fn receipt_summary(receipt: &ToolReceipt) -> String {
    let operation = match receipt.tool_name.as_str() {
        "delete_path" => "删除文件或目录",
        "write_file" => "写入文件",
        "str_replace" => "修改文件",
        "move_path" => "移动文件或目录",
        "execute" => "执行命令",
        _ => &receipt.tool_name,
    };
    let state = match receipt.state.as_str() {
        "uncertain" => "需要你确认结果",
        "completed" => "已完成",
        "failed" => "执行失败",
        "no_effect" => "未产生效果",
        "not_executed" => "未执行",
        "pending" => "等待执行",
        "started" => "已开始执行",
        _ => "未知状态",
    };
    let cause = if receipt.error.contains("Access is denied")
        || receipt.error.to_lowercase().contains("permission denied")
    {
        "访问被拒绝，可能没有所需权限。报错本身不能证明操作完全没有产生效果。".to_owned()
    } else if receipt.error.contains("context canceled") {
        "操作被中断，可能已经完成了部分工作。".to_owned()
    } else if receipt.error.is_empty() {
        "请根据实际文件或目标服务的记录确认结果。".to_owned()
    } else {
        format!(
            "工具报告：{}",
            receipt.error.chars().take(400).collect::<String>()
        )
    };
    let detail = if receipt.error.contains("Access is denied")
        || receipt.error.to_lowercase().contains("permission denied")
    {
        format!(
            "\n原始错误：{}",
            receipt.error.chars().take(400).collect::<String>()
        )
    } else {
        String::new()
    };
    format!("{operation} · {state}\n{cause}{detail}")
}

fn receipt_guidance(tool: &str) -> &'static str {
    match tool {
        "delete_path" => {
            "请检查原目标是否还在。删除目录时还要检查内部文件是否被部分删除；仅凭目录仍在，不能认定完全没有变化。"
        }
        "write_file" | "str_replace" => {
            "请打开目标文件，对照预期内容及操作前版本，确认是否写入成功或只写入了一部分。"
        }
        "move_path" => "请同时检查原位置和目标位置，确认文件是否移动成功。",
        _ => "请检查操作对应的文件、应用或网站记录。命令报错或没有输出，并不代表它没有产生效果。",
    }
}

#[cfg(test)]
mod recovery_tests {
    use super::*;

    fn page(receipts: Vec<ToolReceipt>) -> ReceiptPage {
        ReceiptPage {
            receipts,
            next_cursor: String::new(),
            has_more: false,
            consistency: "live_keyset".into(),
        }
    }

    fn loaded() -> ReceiptBrowser {
        let mut browser = ReceiptBrowser::default();
        browser
            .accept_page("session", None, 4, true, page(vec![receipt()]))
            .unwrap();
        browser
    }

    fn receipt() -> ToolReceipt {
        serde_json::from_value(serde_json::json!({
            "sessionId": "session", "runId": "run", "toolCallId": "call",
            "toolName": "delete_path", "argumentsDigest": "a".repeat(64),
            "argumentsSummary": "{path:string}", "configVersion": 1,
            "state": "uncertain", "version": 3,
            "error": "removeat server_key: Access is denied."
        }))
        .unwrap()
    }

    #[test]
    fn receipt_recovery_explains_permission_without_claiming_no_effect() {
        let summary = receipt_summary(&receipt());
        assert!(summary.contains("删除文件或目录"));
        assert!(summary.contains("server_key"));
        assert!(summary.contains("不能证明"));
        assert!(!summary.contains("network"));
        assert!(receipt_guidance("delete_path").contains("部分删除"));
    }

    #[test]
    fn receipt_recovery_preserves_evidence_and_rejects_stale_reviews() {
        let mut browser = ReceiptBrowser::default();
        let r = receipt();
        browser
            .accept_page(
                "session",
                None,
                4,
                true,
                ReceiptPage {
                    receipts: vec![r.clone()],
                    next_cursor: String::new(),
                    has_more: false,
                    consistency: "live_keyset".into(),
                },
            )
            .unwrap();
        assert_eq!(browser.rows[0].receipt.error, r.error);
        assert!(browser.rows[0].evidence.contains("server_key"));
        assert!(!browser.is_empty());
        let review = ReceiptReconciliation {
            run_id: "run".into(),
            tool_call_id: "call".into(),
            expected_version: 3,
            outcome: ReceiptOutcome::NoEffect,
            reviewer: "operator".into(),
            note: "Checked original target".into(),
        };
        assert!(browser.accept_review("session", &review, r).is_err());
        assert!(!browser.is_empty());
        let mut reviewed = receipt();
        reviewed.state = "no_effect".into();
        reviewed.version = 4;
        reviewed.review = Some(serde_json::json!({
            "reviewer": "operator", "note": "Checked original target", "outcome": "no_effect"
        }));
        browser.accept_review("session", &review, reviewed).unwrap();
        assert!(
            browser.page.is_none(),
            "a new first-page query must establish completion"
        );
        assert!(
            !browser.is_empty(),
            "missing data is not an empty successful query"
        );
    }

    #[test]
    fn receipt_review_builds_one_exact_submission_for_either_outcome() {
        let browser = loaded();
        let target = ReviewTarget::from(&receipt());
        for outcome in [ReceiptOutcome::Completed, ReceiptOutcome::NoEffect] {
            let action = browser
                .review_action(
                    "session",
                    &target,
                    outcome.clone(),
                    " local-user ",
                    " Checked the actual file. \n",
                )
                .unwrap();
            let Action::ReceiptReconcile { review } = action else {
                panic!("a result choice must submit only a receipt review");
            };
            assert_eq!(review.run_id, "run");
            assert_eq!(review.tool_call_id, "call");
            assert_eq!(review.expected_version, 3);
            assert_eq!(review.outcome, outcome);
            assert_eq!(review.reviewer, "local-user");
            assert_eq!(review.note, "Checked the actual file.");
        }
    }

    #[test]
    fn receipt_review_requires_real_bounded_fields() {
        let browser = loaded();
        let target = ReviewTarget::from(&receipt());
        for (reviewer, note) in [
            ("", "Checked the file"),
            (" \n", "Checked the file"),
            ("local-user", ""),
            ("local-user", " \t\n"),
            (&"r".repeat(257), "Checked the file"),
            ("local-user", &"n".repeat(16385)),
            ("local-user", &"核".repeat(5462)),
        ] {
            assert!(
                browser
                    .review_action(
                        "session",
                        &target,
                        ReceiptOutcome::Completed,
                        reviewer,
                        note
                    )
                    .is_err()
            );
        }
        assert!(valid_review_fields(&"r".repeat(256), &"n".repeat(16384)));
    }

    #[test]
    fn receipt_review_rejects_foreign_replaced_terminal_and_stale_targets() {
        let mut browser = loaded();
        let target = ReviewTarget::from(&receipt());
        let review = |browser: &ReceiptBrowser, session: &str, target: &ReviewTarget| {
            browser.review_action(
                session,
                target,
                ReceiptOutcome::NoEffect,
                "local-user",
                "Checked the original target",
            )
        };
        assert!(review(&browser, "other-session", &target).is_err());
        for wrong in [
            ReviewTarget {
                session_id: "other-session".into(),
                ..target.clone()
            },
            ReviewTarget {
                run_id: "other-run".into(),
                ..target.clone()
            },
            ReviewTarget {
                tool_call_id: "other-call".into(),
                ..target.clone()
            },
            ReviewTarget {
                expected_version: 4,
                ..target.clone()
            },
        ] {
            assert!(review(&browser, "session", &wrong).is_err());
        }
        browser.invalidate();
        assert!(review(&browser, "session", &target).is_err());
        let mut updated = receipt();
        updated.version += 1;
        browser
            .accept_page("session", None, 4, true, page(vec![updated]))
            .unwrap();
        assert!(review(&browser, "session", &target).is_err());
        let mut terminal = receipt();
        terminal.state = "completed".into();
        browser
            .accept_page("session", None, 4, false, page(vec![terminal]))
            .unwrap();
        assert!(review(&browser, "session", &target).is_err());
        browser.clear();
        assert!(review(&browser, "session", &target).is_err());
    }

    #[test]
    fn receipt_review_disables_duplicates_and_only_queries_after_saving() {
        let mut browser = super::super::tests::browser();
        browser.capabilities.tool_receipts = true;
        browser.receipts = loaded();
        browser.recovery_required = true;
        let target = ReviewTarget::from(&receipt());
        let Action::ReceiptReconcile { review } = browser
            .receipts
            .review_action(
                "session",
                &target,
                ReceiptOutcome::NoEffect,
                "local-user",
                "Checked original target",
            )
            .unwrap()
        else {
            panic!("expected review")
        };
        assert!(browser.receipt_review_available());
        let command = browser
            .begin(Action::ReceiptReconcile {
                review: review.clone(),
            })
            .unwrap();
        assert!(!browser.receipt_review_available());
        assert!(browser.begin(command.action.clone()).is_none());
        let mut reviewed = receipt();
        reviewed.state = "no_effect".into();
        reviewed.version += 1;
        reviewed.review = Some(serde_json::json!({
            "reviewer": review.reviewer, "note": review.note, "outcome": "no_effect"
        }));
        browser
            .accept(&command, Response::Reconciled { receipt: reviewed })
            .unwrap();
        assert!(browser.recovery_required);
        assert!(!browser.receipt_review_available());
        assert_eq!(browser.next, Some(list_action(None, true)));
        let command = browser.begin(browser.next.clone().unwrap()).unwrap();
        browser
            .accept(&command, Response::Receipts(page(vec![])))
            .unwrap();
        assert!(!browser.recovery_required);
        assert!(browser.receipts.is_empty());
        assert!(
            browser.next.is_none(),
            "saving must never resume or retry tools"
        );
    }

    #[test]
    fn receipt_review_requires_connection_capability_and_no_queued_work() {
        let mut browser = super::super::tests::browser();
        browser.receipts = loaded();
        assert!(!browser.receipt_review_available());
        browser.capabilities.tool_receipts = true;
        assert!(browser.receipt_review_available());
        browser.next = Some(list_action(None, true));
        assert!(!browser.receipt_review_available());
        browser.next = None;
        browser.queued_action = Some(list_action(None, true));
        assert!(!browser.receipt_review_available());
        browser.queued_action = None;
        browser.save_selecting = true;
        assert!(!browser.receipt_review_available());
        browser.save_selecting = false;
        browser.connection.clear();
        assert!(!browser.receipt_review_available());
    }

    #[test]
    fn receipt_review_ui_has_inline_steps_without_entry_or_second_confirmation() {
        let source = include_str!("receipts.rs");
        let implementation = source.rsplit_once("impl Waku {").unwrap().1;
        assert!(!implementation.contains("open_receipt_review"));
        assert!(!implementation.contains("prepare_receipt_review"));
        assert!(!implementation.contains("inputs.confirmation"));
        assert!(implementation.contains("1. 检查并填写说明"));
        assert!(implementation.contains("2. 选择实际结果并保存"));
        assert!(implementation.contains("this.submit_receipt_review("));
        assert!(
            include_str!("../right_panel.rs")
                .contains("self.sync_receipt_review_inputs(window, cx)")
        );
    }
}

fn validate(receipt: &ToolReceipt, session: &str) -> anyhow::Result<SharedString> {
    anyhow::ensure!(
        receipt.session_id == session
            && bounded(&receipt.run_id, 256)
            && bounded(&receipt.tool_call_id, 1024)
            && bounded(&receipt.tool_name, 256)
            && digest(&receipt.arguments_digest)
            && version(receipt.version)
            && receipt.version > 0
            && receipt.arguments_summary.len() <= 8192
            && receipt.error.len() <= 8192
            && matches!(
                receipt.state.as_str(),
                "pending"
                    | "started"
                    | "completed"
                    | "failed"
                    | "not_executed"
                    | "no_effect"
                    | "uncertain"
            ),
        "工具回执身份或状态无效"
    );
    let evidence = serde_json::to_string_pretty(receipt)?;
    anyhow::ensure!(evidence.len() <= 1024 * 1024, "工具回执证据超过显示上限");
    Ok(evidence.into())
}

impl ReceiptBrowser {
    pub(super) fn invalidate(&mut self) {
        self.fresh = false;
    }

    fn review_action(
        &self,
        session: &str,
        target: &ReviewTarget,
        outcome: ReceiptOutcome,
        reviewer: &str,
        note: &str,
    ) -> anyhow::Result<Action> {
        anyhow::ensure!(
            self.fresh
                && target.session_id == session
                && self
                    .rows
                    .iter()
                    .any(|row| row.target == *target && row.receipt.state == "uncertain"),
            "操作记录已变化，请刷新后重新核对。"
        );
        anyhow::ensure!(
            valid_review_fields(reviewer, note),
            "请填写核对人（最多 256 字节）和实际核实说明（最多 16KB）。"
        );
        Ok(Action::ReceiptReconcile {
            review: ReceiptReconciliation {
                run_id: target.run_id.clone(),
                tool_call_id: target.tool_call_id.clone(),
                expected_version: target.expected_version,
                outcome,
                reviewer: reviewer.trim().to_owned(),
                note: note.trim().to_owned(),
            },
        })
    }

    pub(super) fn is_empty(&self) -> bool {
        self.page
            .as_ref()
            .is_some_and(|page| page.receipts.is_empty())
    }
    pub(super) fn clear(&mut self) {
        *self = Self::default();
    }
    pub(super) fn accept_page(
        &mut self,
        session: &str,
        cursor: Option<&str>,
        limit: u32,
        uncertain_only: bool,
        page: ReceiptPage,
    ) -> anyhow::Result<()> {
        anyhow::ensure!(
            limit <= RECEIPT_PAGE_SIZE
                && page.receipts.len() <= limit as usize
                && page.consistency == "live_keyset"
                && page.next_cursor.len() <= 8192
                && page.has_more == !page.next_cursor.is_empty()
                && (!page.has_more || page.receipts.len() == limit as usize)
                && cursor
                    .filter(|c| !c.is_empty())
                    .is_none_or(|c| self.uncertain_only == uncertain_only
                        && self
                            .page
                            .as_ref()
                            .is_some_and(|p| p.has_more && p.next_cursor == c)),
            "工具回执分页已变化，请刷新"
        );
        let mut rows = Vec::with_capacity(page.receipts.len());
        let mut previous = if cursor.filter(|c| !c.is_empty()).is_some() {
            self.rows
                .last()
                .map(|r| (&r.receipt.run_id, &r.receipt.tool_call_id))
        } else {
            None
        };
        for receipt in &page.receipts {
            let id = (&receipt.run_id, &receipt.tool_call_id);
            anyhow::ensure!(
                previous.is_none_or(|p| p < id)
                    && (!uncertain_only || receipt.state == "uncertain"),
                "工具回执顺序或筛选无效"
            );
            rows.push(CachedReceipt {
                receipt: receipt.clone(),
                target: ReviewTarget::from(receipt),
                evidence: validate(receipt, session)?,
                summary: receipt_summary(receipt).into(),
                guidance: receipt_guidance(&receipt.tool_name),
            });
            previous = Some(id);
        }
        self.rows = rows; // Keep one page, so arbitrarily long sessions stay bounded.
        self.uncertain_only = uncertain_only;
        self.page = Some(page);
        self.fresh = true;
        self.reviews.retain(|inputs| {
            self.rows
                .iter()
                .any(|row| row.receipt.state == "uncertain" && row.target == inputs.target)
        });
        Ok(())
    }
    pub(super) fn accept_review(
        &mut self,
        session: &str,
        review: &ReceiptReconciliation,
        receipt: ToolReceipt,
    ) -> anyhow::Result<()> {
        validate(&receipt, session)?;
        let outcome = match review.outcome {
            ReceiptOutcome::Completed => "completed",
            ReceiptOutcome::NoEffect => "no_effect",
        };
        anyhow::ensure!(
            receipt.run_id == review.run_id
                && receipt.tool_call_id == review.tool_call_id
                && receipt.version == review.expected_version + 1
                && receipt.state == outcome
                && receipt
                    .review
                    .as_ref()
                    .is_some_and(|r| r["reviewer"] == review.reviewer
                        && r["note"] == review.note
                        && r["outcome"] == outcome),
            "核对响应与提交的证据不一致"
        );
        // Keep other drafts only for the exact receipt/version. A fresh query
        // must establish both the next form and whether recovery is complete.
        self.reviews.retain(|inputs| {
            inputs.target.run_id != review.run_id
                || inputs.target.tool_call_id != review.tool_call_id
        });
        self.rows.clear();
        self.page = None;
        self.fresh = false;
        Ok(())
    }
}

impl TaskBrowser {
    fn receipt_review_available(&self) -> bool {
        !self.connection.is_empty()
            && self.capabilities.tool_receipts
            && self.receipts.fresh
            && self.pending.is_none()
            && self.queued_action.is_none()
            && self.next.is_none()
            && self.transfer.is_none()
            && !self.save_selecting
    }
}

impl Waku {
    pub(super) fn open_deerflow_receipts(&mut self, cx: &mut Context<Self>) {
        let Some(session) = self
            .selected_session()
            .filter(|s| s.provider == ProviderKind::DeerFlow)
        else {
            return;
        };
        let id = session.id;
        let has_cursor = session.provider_cursor.is_some();
        let browser = self.deerflow_tasks.entry(id).or_default();
        browser.show_receipts = true;
        browser.next = Some(list_action(None, true));
        self.open_right_panel_surface(RightPanelSurface::DeerFlowTasks, cx);
        if has_cursor && !self.runtimes.contains_key(&id) {
            self.start_goal_runtime(id, cx);
        }
        cx.notify();
    }

    pub(super) fn render_deerflow_receipt_recovery(&self, cx: &mut Context<Self>) -> AnyElement {
        let theme = Theme::current(cx);
        let Some(session) = self.selected_session() else {
            return task_text("请选择需要恢复的对话", theme).into_any_element();
        };
        let id = session.id;
        let mut panel = div()
            .id("df-receipt-recovery")
            .tab_group()
            .size_full()
            .overflow_y_scroll()
            .flex()
            .flex_col()
            .gap(px(10.0))
            .p(px(12.0))
            .child(task_button(
                "df-receipt-back",
                "返回运行状态",
                false,
                cx,
                |this, _, cx| this.open_deerflow_tasks(cx),
            ));
        if let Some(browser) = self.deerflow_tasks.get(&id) {
            if browser.connection.is_empty() {
                panel = panel
                    .child(task_text(
                        "需要连接原对话才能读取操作记录。新对话请先发送第一条消息。",
                        theme,
                    ))
                    .child(task_button(
                        "df-receipt-connect",
                        "连接原对话",
                        self.goal_runtime_starts.contains(&id) || session.provider_cursor.is_none(),
                        cx,
                        |this, _, cx| this.open_deerflow_receipts(cx),
                    ));
            }
            if let Some(error) = &browser.error {
                panel = panel.child(task_text(error.clone(), theme));
            }
            panel = panel.child(self.render_deerflow_receipts(id, browser, cx));
        }
        panel.into_any_element()
    }

    pub(in crate::app) fn sync_receipt_review_inputs(
        &mut self,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Some(session) = self.state.selected_session else {
            return;
        };
        let Some(browser) = self.deerflow_tasks.get(&session) else {
            return;
        };
        if !browser.show_receipts || !browser.receipt_review_available() {
            return;
        }
        // At most one bounded page of inputs is created, never per frame or
        // with a focus change that would interrupt typing in the conversation.
        let missing: Vec<_> = browser
            .receipts
            .rows
            .iter()
            .filter(|row| {
                row.receipt.state == "uncertain"
                    && !browser
                        .receipts
                        .reviews
                        .iter()
                        .any(|inputs| inputs.target == row.target)
            })
            .map(|row| (row.target.clone(), row.evidence.clone()))
            .collect();
        for (target, text) in missing {
            let reviewer = cx.new(|cx| TextInput::new(window, cx).placeholder("核对人"));
            reviewer.update(cx, |input, cx| input.set_content("local-user", cx));
            let note = cx.new(|cx| {
                TextInput::new(window, cx)
                    .multi_line()
                    .placeholder("填写实际检查的位置、观察到的结果及判断依据")
            });
            let evidence = cx.new(|cx| TextInput::new(window, cx).multi_line().read_only(true));
            evidence.update(cx, |input, cx| input.set_content(text, cx));
            let subscriptions = vec![
                cx.observe(&reviewer, |_, _, cx| cx.notify()),
                cx.observe(&note, |_, _, cx| cx.notify()),
            ];
            self.deerflow_tasks
                .get_mut(&session)
                .unwrap()
                .receipts
                .reviews
                .push(ReviewInputs {
                    target,
                    reviewer,
                    note,
                    evidence,
                    show_evidence: false,
                    _subscriptions: subscriptions,
                });
        }
    }

    fn submit_receipt_review(
        &mut self,
        session: Uuid,
        target: &ReviewTarget,
        expected_note: &Entity<TextInput>,
        outcome: ReceiptOutcome,
        cx: &mut Context<Self>,
    ) {
        if self.state.selected_session != Some(session) {
            return;
        }
        let Some(browser) = self.deerflow_tasks.get(&session) else {
            return;
        };
        if !browser.show_receipts || !browser.receipt_review_available() {
            return;
        }
        let Some(inputs) = browser
            .receipts
            .reviews
            .iter()
            .find(|inputs| inputs.target == *target && inputs.note == *expected_note)
        else {
            return;
        };
        let action = browser.receipts.review_action(
            &browser.session,
            target,
            outcome,
            inputs.reviewer.read(cx).content(),
            inputs.note.read(cx).content(),
        );
        match action {
            Ok(action) => self.request_deerflow_task(session, action, cx),
            Err(error) => {
                self.deerflow_tasks.get_mut(&session).unwrap().error = Some(error.to_string());
                cx.notify();
            }
        }
    }

    pub(super) fn render_deerflow_receipts(
        &self,
        session: Uuid,
        browser: &TaskBrowser,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let theme = Theme::current(cx);
        let disabled = browser.controls_busy()
            || browser.next.is_some()
            || browser.transfer.is_some()
            || browser.save_selecting
            || browser.connection.is_empty()
            || !browser.capabilities.tool_receipts;
        let mut panel = div().id("df-tool-receipts").tab_group().flex().flex_col().gap(px(8.0))
            .child(task_text("确认工具结果", theme))
            .child(task_text("上一次操作出错或中断后，可能已经产生了效果。为避免重复操作，需要先确认实际结果。", theme))
            .child(task_text("每项只需两步：检查并填写说明 → 选择实际结果并保存。全部核对完成后，重新发送消息继续；这里不会重试任何工具。", theme))
            .child(div().flex().flex_wrap().gap(px(6.0))
                .child(task_button("df-receipts-load", "刷新待确认操作", disabled, cx, move |this, _, cx| this.request_deerflow_task(session, list_action(None, true), cx)))
                .child(task_button("df-receipts-all", "查看全部操作记录", disabled, cx, move |this, _, cx| this.request_deerflow_task(session, list_action(None, false), cx))));
        if browser.pending.is_some()
            || browser.next.is_some()
                && !browser.connection.is_empty()
                && browser.capabilities.tool_receipts
        {
            panel = panel.child(task_text("正在读取或保存，请稍候…", theme));
        }
        if !browser.capabilities.tool_receipts && !browser.connection.is_empty() {
            panel = panel.child(task_text(
                "当前服务未提供分页回执核对能力，请升级 Go harness。",
                theme,
            ));
        }
        if let Some(page) = &browser.receipts.page {
            if page.receipts.is_empty() {
                panel = panel.child(task_text(
                    if browser.receipts.uncertain_only {
                        if browser.pending.is_some() || browser.next.is_some() || browser.error.is_some() {
                            "请等待查询完成；如果读取失败，请重新连接或刷新后再确认是否可以继续。"
                        } else if browser.recovery_required {
                            "本页没有更多操作。请点击「刷新待确认操作」检查是否还有未处理的项目。"
                        } else {
                            "已全部核对，没有待确认的操作。你可以回到输入框，重新发送消息继续；不会自动重发之前的消息，也不会修复文件权限等原始问题。"
                        }
                    } else {
                        "当前没有操作记录。"
                    },
                    theme,
                ));
            }
            for (index, row) in browser.receipts.rows.iter().enumerate() {
                let evidence = row.evidence.clone();
                let mut item = div()
                    .flex_none()
                    .flex()
                    .flex_col()
                    .gap(px(8.0))
                    .p(px(10.0))
                    .rounded(px(6.0))
                    .border_1()
                    .border_color(theme.border)
                    .bg(theme.raised)
                    .child(task_text(row.summary.clone(), theme).text_color(theme.text));
                if row.receipt.state == "uncertain" {
                    if let Some(inputs) = browser
                        .receipts
                        .reviews
                        .iter()
                        .find(|inputs| inputs.target == row.target)
                    {
                        item = item.child(
                            self.render_receipt_review(session, index, row, inputs, browser, cx),
                        );
                    } else {
                        item = item.child(task_text("正在准备核对表单…", theme));
                    }
                }
                item = item.child(task_button(
                    SharedString::from(format!("df-receipt-copy-{index}")),
                    "复制技术详情",
                    false,
                    cx,
                    move |_, _, cx| {
                        cx.write_to_clipboard(ClipboardItem::new_string(evidence.to_string()))
                    },
                ));
                panel = panel.child(item);
            }
            if page.has_more {
                let action = list_action(
                    Some(page.next_cursor.clone()),
                    browser.receipts.uncertain_only,
                );
                panel = panel.child(task_button(
                    "df-receipts-next",
                    "下一页操作",
                    disabled,
                    cx,
                    move |this, _, cx| this.request_deerflow_task(session, action.clone(), cx),
                ));
            }
        }
        panel.into_any_element()
    }

    fn render_receipt_review(
        &self,
        session: Uuid,
        index: usize,
        row: &CachedReceipt,
        inputs: &ReviewInputs,
        browser: &TaskBrowser,
        cx: &mut Context<Self>,
    ) -> Div {
        let theme = Theme::current(cx);
        let disabled = !browser.receipt_review_available();
        let fields_valid = valid_review_fields(
            inputs.reviewer.read(cx).content(),
            inputs.note.read(cx).content(),
        );
        let target = inputs.target.clone();
        let expected_note = inputs.note.clone();
        let mut form = div()
            .flex()
            .flex_col()
            .gap(px(8.0))
            .child(
                task_text("1. 检查并填写说明", theme)
                    .text_color(theme.text)
                    .font_weight(FontWeight::SEMIBOLD),
            )
            .child(task_text(row.guidance, theme))
            .child(task_text(
                "你检查了哪里、看到了什么？（必填，最多 16KB，不要填写密码或密钥）",
                theme,
            ))
            .child(
                div()
                    .id(SharedString::from(format!("df-receipt-note-{index}")))
                    .h(px(100.0))
                    .overflow_y_scroll()
                    .p(px(8.0))
                    .border_1()
                    .border_color(theme.border)
                    .child(inputs.note.clone()),
            )
            .child(
                task_text("2. 选择实际结果并保存", theme)
                    .text_color(theme.text)
                    .font_weight(FontWeight::SEMIBOLD),
            )
            .child(task_text(
                "点击结果即提交，不会重试工具。只完成一部分或无法判断时，请保持暂停。",
                theme,
            ));
        let mut actions = div().flex().flex_wrap().gap(px(6.0));
        for (suffix, label, outcome) in [
            ("completed", "确认已完成", ReceiptOutcome::Completed),
            ("no-effect", "确认未产生效果", ReceiptOutcome::NoEffect),
        ] {
            let target = target.clone();
            let expected_note = expected_note.clone();
            actions = actions.child(task_button(
                SharedString::from(format!("df-receipt-{suffix}-{index}")),
                label,
                disabled || !fields_valid,
                cx,
                move |this, _, cx| {
                    this.submit_receipt_review(
                        session,
                        &target,
                        &expected_note,
                        outcome.clone(),
                        cx,
                    )
                },
            ));
        }
        form = form.child(actions).child(task_button(
            SharedString::from(format!("df-receipt-cancel-{index}")),
            "暂时无法确认，保持暂停",
            disabled,
            cx,
            move |this, _, cx| {
                if this.state.selected_session == Some(session) {
                    this.open_deerflow_tasks(cx);
                }
            },
        ));
        if browser.pending.as_ref().is_some_and(|command| {
            matches!(&command.action, Action::ReceiptReconcile { review }
                if review.run_id == target.run_id && review.tool_call_id == target.tool_call_id)
        }) {
            form = form.child(task_text("正在保存你的核对结果，请勿重复提交…", theme));
        }
        if !browser.receipts.fresh && browser.pending.is_none() {
            form = form.child(task_text("操作记录需要刷新，暂时不能提交。", theme));
        }
        form = form.child(task_button(
            SharedString::from(format!("df-receipt-toggle-details-{index}")),
            if inputs.show_evidence {
                "收起技术详情"
            } else {
                "展开技术详情（错误、执行证据与核对人）"
            },
            false,
            cx,
            move |this, _, cx| {
                if this.state.selected_session == Some(session)
                    && let Some(inputs) =
                        this.deerflow_tasks.get_mut(&session).and_then(|browser| {
                            browser.receipts.reviews.iter_mut().find(|inputs| {
                                inputs.target == target && inputs.note == expected_note
                            })
                        })
                {
                    inputs.show_evidence = !inputs.show_evidence;
                    cx.notify();
                }
            },
        ));
        if inputs.show_evidence {
            form = form
                .child(task_text("核对人（默认 local-user，可修改）", theme))
                .child(TextField::new(
                    SharedString::from(format!("df-receipt-reviewer-{index}")),
                    inputs.reviewer.clone(),
                ))
                .child(
                    div()
                        .id(SharedString::from(format!("df-receipt-evidence-{index}")))
                        .h(px(180.0))
                        .overflow_y_scroll()
                        .p(px(8.0))
                        .border_1()
                        .border_color(theme.border)
                        .child(inputs.evidence.clone()),
                );
        }
        form
    }
}
