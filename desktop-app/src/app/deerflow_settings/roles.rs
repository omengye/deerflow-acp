//! Role draft operations are in-memory and atomic. Selection never materializes
//! builtin overrides; unknown historical fields remain visible for migration.
use super::*;

pub(super) const NAME_PATH: &str = "/subagents/custom_agents";
const BUILTINS: &[&str] = &["general-purpose", "bash"];
const KEYS: &[&str] = &[
    "description",
    "system_prompt",
    "model",
    "tools",
    "disallowed_tools",
    "skills",
    "timeout_seconds",
    "max_turns",
    "model_settings",
    "thinking_enabled",
    "reasoning_effort",
    "delegate_to",
];

#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) struct RoleKey {
    pub(super) collection: &'static str,
    pub(super) name: String,
}
impl Default for RoleKey {
    fn default() -> Self {
        Self {
            collection: "agents",
            name: "general-purpose".into(),
        }
    }
}
impl RoleKey {
    pub(super) fn root(&self) -> String {
        format!(
            "/subagents/{}/{}",
            self.collection,
            self.name.replace('~', "~0").replace('/', "~1")
        )
    }
    pub(super) fn custom(&self) -> bool {
        self.collection == "custom_agents"
    }
    fn supported(&self) -> bool {
        valid_name(&self.name)
            && if self.custom() {
                !BUILTINS.contains(&self.name.as_str())
            } else {
                BUILTINS.contains(&self.name.as_str())
            }
    }
}

#[derive(Default)]
pub(super) struct RoleEditor {
    pub(super) selected: RoleKey,
    pub(super) pending_name: Option<String>,
    pub(super) choices: Vec<RoleKey>,
    pub(super) diagnostics: Vec<String>,
}
impl RoleEditor {
    pub(super) fn refresh(&mut self, document: &Value) {
        self.choices = BUILTINS
            .iter()
            .map(|name| RoleKey {
                collection: "agents",
                name: (*name).into(),
            })
            .collect();
        self.diagnostics.clear();
        for collection in ["agents", "custom_agents"] {
            let Some(roles) = document["subagents"][collection].as_object() else {
                self.diagnostics.push(format!(
                    "subagents.{collection} 不是角色对象；原值已保留，请先迁移配置。"
                ));
                continue;
            };
            for (name, role) in roles {
                let key = RoleKey {
                    collection,
                    name: name.clone(),
                };
                if !self.choices.contains(&key) {
                    self.choices.push(key.clone());
                }
                if !key.supported() {
                    self.diagnostics.push(format!(
                        "subagents.{collection}.{name} 不是有效的原生角色名；请迁移或从草稿移除。"
                    ));
                }
                let Some(fields) = role.as_object() else {
                    self.diagnostics.push(format!(
                        "subagents.{collection}.{name} 不是角色对象；原值已保留。"
                    ));
                    continue;
                };
                for (field, value) in fields {
                    if !KEYS.contains(&field.as_str()) {
                        self.diagnostics.push(format!("subagents.{collection}.{name}.{field} 尚未支持；原值已保留，需显式迁移。"));
                    }
                    if field == "model_settings" && !value.is_null() {
                        if let Some(options) = value.as_object() {
                            for option in options.keys() {
                                if !["temperature", "max_tokens", "thinking_budget_tokens"]
                                    .contains(&option.as_str())
                                {
                                    self.diagnostics.push(format!("subagents.{collection}.{name}.model_settings.{option} 尚未支持；原值已保留。"));
                                }
                            }
                        } else {
                            self.diagnostics.push(format!("subagents.{collection}.{name}.model_settings 必须为对象或 null；原值已保留。"));
                        }
                    }
                }
            }
        }
        if !self.choices.contains(&self.selected) {
            self.selected = RoleKey::default();
            self.pending_name = None;
        }
    }

    pub(super) fn projected(&self, document: &Value) -> Result<Value, String> {
        match &self.pending_name {
            Some(name) => rename(document, &self.selected, name),
            None => Ok(document.clone()),
        }
    }

