//! Cached capability intersection for the sections described by Go's shared
//! settings schema. Evaluating this projection never performs I/O or edits drafts.
use serde_json::Value;
use std::collections::{HashMap, HashSet};

const VERSION: &str = "deerflow-settings-v2";

#[derive(Default)]
pub(super) struct Capabilities {
    allowed: HashSet<String>,
    pub(super) issue: Option<String>,
}

impl Capabilities {
    pub(super) fn read(document: &Value, status: &Value) -> Self {
        let mut result = Self::default();
        let Some(local) = descriptors(&document["config_schema"]) else {
            result.issue = Some("配置工具未提供兼容的字段定义，相关设置暂为只读。请更新配套程序后重新加载；草稿已保留。".into());
            return result;
        };
        let remote = if status["running"] == true {
            match descriptors(&status["config_schema"]) {
                Some(fields) => Some(fields),
                None => {
                    result.issue = Some("运行服务的字段定义不可用或版本不兼容，相关设置暂为只读。请更新配套服务；草稿已保留。".into());
                    return result;
                }
            }
        } else {
            None
        };
        for (path, field) in local {
            if remote
                .as_ref()
                .is_none_or(|fields| fields.get(path) == Some(&field))
            {
                result.allowed.insert(path.to_owned());
            }
        }
        if !result.can_save(document) {
            result.issue = Some("配置工具与运行服务支持的部分字段不一致，相关设置暂为只读。请更新配套程序后重新加载；草稿已保留。".into());
        }
        result
    }

    pub(super) fn allows(&self, path: &str) -> bool {
        // Other sections have independent existing contracts; this partial
        // schema is not evidence that arbitrary settings there are supported.
        if path == "/skill_sources/managed" {
            return self.allowed.contains("/skill_sources")
                && self.allowed.contains(path)
                && SOURCE_FIELDS
                    .iter()
                    .all(|key| self.allowed.contains(&format!("{path}/*{key}")));
        }
        if path.starts_with("/skill_sources/") {
            return self.allowed.contains("/skill_sources")
                && (!path.starts_with("/skill_sources/managed/")
                    || (self.allowed.contains("/skill_sources/managed")
                        && self.allowed.contains("/skill_sources/managed/*")))
                && self.allowed.contains(&schema_path(path));
        }
        if path == "/skill_sources" {
            return false;
        }
        if path == "/tools" {
            return self.allowed.contains(path)
                && TOOL_FIELDS
                    .iter()
                    .all(|key| self.allowed.contains(&format!("/tools/*/{key}")));
        }
        if matches!(path, "/subagents/agents" | "/subagents/custom_agents") {
            return self.allowed.contains(path)
                && ROLE_FIELDS
                    .iter()
                    .all(|key| self.allowed.contains(&format!("{path}/*{key}")));
        }
        let canonical = schema_path(path);
        for group in [
            "validation",
            "generation",
            "budget",
            "discovery",
            "behavior",
            "auto_patch",
        ] {
            let parent = format!("/skill_evolution/{group}");
            if path.starts_with(&format!("{parent}/")) {
                return self.allowed.contains(&parent) && self.allowed.contains(path);
            }
        }
        if let Some((root, child)) = canonical.split_once("/*/") {
            if root.starts_with("/subagents/") {
                return self.allowed.contains(root)
                    && self.allowed.contains(&format!("{root}/*"))
                    && (!child.starts_with("model_settings/")
                        || self.allowed.contains(&format!("{root}/*/model_settings")))
                    && self.allowed.contains(&canonical);
            }
        }
        !scoped(path) || self.allowed.contains(&canonical)
    }

