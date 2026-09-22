//! DeerFlow session approvals. Render reads local state; provider changes run
//! on background workers and become effective only after the ACP reply.
use super::*;

#[derive(Clone, Copy)]
pub(super) struct PendingApproval {
    id: Uuid,
    mode: RuntimeMode,
    sent: bool,
}

fn approval_value(mode: RuntimeMode) -> Option<&'static str> {
    match mode {
        RuntimeMode::FullAccess => Some("allow_always"),
        RuntimeMode::Ask => Some("ask"),
        _ => None,
    }
}

fn approval_runtime_mode(value: &str) -> Option<RuntimeMode> {
    match value {
        "allow_always" | "off" => Some(RuntimeMode::FullAccess),
        "ask" | "reject_always" => Some(RuntimeMode::Ask),
        _ => None,
    }
}

impl Waku {
    pub(super) fn block_during_deerflow_approval(&mut self, cx: &mut Context<Self>) -> bool {
        if self
            .selected_session()
            .is_some_and(|session| self.pending_deerflow_approvals.contains_key(&session.id))
        {
            self.show_toast("请等待权限切换完成后再修改会话设置。");
            cx.notify();
            true
        } else {
            false
        }
    }

    pub(super) fn record_deerflow_approval(&mut self, session_id: Uuid, value: String) {
        let Some(mode) = approval_runtime_mode(&value) else {
            return;
        };
        let Some(session) = self.state.session_mut(session_id) else {
            return;
        };
        if session.provider != ProviderKind::DeerFlow {
            return;
        }
        session.runtime_mode = mode;
        self.deerflow_approval_states.insert(session_id, value);
        self.state.mark_session_dirty(session_id);
    }

    pub(super) fn set_deerflow_tool_approval(&mut self, mode: RuntimeMode, cx: &mut Context<Self>) {
        if approval_value(mode).is_none() {
            return;
        }
        let Some(session) = self.selected_session() else {
            return;
        };
        let session_id = session.id;
        if session.is_busy()
            || self.submission_preparations.contains(&session_id)
            || self.goal_runtime_starts.contains(&session_id)
            || self.pending_deerflow_approvals.contains_key(&session_id)
        {
            self.show_toast("请先等待当前操作结束，或停止任务后切换权限。");
            cx.notify();
            return;
        }
        if self
            .deerflow_approval_states
            .get(&session_id)
            .is_some_and(|value| value == "off")
        {
            self.show_toast("服务已关闭工具审批，请在 DeerFlow 设置的“工具与权限”中启用。");
            cx.notify();
            return;
        }
        // A draft has no provider session yet. This is its launch preference,
        // applied and confirmed by the driver before the first prompt.
        if session.provider_cursor.is_none() && !self.runtimes.contains_key(&session_id) {
            self.state.session_mut(session_id).unwrap().runtime_mode = mode;
            self.state.last_runtime_mode = mode;
            self.state.mark_session_dirty(session_id);
            self.save();
            cx.notify();
            return;
        }
        self.pending_deerflow_approvals.insert(
            session_id,
            PendingApproval {
                id: Uuid::new_v4(),
                mode,
                sent: false,
            },
        );
        if let Some(runtime) = self.runtimes.get(&session_id) {
            // A reattached runtime may have replayed past its initial policy
            // event. The explicit request still goes to its live ACP session.
            self.send_pending_deerflow_approval(session_id, runtime.driver.clone(), cx);
        } else {
            // Reuse the existing background-only runtime start, without
            // adding a chat turn. Its initial approval event drains this request.
            self.start_goal_runtime(session_id, cx);
            if !self.goal_runtime_starts.contains(&session_id)
                && !self.runtimes.contains_key(&session_id)
            {
                self.pending_deerflow_approvals.remove(&session_id);
            }
        }
        cx.notify();
    }

