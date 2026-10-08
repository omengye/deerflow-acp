//! Durable Go task state belongs to the attached conversation, not a chat turn.
//! Parsing, tree ordering and display strings are cached when responses arrive.
use super::*;
mod artifacts;
mod receipts;
pub(super) mod runtime_view;
use crate::ui::ActivationExt;
use std::collections::BTreeMap;
use waku_protocol::deerflow_tasks::{
    Action, ApprovalDecision, Artifact, ArtifactPage, BackgroundDetail, CancelScope, Capabilities,
    Command, Decision, Event, Execution, Node, Response,
};

const PAGE_SIZE: u32 = 40;
const ARTIFACT_PAGE_SIZE: u32 = 32;
const MAX_NODES: usize = 2048;
const MAX_CACHE_BYTES: usize = 16 * 1024 * 1024;
type Key = (String, String);

fn bounded(value: &str, max: usize) -> bool {
    !value.is_empty() && value.len() <= max && !value.chars().any(char::is_control)
}
fn digest(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
}
fn version(value: u64) -> bool {
    value <= i64::MAX as u64
}
fn key(node: &Node) -> Key {
    (node.root_id.clone(), node.id.clone())
}
fn background_id(node: &Node) -> Option<&str> {
    if !node.task_id.is_empty() {
        Some(&node.task_id)
    } else {
        node.background_execution
            .as_ref()
            .map(|reference| reference.task_id.as_str())
    }
}
fn background_version(node: &Node) -> u64 {
    if !node.task_id.is_empty() {
        node.version
    } else {
        node.background_execution
            .as_ref()
            .map_or(0, |reference| reference.version)
    }
}
fn status_label(status: &str) -> &str {
    match status {
        "queued" | "pending" => "排队中",
        "cancelling" => "正在取消",
        "running" => "执行中",
        "waiting_input" => "等待继续",
        "suspended" => "已暂停",
        "suspending" => "正在暂停",
        "resuming" => "正在恢复",
        "completed" => "已完成",
        "failed" => "失败",
        "cancelled" | "canceled" => "已取消",
        "needs_reconciliation" => "等待核对工具结果",
        "unavailable" => "历史状态不可用",
        _ => status,
    }
}
fn is_terminal(status: &str) -> bool {
    matches!(
        status,
        "completed" | "failed" | "cancelled" | "canceled" | "unavailable"
    )
}

struct CachedNode {
    bytes: usize,
    node: Node,
    title: SharedString,
    subtitle: SharedString,
    runtime_description: SharedString,
    progress: SharedString,
    runtime_metadata: SharedString,
    started_at: Option<u64>,
    details: SharedString,
    result: SharedString,
}
impl CachedNode {
    fn parse(node: Node, session: &str) -> anyhow::Result<Self> {
        anyhow::ensure!(
            bounded(&node.id, 4096)
                && bounded(&node.root_id, 4096)
                && node.root_session_id == session
                && bounded(&node.role, 256)
                && bounded(&node.status, 64)
                && version(node.version)
                && version(node.attempt)
                && node.depth <= 3
                && node.model.len() <= 256
                && node.description.len() <= 32768
                && node.blocked_reason.len() <= 4096
                && node.allowed_actions.len() <= 16
                && node.allowed_actions.iter().all(|a| bounded(a, 64))
                && matches!(node.mode.as_str(), "root" | "foreground" | "background")
                && node.task_id.len() <= 256
                && node.invocation_id.len() <= 4096
                && node.parent_id.len() <= 4096
                && node.parent_id != node.id
                && node.artifacts.len() <= 128,
            "任务节点身份或状态无效"
        );
        if node.mode == "root" {
            anyhow::ensure!(
                node.id == format!("root/{}", node.root_id)
                    && node.parent_id.is_empty()
                    && node.depth == 0,
                "根任务身份无效"
            );
        } else {
            anyhow::ensure!(
                node.id == node.invocation_id && !node.parent_id.is_empty() && node.depth > 0,
                "子任务身份无效"
            );
        }
        if let Some(execution) = &node.execution {
            anyhow::ensure!(
                bounded(&execution.run_id, 256)
                    && bounded(&execution.status, 64)
                    && execution.version > 0
                    && version(execution.version),
                "任务执行引用无效"
            );
        }
        if let Some(reference) = &node.background_execution {
            anyhow::ensure!(
                bounded(&reference.task_id, 256)
                    && reference.version > 0
                    && version(reference.version)
                    && reference.interaction_id.len() <= 256
                    && bounded(&reference.status, 64),
                "后台执行引用无效"
            );
        }
        for time in [&node.created_at, &node.updated_at].into_iter().flatten() {
            anyhow::ensure!(
                chrono::DateTime::parse_from_rfc3339(time).is_ok(),
                "任务时间无效"
            );
        }
        let mut lines = vec![format!(
            "角色：{}\n模式：{}\n状态：{}\n模型：{}\n尝试 {} · 状态版本 {}",
            node.role,
            match node.mode.as_str() {
                "root" => "根任务",
                "foreground" => "前台",
                _ => "后台",
            },
            status_label(&node.status),
            if node.model.is_empty() {
                "未提供"
            } else {
                &node.model
            },
            node.attempt,
            node.version
        )];
        if !node.description.is_empty() {
            lines.push(node.description.clone());
        }
        let mut wait_label = String::new();
        if let Some(waiting) = &node.waiting {
            anyhow::ensure!(
                bounded(&waiting.kind, 64)
                    && waiting.task_ids.len() <= 128
                    && waiting.task_ids.iter().all(|s| bounded(s, 256))
                    && waiting.intent_count <= 128,
                "等待状态无效"
            );
            wait_label = match waiting.kind.as_str() {
                "permission" => format!("等待 {} 项工具审批", waiting.intent_count),
                "task_wait" => format!("等待 {} 个后代任务", waiting.task_ids.len()),
                "mixed" => "等待审批及后代任务".into(),
                "suspended" => "等待恢复".into(),
                _ => "等待条件尚未满足".into(),
            };
            lines.push(wait_label.clone());
        }
        if !node.blocked_reason.is_empty() {
            lines.push(format!("等待原因：{}", node.blocked_reason));
        }
        if let Some(usage) = &node.usage {
            anyhow::ensure!(
                matches!(usage.scope.as_str(), "root_total" | "invocation")
                    && (usage.scope == "root_total") == (node.mode == "root")
                    && [
                        usage.model_calls,
                        usage.tool_calls,
                        usage.input_tokens,
                        usage.output_tokens,
                        usage.total_tokens,
                        usage.held_tokens
                    ]
                    .into_iter()
                    .all(version),
                "任务用量无效"
            );
            lines.push(format!(
                "{}：模型 {} 次 · 工具 {} 次\n输入 {} / 输出 {} / 总计 {} tokens · 预留 {}{}",
                if usage.scope == "root_total" {
                    "根预算总量（含后代，不与子任务累加）"
                } else {
                    "此角色自身用量（不含后代，不与根预算累加）"
                },
                usage.model_calls,
                usage.tool_calls,
                usage.input_tokens,
                usage.output_tokens,
                usage.total_tokens,
                usage.held_tokens,
                if usage.estimated { "（估算）" } else { "" }
            ));
        } else {
            lines.push("用量：宿主未提供可归属的记录".into());
        }
        for artifact in &node.artifacts {
            anyhow::ensure!(
                bounded(&artifact.id, 1024)
                    && bounded(&artifact.name, 1024)
                    && artifact.mime_type.len() <= 256
                    && version(artifact.size),
                "产物元数据无效"
            );
            lines.push(format!(
                "产物：{} · {} · {} 字节\n标识：{}",
                artifact.name, artifact.mime_type, artifact.size, artifact.id
            ));
        }
        if !node.artifacts.is_empty() {
            lines.push("此处为最多 32 项产物摘要，完整产物请使用下方分页入口。".into());
        }
        let result = if let Some(result) = &node.result {
            anyhow::ensure!(result.text.len() <= 131072, "任务结果超过限制");
            format!(
                "{}{}",
                result.text,
                if result.truncated {
                    "\n（结果已截断）"
                } else {
                    ""
                }
            )
        } else {
            "尚无可展示的最终结果".into()
        };
        lines.push(format!(
            "根任务 {}\n节点 {}{}",
            node.root_id,
            node.id,
            if node.task_id.is_empty() {
                String::new()
            } else {
                format!("\n后台执行 {}", node.task_id)
            }
        ));
        let title = format!("{} · {}", node.role, status_label(&node.status)).into();
        let subtitle = format!(
            "{}{}",
            match node.mode.as_str() {
                "root" => "根任务",
                "foreground" => "前台",
                _ => "后台",
            },
            if wait_label.is_empty() {
                String::new()
            } else {
                format!(" · {wait_label}")
            }
        )
        .into();
        let bytes = serde_json::to_vec(&node)?.len() * 2
            + lines.iter().map(String::len).sum::<usize>()
            + result.len();
        let runtime_description: SharedString = if !node.blocked_reason.is_empty() {
            format!("{} · {}", node.description, node.blocked_reason)
        } else if node.description.is_empty() {
            "宿主未提供任务描述".into()
        } else {
            node.description.clone()
        }
        .into();
        let progress: SharedString = node
            .usage
            .as_ref()
            .map_or_else(
                || "调用进度尚未上报".to_owned(),
                |usage| {
                    format!(
                        "{}模型 {} 次 · 工具 {} 次 · {} tokens{}",
                        if node.mode == "root" {
                            "含子 Agent · "
                        } else {
                            ""
                        },
                        usage.model_calls,
                        usage.tool_calls,
                        usage.total_tokens,
                        if usage.estimated { "（估算）" } else { "" }
                    )
                },
            )
            .into();
        let runtime_metadata: SharedString = format!(
            "{} · {}",
            subtitle,
            if node.model.is_empty() {
                "模型未上报"
            } else {
                &node.model
            }
        )
        .into();
        let started_at = node
            .created_at
            .as_ref()
            .and_then(|time| chrono::DateTime::parse_from_rfc3339(time).ok())
            .and_then(|time| u64::try_from(time.timestamp()).ok());
        let bytes = bytes + runtime_description.len() + progress.len() + runtime_metadata.len();
        Ok(Self {
            bytes,
            node,
            title,
            subtitle,
            runtime_description,
            progress,
            runtime_metadata,
            started_at,
            details: lines.join("\n\n").into(),
            result: result.into(),
        })
    }
}

