use super::*;
use crate::ui::ActivationExt;
use waku_client::credential as remote_credential;
use waku_protocol::model::CredentialForm;

pub(super) struct EnrollmentEditor {
    session: Uuid,
    request: String,
    form: CredentialForm,
    connection: waku_client::DaemonClient,
    username: Option<Entity<TextInput>>,
    key: Option<Entity<TextInput>>,
    cancel_focus: FocusHandle,
    submit_focus: FocusHandle,
    error: Option<String>,
}

impl Waku {
    fn current_enrollment(&self, session: Uuid, request: &str) -> Option<&CredentialForm> {
        if self.state.selected_session != Some(session) {
            return None;
        }
        let card = self.runtimes.get(&session)?.pending_permission.as_ref()?;
        if card.request_id != request {
            return None;
        }
        card.options
            .iter()
            .find(|o| o.allow && o.id == "allow_once")?
            .credential_form
            .as_ref()
    }

    pub(super) fn open_enrollment(
        &mut self,
        request: String,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Some(session) = self.state.selected_session else {
            return;
        };
        let Some(form) = self.current_enrollment(session, &request).cloned() else {
            return;
        };
        if self.credential_submissions.get(&session)
            == Some(&(request.clone(), form.public_key.clone()))
        {
            self.show_toast("已提交，正在等待服务端处理");
            cx.notify();
            return;
        }
        let connection = self.daemon.client();
        if !connection.allows_credential_enrollment() {
            self.show_toast("远程录入凭据需要 WSS 安全连接或本机回环连接");
            cx.notify();
            return;
        }
        let (user, key) = match remote_credential::fields(&form) {
            Ok(fields) => fields,
            Err(e) => {
                self.show_toast(e.to_string());
                cx.notify();
                return;
            }
        };
        let mut input = |label: &'static str, masked: bool| {
            cx.new(|cx| {
                let mut field = TextInput::new(window, cx).placeholder(label);
                field.set_masked(masked, cx);
                field
            })
        };
        let username = user.then(|| input("用户名", false));
        let key = key.then(|| input("密钥或密码", true));
        let focus = username
            .as_ref()
            .or(key.as_ref())
            .map(|field| field.read(cx).focus());
        self.credential_editor = Some(EnrollmentEditor {
            session,
            request,
            form,
            connection,
            username,
            key,
            cancel_focus: cx.focus_handle(),
            submit_focus: cx.focus_handle(),
            error: None,
        });
        if let Some(focus) = focus {
            window.defer(cx, move |window, cx| focus.focus(window, cx));
        }
        cx.notify();
    }

    fn submit_enrollment(&mut self, cx: &mut Context<Self>) {
        let Some(editor) = self.credential_editor.as_ref() else {
            return;
        };
        let session = editor.session;
        let request = editor.request.clone();
        if self.current_enrollment(session, &request) != Some(&editor.form)
            || !editor.connection.same_connection(&self.daemon.client())
            || !editor.connection.allows_credential_enrollment()
        {
            self.credential_editor = None;
            self.show_toast("授权或连接已变化，请重新打开当前请求");
            cx.notify();
            return;
        }
        let user = editor
            .username
            .as_ref()
            .map_or("", |f| f.read(cx).content());
        let key = editor.key.as_ref().map_or("", |f| f.read(cx).content());
        let sealed = match remote_credential::seal(&editor.form, user, key) {
            Ok(value) => value,
            Err(e) => {
                self.credential_editor.as_mut().unwrap().error = Some(e.to_string());
                cx.notify();
                return;
            }
        };
        let identity = (request.clone(), editor.form.public_key.clone());
        // Pin this submission to the same authenticated connection and runtime as the form.
        let connection = editor.connection.clone();
        let driver = self.runtimes[&session].driver.clone();
        self.credential_editor = None;
        self.credential_submissions
            .insert(session, identity.clone());
        cx.notify();
        cx.spawn(async move |this, cx| {
            let result = cx
                .background_executor()
                .spawn(async move { driver.respond_credential(connection, request, sealed) })
                .await;
            let _ = this.update(cx, |this, cx| {
                if this.credential_submissions.get(&session) == Some(&identity) {
                    if result.is_err() {
                        this.credential_submissions.remove(&session);
                        this.show_toast("未收到提交回执，请核对当前授权后重试");
                    } else {
                        this.show_success_toast("凭据已提交，正在等待工具结果");
                    }
                    cx.notify();
                }
            });
        })
        .detach();
    }

    pub(super) fn render_enrollment(&mut self, cx: &mut Context<Self>) -> Option<AnyElement> {
        let editor = self.credential_editor.as_ref()?;
        if self.current_enrollment(editor.session, &editor.request) != Some(&editor.form)
            || editor.connection.is_disconnected()
        {
            self.credential_editor = None;
            return None;
        }
        let theme = Theme::current(cx);
        let mut card = div()
            .id("credential-editor")
            .tab_group()
            .tab_stop(false)
            .w_full()
            .max_w(px(435.))
            .p(px(18.))
            .flex()
            .flex_col()
            .gap(px(10.))
            .rounded(px(12.))
            .shadow_xl()
            .bg(theme.raised)
            .text_color(theme.text)
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .on_key_down(cx.listener(|this, event: &KeyDownEvent, _, cx| {
                if event.keystroke.key == "escape" {
                    this.credential_editor = None;
                    cx.stop_propagation();
                    cx.notify();
                }
            }))
            .child(
                div()
                    .font_weight(FontWeight::SEMIBOLD)
                    .child("保存服务凭据"),
            )
            .child(format!("服务　{}", editor.form.service))
            .child(format!("调用方　{}", editor.form.consumer))
            .child(format!("账号别名　{}", editor.form.account));
        for (field, id, label) in [
            (&editor.username, "credential-username", "用户名"),
            (&editor.key, "credential-key", "密钥或密码"),
        ] {
            if let Some(field) = field {
                card = card
                    .child(label)
                    .child(TextField::new(id, field.clone()).w_full());
            }
        }
        if let Some(error) = &editor.error {
            card = card.child(div().text_color(theme.warning).child(error.clone()));
        }
        let button = |id: &'static str, label: &'static str, focus: &FocusHandle| {
            div()
                .id(id)
                .track_focus(focus)
                .tab_index(0)
                .tab_stop(true)
                .px(px(12.))
                .py(px(6.))
                .border_1()
                .border_color(theme.border_strong)
                .rounded(px(6.))
                .cursor_pointer()
                .focus_visible(|s| s.border_color(theme.accent))
                .child(label)
        };
        let request = editor.request.clone();
        card =
            card.child(
                div()
                    .flex()
                    .justify_end()
                    .gap(px(8.))
                    .child(
                        button("cancel-credential", "取消本次", &editor.cancel_focus)
                            .on_activation(cx, move |this, _, cx| {
                                this.credential_editor = None;
                                this.respond_permission(request.clone(), "reject_once".into(), cx);
                            }),
                    )
                    .child(
                        button("submit-credential", "保存并允许本次", &editor.submit_focus)
                            .bg(theme.inverse)
                            .text_color(theme.on_inverse)
                            .on_activation(cx, |this, _, cx| this.submit_enrollment(cx)),
                    ),
            );
        Some(
            div()
                .absolute()
                .inset_0()
                .occlude()
                .bg(theme.overlay.opacity(0.9))
                .flex()
                .justify_center()
                .items_center()
                .child(card)
                .into_any_element(),
        )
    }
}
