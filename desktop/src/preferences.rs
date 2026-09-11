use super::*;
use iced::widget::column;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ModelChoice {
    pub name: Option<String>,
    label: String,
}

impl std::fmt::Display for ModelChoice {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.label)
    }
}

pub fn model_label(doc: &ConfigDocument, name: &str) -> String {
    doc.models
        .iter()
        .find(|m| m.name == name)
        .map(|m| {
            if m.display_name.trim().is_empty() || m.display_name == m.name {
                m.name.clone()
            } else {
                format!("{} ({})", m.display_name, m.name)
            }
        })
        .unwrap_or_else(|| name.to_owned())
}

pub fn model_choices(doc: &ConfigDocument, inherit: &str) -> Vec<ModelChoice> {
    std::iter::once(ModelChoice {
        name: None,
        label: inherit.into(),
    })
    .chain(doc.models.iter().map(|m| ModelChoice {
        name: Some(m.name.clone()),
        label: model_label(doc, &m.name),
    }))
    .collect()
}

pub fn selected_model_choice(
    doc: &ConfigDocument,
    name: &Option<String>,
    inherit: &str,
) -> ModelChoice {
    ModelChoice {
        name: name.clone(),
        label: name
            .as_deref()
            .map(|v| model_label(doc, v))
            .unwrap_or_else(|| inherit.into()),
    }
}

pub fn effective_model(doc: &ConfigDocument) -> (&str, &'static str) {
    if let Some(name) = doc.runtime.model_name.as_deref() {
        (name, "ACP 指定设置")
    } else if let Some(model) = doc
        .runtime
        .agent_name
        .as_ref()
        .and_then(|name| doc.agents.iter().find(|a| &a.name == name))
        .and_then(|a| a.model.as_deref())
    {
        (model, "Agent 指定模型")
    } else {
        (&doc.default_model, "全局默认")
    }
}

pub fn delete_model(
    doc: &mut ConfigDocument,
    name: &str,
    replacement: Option<&str>,
) -> Result<(), String> {
    if doc.models.len() <= 1 {
        return Err("至少需要保留一个模型".into());
    }
    let index = doc
        .models
        .iter()
        .position(|m| m.name == name)
        .ok_or("模型已不存在，请刷新列表")?;
    let references = model_references(doc, name);
    if !references.is_empty() {
        return Err(format!(
            "模型仍被引用：{}。请先修改这些配置。",
            references.join("、")
        ));
    }
    if doc.default_model == name {
        let replacement = replacement
            .filter(|r| *r != name && doc.models.iter().any(|m| &m.name == r))
            .ok_or("请先选择新的默认模型")?;
        doc.default_model = replacement.into();
    }
    doc.models.remove(index);
    Ok(())
}

impl App {
    pub fn select_invalid_model(&mut self) {
        if let Some(index) = self.document.as_ref().and_then(|doc| {
            doc.models
                .iter()
                .position(|m| !valid_provider_path(&m.use_path))
        }) {
            self.selected_model = index;
        }
    }
    pub fn advanced_open(&self, page: Page) -> bool {
        self.advanced_pages.contains(&page)
            || self.validation_issue().is_some_and(|(p, _)| p == page)
    }

