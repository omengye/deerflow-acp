//! Daemon-owned Git checkpoint capture and restoration.

use std::collections::{BTreeMap, HashMap, HashSet};
use std::ffi::OsStr;
use std::fs;
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::process::{Command, Output, Stdio};

use anyhow::{Context as _, anyhow, bail};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::model::{Checkpoint, CheckpointFile, CheckpointStatus, unix_time};

const TURN_START_METADATA_PREFIX: &str = "Waku-Turn-Start: ";
const EMPTY_TREE: &str = "4b825dc642cb6eb9a060e54bf8d69288fbee4904";

#[derive(Debug, Deserialize, Serialize)]
struct TurnStartMetadata {
    head: Option<String>,
    branch: Option<String>,
    refs: BTreeMap<String, String>,
}

pub fn checkpoint_ref(session_id: Uuid, turn_count: usize) -> String {
    format!("refs/waku/session-{session_id}-turn-{turn_count}")
}

pub fn turn_start_ref(session_id: Uuid, turn_count: usize) -> String {
    format!("refs/waku/session-{session_id}-turn-start-{turn_count}")
}

pub fn turn_diff_base_ref(session_id: Uuid, turn_count: usize) -> String {
    format!("refs/waku/session-{session_id}-turn-diff-{turn_count}")
}

/// Capture the exact workspace state accepted for a turn before its provider
/// starts. This is intentionally distinct from the preceding turn's ending
/// checkpoint: a branch switch or terminal edit between turns must not be
/// attributed to either response.
pub fn capture_turn_start(cwd: &Path, session_id: Uuid, turn_count: usize) -> anyhow::Result<()> {
    let Some(worktree) = CheckpointWorktree::open(cwd)? else {
        return Ok(());
    };

    let branch = symbolic_head(cwd);
    let refs = repository_refs(cwd)?;
    let metadata = TurnStartMetadata {
        head: worktree.head.clone(),
        branch,
        refs,
    };
    let message = format!(
        "Waku turn start snapshot\n\n{TURN_START_METADATA_PREFIX}{}",
        serde_json::to_string(&metadata)?
    );
    let mut parents = Vec::new();
    let mut seen = HashSet::new();
    if let Some(head) = worktree.head.as_ref()
        && seen.insert(head.clone())
    {
        parents.push(head.clone());
    }
    for commit in metadata.refs.values() {
        if seen.insert(commit.clone()) {
            parents.push(commit.clone());
        }
    }
    let commit = capture_worktree_commit_from(cwd, &worktree, &message, &parents)?;
    let start_ref = turn_start_ref(session_id, turn_count);
    let baseline_ref = checkpoint_ref(session_id, turn_count.saturating_sub(1));
    let mut commands = format!("update {start_ref} {commit}\n");
    if !has_ref(cwd, &baseline_ref) {
        commands.push_str(&format!("update {baseline_ref} {commit}\n"));
    }
    update_refs(cwd, commands)
}

pub fn capture_turn(cwd: &Path, session_id: Uuid, turn_count: usize) -> anyhow::Result<Checkpoint> {
    let git_ref = checkpoint_ref(session_id, turn_count);
    let Some(worktree) = CheckpointWorktree::open(cwd)? else {
        return Ok(Checkpoint {
            turn_count,
            git_ref,
            status: CheckpointStatus::Unavailable,
            files: Vec::new(),
            additions: 0,
            deletions: 0,
            created_at: unix_time(),
        });
    };

    let end_branch = symbolic_head(cwd);
    let end_commit = capture_worktree_commit_from(cwd, &worktree, "Waku worktree snapshot", &[])?;
    git_output(cwd, ["update-ref", &git_ref, &end_commit])?;
    let files = if turn_count == 0 {
        Vec::new()
    } else {
        let start_ref = turn_start_ref(session_id, turn_count);
        let legacy_ref = checkpoint_ref(session_id, turn_count - 1);
        let diff_base = if has_ref(cwd, &start_ref) {
            prepare_turn_diff_base(
                cwd,
                session_id,
                turn_count,
                &start_ref,
                worktree.head.as_deref(),
                end_branch.as_deref(),
            )?
        } else {
            legacy_ref
        };
        if has_ref(cwd, &diff_base) {
            diff_files(cwd, &diff_base, &git_ref)?
        } else {
            Vec::new()
        }
    };
    let additions = files.iter().map(|file| file.additions).sum();
    let deletions = files.iter().map(|file| file.deletions).sum();
    Ok(Checkpoint {
        turn_count,
        git_ref,
        status: CheckpointStatus::Ready,
        files,
        additions,
        deletions,
        created_at: unix_time(),
    })
}

pub fn capture_ref(cwd: &Path, git_ref: &str) -> anyhow::Result<()> {
    if !is_git_repository(cwd) {
        bail!("checkpoints require a Git repository");
    }

    let commit = capture_worktree_commit(cwd)?;
    git_output(cwd, ["update-ref", git_ref, &commit])?;
    Ok(())
}

/// Different subdirectories of one Git worktree share a history admission
/// fence. Separate Git worktrees retain separate worktree identities.
pub(crate) fn workspace_root(cwd: &Path) -> anyhow::Result<PathBuf> {
    let root = match git_output(cwd, ["rev-parse", "--show-toplevel"]) {
        Ok(root) if !root.trim().is_empty() => PathBuf::from(root.trim()),
        _ => cwd.to_owned(),
    };
    root.canonicalize()
        .context("the workspace path is unavailable")
}

/// Capture the current worktree and untracked files as a dangling commit.
///
/// This shares the checkpoint path's isolated temporary index, so it never
/// stages or unstages the user's files. Review uses the returned treeish to
/// compare a stable worktree snapshot while edits continue on disk.
pub fn capture_worktree_commit(cwd: &Path) -> anyhow::Result<String> {
    let worktree = CheckpointWorktree::open(cwd)?
        .context("worktree snapshots require a Git workspace with paths eligible for capture")?;
    capture_worktree_commit_from(cwd, &worktree, "Waku worktree snapshot", &[])
}

struct CheckpointWorktree {
    head: Option<String>,
    // In an ignored directory, only these NUL-delimited tracked paths can be captured.
    tracked_paths: Option<Vec<u8>>,
}

impl CheckpointWorktree {
    fn open(cwd: &Path) -> anyhow::Result<Option<Self>> {
        let location = git_output(cwd, ["rev-parse", "--is-inside-work-tree", "--show-cdup"])
            .unwrap_or_default();
        let mut location = location.lines();
        if location.next() != Some("true") {
            return Ok(None);
        }
        let mut worktree = Self {
            head: resolve_ref(cwd, "HEAD"),
            tracked_paths: None,
        };
        if location.next().unwrap_or_default().is_empty() {
            return Ok(Some(worktree));
        }

        // Check the directory itself. Git can match "." against a rule for its
        // children (such as dist/*), which may have explicitly included files.
        let ignored = crate::command_env::plain_command("git")
            .args(["check-ignore", "--quiet", "--no-index", "--"])
            .arg(cwd.canonicalize()?)
            .current_dir(cwd)
            .output()
            .context("failed to check Git ignore rules")?;
        match ignored.status.code() {
            Some(1) => {}
            Some(0) => {
                // Include staged additions and paths removed from the index since HEAD.
                let with_tree = worktree
                    .head
                    .as_ref()
                    .map(|head| format!("--with-tree={head}"));
                let args = ["ls-files", "--cached", "-z"]
                    .into_iter()
                    .chain(with_tree.as_deref())
                    .chain(["--", "."]);
                let paths = git_output_bytes(cwd, args)?;
                if paths.is_empty() {
                    return Ok(None);
                }
                worktree.tracked_paths = Some(paths);
            }
            _ => bail!("{}", command_error(&ignored)),
        }
        Ok(Some(worktree))
    }
}

