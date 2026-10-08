//! Runtime snapshots are prepared on events; frames only read cached rows.
use super::*;
use gpui::relative;
use serde::Deserialize;

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
enum TodoStatus {
    Pending,
    InProgress,
    Completed,
}

impl TodoStatus {
    fn label(self) -> &'static str {
        match self {
            Self::Pending => "待执行",
            Self::InProgress => "进行中",
            Self::Completed => "已完成",
        }
    }

    fn color(self, theme: Theme) -> Hsla {
        match self {
            Self::Pending => theme.text_secondary,
            Self::InProgress => theme.accent,
            Self::Completed => theme.success,
        }
    }
}

struct TodoRow {
    content: SharedString,
    status: TodoStatus,
}

#[derive(Default)]
struct TodoProgress {
    rows: Vec<TodoRow>,
    completed: usize,
    summary: SharedString,
}

impl TodoProgress {
    fn from_activity(item: &ActivityItem) -> Option<Self> {
        if item.kind != ActivityKind::Plan || item.failed {
            return None;
        }
        #[derive(Deserialize)]
        struct Entry {
            content: String,
            status: TodoStatus,
        }
        #[derive(Deserialize)]
        struct Plan {
            entries: Vec<Entry>,
        }
        let rows = if let Some(arguments) = &item.arguments {
            let plan: Plan = serde_json::from_str(arguments).ok()?;
            plan.entries
                .into_iter()
                .map(|entry| TodoRow {
                    content: entry.content.into(),
                    status: entry.status,
                })
                .collect::<Vec<_>>()
        } else {
            // Older saved ACP plans have only a rendered checklist. Do not
            // infer completion from ActivityItem.complete: turn settlement
            // completes activities even when their todos are unfinished.
            let output = item.output.as_deref()?;
            let mut rows = Vec::new();
            for line in output.lines().filter(|line| !line.trim().is_empty()) {
                let (content, status) = if let Some(content) = line.strip_prefix("- [x] ") {
                    (content, TodoStatus::Completed)
                } else if let Some(content) = line.strip_prefix("- [ ] ") {
                    match content.strip_suffix(" (in progress)") {
                        Some(content) => (content, TodoStatus::InProgress),
                        None => (content, TodoStatus::Pending),
                    }
                } else {
                    return None;
                };
                rows.push(TodoRow {
                    content: content.to_owned().into(),
                    status,
                });
            }
            rows
        };
        let completed = rows
            .iter()
            .filter(|row| row.status == TodoStatus::Completed)
            .count();
        let running = rows
            .iter()
            .filter(|row| row.status == TodoStatus::InProgress)
            .count();
        let summary = format!("{completed}/{} 已完成 · {running} 进行中", rows.len()).into();
        Some(Self {
            rows,
            completed,
            summary,
        })
    }

    fn fraction(&self) -> f32 {
        if self.rows.is_empty() {
            0.0
        } else {
            self.completed as f32 / self.rows.len() as f32
        }
    }
}

pub(in crate::app) struct RuntimeView {
    initialized: bool,
    turn_id: Option<Uuid>,
    turn_started_at: Option<u64>,
    todos: Option<TodoProgress>,
    todo_list: ListState,
    activity: Option<ActivitySnapshot>,
    agent_rows: Vec<Key>,
    agent_list: ListState,
    agent_summary: SharedString,
    latest_root: Option<Key>,
}

impl Default for RuntimeView {
    fn default() -> Self {
        Self {
            initialized: false,
            turn_id: None,
            turn_started_at: None,
            todos: None,
            todo_list: ListState::new(0, ListAlignment::Top, px(60.0)),
            activity: None,
            agent_rows: Vec::new(),
            agent_list: ListState::new(0, ListAlignment::Top, px(120.0)),
            agent_summary: "尚无子 Agent".into(),
            latest_root: None,
        }
    }
}

