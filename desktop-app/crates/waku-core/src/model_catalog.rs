//! DeerFlow reports the model catalog for its current configuration over ACP.

use std::path::Path;
use std::time::Duration;

use agent_client_protocol::schema::v1::{SessionConfigKind, SessionConfigOption, SessionConfigOptionCategory};
use agent_client_protocol::UntypedMessage;
use serde_json::{Value, json};

use crate::model::{ProviderKind, ProviderModel, ProviderModelOption};

// An explicitly started portable DeerFlow may still be warming its catalog.
// Background discovery waits for it, but must never start a stopped service.
const DEERFLOW_ACP_DISCOVERY_TIMEOUT: Duration = Duration::from_secs(60);

pub(crate) fn parse_deerflow_models(options: &[SessionConfigOption]) -> Vec<ProviderModel> {
    let Some(model_option) = options.iter().find(|option| {
        option.id.to_string().eq_ignore_ascii_case("model")
            || option.category == Some(SessionConfigOptionCategory::Model)
    }) else {
        return Vec::new();
    };
    let mut models = crate::driver::parse_acp_config_models(model_option);
    let metadata = serde_json::to_value(model_option).unwrap_or_default();
    if let Some(capabilities) = metadata.pointer("/_meta/deerflow/reasoningEfforts") {
        for model in &mut models {
            if let Some(efforts) = capabilities.get(&model.id).and_then(Value::as_array)
                && !efforts.is_empty()
            {
                model
                    .reasoning_efforts
                    .push(ProviderModelOption::new("default", "Default"));
                model.reasoning_efforts.extend(
                    efforts.iter().filter_map(Value::as_str).map(|value| {
                        ProviderModelOption::new(value, reasoning_effort_label(value))
                    }),
                );
                model.default_reasoning_effort = Some("default".into());
            }
        }
    }
    if let Some(capabilities) = metadata.pointer("/_meta/deerflow/thinkingModes") {
        for model in &mut models {
            if capabilities.get(&model.id).and_then(Value::as_bool) == Some(true) {
                model.thinking_modes = vec![
                    ProviderModelOption::new("default", "Model default"),
                    ProviderModelOption::new("on", "On"),
                    ProviderModelOption::new("off", "Off"),
                ];
            }
        }
    }
    if let Some(capabilities) = metadata.pointer("/_meta/deerflow/thinkingAvailability") {
        for model in &mut models {
            model.thinking_availability = capabilities
                .get(&model.id)
                .and_then(Value::as_str)
                .map(str::to_owned);
        }
    }
    if let Some(thinking) = options
        .iter()
        .find(|option| option.id.to_string() == "thinking_enabled")
    {
        for model in models.iter_mut().filter(|model| model.is_default) {
            model.thinking_modes = match &thinking.kind {
                SessionConfigKind::Boolean(_) => vec![
                    ProviderModelOption::new("on", "On"),
                    ProviderModelOption::new("off", "Off"),
                ],
                _ => crate::driver::parse_acp_config_models(thinking)
                    .into_iter()
                    .map(|choice| ProviderModelOption::new(choice.id, choice.name))
                    .collect(),
            };
        }
    } else {
        for model in models.iter_mut().filter(|model| model.is_default) {
            model.thinking_modes.clear();
        }
    }
    if let Some(option) = options.iter().find(|option| {
        option.id.to_string() == "reasoning_effort"
            || (option.category == Some(SessionConfigOptionCategory::ThoughtLevel)
                && option.id.to_string() != "thinking_enabled")
    }) {
        for model in models.iter_mut().filter(|model| model.is_default) {
            model.reasoning_efforts = crate::driver::parse_acp_config_models(option)
                .into_iter()
                .map(|choice| ProviderModelOption::new(choice.id, choice.name))
                .collect();
            model.default_reasoning_effort = model
                .reasoning_efforts
                .iter()
                .find(|choice| choice.id == "default")
                .map(|choice| choice.id.clone());
        }
        return models;
    }
    models
}