fn capture_worktree_commit_from(
    cwd: &Path,
    worktree: &CheckpointWorktree,
    message: &str,
    parents: &[String],
) -> anyhow::Result<String> {
    let common_dir = git_output(cwd, ["rev-parse", "--git-common-dir"])?
        .trim()
        .to_owned();
    if common_dir.is_empty() {
        bail!("git did not return its common directory");
    }
    let common_dir = PathBuf::from(common_dir);
    let common_dir = if common_dir.is_absolute() {
        common_dir
    } else {
        cwd.join(common_dir)
    };
    let temporary_index = common_dir.join(format!("waku-checkpoint-index-{}", Uuid::new_v4()));

    let result = (|| {
        if let Some(head) = worktree.head.as_deref() {
            git_with_index(cwd, &temporary_index, ["read-tree", head])?;
        }
        if let Some(paths) = &worktree.tracked_paths {
            git_with_input(
                crate::command_env::plain_command("git")
                    .args([
                        "update-index",
                        "--add",
                        "--remove",
                        "--replace",
                        "-z",
                        "--stdin",
                    ])
                    .current_dir(cwd)
                    .env("GIT_INDEX_FILE", &temporary_index),
                paths,
            )?;
        } else {
            git_with_index(cwd, &temporary_index, ["add", "-A", "--", "."])?;
        }
        let tree = git_with_index(cwd, &temporary_index, ["write-tree"])?
            .trim()
            .to_owned();
        if tree.is_empty() {
            bail!("git write-tree returned no object id");
        }
        let mut arguments = vec![
            "commit-tree".to_owned(),
            tree,
            "-m".to_owned(),
            message.to_owned(),
        ];
        for parent in parents {
            arguments.push("-p".to_owned());
            arguments.push(parent.clone());
        }
        let commit = git_with_identity_and_index(cwd, &temporary_index, &arguments)?
            .trim()
            .to_owned();
        if commit.is_empty() {
            bail!("git commit-tree returned no object id");
        }
        Ok(commit)
    })();

    let _ = fs::remove_file(&temporary_index);
    let _ = fs::remove_file(temporary_index.with_extension("lock"));
    result
}

fn prepare_turn_diff_base(
    cwd: &Path,
    session_id: Uuid,
    turn_count: usize,
    start_ref: &str,
    end_head: Option<&str>,
    end_branch: Option<&str>,
) -> anyhow::Result<String> {
    let metadata = turn_start_metadata(cwd, start_ref)?;
    let same_line = match (metadata.branch.as_deref(), end_branch) {
        (Some(start), Some(end)) => start == end,
        (None, None) => metadata.head.as_deref() == end_head,
        _ => false,
    };

    let commit = if same_line {
        resolve_ref(cwd, start_ref)
            .ok_or_else(|| anyhow!("turn starting checkpoint `{start_ref}` is unavailable"))?
    } else {
        let target_base = target_branch_start(cwd, start_ref, &metadata, end_head, end_branch)?;
        virtual_branch_start(cwd, start_ref, metadata.head.as_deref(), &target_base)?
    };
    let diff_ref = turn_diff_base_ref(session_id, turn_count);
    git_output(cwd, ["update-ref", &diff_ref, &commit])?;
    Ok(diff_ref)
}

fn turn_start_metadata(cwd: &Path, start_ref: &str) -> anyhow::Result<TurnStartMetadata> {
    let message = git_output(cwd, ["show", "-s", "--format=%B", start_ref])?;
    let encoded = message
        .lines()
        .find_map(|line| line.strip_prefix(TURN_START_METADATA_PREFIX))
        .ok_or_else(|| anyhow!("turn starting checkpoint metadata is unavailable"))?;
    serde_json::from_str(encoded).context("invalid turn starting checkpoint metadata")
}

fn target_branch_start(
    cwd: &Path,
    start_ref: &str,
    metadata: &TurnStartMetadata,
    end_head: Option<&str>,
    end_branch: Option<&str>,
) -> anyhow::Result<String> {
    if let Some(branch) = end_branch
        && let Some(commit) = metadata.refs.get(branch)
    {
        return Ok(commit.clone());
    }
    let Some(end_head) = end_head else {
        return empty_tree_commit(cwd);
    };
    let new_commits = git_output(
        cwd,
        [
            "rev-list",
            "--first-parent",
            "--reverse",
            end_head,
            "--not",
            start_ref,
        ],
    )?;
    let Some(first_new_commit) = new_commits.lines().find(|line| !line.trim().is_empty()) else {
        return Ok(end_head.to_owned());
    };
    resolve_ref(cwd, &format!("{first_new_commit}^1")).map_or_else(|| empty_tree_commit(cwd), Ok)
}

fn virtual_branch_start(
    cwd: &Path,
    start_ref: &str,
    start_head: Option<&str>,
    target_base: &str,
) -> anyhow::Result<String> {
    let Some(start_head) = start_head else {
        return Ok(target_base.to_owned());
    };
    if start_head == target_base {
        return resolve_ref(cwd, start_ref)
            .ok_or_else(|| anyhow!("turn starting checkpoint `{start_ref}` is unavailable"));
    }
    if git_output(cwd, ["diff", "--name-only", start_head, start_ref])?
        .trim()
        .is_empty()
    {
        return Ok(target_base.to_owned());
    }

    // Recreate the state Git would have after carrying the user's pre-turn
    // dirty files onto the target branch. Comparing against the raw target tip
    // would otherwise attribute those already-present edits to the response.
    let output = git_output(
        cwd,
        [
            "merge-tree",
            "--write-tree",
            "--merge-base",
            start_head,
            target_base,
            start_ref,
        ],
    )?;
    let tree = output
        .lines()
        .next()
        .map(str::trim)
        .filter(|tree| !tree.is_empty())
        .ok_or_else(|| anyhow!("git merge-tree returned no tree"))?;
    commit_tree(cwd, tree, "Waku turn diff base", &[])
}

fn empty_tree_commit(cwd: &Path) -> anyhow::Result<String> {
    commit_tree(cwd, EMPTY_TREE, "Waku empty turn diff base", &[])
}

