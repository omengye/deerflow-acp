//! The settings page renders bounded, cached projections; all host I/O is background work.
use super::deerflow_settings::df_button;
use super::*;
use serde_json::{Value, json};

const PAGE_SIZE: usize = 10;

#[derive(Default)]
struct Requests {
    generation: u64,
    action: Option<u64>,
    poll: Option<u64>,
}
impl Requests {
    fn begin(&mut self, poll: bool) -> Option<u64> {
        if self.action.is_some() || (poll && self.poll.is_some()) {
            return None;
        }
        if !poll {
            self.poll = None;
        }
        self.generation += 1;
        *if poll {
            &mut self.poll
        } else {
            &mut self.action
        } = Some(self.generation);
        Some(self.generation)
    }
    fn finish(&mut self, poll: bool, generation: u64) -> bool {
        let slot = if poll {
            &mut self.poll
        } else {
            &mut self.action
        };
        if *slot != Some(generation) {
            return false;
        }
        *slot = None;
        generation == self.generation
    }
}

#[derive(Clone)]
struct Guard {
    id: String,
    revision: i64,
    instance: String,
}
impl Guard {
    fn from_item(item: &Value, instance: &str) -> Self {
        Self {
            id: text(&item["spec"], "id").into(),
            revision: item["revision"].as_i64().unwrap_or(0),
            instance: instance.into(),
        }
    }
    fn matches(&self, items: &[Value], instance: &str) -> bool {
        !self.instance.is_empty()
            && self.instance == instance
            && items.iter().any(|item| {
                item["spec"]["id"] == self.id && item["revision"].as_i64() == Some(self.revision)
            })
    }
    fn request(&self, operation: &str) -> Value {
        json!({"operation":operation,"id":self.id,"expected_revision":self.revision,
            "expected_instance_id":self.instance})
    }
}

struct Draft {
    guard: Guard,
    kind: String,
    rearm: bool,
}

pub(super) struct ScheduleSettings {
    items: Vec<Value>,
    status: Value,
    instance: String,
    loaded: bool,
    connected: bool,
    requests: Requests,
    polling: bool,
    error: Option<String>,
    notice: Option<String>,
    needs_review: bool,
    draft: Option<Draft>,
    pub(super) value: Entity<TextInput>,
    pub(super) timezone: Entity<TextInput>,
    preview: Option<(Value, String)>,
    delete: Option<Guard>,
    history_id: String,
    history: Vec<Value>,
    page: usize,
    history_page: usize,
    opening_session: bool,
}
impl ScheduleSettings {
    pub(super) fn new(window: &mut Window, cx: &mut App) -> Self {
        Self {
            items: vec![],
            status: Value::Null,
            instance: String::new(),
            loaded: false,
            connected: false,
            requests: Requests::default(),
            polling: false,
            error: None,
            notice: None,
            needs_review: false,
            draft: None,
            value: cx.new(|cx| TextInput::new(window, cx)),
            timezone: cx.new(|cx| TextInput::new(window, cx)),
            preview: None,
            delete: None,
            history_id: String::new(),
            history: vec![],
            page: 0,
            history_page: 0,
            opening_session: false,
        }
    }
    fn busy(&self) -> bool {
        self.requests.action.is_some() || self.opening_session
    }
    fn ready(&self) -> bool {
        self.connected
            && !self.needs_review
            && self.status["running"] == true
            && self.status["reschedule_version"] == 1
            && !self.instance.is_empty()
    }
    fn item(&self, id: &str) -> Option<&Value> {
        self.items.iter().find(|item| item["spec"]["id"] == id)
    }
    fn timing(&self, cx: &App) -> Result<Value, String> {
        let draft = self.draft.as_ref().ok_or("请先选择要修改的任务")?;
        timing_payload(
            &draft.kind,
            self.value.read(cx).content(),
            self.timezone.read(cx).content(),
        )
    }
}

fn text<'a>(value: &'a Value, key: &str) -> &'a str {
    value[key].as_str().unwrap_or("")
}

fn timing_payload(kind: &str, value: &str, timezone: &str) -> Result<Value, String> {
    let value = value.trim();
    let timezone = timezone.trim();
    if timezone.is_empty() {
        return Err("请填写 IANA 时区，例如 Asia/Shanghai".into());
    }
    let mut timing = json!({"kind":kind,"timezone":timezone});
    match kind {
        "once" => {
            chrono::DateTime::parse_from_rfc3339(value)
                .map_err(|_| "单次时间须含时区偏移，例如 2026-12-01T09:00:00+08:00".to_owned())?;
            timing["at"] = json!(value);
        }
        "interval" => {
            let seconds = value
                .parse::<u64>()
                .ok()
                .filter(|v| (60..=31_536_000).contains(v))
                .ok_or("间隔须为 60 到 31536000 之间的整数秒")?;
            timing["every_seconds"] = json!(seconds);
        }
        "cron" => {
            if value.len() > 128 || value.contains("TZ=") || value.split_whitespace().count() != 5 {
                return Err("Cron 须为五段表达式，时区请单独填写".into());
            }
            timing["cron"] = json!(value);
        }
        _ => return Err("无法识别的定时类型".into()),
    }
    Ok(timing)
}

