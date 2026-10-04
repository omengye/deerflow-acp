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
            "api_key",
            "Brave API 密钥",
            "留空保留已有密钥；可填写 $变量名",
            Kind::Text,
        ),
        (
            "clear_api_key",
            "清除已保存密钥",
            "保存时移除该工具的密钥",
            Kind::Bool,
        ),
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
        let mut picker = div().flex().flex_wrap().gap(px(6.0));
        for (index, name) in NAMES.iter().enumerate() {
            let configured = self::index(document, index).is_some();
            picker = picker.child(df_button(
                SharedString::from(format!("df-tool-{name}")),
                if configured {
                    format!("{name} · 已配置")
                } else {
                    (*name).to_owned()
                },
                state.busy() || !state.errors.is_empty(),
                index == selected,
                theme,
                cx,
                move |this, _, cx| {
                    this.deerflow_settings.selected_tool = index;
                    this.deerflow_rebind(cx);
                    cx.notify();
                },
            ));
        }
        let configured = index(document, selected).is_some();
        let mut group = df_group(theme)
            .child(df_label("Go 原生工具参数", "网页工具和 OpenCLI 需要配置条目；文件及命令工具另受全局权限控制，移除参数条目不等于禁用工具。", theme))
            .child(picker)
            .child(df_button("df-tool-membership", if configured { "移除所选工具配置" } else { "添加所选工具配置" },
                !state.can_edit_field("/tools") || !state.errors.is_empty(), false, theme, cx,
                |this, _, cx| {
                    if !this.deerflow_settings.can_edit_field("/tools") || !this.deerflow_settings.errors.is_empty() { return; }
                    let selected = this.deerflow_settings.selected_tool;
                    if let Some(document) = &mut this.deerflow_settings.draft {
                        if let Err(error) = change_membership(document, selected) { this.deerflow_settings.error = Some(error); cx.notify(); return; }
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
            let keys = ["api_key", "https_proxy"]
                .into_iter()
                .filter(|key| tool[format!("{key}_configured")] == true)
                .collect::<Vec<_>>();
            if !keys.is_empty() {
                group = group.child(df_label(
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
                group = group.child(df_label(
                    "无额外参数",
                    "该工具通过下方允许/拒绝列表控制使用权限。",
                    theme,
                ));
            }
        }
        if let Some(diagnostics) = document["tool_diagnostics"].as_array() {
            for (index, diagnostic) in diagnostics.iter().enumerate() {
                if let Some(message) = diagnostic.as_str() {
                    group = group.child(df_label(
                        format!("保留的旧配置 {}", index + 1),
                        message.to_owned(),
                        theme,
                    ));
                }
            }
        }
        group.into_any_element()
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
}