    pub fn advanced_panel<'a>(
        &'a self,
        page: Page,
        content: impl Into<Element<'a, Message>>,
    ) -> Element<'a, Message> {
        let (title, description) = match page {
            Page::Models => ("模型高级配置", "调整 Provider 类等底层接入参数"),
            Page::SandboxTools => ("工具高级配置", "调整挂载、输出限制和工具原始 JSON"),
            Page::Runtime => ("会话高级配置", "调整执行超时、长任务续跑和会话保留策略"),
            Page::Memory => ("记忆高级配置", "调整提取模型、写入策略和存储位置"),
            Page::Skills => (
                "自进化高级配置",
                "调整模型分工、发现阈值、评估和自动回滚条件",
            ),
            _ => ("高级配置", "调整此功能的扩展参数"),
        };
        let expanded = self.advanced_open(page);
        let mut section = column![
            button(
                row![
                    ui::icon_view(
                        if expanded {
                            ui::Icon::ChevronDown
                        } else {
                            ui::Icon::ChevronRight
                        },
                        17.0,
                        ui::TEXT_SECONDARY
                    ),
                    column![
                        text(title).size(16),
                        text(description).size(12).color(ui::TEXT_SECONDARY)
                    ]
                    .spacing(5)
                    .width(Fill),
                    text(if expanded { "收起" } else { "展开" }).size(12),
                ]
                .spacing(10)
                .align_y(iced::Alignment::Center)
            )
            .on_press(Message::ToggleAdvanced(page))
            .padding(14)
            .width(Fill)
            .style(ui::nav_button(false))
        ]
        .spacing(10);
        if expanded {
            if let Some((p, error)) = self.validation_issue().filter(|(p, _)| *p == page) {
                let _ = p;
                section = section.push(
                    text(format!("请修正：{error}（修正前保持展开）"))
                        .size(12)
                        .color(ui::DANGER),
                );
            }
            section = section.push(content.into());
        }
        container(section)
            .id(format!("advanced-{page:?}"))
            .padding(10)
            .width(Fill)
            .style(ui::inset_card)
            .into()
    }

    pub fn validation_issue(&self) -> Option<(Page, String)> {
        let mut doc = self.document.as_ref()?.clone();
        if let Some(model) = doc
            .models
            .iter()
            .find(|m| !valid_provider_path(&m.use_path))
        {
            return Some((
                Page::Models,
                format!(
                    "Provider 类格式无效：模型 {}，请使用 module:Class 格式",
                    model.name
                ),
            ));
        }
        if let Err(error) = doc.sync_runtime_inputs() {
            return Some((Page::Runtime, error));
        }
        if let Err(error) = doc.sync_evolution_inputs() {
            return Some((Page::Skills, error));
        }
        if let Err(error) = doc.sync_subagent_editors() {
            return Some((Page::Agents, error));
        }
        if let Err(error) = doc.sync_sandbox_tool_editors() {
            return Some((Page::SandboxTools, error));
        }
        if let Err(error) = doc.sync_memory_inputs() {
            return Some((Page::Memory, error));
        }
        None
    }

    pub fn default_model_summary(&self, doc: &ConfigDocument) -> Element<'_, Message> {
        let (effective, source) = effective_model(doc);
        container(
            column![
                text(format!(
                    "全局默认模型：{}",
                    model_label(doc, &doc.default_model)
                ))
                .size(15),
                row![
                    text(format!(
                        "新 ACP 会话：{} · 来自{source}",
                        model_label(doc, effective)
                    ))
                    .size(13)
                    .width(Fill),
                    ui::action_button("查看 ACP 设置").on_press(Message::Navigate(Page::Runtime))
                ]
                .spacing(12)
                .align_y(iced::Alignment::Center),
                text(if self.dirty {
                    "未保存 · 保存并应用后生效"
                } else if self.pending_apply || self.waiting_apply {
                    "已保存 · 应用后生效"
                } else {
                    "配置预览 · 客户端仍可为具体会话选择其他模型"
                })
                .size(12)
                .color(ui::TEXT_SECONDARY),
            ]
            .spacing(8),
        )
        .padding(14)
        .width(Fill)
        .style(ui::card)
        .into()
    }

    pub fn delete_model_options(&self) -> Element<'_, Message> {
        let Some(Message::DeleteModel(name, replacement)) = self.pending_action.as_ref() else {
            return iced::widget::Space::new().into();
        };
        let Some(doc) = &self.document else {
            return text("配置未加载").into();
        };
        if doc.default_model != *name {
            return text("此操作先修改草稿，保存后写入配置。").size(12).into();
        }
        let choices: Vec<ModelChoice> = doc
            .models
            .iter()
            .filter(|m| &m.name != name)
            .map(|m| ModelChoice {
                name: Some(m.name.clone()),
                label: model_label(doc, &m.name),
            })
            .collect();
        let selected = choices.iter().find(|c| &c.name == replacement).cloned();
        column![
            text("此模型是全局默认，请先选择替代模型："),
            pick_list(choices, selected, |choice| Message::ReplacementModel(
                choice.name.unwrap_or_default()
            ))
            .placeholder("选择新的默认模型")
            .style(ui::pick_list_style),
            text("确认后修改草稿；保存并应用后生效。").size(12),
        ]
        .spacing(8)
        .into()
    }

    pub fn confirmation_ready(&self) -> bool {
        match (&self.pending_action, &self.document) {
            (Some(Message::DeleteModel(name, replacement)), Some(doc)) => {
                doc.default_model != *name
                    || replacement
                        .as_ref()
                        .is_some_and(|r| r != name && doc.models.iter().any(|m| &m.name == r))
            }
            _ => true,
        }
    }
}

fn valid_provider_path(path: &str) -> bool {
    path.split_once(':').is_some_and(|(module, class)| {
        [module, class].iter().all(|s| {
            s.split('.').all(|part| {
                !part.is_empty()
                    && part.chars().enumerate().all(|(i, c)| {
                        c == '_' || c.is_ascii_alphabetic() || (i > 0 && c.is_ascii_digit())
                    })
            })
        })
    })
}