    pub(super) fn can_save(&self, document: &Value) -> bool {
        ["runtime", "memory", "subagents", "sandbox"]
            .into_iter()
            .all(|section| {
                document[section].as_object().is_some_and(|values| {
                    !values.is_empty()
                        && values
                            .keys()
                            .all(|key| self.allowed.contains(&format!("/{section}/{key}")))
                })
            })
            && ["agents", "custom_agents"].into_iter().all(|collection| {
                let Some(roles) = document["subagents"].get(collection) else {
                    return true;
                };
                roles.as_object().is_some_and(|roles| {
                    roles.values().all(|role| {
                        let root = format!("/subagents/{collection}/*");
                        self.allowed.contains(&root)
                            && role.as_object().is_some_and(|fields| {
                                fields.iter().all(|(key, value)| {
                                    let path = format!("{root}/{key}");
                                    self.allowed.contains(&path)
                                        && (key != "model_settings"
                                            || value.is_null()
                                            || value.as_object().is_some_and(|settings| {
                                                settings.keys().all(|name| {
                                                    self.allowed.contains(&format!("{path}/{name}"))
                                                })
                                            }))
                                })
                            })
                    })
                })
            })
            && self.allowed.contains("/default_model")
            && document["skills_enabled"].is_boolean()
            && self.allowed.contains("/skills_enabled")
            && document.get("skills").is_none()
            && self.evolution_can_save(document)
            && self.sources_can_save(document)
            && (document.get("config_migration_ack").is_none()
                || self.allowed.contains("/config_migration_ack"))
            && self.allowed.contains("/models")
            && self.allows("/tools")
            && document["tools"].as_array().is_some_and(|tools| {
                tools.iter().all(|tool| {
                    tool.as_object().is_some_and(|fields| {
                        fields.keys().all(|key| {
                            matches!(
                                key.as_str(),
                                "original_name" | "api_key_configured" | "https_proxy_configured"
                            ) || self.allowed.contains(&format!("/tools/*/{key}"))
                        })
                    })
                })
            })
            && document["models"].as_array().is_some_and(|models| {
                !models.is_empty()
                    && models.iter().all(|model| {
                        model.as_object().is_some_and(|fields| {
                            fields.keys().all(|key| {
                                matches!(
                                    key.as_str(),
                                    "original_name" | "api_key_configured" | "api_key_literal"
                                ) || self.allowed.contains(&format!("/models/*/{key}"))
                            })
                        })
                    })
            })
    }
    fn sources_can_save(&self, document: &Value) -> bool {
        let Some(source) = document["skill_sources"].as_object() else {
            return false;
        };
        SOURCE_SETTINGS
            .iter()
            .all(|key| source.contains_key(*key) && self.allows(&format!("/skill_sources/{key}")))
            && source
                .keys()
                .all(|key| self.allows(&format!("/skill_sources/{key}")))
            && source["managed"].as_array().is_some_and(|items| {
                items.iter().all(|item| {
                    item.as_object().is_some_and(|fields| {
                        SOURCE_FIELDS
                            .iter()
                            .filter(|key| !key.is_empty())
                            .all(|key| fields.contains_key(&key[1..]))
                            && fields
                                .keys()
                                .all(|key| self.allows(&format!("/skill_sources/managed/0/{key}")))
                    })
                })
            })
    }
    fn evolution_can_save(&self, document: &Value) -> bool {
        let Some(fields) = document["skill_evolution"].as_object() else {
            return false;
        };
        EVOLUTION_FIELDS.iter().all(|key| {
            document["skill_evolution"].pointer(key).is_some()
                && self.allows(&format!("/skill_evolution{key}"))
        }) && fields
            .keys()
            .all(|key| self.allows(&format!("/skill_evolution/{key}")))
            && [
                "validation",
                "generation",
                "budget",
                "discovery",
                "behavior",
                "auto_patch",
            ]
            .into_iter()
            .all(|group| {
                document["skill_evolution"][group]
                    .as_object()
                    .is_some_and(|limits| {
                        limits
                            .keys()
                            .all(|key| self.allows(&format!("/skill_evolution/{group}/{key}")))
                    })
            })
            && document["skill_evolution_migration_ack"].is_string()
            && self.allowed.contains("/skill_evolution_migration_ack")
    }
}

fn scoped(path: &str) -> bool {
    path == "/config_migration_ack"
        || path == "/skills_enabled"
        || path == "/skills"
        || path.starts_with("/skills/")
        || path == "/runtime"
        || path == "/memory"
        || path.starts_with("/runtime/")
        || path.starts_with("/memory/")
        || path == "/models"
        || path == "/default_model"
        || path == "/subagents"
        || path.starts_with("/models/")
        || path.starts_with("/subagents/")
        || path == "/tools"
        || path == "/tool_groups"
        || path.starts_with("/tools/")
        || path == "/sandbox"
        || path.starts_with("/sandbox/")
        || path == "/skill_evolution"
        || path.starts_with("/skill_evolution/")
        || path == "/skill_evolution_migration_ack"
        || path == "/skill_sources"
        || path.starts_with("/skill_sources/")
}

fn schema_path(path: &str) -> String {
    let parts: Vec<_> = path.split('/').collect();
    if parts.len() >= 4
        && parts[1] == "skill_sources"
        && parts[2] == "managed"
        && parts[3].parse::<usize>().is_ok()
    {
        return format!(
            "/skill_sources/managed/*{}",
            if parts.len() == 4 {
                String::new()
            } else {
                format!("/{}", parts[4..].join("/"))
            }
        );
    }
    if parts.len() == 4 && parts[1] == "models" && parts[2].parse::<usize>().is_ok() {
        return format!("/models/*/{}", parts[3]);
    }
    if parts.len() == 4 && parts[1] == "tools" && parts[2].parse::<usize>().is_ok() {
        return format!("/tools/*/{}", parts[3]);
    }
    if parts.len() >= 4 && parts[1] == "subagents" && matches!(parts[2], "agents" | "custom_agents")
    {
        return format!(
            "/subagents/{}/*{}",
            parts[2],
            if parts.len() == 4 {
                String::new()
            } else {
                format!("/{}", parts[4..].join("/"))
            }
        );
    }
    path.to_owned()
}

