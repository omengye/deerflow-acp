use super::*;
use iced::widget::column;

#[derive(Debug, Default)]
pub struct State {
    pub live: Value,
    pub report: Value,
    pub sessions: Vec<Value>,
    pub selected_session: Option<String>,
    pub selected_backup: Option<String>,
    pub memory: Value,
    pub output: text_editor::Content,
    pub diagnostic_output: text_editor::Content,
    pub soul: text_editor::Content,
    pub provider: Option<String>,
    pub tool_search: String,
}

#[derive(Debug, Clone)]
pub enum Message {
    Inspect,
    Inspected(Result<Value, String>),
    CopyReport,
    CopyClient,
    TestModel,
    ModelTested(Result<Value, String>),
    Provider(String),
    Soul(text_editor::Action),
    Output(text_editor::Action),
    DiagnosticOutput(text_editor::Action),
    ConfirmRestart,
    ConfirmStop,
    ListSessions,
    SessionsLoaded(Result<Value, String>),
    SelectSession(String),
    MemoryLoaded(Result<Value, String>),
    RequestDeleteSession(String),
    DeleteSession(String),
    RequestDeleteFact(String),
    DeleteFact(String),
    Mutated(Result<Value, String>),
    SelectBackup(String),
    RequestRestore,
    Restore,
    Restored(Result<Value, String>),
    History,
    HistoryLoaded(Result<Value, String>),
    RequestRollback(String, u64),
    Rollback(String, u64),
    ToolField(usize, String, String),
    AddTool,
    RemoveTool(usize),
    GroupName(usize, String),
    AddGroup,
    RemoveGroup(usize),
    ToolSearch(String),
}

impl State {
    pub fn confirmation(&self, message: &Message) -> String {
        match message {
            Message::DeleteSession(id) => {
                format!("永久删除会话 {id} 及其 checkpoint。产物文件保留；连接中的会话不能删除。")
            }
            Message::DeleteFact(id) => {
                format!("删除当前工作区/会话作用域中的记忆事实 {id}。这不会清空其他记忆。")
            }
            Message::Restore => format!(
                "恢复备份 {} 的配置。当前配置会自动备份；会话、记忆事实和产物保留。恢复后需应用配置。",
                self.selected_backup.as_deref().unwrap_or("")
            ),
            Message::Rollback(name, version) => {
                format!("将 Skill {name} 恢复至版本 {version}；该操作会创建新的修订记录。")
            }
            _ => "确认执行操作？".into(),
        }
    }
}