struct Preview {
    text: SharedString,
    decision: Option<Decision>,
}
pub(super) struct TaskBrowser {
    connection: String,
    session: String,
    capabilities: Capabilities,
    pending: Option<Command>,
    next: Option<Action>,
    nodes: BTreeMap<Key, CachedNode>,
    task_index: HashMap<String, Key>,
    live_count: usize,
    detached_count: usize,
    child_active_count: usize,
    pub(super) runtime_view: runtime_view::RuntimeView,
    show_management: bool,
    uncached_live: bool,
    uncached_detached: bool,
    partial_view: bool,
    refresh_needed: bool,
    rows: Vec<Key>,
    collapsed: HashSet<Key>,
    list: ListState,
    selected: Option<Key>,
    root_filter: Option<String>,
    cursor: String,
    has_more: bool,
    loaded: bool,
    stale: bool,
    seen: HashSet<Key>,
    traversing: bool,
    last_refresh: Instant,
    error: Option<String>,
    detail: Option<BackgroundDetail>,
    execution: Option<Execution>,
    resource_summary: Option<SharedString>,
    previews: HashMap<String, Preview>,
    intent_index: usize,
    confirmation: Option<(String, Action)>,
    receipts: receipts::ReceiptBrowser,
    transfer: Option<artifacts::Transfer>,
    save_selecting: bool,
    artifact_notice: Option<SharedString>,
    saved_artifact: Option<PathBuf>,
    artifact_page: Option<ArtifactPage>,
}
impl Default for TaskBrowser {
    fn default() -> Self {
        Self {
            connection: String::new(),
            session: String::new(),
            capabilities: Capabilities::default(),
            pending: None,
            next: None,
            nodes: BTreeMap::new(),
            task_index: HashMap::new(),
            live_count: 0,
            detached_count: 0,
            child_active_count: 0,
            runtime_view: Default::default(),
            show_management: false,
            uncached_live: false,
            uncached_detached: false,
            partial_view: false,
            refresh_needed: false,
            rows: vec![],
            collapsed: HashSet::new(),
            list: ListState::new(0, ListAlignment::Top, px(200.0)),
            selected: None,
            root_filter: None,
            cursor: String::new(),
            has_more: false,
            loaded: false,
            stale: true,
            seen: HashSet::new(),
            traversing: false,
            last_refresh: Instant::now(),
            error: None,
            detail: None,
            execution: None,
            resource_summary: None,
            previews: HashMap::new(),
            intent_index: 0,
            confirmation: None,
            receipts: Default::default(),
            transfer: None,
            save_selecting: false,
            artifact_notice: None,
            saved_artifact: None,
            artifact_page: None,
        }
    }
}

