//! Explicit conversion of old data through the Go config service. Previews
//! bind the current draft revision; archived history is read-only and paged.
use super::skills::digest;
use super::*;
use serde::Deserialize;

struct Preview {
    name: String,
    role: Value,
    summary: String,
    edits: u64,
    config_revision: Value,
}

#[derive(Default)]
pub(super) struct LegacyBrowser {
    preview: Option<Preview>,
    selecting: bool,
    archives: Vec<String>,
    loaded: bool,
    archive_page: usize,
    selected: Option<String>,
    entries: Vec<(String, String)>,
    after: u64,
    next: Option<u64>,
    warnings: String,
}

impl LegacyBrowser {
    pub(super) fn clear_preview(&mut self) {
        self.preview = None;
    }
}

fn warnings(values: &[String]) -> anyhow::Result<String> {
    anyhow::ensure!(
        values.len() <= 32 && values.iter().all(|v| v.len() <= 2048 && !v.contains('\0')),
        "旧数据说明超出限制"
    );
    Ok(values.join("\n"))
}

fn prompt_preview(value: &str) -> String {
    if value.len() <= 2048 {
        return value.to_owned();
    }
    let mut end = 2048;
    while !value.is_char_boundary(end) {
        end -= 1;
    }
    format!(
        "{}\n…预览已截断，加入草稿后可在角色表单查看完整内容。",
        &value[..end]
    )
}