fn commit_tree(
    cwd: &Path,
    tree: &str,
    message: &str,
    parents: &[String],
) -> anyhow::Result<String> {
    let common_dir = git_output(cwd, ["rev-parse", "--git-common-dir"])?
        .trim()
        .to_owned();
    let common_dir = PathBuf::from(common_dir);
    let common_dir = if common_dir.is_absolute() {
        common_dir
    } else {
        cwd.join(common_dir)
    };
    let temporary_index = common_dir.join(format!("waku-checkpoint-index-{}", Uuid::new_v4()));
    let mut arguments = vec![
        "commit-tree".to_owned(),
        tree.to_owned(),
        "-m".to_owned(),
        message.to_owned(),
    ];
    for parent in parents {
        arguments.push("-p".to_owned());
        arguments.push(parent.clone());
    }
    git_with_identity_and_index(cwd, &temporary_index, &arguments)
        .map(|commit| commit.trim().to_owned())
}

pub fn restore_ref(cwd: &Path, git_ref: &str) -> anyhow::Result<()> {
    let current = capture_worktree_commit(cwd)?;
    restore_snapshot(cwd, git_ref, &current)
}

pub fn same_tree(cwd: &Path, left: &str, right: &str) -> anyhow::Result<bool> {
    let left = git_output(cwd, ["rev-parse", "--verify", &format!("{left}^{{tree}}")])?;
    let right = git_output(cwd, ["rev-parse", "--verify", &format!("{right}^{{tree}}")])?;
    Ok(left.trim() == right.trim())
}

/// Refuse to overwrite edits made since the operation's accepted snapshot.
/// Staged changes belong to the user and are never reset by this path.
pub fn restore_ref_if_unchanged(cwd: &Path, git_ref: &str, expected: &str) -> anyhow::Result<()> {
    let staged = git_output(cwd, ["diff", "--cached", "--name-only", "--", "."])?;
    if !staged.trim().is_empty() {
        bail!("The workspace has staged changes; preserve them before rewinding");
    }
    let current = capture_worktree_commit(cwd)?;
    if !same_tree(cwd, &current, expected)? {
        bail!("The workspace changed after the rewind was prepared; no files were restored");
    }
    restore_snapshot(cwd, git_ref, expected)
}

fn restore_snapshot(cwd: &Path, git_ref: &str, expected: &str) -> anyhow::Result<()> {
    let commit = resolve_ref(cwd, git_ref)
        .ok_or_else(|| anyhow!("checkpoint `{git_ref}` is unavailable"))?;
    // Only remove paths that are explicitly present in the accepted snapshot
    // and absent from the target. Unrecorded and ignored files are preserved.
    let deleted = git_output(
        cwd,
        [
            "diff",
            "--relative",
            "--name-only",
            "--diff-filter=D",
            "-z",
            expected,
            &commit,
            "--",
            ".",
        ],
    )?;
    let root = cwd.canonicalize()?;
    let removals = deleted
        .split('\0')
        .filter(|path| !path.is_empty())
        .map(|path| {
            let relative = Path::new(path);
            if relative.is_absolute()
                || relative
                    .components()
                    .any(|part| !matches!(part, std::path::Component::Normal(_)))
            {
                bail!("checkpoint contains an invalid removal path");
            }
            let path = root.join(relative);
            if path.exists()
                && !path
                    .parent()
                    .ok_or_else(|| anyhow!("invalid checkpoint path"))?
                    .canonicalize()?
                    .starts_with(&root)
            {
                bail!("checkpoint removal escapes the workspace");
            }
            Ok(path)
        })
        .collect::<anyhow::Result<Vec<_>>>()?;
    git_output(
        cwd,
        [
            "restore",
            "--source",
            &commit,
            "--worktree",
            "--staged",
            "--",
            ".",
        ],
    )?;
    for path in removals {
        match fs::remove_file(&path) {
            Ok(()) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => {
                return Err(error).with_context(|| {
                    format!("could not remove checkpoint-owned file {}", path.display())
                });
            }
        }
    }
    if has_head(cwd) {
        git_output(cwd, ["reset", "--quiet", "--", "."])?;
    }
    Ok(())
}

pub fn has_ref(cwd: &Path, git_ref: &str) -> bool {
    resolve_ref(cwd, git_ref).is_some()
}

/// Every turn count that has a checkpoint ref for `session_id`, resolved with
/// a single `git for-each-ref` instead of one `git rev-parse` per turn.
pub fn session_turn_refs(cwd: &Path, session_id: Uuid) -> HashSet<usize> {
    session_checkpoint_ref_commits(cwd, session_id)
        .turns
        .into_keys()
        .collect()
}

pub fn delete_ref(cwd: &Path, git_ref: &str) -> anyhow::Result<()> {
    let output = crate::command_env::plain_command("git")
        .args(["update-ref", "-d", git_ref])
        .current_dir(cwd)
        .output()
        .with_context(|| format!("failed to delete checkpoint `{git_ref}`"))?;
    if output.status.success() {
        Ok(())
    } else {
        bail!("{}", command_error(&output))
    }
}

pub fn delete_turn_refs_after(
    cwd: &Path,
    session_id: Uuid,
    retained_turn_count: usize,
    previous_turn_count: usize,
) -> anyhow::Result<()> {
    let mut commands = String::new();
    for turn_count in retained_turn_count + 1..=previous_turn_count {
        commands.push_str(&format!(
            "delete {}\ndelete {}\ndelete {}\n",
            checkpoint_ref(session_id, turn_count),
            turn_start_ref(session_id, turn_count),
            turn_diff_base_ref(session_id, turn_count)
        ));
    }
    update_refs(cwd, commands)
}

pub fn delete_session_refs(
    cwd: &Path,
    session_id: Uuid,
    last_turn_count: usize,
) -> anyhow::Result<()> {
    let mut commands = String::new();
    for turn_count in 0..=last_turn_count {
        commands.push_str(&format!(
            "delete {}\n",
            checkpoint_ref(session_id, turn_count)
        ));
        if turn_count > 0 {
            commands.push_str(&format!(
                "delete {}\ndelete {}\n",
                turn_start_ref(session_id, turn_count),
                turn_diff_base_ref(session_id, turn_count)
            ));
        }
    }
    update_refs(cwd, commands)
}

/// Delete every checkpoint ref owned by a session without requiring a client
/// to know how many turns are stored. This is the remote-safe deletion path:
/// the daemon enumerates its own repository refs as the authority.
pub fn delete_all_session_refs(cwd: &Path, session_id: Uuid) -> anyhow::Result<()> {
    let refs = session_checkpoint_ref_commits_strict(cwd, session_id)?;
    let mut commands = String::new();
    for turn_count in refs.turns.keys() {
        commands.push_str(&format!(
            "delete {}\n",
            checkpoint_ref(session_id, *turn_count)
        ));
    }
    for turn_count in refs.starts.keys() {
        commands.push_str(&format!(
            "delete {}\n",
            turn_start_ref(session_id, *turn_count)
        ));
    }
    for turn_count in refs.diff_bases.keys() {
        commands.push_str(&format!(
            "delete {}\n",
            turn_diff_base_ref(session_id, *turn_count)
        ));
    }
    update_refs(cwd, commands)
}