    pub(super) fn send_pending_deerflow_approval(
        &mut self,
        session_id: Uuid,
        driver: DriverHandle,
        cx: &mut Context<Self>,
    ) {
        let Some(pending) = self.pending_deerflow_approvals.get_mut(&session_id) else {
            return;
        };
        if pending.sent {
            return;
        }
        pending.sent = true;
        let request = *pending;
        let value = approval_value(request.mode).unwrap().to_owned();
        cx.spawn(async move |this, cx| {
            let result = cx
                .background_executor()
                .spawn(async move { driver.set_tool_approval(value) })
                .await;
            let _ = this.update(cx, |this, cx| {
                if !this
                    .pending_deerflow_approvals
                    .get(&session_id)
                    .is_some_and(|pending| pending.id == request.id)
                {
                    return;
                }
                this.pending_deerflow_approvals.remove(&session_id);
                match result {
                    Ok(value) if Some(value.as_str()) == approval_value(request.mode) => {
                        this.record_deerflow_approval(session_id, value);
                        this.state.last_runtime_mode = request.mode;
                        this.save();
                    }
                    Ok(_) => this.show_toast("权限切换未得到服务确认，请重新连接后重试。"),
                    Err(error) => this.show_toast(format!("权限切换失败：{error}")),
                }
                cx.notify();
            });
        })
        .detach();
    }

    pub(super) fn render_deerflow_access_control(&self, cx: &mut Context<Self>) -> AnyElement {
        let Some(session) = self.selected_session() else {
            return div().into_any_element();
        };
        let session_id = session.id;
        let confirmed = self
            .deerflow_approval_states
            .get(&session_id)
            .map(String::as_str);
        let pending = self.pending_deerflow_approvals.contains_key(&session_id);
        let busy = session.is_busy()
            || self.submission_preparations.contains(&session_id)
            || self.goal_runtime_starts.contains(&session_id);
        let disabled = confirmed == Some("off");
        let rejecting = confirmed == Some("reject_always");
        let label = if pending {
            "切换权限中…".to_owned()
        } else {
            let label = match confirmed {
                Some("off") => "Full Access · 服务关闭审批",
                Some("reject_always") => "拒绝受控操作",
                Some("ask") => "Ask · 操作前确认",
                Some("allow_always") => "Full Access",
                _ if session.runtime_mode == RuntimeMode::Ask => "Ask · 操作前确认",
                _ => "Full Access",
            };
            if confirmed.is_none() && session.provider_cursor.is_some() {
                format!("{label} · 待同步")
            } else {
                label.to_owned()
            }
        };
        let theme = Theme::current(cx);
        let handle = self.menu_handle("deerflow-tool-approval", cx);
        let weak = cx.entity().downgrade();
        let selected_mode = confirmed
            .and_then(approval_runtime_mode)
            .unwrap_or(session.runtime_mode);
        dropdown_menu(
            MenuChip::new("deerflow-tool-approval")
                .icon(selected_mode.icon(), theme.text_tertiary)
                .label(label)
                .selected(handle.is_open()),
            "deerflow-tool-approval-menu",
            &handle,
            MenuAlign::AboveLeft,
            move |_| {
                let mut items = vec![MenuItem::Header("当前会话的工具审批".into())];
                for (mode, label) in [
                    (
                        RuntimeMode::FullAccess,
                        "Full Access · 自动批准已启用的工具",
                    ),
                    (RuntimeMode::Ask, "Ask · 按配置确认受控操作"),
                ] {
                    let weak = weak.clone();
                    items.push(
                        MenuItem::new(label, move |_, cx| {
                            let _ = weak.update(cx, |this, cx| this.set_runtime_mode(mode, cx));
                        })
                        .selected(!disabled && !rejecting && selected_mode == mode)
                        .disabled(busy || pending || disabled),
                    );
                }
                items.push(MenuItem::Separator);
                items.push(MenuItem::Header("工具禁用及文件访问范围仍然有效".into()));
                if busy || pending {
                    items.push(MenuItem::Header("当前操作结束或停止任务后可切换".into()));
                }
                if disabled {
                    let weak = weak.clone();
                    items.push(MenuItem::new(
                        "前往工具与权限设置…",
                        move |_, cx| {
                            let _ =
                                weak.update(cx, |this, cx| this.open_deerflow_tools_settings(cx));
                        },
                    ));
                }
                items
            },
        )
    }
}