impl RuntimeView {
    pub(in crate::app) fn sync(&mut self, session: &AgentSession) -> bool {
        if !session.detail_loaded {
            return false;
        }
        let turn_id = session.turns.last().map(|turn| turn.id);
        if self.initialized && self.turn_id == turn_id {
            return false;
        }
        self.initialized = true;
        self.turn_id = turn_id;
        self.turn_started_at = session.turns.last().map(|turn| turn.started_at);
        self.todos = None;
        self.activity = None;
        self.todo_list.reset(0);
        // Hydration and turn changes rebuild once. Tool updates below amend
        // this snapshot directly, without walking the transcript in render.
        let mut found_plan = false;
        for block in session.transcript_blocks.iter().rev() {
            if block.turn_id != turn_id {
                continue;
            }
            for item in block.activities.iter().rev() {
                if item.kind == ActivityKind::Plan && !found_plan {
                    if let Some(todos) = TodoProgress::from_activity(item) {
                        self.todo_list.reset(todos.rows.len());
                        self.todos = Some(todos);
                        found_plan = true;
                    }
                } else if !matches!(item.kind, ActivityKind::Plan | ActivityKind::Reasoning)
                    && self.activity.is_none()
                {
                    self.activity = Some(activity_summary(item));
                }
            }
            if found_plan && self.activity.is_some() {
                break;
            }
        }
        true
    }

    pub(in crate::app) fn observe(&mut self, item: &ActivityItem) {
        if let Some(todos) = TodoProgress::from_activity(item) {
            // Fixed-height rows can update without resetting their scroll.
            let old_len = self.todos.as_ref().map_or(0, |todos| todos.rows.len());
            if old_len != todos.rows.len() {
                self.todo_list.splice(0..old_len, todos.rows.len());
            }
            self.todos = Some(todos);
        } else if !matches!(item.kind, ActivityKind::Plan | ActivityKind::Reasoning) {
            self.activity = Some(activity_summary(item));
        }
    }

