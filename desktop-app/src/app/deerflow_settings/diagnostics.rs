use super::*;

pub(super) fn render(data: &Value, theme: Theme) -> AnyElement {
    let text = |key: &str| data[key].as_str().unwrap_or("未记录");
    let number = |key: &str| {
        data[key]
            .as_i64()
            .map(|v| v.to_string())
            .unwrap_or_else(|| "未记录".into())
    };
    let mut group = df_group(theme).child(df_label("执行诊断", format!(
        "会话 {}\n运行 {} · 状态 {} · 采样时 {}\n执行 {} · 停止 {} · 阻塞 {}\n根任务累计：模型 {} 次 · 工具 {} 次 · 已用 {} token · 预留 {} token · 待核对 {} 条",
        text("session_id"), text("run_id"), text("status"), text("phase"), text("execution_status"), text("stop_reason"), text("blocked_reason"),
        number("model_calls"),number("tool_calls"),number("spent_tokens"),number("held_tokens"),number("unreconciled_receipts")), theme));
    if let Some(rows) = data["model_timings"].as_array() {
        for (index, row) in rows.iter().take(6).enumerate() {
            let t = &row["timing"];
            group = group.child(df_label(
                format!(
                    "模型请求 {} · {}",
                    index + 1,
                    row["status"].as_str().unwrap_or("未知")
                ),
                format!(
                    "总耗时 {} ms · 首 token {} ms · attempt {}",
                    t["duration_ms"],
                    t["first_token_ms"],
                    t["attempt_id"].as_str().unwrap_or("未记录")
                ),
                theme,
            ));
        }
    }
    if let Some(rows) = data["tools"].as_array() {
        for row in rows.iter().take(6) {
            group = group.child(df_label(
                row["name"].as_str().unwrap_or("工具").to_owned(),
                format!(
                    "{} · 原因 {} · 生命周期 {} ms（含审批等待）",
                    row["state"].as_str().unwrap_or("未知"),
                    row["failure_code"].as_str().unwrap_or("未记录"),
                    row["lifecycle_ms"]
                ),
                theme,
            ));
        }
    }
    group.child(df_label("诊断快照", format!("采样时间 {}。点击会话的查看诊断可刷新。只展示最近运行及有界记录；旧运行没有模型计时。",text("sampled_at")),theme)).into_any_element()
}
