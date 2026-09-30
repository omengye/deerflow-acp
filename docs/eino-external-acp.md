# Go Harness 外部 ACP Agent 委派

此能力默认关闭。宿主创建 JSON 允许列表，再用 `--acp-agents-config` 传给 `deerflow-acp-go` 或 `deerflow-acpd-go`。例如：

```json
{
  "reviewer": {
    "command": "C:\\Tools\\reviewer-acp.exe",
    "args": [],
    "description": "代码审阅 Agent",
    "env": {"REVIEWER_API_KEY": "$REVIEWER_API_KEY"},
    "timeout_seconds": 600
  }
}
```

`command` 必须是已存在的绝对文件路径；`env` 中的 `$NAME` 在启动时从宿主进程环境解析，缺失即报错。普通模型凭据不会自动继承到子进程。可执行程序由宿主选择并信任；独立工作目录和精简环境不提供操作系统级隔离。外层委派仍须走当前会话工具权限。

工具接受 `{"agent":"reviewer","prompt":"..."}`。远端 `session/new` 成功后先把 remote session ID 持久化；每次发送 `session/prompt` 前再保存未决 prompt 身份。本地状态写入失败时不发送 prompt。远端成功返回后，只有外层工具回执提交才清除未决标记。同一父会话再次调用同名 Agent 时使用已保存的 ID 进行 `session/load`；配置变化会拒绝复用旧会话，避免以变化的权限和凭据继续。删除父会话后清理私有目录。取消或超时会发送 `session/cancel`，随后终止外部进程树。

断线、超时或回执写入失败后，如果 remote prompt 的结果不明，后续调用会被阻止。嵌入式 Go SDK 提供 `PendingExternalPrompt(ctx, sessionID, agent)` 查询未决身份。核对远端会话结果后，先用现有 `ReconcileToolReceipt` 审核不明的父工具回执，再用 `AcknowledgeExternalPrompt(ctx, sessionID, agent, promptID)` 清除精确匹配的阻断标记。已成功提交的父工具回执可直接作为确认依据。stdio ACP/daemon 连接可调用 `_deerflow/external_prompt/pending`，参数为 `sessionId` 和 `agent`；确认时调用 `_deerflow/external_prompt/acknowledge`，再传 `promptId`。能力仅在配置外部 Agent 时宣告于 `initialize._meta.deerflow.externalPrompts`。这些方法不会重新发送远端 prompt。

目前外部 Agent 请求客户端 fs/terminal 时会收到不支持。连接仍在时，反向工具权限经当前会话策略和 ACP 客户端实时决定；标准 ACP 的 `session/load` 不能证明原 prompt 是否已执行，因此断线后不能通用地原位恢复该 prompt。远端终态响应若提供 `usage`，Go 校验后将其记入父预算及用量事件；没有提供时继续估算，失败且结果不明时保守结算预留额度。标准 ACP 仍不能强制远端限制模型调用数或实际 token 消耗，也不能证明其自报用量完整。外部媒体和资源链接尚未导入本地资产库；文件链接仅允许指向外部独立 workspace。需要完整远端预算或断线续批的任务应继续使用本机 Eino 工具或原生子 Agent。
