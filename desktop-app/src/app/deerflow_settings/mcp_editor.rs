//! Native editor for Waku's existing stdio MCP sidecar. Host authority remains
//! in the configuration service; adding an entry never grants an executable.
use super::*;

pub(super) fn fields(state: &DeerFlowSettings, doc: &Value) -> Vec<Field> {
    if state.advanced {
        return vec![Field::new(
            "/client_mcp_servers",
            "MCP 原始配置",
            "敏感环境值保持脱敏标记。更改服务器名称、命令或参数后，需重新填写其环境变量值。",
            Kind::Json,
        )];
    }
    let Some(servers) = doc["client_mcp_servers"]
        .as_array()
        .filter(|items| !items.is_empty())
    else {
        return vec![];
    };
    let index = state.selected_mcp.min(servers.len() - 1);
    let root = format!("/client_mcp_servers/{index}");
    let mut fields = vec![
        Field::new(
            format!("{root}/name"),
            "服务器名称",
            "使用唯一名称",
            Kind::Text,
        ),
        Field::new(
            format!("{root}/command"),
            "程序路径",
            "已存在的绝对路径；需由宿主另行授权",
            Kind::Text,
        ),
        Field::new(
            format!("{root}/args"),
            "启动参数",
            "每行一个参数，行内空格保留；留空表示无参数",
            Kind::Arguments,
        ),
    ];
    if let Some(env) = servers[index]["env"]
        .as_array()
        .filter(|items| !items.is_empty())
    {
        let i = state.selected_mcp_env.min(env.len() - 1);
        fields.extend([
            Field::new(
                format!("{root}/env/{i}/name"),
                "环境变量名称",
                "仅传入此 MCP 服务器",
                Kind::Text,
            ),
            Field::new(
                format!("{root}/env/{i}/value"),
                "环境变量值",
                "脱敏标记保留已有值；输入新值会更新凭据",
                Kind::Text,
            ),
        ]);
    }
    fields
}

impl Waku {
    pub(super) fn render_deerflow_mcp_editor(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let servers = state
            .draft
            .as_ref()
            .and_then(|doc| doc["client_mcp_servers"].as_array());
        let selected = state.selected_mcp;
        let editable = state.can_edit_field("/client_mcp_servers") && state.errors.is_empty();
        let mut group = df_group(theme).child(df_label("MCP 服务器", "仅支持 stdio。宿主接入开关和程序允许列表仍需在 config.yaml 中配置，添加服务器不授予执行权限。", theme))
            .child(div().flex().flex_wrap().gap(px(6.0))
                .child(df_button("df-mcp-back", "返回工具", state.busy(), false, theme, cx, |this, _, cx| this.deerflow_select_section(Section::Tools, cx)))
                .child(df_button("df-mcp-add", "添加服务器", !editable || servers.is_some_and(|items| items.len() >= 32), false, theme, cx, |this, _, cx| this.deerflow_mcp_mutation("add", cx)))
                .child(df_button("df-mcp-remove", "移除所选服务器", !editable || servers.is_none_or(|items| items.is_empty()), false, theme, cx, |this, _, cx| this.deerflow_mcp_mutation("remove", cx))));
        if let Some(servers) = servers {
            for (index, server) in servers.iter().enumerate() {
                let name = server["name"]
                    .as_str()
                    .filter(|name| !name.is_empty())
                    .unwrap_or("未命名服务器");
                group = group.child(
                    df_button(
                        SharedString::from(format!("df-mcp-{index}")),
                        name.to_owned(),
                        state.busy(),
                        selected == index,
                        theme,
                        cx,
                        move |this, _, cx| {
                            this.deerflow_settings.selected_mcp = index;
                            this.deerflow_settings.selected_mcp_env = 0;
                            this.deerflow_rebind(cx);
                            cx.notify();
                        },
                    )
                    .w_full()
                    .justify_start(),
                );
            }
            if let Some(env) = servers
                .get(selected)
                .and_then(|server| server["env"].as_array())
            {
                let mut choices = div().flex().flex_wrap().gap(px(6.0));
                for (index, entry) in env.iter().enumerate() {
                    let label = entry["name"]
                        .as_str()
                        .filter(|name| !name.is_empty())
                        .unwrap_or("未命名变量");
                    choices = choices.child(df_button(
                        SharedString::from(format!("df-mcp-env-{index}")),
                        label.to_owned(),
                        state.busy(),
                        state.selected_mcp_env == index,
                        theme,
                        cx,
                        move |this, _, cx| {
                            this.deerflow_settings.selected_mcp_env = index;
                            this.deerflow_rebind(cx);
                            cx.notify();
                        },
                    ));
                }
                group = group
                    .child(df_label(
                        "环境变量",
                        "一次编辑一个变量，已有值不会显示明文。",
                        theme,
                    ))
                    .child(choices)
                    .child(
                        div()
                            .flex()
                            .gap(px(6.0))
                            .child(df_button(
                                "df-mcp-env-add",
                                "添加变量",
                                !editable || env.len() >= 128,
                                false,
                                theme,
                                cx,
                                |this, _, cx| this.deerflow_mcp_mutation("env-add", cx),
                            ))
                            .child(df_button(
                                "df-mcp-env-remove",
                                "移除所选变量",
                                !editable || env.is_empty(),
                                false,
                                theme,
                                cx,
                                |this, _, cx| this.deerflow_mcp_mutation("env-remove", cx),
                            )),
                    );
            }
        }
        group.into_any_element()
    }

    fn deerflow_mcp_mutation(&mut self, action: &str, cx: &mut Context<Self>) {
        if !self.deerflow_settings.can_edit_field("/client_mcp_servers")
            || !self.deerflow_settings.errors.is_empty()
        {
            return;
        }
        let state = &mut self.deerflow_settings;
        let Some(servers) = state
            .draft
            .as_mut()
            .and_then(|doc| doc["client_mcp_servers"].as_array_mut())
        else {
            return;
        };
        match action {
            "add" if servers.len() < 32 => {
                servers.push(json!({"name":"","command":"","args":[],"env":[]}));
                state.selected_mcp = servers.len() - 1;
                state.selected_mcp_env = 0;
            }
            "remove" if state.selected_mcp < servers.len() => {
                servers.remove(state.selected_mcp);
                state.selected_mcp = state.selected_mcp.min(servers.len().saturating_sub(1));
                state.selected_mcp_env = 0;
            }
            "env-add" | "env-remove" => {
                let Some(env) = servers
                    .get_mut(state.selected_mcp)
                    .and_then(|server| server["env"].as_array_mut())
                else {
                    return;
                };
                if action == "env-add" && env.len() < 128 {
                    env.push(json!({"name":"","value":""}));
                    state.selected_mcp_env = env.len() - 1;
                } else if action == "env-remove" && state.selected_mcp_env < env.len() {
                    env.remove(state.selected_mcp_env);
                    state.selected_mcp_env =
                        state.selected_mcp_env.min(env.len().saturating_sub(1));
                } else {
                    return;
                }
            }
            _ => return,
        }
        state.dirty = true;
        state.edit_generation += 1;
        state.notice = None;
        self.deerflow_rebind(cx);
        cx.notify();
    }
}