    pub(super) fn rebuild_agents(&mut self, nodes: &BTreeMap<Key, CachedNode>) {
        self.latest_root = nodes
            .iter()
            .filter(|(_, cached)| cached.node.mode == "root")
            .max_by_key(|(key, cached)| (cached.node.created_at.as_deref().unwrap_or(""), *key))
            .map(|(key, _)| key.clone());
        let latest_root = self.latest_root.as_ref().map(|key| key.0.as_str());
        let current_root = self.latest_root.as_ref().is_some_and(|key| {
            self.turn_started_at.is_none_or(|turn| {
                nodes[key]
                    .started_at
                    .is_none_or(|started| started >= turn.saturating_sub(1))
            })
        });
        let mut rows = nodes
            .iter()
            .filter(|(_, cached)| {
                cached.node.mode != "root"
                    && (!is_terminal(&cached.node.status)
                        || (current_root && Some(cached.node.root_id.as_str()) == latest_root))
            })
            .map(|(key, _)| key.clone())
            .collect::<Vec<_>>();
        rows.sort_by(|a, b| {
            let a_node = &nodes[a].node;
            let b_node = &nodes[b].node;
            agent_rank(a_node)
                .cmp(&agent_rank(b_node))
                .then_with(|| b_node.created_at.cmp(&a_node.created_at))
                .then_with(|| a.cmp(b))
        });
        let mut counts = [0usize; 7];
        for key in &rows {
            counts[agent_rank(&nodes[key].node)] += 1;
        }
        self.agent_summary = if rows.is_empty() {
            "尚无子 Agent".into()
        } else {
            counts
                .into_iter()
                .zip(["执行", "等待", "排队", "失败", "完成", "取消", "状态未确认"])
                .filter(|(count, _)| *count > 0)
                .map(|(count, label)| format!("{count} {label}"))
                .collect::<Vec<_>>()
                .join(" · ")
                .into()
        };
        if self.agent_rows != rows {
            self.agent_list.splice(0..self.agent_rows.len(), rows.len());
            self.agent_rows = rows;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn plan(entries: serde_json::Value) -> ActivityItem {
        ActivityItem::new(
            Some("acp-plan".into()),
            ActivityKind::Plan,
            "Plan",
            None,
            true,
        )
        .with_arguments(Some(json!({"entries": entries}).to_string()))
    }

    #[test]
    fn todo_progress_uses_reported_status_and_preserves_multiline_content() {
        let item = plan(json!([
            {"content": "Read inputs", "status": "completed"},
            {"content": "Write\n- [x] this is part of a title", "status": "in_progress"},
            {"content": "Verify (in progress)", "status": "pending"}
        ]));
        let progress = TodoProgress::from_activity(&item).unwrap();
        assert_eq!(progress.completed, 1);
        assert_eq!(progress.rows[1].status, TodoStatus::InProgress);
        assert_eq!(progress.rows[2].status, TodoStatus::Pending);
        assert_eq!(
            progress.rows[1].content.as_ref(),
            "Write\n- [x] this is part of a title"
        );
        assert!((progress.fraction() - 1.0 / 3.0).abs() < 0.001);
    }

    #[test]
    fn legacy_plan_settlement_does_not_complete_unfinished_todos() {
        let item = ActivityItem::new(
            Some("acp-plan".into()),
            ActivityKind::Plan,
            "Plan",
            None,
            true,
        )
        .with_output(Some(
            "- [x] Read\n- [ ] Write (in progress)\n- [ ] Verify".into(),
        ));
        let progress = TodoProgress::from_activity(&item).unwrap();
        assert_eq!(progress.completed, 1);
        assert_eq!(progress.rows[1].status, TodoStatus::InProgress);
        assert_eq!(progress.rows[2].status, TodoStatus::Pending);
    }

    #[test]
    fn empty_plan_clears_progress_and_new_turn_does_not_show_old_todos() {
        let mut view = RuntimeView::default();
        view.observe(&plan(
            json!([{"content":"Inspect", "status":"in_progress"}]),
        ));
        assert_eq!(view.todos.as_ref().unwrap().rows.len(), 1);
        view.observe(&plan(json!([])));
        assert!(view.todos.as_ref().unwrap().rows.is_empty());
        let mut session = AgentSession::new(Uuid::new_v4(), ProviderKind::DeerFlow);
        let first_turn = session.begin_turn("First task");
        session.transcript_blocks.push(TranscriptBlock {
            after_message: session.messages.len(),
            turn_id: Some(first_turn),
            activities: vec![plan(json!([{"content":"Old task", "status":"pending"}]))],
        });
        view.sync(&session);
        assert_eq!(view.todos.as_ref().unwrap().rows.len(), 1);
        session.finish_active_turn(TurnStatus::Completed);
        session.begin_turn("New task");
        view.sync(&session);
        assert!(view.todos.is_none());
        assert!(view.activity.is_none());
    }

    #[test]
    fn agent_overview_prioritizes_live_work_and_excludes_old_completed_roots() {
        let mut nodes = BTreeMap::new();
        for (root, id, mode, status, created_at) in [
            (
                "old",
                "root/old",
                "root",
                "completed",
                "2026-10-07T01:00:00Z",
            ),
            (
                "old",
                "old-done",
                "foreground",
                "completed",
                "2026-10-07T01:00:00Z",
            ),
            (
                "old",
                "old-live",
                "background",
                "running",
                "2026-10-07T01:00:00Z",
            ),
            ("new", "root/new", "root", "running", "2026-10-07T02:00:00Z"),
            (
                "new",
                "new-done",
                "foreground",
                "completed",
                "2026-10-07T02:00:00Z",
            ),
            (
                "new",
                "new-wait",
                "background",
                "waiting_input",
                "2026-10-07T02:00:00Z",
            ),
            (
                "new",
                "new-live",
                "foreground",
                "running",
                "2026-10-07T02:00:00Z",
            ),
        ] {
            let node: Node = serde_json::from_value(json!({
                "id": id, "rootId": root, "rootSessionId": "session", "role": "researcher", "mode": mode,
                "invocationId": if mode == "root" { "" } else { id }, "parentId": if mode == "root" { String::new() } else { format!("root/{root}") },
                "depth": if mode == "root" { 0 } else { 1 }, "status": status, "version": 1, "attempt": 1,
                "createdAt": created_at, "allowedActions": []
            })).unwrap();
            nodes.insert(key(&node), CachedNode::parse(node, "session").unwrap());
        }
        let mut view = RuntimeView::default();
        view.rebuild_agents(&nodes);
        let ids = view
            .agent_rows
            .iter()
            .map(|key| key.1.as_str())
            .collect::<Vec<_>>();
        assert_eq!(ids, ["new-live", "old-live", "new-wait", "new-done"]);
        assert!(view.agent_summary.contains("2 执行 · 1 等待"));
        nodes
            .get_mut(&("old".into(), "old-live".into()))
            .unwrap()
            .node
            .status = "completed".into();
        view.rebuild_agents(&nodes);
        assert!(!view.agent_rows.iter().any(|key| key.1 == "old-live"));
    }
}

fn agent_rank(node: &Node) -> usize {
    match node.status.as_str() {
        "running" | "resuming" | "cancelling" | "suspending" if node.waiting.is_none() => 0,
        "waiting_input" | "suspended" | "needs_reconciliation" => 1,
        "queued" | "pending" => 2,
        "failed" => 3,
        "completed" => 4,
        "cancelled" | "canceled" => 5,
        _ if node.waiting.is_some() => 1,
        _ => 6,
    }
}

struct ActivitySnapshot {
    subject: SharedString,
    complete: bool,
    failed: bool,
}

fn activity_summary(item: &ActivityItem) -> ActivitySnapshot {
    let subject = item
        .display_description
        .as_deref()
        .or(item.display_target.as_deref())
        .or(item.tool_name.as_deref())
        .unwrap_or(&item.title);
    ActivitySnapshot {
        subject: subject.to_owned().into(),
        complete: item.complete,
        failed: item.failed,
    }
}

fn main_status(session: &AgentSession, phase: Option<StreamPhase>) -> &'static str {
    match session.status {
        SessionStatus::Connecting => "正在连接",
        SessionStatus::Working => match phase {
            Some(StreamPhase::Reasoning) => "思考中",
            Some(StreamPhase::Text) => "生成回复中",
            _ => "执行中",
        },
        SessionStatus::Waiting => "等待你的审批或回复",
        SessionStatus::Background => "等待后台任务",
        SessionStatus::Failed => "执行失败",
        SessionStatus::Idle => match session.turns.last().map(|turn| turn.status) {
            Some(TurnStatus::Completed) => "已完成",
            Some(TurnStatus::Interrupted) => "已中断",
            Some(TurnStatus::Failed) => "执行失败",
            Some(TurnStatus::Running) => "运行状态待同步",
            None => "等待任务",
        },
    }
}

fn heading(title: &str, theme: Theme) -> Div {
    div()
        .text_size(sp(13.0))
        .font_weight(FontWeight::SEMIBOLD)
        .text_color(theme.text)
        .child(title.to_owned())
}

impl Waku {
    pub(super) fn render_deerflow_runtime_view(&self, cx: &mut Context<Self>) -> AnyElement {
        let theme = Theme::current(cx);
        let Some(session) = self
            .selected_session()
            .filter(|session| session.provider == ProviderKind::DeerFlow)
        else {
            return task_text("请选择 DeerFlow 会话", theme).into_any_element();
        };
        let session_id = session.id;
        let browser = self.deerflow_tasks.get(&session_id);
        let view = browser.map(|browser| &browser.runtime_view);
        let phase = self
            .runtimes
            .get(&session_id)
            .and_then(|runtime| runtime.stream_phase);
        let mut panel = div()
            .id("deerflow-runtime")
            .tab_group()
            .size_full()
            .flex()
            .flex_col()
            .gap(px(8.0))
            .p(px(12.0))
            .overflow_hidden();
        let disconnected = browser.is_none_or(|browser| browser.connection.is_empty());
        panel = panel.child(
            div()
                .flex()
                .flex_wrap()
                .gap(px(6.0))
                .child(task_button(
                    "df-runtime-refresh",
                    if disconnected {
                        "连接运行状态"
                    } else {
                        "刷新状态"
                    },
                    self.goal_runtime_starts.contains(&session_id)
                        || browser.is_some_and(|browser| browser.pending.is_some()),
                    cx,
                    |this, _, cx| this.open_deerflow_tasks(cx),
                ))
                .child(task_button(
                    "df-runtime-management",
                    "任务详情与管理",
                    false,
                    cx,
                    move |this, _, cx| {
                        this.deerflow_tasks
                            .entry(session_id)
                            .or_default()
                            .show_management = true;
                        cx.notify();
                    },
                )),
        );
        panel = panel.child(task_text(
            if disconnected {
                "运行状态未连接；Todo 来自当前对话记录。"
            } else if browser.is_some_and(|browser| !browser.capabilities.tree) {
                "Todo 实时更新 · 当前服务未提供 Agent 状态"
            } else if browser.is_some_and(|browser| browser.stale) {
                "正在同步 · Agent 列表为上次缓存状态"
            } else {
                "Todo 实时更新 · Agent 状态每 5 秒刷新"
            },
            theme,
        ));
        if let Some(error) = browser.and_then(|browser| browser.error.as_ref()) {
            panel = panel.child(task_text(error.clone(), theme));
        }
        let root = view
            .and_then(|view| view.latest_root.as_ref())
            .and_then(|key| browser?.nodes.get(key))
            .filter(|root| {
                session.turns.last().is_none_or(|turn| {
                    root.started_at
                        .is_none_or(|started| started >= turn.started_at.saturating_sub(1))
                })
            });
        let model = root
            .map(|root| root.node.model.as_str())
            .filter(|model| !model.is_empty())
            .or(session.model.as_deref())
            .unwrap_or("默认模型");
        let mut main = div()
            .flex_none()
            .p(px(10.0))
            .rounded(px(6.0))
            .border_1()
            .border_color(theme.border)
            .bg(theme.raised)
            .child(heading(
                &format!("主 Agent · {}", main_status(session, phase)),
                theme,
            ))
            .child(task_text(model.to_owned(), theme));
        if let Some(activity) = view.and_then(|view| view.activity.as_ref()) {
            let label = if activity.failed {
                "工具失败"
            } else if !activity.complete
                && session.status.is_busy()
                && phase == Some(StreamPhase::Activity)
            {
                "正在执行"
            } else {
                "最近活动"
            };
            main = main
                .child(task_text(format!("{label} · {}", activity.subject), theme).line_clamp(2));
        }
        if let Some(root) = root {
            main = main.child(task_text(root.progress.clone(), theme));
        }
        panel = panel.child(main).child(heading("Todo 进度", theme));
        if let Some((view, todos)) =
            view.and_then(|view| view.todos.as_ref().map(|todos| (view, todos)))
        {
            if todos.rows.is_empty() {
                panel = panel.child(task_text("当前 Todo 列表为空。", theme));
            } else {
                panel = panel.child(task_text(todos.summary.clone(), theme)).child(
                    div()
                        .h(px(4.0))
                        .w_full()
                        .flex_none()
                        .rounded_full()
                        .bg(theme.overlay_strong)
                        .child(
                            div()
                                .h_full()
                                .w(relative(todos.fraction()))
                                .rounded_full()
                                .bg(theme.success),
                        ),
                );
                let weak = cx.entity().downgrade();
                panel = panel.child(
                    div()
                        .h(px((todos.rows.len() as f32 * 62.0).min(186.0)))
                        .flex_none()
                        .overflow_hidden()
                        .child(
                            list(view.todo_list.clone(), move |index, _, cx| {
                                weak.update(cx, |this, cx| {
                                    this.render_deerflow_todo_row(session_id, index, cx)
                                })
                                .unwrap_or_else(|_| div().into_any_element())
                            })
                            .size_full(),
                        ),
                );
            }
        } else {
            panel = panel.child(task_text(
                "当前任务尚未上报 Todo；Agent 制定计划后会显示进度。",
                theme,
            ));
        }
        panel = panel.child(heading("子 Agent", theme));
        if let Some(view) = view {
            panel = panel.child(task_text(view.agent_summary.clone(), theme));
            if browser.is_some_and(|browser| {
                browser.has_more || browser.partial_view || browser.root_filter.is_some()
            }) {
                panel = panel.child(task_text(
                    "当前 Agent 列表不完整，可在任务详情与管理中加载更多。",
                    theme,
                ));
            }
            if view.agent_rows.is_empty() {
                panel = panel.child(task_text(
                    if browser.is_none_or(|browser| !browser.loaded || browser.stale) {
                        "子 Agent 状态尚未同步。"
                    } else if browser.is_some_and(|browser| {
                        browser.has_more || browser.partial_view || browser.root_filter.is_some()
                    }) {
                        "已读取的记录中暂无子 Agent，完整列表仍待加载。"
                    } else {
                        "尚未委派子任务，当前由主 Agent 执行。"
                    },
                    theme,
                ));
            } else {
                let weak = cx.entity().downgrade();
                panel = panel.child(
                    div().flex_1().min_h(px(72.0)).overflow_hidden().child(
                        list(view.agent_list.clone(), move |index, _, cx| {
                            weak.update(cx, |this, cx| {
                                this.render_deerflow_runtime_agent(session_id, index, cx)
                            })
                            .unwrap_or_else(|_| div().into_any_element())
                        })
                        .size_full(),
                    ),
                );
            }
        }
        panel.into_any_element()
    }

