---
name: weather-cma
description: 查询中国城市未来七天天气、明天下雨及气温，使用中国天气网和国家气象中心官方预报并核对真实日期。默认城市北京。
---

# 中国城市天气预报

优先通过 `skill_run` 执行本技能的 `scripts/weather_forecast.py`。它读取完整官方预报页、验证发布时间和日期序列，再输出所需日期的精简 JSON，避免通用 `web_fetch` 的导航和长度截断。不需要 API Key。

## 查询

加载本技能后，用 `skill_run` 调用：

```json
{"action":"run","name":"weather-cma","path":"scripts/weather_forecast.py","args":["--city","北京","--date","tomorrow"],"timeout_seconds":30}
```

- “今天 / 明天 / 后天”分别使用 `today / tomorrow / after-tomorrow`；脚本每次读取服务器的 `Asia/Shanghai` 当前日期计算，不要自行猜日期或沿用旧轮次的日期。
- 指定日期使用 `--date YYYY-MM-DD`；连续多日加 `--days N`（总范围限当前七天）。用户未说城市时用北京。
- 内置北京、上海、广州、深圳、成都、杭州、武汉、南京、天津、重庆、西安。其他城市先从中国天气网实际页面确认城市代码，再传 `--city 城市 --weather-code 九位代码`；不要把 CMA 五位站号当成九位代码。原始站号参考保留在 [references/station-ids.md](references/station-ids.md)。
- 若运行工具要求先检查环境，可先 `skill_run` 的 `action=check`。不要将 `skill://` 位置当成本地脚本路径；工具会执行本轮锁定的技能快照。

## 依据实际结果回答

只有 `ok=true` 且 `forecast[].date` 覆盖用户所问日期，才报告其中天气、温度和风力。`checked_at` 是查询时间，`source_published_at` 是来源标注的预报时间，不要混用。

语音玩具对话沿用角色的简短朗读风格，直接说日期、会不会下雨、最高/最低温和必要的穿衣建议，一般两三句；不朗读 URL、JSON、表格或工具日志。文字对话可附 `source_url` 作为出处，并按用户要求给表格，使用实际成功的数据源名称。

脚本自动从中国天气网切换到国家气象中心。`ok=false` 时说明未取得对应日期的可靠预报，不从旧新闻或搜索摘要拼出天气。搜索只能用于发现当前官方预报页面，不能代替日期核验。CMA 页面出现人机验证时换上述官方来源，不反复抓取、不绕过验证码。
