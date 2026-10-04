use serde_json::{Value, json};

pub(super) fn scope_label(scope: &str) -> &str {
    match scope {
        "global" => "全局",
        "workspace" => "项目",
        "session" => "会话",
        "user" => "旧版用户（项目内）",
        _ => scope,
    }
}

pub(super) fn get_request(scope: &str, session: Option<&str>) -> Option<Value> {
    let mut request = json!({"operation":"memory.get", "scope":scope});
    match scope {
        "global" => {}
        "workspace" | "session" | "user" => {
            request["session_id"] = Value::from(session.filter(|id| !id.is_empty())?);
        }
        _ => return None,
    }
    Some(request)
}

pub(super) fn mutation_request(
    operation: &str,
    scope: &str,
    session: Option<&str>,
    fact: &str,
    revision: i64,
) -> Option<Value> {
    if revision < 1
        || fact.is_empty()
        || !matches!(operation, "memory.delete" | "memory.promote")
        || (operation == "memory.promote" && scope == "global")
    {
        return None;
    }
    let mut request = get_request(scope, session)?;
    request["operation"] = Value::from(operation);
    request["fact_id"] = Value::from(fact);
    request["expected_revision"] = Value::from(revision);
    Some(request)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn memory_management_scope_and_reviewed_revision() {
        let global = get_request("global", Some("old-session")).unwrap();
        assert!(global.get("session_id").is_none());
        assert!(get_request("workspace", None).is_none());
        let promote = mutation_request("memory.promote", "workspace", Some("s1"), "f1", 3).unwrap();
        assert_eq!(promote["session_id"], "s1");
        assert_eq!(promote["scope"], "workspace");
        assert_eq!(promote["expected_revision"], 3);
        assert!(mutation_request("memory.promote", "global", None, "f1", 3).is_none());
        assert!(mutation_request("memory.delete", "global", None, "f1", 0).is_none());
        let delete = mutation_request("memory.delete", "global", None, "f1", 3).unwrap();
        assert!(delete.get("session_id").is_none());
    }
}