const ROLE_FIELDS: &[&str] = &[
    "",
    "/description",
    "/system_prompt",
    "/model",
    "/tools",
    "/disallowed_tools",
    "/skills",
    "/delegate_to",
    "/timeout_seconds",
    "/max_turns",
    "/thinking_enabled",
    "/reasoning_effort",
    "/model_settings",
    "/model_settings/temperature",
    "/model_settings/max_tokens",
    "/model_settings/thinking_budget_tokens",
];

const SOURCE_SETTINGS: &[&str] = &["import_path", "export_root", "include_global", "managed"];
const SOURCE_FIELDS: &[&str] = &["", "/id", "/root", "/scope", "/workspace"];

const TOOL_FIELDS: &[&str] = &[
    "name",
    "api_key",
    "clear_api_key",
    "https_proxy",
    "clear_https_proxy",
    "timeout",
    "max_results",
    "max_output_chars",
    "executable",
    "allowed_sites",
];

const EVOLUTION_FIELDS: &[&str] = &[
    "/enabled",
    "/mode",
    "/allow_builtin_overlays",
    "/moderation_model_name",
    "/evaluation_model_name",
    "/generation_model_name",
    "/security_fail_closed",
    "/validation",
    "/validation/timeout_seconds",
    "/validation/max_input_bytes",
    "/validation/max_output_tokens",
    "/generation",
    "/generation/enabled",
    "/generation/timeout_seconds",
    "/generation/max_input_bytes",
    "/generation/max_output_tokens",
    "/discovery",
    "/discovery/operator_corrections_enabled",
    "/discovery/missing_reports_enabled",
    "/discovery/successful_workflows_enabled",
    "/discovery/new_skill_source_id",
    "/discovery/repeat_threshold",
    "/discovery/repeat_window_days",
    "/discovery/cooldown_hours",
    "/discovery/max_daily_proposals",
    "/discovery/max_pending_proposals",
    "/behavior",
    "/behavior/enabled",
    "/behavior/suite_path",
    "/behavior/timeout_seconds",
    "/behavior/max_steps",
    "/behavior/max_input_bytes",
    "/behavior/max_output_tokens",
    "/auto_patch",
    "/auto_patch/max_changed_lines",
    "/auto_patch/max_daily_publications",
    "/auto_patch/observation_hours",
    "/auto_patch/min_observations",
    "/auto_patch/rollback_failure_threshold",
    "/budget",
    "/budget/max_daily_model_calls",
    "/budget/max_daily_tokens",
    "/budget/max_job_seconds",
    "/budget/workers",
];

