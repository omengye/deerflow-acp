//! Cached search provider settings. All configuration and
//! daemon I/O happens on the background executor, never during rendering.
use super::deerflow_settings::{df_button, df_label};
use super::*;
use serde_json::{Value, json};

mod requests;
use requests::SearchRequests;

pub(super) const PROVIDERS: [(&str, &str); 3] = [
    ("brave", "Brave"),
    ("deepseek", "DeepSeek"),
    ("searxng", "SearXNG"),
];

pub(super) struct SearchPlugins {
    value: Value,
    pub(super) loaded: bool,
    requests: SearchRequests,
    polling: bool,
    configuring: Option<String>,
    config_revision: String,
    advanced: bool,
    auth_header: String,
    clear_key: bool,
    clear_proxy: bool,
    inputs: [Entity<TextInput>; 4],
    error: Option<String>,
    error_slot: Option<usize>,
}

impl SearchPlugins {
    pub(super) fn new(window: &mut Window, cx: &mut App) -> Self {
        let inputs = [
            cx.new(|cx| {
                TextInput::new(window, cx).placeholder("实例地址，例如 https://search.example.com")
            }),
            cx.new(|cx| {
                let mut input = TextInput::new(window, cx).placeholder("API Key；留空保留已有值");
                input.set_masked(true, cx);
                input
            }),
            cx.new(|cx| {
                let mut input = TextInput::new(window, cx).placeholder("代理地址；留空保留已有值");
                input.set_masked(true, cx);
                input
            }),
            cx.new(|cx| TextInput::new(window, cx).placeholder("超时秒数，默认 30")),
        ];
        Self {
            value: Value::Null,
            loaded: false,
            requests: SearchRequests::default(),
            polling: false,
            configuring: None,
            config_revision: String::new(),
            advanced: false,
            auth_header: "Authorization".into(),
            clear_key: false,
            clear_proxy: false,
            inputs,
            error: None,
            error_slot: None,
        }
    }

    fn provider(&self, name: &str) -> Value {
        self.value["providers"]
            .as_array()
            .and_then(|providers| providers.iter().find(|p| p["name"] == name))
            .cloned()
            .unwrap_or(Value::Null)
    }
}

fn state_label(value: &Value) -> &'static str {
    match value["state"].as_str() {
        Some("verified") => "搜索成功",
        Some("configured") => "已配置",
        _ => "待配置",
    }
}

fn configuration_request(
    provider: &str,
    revision: &str,
    key: &str,
    base_url: &str,
    key_configured: bool,
    make_default: bool,
) -> Result<Value, &'static str> {
    let key = key.trim();
    let base_url = base_url.trim();
    if provider == "searxng" && base_url.is_empty() {
        return Err("请填写 SearXNG 实例地址；Key 可以留空。");
    }
    if provider != "searxng" && key.is_empty() && !key_configured {
        return Err("请填写 API Key。");
    }
    Ok(
        json!({"provider":provider,"expected_revision":revision,"make_default":make_default,"api_key":key,"base_url":base_url}),
    )
}

impl Waku {
    pub(super) fn search_plugins_request(
        &mut self,
        operation: &str,
        input: Value,
        cx: &mut Context<Self>,
    ) {
        let mutating = operation != "search-status";
        let Some(generation) = self.search_plugins.requests.begin(mutating) else {
            return;
        };
        let operation = operation.to_owned();
        if mutating {
            self.search_plugins.error = None;
            self.search_plugins.error_slot = None;
            for input in &self.search_plugins.inputs {
                input.update(cx, |input, cx| {
                    input.set_read_only(true);
                    cx.notify();
                });
            }
        }
        let daemon = self.daemon.client();
        cx.spawn(async move |this, cx| {
            let result: anyhow::Result<Value> = cx
                .background_executor()
                .spawn(async move {
                    match daemon.request(
                        Uuid::nil(),
                        Uuid::nil(),
                        waku_client::Command::DeerFlow { operation, input },
                    )? {
                        waku_client::ResponsePayload::DeerFlow { data } => Ok(data),
                        _ => anyhow::bail!("无法读取搜索服务配置"),
                    }
                })
                .await;
            let _ = this.update(cx, |this, cx| {
                if !this.search_plugins.requests.finish(generation) {
                    return;
                }
                if mutating {
                    for input in &this.search_plugins.inputs {
                        input.update(cx, |input, cx| {
                            input.set_read_only(false);
                            cx.notify();
                        });
                    }
                }
                match result {
                    Ok(value) => {
                        this.search_plugins.value = value;
                        this.search_plugins.loaded = true;
                        if mutating || this.search_plugins.configuring.is_none() {
                            let provider =
                                this.search_plugins.configuring.clone().unwrap_or_else(|| {
                                    this.search_plugins.value["default_provider"]
                                        .as_str()
                                        .unwrap_or("brave")
                                        .to_owned()
                                });
                            this.open_search_plugin_config(&provider, cx);
                        }
                    }
                    Err(error) => {
                        this.search_plugins.error = Some(error.to_string());
                        this.search_plugins.error_slot = None;
                    }
                }
                // Poll only while a search task can be waiting or completing.
                // A one-second cadence also observes a natural-language default
                // change made by the model's host tool.
                let working = this
                    .selected_session()
                    .is_some_and(|s| s.provider == ProviderKind::DeerFlow && s.is_busy());
                if working && !this.search_plugins.polling {
                    this.search_plugins.polling = true;
                    cx.spawn(async move |this, cx| {
                        cx.background_executor().timer(Duration::from_secs(1)).await;
                        let _ = this.update(cx, |this, cx| {
                            this.search_plugins.polling = false;
                            this.search_plugins_request("search-status", json!({}), cx);
                        });
                    })
                    .detach();
                }
                cx.notify();
            });
        })
        .detach();
        if mutating {
            cx.notify();
        }
    }

