//! Channel UI only renders cached, redacted host state. Plugin code never runs here.
use super::deerflow_settings::df_button;
use super::*;
use base64::Engine as _;
use serde_json::{Value, json};

#[derive(Clone, Copy, PartialEq, Eq)]
enum ChannelRequestMode {
    Action,
    Poll,
}

#[derive(Default)]
struct ChannelRequests {
    generation: u64,
    action: Option<u64>,
    poll: Option<u64>,
}

impl ChannelRequests {
    fn busy(&self) -> bool {
        self.action.is_some()
    }

    fn begin(&mut self, mode: ChannelRequestMode) -> Option<u64> {
        if self.busy() || (mode == ChannelRequestMode::Poll && self.poll.is_some()) {
            return None;
        }
        self.generation += 1;
        let slot = match mode {
            ChannelRequestMode::Action => &mut self.action,
            ChannelRequestMode::Poll => &mut self.poll,
        };
        *slot = Some(self.generation);
        Some(self.generation)
    }

    fn finish(&mut self, mode: ChannelRequestMode, generation: u64) -> bool {
        let slot = match mode {
            ChannelRequestMode::Action => &mut self.action,
            ChannelRequestMode::Poll => &mut self.poll,
        };
        if *slot != Some(generation) {
            return false;
        }
        *slot = None;
        self.generation == generation
    }

    fn invalidate_poll(&mut self) {
        self.generation += 1;
    }
}

pub(super) struct ChannelSettings {
    items: Vec<Value>,
    selected: String,
    workspace: Entity<TextInput>,
    verify: Entity<TextInput>,
    options: Vec<Entity<TextInput>>,
    requests: ChannelRequests,
    polling: bool,
    login: Value,
    qr: Option<Arc<gpui::Image>>,
    error: Option<String>,
    poll_error: Option<String>,
    notice: Option<String>,
    confirm_logout: bool,
    confirm_full_access: bool,
    login_after_save: bool,
    history: Vec<Value>,
    expanded: Option<(String, String)>,
    messages: Vec<Value>,
    message_pages: Vec<i64>,
    next_before: i64,
    form: Value,
    opening_session: bool,
}
impl ChannelSettings {
    pub(super) fn new(window: &mut Window, cx: &mut App) -> Self {
        Self {
            items: vec![],
            selected: String::new(),
            workspace: cx.new(|cx| TextInput::new(window, cx)),
            verify: cx.new(|cx| TextInput::new(window, cx)),
            options: (0..16)
                .map(|_| cx.new(|cx| TextInput::new(window, cx)))
                .collect(),
            requests: ChannelRequests::default(),
            polling: false,
            login: Value::Null,
            qr: None,
            error: None,
            poll_error: None,
            notice: None,
            confirm_logout: false,
            confirm_full_access: false,
            login_after_save: false,
            history: vec![],
            expanded: None,
            messages: vec![],
            message_pages: vec![],
            next_before: 0,
            form: Value::Null,
            opening_session: false,
        }
    }
    fn current(&self) -> Value {
        self.items
            .iter()
            .find(|v| v["id"] == self.selected)
            .cloned()
            .unwrap_or(Value::Null)
    }

    fn clear_history(&mut self) {
        self.history.clear();
        self.close_messages();
    }

    fn close_messages(&mut self) {
        self.expanded = None;
        self.messages.clear();
        self.message_pages.clear();
        self.next_before = 0;
    }

    fn set_message_page(&mut self, before: i64, data: &Value) {
        if before == 0 {
            self.message_pages.clear();
        } else if let Some(index) = self
            .message_pages
            .iter()
            .position(|cursor| *cursor == before)
        {
            self.message_pages.truncate(index);
        }
        self.message_pages.push(before);
        self.messages = data["items"].as_array().cloned().unwrap_or_default();
        self.next_before = data["next_before"].as_i64().unwrap_or(0);
    }
}

fn task_status_label(status: &str) -> &str {
    match status {
        "completed" => "已完成",
        "running" | "resuming" | "suspending" => "执行中",
        "waiting_tasks" => "等待后台任务",
        "waiting_approval" => "等待确认",
        "waiting_input" => "等待输入",
        "blocked" => "恢复受阻",
        "stopped" => "自动续跑已停止",
        "needs_reconciliation" => "待核对",
        "failed" => "执行失败",
        "cancelled" => "已取消",
        "idle" => "尚无任务",
        "history_only" => "仅有历史记录",
        other => other,
    }
}

fn delivery_status_label(status: &str) -> &str {
    match status {
        "sent" => "回复已发送",
        "unknown" => "投递待核对",
        "reviewed" => "已核对",
        "ignored" => "已忽略",
        "queued" => "已排队",
        "waiting_task" => "等待任务结果",
        "running" => "处理中",
        "ready" => "待发送",
        "sending" => "发送中",
        other => other,
    }
}

fn channel_view_changed(previous: &Value, next: &Value) -> bool {
    // Heartbeats and queue counters are cached but do not affect this page.
    [
        "id",
        "descriptor",
        "enabled",
        "workspace",
        "options",
        "authenticated",
        "bound",
        "connected",
        "account",
        "owner",
        "phase",
        "error",
        "legacy_policy",
        "approval",
        "full_access_authorized",
    ]
    .iter()
    .any(|key| previous[*key] != next[*key])
}

fn channel_should_rebind(form: &Value, item: &Value, has_draft: bool) -> bool {
    !has_draft
        && ["id", "descriptor", "workspace", "options", "revision"]
            .iter()
            .any(|key| form[*key] != item[*key])
}

fn channel_login_changed(previous: &Value, next: &Value) -> bool {
    ["id", "state", "pairing_code", "qr_png"]
        .iter()
        .any(|key| previous[*key] != next[*key])
}

fn channel_login_poll_matches(request: &Value, selected: &str, login: &Value) -> bool {
    request["operation"] == "channel.login.poll"
        && request["id"] == selected
        && request["login_id"]
            .as_str()
            .is_some_and(|id| !id.is_empty())
        && request["login_id"] == login["id"]
}