impl TaskBrowser {
    pub(super) fn sync_runtime(&mut self, session: &AgentSession) -> bool {
        if self.runtime_view.sync(session) {
            self.runtime_view.rebuild_agents(&self.nodes);
            true
        } else {
            false
        }
    }
    pub(super) fn has_live(&self) -> bool {
        self.live_count > 0 || self.uncached_live
    }
    pub(super) fn has_live_detached(&self) -> bool {
        self.detached_count > 0 || self.uncached_detached
    }
    fn disconnect(&mut self) {
        self.receipts.clear();
        self.connection.clear();
        self.pending = None;
        self.next = None;
        self.stale = true;
        self.clear_detail();
        self.transfer = None;
        self.save_selecting = false;
        self.artifact_page = None;
        self.error = Some("连接已断开。任务可能仍在运行，重新连接后刷新。".into());
    }
    fn clear_detail(&mut self) {
        self.detail = None;
        self.execution = None;
        self.resource_summary = None;
        self.previews.clear();
        self.intent_index = 0;
        self.confirmation = None;
    }
    fn drop_cached_nodes(&mut self) {
        // A bounded page window cannot prove evicted work completed. Retain
        // two aggregate facts, cleared only by a complete unfiltered refresh.
        self.uncached_live |= self.live_count > 0;
        self.uncached_detached |= self.detached_count > 0;
        self.nodes.clear();
        self.rows.clear();
        self.seen.clear();
        self.collapsed.clear();
        self.selected = None;
        self.artifact_page = None;
        self.clear_detail();
        self.partial_view = true;
        self.rebuild();
    }
    fn list_action(&self, cursor: Option<String>) -> Action {
        Action::List {
            root_id: self.root_filter.clone(),
            cursor,
            limit: PAGE_SIZE,
        }
    }
    fn runtime_page_action(&self) -> Option<Action> {
        (self.has_more
            && !self.show_management
            && !self.partial_view
            && self.root_filter.is_none()
            && self.error.is_none()
            && self.nodes.len() + PAGE_SIZE as usize <= MAX_NODES)
            .then(|| self.list_action(Some(self.cursor.clone())))
    }
    fn selected(&self) -> Option<&CachedNode> {
        self.nodes.get(self.selected.as_ref()?)
    }
    fn artifacts_for<'a>(&'a self, node: &'a Node) -> &'a [Artifact] {
        self.artifact_page
            .as_ref()
            .filter(|page| page.root_id == node.root_id && page.node_id == node.id)
            .map_or(&node.artifacts, |page| &page.artifacts)
    }
    fn rebuild(&mut self) {
        self.task_index.clear();
        self.live_count = 0;
        self.detached_count = 0;
        self.child_active_count = 0;
        for (key, cached) in &self.nodes {
            if !cached.node.task_id.is_empty() {
                self.task_index
                    .insert(cached.node.task_id.clone(), key.clone());
            }
            if !is_terminal(&cached.node.status) {
                self.live_count += 1;
                if cached.node.mode == "background" {
                    self.detached_count += 1;
                }
                if cached.node.mode != "root" {
                    self.child_active_count += 1;
                }
            }
        }
        fn walk(
            key: &Key,
            nodes: &BTreeMap<Key, CachedNode>,
            children: &BTreeMap<Key, Vec<Key>>,
            collapsed: &HashSet<Key>,
            seen: &mut HashSet<Key>,
            rows: &mut Vec<Key>,
        ) {
            if !seen.insert(key.clone()) {
                return;
            }
            rows.push(key.clone());
            if !collapsed.contains(key) {
                if let Some(items) = children.get(key) {
                    for child in items {
                        if nodes.contains_key(child) {
                            walk(child, nodes, children, collapsed, seen, rows);
                        }
                    }
                }
            }
        }
        let mut children: BTreeMap<Key, Vec<Key>> = BTreeMap::new();
        let mut roots = vec![];
        for (key, cached) in &self.nodes {
            let parent = (cached.node.root_id.clone(), cached.node.parent_id.clone());
            if cached.node.parent_id.is_empty() || !self.nodes.contains_key(&parent) {
                roots.push(key.clone());
            } else {
                children.entry(parent).or_default().push(key.clone());
            }
        }
        let mut seen = HashSet::new();
        let mut rows = vec![];
        for key in roots {
            walk(
                &key,
                &self.nodes,
                &children,
                &self.collapsed,
                &mut seen,
                &mut rows,
            );
        }
        self.rows = rows;
        self.list.reset(self.rows.len());
        self.runtime_view.rebuild_agents(&self.nodes);
    }
    fn begin(&mut self, action: Action) -> Option<Command> {
        let supported = if matches!(
            action,
            Action::ReceiptList { .. } | Action::ReceiptReconcile { .. }
        ) {
            self.capabilities.tool_receipts
        } else {
            self.capabilities.tree
        };
        if self.connection.is_empty() || self.pending.is_some() || !supported {
            return None;
        }
        if self.transfer.is_some() && !matches!(action, Action::ArtifactRead { .. }) {
            return None;
        }
        if matches!(&action,Action::List{cursor,..} if cursor.as_ref().is_none_or(String::is_empty))
        {
            self.seen.clear();
            self.traversing = true;
            self.stale = true;
            self.partial_view = false;
        }
        let command = Command {
            request_id: Uuid::new_v4().to_string(),
            connection_id: self.connection.clone(),
            session_id: self.session.clone(),
            action,
        };
        self.pending = Some(command.clone());
        self.error = None;
        self.confirmation = None;
        self.last_refresh = Instant::now();
        Some(command)
    }
    fn accepts(&self, command: &Command) -> bool {
        self.connection == command.connection_id
            && self.session == command.session_id
            && self
                .pending
                .as_ref()
                .is_some_and(|p| p.request_id == command.request_id && p.action == command.action)
    }
    fn insert(&mut self, node: Node) -> anyhow::Result<()> {
        let node = CachedNode::parse(node, &self.session)?;
        let key = key(&node.node);
        if let Some(parent) = self
            .nodes
            .get(&(node.node.root_id.clone(), node.node.parent_id.clone()))
        {
            anyhow::ensure!(
                parent.node.depth + 1 == node.node.depth,
                "任务层级与父节点不一致"
            );
        }
        anyhow::ensure!(
            self.nodes
                .values()
                .filter(|child| child.node.root_id == node.node.root_id
                    && child.node.parent_id == node.node.id)
                .all(|child| child.node.depth == node.node.depth + 1),
            "后代任务层级不一致"
        );
        if let Some(old) = self.nodes.get(&key) {
            anyhow::ensure!(
                old.node.root_session_id == node.node.root_session_id
                    && old.node.parent_id == node.node.parent_id,
                "任务关系发生变化"
            );
            if old.node.task_id == node.node.task_id && old.node.version > node.node.version {
                return Ok(());
            }
        }
        anyhow::ensure!(
            self.nodes.contains_key(&key) || self.nodes.len() < MAX_NODES,
            "任务缓存已达上限，请选择一个根任务后刷新"
        );
        let retained_bytes: usize = self
            .nodes
            .iter()
            .filter(|(existing, _)| **existing != key)
            .map(|(_, cached)| cached.bytes)
            .sum();
        anyhow::ensure!(
            retained_bytes + node.bytes <= MAX_CACHE_BYTES,
            "任务结果缓存已达上限，请按根任务筛选后刷新"
        );
        self.nodes.insert(key, node);
        Ok(())
    }
    fn accept(&mut self, command: &Command, response: Response) -> anyhow::Result<()> {
        anyhow::ensure!(self.accepts(command), "任务响应已过期");
        match (&command.action, response) {
            (
                Action::ReceiptList {
                    cursor,
                    limit,
                    uncertain_only,
                },
                Response::Receipts(page),
            ) => {
                self.receipts.accept_page(
                    &self.session,
                    cursor.as_deref(),
                    *limit,
                    *uncertain_only,
                    page,
                )?;
            }
            (Action::ReceiptReconcile { review }, Response::Reconciled { receipt }) => {
                self.receipts
                    .accept_review(&self.session, review, receipt)?;
                self.next = Some(receipts::list_action(None, true));
            }
            (
                Action::ArtifactList {
                    root_id,
                    node_id,
                    cursor,
                    limit,
                },
                Response::Artifacts(page),
            ) => {
                anyhow::ensure!(
                    self.selected.as_ref() == Some(&(root_id.clone(), node_id.clone()))
                        && page.root_id == *root_id
                        && page.node_id == *node_id
                        && page.consistency == "live_keyset"
                        && page.artifacts.len() <= *limit as usize
                        && *limit <= ARTIFACT_PAGE_SIZE
                        && page.next_cursor.len() <= 16384
                        && page.has_more == !page.next_cursor.is_empty()
                        && (!page.has_more || page.artifacts.len() == *limit as usize)
                        && (cursor.as_deref().unwrap_or("").is_empty()
                            || self.artifact_page.as_ref().is_some_and(|old| old.root_id
                                == *root_id
                                && old.node_id == *node_id
                                && old.has_more
                                && Some(old.next_cursor.as_str()) == cursor.as_deref())),
                    "产物分页响应与当前节点不一致"
                );
                let mut previous = if cursor.as_deref().unwrap_or("").is_empty() {
                    None
                } else {
                    self.artifact_page
                        .as_ref()
                        .and_then(|old| old.artifacts.last())
                        .map(|artifact| artifact.id.as_str())
                };
                for artifact in &page.artifacts {
                    anyhow::ensure!(
                        bounded(&artifact.id, 1024)
                            && bounded(&artifact.name, 1024)
                            && artifact.mime_type.len() <= 256
                            && version(artifact.size)
                            && previous.is_none_or(|id| id < artifact.id.as_str()),
                        "产物分页元数据无效"
                    );
                    previous = Some(&artifact.id);
                }
                self.artifact_page = Some(page);
            }
            (Action::ArtifactRead { .. }, Response::Artifact(chunk)) => {
                self.transfer
                    .as_ref()
                    .ok_or_else(|| anyhow::anyhow!("产物保存已取消"))?
                    .receive(command, chunk)?;
                self.pending = None;
                return Ok(());
            }
            (
                Action::List {
                    root_id,
                    cursor,
                    limit,
                },
                Response::Page(page),
            ) => {
                anyhow::ensure!(
                    page.session_id == self.session
                        && page.consistency == "live_keyset"
                        && page.nodes.len() <= *limit as usize
                        && page.next_cursor.len() <= 16384
                        && page.has_more == !page.next_cursor.is_empty()
                        && root_id == &self.root_filter
                        && (cursor.as_deref().unwrap_or("").is_empty()
                            || cursor.as_deref() == Some(self.cursor.as_str())),
                    "任务分页响应无效"
                );
                let mut page_keys = HashSet::new();
                for node in &page.nodes {
                    anyhow::ensure!(
                        root_id.as_ref().is_none_or(|root| root == &node.root_id)
                            && page_keys.insert(key(node)),
                        "任务分页来源无效"
                    );
                    CachedNode::parse(node.clone(), &self.session)?;
                }
                for node in page.nodes {
                    self.seen.insert(key(&node));
                    self.insert(node)?;
                }
                self.cursor = page.next_cursor;
                self.has_more = page.has_more;
                self.loaded = true;
                self.stale = false;
                if !self.has_more {
                    self.nodes.retain(|key, _| self.seen.contains(key));
                    self.traversing = false;
                    if self.root_filter.is_none() && !self.partial_view {
                        self.uncached_live = false;
                        self.uncached_detached = false;
                    }
                }
                if self
                    .selected
                    .as_ref()
                    .is_some_and(|key| !self.nodes.contains_key(key))
                {
                    self.selected = None;
                    self.artifact_page = None;
                    self.clear_detail();
                }
                if self.detail.as_ref().is_some_and(|detail| {
                    self.selected().is_none_or(|selected| {
                        background_id(&selected.node) != Some(detail.id.as_str())
                            || background_version(&selected.node) != detail.version
                    })
                }) {
                    self.clear_detail();
                }
                if self
                    .execution
                    .as_ref()
                    .is_some_and(|execution| !self.execution_matches(execution))
                {
                    self.execution = None;
                }
            }
            (Action::Get { root_id, node_id }, Response::Node(node))
            | (
                Action::Cancel {
                    root_id, node_id, ..
                },
                Response::Node(node),
            ) => {
                anyhow::ensure!(
                    node.root_id == *root_id && node.id == *node_id,
                    "节点响应与选择不一致"
                );
                let task_id = background_id(&node).unwrap_or_default().to_owned();
                let execution = node.execution.clone();
                self.insert(node)?;
                self.stale = false;
                if self.selected.as_ref() != Some(&(root_id.clone(), node_id.clone())) {
                    self.artifact_page = None;
                }
                self.selected = Some((root_id.clone(), node_id.clone()));
                self.clear_detail();
                if !task_id.is_empty() {
                    self.next = Some(Action::BackgroundGet { task_id });
                } else if self.capabilities.executions
                    && let Some(execution) = execution
                {
                    self.next = Some(Action::ExecutionGet {
                        run_id: execution.run_id,
                    });
                }
            }
            (Action::ExecutionGet { run_id }, Response::Execution(execution)) => {
                anyhow::ensure!(
                    execution.session_id == self.session
                        && execution.run_id == *run_id
                        && bounded(&execution.run_id, 256)
                        && execution.version > 0
                        && version(execution.version)
                        && bounded(&execution.status, 64)
                        && execution.blocked_reason.len() <= 4096
                        && self.execution_matches(&execution),
                    "前台执行状态与当前任务不一致"
                );
                self.execution = Some(execution);
            }
            (Action::ExecutionResume { .. }, Response::ExecutionFinished) => {
                self.clear_detail();
                self.next = Some(self.list_action(None));
            }
            (Action::BackgroundGet { task_id }, Response::Background(detail))
            | (Action::Approve { task_id, .. }, Response::Background(detail))
            | (Action::Resume { task_id, .. }, Response::Background(detail))
            | (Action::AuthorizeResources { task_id, .. }, Response::Background(detail)) => {
                anyhow::ensure!(
                    detail.id == *task_id
                        && detail.version > 0
                        && version(detail.version)
                        && self
                            .selected()
                            .is_some_and(|n| background_id(&n.node) == Some(task_id.as_str()))
                        && (detail.root_session_id == self.session
                            || (detail.root_session_id.is_empty()
                                && detail.session_id == self.session)),
                    "后台详情与当前任务不一致"
                );
                if matches!(command.action, Action::BackgroundGet { .. })
                    && let Some(reference) = self
                        .selected()
                        .and_then(|selected| selected.node.background_execution.as_ref())
                {
                    anyhow::ensure!(
                        reference.version == detail.version
                            && reference.interaction_id
                                == detail
                                    .interaction
                                    .as_ref()
                                    .map_or("", |interaction| interaction.id.as_str()),
                        "后台执行交互已变化，请刷新节点"
                    );
                }
                if let Some(interaction) = &detail.interaction {
                    anyhow::ensure!(
                        bounded(&interaction.id, 256)
                            && interaction.task_version == detail.version
                            && interaction.inputs().len() <= 128,
                        "任务交互身份无效"
                    );
                    let mut ids = HashSet::new();
                    for intent in interaction.inputs() {
                        anyhow::ensure!(
                            bounded(&intent.id, 256)
                                && ids.insert(&intent.id)
                                && intent.version > 0
                                && version(intent.version)
                                && bounded(&intent.tool_call_id, 256)
                                && bounded(&intent.tool_name, 512)
                                && digest(&intent.arguments_digest)
                                && intent.arguments_summary.len() <= 16384,
                            "任务审批描述无效"
                        );
                    }
                }
                self.clear_detail();
                if let Some(resources) = &detail.resources {
                    anyhow::ensure!(
                        resources.version > 0
                            && version(resources.version)
                            && resources.descriptor.version == 1
                            && digest(&resources.descriptor.config_digest)
                            && digest(&resources.descriptor.schema_digest)
                            && resources.descriptor.servers.len() <= 128
                            && resources.descriptor.tools.len() <= 1024
                            && resources.descriptor.servers.iter().all(|s| bounded(s, 512))
                            && resources.descriptor.tools.iter().all(|s| bounded(s, 512))
                            && resources.reason.len() <= 4096,
                        "任务资源描述无效"
                    );
                    self.resource_summary = Some(
                        format!(
                            "连接资源：{}\n{}\n服务：{}\n工具：{}\n配置摘要 {}\n工具协议摘要 {}",
                            resources.status,
                            resources.reason,
                            resources.descriptor.servers.join(", "),
                            resources.descriptor.tools.join(", "),
                            resources.descriptor.config_digest,
                            resources.descriptor.schema_digest
                        )
                        .into(),
                    );
                }
                self.detail = Some(detail);
                if !matches!(command.action, Action::BackgroundGet { .. }) {
                    if let Some(node) = self.selected() {
                        self.next = Some(Action::Get {
                            root_id: node.node.root_id.clone(),
                            node_id: node.node.id.clone(),
                        });
                    }
                }
            }
            (
                Action::Permission {
                    task_id,
                    interaction_id,
                    task_version,
                    intent_id,
                },
                Response::Permission(data),
            ) => {
                let detail = self
                    .detail
                    .as_ref()
                    .ok_or_else(|| anyhow::anyhow!("审批详情已变化"))?;
                let interaction = detail
                    .interaction
                    .as_ref()
                    .ok_or_else(|| anyhow::anyhow!("审批已结束"))?;
                let intent = interaction
                    .inputs()
                    .iter()
                    .find(|i| i.id == *intent_id)
                    .ok_or_else(|| anyhow::anyhow!("审批项目已变化"))?;
                anyhow::ensure!(
                    detail.id == *task_id
                        && interaction.id == *interaction_id
                        && interaction.task_version == *task_version
                        && data["sessionId"] == detail.child_session_id
                        && !detail.child_session_id.is_empty()
                        && data["toolCallId"] == intent.tool_call_id
                        && data["toolName"] == intent.tool_name
                        && data["configVersion"].as_u64() == Some(intent.config_version)
                        && data["arguments"].is_object()
                        && serde_json::to_vec(&data)?.len() <= 131072,
                    "工具参数预览与当前审批不一致"
                );
                let text = format!(
                    "工具：{}\n调用：{}\n参数摘要：{}\n\n{}",
                    intent.tool_name,
                    intent.tool_call_id,
                    intent.arguments_digest,
                    serde_json::to_string_pretty(&data["arguments"])?
                );
                self.previews.insert(
                    intent_id.clone(),
                    Preview {
                        text: text.into(),
                        decision: None,
                    },
                );
            }
            _ => anyhow::bail!("任务响应类型不一致"),
        }
        self.pending = None;
        self.rebuild();
        Ok(())
    }
    fn approval_action(&self) -> Option<Action> {
        let detail = self.detail.as_ref()?;
        let interaction = detail.interaction.as_ref()?;
        if self.stale
            || detail.status != "waiting_input"
            || interaction.inputs().is_empty()
            || !self.capabilities.permission
            || !self.selected_allows("approve")
        {
            return None;
        }
        let decisions = interaction
            .inputs()
            .iter()
            .map(|intent| {
                Some(ApprovalDecision {
                    intent_id: intent.id.clone(),
                    version: intent.version,
                    decision: self.previews.get(&intent.id)?.decision.clone()?,
                })
            })
            .collect::<Option<Vec<_>>>()?;
        Some(Action::Approve {
            task_id: detail.id.clone(),
            interaction_id: interaction.id.clone(),
            task_version: interaction.task_version,
            decisions,
        })
    }
    fn selected_allows(&self, action: &str) -> bool {
        self.selected().is_some_and(|selected| {
            selected
                .node
                .allowed_actions
                .iter()
                .any(|allowed| allowed == action)
        })
    }
    fn resume_action(&self) -> Option<Action> {
        let detail = self.detail.as_ref()?;
        (!self.stale
            && self.capabilities.resume
            && self.selected_allows("resume")
            && (detail.status == "suspended"
                || (detail.status == "waiting_input"
                    && detail
                        .interaction
                        .as_ref()
                        .is_some_and(|interaction| interaction.kind == "cancelled_roles")))
            && detail
                .interaction
                .as_ref()
                .is_none_or(|interaction| interaction.resumable)
            && detail
                .resources
                .as_ref()
                .is_none_or(|resources| resources.status == "ready"))
        .then(|| Action::Resume {
            task_id: detail.id.clone(),
            version: detail.version,
        })
    }
    fn execution_resume_action(&self) -> Option<Action> {
        let execution = self.execution.as_ref()?;
        (!self.stale
            && self.capabilities.executions
            && execution.status == "waiting_input"
            && execution.resumable
            && self.execution_matches(execution))
        .then(|| Action::ExecutionResume {
            run_id: execution.run_id.clone(),
            expected_version: execution.version,
        })
    }
    fn execution_matches(&self, execution: &Execution) -> bool {
        self.selected().is_some_and(|selected| {
            selected.node.task_id.is_empty()
                && selected.node.execution.as_ref().is_some_and(|reference| {
                    reference.run_id == execution.run_id && reference.version == execution.version
                })
        })
    }
}