fn permission_label(approval: &str) -> &'static str {
    match approval {
        "allow_always" => "Full Access",
        "ask" => "询问（无人值守时可能暂停等待处理）",
        "read_only" | "" => "只读",
        _ => "未知权限（未更改）",
    }
}

fn plan_label(item: &Value) -> &'static str {
    if item["paused"] != true {
        return "已启用";
    }
    match text(item, "pause_reason") {
        "exhausted" if item["active"] == true => "单次已触发，不再重复",
        "exhausted" => "单次已结束",
        "interrupted" => "执行中断，需检查会话",
        "needs_attention" => "已暂停，需处理执行问题",
        _ => "已暂停",
    }
}

fn outcome_label(status: &str) -> &'static str {
    match status {
        "running" => "执行中",
        "completed" => "已完成",
        "needs_attention" => "需处理",
        "unknown" => "结果未知，请检查会话",
        _ => "暂无执行结果",
    }
}

fn notification_label(status: &str) -> &'static str {
    match status {
        "ready" => "等待发送",
        "sending" => "发送中",
        "sent" | "delivered" => "已送达",
        "blocked" | "ignored" => "授权或渠道状态变化，未发送",
        "unknown" => "送达结果未知，不自动重发",
        "reviewed" => "已人工核对",
        _ => "无渠道通知记录",
    }
}

fn schedule_rule(item: &Value) -> String {
    let spec = &item["spec"];
    match text(spec, "kind") {
        "once" => format!("单次 · {}", text(spec, "at")),
        "interval" => format!("每 {} 秒", spec["every_seconds"].as_i64().unwrap_or(0)),
        "cron" => format!("Cron · {}", text(spec, "cron")),
        _ => "无法识别的定时规则".into(),
    }
}

fn ended_once(item: &Value) -> bool {
    item["spec"]["kind"] == "once" && text(item, "next_local").is_empty()
}

fn management_message(envelope: &Value) -> String {
    let hint = match text(envelope, "code") {
        "conflict" => "任务已被修改或触发，请核对刷新后的状态，再重新编辑。",
        "busy" => "任务正在执行，不能修改时间或删除。可暂停后续执行，等待当前执行结束。",
        "not_found" => "任务已不存在，请刷新列表。",
        "instance_mismatch" => "后台服务已更换，请刷新并重新编辑。此次修改未提交。",
        "invalid_request" => "定时参数无效，请检查时间、时区和 Cron 表达式。",
        _ => "请求未成功，请检查服务状态。",
    };
    format!("{hint} {}", text(envelope, "error"))
}

fn known_rejection(envelope: &Value) -> bool {
    matches!(
        text(envelope, "code"),
        "conflict" | "busy" | "not_found" | "invalid_request" | "instance_mismatch"
    )
}

impl Waku {
    pub(super) fn schedule_input_edited(&mut self, cx: &mut Context<Self>) {
        self.schedule_settings.preview = None;
        cx.notify();
    }

    pub(super) fn enter_schedules(&mut self, cx: &mut Context<Self>) {
        self.schedule_refresh(false, cx);
        if self.schedule_settings.polling {
            return;
        }
        self.schedule_settings.polling = true;
        cx.spawn(async move |this, cx| {
            loop {
                cx.background_executor().timer(Duration::from_secs(5)).await;
                let keep = this
                    .update(cx, |this, cx| {
                        if this.settings_page != Some(SettingsPage::Schedules) {
                            this.schedule_settings.polling = false;
                            return false;
                        }
                        this.schedule_refresh(true, cx);
                        true
                    })
                    .unwrap_or(false);
                if !keep {
                    break;
                }
            }
        })
        .detach();
    }

    fn schedule_refresh(&mut self, poll: bool, cx: &mut Context<Self>) {
        self.schedule_dispatch(
            json!({"operation":"schedule.snapshot", "id":self.schedule_settings.history_id}),
            poll,
            cx,
        );
    }

