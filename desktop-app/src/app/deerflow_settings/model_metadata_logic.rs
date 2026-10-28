//! Matching and draft changes for model metadata. These functions perform no I/O.

use super::models_dev::{Catalog, CatalogEntry};
use serde_json::Value;
use std::collections::HashSet;
use url::Url;

const MAX_CONTEXT_WINDOW: u64 = 1 << 30;
const REASONING_EFFORTS: &[&str] = &["low", "medium", "high", "xhigh", "max"];

#[derive(Debug, Default, PartialEq, Eq)]
pub(super) struct CandidateMatches {
    /// Exact model-ID matches, including hosts that require a manual choice.
    pub(super) indices: Vec<usize>,
    /// Set only when exactly one candidate also matches the configured endpoint.
    pub(super) automatic: Option<usize>,
}

/// A protocol adapter such as `openai` does not identify the hosting provider.
/// Automatic matching therefore requires the endpoint as well as the exact ID.
pub(super) fn match_candidates(catalog: &Catalog, model: &Value) -> CandidateMatches {
    let model_id = string_field(model, "model");
    if model_id.is_empty() {
        return CandidateMatches::default();
    }
    let indices: Vec<_> = catalog
        .entries
        .iter()
        .enumerate()
        .filter_map(|(index, entry)| (entry.model_id == model_id).then_some(index))
        .collect();
    let base_url = string_field(model, "base_url");
    let endpoint = normalized_endpoint(base_url);
    let default_provider = if base_url.is_empty() {
        match adapter(model) {
            "openai" => Some("openai"),
            "claude" => Some("anthropic"),
            _ => None,
        }
    } else {
        None
    };
    let mut exact_endpoints = indices.iter().copied().filter(|index| {
        let entry = &catalog.entries[*index];
        if let Some(provider) = default_provider {
            // Empty native API URLs use the SDK's official provider default.
            return entry.provider_id == provider;
        }
        let Some(endpoint) = endpoint.as_ref() else {
            return false;
        };
        let known_api = entry
            .api
            .as_deref()
            .or_else(|| match entry.provider_id.as_str() {
                "openai" => Some("https://api.openai.com/v1"),
                "anthropic" => Some("https://api.anthropic.com"),
                _ => None,
            });
        known_api.and_then(normalized_endpoint).as_ref() == Some(endpoint)
    });
    let first = exact_endpoints.next();
    let automatic = first.filter(|_| exact_endpoints.next().is_none());
    CandidateMatches { indices, automatic }
}

fn normalized_endpoint(raw: &str) -> Option<String> {
    let url = Url::parse(raw.trim()).ok()?;
    if !matches!(url.scheme(), "https" | "http")
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return None;
    }
    Some(format!(
        "{}{}",
        url.origin().ascii_serialization(),
        url.path().trim_end_matches('/')
    ))
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub(super) struct MetadataChange {
    /// Model-relative field name, without a JSON-pointer prefix.
    pub(super) field: &'static str,
    pub(super) current: Value,
    pub(super) suggested: Value,
}

/// Returns differences only. Callers may present these for a manual choice even
/// when the endpoint is unknown, but must never apply such a choice implicitly.
pub(super) fn suggested_patch(entry: &CatalogEntry, model: &Value) -> Vec<MetadataChange> {
    let mut changes = Vec::new();
    let mut add = |field: &'static str, suggested: Value| {
        let current = model.get(field).cloned().unwrap_or(Value::Null);
        if current != suggested {
            changes.push(MetadataChange {
                field,
                current,
                suggested,
            });
        }
    };
    if let Some(context) = entry
        .context_window
        .filter(|value| (1..=MAX_CONTEXT_WINDOW).contains(value))
    {
        add("context_window", context.into());
    }
    if let Some(vision) = entry.supports_vision {
        add("supports_vision", vision.into());
    }
    // This adapter supports a narrower set than the external catalog. Changing
    // levels underneath an explicitly configured on/off interface can invalidate
    // its current choices, so leave that configuration entirely user-owned.
    if adapter(model) == "openai" && string_field(model, "thinking_parameter") != "reasoning_effort"
    {
        let efforts: Vec<Value> = REASONING_EFFORTS
            .iter()
            .filter(|effort| {
                entry
                    .reasoning_efforts
                    .iter()
                    .any(|value| value == **effort)
            })
            .map(|effort| Value::String((*effort).to_owned()))
            .collect();
        if !efforts.is_empty() {
            add("reasoning_efforts", Value::Array(efforts));
        }
    }
    changes
}

/// Existing presets always require a manual application. For a new preset,
/// explicit edits (including false, zero or an empty array) take precedence.
/// After applying changes, the caller should mark their fields as edited too so
/// repeated lookups cannot overwrite a value already adopted by the user.
pub(super) fn automatic_patch(
    entry: &CatalogEntry,
    model: &Value,
    is_new: bool,
    edited: &HashSet<String>,
) -> Vec<MetadataChange> {
    if !is_new {
        return Vec::new();
    }
    suggested_patch(entry, model)
        .into_iter()
        .filter(|change| !edited.contains(change.field))
        .collect()
}

