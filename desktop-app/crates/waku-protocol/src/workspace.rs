use std::path::PathBuf;

use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::composer::{FileEntry, SlashCommand};
use crate::git::{AgentInvocation, BranchSnapshot, CommitSnapshot, CreatedWorktree};
use crate::model::{Checkpoint, ProviderKind};

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub enum ReviewDiffSource {
    LastTurn {
        session_id: Uuid,
        turn_id: Uuid,
        turn_count: usize,
    },
    Uncommitted,
    Unstaged,
    Staged,
    Committed,
    Branch,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ReviewDiffData {
    pub source: ReviewDiffSource,
    pub numstat: String,
    pub patch: String,
    pub complete_context: bool,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct WorkingTreeEntry {
    pub relative_path: String,
    pub absolute_path: PathBuf,
    pub name: String,
    pub is_dir: bool,
    pub expanded: bool,
    pub depth: usize,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(tag = "type", rename_all = "camelCase")]
pub enum WorkspaceOperation {
    ListTree {
        root: PathBuf,
        expanded_paths: Vec<PathBuf>,
    },
    BrowseDirectory {
        path: Option<PathBuf>,
    },
    ReadTextFile {
        root: PathBuf,
        relative_path: PathBuf,
    },
    WriteTextFile {
        root: PathBuf,
        relative_path: PathBuf,
        content: String,
    },
    ListProjectFiles {
        root: PathBuf,
        cap: usize,
    },
    DiscoverSlashCommands {
        provider: ProviderKind,
        project_root: PathBuf,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        binary_override: Option<String>,
    },
    CreateProjectlessWorkspace {
        prompt: Option<String>,
    },
    MigrateProjectlessWorkspace {
        path: PathBuf,
    },
    InspectBranches {
        cwd: PathBuf,
    },
    CheckoutBranch {
        cwd: PathBuf,
        branch: String,
        create: bool,
    },
    CreateWorktree {
        project_path: PathBuf,
        project_id: Uuid,
        session_id: Uuid,
        prompt: String,
        base_branch: Option<String>,
    },
    InspectCommit {
        cwd: PathBuf,
    },
    GenerateCommitMessage {
        cwd: PathBuf,
        include_unstaged: bool,
        invocation: AgentInvocation,
    },
    Commit {
        cwd: PathBuf,
        message: String,
        include_unstaged: bool,
        push: bool,
    },
    Push {
        cwd: PathBuf,
    },
    CaptureTurnStart {
        cwd: PathBuf,
        session_id: Uuid,
        turn_count: usize,
    },
    CaptureTurn {
        cwd: PathBuf,
        session_id: Uuid,
        turn_count: usize,
    },
    CaptureRef {
        cwd: PathBuf,
        git_ref: String,
    },
    RestoreRef {
        cwd: PathBuf,
        git_ref: String,
    },
    HasRef {
        cwd: PathBuf,
        git_ref: String,
    },
    SessionTurnRefs {
        cwd: PathBuf,
        session_id: Uuid,
    },
    DeleteRef {
        cwd: PathBuf,
        git_ref: String,
    },
    DeleteTurnRefsAfter {
        cwd: PathBuf,
        session_id: Uuid,
        retained_turn_count: usize,
        previous_turn_count: usize,
    },
    DeleteSessionRefs {
        cwd: PathBuf,
        session_id: Uuid,
    },
    CopySessionRefs {
        cwd: PathBuf,
        source_session_id: Uuid,
        target_session_id: Uuid,
        through_turn_count: usize,
    },
    CollectReviewDiff {
        cwd: PathBuf,
        source: ReviewDiffSource,
    },
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(tag = "type", rename_all = "camelCase")]
pub enum WorkspaceResult {
    Ack,
    WorkingTree {
        entries: Vec<WorkingTreeEntry>,
    },
    Directory {
        path: PathBuf,
        parent: Option<PathBuf>,
        home: PathBuf,
        filesystem_root: PathBuf,
        entries: Vec<WorkingTreeEntry>,
    },
    TextFile {
        content: String,
    },
    ProjectFiles {
        entries: Vec<FileEntry>,
    },
    SlashCommands {
        commands: Vec<SlashCommand>,
    },
    ProjectlessWorkspace {
        cwd: PathBuf,
    },
    Branches {
        snapshot: Option<BranchSnapshot>,
    },
    BranchChanged {
        snapshot: BranchSnapshot,
    },
    WorktreeCreated {
        worktree: CreatedWorktree,
    },
    CommitSnapshot {
        snapshot: CommitSnapshot,
    },
    CommitMessage {
        message: String,
    },
    Checkpoint {
        checkpoint: Checkpoint,
    },
    Bool {
        value: bool,
    },
    TurnRefs {
        turn_counts: Vec<usize>,
    },
    ReviewDiff {
        data: ReviewDiffData,
    },
}
