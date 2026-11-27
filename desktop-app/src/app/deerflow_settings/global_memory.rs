use super::*;
use serde::Deserialize;

const PAGE_SIZE: usize = 12;
const MAX_CONTENT_BYTES: usize = 2048;
const LIST: &str = "memory.personal.get";
const MUTATE: &str = "memory.personal.mutate";
const RECEIPT: &str = "memory.personal.receipt";

#[derive(Clone, Deserialize)]
struct Fact {
    id: String,
    revision: i64,
    content: String,
    category: String,
    updated_at: DateTime<Utc>,
}

#[derive(Deserialize)]
struct Page {
    profile_id: String,
    facts: Vec<Fact>,
    next_cursor: String,
}

enum Form {
    Add(&'static str),
    Edit(Fact),
    Delete(Fact),
}

#[derive(Default)]
pub(super) struct GlobalMemory {
    profile: String,
    facts: Vec<Fact>,
    navigation: skills::PageNav,
    // A newly committed item can sort outside the current server page.
    added: Option<Fact>,
    form: Option<Form>,
    conflict: bool,
    reviewed: Option<Fact>,
    loading: bool,
    connected: bool,
    unresolved: Option<Value>,
    message: Option<String>,
}

fn first_page() -> Value {
    json!({"operation":LIST, "limit":PAGE_SIZE, "facts_only":true})
}

fn category_label(category: &str) -> &str {
    match category {
        "preference" => "个人偏好",
        "profile" => "个人资料",
        "project" => "项目资料",
        other => other,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fact(id: &str, revision: i64) -> Value {
        json!({"id":id,"revision":revision,"category":"profile","content":"fixture",
            "updated_at":"2026-11-26T00:00:00Z"})
    }

    fn page(id: &str, facts: Value, next: &str) -> Value {
        json!({"profile_id":id,"facts":facts,"next_cursor":next})
    }

    #[test]
    fn global_memory_timeout_reuses_exact_add_request_and_shows_receipted_row() {
        let mut state = GlobalMemory::default();
        state
            .accept_page(&first_page(), page("p", json!([]), ""))
            .unwrap();
        state.form = Some(Form::Add("preference"));
        assert!(state.submit_request(&"字".repeat(683)).is_err());
        let request = state.submit_request(" 使用中文 ").unwrap();
        assert_eq!(request["content"], "使用中文");
        assert_eq!(request["profile_id"], "p");
        assert_eq!(request["expected_revision"], 0);
        state.unresolved = Some(request.clone());
        assert!(
            state
                .finish_request(&request, Err(anyhow::anyhow!("timeout")))
                .is_none()
        );
        assert!(state.submit_request("使用中文").is_err());
        let check = json!({"operation":RECEIPT,"operation_id":request["operation_id"]});
        assert_eq!(
            state.finish_request(&check, Ok(json!({"ok":true,"data":{"found":false}}))),
            Some(request.clone())
        );
        let receipt = json!({"operation_id":request["operation_id"],"key":request["key"],"scope":"global",
            "fact_id":"z","revision":1,"updated_at":"2026-11-26T00:00:00Z","status":"saved"});
        assert_eq!(
            state.finish_request(
                &check,
                Ok(json!({"ok":true,"data":{"found":true,"receipt":receipt}}))
            ),
            Some(first_page())
        );
        state
            .accept_page(&first_page(), page("p", json!([fact("a", 1)]), "a"))
            .unwrap();
        assert_eq!(state.added.as_ref().unwrap().content, "使用中文");
        assert!(state.unresolved.is_none() && state.form.is_none());
    }

    #[test]
    fn global_memory_edit_conflict_keeps_reviewed_fact_and_draft_target() {
        let mut state = GlobalMemory::default();
        state
            .accept_page(&first_page(), page("p", json!([fact("promoted", 2)]), ""))
            .unwrap();
        state.form = Some(Form::Edit(state.facts[0].clone()));
        let request = state.submit_request("新的内容").unwrap();
        assert_eq!(request["action"], "edit");
        assert_eq!(request["fact_id"], "promoted");
        assert_eq!(request["expected_revision"], 2);
        assert!(request.get("key").is_none() && request.get("category").is_none());
        state.unresolved = Some(request.clone());
        let reload = state
            .finish_request(&request, Ok(json!({"ok":false,"code":"memory_conflict"})))
            .unwrap();
        assert_eq!(reload["review_fact_id"], "promoted");
        let mut response = page("p", json!([fact("other", 1)]), "other");
        response["reviewed_fact"] = fact("promoted", 3);
        state.accept_page(&reload, response).unwrap();
        assert!(matches!(&state.form, Some(Form::Edit(f)) if f.revision == 2));
        assert!(state.conflict && state.unresolved.is_none());
        assert!(state.submit_request("新的内容").is_err());
        assert!(
            state
                .navigation
                .request(true)
                .unwrap()
                .get("review_fact_id")
                .is_none()
        );
        assert_eq!(state.reviewed.as_ref().unwrap().id, "promoted");
        state.form = Some(Form::Delete(state.reviewed.clone().unwrap()));
        state.conflict = false;
        let delete = state.submit_request("").unwrap();
        assert_eq!(delete["action"], "forget");
        assert_eq!(delete["expected_revision"], 3);
        assert!(delete.get("content").is_none());
    }

    #[test]
    fn global_memory_reviewed_missing_fact_preserves_draft_and_empty_last_page_goes_back() {
        let mut state = GlobalMemory::default();
        state
            .accept_page(&first_page(), page("p", json!([fact("a", 1)]), "a"))
            .unwrap();
        let second = state.navigation.request(true).unwrap();
        state
            .accept_page(&second, page("p", json!([fact("z", 1)]), ""))
            .unwrap();
        state.form = Some(Form::Edit(state.facts[0].clone()));
        let review = state.current_page();
        let mut empty = page("p", json!([]), "");
        empty["reviewed_fact"] = Value::Null;
        let previous = state
            .finish_request(&review, Ok(json!({"ok":true, "data":empty})))
            .unwrap();
        assert_eq!(previous.get("cursor"), None);
        assert_eq!(previous["review_fact_id"], "z");
        assert!(state.conflict && state.reviewed.is_none());
        assert!(state.message.as_deref().unwrap().contains("已删除"));
        assert!(matches!(&state.form, Some(Form::Edit(f)) if f.id == "z"));
        let mut first = page("p", json!([fact("a", 1)]), "");
        first["reviewed_fact"] = Value::Null;
        assert_eq!(
            state.finish_request(&previous, Ok(json!({"ok":true, "data":first}))),
            None
        );
        assert_eq!(state.navigation.page_number(), 1);
        assert_eq!(state.facts[0].id, "a");
    }

    #[test]
    fn global_memory_pagination_discards_previous_profile_data() {
        let mut state = GlobalMemory::default();
        state
            .accept_page(&first_page(), page("p", json!([fact("a", 1)]), "a"))
            .unwrap();
        let next = state.navigation.request(true).unwrap();
        assert_eq!(next["facts_only"], true);
        assert_eq!(next["cursor"], "a");
        assert!(
            state
                .accept_page(&next, page("other", json!([]), ""))
                .is_err()
        );
        assert!(state.profile.is_empty() && state.facts.is_empty());
        assert_eq!(state.current_page(), first_page());
    }
}

impl GlobalMemory {
    fn locked(&self) -> bool {
        self.loading || self.unresolved.is_some()
    }

    fn current_page(&self) -> Value {
        self.with_review(self.navigation.current_request().unwrap_or_else(first_page))
    }

    fn with_review(&self, mut page: Value) -> Value {
        if let Some(Form::Edit(fact) | Form::Delete(fact)) = &self.form {
            page["review_fact_id"] = json!(fact.id);
        }
        page
    }

    fn accept_page(&mut self, request: &Value, data: Value) -> anyhow::Result<()> {
        let mut page_request = request.clone();
        let target = page_request
            .as_object_mut()
            .and_then(|map| map.remove("review_fact_id"));
        let reviewed = if let Some(id) = &target {
            let field = data
                .get("reviewed_fact")
                .ok_or_else(|| anyhow::anyhow!("缺少指定记忆"))?;
            let fact: Option<Fact> = serde_json::from_value(field.clone())?;
            anyhow::ensure!(
                fact.as_ref()
                    .is_none_or(|f| Some(f.id.as_str()) == id.as_str() && f.revision > 0)
            );
            fact
        } else {
            None
        };
        let page: Page = serde_json::from_value(data)?;
        anyhow::ensure!(!page.profile_id.is_empty() && page.facts.len() <= PAGE_SIZE);
        anyhow::ensure!(
            page.facts
                .iter()
                .all(|f| !f.id.is_empty() && f.revision > 0)
        );
        if !self.profile.is_empty() && page.profile_id != self.profile {
            self.form = None;
            self.conflict = false;
            self.reviewed = None;
            self.added = None;
            self.unresolved = None;
            self.navigation = Default::default();
            self.facts.clear();
            self.profile.clear();
            anyhow::ensure!(request.get("cursor").is_none(), "数据目录已变化，请刷新。");
        }
        self.navigation
            .accept(&page_request, &page.next_cursor, "cursor")?;
        if let Some(Form::Edit(original) | Form::Delete(original)) = &self.form
            && target.as_ref().and_then(Value::as_str) == Some(&original.id)
        {
            if reviewed.is_none() {
                self.conflict = true;
                self.message = Some(
                    if matches!(self.form, Some(Form::Delete(_))) {
                        "这条记忆已删除。请取消确认以返回列表。"
                    } else {
                        "这条记忆已删除。草稿已保留，可复制正文后取消编辑。"
                    }
                    .into(),
                );
            }
            self.reviewed = reviewed;
        }
        if let Some(added) = &self.added {
            if let Some(current) = page.facts.iter().find(|f| f.id == added.id) {
                self.added = Some(current.clone());
            } else if added.id.as_str() > request["cursor"].as_str().unwrap_or("")
                && (page.next_cursor.is_empty() || added.id <= page.next_cursor)
            {
                self.added = None;
            }
        }
        self.profile = page.profile_id;
        self.facts = page.facts;
        self.connected = true;
        Ok(())
    }

    fn submit_request(&self, content: &str) -> Result<Value, &'static str> {
        if self.locked() || self.conflict || !self.connected {
            return Err("请先刷新并核对当前记忆。");
        }
        let form = self.form.as_ref().ok_or("请选择一条记忆。")?;
        let content = content.trim();
        if !matches!(form, Form::Delete(_)) {
            if content.is_empty() {
                return Err("请输入记忆内容。");
            }
            if content.len() > MAX_CONTENT_BYTES {
                return Err("记忆内容超过 2048 个 UTF-8 字节，请缩短后保存。");
            }
        }
        let operation_id = Uuid::new_v4().to_string();
        let mut request = match form {
            Form::Add(category) => json!({
                "action":"remember", "expected_revision":0, "category":category,
                "content":content, "key":format!("manual.{operation_id}")
            }),
            Form::Edit(fact) => json!({
                "action":"edit", "expected_revision":fact.revision, "fact_id":fact.id, "content":content
            }),
            Form::Delete(fact) => json!({
                "action":"forget", "expected_revision":fact.revision, "fact_id":fact.id
            }),
        };
        request["operation"] = json!(MUTATE);
        request["profile_id"] = json!(self.profile);
        request["operation_id"] = json!(operation_id);
        Ok(request)
    }

    fn accept_receipt(&mut self, receipt: &Value) -> anyhow::Result<()> {
        let request = self
            .unresolved
            .as_ref()
            .ok_or_else(|| anyhow::anyhow!("缺少待核对的修改。"))?;
        let status = receipt["status"].as_str().unwrap_or("");
        let action = request["action"].as_str().unwrap_or("");
        let id = receipt["fact_id"].as_str().unwrap_or("");
        anyhow::ensure!(
            receipt["operation_id"] == request["operation_id"] && receipt["scope"] == "global"
        );
        if action == "remember" {
            anyhow::ensure!(
                receipt["key"] == request["key"] && status == "saved" && !id.is_empty()
            );
        } else {
            anyhow::ensure!(id == request["fact_id"] || (id.is_empty() && status == "not_found"));
        }
        let revision = receipt["revision"].as_i64().unwrap_or_default();
        let message = match (action, status) {
            ("remember", "saved") => {
                anyhow::ensure!(revision > 0);
                self.added = Some(Fact {
                    id: id.to_owned(),
                    content: request["content"].as_str().unwrap_or_default().to_owned(),
                    category: request["category"].as_str().unwrap_or_default().to_owned(),
                    updated_at: serde_json::from_value(receipt["updated_at"].clone())?,
                    revision,
                });
                "已添加全局记忆。"
            }
            ("edit", "updated" | "unchanged") => {
                if let Some(fact) = &mut self.added
                    && fact.id == id
                {
                    fact.content = request["content"].as_str().unwrap_or_default().to_owned();
                    fact.revision = revision;
                    fact.updated_at = serde_json::from_value(receipt["updated_at"].clone())?;
                }
                "已保存修改。"
            }
            ("forget", "forgotten" | "already_forgotten" | "not_found") => {
                if self.added.as_ref().is_some_and(|f| f.id == id) {
                    self.added = None;
                }
                self.facts
                    .retain(|f| Some(f.id.as_str()) != request["fact_id"].as_str());
                "这条全局记忆已移除。"
            }
            ("edit", "not_found") => {
                self.conflict = true;
                self.added = None;
                self.unresolved = None;
                self.message = Some("这条记忆已被删除。当前草稿已保留，可复制正文。".into());
                return Ok(());
            }
            _ => anyhow::bail!("服务返回了未知的修改结果。"),
        };
        self.form = None;
        self.conflict = false;
        self.reviewed = None;
        self.unresolved = None;
        self.message = Some(message.into());
        Ok(())
    }
}

impl Waku {
    pub(in crate::app) fn open_global_memory(&mut self, cx: &mut Context<Self>) {
        let state = &mut self.deerflow_settings.global_memory;
        if state.loading {
            return;
        }
        if let Some(request) = &state.unresolved {
            let check = json!({"operation":RECEIPT, "operation_id":request["operation_id"]});
            self.global_memory_dispatch(check, cx);
        } else {
            state.added = None;
            if !state.conflict {
                state.message = None;
            }
            let page = state.current_page();
            self.global_memory_dispatch(page, cx);
        }
    }

