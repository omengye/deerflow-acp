use super::*;
use crate::daemon::connection::ConnectionSettings;
use crate::ui::ActivationExt;

pub(super) struct ServerConnectionUi {
    pub address_input: Entity<TextInput>,
    pub token_input: Entity<TextInput>,
    pub remote: bool,
    pub pending: bool,
    pub result: Option<(bool, String)>,
    pub startup_error: Option<String>,
    pub active_address: String,
}

impl ServerConnectionUi {
    pub fn new(window: &mut Window, cx: &mut App) -> Self {
        let startup = crate::daemon::startup_connection();
        let address_input = cx.new(|cx| {
            let mut input = TextInput::new(window, cx)
                .select_all_on_focus_click()
                .placeholder("ws://192.168.1.190:18787");
            input.set_content(startup.settings.address.clone(), cx);
            input
        });
        let token_input = cx.new(|cx| {
            let mut input = TextInput::new(window, cx).placeholder("Token");
            input.set_content(startup.settings.token.clone(), cx);
            input.set_masked(true, cx);
            input
        });
        Self {
            remote: startup.settings.remote,
            pending: false,
            result: None,
            active_address: startup.settings.address,
            startup_error: startup.error,
            address_input,
            token_input,
        }
    }

    fn settings(&self, cx: &App) -> ConnectionSettings {
        ConnectionSettings {
            remote: self.remote,
            address: self.address_input.read(cx).content().to_owned(),
            token: self.token_input.read(cx).content().to_owned(),
        }
    }
}

impl Waku {
    pub(super) fn render_server_connection(&self, cx: &mut Context<Self>) -> AnyElement {
        let theme = Theme::current(cx);
        let pending = self.server_connection.pending;
        let remote = self.server_connection.remote;
        let active = if self.daemon.is_remote() {
            format!(
                "{} {}",
                tr!("server.current_remote"),
                self.server_connection.active_address
            )
        } else {
            tr!("server.current_local")
        };
        let mut modes = div().flex().gap(px(8.0));
        for (id, is_remote, label) in [
            ("server-mode-local", false, tr!("server.local")),
            ("server-mode-remote", true, tr!("server.remote")),
        ] {
            let selected = remote == is_remote;
            modes = modes.child(
                div()
                    .id(id)
                    .tab_index(0)
                    .h(px(32.0))
                    .px(px(13.0))
                    .flex()
                    .items_center()
                    .gap(px(6.0))
                    .rounded(px(7.0))
                    .border_1()
                    .border_color(if selected {
                        theme.accent
                    } else {
                        theme.border_strong
                    })
                    .text_color(if selected {
                        theme.accent
                    } else {
                        theme.text_secondary
                    })
                    .text_size(sp(13.0))
                    .cursor_default()
                    .opacity(if pending { 0.55 } else { 1.0 })
                    .focus_visible(|style| style.border_color(theme.accent))
                    .child(if selected { "●" } else { "○" })
                    .child(label)
                    .on_activation(cx, move |this, _, cx| {
                        if !this.server_connection.pending {
                            this.server_connection.remote = is_remote;
                            this.server_connection.result = None;
                            cx.notify();
                        }
                    }),
            );
        }
        let test = div()
            .id("server-test-connection")
            .tab_index(0)
            .h(px(30.0))
            .px(px(11.0))
            .rounded(px(7.0))
            .border_1()
            .border_color(theme.border_strong)
            .flex()
            .items_center()
            .text_size(sp(12.5))
            .text_color(theme.text_secondary)
            .opacity(if pending || !remote { 0.5 } else { 1.0 })
            .focus_visible(|style| style.border_color(theme.accent))
            .child(tr!("server.test"))
            .on_activation(cx, |this, _, cx| {
                if this.server_connection.remote {
                    this.configure_server_connection(false, cx);
                }
            });
        let apply = div()
            .id("server-save-connection")
            .tab_index(0)
            .h(px(30.0))
            .px(px(11.0))
            .rounded(px(7.0))
            .border_1()
            .border_color(theme.accent)
            .flex()
            .items_center()
            .text_size(sp(12.5))
            .text_color(theme.accent)
            .opacity(if pending { 0.5 } else { 1.0 })
            .focus_visible(|style| style.border_color(theme.text))
            .child(if pending {
                tr!("server.connecting")
            } else {
                tr!("server.save_restart")
            })
            .on_activation(cx, |this, _, cx| this.configure_server_connection(true, cx));

        div()
            .mt(px(15.0))
            .w_full()
            .px(px(20.0))
            .py(px(18.0))
            .rounded(px(13.0))
            .bg(theme.raised)
            .flex()
            .flex_col()
            .gap(px(13.0))
            .child(
                div()
                    .text_size(sp(13.5))
                    .font_weight(FontWeight::MEDIUM)
                    .child(tr!("server.title")),
            )
            .child(
                div()
                    .text_size(sp(12.5))
                    .text_color(theme.text_secondary)
                    .whitespace_normal()
                    .child(tr!("server.description")),
            )
            .child(
                div()
                    .text_size(sp(12.5))
                    .text_color(theme.text_secondary)
                    .whitespace_normal()
                    .child(active),
            )
            .child(modes)
            .when_some(
                self.server_connection.startup_error.as_ref(),
                |column, error| {
                    column.child(
                        div()
                            .text_size(sp(12.5))
                            .text_color(theme.warning)
                            .whitespace_normal()
                            .child(error.clone()),
                    )
                },
            )
            .when(remote, |column| {
                column
                    .child(div().text_size(sp(12.5)).child(tr!("server.address")))
                    .child(
                        TextField::new(
                            "server-address-field",
                            self.server_connection.address_input.clone(),
                        )
                        .w_full(),
                    )
                    .child(div().text_size(sp(12.5)).child("Token"))
                    .child(
                        TextField::new(
                            "server-token-field",
                            self.server_connection.token_input.clone(),
                        )
                        .w_full(),
                    )
                    .child(
                        div()
                            .text_size(sp(12.0))
                            .text_color(theme.text_tertiary)
                            .whitespace_normal()
                            .child(tr!("server.token_storage")),
                    )
            })
            .when_some(
                self.server_connection.result.as_ref(),
                |column, (ok, message)| {
                    column.child(
                        div()
                            .text_size(sp(12.5))
                            .whitespace_normal()
                            .text_color(if *ok {
                                theme.text_secondary
                            } else {
                                theme.warning
                            })
                            .child(message.clone()),
                    )
                },
            )
            .child(
                div()
                    .flex()
                    .justify_end()
                    .gap(px(8.0))
                    .child(test)
                    .child(apply),
            )
            .into_any_element()
    }