    fn schedule_dispatch(&mut self, request: Value, poll: bool, cx: &mut Context<Self>) {
        let Some(generation) = self.schedule_settings.requests.begin(poll) else {
            return;
        };
        let mutation = matches!(
            request["operation"].as_str(),
            Some("schedule.reschedule" | "schedule.delete" | "schedule.pause")
        );
        if !poll {
            self.schedule_settings.error = None;
            self.schedule_settings.notice = None;
            self.schedule_settings
                .value
                .update(cx, |input, _| input.set_read_only(true));
            self.schedule_settings
                .timezone
                .update(cx, |input, _| input.set_read_only(true));
            cx.notify();
        }
        let client = self.daemon.client();
        let sent = request.clone();
        cx.spawn(async move |this, cx| {
            let result: anyhow::Result<Value> = cx.background_executor().spawn(async move {
                match client.request(Uuid::nil(), Uuid::nil(), waku_client::Command::DeerFlow {
                    operation: "manage-envelope".into(), input: request,
                })? {
                    waku_client::ResponsePayload::DeerFlow { data } => Ok(data),
                    _ => anyhow::bail!("无法识别定时任务响应"),
                }
            }).await;
            let _ = this.update(cx, |this, cx| {
                let s = &mut this.schedule_settings;
                if !s.requests.finish(poll, generation) { return; }
                if !poll {
                    s.value.update(cx, |input, _| input.set_read_only(false));
                    s.timezone.update(cx, |input, _| input.set_read_only(false));
                }
                let snapshot = sent["operation"] == "schedule.snapshot";
                match result {
                    Ok(envelope) if envelope["ok"] == true => {
                        let data = &envelope["data"];
                        if snapshot {
                            if let Some(items) = data["items"].as_array() {
                                s.items = items.clone();
                                s.status = data["status"].clone();
                                s.instance = text(&envelope["runtime"], "instance_id").into();
                                s.loaded = true;
                                s.connected = true;
                                s.page = s.page.min(s.items.len().saturating_sub(1) / PAGE_SIZE);
                                if sent["id"] == s.history_id {
                                    s.history = data["history"].as_array().cloned().unwrap_or_default();
                                    s.history_page = s.history_page.min(s.history.len().saturating_sub(1) / PAGE_SIZE);
                                }
                                // A poll never overwrites a draft or hides an actionable error.
                                if poll && s.error.as_deref().is_some_and(|e| e.starts_with("连接不可用")) {
                                    s.error = None;
                                }
                            } else {
                                s.connected = false;
                                s.error = Some("服务返回的任务列表无效，请升级服务后刷新。".into());
                            }
                        } else if sent["operation"] == "schedule.preview" {
                            s.preview = Some((sent["timing"].clone(), text(data, "next_local").into()));
                        } else if mutation {
                            s.connected = false;
                            s.draft = None;
                            s.delete = None;
                            s.preview = None;
                            s.notice = Some(if sent["operation"] == "schedule.delete" {
                                "任务已删除，执行历史和会话保留。"
                            } else { "已保存并立即生效，无需再次确认或应用配置。" }.into());
                        }
                    }
                    Ok(envelope) => {
                        if snapshot {
                            s.connected = false;
                            s.error = Some(format!("无法读取定时任务，请确认后台服务已启动且已升级为支持定时任务页面的版本。{}", text(&envelope, "error")));
                        } else if mutation && !known_rejection(&envelope) {
                            s.needs_review = true;
                            s.connected = false;
                            s.error = Some(format!("服务未确认修改结果，请刷新并核对状态；不会自动重试。{}", text(&envelope, "error")));
                        } else {
                            s.error = Some(management_message(&envelope));
                        }
                    }
                    Err(error) => {
                        s.connected = false;
                        if mutation {
                            s.needs_review = true;
                            s.error = Some(format!("修改结果未知，已停止自动重试。请刷新并核对最新状态后再操作，避免重复重置时间。{error}"));
                        } else {
                            s.error = Some(format!("连接不可用，保留上次读取的数据；这不表示任务为空。{error}"));
                        }
                    }
                }
                cx.notify();
                if mutation {
                    // Read-after-write only: never replay a mutation whose reply was lost.
                    this.schedule_refresh(true, cx);
                }
            });
        }).detach();
    }

    fn schedule_edit(&mut self, id: &str, rearm: bool, cx: &mut Context<Self>) {
        let s = &mut self.schedule_settings;
        if s.busy() || !s.ready() || s.draft.is_some() {
            return;
        }
        let Some(item) = s.item(id).cloned() else {
            return;
        };
        if item["active"] == true {
            return;
        }
        let spec = &item["spec"];
        let kind = text(spec, "kind").to_owned();
        let value = match kind.as_str() {
            "once" if rearm => String::new(),
            "once" => text(spec, "at").to_owned(),
            "cron" => text(spec, "cron").to_owned(),
            _ => spec["every_seconds"].to_string(),
        };
        s.value.update(cx, |input, cx| input.set_content(value, cx));
        s.timezone.update(cx, |input, cx| {
            input.set_content(text(spec, "timezone").to_owned(), cx)
        });
        s.draft = Some(Draft {
            guard: Guard::from_item(&item, &s.instance),
            kind,
            rearm,
        });
        s.preview = None;
        s.delete = None;
        s.error = None;
        s.notice = None;
        self.settings_scroll.set_offset(gpui::Point::default());
        cx.notify();
    }

