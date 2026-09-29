# Go daemon 的 `--config` 兼容范围

Rust Bridge 和桌面端可以继续传入现有 `config.yaml`。Go daemon 读取文件后，将规范化配置路径写进受保护的 endpoint，并在 MANAGE 状态中返回原文件的 SHA-256 `config_revision`，供 Bridge 校验和桌面重启提示使用。

当前映射如下：

| Python 配置 | Go daemon |
| --- | --- |
| `local_acp.model_name`，否则 `api.model_name`、`default_model`、首个模型 | 选中的模型 ID 与提供商 |
| 模型 `use` | `langchain_openai:ChatOpenAI` → OpenAI，`langchain_anthropic:ChatAnthropic` → Claude |
| 模型 `api_key`、`base_url`、`supports_vision`、`context_window` | 对应 Go 模型配置；已知窗口与主模型单次 usage 生成 ACP `usage_update`；支持 Python 的完整标量 `$NAME`、`${NAME}` 和 `${NAME:-fallback}` |
| 同提供商、同凭据与同端点的其他模型 | ACP 模型选项 |
| `local_acp.subagent_enabled`、`max_active_connections`、`run_timeout_seconds` | 子 Agent 开关、ACP 连接上限、共享预算运行期限 |

显式 Go 命令行标志优先于对应 YAML 字段。选中模型未配置凭据时，仍回退到现有 `DEERFLOW_MODEL_API_KEY` 或提供商环境变量。未指定 `--data-dir` 或 `DEERFLOW_GO_DATA_DIR` 时，Go 状态写在配置文件所在目录的 `go-harness-state`，不会复用 Python SQLite 数据库。

启动时明确拒绝尚无对应策略的 `local_acp.permission_mode`（除 `dangerous`）、工具 allow/deny 列表和自动 goal continuation。`enable_bash` 要求显式 Go sandbox 标志；允许客户端 MCP 要求显式 Go MCP allow 标志。其他 Python API、调度器和管理界面配置目前不进入 Go harness。跨提供商或不同凭据的模型不出现在 Go ACP 模型切换列表中。

此兼容层只解决 Bridge 自动启动及模型选择。Python 会话、checkpoint、记忆和扩展状态不被自动导入；这些数据有独立的迁移步骤。
