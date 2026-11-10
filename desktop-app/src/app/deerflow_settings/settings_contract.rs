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
        if path.starts_with("/skill_evolution") {
            return false;
        }
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
            && self.network_can_save(document)
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
    fn network_can_save(&self, document: &Value) -> bool {
        let Some(network) = document.get("network") else {
            return true;
        };
        network
            .as_object()
            .is_some_and(|obj| obj.len() == 1 && obj.contains_key("proxy"))
            && network["proxy"].as_object().is_some_and(|fields| {
                ["enabled", "url", "clear_url", "no_proxy"]
                    .iter()
                    .all(|key| {
                        fields.contains_key(*key)
                            && self.allowed.contains(&format!("/network/proxy/{key}"))
                    })
                    && fields.keys().all(|key| {
                        key == "url_configured"
                            || self.allowed.contains(&format!("/network/proxy/{key}"))
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
        document.get("skill_evolution").is_none()
    }
}

fn scoped(path: &str) -> bool {
    path == "/config_migration_ack"
        || path == "/skills_enabled"
        || path == "/skills"
        || path.starts_with("/skills/")
        || path == "/runtime"
        || path == "/memory"
        || path == "/network"
        || path.starts_with("/network/")
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
    "https_proxy",
    "clear_https_proxy",
    "timeout",
    "max_results",
    "max_output_chars",
    "executable",
    "allowed_sites",
];

fn expected_type(path: &str) -> Option<&'static str> {
    match path {
        "/network/proxy/enabled" | "/network/proxy/clear_url" => return Some("boolean"),
        "/network/proxy/url" => return Some("string"),
        "/network/proxy/no_proxy" => return Some("string_array"),
        _ => {}
    }
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
        "/tools" => Some("tool_catalog"),
        "/tools/*/name" | "/tools/*/https_proxy" | "/tools/*/executable" | "/sandbox/provider" => {
            Some("string")
        }
        "/tools/*/clear_https_proxy" | "/sandbox/allow_host_bash" | "/sandbox/allow_host_tools" => {
            Some("boolean")
        }
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
        | "/models/*/api"
        | "/models/*/model"
        | "/models/*/api_key"
        | "/models/*/output_token_parameter"
        | "/models/*/thinking_parameter"
        | "/models/*/base_url" => Some("string"),
        "/models" => Some("model_catalog"),
        "/models/*/reasoning_efforts" => Some("reasoning_efforts"),
        "/models/*/context_window"
        | "/models/*/thinking_budget_tokens"
        | "/models/*/max_output_tokens"
        | "/models/*/max_supported_output_tokens"
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
        "/runtime/enable_bash"
        | "/memory/enabled"
        | "/memory/include_global"
        | "/memory/injection_enabled"
        | "/memory/retrieval_enabled" => Some("boolean"),
        "/runtime/max_active_connections"
        | "/runtime/max_output_tokens"
        | "/runtime/max_active_runs"
        | "/runtime/max_run_tokens"
        | "/runtime/max_model_calls"
        | "/runtime/max_tool_calls" => Some("integer"),
        "/runtime/run_timeout_seconds" | "/runtime/queue_timeout_seconds" => Some("number"),
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

    #[test]
    fn output_budget_fields_require_live_contract() {
        let mut doc = document();
        let mut applied = doc.clone();
        applied["running"] = json!(true);
        assert!(!Capabilities::read(&doc, &applied).allows("/runtime/max_output_tokens"));
        for (path, kind) in [
            ("/runtime/max_output_tokens", "integer"),
            ("/models/*/output_token_parameter", "string"),
            ("/models/*/api", "string"),
            ("/models/*/thinking_parameter", "string"),
            ("/models/*/thinking_budget_tokens", "integer"),
            ("/models/*/max_output_tokens", "integer"),
            ("/models/*/max_supported_output_tokens", "integer"),
        ] {
            let field = json!({"path":path,"type":kind,"apply":"restart","operations":["validate","save"],"hash":"output-v1"});
            doc["config_schema"]["fields"]
                .as_array_mut()
                .unwrap()
                .push(field.clone());
            assert!(!Capabilities::read(&doc, &applied).allows(path));
            applied["config_schema"]["fields"]
                .as_array_mut()
                .unwrap()
                .push(field);
            assert!(Capabilities::read(&doc, &applied).allows(path));
        }
    }

    #[test]
    fn network_fields_require_matching_backend_capabilities() {
        let mut doc = document();
        doc["network"] = json!({"proxy":{"enabled":true,"url":"","url_configured":true,"clear_url":false,"no_proxy":["localhost"]}});
        for key in ["enabled", "url", "clear_url", "no_proxy"] {
            let path = format!("/network/proxy/{key}");
            let kind = expected_type(&path).unwrap();
            doc["config_schema"]["fields"].as_array_mut().unwrap().push(json!({"path":path,"type":kind,"apply":"restart","operations":["validate","save"],"hash":"network-v1"}));
        }
        let stopped = json!({"running":false});
        assert!(Capabilities::read(&doc, &stopped).can_save(&doc));
        let old = document();
        let running = json!({"running":true,"config_schema":old["config_schema"]});
        let contract = Capabilities::read(&doc, &running);
        assert!(!contract.can_save(&doc));
        assert!(!contract.allows("/network/proxy/url"));
        doc["network"]["proxy"]["unexpected"] = json!(true);
        assert!(!Capabilities::read(&doc, &stopped).can_save(&doc));
    }

    #[test]
    fn retired_evolution_fields_are_never_writable() {
        let mut doc = document();
        let status = json!({"running":false});
        let caps = Capabilities::read(&doc, &status);
        assert!(caps.can_save(&doc));
        for key in ["skill_evolution", "skill_evolution_migration_ack"] {
            assert!(!caps.allows(&format!("/{key}")));
        }
        doc["skill_evolution"] = json!({"mode":"auto_patch"});
        assert!(!caps.can_save(&doc));
    }

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
                assert!(!partial.allows("/skill_evolution/enabled"));
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
    fn deerflow_search_plugin_migration_keeps_settings_writable() {
        let mut doc = document();
        doc["tools"] = json!([{"name":"web_search","original_name":"web_search","max_results":5}]);
        // The Go tool schema no longer advertises search credentials; they are
        // configured independently through the search plugin settings.
        doc["config_schema"]["fields"]
            .as_array_mut()
            .unwrap()
            .retain(|field| {
                !matches!(
                    field["path"].as_str(),
                    Some("/tools/*/api_key" | "/tools/*/clear_api_key")
                )
            });
        doc["models"][0]["name"] = json!("edited-model");
        doc["default_model"] = json!("edited-model");
        let original = doc.clone();
        for running in [false, true] {
            let status = json!({"running":running,"config_schema":doc["config_schema"]});
            let contract = Capabilities::read(&doc, &status);
            assert!(contract.allows("/tools"), "running={running}");
            assert!(contract.allows("/tools/0/max_results"));
            assert!(!contract.allows("/tools/0/api_key"));
            assert!(!contract.allows("/tools/0/clear_api_key"));
            assert!(contract.can_save(&doc), "running={running}");
            assert!(contract.issue.is_none());
            for key in ["api_key", "clear_api_key"] {
                let mut legacy_draft = doc.clone();
                legacy_draft["tools"][0][key] = json!("");
                assert!(!contract.can_save(&legacy_draft));
            }
        }
        assert_eq!(doc, original);
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