    fn schedule_change_kind(&mut self, kind: &str, cx: &mut Context<Self>) {
        let s = &mut self.schedule_settings;
        if s.busy() {
            return;
        }
        if let Some(draft) = &mut s.draft {
            if draft.rearm || draft.kind == kind {
                return;
            }
            draft.kind = kind.into();
            s.value.update(cx, |input, cx| {
                input.set_content(
                    match kind {
                        "interval" => "3600",
                        "cron" => "0 9 * * *",
                        _ => "",
                    },
                    cx,
                )
            });
            s.preview = None;
            cx.notify();
        }
    }

    fn schedule_save(&mut self, preview: bool, cx: &mut Context<Self>) {
        let s = &self.schedule_settings;
        let Some(draft) = &s.draft else {
            return;
        };
        if s.busy() || !s.ready() || !draft.guard.matches(&s.items, &s.instance) {
            return;
        }
        let timing = match s.timing(cx) {
            Ok(timing) => timing,
            Err(error) => {
                self.schedule_settings.error = Some(error);
                cx.notify();
                return;
            }
        };
        let request = if preview {
            json!({"operation":"schedule.preview", "timing":timing, "expected_instance_id":draft.guard.instance})
        } else {
            let mut request = draft.guard.request("schedule.reschedule");
            request["timing"] = timing;
            request["rearm"] = json!(draft.rearm);
            request
        };
        self.schedule_dispatch(request, false, cx);
    }

    fn schedule_mutate(&mut self, guard: Guard, paused: Option<bool>, cx: &mut Context<Self>) {
        let s = &self.schedule_settings;
        if s.busy() || !s.ready() || !guard.matches(&s.items, &s.instance) {
            self.schedule_settings.error = Some("任务状态已变化，请刷新并重新操作。".into());
            cx.notify();
            return;
        }
        let mut request = guard.request(if paused.is_some() {
            "schedule.pause"
        } else {
            "schedule.delete"
        });
        if let Some(paused) = paused {
            request["paused"] = json!(paused);
        }
        self.schedule_dispatch(request, false, cx);
    }

    fn schedule_open_session(
        &mut self,
        item: Value,
        entry: Value,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if self.schedule_settings.busy() {
            return;
        }
        if entry["outcome"]["status"] == "running" {
            self.schedule_settings.error = Some(
                "执行会话仍由后台持有；结束或进入待处理状态后再打开，不会抢占当前执行。".into(),
            );
            cx.notify();
            return;
        }
        let id = text(&entry["outcome"], "session_id");
        if id.is_empty() {
            return;
        }
        let timestamp = chrono::DateTime::parse_from_rfc3339(text(&entry, "due"))
            .ok()
            .map(|v| v.timestamp().max(0) as u64)
            .unwrap_or(0);
        let summary = ProviderSessionSummary {
            cursor: ProviderResumeCursor::DeerFlow {
                session_id: id.into(),
            },
            title: format!("定时任务 · {}", text(&item["spec"], "name")),
            cwd: PathBuf::from(text(&item["spec"], "workspace")),
            created_at: timestamp,
            updated_at: timestamp,
        };
        if let Some(id) = self
            .state
            .sessions
            .iter()
            .find(|session| session.provider_cursor.as_ref() == Some(&summary.cursor))
            .map(|s| s.id)
        {
            self.settings_page = None;
            self.select_session(id, cx);
            self.open_deerflow_tasks(cx);
            return;
        }
        self.schedule_settings.opening_session = true;
        let instance = self.schedule_settings.instance.clone();
        let history_id = self.schedule_settings.history_id.clone();
        let full_access = item["spec"]["approval"] == "allow_always";
        let fetch = self
            .store
            .provider_session_history(summary.cursor.clone(), summary.cwd.clone());
        let handle = window.window_handle();
        cx.notify();
        cx.spawn(async move |this, cx| {
            let result = cx.background_executor().spawn(async move { fetch() }).await;
            let _ = handle.update(cx, |_, window, cx| {
                let _ = this.update(cx, |this, cx| {
                    this.schedule_settings.opening_session = false;
                    cx.notify();
                    if this.settings_page != Some(SettingsPage::Schedules)
                        || this.schedule_settings.instance != instance
                        || this.schedule_settings.history_id != history_id
                    {
                        return;
                    }
                    match result {
                        Ok(history) => {
                            let existing = this
                                .state
                                .sessions
                                .iter()
                                .any(|s| s.provider_cursor.as_ref() == Some(&summary.cursor));
                            let id = this.import_provider_session(summary, history, window, cx);
                            if !existing && let Some(session) = this.state.session_mut(id) {
                                session.runtime_mode = if full_access {
                                    RuntimeMode::FullAccess
                                } else {
                                    RuntimeMode::Ask
                                };
                            }
                            this.open_deerflow_tasks(cx);
                        }
                        Err(error) => {
                            this.schedule_settings.error = Some(format!(
                                "无法打开执行会话（可能仍被占用或已被清理），不会重新执行任务。{error}"
                            ));
                            cx.notify();
                        }
                    }
                });
            });
        })
        .detach();
    }

