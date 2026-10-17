//! One decision surface for live requests, restored executions and background tasks.
use super::*;
use waku_protocol::model::PermissionOption;

pub(super) fn needs_approval(node: &Node) -> bool {
    !is_terminal(&node.status)
        && node.waiting.as_ref().is_some_and(|waiting| {
            waiting.intent_count > 0 && matches!(waiting.kind.as_str(), "permission" | "mixed")
        })
}

fn permission_label(option: &PermissionOption) -> &str {
    match (option.id.as_str(), option.allow) {
        ("allow_once", true) => "确认执行",
        ("reject_once", false) => "取消本次操作",
        ("allow_always", true) => "本会话允许相同操作",
        ("reject_always", false) => "本会话拒绝相同操作",
        _ => &option.label,
    }
}

fn approval_card(title: impl Into<SharedString>, theme: Theme) -> Div {
    div()
        .flex_none()
        .w_full()
        .flex()
        .flex_col()
        .gap(px(8.0))
        .p(px(12.0))
        .rounded(px(10.0))
        .border_1()
        .border_color(theme.warning.opacity(0.65))
        .bg(theme.raised)
        .child(
            div()
                .flex()
                .items_center()
                .gap(px(8.0))
                .child(icon("icons/alert.svg", 16.0, theme.warning))
                .child(
                    task_text(title, theme)
                        .text_color(theme.text)
                        .font_weight(FontWeight::SEMIBOLD),
                ),
        )
}

fn decision_button(
    owner: &Waku,
    id: String,
    label: impl Into<SharedString>,
    primary: bool,
    disabled: bool,
    cx: &mut Context<Waku>,
    action: impl Fn(&mut Waku, &mut Window, &mut Context<Waku>) + 'static,
) -> Stateful<Div> {
    let theme = Theme::current(cx);
    let secondary_background = theme.raised.blend(theme.overlay_strong);
    let secondary_border = theme.raised.blend(theme.text_secondary.opacity(0.85));
    let secondary_hover = theme.raised.blend(theme.text.opacity(0.12));
    let focus = owner.transcript_control_focus(id.clone(), cx);
    task_button(SharedString::from(id), label, disabled, cx, action)
        .track_focus(&focus)
        .tab_stop(true)
        .h(px(30.0))
        .min_h(px(30.0))
        .max_h(px(30.0))
        .px(px(10.0))
        .py(px(0.0))
        .flex()
        .items_center()
        .justify_center()
        .text_size(sp(12.0))
        .line_height(sp(16.0))
        .whitespace_nowrap()
        .font_weight(FontWeight::SEMIBOLD)
        .when(primary, |button| {
            button
                .bg(theme.inverse)
                .text_color(theme.on_inverse)
                .border_color(theme.inverse)
        })
        .when(!primary, |button| {
            button
                .bg(secondary_background)
                .border_color(secondary_border)
        })
        .when(!disabled, |button| {
            button
                .cursor_pointer()
                .when(primary, |button| button.hover(|style| style.opacity(0.85)))
                .when(!primary, |button| {
                    button
                        .hover(|style| style.bg(secondary_hover).border_color(theme.text_secondary))
                })
        })
        .focus_visible(|style| style.border_color(theme.accent))
}

fn operation_detail(
    id: &'static str,
    text: impl Into<SharedString>,
    theme: Theme,
) -> Stateful<Div> {
    div()
        .id(id)
        .max_h(px(112.0))
        .overflow_y_scroll()
        .p(px(8.0))
        .rounded(px(6.0))
        .bg(theme.inset)
        .child(task_text(text, theme).whitespace_normal())
}

impl TaskBrowser {
    fn attention_node(&self) -> Option<&CachedNode> {
        self.selected()
            .filter(|cached| needs_approval(&cached.node))
            .or_else(|| {
                self.approval_target
                    .as_ref()
                    .and_then(|key| self.nodes.get(key))
            })
    }

