//! Native source configuration. All rendering reads the cached settings draft.
use super::*;

pub(super) fn fields(document: &Value, selected: usize) -> Vec<Field> {
    let mut fields = vec![
        Field::new(
            "/skill_sources/import_path",
            "技能导入目录",
            "可选；读取该目录下 public/custom。相对路径以配置文件目录为基准",
            Kind::Text,
        ),
        Field::new(
            "/skill_sources/export_root",
            "托管技能导出目录",
            "必须是已存在的目录；生成可重建的文件视图，托管来源需要配置",
            Kind::Text,
        ),
        Field::new(
            "/skill_sources/include_global",
            "使用全局技能",
            "继承默认：配置导入目录时开启；仅托管来源时关闭。工作区技能不受此项影响",
            Kind::OptionalBool,
        ),
    ];
    if let Some(items) = document["skill_sources"]["managed"].as_array() {
        if !items.is_empty() {
            let index = selected.min(items.len() - 1);
            for (key, label, hint, kind) in [
                (
                    "id",
                    "托管来源标识",
                    "1–64 位小写字母、数字或单个连字符；已有来源应保持标识不变",
                    Kind::Text,
                ),
                (
                    "root",
                    "托管来源目录",
                    "已存在的目录；绑定来源身份，目录中的文件不会自动导入",
                    Kind::Text,
                ),
                (
                    "scope",
                    "来源作用域",
                    "工作区来源仅在指定工作区可见；全局来源还需开启使用全局技能",
                    Kind::Choice(&[("workspace", "工作区"), ("global", "全局")]),
                ),
                (
                    "workspace",
                    "所属工作区目录",
                    "工作区来源必填，来源目录须在其内部；全局来源应清空此项",
                    Kind::Text,
                ),
            ] {
                fields.push(Field::new(
                    format!("/skill_sources/managed/{index}/{key}"),
                    label,
                    hint,
                    kind,
                ));
            }
        }
    }
    fields
}

fn change_sources(document: &mut Value, selected: usize, add: bool) -> Result<usize, String> {
    let items = document["skill_sources"]["managed"]
        .as_array_mut()
        .ok_or("技能来源格式无效，请重新加载")?;
    if add {
        if items.len() >= 32 {
            return Err("最多配置 32 个来源；导入目录中的 public/custom 也计入运行上限".into());
        }
        let name = (1..)
            .map(|i| format!("managed-{i}"))
            .find(|name| !items.iter().any(|item| item["id"].as_str() == Some(name)))
            .unwrap();
        items.push(json!({"id":name,"root":"","scope":"workspace","workspace":""}));
        Ok(items.len() - 1)
    } else {
        if selected >= items.len() {
            return Err("所选来源已变化，请重新选择".into());
        }
        items.remove(selected);
        Ok(selected.min(items.len().saturating_sub(1)))
    }
}

impl Waku {
    fn deerflow_change_skill_sources(&mut self, add: bool, cx: &mut Context<Self>) {
        let state = &mut self.deerflow_settings;
        if !state.can_edit_field("/skill_sources/managed") || !state.errors.is_empty() {
            return;
        }
        let Some(document) = &mut state.draft else {
            return;
        };
        match change_sources(document, state.selected_skill_source, add) {
            Ok(index) => state.selected_skill_source = index,
            Err(error) => {
                state.error = Some(error);
                cx.notify();
                return;
            }
        }
        state.dirty = true;
        state.edit_generation += 1;
        state.notice = None;
        state.error = None;
        state.confirm_reload = false;
        self.deerflow_rebind(cx);
        cx.notify();
    }