    pub(super) fn render_schedules(&self, cx: &mut Context<Self>) -> AnyElement {
        let theme = Theme::current(cx);
        let s = &self.schedule_settings;
        let busy = s.busy();
        let locked = busy || !s.ready() || s.draft.is_some() || s.delete.is_some();
        let mut body = div().mt(px(16.0)).flex().flex_col().gap(px(16.0))
            .child(div().text_color(theme.text_secondary).child("管理已存在的任务。新建任务默认 Full Access；旧任务权限不变。普通工具无需逐次审批，但专门的安全审核仍然有效。"))
            .child(div().text_color(theme.text_secondary).child("仅在后台服务运行时触发；电脑休眠或关机时不会唤醒。恢复后合并错过的触发，不重放中断的执行。"))
            .child(df_button("schedule-refresh", "刷新状态", busy, false, theme, cx,
                |this, _, cx| this.schedule_refresh(false, cx)));
        if !s.loaded {
            body = body.child(if busy {
                "正在读取定时任务…"
            } else {
                "尚未读取任务，请确认后台服务已启动后刷新。"
            });
        } else if !s.connected {
            body = body.child("服务连接不可用，以下为上次读取的状态，暂不可修改。");
        } else if s.status["reschedule_version"] != 1 {
            body = body.child("后台版本不支持修改定时时间，请升级并重启后台服务。");
        } else if s.status["running"] != true {
            body = body.child(format!(
                "调度器未运行，暂不可修改。{}",
                text(&s.status, "error")
            ));
        } else if s.items.is_empty() {
            body = body.child(
                "当前没有定时任务。可在对话中让 Agent 创建；已授权的微信和 Buzz 渠道也支持。",
            );
        }
        if let Some(error) = &s.error {
            body = body.child(div().text_color(theme.text).child(error.clone()));
        }
        if let Some(notice) = &s.notice {
            body = body.child(notice.clone());
        }
        if s.needs_review {
            body = body
                .child(
                    "上次修改未收到可靠回执。请核对卡片的定时规则、状态与执行历史；不会自动重发。",
                )
                .child(df_button(
                    "schedule-reviewed",
                    "已核对最新状态，放弃旧草稿",
                    busy || !s.connected,
                    false,
                    theme,
                    cx,
                    |this, _, cx| {
                        let s = &mut this.schedule_settings;
                        s.needs_review = false;
                        s.draft = None;
                        s.delete = None;
                        s.preview = None;
                        s.error = None;
                        cx.notify();
                    },
                ));
        }
        if let Some(draft) = &s.draft {
            let stale = !draft.guard.matches(&s.items, &s.instance);
            let disabled = busy || !s.ready() || stale;
            let mut editor = div()
                .p(px(18.0))
                .rounded(px(12.0))
                .bg(theme.raised)
                .flex()
                .flex_col()
                .gap(px(12.0))
                .child(format!(
                    "{} · {}",
                    if draft.rearm {
                        "重新安排单次任务"
                    } else {
                        "修改定时时间"
                    },
                    draft.guard.id
                ));
            if stale {
                editor = editor.child(
                    "任务已变化或服务已重启。草稿保留但不能保存，请取消后重新编辑最新版本。",
                );
            }
            let mut kinds = div().flex().flex_wrap().gap(px(8.0));
            for (kind, label) in [("once", "单次"), ("interval", "固定间隔"), ("cron", "Cron")]
            {
                kinds = kinds.child(df_button(
                    SharedString::from(format!("schedule-kind-{kind}")),
                    label,
                    disabled || draft.rearm,
                    draft.kind == kind,
                    theme,
                    cx,
                    move |this, _, cx| this.schedule_change_kind(kind, cx),
                ));
            }
            editor = editor
                .child(kinds)
                .child(match draft.kind.as_str() {
                    "once" => "执行时间（含偏移，例如 2026-12-01T09:00:00+08:00）",
                    "interval" => "间隔秒数（60–31536000；保存或恢复时从当前时间重新计时）",
                    _ => "五段 Cron（分 时 日 月 周，例如 0 9 * * 1-5）",
                })
                .child(s.value.clone())
                .child("IANA 时区，例如 Asia/Shanghai 或 America/New_York")
                .child(s.timezone.clone())
                .child(if draft.rearm {
                    "保存后重新启用此单次任务，历史执行记录保留。"
                } else {
                    "只修改时间和时区；已暂停的任务仍保持暂停，任务内容、权限和历史不变。"
                });
            let timing = s.timing(cx).ok();
            if let Some((payload, next)) = &s.preview
                && timing.as_ref() == Some(payload)
                && !stale
            {
                editor = editor.child(format!(
                    "下次计划执行预览：{next}（以保存时服务端重新计算为准）"
                ));
            }
            editor = editor.child(
                div()
                    .flex()
                    .flex_wrap()
                    .gap(px(8.0))
                    .child(df_button(
                        "schedule-preview",
                        "预览时间（可选）",
                        disabled,
                        false,
                        theme,
                        cx,
                        |this, _, cx| this.schedule_save(true, cx),
                    ))
                    .child(df_button(
                        "schedule-save",
                        "保存时间",
                        disabled,
                        true,
                        theme,
                        cx,
                        |this, _, cx| this.schedule_save(false, cx),
                    ))
                    .child(df_button(
                        "schedule-cancel",
                        "取消编辑",
                        busy,
                        false,
                        theme,
                        cx,
                        |this, _, cx| {
                            this.schedule_settings.draft = None;
                            this.schedule_settings.preview = None;
                            cx.notify();
                        },
                    )),
            );
            body = body.child(editor);
        }
        if let Some(guard) = &s.delete {
            let valid = guard.matches(&s.items, &s.instance);
            let name = s
                .item(&guard.id)
                .map(|item| text(&item["spec"], "name"))
                .unwrap_or(&guard.id);
            body = body.child(
                div()
                    .p(px(18.0))
                    .rounded(px(12.0))
                    .bg(theme.raised)
                    .flex()
                    .flex_col()
                    .gap(px(8.0))
                    .child(format!(
                        "删除任务「{name}」？此操作取消未来触发，不删除历史和会话。"
                    ))
                    .when(!valid, |el| el.child("任务状态已变化，请取消并重新选择。"))
                    .child(df_button(
                        "schedule-delete-confirm",
                        "确认删除",
                        busy || !s.ready() || !valid,
                        false,
                        theme,
                        cx,
                        {
                            let guard = guard.clone();
                            move |this, _, cx| this.schedule_mutate(guard.clone(), None, cx)
                        },
                    ))
                    .child(df_button(
                        "schedule-delete-cancel",
                        "取消",
                        busy,
                        false,
                        theme,
                        cx,
                        |this, _, cx| {
                            this.schedule_settings.delete = None;
                            cx.notify();
                        },
                    )),
            );
        }
        for item in s.items.iter().skip(s.page * PAGE_SIZE).take(PAGE_SIZE) {
            let spec = &item["spec"];
            let id = text(spec, "id");
            let active = item["active"] == true;
            let paused = item["paused"] == true;
            let ended = ended_once(item);
            let mut card = div()
                .p(px(18.0))
                .rounded(px(12.0))
                .bg(theme.raised)
                .flex()
                .flex_col()
                .gap(px(10.0))
                .child(
                    div()
                        .font_weight(FontWeight::MEDIUM)
                        .child(text(spec, "name").to_owned()),
                )
                .child(format!(
                    "计划：{}{}",
                    plan_label(item),
                    if active { " · 当前执行中" } else { "" }
                ))
                .child(format!(
                    "{} · 时区 {}",
                    schedule_rule(item),
                    text(spec, "timezone")
                ))
                .child(format!(
                    "下次计划执行：{}{}",
                    if text(item, "next_local").is_empty() {
                        "无"
                    } else {
                        text(item, "next_local")
                    },
                    if paused && !ended {
                        "（已暂停，不会触发）"
                    } else {
                        ""
                    }
                ))
                .child(format!(
                    "工具权限：{}",
                    permission_label(text(spec, "approval"))
                ))
                .child(format!(
                    "来源：{} · 通知：{}",
                    if text(&spec["source"], "kind").is_empty() {
                        "本机"
                    } else {
                        text(&spec["source"], "kind")
                    },
                    if spec["notify"].is_object() {
                        text(&spec["notify"], "channel")
                    } else if !text(spec, "notify_wechat").is_empty() {
                        "wechat"
                    } else {
                        "仅保留执行记录"
                    }
                ))
                .child(
                    div()
                        .text_color(theme.text_secondary)
                        .child(format!("工作目录：{}", text(spec, "workspace"))),
                )
                .child(
                    div()
                        .text_color(theme.text_secondary)
                        .child(format!("任务内容（摘要）：{}", text(spec, "prompt"))),
                )
                .child(
                    div()
                        .text_color(theme.text_secondary)
                        .child(format!("任务 ID：{id} · 版本 {}", item["revision"])),
                );
            if !item["last"].is_null() {
                card = card.child(format!(
                    "最近执行：{} · {}",
                    text(&item["last"], "due"),
                    outcome_label(text(&item["last"]["outcome"], "status"))
                ));
                if !text(&item["last"], "notification").is_empty() {
                    card = card.child(format!(
                        "结果通知：{}",
                        notification_label(text(&item["last"], "notification"))
                    ));
                }
                if !text(&item["last"]["outcome"], "text").is_empty() {
                    card = card.child(
                        div()
                            .text_color(theme.text_secondary)
                            .child(text(&item["last"]["outcome"], "text").to_owned()),
                    );
                }
                if !text(&item["last"]["outcome"], "session_id").is_empty() {
                    let item = item.clone();
                    card = card.child(df_button(
                        SharedString::from(format!("schedule-open-{id}")),
                        "打开最近执行会话",
                        busy || item["last"]["outcome"]["status"] == "running",
                        false,
                        theme,
                        cx,
                        move |this, window, cx| {
                            this.schedule_open_session(
                                item.clone(),
                                item["last"].clone(),
                                window,
                                cx,
                            )
                        },
                    ));
                }
            } else {
                card = card.child("最近执行：尚未执行");
            }
            let edit_id = id.to_owned();
            let guard = Guard::from_item(item, &s.instance);
            let delete_guard = guard.clone();
            let history_id = id.to_owned();
            card = card.child(
                div()
                    .flex()
                    .flex_wrap()
                    .gap(px(8.0))
                    .child(df_button(
                        SharedString::from(format!("schedule-edit-{id}")),
                        if ended {
                            "重新安排单次任务"
                        } else {
                            "修改时间"
                        },
                        locked || active,
                        false,
                        theme,
                        cx,
                        move |this, _, cx| this.schedule_edit(&edit_id, ended, cx),
                    ))
                    .child(df_button(
                        SharedString::from(format!("schedule-pause-{id}")),
                        if paused {
                            "恢复后续执行"
                        } else {
                            "暂停后续执行"
                        },
                        locked || (paused && (active || ended)),
                        false,
                        theme,
                        cx,
                        move |this, _, cx| this.schedule_mutate(guard.clone(), Some(!paused), cx),
                    ))
                    .child(df_button(
                        SharedString::from(format!("schedule-delete-{id}")),
                        "删除",
                        locked || active,
                        false,
                        theme,
                        cx,
                        move |this, _, cx| {
                            this.schedule_settings.delete = Some(delete_guard.clone());
                            this.settings_scroll.set_offset(gpui::Point::default());
                            cx.notify();
                        },
                    ))
                    .child(df_button(
                        SharedString::from(format!("schedule-history-{id}")),
                        "执行历史",
                        busy,
                        s.history_id == id,
                        theme,
                        cx,
                        move |this, _, cx| {
                            let s = &mut this.schedule_settings;
                            s.history_id = if s.history_id == history_id {
                                String::new()
                            } else {
                                history_id.clone()
                            };
                            s.history.clear();
                            s.history_page = 0;
                            this.schedule_refresh(false, cx);
                        },
                    )),
            );
            if active {
                card = card.child("执行中不能改时间或删除；暂停仅阻止后续触发。会话由后台持有，结束或进入待处理状态后可打开，不会抢占执行。");
            }
            if s.history_id == id {
                card = card.child("执行历史摘要（最近 100 次；时间含偏移；完整结果请打开会话）");
                if s.history.is_empty() {
                    card = card.child(if !s.connected {
                        "执行历史暂不可用，请恢复连接后刷新。"
                    } else if busy {
                        "正在读取…"
                    } else {
                        "暂无执行记录"
                    });
                }
                for entry in s
                    .history
                    .iter()
                    .skip(s.history_page * PAGE_SIZE)
                    .take(PAGE_SIZE)
                {
                    card = card
                        .child(format!(
                            "{} · {}",
                            text(entry, "due"),
                            outcome_label(text(&entry["outcome"], "status"))
                        ))
                        .child(
                            div()
                                .text_color(theme.text_secondary)
                                .child(text(&entry["outcome"], "text").to_owned()),
                        );
                    if !text(entry, "notification").is_empty() {
                        card = card.child(format!(
                            "结果通知：{}",
                            notification_label(text(entry, "notification"))
                        ));
                    }
                    if !text(&entry["outcome"], "session_id").is_empty() {
                        let item = item.clone();
                        let entry = entry.clone();
                        card = card.child(df_button(
                            SharedString::from(format!(
                                "schedule-history-open-{}",
                                text(&entry, "id")
                            )),
                            "打开会话",
                            busy || entry["outcome"]["status"] == "running",
                            false,
                            theme,
                            cx,
                            move |this, window, cx| {
                                this.schedule_open_session(item.clone(), entry.clone(), window, cx)
                            },
                        ));
                    }
                }
                if s.history.len() > PAGE_SIZE {
                    card = card.child(
                        div()
                            .flex()
                            .gap(px(8.0))
                            .child(df_button(
                                "schedule-history-prev",
                                "上一页历史",
                                busy || s.history_page == 0,
                                false,
                                theme,
                                cx,
                                |this, _, cx| {
                                    this.schedule_settings.history_page =
                                        this.schedule_settings.history_page.saturating_sub(1);
                                    cx.notify();
                                },
                            ))
                            .child(df_button(
                                "schedule-history-next",
                                "下一页历史",
                                busy || (s.history_page + 1) * PAGE_SIZE >= s.history.len(),
                                false,
                                theme,
                                cx,
                                |this, _, cx| {
                                    this.schedule_settings.history_page += 1;
                                    cx.notify();
                                },
                            )),
                    );
                }
            }
            body = body.child(card);
        }
        if s.items.len() > PAGE_SIZE {
            body = body.child(
                div()
                    .flex()
                    .flex_wrap()
                    .gap(px(8.0))
                    .child(format!(
                        "第 {} / {} 页 · 共 {} 项",
                        s.page + 1,
                        s.items.len().div_ceil(PAGE_SIZE),
                        s.items.len()
                    ))
                    .child(df_button(
                        "schedule-prev",
                        "上一页",
                        busy || s.page == 0,
                        false,
                        theme,
                        cx,
                        |this, _, cx| {
                            this.schedule_settings.page =
                                this.schedule_settings.page.saturating_sub(1);
                            this.settings_scroll.set_offset(gpui::Point::default());
                            cx.notify();
                        },
                    ))
                    .child(df_button(
                        "schedule-next",
                        "下一页",
                        busy || (s.page + 1) * PAGE_SIZE >= s.items.len(),
                        false,
                        theme,
                        cx,
                        |this, _, cx| {
                            this.schedule_settings.page += 1;
                            this.settings_scroll.set_offset(gpui::Point::default());
                            cx.notify();
                        },
                    )),
            );
        }
        body.into_any_element()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn deerflow_schedule_notification_is_separate_from_execution() {
        assert_eq!(notification_label("unknown"), "送达结果未知，不自动重发");
        assert_eq!(notification_label("delivered"), "已送达");
        assert_eq!(notification_label("sent"), "已送达");
        assert_eq!(outcome_label("completed"), "已完成");
    }

    #[test]
    fn deerflow_schedule_timing_contains_no_permission_or_task_fields() {
        assert_eq!(
            timing_payload("interval", " 60 ", "Asia/Shanghai").unwrap(),
            json!({"kind":"interval","every_seconds":60,"timezone":"Asia/Shanghai"})
        );
        assert!(timing_payload("interval", "59", "UTC").is_err());
        assert!(timing_payload("once", "2026-12-01T09:00:00", "Asia/Shanghai").is_err());
        assert!(timing_payload("once", "2026-12-01T09:00:00+08:00", "Asia/Shanghai").is_ok());
        assert!(timing_payload("cron", "0 9 * * 1-5", "UTC").is_ok());
        assert!(timing_payload("cron", "TZ=UTC 0 9 * * *", "UTC").is_err());
        assert!(timing_payload("cron", "* * * * * *", "UTC").is_err());
    }

    #[test]
    fn deerflow_schedule_guard_rejects_changed_deleted_and_restarted_tasks() {
        let item = json!({"spec":{"id":"task"},"revision":2});
        let guard = Guard::from_item(&item, "daemon-a");
        assert!(guard.matches(&[item.clone()], "daemon-a"));
        assert!(!guard.matches(&[item], "daemon-b"));
        assert!(!guard.matches(&[], "daemon-a"));
        assert!(!guard.matches(&[json!({"spec":{"id":"task"},"revision":3})], "daemon-a"));
        assert_eq!(
            guard.request("schedule.delete"),
            json!({"operation":"schedule.delete","id":"task","expected_revision":2,"expected_instance_id":"daemon-a"})
        );
    }

    #[test]
    fn deerflow_schedule_actions_supersede_polls() {
        let mut requests = Requests::default();
        let poll = requests.begin(true).unwrap();
        let action = requests.begin(false).unwrap();
        assert!(requests.begin(true).is_none());
        assert!(requests.begin(false).is_none());
        assert!(!requests.finish(true, poll));
        assert!(requests.finish(false, action));
        assert!(requests.begin(true).is_some());
    }

    #[test]
    fn deerflow_schedule_action_readback_does_not_wait_for_stale_poll() {
        let mut requests = Requests::default();
        let stale = requests.begin(true).unwrap();
        let action = requests.begin(false).unwrap();
        assert!(requests.finish(false, action));
        let readback = requests.begin(true).unwrap();
        assert!(!requests.finish(true, stale));
        assert!(requests.finish(true, readback));
    }

    #[test]
    fn deerflow_schedule_labels_do_not_confuse_plan_and_execution() {
        let item =
            json!({"spec":{"kind":"interval"},"paused":true,"pause_reason":"manual","active":true});
        assert_eq!(plan_label(&item), "已暂停");
        assert_eq!(outcome_label("running"), "执行中");
        assert!(!ended_once(&item));
        assert_eq!(permission_label(""), "只读");
        assert_eq!(permission_label("allow_always"), "Full Access");
        assert_eq!(
            plan_label(&json!({"paused":true,"pause_reason":"exhausted","active":true})),
            "单次已触发，不再重复"
        );
    }

    #[test]
    fn deerflow_schedule_generic_errors_require_readback_not_retry() {
        assert!(known_rejection(&json!({"code":"conflict"})));
        assert!(known_rejection(&json!({"code":"instance_mismatch"})));
        assert!(!known_rejection(&json!({"code":"operation_failed"})));
        assert!(!known_rejection(&Value::Null));
    }
}
