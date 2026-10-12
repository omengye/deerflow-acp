//! Cached local checks; rendering never probes files, programs or the network.
use super::*;

impl Waku {
    pub(super) fn render_deerflow_readiness(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let mut group = df_group(theme)
            .child(df_label("能力就绪检查", "只检查已保存配置和本地依赖，不执行工具、不访问网络、不调用付费模型。此结果不证明运行服务已应用配置。", theme))
            .child(df_button("df-readiness-refresh", "检查已保存配置", state.busy(), false, theme, cx, |this, _, cx| {
                this.deerflow_request("check-readiness", json!({}), cx);
            }));
        if state.dirty || state.needs_apply {
            group = group.child(df_label(
                "草稿或应用状态尚未同步",
                "当前草稿不参与检查；保存并应用后再检查。",
                theme,
            ));
        }
        if let Some(report) = &state.readiness {
            if state.saved_draft.as_ref().map(|d| &d["config_revision"])
                != Some(&report["config_revision"])
            {
                group = group.child(df_label(
                    "检查结果已过期",
                    "配置版本已变化，请重新加载配置并检查。",
                    theme,
                ));
            } else {
                for item in report["items"].as_array().into_iter().flatten().take(8) {
                    let name = match item["id"].as_str() {
                        Some("model") => "默认模型",
                        Some("web_search") => "网页搜索",
                        Some("host_opencli") => "OpenCLI / 浏览器",
                        Some("commands") => "命令执行",
                        _ => continue,
                    };
                    let status = match item["state"].as_str() {
                        Some("ready") => "本地检查通过",
                        Some("not_authorized") => "未授权",
                        Some("missing_credential") => "缺少凭据",
                        Some("missing_dependency") => "缺少依赖",
                        Some("not_configured") => "未配置",
                        _ => "尚未验证",
                    };
                    group = group.child(df_label(
                        format!("{name} · {status}"),
                        item["reason"].as_str().unwrap_or("请重新检查").to_owned(),
                        theme,
                    ));
                }
            }
        }
        let mut links = div().flex().gap(px(6.0));
        for (section, label) in [
            (Section::Models, "模型设置与测试"),
            (Section::Tools, "工具与授权设置"),
        ] {
            links = links.child(df_button(
                SharedString::from(format!("df-readiness-{label}")),
                label,
                state.busy(),
                false,
                theme,
                cx,
                move |this, _, cx| {
                    this.open_settings_page(
                        match section {
                            Section::Models => SettingsPage::DeerFlowModels,
                            _ => SettingsPage::DeerFlowTools,
                        },
                        cx,
                    );
                },
            ));
        }
        group.child(links).into_any_element()
    }
}
