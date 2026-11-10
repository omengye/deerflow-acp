//! Native tool parameter forms. All data is taken from the in-memory draft.
use super::*;

pub(super) const NAMES: &[&str] = &[
    "web_search",
    "web_fetch",
    "image_search",
    "host_opencli",
    "glob",
    "grep",
    "ls",
    "read_file",
    "write_file",
    "str_replace",
    "move_path",
    "delete_path",
    "bash",
];

pub(super) fn index(document: &Value, selected: usize) -> Option<usize> {
    let name = *NAMES.get(selected)?;
    document["tools"]
        .as_array()?
        .iter()
        .position(|tool| tool["name"] == name)
}

pub(super) fn change_membership(document: &mut Value, selected: usize) -> Result<(), String> {
    let name = *NAMES.get(selected).ok_or("未知工具")?;
    if let Some(index) = index(document, selected) {
        document["tools"]
            .as_array_mut()
            .ok_or("工具列表格式无效")?
            .remove(index);
        return Ok(());
    }
    let descriptors = document["config_schema"]["fields"]
        .as_array()
        .ok_or("缺少工具字段定义")?;
    let mut tool = serde_json::Map::new();
    for field in descriptors {
        let Some(key) = field["path"]
            .as_str()
            .and_then(|path| path.strip_prefix("/tools/*/"))
        else {
            continue;
        };
        if field["for_tools"]
            .as_array()
            .is_some_and(|names| names.iter().any(|item| item == name))
        {
            tool.insert(key.into(), field["default"].clone());
        }
    }
    if !tool.contains_key("name") {
        return Err("当前后端没有该工具的配置定义".into());
    }
    tool.insert("name".into(), json!(name));
    // A re-added entry is a fresh configuration and never borrows hidden keys.
    tool.insert("original_name".into(), json!(""));
    document["tools"]
        .as_array_mut()
        .ok_or("工具列表格式无效")?
        .push(Value::Object(tool));
    Ok(())
}

pub(super) fn fields(document: &Value, selected: usize) -> Vec<Field> {
    let Some(index) = index(document, selected) else {
        return Vec::new();
    };
    let item = &document["tools"][index];
    let name = NAMES[selected];
    let root = format!("/tools/{index}");
    let mut fields = Vec::new();
    for (key, label, hint, kind) in [
        (
            "https_proxy",
            "请求代理",
            "http / https / socks5 / socks5h；支持 $变量名；留空保留已有值",
            Kind::Text,
        ),
        (
            "clear_https_proxy",
            "清除已保存代理",
            "保存时移除代理地址及其中的凭据",
            Kind::Bool,
        ),
        (
            "executable",
            "OpenCLI 可执行文件",
            "留空按默认路径查找；支持 .cmd、绝对路径和环境变量引用",
            Kind::Text,
        ),
        (
            "timeout",
            "调用超时（秒）",
            "0–120；0 使用默认值：OpenCLI 120，网页抓取 10，搜索 30",
            Kind::Integer,
        ),
        (
            "max_results",
            "最多返回结果",
            match name {
                "glob" => "0–1000；0 使用 200",
                "grep" => "0–500；0 使用 100",
                "image_search" => "0–50；0 使用 5",
                _ => "0–20；0 使用 5",
            },
            Kind::Integer,
        ),
        (
            "max_output_chars",
            "网页正文字符上限",
            "0–50000；0 使用 4096",
            Kind::Integer,
        ),
        (
            "allowed_sites",
            "OpenCLI 站点允许列表",
            "每行一个站点名；留空使用内置站点列表；[] 表示空列表",
            Kind::Names,
        ),
    ] {
        if name == "web_search" && key != "max_results" {
            continue;
        }
        if item.get(key).is_some() {
            fields.push(Field::new(format!("{root}/{key}"), label, hint, kind));
        }
    }
    fields
}

