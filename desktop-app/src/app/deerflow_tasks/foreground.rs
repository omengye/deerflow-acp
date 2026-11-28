use super::*;

/// The execution that owns the chat input. The management panel may be looking
/// at a completely different run in the same session.
#[derive(Default)]
pub(super) struct ForegroundRecovery {
    waiting: Option<Execution>,
    initial_lookup_complete: bool,
    pub(super) refresh_needed: bool,
    pub(super) attach_requested: bool,
    lookup_error: Option<String>,
    cancel_error: Option<String>,
}

impl ForegroundRecovery {
    pub(super) fn connected(executions: bool) -> Self {
        Self {
            refresh_needed: executions,
            attach_requested: true,
            initial_lookup_complete: !executions,
            ..Self::default()
        }
    }

    pub(super) fn disconnect(&mut self) {
        self.refresh_needed = false;
        self.initial_lookup_complete = false;
        self.lookup_error = None;
    }

    fn awaiting_initial_state(&self, has_saved_session: bool) -> bool {
        has_saved_session && !self.initial_lookup_complete && self.lookup_error.is_none()
    }

    pub(super) fn connection_failed(&mut self, message: &str) {
        if self.attach_requested {
            self.refresh_needed = false;
            self.lookup_error = Some(message.chars().take(1024).collect());
        }
    }

    pub(super) fn update(&mut self, session: &str, state: Execution) -> anyhow::Result<()> {
        anyhow::ensure!(
            state.session_id == session
                && bounded(&state.run_id, 256)
                && state.version > 0
                && version(state.version)
                && bounded(&state.status, 64)
                && state.blocked_reason.len() <= 4096,
            "当前执行的身份或状态无效"
        );
        if self
            .waiting
            .as_ref()
            .is_none_or(|old| old.run_id != state.run_id || old.version != state.version)
        {
            self.cancel_error = None;
        }
        self.waiting = (state.status == "waiting_input"
            && (state.resumable || state.waiting_tasks.is_empty()))
        .then_some(state);
        self.initial_lookup_complete = true;
        self.lookup_error = None;
        Ok(())
    }

    pub(super) fn failed(&mut self, action: &Action, code: &str, message: &str) -> bool {
        if matches!(action, Action::ExecutionGet {run_id} if run_id.is_empty()) {
            if code == "no_execution" {
                self.initial_lookup_complete = true;
                self.waiting = None;
                self.lookup_error = None;
            } else {
                self.lookup_error = Some(message.chars().take(1024).collect());
            }
            return true;
        }
        if matches!(
            action,
            Action::ExecutionCancel { .. } | Action::ExecutionResume { .. }
        ) {
            self.refresh_needed = true;
        }
        if matches!(action, Action::ExecutionCancel { .. }) {
            self.cancel_error = Some(message.chars().take(1024).collect());
        }
        false
    }
}

fn is_resume_message(prompt: &str) -> bool {
    let prompt = prompt.trim();
    matches!(prompt, "继续" | "继续执行" | "恢复执行")
        || prompt.eq_ignore_ascii_case("continue")
        || prompt.eq_ignore_ascii_case("resume")
}

impl TaskBrowser {
    pub(super) fn foreground_lookup_due(
        &self,
        selected: bool,
        session_idle: impl FnOnce() -> bool,
    ) -> bool {
        self.foreground.refresh_needed
            && self.capabilities.executions
            && (selected || self.foreground_queue_wait)
            && session_idle()
    }

    pub(super) fn foreground_blocks_queue(&self) -> bool {
        self.foreground.waiting.is_some()
            || self.foreground.refresh_needed
            || self.foreground.lookup_error.is_some()
            || self.pending.as_ref().is_some_and(
                |c| matches!(&c.action, Action::ExecutionGet { run_id } if run_id.is_empty()),
            )
    }

    pub(in crate::app) fn defer_queue_for_foreground(&mut self) -> bool {
        self.foreground_queue_wait = self.foreground_blocks_queue();
        self.foreground_queue_wait
    }

    fn recovery_action(&self, cancel: bool) -> Option<Action> {
        let state = self.foreground.waiting.as_ref()?;
        if self.connection.is_empty()
            || self.foreground.refresh_needed
            || self.foreground.lookup_error.is_some()
            || self.controls_busy()
            || self.transfer.is_some()
            || self.save_selecting
        {
            return None;
        }
        if cancel {
            self.capabilities
                .execution_cancel
                .then(|| Action::ExecutionCancel {
                    run_id: state.run_id.clone(),
                    expected_version: state.version,
                })
        } else {
            (self.capabilities.executions && state.resumable).then(|| Action::ExecutionResume {
                run_id: state.run_id.clone(),
                expected_version: state.version,
            })
        }
    }
}

impl Waku {
    fn run_foreground_action(&mut self, session: Uuid, action: &Action, cx: &mut Context<Self>) {
        let cancel = matches!(action, Action::ExecutionCancel { .. });
        if self
            .selected_session()
            .is_some_and(|s| s.id == session && !s.is_busy())
            && self
                .deerflow_tasks
                .get(&session)
                .and_then(|b| b.recovery_action(cancel))
                .as_ref()
                == Some(action)
        {
            self.request_deerflow_task(session, action.clone(), cx);
        }
    }