impl Waku {
    fn request_deerflow_task(&mut self, session: Uuid, action: Action, cx: &mut Context<Self>) {
        let Some(driver) = self
            .runtimes
            .get(&session)
            .map(|runtime| runtime.driver.clone())
        else {
            return;
        };
        let Some(command) = self
            .deerflow_tasks
            .entry(session)
            .or_default()
            .begin(action)
        else {
            return;
        };
        cx.background_executor()
            .spawn(async move {
                driver.deerflow_tasks(command);
            })
            .detach();
        cx.notify();
    }
    pub(super) fn handle_deerflow_task_event(&mut self, session: Uuid, event: Event) {
        let browser = self.deerflow_tasks.entry(session).or_default();
        match event {
            Event::Invalidated {
                connection_id,
                session_id,
            } => {
                if browser.connection == connection_id && browser.session == session_id {
                    browser.refresh_needed = true;
                }
            }
            Event::Ready {
                connection_id,
                session_id,
                capabilities,
            } => {
                if !bounded(&connection_id, 128) || !bounded(&session_id, 256) {
                    return;
                }
                if browser.connection != connection_id {
                    browser.receipts.clear();
                    browser.transfer = None;
                    browser.save_selecting = false;
                    browser.artifact_page = None;
                    if browser.session != session_id {
                        browser.nodes.clear();
                        browser.rows.clear();
                        browser.selected = None;
                        browser.root_filter = None;
                        browser.uncached_live = false;
                        browser.uncached_detached = false;
                        browser.partial_view = false;
                        browser.rebuild();
                    }
                    browser.pending = None;
                    browser.clear_detail();
                    browser.stale = true;
                    browser.connection = connection_id;
                    browser.session = session_id;
                    browser.capabilities = capabilities;
                    if browser.capabilities.tree {
                        browser.next = Some(browser.list_action(None));
                    }
                }
            }
            Event::Reply { command, response } => {
                if !browser.accepts(&command) {
                    return;
                }
                if let Err(error) = browser.accept(&command, response) {
                    if let Some(transfer) = &browser.transfer {
                        transfer.fail(error.to_string());
                    }
                    browser.pending = None;
                    browser.stale = true;
                    browser.clear_detail();
                    browser.error = Some(error.to_string());
                }
            }
            Event::Failed {
                command,
                code,
                message,
            } => {
                if !browser.accepts(&command) {
                    return;
                }
                browser.pending = None;
                browser.next = None;
                if let Some(transfer) = &browser.transfer {
                    transfer.fail(message.clone());
                }
                browser.stale = true;
                browser.clear_detail();
                if matches!(command.action, Action::ReceiptReconcile { .. }) {
                    browser.receipts.clear();
                    browser.next = Some(receipts::list_action(None, true));
                }
                browser.error = Some(format!(
                    "{}（{}）",
                    message.chars().take(4000).collect::<String>(),
                    code
                ));
            }
        }
    }
    pub(super) fn disconnect_deerflow_tasks(&mut self, session: Uuid) {
        if let Some(browser) = self.deerflow_tasks.get_mut(&session) {
            browser.disconnect();
        }
    }
    pub(super) fn maybe_refresh_deerflow_tasks(&mut self, cx: &mut Context<Self>) {
        if let Some(session) = self.state.sessions.iter().find(|s| {
            Some(s.id) == self.state.selected_session && s.provider == ProviderKind::DeerFlow
        }) {
            let browser = self.deerflow_tasks.entry(session.id).or_default();
            if browser.sync_runtime(session) {
                browser.refresh_needed |=
                    browser.capabilities.tree && !browser.connection.is_empty();
                cx.notify();
            }
        }
        let selected = self.state.selected_session;
        let visible = self.right_panel_visible
            && matches!(
                self.active_right_panel_surface(),
                Some(RightPanelSurface::DeerFlowTasks)
            );
        let mut requests = vec![];
        for (session, browser) in &mut self.deerflow_tasks {
            if browser.pending.is_some()
                || browser.connection.is_empty()
                || browser.transfer.is_some()
            {
                continue;
            }
            // Review forms need a pinned version while they are visible.
            // Returning to the overview must resume live status refresh.
            let reviewing = browser.show_management
                && (browser.receipts.review.is_some()
                    || browser.confirmation.is_some()
                    || !browser.previews.is_empty());
            if let Some(action) = browser.next.take() {
                requests.push((*session, action));
            } else if browser.refresh_needed && !reviewing {
                browser.refresh_needed = false;
                requests.push((*session, browser.list_action(None)));
            } else if selected == Some(*session)
                && visible
                && browser.runtime_page_action().is_some()
                && !reviewing
            {
                // Only the visible runtime overview follows historical pages
                // automatically, until the bounded cache is full.
                requests.push((*session, browser.runtime_page_action().unwrap()));
            } else if selected == Some(*session)
                && visible
                && browser.capabilities.tree
                && !reviewing
                && browser.last_refresh.elapsed() >= Duration::from_secs(5)
                && !browser.has_more
                && !browser.partial_view
            {
                requests.push((*session, browser.list_action(None)));
            }
        }
        for (session, action) in requests {
            self.request_deerflow_task(session, action, cx);
        }
    }
    fn open_deerflow_tasks(&mut self, cx: &mut Context<Self>) {
        let Some(session) = self
            .state
            .sessions
            .iter()
            .find(|s| Some(s.id) == self.state.selected_session)
            .filter(|s| s.provider == ProviderKind::DeerFlow)
        else {
            return;
        };
        let id = session.id;
        let has_cursor = session.provider_cursor.is_some();
        self.deerflow_tasks
            .entry(id)
            .or_default()
            .sync_runtime(session);
        self.open_right_panel_surface(RightPanelSurface::DeerFlowTasks, cx);
        if let Some(browser) = self.deerflow_tasks.get(&id) {
            if browser.pending.is_none()
                && browser.capabilities.tree
                && !browser.connection.is_empty()
            {
                let action = browser.list_action(None);
                self.request_deerflow_task(id, action, cx);
                return;
            }
        }
        if has_cursor && !self.runtimes.contains_key(&id) {
            self.start_goal_runtime(id, cx);
        }
    }
    pub(super) fn render_deerflow_task_chip(&self, cx: &mut Context<Self>) -> Option<AnyElement> {
        let session = self
            .selected_session()
            .filter(|s| s.provider == ProviderKind::DeerFlow)?;
        let count = self
            .deerflow_tasks
            .get(&session.id)
            .map(|b| b.child_active_count)
            .unwrap_or(0);
        Some(
            task_button(
                "deerflow-task-tree-chip",
                format!("运行状态 · {count} 个子 Agent"),
                false,
                cx,
                |this, _, cx| this.open_deerflow_tasks(cx),
            )
            .into_any_element(),
        )
    }
    pub(super) fn render_deerflow_runtime(&self, cx: &mut Context<Self>) -> AnyElement {
        let management = self
            .state
            .selected_session
            .and_then(|session| self.deerflow_tasks.get(&session))
            .is_some_and(|browser| browser.show_management);
        if management {
            self.render_deerflow_task_tree(cx)
        } else {
            self.render_deerflow_runtime_view(cx)
        }
    }
    fn render_deerflow_task_tree(&self, cx: &mut Context<Self>) -> AnyElement {
        let theme = Theme::current(cx);
        let Some(session) = self
            .selected_session()
            .filter(|s| s.provider == ProviderKind::DeerFlow)
            .map(|s| s.id)
        else {
            return task_text("请选择 DeerFlow 会话", theme).into_any_element();
        };
        let mut panel = div()
            .id("deerflow-task-tree")
            .tab_group()
            .size_full()
            .flex()
            .flex_col()
            .gap(px(8.0))
            .p(px(12.0))
            .overflow_hidden()
            .child(task_button(
                "df-runtime-back",
                "返回运行状态",
                false,
                cx,
                move |this, _, cx| {
                    this.deerflow_tasks
                        .entry(session)
                        .or_default()
                        .show_management = false;
                    cx.notify();
                },
            ));
        let Some(browser) = self.deerflow_tasks.get(&session) else {
            return panel
                .child(task_text(
                    "已有会话可以重连读取后台任务。新会话发送第一条消息后显示任务树。",
                    theme,
                ))
                .child(task_button(
                    "deerflow-task-connect",
                    "连接并读取任务",
                    self.goal_runtime_starts.contains(&session)
                        || self
                            .selected_session()
                            .is_none_or(|session| session.provider_cursor.is_none()),
                    cx,
                    |this, _, cx| this.open_deerflow_tasks(cx),
                ))
                .into_any_element();
        };
        let busy =
            browser.pending.is_some() || browser.transfer.is_some() || browser.save_selecting;
        let unavailable = busy || browser.connection.is_empty() || !browser.capabilities.tree;
        let action = browser.list_action(None);
        panel = panel.child(
            div()
                .flex()
                .flex_wrap()
                .gap(px(6.0))
                .child(task_button(
                    "df-task-refresh",
                    "刷新任务树",
                    unavailable,
                    cx,
                    move |this, _, cx| this.request_deerflow_task(session, action.clone(), cx),
                ))
                .child(task_button(
                    "df-task-reconnect",
                    "重新连接",
                    !browser.connection.is_empty() || self.goal_runtime_starts.contains(&session),
                    cx,
                    |this, _, cx| this.open_deerflow_tasks(cx),
                )),
        );
        if !browser.capabilities.tree && !browser.connection.is_empty() {
            panel = panel.child(task_text("此服务尚未提供完整任务树协议。", theme));
        }
        if let Some(error) = &browser.error {
            panel = panel.child(task_text(error.clone(), theme));
        }
        if browser.stale {
            panel = panel.child(task_text(
                "显示的是已缓存状态；刷新后才能执行控制操作。",
                theme,
            ));
        }
        if browser.has_more {
            let action = browser.list_action(Some(browser.cursor.clone()));
            panel = panel
                .child(task_button(
                    "df-task-more",
                    "继续加载任务",
                    unavailable || browser.nodes.len() + PAGE_SIZE as usize > MAX_NODES,
                    cx,
                    move |this, _, cx| this.request_deerflow_task(session, action.clone(), cx),
                ))
                .child(task_button(
                    "df-task-next-window",
                    "清空已读缓存并继续下一页",
                    unavailable,
                    cx,
                    move |this, _, cx| {
                        let browser = this.deerflow_tasks.get_mut(&session).unwrap();
                        let action = browser.list_action(Some(browser.cursor.clone()));
                        browser.drop_cached_nodes();
                        this.request_deerflow_task(session, action, cx);
                    },
                ))
                .child(task_text("列表尚未加载完整，父节点可能位于后续页。", theme));
        }
        if browser.partial_view {
            panel = panel.child(task_text("当前是局部分页视图，已读节点已移出缓存。任务控制仍由宿主确定完整范围；未再看到的运行任务尚不能判定已结束。刷新可从第一页重新读取。", theme));
        }
        if browser.root_filter.is_some() {
            panel = panel.child(task_button(
                "df-task-all-roots",
                "查看所有根任务",
                unavailable,
                cx,
                move |this, _, cx| {
                    let b = this.deerflow_tasks.get_mut(&session).unwrap();
                    b.root_filter = None;
                    b.drop_cached_nodes();
                    let action = b.list_action(None);
                    this.request_deerflow_task(session, action, cx);
                },
            ));
        }
        if browser.loaded && browser.nodes.is_empty() {
            panel = panel.child(task_text("当前会话尚无子代理任务记录。", theme));
        }
        let weak = cx.entity().downgrade();
        panel = panel.child(
            div()
                .h(px(235.0))
                .flex_none()
                .border_1()
                .border_color(theme.border)
                .rounded(px(6.0))
                .overflow_hidden()
                .child(
                    list(browser.list.clone(), move |index, _, cx| {
                        weak.update(cx, |this, cx| {
                            this.render_deerflow_task_row(session, index, cx)
                        })
                        .unwrap_or_else(|_| div().into_any_element())
                    })
                    .size_full(),
                ),
        );
        let mut detail = div()
            .id("deerflow-task-detail")
            .flex_1()
            .min_h_0()
            .overflow_y_scroll()
            .flex()
            .flex_col()
            .gap(px(8.0));
        detail = detail.child(self.render_deerflow_receipts(session, browser, cx));
        if let Some(cached) = browser.selected() {
            let node = &cached.node;
            let get = Action::Get {
                root_id: node.root_id.clone(),
                node_id: node.id.clone(),
            };
            let root = node.root_id.clone();
            detail = detail
                .child(task_text(cached.details.clone(), theme))
                .child(task_button(
                    "df-task-node-refresh",
                    "刷新所选任务详情",
                    unavailable,
                    cx,
                    move |this, _, cx| this.request_deerflow_task(session, get.clone(), cx),
                ))
                .child(task_button(
                    "df-task-filter-root",
                    "只查看这棵任务树",
                    unavailable,
                    cx,
                    move |this, _, cx| {
                        let b = this.deerflow_tasks.get_mut(&session).unwrap();
                        b.root_filter = Some(root.clone());
                        b.drop_cached_nodes();
                        let action = b.list_action(None);
                        this.request_deerflow_task(session, action, cx);
                    },
                ));
            if node.version > 0
                && node.allowed_actions.iter().any(|a| a == "cancel_subtree")
                && browser.capabilities.cancel
            {
                let action = Action::Cancel {
                    root_id: node.root_id.clone(),
                    node_id: node.id.clone(),
                    scope: if node.mode == "root" {
                        CancelScope::Root
                    } else {
                        CancelScope::Subtree
                    },
                    expected_version: node.version,
                };
                let label = if node.mode == "root" {
                    "取消整棵任务树…"
                } else {
                    "取消所选任务及后代…"
                };
                detail = detail.child(task_button(
                    "df-task-cancel",
                    label,
                    unavailable || browser.stale,
                    cx,
                    move |this, _, cx| {
                        this.deerflow_tasks.get_mut(&session).unwrap().confirmation = Some((
                            "停止所选任务及其全部后代。已产生的结果和用量仍保留。".into(),
                            action.clone(),
                        ));
                        cx.notify();
                    },
                ));
            }
            if node.mode != "root" {
                if let Some(root) = browser
                    .nodes
                    .get(&(node.root_id.clone(), format!("root/{}", node.root_id)))
                {
                    if root.node.version > 0
                        && root
                            .node
                            .allowed_actions
                            .iter()
                            .any(|a| a == "cancel_subtree")
                    {
                        let action = Action::Cancel {
                            root_id: root.node.root_id.clone(),
                            node_id: root.node.id.clone(),
                            scope: CancelScope::Root,
                            expected_version: root.node.version,
                        };
                        detail = detail.child(task_button(
                            "df-task-cancel-root",
                            "取消根任务整棵树…",
                            unavailable || browser.stale || !browser.capabilities.cancel,
                            cx,
                            move |this, _, cx| {
                                this.deerflow_tasks.get_mut(&session).unwrap().confirmation =
                                    Some((
                                        "停止根任务及这棵树内所有前台、后台角色。".into(),
                                        action.clone(),
                                    ));
                                cx.notify();
                            },
                        ));
                    }
                }
            }
            if let Some(waiting) = &node.waiting {
                for task in waiting.task_ids.iter().take(32) {
                    let target = browser
                        .task_index
                        .get(task)
                        .and_then(|key| browser.nodes.get(key));
                    if let Some(target) = target {
                        let action = Action::Get {
                            root_id: target.node.root_id.clone(),
                            node_id: target.node.id.clone(),
                        };
                        detail = detail.child(task_button(
                            SharedString::from(format!("df-task-dependency-{task}")),
                            format!("查看等待的角色 {}", target.node.role),
                            unavailable,
                            cx,
                            move |this, _, cx| {
                                this.request_deerflow_task(session, action.clone(), cx)
                            },
                        ));
                    }
                }
            }
            if background_id(node).is_none()
                && node.waiting.as_ref().is_some_and(|w| w.intent_count > 0)
            {
                detail = detail
                    .child(task_text(
                        "此角色的审批由当前对话处理，请查看对话输入区的待审批工具。",
                        theme,
                    ))
                    .child(task_button(
                        "df-task-chat-approval",
                        "返回对话审批区",
                        false,
                        cx,
                        |this, window, cx| {
                            this.set_right_panel_visible(false, cx);
                            this.focus_composer_action(&FocusComposer, window, cx);
                        },
                    ));
            }
            detail = detail
                .child(task_text("最终结果", theme))
                .child(task_text(cached.result.clone(), theme));
            if let Some(result) = &node.result {
                let text = result.text.clone();
                detail = detail.child(task_button(
                    "df-task-copy-result",
                    "复制最终结果",
                    false,
                    cx,
                    move |_, _, cx| cx.write_to_clipboard(ClipboardItem::new_string(text.clone())),
                ));
            }
            if browser.capabilities.artifact_list {
                let first = Action::ArtifactList {
                    root_id: node.root_id.clone(),
                    node_id: node.id.clone(),
                    cursor: None,
                    limit: ARTIFACT_PAGE_SIZE,
                };
                detail = detail
                    .child(task_text(
                        "产物按每页 32 项显示；有新产物时从首页刷新。",
                        theme,
                    ))
                    .child(task_button(
                        "df-task-artifact-first",
                        "查看全部产物 / 刷新首页",
                        unavailable || browser.stale,
                        cx,
                        move |this, _, cx| this.request_deerflow_task(session, first.clone(), cx),
                    ));
                if let Some(page) = browser
                    .artifact_page
                    .as_ref()
                    .filter(|page| page.root_id == node.root_id && page.node_id == node.id)
                {
                    detail = detail.child(task_text(
                        format!(
                            "本页 {} 项{}",
                            page.artifacts.len(),
                            if page.has_more {
                                "，还有更多"
                            } else {
                                "，已到末页"
                            }
                        ),
                        theme,
                    ));
                    let next = Action::ArtifactList {
                        root_id: node.root_id.clone(),
                        node_id: node.id.clone(),
                        cursor: Some(page.next_cursor.clone()),
                        limit: ARTIFACT_PAGE_SIZE,
                    };
                    detail = detail.child(task_button(
                        "df-task-artifact-next",
                        "下一页产物",
                        unavailable || browser.stale || !page.has_more,
                        cx,
                        move |this, _, cx| this.request_deerflow_task(session, next.clone(), cx),
                    ));
                }
            }
            for artifact in browser.artifacts_for(node) {
                let root = node.root_id.clone();
                let node_id = node.id.clone();
                let artifact = artifact.clone();
                detail = detail.child(task_button(
                    SharedString::from(format!("df-task-save-artifact-{}", artifact.id)),
                    format!("另存为 {}（{} 字节）", artifact.name, artifact.size),
                    unavailable
                        || browser.stale
                        || !browser.capabilities.artifact_read
                        || artifact.size > 200 * 1024 * 1024,
                    cx,
                    move |this, _, cx| {
                        this.save_deerflow_task_artifact(
                            session,
                            root.clone(),
                            node_id.clone(),
                            artifact.clone(),
                            cx,
                        )
                    },
                ));
            }
        } else {
            detail = detail.child(task_text(
                "选择一个节点查看用量、结果、等待原因和控制操作。",
                theme,
            ));
        }
        detail = detail.child(self.render_deerflow_task_controls(
            session,
            browser,
            unavailable,
            theme,
            cx,
        ));
        panel.child(detail).into_any_element()
    }
    fn render_deerflow_task_row(
        &self,
        session: Uuid,
        index: usize,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let theme = Theme::current(cx);
        let Some(browser) = self.deerflow_tasks.get(&session) else {
            return div().into_any_element();
        };
        let Some(key) = browser.rows.get(index) else {
            return div().into_any_element();
        };
        let cached = &browser.nodes[key];
        let action = Action::Get {
            root_id: cached.node.root_id.clone(),
            node_id: cached.node.id.clone(),
        };
        let collapse_key = key.clone();
        let selected = browser.selected.as_ref() == Some(key);
        let id = SharedString::from(format!("df-tree-row-{}-{}", key.0, key.1));
        div()
            .h(px(62.0))
            .w_full()
            .flex()
            .items_center()
            .gap(px(5.0))
            .pl(px(6.0 + cached.node.depth as f32 * 13.0))
            .pr(px(6.0))
            .bg(if selected {
                theme.overlay
            } else {
                theme.surface
            })
            .child(task_button(
                SharedString::from(format!("df-tree-expand-{}-{}", key.0, key.1)),
                if browser.collapsed.contains(key) {
                    "+"
                } else {
                    "−"
                },
                false,
                cx,
                move |this, _, cx| {
                    let b = this.deerflow_tasks.get_mut(&session).unwrap();
                    if !b.collapsed.remove(&collapse_key) {
                        b.collapsed.insert(collapse_key.clone());
                    }
                    b.rebuild();
                    cx.notify();
                },
            ))
            .child(
                div()
                    .min_w_0()
                    .flex_1()
                    .child(task_button(
                        id,
                        cached.title.clone(),
                        browser.pending.is_some(),
                        cx,
                        move |this, _, cx| this.request_deerflow_task(session, action.clone(), cx),
                    ))
                    .child(
                        div()
                            .text_size(sp(11.0))
                            .text_color(theme.text_secondary)
                            .truncate()
                            .child(cached.subtitle.clone()),
                    ),
            )
            .into_any_element()
    }
    fn render_deerflow_task_controls(
        &self,
        session: Uuid,
        browser: &TaskBrowser,
        unavailable: bool,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let mut group = div().flex().flex_col().gap(px(6.0));
        if let Some(notice) = &browser.artifact_notice {
            group = group.child(task_text(notice.clone(), theme));
        }
        if browser.transfer.is_some() {
            group = group.child(task_button(
                "df-task-save-cancel",
                "取消产物保存",
                false,
                cx,
                move |this, _, cx| {
                    let browser = this.deerflow_tasks.get_mut(&session).unwrap();
                    if let Some(transfer) = &browser.transfer {
                        transfer.cancel();
                    }
                    browser.artifact_notice = Some("正在取消产物保存…".into());
                    cx.notify();
                },
            ));
        }
        if let Some(path) = &browser.saved_artifact {
            let path = path.clone();
            group = group.child(task_button(
                "df-task-save-reveal",
                "在文件管理器中显示已保存产物",
                false,
                cx,
                move |_, _, cx| crate::platform::reveal_in_file_manager(&path, cx),
            ));
        }
        if let Some(detail) = &browser.detail {
            if let Some(summary) = &browser.resource_summary {
                group = group.child(task_text(summary.clone(), theme));
            }
            if let Some(resources) = &detail.resources {
                if resources.can_authorize && browser.capabilities.resource_authorize {
                    let action = Action::AuthorizeResources {
                        task_id: detail.id.clone(),
                        task_version: detail.version,
                        resource_version: resources.version,
                    };
                    group=group.child(task_button("df-task-authorize-resources","重新授权当前连接资源…",unavailable||browser.stale,cx,move|this,_,cx|{this.deerflow_tasks.get_mut(&session).unwrap().confirmation=Some(("将上方所列 MCP 服务和工具重新授权给此任务。授权不会重跑已完成任务，后续执行仍受宿主调度和工具审批控制。".into(),action.clone()));cx.notify();}));
                }
            }
            if let Some(action) = browser.resume_action() {
                group = group.child(task_button(
                    "df-task-resume",
                    "恢复暂停任务…",
                    unavailable || browser.stale,
                    cx,
                    move |this, _, cx| {
                        this.deerflow_tasks.get_mut(&session).unwrap().confirmation =
                            Some(("从宿主保存的检查点继续此任务。".into(), action.clone()));
                        cx.notify();
                    },
                ));
            }
            if let Some(interaction) = &detail.interaction
                && browser.selected_allows("approve")
            {
                if let Some(intent) = interaction.inputs().get(browser.intent_index) {
                    group = group.child(task_text(
                        format!(
                            "工具审批 {}/{} · {}",
                            browser.intent_index + 1,
                            interaction.inputs().len(),
                            intent.tool_name
                        ),
                        theme,
                    ));
                    let action = Action::Permission {
                        task_id: detail.id.clone(),
                        interaction_id: interaction.id.clone(),
                        task_version: interaction.task_version,
                        intent_id: intent.id.clone(),
                    };
                    group = group.child(task_button(
                        "df-task-permission-preview",
                        "读取完整工具参数",
                        unavailable || browser.stale || !browser.capabilities.permission,
                        cx,
                        move |this, _, cx| this.request_deerflow_task(session, action.clone(), cx),
                    ));
                    if let Some(preview) = browser.previews.get(&intent.id) {
                        group = group.child(task_text(preview.text.clone(), theme));
                        for (label, decision) in [
                            ("允许此次调用", Decision::AllowOnce),
                            ("拒绝此次调用", Decision::RejectOnce),
                        ] {
                            let intent_id = intent.id.clone();
                            let selected = preview.decision.as_ref() == Some(&decision);
                            group = group.child(task_button(
                                SharedString::from(format!("df-task-decision-{label}")),
                                format!("{}{}", if selected { "✓ " } else { "" }, label),
                                unavailable || browser.stale,
                                cx,
                                move |this, _, cx| {
                                    if let Some(p) = this
                                        .deerflow_tasks
                                        .get_mut(&session)
                                        .and_then(|b| b.previews.get_mut(&intent_id))
                                    {
                                        p.decision = Some(decision.clone());
                                        cx.notify();
                                    }
                                },
                            ));
                        }
                    }
                    for (label, index) in [
                        ("上一项", browser.intent_index.checked_sub(1)),
                        (
                            "下一项",
                            (browser.intent_index + 1 < interaction.inputs().len())
                                .then_some(browser.intent_index + 1),
                        ),
                    ] {
                        group = group.child(task_button(
                            SharedString::from(format!("df-task-intent-{label}")),
                            label,
                            unavailable || index.is_none(),
                            cx,
                            move |this, _, cx| {
                                if let Some(index) = index {
                                    this.deerflow_tasks.get_mut(&session).unwrap().intent_index =
                                        index;
                                    cx.notify();
                                }
                            },
                        ));
                    }
                    let action = browser.approval_action();
                    group = group.child(task_button(
                        "df-task-submit-approval",
                        "提交已核对的全部工具决定",
                        unavailable || action.is_none(),
                        cx,
                        move |this, _, cx| {
                            if let Some(action) = &action {
                                this.request_deerflow_task(session, action.clone(), cx);
                            }
                        },
                    ));
                }
            }
        }
        if let Some(detail) = &browser.detail
            && detail
                .interaction
                .as_ref()
                .is_some_and(|interaction| !interaction.inputs().is_empty())
            && !browser.selected_allows("approve")
        {
            group = group.child(task_text(
                "剩余审批属于后台执行中的其他角色，请打开所属后台任务处理。",
                theme,
            ));
            if let Some(target) = browser
                .task_index
                .get(&detail.id)
                .and_then(|key| browser.nodes.get(key))
            {
                let action = Action::Get {
                    root_id: target.node.root_id.clone(),
                    node_id: target.node.id.clone(),
                };
                group = group.child(task_button(
                    "df-task-parent-background-approval",
                    "打开所属后台任务",
                    unavailable,
                    cx,
                    move |this, _, cx| this.request_deerflow_task(session, action.clone(), cx),
                ));
            }
        }
        if let Some(execution) = &browser.execution {
            group = group.child(task_text(
                format!(
                    "前台执行：{} · 版本 {}\n{}",
                    status_label(&execution.status),
                    execution.version,
                    execution.blocked_reason
                ),
                theme,
            ));
            if let Some(action) = browser.execution_resume_action() {
                group = group.child(task_button("df-task-execution-resume", "恢复执行 / 重新显示审批…", unavailable, cx, move |this, _, cx| {
                    this.deerflow_tasks.get_mut(&session).unwrap().confirmation = Some(("从已保存的前台执行继续；若有待审批工具，将在对话区重新显示原始工具请求，由你选择允许或拒绝。".into(), action.clone()));
                    cx.notify();
                }));
            }
        }
        if let Some((message, action)) = &browser.confirmation {
            let action = action.clone();
            group = group
                .child(task_text(message.clone(), theme))
                .child(task_button(
                    "df-task-confirm",
                    "确认执行",
                    unavailable || browser.stale,
                    cx,
                    move |this, _, cx| this.request_deerflow_task(session, action.clone(), cx),
                ))
                .child(task_button(
                    "df-task-abandon",
                    "返回",
                    unavailable,
                    cx,
                    move |this, _, cx| {
                        this.deerflow_tasks.get_mut(&session).unwrap().confirmation = None;
                        cx.notify();
                    },
                ));
        }
        group.into_any_element()
    }
}