impl Waku {
    pub(super) fn render_deerflow_tools(&self, theme: Theme, cx: &mut Context<Self>) -> AnyElement {
        let state = &self.deerflow_settings;
        let Some(document) = &state.draft else {
            return div().into_any_element();
        };
        let selected = state.selected_tool.min(NAMES.len() - 1);
        let mut picker = div()
            .w(px(210.0))
            .flex_shrink_0()
            .flex()
            .flex_col()
            .gap(px(5.0));
        for (index, name) in NAMES.iter().enumerate() {
            if let Some(label) = match index {
                0 => Some("网页"),
                3 => Some("OpenCLI"),
                4 => Some("文件"),
                12 => Some("命令"),
                _ => None,
            } {
                picker = picker.child(
                    div()
                        .mt(px(6.0))
                        .text_size(sp(12.0))
                        .text_color(theme.text_tertiary)
                        .child(label),
                );
            }
            let configured = self::index(document, index).is_some();
            picker = picker.child(
                df_button(
                    SharedString::from(format!("df-tool-{name}")),
                    if index == 0 {
                        format!("{name} · {}", self.current_search_provider_label())
                    } else if configured {
                        format!("{name} · 已配置参数")
                    } else {
                        format!(
                            "{name} · {}",
                            if index <= 3 {
                                "未配置"
                            } else {
                                "默认参数"
                            }
                        )
                    },
                    state.busy(),
                    index == selected,
                    theme,
                    cx,
                    move |this, _, cx| {
                        this.deerflow_settings.selected_tool = index;
                        this.deerflow_rebind(cx);
                        if index <= 1 {
                            this.search_plugins_request("search-status", json!({}), cx);
                        }
                        cx.notify();
                    },
                )
                .w_full()
                .justify_start(),
            );
        }
        let configured = index(document, selected).is_some();
        let mut detail = div().flex_1().min_w_0().flex().flex_col().gap(px(12.0));
        if selected == 0 {
            detail = detail.child(self.render_search_plugin_settings(theme, cx));
        } else if selected == 1 {
            detail = detail.child(self.render_search_fetch_settings(theme, cx));
        }
        detail = detail.child(df_button(
            "df-tool-membership",
            if configured {
                "移除所选工具配置"
            } else {
                "添加所选工具配置"
            },
            !state.can_edit_field("/tools") || !state.errors.is_empty(),
            false,
            theme,
            cx,
            |this, _, cx| {
                if !this.deerflow_settings.can_edit_field("/tools")
                    || !this.deerflow_settings.errors.is_empty()
                {
                    return;
                }
                let selected = this.deerflow_settings.selected_tool;
                if let Some(document) = &mut this.deerflow_settings.draft {
                    if let Err(error) = change_membership(document, selected) {
                        this.deerflow_settings.error = Some(error);
                        cx.notify();
                        return;
                    }
                }
                this.deerflow_settings.dirty = true;
                this.deerflow_settings.edit_generation += 1;
                this.deerflow_settings.notice = None;
                this.deerflow_rebind(cx);
                cx.notify();
            },
        ));
        if let Some(index) = index(document, selected) {
            let tool = &document["tools"][index];
            if matches!(selected, 1 | 2) {
                let independent = tool["clear_https_proxy"] != true
                    && (tool["https_proxy_configured"] == true
                        || tool["https_proxy"]
                            .as_str()
                            .is_some_and(|value| !value.is_empty()));
                detail = detail.child(df_label(
                    "本地请求代理",
                    self.deerflow_proxy_source(independent),
                    theme,
                ));
            }
            let keys = ["api_key", "https_proxy"]
                .into_iter()
                .filter(|key| tool[format!("{key}_configured")] == true)
                .collect::<Vec<_>>();
            if !keys.is_empty() {
                detail = detail.child(df_label(
                    "已有凭据或代理",
                    format!("{} 已配置；留空会保留。", keys.join("、")),
                    theme,
                ));
            }
            if ![
                "api_key",
                "https_proxy",
                "executable",
                "timeout",
                "max_results",
                "max_output_chars",
                "allowed_sites",
            ]
            .iter()
            .any(|key| tool.get(*key).is_some())
            {
                detail = detail.child(df_label(
                    "无额外参数",
                    "该工具通过下方允许/拒绝列表控制使用权限。",
                    theme,
                ));
            }
        }
        if let Some(diagnostics) = document["tool_diagnostics"].as_array() {
            for (index, diagnostic) in diagnostics.iter().enumerate() {
                if let Some(message) = diagnostic.as_str() {
                    detail = detail.child(df_label(
                        format!("保留的旧配置 {}", index + 1),
                        message.to_owned(),
                        theme,
                    ));
                }
            }
        }
        df_group(theme)
            .child(df_label(
                "工具设置",
                "选择工具查看配置。搜索服务的切换和凭据保存即时生效。",
                theme,
            ))
            .child(
                div()
                    .flex()
                    .items_start()
                    .gap(px(18.0))
                    .child(picker)
                    .child(detail),
            )
            .into_any_element()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn deerflow_tools_show_consumed_fields_and_preserve_site_list_semantics() {
        let doc = json!({"tools":[{"name":"host_opencli","executable":"opencli","timeout":0,"allowed_sites":null}]});
        let paths: Vec<_> = fields(&doc, 3)
            .into_iter()
            .map(|field| field.path)
            .collect();
        assert_eq!(
            paths,
            vec![
                "/tools/0/executable",
                "/tools/0/timeout",
                "/tools/0/allowed_sites"
            ]
        );
        assert!(fields(&doc, 0).is_empty());
        assert_eq!(parse_field("", Kind::Names).unwrap(), Value::Null);
        assert_eq!(parse_field("[]", Kind::Names).unwrap(), json!([]));
    }

    #[test]
    fn deerflow_tool_replacement_uses_schema_defaults_without_old_credentials() {
        let mut doc = json!({"tools":[{"name":"web_search","original_name":"web_search","api_key_configured":true,"api_key":""}],"config_schema":{"fields":[
            {"path":"/tools/*/name","for_tools":["web_search"],"default":""},
            {"path":"/tools/*/api_key","for_tools":["web_search"],"default":""},
            {"path":"/tools/*/timeout","for_tools":["web_search"],"default":0},
            {"path":"/tools/*/executable","for_tools":["host_opencli"],"default":""}
        ]}});
        change_membership(&mut doc, 0).unwrap();
        change_membership(&mut doc, 0).unwrap();
        assert_eq!(
            doc["tools"][0],
            json!({"name":"web_search","original_name":"","api_key":"","timeout":0})
        );
        assert!(change_membership(&mut doc, NAMES.len()).is_err());
    }

    #[test]
    fn deerflow_search_parameters_hide_obsolete_credentials_and_transport_fields() {
        let doc = json!({"tools":[{"name":"web_search", "api_key":"", "clear_api_key":false,
            "https_proxy":"", "clear_https_proxy":false, "timeout":30, "max_results":5}]});
        let fields = fields(&doc, 0);
        assert_eq!(fields.len(), 1);
        assert_eq!(fields[0].path, "/tools/0/max_results");
    }
}
