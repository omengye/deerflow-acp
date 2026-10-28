use serde_json::{Map, Value};
use std::collections::BTreeSet;

pub(super) fn rebase_document(
    base: &Value,
    draft: &Value,
    latest: &Value,
) -> Result<Value, String> {
    let (Some(base), Some(draft), Some(latest)) =
        (base.as_object(), draft.as_object(), latest.as_object())
    else {
        return Err(conflict("/"));
    };
    for document in [base, draft, latest] {
        for revision in ["config_revision", "extensions_revision"] {
            if document
                .get(revision)
                .and_then(Value::as_str)
                .is_none_or(str::is_empty)
            {
                return Err(conflict(&pointer("", revision)));
            }
        }
    }
    if base.get("paths") != latest.get("paths") {
        return Err(conflict("/paths"));
    }
    if draft.get("config_schema") != base.get("config_schema")
        || latest.get("config_schema") != base.get("config_schema")
    {
        return Err("配置字段定义已变化；草稿已保留，请重新加载后核对。".into());
    }
    for (acknowledgment, migration) in [
        ("config_migration_ack", "config_migration"),
        ("skill_evolution_migration_ack", "skill_evolution_migration"),
    ] {
        validate_acknowledgment(draft, latest, acknowledgment, migration)?;
    }
    if latest.get("client_mcp_servers_revision") != base.get("client_mcp_servers_revision")
        && draft.get("client_mcp_servers") != base.get("client_mcp_servers")
    {
        // Redacted environment values can change without changing the visible list.
        return Err(conflict("/client_mcp_servers"));
    }
    if latest.get("config_revision") != base.get("config_revision") {
        validate_model_changes(base, draft)?;
        // Tools also contain masked credentials and proxy values. An unchanged
        // visible list does not prove those hidden values have stayed the same.
        if draft.get("tools") != base.get("tools") {
            return Err(conflict("/tools"));
        }
    }

    let mut result = Map::new();
    for key in keys(base, draft, latest) {
        let value = if server_metadata(key) {
            latest.get(key).cloned()
        } else {
            merge_value(
                base.get(key),
                draft.get(key),
                latest.get(key),
                &pointer("", key),
            )?
        };
        if let Some(value) = value {
            result.insert(key.to_owned(), value);
        }
    }
    Ok(Value::Object(result))
}

fn server_metadata(key: &str) -> bool {
    matches!(
        key,
        "config_revision"
            | "extensions_revision"
            | "config_schema"
            | "paths"
            | "backup"
            | "config_migration"
            | "skill_evolution_migration"
            | "client_mcp_servers_revision"
    )
}

fn validate_acknowledgment(
    draft: &Map<String, Value>,
    latest: &Map<String, Value>,
    acknowledgment: &str,
    migration: &str,
) -> Result<(), String> {
    let Some(value) = draft.get(acknowledgment) else {
        return Ok(());
    };
    let Some(acknowledged) = value.as_str() else {
        return Err(conflict(&pointer("", acknowledgment)));
    };
    if !acknowledged.is_empty()
        && (latest.get("config_revision").and_then(Value::as_str) != Some(acknowledged)
            || latest
                .get(migration)
                .and_then(|migration| migration.get("source_revision"))
                .and_then(Value::as_str)
                != Some(acknowledged))
    {
        return Err(conflict(&pointer("", acknowledgment)));
    }
    Ok(())
}

