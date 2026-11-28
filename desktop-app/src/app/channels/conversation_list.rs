use super::*;

const PAGE_SIZE: usize = 8;

pub(super) fn can_open_channel_task(entry: &Value) -> bool {
    entry["session_id"]
        .as_str()
        .is_some_and(|id| !id.is_empty())
        && (entry["status"] != "history_only" || entry["local_history"] == true)
}

impl ChannelSettings {
    pub(super) fn clamp_history_page(&mut self) {
        self.history_page = self
            .history_page
            .min(self.history.len().saturating_sub(1) / PAGE_SIZE);
    }
}

impl Waku {
    pub(super) fn render_channel_conversations(
        &self,
        busy: bool,
        channel: &Value,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> Div {
        let s = &self.channel_settings;
        let header = div()
            .flex()
            .items_center()
            .justify_between()
            .gap(px(12.0))
            .px(px(16.0))
            .py(px(12.0))
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(8.0))
                    .child(
                        div()
                            .text_size(sp(15.0))
                            .font_weight(FontWeight::SEMIBOLD)
                            .child("最近会话"),
                    )
                    .when(s.history_loaded && !s.history.is_empty(), |el| {
                        el.child(
                            div()
                                .text_size(sp(12.0))
                                .text_color(theme.text_secondary)
                                .child(s.history.len().to_string()),
                        )
                    }),
            )
            .child(df_button(
                "channel-history",
                if s.history_loading {
                    "加载中…"
                } else if s.history_loaded {
                    "刷新"
                } else {
                    "查看最近会话"
                },
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
        let mut panel = div()
            .w_full()
            .min_w_0()
            .rounded(px(12.0))
            .border_1()
            .border_color(theme.border)
            .bg(theme.raised)
            .text_color(theme.text)
            .flex()
            .flex_col()
            .child(header);
        if let Some(error) = &s.history_error {
            panel = panel.child(
                div()
                    .px(px(16.0))
                    .pb(px(12.0))
                    .text_size(sp(12.5))
                    .text_color(theme.danger)
                    .child(format!("无法加载最近会话：{error}")),
            );
        } else if s.history_loaded && s.history.is_empty() {
            panel = panel.child(
                div()
                    .px(px(16.0))
                    .py(px(18.0))
                    .border_t_1()
                    .border_color(theme.border)
                    .text_size(sp(13.0))
                    .text_color(theme.text_secondary)
                    .child("这个渠道还没有会话"),
            );
        }
        for (index, entry) in s
            .history
            .iter()
            .enumerate()
            .skip(s.history_page * PAGE_SIZE)
            .take(PAGE_SIZE)
        {
            panel = panel
                .child(self.render_channel_conversation(index, entry, channel, busy, theme, cx));
        }
        let pages = s.history.len().div_ceil(PAGE_SIZE);
        if pages > 1 {
            let page = s.history_page;
            panel = panel.child(
                div()
                    .flex()
                    .items_center()
                    .justify_between()
                    .gap(px(8.0))
                    .px(px(16.0))
                    .py(px(10.0))
                    .border_t_1()
                    .border_color(theme.border)
                    .text_size(sp(12.0))
                    .text_color(theme.text_secondary)
                    .child(format!("第 {} / {pages} 页", page + 1))
                    .child(
                        div()
                            .flex()
                            .gap(px(6.0))
                            .child(df_button(
                                "channel-history-prev",
                                "上一页",
                                busy || page == 0,
                                false,
                                theme,
                                cx,
                                |this, _, cx| {
                                    this.channel_settings.history_page =
                                        this.channel_settings.history_page.saturating_sub(1);
                                    cx.notify();
                                },
                            ))
                            .child(df_button(
                                "channel-history-next",
                                "下一页",
                                busy || page + 1 >= pages,
                                false,
                                theme,
                                cx,
                                |this, _, cx| {
                                    this.channel_settings.history_page += 1;
                                    this.channel_settings.clamp_history_page();
                                    cx.notify();
                                },
                            )),
                    ),
            );
        }
        panel
    }

    fn render_channel_conversation(
        &self,
        index: usize,
        entry: &Value,
        channel: &Value,
        busy: bool,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> Div {
        let status = entry["status"].as_str().unwrap_or("");
        let can_open = can_open_channel_task(entry);
        let label = match status {
            "" if !can_open => "未关联任务",
            "" => "状态未知",
            "history_only" if entry["local_history"] == true => "本地历史",
            _ => task_status_label(status),
        };
        let color = match status {
            "completed" => theme.success,
            "failed" | "blocked" => theme.danger,
            "waiting_approval" | "waiting_input" | "needs_reconciliation" => theme.warning,
            "running" | "resuming" | "suspending" => theme.accent,
            _ => theme.text_secondary,
        };
        let count = entry["message_count"].as_u64().unwrap_or(0);
        let mut meta = format!("{count} 条消息");
        if let Some(ms) = entry["updated_ms"].as_u64().filter(|&ms| ms > 0) {
            meta.push_str(&format!(" · {}", format_message_time(ms / 1000)));
        }
        if let Some(account) = entry["account"]
            .as_str()
            .filter(|a| !a.is_empty() && channel["account"] != *a)
        {
            meta.push_str(&format!(" · 账号 {account}"));
        }
        let title = entry["title"]
            .as_str()
            .filter(|s| !s.trim().is_empty())
            .unwrap_or("渠道会话");
        let summary = div()
            .min_w_0()
            .flex_1()
            .flex()
            .flex_col()
            .gap(px(6.0))
            .child(
                div()
                    .text_size(sp(14.0))
                    .line_height(sp(20.0))
                    .font_weight(FontWeight::MEDIUM)
                    .line_clamp(2)
                    .child(title.to_owned()),
            )
            .child(
                div()
                    .flex()
                    .flex_wrap()
                    .items_center()
                    .gap(px(8.0))
                    .text_size(sp(12.0))
                    .line_height(sp(17.0))
                    .text_color(theme.text_secondary)
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .gap(px(5.0))
                            .child(div().size(px(6.0)).rounded_full().bg(color))
                            .child(label.to_owned()),
                    )
                    .child(div().min_w_0().truncate().child(meta)),
            );
        let mut row = div()
            .w_full()
            .flex()
            .items_center()
            .gap(px(12.0))
            .px(px(16.0))
            .py(px(12.0))
            .border_t_1()
            .border_color(theme.border)
            .child(summary);
        if can_open {
            let open = entry.clone();
            row = row.child(
                df_button(
                    SharedString::from(format!("channel-task-{index}")),
                    "打开任务",
                    busy,
                    false,
                    theme,
                    cx,
                    move |this, window, cx| this.channel_open_session(open.clone(), window, cx),
                )
                .gap(px(6.0))
                .child(icon(
                    "icons/arrow-up-right.svg",
                    12.0,
                    theme.text_secondary,
                )),
            );
        }
        row
    }
}