    fn server_switch_blocked(&self) -> bool {
        self.daemon_reconfigure_pending
            || self.state.sessions.iter().any(|session| {
                !matches!(session.status, SessionStatus::Idle | SessionStatus::Failed)
            })
    }

    fn configure_server_connection(&mut self, apply: bool, cx: &mut Context<Self>) {
        if self.server_connection.pending {
            return;
        }
        if apply && self.server_switch_blocked() {
            self.server_connection.result = Some((false, tr!("server.stop_tasks")));
            cx.notify();
            return;
        }
        let settings = match self.server_connection.settings(cx).validate() {
            Ok(settings) => settings,
            Err(error) => {
                self.server_connection.result = Some((false, error.to_string()));
                cx.notify();
                return;
            }
        };
        self.server_connection.pending = true;
        self.server_connection.result = None;
        for input in [
            &self.server_connection.address_input,
            &self.server_connection.token_input,
        ] {
            input.update(cx, |input, _| input.set_read_only(true));
        }
        let work = cx.background_executor().spawn(async move {
            if settings.remote {
                crate::daemon::test_connection(&settings)?;
            }
            if apply {
                settings.save()?;
            }
            Ok::<_, anyhow::Error>(())
        });
        cx.spawn(async move |this, cx| {
            let result = work.await;
            let _ = this.update(cx, |this, cx| {
                this.server_connection.pending = false;
                for input in [
                    &this.server_connection.address_input,
                    &this.server_connection.token_input,
                ] {
                    input.update(cx, |input, _| input.set_read_only(false));
                }
                match result {
                    Ok(()) if apply => {
                        if this.server_switch_blocked() {
                            this.server_connection.result =
                                Some((true, tr!("server.saved_restart_later")));
                        } else {
                            this.save();
                            cx.restart();
                        }
                    }
                    Ok(()) => {
                        this.server_connection.result = Some((true, tr!("server.test_success")))
                    }
                    Err(error) => {
                        this.server_connection.result =
                            Some((false, format!("{} {error:#}", tr!("server.failed"))))
                    }
                }
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }
}