pub fn copy_session_refs(
    cwd: &Path,
    source_session_id: Uuid,
    target_session_id: Uuid,
    through_turn_count: usize,
) -> anyhow::Result<()> {
    if !is_git_repository(cwd) {
        return Ok(());
    }

    let source = session_checkpoint_ref_commits(cwd, source_session_id);
    let mut commands = String::new();
    for turn_count in 0..=through_turn_count {
        if let Some(commit) = source.turns.get(&turn_count) {
            commands.push_str(&format!(
                "update {} {commit}\n",
                checkpoint_ref(target_session_id, turn_count)
            ));
        }
        if turn_count == 0 {
            continue;
        }
        if let Some(commit) = source.starts.get(&turn_count) {
            commands.push_str(&format!(
                "update {} {commit}\n",
                turn_start_ref(target_session_id, turn_count)
            ));
        }
        if let Some(commit) = source.diff_bases.get(&turn_count) {
            commands.push_str(&format!(
                "update {} {commit}\n",
                turn_diff_base_ref(target_session_id, turn_count)
            ));
        }
    }
    update_refs(cwd, commands)
}

#[derive(Default)]
struct SessionCheckpointRefs {
    turns: HashMap<usize, String>,
    starts: HashMap<usize, String>,
    diff_bases: HashMap<usize, String>,
}

/// Every checkpoint ref for `session_id`, resolved in one `git for-each-ref`.
fn session_checkpoint_ref_commits(cwd: &Path, session_id: Uuid) -> SessionCheckpointRefs {
    let prefix = format!("refs/waku/session-{session_id}-");
    git_output(
        cwd,
        [
            "for-each-ref",
            "--format=%(refname) %(objectname)",
            &format!("{prefix}*"),
        ],
    )
    .map(|output| parse_session_checkpoint_refs(&prefix, &output))
    .unwrap_or_default()
}

// Deletion cannot interpret a failed inventory as an empty repository. A
// removed workspace or a directory outside Git has no reachable refs to clean.
fn session_checkpoint_ref_commits_strict(
    cwd: &Path,
    session_id: Uuid,
) -> anyhow::Result<SessionCheckpointRefs> {
    match std::fs::metadata(cwd) {
        Ok(_) => {}
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Ok(SessionCheckpointRefs::default());
        }
        Err(error) => return Err(error).context("cannot inspect checkpoint workspace"),
    }
    let prefix = format!("refs/waku/session-{session_id}-");
    let output = crate::command_env::plain_command("git")
        .args([
            "for-each-ref",
            "--format=%(refname) %(objectname)",
            &format!("{prefix}*"),
        ])
        .env("LC_ALL", "C")
        .current_dir(cwd)
        .output();
    let output = match output {
        Ok(output) => output,
        Err(error)
            if error.kind() == std::io::ErrorKind::NotFound
                && !may_have_checkpoint_repository(cwd)? =>
        {
            // Git is optional for ordinary chat directories. Only accept the
            // missing executable when there is no repository to leave behind.
            return Ok(SessionCheckpointRefs::default());
        }
        Err(error) => return Err(error).context("failed to enumerate session checkpoint refs"),
    };
    if !output.status.success() {
        let error = command_error(&output);
        if error.starts_with("fatal: not a git repository") {
            // A broken or inaccessible checkout must not masquerade as a
            // plain directory merely because Git cannot open its metadata.
            if may_have_checkpoint_repository(cwd)? {
                bail!("could not enumerate session checkpoint refs: {error}");
            }
            return Ok(SessionCheckpointRefs::default());
        }
        bail!("could not enumerate session checkpoint refs: {error}");
    }
    Ok(parse_session_checkpoint_refs(
        &prefix,
        &String::from_utf8_lossy(&output.stdout),
    ))
}

// This is a conservative fallback when Git cannot identify the repository,
// not a replacement for Git discovery. Include worktree gitfiles, symlinks,
// bare repositories and explicit repository environment overrides.
fn may_have_checkpoint_repository(cwd: &Path) -> anyhow::Result<bool> {
    if ["GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR"]
        .iter()
        .any(|name| std::env::var_os(name).is_some_and(|value| !value.is_empty()))
    {
        return Ok(true);
    }
    let canonical = fs::canonicalize(cwd).context("cannot resolve checkpoint workspace")?;
    for directory in canonical.ancestors() {
        for marker in [".git", "HEAD", "objects", "refs"] {
            match fs::symlink_metadata(directory.join(marker)) {
                Ok(_) => return Ok(true),
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
                Err(error) => return Err(error).context("cannot inspect checkpoint repository"),
            }
        }
    }
    Ok(false)
}

fn parse_session_checkpoint_refs(prefix: &str, output: &str) -> SessionCheckpointRefs {
    let mut refs = SessionCheckpointRefs::default();
    for line in output.lines() {
        let Some((refname, commit)) = line.trim().split_once(' ') else {
            continue;
        };
        let Some(suffix) = refname.strip_prefix(prefix) else {
            continue;
        };
        let target = if let Some(turn_count) = suffix.strip_prefix("turn-start-") {
            turn_count
                .parse()
                .ok()
                .map(|turn_count| (&mut refs.starts, turn_count))
        } else if let Some(turn_count) = suffix.strip_prefix("turn-diff-") {
            turn_count
                .parse()
                .ok()
                .map(|turn_count| (&mut refs.diff_bases, turn_count))
        } else if let Some(turn_count) = suffix.strip_prefix("turn-") {
            turn_count
                .parse()
                .ok()
                .map(|turn_count| (&mut refs.turns, turn_count))
        } else {
            None
        };
        if let Some((target, turn_count)) = target {
            target.insert(turn_count, commit.to_owned());
        }
    }
    refs
}

/// Applies a batch of ref updates through one `git update-ref --stdin`.
///
/// Deleting a fifty-turn session's checkpoints was fifty `git` invocations run
/// serially on the thread that had just been asked to remove the session. The
/// batch form is one process, and it is atomic: either the whole set applies or
/// none of it does. Deleting a ref that is already gone is not an error.
///
/// Checkpoint ref names are generated, never user text, so they cannot contain
/// the space or newline this line-oriented format delimits on.
fn update_refs(cwd: &Path, commands: String) -> anyhow::Result<()> {
    if commands.is_empty() {
        return Ok(());
    }
    git_with_input(
        crate::command_env::plain_command("git")
            .args(["update-ref", "--stdin"])
            .current_dir(cwd),
        commands.as_bytes(),
    )
}

fn git_with_input(command: &mut Command, input: &[u8]) -> anyhow::Result<()> {
    let mut child = command
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .spawn()
        .context("failed to execute git")?;
    let mut stdin = child.stdin.take().context("git stdin is unavailable")?;
    std::thread::scope(|scope| {
        let writer = scope.spawn(move || stdin.write_all(input));
        let output = child.wait_with_output().context("failed to execute git")?;
        if !output.status.success() {
            bail!("{}", command_error(&output));
        }
        writer
            .join()
            .expect("git stdin writer panicked")
            .context("failed to send input to git")
    })
}