    fn open_search_plugin_config(&mut self, name: &str, cx: &mut Context<Self>) {
        let provider = self.search_plugins.provider(name);
        self.search_plugins.configuring = Some(name.to_owned());
        self.search_plugins.config_revision = self.search_plugins.value["revision"]
            .as_str()
            .unwrap_or("")
            .to_owned();
        self.search_plugins.advanced = false;
        self.search_plugins.clear_key = false;
        self.search_plugins.clear_proxy = false;
        self.search_plugins.error = None;
        self.search_plugins.error_slot = None;
        self.search_plugins.auth_header = provider["auth_header"]
            .as_str()
            .unwrap_or("Authorization")
            .to_owned();
        for (slot, text) in [
            (0, provider["base_url"].as_str().unwrap_or("").to_owned()),
            (1, String::new()),
            (2, String::new()),
            (3, provider["timeout"].as_u64().unwrap_or(30).to_string()),
        ] {
            self.search_plugins.inputs[slot].update(cx, |input, cx| input.set_content(text, cx));
        }
        cx.notify();
    }

    fn save_search_plugin(&mut self, cx: &mut Context<Self>) {
        if self.search_plugins.requests.mutating() || !self.search_plugins.loaded {
            return;
        }
        let Some(provider) = self.search_plugins.configuring.clone() else {
            return;
        };
        let key = self.search_plugins.inputs[1]
            .read(cx)
            .content()
            .trim()
            .to_owned();
        let base_url = self.search_plugins.inputs[0]
            .read(cx)
            .content()
            .trim()
            .to_owned();
        let mut request = match configuration_request(
            &provider,
            &self.search_plugins.config_revision,
            &key,
            &base_url,
            self.search_plugins.provider(&provider)["api_key_configured"] == true,
            true,
        ) {
            Ok(request) => request,
            Err(error) => {
                self.search_plugins.error = Some(error.into());
                self.search_plugins.error_slot =
                    Some(if provider == "searxng" && base_url.is_empty() {
                        0
                    } else {
                        1
                    });
                cx.notify();
                return;
            }
        };
        if self.search_plugins.advanced {
            request["clear_api_key"] = json!(self.search_plugins.clear_key);
            if self.search_plugins.clear_proxy {
                request["https_proxy"] = json!("");
            }
            let timeout = self.search_plugins.inputs[3]
                .read(cx)
                .content()
                .parse::<u64>();
            let Ok(timeout @ 1..=120) = timeout else {
                self.search_plugins.error = Some("超时必须为 1–120 的整数。".into());
                self.search_plugins.error_slot = Some(3);
                cx.notify();
                return;
            };
            request["timeout"] = json!(timeout);
            request["auth_header"] = json!(self.search_plugins.auth_header);
            let proxy = self.search_plugins.inputs[2]
                .read(cx)
                .content()
                .trim()
                .to_owned();
            // A blank masked proxy field retains an existing proxy.
            if !proxy.is_empty() && !self.search_plugins.clear_proxy {
                request["https_proxy"] = json!(proxy);
            }
        }
        self.search_plugins_request("search-configure", request, cx);
    }