    pub(in crate::app) fn render_foreground_recovery(&self, cx: &mut Context<Self>) -> Option<Div> {
        let session = self
            .selected_session()
            .filter(|s| s.provider == ProviderKind::DeerFlow && !s.is_busy())?
            .id;
        let browser = self.deerflow_tasks.get(&session)?;
        let state = browser.foreground.waiting.as_ref();
        if state.is_none() && browser.foreground.lookup_error.is_none() {
            return None;
        }
        let theme = Theme::current(cx);
        let mut card = approvals::approval_card(
            tr!(if state.is_some() {
                "session.foreground_waiting"
            } else {
                "session.foreground_status_unavailable"
            }),
            theme,
        );
        let mut buttons = div().flex().flex_wrap().gap(px(8.0));
        if state.is_some_and(|s| s.resumable) {
            card = card.child(task_text(tr!("session.foreground_resume_hint"), theme));
        }
        if let Some(state) = state.filter(|s| !s.blocked_reason.is_empty()) {
            card = card.child(task_text(state.blocked_reason.clone(), theme));
        }
        if let Some(error) = browser
            .foreground
            .lookup_error
            .as_ref()
            .or(browser.foreground.cancel_error.as_ref())
        {
            card = card.child(task_text(error.clone(), theme).text_color(theme.danger));
        }
        if browser.connection.is_empty() || browser.foreground.lookup_error.is_some() {
            buttons = buttons.child(approvals::decision_button(
                self,
                "df-chat-recovery-refresh".into(),
                tr!("session.foreground_refresh"),
                true,
                false,
                cx,
                move |this, _, cx| {
                    if this.state.selected_session != Some(session) {
                        return;
                    }
                    if let Some(browser) = this.deerflow_tasks.get_mut(&session) {
                        browser.foreground.refresh_needed = true;
                    }
                    if this.runtimes.contains_key(&session) {
                        this.maybe_refresh_deerflow_tasks(cx);
                    } else {
                        this.start_idle_runtime(session, cx);
                    }
                },
            ));
        } else if let Some(state) = state {
            for (cancel, available, label) in [
                (false, state.resumable, tr!("session.foreground_resume")),
                (
                    true,
                    browser.capabilities.execution_cancel,
                    tr!("session.foreground_cancel"),
                ),
            ] {
                if available {
                    let action = browser.recovery_action(cancel);
                    buttons = buttons.child(approvals::decision_button(
                        self,
                        format!("df-chat-recovery-{cancel}"),
                        label,
                        !cancel,
                        action.is_none(),
                        cx,
                        move |this, _, cx| {
                            if let Some(action) = &action {
                                this.run_foreground_action(session, action, cx);
                            }
                        },
                    ));
                }
            }
        }
        Some(
            div().px(px(20.0)).pb(px(8.0)).child(
                div()
                    .w_full()
                    .max_w(px(CONTENT_MAX_WIDTH))
                    .mx_auto()
                    .child(card.child(buttons)),
            ),
        )
    }