fn task_text(text: impl Into<SharedString>, theme: Theme) -> Div {
    div()
        .text_size(sp(12.5))
        .line_height(sp(18.0))
        .text_color(theme.text_secondary)
        .child(text.into())
}
fn task_button(
    id: impl Into<gpui::ElementId>,
    label: impl Into<SharedString>,
    disabled: bool,
    cx: &mut Context<Waku>,
    action: impl Fn(&mut Waku, &mut Window, &mut Context<Waku>) + 'static,
) -> Stateful<Div> {
    let theme = Theme::current(cx);
    let button = div()
        .id(id)
        .tab_index(0)
        .flex_none()
        .min_h(px(29.0))
        .px(px(8.0))
        .py(px(4.0))
        .rounded(px(6.0))
        .border_1()
        .border_color(theme.border)
        .bg(theme.raised)
        .text_size(sp(12.0))
        .text_color(theme.text)
        .focus_visible(|s| s.border_color(theme.accent))
        .when(disabled, |s| s.opacity(0.5))
        .child(label.into());
    if disabled {
        button
    } else {
        button.on_activation(cx, action)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{Value, json};
    use waku_protocol::deerflow_tasks::Page;

    fn node(id: &str, parent: &str, mode: &str, depth: u32) -> Node {
        serde_json::from_value(json!({"id":id,"rootId":"run","rootSessionId":"session","parentId":parent,
            "invocationId":if mode=="root"{""}else{id},"taskId":if mode=="background"{"task"}else{""},"role":if mode=="root"{"lead"}else{"researcher"},"mode":mode,"depth":depth,
            "status":"running","version":4,"attempt":1,"allowedActions":["cancel_subtree"]})).unwrap()
    }
    fn browser() -> TaskBrowser {
        TaskBrowser {
            connection: "connection".into(),
            session: "session".into(),
            capabilities: Capabilities {
                tree: true,
                cancel: true,
                permission: true,
                resume: true,
                resource_authorize: true,
                executions: true,
                artifact_read: true,
                ..Default::default()
            },
            ..Default::default()
        }
    }
    fn page(nodes: Vec<Node>, cursor: &str) -> Response {
        Response::Page(Page {
            session_id: "session".into(),
            nodes,
            next_cursor: cursor.into(),
            has_more: !cursor.is_empty(),
            consistency: "live_keyset".into(),
        })
    }
    fn load(browser: &mut TaskBrowser, nodes: Vec<Node>) {
        let command = browser.begin(browser.list_action(None)).unwrap();
        browser.accept(&command, page(nodes, "")).unwrap();
    }
    fn background() -> BackgroundDetail {
        serde_json::from_value(json!({"id":"task","sessionId":"session","rootSessionId":"session","childSessionId":"child","status":"waiting_input","version":4,
        "interaction":{"id":"batch","taskVersion":4,"kind":"permission","resumable":false,"waitingInputs":[{"id":"intent","kind":"tool_permission","version":1,"toolCallId":"call","toolName":"write_file","argumentsDigest":"a".repeat(64),"argumentsSummary":"path/content","configVersion":2}]}})).unwrap()
    }
    fn approval_browser() -> TaskBrowser {
        let mut b = browser();
        load(
            &mut b,
            vec![
                node("root/run", "", "root", 0),
                node("worker", "root/run", "background", 1),
            ],
        );
        b.selected = Some(("run".into(), "worker".into()));
        b.nodes
            .get_mut(b.selected.as_ref().unwrap())
            .unwrap()
            .node
            .allowed_actions
            .push("approve".into());
        let command = b
            .begin(Action::BackgroundGet {
                task_id: "task".into(),
            })
            .unwrap();
        b.accept(&command, Response::Background(background()))
            .unwrap();
        b
    }

    #[test]
    fn deerflow_tree_keeps_mixed_parentage_and_collapses_descendants_without_losing_records() {
        let mut b = browser();
        load(
            &mut b,
            vec![
                node("background", "foreground", "background", 2),
                node("foreground", "root/run", "foreground", 1),
                node("root/run", "", "root", 0),
            ],
        );
        assert_eq!(
            b.rows.iter().map(|(_, id)| id.as_str()).collect::<Vec<_>>(),
            ["root/run", "foreground", "background"]
        );
        b.collapsed.insert(("run".into(), "foreground".into()));
        b.rebuild();
        assert_eq!(b.rows.len(), 2);
        assert_eq!(b.nodes.len(), 3);
        assert!(b.has_live_detached());
        b.nodes
            .get_mut(&("run".into(), "root/run".into()))
            .unwrap()
            .node
            .status = "completed".into();
        assert!(b.has_live_detached());
        b.disconnect();
        assert!(b.has_live_detached());
        assert!(b.stale);
        assert!(b.pending.is_none());
    }
    #[test]
    fn runtime_overview_follows_pages_while_management_and_failed_pages_stay_manual() {
        let mut b = browser();
        let command = b.begin(b.list_action(None)).unwrap();
        b.accept(
            &command,
            page(vec![node("root/run", "", "root", 0)], "next"),
        )
        .unwrap();
        assert!(
            matches!(b.runtime_page_action(), Some(Action::List { cursor: Some(cursor), .. }) if cursor == "next")
        );
        b.show_management = true;
        assert!(b.runtime_page_action().is_none());
        b.show_management = false;
        b.error = Some("cache limit".into());
        assert!(b.runtime_page_action().is_none());
        b.error = None;
        b.root_filter = Some("run".into());
        assert!(b.runtime_page_action().is_none());
    }
    #[test]
    fn deerflow_tree_live_keyset_pages_preserve_unseen_nodes_until_traversal_finishes() {
        let mut b = browser();
        load(
            &mut b,
            vec![
                node("root/run", "", "root", 0),
                node("older", "root/run", "foreground", 1),
            ],
        );
        let command = b.begin(b.list_action(None)).unwrap();
        b.accept(
            &command,
            page(vec![node("root/run", "", "root", 0)], "cursor-next"),
        )
        .unwrap();
        assert_eq!(b.nodes.len(), 2);
        assert!(b.has_more);
        let wrong = b.begin(b.list_action(Some("unknown".into()))).unwrap();
        assert!(b.accept(&wrong, page(vec![], "")).is_err());
        b.pending = None;
        let command = b.begin(b.list_action(Some("cursor-next".into()))).unwrap();
        b.accept(
            &command,
            page(vec![node("new", "root/run", "foreground", 1)], ""),
        )
        .unwrap();
        assert_eq!(b.nodes.len(), 2);
        assert!(!b.nodes.contains_key(&("run".into(), "older".into())));
    }
    #[test]
    fn deerflow_tree_rejects_foreign_and_late_responses_and_preserves_newer_attempts() {
        let mut b = browser();
        let command = b.begin(b.list_action(None)).unwrap();
        b.disconnect();
        assert!(!b.accepts(&command));
        assert!(b.accept(&command, page(vec![], "")).is_err());
        let mut b = browser();
        let mut foreign = node("root/run", "", "root", 0);
        foreign.root_session_id = "other".into();
        let command = b.begin(b.list_action(None)).unwrap();
        assert!(b.accept(&command, page(vec![foreign], "")).is_err());
        assert!(b.nodes.is_empty());
        b.pending = None;
        load(
            &mut b,
            vec![
                node("root/run", "", "root", 0),
                node("worker", "root/run", "background", 1),
            ],
        );
        let mut stale = node("worker", "root/run", "background", 1);
        stale.version = 2;
        b.insert(stale.clone()).unwrap();
        assert_eq!(b.nodes[&key(&stale)].node.version, 4);
        stale.task_id = "continuation".into();
        stale.continuation_of = "task".into();
        b.insert(stale.clone()).unwrap();
        assert_eq!(b.nodes[&key(&stale)].node.task_id, "continuation");
        assert_eq!(b.nodes[&key(&stale)].node.version, 2);
    }
    #[test]
    fn deerflow_tree_unknown_history_does_not_invent_usage_results_or_terminal_transitions() {
        let mut legacy = node("worker", "root/run", "foreground", 1);
        legacy.status = "unavailable".into();
        legacy.version = 0;
        legacy.allowed_actions.clear();
        let cached = CachedNode::parse(legacy, "session").unwrap();
        assert!(cached.details.contains("未提供可归属"));
        assert_eq!(cached.result.as_ref(), "尚无可展示的最终结果");
        assert_eq!(cached.node.version, 0);
        assert!(!is_terminal("waiting_input"));
        assert!(!is_terminal("suspended"));
        assert!(!is_terminal("cancelling"));
        let mut b = browser();
        b.insert(node("root/run", "", "root", 0)).unwrap();
        assert!(b.insert(node("bad", "root/run", "foreground", 3)).is_err());
    }
    #[test]
    fn deerflow_tree_permission_requires_exact_preview_and_explicit_decision() {
        let mut b = approval_browser();
        assert!(b.approval_action().is_none());
        let action = Action::Permission {
            task_id: "task".into(),
            interaction_id: "batch".into(),
            task_version: 4,
            intent_id: "intent".into(),
        };
        let command = b.begin(action.clone()).unwrap();
        let mut preview = json!({"sessionId":"wrong","toolCallId":"call","toolName":"write_file","configVersion":2,"arguments":{"path":"report.md","content":"hello"}});
        assert!(
            b.accept(&command, Response::Permission(preview.clone()))
                .is_err()
        );
        assert!(b.previews.is_empty());
        b.pending = None;
        preview["sessionId"] = json!("child");
        let command = b.begin(action).unwrap();
        b.accept(&command, Response::Permission(preview)).unwrap();
        assert!(b.approval_action().is_none());
        b.previews.get_mut("intent").unwrap().decision = Some(Decision::RejectOnce);
        let Action::Approve {
            decisions,
            task_version,
            ..
        } = b.approval_action().unwrap()
        else {
            panic!("expected approval");
        };
        assert_eq!(task_version, 4);
        assert_eq!(decisions[0].decision, Decision::RejectOnce);
        b.disconnect();
        assert!(b.approval_action().is_none());
        assert!(b.previews.is_empty());
    }
    #[test]
    fn deerflow_tree_resource_authorization_and_descendant_wait_never_approve_tools() {
        let mut b = approval_browser();
        let mut detail = background();
        detail.status = "suspended".into();
        detail.interaction = None;
        detail.resources=Some(serde_json::from_value(json!({"version":2,"status":"waiting_authorization","canAuthorize":true,"descriptor":{"version":1,"configDigest":"b".repeat(64),"schemaDigest":"c".repeat(64),"servers":["local"],"tools":["mcp/search"]}})).unwrap());
        let command = b
            .begin(Action::BackgroundGet {
                task_id: "task".into(),
            })
            .unwrap();
        b.accept(&command, Response::Background(detail)).unwrap();
        assert!(b.resource_summary.as_ref().unwrap().contains("mcp/search"));
        assert!(b.approval_action().is_none());
        b.nodes
            .get_mut(b.selected.as_ref().unwrap())
            .unwrap()
            .node
            .allowed_actions
            .push("resume".into());
        assert!(
            b.resume_action().is_none(),
            "nonready resources block resume even if stale actions allow it"
        );
        b.detail
            .as_mut()
            .unwrap()
            .resources
            .as_mut()
            .unwrap()
            .status = "ready".into();
        assert!(matches!(
            b.resume_action(),
            Some(Action::Resume { version: 4, .. })
        ));
        b.nodes
            .get_mut(b.selected.as_ref().unwrap())
            .unwrap()
            .node
            .allowed_actions
            .clear();
        assert!(
            b.resume_action().is_none(),
            "host action permission is required"
        );
        let mut waiting: Value = serde_json::to_value(background()).unwrap();
        waiting["interaction"]["kind"] = json!("task_wait");
        waiting["interaction"]["waitingInputs"] = Value::Null;
        let command = b
            .begin(Action::BackgroundGet {
                task_id: "task".into(),
            })
            .unwrap();
        b.accept(
            &command,
            Response::Background(serde_json::from_value(waiting).unwrap()),
        )
        .unwrap();
        assert!(b.approval_action().is_none());
    }
    #[test]
    fn deerflow_tree_preview_expires_when_a_fresh_tree_reports_new_task_version() {
        let mut b = approval_browser();
        b.previews.insert(
            "intent".into(),
            Preview {
                text: "old".into(),
                decision: Some(Decision::AllowOnce),
            },
        );
        let mut changed = node("worker", "root/run", "background", 1);
        changed.version = 5;
        load(&mut b, vec![node("root/run", "", "root", 0), changed]);
        assert!(b.detail.is_none());
        assert!(b.previews.is_empty());
        assert!(b.approval_action().is_none());
    }
    #[test]
    fn deerflow_tree_execution_resume_requires_fresh_resumable_host_state() {
        let mut b = browser();
        load(&mut b, vec![node("root/run", "", "root", 0)]);
        b.selected = Some(("run".into(), "root/run".into()));
        b.nodes
            .get_mut(b.selected.as_ref().unwrap())
            .unwrap()
            .node
            .execution = Some(waku_protocol::deerflow_tasks::ExecutionReference {
            run_id: "continuation-run".into(),
            version: 4,
            status: "waiting_input".into(),
            resumable: true,
        });
        let command = b
            .begin(Action::ExecutionGet {
                run_id: "continuation-run".into(),
            })
            .unwrap();
        let mut execution = Execution {
            session_id: "other".into(),
            run_id: "continuation-run".into(),
            status: "waiting_input".into(),
            version: 4,
            resumable: true,
            blocked_reason: String::new(),
        };
        assert!(
            b.accept(&command, Response::Execution(execution.clone()))
                .is_err()
        );
        execution.session_id = "session".into();
        b.accept(&command, Response::Execution(execution)).unwrap();
        assert!(matches!(
            b.execution_resume_action(),
            Some(Action::ExecutionResume {
                expected_version: 4,
                ..
            })
        ));
        b.execution.as_mut().unwrap().resumable = false;
        assert!(b.execution_resume_action().is_none());
        b.execution.as_mut().unwrap().resumable = true;
        b.execution.as_mut().unwrap().status = "needs_reconciliation".into();
        assert!(b.execution_resume_action().is_none());
        b.disconnect();
        assert!(b.execution_resume_action().is_none());
    }
    #[test]
    fn deerflow_tree_page_window_is_bounded_and_never_finishes_evicted_work() {
        let mut b = browser();
        let command = b.begin(b.list_action(None)).unwrap();
        b.accept(
            &command,
            page(vec![node("worker", "root/run", "background", 1)], "next"),
        )
        .unwrap();
        b.drop_cached_nodes();
        assert!(b.nodes.is_empty());
        assert!(b.seen.is_empty());
        assert!(b.has_live_detached());
        assert!(b.partial_view);
        let command = b.begin(b.list_action(Some("next".into()))).unwrap();
        b.accept(&command, page(vec![], "")).unwrap();
        assert!(
            b.has_live_detached(),
            "partial last page cannot prove prior work finished"
        );
        load(&mut b, vec![]);
        assert!(
            !b.has_live_detached(),
            "complete unfiltered refresh supersedes evicted activity"
        );
        assert!(!b.partial_view);
    }
    #[test]
    fn deerflow_tree_stopped_foreground_role_resumes_its_exact_background_execution() {
        let mut b = browser();
        let mut role = node("role", "root/run", "foreground", 1);
        role.version = 90;
        role.allowed_actions = vec!["resume".into()];
        role.background_execution = Some(
            waku_protocol::deerflow_tasks::BackgroundExecutionReference {
                task_id: "task".into(),
                version: 4,
                status: "waiting_input".into(),
                interaction_id: "batch".into(),
                resumable: true,
            },
        );
        load(&mut b, vec![node("root/run", "", "root", 0), role]);
        b.selected = Some(("run".into(), "role".into()));
        let command = b
            .begin(Action::BackgroundGet {
                task_id: "task".into(),
            })
            .unwrap();
        let mut detail = background();
        let interaction = detail.interaction.as_mut().unwrap();
        interaction.kind = "cancelled_roles".into();
        interaction.waiting_inputs = Some(vec![]);
        interaction.resumable = true;
        let mut changed = detail.clone();
        changed.interaction.as_mut().unwrap().id = "stale-batch".into();
        assert!(b.accept(&command, Response::Background(changed)).is_err());
        b.accept(&command, Response::Background(detail)).unwrap();
        assert!(
            matches!(b.resume_action(),Some(Action::Resume {task_id,version:4}) if task_id=="task")
        );
        assert!(b.approval_action().is_none());
    }
}
