//! Read-only tool evidence is validated and cached once per bounded page.
use super::*;
use waku_protocol::deerflow_tasks::{ReceiptPage, ToolReceipt};

// Escaped evidence can exceed the raw 64 KiB budget. Four receipts fit the
// shared 4 MiB control response cap; only a bounded preview is rendered.
const RECEIPT_PAGE_SIZE: u32 = 4;
const EVIDENCE_PREVIEW_CHARS: usize = 8192;

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
    preview: SharedString,
    summary: SharedString,
    expanded: bool,
}

#[derive(Default)]
pub(super) struct ReceiptBrowser {
    page: Option<ReceiptPage>,
    rows: Vec<CachedReceipt>,
    uncertain_only: bool,
    fresh: bool,
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
        "uncertain" => "结果尚不确定",
        "completed" => "已完成",
        "failed" => "执行失败",
        "no_effect" => "未产生效果",
        "not_executed" => "未执行",
        "pending" => "等待执行",
        "started" => "已开始执行",
        _ => "未知状态",
    };
    let permission_error = receipt.error.contains("Access is denied")
        || receipt.error.to_lowercase().contains("permission denied");
    let cause = if permission_error {
        "访问被拒绝，可能没有所需权限。报错本身不能证明操作完全没有产生效果。".to_owned()
    } else if receipt.error.contains("context canceled") {
        "操作被中断，可能已经完成了部分工作。".to_owned()
    } else if receipt.error.is_empty() {
        if receipt.state == "uncertain" {
            "现有执行记录不足以确定实际结果。".to_owned()
        } else {
            String::new()
        }
    } else {
        format!(
            "工具报告：{}",
            receipt.error.chars().take(400).collect::<String>()
        )
    };
    let detail = if permission_error {
        format!(
            "\n原始错误：{}",
            receipt.error.chars().take(400).collect::<String>()
        )
    } else {
        String::new()
    };
    if cause.is_empty() {
        format!("{operation} · {state}")
    } else {
        format!("{operation} · {state}\n{cause}{detail}")
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
        "工具执行记录身份或状态无效"
    );
    let evidence = serde_json::to_string_pretty(receipt)?;
    anyhow::ensure!(evidence.len() <= 1024 * 1024, "工具执行证据超过显示上限");
    Ok(evidence.into())
}

fn evidence_preview(evidence: &str) -> SharedString {
    let mut chars = evidence.chars();
    let mut preview = chars
        .by_ref()
        .take(EVIDENCE_PREVIEW_CHARS)
        .collect::<String>();
    if chars.next().is_some() {
        preview.push_str("\n…内容较长，复制技术详情可获取完整记录。");
    }
    preview.into()
}