fn phase_label(value: &str) -> &'static str {
    match value {
        "disabled" => "已停用",
        "login_required" => "需要登录",
        "login_pending" | "pending" => "等待扫码",
        "scanned" => "已扫码，请在手机确认",
        "verification_required" => "需要输入微信验证码",
        "binding_required" => "需要确认使用者",
        "connecting" => "正在连接",
        "connected" => "已连接",
        "error" => "连接异常",
        "expired" => "二维码已过期",
        "failed" => "登录失败",
        "confirmed" => "已绑定",
        _ => "等待状态更新",
    }
}

impl Waku {
    pub(super) fn enter_channels(&mut self, cx: &mut Context<Self>) {
        self.channel_request(json!({"operation":"channel.list"}), cx);
        if self.channel_settings.polling {
            return;
        }
        self.channel_settings.polling = true;
        cx.spawn(async move |this, cx| {
            loop {
                cx.background_executor().timer(Duration::from_secs(3)).await;
                let keep = this.update(cx, |this, cx| {
                    if this.settings_page != Some(SettingsPage::Channels) {
                        this.channel_settings.polling = false;
                        return false;
                    }
                    if !this.channel_settings.requests.busy() {
                        let state = &this.channel_settings;
                        let login = state.login["id"].as_str().unwrap_or("");
                        if !login.is_empty() && matches!(state.login["state"].as_str(), Some("pending" | "scanned")) {
                            this.channel_dispatch(json!({
                                "operation": "channel.login.poll", "id": state.selected, "login_id": login
                            }), ChannelRequestMode::Poll, cx);
                        } else if state.login["state"] != "verification_required" {
                            this.channel_dispatch(json!({"operation": "channel.status", "id": state.selected}), ChannelRequestMode::Poll, cx);
                        }
                    }
                    true
                }).unwrap_or(false);
                if !keep {
                    break;
                }
            }
        }).detach();
    }
    fn channel_select(&mut self, id: String, cx: &mut Context<Self>) {
        if self.channel_settings.requests.busy() || self.channel_settings.opening_session {
            return;
        }
        if self.channel_has_draft(cx) {
            self.channel_settings.error = Some("请先保存或还原未保存的修改，再切换渠道。".into());
            cx.notify();
            return;
        }
        if self.channel_settings.login["id"]
            .as_str()
            .is_some_and(|s| !s.is_empty())
        {
            self.channel_settings.error = Some("请先取消当前登录，再切换渠道。".into());
            cx.notify();
            return;
        }
        self.channel_settings.requests.invalidate_poll();
        self.channel_settings.poll_error = None;
        self.channel_settings.selected = id;
        self.channel_settings.login = Value::Null;
        self.channel_settings.qr = None;
        self.channel_settings.confirm_logout = false;
        self.channel_settings.confirm_full_access = false;
        self.channel_settings.clear_history();
        self.channel_rebind(cx);
        cx.notify();
    }
    fn channel_rebind(&mut self, cx: &mut Context<Self>) {
        let item = self.channel_settings.current();
        self.channel_settings.confirm_full_access = false;
        self.channel_settings.form = item.clone();
        self.channel_settings.workspace.update(cx, |input, cx| {
            let value = item["workspace"].as_str().unwrap_or("");
            if input.content() != value {
                input.set_content(value.to_owned(), cx);
            }
        });
        for (index, input) in self.channel_settings.options.iter().enumerate() {
            let key = item["descriptor"]["fields"][index]["key"]
                .as_str()
                .unwrap_or("");
            input.update(cx, |input, cx| {
                input.set_masked(item["descriptor"]["fields"][index]["secret"] == true, cx);
                let value = item["options"][key].as_str().unwrap_or("");
                if input.content() != value {
                    input.set_content(value.to_owned(), cx);
                }
            });
        }
    }
    fn channel_has_draft(&self, cx: &App) -> bool {
        let state = &self.channel_settings;
        let form = &state.form;
        if state.workspace.read(cx).content() != form["workspace"].as_str().unwrap_or("") {
            return true;
        }
        form["descriptor"]["fields"]
            .as_array()
            .is_some_and(|fields| {
                fields.iter().take(16).enumerate().any(|(index, field)| {
                    let key = field["key"].as_str().unwrap_or("");
                    state.options[index].read(cx).content()
                        != form["options"][key].as_str().unwrap_or("")
                })
            })
    }
    fn channel_request(&mut self, request: Value, cx: &mut Context<Self>) {
        self.channel_dispatch(request, ChannelRequestMode::Action, cx);
    }
    fn channel_dispatch(
        &mut self,
        request: Value,
        mode: ChannelRequestMode,
        cx: &mut Context<Self>,
    ) {
        if request["operation"] == "channel.status" && self.channel_settings.selected.is_empty() {
            return;
        }
        let Some(generation) = self.channel_settings.requests.begin(mode) else {
            return;
        };
        if mode == ChannelRequestMode::Action
            && matches!(
                request["operation"].as_str(),
                Some("channel.logout" | "channel.login.cancel")
            )
        {
            self.channel_settings.login = Value::Null;
            self.channel_settings.qr = None;
        }
        if mode == ChannelRequestMode::Action && request["operation"] == "channel.login.poll" {
            self.channel_settings
                .verify
                .update(cx, |input, cx| input.set_content("", cx));
        }
        let read_only = matches!(
            request["operation"].as_str(),
            Some("channel.status" | "channel.history" | "channel.sessions")
        );
        if mode == ChannelRequestMode::Action {
            self.channel_freeze_inputs(!read_only, cx);
        }
        let client = self.daemon.client();
        let mut sent = request.clone();
        if let Some(object) = sent.as_object_mut() {
            object.remove("fields");
            object.remove("verify_code");
        }
        cx.spawn(async move |this, cx| {
            let result: anyhow::Result<(Value, Option<Arc<gpui::Image>>)> = cx
                .background_executor()
                .spawn(async move {
                    let response = client.request(
                        Uuid::nil(),
                        Uuid::nil(),
                        waku_client::Command::DeerFlow {
                            operation: "manage".into(),
                            input: request,
                        },
                    )?;
                    let waku_client::ResponsePayload::DeerFlow { data } = response else {
                        anyhow::bail!("无法识别渠道响应")
                    };
                    let image = data["qr_png"]
                        .as_str()
                        .filter(|s| s.len() < 256 * 1024)
                        .and_then(|s| base64::engine::general_purpose::STANDARD.decode(s).ok())
                        .map(|bytes| {
                            Arc::new(gpui::Image::from_bytes(gpui::ImageFormat::Png, bytes))
                        });
                    Ok((data, image))
                })
                .await;
            let _ = this.update(cx, |this, cx| {
                // Actions supersede polls without waiting for a background IPC round trip.
                let current = this.channel_settings.requests.finish(mode, generation);
                // Login polling may commit credentials on the host. A history/list refresh
                // must not discard its completion; cancel/start/save clear the attempt instead.
                let current_login = mode == ChannelRequestMode::Poll
                    && channel_login_poll_matches(
                        &sent,
                        &this.channel_settings.selected,
                        &this.channel_settings.login,
                    );
                if !current && !current_login {
                    return;
                }
                if mode == ChannelRequestMode::Poll
                    && (this.settings_page != Some(SettingsPage::Channels)
                        || sent["id"] != this.channel_settings.selected)
                {
                    return;
                }
                if mode == ChannelRequestMode::Action {
                    this.channel_freeze_inputs(false, cx);
                }
                let mut changed = mode == ChannelRequestMode::Action;
                let operation = sent["operation"].as_str().unwrap_or("");
                match result {
                    Err(error) => {
                        let error = Some(error.to_string());
                        if mode == ChannelRequestMode::Poll {
                            changed |= this.channel_settings.poll_error != error;
                            this.channel_settings.poll_error = error;
                        } else {
                            this.channel_settings.error = error;
                            this.channel_settings.login_after_save = false;
                        }
                        if operation.starts_with("channel.login.") {
                            this.channel_settings.login = Value::Null;
                            this.channel_settings.qr = None;
                            changed = true;
                        }
                    }
                    Ok((data, image)) => {
                        changed |= this.channel_settings.poll_error.take().is_some();
                        if mode == ChannelRequestMode::Action {
                            this.channel_settings.error = None;
                        }
                        match operation {
                            "channel.list" => {
                                let preserve_draft = this.channel_has_draft(cx);
                                let previous = this.channel_settings.selected.clone();
                                this.channel_settings.items =
                                    data["channels"].as_array().cloned().unwrap_or_default();
                                if !this
                                    .channel_settings
                                    .items
                                    .iter()
                                    .any(|v| v["id"] == this.channel_settings.selected)
                                {
                                    this.channel_settings.selected = this
                                        .channel_settings
                                        .items
                                        .first()
                                        .and_then(|v| v["id"].as_str())
                                        .unwrap_or("")
                                        .to_owned();
                                    this.channel_settings.clear_history();
                                }
                                if channel_should_rebind(
                                    &this.channel_settings.form,
                                    &this.channel_settings.current(),
                                    preserve_draft && previous == this.channel_settings.selected,
                                ) {
                                    this.channel_rebind(cx);
                                }
                            }
                            "channel.status" => {
                                let preserve_draft = this.channel_has_draft(cx);
                                if let Some(item) = this
                                    .channel_settings
                                    .items
                                    .iter_mut()
                                    .find(|v| v["id"] == data["id"])
                                {
                                    changed |= channel_view_changed(item, &data);
                                    *item = data;
                                }
                                if channel_should_rebind(
                                    &this.channel_settings.form,
                                    &this.channel_settings.current(),
                                    preserve_draft,
                                ) {
                                    this.channel_rebind(cx);
                                }
                                if this.channel_settings.current()["owner"]
                                    .as_str()
                                    .is_some_and(|v| !v.is_empty())
                                    && this.channel_settings.login["state"] == "binding_required"
                                {
                                    this.channel_settings.login = Value::Null;
                                    changed = true;
                                }
                            }
                            "channel.configure" => {
                                if let Some(item) = this
                                    .channel_settings
                                    .items
                                    .iter_mut()
                                    .find(|v| v["id"] == sent["id"])
                                {
                                    for key in ["workspace", "enabled", "options"] {
                                        item[key] = sent[key].clone();
                                    }
                                    item["revision"] = data["revision"].clone();
                                    this.channel_settings.form = item.clone();
                                }
                                if this.channel_settings.login_after_save {
                                    this.channel_settings.login_after_save = false;
                                    this.channel_login(cx)
                                } else {
                                    this.channel_request(json!({"operation":"channel.list"}), cx)
                                }
                                this.channel_settings.notice =
                                    Some("渠道配置已保存并单独应用，不影响其他渠道。".into());
                            }
                            "channel.permissions" => {
                                this.channel_settings.confirm_full_access = false;
                                this.channel_settings.notice = Some("权限已保存。新消息使用当前策略；旧待确认任务可在微信发送 /resume。".into());
                                this.channel_request(json!({"operation":"channel.list"}),cx);
                            }
                            "channel.login.start" | "channel.login.poll" => {
                                changed |= channel_login_changed(&this.channel_settings.login, &data);
                                if image.is_some()
                                    && (this.channel_settings.qr.is_none()
                                        || this.channel_settings.login["qr_png"] != data["qr_png"])
                                {
                                    this.channel_settings.qr = image;
                                    changed = true;
                                }
                                let terminal = matches!(
                                    data["state"].as_str(),
                                    Some("confirmed" | "binding_required" | "expired" | "failed")
                                );
                                this.channel_settings.login = data;
                                if terminal {
                                    this.channel_settings.qr = None;
                                    this.channel_settings.login["id"] = json!("");
                                    if this.channel_settings.login["state"] == "confirmed" {
                                        this.channel_settings.login = Value::Null;
                                        this.channel_settings.notice = Some("登录成功，已绑定当前账号。".into());
                                    }
                                    this.channel_settings
                                        .verify
                                        .update(cx, |input, cx| input.set_content("", cx));
                                    this.channel_request(json!({"operation":"channel.list"}), cx)
                                }
                            }
                            "channel.logout" | "channel.login.cancel" => {
                                this.channel_settings.login = Value::Null;
                                this.channel_settings.qr = None;
                                this.channel_settings.verify.update(cx, |input, cx| input.set_content("", cx));
                                this.channel_settings.confirm_logout = false;
                                this.channel_request(json!({"operation":"channel.list"}), cx)
                            }
                            "channel.sessions" => {
                                this.channel_settings.history =
                                    data.as_array().cloned().unwrap_or_default();
                                // A purged provider session can still have a local transcript.
                                for entry in &mut this.channel_settings.history {
                                    if entry["status"] == "history_only" {
                                        let local = this.state.sessions.iter().any(|s| matches!(
                                            s.provider_cursor.as_ref(),
                                            Some(ProviderResumeCursor::DeerFlow { session_id }) if entry["session_id"] == session_id.as_str()
                                        ));
                                        entry["local_history"] = json!(local);
                                    }
                                }
                                if let Some((account, session)) = this.channel_settings.expanded.clone() {
                                    if this.channel_settings.history.iter().any(|entry| entry["account"] == account.as_str() && entry["session_id"] == session.as_str()) {
                                        this.channel_session_messages(account, session, 0, cx);
                                    } else {
                                        this.channel_settings.close_messages();
                                    }
                                }
                            }
                            "channel.history" => {
                                let state = &mut this.channel_settings;
                                if state.expanded.as_ref().is_some_and(|(a,s)| sent["account"] == a.as_str() && sent["session_id"] == s.as_str()) {
                                    state.set_message_page(sent["before"].as_i64().unwrap_or(0), &data);
                                }
                            }
                            "channel.review" => {
                                this.channel_request(json!({"operation":"channel.sessions","id":this.channel_settings.selected}), cx);
                            }
                            _ => this.channel_request(json!({"operation":"channel.list"}), cx),
                        }
                    }
                }
                if changed {
                    cx.notify();
                }
            });
        })
        .detach();
        if mode == ChannelRequestMode::Action {
            cx.notify();
        }
    }
    fn channel_save(&mut self, enabled: bool, cx: &mut Context<Self>) {
        let item = self.channel_settings.current();
        let mut options = serde_json::Map::new();
        if let Some(fields) = item["descriptor"]["fields"].as_array() {
            for (index, field) in fields.iter().take(16).enumerate() {
                if field["login"] == true {
                    continue;
                }
                if let Some(key) = field["key"].as_str() {
                    options.insert(
                        key.into(),
                        json!(self.channel_settings.options[index].read(cx).content()),
                    );
                }
            }
        }
        self.channel_settings.login_after_save = enabled && item["authenticated"] != true;
        self.channel_settings.login = Value::Null;
        self.channel_settings.qr = None;
        self.channel_request(
            json!({
                "operation": "channel.configure",
                "id": self.channel_settings.selected,
                "revision": self.channel_settings.form["revision"],
                "workspace": self.channel_settings.workspace.read(cx).content(),
                "enabled": enabled,
                "options": options
            }),
            cx,
        );
    }
    fn channel_freeze_inputs(&mut self, busy: bool, cx: &mut Context<Self>) {
        self.channel_settings
            .workspace
            .update(cx, |input, _| input.set_read_only(busy));
        self.channel_settings
            .verify
            .update(cx, |input, _| input.set_read_only(busy));
        for input in &self.channel_settings.options {
            input.update(cx, |input, _| input.set_read_only(busy));
        }
    }
    fn channel_session_messages(
        &mut self,
        account: String,
        session: String,
        before: i64,
        cx: &mut Context<Self>,
    ) {
        self.channel_request(json!({"operation":"channel.history", "id":self.channel_settings.selected, "account":account, "session_id":session, "before":before}), cx);
    }