fn expected_type(path: &str) -> Option<&'static str> {
    for root in ["/subagents/agents/*", "/subagents/custom_agents/*"] {
        if let Some(key) = path.strip_prefix(root) {
            return match key {
                "" => Some("agent_role"),
                "/model_settings" => Some("agent_model_settings"),
                "/description" | "/system_prompt" | "/model" | "/reasoning_effort" => {
                    Some("string")
                }
                "/tools" | "/disallowed_tools" => Some("tool_names"),
                "/skills" | "/delegate_to" => Some("role_names"),
                "/timeout_seconds" | "/model_settings/temperature" => Some("number"),
                "/max_turns"
                | "/model_settings/max_tokens"
                | "/model_settings/thinking_budget_tokens" => Some("integer"),
                "/thinking_enabled" => Some("boolean"),
                _ => None,
            };
        }
    }
    match path {
        "/skill_sources" => Some("skill_source_settings"),
        "/skill_sources/managed" => Some("skill_source_catalog"),
        "/skill_sources/managed/*" => Some("skill_source"),
        "/skill_sources/include_global" => Some("boolean"),
        "/skill_sources/import_path"
        | "/skill_sources/export_root"
        | "/skill_sources/managed/*/id"
        | "/skill_sources/managed/*/root"
        | "/skill_sources/managed/*/scope"
        | "/skill_sources/managed/*/workspace" => Some("string"),
        "/skills_enabled" => Some("boolean"),
        "/skill_evolution_migration_ack"
        | "/skill_evolution/mode"
        | "/skill_evolution/moderation_model_name"
        | "/skill_evolution/generation_model_name"
        | "/skill_evolution/evaluation_model_name"
        | "/skill_evolution/discovery/new_skill_source_id"
        | "/skill_evolution/behavior/suite_path" => Some("string"),
        "/skill_evolution/enabled"
        | "/skill_evolution/allow_builtin_overlays"
        | "/skill_evolution/generation/enabled"
        | "/skill_evolution/discovery/operator_corrections_enabled"
        | "/skill_evolution/discovery/missing_reports_enabled"
        | "/skill_evolution/discovery/successful_workflows_enabled"
        | "/skill_evolution/behavior/enabled"
        | "/skill_evolution/security_fail_closed" => Some("boolean"),
        "/skill_evolution/validation" => Some("review_limits"),
        "/skill_evolution/generation" => Some("generation_settings"),
        "/skill_evolution/discovery" => Some("discovery_settings"),
        "/skill_evolution/behavior" => Some("behavior_settings"),
        "/skill_evolution/auto_patch" => Some("auto_patch_settings"),
        "/skill_evolution/budget" => Some("maintenance_budget"),
        "/skill_evolution/validation/timeout_seconds"
        | "/skill_evolution/validation/max_input_bytes"
        | "/skill_evolution/validation/max_output_tokens"
        | "/skill_evolution/generation/timeout_seconds"
        | "/skill_evolution/generation/max_input_bytes"
        | "/skill_evolution/generation/max_output_tokens"
        | "/skill_evolution/discovery/repeat_threshold"
        | "/skill_evolution/discovery/repeat_window_days"
        | "/skill_evolution/discovery/cooldown_hours"
        | "/skill_evolution/discovery/max_daily_proposals"
        | "/skill_evolution/discovery/max_pending_proposals"
        | "/skill_evolution/behavior/timeout_seconds"
        | "/skill_evolution/behavior/max_steps"
        | "/skill_evolution/behavior/max_input_bytes"
        | "/skill_evolution/behavior/max_output_tokens"
        | "/skill_evolution/auto_patch/max_changed_lines"
        | "/skill_evolution/auto_patch/max_daily_publications"
        | "/skill_evolution/auto_patch/observation_hours"
        | "/skill_evolution/auto_patch/min_observations"
        | "/skill_evolution/auto_patch/rollback_failure_threshold"
        | "/skill_evolution/budget/max_daily_model_calls"
        | "/skill_evolution/budget/max_daily_tokens"
        | "/skill_evolution/budget/max_job_seconds"
        | "/skill_evolution/budget/workers" => Some("integer"),
        "/tools" => Some("tool_catalog"),
        "/tools/*/name"
        | "/tools/*/api_key"
        | "/tools/*/https_proxy"
        | "/tools/*/executable"
        | "/sandbox/provider" => Some("string"),
        "/tools/*/clear_api_key"
        | "/tools/*/clear_https_proxy"
        | "/sandbox/allow_host_bash"
        | "/sandbox/allow_host_tools" => Some("boolean"),
        "/tools/*/timeout" | "/tools/*/max_results" | "/tools/*/max_output_chars" => {
            Some("integer")
        }
        "/tools/*/allowed_sites" => Some("opencli_sites"),
        "/config_migration_ack"
        | "/default_model"
        | "/models/*/name"
        | "/models/*/display_name"
        | "/models/*/description"
        | "/models/*/provider"
        | "/models/*/model"
        | "/models/*/api_key"
        | "/models/*/base_url" => Some("string"),
        "/models" => Some("model_catalog"),
        "/models/*/context_window"
        | "/subagents/max_turns"
        | "/subagents/max_concurrent"
        | "/subagents/max_depth"
        | "/subagents/background_workers"
        | "/subagents/max_tasks_per_root"
        | "/subagents/max_auto_continuations" => Some("integer"),
        "/models/*/supports_vision"
        | "/models/*/clear_api_key"
        | "/subagents/enabled"
        | "/subagents/auto_continue" => Some("boolean"),
        "/subagents/timeout_seconds" => Some("number"),
        "/subagents/agents" | "/subagents/custom_agents" => Some("agent_map"),
        "/runtime/model_name"
        | "/runtime/permission_mode"
        | "/runtime/memory_scope"
        | "/runtime/prompt_overlay"
        | "/memory/mode" => Some("string"),
        "/runtime/session_cleanup_enabled"
        | "/runtime/enable_bash"
        | "/memory/enabled"
        | "/memory/injection_enabled"
        | "/memory/retrieval_enabled" => Some("boolean"),
        "/runtime/max_active_connections"
        | "/runtime/max_active_runs"
        | "/runtime/inactive_session_retention_days"
        | "/runtime/closed_session_retention_days" => Some("integer"),
        "/runtime/run_timeout_seconds"
        | "/runtime/queue_timeout_seconds"
        | "/runtime/session_cleanup_interval_seconds" => Some("number"),
        "/runtime/tool_allowlist" | "/runtime/tool_denylist" => Some("string_array"),
        _ => None,
    }
}