impl ReceiptBrowser {
    pub(super) fn invalidate(&mut self) {
        self.fresh = false;
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
            limit > 0
                && limit <= RECEIPT_PAGE_SIZE
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
            "工具执行记录分页已变化，请刷新"
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
                "工具执行记录顺序或筛选无效"
            );
            let evidence = validate(receipt, session)?;
            rows.push(CachedReceipt {
                receipt: receipt.clone(),
                preview: evidence_preview(&evidence),
                evidence,
                summary: receipt_summary(receipt).into(),
                expanded: false,
            });
            previous = Some(id);
        }
        // Retain only one page so even very long conversations stay bounded.
        self.rows = rows;
        self.uncertain_only = uncertain_only;
        self.page = Some(page);
        self.fresh = true;
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
        browser.next = Some(list_action(None, false));
        self.open_right_panel_surface(RightPanelSurface::DeerFlowTasks, cx);
        if has_cursor && !self.runtimes.contains_key(&id) {
            self.start_goal_runtime(id, cx);
        }
        cx.notify();
    }

    pub(super) fn render_deerflow_receipt_history(&self, cx: &mut Context<Self>) -> AnyElement {
        let theme = Theme::current(cx);
        let Some(session) = self.selected_session() else {
            return task_text("请选择对话以查看执行记录", theme).into_any_element();
        };
        let id = session.id;
        let mut panel = div()
            .id("df-receipt-history")
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
                        "需要连接原对话才能读取执行记录。新对话请先发送第一条消息。",
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
        let mut panel = div()
            .id("df-tool-receipts")
            .tab_group()
            .flex()
            .flex_col()
            .gap(px(8.0))
            .child(task_text("工具执行记录", theme))
            .child(task_text(
                "工具失败由代理处理。需要补充信息或确认时，代理会在对话中提问，请在主输入框回复。",
                theme,
            ))
            .child(
                div()
                    .flex()
                    .flex_wrap()
                    .gap(px(6.0))
                    .child(task_button(
                        "df-receipts-load",
                        "刷新执行记录",
                        disabled,
                        cx,
                        move |this, _, cx| {
                            this.request_deerflow_task(session, list_action(None, false), cx)
                        },
                    ))
                    .child(task_button(
                        "df-receipts-uncertain",
                        "仅看结果不确定",
                        disabled,
                        cx,
                        move |this, _, cx| {
                            this.request_deerflow_task(session, list_action(None, true), cx)
                        },
                    )),
            );
        if browser.pending.is_some()
            || browser.next.is_some()
                && !browser.connection.is_empty()
                && browser.capabilities.tool_receipts
        {
            panel = panel.child(task_text("正在读取执行记录…", theme));
        }
        if !browser.capabilities.tool_receipts && !browser.connection.is_empty() {
            panel = panel.child(task_text("当前服务未提供工具执行记录。", theme));
        }
        if let Some(page) = &browser.receipts.page {
            if !browser.receipts.fresh {
                panel = panel.child(task_text(
                    "以下为上次读取的记录，可刷新获取最新状态。",
                    theme,
                ));
            }
            if page.receipts.is_empty() {
                panel = panel.child(task_text(
                    if browser.receipts.uncertain_only {
                        "本页没有结果不确定的执行记录。"
                    } else {
                        "本页没有执行记录。"
                    },
                    theme,
                ));
            }
            for (index, row) in browser.receipts.rows.iter().enumerate() {
                let evidence = row.evidence.clone();
                let target = (
                    row.receipt.run_id.clone(),
                    row.receipt.tool_call_id.clone(),
                    row.receipt.version,
                );
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
                    .child(task_text(row.summary.clone(), theme).text_color(theme.text))
                    .child(task_button(
                        SharedString::from(format!("df-receipt-details-{index}")),
                        if row.expanded {
                            "收起技术详情"
                        } else {
                            "展开技术详情"
                        },
                        false,
                        cx,
                        move |this, _, cx| {
                            if this.state.selected_session == Some(session)
                                && let Some(row) =
                                    this.deerflow_tasks.get_mut(&session).and_then(|b| {
                                        b.receipts.rows.iter_mut().find(|row| {
                                            row.receipt.run_id == target.0
                                                && row.receipt.tool_call_id == target.1
                                                && row.receipt.version == target.2
                                        })
                                    })
                            {
                                row.expanded = !row.expanded;
                                cx.notify();
                            }
                        },
                    ));
                if row.expanded {
                    item = item.child(
                        div()
                            .id(SharedString::from(format!("df-receipt-evidence-{index}")))
                            .max_h(px(240.0))
                            .overflow_y_scroll()
                            .child(task_text(row.preview.clone(), theme)),
                    );
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
                    "下一页记录",
                    disabled,
                    cx,
                    move |this, _, cx| this.request_deerflow_task(session, action.clone(), cx),
                ));
            }
        }
        panel.into_any_element()
    }
}