    pub(super) fn current_permission_action(&self) -> Option<Action> {
        let detail = self.detail.as_ref()?;
        let interaction = detail.interaction.as_ref()?;
        let intent = interaction.inputs().get(self.intent_index)?;
        (!self.stale
            && detail.status == "waiting_input"
            && self.capabilities.permission
            && self.selected_allows("approve"))
        .then(|| Action::Permission {
            task_id: detail.id.clone(),
            interaction_id: interaction.id.clone(),
            task_version: interaction.task_version,
            intent_id: intent.id.clone(),
        })
    }

    fn decide_intent(&mut self, expected: &Action, decision: Decision) -> Option<Action> {
        if self.controls_busy() || self.current_permission_action().as_ref() != Some(expected) {
            return None;
        }
        let Action::Permission { intent_id, .. } = expected else {
            return None;
        };
        let inputs = self.detail.as_ref()?.interaction.as_ref()?.inputs();
        if inputs.get(self.intent_index)?.id != *intent_id {
            return None;
        }
        self.previews.get_mut(intent_id)?.decision = Some(decision);
        if let Some(action) = self.approval_action() {
            return Some(action);
        }
        if let Some(index) = inputs.iter().position(|intent| {
            self.previews
                .get(&intent.id)
                .is_none_or(|preview| preview.decision.is_none())
        }) {
            self.intent_index = index;
            self.next = self.current_permission_action();
        }
        None
    }
}

impl Waku {
    pub(in crate::app) fn render_deerflow_permission_card(
        &self,
        permission: &PendingPermission,
        location: &'static str,
        cx: &mut Context<Self>,
    ) -> Div {
        let theme = Theme::current(cx);
        let session = self.state.selected_session;
        let mut actions = div().flex().flex_wrap().gap(px(8.0));
        for option in permission
            .options
            .iter()
            .filter(|option| !matches!(option.id.as_str(), "allow_always" | "reject_always"))
        {
            let request = permission.request_id.clone();
            let option_id = option.id.clone();
            let id = format!("df-{location}-permission-{request}-{option_id}");
            let respond = move |this: &mut Waku, _: &mut Window, cx: &mut Context<Waku>| {
                if this.state.selected_session == session {
                    this.respond_permission(request.clone(), option_id.clone(), cx);
                }
            };
            actions = actions.child(decision_button(
                self,
                id,
                permission_label(option).to_owned(),
                option.allow,
                false,
                cx,
                respond,
            ));
        }
        let mut card = approval_card("需要你确认 · 工具执行请求", theme)
            .child(
                task_text(permission.title.clone(), theme)
                    .text_color(theme.text)
                    .line_clamp(2),
            )
            .child(operation_detail(
                if location == "panel" {
                    "df-panel-operation"
                } else {
                    "df-composer-operation"
                },
                permission.detail.clone(),
                theme,
            ))
            .child(actions)
            .child(task_text(
                "仅处理本次工具调用；取消不会撤销已完成的操作。",
                theme,
            ));
        let policies: Vec<_> = permission
            .options
            .iter()
            .filter(|option| matches!(option.id.as_str(), "allow_always" | "reject_always"))
            .cloned()
            .collect();
        if !policies.is_empty() {
            let id = SharedString::from(format!("df-{location}-permission-policy"));
            let handle = self.menu_handle(id.clone(), cx);
            let weak = cx.entity().downgrade();
            let request = permission.request_id.clone();
            card = card.child(dropdown_menu(
                MenuChip::new(id)
                    .label("更多权限选项")
                    .selected(handle.is_open()),
                SharedString::from(format!("df-{location}-permission-policy-menu")),
                &handle,
                MenuAlign::AboveLeft,
                move |_| {
                    let mut items =
                        vec![MenuItem::Header("以下选项会影响本会话后续相同操作".into())];
                    for option in &policies {
                        let weak = weak.clone();
                        let request = request.clone();
                        let option_id = option.id.clone();
                        items.push(MenuItem::new(
                            permission_label(option).to_owned(),
                            move |_, cx| {
                                let _ = weak.update(cx, |this, cx| {
                                    if this.state.selected_session == session {
                                        this.respond_permission(
                                            request.clone(),
                                            option_id.clone(),
                                            cx,
                                        );
                                    }
                                });
                            },
                        ));
                    }
                    items
                },
            ));
        }
        card
    }