fn validate_model_changes(
    base: &Map<String, Value>,
    draft: &Map<String, Value>,
) -> Result<(), String> {
    if base.get("models") == draft.get("models") {
        return Ok(());
    }
    let (Some(previous), Some(models)) = (
        base.get("models").and_then(Value::as_array),
        draft.get("models").and_then(Value::as_array),
    ) else {
        return Err(conflict("/models"));
    };
    // Existing credentials are hidden in snapshots. Only untouched backend
    // identities may reuse secrets from the latest YAML during a stale rebase.
    for old in previous {
        let original = old["original_name"].as_str().unwrap_or("");
        if original.is_empty() {
            return Err(conflict("/models"));
        }
        let mut matching = models
            .iter()
            .filter(|model| model["original_name"].as_str() == Some(original));
        let Some(model) = matching.next() else {
            return Err(conflict("/models"));
        };
        if matching.next().is_some() {
            return Err(conflict("/models"));
        }
        for key in [
            "name",
            "original_name",
            "provider",
            "base_url",
            "api_key",
            "clear_api_key",
        ] {
            if model.get(key) != old.get(key) {
                return Err(conflict("/models"));
            }
        }
    }
    for model in models {
        match model["original_name"].as_str() {
            Some("") => {
                if previous
                    .iter()
                    .any(|old| old.get("name") == model.get("name"))
                {
                    return Err(conflict("/models"));
                }
            }
            Some(original)
                if previous
                    .iter()
                    .any(|old| old["original_name"].as_str() == Some(original)) => {}
            _ => return Err(conflict("/models")),
        }
    }
    Ok(())
}

fn keys<'a>(
    base: &'a Map<String, Value>,
    draft: &'a Map<String, Value>,
    latest: &'a Map<String, Value>,
) -> BTreeSet<&'a str> {
    base.keys()
        .chain(draft.keys())
        .chain(latest.keys())
        .map(String::as_str)
        .collect()
}

fn merge_value(
    base: Option<&Value>,
    draft: Option<&Value>,
    latest: Option<&Value>,
    path: &str,
) -> Result<Option<Value>, String> {
    if draft == base {
        return Ok(latest.cloned());
    }
    if latest == base || draft == latest {
        return Ok(draft.cloned());
    }
    if let (Some(Value::Object(base)), Some(Value::Object(draft)), Some(Value::Object(latest))) =
        (base, draft, latest)
    {
        let mut result = Map::new();
        for key in keys(base, draft, latest) {
            if let Some(value) = merge_value(
                base.get(key),
                draft.get(key),
                latest.get(key),
                &pointer(path, key),
            )? {
                result.insert(key.to_owned(), value);
            }
        }
        return Ok(Some(Value::Object(result)));
    }
    // Arrays are atomic: an index is not a stable model, tool, or MCP identity.
    Err(conflict(path))
}

fn pointer(parent: &str, key: &str) -> String {
    format!("{parent}/{}", key.replace('~', "~0").replace('/', "~1"))
}