    pub(super) fn fields(&self, document: &Value) -> Vec<Field> {
        let key = &self.selected;
        if !key.supported()
            || document
                .pointer(&key.root())
                .is_some_and(|role| !role.is_object())
        {
            return vec![];
        }
        let mut fields = Vec::new();
        if key.custom() {
            fields.push(Field::new(NAME_PATH, "角色名称", "1–64 个英文、数字、下划线或连字符，首字符为英文或数字；确认或保存时更新所有委派引用", Kind::Text));
        }
        let mut add = |path: &str, label: &str, hint: &str, kind| {
            fields.push(Field::new(
                format!("{}/{path}", key.root()),
                label,
                hint,
                kind,
            ))
        };
        add(
            "description",
            "角色描述",
            if key.custom() {
                "必填；说明何时委派给此角色，最多 4096 UTF-8 字节"
            } else {
                "留空使用内置描述，最多 4096 UTF-8 字节"
            },
            Kind::Text,
        );
        add(
            "system_prompt",
            "角色指令",
            if key.custom() {
                "必填；最多 65536 UTF-8 字节"
            } else {
                "留空使用内置指令，最多 65536 UTF-8 字节"
            },
            Kind::Multiline,
        );
        add(
            "model",
            "角色模型",
            "继承创建任务时的父模型，或选择一个已配置模型",
            Kind::ModelReference,
        );
        add(
            "model_settings/temperature",
            "Temperature",
            "留空继承；0–2，Claude 为 0–1，开启 thinking 时须为 1",
            Kind::OptionalNumber,
        );
        add(
            "model_settings/max_tokens",
            "输出 Token 上限",
            "留空继承；1–1073741824，实际仍受宿主和根任务预算限制",
            Kind::OptionalInteger,
        );
        add(
            "thinking_enabled",
            "Thinking",
            "继承 / 开启 / 关闭；仅 Ark 和 Claude 支持显式设置",
            Kind::OptionalBool,
        );
        add(
            "model_settings/thinking_budget_tokens",
            "Thinking Token 预算",
            "留空使用默认；仅 Claude 开启 thinking 时可设置，须小于实际输出上限",
            Kind::OptionalInteger,
        );
        add(
            "reasoning_effort",
            "推理强度",
            "继承或显式指定；按角色模型校验支持范围",
            Kind::OptionalChoice(&[
                ("", "继承"),
                ("low", "低"),
                ("medium", "中"),
                ("high", "高"),
                ("xhigh", "最高"),
            ]),
        );
        add(
            "tools",
            "允许的工具",
            if key.name == "bash" && !key.custom() {
                "留空使用内置 bash 工具集合；[] 不授予；每行一个名称。仍受父级权限限制"
            } else {
                "留空继承父级；[] 不授予；每行一个名称"
            },
            Kind::Names,
        );
        add(
            "disallowed_tools",
            "拒绝的工具",
            "每行一个名称；留空或 [] 不额外拒绝，父级拒绝仍有效",
            Kind::Names,
        );
        add(
            "skills",
            "可用技能",
            "留空继承父级；[] 不授予；每行一个名称",
            Kind::Names,
        );
        add(
            "delegate_to",
            "可委派角色",
            "每行一个角色名；留空或 [] 禁止继续委派；循环和超深度由后端拒绝",
            Kind::Names,
        );
        add(
            "max_turns",
            "角色轮次",
            "留空继承全局设置；1–10000，不能扩大父预算",
            Kind::OptionalInteger,
        );
        add(
            "timeout_seconds",
            "角色超时（秒）",
            "留空继承全局设置；0.001–86400，不能扩大父预算",
            Kind::OptionalNumber,
        );
        fields
    }
}

fn valid_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 64
        && name.as_bytes()[0].is_ascii_alphanumeric()
        && name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
}