pub fn validation_target(page: Page, error: &str) -> String {
    for (prefix, field) in [
        ("Provider 类", "Provider 类"),
        ("最大连接数", "最大连接数"),
        ("最大并发运行", "最大并发运行"),
        ("Subagent 并发", "Subagent 并发"),
        ("执行超时", "运行超时（秒）"),
        ("最少工具调用", "最少工具调用"),
        ("重复阈值", "重复阈值"),
        ("重复窗口天数", "重复窗口（天）"),
        ("冷却小时", "冷却时间（小时）"),
        ("每日候选上限", "每日候选上限"),
        ("待审候选上限", "待审候选上限"),
        ("候选文件数", "最大文件数"),
        ("候选总字节", "总大小上限（bytes）"),
        ("单文件字节", "单文件上限（bytes）"),
        ("变更行数", "Auto Patch 最大改动行"),
        ("观察次数", "Probation 使用次数"),
        ("回滚失败阈值", "连续失败自动回滚阈值"),
        ("关闭刷新超时", "关闭刷新超时（秒，0.1–300）"),
        ("记忆写入延迟", "写入延迟（秒，1–300）"),
        ("最大事实数", "最大事实数（10–500）"),
        ("事实置信度阈值", "事实置信度（0–1）"),
        ("最大注入 Token", "最大注入 Token（100–8000）"),
        ("检索 Top K", "检索 Top K（1–100）"),
    ] {
        if error.starts_with(prefix) {
            return format!("field-{field}");
        }
    }
    format!("advanced-{page:?}")
}

