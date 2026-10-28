//! Optional desktop-only enrichment. No daemon requests, configuration locks or
//! global settings busy state are involved in catalog reads.
use super::model_metadata_logic::{self as logic, MetadataChange, RequestGuard};
use super::models_dev::CatalogSnapshot;
use super::*;
use std::collections::{HashMap, HashSet};
use std::time::Duration;

const PAGE_SIZE: usize = 8;

#[derive(Default)]
pub(super) struct MetadataState {
    generation: u64,
    revision: u64,
    pending: bool,
    automatic_requested: bool,
    snapshot: Option<CatalogSnapshot>,
    candidates: Vec<usize>,
    page: usize,
    selected: Option<usize>,
    changes: Vec<MetadataChange>,
    checked: HashSet<&'static str>,
    guard: Option<RequestGuard>,
    notice: Option<String>,
    query: String,
    new_models: HashMap<usize, HashSet<String>>,
    automatic_values: HashMap<usize, HashMap<&'static str, Value>>,
}

impl MetadataState {
    pub(super) fn invalidate(&mut self) {
        self.generation += 1;
        self.revision += 1;
        self.pending = false;
        self.automatic_requested = false;
        self.candidates.clear();
        self.page = 0;
        self.selected = None;
        self.changes.clear();
        self.checked.clear();
        self.guard = None;
        self.notice = None;
    }

    pub(super) fn reset_draft(&mut self) {
        self.invalidate();
        self.new_models.clear();
        self.automatic_values.clear();
    }

    fn replace_catalog(&mut self, snapshot: CatalogSnapshot, indices: Vec<usize>) {
        self.snapshot = Some(snapshot);
        self.candidates = indices;
        self.page = 0;
        self.selected = None;
        self.changes.clear();
        self.checked.clear();
    }

    pub(super) fn added(&mut self, index: usize) {
        self.invalidate();
        self.new_models.insert(index, HashSet::new());
    }

    pub(super) fn removed(&mut self, index: usize) {
        self.invalidate();
        self.new_models = std::mem::take(&mut self.new_models)
            .into_iter()
            .filter(|(i, _)| *i != index)
            .map(|(i, fields)| (if i > index { i - 1 } else { i }, fields))
            .collect();
        self.automatic_values = std::mem::take(&mut self.automatic_values)
            .into_iter()
            .filter(|(i, _)| *i != index)
            .map(|(i, fields)| (if i > index { i - 1 } else { i }, fields))
            .collect();
    }

    pub(super) fn edited(&mut self, path: &str) -> Vec<(&'static str, Value)> {
        let Some(rest) = path.strip_prefix("/models/") else {
            return Vec::new();
        };
        self.invalidate();
        if let Some((index, field)) = rest.split_once('/')
            && let Ok(index) = index.parse::<usize>()
        {
            if let Some(edited) = self.new_models.get_mut(&index) {
                edited.insert(field.to_owned());
            }
            if matches!(field, "provider" | "model" | "base_url") {
                return self
                    .automatic_values
                    .remove(&index)
                    .unwrap_or_default()
                    .into_iter()
                    .collect();
            }
            if let Some(values) = self.automatic_values.get_mut(&index) {
                values.remove(field);
            }
        }
        Vec::new()
    }
}

// All potentially large scans happen on the background executor, never render.
fn candidates(
    snapshot: &CatalogSnapshot,
    model: &Value,
    query: &str,
) -> (Vec<usize>, Option<usize>) {
    if query.trim().is_empty() {
        let matches = logic::match_candidates(&snapshot.catalog, model);
        return (matches.indices, matches.automatic);
    }
    let terms: Vec<_> = query.split_whitespace().map(str::to_lowercase).collect();
    let indices = snapshot
        .catalog
        .entries
        .iter()
        .enumerate()
        .filter_map(|(index, entry)| {
            let haystack = format!(
                "{} {} {} {}",
                entry.provider_id, entry.provider_name, entry.model_id, entry.name
            )
            .to_lowercase();
            terms
                .iter()
                .all(|term| haystack.contains(term))
                .then_some(index)
        })
        .collect();
    (indices, None)
}