fn applied_settings_summary(status: &Value) -> Option<String> {
    let settings = &status["subagent_settings"];
    if status["running"] != true || settings["version"] != "deerflow-subagent-settings-v1" {
        return None;
    }
    let enabled = settings["enabled"].as_bool()?;
    let concurrent = settings["max_concurrent"]
        .as_u64()
        .filter(|value| (1..=64).contains(value))?;
    let source = |value: &Value| match value.as_str() {
        Some("yaml") => "配置文件",
        Some("cli") => "显式命令行参数",
        Some("legacy") => "旧版配置",
        Some("legacy_host_gate") => "旧版角色硬限制",
        Some("default") => "默认值",
        _ => "来源未知",
    };
    let restriction = if settings["tool_policy_allows_task"] == false {
        "；工具策略禁止 task"
    } else {
        ""
    };
    Some(format!(
        "允许子代理：{}（{}{}）；根任务并发：{}（{}）。对新任务生效，已有任务保留原限制。",
        if enabled { "开启" } else { "关闭" },
        source(&settings["enabled_source"]),
        restriction,
        concurrent,
        source(&settings["concurrency_source"])
    ))
}

fn rename(document: &Value, key: &RoleKey, name: &str) -> Result<Value, String> {
    let name = name.trim();
    if !key.custom() {
        return Err("内置角色不能重命名。".into());
    }
    if !valid_name(name) || BUILTINS.contains(&name) {
        return Err("角色名称无效或与内置角色重名。".into());
    }
    let roles = document["subagents"]["custom_agents"]
        .as_object()
        .ok_or("自定义角色列表格式错误。")?;
    if !roles.contains_key(&key.name) {
        return Err("角色已变化，请重新加载。".into());
    }
    if name != key.name && roles.contains_key(name) {
        return Err("角色名称已存在。".into());
    }
    let mut updated = document.clone();
    let roles = updated["subagents"]["custom_agents"]
        .as_object_mut()
        .unwrap();
    let role = roles.remove(&key.name).unwrap();
    roles.insert(name.to_owned(), role);
    for collection in ["agents", "custom_agents"] {
        if let Some(roles) = updated["subagents"][collection].as_object_mut() {
            for role in roles.values_mut() {
                if let Some(references) = role.get_mut("delegate_to").and_then(Value::as_array_mut)
                {
                    for reference in references {
                        if reference.as_str() == Some(key.name.as_str()) {
                            *reference = json!(name);
                        }
                    }
                }
            }
        }
    }
    Ok(updated)
}

// Only an explicit field edit may create a sparse builtin override. Refuse to
// replace malformed historical values, even when their child cannot be addressed.
pub(super) fn set_field(document: &mut Value, path: &str, value: Value) -> Result<(), String> {
    let parts: Vec<_> = path.split('/').collect();
    if parts.len() < 5
        || parts[1] != "subagents"
        || !["agents", "custom_agents"].contains(&parts[2])
    {
        return Err("无效角色字段。".into());
    }
    let mut updated = document.clone();
    let mut cursor = &mut updated;
    for part in &parts[1..parts.len() - 1] {
        let name = part.replace("~1", "/").replace("~0", "~");
        let object = cursor
            .as_object_mut()
            .ok_or("旧角色字段格式不兼容，请先迁移。")?;
        cursor = object.entry(name).or_insert_with(|| json!({}));
        if cursor.is_null() && *part == "model_settings" {
            *cursor = json!({});
        }
    }
    let parent = cursor
        .as_object_mut()
        .ok_or("旧角色字段格式不兼容，请先迁移。")?;
    parent.insert(parts.last().unwrap().to_string(), value);
    *document = updated;
    Ok(())
}

fn remove_role(document: &Value, key: &RoleKey) -> Result<Value, String> {
    if key.custom() {
        for collection in ["agents", "custom_agents"] {
            if document["subagents"][collection]
                .as_object()
                .is_some_and(|roles| {
                    roles.iter().any(|(name, role)| {
                        !(collection == key.collection && name == &key.name)
                            && role["delegate_to"]
                                .as_array()
                                .is_some_and(|items| items.iter().any(|item| item == &key.name))
                    })
                })
            {
                return Err("该角色仍被其他角色委派；先删除相应的可委派角色引用。".into());
            }
        }
    }
    let mut updated = document.clone();
    let roles = updated["subagents"][key.collection]
        .as_object_mut()
        .ok_or("角色列表格式错误。")?;
    roles.remove(&key.name).ok_or("角色已变化，请重新加载。")?;
    Ok(updated)
}

