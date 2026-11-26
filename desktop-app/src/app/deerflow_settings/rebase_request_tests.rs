use super::*;

fn base_document() -> Value {
    json!({
        "config_revision":"old", "extensions_revision":"extensions",
        "config_schema":{"version":"v1", "hash":"schema"},
        "config_migration_ack":"", "skill_evolution_migration_ack":"",
        "models":[{"original_name":"first", "name":"first", "provider":"openai",
            "model":"first", "base_url":"https://example.test/v1", "api_key":"", "clear_api_key":false}],
        "tools":[{"name":"host_opencli", "executable_path":"old-opencli.cmd"}]
    })
}

#[test]
fn mcp_only_save_uses_live_settings_and_mixed_edits_still_apply_together() {
    let mut base = base_document();
    base["client_mcp_servers"] = json!([]);
    base["client_mcp_servers_revision"] = json!("old-mcp");
    for mixed in [false, true] {
        let mut draft = base.clone();
        draft["client_mcp_servers"] = json!([{"name":"docs","command":"local","env":[]}]);
        if mixed {
            draft["models"][0]["model"] = json!("updated");
        }
        let result = request_deerflow_with_draft("save-and-apply", draft.clone(), Some(&base), |op, input| {
            if op == "snapshot" { return Ok(base.clone()); }
            if mixed {
                assert_eq!(op, "save-and-apply");
                assert_eq!(input, draft);
                return Ok(json!({}));
            }
            assert_eq!(op, "save-mcp");
            assert_eq!(input["client_mcp_servers_revision"], "old-mcp");
            assert_eq!(input["client_mcp_servers"], draft["client_mcp_servers"]);
            Ok(json!({"client_mcp_servers":input["client_mcp_servers"], "client_mcp_servers_revision":"new-mcp"}))
        }).unwrap();
        if !mixed {
            assert_eq!(result["mcp_applied"], true);
            assert_eq!(result["document"]["client_mcp_servers_revision"], "new-mcp");
            assert_eq!(result["document"]["models"], base["models"]);
        }
    }
}

#[test]
fn saves_fetch_latest_snapshot_before_submitting_the_merged_draft() {
    for operation in ["save", "save-and-apply", "validate"] {
        let base = base_document();
        let mut draft = base.clone();
        draft["models"].as_array_mut().unwrap().push(json!({
            "original_name":"", "name":"new-model", "provider":"openai",
            "model":"new-model", "api_key":"new-model-key"
        }));
        let original_draft = draft.clone();
        let mut latest = base.clone();
        latest["config_revision"] = json!("current");
        latest["tools"][0]["executable_path"] = json!("new-opencli.cmd");
        let mut events = Vec::new();
        let result = request_deerflow_with_draft(
            operation,
            draft.clone(),
            Some(&base),
            |requested, input| {
                events.push(requested.to_owned());
                if requested == "snapshot" {
                    assert_eq!(input, json!({}));
                    return Ok(latest.clone());
                }
                assert_eq!(requested, operation);
                assert_eq!(input["config_revision"], "current");
                assert_eq!(input["tools"][0]["executable_path"], "new-opencli.cmd");
                assert_eq!(input["models"], draft["models"]);
                Ok(json!({"completed":true}))
            },
        )
        .unwrap();
        assert_eq!(result, json!({"completed":true}));
        assert_eq!(events, ["snapshot", operation]);
        assert_eq!(draft, original_draft);
    }
}

#[test]
fn conflicts_and_preflight_failures_never_submit_or_replace_the_draft() {
    for snapshot_fails in [false, true] {
        let base = base_document();
        let mut draft = base.clone();
        draft["default_model"] = json!("local");
        let original_draft = draft.clone();
        let mut latest = base.clone();
        latest["config_revision"] = json!("current");
        latest["default_model"] = json!("external");
        let mut requests = 0;
        let result = request_deerflow_with_draft("save", draft.clone(), Some(&base), |op, _| {
            requests += 1;
            assert_eq!(op, "snapshot");
            if snapshot_fails {
                anyhow::bail!("snapshot unavailable");
            }
            Ok(latest.clone())
        });
        assert!(result.is_err());
        assert_eq!(requests, 1);
        assert_eq!(draft, original_draft);
    }
}

#[test]
fn concurrent_write_after_preflight_is_not_retried() {
    let base = base_document();
    let mut events = Vec::new();
    let error =
        request_deerflow_with_draft("save-and-apply", base.clone(), Some(&base), |op, _| {
            events.push(op.to_owned());
            if op == "snapshot" {
                return Ok(base.clone());
            }
            anyhow::bail!("configuration changed after snapshot");
        })
        .unwrap_err();
    assert_eq!(error.to_string(), "configuration changed after snapshot");
    assert_eq!(events, ["snapshot", "save-and-apply"]);
}

#[test]
fn missing_base_fails_closed_and_other_operations_do_not_fetch_a_snapshot() {
    assert!(
        request_deerflow_with_draft("save", json!({}), None, |_, _| {
            panic!("must not submit a draft without its original snapshot")
        })
        .is_err()
    );
    let mut events = Vec::new();
    let input = json!({"operation":"skill.catalog"});
    let result = request_deerflow_with_draft("manage", input.clone(), None, |op, request| {
        events.push(op.to_owned());
        assert_eq!(request, input);
        Ok(json!({"items":[]}))
    })
    .unwrap();
    assert_eq!(events, ["manage"]);
    assert_eq!(result, json!({"items":[]}));
}