impl Waku {
    pub(super) fn deerflow_metadata_edited(&mut self, path: &str, cx: &mut Context<Self>) {
        let state = &mut self.deerflow_settings;
        let restored = state.model_metadata.edited(path);
        let Some((index, _)) = path
            .strip_prefix("/models/")
            .and_then(|rest| rest.split_once('/'))
        else {
            return;
        };
        let Ok(index) = index.parse::<usize>() else {
            return;
        };
        // A different endpoint/model must never inherit unreviewed values
        // inferred for the previous identity. Explicit edits are retained.
        for (field, original) in restored {
            let path = format!("/models/{index}/{field}");
            if let Some(document) = state.draft.as_mut() {
                set_value(document, &path, original.clone());
            }
            if let Some(field) = state.fields.iter().find(|field| field.path == path)
                && let Some(slot) = field.slot
            {
                let text = match &original {
                    Value::Null => String::new(),
                    Value::Array(items) => items
                        .iter()
                        .filter_map(Value::as_str)
                        .collect::<Vec<_>>()
                        .join("\n"),
                    _ => original.to_string(),
                };
                state.input_text[slot] = text.clone();
                state.inputs[slot].update(cx, |input, cx| input.set_content(text, cx));
            }
        }
    }

    pub(super) fn deerflow_schedule_model_metadata(&mut self, cx: &mut Context<Self>) {
        let state = &mut self.deerflow_settings;
        if !state
            .model_metadata
            .new_models
            .contains_key(&state.selected_model)
        {
            return;
        }
        state.model_metadata.automatic_requested = true;
        let generation = state.model_metadata.generation;
        cx.spawn(async move |this, cx| {
            cx.background_executor()
                .timer(Duration::from_millis(500))
                .await;
            let _ = this.update(cx, |this, cx| {
                if this.deerflow_settings.model_metadata.generation == generation {
                    this.deerflow_lookup_model_metadata(true, false, cx);
                }
            });
        })
        .detach();
    }

    fn deerflow_lookup_model_metadata(
        &mut self,
        automatic: bool,
        force: bool,
        cx: &mut Context<Self>,
    ) {
        let state = &mut self.deerflow_settings;
        if !state.can_edit() || state.model_metadata.pending {
            return;
        }
        let Some(model) = state
            .draft
            .as_ref()
            .and_then(|d| d["models"].get(state.selected_model))
            .cloned()
        else {
            return;
        };
        if automatic
            && model["model"]
                .as_str()
                .is_none_or(|id| id.trim().is_empty())
        {
            return;
        }
        state.model_metadata.invalidate();
        state.model_metadata.automatic_requested = automatic;
        state.model_metadata.query.clear();
        state
            .model_metadata_search
            .update(cx, |input, cx| input.set_content(String::new(), cx));
        state.model_metadata.pending = true;
        state.model_metadata.notice = Some("正在查询模型信息，可继续编辑或保存。".into());
        let generation = state.model_metadata.generation;
        let guard = RequestGuard::capture(&model, state.model_metadata.revision);
        state.model_metadata.guard = Some(guard.clone());
        cx.spawn(async move |this, cx| {
            let cached = cx
                .background_executor()
                .spawn({
                    let model = model.clone();
                    async move {
                        models_dev::load_cached().map(|snapshot| {
                            let found = candidates(&snapshot, &model, "");
                            (snapshot, found)
                        })
                    }
                })
                .await;
            let needs_refresh = force || cached.as_ref().is_none_or(|(snapshot, _)| snapshot.stale);
            if let Some((snapshot, found)) = cached {
                let _ = this.update(cx, |this, cx| {
                    this.deerflow_model_metadata_result(
                        generation,
                        &guard,
                        Ok((snapshot, found)),
                        automatic,
                        needs_refresh,
                        cx,
                    )
                });
            }
            if needs_refresh {
                let result = cx
                    .background_executor()
                    .spawn(async move {
                        models_dev::refresh(force).map(|snapshot| {
                            let found = candidates(&snapshot, &model, "");
                            (snapshot, found)
                        })
                    })
                    .await;
                let _ = this.update(cx, |this, cx| {
                    this.deerflow_model_metadata_result(
                        generation, &guard, result, automatic, false, cx,
                    )
                });
            }
        })
        .detach();
        cx.notify();
    }