    pub(super) fn render_deerflow_attention(&self, cx: &mut Context<Self>) -> Option<AnyElement> {
        let session = self
            .selected_session()
            .filter(|s| s.provider == ProviderKind::DeerFlow)?
            .id;
        let theme = Theme::current(cx);
        let browser = self.deerflow_tasks.get(&session);
        let unavailable = browser.is_none_or(|browser| {
            browser.connection.is_empty()
                || browser.controls_busy()
                || browser.transfer.is_some()
                || browser.save_selecting
                || browser.stale
        });
        if let Some((message, action)) = browser.and_then(|b| b.confirmation.as_ref()) {
            let action = action.clone();
            let cancel = matches!(action, Action::Cancel { .. });
            return Some(
                approval_card(
                    if cancel {
                        "确认停止任务？"
                    } else {
                        "确认任务操作"
                    },
                    theme,
                )
                .child(task_text(message.clone(), theme))
                .child(
                    div()
                        .flex()
                        .flex_wrap()
                        .gap(px(8.0))
                        .child(decision_button(
                            self,
                            "df-attention-confirm".into(),
                            if cancel {
                                "确认停止"
                            } else {
                                "确认执行"
                            },
                            true,
                            unavailable,
                            cx,
                            move |this, _, cx| {
                                this.request_deerflow_task(session, action.clone(), cx)
                            },
                        ))
                        .child(decision_button(
                            self,
                            "df-attention-dismiss".into(),
                            "返回，不执行",
                            false,
                            false,
                            cx,
                            move |this, _, cx| {
                                if let Some(browser) = this.deerflow_tasks.get_mut(&session) {
                                    browser.confirmation = None;
                                }
                                cx.notify();
                            },
                        )),
                )
                .into_any_element(),
            );
        }
        if let Some(permission) = self
            .runtimes
            .get(&session)
            .and_then(|r| r.pending_permission.as_ref())
        {
            return Some(
                self.render_deerflow_permission_card(permission, "panel", cx)
                    .into_any_element(),
            );
        }
        let browser = browser?;
        if let Some(detail) = &browser.detail
            && let Some(interaction) = &detail.interaction
            && let Some(intent) = interaction.inputs().get(browser.intent_index)
            && browser.selected_allows("approve")
            && detail.status == "waiting_input"
        {
            let preview = browser.previews.get(&intent.id);
            let submitting = browser
                .pending
                .as_ref()
                .is_some_and(|command| matches!(command.action, Action::Approve { .. }));
            let mut card = approval_card(
                if submitting {
                    "正在提交你的决定…".to_owned()
                } else {
                    format!(
                        "需要你确认 · 操作 {}/{}",
                        browser.intent_index + 1,
                        interaction.inputs().len()
                    )
                },
                theme,
            )
            .child(task_text(format!("工具 · {}", intent.tool_name), theme).text_color(theme.text))
            .child(operation_detail(
                "df-background-operation",
                preview.map_or_else(
                    || SharedString::from(intent.arguments_summary.clone()),
                    |preview| preview.text.clone(),
                ),
                theme,
            ));
            if preview.is_none() {
                let action = browser.current_permission_action();
                card = card.child(task_button(
                    "df-attention-preview",
                    if browser.pending.is_some() {
                        "正在读取完整参数…"
                    } else {
                        "读取完整参数后确认"
                    },
                    unavailable || action.is_none(),
                    cx,
                    move |this, _, cx| {
                        if let Some(action) = &action {
                            this.request_deerflow_task(session, action.clone(), cx);
                        }
                    },
                ));
            }
            let mut actions = div().flex().flex_wrap().gap(px(8.0));
            for (label, decision) in [
                ("确认执行", Decision::AllowOnce),
                ("取消本次操作", Decision::RejectOnce),
            ] {
                let expected = browser.current_permission_action();
                let allow = decision == Decision::AllowOnce;
                actions = actions.child(decision_button(
                    self,
                    format!("df-background-decision-{allow}"),
                    label,
                    allow,
                    unavailable || preview.is_none() || expected.is_none(),
                    cx,
                    move |this, _, cx| {
                        let action = this.deerflow_tasks.get_mut(&session).and_then(|browser| {
                            browser.decide_intent(expected.as_ref()?, decision.clone())
                        });
                        if let Some(action) = action {
                            this.request_deerflow_task(session, action, cx);
                        }
                        cx.notify();
                    },
                ));
            }
            card = card.child(actions).child(task_text(
                if submitting {
                    "正在等待服务确认，请勿重复提交。"
                } else if interaction.inputs().len() > 1 {
                    "逐项核对参数；最后一项选择后统一提交，不会自动同意其他操作。"
                } else {
                    "选择后立即提交，仅对本次工具调用生效。"
                },
                theme,
            ));
            if let Some(error) = &browser.error {
                card = card.child(task_text(error.clone(), theme).text_color(theme.danger));
            }
            if browser.stale {
                card = card.child(task_text("任务状态已变化，请先刷新任务树。", theme));
            }
            return Some(card.into_any_element());
        }
        let node = &browser.attention_node()?.node;
        let mut card = approval_card("需要你确认 · 待审批任务", theme).child(
            task_text(
                format!(
                    "{} · 等待 {} 项工具审批",
                    node.role,
                    node.waiting.as_ref().map_or(0, |w| w.intent_count)
                ),
                theme,
            )
            .text_color(theme.text),
        );
        let selected = browser
            .selected()
            .is_some_and(|cached| key(&cached.node) == key(node));
        let restore = selected
            .then(|| browser.execution_resume_action())
            .flatten();
        let action = restore.clone().unwrap_or_else(|| Action::Get {
            root_id: node.root_id.clone(),
            node_id: node.id.clone(),
        });
        card = card.child(task_text(
            if restore.is_some() {
                "审批请求尚未显示。恢复后会在这里展示原始操作，再由你确认或取消。"
            } else {
                "请先加载待审批操作及参数，不会直接执行工具。"
            },
            theme,
        ));
        let mut actions = div().flex().flex_wrap().gap(px(8.0)).child(decision_button(
            self,
            "df-attention-load".into(),
            if browser.controls_busy() {
                "正在同步…"
            } else if restore.is_some() {
                "恢复审批"
            } else {
                "加载审批操作"
            },
            true,
            unavailable,
            cx,
            move |this, _, cx| {
                this.deerflow_tasks
                    .get_mut(&session)
                    .unwrap()
                    .show_management = true;
                this.request_deerflow_task(session, action.clone(), cx);
            },
        ));
        if node.version > 0
            && browser.capabilities.cancel
            && node.allowed_actions.iter().any(|a| a == "cancel_subtree")
        {
            let action = Action::Cancel {
                root_id: node.root_id.clone(),
                node_id: node.id.clone(),
                scope: if node.mode == "root" {
                    CancelScope::Root
                } else {
                    CancelScope::Subtree
                },
                expected_version: node.version,
            };
            actions = actions.child(decision_button(
                self,
                "df-attention-stop".into(),
                "停止任务…",
                false,
                unavailable,
                cx,
                move |this, _, cx| {
                    this.deerflow_tasks.get_mut(&session).unwrap().confirmation = Some((
                        "停止此任务及全部后代；已完成的操作和结果不会撤销。".into(),
                        action.clone(),
                    ));
                    cx.notify();
                },
            ));
        }
        card = card.child(actions);
        if browser.connection.is_empty() || browser.stale {
            card = card.child(task_button(
                "df-attention-reconnect",
                "重新连接 / 刷新审批状态",
                browser.pending.is_some() || self.goal_runtime_starts.contains(&session),
                cx,
                |this, _, cx| this.open_deerflow_tasks(cx),
            ));
        }
        if let Some(error) = &browser.error {
            card = card.child(task_text(error.clone(), theme).text_color(theme.danger));
        }
        Some(card.into_any_element())
    }
}

