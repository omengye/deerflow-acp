//! Shared model choices for settings. Skill evolution inherits the primary model.
use super::*;

pub(super) fn model_options(document: &Value) -> Vec<(Value, String)> {
    let mut options = vec![(json!(""), "未选择".into())];
    if let Some(models) = document["models"].as_array() {
        options.extend(
            models
                .iter()
                .filter_map(|model| model["name"].as_str())
                .map(|name| (json!(name), name.to_owned())),
        );
    }
    options
}

pub(super) fn inheritable_model_options(document: &Value) -> Vec<(Value, String)> {
    let mut options = model_options(document);
    options[0] = (Value::Null, "继承默认模型".into());
    options
}
