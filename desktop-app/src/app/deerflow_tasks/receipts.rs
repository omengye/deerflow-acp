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
    evidence: SharedString,
}
pub(super) struct ReviewInputs {
    index: usize,
    reviewer: Entity<TextInput>,
    note: Entity<TextInput>,
    evidence: Entity<TextInput>,
    confirmation: Option<ReceiptReconciliation>,
    _subscriptions: Vec<Subscription>,
}

#[derive(Default)]
pub(super) struct ReceiptBrowser {
    page: Option<ReceiptPage>,
    rows: Vec<CachedReceipt>,
    uncertain_only: bool,
    pub(super) review: Option<ReviewInputs>,
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
                evidence: validate(receipt, session)?,
            });
            previous = Some(id);
        }
        self.rows = rows; // Keep one page, so arbitrarily long sessions stay bounded.
        self.uncertain_only = uncertain_only;
        self.page = Some(page);
        self.review = None;
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
        self.clear();
        Ok(())
    }
}

impl Waku {
    fn open_receipt_review(
        &mut self,
        session: Uuid,
        index: usize,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Some(browser) = self.deerflow_tasks.get(&session) else {
            return;
        };
        if browser.pending.is_some()
            || browser.connection.is_empty()
            || browser
                .receipts
                .rows
                .get(index)
                .is_none_or(|r| r.receipt.state != "uncertain")
        {
            return;
        }
        let reviewer = cx.new(|cx| TextInput::new(window, cx).placeholder("核对人"));
        reviewer.update(cx, |input, cx| input.set_content("local-user", cx));
        let note = cx.new(|cx| {
            TextInput::new(window, cx)
                .multi_line()
                .placeholder("填写实际检查的位置、观察到的结果及判断依据")
        });
        let evidence = cx.new(|cx| TextInput::new(window, cx).multi_line().read_only(true));
        evidence.update(cx, |input, cx| {
            input.set_content(browser.receipts.rows[index].evidence.clone(), cx)
        });
        let subscriptions = vec![
            cx.observe(&reviewer, |_, _, cx| cx.notify()),
            cx.observe(&note, |_, _, cx| cx.notify()),
        ];
        window.focus(&note.read(cx).focus(), cx);
        self.deerflow_tasks
            .get_mut(&session)
            .unwrap()
            .receipts
            .review = Some(ReviewInputs {
            index,
            reviewer,
            note,
            evidence,
            confirmation: None,
            _subscriptions: subscriptions,
        });
        cx.notify();
    }

    fn prepare_receipt_review(
        &mut self,
        session: Uuid,
        outcome: ReceiptOutcome,
        cx: &mut Context<Self>,
    ) {
        let Some(browser) = self.deerflow_tasks.get_mut(&session) else {
            return;
        };
        let Some(inputs) = browser.receipts.review.as_mut() else {
            return;
        };
        let Some(row) = browser.receipts.rows.get(inputs.index) else {
            return;
        };
        let reviewer = inputs.reviewer.read(cx).content().trim().to_owned();
        let note = inputs.note.read(cx).content().trim().to_owned();
        if reviewer.is_empty() || reviewer.len() > 256 || note.is_empty() || note.len() > 16384 {
            browser.error =
                Some("请填写核对人（最多 256 字节）和实际核实说明（最多 16KB）。".into());
        } else {
            inputs.confirmation = Some(ReceiptReconciliation {
                run_id: row.receipt.run_id.clone(),
                tool_call_id: row.receipt.tool_call_id.clone(),
                expected_version: row.receipt.version,
                outcome,
                reviewer,
                note,
            });
            browser.error = None;
        }
        cx.notify();
    }