    fn deerflow_model_metadata_result(
        &mut self,
        generation: u64,
        guard: &RequestGuard,
        result: Result<(CatalogSnapshot, (Vec<usize>, Option<usize>)), String>,
        automatic: bool,
        pending: bool,
        cx: &mut Context<Self>,
    ) {
        let state = &mut self.deerflow_settings;
        if state.model_metadata.generation != generation {
            if automatic && !pending && result.is_ok() && state.model_metadata.automatic_requested {
                self.deerflow_schedule_model_metadata(cx);
            }
            return;
        }
        state.model_metadata.pending = false;
        let Some(model) = state
            .draft
            .as_ref()
            .and_then(|d| d["models"].get(state.selected_model))
        else {
            return;
        };
        if !guard.matches(model, state.model_metadata.revision) || !state.can_edit() {
            return;
        }
        match result {
            Err(error) => {
                state.model_metadata.notice =
                    Some(format!("模型信息暂不可用，可继续手动配置。{error}"))
            }
            Ok((snapshot, (indices, matched))) => {
                // A stale result is still useful for explicit review, but must
                // not invalidate the fresh result that is on its way.
                let auto_fill = automatic && !pending && !snapshot.stale;
                state.model_metadata.pending = pending;
                let available = !indices.is_empty();
                state.model_metadata.notice = Some(
                    if snapshot.stale {
                        "正在使用缓存中的模型信息；可继续手动配置。"
                    } else if !available {
                        "未找到精确匹配，可搜索并选择目录中的模型。"
                    } else if matched.is_none() {
                        "请核对供应商并选择目录模型；自定义接口的能力可能不同。"
                    } else {
                        "已匹配模型信息。选择需要填入的字段，保存后生效。"
                    }
                    .into(),
                );
                state.model_metadata.replace_catalog(snapshot, indices);
                // Do not auto-select an unknown host, even with only one candidate.
                if let Some(index) = matched {
                    self.deerflow_select_model_metadata(index, auto_fill, cx);
                }
            }
        }
        cx.notify();
    }

    fn deerflow_select_model_metadata(
        &mut self,
        index: usize,
        automatic: bool,
        cx: &mut Context<Self>,
    ) {
        let state = &mut self.deerflow_settings;
        let Some(model) = state
            .draft
            .as_ref()
            .and_then(|d| d["models"].get(state.selected_model))
        else {
            return;
        };
        let Some(entry) = state
            .model_metadata
            .snapshot
            .as_ref()
            .and_then(|s| s.catalog.entries.get(index))
        else {
            return;
        };
        let changes = if automatic {
            let edited = state.model_metadata.new_models.get(&state.selected_model);
            logic::automatic_patch(
                entry,
                model,
                edited.is_some(),
                edited.unwrap_or(&HashSet::new()),
            )
        } else {
            logic::suggested_patch(entry, model)
        };
        state.model_metadata.selected = Some(index);
        state.model_metadata.guard =
            Some(RequestGuard::capture(model, state.model_metadata.revision));
        state.model_metadata.changes = changes;
        state.model_metadata.checked = state
            .model_metadata
            .changes
            .iter()
            .map(|change| change.field)
            .collect();
        if automatic && !state.model_metadata.changes.is_empty() {
            self.deerflow_apply_model_metadata(true, cx);
        }
        cx.notify();
    }

