# Eino Go 配置与 Bridge

Rust Bridge 和 Waku 使用 Go 配置工具与 daemon。文件格式为 `config_version: eino-harness-v1`，可复制根目录 `config.example.yaml`；桌面首次启动使用 `resources/desktop-default-config.yaml`。新模板只声明已有消费者的原生字段。

## 当前配置

| 配置 | Go 行为 |
| --- | --- |
| `models[].provider` | `openai`、`claude`、`ark` 的 Eino 模型适配器 |
| `models[].api_key/base_url/model` | 各模型独立的服务端路由；支持完整标量 `$NAME`、`${NAME}`、`${NAME:-fallback}` 环境引用 |
| `models[].supports_vision/context_window` | 图片输入准入及已知上下文窗口展示 |
| `local_acp.model_name`、`default_model` | 初始模型选择，未指定时使用首个模型 |
| `local_acp.max_active_connections/max_active_runs` | 独立的连接与前台运行数量限制 |
| `local_acp.queue_timeout_seconds/run_timeout_seconds` | 排队与执行期限 |
| `local_acp.permission_mode/tool_allowlist/tool_denylist` | 审批模式和宿主工具范围 |
| `local_acp.prompt_overlay/prompt_overlay_file` | 主代理的宿主指令；文件相对配置解析，正文受大小限制 |
| `local_acp.session_cleanup_enabled` 及 retention 字段 | 已脱离连接的会话清理；活动任务和未核对回执受保护 |
| `subagents.enabled/max_concurrent/agents/custom_agents` | 角色配置、共享并发及工具/技能限制 |
| `memory.enabled/mode/injection_enabled/retrieval_enabled` | Go 记忆、注入与检索行为；作用域由 `local_acp.memory_scope` 指定 |
| `tools[].name` 及该工具原生参数 | 网页、文件、命令与 OpenCLI；详见 [Go 工具](go-native-tools.md) |
| `skills.path/extensions_file/managed_sources/export_root` | 显式技能源、导入启停状态和可重建导出视图 |
| `skill_evolution` | 候选审查、显式生成模型与维护预算；自动发现和发布待实施 |

无 CLI 模型路由覆盖时，配置中不同端点、Provider 或凭据的模型可以分别注册为会话选项；公开选项不包含凭据。模型 ID 重复时使用宿主生成的唯一选择 ID。显式 Provider/endpoint/凭据覆盖不能将原后端凭据转发给新后端。

`local_acp.enable_bash` 与 `sandbox.allow_host_bash` 同时启用时，Windows 桌面选择 Go PowerShell 提供方。其他命令提供方通过明确 CLI 参数配置，参见 [sandbox flags](../go-harness/internal/launch/sandbox.md)。OpenCLI 还需要 `sandbox.allow_host_tools` 与已安装的可执行程序。`tool_groups` 是旧元数据，不是授权入口。

客户端 stdio MCP 由 `local_acp.accept_client_mcp_servers` 和 `client_mcp_allowed_commands` 决定宿主授权，Bridge 将 Go 配置工具校验的策略传给 daemon。客户端会话的实际 MCP 定义单独保存，参见 [ACP 客户端 MCP](go-acp-portable.md#desktop-acp-client-mcp)。

## 保存与应用

配置工具和 daemon 共用字段校验；Waku 读取两侧能力交集，只开放匹配字段。保存使用配置 revision 避免覆盖其他窗口，并先备份原文件。保存后的 revision 与 daemon 启动时加载的 revision 分别记录，需要重启应用的设置不能提前显示为已生效。

显式 CLI 优先级按各字段实现执行；工具策略仍取交集，session 配置不能扩大宿主授权。当前子代理有效设置已有来源报告，其他 CLI 覆盖的完整桌面展示仍在 P0/P7 范围内，不能以 YAML 草稿代替 daemon 实际值。

## 状态与旧数据

桌面 Go 状态固定在 `user-data/data/go-harness`。直接使用 daemon 时，`--data-dir` 或 `DEERFLOW_GO_DATA_DIR` 可指定位置；没有覆盖则位于配置旁的 `go-harness-state`。旧 Python SQLite/LangGraph 数据不能直接作为 Go 执行数据库打开。

旧配置兼容读取仍识别有限的模型类名和历史字段，并仅映射到 Go 实现；不动态加载类或启动解释器。未知旧字段在无关保存时保留，转换需显式审阅。新模板不生成 `use`、旧 HTTP 服务参数、Python memory storage、goal continuation 或无消费者的模型能力字段。旧 `local_acp.goal_auto_continue=true` 会被拒绝。

旧数据导入与完全收敛的剩余事项见[实施记录](eino-subagents-skill-evolution-status.md)，格式依据见[固定迁移基线](eino-legacy-migration-baseline.md)。现有历史技能源标识为保证检查点兼容保持稳定，不能仅为移除关键词而重命名。
