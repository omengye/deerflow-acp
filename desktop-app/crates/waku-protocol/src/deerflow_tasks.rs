//! Typed control for the attached DeerFlow task tree. This contract contains no
//! process, checkpoint or credential handles; the host authorizes every action.
use serde::{Deserialize, Serialize};
use serde_json::Value;
use ts_rs::TS;

#[derive(Clone, Debug, Default, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct Capabilities {
    pub tree: bool,
    pub cancel: bool,
    pub permission: bool,
    pub resume: bool,
    pub resource_authorize: bool,
    pub executions: bool,
    pub artifact_read: bool,
    #[serde(default)]
    pub artifact_list: bool,
    #[serde(default)]
    pub tool_receipts: bool,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum CancelScope {
    Subtree,
    Root,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Decision {
    AllowOnce,
    RejectOnce,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct ApprovalDecision {
    pub intent_id: String,
    pub version: u64,
    pub decision: Decision,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS, PartialEq, Eq)]
#[serde(
    tag = "kind",
    rename_all = "camelCase",
    rename_all_fields = "camelCase"
)]
pub enum Action {
    ReceiptList {
        cursor: Option<String>,
        limit: u32,
        uncertain_only: bool,
    },
    ReceiptReconcile {
        review: ReceiptReconciliation,
    },
    List {
        root_id: Option<String>,
        cursor: Option<String>,
        limit: u32,
    },
    Get {
        root_id: String,
        node_id: String,
    },
    Cancel {
        root_id: String,
        node_id: String,
        scope: CancelScope,
        expected_version: u64,
    },
    BackgroundGet {
        task_id: String,
    },
    Permission {
        task_id: String,
        interaction_id: String,
        task_version: u64,
        intent_id: String,
    },
    Approve {
        task_id: String,
        interaction_id: String,
        task_version: u64,
        decisions: Vec<ApprovalDecision>,
    },
    Resume {
        task_id: String,
        version: u64,
    },
    AuthorizeResources {
        task_id: String,
        task_version: u64,
        resource_version: u64,
    },
    ExecutionGet {
        run_id: String,
    },
    ArtifactRead {
        root_id: String,
        node_id: String,
        artifact_id: String,
        offset: u64,
        limit: u32,
        expected_sha256: Option<String>,
    },
    ArtifactList {
        root_id: String,
        node_id: String,
        cursor: Option<String>,
        limit: u32,
    },
    ExecutionResume {
        run_id: String,
        expected_version: u64,
    },
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct Command {
    pub request_id: String,
    pub connection_id: String,
    pub session_id: String,
    pub action: Action,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct Page {
    pub session_id: String,
    pub nodes: Vec<Node>,
    #[serde(default)]
    pub next_cursor: String,
    pub has_more: bool,
    pub consistency: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct Node {
    pub id: String,
    pub root_id: String,
    pub root_session_id: String,
    #[serde(default)]
    pub parent_id: String,
    #[serde(default)]
    pub invocation_id: String,
    #[serde(default)]
    pub task_id: String,
    #[serde(default)]
    pub continuation_of: String,
    pub role: String,
    #[serde(default)]
    pub model: String,
    pub mode: String,
    pub depth: u32,
    #[serde(default)]
    pub description: String,
    pub status: String,
    pub version: u64,
    pub attempt: u64,
    #[serde(default)]
    pub blocked_reason: String,
    pub waiting: Option<Waiting>,
    pub execution: Option<ExecutionReference>,
    pub background_execution: Option<BackgroundExecutionReference>,
    pub resources: Option<Resources>,
    pub usage: Option<Usage>,
    pub result: Option<TaskResult>,
    #[serde(default)]
    pub artifacts: Vec<Artifact>,
    pub allowed_actions: Vec<String>,
    pub created_at: Option<String>,
    pub updated_at: Option<String>,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct Waiting {
    pub kind: String,
    #[serde(default)]
    pub task_ids: Vec<String>,
    #[serde(default)]
    pub intent_count: u32,
    pub resumable: bool,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct Usage {
    pub scope: String,
    pub model_calls: u64,
    pub tool_calls: u64,
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub total_tokens: u64,
    pub held_tokens: u64,
    pub estimated: bool,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
pub struct TaskResult {
    pub text: String,
    pub truncated: bool,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct Artifact {
    pub id: String,
    pub name: String,
    pub mime_type: String,
    pub size: u64,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct ArtifactChunk {
    pub root_id: String,
    pub node_id: String,
    pub artifact: Artifact,
    pub sha256: String,
    pub encoding: String,
    pub data: String,
    pub offset: u64,
    pub next_offset: u64,
    pub eof: bool,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct ArtifactPage {
    pub root_id: String,
    pub node_id: String,
    pub artifacts: Vec<Artifact>,
    #[serde(default)]
    pub next_cursor: String,
    pub has_more: bool,
    pub consistency: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct BackgroundExecutionReference {
    pub task_id: String,
    pub version: u64,
    pub status: String,
    #[serde(default)]
    pub interaction_id: String,
    pub resumable: bool,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct BackgroundDetail {
    pub id: String,
    pub session_id: String,
    #[serde(default)]
    pub child_session_id: String,
    #[serde(default)]
    pub root_session_id: String,
    pub status: String,
    pub version: u64,
    pub interaction: Option<Interaction>,
    pub resources: Option<Resources>,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct Resources {
    pub version: u64,
    pub status: String,
    #[serde(default)]
    pub reason: String,
    pub descriptor: ResourceDescriptor,
    pub can_authorize: bool,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct ResourceDescriptor {
    pub version: u64,
    pub config_digest: String,
    pub schema_digest: String,
    pub servers: Vec<String>,
    pub tools: Vec<String>,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct Interaction {
    pub id: String,
    pub task_version: u64,
    #[serde(default)]
    pub kind: String,
    pub waiting_inputs: Option<Vec<Intent>>,
    pub resumable: bool,
}

impl Interaction {
    pub fn inputs(&self) -> &[Intent] {
        self.waiting_inputs.as_deref().unwrap_or(&[])
    }
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct Intent {
    pub id: String,
    pub kind: String,
    pub version: u64,
    pub tool_call_id: String,
    pub tool_name: String,
    pub arguments_digest: String,
    pub arguments_summary: String,
    pub config_version: u64,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct Execution {
    pub session_id: String,
    pub run_id: String,
    pub status: String,
    pub version: u64,
    pub resumable: bool,
    #[serde(default)]
    pub blocked_reason: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct ExecutionReference {
    pub run_id: String,
    pub status: String,
    pub version: u64,
    pub resumable: bool,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(tag = "kind", content = "data", rename_all = "camelCase")]
pub enum Response {
    Receipts(ReceiptPage),
    Reconciled { receipt: ToolReceipt },
    Page(Page),
    Node(Node),
    Background(BackgroundDetail),
    Permission(Value),
    Execution(Execution),
    ExecutionFinished,
    Artifact(ArtifactChunk),
    Artifacts(ArtifactPage),
}

#[derive(Clone, Debug, Deserialize, Serialize, TS, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum ReceiptOutcome {
    Completed,
    NoEffect,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct ReceiptReconciliation {
    pub run_id: String,
    pub tool_call_id: String,
    pub expected_version: u64,
    pub outcome: ReceiptOutcome,
    pub reviewer: String,
    pub note: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct ReceiptPage {
    pub receipts: Vec<ToolReceipt>,
    pub next_cursor: String,
    pub has_more: bool,
    pub consistency: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(rename_all = "camelCase")]
pub struct ToolReceipt {
    pub session_id: String,
    pub run_id: String,
    pub tool_call_id: String,
    pub tool_name: String,
    pub arguments_digest: String,
    pub arguments_summary: String,
    pub config_version: u64,
    pub state: String,
    pub version: u64,
    #[serde(default)]
    pub result: Vec<Value>,
    #[serde(default)]
    pub result_truncated: bool,
    #[serde(default)]
    pub error: String,
    pub review: Option<Value>,
}

#[derive(Clone, Debug, Deserialize, Serialize, TS)]
#[serde(
    tag = "kind",
    rename_all = "camelCase",
    rename_all_fields = "camelCase"
)]
pub enum Event {
    Invalidated {
        connection_id: String,
        session_id: String,
    },
    Ready {
        connection_id: String,
        session_id: String,
        capabilities: Capabilities,
    },
    Reply {
        command: Command,
        response: Response,
    },
    Failed {
        command: Command,
        code: String,
        message: String,
    },
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn deerflow_task_wire_roundtrip_keeps_management_errors_separate_and_versions_exact() {
        let command = Command {
            request_id: "request".into(),
            connection_id: "connection".into(),
            session_id: "session".into(),
            action: Action::Cancel {
                root_id: "run".into(),
                node_id: "root/run".into(),
                scope: CancelScope::Root,
                expected_version: 9_007_199_254_740_993,
            },
        };
        let event = crate::model::DriverEvent::DeerFlowTasks(Event::Failed {
            command: command.clone(),
            code: "-32021".into(),
            message: "refresh".into(),
        });
        let wire = crate::event_to_wire(event).unwrap();
        assert_eq!(wire.kind, "deerflowTasks");
        let decoded = crate::event_from_wire(wire).unwrap();
        let crate::model::DriverEvent::DeerFlowTasks(Event::Failed {
            command: decoded, ..
        }) = decoded
        else {
            panic!("task error must stay separate");
        };
        assert_eq!(decoded.action, command.action);
        let json = serde_json::to_value(crate::Command::DeerFlowTasks { command }).unwrap();
        assert_eq!(
            json["command"]["action"]["expectedVersion"].as_u64(),
            Some(9_007_199_254_740_993)
        );
    }
}
