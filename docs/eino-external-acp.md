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

工具接受 `{"agent":"reviewer","prompt":"..."}`。同一父会话再次调用同名 Agent 时使用已保存的 remote session ID 进行 `session/load`；配置变化会拒绝复用旧会话，避免以变化的权限和凭据继续。删除父会话后清理私有目录。取消或超时会发送 `session/cancel`，随后终止外部进程树。

目前外部 Agent 请求客户端 fs/terminal 时会收到不支持；反向工具权限在持久执行路径中会取消。远端的实际模型调用数和完整 token 用量无法从标准 ACP 强制读取，父预算仅预留一次模型调用并估算传入/流出的文本。外部媒体和资源链接尚未导入本地资产库。需要这些能力的任务应继续使用本机 Eino 工具或原生子 Agent。