#[cfg(test)]
mod tests {
    use super::super::tests::{approval_browser, browser, load, node};
    use super::*;

    fn preview_current(browser: &mut TaskBrowser) {
        let action = browser.current_permission_action().unwrap();
        let command = browser.begin(action).unwrap();
        browser
            .accept(
                &command,
                Response::Permission(serde_json::json!({
                    "sessionId": "child", "toolCallId": "call", "toolName": "write_file",
                    "configVersion": 2, "arguments": {"path": "report.md", "content": "hello"}
                })),
            )
            .unwrap();
    }

    #[test]
    fn approval_labels_distinguish_once_from_session_policy() {
        for (id, allow, label) in [
            ("allow_once", true, "确认执行"),
            ("reject_once", false, "取消本次操作"),
            ("allow_always", true, "本会话允许相同操作"),
            ("reject_always", false, "本会话拒绝相同操作"),
            ("custom", true, "Provider label"),
        ] {
            assert_eq!(
                permission_label(&PermissionOption {
                    id: id.into(),
                    label: "Provider label".into(),
                    allow
                }),
                label
            );
        }
    }

    #[test]
    fn attention_finds_waiting_task_without_selection_and_ignores_finished_nodes() {
        let mut browser = browser();
        let mut root = node("root/run", "", "root", 0);
        root.status = "waiting_input".into();
        root.waiting = Some(waku_protocol::deerflow_tasks::Waiting {
            kind: "permission".into(),
            task_ids: vec![],
            intent_count: 1,
            resumable: true,
        });
        load(&mut browser, vec![root.clone()]);
        assert_eq!(browser.attention_node().unwrap().node.id, "root/run");
        root.status = "completed".into();
        root.version += 1;
        load(&mut browser, vec![root]);
        assert!(browser.attention_node().is_none());
    }