impl App {
    pub fn update_portable(&mut self, message: Message) -> Task<super::Message> {
        use super::Message as M;
        // Navigation/results stay responsive; serialize writes and protect drafts.
        if self.busy
            && !matches!(
                message,
                Message::Inspected(_)
                    | Message::ModelTested(_)
                    | Message::SessionsLoaded(_)
                    | Message::MemoryLoaded(_)
                    | Message::Mutated(_)
                    | Message::Restored(_)
                    | Message::HistoryLoaded(_)
                    | Message::Output(_)
                    | Message::DiagnosticOutput(_)
            )
        {
            return Task::none();
        }
        let paths = self.paths.clone();
        match message {
            Message::DiagnosticOutput(action) => {
                if !action.is_edit() {
                    self.portable.diagnostic_output.perform(action);
                }
            }
            Message::Output(action) => {
                if !action.is_edit() {
                    self.portable.output.perform(action);
                }
            }
            Message::ToolSearch(value) => self.portable.tool_search = value,
            Message::ToolField(index, field, value) => {
                if let Some(doc) = &mut self.document {
                    if let Some(tool) = doc.tools.get_mut(index) {
                        tool.insert(field, Value::String(value));
                    }
                    doc.tools_editor =
                        text_editor::Content::with_text(&pretty_json_objects(&doc.tools));
                    self.dirty = true;
                }
            }
            Message::AddTool => {
                if let Some(doc) = &mut self.document {
                    let mut index = doc.tools.len() + 1;
                    while doc
                        .tools
                        .iter()
                        .any(|t| t["name"] == format!("custom_tool_{index}"))
                    {
                        index += 1;
                    }
                    let group = doc
                        .tool_groups
                        .first()
                        .and_then(|g| g["name"].as_str())
                        .unwrap_or("custom");
                    let tool =
                        json!({"name":format!("custom_tool_{index}"),"group":group,"use":""});
                    doc.tools.push(tool.as_object().unwrap().clone());
                    doc.tools_editor =
                        text_editor::Content::with_text(&pretty_json_objects(&doc.tools));
                    self.dirty = true;
                }
            }
            Message::RemoveTool(index) => {
                if let Some(doc) = &mut self.document {
                    if index < doc.tools.len() {
                        doc.tools.remove(index);
                    }
                    doc.tools_editor =
                        text_editor::Content::with_text(&pretty_json_objects(&doc.tools));
                    self.dirty = true;
                }
            }
            Message::GroupName(index, value) => {
                if let Some(doc) = &mut self.document {
                    if let Some(group) = doc.tool_groups.get_mut(index) {
                        let old = group["name"].as_str().unwrap_or("").to_owned();
                        group.insert("name".into(), Value::String(value.clone()));
                        for tool in &mut doc.tools {
                            if tool["group"] == old {
                                tool.insert("group".into(), Value::String(value.clone()));
                            }
                        }
                        for agent in &mut doc.agents {
                            for item in &mut agent.tool_groups {
                                if item == &old {
                                    *item = value.clone();
                                }
                            }
                            agent.tool_groups_input = agent.tool_groups.join(", ");
                        }
                    }
                    doc.tool_groups_editor =
                        text_editor::Content::with_text(&pretty_json_objects(&doc.tool_groups));
                    doc.tools_editor =
                        text_editor::Content::with_text(&pretty_json_objects(&doc.tools));
                    self.dirty = true;
                }
            }
            Message::AddGroup => {
                if let Some(doc) = &mut self.document {
                    let mut index = doc.tool_groups.len() + 1;
                    while doc
                        .tool_groups
                        .iter()
                        .any(|g| g["name"] == format!("custom-{index}"))
                    {
                        index += 1;
                    }
                    doc.tool_groups.push(
                        json!({"name":format!("custom-{index}")})
                            .as_object()
                            .unwrap()
                            .clone(),
                    );
                    doc.tool_groups_editor =
                        text_editor::Content::with_text(&pretty_json_objects(&doc.tool_groups));
                    self.dirty = true;
                }
            }
            Message::RemoveGroup(index) => {
                if let Some(doc) = &mut self.document {
                    if let Some(group) = doc.tool_groups.get(index) {
                        let name = group["name"].as_str().unwrap_or("");
                        if doc.tools.iter().any(|t| t["group"] == name)
                            || doc
                                .agents
                                .iter()
                                .any(|a| a.tool_groups.iter().any(|g| g == name))
                        {
                            self.error =
                                Some(format!("工具组 {name} 仍被工具或 Agent 引用，请先调整引用"));
                            return Task::none();
                        }
                        doc.tool_groups.remove(index);
                        doc.tool_groups_editor =
                            text_editor::Content::with_text(&pretty_json_objects(&doc.tool_groups));
                        self.dirty = true;
                    }
                }
            }
            Message::Soul(action) => {
                let editing = action.is_edit();
                self.portable.soul.perform(action);
                if editing {
                    let value = self.portable.soul.text();
                    self.agent_mut(|agent| agent.soul = value);
                    self.dirty = true;
                }
            }
            Message::Provider(value) => {
                self.portable.provider = Some(value.clone());
                let (class, url) = match value.as_str() {
                    "Anthropic" => (
                        "langchain_anthropic:ChatAnthropic",
                        "https://api.anthropic.com",
                    ),
                    "DeepSeek" => (
                        "langchain_deepseek:ChatDeepSeek",
                        "https://api.deepseek.com",
                    ),
                    _ => ("langchain_openai:ChatOpenAI", "https://api.openai.com/v1"),
                };
                self.model_mut(|model| {
                    model.use_path = class.into();
                    model.base_url = url.into();
                });
                self.dirty = true;
            }
            Message::ConfirmRestart => self.pending_action = Some(M::ApplyNow),
            Message::ConfirmStop => self.pending_action = Some(M::StopDaemon),
            Message::CopyClient => {
                let client = json!({"agent_servers":{"DeerFlow Local":{"type":"custom","command":self.paths.bridge.to_string_lossy(),"args":[]}}});
                self.notice =
                    Some("已复制 Zed 配置；其他 ACP 客户端使用同一 EXE，stdio，参数留空".into());
                return iced::clipboard::write(serde_json::to_string_pretty(&client).unwrap());
            }
            Message::CopyReport => {
                self.notice = Some("已复制脱敏诊断报告；分享前请检查其中的本地路径".into());
                return iced::clipboard::write(self.portable.diagnostic_output.text());
            }
            Message::Inspect => {
                self.busy = true;
                let backup = self.portable.selected_backup.clone();
                return Task::perform(
                    async move { config_service(&paths, "inspect", Some(&json!({"backup":backup}))) },
                    |r| M::Portable(Message::Inspected(r)),
                );
            }
            Message::Inspected(result) => {
                self.busy = false;
                match result {
                    Ok(value) => {
                        self.portable.output = text_editor::Content::with_text(
                            &serde_json::to_string_pretty(&value).unwrap_or_default(),
                        );
                        self.portable.diagnostic_output = text_editor::Content::with_text(
                            &serde_json::to_string_pretty(&value).unwrap_or_default(),
                        );
                        self.portable.report = value;
                        self.error = None;
                    }
                    Err(e) => self.error = Some(e),
                }
            }
            Message::TestModel => {
                let Some(model) = self
                    .document
                    .as_ref()
                    .and_then(|d| d.models.get(self.selected_model))
                    .cloned()
                else {
                    return Task::none();
                };
                self.busy = true;
                self.notice = Some("正在发送一次最小文本请求，最长等待 30 秒…".into());
                return Task::perform(
                    async move { config_service(&paths, "test-model", Some(&json!({"model":model}))) },
                    |r| M::Portable(Message::ModelTested(r)),
                );
            }
            Message::ModelTested(result) => {
                self.busy = false;
                match result {
                    Ok(value) if value["ok"].as_bool() == Some(true) => {
                        self.notice =
                            Some(format!("模型测试成功，耗时 {} ms", value["latency_ms"]));
                        self.error = None;
                    }
                    Ok(value) => {
                        self.error = Some(format!(
                            "模型测试失败：{} · {}",
                            value["error_type"], value["note"]
                        ))
                    }
                    Err(e) => self.error = Some(e),
                }
            }
            Message::ListSessions => {
                self.busy = true;
                return Task::perform(
                    async move { management_service(&paths, &json!({"operation":"session.list"})) },
                    |r| M::Portable(Message::SessionsLoaded(r)),
                );
            }
            Message::SessionsLoaded(result) => {
                self.busy = false;
                match result {
                    Ok(value) => {
                        self.portable.sessions =
                            value["sessions"].as_array().cloned().unwrap_or_default();
                        self.error = None;
                    }
                    Err(e) => self.error = Some(e),
                }
            }
            Message::SelectSession(id) => {
                self.portable.selected_session = Some(id.clone());
                self.portable.memory = Value::Null;
                self.busy = true;
                return Task::perform(
                    async move {
                        management_service(
                            &paths,
                            &json!({"operation":"memory.get","session_id":id}),
                        )
                    },
                    |r| M::Portable(Message::MemoryLoaded(r)),
                );
            }
            Message::MemoryLoaded(result) => {
                self.busy = false;
                match result {
                    Ok(value) => {
                        self.portable.memory = value;
                        self.error = None;
                    }
                    Err(e) => self.error = Some(e),
                }
            }
            Message::RequestDeleteSession(id) => {
                self.pending_action = Some(M::Portable(Message::DeleteSession(id)))
            }
            Message::RequestDeleteFact(id) => {
                self.pending_action = Some(M::Portable(Message::DeleteFact(id)))
            }
            Message::DeleteSession(id) => {
                self.busy = true;
                return Task::perform(
                    async move {
                        management_service(
                            &paths,
                            &json!({"operation":"session.delete","session_id":id}),
                        )
                    },
                    |r| M::Portable(Message::Mutated(r)),
                );
            }
            Message::DeleteFact(id) => {
                self.busy = true;
                let session = self.portable.selected_session.clone();
                return Task::perform(
                    async move {
                        management_service(
                            &paths,
                            &json!({"operation":"memory.delete","session_id":session,"fact_id":id}),
                        )
                    },
                    |r| M::Portable(Message::MemoryLoaded(r)),
                );
            }
            Message::Mutated(result) => {
                self.busy = false;
                match result {
                    Ok(_) => {
                        self.notice = Some("操作完成".into());
                        return self.update_portable(Message::ListSessions);
                    }
                    Err(e) => self.error = Some(e),
                }
            }
            Message::SelectBackup(name) => {
                self.portable.selected_backup = Some(name);
                return self.update_portable(Message::Inspect);
            }
            Message::RequestRestore => {
                if self.dirty {
                    self.error = Some("请先保存或重新加载，处理未保存修改后再恢复".into());
                } else if self.portable.report["preview"]["backup"].as_str()
                    == self.portable.selected_backup.as_deref()
                {
                    self.pending_action = Some(M::Portable(Message::Restore));
                }
            }
            Message::Restore => {
                if self.dirty {
                    self.error = Some("存在未保存修改，请先处理".into());
                    return Task::none();
                }
                self.busy = true;
                let name = self.portable.selected_backup.clone();
                let revision = self.document.as_ref().map(|d| d.config_revision.clone());
                return Task::perform(
                    async move {
                        config_service(
                            &paths,
                            "restore",
                            Some(&json!({"backup":name,"revision":revision})),
                        )
                    },
                    |r| M::Portable(Message::Restored(r)),
                );
            }
            Message::Restored(result) => {
                self.busy = false;
                match result {
                    Ok(_) => {
                        self.pending_apply = true;
                        return self.update(M::Reload);
                    }
                    Err(e) => self.error = Some(e),
                }
            }
            Message::History => {
                self.busy = true;
                return Task::perform(
                    async move { management_service(&paths, &json!({"operation":"proposal.history"})) },
                    |r| M::Portable(Message::HistoryLoaded(r)),
                );
            }
            Message::HistoryLoaded(result) => {
                self.busy = false;
                match result {
                    Ok(value) => {
                        self.portable.output = text_editor::Content::with_text(
                            &serde_json::to_string_pretty(&value).unwrap_or_default(),
                        );
                        self.portable.report["history"] = value;
                        self.error = None;
                    }
                    Err(e) => self.error = Some(e),
                }
            }
            Message::RequestRollback(name, version) => {
                self.pending_action = Some(M::Portable(Message::Rollback(name, version)))
            }
            Message::Rollback(name, version) => {
                self.busy = true;
                return Task::perform(
                    async move {
                        management_service(
                            &paths,
                            &json!({"operation":"proposal.rollback","name":name,"version":version}),
                        )
                    },
                    |r| M::Portable(Message::HistoryLoaded(r)),
                );
            }
        }
        Task::none()
    }