    fn render_deerflow_todo_row(
        &self,
        session: Uuid,
        index: usize,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let theme = Theme::current(cx);
        let Some(row) = self
            .deerflow_tasks
            .get(&session)
            .and_then(|browser| browser.runtime_view.todos.as_ref())
            .and_then(|todos| todos.rows.get(index))
        else {
            return div().into_any_element();
        };
        div()
            .h(px(62.0))
            .w_full()
            .py(px(5.0))
            .overflow_hidden()
            .child(
                div()
                    .text_size(sp(11.0))
                    .text_color(row.status.color(theme))
                    .child(row.status.label()),
            )
            .child(
                task_text(row.content.clone(), theme)
                    .text_color(theme.text)
                    .line_clamp(2),
            )
            .into_any_element()
    }

    fn render_deerflow_runtime_agent(
        &self,
        session: Uuid,
        index: usize,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let theme = Theme::current(cx);
        let Some(browser) = self.deerflow_tasks.get(&session) else {
            return div().into_any_element();
        };
        let Some(key) = browser.runtime_view.agent_rows.get(index) else {
            return div().into_any_element();
        };
        let cached = &browser.nodes[key];
        let action = Action::Get {
            root_id: key.0.clone(),
            node_id: key.1.clone(),
        };
        div()
            .h(px(132.0))
            .w_full()
            .py(px(4.0))
            .overflow_hidden()
            .child(
                div()
                    .p(px(8.0))
                    .rounded(px(6.0))
                    .border_1()
                    .border_color(theme.border)
                    .bg(theme.raised)
                    .child(
                        task_button(
                            SharedString::from(format!("df-runtime-agent-{}-{}", key.0, key.1)),
                            cached.title.clone(),
                            browser.pending.is_some() || browser.connection.is_empty(),
                            cx,
                            move |this, _, cx| {
                                this.deerflow_tasks
                                    .get_mut(&session)
                                    .unwrap()
                                    .show_management = true;
                                this.request_deerflow_task(session, action.clone(), cx);
                            },
                        )
                        .w_full()
                        .truncate(),
                    )
                    .child(
                        task_text(cached.runtime_description.clone(), theme)
                            .text_color(theme.text)
                            .line_clamp(2),
                    )
                    .child(task_text(cached.runtime_metadata.clone(), theme).truncate())
                    .child(task_text(cached.progress.clone(), theme).truncate()),
            )
            .into_any_element()
    }
}