fn diff_files(cwd: &Path, from_ref: &str, to_ref: &str) -> anyhow::Result<Vec<CheckpointFile>> {
    let output = git_output(cwd, ["diff", "--numstat", from_ref, to_ref, "--", "."])?;
    let mut files = Vec::new();
    for line in output.lines().filter(|line| !line.trim().is_empty()) {
        let mut columns = line.splitn(3, '\t');
        let additions = columns.next().unwrap_or("0").parse().unwrap_or(0);
        let deletions = columns.next().unwrap_or("0").parse().unwrap_or(0);
        let Some(path) = columns.next() else {
            continue;
        };
        files.push(CheckpointFile {
            path: path.to_owned(),
            additions,
            deletions,
        });
    }
    Ok(files)
}

fn is_git_repository(cwd: &Path) -> bool {
    crate::command_env::plain_command("git")
        .args(["rev-parse", "--is-inside-work-tree"])
        .current_dir(cwd)
        .output()
        .is_ok_and(|output| output.status.success())
}

fn symbolic_head(cwd: &Path) -> Option<String> {
    let output = crate::command_env::plain_command("git")
        .args(["symbolic-ref", "--quiet", "HEAD"])
        .current_dir(cwd)
        .output()
        .ok()?;
    output
        .status
        .success()
        .then(|| String::from_utf8_lossy(&output.stdout).trim().to_owned())
        .filter(|branch| !branch.is_empty())
}

fn repository_refs(cwd: &Path) -> anyhow::Result<BTreeMap<String, String>> {
    let output = git_output(
        cwd,
        [
            "for-each-ref",
            "--format=%(refname)%09%(objecttype)%09%(objectname)%09%(*objecttype)%09%(*objectname)",
            "refs/heads",
            "refs/remotes",
            "refs/tags",
        ],
    )?;
    Ok(output
        .lines()
        .filter_map(|line| {
            let mut fields = line.trim().split('\t');
            let refname = fields.next()?;
            let object_type = fields.next()?;
            let object = fields.next()?;
            let peeled_type = fields.next().unwrap_or_default();
            let peeled = fields.next().unwrap_or_default();
            let commit = if object_type == "commit" {
                object
            } else if peeled_type == "commit" {
                peeled
            } else {
                return None;
            };
            (!refname.is_empty() && !commit.is_empty())
                .then(|| (refname.to_owned(), commit.to_owned()))
        })
        .collect())
}

fn has_head(cwd: &Path) -> bool {
    crate::command_env::plain_command("git")
        .args(["rev-parse", "--verify", "HEAD"])
        .current_dir(cwd)
        .output()
        .is_ok_and(|output| output.status.success())
}

fn resolve_ref(cwd: &Path, git_ref: &str) -> Option<String> {
    let output = crate::command_env::plain_command("git")
        .args(["rev-parse", "--verify", &format!("{git_ref}^{{commit}}")])
        .current_dir(cwd)
        .output()
        .ok()?;
    output
        .status
        .success()
        .then(|| String::from_utf8_lossy(&output.stdout).trim().to_owned())
        .filter(|value| !value.is_empty())
}

fn git_output<I, S>(cwd: &Path, args: I) -> anyhow::Result<String>
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
{
    Ok(String::from_utf8_lossy(&git_output_bytes(cwd, args)?).into_owned())
}

fn git_output_bytes<I, S>(cwd: &Path, args: I) -> anyhow::Result<Vec<u8>>
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
{
    let output = crate::command_env::plain_command("git")
        .args(args)
        .current_dir(cwd)
        .output()
        .context("failed to execute git")?;
    if output.status.success() {
        Ok(output.stdout)
    } else {
        bail!("{}", command_error(&output))
    }
}

fn git_with_index<I, S>(cwd: &Path, index: &Path, args: I) -> anyhow::Result<String>
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
{
    git_with_environment(cwd, index, args, false)
}

fn git_with_identity_and_index<I, S>(cwd: &Path, index: &Path, args: I) -> anyhow::Result<String>
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
{
    git_with_environment(cwd, index, args, true)
}

fn git_with_environment<I, S>(
    cwd: &Path,
    index: &Path,
    args: I,
    identity: bool,
) -> anyhow::Result<String>
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
{
    let mut command = crate::command_env::plain_command("git");
    command
        .args(args)
        .current_dir(cwd)
        .env("GIT_INDEX_FILE", index);
    if identity {
        command
            .env("GIT_AUTHOR_NAME", "Waku")
            .env("GIT_AUTHOR_EMAIL", "waku@localhost")
            .env("GIT_COMMITTER_NAME", "Waku")
            .env("GIT_COMMITTER_EMAIL", "waku@localhost");
    }
    let output = command.output().context("failed to execute git")?;
    if output.status.success() {
        Ok(String::from_utf8_lossy(&output.stdout).into_owned())
    } else {
        bail!("{}", command_error(&output))
    }
}