#[cfg(test)]
mod history_tests {
    use super::*;
    fn page(receipts: Vec<ToolReceipt>) -> ReceiptPage {
        ReceiptPage {
            receipts,
            next_cursor: String::new(),
            has_more: false,
            consistency: "live_keyset".into(),
        }
    }
    fn receipt() -> ToolReceipt {
        serde_json::from_value(serde_json::json!({
            "sessionId": "session", "runId": "run", "toolCallId": "call",
            "toolName": "delete_path", "argumentsDigest": "a".repeat(64),
            "argumentsSummary": "{path:string}", "configVersion": 1,
            "state": "uncertain", "version": 3, "error": "removeat server_key: Access is denied."
        }))
        .unwrap()
    }
    #[test]
    fn receipt_history_preserves_unknown_outcome_and_original_evidence() {
        let mut browser = ReceiptBrowser::default();
        let receipt = receipt();
        browser
            .accept_page("session", None, 4, false, page(vec![receipt.clone()]))
            .unwrap();
        let row = &browser.rows[0];
        assert_eq!(row.receipt.state, "uncertain");
        assert!(row.summary.contains("结果尚不确定"));
        assert!(row.summary.contains("不能证明"));
        assert!(row.summary.contains("server_key"));
        assert_eq!(
            serde_json::from_str::<serde_json::Value>(&row.evidence).unwrap(),
            serde_json::to_value(receipt).unwrap()
        );
        browser.invalidate();
        assert!(!browser.fresh);
        assert_eq!(browser.rows[0].receipt.state, "uncertain");
    }
    #[test]
    fn receipt_history_does_not_invent_error_for_completed_operations() {
        let mut receipt = receipt();
        receipt.state = "completed".into();
        receipt.error.clear();
        assert_eq!(receipt_summary(&receipt), "删除文件或目录 · 已完成");
        receipt.state = "uncertain".into();
        assert!(receipt_summary(&receipt).contains("不足以确定实际结果"));
    }
    #[test]
    fn receipt_history_rejects_foreign_or_invalid_pages_without_replacing_evidence() {
        let mut browser = ReceiptBrowser::default();
        browser
            .accept_page("session", None, 4, false, page(vec![receipt()]))
            .unwrap();
        let mut foreign = receipt();
        foreign.session_id = "other-session".into();
        let mut wrong_state = receipt();
        wrong_state.state = "fabricated".into();
        for invalid in [
            page(vec![foreign]),
            page(vec![wrong_state]),
            page(vec![receipt(), receipt()]),
        ] {
            assert!(
                browser
                    .accept_page("session", None, 4, false, invalid)
                    .is_err()
            );
            assert_eq!(browser.rows.len(), 1);
            assert_eq!(browser.rows[0].receipt.state, "uncertain");
        }
        let mut completed = receipt();
        completed.state = "completed".into();
        assert!(
            browser
                .accept_page("session", None, 4, true, page(vec![completed]))
                .is_err()
        );
    }
    #[test]
    fn receipt_history_uses_a_bounded_page_and_validates_cursor_and_filter() {
        let mut browser = ReceiptBrowser::default();
        let first = ReceiptPage {
            next_cursor: "next".into(),
            has_more: true,
            ..page(vec![receipt()])
        };
        browser
            .accept_page("session", None, 1, true, first)
            .unwrap();
        let mut next = receipt();
        next.tool_call_id = "call-2".into();
        assert!(
            browser
                .accept_page("session", Some("wrong"), 1, true, page(vec![next.clone()]))
                .is_err()
        );
        assert!(
            browser
                .accept_page("session", Some("next"), 1, false, page(vec![next.clone()]))
                .is_err()
        );
        assert!(
            browser
                .accept_page("session", Some("next"), 1, true, page(vec![receipt()]))
                .is_err()
        );
        browser
            .accept_page("session", Some("next"), 1, true, page(vec![next]))
            .unwrap();
        assert_eq!(browser.rows.len(), 1);
        assert_eq!(browser.rows[0].receipt.tool_call_id, "call-2");
        assert!(
            browser
                .accept_page("session", None, 0, false, page(vec![]))
                .is_err()
        );
        assert!(
            browser
                .accept_page("session", None, 5, false, page(vec![]))
                .is_err()
        );
    }
    #[test]
    fn receipt_history_bounds_rendered_preview_without_truncating_copied_evidence() {
        let mut receipt = receipt();
        receipt.review = Some(serde_json::json!({"legacyEvidence": "核".repeat(20_000)}));
        let mut browser = ReceiptBrowser::default();
        browser
            .accept_page("session", None, 4, false, page(vec![receipt]))
            .unwrap();
        let row = &browser.rows[0];
        assert!(row.preview.chars().count() < EVIDENCE_PREVIEW_CHARS + 50);
        assert!(row.preview.contains("复制技术详情"));
        assert!(row.evidence.contains(&"核".repeat(20_000)));
    }
}