    fn channel_open_session(&mut self, entry: Value, window: &mut Window, cx: &mut Context<Self>) {
        if self.channel_settings.opening_session {
            return;
        }
        let Some(id) = entry["session_id"].as_str().filter(|v| !v.is_empty()) else {
            return;
        };
        let summary = ProviderSessionSummary {
            cursor: ProviderResumeCursor::DeerFlow {
                session_id: id.to_owned(),
            },
            title: entry["title"].as_str().unwrap_or("渠道会话").to_owned(),
            cwd: std::path::PathBuf::from(entry["workspace"].as_str().unwrap_or("")),
            created_at: entry["created_ms"].as_u64().unwrap_or(0) / 1000,
            updated_at: entry["updated_ms"].as_u64().unwrap_or(0) / 1000,
        };
        if let Some(id) = self
            .state
            .sessions
            .iter()
            .find(|session| session.provider_cursor.as_ref() == Some(&summary.cursor))
            .map(|session| session.id)
        {
            self.settings_page = None;
            self.select_session(id, cx);
            self.open_deerflow_tasks(cx);
            return;
        }
        self.channel_settings.opening_session = true;
        cx.notify();
        let fetch = self
            .store
            .provider_session_history(summary.cursor.clone(), summary.cwd.clone());
        let handle = window.window_handle();
        let selected = self.channel_settings.selected.clone();
        cx.spawn(async move |this, cx| {
            let result = cx.background_executor().spawn(async move { fetch() }).await;
            let _ = handle.update(cx, |_, window, cx| {
                let _ = this.update(cx, |this, cx| {
                    this.channel_settings.opening_session = false;
                    cx.notify();
                    if this.settings_page != Some(SettingsPage::Channels)
                        || this.channel_settings.selected != selected
                    {
                        return;
                    }
                    match result {
                        Ok(history) => {
                            let existing = this.state.sessions.iter().any(|session| {
                                session.provider_cursor.as_ref() == Some(&summary.cursor)
                            });
                            let id = this.import_provider_session(summary, history, window, cx);
                            if !existing && let Some(session) = this.state.session_mut(id) {
                                session.runtime_mode = RuntimeMode::Ask;
                            }
                            this.open_deerflow_tasks(cx);
                        }
                        Err(e) => {
                            this.channel_settings.error = Some(e.to_string());
                            cx.notify()
                        }
                    }
                });
            });
        })
        .detach();
    }
    fn channel_pick_workspace(&mut self, cx: &mut Context<Self>) {
        if self.daemon.is_remote() {
            self.channel_settings.error = Some("远程服务请手动填写服务端工作目录。".into());
            cx.notify();
            return;
        }
        let receiver = cx.prompt_for_paths(PathPromptOptions {
            files: false,
            directories: true,
            multiple: false,
            prompt: Some("选择渠道工作目录".into()),
        });
        let id = self.channel_settings.selected.clone();
        cx.spawn(async move |this, cx| {
            if let Ok(Ok(Some(paths))) = receiver.await
                && let Some(path) = paths.first()
            {
                let path = path.to_string_lossy().into_owned();
                let _ = this.update(cx, |this, cx| {
                    if this.channel_settings.selected == id {
                        this.channel_settings
                            .workspace
                            .update(cx, |input, cx| input.set_content(path, cx));
                        cx.notify()
                    }
                });
            }
        })
        .detach();
    }
    fn channel_login(&mut self, cx: &mut Context<Self>) {
        let item = self.channel_settings.current();
        let mut fields = serde_json::Map::new();
        if let Some(list) = item["descriptor"]["fields"].as_array() {
            for (index, field) in list.iter().take(16).enumerate() {
                if field["login"] != true {
                    continue;
                }
                if let Some(key) = field["key"].as_str() {
                    fields.insert(
                        key.into(),
                        json!(self.channel_settings.options[index].read(cx).content()),
                    );
                    self.channel_settings.options[index]
                        .update(cx, |input, cx| input.set_content("", cx));
                }
            }
        }
        self.channel_settings.confirm_logout = false;
        self.channel_settings.login = Value::Null;
        self.channel_settings.qr = None;
        self.channel_settings
            .verify
            .update(cx, |input, cx| input.set_content("", cx));
        self.channel_request(json!({"operation":"channel.login.start","id":self.channel_settings.selected,"fields":fields}),cx);
    }
    pub(super) fn render_channels(&self, cx: &mut Context<Self>) -> AnyElement {
        let theme = Theme::current(cx);
        let s = &self.channel_settings;
        let item = s.current();
        let busy = s.requests.busy() || s.opening_session;
        let mut body = div().mt(px(16.0)).flex().flex_col().gap(px(16.0)).child(
            div()
                .text_color(theme.text_secondary)
                .child("渠道插件独立运行；渠道授权可覆盖普通工具审批，但不绕过本机工具限制。"),
        );
        body = body.child(df_button(
            "channel-refresh",
            "刷新渠道",
            busy,
            false,
            theme,
            cx,
            |this, _, cx| this.channel_request(json!({"operation":"channel.list"}), cx),
        ));
        for entry in &s.items {
            let id = entry["id"].as_str().unwrap_or("").to_owned();
            let label = format!(
                "{} · {}",
                entry["descriptor"]["name"].as_str().unwrap_or(&id),
                phase_label(entry["phase"].as_str().unwrap_or(""))
            );
            body = body.child(df_button(
                SharedString::from(format!("channel-{id}")),
                label,
                busy,
                id == s.selected,
                theme,
                cx,
                move |this, _, cx| this.channel_select(id.clone(), cx),
            ));
        }
        if s.items.is_empty() {
            body = body.child(
                "暂无可用渠道插件。请安装随包提供的微信插件，或通过本地管理命令注册可信插件。",
            );
        }
        if !item.is_null() {
            let enabled = item["enabled"] == true;
            let mut card = div()
                .p(px(18.0))
                .rounded(px(12.0))
                .bg(theme.raised)
                .flex()
                .flex_col()
                .gap(px(12.0));
            card = card.child(
                div()
                    .flex()
                    .items_center()
                    .justify_between()
                    .child("启用渠道")
                    .child(toggle_switch(
                        "channel-enabled",
                        enabled,
                        busy,
                        theme,
                        cx,
                        move |this, _, cx| this.channel_save(!enabled, cx),
                    )),
            );
            card = card.child("工作目录").child(
                div()
                    .flex()
                    .gap(px(8.0))
                    .child(div().flex_1().child(s.workspace.clone()))
                    .child(df_button(
                        "channel-directory",
                        "选择目录",
                        busy,
                        false,
                        theme,
                        cx,
                        |this, _, cx| this.channel_pick_workspace(cx),
                    )),
            );
            if let Some(fields) = item["descriptor"]["fields"].as_array() {
                for (index, field) in fields.iter().take(16).enumerate() {
                    card = card
                        .child(field["label"].as_str().unwrap_or("插件配置").to_owned())
                        .child(s.options[index].clone());
                }
            }
            card = card.child(df_button(
                "channel-save",
                "保存并应用",
                busy,
                false,
                theme,
                cx,
                move |this, _, cx| this.channel_save(enabled, cx),
            ));
            card = card.child(df_button(
                "channel-reset",
                "还原未保存修改",
                busy,
                false,
                theme,
                cx,
                |this, _, cx| {
                    this.channel_rebind(cx);
                    this.channel_settings.error = None;
                    cx.notify();
                },
            ));
            card = card
                .child(format!(
                    "绑定状态：{}",
                    if item["bound"] == true {
                        "已绑定"
                    } else {
                        "未绑定"
                    }
                ))
                .child(format!(
                    "连接状态：{}",
                    phase_label(item["phase"].as_str().unwrap_or(""))
                ));
            if let Some(account) = item["account"].as_str().filter(|s| !s.is_empty()) {
                card = card.child(format!("账号：{account}"));
            }
            if item["legacy_policy"] == true {
                card = card.child("已保留旧版渠道策略；重新绑定后使用个人账号绑定及全局权限。");
            }
            card = card.child(format!(
                "工具权限：{}",
                item["permissions"].as_str().unwrap_or("跟随全局")
            ));
            let policy_busy = busy || item["bound"] != true || self.channel_has_draft(cx);
            if item["bound"] != true {
                card = card.child("请先完成渠道登录和账号绑定，再设置 Full Access 等工具权限。");
            } else if self.channel_has_draft(cx) {
                card = card.child("请先保存或还原未保存的修改，再设置工具权限。");
            }
            for (mode, label) in [
                ("global", "跟随全局 / 撤销 Full Access"),
                ("read_only", "只读"),
                ("allow_always", "Full Access"),
            ] {
                card = card.child(df_button(
                    SharedString::from(format!("channel-permission-{mode}")),label,policy_busy,item["approval"]==mode,theme,cx,
                    move |this,_,cx| {
                        if mode=="allow_always" {this.channel_settings.confirm_full_access=true;cx.notify();}
                        else {let current=this.channel_settings.current();this.channel_request(json!({"operation":"channel.permissions","id":this.channel_settings.selected,"revision":current["revision"],"approval":mode}),cx);}
                    },
                ));
            }
            if s.confirm_full_access {
                card=card.child("授权此绑定用户在当前工作区使用已启用的本机工具，普通调用不再逐次询问。本地命令不保证隔离在工作目录内；账号被盗或模型误操作可能修改文件或发送数据。更换账号、工作区或退出登录将撤销授权。");
                card=card.child(df_button("channel-confirm-full","我理解风险，授权 Full Access",policy_busy,false,theme,cx,|this,_,cx| {
                    let current=this.channel_settings.current();this.channel_request(json!({"operation":"channel.permissions","id":this.channel_settings.selected,"revision":current["revision"],"approval":"allow_always"}),cx);
                })).child(df_button("channel-cancel-full","取消",busy,false,theme,cx,|this,_,cx|{this.channel_settings.confirm_full_access=false;cx.notify();}));
            }
            if let Some(error) = item["error"].as_str().filter(|s| !s.is_empty()) {
                card = card.child(error.to_owned());
            }
            let login_label = if item["descriptor"]["login"]
                .as_array()
                .is_some_and(|a| a.iter().any(|v| v == "qr"))
            {
                "扫码登录 / 刷新二维码"
            } else {
                "开始登录"
            };
            card = card.child(
                div()
                    .flex()
                    .gap(px(8.0))
                    .child(df_button(
                        "channel-login",
                        login_label,
                        busy || !enabled,
                        false,
                        theme,
                        cx,
                        |this, _, cx| this.channel_login(cx),
                    ))
                    .child(df_button(
                        "channel-logout",
                        "退出登录",
                        busy || item["authenticated"] != true,
                        false,
                        theme,
                        cx,
                        |this, _, cx| {
                            this.channel_settings.confirm_logout = true;
                            cx.notify()
                        },
                    )),
            );
            if !enabled {
                card = card.child("选择工作目录并启用渠道后即可登录。");
            }
            if s.confirm_logout {
                card = card.child("退出将停用渠道、清除本机登录凭据并停止渠道任务，聊天历史保留。")
                    .child(df_button("channel-logout-confirm", "确认退出", busy, false, theme, cx, |this, _, cx| {
                        this.channel_request(json!({"operation": "channel.logout", "id": this.channel_settings.selected}), cx);
                    }));
            }
            if !s.login.is_null() {
                card = card.child(phase_label(s.login["state"].as_str().unwrap_or("")));
                if let Some(image) = &s.qr {
                    card = card.child(gpui::img(image.clone()).w(px(256.0)).h(px(256.0)));
                }
                if s.login["state"] == "verification_required" {
                    card = card
                        .child("输入平台显示的验证码")
                        .child(s.verify.clone())
                        .child(df_button(
                            "channel-verify",
                            "提交验证码",
                            busy,
                            false,
                            theme,
                            cx,
                            |this, _, cx| {
                                this.channel_request(json!({
                                "operation": "channel.login.poll",
                                "id": this.channel_settings.selected,
                                "login_id": this.channel_settings.login["id"],
                                "verify_code": this.channel_settings.verify.read(cx).content()
                            }), cx);
                            },
                        ));
                }
                if let Some(code) = s.login["pairing_code"].as_str().filter(|s| !s.is_empty()) {
                    card = card.child(format!("平台未返回账号身份，请在 3 分钟内向机器人发送 /pair {code}。不会自动绑定首个发消息的人。"));
                }
                if s.login["id"].as_str().is_some_and(|id| !id.is_empty())
                    || s.login["state"] == "binding_required"
                {
                    card = card.child(df_button("channel-login-cancel", "取消登录", busy, false, theme, cx, |this, _, cx| {
                        this.channel_request(json!({"operation": "channel.login.cancel", "id": this.channel_settings.selected}), cx);
                    }));
                }
            }
            body = body.child(card);
            body = body.child(df_button(
                "channel-history",
                "查看最近会话",
                busy,
                false,
                theme,
                cx,
                |this, _, cx| {
                    this.channel_request(
                        json!({"operation":"channel.sessions","id":this.channel_settings.selected}),
                        cx,
                    )
                },
            ));
            for (index, entry) in s.history.iter().enumerate() {
                let session = entry["session_id"].as_str().unwrap_or("");
                let account = entry["account"].as_str().unwrap_or("");
                let expanded = s
                    .expanded
                    .as_ref()
                    .is_some_and(|(a, id)| a == account && id == session);
                let count = entry["message_count"].as_u64().unwrap_or(0);
                let mut subtitle = format!("{count} 条消息");
                if !account.is_empty() && entry["account"] != item["account"] {
                    subtitle.push_str(&format!(" · 账号 {account}"));
                }
                if !session.is_empty() {
                    subtitle = format!(
                        "{} · {subtitle} · {}",
                        task_status_label(entry["status"].as_str().unwrap_or("")),
                        session.chars().take(8).collect::<String>()
                    );
                }
                let key = (account.to_owned(), session.to_owned());
                let open = entry.clone();
                let mut row = div()
                    .flex()
                    .flex_wrap()
                    .gap(px(8.0))
                    .items_center()
                    .child(subtitle);
                if !session.is_empty()
                    && (entry["status"] != "history_only" || entry["local_history"] == true)
                {
                    row = row.child(df_button(
                        SharedString::from(format!("channel-task-{index}")),
                        "打开任务",
                        busy,
                        false,
                        theme,
                        cx,
                        move |this, window, cx| this.channel_open_session(open.clone(), window, cx),
                    ));
                }
                row = row.child(df_button(
                    SharedString::from(format!("channel-messages-{index}")),
                    if expanded {
                        "收起消息"
                    } else {
                        "查看消息"
                    },
                    busy,
                    expanded,
                    theme,
                    cx,
                    move |this, _, cx| {
                        this.channel_settings.close_messages();
                        if !expanded {
                            this.channel_settings.expanded = Some(key.clone());
                            this.channel_session_messages(key.0.clone(), key.1.clone(), 0, cx);
                        }
                        cx.notify();
                    },
                ));
                body = body.child(
                    div()
                        .flex()
                        .flex_col()
                        .gap(px(4.0))
                        .child(entry["title"].as_str().unwrap_or("渠道会话").to_owned())
                        .child(row),
                );
                if expanded {
                    for message in &s.messages {
                        let status = message["status"].as_str().unwrap_or("");
                        let mut detail = div()
                            .flex()
                            .flex_wrap()
                            .gap(px(8.0))
                            .items_center()
                            .pl(px(12.0))
                            .child(format!(
                                "{} · {}",
                                delivery_status_label(status),
                                message["preview"]
                                    .as_str()
                                    .filter(|v| !v.is_empty())
                                    .unwrap_or("渠道消息")
                            ));
                        if status == "unknown" {
                            let id = message["id"].clone();
                            let account = account.to_owned();
                            detail = detail.child(df_button(SharedString::from(format!("channel-review-{id}")), "已核对，不重试", busy, false, theme, cx, move |this, _, cx| {
                                this.channel_request(json!({"operation": "channel.review", "id": this.channel_settings.selected, "account": account, "message_id": id}), cx);
                            }));
                        }
                        body = body.child(detail);
                    }
                    let newer = s.message_pages.iter().rev().nth(1).copied();
                    let older = (s.next_before > 0).then_some(s.next_before);
                    for (cursor, id, label) in [
                        (newer, "channel-newer-messages", "较新的消息"),
                        (older, "channel-older-messages", "更早的消息"),
                    ] {
                        let Some(before) = cursor else { continue };
                        let account = account.to_owned();
                        let session = session.to_owned();
                        body = body.child(df_button(
                            id,
                            label,
                            busy,
                            false,
                            theme,
                            cx,
                            move |this, _, cx| {
                                this.channel_session_messages(
                                    account.clone(),
                                    session.clone(),
                                    before,
                                    cx,
                                );
                            },
                        ));
                    }
                }
            }
        }
        if let Some(error) = s.error.as_ref().or(s.poll_error.as_ref()) {
            let label = if s.error.is_some() {
                "操作失败"
            } else {
                "状态更新失败"
            };
            body = body.child(format!("{label}：{error}"));
            body = body.child(df_button(
                "channel-host-settings",
                "打开 DeerFlow 服务设置",
                busy,
                false,
                theme,
                cx,
                |this, _, cx| this.open_settings_page(SettingsPage::DeerFlow, cx),
            ));
        }
        if let Some(notice) = &s.notice {
            body = body.child(notice.clone());
        }
        body.into_any_element()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn status_labels_distinguish_binding_and_transport() {
        assert_ne!(phase_label("confirmed"), phase_label("connected"));
        assert_ne!(phase_label("disabled"), phase_label("login_required"));
    }

    #[test]
    fn task_wait_and_message_delivery_have_independent_labels() {
        assert_eq!(task_status_label("blocked"), "恢复受阻");
        assert_ne!(
            task_status_label("waiting_tasks"),
            task_status_label("waiting_approval")
        );
        assert_eq!(delivery_status_label("sent"), "回复已发送");
        assert_eq!(delivery_status_label("unknown"), "投递待核对");
    }

    #[test]
    fn permission_changes_are_visible() {
        let previous = status();
        for key in ["approval", "full_access_authorized"] {
            let mut next = previous.clone();
            next[key] = json!("changed");
            assert!(channel_view_changed(&previous, &next));
        }
    }

    #[test]
    fn background_polls_never_set_action_busy_or_overlap() {
        let mut requests = ChannelRequests::default();
        let poll = requests.begin(ChannelRequestMode::Poll).unwrap();
        assert!(!requests.busy());
        assert!(requests.begin(ChannelRequestMode::Poll).is_none());
        assert!(requests.finish(ChannelRequestMode::Poll, poll));
        assert!(!requests.busy());
        assert!(requests.begin(ChannelRequestMode::Poll).is_some());
    }

    #[test]
    fn actions_supersede_polls_and_stale_results_cannot_unlock_them() {
        let mut requests = ChannelRequests::default();
        let poll = requests.begin(ChannelRequestMode::Poll).unwrap();
        let action = requests.begin(ChannelRequestMode::Action).unwrap();
        assert!(requests.busy());
        assert!(requests.begin(ChannelRequestMode::Action).is_none());
        assert!(requests.begin(ChannelRequestMode::Poll).is_none());
        assert!(!requests.finish(ChannelRequestMode::Poll, poll));
        assert!(requests.busy());
        assert!(requests.finish(ChannelRequestMode::Action, action));
        assert!(!requests.busy());
        let next = requests.begin(ChannelRequestMode::Poll).unwrap();
        assert!(requests.finish(ChannelRequestMode::Poll, next));
    }

    #[test]
    fn polls_remain_stale_even_if_the_action_finishes_first() {
        let mut requests = ChannelRequests::default();
        let poll = requests.begin(ChannelRequestMode::Poll).unwrap();
        let action = requests.begin(ChannelRequestMode::Action).unwrap();
        assert!(requests.finish(ChannelRequestMode::Action, action));
        assert!(!requests.busy());
        assert!(requests.begin(ChannelRequestMode::Poll).is_none());
        assert!(!requests.finish(ChannelRequestMode::Poll, poll));
        let next = requests.begin(ChannelRequestMode::Poll).unwrap();
        assert!(!requests.finish(ChannelRequestMode::Poll, poll));
        assert!(requests.finish(ChannelRequestMode::Poll, next));
    }

    #[test]
    fn switching_channels_invalidates_in_flight_polls() {
        let mut requests = ChannelRequests::default();
        let poll = requests.begin(ChannelRequestMode::Poll).unwrap();
        requests.invalidate_poll();
        assert!(!requests.finish(ChannelRequestMode::Poll, poll));
        let next = requests.begin(ChannelRequestMode::Poll).unwrap();
        assert!(requests.finish(ChannelRequestMode::Poll, next));
    }

    fn status() -> Value {
        json!({
            "id": "wechat", "descriptor": {"name": "WeChat", "fields": []},
            "workspace": "D:\\work", "options": {}, "revision": 2,
            "enabled": true, "authenticated": true, "bound": true,
            "connected": true, "account": "account", "owner": "owner",
            "phase": "connected", "error": "", "legacy_policy": false,
            "last_success": "2026-10-17T15:00:00Z", "queued": 0, "unknown": 0
        })
    }

    #[test]
    fn unchanged_and_heartbeat_only_status_does_not_repaint_or_rebind() {
        let previous = status();
        let mut next = previous.clone();
        assert!(!channel_view_changed(&previous, &next));
        assert!(!channel_should_rebind(&previous, &next, false));
        next["last_success"] = json!("2026-10-17T15:00:03Z");
        next["queued"] = json!(3);
        next["unknown"] = json!(1);
        assert!(!channel_view_changed(&previous, &next));
        assert!(!channel_should_rebind(&previous, &next, false));
    }

    #[test]
    fn visible_status_changes_refresh_without_resetting_the_form() {
        let previous = status();
        for (key, value) in [
            ("phase", json!("error")),
            ("error", json!("connection lost")),
            ("connected", json!(false)),
            ("bound", json!(false)),
            ("authenticated", json!(false)),
            ("account", json!("other account")),
            ("owner", json!("other owner")),
            ("enabled", json!(false)),
            ("legacy_policy", json!(true)),
        ] {
            let mut next = previous.clone();
            next[key] = value;
            assert!(channel_view_changed(&previous, &next), "{key}");
            assert!(!channel_should_rebind(&previous, &next, false), "{key}");
        }
    }

    #[test]
    fn saved_configuration_refreshes_the_form_without_overwriting_drafts() {
        let previous = status();
        for (key, value) in [
            ("id", json!("another-channel")),
            ("workspace", json!("D:\\another-workspace")),
            ("options", json!({"server": "https://example.com"})),
            (
                "descriptor",
                json!({"name": "Updated plugin", "fields": []}),
            ),
            ("revision", json!(3)),
        ] {
            let mut next = previous.clone();
            next[key] = value;
            assert!(channel_should_rebind(&previous, &next, false), "{key}");
            assert!(!channel_should_rebind(&previous, &next, true), "{key}");
        }
    }

    #[test]
    fn login_countdown_does_not_repaint_but_state_and_qr_changes_do() {
        let previous = json!({
            "id": "attempt", "state": "pending", "expires_in": 180,
            "qr_png": "first-image", "pairing_code": ""
        });
        let mut next = previous.clone();
        assert!(!channel_login_changed(&previous, &next));
        next["expires_in"] = json!(177);
        assert!(!channel_login_changed(&previous, &next));
        for (key, value) in [
            ("id", "new-attempt"),
            ("state", "scanned"),
            ("state", "verification_required"),
            ("state", "confirmed"),
            ("state", "expired"),
            ("pairing_code", "pair-code"),
            ("qr_png", "new-image"),
        ] {
            let mut next = previous.clone();
            next[key] = json!(value);
            assert!(channel_login_changed(&previous, &next), "{key}");
        }
    }

    #[test]
    fn login_completion_survives_history_refresh_but_not_cancellation_or_replacement() {
        let request = json!({
            "operation": "channel.login.poll", "id": "wechat", "login_id": "attempt"
        });
        let login = json!({"id": "attempt", "state": "pending"});
        let mut requests = ChannelRequests::default();
        let poll = requests.begin(ChannelRequestMode::Poll).unwrap();
        let action = requests.begin(ChannelRequestMode::Action).unwrap();
        assert!(!requests.finish(ChannelRequestMode::Poll, poll));
        assert!(channel_login_poll_matches(&request, "wechat", &login));
        assert!(!channel_login_poll_matches(&request, "other", &login));
        assert!(!channel_login_poll_matches(
            &request,
            "wechat",
            &Value::Null
        ));
        assert!(!channel_login_poll_matches(
            &request,
            "wechat",
            &json!({"id": "new-attempt"})
        ));
        assert!(requests.busy());
        assert!(requests.finish(ChannelRequestMode::Action, action));
    }
}