fn command_error(output: &Output) -> String {
    let stderr = String::from_utf8_lossy(&output.stderr).trim().to_owned();
    if stderr.is_empty() {
        format!("git exited with {}", output.status)
    } else {
        stderr
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn session_ref_deletion_without_git_distinguishes_plain_and_git_workspaces() {
        const FIXTURE: &str = "WAKU_TEST_CHECKPOINT_NO_GIT_ROOT";
        if let Some(root) = std::env::var_os(FIXTURE) {
            let root = PathBuf::from(root);
            let error = crate::command_env::plain_command("git")
                .arg("--version")
                .output()
                .expect_err("fixture must not have Git available");
            assert_eq!(error.kind(), std::io::ErrorKind::NotFound);
            let session = Uuid::new_v4();
            delete_all_session_refs(&root.join("plain"), session).unwrap();
            for name in ["checkout", "bare", "worktree"] {
                assert!(
                    delete_all_session_refs(&root.join(name), session).is_err(),
                    "{name} must remain retryable while Git is unavailable"
                );
            }
            return;
        }
        let root = std::env::temp_dir().join(format!("waku-no-git-{}", Uuid::new_v4()));
        for name in ["plain", "checkout", "bare", "worktree", "empty-path"] {
            fs::create_dir_all(root.join(name)).unwrap();
        }
        git_ok(&root.join("checkout"), &["init", "--quiet"]);
        git_ok(&root.join("bare"), &["init", "--quiet", "--bare"]);
        fs::write(root.join("worktree/.git"), b"gitdir: missing-checkout\n").unwrap();
        let output = crate::command_env::plain_command(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "checkpoint::tests::session_ref_deletion_without_git_distinguishes_plain_and_git_workspaces",
                "--nocapture",
            ])
            .env(FIXTURE, &root)
            .env("PATH", root.join("empty-path"))
            .env_remove("GIT_DIR")
            .env_remove("GIT_WORK_TREE")
            .env_remove("GIT_COMMON_DIR")
            .current_dir(&root)
            .output()
            .unwrap();
        fs::remove_dir_all(&root).unwrap();
        assert!(
            output.status.success(),
            "{}\n{}",
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr)
        );
    }

    #[test]
    fn strict_session_ref_deletion_preserves_failures_and_allows_missing_workspaces() {
        let directory = diverged_repository();
        let session = Uuid::new_v4();
        capture_turn(&directory, session, 0).unwrap();
        let reference = checkpoint_ref(session, 0);
        assert!(has_ref(&directory, &reference));
        let config_path = directory.join(".git/config");
        let config = fs::read(&config_path).unwrap();
        fs::write(&config_path, b"[invalid-config\n").unwrap();
        assert!(
            delete_all_session_refs(&directory, session).is_err(),
            "failed enumeration must not confirm cleanup"
        );
        fs::write(&config_path, config).unwrap();
        assert!(has_ref(&directory, &reference));
        let lock = directory.join(".git").join(format!("{reference}.lock"));
        fs::write(&lock, b"fixture lock").unwrap();
        assert!(
            delete_all_session_refs(&directory, session).is_err(),
            "failed ref deletion must remain retryable"
        );
        assert!(has_ref(&directory, &reference));
        fs::remove_file(lock).unwrap();
        delete_all_session_refs(&directory, session).unwrap();
        assert!(!has_ref(&directory, &reference));
        delete_all_session_refs(&directory.join("removed-workspace"), session).unwrap();
        fs::remove_dir_all(&directory).unwrap();
        // A project directory removed outside Waku cannot trap local deletion.
        delete_all_session_refs(&directory, session).unwrap();
        fs::create_dir_all(&directory).unwrap();
        delete_all_session_refs(&directory, session).unwrap();
        fs::remove_dir_all(&directory).unwrap();
    }

    fn git_ok(cwd: &Path, args: &[&str]) {
        let status = crate::command_env::plain_command("git")
            .args(args)
            .current_dir(cwd)
            .status()
            .unwrap();
        assert!(status.success(), "git {args:?} failed");
    }

    fn git_text(cwd: &Path, args: &[&str]) -> String {
        let output = crate::command_env::plain_command("git")
            .args(args)
            .current_dir(cwd)
            .output()
            .unwrap();
        assert!(output.status.success(), "git {args:?} failed");
        String::from_utf8_lossy(&output.stdout).trim().to_owned()
    }

    fn diverged_repository() -> PathBuf {
        let directory = std::env::temp_dir().join(format!("waku-checkpoints-{}", Uuid::new_v4()));
        fs::create_dir_all(&directory).unwrap();
        git_ok(&directory, &["init", "--quiet", "--initial-branch=main"]);
        git_ok(&directory, &["config", "user.name", "Waku Test"]);
        git_ok(&directory, &["config", "user.email", "waku@example.com"]);
        fs::write(directory.join("shared.txt"), "shared\n").unwrap();
        git_ok(&directory, &["add", "shared.txt"]);
        git_ok(&directory, &["commit", "--quiet", "-m", "baseline"]);

        git_ok(&directory, &["switch", "--quiet", "-c", "feature"]);
        fs::write(directory.join("feature-only.txt"), "feature\n").unwrap();
        fs::write(directory.join("target.txt"), "feature baseline\n").unwrap();
        git_ok(&directory, &["add", "feature-only.txt", "target.txt"]);
        git_ok(&directory, &["commit", "--quiet", "-m", "feature"]);

        git_ok(&directory, &["switch", "--quiet", "main"]);
        fs::write(directory.join("main-only.txt"), "main\n").unwrap();
        git_ok(&directory, &["add", "main-only.txt"]);
        git_ok(&directory, &["commit", "--quiet", "-m", "main"]);
        directory
    }

    #[test]
    fn guarded_restore_rejects_manual_edits_and_preserves_ignored_files() {
        let directory = diverged_repository();
        git_ok(&directory, &["config", "core.autocrlf", "false"]);
        fs::write(directory.join(".gitignore"), "private-cache/\n").unwrap();
        let baseline = capture_worktree_commit(&directory).unwrap();
        fs::write(directory.join("tool-output.txt"), "tool-created\n").unwrap();
        fs::write(directory.join("shared.txt"), "agent edit\n").unwrap();
        let expected = capture_worktree_commit(&directory).unwrap();
        fs::write(directory.join("human-note.txt"), "keep this\n").unwrap();
        assert!(restore_ref_if_unchanged(&directory, &baseline, &expected).is_err());
        assert_eq!(
            fs::read_to_string(directory.join("shared.txt")).unwrap(),
            "agent edit\n"
        );
        fs::remove_file(directory.join("human-note.txt")).unwrap();
        fs::create_dir_all(directory.join("private-cache")).unwrap();
        fs::write(directory.join("private-cache/keep.txt"), "private\n").unwrap();
        restore_ref_if_unchanged(&directory, &baseline, &expected).unwrap();
        assert!(!directory.join("tool-output.txt").exists());
        assert_eq!(
            fs::read_to_string(directory.join("shared.txt")).unwrap(),
            "shared\n"
        );
        assert_eq!(
            fs::read_to_string(directory.join("private-cache/keep.txt")).unwrap(),
            "private\n"
        );
        fs::remove_dir_all(directory).ok();
    }

    #[test]
    fn wholly_ignored_workspace_has_no_turn_checkpoints() {
        let directory = diverged_repository();
        let workspace = directory.join("dist/projects/chat");
        fs::create_dir_all(&workspace).unwrap();
        fs::write(directory.join(".gitignore"), "/dist/\n").unwrap();
        fs::write(workspace.join("output.txt"), "generated\n").unwrap();

        let session = Uuid::new_v4();
        capture_turn_start(&workspace, session, 1).unwrap();
        let turn = capture_turn(&workspace, session, 1).unwrap();
        assert_eq!(turn.status, CheckpointStatus::Unavailable);
        assert!(session_turn_refs(&directory, session).is_empty());
        assert!(!has_ref(&directory, &turn_start_ref(session, 1)));
        assert!(git_text(&directory, &["diff", "--cached", "--name-only"]).is_empty());
        fs::remove_dir_all(directory).ok();
    }

    #[test]
    fn ignore_exceptions_are_captured_at_the_root_and_in_a_subdirectory() {
        let directory = diverged_repository();
        let workspace = directory.join("nested");
        fs::create_dir(&workspace).unwrap();
        fs::write(
            directory.join(".gitignore"),
            "*\n!root.txt\n!nested/\n!nested/keep.txt\n",
        )
        .unwrap();
        fs::write(directory.join("root.txt"), "include\n").unwrap();
        fs::write(workspace.join("keep.txt"), "include\n").unwrap();
        fs::write(workspace.join("ignored.txt"), "skip\n").unwrap();

        for (cwd, expected) in [
            (&directory, "nested/keep.txt\nroot.txt"),
            (&workspace, "nested/keep.txt"),
        ] {
            let snapshot = capture_worktree_commit(cwd).unwrap();
            assert_eq!(
                git_text(&directory, &["diff", "--name-only", "HEAD", &snapshot]),
                expected
            );
        }
        fs::remove_dir_all(directory).ok();
    }

    #[test]
    fn ignored_workspace_snapshots_only_its_tracked_files() {
        let directory = diverged_repository();
        let workspace = directory.join("dist");
        fs::create_dir(&workspace).unwrap();
        fs::write(workspace.join("old.txt"), "old\n").unwrap();
        git_ok(&directory, &["add", "-f", "--", "dist/old.txt"]);
        git_ok(&directory, &["commit", "--quiet", "-m", "track old file"]);
        fs::write(directory.join(".gitignore"), "/dist/\n").unwrap();
        fs::write(workspace.join("output.txt"), "generated\n").unwrap();
        fs::write(directory.join("shared.txt"), "outside the workspace\n").unwrap();

        let session = Uuid::new_v4();
        capture_turn_start(&workspace, session, 1).unwrap();
        git_ok(&workspace, &["rm", "--quiet", "old.txt"]);
        fs::write(workspace.join("new file.txt"), "staged\n").unwrap();
        git_ok(&workspace, &["add", "-f", "--", "new file.txt"]);
        fs::write(workspace.join("new file.txt"), "working copy\n").unwrap();
        let user_index = git_text(&directory, &["write-tree"]);

        let turn = capture_turn(&workspace, session, 1).unwrap();
        assert_eq!(turn.status, CheckpointStatus::Ready);
        assert_eq!(
            git_text(
                &directory,
                &["ls-tree", "-r", "--name-only", &turn.git_ref, "--", "dist"]
            ),
            "dist/new file.txt"
        );
        assert_eq!(
            git_text(
                &directory,
                &["show", &format!("{}:dist/new file.txt", turn.git_ref)]
            ),
            "working copy"
        );
        assert_eq!(
            git_text(
                &directory,
                &["show", &format!("{}:shared.txt", turn.git_ref)]
            ),
            "shared"
        );
        assert_eq!(git_text(&directory, &["write-tree"]), user_index);
        fs::remove_dir_all(directory).ok();
    }

    #[test]
    fn session_turn_refs_lists_the_sessions_checkpoints_in_one_call() {
        let directory = std::env::temp_dir().join(format!("waku-checkpoints-{}", Uuid::new_v4()));
        fs::create_dir_all(&directory).unwrap();
        git_ok(&directory, &["init", "--quiet"]);
        fs::write(directory.join("tracked.txt"), "baseline\n").unwrap();
        git_ok(&directory, &["add", "tracked.txt"]);
        git_ok(
            &directory,
            &[
                "-c",
                "user.name=Waku Test",
                "-c",
                "user.email=waku@example.com",
                "commit",
                "--quiet",
                "-m",
                "baseline",
            ],
        );

        let session = Uuid::new_v4();
        let other = Uuid::new_v4();
        capture_turn(&directory, session, 0).unwrap();
        capture_turn(&directory, session, 2).unwrap();
        capture_turn(&directory, other, 5).unwrap();

        assert_eq!(
            session_turn_refs(&directory, session),
            HashSet::from([0, 2])
        );
        assert_eq!(session_turn_refs(&directory, other), HashSet::from([5]));
        assert!(session_turn_refs(&directory, Uuid::new_v4()).is_empty());
        fs::remove_dir_all(&directory).ok();
    }

    /// Deletes and copies go through one batched `git update-ref --stdin`
    /// rather than a process per turn, so the semantics the loop used to give
    /// for free — gaps are skipped, an already-missing ref is not an error —
    /// are worth pinning down.
    #[test]
    fn refs_are_deleted_and_copied_in_batches() {
        let directory = std::env::temp_dir().join(format!("waku-checkpoints-{}", Uuid::new_v4()));
        fs::create_dir_all(&directory).unwrap();
        git_ok(&directory, &["init", "--quiet"]);
        fs::write(directory.join("tracked.txt"), "baseline\n").unwrap();
        git_ok(&directory, &["add", "tracked.txt"]);
        git_ok(
            &directory,
            &[
                "-c",
                "user.name=Waku Test",
                "-c",
                "user.email=waku@example.com",
                "commit",
                "--quiet",
                "-m",
                "baseline",
            ],
        );

        let session = Uuid::new_v4();
        capture_turn(&directory, session, 0).unwrap();
        capture_turn_start(&directory, session, 1).unwrap();
        capture_turn(&directory, session, 1).unwrap();
        capture_turn(&directory, session, 3).unwrap();
        capture_ref(&directory, &turn_start_ref(session, 3)).unwrap();
        capture_ref(&directory, &turn_diff_base_ref(session, 3)).unwrap();

        // Turn 2 was never captured; the batch must tolerate the gap.
        let fork = Uuid::new_v4();
        copy_session_refs(&directory, session, fork, 3).unwrap();
        assert_eq!(
            session_turn_refs(&directory, fork),
            HashSet::from([0, 1, 3]),
            "a copy carries every ref the source actually has"
        );
        assert_eq!(
            resolve_ref(&directory, &checkpoint_ref(fork, 1)),
            resolve_ref(&directory, &checkpoint_ref(session, 1)),
            "and points at the same commit"
        );
        assert_eq!(
            resolve_ref(&directory, &turn_start_ref(fork, 1)),
            resolve_ref(&directory, &turn_start_ref(session, 1)),
            "the turn's distinct starting snapshot is copied too"
        );
        assert_eq!(
            resolve_ref(&directory, &turn_diff_base_ref(fork, 1)),
            resolve_ref(&directory, &turn_diff_base_ref(session, 1)),
            "the branch-aware review base is copied too"
        );

        delete_turn_refs_after(&directory, session, 1, 3).unwrap();
        assert_eq!(
            session_turn_refs(&directory, session),
            HashSet::from([0, 1]),
            "everything after the retained turn goes, missing ones included"
        );
        assert!(!has_ref(&directory, &turn_start_ref(session, 3)));
        assert!(!has_ref(&directory, &turn_diff_base_ref(session, 3)));

        delete_session_refs(&directory, fork, 3).unwrap();
        assert!(
            session_turn_refs(&directory, fork).is_empty(),
            "and a session's whole set goes in one call"
        );
        assert!(!has_ref(&directory, &turn_start_ref(fork, 1)));
        assert!(!has_ref(&directory, &turn_diff_base_ref(fork, 1)));

        // Nothing left to remove is a no-op, not a failure.
        delete_session_refs(&directory, fork, 3).unwrap();
        fs::remove_dir_all(&directory).ok();
    }

    #[test]
    fn captures_diffs_and_restores_tracked_and_untracked_files() {
        let directory = std::env::temp_dir().join(format!("waku-checkpoints-{}", Uuid::new_v4()));
        fs::create_dir_all(&directory).unwrap();
        git_ok(&directory, &["init", "--quiet"]);
        git_ok(&directory, &["config", "core.autocrlf", "false"]);
        fs::write(directory.join("tracked.txt"), "baseline\n").unwrap();
        git_ok(&directory, &["add", "tracked.txt"]);
        git_ok(
            &directory,
            &[
                "-c",
                "user.name=Waku Test",
                "-c",
                "user.email=waku@example.com",
                "commit",
                "--quiet",
                "-m",
                "baseline",
            ],
        );

        let session_id = Uuid::new_v4();
        let baseline = capture_turn(&directory, session_id, 0).unwrap();
        assert_eq!(baseline.status, CheckpointStatus::Ready);

        fs::write(directory.join("tracked.txt"), "changed\n").unwrap();
        fs::write(directory.join("new.txt"), "new\n").unwrap();
        fs::write(directory.join("already-staged.txt"), "staged\n").unwrap();
        git_ok(&directory, &["add", "already-staged.txt"]);
        let turn = capture_turn(&directory, session_id, 1).unwrap();
        assert_eq!(turn.files.len(), 3);
        assert!(turn.totals_are_current());
        assert_eq!(
            turn.additions,
            turn.files.iter().map(|file| file.additions).sum::<u64>()
        );
        assert_eq!(
            turn.deletions,
            turn.files.iter().map(|file| file.deletions).sum::<u64>()
        );
        assert_eq!(
            git_text(&directory, &["diff", "--cached", "--name-only"]),
            "already-staged.txt"
        );

        let fork_session_id = Uuid::new_v4();
        copy_session_refs(&directory, session_id, fork_session_id, 1).unwrap();
        assert_eq!(
            resolve_ref(&directory, &checkpoint_ref(fork_session_id, 0)),
            resolve_ref(&directory, &checkpoint_ref(session_id, 0))
        );
        assert_eq!(
            resolve_ref(&directory, &checkpoint_ref(fork_session_id, 1)),
            resolve_ref(&directory, &checkpoint_ref(session_id, 1))
        );

        fs::write(directory.join("tracked.txt"), "later\n").unwrap();
        fs::remove_file(directory.join("new.txt")).unwrap();
        fs::write(directory.join("discard.txt"), "discard\n").unwrap();
        restore_ref(&directory, &turn.git_ref).unwrap();

        assert_eq!(
            fs::read_to_string(directory.join("tracked.txt")).unwrap(),
            "changed\n"
        );
        assert_eq!(
            fs::read_to_string(directory.join("new.txt")).unwrap(),
            "new\n"
        );
        assert!(!directory.join("discard.txt").exists());

        fs::remove_dir_all(directory).ok();
    }

    #[test]
    fn switching_to_an_existing_branch_does_not_report_its_history_as_turn_changes() {
        let directory = diverged_repository();
        let session_id = Uuid::new_v4();
        capture_turn_start(&directory, session_id, 1).unwrap();

        git_ok(&directory, &["switch", "--quiet", "feature"]);
        let turn = capture_turn(&directory, session_id, 1).unwrap();

        assert!(
            turn.files.is_empty(),
            "a branch switch alone is not a file edit: {:?}",
            turn.files
        );
        fs::remove_dir_all(directory).ok();
    }

    #[test]
    fn switching_between_preexisting_detached_commits_does_not_report_their_history() {
        let directory = diverged_repository();
        let older = git_text(&directory, &["rev-parse", "main^1"]);
        let newer = git_text(&directory, &["rev-parse", "main"]);
        git_ok(&directory, &["switch", "--quiet", "--detach", &older]);
        let session_id = Uuid::new_v4();
        capture_turn_start(&directory, session_id, 1).unwrap();

        git_ok(&directory, &["switch", "--quiet", "--detach", &newer]);
        let turn = capture_turn(&directory, session_id, 1).unwrap();

        assert!(
            turn.files.is_empty(),
            "a detached checkout alone is not a file edit: {:?}",
            turn.files
        );
        fs::remove_dir_all(directory).ok();
    }

    #[test]
    fn branch_switch_reports_only_changes_made_on_the_target_branch() {
        let directory = diverged_repository();
        let session_id = Uuid::new_v4();
        capture_turn_start(&directory, session_id, 1).unwrap();

        git_ok(&directory, &["switch", "--quiet", "feature"]);
        fs::write(directory.join("target.txt"), "changed during turn\n").unwrap();
        git_ok(&directory, &["add", "target.txt"]);
        git_ok(&directory, &["commit", "--quiet", "-m", "turn change"]);
        fs::write(directory.join("untracked.txt"), "new during turn\n").unwrap();
        let turn = capture_turn(&directory, session_id, 1).unwrap();

        let paths = turn
            .files
            .iter()
            .map(|file| file.path.as_str())
            .collect::<HashSet<_>>();
        assert_eq!(paths, HashSet::from(["target.txt", "untracked.txt"]));
        assert!(!paths.contains("feature-only.txt"));
        assert!(!paths.contains("main-only.txt"));
        fs::remove_dir_all(directory).ok();
    }

    #[test]
    fn a_branch_created_during_the_turn_uses_its_creation_point_as_the_diff_base() {
        let directory = diverged_repository();
        let session_id = Uuid::new_v4();
        capture_turn_start(&directory, session_id, 1).unwrap();

        git_ok(&directory, &["switch", "--quiet", "-c", "new-branch"]);
        fs::write(directory.join("shared.txt"), "changed on new branch\n").unwrap();
        git_ok(&directory, &["add", "shared.txt"]);
        git_ok(
            &directory,
            &["commit", "--quiet", "-m", "new branch change"],
        );
        let turn = capture_turn(&directory, session_id, 1).unwrap();

        assert_eq!(turn.files.len(), 1);
        assert_eq!(turn.files[0].path, "shared.txt");
        fs::remove_dir_all(directory).ok();
    }

    #[test]
    fn dirty_files_carried_across_a_branch_switch_remain_part_of_the_starting_state() {
        let directory = diverged_repository();
        fs::write(directory.join("shared.txt"), "already dirty\n").unwrap();
        let session_id = Uuid::new_v4();
        capture_turn_start(&directory, session_id, 1).unwrap();

        git_ok(&directory, &["switch", "--quiet", "feature"]);
        let turn = capture_turn(&directory, session_id, 1).unwrap();

        assert!(
            turn.files.is_empty(),
            "pre-turn dirty state carried by Git is not a response edit: {:?}",
            turn.files
        );
        fs::remove_dir_all(directory).ok();
    }

    #[test]
    fn each_turn_uses_its_own_start_snapshot() {
        let directory = diverged_repository();
        let session_id = Uuid::new_v4();
        capture_turn_start(&directory, session_id, 1).unwrap();
        fs::write(directory.join("first-turn.txt"), "first\n").unwrap();
        let first = capture_turn(&directory, session_id, 1).unwrap();
        assert_eq!(first.files.len(), 1);

        fs::write(directory.join("between-turns.txt"), "external\n").unwrap();
        capture_turn_start(&directory, session_id, 2).unwrap();
        fs::write(directory.join("second-turn.txt"), "second\n").unwrap();
        let second = capture_turn(&directory, session_id, 2).unwrap();

        assert_eq!(second.files.len(), 1);
        assert_eq!(second.files[0].path, "second-turn.txt");
        fs::remove_dir_all(directory).ok();
    }
}