    #[test]
    fn background_approval_preloads_parameters_and_submits_one_explicit_decision() {
        for decision in [Decision::AllowOnce, Decision::RejectOnce] {
            let mut browser = approval_browser();
            let action = browser.current_permission_action().unwrap();
            assert_eq!(browser.next.as_ref(), Some(&action));
            assert!(browser.decide_intent(&action, decision.clone()).is_none());
            preview_current(&mut browser);
            assert!(
                browser.next.is_none(),
                "manual fetch must consume queued fetch"
            );
            let Action::Approve { decisions, .. } =
                browser.decide_intent(&action, decision.clone()).unwrap()
            else {
                panic!("expected approval")
            };
            assert_eq!(decisions.len(), 1);
            assert_eq!(decisions[0].decision, decision);
        }
    }

    #[test]
    fn background_approval_never_accepts_stale_or_busy_decisions() {
        let mut browser = approval_browser();
        preview_current(&mut browser);
        let action = browser.current_permission_action().unwrap();
        let mut wrong = action.clone();
        if let Action::Permission { task_version, .. } = &mut wrong {
            *task_version += 1;
        }
        assert!(browser.decide_intent(&wrong, Decision::AllowOnce).is_none());
        assert!(browser.previews["intent"].decision.is_none());
        browser.stale = true;
        assert!(
            browser
                .decide_intent(&action, Decision::AllowOnce)
                .is_none()
        );
        browser.stale = false;
        browser.begin(action.clone()).unwrap();
        assert!(
            browser
                .decide_intent(&action, Decision::AllowOnce)
                .is_none()
        );
        assert!(browser.previews["intent"].decision.is_none());
    }

    #[test]
    fn background_batch_advances_without_approving_unreviewed_calls() {
        let mut browser = approval_browser();
        let interaction = browser
            .detail
            .as_mut()
            .unwrap()
            .interaction
            .as_mut()
            .unwrap();
        let inputs = interaction.waiting_inputs.as_mut().unwrap();
        let mut second = inputs[0].clone();
        second.id = "second".into();
        inputs.push(second);
        preview_current(&mut browser);
        let first = browser.current_permission_action().unwrap();
        assert!(
            browser
                .decide_intent(&first, Decision::RejectOnce)
                .is_none()
        );
        assert_eq!(browser.intent_index, 1);
        assert!(browser.approval_action().is_none());
        assert!(browser.decide_intent(&first, Decision::AllowOnce).is_none());
        preview_current(&mut browser);
        let second = browser.current_permission_action().unwrap();
        let Action::Approve { decisions, .. } =
            browser.decide_intent(&second, Decision::AllowOnce).unwrap()
        else {
            panic!("expected batch")
        };
        assert_eq!(decisions[0].decision, Decision::RejectOnce);
        assert_eq!(decisions[1].decision, Decision::AllowOnce);
    }
}