    pub fn onboarding_view(&self) -> Element<'_, super::Message> {
        use super::Message as M;
        container(
            column![
                text("开始使用 DeerFlow").size(22),
                text("1 配置并测试模型 → 2 保存并应用 → 3 复制客户端配置 → 4 在 ACP 客户端验收")
                    .size(14),
                row![
                    ui::action_button("配置模型").on_press(M::Navigate(Page::Models)),
                    ui::action_button("保存并应用")
                        .on_press_maybe((!self.busy && self.dirty).then_some(M::SaveApply))
                        .style(ui::primary_button),
                    ui::action_button("复制客户端配置").on_press(M::Portable(Message::CopyClient)),
                ]
                .spacing(10),
                text(format!(
                    "程序路径：{} · stdio · 参数留空；移动便携目录后请重新复制。",
                    self.paths.bridge.display()
                ))
                .size(12),
            ]
            .spacing(10),
        )
        .padding(16)
        .width(Fill)
        .style(ui::accent_card)
        .into()
    }

    pub fn effective_view(&self) -> Element<'_, super::Message> {
        let Some(doc) = &self.document else {
            return text("配置未加载").into();
        };
        let r = &doc.runtime;
        let agent = r
            .agent_name
            .as_ref()
            .and_then(|name| doc.agents.iter().find(|a| &a.name == name));
        let model = r
            .model_name
            .as_ref()
            .or_else(|| agent.and_then(|a| a.model.as_ref()))
            .unwrap_or(&doc.default_model);
        let subagents = r.subagent_enabled && doc.subagents.enabled;
        let bash = r.enable_bash && doc.sandbox.allow_host_bash;
        let mut content = column![
            text("下次会话的默认能力").size(20),
            text(format!(
                "模型：{model} · Agent：{} · 记忆：{}",
                r.agent_name.as_deref().unwrap_or("默认"),
                r.memory_scope
            )),
            text("客户端的会话选择可覆盖默认值；以下根据当前编辑内容计算，需保存并应用。"),
            text(format!(
                "Subagents：{}（全局与 ACP 开关需同时启用）",
                if subagents { "启用" } else { "禁用" }
            )),
            text(format!(
                "Bash：{}（ACP 与 Host Bash 开关需同时启用，仍受工具白名单限制）",
                if bash { "启用" } else { "禁用" }
            )),
            text(match r.permission_mode.as_str() {
                "off" => "权限：不请求审批",
                "all" => "权限：所有工具请求审批",
                _ => "权限：危险操作请求审批，读取和搜索直接执行",
            }),
        ]
        .spacing(8);
        for tool in &doc.tools {
            let name = tool["name"].as_str().unwrap_or("?");
            let group = tool["group"].as_str().unwrap_or("");
            let reason = if r.tool_denylist.iter().any(|v| v == name) {
                "禁用：ACP 黑名单"
            } else if r
                .tool_allowlist
                .as_ref()
                .is_some_and(|a| !a.iter().any(|v| v == name))
            {
                "禁用：未在 ACP 白名单"
            } else if name == "invoke_acp_agent"
                || [
                    "create_scheduled_task",
                    "list_scheduled_tasks",
                    "set_scheduled_task_enabled",
                    "delete_scheduled_task",
                    "list_scheduled_task_runs",
                ]
                .contains(&name)
            {
                "禁用：便携 ACP 不开放此能力"
            } else if name == "bash" && !bash {
                "禁用：Bash 开关未同时开启"
            } else if (name == "task" || name == "task_status") && !subagents {
                "禁用：Subagents 未同时开启"
            } else if (group.starts_with("host:")
                || tool["use"]
                    .as_str()
                    .is_some_and(|p| p.contains("host_opencli")))
                && !doc.sandbox.allow_host_tools
            {
                "禁用：Host Tools 未开启"
            } else if agent.is_some_and(|a| {
                !a.tool_groups.is_empty() && !a.tool_groups.iter().any(|g| g == group)
            }) {
                "禁用：Agent 工具组限制"
            } else {
                "允许（执行时仍按权限策略审批）"
            };
            content = content.push(text(format!("{name} · {reason}")).size(12));
        }
        container(content).padding(16).style(ui::card).into()
    }

    pub fn data_view(&self) -> Element<'_, super::Message> {
        use super::Message as M;
        let mut content = column![
            text("数据与恢复").size(22),
            text("配置恢复会自动备份当前设置。会话清理永久删除 checkpoint，产物文件保留。记忆按选中会话的当前作用域查看。"),
            row![ui::action_button("备份与存储占用").on_press(M::Portable(Message::Inspect)),
                 ui::action_button("会话与清理预览").on_press(M::Portable(Message::ListSessions)),
                 ui::action_button("自进化历史").on_press(M::Portable(Message::History))].spacing(10),
            ui::action_button("查看旧版共享记忆").on_press(M::Portable(Message::SelectSession("__legacy__".into()))),
        ].spacing(12);
        let backups: Vec<String> = self.portable.report["backups"]
            .as_array()
            .map(|a| {
                a.iter()
                    .filter_map(|v| v.as_str().map(str::to_owned))
                    .collect()
            })
            .unwrap_or_default();
        if let Some(sizes) = self.portable.report["storage_bytes"].as_object() {
            content = content.push(text(format!(
                "存储占用：{}",
                sizes
                    .iter()
                    .map(|(name, bytes)| format!(
                        "{name} {:.1} MB",
                        bytes.as_u64().unwrap_or(0) as f64 / 1048576.0
                    ))
                    .collect::<Vec<_>>()
                    .join(" · ")
            )));
        }
        if !backups.is_empty() {
            content = content.push(
                row![
                    pick_list(backups, self.portable.selected_backup.clone(), |v| {
                        M::Portable(Message::SelectBackup(v))
                    })
                    .placeholder("选择备份并预览"),
                    ui::action_button("恢复所选备份…").on_press_maybe(
                        (self.portable.selected_backup.is_some() && !self.busy && !self.dirty)
                            .then_some(M::Portable(Message::RequestRestore))
                    ),
                ]
                .spacing(10),
            );
        }
        if let Some(preview) = self.portable.report.get("preview") {
            content = content.push(text(format!("恢复预览：{}", preview)).size(13));
        }
        for session in &self.portable.sessions {
            let id = session["session_id"].as_str().unwrap_or("").to_owned();
            let title = session["title"].as_str().unwrap_or("未命名会话");
            let cwd = session["cwd"].as_str().unwrap_or("");
            let expired = session["cleanup_eligible"].as_bool().unwrap_or(false);
            content = content.push(
                row![
                    column![
                        text(format!(
                            "{title} · {}",
                            if expired {
                                "符合清理条件"
                            } else {
                                "保留"
                            }
                        )),
                        text(cwd).size(12)
                    ]
                    .width(Fill),
                    ui::action_button("查看记忆")
                        .on_press(M::Portable(Message::SelectSession(id.clone()))),
                    ui::action_button("删除会话…").on_press_maybe(
                        session["phase"]
                            .is_null()
                            .then_some(M::Portable(Message::RequestDeleteSession(id)))
                    ),
                ]
                .spacing(8),
            );
        }
        if !self.portable.memory.is_null() {
            content = content.push(
                text(format!(
                    "记忆 · {} · {}",
                    self.portable.memory["workspace"], self.portable.memory["scope"]
                ))
                .size(16),
            );
            if let Some(facts) = self.portable.memory["memory"]["facts"].as_array() {
                if facts.is_empty() {
                    content = content.push(text("此作用域暂无记忆事实"));
                }
                for fact in facts {
                    content = content.push(
                        row![
                            text(fact["content"].as_str().unwrap_or("")).width(Fill),
                            ui::action_button("删除事实…").on_press(M::Portable(
                                Message::RequestDeleteFact(
                                    fact["id"].as_str().unwrap_or("").into()
                                )
                            )),
                        ]
                        .spacing(8),
                    );
                }
            }
        }
        if let Some(revisions) = self.portable.report["history"]["revisions"].as_array() {
            for revision in revisions {
                let name = revision["name"].as_str().unwrap_or("").to_owned();
                let version = revision["version"].as_u64().unwrap_or(0);
                content = content.push(
                    row![
                        text(format!("{name} · 版本 {version}")).width(Fill),
                        ui::action_button("回滚至此版本…")
                            .on_press(M::Portable(Message::RequestRollback(name, version)))
                    ]
                    .spacing(8),
                );
            }
        }
        content = content.push(
            text_editor(&self.portable.output)
                .on_action(|a| M::Portable(Message::Output(a)))
                .height(230),
        );
        scrollable(container(content).padding(16).style(ui::card))
            .height(Fill)
            .into()
    }

    pub fn tools_simple_view(&self) -> Element<'_, super::Message> {
        use super::Message as M;
        let Some(doc) = &self.document else {
            return text("配置未加载").into();
        };
        let groups: Vec<String> = doc
            .tool_groups
            .iter()
            .filter_map(|g| g["name"].as_str().map(str::to_owned))
            .collect();
        let mut content = column![
            text("本地工具与权限").size(22),
            text("本地执行共享宿主机权限。下方“工具高级配置”可编辑挂载、输出限制和扩展参数。"),
            checkbox(doc.sandbox.allow_host_bash)
                .label("允许 Host Bash（还需开启 ACP Bash）")
                .on_toggle(M::SandboxAllowHostBash),
            checkbox(doc.sandbox.allow_host_tools)
                .label("允许 Host Tools")
                .on_toggle(M::SandboxAllowHostTools),
            labeled_input(
                "ACP 工具白名单",
                &doc.runtime.tool_allowlist_input,
                "* 表示全部；空表示全部禁用",
                M::RuntimeToolAllowlist
            ),
            labeled_input(
                "ACP 工具黑名单",
                &doc.runtime.tool_denylist_input,
                "逗号分隔，优先于白名单",
                M::RuntimeToolDenylist
            ),
            text("工具组").size(18),
            ui::action_button("添加工具组").on_press(M::Portable(Message::AddGroup)),
        ]
        .spacing(12);
        for (index, group) in doc.tool_groups.iter().enumerate() {
            content = content.push(
                row![
                    text_input("组名", group["name"].as_str().unwrap_or(""))
                        .on_input(move |v| M::Portable(Message::GroupName(index, v))),
                    ui::action_button("移除").on_press(M::Portable(Message::RemoveGroup(index))),
                ]
                .spacing(8),
            );
        }
        content = content
            .push(text("工具定义").size(18))
            .push(ui::action_button("添加工具").on_press(M::Portable(Message::AddTool)))
            .push(
                text_input("搜索工具名称、工具组或实现", &self.portable.tool_search)
                    .on_input(|v| M::Portable(Message::ToolSearch(v))),
            );
        for (index, tool) in doc.tools.iter().enumerate() {
            let query = self.portable.tool_search.to_lowercase();
            if !format!("{} {} {}", tool["name"], tool["group"], tool["use"])
                .to_lowercase()
                .contains(&query)
            {
                continue;
            }
            content = content.push(
                container(
                    column![
                        row![
                            text("名称").width(50),
                            text_input("工具名", tool["name"].as_str().unwrap_or("")).on_input(
                                move |v| M::Portable(Message::ToolField(index, "name".into(), v))
                            ),
                            ui::action_button("移除")
                                .on_press(M::Portable(Message::RemoveTool(index)))
                        ]
                        .spacing(8),
                        row![
                            text("实现").width(50),
                            text_input("module:function", tool["use"].as_str().unwrap_or(""))
                                .on_input(move |v| M::Portable(Message::ToolField(
                                    index,
                                    "use".into(),
                                    v
                                )))
                        ]
                        .spacing(8),
                        row![
                            text("工具组").width(50),
                            pick_list(
                                groups.clone(),
                                tool["group"].as_str().map(str::to_owned),
                                move |v| M::Portable(Message::ToolField(index, "group".into(), v))
                            )
                        ]
                        .spacing(8),
                    ]
                    .spacing(8),
                )
                .padding(12)
                .style(ui::inset_card),
            );
        }
        content = content.push(self.advanced_panel(Page::SandboxTools, self.tools_advanced_view()));
        scrollable(container(content).padding(16).style(ui::card))
            .id("page-SandboxTools")
            .height(Fill)
            .into()
    }
}