    fn global_memory_dispatch(&mut self, request: Value, cx: &mut Context<Self>) {
        let state = &mut self.deerflow_settings.global_memory;
        if state.loading {
            return;
        }
        if request["operation"] == MUTATE {
            if state
                .unresolved
                .as_ref()
                .is_some_and(|prior| prior != &request)
            {
                return;
            }
            state.unresolved = Some(request.clone());
        }
        state.loading = true;
        self.deerflow_settings
            .global_memory_input
            .update(cx, |input, _| input.set_read_only(true));
        let client = self.daemon.client();
        let sent = request.clone();
        cx.spawn(async move |this, cx| {
            let result: anyhow::Result<Value> = cx
                .background_executor()
                .spawn(async move {
                    match client.request(
                        Uuid::nil(),
                        Uuid::nil(),
                        waku_client::Command::DeerFlow {
                            operation: "manage-envelope".into(),
                            input: request,
                        },
                    )? {
                        waku_client::ResponsePayload::DeerFlow { data } => Ok(data),
                        _ => anyhow::bail!("服务响应无效"),
                    }
                })
                .await;
            let _ = this.update(cx, |this, cx| {
                let state = &mut this.deerflow_settings.global_memory;
                state.loading = false;
                let next = state.finish_request(&sent, result);
                let frozen = state.unresolved.is_some();
                this.deerflow_settings
                    .global_memory_input
                    .update(cx, |input, cx| {
                        input.set_read_only(frozen);
                        cx.notify();
                    });
                if let Some(next) = next {
                    this.global_memory_dispatch(next, cx);
                }
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }

    fn global_memory_form(&mut self, form: Form, window: &mut Window, cx: &mut Context<Self>) {
        let state = &mut self.deerflow_settings.global_memory;
        if state.locked() || !state.connected || state.form.is_some() {
            return;
        }
        if !matches!(&form, Form::Delete(_)) {
            let content = match &form {
                Form::Edit(fact) => fact.content.clone(),
                _ => String::new(),
            };
            self.deerflow_settings
                .global_memory_input
                .update(cx, |input, cx| {
                    input.set_content(content, cx);
                    input.focus_handle(cx).focus(window, cx);
                });
        }
        state.form = Some(form);
        state.conflict = false;
        state.reviewed = None;
        state.message = None;
        self.settings_scroll.set_offset(gpui::Point::default());
        cx.notify();
    }

    fn global_memory_submit(&mut self, cx: &mut Context<Self>) {
        let state = &mut self.deerflow_settings.global_memory;
        match state.submit_request(
            self.deerflow_settings
                .global_memory_input
                .read(cx)
                .content(),
        ) {
            Ok(request) => self.global_memory_dispatch(request, cx),
            Err(message) => {
                state.message = Some(message.into());
                cx.notify();
            }
        }
    }
}

impl GlobalMemory {
    fn finish_request(&mut self, sent: &Value, result: anyhow::Result<Value>) -> Option<Value> {
        let op = sent["operation"].as_str()?;
        let envelope = match result {
            Ok(envelope) => envelope,
            Err(_) => {
                if op == LIST {
                    self.connected = false;
                }
                self.message = Some(
                    if self.unresolved.is_some() {
                        "服务未确认操作结果。重新连接后可核对本次操作。"
                    } else {
                        "无法读取全局记忆，请确认后台服务已启动，然后刷新。"
                    }
                    .into(),
                );
                return None;
            }
        };
        if envelope["ok"] != true {
            if op == LIST {
                self.connected = false;
            }
            let code = envelope["code"].as_str().unwrap_or("");
            self.message = Some(
                match (op, code) {
                    (MUTATE, "memory_conflict") => {
                        self.unresolved = None;
                        self.conflict = true;
                        self.reviewed = None;
                        self.added = None;
                        "记忆已发生变化，草稿已保留。请核对最新正文。"
                    }
                    (MUTATE, "invalid_request") => {
                        self.unresolved = None;
                        "服务拒绝了修改。请刷新确认当前用户，检查正文长度及是否含有密钥。"
                    }
                    _ if self.unresolved.is_some() => "未能确认操作结果，请核对本次操作。",
                    _ => "无法读取记忆，请确认后台服务已启动并刷新。",
                }
                .into(),
            );
            if op == MUTATE && code == "memory_conflict" {
                return Some(self.current_page());
            }
            return None;
        }
        let data = &envelope["data"];
        if op == LIST {
            if self.accept_page(sent, data.clone()).is_err() {
                self.connected = false;
                self.message = Some("记忆列表发生变化或响应无效，请刷新。".into());
            } else if self.facts.is_empty()
                && let Some(previous) = self.navigation.request(false)
            {
                return Some(self.with_review(previous));
            }
            return None;
        }
        if op == RECEIPT && data["found"] == false {
            // Retry the exact same operation, so a delayed first response
            // cannot turn a double-click/timeout into a second revision.
            return self.unresolved.clone();
        }
        let receipt = if op == RECEIPT {
            &data["receipt"]
        } else {
            data
        };
        if self.accept_receipt(receipt).is_err() {
            self.message = Some("服务未返回可核对的回执，请重新核对本次操作。".into());
            return None;
        }
        Some(self.current_page())
    }
}

impl Waku {
    pub(in crate::app) fn render_global_memory(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings.global_memory;
        let locked = state.locked();
        let editing = state.form.is_some();
        let mut header = df_group(theme).child(df_label(
            "全局记忆",
            "当前用户保存的记忆可在不同项目中使用。",
            theme,
        ));
        let actions = div()
            .flex()
            .flex_wrap()
            .gap(px(8.0))
            .child(df_button(
                "global-memory-add",
                "添加记忆",
                locked || editing || !state.connected,
                true,
                theme,
                cx,
                |this, window, cx| this.global_memory_form(Form::Add("preference"), window, cx),
            ))
            .child(df_button(
                "global-memory-refresh",
                "刷新",
                locked,
                false,
                theme,
                cx,
                |this, _, cx| {
                    let state = &mut this.deerflow_settings.global_memory;
                    state.message = None;
                    state.added = None;
                    let request = state.current_page();
                    this.global_memory_dispatch(request, cx);
                },
            ));
        header = header.child(actions);
        if let Some(message) = &state.message {
            header = header.child(
                div()
                    .text_size(sp(13.0))
                    .text_color(theme.text)
                    .child(message.clone()),
            );
        }
        if state.loading {
            header = header.child(
                div()
                    .text_size(sp(12.5))
                    .text_color(theme.text_secondary)
                    .child("正在处理…"),
            );
        } else if let Some(request) = &state.unresolved {
            let check = json!({"operation":RECEIPT, "operation_id":request["operation_id"]});
            header = header.child(df_button(
                "global-memory-check",
                "核对操作结果",
                false,
                true,
                theme,
                cx,
                move |this, _, cx| this.global_memory_dispatch(check.clone(), cx),
            ));
        }
        let mut body = div().child(header);
        if let Some(form) = &state.form {
            let deleting = matches!(form, Form::Delete(_));
            let mut form_view = df_group(theme);
            match form {
                Form::Add(category) => {
                    let mut categories = div().flex().gap(px(6.0));
                    for key in ["preference", "profile"] {
                        categories = categories.child(df_button(
                            SharedString::from(format!("global-memory-category-{key}")),
                            category_label(key),
                            locked,
                            key == *category,
                            theme,
                            cx,
                            move |this, _, cx| {
                                if let Some(Form::Add(value)) =
                                    &mut this.deerflow_settings.global_memory.form
                                {
                                    *value = key;
                                    cx.notify();
                                }
                            },
                        ));
                    }
                    form_view = form_view
                        .child(df_label("添加记忆", "分类", theme))
                        .child(categories);
                }
                Form::Edit(fact) => {
                    form_view = form_view.child(df_label(
                        "编辑记忆",
                        category_label(&fact.category).to_owned(),
                        theme,
                    ))
                }
                Form::Delete(fact) => {
                    form_view =
                        form_view.child(df_label("删除这条全局记忆？", fact.content.clone(), theme))
                }
            }
            if !deleting {
                form_view = form_view.child(
                    div()
                        .id("global-memory-content")
                        .h(px(120.0))
                        .w_full()
                        .flex_none()
                        .overflow_hidden()
                        .overflow_y_scroll()
                        .p(px(10.0))
                        .rounded(px(6.0))
                        .border_1()
                        .border_color(theme.border_strong)
                        .bg(theme.inset)
                        .text_size(sp(13.5))
                        .line_height(sp(20.0))
                        .child(self.deerflow_settings.global_memory_input.clone()),
                );
            }
            let target = match form {
                Form::Edit(fact) | Form::Delete(fact) => Some(fact),
                Form::Add(_) => None,
            };
            // Keep the exact revision the user reviewed until they explicitly
            // accept the concurrently changed value. Their text stays in the input.
            let latest = target.and_then(|target| {
                if state.conflict {
                    state.reviewed.as_ref().filter(|f| f.id == target.id)
                } else {
                    state
                        .reviewed
                        .as_ref()
                        .or_else(|| state.facts.iter().find(|f| f.id == target.id))
                        .filter(|f| f.id == target.id && f.revision > target.revision)
                }
            });
            if let Some(latest) = latest {
                let latest = latest.clone();
                form_view = form_view
                    .child(df_label("最新保存的正文", latest.content.clone(), theme))
                    .child(df_button(
                        "global-memory-review",
                        "已核对，使用此版本继续",
                        locked || !state.connected,
                        false,
                        theme,
                        cx,
                        move |this, _, cx| {
                            let state = &mut this.deerflow_settings.global_memory;
                            if let Some(Form::Edit(fact) | Form::Delete(fact)) = &mut state.form
                                && fact.id == latest.id
                            {
                                *fact = latest.clone();
                                state.conflict = false;
                                state.reviewed = None;
                                state.message = None;
                                cx.notify();
                            }
                        },
                    ));
            }
            form_view = form_view.child(
                div()
                    .flex()
                    .gap(px(8.0))
                    .child(df_button(
                        "global-memory-save",
                        if deleting { "确认删除" } else { "保存" },
                        locked || !state.connected || state.conflict || latest.is_some(),
                        true,
                        theme,
                        cx,
                        |this, _, cx| this.global_memory_submit(cx),
                    ))
                    .child(df_button(
                        "global-memory-cancel",
                        "取消",
                        locked,
                        false,
                        theme,
                        cx,
                        |this, _, cx| {
                            let state = &mut this.deerflow_settings.global_memory;
                            state.form = None;
                            state.conflict = false;
                            state.reviewed = None;
                            state.message = None;
                            cx.notify();
                        },
                    )),
            );
            body = body.child(form_view);
        }
        if state.connected
            && !state.loading
            && state.navigation.page_number() == 1
            && state.facts.is_empty()
            && state.added.is_none()
        {
            body = body.child(df_group(theme).child(df_label(
                "暂无全局记忆",
                "点击「添加记忆」保存一条个人偏好或资料。",
                theme,
            )));
        }
        for fact in state.added.iter().chain(
            state
                .facts
                .iter()
                .filter(|f| Some(f.id.as_str()) != state.added.as_ref().map(|f| f.id.as_str())),
        ) {
            let edit = fact.clone();
            let delete = fact.clone();
            let meta = format!(
                "{} · 更新于 {}",
                category_label(&fact.category),
                fact.updated_at
                    .with_timezone(&Local)
                    .format("%Y-%m-%d %H:%M")
            );
            let disabled = locked || editing || !state.connected;
            body = body.child(
                df_group(theme)
                    .child(df_label(fact.content.clone(), meta, theme))
                    .child(
                        div()
                            .flex()
                            .gap(px(8.0))
                            .child(df_button(
                                SharedString::from(format!("global-memory-edit-{}", fact.id)),
                                "编辑",
                                disabled,
                                false,
                                theme,
                                cx,
                                move |this, window, cx| {
                                    this.global_memory_form(Form::Edit(edit.clone()), window, cx)
                                },
                            ))
                            .child(df_button(
                                SharedString::from(format!("global-memory-delete-{}", fact.id)),
                                "删除…",
                                disabled,
                                false,
                                theme,
                                cx,
                                move |this, window, cx| {
                                    this.global_memory_form(
                                        Form::Delete(delete.clone()),
                                        window,
                                        cx,
                                    )
                                },
                            )),
                    ),
            );
        }
        let prev = state
            .navigation
            .request(false)
            .map(|page| state.with_review(page));
        let next = state
            .navigation
            .request(true)
            .map(|page| state.with_review(page));
        if prev.is_some() || next.is_some() {
            let mut pages = div()
                .mt(px(14.0))
                .flex()
                .items_center()
                .gap(px(10.0))
                .child(format!("第 {} 页", state.navigation.page_number()));
            for (id, label, request) in [
                ("global-memory-prev", "上一页", prev),
                ("global-memory-next", "下一页", next),
            ] {
                pages = pages.child(df_button(
                    id,
                    label,
                    locked || request.is_none(),
                    false,
                    theme,
                    cx,
                    move |this, _, cx| {
                        if let Some(request) = &request {
                            this.deerflow_settings.global_memory.added = None;
                            this.global_memory_dispatch(request.clone(), cx);
                        }
                    },
                ));
            }
            body = body.child(pages);
        }
        body.into_any_element()
    }
}
