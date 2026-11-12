//! Evidence for metadata suggestions, never a runtime allowlist or account probe.
use super::model_metadata_logic::{adapter, normalized_endpoint, string_field};
use serde_json::Value;

// Reviewed against the documented top-level Responses models, not native output
// modalities or generic function calling. Keep exact IDs; missing IDs are unknown.
pub(super) const SOURCE: &str =
    "https://developers.openai.com/api/docs/guides/tools-image-generation";
pub(super) const REVIEWED: &str = "2026-11-11";
const SUPPORTED_MODELS: &[&str] = &[
    "chat-latest",
    "gpt-5.2",
    "gpt-5.2-pro",
    "gpt-5.4",
    "gpt-5.4-mini",
    "gpt-5.4-pro",
    "gpt-5.5",
    "gpt-5.5-pro",
    "gpt-5.6-cyber",
    "gpt-5.6-luna",
    "gpt-5.6-sol",
    "gpt-5.6-terra",
    "gpt-6-astra",
    "gpt-6-cyber",
    "gpt-6-luna",
    "gpt-6-sol",
    "gpt-6.1-sol",
    "gpt-6.1-terra",
];
const IMAGE_TOOL_MODELS: &[&str] = &[
    "gpt-image-2.5-sunburst",
    "gpt-image-2.5-flare",
    "gpt-image-2",
    "gpt-image-1.5",
    "gpt-image-1-mini",
];

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(super) enum Support {
    Supported,
    Unsupported,
    Unknown,
}

pub(super) fn protocol_eligible(model: &Value) -> bool {
    matches!(
        adapter(model).to_ascii_lowercase().as_str(),
        "openai" | "openai-compatible"
    ) && string_field(model, "api") == "responses"
}

pub(super) fn evidence(model: &Value) -> Support {
    if !protocol_eligible(model) {
        return Support::Unknown;
    }
    let endpoint = string_field(model, "base_url");
    if !endpoint.is_empty()
        && normalized_endpoint(endpoint).as_deref() != Some("https://api.openai.com/v1")
    {
        return Support::Unknown;
    }
    let id = string_field(model, "model");
    if SUPPORTED_MODELS.contains(&id) {
        Support::Supported
    } else if IMAGE_TOOL_MODELS.contains(&id) {
        // These are image_generation tool models, not top-level chat models.
        Support::Unsupported
    } else {
        Support::Unknown
    }
}

pub(super) fn setting_hint(model: &Value) -> String {
    if !protocol_eligible(model) {
        return if model["supports_image_generation"] == true {
            "当前配置冲突：请选择 OpenAI 兼容提供商和 Responses 接口，或关闭图片生成后保存。".into()
        } else {
            "此功能需要 OpenAI 兼容提供商和 Responses 接口；自动协议不会启用 Responses。".into()
        };
    }
    let description = match evidence(model) {
        Support::Supported => format!(
            "OpenAI 官方工具文档已确认此模型支持（核验于 {REVIEWED}，{}）；当前账号权限仍以服务商为准。",
            SOURCE
                .trim_start_matches("https://")
                .split('/')
                .next()
                .unwrap_or_default()
        ),
        Support::Unsupported => {
            "这是专用图片模型，请在模型 ID 中选择支持图片生成工具的聊天模型。".into()
        }
        Support::Unknown => {
            "无法自动确认当前端点和模型的图片生成能力；可按服务商说明手动开启。".into()
        }
    };
    format!("{description} 开启后可生成和修改图片，按服务商规则计费。")
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn model() -> Value {
        json!({"provider":"openai", "api":"responses", "model":"gpt-5.2", "base_url":""})
    }

    #[test]
    fn evidence_requires_the_actual_official_endpoint_and_exact_chat_model() {
        let mut current = model();
        assert_eq!(evidence(&current), Support::Supported);
        current["base_url"] = " HTTPS://API.OPENAI.COM:443/v1/ ".into();
        assert_eq!(evidence(&current), Support::Supported);
        for endpoint in [
            "https://proxy.example/v1",
            "https://api.openai.com.evil.test/v1",
            "https://api.openai.com/tenant/v1",
            "http://api.openai.com/v1",
            "https://api.openai.com/v1?tenant=other",
            "$OPENAI_BASE_URL",
        ] {
            current["base_url"] = endpoint.into();
            assert_eq!(evidence(&current), Support::Unknown, "{endpoint}");
        }
        current["base_url"] = "".into();
        for id in [
            "gpt-5.2-custom",
            "gpt-5.2-2025-12-11",
            "gpt-999",
            "GPT-5.2",
            "",
        ] {
            current["model"] = id.into();
            assert_eq!(evidence(&current), Support::Unknown, "{id}");
        }
    }

    #[test]
    fn protocol_and_image_output_do_not_prove_hosted_tool_support() {
        let mut current = model();
        current["modalities"] = json!({"output":["text"]});
        assert_eq!(evidence(&current), Support::Supported);
        current["model"] = "gpt-image-1.5".into();
        current["modalities"] = json!({"output":["text", "image"]});
        assert_eq!(evidence(&current), Support::Unsupported);
        current["model"] = "unknown".into();
        current["tool_call"] = true.into();
        assert_eq!(evidence(&current), Support::Unknown);
        current["model"] = "gpt-5.2".into();
        for api in ["auto", "chat_completions", ""] {
            current["api"] = api.into();
            assert!(!protocol_eligible(&current));
            assert_eq!(evidence(&current), Support::Unknown);
        }
        current["api"] = "responses".into();
        current["provider"] = "claude".into();
        assert!(!protocol_eligible(&current));
        current["provider"] = "openai-compatible".into();
        assert_eq!(evidence(&current), Support::Supported);
    }
}