    fn deerflow_apply_model_metadata(&mut self, automatic: bool, cx: &mut Context<Self>) {
        let state = &self.deerflow_settings;
        let index = state.selected_model;
        let Some(model) = state.draft.as_ref().and_then(|d| d["models"].get(index)) else {
            return;
        };
        if !state.can_edit()
            || state
                .model_metadata
                .guard
                .as_ref()
                .is_none_or(|g| !g.matches(model, state.model_metadata.revision))
        {
            return;
        }
        let changes: Vec<_> = state
            .model_metadata
            .changes
            .iter()
            .filter(|change| {
                state.model_metadata.checked.contains(change.field)
                    && state.can_edit_field(&format!("/models/{index}/{}", change.field))
                    && !state
                        .invalid_text
                        .contains_key(&format!("/models/{index}/{}", change.field))
            })
            .map(|change| {
                (
                    change.field,
                    change.current.clone(),
                    change.suggested.clone(),
                )
            })
            .collect();
        if changes.is_empty() {
            return;
        }
        // Apply a reviewed, bounded patch atomically to the current draft. Avoid
        // individual field handlers, which would reschedule automatic lookups.
        let state = &mut self.deerflow_settings;
        for (field, original, value) in changes {
            let path = format!("/models/{index}/{field}");
            set_value(state.draft.as_mut().unwrap(), &path, value);
            state.errors.remove(&path);
            if automatic {
                state
                    .model_metadata
                    .automatic_values
                    .entry(index)
                    .or_default()
                    .entry(field)
                    .or_insert(original);
            } else {
                if let Some(edited) = state.model_metadata.new_models.get_mut(&index) {
                    edited.insert(field.into());
                }
                if let Some(values) = state.model_metadata.automatic_values.get_mut(&index) {
                    values.remove(field);
                }
            }
        }
        state.dirty = true;
        state.edit_generation += 1;
        state.confirm_reload = false;
        state.model_metadata.invalidate();
        state.model_metadata.notice =
            Some("模型信息已填入草稿，可继续修改；保存并应用后生效。".into());
        self.deerflow_rebind(cx);
        cx.notify();
    }