pub(crate) fn discover_deerflow_models(binary: &Path) -> Vec<ProviderModel> {
    let cwd = match crate::acp_session::catalog_working_directory() {
        Ok(cwd) => cwd,
        Err(_) => return Vec::new(),
    };
    let agent = match crate::driver::catalog_agent(ProviderKind::DeerFlow, binary, &cwd, false) {
        Ok(agent) => agent,
        Err(_) => return Vec::new(),
    };

    let client_info =
        agent_client_protocol::schema::v1::Implementation::new("waku", env!("CARGO_PKG_VERSION"));
    let client_capabilities =
        agent_client_protocol::schema::v1::ClientCapabilities::new().terminal(false);
    let initialize_req = agent_client_protocol::schema::v1::InitializeRequest::new(
        agent_client_protocol::schema::ProtocolVersion::V1,
    )
    .client_capabilities(client_capabilities)
    .client_info(client_info);

    let session_cwd = cwd.clone();
    let probe = agent_client_protocol::Client
        .builder()
        .name("waku-model-probe")
        .connect_with(
            agent,
            async move |connection: agent_client_protocol::ConnectionTo<
                agent_client_protocol::Agent,
            >| {
                let _ = connection.send_request(initialize_req).block_task().await?;
                let new_session = connection
                    .send_request(agent_client_protocol::schema::v1::NewSessionRequest::new(
                        &session_cwd,
                    ))
                    .block_task()
                    .await?;
                let models = parse_deerflow_models(
                    new_session.config_options.as_deref().unwrap_or_default(),
                );
                // Discovery never runs a prompt. Close its empty temporary session
                // so every picker refresh does not leave another imported chat.
                if let Ok(close) = UntypedMessage::new(
                    "session/close",
                    json!({"sessionId": new_session.session_id}),
                ) {
                    let _ = connection.send_request(close).block_task().await;
                }
                Ok(models)
            },
        );

    let result = smol::block_on(smol::future::race(
        async move { probe.await.map_err(anyhow::Error::new) },
        async move {
            smol::Timer::after(DEERFLOW_ACP_DISCOVERY_TIMEOUT).await;
            Err(anyhow::anyhow!("DeerFlow ACP model probe timed out"))
        },
    ));

    result.unwrap_or_default()
}

fn reasoning_effort_label(effort: &str) -> String {
    match effort {
        "none" => tr!("model_option.none"),
        "minimal" => tr!("model_option.minimal"),
        "low" => tr!("model_option.low"),
        "medium" => tr!("model_option.medium"),
        "high" => tr!("model_option.high"),
        "xhigh" => tr!("model_option.extra_high"),
        "max" => tr!("model_option.max"),
        "ultra" => tr!("model_option.ultra"),
        "ultracode" => tr!("model_option.ultracode"),
        other => display_name_from_slug(other),
    }
}