    pub(in crate::app) fn handle_foreground_recovery_input(
        &mut self,
        prompt: &str,
        cx: &mut Context<Self>,
    ) -> bool {
        let Some(session) = self
            .selected_session()
            .filter(|s| s.provider == ProviderKind::DeerFlow && !s.is_busy())
        else {
            return false;
        };
        let id = session.id;
        let has_saved_session = session.provider_cursor.is_some();
        let failed = session.status == SessionStatus::Failed;
        let browser = self.deerflow_tasks.entry(id).or_default();
        let checking = browser.foreground.awaiting_initial_state(has_saved_session)
            || failed
                && (browser.foreground.refresh_needed
                    || self.idle_runtime_starts.contains(&id)
                    || browser.pending.as_ref().is_some_and(
                        |c| matches!(&c.action, Action::ExecutionGet {run_id} if run_id.is_empty()),
                    ));
        if browser.foreground.waiting.is_none()
            && !checking
            && !(browser.foreground.lookup_error.is_some() && is_resume_message(prompt))
        {
            return false;
        }
        if is_resume_message(prompt)
            && self.composer_attachments.is_empty()
            && let Some(action) = browser.recovery_action(false)
        {
            self.composer.update(cx, |input, cx| input.clear(cx));
            self.discard_current_composer_draft(cx);
            self.run_foreground_action(id, &action, cx);
        } else {
            // Both keyboard and button submissions retain the draft until admitted.
            let notice = tr!(if browser.foreground.lookup_error.is_some() {
                "session.foreground_refresh_draft"
            } else if checking {
                "session.foreground_checking"
            } else {
                "session.foreground_draft"
            });
            if checking {
                if browser.connection.is_empty()
                    && !self.idle_runtime_starts.contains(&id)
                    && !self.runtime_attach_pending.contains(&id)
                    && !self.runtimes.contains_key(&id)
                {
                    browser.foreground.attach_requested = false;
                }
                self.maybe_refresh_deerflow_tasks(cx);
            }
            self.show_toast(notice);
            cx.notify();
        }
        true
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::app::deerflow_tasks::tests::browser;

    fn state(run: &str, version: u64) -> Execution {
        Execution {
            session_id: "session".into(),
            run_id: run.into(),
            status: "waiting_input".into(),
            version,
            resumable: true,
            blocked_reason: String::new(),
            waiting_tasks: vec![],
        }
    }

    #[test]
    fn foreground_recovery_uses_active_run_without_panel_selection() {
        let mut b = browser();
        b.capabilities.execution_cancel = true;
        let command = b
            .begin(Action::ExecutionGet {
                run_id: String::new(),
            })
            .unwrap();
        b.accept(&command, Response::Execution(state("active", 7)))
            .unwrap();
        assert_eq!(
            b.recovery_action(false),
            Some(Action::ExecutionResume {
                run_id: "active".into(),
                expected_version: 7
            })
        );
        assert_eq!(
            b.recovery_action(true),
            Some(Action::ExecutionCancel {
                run_id: "active".into(),
                expected_version: 7
            })
        );
        b.foreground.refresh_needed = true;
        assert!(b.recovery_action(false).is_none());
        assert!(b.recovery_action(true).is_none());
        b.foreground.refresh_needed = false;
        let command = b.begin(b.recovery_action(true).unwrap()).unwrap();
        let mut cancelled = state("active", 8);
        cancelled.status = "cancelled".into();
        b.accept(&command, Response::Execution(cancelled)).unwrap();
        assert!(b.foreground.waiting.is_none());
    }

    #[test]
    fn foreground_recovery_rejects_wrong_session_and_automatic_waits() {
        let mut b = browser();
        let mut waiting = state("run", 4);
        assert!(b.foreground.update("other", waiting.clone()).is_err());
        waiting.resumable = false;
        waiting.waiting_tasks.push("background-worker".into());
        b.foreground.update("session", waiting).unwrap();
        assert!(b.foreground.waiting.is_none());
        assert!(!b.foreground.failed(
            &Action::ExecutionResume {
                run_id: "run".into(),
                expected_version: 4,
            },
            "conflict",
            "changed"
        ));
        assert!(b.foreground.refresh_needed);
        let lookup = Action::ExecutionGet {
            run_id: String::new(),
        };
        b.foreground.update("session", state("run", 5)).unwrap();
        assert!(
            b.foreground
                .failed(&lookup, "invalid_response", "malformed data")
        );
        assert!(b.foreground.waiting.is_some());
        assert!(b.foreground.lookup_error.is_some());
        assert!(b.recovery_action(false).is_none());
        assert!(b.foreground.failed(&lookup, "no_execution", "not found"));
        assert!(b.foreground.waiting.is_none());
    }

    #[test]
    fn foreground_recovery_preserves_first_input_until_reattachment_is_checked() {
        let mut recovery = ForegroundRecovery::default();
        assert!(!recovery.awaiting_initial_state(false));
        assert!(recovery.awaiting_initial_state(true));
        recovery = ForegroundRecovery::connected(true);
        assert!(recovery.awaiting_initial_state(true));
        let lookup = Action::ExecutionGet {
            run_id: String::new(),
        };
        assert!(recovery.failed(&lookup, "no_execution", "not found"));
        assert!(!recovery.awaiting_initial_state(true));
        recovery.disconnect();
        assert!(recovery.awaiting_initial_state(true));
        recovery.update("session", state("run", 2)).unwrap();
        assert!(recovery.waiting.is_some());
        assert!(!recovery.awaiting_initial_state(true));
        assert!(!ForegroundRecovery::connected(false).awaiting_initial_state(true));
    }

    #[test]
    fn foreground_recovery_only_exact_continue_requests() {
        for message in [" 继续 ", "继续执行", "恢复执行", "CONTINUE", "Resume"] {
            assert!(is_resume_message(message));
        }
        for message in [
            "",
            "继续解释一下",
            "continue with something else",
            "同意",
            "取消",
        ] {
            assert!(!is_resume_message(message));
        }
    }

    #[test]
    fn foreground_recovery_keeps_queued_input_until_discovery_completes() {
        let mut b = browser();
        b.foreground.refresh_needed = true;
        assert!(!b.foreground_lookup_due(false, || true));
        assert!(b.defer_queue_for_foreground());
        assert!(
            b.foreground_lookup_due(false, || true),
            "a queued background tab must check the run without waiting for selection"
        );
        assert!(!b.foreground_lookup_due(false, || false));
        b.foreground.refresh_needed = false;
        let lookup = Action::ExecutionGet {
            run_id: String::new(),
        };
        let command = b.begin(lookup.clone()).unwrap();
        assert!(b.defer_queue_for_foreground());
        b.accept(&command, Response::Execution(state("run", 7)))
            .unwrap();
        assert!(b.defer_queue_for_foreground());
        assert!(
            b.foreground
                .failed(&lookup, "disconnected", "connection lost")
        );
        assert!(b.defer_queue_for_foreground());
        assert!(b.foreground.failed(&lookup, "no_execution", "not found"));
        assert!(!b.defer_queue_for_foreground());
    }
}