fn descriptors(schema: &Value) -> Option<HashMap<&str, &Value>> {
    if schema["version"] != VERSION || schema["hash"].as_str().is_none_or(str::is_empty) {
        return None;
    }
    let sections = schema["sections"].as_array()?;
    if !["runtime", "memory"]
        .into_iter()
        .all(|section| sections.iter().any(|item| item == section))
    {
        return None;
    }
    let mut fields = HashMap::new();
    let mut seen = HashSet::new();
    for field in schema["fields"].as_array()? {
        let path = field["path"].as_str()?;
        if !seen.insert(path) {
            return None;
        }
        let Some(kind) = expected_type(path) else {
            continue;
        };
        let operations = field["operations"].as_array()?;
        if field["type"] != kind
            || field["apply"] != "restart"
            || field["hash"].as_str().is_none_or(str::is_empty)
            || !["validate", "save"]
                .into_iter()
                .all(|op| operations.iter().any(|item| item == op))
        {
            continue;
        }
        fields.insert(path, field);
    }
    Some(fields)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn document() -> Value {
        let mut doc = json!({"runtime":{"max_active_runs":3},"memory":{"enabled":false},"subagents":{"enabled":false},"tools":[],"sandbox":{"provider":"local","allow_host_bash":false,"allow_host_tools":false},"models":[{"name":"example","provider":"openai","original_name":"example"}],"default_model":"example","config_schema":{
        "version":VERSION,"hash":"fixture-schema","sections":["runtime","memory"],"fields":[
            {"path":"/runtime/max_active_runs","type":"integer","apply":"restart","operations":["validate","save"],"hash":"runs-v1"},
            {"path":"/memory/enabled","type":"boolean","apply":"restart","operations":["validate","save"],"hash":"memory-v1"},
            {"path":"/subagents/enabled","type":"boolean","apply":"restart","operations":["validate","save"],"hash":"agents-v1"},
            {"path":"/models","type":"model_catalog","apply":"restart","operations":["validate","save"],"hash":"models-v1"},
            {"path":"/models/*/name","type":"string","apply":"restart","operations":["validate","save"],"hash":"name-v1"},
            {"path":"/models/*/provider","type":"string","apply":"restart","operations":["validate","save"],"hash":"provider-v1"},
            {"path":"/default_model","type":"string","apply":"restart","operations":["validate","save"],"hash":"default-v1"}
        ]}});
        doc["skill_evolution"] = json!({"enabled":false,"mode":"review","allow_builtin_overlays":false,
            "moderation_model_name":"","evaluation_model_name":"","generation_model_name":"","security_fail_closed":true,
            "validation":{"timeout_seconds":0,"max_input_bytes":0,"max_output_tokens":0},
            "generation":{"enabled":false,"timeout_seconds":0,"max_input_bytes":0,"max_output_tokens":0},
            "discovery":{"operator_corrections_enabled":false,"missing_reports_enabled":false,"successful_workflows_enabled":false,"new_skill_source_id":"","repeat_threshold":2,"repeat_window_days":30,"cooldown_hours":24,"max_daily_proposals":5,"max_pending_proposals":20},
            "auto_patch":{"max_changed_lines":40,"max_daily_publications":2,"observation_hours":72,"min_observations":3,"rollback_failure_threshold":2},
            "behavior":{"enabled":false,"suite_path":"","timeout_seconds":60,"max_steps":6,"max_input_bytes":131072,"max_output_tokens":1024},
            "budget":{"max_daily_model_calls":30,"max_daily_tokens":100000,"max_job_seconds":300,"workers":1}});
        doc["skill_evolution_migration_ack"] = json!("");
        doc["skills_enabled"] = json!(true);
        doc["skill_sources"] =
            json!({"import_path":"../skills","export_root":"","include_global":null,"managed":[]});
        let fields = doc["config_schema"]["fields"].as_array_mut().unwrap();
        for path in std::iter::once("/skill_sources".to_owned())
            .chain(
                SOURCE_SETTINGS
                    .iter()
                    .map(|key| format!("/skill_sources/{key}")),
            )
            .chain(
                SOURCE_FIELDS
                    .iter()
                    .map(|key| format!("/skill_sources/managed/*{key}")),
            )
        {
            let kind = expected_type(&path).unwrap();
            fields.push(json!({"path":path,"type":kind,"apply":"restart","operations":["validate","save"],"hash":"sources-v1"}));
        }
        for path in std::iter::once("/skill_evolution_migration_ack".to_owned()).chain(
            EVOLUTION_FIELDS
                .iter()
                .map(|key| format!("/skill_evolution{key}")),
        ) {
            let kind = expected_type(&path).unwrap();
            fields.push(json!({"path":path,"type":kind,"apply":"restart","operations":["validate","save"],"hash":"review-v1"}));
        }
        for path in [
            "/skills_enabled",
            "/tools",
            "/sandbox/provider",
            "/sandbox/allow_host_bash",
            "/sandbox/allow_host_tools",
        ]
        .into_iter()
        .map(str::to_owned)
        .chain(TOOL_FIELDS.iter().map(|key| format!("/tools/*/{key}")))
        {
            let kind = expected_type(&path).unwrap();
            fields.push(json!({"path":path,"type":kind,"apply":"restart","operations":["validate","save"],"hash":"tools-v1"}));
        }
        doc
    }

    #[test]
    fn deerflow_settings_stopped_uses_config_tool_contract() {
        let doc = document();
        let contract = Capabilities::read(&doc, &json!({"running":false}));
        assert!(contract.can_save(&doc));
        assert!(contract.allows("/runtime/max_active_runs"));
        assert!(contract.allows("/skills_enabled"));
        assert!(!contract.allows("/skills/0/enabled"));
        assert!(!contract.allows("/skills"));
        assert!(!contract.allows("/runtime/goal_auto_continue"));
        assert!(!contract.allows("/memory/advanced"));
        assert!(!contract.allows("/runtime"));
    }

    #[test]
    fn deerflow_source_edits_require_complete_parent_and_nested_contracts() {
        let mut doc = document();
        doc["skill_sources"]["managed"] =
            json!([{"id":"generated","root":"../skills","scope":"workspace","workspace":".."}]);
        let original = doc.clone();
        let full = Capabilities::read(&doc, &json!({"running":false}));
        assert!(full.can_save(&doc));
        assert!(full.allows("/skill_sources/managed"));
        assert!(full.allows("/skill_sources/managed/0/scope"));
        for path in [
            "/skill_sources",
            "/skill_sources/advanced",
            "/skill_sources/managed/0/api_key",
        ] {
            assert!(!full.allows(path));
        }
        for path in [
            "/skill_sources",
            "/skill_sources/managed",
            "/skill_sources/managed/*",
            "/skill_sources/managed/*/scope",
        ] {
            for remove in [true, false] {
                let mut status = json!({"running":true,"config_schema":doc["config_schema"]});
                let fields = status["config_schema"]["fields"].as_array_mut().unwrap();
                if remove {
                    fields.retain(|field| field["path"] != path);
                } else {
                    fields
                        .iter_mut()
                        .find(|field| field["path"] == path)
                        .unwrap()["hash"] = json!("source-policy-changed");
                }
                let partial = Capabilities::read(&doc, &status);
                assert!(!partial.can_save(&doc), "{path}");
                assert!(!partial.allows("/skill_sources/managed"), "{path}");
                assert!(!partial.allows("/skill_sources/managed/0/scope"), "{path}");
                assert!(partial.allows("/skill_evolution/enabled"));
            }
        }
        assert_eq!(doc, original);
        doc["skill_sources"]["managed"][0]["advanced"] = json!({});
        assert!(!full.can_save(&doc));
        let mut missing = original;
        missing["skill_sources"]["managed"][0]
            .as_object_mut()
            .unwrap()
            .remove("workspace");
        assert!(!full.can_save(&missing));
    }

    #[test]
    fn deerflow_evolution_requires_complete_matching_nested_contract() {
        let doc = document();
        let original = doc.clone();
        let stopped = Capabilities::read(&doc, &json!({"running":false}));
        assert!(stopped.can_save(&doc));
        for path in [
            "/skill_evolution",
            "/skill_evolution/advanced",
            "/skill_evolution/discovery/enabled",
            "/skill_evolution/validation/unknown",
            "/skill_evolution/generation/unknown",
            "/skill_evolution/budget/unknown",
            "/skill_evolution/discovery/unknown",
            "/skill_evolution/behavior/unknown",
            "/skill_evolution/auto_patch/unknown",
        ] {
            assert!(!stopped.allows(path), "{path}");
        }
        for path in [
            "/skill_evolution/validation",
            "/skill_evolution/validation/max_input_bytes",
            "/skill_evolution/generation",
            "/skill_evolution/generation/enabled",
            "/skill_evolution/generation/max_output_tokens",
            "/skill_evolution/generation_model_name",
            "/skill_evolution/budget",
            "/skill_evolution/budget/max_daily_tokens",
            "/skill_evolution/discovery",
            "/skill_evolution/discovery/operator_corrections_enabled",
            "/skill_evolution/discovery/missing_reports_enabled",
            "/skill_evolution/discovery/successful_workflows_enabled",
            "/skill_evolution/discovery/new_skill_source_id",
            "/skill_evolution/discovery/repeat_threshold",
            "/skill_evolution/discovery/repeat_window_days",
            "/skill_evolution/discovery/cooldown_hours",
            "/skill_evolution/discovery/max_daily_proposals",
            "/skill_evolution/discovery/max_pending_proposals",
            "/skill_evolution/behavior",
            "/skill_evolution/behavior/enabled",
            "/skill_evolution/behavior/suite_path",
            "/skill_evolution/behavior/timeout_seconds",
            "/skill_evolution/behavior/max_steps",
            "/skill_evolution/behavior/max_input_bytes",
            "/skill_evolution/behavior/max_output_tokens",
            "/skill_evolution/auto_patch",
            "/skill_evolution/auto_patch/max_changed_lines",
            "/skill_evolution/auto_patch/max_daily_publications",
            "/skill_evolution/auto_patch/observation_hours",
            "/skill_evolution/auto_patch/min_observations",
            "/skill_evolution/auto_patch/rollback_failure_threshold",
            "/skill_evolution_migration_ack",
            "/skills_enabled",
        ] {
            for remove in [false, true] {
                let mut status = json!({"running":true,"config_schema":doc["config_schema"]});
                let fields = status["config_schema"]["fields"].as_array_mut().unwrap();
                if remove {
                    fields.retain(|field| field["path"] != path);
                } else {
                    fields
                        .iter_mut()
                        .find(|field| field["path"] == path)
                        .unwrap()["hash"] = json!("changed-review-contract");
                }
                let partial = Capabilities::read(&doc, &status);
                assert!(!partial.can_save(&doc));
                assert!(!partial.allows(path));
                assert!(partial.allows("/skill_evolution/enabled"));
                for (parent, child) in [
                    ("validation", "timeout_seconds"),
                    ("generation", "enabled"),
                    ("budget", "workers"),
                    ("discovery", "operator_corrections_enabled"),
                    ("behavior", "enabled"),
                    ("auto_patch", "max_changed_lines"),
                ] {
                    if path == format!("/skill_evolution/{parent}") {
                        assert!(!partial.allows(&format!("{path}/{child}")));
                    }
                }
            }
        }
        assert_eq!(doc, original);
        let mut obsolete = doc.clone();
        obsolete["skills"] = json!([]);
        assert!(!stopped.can_save(&obsolete));
        for pointer in [
            "/skill_evolution",
            "/skill_evolution/validation",
            "/skill_evolution/generation",
            "/skill_evolution/budget",
            "/skill_evolution/discovery",
            "/skill_evolution/behavior",
            "/skill_evolution/auto_patch",
        ] {
            let mut invalid = doc.clone();
            invalid
                .pointer_mut(pointer)
                .unwrap()
                .as_object_mut()
                .unwrap()
                .insert("advanced".into(), json!({}));
            assert!(!stopped.can_save(&invalid));
        }
        let mut incomplete = doc.clone();
        incomplete["skill_evolution"]["validation"]
            .as_object_mut()
            .unwrap()
            .remove("max_output_tokens");
        assert!(!stopped.can_save(&incomplete));
    }

    #[test]
    fn deerflow_settings_intersects_live_fields_without_changing_draft() {
        let doc = document();
        let original = doc.clone();
        let mut status = json!({"running":true,"config_schema":doc["config_schema"]});
        assert!(Capabilities::read(&doc, &status).can_save(&doc));
        status["config_schema"]["fields"][0]["hash"] = json!("changed-range");
        let contract = Capabilities::read(&doc, &status);
        assert!(!contract.allows("/runtime/max_active_runs"));
        assert!(contract.allows("/memory/enabled"));
        assert!(!contract.can_save(&doc));
        assert!(contract.issue.is_some());
        assert_eq!(doc, original);
    }

    #[test]
    fn deerflow_settings_unknown_schema_and_malformed_fields_fail_closed() {
        let doc = document();
        for schema in [
            Value::Null,
            json!({"version":"deerflow-settings-v1"}),
            json!({"version":"future"}),
            json!({"version":VERSION,"hash":"x"}),
        ] {
            let status = json!({"running":true,"config_schema":schema});
            let contract = Capabilities::read(&doc, &status);
            assert!(!contract.allows("/memory/enabled"));
            assert!(!contract.can_save(&doc));
            assert!(contract.issue.is_some());
        }
        let mut changed = doc.clone();
        changed["config_schema"]["fields"][0]["type"] = json!("string");
        assert!(!Capabilities::read(&changed, &Value::Null).allows("/runtime/max_active_runs"));
        changed["config_schema"]["fields"][1] = changed["config_schema"]["fields"][0].clone();
        assert!(!Capabilities::read(&changed, &Value::Null).allows("/memory/enabled"));
        let mut unknown = doc.clone();
        unknown["memory"]["storage_class"] = json!("legacy");
        assert!(!Capabilities::read(&unknown, &Value::Null).can_save(&unknown));
    }

    #[test]
    fn deerflow_model_and_role_fields_require_matching_live_descriptors() {
        let doc = document();
        let mut status = json!({"running":true,"config_schema":doc["config_schema"]});
        let contract = Capabilities::read(&doc, &status);
        assert!(contract.allows("/models/0/provider"));
        assert!(contract.allows("/models"));
        assert!(contract.allows("/subagents/enabled"));
        assert!(!contract.allows("/models/0/advanced"));
        assert!(!contract.allows("/models/0/use_path"));
        assert!(!contract.allows("/subagents"));
        assert!(!contract.allows("/subagents/advanced"));
        status["config_schema"]["fields"]
            .as_array_mut()
            .unwrap()
            .retain(|field| field["path"] != "/subagents/enabled");
        let contract = Capabilities::read(&doc, &status);
        assert!(contract.allows("/models/0/provider"));
        assert!(!contract.allows("/subagents/enabled"));
        assert!(!contract.can_save(&doc));
        let mut changed = doc.clone();
        changed["models"][0]["advanced"] = json!({"temperature":1});
        assert!(!Capabilities::read(&changed, &Value::Null).can_save(&changed));
    }

    #[test]
    fn deerflow_nested_roles_intersect_each_field_and_guard_collection_edits() {
        let mut doc = document();
        doc["subagents"]["agents"] = json!({});
        doc["subagents"]["custom_agents"] = json!({"reviewer":{"model_settings":{"temperature":0},"thinking_enabled":null,"tools":[]}});
        let fields = doc["config_schema"]["fields"].as_array_mut().unwrap();
        for collection in ["agents", "custom_agents"] {
            let root = format!("/subagents/{collection}");
            for path in std::iter::once(root.clone())
                .chain(ROLE_FIELDS.iter().map(|key| format!("{root}/*{key}")))
            {
                let kind = expected_type(&path).unwrap();
                fields.push(json!({"path":path,"type":kind,"apply":"restart","operations":["validate","save"],"hash":"nested-v1"}));
            }
        }
        let original = doc.clone();
        let mut status = json!({"running":true,"config_schema":doc["config_schema"]});
        let full = Capabilities::read(&doc, &status);
        assert!(full.can_save(&doc));
        assert!(full.allows("/subagents/custom_agents"));
        assert!(full.allows("/subagents/custom_agents/reviewer/model_settings/temperature"));
        assert!(!full.allows("/subagents/custom_agents/reviewer/advanced"));
        status["config_schema"]["fields"]
            .as_array_mut()
            .unwrap()
            .iter_mut()
            .find(|field| field["path"] == "/subagents/custom_agents/*/model_settings/temperature")
            .unwrap()["hash"] = json!("new-range");
        let partial = Capabilities::read(&doc, &status);
        assert!(!partial.can_save(&doc));
        assert!(!partial.allows("/subagents/custom_agents"));
        assert!(!partial.allows("/subagents/custom_agents/reviewer/model_settings/temperature"));
        assert!(partial.allows("/subagents/custom_agents/reviewer/thinking_enabled"));
        assert_eq!(doc, original);
        doc["subagents"]["custom_agents"]["reviewer"]["model_settings"]["unknown"] = json!(1);
        assert!(!Capabilities::read(&doc, &Value::Null).can_save(&doc));
    }

    #[test]
    fn deerflow_tool_fields_require_live_contract_and_reject_removed_editors() {
        let mut doc = document();
        doc["tools"] = json!([{"name":"host_opencli","original_name":"host_opencli","executable":"opencli","timeout":0,"allowed_sites":[]}]);
        let mut status = json!({"running":true,"config_schema":doc["config_schema"]});
        let contract = Capabilities::read(&doc, &status);
        assert!(contract.can_save(&doc));
        assert!(contract.allows("/tools/0/allowed_sites"));
        assert!(contract.allows("/sandbox/allow_host_tools"));
        for path in [
            "/tools/0/use",
            "/tools/0/advanced",
            "/tool_groups",
            "/sandbox/use",
            "/sandbox/advanced",
        ] {
            assert!(!contract.allows(path));
        }
        status["config_schema"]["fields"]
            .as_array_mut()
            .unwrap()
            .iter_mut()
            .find(|field| field["path"] == "/tools/*/allowed_sites")
            .unwrap()["hash"] = json!("changed-sites");
        let partial = Capabilities::read(&doc, &status);
        assert!(!partial.allows("/tools/0/allowed_sites"));
        assert!(!partial.allows("/tools"));
        assert!(!partial.can_save(&doc));
        assert!(partial.allows("/tools/0/timeout"));
        doc["tools"][0]["advanced"] = json!({});
        assert!(!contract.can_save(&doc));
    }

    #[test]
    fn deerflow_native_controls_do_not_fall_back_to_removed_runtime_fields() {
        let mut doc = document();
        doc["subagents"]["max_concurrent"] = json!(3);
        doc["config_migration_ack"] = json!("");
        let fields = doc["config_schema"]["fields"].as_array_mut().unwrap();
        for (path, kind) in [
            ("/subagents/max_concurrent", "integer"),
            ("/config_migration_ack", "string"),
        ] {
            fields.push(json!({"path":path,"type":kind,"apply":"restart","operations":["validate","save"],"hash":"native-controls-v1"}));
        }
        let contract = Capabilities::read(&doc, &Value::Null);
        assert!(contract.can_save(&doc));
        assert!(contract.allows("/subagents/max_concurrent"));
        assert!(contract.allows("/config_migration_ack"));
        assert!(!contract.allows("/runtime/subagent_enabled"));
        assert!(!contract.allows("/runtime/max_concurrent_subagents"));
        doc["runtime"]["subagent_enabled"] = json!(true);
        assert!(!contract.can_save(&doc));
    }
}