impl Waku {
    pub(super) fn deerflow_select_legacy(&mut self, agent: bool, cx: &mut Context<Self>) {
        if self.deerflow_settings.busy()
            || self.deerflow_settings.applying()
            || self.deerflow_settings.legacy.selecting
            || self.daemon.is_remote()
        {
            return;
        }
        self.deerflow_settings.legacy.selecting = true;
        let edits = self.deerflow_settings.edit_generation;
        let request_generation = self.deerflow_settings.request_generation;
        let receiver = cx.prompt_for_paths(PathPromptOptions {
            files: false,
            directories: true,
            multiple: false,
            prompt: Some(
                if agent {
                    "选择包含 config.yaml 和 SOUL.md 的旧 Agent 目录"
                } else {
                    "选择包含 state.json 的旧技能进化目录"
                }
                .into(),
            ),
        });
        cx.spawn(async move |this, cx| {
            let result = receiver.await;
            let _ = this.update(cx, |this, cx| {
                this.deerflow_settings.legacy.selecting = false;
                match result {
                    Ok(Ok(Some(paths))) if paths.len() == 1 => {
                        if this.deerflow_settings.edit_generation != edits
                            || this.deerflow_settings.request_generation != request_generation
                        {
                            this.deerflow_settings.error =
                                Some("选择目录期间配置已变化，请重新选择；当前草稿已保留。".into());
                        } else if let Some(path) =
                            paths.into_iter().next().filter(|path| path.is_absolute())
                        {
                            this.deerflow_request(
                                if agent {
                                    "legacy-agent-preview"
                                } else {
                                    "legacy-history-import"
                                },
                                json!({"directory":path}),
                                cx,
                            );
                        }
                    }
                    Ok(Err(error)) => {
                        this.deerflow_service_error(&format!("无法选择目录：{error}"), cx)
                    }
                    _ => {}
                }
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }

    pub(super) fn deerflow_legacy_response(
        &mut self,
        operation: &str,
        request: &Value,
        data: Value,
        edits: u64,
        cx: &mut Context<Self>,
    ) {
        let result = self.accept_legacy_response(operation, request, data, edits);
        if let Err(error) = result {
            self.deerflow_service_error(&format!("旧数据处理失败：{error}"), cx);
        }
    }

    fn accept_legacy_response(
        &mut self,
        operation: &str,
        request: &Value,
        data: Value,
        edits: u64,
    ) -> anyhow::Result<()> {
        let state = &mut self.deerflow_settings;
        match operation {
            "legacy-agent-preview" => {
                #[derive(Deserialize)]
                struct Result {
                    name: String,
                    role: Value,
                    warnings: Vec<String>,
                    source_sha256: String,
                    draft_only: bool,
                }
                let result: Result = serde_json::from_value(data)?;
                anyhow::ensure!(
                    result.draft_only && digest(&result.source_sha256),
                    "旧角色响应缺少草稿标志或来源摘要"
                );
                anyhow::ensure!(
                    state.edit_generation == edits,
                    "当前草稿已变化，请重新预览；已有修改已保留"
                );
                let document = state
                    .draft
                    .as_ref()
                    .ok_or_else(|| anyhow::anyhow!("请先加载配置"))?;
                roles::import_role(document, &result.name, &result.role)
                    .map_err(anyhow::Error::msg)?;
                let summary = format!(
                    "{}\n源摘要 {}\n说明 {}\n模型 {} · 工具 {}\n技能 {}\n委派说明\n{}",
                    warnings(&result.warnings)?,
                    result.source_sha256,
                    result.role["description"].as_str().unwrap_or(""),
                    result.role["model"].as_str().unwrap_or("继承"),
                    result.role["tools"],
                    result.role["skills"],
                    prompt_preview(result.role["system_prompt"].as_str().unwrap_or(""))
                );
                anyhow::ensure!(summary.len() <= 128 * 1024, "旧角色预览超过显示限制");
                state.legacy.preview = Some(Preview {
                    name: result.name,
                    role: result.role,
                    summary,
                    edits,
                    config_revision: document["config_revision"].clone(),
                });
                state.notice = Some(
                    "已读取旧 Agent 并生成预览；点击“加入角色草稿”后可在原生表单继续编辑。".into(),
                );
            }
            "legacy-history-import" => {
                anyhow::ensure!(
                    data["read_only"] == true
                        && data["id"].as_str().is_some_and(digest)
                        && data["entries"].as_u64().is_some_and(|n| n <= 2000),
                    "旧历史归档响应无效"
                );
                let notes: Vec<String> = serde_json::from_value(data["warnings"].clone())?;
                state.legacy.warnings = warnings(&notes)?;
                state.legacy.loaded = false;
                state.legacy.entries.clear();
                state.legacy.selected = None;
                state.notice = Some(format!(
                    "已导入 {} 条只读历史记录。刷新归档目录即可查阅。",
                    data["entries"]
                ));
            }
            "legacy-history-list" => {
                #[derive(Deserialize)]
                struct Archive {
                    id: String,
                    read_only: bool,
                }
                #[derive(Deserialize)]
                struct Page {
                    archives: Vec<Archive>,
                }
                let page: Page = serde_json::from_value(data)?;
                anyhow::ensure!(
                    page.archives.len() <= 100
                        && page.archives.iter().all(|a| a.read_only && digest(&a.id)),
                    "只读归档目录无效"
                );
                let mut ids = std::collections::HashSet::new();
                anyhow::ensure!(
                    page.archives.iter().all(|a| ids.insert(a.id.clone())),
                    "归档目录包含重复条目"
                );
                state.legacy.archives = page.archives.into_iter().map(|a| a.id).collect();
                state.legacy.archive_page = 0;
                state.legacy.loaded = true;
                state.legacy.selected = None;
                state.legacy.entries.clear();
            }
            "legacy-history-get" => {
                #[derive(Deserialize)]
                struct Entry {
                    path: String,
                    sha256: String,
                    size: u64,
                    omitted: bool,
                    preview: String,
                    truncated: bool,
                }
                #[derive(Deserialize)]
                struct Page {
                    id: String,
                    imported_at: String,
                    read_only: bool,
                    entries: Vec<Entry>,
                    after: u64,
                    next: u64,
                    has_more: bool,
                    warnings: Vec<String>,
                }
                let page: Page = serde_json::from_value(data)?;
                let after = request["after"].as_u64().unwrap_or(0);
                anyhow::ensure!(
                    page.read_only
                        && request["id"] == page.id
                        && digest(&page.id)
                        && page.after == after
                        && after <= 2000
                        && page.entries.len() <= 20
                        && page.next == after + page.entries.len() as u64
                        && page.next <= 2000
                        && (!page.has_more || page.next > after)
                        && chrono::DateTime::parse_from_rfc3339(&page.imported_at).is_ok(),
                    "历史页面身份或游标无效"
                );
                let mut rows = Vec::with_capacity(page.entries.len());
                for entry in page.entries {
                    anyhow::ensure!(
                        !entry.path.is_empty()
                            && entry.path.len() <= 32768
                            && !entry.path.contains('\0')
                            && digest(&entry.sha256)
                            && entry.size <= 48 * 1024 * 1024
                            && entry.preview.len() <= 2048,
                        "历史条目无效"
                    );
                    rows.push((
                        format!("{} · {} 字节", entry.path, entry.size),
                        format!(
                            "SHA256 {}{}{}\n{}",
                            entry.sha256,
                            if entry.omitted {
                                " · 内容未导入，仅保存摘要"
                            } else {
                                ""
                            },
                            if entry.truncated {
                                " · 以下为截断预览"
                            } else {
                                ""
                            },
                            entry.preview
                        ),
                    ));
                }
                state.legacy.warnings = format!(
                    "导入时间 {}\n{}",
                    page.imported_at,
                    warnings(&page.warnings)?
                );
                state.legacy.selected = Some(page.id);
                state.legacy.entries = rows;
                state.legacy.after = page.after;
                state.legacy.next = page.has_more.then_some(page.next);
            }
            _ => anyhow::bail!("未知旧数据操作"),
        }
        Ok(())
    }

    fn deerflow_add_legacy_role(&mut self, cx: &mut Context<Self>) {
        let state = &mut self.deerflow_settings;
        if !state.can_edit_field(roles::NAME_PATH)
            || !state.errors.is_empty()
            || state.roles.pending_name.is_some()
        {
            return;
        }
        let Some(preview) = &state.legacy.preview else {
            return;
        };
        let Some(document) = &state.draft else {
            return;
        };
        if state.edit_generation != preview.edits
            || document["config_revision"] != preview.config_revision
        {
            state.error = Some("草稿或保存版本已变化，请重新预览旧 Agent。".into());
            cx.notify();
            return;
        }
        match roles::import_role(document, &preview.name, &preview.role) {
            Ok(document) => {
                state.draft = Some(document);
                state.roles.selected = roles::RoleKey {
                    collection: "custom_agents",
                    name: preview.name.clone(),
                };
                state.legacy.preview = None;
                state.dirty = true;
                state.edit_generation += 1;
                state.notice = Some(
                    "旧 Agent 已加入子代理草稿；检查模型、工具、技能及委派说明，保存并应用后生效。"
                        .into(),
                );
                state.error = None;
                state.confirm_reload = false;
                self.deerflow_rebind(cx);
            }
            Err(error) => state.error = Some(error),
        }
        cx.notify();
    }

    pub(super) fn render_deerflow_legacy_agent(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let disabled = self.daemon.is_remote()
            || state.legacy.selecting
            || !state.can_edit_field(roles::NAME_PATH)
            || !state.errors.is_empty()
            || state.roles.pending_name.is_some();
        let mut group = df_group(theme).child(df_label("导入旧 Agent 为子代理", "选择旧 Agent 目录后，Go 服务将 config.yaml 和 SOUL.md 转换为可编辑草稿。原工具权限会按当前已保存的 Go 配置映射；同名角色不会被覆盖。", theme))
            .child(df_button("df-legacy-agent-pick", "选择旧 Agent 目录并预览…", disabled, false, theme, cx, |this, _, cx| this.deerflow_select_legacy(true, cx)));
        if let Some(preview) = &state.legacy.preview {
            group = group
                .child(df_label(
                    format!("角色预览：{}", preview.name),
                    preview.summary.clone(),
                    theme,
                ))
                .child(df_button(
                    "df-legacy-agent-add",
                    "加入角色草稿",
                    disabled
                        || state.edit_generation != preview.edits
                        || state
                            .draft
                            .as_ref()
                            .is_none_or(|doc| doc["config_revision"] != preview.config_revision),
                    false,
                    theme,
                    cx,
                    |this, _, cx| this.deerflow_add_legacy_role(cx),
                ));
        }
        group.into_any_element()
    }

    pub(super) fn render_deerflow_legacy_history(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let browser = &state.legacy;
        let disabled =
            self.daemon.is_remote() || state.busy() || state.applying() || browser.selecting;
        let mut group = df_group(theme).child(df_label("旧技能进化历史归档", "显式导入旧提案、发布和观察记录供只读查阅，不恢复旧任务。每页最多 20 条，内容仅显示 2048 字节预览。", theme))
            .child(div().flex().flex_wrap().gap(px(6.0))
                .child(df_button("df-legacy-history-import", "选择旧历史目录并归档…", disabled, false, theme, cx, |this, _, cx| this.deerflow_select_legacy(false, cx)))
                .child(df_button("df-legacy-history-list", "刷新只读归档目录", disabled, false, theme, cx, |this, _, cx| this.deerflow_request("legacy-history-list", json!({}), cx))));
        if browser.loaded && browser.archives.is_empty() {
            group = group.child(df_label(
                "暂无旧历史归档",
                "导入操作仅向本地只读归档写入副本，旧目录保持原样。",
                theme,
            ));
        }
        for id in browser
            .archives
            .iter()
            .skip(browser.archive_page * 20)
            .take(20)
        {
            let id = id.clone();
            group = group.child(df_button(
                SharedString::from(format!("df-legacy-archive-{id}")),
                format!("归档 {}", &id[..16]),
                disabled,
                browser.selected.as_ref() == Some(&id),
                theme,
                cx,
                move |this, _, cx| {
                    this.deerflow_request("legacy-history-get", json!({"id":id,"after":0}), cx)
                },
            ));
        }
        if browser.archives.len() > 20 {
            let mut nav = div().flex().gap(px(6.0));
            for (next, caption) in [(false, "归档目录上一页"), (true, "归档目录下一页")]
            {
                let page = if next {
                    browser.archive_page + 1
                } else {
                    browser.archive_page.saturating_sub(1)
                };
                nav = nav.child(df_button(
                    SharedString::from(format!("df-legacy-archive-page-{next}")),
                    caption,
                    disabled || page == browser.archive_page || page * 20 >= browser.archives.len(),
                    false,
                    theme,
                    cx,
                    move |this, _, cx| {
                        this.deerflow_settings.legacy.archive_page = page;
                        cx.notify();
                    },
                ));
            }
            group = group.child(nav).child(df_label(
                "归档目录最多列出 100 份",
                "相同内容重复导入按摘要合并。",
                theme,
            ));
        }
        if !browser.warnings.is_empty() {
            group = group.child(df_label("归档说明", browser.warnings.clone(), theme));
        }
        if let Some(id) = &browser.selected {
            group = group.child(df_label(
                format!("只读归档 {id}"),
                format!(
                    "当前条目 {}–{}",
                    browser.after + 1,
                    browser.after + browser.entries.len() as u64
                ),
                theme,
            ));
            let mut nav = div().flex().gap(px(6.0));
            for (next, caption) in [(false, "历史上一页"), (true, "历史下一页")] {
                let after = if next {
                    browser.next
                } else {
                    (browser.after > 0).then_some(browser.after.saturating_sub(20))
                };
                let id = id.clone();
                nav = nav.child(df_button(
                    SharedString::from(format!("df-legacy-history-page-{next}")),
                    caption,
                    disabled || after.is_none(),
                    false,
                    theme,
                    cx,
                    move |this, _, cx| {
                        if let Some(after) = after {
                            this.deerflow_request(
                                "legacy-history-get",
                                json!({"id":id,"after":after}),
                                cx,
                            );
                        }
                    },
                ));
            }
            group = group.child(nav);
            for (title, details) in &browser.entries {
                group = group.child(df_label(title.clone(), details.clone(), theme));
            }
        }
        group.into_any_element()
    }
}
