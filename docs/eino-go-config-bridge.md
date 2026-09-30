# Go daemon 的 `--config` 兼容范围

Rust Bridge 和桌面端可以继续传入现有 `config.yaml`。Go daemon 读取文件后，将规范化配置路径写进受保护的 endpoint，并在 MANAGE 状态中返回原文件的 SHA-256 `config_revision`，供 Bridge 校验和桌面重启提示使用。

当前映射如下：

| Python 配置 | Go daemon |
| --- | --- |
| `local_acp.model_name`，否则 `api.model_name`、`default_model`、首个模型 | 选中的模型 ID 与提供商 |
| 模型 `use` | `langchain_openai:ChatOpenAI` → OpenAI，`langchain_anthropic:ChatAnthropic` → Claude |
| 模型 `api_key`、`base_url`、`supports_vision`、`context_window` | 对应 Go 模型配置；已知窗口与主模型单次 usage 生成 ACP `usage_update`；支持 Python 的完整标量 `$NAME`、`${NAME}` 和 `${NAME:-fallback}` |
| 同提供商、同凭据与同端点的其他模型 | ACP 模型选项 |
| `local_acp.subagent_enabled`、`max_active_connections`、`run_timeout_seconds` | 子 Agent 开关、ACP 连接上限、共享预算运行期限；缺省时分别为关闭、16、600 秒，运行超时还可继承 `api.chat_request_timeout` |
| `local_acp.prompt_overlay`、`prompt_overlay_file` | 追加到 Go 主 Agent 的宿主指令；文件相对 `config.yaml` 解析，文件内容优先，最多 65536 个字符 |
| `local_acp.permission_mode`、`tool_allowlist`、`tool_denylist` | 前台和后台的审批模式及宿主工具边界；工具列表与显式 Go 策略取交集，拒绝列表取并集 |
| `local_acp.session_cleanup_enabled`、`inactive_session_retention_days`、`closed_session_retention_days`、`session_cleanup_interval_seconds` | 启动及定期清理脱离连接且到期的会话；默认开启、30 天/30 天、每小时扫描 |

显式 Go 命令行标志优先于对应 YAML 字段，包括 `--session-cleanup-enabled`、`--inactive-session-retention-days`、`--closed-session-retention-days` 和 `--session-cleanup-interval`。已附着或运行的会话、关联后台任务及未对账工具回执不会自动删除；`session/load` 成功后重置关闭状态和活动时间。

若显式 `--provider` 或 `--base-url` 指向不同于 Python 所选模型的后端，Go 不会带入该模型的 `api_key`、候选模型、视觉声明和上下文窗口。调用方已经提供的 Go 凭据也不会被 YAML 覆盖；只有候选模型的凭据与最终 Go 凭据一致时才公开该模型的能力。相同提供商、端点和凭据的模型 ID 覆盖仍可复用兼容候选模型。

选中模型未配置凭据时，仍回退到现有 `DEERFLOW_MODEL_API_KEY` 或提供商环境变量。未指定 `--data-dir` 或 `DEERFLOW_GO_DATA_DIR` 时，Go 状态写在配置文件所在目录的 `go-harness-state`，不会复用 Python SQLite 数据库。

启动时明确拒绝尚无对应运行时的自动 goal continuation。`enable_bash` 要求显式 Go sandbox 标志；允许客户端 MCP 要求显式 Go MCP allow 标志。`max_active_runs`、队列超时、memory scope、thinking/profile 等仍未完整映射，不能据此认为 Go daemon 已等价于 Python ACP。其他 Python API、调度器和管理界面配置目前不进入 Go harness。跨提供商或不同凭据的模型不出现在 Go ACP 模型切换列表中。

此兼容层只解决 Bridge 自动启动及模型选择。Python 会话、checkpoint、记忆和扩展状态不被自动导入；这些数据有独立的迁移步骤。