    pub(in crate::app) fn deerflow_model_metadata_search(&mut self, cx: &mut Context<Self>) {
        let state = &mut self.deerflow_settings;
        let query = state.model_metadata_search.read(cx).content().to_owned();
        if query == state.model_metadata.query {
            return;
        }
        state.model_metadata.invalidate();
        state.model_metadata.query = query.clone();
        let Some(snapshot) = state.model_metadata.snapshot.clone() else {
            return;
        };
        let Some(model) = state
            .draft
            .as_ref()
            .and_then(|d| d["models"].get(state.selected_model))
            .cloned()
        else {
            return;
        };
        let generation = state.model_metadata.generation;
        let guard = RequestGuard::capture(&model, state.model_metadata.revision);
        state.model_metadata.guard = Some(guard.clone());
        cx.spawn(async move |this, cx| {
            cx.background_executor()
                .timer(Duration::from_millis(180))
                .await;
            let found = cx
                .background_executor()
                .spawn(async move { candidates(&snapshot, &model, &query) })
                .await;
            let _ = this.update(cx, |this, cx| {
                let state = &mut this.deerflow_settings;
                if state.model_metadata.generation != generation {
                    return;
                }
                state.model_metadata.candidates = found.0;
                state.model_metadata.notice = Some(
                    "请选择实际使用的供应商和模型；选择目录条目不会更改接口地址或模型 ID。".into(),
                );
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }

    pub(super) fn render_model_metadata(&self, theme: Theme, cx: &mut Context<Self>) -> Div {
        let state = &self.deerflow_settings;
        let metadata = &state.model_metadata;
        let mut group = df_group(theme)
            .child(df_label(
                "模型信息补全",
                "来源 models.dev。补全上下文、图片输入和兼容的推理强度；查询不可用时仍可手动配置。",
                theme,
            ))
            .child(
                div()
                    .flex()
                    .flex_wrap()
                    .gap(px(6.0))
                    .child(df_button(
                        "df-metadata-lookup",
                        if metadata.pending {
                            "正在查询…"
                        } else {
                            "补全模型信息"
                        },
                        metadata.pending || !state.can_edit(),
                        false,
                        theme,
                        cx,
                        |this, _, cx| this.deerflow_lookup_model_metadata(false, false, cx),
                    ))
                    .child(df_button(
                        "df-metadata-refresh",
                        "刷新目录",
                        metadata.pending || !state.can_edit(),
                        false,
                        theme,
                        cx,
                        |this, _, cx| this.deerflow_lookup_model_metadata(false, true, cx),
                    )),
            );
        if let Some(notice) = &metadata.notice {
            group = group.child(
                div()
                    .text_size(sp(12.5))
                    .text_color(theme.text_secondary)
                    .child(notice.clone()),
            );
        }
        if let Some(snapshot) = &metadata.snapshot {
            let fetched = chrono::DateTime::from_timestamp(snapshot.fetched_at as i64, 0)
                .map(|date| {
                    date.with_timezone(&chrono::Local)
                        .format("%Y-%m-%d %H:%M")
                        .to_string()
                })
                .unwrap_or_else(|| "未知".into());
            group = group.child(df_label(
                format!(
                    "目录更新时间：{fetched}{}",
                    if snapshot.stale { "（缓存）" } else { "" }
                ),
                snapshot.warning.clone().unwrap_or_default(),
                theme,
            ));
            group = group.child(TextField::new(
                "df-metadata-search",
                state.model_metadata_search.clone(),
            ));
            let count = metadata.candidates.len();
            for &index in metadata
                .candidates
                .iter()
                .skip(metadata.page * PAGE_SIZE)
                .take(PAGE_SIZE)
            {
                let entry = &snapshot.catalog.entries[index];
                group = group.child(
                    df_button(
                        SharedString::from(format!("df-metadata-candidate-{index}")),
                        format!("{} · {}", entry.provider_name, entry.model_id),
                        !state.can_edit(),
                        metadata.selected == Some(index),
                        theme,
                        cx,
                        move |this, _, cx| {
                            this.deerflow_settings.model_metadata.generation += 1;
                            this.deerflow_settings.model_metadata.pending = false;
                            this.deerflow_settings.model_metadata.automatic_requested = false;
                            this.deerflow_select_model_metadata(index, false, cx);
                        },
                    )
                    .w_full()
                    .justify_start(),
                );
            }
            if count > PAGE_SIZE {
                group = group.child(
                    div()
                        .flex()
                        .gap(px(6.0))
                        .child(df_button(
                            "df-metadata-previous",
                            "上一页",
                            metadata.page == 0,
                            false,
                            theme,
                            cx,
                            |this, _, cx| {
                                this.deerflow_settings.model_metadata.page =
                                    this.deerflow_settings.model_metadata.page.saturating_sub(1);
                                cx.notify();
                            },
                        ))
                        .child(df_button(
                            "df-metadata-next",
                            format!("下一页（共 {count} 项）"),
                            (metadata.page + 1) * PAGE_SIZE >= count,
                            false,
                            theme,
                            cx,
                            |this, _, cx| {
                                this.deerflow_settings.model_metadata.page += 1;
                                cx.notify();
                            },
                        )),
                );
            }
        }
        if metadata.selected.is_some() {
            if metadata.changes.is_empty() {
                group = group.child(df_label(
                    "没有可补全的差异",
                    "当前配置已一致，或目录未提供本应用支持的字段。",
                    theme,
                ));
            }
            for change in &metadata.changes {
                let field = change.field;
                let label = match field {
                    "context_window" => "上下文窗口",
                    "supports_vision" => "支持图片输入",
                    _ => "推理强度",
                };
                group = group.child(df_button(
                    SharedString::from(format!("df-metadata-field-{field}")),
                    format!(
                        "{} {label}：{} → {}",
                        if metadata.checked.contains(field) {
                            "☑"
                        } else {
                            "☐"
                        },
                        display_value(&change.current),
                        display_value(&change.suggested)
                    ),
                    !state.can_edit_field(&format!("/models/{}/{field}", state.selected_model)),
                    metadata.checked.contains(field),
                    theme,
                    cx,
                    move |this, _, cx| {
                        // Reviewing a cached suggestion takes ownership of the
                        // panel; a background refresh must not reset choices.
                        this.deerflow_settings.model_metadata.generation += 1;
                        this.deerflow_settings.model_metadata.pending = false;
                        this.deerflow_settings.model_metadata.automatic_requested = false;
                        let checked = &mut this.deerflow_settings.model_metadata.checked;
                        if !checked.remove(field) {
                            checked.insert(field);
                        }
                        cx.notify();
                    },
                ));
            }
            if !metadata.changes.is_empty() {
                group = group.child(df_button(
                    "df-metadata-apply",
                    "填入选中字段",
                    metadata.checked.is_empty() || !state.can_edit(),
                    false,
                    theme,
                    cx,
                    |this, _, cx| this.deerflow_apply_model_metadata(false, cx),
                ));
            }
        }
        group
    }
}

fn display_value(value: &Value) -> String {
    match value {
        Value::Null => "未设置".into(),
        Value::Bool(value) => if *value { "是" } else { "否" }.into(),
        Value::Array(values) => values
            .iter()
            .filter_map(Value::as_str)
            .collect::<Vec<_>>()
            .join(" / "),
        _ => value.to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn metadata_edits_and_saves_invalidate_pending_results() {
        let mut state = MetadataState::default();
        state.added(2);
        let generation = state.generation;
        state.pending = true;
        state.edited("/models/2/supports_vision");
        assert!(state.generation > generation);
        assert!(!state.pending);
        assert!(state.new_models[&2].contains("supports_vision"));
        state.reset_draft();
        assert!(state.new_models.is_empty());
    }

    #[test]
    fn metadata_removal_preserves_field_protection_at_shifted_indices() {
        let mut state = MetadataState::default();
        state.added(1);
        state.added(2);
        state.edited("/models/2/context_window");
        state.removed(1);
        assert_eq!(state.new_models.len(), 1);
        assert!(state.new_models[&1].contains("context_window"));
    }

    #[test]
    fn metadata_identity_edit_reverts_only_automatic_values() {
        let mut state = MetadataState::default();
        state.added(0);
        state.automatic_values.insert(
            0,
            HashMap::from([
                ("context_window", json!(0)),
                ("supports_vision", json!(false)),
            ]),
        );
        assert!(state.edited("/models/0/supports_vision").is_empty());
        let reverted = state.edited("/models/0/model");
        assert_eq!(reverted, vec![("context_window", json!(0))]);
        assert!(state.new_models[&0].contains("supports_vision"));
        assert!(!state.automatic_values.contains_key(&0));
    }

    #[test]
    fn metadata_refresh_removes_suggestions_for_deleted_catalog_model() {
        let mut state = MetadataState::default();
        state.selected = Some(3);
        state.changes.push(MetadataChange {
            field: "context_window",
            current: json!(0),
            suggested: json!(128000),
        });
        state.checked.insert("context_window");
        state.replace_catalog(
            CatalogSnapshot {
                catalog: std::sync::Arc::new(models_dev::Catalog::default()),
                fetched_at: 1,
                stale: false,
                warning: None,
            },
            Vec::new(),
        );
        assert!(state.selected.is_none());
        assert!(state.changes.is_empty());
        assert!(state.checked.is_empty());
    }

    #[test]
    fn metadata_save_cancels_automatic_retry_and_preserves_adopted_values() {
        let mut state = MetadataState::default();
        state.added(0);
        state.automatic_requested = true;
        state
            .automatic_values
            .insert(0, HashMap::from([("context_window", json!(0))]));
        state.reset_draft();
        assert!(!state.automatic_requested);
        assert!(state.edited("/models/0/base_url").is_empty());
        assert!(state.new_models.is_empty());
    }
}