pub(crate) fn display_name_from_slug(slug: &str) -> String {
    let words = slug
        .split(['-', '_'])
        .filter(|part| !part.is_empty())
        .map(|part| match part.to_ascii_lowercase().as_str() {
            "gpt" => "GPT".to_owned(),
            "ai" => "AI".to_owned(),
            "xai" => "xAI".to_owned(),
            _ if part
                .chars()
                .all(|char| char.is_ascii_digit() || char == '.') =>
            {
                part.to_owned()
            }
            _ => {
                let mut chars = part.chars();
                chars.next().map_or_else(String::new, |first| {
                    first.to_uppercase().collect::<String>() + chars.as_str()
                })
            }
        })
        .collect::<Vec<_>>();
    if words.first().is_some_and(|word| word == "GPT") {
        words.join("-")
    } else {
        words.join(" ")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn session_reasoning_catalog_is_scoped_to_each_model() {
        let options: Vec<SessionConfigOption> = serde_json::from_value(json!([
            {"id":"model","name":"Model","type":"select","currentValue":"a","options":[
                {"value":"a","name":"A"},{"value":"b","name":"B"},{"value":"c","name":"C"}
            ],"_meta":{"deerflow":{"reasoningEfforts":{"a":["low","high"],"b":[],"c":["low"]}}}},
            {"id":"reasoning_effort","name":"Reasoning","category":"thought_level","type":"select","currentValue":"high","options":[
                {"value":"default","name":"Default"},{"value":"low","name":"Low"},{"value":"high","name":"High"}
            ]}
        ])).unwrap();
        let models = parse_deerflow_models(&options);
        assert_eq!(
            models[0]
                .reasoning_efforts
                .iter()
                .map(|option| option.id.as_str())
                .collect::<Vec<_>>(),
            ["default", "low", "high"]
        );
        assert!(models[1].reasoning_efforts.is_empty());
        assert_eq!(
            models[2]
                .reasoning_efforts
                .iter()
                .map(|option| option.id.as_str())
                .collect::<Vec<_>>(),
            ["default", "low"]
        );
        assert_eq!(
            models[0].default_reasoning_effort.as_deref(),
            Some("default")
        );
    }

    #[test]
    fn deerflow_catalog_keeps_thinking_and_effort_capabilities_separate() {
        let options: Vec<SessionConfigOption> = serde_json::from_value(json!([
            {"id":"model","name":"Model","type":"select","currentValue":"a","options":[
                {"value":"a","name":"A"},{"value":"b","name":"B"},{"value":"c","name":"C"}
            ],"_meta":{"deerflow":{"reasoningEfforts":{"a":["high"],"b":[],"c":["low"]},"thinkingModes":{"a":true,"b":true,"c":false},"thinkingAvailability":{"a":"configured","b":"unconfigured","c":"unsupported"}}}},
            {"id":"reasoning_effort","name":"Reasoning","category":"thought_level","type":"select","currentValue":"high","options":[
                {"value":"default","name":"Default"},{"value":"high","name":"High"}
            ]},
            {"id":"thinking_enabled","name":"Thinking","type":"select","currentValue":"off","options":[
                {"value":"default","name":"Default"},{"value":"on","name":"On"},{"value":"off","name":"Off"}
            ]}
        ])).unwrap();
        let models = parse_deerflow_models(&options);
        assert_eq!(models[0].thinking_modes.len(), 3);
        assert_eq!(models[0].reasoning_efforts.len(), 2);
        assert_eq!(models[1].thinking_modes.len(), 3);
        assert!(models[1].reasoning_efforts.is_empty());
        assert!(models[2].thinking_modes.is_empty());
        assert_eq!(models[2].reasoning_efforts.len(), 2);
        assert_eq!(
            models[0].thinking_availability.as_deref(),
            Some("configured")
        );
        assert_eq!(
            models[1].thinking_availability.as_deref(),
            Some("unconfigured")
        );
        assert_eq!(
            models[2].thinking_availability.as_deref(),
            Some("unsupported")
        );
    }

    #[test]
    fn deerflow_catalog_exposes_its_actual_thinking_switch() {
        let options: Vec<SessionConfigOption> = serde_json::from_value(json!([
            {"id":"model","name":"Model","type":"select","currentValue":"local-a","options":[
                {"value":"local-a","name":"Local A"},{"value":"local-b","name":"Local B"}
            ]},
            {"id":"thinking_enabled","name":"Thinking","type":"select","currentValue":"off","options":[
                {"value":"on","name":"On"},{"value":"off","name":"Off"}
            ]}
        ])).unwrap();
        let models = parse_deerflow_models(&options);
        assert_eq!(models.len(), 2);
        assert!(models[0].is_default);
        assert!(models[0].reasoning_efforts.is_empty());
        assert!(models[1].thinking_modes.is_empty());
        assert_eq!(
            models[0]
                .thinking_modes
                .iter()
                .map(|option| option.id.as_str())
                .collect::<Vec<_>>(),
            ["on", "off"]
        );
        assert!(
            parse_deerflow_models(&options[..1])[0]
                .reasoning_efforts
                .is_empty()
        );
    }

}