    pub(super) fn current_search_provider_label(&self) -> &'static str {
        let current = self.search_plugins.value["default_provider"].as_str();
        PROVIDERS
            .iter()
            .find(|(name, _)| Some(*name) == current)
            .map(|(_, label)| *label)
            .unwrap_or("未配置")
    }

    pub(super) fn select_search_plugin(&mut self, name: &str, cx: &mut Context<Self>) {
        if self.search_plugins.requests.mutating() || !self.search_plugins.loaded {
            return;
        }
        if self.search_plugins.configuring.as_deref() == Some(name)
            && self.search_plugins.value["default_provider"] == name
        {
            return;
        }
        self.open_search_plugin_config(name, cx);
        if self.search_plugins.provider(name)["configured"] == true
            && self.search_plugins.value["default_provider"] != name
        {
            self.search_plugins_request("search-select", json!({"provider":name}), cx);
        }
    }

    fn reload_search_plugins(&mut self, cx: &mut Context<Self>) {
        if !self.search_plugins.requests.begin_refresh() {
            return;
        }
        self.search_plugins.configuring = None;
        self.search_plugins.error = None;
        self.search_plugins.error_slot = None;
        self.search_plugins_request("search-status", json!({}), cx);
        cx.notify();
    }

    fn render_search_field_error(&self, slot: usize, theme: Theme) -> Option<AnyElement> {
        if self.search_plugins.error_slot != Some(slot) {
            return None;
        }
        self.search_plugins.error.as_ref().map(|error| {
            div()
                .text_size(sp(12.5))
                .text_color(theme.danger)
                .child(error.clone())
                .into_any_element()
        })
    }

    pub(super) fn render_search_plugin_settings(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let disabled = self.search_plugins.requests.mutating();
        let mut panel = div().flex().flex_col().gap(px(12.0)).min_w_0();
        panel = panel.child(df_label(
            format!("当前使用：{}", self.current_search_provider_label()),
            "选择已配置的服务立即生效；首次配置后保存并启用。无需重启。",
            theme,
        ));
        panel = panel.child(div().flex().flex_wrap().gap(px(6.0)).children(
            PROVIDERS.into_iter().map(|(name, label)| {
                let status = self.search_plugins.provider(name);
                df_button(
                    SharedString::from(format!("search-provider-{name}")),
                    format!("{label} · {}", state_label(&status)),
                    disabled || !self.search_plugins.loaded,
                    self.search_plugins.configuring.as_deref() == Some(name),
                    theme,
                    cx,
                    move |this, _, cx| this.select_search_plugin(name, cx),
                )
            }),
        ));
        if let Some(provider) = &self.search_plugins.configuring {
            let configured = self.search_plugins.provider(provider)["api_key_configured"] == true;
            panel = panel.child(df_label(
                "代理配置",
                self.deerflow_proxy_source(
                    self.search_plugins.provider(provider)["https_proxy_configured"] == true,
                ),
                theme,
            ));
            if provider == "searxng" {
                panel = panel
                    .child(df_label(
                        "实例地址",
                        "必填，例如 https://search.example.com",
                        theme,
                    ))
                    .child(self.search_plugins.inputs[0].clone())
                    .children(self.render_search_field_error(0, theme));
            }
            panel = panel
                .child(df_label(
                    if provider == "searxng" {
                        "Key（选填）"
                    } else {
                        "API Key"
                    },
                    if configured {
                        "已保存；留空保留已有密钥。"
                    } else if provider == "searxng" {
                        "实例无需认证时留空。"
                    } else {
                        "必填。"
                    },
                    theme,
                ))
                .child(self.search_plugins.inputs[1].clone())
                .children(self.render_search_field_error(1, theme));
            panel = panel.child(df_button(
                "search-advanced",
                "高级设置",
                disabled,
                self.search_plugins.advanced,
                theme,
                cx,
                |this, _, cx| {
                    this.search_plugins.advanced = !this.search_plugins.advanced;
                    cx.notify();
                },
            ));
            if self.search_plugins.advanced {
                if provider != "searxng" {
                    panel = panel
                        .child(df_label("服务地址", "默认使用官方服务地址。", theme))
                        .child(self.search_plugins.inputs[0].clone())
                        .children(self.render_search_field_error(0, theme));
                }
                panel = panel
                    .child(df_label(
                        "请求代理",
                        "留空保留已有代理；支持 http、https、socks5 和 socks5h。",
                        theme,
                    ))
                    .child(self.search_plugins.inputs[2].clone())
                    .child(df_label("超时（秒）", "1–120，默认 30。", theme))
                    .child(self.search_plugins.inputs[3].clone())
                    .children(self.render_search_field_error(3, theme))
                    .child(df_button(
                        "search-clear-proxy",
                        "清除已保存代理",
                        disabled,
                        self.search_plugins.clear_proxy,
                        theme,
                        cx,
                        |this, _, cx| {
                            this.search_plugins.clear_proxy = !this.search_plugins.clear_proxy;
                            cx.notify();
                        },
                    ));
                if provider == "searxng" {
                    panel = panel
                        .child(df_label("认证方式", "填写 Key 后使用所选请求头。", theme))
                        .child(
                            div().flex().flex_wrap().gap(px(6.0)).children(
                                [("Authorization", "Bearer Key"), ("X-API-Key", "X-API-Key")]
                                    .into_iter()
                                    .map(|(header, label)| {
                                        df_button(
                                            SharedString::from(format!("search-auth-{header}")),
                                            label,
                                            disabled,
                                            self.search_plugins.auth_header == header,
                                            theme,
                                            cx,
                                            move |this, _, cx| {
                                                this.search_plugins.auth_header = header.into();
                                                cx.notify();
                                            },
                                        )
                                    }),
                            ),
                        )
                        .child(df_button(
                            "search-clear-key",
                            "清除已保存 Key",
                            disabled,
                            self.search_plugins.clear_key,
                            theme,
                            cx,
                            |this, _, cx| {
                                this.search_plugins.clear_key = !this.search_plugins.clear_key;
                                cx.notify();
                            },
                        ));
                }
            }
            panel = panel.child(df_button(
                "search-save",
                if disabled {
                    "正在保存…"
                } else {
                    "保存并启用"
                },
                disabled,
                false,
                theme,
                cx,
                |this, _, cx| this.save_search_plugin(cx),
            ));
        } else {
            panel = panel.child(df_label("搜索服务", "正在读取配置…", theme));
        }
        if let Some(error) = &self.search_plugins.error
            && self.search_plugins.error_slot.is_none()
        {
            panel = panel.child(
                div()
                    .text_size(sp(12.5))
                    .text_color(theme.danger)
                    .child(error.clone()),
            );
        }
        panel
            .child(df_button(
                "search-refresh",
                "重新读取",
                self.search_plugins.requests.refresh_disabled(),
                false,
                theme,
                cx,
                |this, _, cx| this.reload_search_plugins(cx),
            ))
            .into_any_element()
    }

    pub(super) fn render_search_fetch_settings(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let mut panel = div().flex().flex_col().gap(px(10.0)).child(df_label(
            "网页抓取服务",
            "与搜索服务分别保存，选择后立即生效。DeepSeek Fetch 使用 DeepSeek 搜索服务的代理配置；本地抓取使用下方的请求代理或统一代理。",
            theme,
        ));
        for (provider, label) in [("local", "本地抓取"), ("deepseek", "DeepSeek Fetch")] {
            panel = panel.child(df_button(SharedString::from(format!("search-fetch-{provider}")), label,
                self.search_plugins.requests.mutating() || !self.search_plugins.loaded,
                self.search_plugins.value["fetch_provider"] == provider, theme, cx,
                move |this, _, cx| {
                    this.search_plugins_request("search-configure", json!({
                        "provider":"deepseek", "expected_revision":this.search_plugins.value["revision"],
                        "fetch_provider":provider
                    }), cx);
                }));
        }
        if let Some(error) = &self.search_plugins.error {
            panel = panel.child(
                div()
                    .text_size(sp(12.5))
                    .text_color(theme.danger)
                    .child(error.clone()),
            );
        }
        panel.into_any_element()
    }

    pub(super) fn render_search_configuration_notice(
        &self,
        cx: &mut Context<Self>,
    ) -> Option<AnyElement> {
        if self.selected_session()?.provider != ProviderKind::DeerFlow {
            return None;
        }
        let provider = self.search_plugins.value["awaiting_configuration"]
            .as_array()?
            .first()?
            .as_str()?
            .to_owned();
        let theme = Theme::current(cx);
        Some(
            div()
                .mx(px(10.0))
                .mb(px(8.0))
                .flex()
                .items_center()
                .gap(px(8.0))
                .child(
                    div()
                        .text_size(sp(12.5))
                        .text_color(theme.text_secondary)
                        .child("搜索服务尚未配置"),
                )
                .child(df_button(
                    "search-open-settings",
                    "前往设置",
                    false,
                    false,
                    theme,
                    cx,
                    move |this, _, cx| {
                        this.open_deerflow_search_settings(cx);
                        this.open_search_plugin_config(&provider, cx);
                    },
                ))
                .into_any_element(),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn search_configuration_requires_a_searxng_address_but_not_a_key() {
        assert!(configuration_request("searxng", "form-revision", "", "", false, true).is_err());
        let request = configuration_request(
            "searxng",
            "form-revision",
            "",
            " https://search.example.test ",
            false,
            true,
        )
        .unwrap();
        assert_eq!(request["base_url"], "https://search.example.test");
        assert_eq!(request["api_key"], "");
        assert_eq!(request["expected_revision"], "form-revision");
        assert_eq!(request["make_default"], true);
    }
    #[test]
    fn search_configuration_reuses_a_masked_key_and_does_not_change_default_during_setup() {
        for provider in ["brave", "deepseek"] {
            assert!(configuration_request(provider, "revision", "", "", false, false).is_err());
            let request = configuration_request(provider, "revision", "", "", true, false).unwrap();
            assert_eq!(request["api_key"], "");
            assert_eq!(request["make_default"], false);
            assert_eq!(request["provider"], provider);
        }
    }
}
