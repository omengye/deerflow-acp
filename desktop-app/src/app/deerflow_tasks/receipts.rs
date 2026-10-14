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
    summary: SharedString,
    guidance: &'static str,
}
pub(super) struct ReviewInputs {
    index: usize,
    reviewer: Entity<TextInput>,
    note: Entity<TextInput>,
    evidence: Entity<TextInput>,
    confirmation: Option<ReceiptReconciliation>,
    show_evidence: bool,
    _subscriptions: Vec<Subscription>,
}

#[derive(Default)]
pub(super) struct ReceiptBrowser {
    page: Option<ReceiptPage>,
    rows: Vec<CachedReceipt>,
    uncertain_only: bool,
    pub(super) review: Option<ReviewInputs>,
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
                evidence: validate(receipt, session)?,
                summary: receipt_summary(receipt).into(),
                guidance: receipt_guidance(&receipt.tool_name),
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
            show_evidence: false,
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
            .child(task_text("确认工具结果", theme))
            .child(task_text("上一次操作出错或中断后，可能已经产生了效果。为避免重复操作，需要先确认实际结果。", theme))
            .child(task_text("1. 查看下方操作及原因。\n2. 检查对应文件、应用或网站，再填写你看到的结果。\n3. 确认完成后，重新发送消息继续。这里不会重试任何工具。", theme))
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
                            "没有待确认的操作。你可以回到输入框，重新发送消息继续；不会自动重发之前的消息，也不会修复文件权限等原始问题。"
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
                    .flex()
                    .flex_col()
                    .gap(px(4.0))
                    .child(task_text(row.summary.clone(), theme))
                    .child(task_button(
                        SharedString::from(format!("df-receipt-copy-{index}")),
                        "复制技术详情",
                        false,
                        cx,
                        move |_, _, cx| {
                            cx.write_to_clipboard(ClipboardItem::new_string(evidence.to_string()))
                        },
                    ));
                if row.receipt.state == "uncertain" {
                    item = item.child(task_button(
                        SharedString::from(format!("df-receipt-review-{index}")),
                        "检查并确认结果…",
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
                    "下一页操作",
                    disabled,
                    cx,
                    move |this, _, cx| this.request_deerflow_task(session, action.clone(), cx),
                ));
            }
        }
        if let Some(inputs) = &browser.receipts.review {
            if let Some(row) = browser.receipts.rows.get(inputs.index) {
                panel = panel
                    .child(task_text(row.summary.clone(), theme))
                    .child(task_text(row.guidance, theme))
                    .child(task_text("已完成：实际结果符合预期。\n完全没有变化：已核实该操作没有产生任何效果。\n只完成一部分或无法判断：暂时不要提交，保留暂停状态。确认不会修复权限或重新执行操作。", theme))
                    .child(task_button("df-receipt-toggle-details", if inputs.show_evidence { "收起技术详情" } else { "展开技术详情（错误与执行证据）" }, false, cx, move |this, _, cx| {
                        if let Some(review) = this.deerflow_tasks.get_mut(&session).and_then(|b| b.receipts.review.as_mut()) {
                            review.show_evidence = !review.show_evidence;
                        }
                        cx.notify();
                    }));
                if inputs.show_evidence {
                    panel = panel.child(
                        div()
                            .id("df-receipt-evidence")
                            .h(px(180.0))
                            .overflow_y_scroll()
                            .p(px(8.0))
                            .border_1()
                            .border_color(theme.border)
                            .child(inputs.evidence.clone()),
                    );
                }
                panel = panel
                    .child(task_text("核对人", theme))
                    .child(TextField::new(
                        "df-receipt-reviewer",
                        inputs.reviewer.clone(),
                    ))
                    .child(task_text(
                        "你检查了哪里、看到了什么？（必填，不要填写密码或密钥）",
                        theme,
                    ))
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
                            "确认保存结果（不会重试操作）",
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
                            "我已检查，操作已完成…",
                            disabled,
                            cx,
                            move |this, _, cx| {
                                this.prepare_receipt_review(session, ReceiptOutcome::Completed, cx)
                            },
                        ))
                        .child(task_button(
                            "df-receipt-no-effect",
                            "我已检查，完全没有变化…",
                            disabled,
                            cx,
                            move |this, _, cx| {
                                this.prepare_receipt_review(session, ReceiptOutcome::NoEffect, cx)
                            },
                        ));
                }
                panel = panel.child(task_button(
                    "df-receipt-cancel",
                    "暂时无法确认，保持暂停",
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