// Query actual layout bounds after the page and disclosure have been rendered.
// This avoids brittle pixel offsets when fonts, DPI or window dimensions change.
pub fn locate_field(page: Page, target: String) -> Task<Message> {
    use iced::advanced::widget::{Id, Operation, operation::Outcome};
    #[derive(Default)]
    struct FindField {
        target: Option<Id>,
        scroll: Option<Id>,
        field_y: Option<f32>,
        scroll_y: Option<f32>,
    }
    impl Operation<f32> for FindField {
        fn traverse(&mut self, operate: &mut dyn FnMut(&mut dyn Operation<f32>)) {
            operate(self);
        }
        fn container(&mut self, id: Option<&Id>, bounds: iced::Rectangle) {
            if id == self.target.as_ref() {
                self.field_y = Some(bounds.y);
            }
        }
        fn focusable(
            &mut self,
            id: Option<&Id>,
            bounds: iced::Rectangle,
            state: &mut dyn iced::advanced::widget::operation::Focusable,
        ) {
            if id == self.target.as_ref() {
                self.field_y = Some(bounds.y);
                state.focus();
            }
        }
        fn scrollable(
            &mut self,
            id: Option<&Id>,
            bounds: iced::Rectangle,
            _: iced::Rectangle,
            _: iced::Vector,
            _: &mut dyn iced::advanced::widget::operation::Scrollable,
        ) {
            if id == self.scroll.as_ref() {
                self.scroll_y = Some(bounds.y);
            }
        }
        fn finish(&self) -> Outcome<f32> {
            Outcome::Some(
                (self.field_y.unwrap_or(0.0) - self.scroll_y.unwrap_or(0.0) - 32.0).max(0.0),
            )
        }
    }
    iced::advanced::widget::operate(FindField {
        target: Some(Id::from(target)),
        scroll: Some(Id::from(format!("page-{page:?}"))),
        ..Default::default()
    })
    .map(move |offset| Message::ScrollToValidation(page, offset))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn app() -> App {
        let mut doc: ConfigDocument = serde_json::from_str(include_str!(
            "../../tests/fixtures/portable_preferences.json"
        ))
        .unwrap();
        let mut second = doc.models[0].clone();
        second.name = "second".into();
        second.original_name = "second".into();
        second.display_name = "Second model".into();
        doc.models.push(second);
        doc.prepare_editor_state();
        let (mut app, _) = App::new();
        app.document = Some(doc);
        app.busy = false;
        app.dirty = false;
        app
    }

    #[test]
    fn selecting_a_model_does_not_change_default_and_setting_default_preserves_override() {
        let mut app = app();
        let original = app.document.as_ref().unwrap().default_model.clone();
        let _ = app.update(Message::SelectModel(1));
        assert_eq!(app.document.as_ref().unwrap().default_model, original);
        assert!(!app.dirty);
        app.document.as_mut().unwrap().runtime.model_name = Some(original.clone());
        let _ = app.update(Message::DefaultModel("second".into()));
        let doc = app.document.as_ref().unwrap();
        assert_eq!(doc.default_model, "second");
        assert_eq!(effective_model(doc), (original.as_str(), "ACP 指定设置"));
        assert!(app.dirty);
        assert!(!app.busy);
    }

    #[test]
    fn deletion_requires_explicit_replacement_and_cancel_does_not_touch_draft() {
        let mut app = app();
        let before = serde_json::to_value(app.document.as_ref().unwrap()).unwrap();
        let _ = app.update(Message::RemoveModel);
        assert!(!app.confirmation_ready());
        let _ = app.update(Message::ConfirmAction);
        assert!(app.pending_action.is_some());
        let _ = app.update(Message::CancelAction);
        assert!(!app.dirty);
        assert_eq!(
            serde_json::to_value(app.document.as_ref().unwrap()).unwrap(),
            before
        );
        let _ = app.update(Message::RemoveModel);
        let _ = app.update(Message::ReplacementModel("second".into()));
        assert!(app.confirmation_ready());
        let _ = app.update(Message::ConfirmAction);
        let doc = app.document.as_ref().unwrap();
        assert_eq!(doc.models.len(), 1);
        assert_eq!(doc.default_model, "second");
        assert!(app.dirty);
        let mut last = doc.clone();
        assert!(delete_model(&mut last, "second", None).is_err());
    }

    #[test]
    fn referenced_models_and_invalid_replacements_cannot_be_deleted() {
        let mut app = app();
        let doc = app.document.as_mut().unwrap();
        let original = doc.default_model.clone();
        assert!(delete_model(doc, &original, Some("missing")).is_err());
        assert_eq!(doc.models.len(), 2);
        doc.memory.model_name = Some(original.clone());
        assert!(
            delete_model(doc, &original, Some("second"))
                .unwrap_err()
                .contains("记忆提取")
        );
        assert_eq!(doc.default_model, original);
    }

    #[test]
    fn inheritance_uses_agent_then_global_and_display_names_do_not_change_keys() {
        let mut app = app();
        let doc = app.document.as_mut().unwrap();
        assert_eq!(effective_model(doc).1, "全局默认");
        let agent = AgentDocument {
            name: "writer".into(),
            model: Some("second".into()),
            ..Default::default()
        };
        doc.agents.push(agent);
        doc.runtime.agent_name = Some("writer".into());
        assert_eq!(effective_model(doc), ("second", "Agent 指定模型"));
        let choice = selected_model_choice(doc, &Some("second".into()), "继承全局默认");
        assert_eq!(choice.name.as_deref(), Some("second"));
        assert!(choice.to_string().contains("Second model"));
        assert_eq!(model_choices(doc, "继承全局默认")[0].name, None);
    }

    #[test]
    fn disclosures_are_independent_and_preserve_unsaved_values() {
        let mut app = app();
        assert!(app.validation_issue().is_none());
        let _ = app.update(Message::ModelUse("custom.provider:Model".into()));
        let before = serde_json::to_value(app.document.as_ref().unwrap()).unwrap();
        let _ = app.update(Message::ToggleAdvanced(Page::Models));
        assert!(app.advanced_open(Page::Models));
        assert!(!app.advanced_open(Page::Memory));
        let _ = app.update(Message::Navigate(Page::Memory));
        let _ = app.update(Message::ToggleAdvanced(Page::Memory));
        let _ = app.update(Message::Navigate(Page::Models));
        let _ = app.update(Message::ToggleAdvanced(Page::Models));
        assert!(!app.advanced_open(Page::Models));
        assert!(app.advanced_open(Page::Memory));
        assert_eq!(
            serde_json::to_value(app.document.as_ref().unwrap()).unwrap(),
            before
        );
        assert!(app.dirty);
    }

    #[test]
    fn invalid_hidden_fields_expand_and_route_to_the_affected_page() {
        let mut app = app();
        app.document.as_mut().unwrap().memory.debounce_input = "bad".into();
        assert_eq!(app.validation_issue().unwrap().0, Page::Memory);
        assert!(app.advanced_open(Page::Memory));
        let _ = app.update(Message::LocateValidation(Page::Memory));
        assert_eq!(app.page, Page::Memory);
        let _ = app.update(Message::ToggleAdvanced(Page::Memory));
        assert!(app.advanced_open(Page::Memory));
        let _ = app.update(Message::MemoryDebounce("30".into()));
        assert!(app.validation_issue().is_none());
        assert!(app.advanced_open(Page::Memory));
    }
}
