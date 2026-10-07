use serde_json::Value;

pub(super) fn capability_notice(data: &Value) -> String {
    let c = &data["capabilities"];
    let state = |key: &str| {
        if c[key]["ok"] == true {
            "通过"
        } else {
            "未通过"
        }
    };
    format!(
        "流式文本{} · 工具调用{} · usage {} · {} 次请求，{} ms。图片与 thinking 未测试。",
        state("streaming"),
        state("tools"),
        if c["usage_reported"] == true {
            "已报告"
        } else {
            "未报告，使用估算"
        },
        c["requests"],
        data["latency_ms"]
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn deerflow_agent_probe_distinguishes_missing_usage() {
        let text = capability_notice(
            &serde_json::json!({"capabilities":{"streaming":{"ok":true},"tools":{"ok":false},"usage_reported":false,"requests":2},"latency_ms":42}),
        );
        assert!(text.contains("流式文本通过"));
        assert!(text.contains("工具调用未通过"));
        assert!(text.contains("使用估算"));
    }
}
