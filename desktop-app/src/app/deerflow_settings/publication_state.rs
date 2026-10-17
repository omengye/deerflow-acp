//! Publication feedback is independent of transport success and approval intent.
use serde_json::{Value, json};

pub(super) fn notice(proposal: &Value) -> &'static str {
    match proposal["publication"]["state"].as_str() {
        Some("published") => "提案已发布并完成安装；启用状态以技能详情为准。",
        Some("approved_pending") => "批准已保存，发布尚未完成。可重试原发布请求，无需重新审核。",
        Some("needs_reconciliation" | "publishing") => {
            "批准已保存，发布事务尚未完成。重试原发布可继续恢复；不要删除或覆盖源文件。"
        }
        Some("blocked") => match proposal["publication"]["error_code"].as_str() {
            Some("target_content_differs") => {
                "已记录对候选版本的批准，但目标目录含不同或额外文件，未发布。可基于当前目录创建新的审核版本。"
            }
            Some("base_changed") => {
                "批准针对的基础版本已变化，未覆盖现有技能。请基于当前版本重新准备提案。"
            }
            _ => "批准已记录，但发布被阻止。请核对错误及源目录，不要重复覆盖文件。",
        },
        _ if proposal["status"] == "published" => "提案已批准并发布。",
        _ => "发布尚未确认完成，请核对提案状态。",
    }
}

pub(super) fn retry_request(proposal: &Value) -> Option<Value> {
    if proposal["publication"]["retryable"] != true {
        return None;
    }
    let review = &proposal["approval"]["review"];
    let state = review["ExpectedStateVersion"].as_u64().filter(|v| *v > 0)?;
    Some(
        json!({"operation":"proposal.publish_retry", "proposal_id":proposal["id"],
        "expected_approval_version":state}),
    )
}

pub(super) fn can_review_source(proposal: &Value) -> bool {
    proposal["action"] == "create"
        && matches!(
            proposal["status"].as_str(),
            Some("stale" | "failed" | "rejected")
        )
        && proposal["file_publication_pending"] != true
}

pub(super) fn source_confirmation(proposal: &Value, request: &Value) -> bool {
    can_review_source(proposal)
        && request["operation"] == "proposal.source_review"
        && proposal["id"].as_str().is_some_and(|id| !id.is_empty())
        && request["proposal_id"] == proposal["id"]
        && request["expected_state_version"] == proposal["state_version"]
}

pub(super) fn review_actions(proposal: &Value) -> (bool, bool) {
    if !proposal["approval"].is_null() {
        return (false, false);
    }
    match proposal["status"].as_str() {
        Some("pending_review") => (true, true),
        Some("validating") => (false, true),
        _ => (false, false),
    }
}

pub(super) fn review_hint(proposal: &Value) -> &'static str {
    if !proposal["approval"].is_null() {
        return notice(proposal);
    }
    match proposal["status"].as_str() {
        Some("stale") => {
            "这是已失效的旧提案，不能发布或再次拒绝。审核当前目录会创建新候选，不会把旧提案改回待审核。"
        }
        Some("failed") => "此提案检查失败，不能发布。请查看失败原因；修正后需提交新的候选。",
        Some("rejected") => "此提案已拒绝，不再接受发布或重复拒绝。",
        Some("validating") => {
            "新候选正在检查，尚不能发布；可拒绝此候选。稍后刷新此提案查看检查结果。"
        }
        Some("pending_review") => "此候选已完成必要检查。确认发布后将立即尝试安装；无需另行导入。",
        Some("published") => "此提案已发布，无需重复确认。",
        _ => "请核对当前提案状态；仅检查完成的候选可以确认发布。",
    }
}

pub(super) fn source_review_error(code: &str) -> &'static str {
    match code {
        "source_credentials_require_local_handling" => {
            "未创建新候选：当前目录含疑似凭据，未发送给审核模型，原文件未修改。请先在本地将真实凭据迁至技能包外的环境变量或安全配置，并更新技能读取方式，再刷新此提案重新审核。不要仅删除配置文件或把密钥粘贴到审核内容中。"
        }
        "source_invalid_or_unavailable" | "source_unavailable" => {
            "未创建新候选：源目录无法安全读取或结构无效，原文件未修改。修正后刷新此提案再试。"
        }
        "source_changed_while_reading" => {
            "未创建新候选：读取期间源文件发生变化。请完成文件编辑后刷新此提案再试。"
        }
        "conflict" | "permission_denied" | "not_found" => {
            "未确认创建新候选：来源、权限或提案版本已变化。请先刷新此提案核对当前状态。"
        }
        _ => {
            "未确认创建新候选，请查看操作错误。若请求结果不明，只能重试原请求核对，不能将旧提案当作新候选发布。"
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn approved_is_not_published_and_retry_keeps_original_review() {
        let mut proposal = json!({"id":"p_one","status":"publishing","state_version":3,
            "approval":{"review":{"ExpectedStateVersion":2,"Actor":"desktop","Note":"checked"}},
            "publication":{"state":"needs_reconciliation","retryable":true}});
        assert!(notice(&proposal).contains("尚未完成"));
        let retry = retry_request(&proposal).unwrap();
        assert_eq!(retry["operation"], "proposal.publish_retry");
        assert_eq!(retry["expected_approval_version"], 2);
        assert!(retry.get("note").is_none());
        proposal["approval"]["review"]["Actor"] = json!("auto_patch");
        assert_eq!(retry_request(&proposal).unwrap(), retry);
        proposal["publication"] =
            json!({"state":"blocked","error_code":"target_content_differs","retryable":false});
        assert!(notice(&proposal).contains("不同或额外文件"));
        assert!(retry_request(&proposal).is_none());
        proposal["action"] = json!("create");
        assert!(!can_review_source(&proposal));
        proposal["status"] = json!("stale");
        assert!(can_review_source(&proposal));
    }

    #[test]
    fn source_confirmation_belongs_beside_the_exact_old_proposal() {
        let mut old = json!({"id":"p_old","action":"create","status":"stale","state_version":3});
        let request = json!({"operation":"proposal.source_review","proposal_id":"p_old","expected_state_version":3});
        assert!(source_confirmation(&old, &request));
        assert_eq!(review_actions(&old), (false, false));
        assert!(review_hint(&old).contains("旧提案"));
        old["state_version"] = json!(4);
        assert!(!source_confirmation(&old, &request));
        old["id"] = json!("p_new");
        old["status"] = json!("validating");
        assert!(!source_confirmation(&old, &request));
        assert_eq!(review_actions(&old), (false, true));
        old["status"] = json!("pending_review");
        assert_eq!(review_actions(&old), (true, true));
        old["approval"] = json!({"review":{"ExpectedStateVersion":4}});
        assert_eq!(review_actions(&old), (false, false));
    }
}
