use super::{AgentSession, MessageRole, Path, PathBuf, ProviderKind, normalized_path};

fn snapshot_url(target: &str) -> bool {
    let Some(path) = target.strip_prefix("file://") else {
        return false;
    };
    let Some((_, blob)) = path.rsplit_once("/assets/") else {
        return false;
    };
    blob.strip_suffix(".blob").is_some_and(|id| {
        (20..=128).contains(&id.len())
            && id
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'-'))
    })
}

/// Old ACP resource links were emitted on their own line with an angle-bracket
/// destination. Match that encoding to the successful present_files activity
/// at this message position. A filename alone cannot identify a nested output.
pub(super) fn cached_output_path(
    session: &AgentSession,
    workspace: &Path,
    target: &str,
) -> Option<PathBuf> {
    if session.provider != ProviderKind::DeerFlow || !snapshot_url(target) {
        return None;
    }
    let (position, message) = session.messages.iter().enumerate().find(|(_, message)| {
        message.role == MessageRole::Assistant && message.content.contains(target)
    })?;
    let links: Vec<_> = message
        .content
        .lines()
        .filter_map(|line| {
            let (label, uri) = line
                .strip_prefix('[')?
                .strip_suffix(">)")?
                .rsplit_once("](<")?;
            snapshot_url(uri).then_some((label, uri))
        })
        .collect();
    let index = links.iter().position(|(_, uri)| *uri == target)?;
    let output_root = workspace.join(".deerflow").join("outputs");
    let mut paths = Vec::new();
    for activity in session
        .transcript_blocks
        .iter()
        .filter(|block| block.after_message == position && block.turn_id == message.turn_id)
        .flat_map(|block| &block.activities)
        .filter(|activity| {
            activity.complete
                && !activity.failed
                && activity.tool_name.as_deref().unwrap_or(&activity.title) == "present_files"
        })
    {
        let args: serde_json::Value = serde_json::from_str(activity.arguments.as_ref()?).ok()?;
        let mut seen = std::collections::HashSet::new();
        for value in args.get("paths")?.as_array()? {
            let path = value.as_str()?;
            let path = if let Some(relative) = path.strip_prefix("/mnt/user-data/outputs/") {
                output_root.join(relative)
            } else if Path::new(path).starts_with(Path::new(".deerflow").join("outputs")) {
                workspace.join(path)
            } else {
                output_root.join(path)
            };
            let path = normalized_path(&path);
            if !path.starts_with(&output_root) || path == output_root {
                return None;
            }
            if seen.insert(path.clone()) {
                paths.push(path);
            }
        }
    }
    // The store de-duplicates each call in input order. Check every label
    // before relying on that order, including when two basenames are equal.
    if paths.len() != links.len()
        || !paths.iter().zip(&links).all(|(path, (label, _))| {
            path.file_name()
                .and_then(|name| name.to_str())
                .is_some_and(|name| {
                    name.replace('\\', "\\\\")
                        .replace('[', "\\[")
                        .replace(']', "\\]")
                        .replace(['\n', '\r'], " ")
                        == *label
                })
        })
    {
        return None;
    }
    Some(paths.swap_remove(index))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::{ActivityItem, ActivityKind, Message, TranscriptBlock};
    use uuid::Uuid;

    #[test]
    fn cached_artifact_links_recover_the_presented_paths_from_the_same_turn() {
        let root = Path::new(env!("CARGO_MANIFEST_DIR"));
        let outputs = root.join(".deerflow").join("outputs");
        let turn = Uuid::new_v4();
        let first = "file:///C:/cache/assets/AAAAAAAAAAAAAAAAAAAAAAAAAA.blob";
        let second = "file:///C:/cache/assets/BBBBBBBBBBBBBBBBBBBBBBBBBB.blob";
        let third = "file:///C:/cache/assets/CCCCCCCCCCCCCCCCCCCCCCCCCC.blob";
        let mut session = AgentSession::new(Uuid::new_v4(), ProviderKind::DeerFlow);
        session.messages.push(Message::new_for_turn(MessageRole::Assistant, format!(
            "[传国玺.html](<{first}>)\n\n[图 \\[1\\].png](<{second}>)\n\n[图 \\[1\\].png](<{third}>)"
        ), turn));
        let activity = ActivityItem::new(None, ActivityKind::Tool, "present_files", None, true)
            .with_arguments(Some(serde_json::json!({"paths": [
                "传国玺.html", "传国玺.html", "/mnt/user-data/outputs/a/图 [1].png", ".deerflow/outputs/b/图 [1].png"
            ]}).to_string()));
        session.transcript_blocks.push(TranscriptBlock {
            after_message: 0,
            turn_id: Some(turn),
            activities: vec![activity],
        });
        assert_eq!(
            cached_output_path(&session, root, first),
            Some(outputs.join("传国玺.html"))
        );
        assert_eq!(
            cached_output_path(&session, root, second),
            Some(outputs.join("a/图 [1].png"))
        );
        assert_eq!(
            cached_output_path(&session, root, third),
            Some(outputs.join("b/图 [1].png"))
        );
        session.transcript_blocks[0].turn_id = Some(Uuid::new_v4());
        assert_eq!(cached_output_path(&session, root, third), None);
        session.transcript_blocks[0].turn_id = Some(turn);
        session.transcript_blocks[0].activities[0].arguments = Some(
            serde_json::json!({"paths":["../传国玺.html","a/图 [1].png","b/图 [1].png"]})
                .to_string(),
        );
        assert_eq!(cached_output_path(&session, root, first), None);
    }
}
