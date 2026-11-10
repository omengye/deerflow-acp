use super::*;

pub(super) fn fields() -> Vec<Field> {
    vec![
        Field::new(
            "/network/proxy/enabled",
            "启用统一代理",
            "用于模型请求、网页搜索和网页抓取；关闭后恢复原有连接配置",
            Kind::Bool,
        ),
        Field::new(
            "/network/proxy/url",
            "代理地址",
            "例如 http://127.0.0.1:7890；支持 HTTP、HTTPS、SOCKS5；留空保留已保存地址",
            Kind::Text,
        ),
        Field::new(
            "/network/proxy/no_proxy",
            "绕过代理",
            "每行一个主机、IP、域名或 CIDR；这些目标直接连接",
            Kind::Names,
        ),
        Field::new(
            "/network/proxy/clear_url",
            "清除已保存代理地址",
            "保存时移除地址及其中的凭据；关闭代理开关可保留地址",
            Kind::Bool,
        ),
    ]
}

pub(super) fn probe_notice(data: &Value) -> String {
    if data["ok"] == true {
        format!(
            "代理连接成功，耗时 {} ms。{}",
            data["latency_ms"],
            data["note"].as_str().unwrap_or("")
        )
    } else {
        data["note"]
            .as_str()
            .unwrap_or("代理连接失败，请检查代理地址、认证信息和网络。")
            .to_owned()
    }
}

pub(super) fn source_label(proxy: &Value, independent: bool) -> &'static str {
    if independent {
        "使用单独代理"
    } else if proxy["enabled"] == true {
        "使用统一代理"
    } else {
        "未启用代理"
    }
}

impl Waku {
    pub(in crate::app) fn deerflow_proxy_source(&self, independent: bool) -> String {
        let state = &self.deerflow_settings;
        let proxy = &state.draft.as_ref().unwrap_or(&Value::Null)["network"]["proxy"];
        let label = source_label(proxy, independent);
        if state.dirty || state.needs_apply {
            format!("{label}；全局配置以常用页保存并应用后的值为准")
        } else {
            label.to_owned()
        }
    }
    pub(super) fn render_deerflow_network(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let proxy = &state.draft.as_ref().unwrap_or(&Value::Null)["network"]["proxy"];
        let advanced = state.network_advanced
            || state.search_target.as_deref().is_some_and(|path| {
                matches!(path, "/network/proxy/no_proxy" | "/network/proxy/clear_url")
            });
        let mut group = df_group(theme).child(df_label(
            "网络代理",
            "统一设置模型、网页搜索和网页抓取的网络连接；工具中单独设置的代理优先。",
            theme,
        ));
        for field in &state.fields {
            if matches!(
                field.path.as_str(),
                "/network/proxy/enabled" | "/network/proxy/url"
            ) {
                group = group.child(self.render_deerflow_field(field, theme, cx));
            }
        }
        group = group.child(df_button(
            "df-network-advanced",
            if advanced {
                "收起高级选项"
            } else {
                "高级选项"
            },
            state.busy(),
            advanced,
            theme,
            cx,
            move |this, _, cx| {
                this.deerflow_settings.network_advanced = !advanced;
                this.deerflow_settings.search_target = None;
                cx.notify();
            },
        ));
        if advanced {
            for field in &state.fields {
                if matches!(
                    field.path.as_str(),
                    "/network/proxy/no_proxy" | "/network/proxy/clear_url"
                ) {
                    group = group.child(self.render_deerflow_field(field, theme, cx));
                }
            }
        }
        group = group.child(df_button("df-network-test", if state.pending.as_deref() == Some("test-network") { "正在测试…" } else { "测试连接" },
            !state.can_edit_field("/network/proxy/url") || !state.errors.is_empty() || proxy["enabled"] != true,
            false, theme, cx, |this, _, cx| {
                if let Some(doc) = &this.deerflow_settings.draft {
                    let input = json!({"network": doc["network"]});
                    this.deerflow_settings.network_probe = None;
                    this.deerflow_request("test-network", input, cx);
                }
            })).child(df_label("连接测试", "使用当前填写的代理访问 example.com，验证 HTTPS 连接；测试时不使用绕过列表，不保存配置或调用模型。", theme));
        if let Some(result) = &state.network_probe {
            group = group.child(df_label("测试结果", result.clone(), theme));
        }
        group.into_any_element()
    }
}