fn conflict(path: &str) -> String {
    format!("配置字段 {path} 与外部修改冲突；草稿已保留，请重新加载后核对。")
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn document() -> Value {
        json!({
            "config_revision": "base", "extensions_revision": "extensions-base",
            "config_schema": {"version": "v1", "hash": "schema-base"},
            "paths": {"config": "old-path"}, "backup": "old-backup",
            "config_migration": {"required": false, "source_revision": "base"},
            "config_migration_ack": "",
            "skill_evolution_migration": {"required": false, "source_revision": "base"},
            "skill_evolution_migration_ack": "",
            "models": [{
                "original_name": "first", "name": "first", "provider": "openai",
                "model": "gpt-test", "base_url": "https://example.test/v1",
                "api_key": "", "clear_api_key": false, "display_name": "First"
            }],
            "tools": [{"name": "opencli", "command": "old-command"}],
            "runtime": {"max_model_calls": 10, "max_tool_calls": 20},
            "client_mcp_servers": [{"name": "mcp", "env": [{"name": "KEY", "value": ""}]}],
            "client_mcp_servers_revision": "mcp-base"
        })
    }

    fn external_change(base: &Value) -> Value {
        let mut latest = base.clone();
        latest["config_revision"] = json!("latest");
        latest["config_migration"]["source_revision"] = json!("latest");
        latest["skill_evolution_migration"]["source_revision"] = json!("latest");
        latest
    }

    fn add_model(document: &mut Value) {
        document["models"].as_array_mut().unwrap().push(json!({
            "original_name": "", "name": "new-model", "provider": "openai",
            "model": "new-model-id", "base_url": "https://new.test/v1",
            "api_key": "new-private-key", "clear_api_key": false
        }));
    }

    #[test]
    fn new_model_merges_external_tool_repair_with_current_metadata() {
        let base = document();
        let mut draft = base.clone();
        add_model(&mut draft);
        let mut latest = external_change(&base);
        latest["tools"][0]["command"] = json!("repaired-command");
        latest["extensions_revision"] = json!("extensions-latest");
        latest.as_object_mut().unwrap().remove("backup");
        let merged = rebase_document(&base, &draft, &latest).unwrap();
        assert_eq!(merged["models"], draft["models"]);
        assert_eq!(merged["tools"], latest["tools"]);
        for key in [
            "config_revision",
            "extensions_revision",
            "paths",
            "config_migration",
            "skill_evolution_migration",
            "client_mcp_servers_revision",
            "config_schema",
        ] {
            assert_eq!(merged[key], latest[key], "{key}");
        }
        assert!(merged.get("backup").is_none());
    }

    #[test]
    fn metadata_only_comes_from_latest_even_when_main_revision_matches() {
        let base = document();
        let mut draft = base.clone();
        draft["paths"] = json!({"config": "stale-draft-path"});
        draft["config_revision"] = json!("not-a-new-token");
        let mut latest = base.clone();
        latest["extensions_revision"] = json!("new-extension-token");
        assert_eq!(rebase_document(&base, &draft, &latest).unwrap(), latest);
    }

    #[test]
    fn unchanged_draft_adopts_latest_without_reverting_external_edits() {
        let base = document();
        let mut latest = external_change(&base);
        latest["models"][0]["display_name"] = json!("External name");
        latest["runtime"]["max_model_calls"] = json!(50);
        assert_eq!(rebase_document(&base, &base, &latest).unwrap(), latest);
    }

    #[test]
    fn independent_nested_fields_merge_and_identical_desired_values_are_allowed() {
        let base = document();
        let mut draft = base.clone();
        draft["runtime"]["max_model_calls"] = json!(30);
        let mut latest = external_change(&base);
        latest["runtime"]["max_tool_calls"] = json!(60);
        let merged = rebase_document(&base, &draft, &latest).unwrap();
        assert_eq!(
            merged["runtime"],
            json!({"max_model_calls": 30, "max_tool_calls": 60})
        );
        latest["runtime"]["max_model_calls"] = json!(30);
        assert_eq!(rebase_document(&base, &draft, &latest).unwrap(), latest);
    }

    #[test]
    fn same_field_conflict_reports_only_the_pointer_not_values() {
        let base = document();
        let mut draft = base.clone();
        draft["runtime"]["max_model_calls"] = json!("local-private-value");
        let mut latest = external_change(&base);
        latest["runtime"]["max_model_calls"] = json!("external-private-value");
        let error = rebase_document(&base, &draft, &latest).unwrap_err();
        assert!(error.contains("/runtime/max_model_calls"));
        assert!(!error.contains("local-private-value"));
        assert!(!error.contains("external-private-value"));
    }

    #[test]
    fn arrays_conflict_instead_of_matching_unstable_indexes() {
        let base = document();
        let mut draft = base.clone();
        add_model(&mut draft);
        let mut latest = external_change(&base);
        latest["models"][0]["display_name"] = json!("External name");
        assert!(
            rebase_document(&base, &draft, &latest)
                .unwrap_err()
                .contains("/models")
        );
        draft = base.clone();
        draft["tools"][0]["command"] = json!("local-command");
        latest = external_change(&base);
        latest["tools"]
            .as_array_mut()
            .unwrap()
            .push(json!({"name": "external-tool"}));
        assert!(
            rebase_document(&base, &draft, &latest)
                .unwrap_err()
                .contains("/tools")
        );
    }

    #[test]
    fn identical_concurrent_new_model_lists_do_not_conflict() {
        let base = document();
        let mut draft = base.clone();
        add_model(&mut draft);
        let mut latest = external_change(&base);
        latest["models"] = draft["models"].clone();
        assert_eq!(rebase_document(&base, &draft, &latest).unwrap(), latest);
    }

    #[test]
    fn stale_existing_model_credentials_and_backend_identity_edits_are_refused() {
        let base = document();
        let latest = external_change(&base);
        for (key, value) in [
            ("api_key", json!("replacement-private-key")),
            ("clear_api_key", json!(true)),
            ("provider", json!("claude")),
            ("base_url", json!("https://other.test/v1")),
            ("name", json!("renamed")),
            ("original_name", json!("")),
        ] {
            let mut draft = base.clone();
            draft["models"][0][key] = value;
            let error = rebase_document(&base, &draft, &latest).unwrap_err();
            assert!(error.contains("/models"), "{key}");
            assert!(!error.contains("replacement-private-key"));
            assert!(rebase_document(&base, &draft, &base).is_ok(), "{key}");
        }
    }

    #[test]
    fn stale_local_tool_edits_cannot_overwrite_masked_credentials_or_proxy_values() {
        let base = document();
        let latest = external_change(&base);
        for (key, value) in [
            ("api_key", json!("new-private-key")),
            ("clear_api_key", json!(true)),
            ("https_proxy", json!("private-proxy")),
            ("clear_https_proxy", json!(true)),
            ("command", json!("local-command")),
        ] {
            let mut draft = base.clone();
            draft["tools"][0][key] = value;
            let error = rebase_document(&base, &draft, &latest).unwrap_err();
            assert!(error.contains("/tools"), "{key}");
            assert!(!error.contains("new-private-key"));
            assert!(!error.contains("private-proxy"));
            assert!(rebase_document(&base, &draft, &base).is_ok(), "{key}");
        }
    }

    #[test]
    fn stale_model_removal_and_disguised_identity_replacement_are_refused() {
        let base = document();
        let latest = external_change(&base);
        let mut draft = base.clone();
        draft["models"] = json!([]);
        assert!(rebase_document(&base, &draft, &latest).is_err());
        add_model(&mut draft);
        assert!(rebase_document(&base, &draft, &latest).is_err());
        draft = base.clone();
        add_model(&mut draft);
        draft["models"][1]["name"] = json!("first");
        assert!(rebase_document(&base, &draft, &latest).is_err());
    }

    #[test]
    fn noncredential_model_edit_can_merge_with_an_external_unrelated_change() {
        let base = document();
        let mut draft = base.clone();
        draft["models"][0]["display_name"] = json!("Local display name");
        let latest = external_change(&base);
        assert_eq!(
            rebase_document(&base, &draft, &latest).unwrap()["models"],
            draft["models"]
        );
    }

    #[test]
    fn schema_changes_are_not_silently_promoted() {
        let base = document();
        let mut latest = base.clone();
        latest["config_schema"]["hash"] = json!("new-schema");
        assert!(rebase_document(&base, &base, &latest).is_err());
        let mut draft = base.clone();
        draft["config_schema"] = latest["config_schema"].clone();
        assert!(rebase_document(&base, &draft, &latest).is_err());
        assert!(rebase_document(&base, &draft, &base).is_err());
    }

    #[test]
    fn missing_or_invalid_revision_tokens_are_not_silently_promoted() {
        let base = document();
        for key in ["config_revision", "extensions_revision"] {
            let mut invalid = base.clone();
            invalid.as_object_mut().unwrap().remove(key);
            assert!(rebase_document(&invalid, &base, &base).is_err());
            assert!(rebase_document(&base, &invalid, &base).is_err());
            assert!(rebase_document(&base, &base, &invalid).is_err());
            invalid[key] = json!(42);
            assert!(rebase_document(&base, &base, &invalid).is_err());
            invalid[key] = json!("");
            assert!(rebase_document(&base, &base, &invalid).is_err());
        }
    }

    #[test]
    fn changed_configuration_identity_cannot_receive_an_old_draft() {
        let base = document();
        let mut draft = base.clone();
        add_model(&mut draft);
        let mut latest = external_change(&base);
        latest["paths"]["config"] = json!("different-profile/config.yaml");
        assert!(
            rebase_document(&base, &draft, &latest)
                .unwrap_err()
                .contains("/paths")
        );
        latest.as_object_mut().unwrap().remove("paths");
        assert!(rebase_document(&base, &draft, &latest).is_err());
    }

    #[test]
    fn migration_acknowledgments_are_preserved_only_for_the_reviewed_revision() {
        let base = document();
        for (ack, migration) in [
            ("config_migration_ack", "config_migration"),
            ("skill_evolution_migration_ack", "skill_evolution_migration"),
        ] {
            let mut draft = base.clone();
            draft[ack] = json!("base");
            assert_eq!(rebase_document(&base, &draft, &base).unwrap()[ack], "base");
            let latest = external_change(&base);
            assert!(
                rebase_document(&base, &draft, &latest)
                    .unwrap_err()
                    .contains(ack)
            );
            draft[ack] = json!("");
            assert_eq!(rebase_document(&base, &draft, &latest).unwrap()[ack], "");
            draft[ack] = json!("latest");
            let mut mismatched = latest.clone();
            mismatched[migration]["source_revision"] = json!("different-source");
            assert!(rebase_document(&base, &draft, &mismatched).is_err());
            draft[ack] = json!(true);
            assert!(rebase_document(&base, &draft, &latest).is_err());
        }
    }

    #[test]
    fn independently_changed_mcp_revision_blocks_local_redacted_list_edits() {
        let base = document();
        let mut draft = base.clone();
        draft["client_mcp_servers"][0]["env"][0]["value"] = json!("new-private-env");
        let mut latest = base.clone();
        latest["client_mcp_servers_revision"] = json!("rotated-hidden-env");
        let error = rebase_document(&base, &draft, &latest).unwrap_err();
        assert!(error.contains("/client_mcp_servers"));
        assert!(!error.contains("new-private-env"));
        latest["client_mcp_servers"] = draft["client_mcp_servers"].clone();
        assert!(rebase_document(&base, &draft, &latest).is_err());
        assert_eq!(rebase_document(&base, &base, &latest).unwrap(), latest);
    }

    #[test]
    fn local_mcp_changes_are_allowed_when_only_main_config_changes() {
        let base = document();
        let mut draft = base.clone();
        draft["client_mcp_servers"][0]["env"][0]["value"] = json!("new-private-env");
        let latest = external_change(&base);
        assert_eq!(
            rebase_document(&base, &draft, &latest).unwrap()["client_mcp_servers"],
            draft["client_mcp_servers"]
        );
    }

    #[test]
    fn removed_fields_are_not_confused_with_null_or_external_updates() {
        let base = document();
        let mut draft = base.clone();
        draft["runtime"]
            .as_object_mut()
            .unwrap()
            .remove("max_model_calls");
        let mut latest = external_change(&base);
        latest["runtime"]["max_tool_calls"] = json!(40);
        let merged = rebase_document(&base, &draft, &latest).unwrap();
        assert!(merged["runtime"].get("max_model_calls").is_none());
        assert_eq!(merged["runtime"]["max_tool_calls"], 40);
        latest["runtime"]["max_model_calls"] = Value::Null;
        assert!(rebase_document(&base, &draft, &latest).is_err());
    }

    #[test]
    fn nested_conflict_paths_escape_json_pointer_delimiters() {
        let mut base = document();
        base["custom"] = json!({"a/b~c": "base"});
        let mut draft = base.clone();
        draft["custom"]["a/b~c"] = json!("local");
        let mut latest = external_change(&base);
        latest["custom"]["a/b~c"] = json!("external");
        assert!(
            rebase_document(&base, &draft, &latest)
                .unwrap_err()
                .contains("/custom/a~1b~0c")
        );
    }
}