fn create_role(document: &Value) -> Result<(Value, RoleKey), String> {
    let count: usize = ["agents", "custom_agents"]
        .iter()
        .map(|collection| {
            document["subagents"][*collection]
                .as_object()
                .map_or(0, |roles| roles.len())
        })
        .sum();
    if count >= 64 {
        return Err("最多支持 64 个角色配置（含内置覆盖）。".into());
    }
    let mut updated = document.clone();
    let roles = updated["subagents"]["custom_agents"]
        .as_object_mut()
        .ok_or("自定义角色列表格式错误。")?;
    let mut index = roles.len() + 1;
    while roles.contains_key(&format!("role-{index}")) {
        index += 1;
    }
    let name = format!("role-{index}");
    roles.insert(name.clone(), json!({"description":"", "system_prompt":""}));
    Ok((
        updated,
        RoleKey {
            collection: "custom_agents",
            name,
        },
    ))
}

pub(super) fn import_role(document: &Value, name: &str, role: &Value) -> Result<Value, String> {
    if !valid_name(name) || BUILTINS.contains(&name) {
        return Err("旧角色名称无效或与内置角色重名。".into());
    }
    let fields = role.as_object().ok_or("旧角色预览格式无效。")?;
    if fields.keys().any(|key| !KEYS.contains(&key.as_str())) {
        return Err("旧角色预览包含当前未支持的设置。".into());
    }
    let count: usize = ["agents", "custom_agents"]
        .iter()
        .map(|key| {
            document["subagents"][*key]
                .as_object()
                .map_or(0, |roles| roles.len())
        })
        .sum();
    if count >= 64 {
        return Err("最多支持 64 个角色配置（含内置覆盖）。".into());
    }
    if ["agents", "custom_agents"]
        .iter()
        .any(|key| document["subagents"][*key].get(name).is_some())
    {
        return Err("当前草稿已有同名角色，导入不会覆盖；请先调整角色名称。".into());
    }
    let mut updated = document.clone();
    updated["subagents"]["custom_agents"]
        .as_object_mut()
        .ok_or("自定义角色列表格式错误。")?
        .insert(name.to_owned(), role.clone());
    Ok(updated)
}