/// Snapshot of the selected preset and the caller's independent metadata
/// revision. Credentials are intentionally neither captured nor compared.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(super) struct RequestGuard {
    identity: [String; 5],
    revision: u64,
}

impl RequestGuard {
    pub(super) fn capture(model: &Value, revision: u64) -> Self {
        Self {
            identity: ["name", "original_name", "provider", "model", "base_url"]
                .map(|field| string_field(model, field).to_owned()),
            revision,
        }
    }

    pub(super) fn matches(&self, model: &Value, revision: u64) -> bool {
        self == &Self::capture(model, revision)
    }
}

fn string_field<'a>(model: &'a Value, key: &str) -> &'a str {
    model.get(key).and_then(Value::as_str).unwrap_or("").trim()
}

fn adapter(model: &Value) -> &str {
    match string_field(model, "provider") {
        "" => "openai",
        provider => provider,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn entry(provider: &str, api: Option<&str>) -> CatalogEntry {
        CatalogEntry {
            provider_id: provider.into(),
            provider_name: provider.into(),
            model_id: "shared-model".into(),
            name: "Shared model".into(),
            api: api.map(str::to_owned),
            context_window: Some(128_000),
            supports_vision: Some(true),
            reasoning_efforts: vec!["high".into(), "low".into()],
        }
    }

    fn model() -> Value {
        json!({"name":"preset", "original_name":"", "provider":"openai", "model":"shared-model", "base_url":"https://api.example.com/v1", "context_window":0, "supports_vision":false})
    }

    #[test]
    fn exact_id_candidates_require_a_unique_endpoint_for_auto_selection() {
        let catalog = Catalog {
            entries: vec![
                entry("host-a", Some("https://other.example.com/v1")),
                entry("host-b", Some("https://api.example.com/v1")),
            ],
        };
        assert_eq!(
            match_candidates(&catalog, &model()),
            CandidateMatches {
                indices: vec![0, 1],
                automatic: Some(1)
            }
        );
        let mut custom = model();
        custom["base_url"] = "https://my-gateway.example.com/v1".into();
        assert_eq!(
            match_candidates(&catalog, &custom),
            CandidateMatches {
                indices: vec![0, 1],
                automatic: None
            }
        );
        let duplicate = Catalog {
            entries: vec![catalog.entries[1].clone(), catalog.entries[1].clone()],
        };
        assert_eq!(match_candidates(&duplicate, &model()).automatic, None);
    }

    #[test]
    fn endpoint_normalization_keeps_origin_and_path_boundaries() {
        let catalog = Catalog {
            entries: vec![entry("host", Some("https://api.example.com/v1"))],
        };
        let mut current = model();
        current["base_url"] = " HTTPS://API.EXAMPLE.COM:443/v1/ ".into();
        assert_eq!(match_candidates(&catalog, &current).automatic, Some(0));
        for endpoint in [
            "https://api.example.com/v2",
            "https://api.example.com/tenant/v1",
            "https://api.example.com/v1/tenant",
            "https://api.example.com/V1",
            "https://api.example.com.evil.test/v1",
            "http://api.example.com/v1",
            "https://api.example.com:8443/v1",
            "https://user@api.example.com/v1",
            "https://api.example.com/v1?tenant=other",
            "https://api.example.com/v1#other",
        ] {
            current["base_url"] = endpoint.into();
            assert_eq!(
                match_candidates(&catalog, &current).automatic,
                None,
                "{endpoint}"
            );
        }
    }

    #[test]
    fn native_empty_endpoints_match_only_official_provider() {
        let catalog = Catalog {
            entries: vec![
                entry("custom", Some("https://api.example.com/v1")),
                entry("openai", None),
                entry("anthropic", None),
            ],
        };
        let mut current = model();
        current["base_url"] = "".into();
        assert_eq!(match_candidates(&catalog, &current).automatic, Some(1));
        current["provider"] = "claude".into();
        assert_eq!(match_candidates(&catalog, &current).automatic, Some(2));
        current["provider"] = "unknown-adapter".into();
        assert_eq!(match_candidates(&catalog, &current).automatic, None);
    }

    #[test]
    fn openai_compatible_does_not_identify_an_official_host() {
        let catalog = Catalog {
            entries: vec![entry("openai", None)],
        };
        let mut current = model();
        assert_eq!(match_candidates(&catalog, &current).automatic, None);
        current["base_url"] = "https://api.openai.com/v1".into();
        assert_eq!(match_candidates(&catalog, &current).automatic, Some(0));
        current["base_url"] = "https://api.openai.com/custom/v1".into();
        assert_eq!(match_candidates(&catalog, &current).automatic, None);
    }

    #[test]
    fn ids_are_exact_without_prefix_suffix_or_case_guessing() {
        let catalog = Catalog {
            entries: vec![entry("host", Some("https://api.example.com/v1"))],
        };
        let mut current = model();
        for id in [
            "",
            "shared",
            "Shared-Model",
            "shared-model-latest",
            "host/shared-model",
        ] {
            current["model"] = id.into();
            assert_eq!(
                match_candidates(&catalog, &current),
                CandidateMatches::default()
            );
        }
    }

    #[test]
    fn suggestions_only_include_supported_fields_and_differences() {
        let mut metadata = entry("host", None);
        metadata.reasoning_efforts = vec![
            "max", "high", "minimal", "none", "high", "LOW", "low", "medium", "xhigh", "ultra",
        ]
        .into_iter()
        .map(str::to_owned)
        .collect();
        let changes = suggested_patch(&metadata, &model());
        assert_eq!(
            changes
                .iter()
                .map(|change| change.field)
                .collect::<Vec<_>>(),
            vec!["context_window", "supports_vision", "reasoning_efforts"]
        );
        assert_eq!(
            changes[2].suggested,
            json!(["low", "medium", "high", "xhigh", "max"])
        );
        assert_eq!(changes[0].current, json!(0));
        let mut current = model();
        for change in changes {
            current[change.field] = change.suggested;
        }
        assert!(suggested_patch(&metadata, &current).is_empty());
    }

    #[test]
    fn absent_or_invalid_metadata_never_clears_existing_values() {
        let mut metadata = entry("host", None);
        metadata.supports_vision = None;
        metadata.reasoning_efforts = vec!["minimal".into(), "none".into()];
        let mut current = model();
        current["supports_vision"] = true.into();
        current["reasoning_efforts"] = json!(["medium"]);
        for context in [None, Some(0), Some(MAX_CONTEXT_WINDOW + 1), Some(u64::MAX)] {
            metadata.context_window = context;
            assert!(suggested_patch(&metadata, &current).is_empty());
        }
        metadata.context_window = Some(MAX_CONTEXT_WINDOW);
        let changes = suggested_patch(&metadata, &current);
        assert_eq!(changes.len(), 1);
        assert_eq!(changes[0].suggested, json!(MAX_CONTEXT_WINDOW));
    }

    #[test]
    fn explicit_negative_vision_metadata_is_a_valid_manual_suggestion() {
        let mut metadata = entry("host", None);
        metadata.supports_vision = Some(false);
        let mut current = model();
        current["supports_vision"] = true.into();
        let changes = suggested_patch(&metadata, &current);
        assert_eq!(
            changes
                .iter()
                .find(|change| change.field == "supports_vision")
                .unwrap()
                .suggested,
            json!(false)
        );
    }

    #[test]
    fn claude_and_explicit_reasoning_switches_keep_their_effort_configuration() {
        let metadata = entry("host", None);
        let mut current = model();
        current["reasoning_efforts"] = json!(["medium"]);
        current["provider"] = "claude".into();
        assert!(
            !suggested_patch(&metadata, &current)
                .iter()
                .any(|change| change.field == "reasoning_efforts")
        );
        current["provider"] = "openai".into();
        current["thinking_parameter"] = "reasoning_effort".into();
        assert!(
            !suggested_patch(&metadata, &current)
                .iter()
                .any(|change| change.field == "reasoning_efforts")
        );
    }

    #[test]
    fn automatic_changes_never_modify_existing_or_explicitly_edited_values() {
        let metadata = entry("host", None);
        let current = model();
        assert!(automatic_patch(&metadata, &current, false, &HashSet::new()).is_empty());
        let edited = HashSet::from([
            "context_window".to_owned(),
            "supports_vision".to_owned(),
            "reasoning_efforts".to_owned(),
        ]);
        assert!(automatic_patch(&metadata, &current, true, &edited).is_empty());
        assert_eq!(
            automatic_patch(
                &metadata,
                &current,
                true,
                &HashSet::from(["supports_vision".to_owned()])
            )
            .iter()
            .map(|change| change.field)
            .collect::<Vec<_>>(),
            vec!["context_window", "reasoning_efforts"]
        );
    }

    #[test]
    fn adopting_auto_fields_protects_them_from_later_catalog_updates() {
        let mut metadata = entry("host", None);
        let mut current = model();
        let mut edited = HashSet::new();
        for change in automatic_patch(&metadata, &current, true, &edited) {
            edited.insert(change.field.to_owned());
            current[change.field] = change.suggested;
        }
        metadata.context_window = Some(256_000);
        metadata.supports_vision = Some(false);
        metadata.reasoning_efforts = vec!["medium".into()];
        assert!(automatic_patch(&metadata, &current, true, &edited).is_empty());
    }

    #[test]
    fn request_guard_rejects_selection_identity_and_revision_changes() {
        let current = model();
        let guard = RequestGuard::capture(&current, 7);
        assert!(guard.matches(&current, 7));
        assert!(!guard.matches(&current, 8));
        for field in ["name", "original_name", "provider", "model", "base_url"] {
            let mut changed = current.clone();
            changed[field] = "different".into();
            assert!(!guard.matches(&changed, 7), "{field}");
        }
        let mut key_changed = current.clone();
        key_changed["api_key"] = "private-value-not-part-of-metadata-identity".into();
        assert!(guard.matches(&key_changed, 7));
        assert!(!format!("{guard:?}").contains("private-value"));
    }
}