    pub(super) fn render_deerflow_skill_sources(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let Some(document) = &state.draft else {
            return div().into_any_element();
        };
        let items = document["skill_sources"]["managed"]
            .as_array()
            .map(Vec::as_slice)
            .unwrap_or(&[]);
        let selected = state
            .selected_skill_source
            .min(items.len().saturating_sub(1));
        let mut picker = div().flex().flex_wrap().gap(px(6.0));
        for (index, item) in items.iter().enumerate() {
            picker = picker.child(df_button(
                SharedString::from(format!("df-skill-source-{index}")),
                item["id"]
                    .as_str()
                    .filter(|id| !id.is_empty())
                    .unwrap_or("未命名来源")
                    .to_owned(),
                state.busy() || !state.errors.is_empty(),
                index == selected,
                theme,
                cx,
                move |this, _, cx| {
                    if this.deerflow_settings.busy() || !this.deerflow_settings.errors.is_empty() {
                        return;
                    }
                    this.deerflow_settings.selected_skill_source = index;
                    this.deerflow_rebind(cx);
                    cx.notify();
                },
            ));
        }
        let editable = state.can_edit_field("/skill_sources/managed") && state.errors.is_empty();
        let mut group = df_group(theme)
            .child(df_label("技能来源", "在下方填写目录和来源配置。目录必须已存在，保存设置不会创建目录或导入托管文件。修改后需要保存并应用。", theme))
            .child(picker)
            .child(div().flex().flex_wrap().gap(px(6.0))
                .child(df_button("df-source-add", "添加托管来源", !editable || items.len() >= 32, false, theme, cx,
                    |this, _, cx| this.deerflow_change_skill_sources(true, cx)))
                .child(df_button("df-source-remove", "从草稿移除所选来源", !editable || items.is_empty(), false, theme, cx,
                    |this, _, cx| this.deerflow_change_skill_sources(false, cx))))
            .child(df_label("来源与已发布技能", "标识、目录和作用域共同确定来源。修改这些值或移除来源会改变技能可见范围，并可能使旧任务无法恢复；数据库历史和磁盘文件仍保留。", theme));
        let source = &document["skill_sources"];
        let global = source["include_global"].as_bool().unwrap_or_else(|| {
            source["import_path"]
                .as_str()
                .is_some_and(|path| !path.is_empty())
        });
        group = group.child(df_label(
            "草稿中的全局技能选择",
            if global {
                "开启：可使用已启用的全局来源技能"
            } else {
                "关闭：只选择当前工作区可见技能"
            },
            theme,
        ));
        for path in document["skill_source_diagnostics"]
            .as_array()
            .into_iter()
            .flatten()
            .filter_map(Value::as_str)
        {
            group = group.child(df_label("技能来源诊断（原文件与配置已保留）", path, theme));
        }
        group.into_any_element()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn deerflow_source_add_remove_preserves_other_settings_and_identity() {
        let mut document = json!({"skill_sources":{"import_path":"../skills","export_root":"../exports","include_global":null,"managed":[{"id":"managed-1","root":"../one","scope":"workspace","workspace":".."}]},"skill_evolution":{"enabled":false},"unrelated":{"token":"fixture"}});
        let original = document.clone();
        let index = change_sources(&mut document, 0, true).unwrap();
        assert_eq!(index, 1);
        assert_eq!(document["skill_sources"]["managed"][1]["id"], "managed-2");
        assert_eq!(
            document["skill_sources"]["managed"][0],
            original["skill_sources"]["managed"][0]
        );
        assert_eq!(document["skill_evolution"], original["skill_evolution"]);
        assert_eq!(change_sources(&mut document, 1, false).unwrap(), 0);
        assert_eq!(document, original);
        assert!(change_sources(&mut document, 7, false).is_err());
        assert_eq!(document, original);
    }

    #[test]
    fn deerflow_source_fields_are_native_and_selection_does_not_materialize_changes() {
        let document = json!({"skill_sources":{"managed":[{"id":"one","root":"relative","scope":"global","workspace":""}]}});
        let original = document.clone();
        let fields = fields(&document, 99);
        assert!(fields.iter().all(|field| !matches!(field.kind, Kind::Json)));
        assert!(
            fields
                .iter()
                .any(|field| field.path == "/skill_sources/managed/0/root")
        );
        assert_eq!(document, original);
    }
}