    pub(super) fn render_deerflow_receipts(
        &self,
        session: Uuid,
        browser: &TaskBrowser,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let theme = Theme::current(cx);
        let disabled = browser.pending.is_some()
            || browser.transfer.is_some()
            || browser.connection.is_empty()
            || !browser.capabilities.tool_receipts;
        let mut panel = div().id("df-tool-receipts").tab_group().flex().flex_col().gap(px(8.0))
            .child(task_text("工具结果核对", theme))
            .child(task_text("结果不确定的调用需要先核实实际效果。核对只保存说明和结论，不重新执行工具；全部核对完成后可发送新消息。", theme))
            .child(div().flex().flex_wrap().gap(px(6.0))
                .child(task_button("df-receipts-load", "读取待核对回执", disabled, cx, move |this, _, cx| this.request_deerflow_task(session, list_action(None, true), cx)))
                .child(task_button("df-receipts-all", "查看全部工具回执", disabled, cx, move |this, _, cx| this.request_deerflow_task(session, list_action(None, false), cx))));
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
                        "当前没有待核对回执。"
                    } else {
                        "当前没有工具回执。"
                    },
                    theme,
                ));
            }
            for (index, row) in browser.receipts.rows.iter().enumerate() {
                let evidence = row.evidence.clone();
                let mut item = div()
                    .flex()
                    .flex_col()
                    .gap(px(4.0))
                    .child(task_text(
                        format!(
                            "{} · {} · v{}\nrun: {}\ncall: {}",
                            row.receipt.tool_name,
                            row.receipt.state,
                            row.receipt.version,
                            row.receipt.run_id,
                            row.receipt.tool_call_id
                        ),
                        theme,
                    ))
                    .child(task_button(
                        SharedString::from(format!("df-receipt-copy-{index}")),
                        "复制完整证据",
                        false,
                        cx,
                        move |_, _, cx| {
                            cx.write_to_clipboard(ClipboardItem::new_string(evidence.to_string()))
                        },
                    ));
                if row.receipt.state == "uncertain" {
                    item = item.child(task_button(
                        SharedString::from(format!("df-receipt-review-{index}")),
                        "查看并核对此调用…",
                        disabled,
                        cx,
                        move |this, window, cx| {
                            this.open_receipt_review(session, index, window, cx)
                        },
                    ));
                }
                panel = panel.child(item);
            }
            if page.has_more {
                let action = list_action(
                    Some(page.next_cursor.clone()),
                    browser.receipts.uncertain_only,
                );
                panel = panel.child(task_button(
                    "df-receipts-next",
                    "下一页回执",
                    disabled,
                    cx,
                    move |this, _, cx| this.request_deerflow_task(session, action.clone(), cx),
                ));
            }
        }
        if let Some(inputs) = &browser.receipts.review {
            if browser.receipts.rows.get(inputs.index).is_some() {
                panel = panel
                    .child(
                        div()
                            .id("df-receipt-evidence")
                            .h(px(180.0))
                            .overflow_y_scroll()
                            .p(px(8.0))
                            .border_1()
                            .border_color(theme.border)
                            .child(inputs.evidence.clone()),
                    )
                    .child(task_text("核对人", theme))
                    .child(TextField::new(
                        "df-receipt-reviewer",
                        inputs.reviewer.clone(),
                    ))
                    .child(task_text("实际核实说明（必填）", theme))
                    .child(
                        div()
                            .id("df-receipt-note")
                            .h(px(100.0))
                            .overflow_y_scroll()
                            .p(px(8.0))
                            .border_1()
                            .border_color(theme.border)
                            .child(inputs.note.clone()),
                    );
                if let Some(review) = &inputs.confirmation {
                    let action = Action::ReceiptReconcile {
                        review: review.clone(),
                    };
                    panel = panel
                        .child(task_text(
                            format!(
                                "即将记录：{}\n核对人：{}\n依据：{}",
                                match review.outcome {
                                    ReceiptOutcome::Completed => "已确认完成",
                                    ReceiptOutcome::NoEffect => "已确认未产生效果",
                                },
                                review.reviewer,
                                review.note
                            ),
                            theme,
                        ))
                        .child(task_button(
                            "df-receipt-submit",
                            "提交这份核对",
                            disabled,
                            cx,
                            move |this, _, cx| {
                                this.request_deerflow_task(session, action.clone(), cx)
                            },
                        ))
                        .child(task_button(
                            "df-receipt-edit",
                            "返回编辑",
                            disabled,
                            cx,
                            move |this, _, cx| {
                                if let Some(inputs) = this
                                    .deerflow_tasks
                                    .get_mut(&session)
                                    .and_then(|b| b.receipts.review.as_mut())
                                {
                                    inputs.confirmation = None;
                                }
                                cx.notify();
                            },
                        ));
                } else {
                    panel = panel
                        .child(task_button(
                            "df-receipt-completed",
                            "记录已确认完成…",
                            disabled,
                            cx,
                            move |this, _, cx| {
                                this.prepare_receipt_review(session, ReceiptOutcome::Completed, cx)
                            },
                        ))
                        .child(task_button(
                            "df-receipt-no-effect",
                            "记录已确认未产生效果…",
                            disabled,
                            cx,
                            move |this, _, cx| {
                                this.prepare_receipt_review(session, ReceiptOutcome::NoEffect, cx)
                            },
                        ));
                }
                panel = panel.child(task_button(
                    "df-receipt-cancel",
                    "取消核对",
                    disabled,
                    cx,
                    move |this, _, cx| {
                        this.deerflow_tasks
                            .get_mut(&session)
                            .unwrap()
                            .receipts
                            .review = None;
                        cx.notify();
                    },
                ));
            }
        }
        panel.into_any_element()
    }
}