impl Waku {
    pub(super) fn deerflow_role_mutation(&mut self, remove: bool, cx: &mut Context<Self>) {
        let state = &mut self.deerflow_settings;
        let key = state.roles.selected.clone();
        let path = if remove {
            format!("/subagents/{}", key.collection)
        } else {
            NAME_PATH.into()
        };
        if !state.can_edit_field(&path)
            || !state.errors.is_empty()
            || state.roles.pending_name.is_some()
        {
            return;
        }
        let Some(document) = state.draft.as_ref() else {
            return;
        };
        let result = if remove {
            remove_role(document, &key).map(|doc| (doc, key.clone()))
        } else {
            create_role(document)
        };
        match result {
            Ok((doc, selected)) => {
                state.draft = Some(doc);
                state.roles.selected = selected;
            }
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

    pub(super) fn render_deerflow_roles(&self, theme: Theme, cx: &mut Context<Self>) -> AnyElement {
        let state = &self.deerflow_settings;
        let mut picker = div().flex().flex_wrap().gap(px(6.0));
        for key in &state.roles.choices {
            let selected = *key == state.roles.selected;
            let label = format!(
                "{} · {}",
                key.name,
                if key.custom() { "自定义" } else { "内置" }
            );
            let key = key.clone();
            picker = picker.child(df_button(
                SharedString::from(format!("df-role-{}-{}", key.collection, key.name)),
                label,
                state.busy() || !state.errors.is_empty(),
                selected,
                theme,
                cx,
                move |this, _, cx| {
                    let same = this.deerflow_settings.roles.selected == key;
                    if !this.deerflow_commit_names(cx) {
                        return;
                    }
                    if !same {
                        this.deerflow_settings.roles.selected = key.clone();
                    }
                    this.deerflow_rebind(cx);
                    cx.notify();
                },
            ));
        }
        let key = &state.roles.selected;
        let pending = state.roles.pending_name.is_some();
        let invalid = !state.errors.is_empty();
        let mut group = df_group(theme).child(picker).child(
            div()
                .flex()
                .flex_wrap()
                .gap(px(6.0))
                .child(df_button(
                    "df-role-add",
                    "添加角色",
                    !state.can_edit_field(NAME_PATH) || invalid || pending,
                    false,
                    theme,
                    cx,
                    |this, _, cx| this.deerflow_role_mutation(false, cx),
                ))
                .child(df_button(
                    "df-role-remove",
                    if key.custom() {
                        "从草稿移除角色"
                    } else {
                        "恢复内置配置"
                    },
                    !state.can_edit_field(&format!("/subagents/{}", key.collection))
                        || invalid
                        || pending
                        || state
                            .draft
                            .as_ref()
                            .is_none_or(|doc| doc.pointer(&key.root()).is_none()),
                    false,
                    theme,
                    cx,
                    |this, _, cx| this.deerflow_role_mutation(true, cx),
                ))
                .child(df_button(
                    "df-role-rename",
                    "确认名称",
                    !state.can_edit_field(NAME_PATH) || invalid || !pending,
                    false,
                    theme,
                    cx,
                    |this, _, cx| {
                        this.deerflow_commit_names(cx);
                    },
                )),
        );
        group = group.child(df_label("角色配置", "选择角色只查看草稿；修改内置角色会创建覆盖配置。保存并应用后对新任务生效，已有任务保持原角色快照。", theme));
        group = group.child(df_label(
            "当前服务实际配置",
            applied_settings_summary(&state.status)
                .unwrap_or_else(|| "服务未运行或尚未提供实际值；下方显示已加载的配置草稿。".into()),
            theme,
        ));
        for diagnostic in &state.roles.diagnostics {
            group = group.child(df_label("迁移提示", diagnostic.clone(), theme));
        }
        group.into_any_element()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn deerflow_applied_subagent_summary_requires_live_versioned_state() {
        let mut status = json!({"running":true,"subagent_settings":{"version":"deerflow-subagent-settings-v1","enabled":false,"max_concurrent":3,"enabled_source":"cli","concurrency_source":"yaml","tool_policy_allows_task":false}});
        let summary = applied_settings_summary(&status).unwrap();
        assert!(
            summary.contains("显式命令行参数")
                && summary.contains("工具策略禁止 task")
                && summary.contains("根任务并发：3")
        );
        status["running"] = json!(false);
        assert!(applied_settings_summary(&status).is_none());
        status["running"] = json!(true);
        status["subagent_settings"]["version"] = json!("future");
        assert!(applied_settings_summary(&status).is_none());
    }

    fn fixture() -> Value {
        json!({"subagents":{"agents":{"general-purpose":{"delegate_to":["writer"]}},"custom_agents":{
            "writer":{"description":"Write", "system_prompt":"Use evidence", "tools":null, "skills":[], "thinking_enabled":false, "model_settings":{"temperature":0}},
            "coordinator":{"description":"Coordinate", "system_prompt":"Delegate", "delegate_to":["writer"]}
        }}})
    }
    fn writer() -> RoleKey {
        RoleKey {
            collection: "custom_agents",
            name: "writer".into(),
        }
    }

    #[test]
    fn deerflow_role_selection_and_fields_preserve_sparse_inheritance() {
        let doc = fixture();
        let original = doc.clone();
        let mut editor = RoleEditor::default();
        editor.refresh(&doc);
        editor.selected = RoleKey {
            collection: "agents",
            name: "bash".into(),
        };
        let fields = editor.fields(&doc);
        assert_eq!(fields.len(), 14);
        assert!(!fields.iter().any(|field| matches!(field.kind, Kind::Json)));
        assert!(
            fields.iter().filter(|field| field.kind.multiline()).count()
                < FIELD_COUNT - SINGLE_FIELDS
        );
        assert_eq!(editor.projected(&doc).unwrap(), original);
        let mut edited = doc.clone();
        set_field(
            &mut edited,
            "/subagents/agents/bash/model_settings/temperature",
            json!(0),
        )
        .unwrap();
        assert_eq!(
            edited["subagents"]["agents"]["bash"],
            json!({"model_settings":{"temperature":0}})
        );
        assert!(edited["subagents"]["agents"]["bash"].get("tools").is_none());
        for value in [Value::Null, json!([]), json!(["read_file", "host_opencli"])] {
            assert_eq!(
                parse_field(&field_text(&value, Kind::Names), Kind::Names).unwrap(),
                value
            );
        }
        for value in [Value::Null, json!(true), json!(false)] {
            set_field(
                &mut edited,
                "/subagents/agents/bash/thinking_enabled",
                value.clone(),
            )
            .unwrap();
            assert_eq!(
                edited["subagents"]["agents"]["bash"]["thinking_enabled"],
                value
            );
        }
    }

    #[test]
    fn deerflow_role_rename_updates_all_delegations_atomically() {
        let doc = fixture();
        let original = doc.clone();
        for invalid in ["", "bash", "coordinator", "bad/name", "名字", "_role"] {
            assert!(rename(&doc, &writer(), invalid).is_err());
            assert_eq!(doc, original);
        }
        let renamed = rename(&doc, &writer(), " author ").unwrap();
        assert_eq!(
            renamed["subagents"]["custom_agents"]["author"],
            doc["subagents"]["custom_agents"]["writer"]
        );
        assert!(
            renamed["subagents"]["custom_agents"]
                .get("writer")
                .is_none()
        );
        assert_eq!(
            renamed["subagents"]["agents"]["general-purpose"]["delegate_to"],
            json!(["author"])
        );
        assert_eq!(
            renamed["subagents"]["custom_agents"]["coordinator"]["delegate_to"],
            json!(["author"])
        );
        assert!(rename(&doc, &RoleKey::default(), "new").is_err());
    }

    #[test]
    fn deerflow_role_delete_reset_and_create_preserve_graph() {
        let doc = fixture();
        assert!(remove_role(&doc, &writer()).is_err());
        let reset = remove_role(&doc, &RoleKey::default()).unwrap();
        assert!(reset["subagents"]["agents"].as_object().unwrap().is_empty());
        let mut editor = RoleEditor::default();
        editor.refresh(&reset);
        assert!(editor.choices.contains(&RoleKey::default()));
        let (created, key) = create_role(&doc).unwrap();
        assert_eq!(
            created["subagents"]["custom_agents"][&key.name],
            json!({"description":"", "system_prompt":""})
        );
        assert_eq!(remove_role(&created, &key).unwrap(), doc);
    }

    #[test]
    fn deerflow_role_unknown_history_is_diagnosed_and_never_replaced_by_selection() {
        let mut doc = fixture();
        doc["subagents"]["custom_agents"]["writer"]["legacy_option"] = json!({"redacted":true});
        doc["subagents"]["custom_agents"]["writer"]["model_settings"] = json!("legacy");
        let original = doc.clone();
        let mut editor = RoleEditor::default();
        editor.selected = writer();
        editor.refresh(&doc);
        assert_eq!(editor.diagnostics.len(), 2);
        assert_eq!(editor.projected(&doc).unwrap(), original);
        assert!(
            set_field(
                &mut doc,
                "/subagents/custom_agents/writer/model_settings/temperature",
                json!(1)
            )
            .is_err()
        );
        assert_eq!(doc, original);
        assert_eq!(
            rename(&doc, &writer(), "author").unwrap()["subagents"]["custom_agents"]["author"]["legacy_option"],
            json!({"redacted":true})
        );
    }
}
