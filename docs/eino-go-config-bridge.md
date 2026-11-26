# Eino Go 配置与 Bridge

Rust Bridge 和 Waku 使用 Go 配置工具与 daemon。文件格式为 `config_version: eino-harness-v1`，可复制根目录 `config.example.yaml`；桌面首次启动使用 `resources/desktop-default-config.yaml`。新模板只声明已有消费者的原生字段。

## 当前配置

| 配置 | Go 行为 |
| --- | --- |
| `models[].provider` | `openai`、`claude` 的 Eino 模型适配器 |
| `models[].api_key/base_url/model` | 各模型独立的服务端路由；支持完整标量 `$NAME`、`${NAME}`、`${NAME:-fallback}` 环境引用，凭据也可写成 `${secret:NAME}` 密钥库引用（见下文） |
| `models[].supports_vision/context_window` | 图片输入准入及已知上下文窗口展示 |
| `local_acp.model_name`、`default_model` | 初始模型选择，未指定时使用首个模型 |
| `local_acp.max_active_connections/max_active_runs` | 独立的连接与前台运行数量限制 |
| `local_acp.queue_timeout_seconds/run_timeout_seconds` | 排队与执行期限 |
| `local_acp.permission_mode/tool_allowlist/tool_denylist` | 审批模式和宿主工具范围 |
| `local_acp.prompt_overlay/prompt_overlay_file` | 主代理的宿主指令；文件相对配置解析，正文受大小限制 |
| `local_acp.session_cleanup_enabled` 及 retention 字段 | 已脱离连接的会话清理；活动任务和未核对回执受保护 |
| `subagents.enabled/max_concurrent/agents/custom_agents` | 角色配置、共享并发及工具/技能限制 |
| `memory.enabled/mode/injection_enabled/retrieval_enabled` | Go 记忆、注入与检索行为；作用域由 `local_acp.memory_scope` 指定 |
| `memory.daily_attempts` | single_user worker 附加 UTC 日调用认领上限；默认 200，0 暂停新调用，范围 0–1000000。持久化跨重启，已保存结果重放不计；宿主 YAML、重启生效，不代替来源预算 |
| `memory.embedding_url/embedding_model` | 配置 embedding 服务与固定模型身份；记忆开启时独立建立索引，不依赖自动提取或 single_user。关闭 memory.enabled 停止索引及查询 embedding，保留事实 |
| `memory.index_max_attempts/index_retry_seconds/index_max_retry_seconds` | 每个事实 revision/model 失败上限、指数退避初始秒数和最大秒数；默认 3/60/3600，次数 1–20，退避上限不超过 86400 秒且不小于初始值。省略或 0 采用默认；宿主 YAML、重启生效 |
| `tools[].name` 及该工具原生参数 | 网页、文件、命令与 OpenCLI；详见 [Go 工具](go-native-tools.md) |
| `skills.path/extensions_file/managed_sources/export_root` | 显式技能源、导入启停状态和可重建导出视图 |
| `skill_evolution` | 候选审查、显式生成模型与维护预算；自动发现和发布待实施 |

无 CLI 模型路由覆盖时，配置中不同端点、Provider 或凭据的模型可以分别注册为会话选项；公开选项不包含凭据。模型 ID 重复时使用宿主生成的唯一选择 ID。显式 Provider/endpoint/凭据覆盖不能将原后端凭据转发给新后端。

`local_acp.enable_bash` 与 `sandbox.allow_host_bash` 同时启用时，Windows 桌面选择 Go PowerShell 提供方。其他命令提供方通过明确 CLI 参数配置，参见 [sandbox flags](../go-harness/internal/launch/sandbox.md)。OpenCLI 还需要 `sandbox.allow_host_tools` 与已安装的可执行程序。`tool_groups` 是旧元数据，不是授权入口。

桌面设置保存的 stdio MCP 条目授权对应的程序、参数和环境变量，已有聊天在下一轮应用增删。外部 ACP 客户端仍可使用宿主显式配置的可执行程序授权。参见 [ACP 客户端 MCP](go-acp-portable.md#desktop-acp-client-mcp)。

## 密钥库引用

运行时读取的凭据字段可以把整个值写成 `${secret:NAME}`，从内置密钥库取值，config.yaml 里不再保留明文：

| 字段 | 解析时机 |
| --- | --- |
| `models[].api_key/base_url`、`tools[].api_key/https_proxy/executable`、`network.proxy.url`、`mcp.host_recipes[].servers[].env/headers/url` | 启动时 |
| `search.providers.<name>.api_key` | 每次搜索请求时；搜索设置和固定的执行版本只保存引用 |

- NAME 只能用大写字母、数字和下划线；写错的引用直接报错，不会被当作明文，也没有 fallback 写法。`search.providers.*` 的 `base_url`、`https_proxy` 不支持引用。
- 启动时以只读方式打开运行数据目录中的密钥库：不加运行锁、不创建或迁移任何文件，已在运行的 daemon 不受影响。数据目录或密钥库不存在时，引用按缺失处理。
- 选中的模型，以及已配置的工具、网络代理和宿主 MCP 引用的密钥缺失时，启动失败；其他模型路由会被跳过。二进制（文件类）密钥不能作为配置值。
- 桌面配置工具的就绪检查、模型连接测试和网络校验用同样的只读方式读取。
- 改写后的 config.yaml 只适用于支持 `${secret:NAME}` 的 Go 运行时；仍由其他程序读取的字段不要改成引用。

已有的明文凭据可由 agent 调用 `secret_manage` 的 `import_config` 迁移，建议先用 `dry_run` 查看。它只处理本运行时读取的上述字段，同时迁移 `search-providers.json` 中保存的搜索 key：相同的值共用一个密钥，库中已有的相同值直接复用，已被其他值（包括已删除的密钥）占用的名称改用 `_2`、`_3` 等后缀。整个过程持有 `.config-tool.lock`：先在内存中改写并校验，再把密钥写入密钥库，最后原子替换 config.yaml。只替换凭据值本身，注释、BOM、换行风格和其他设置保持不变；文件在迁移期间被其他程序改动时放弃替换。本运行时不读取的疑似凭据（例如 `channels.*`）不会改动，结果会说明原因；早期运行固定的搜索执行版本也不改写，以便这些运行按原配置恢复。正在运行的 daemon 继续使用启动时读到的值，下次启动改从密钥库读取。

## 保存与应用

配置工具和 daemon 共用字段校验；Waku 读取两侧能力交集，只开放匹配字段。保存使用配置 revision 避免覆盖其他窗口，并先备份原文件。保存后的 revision 与 daemon 启动时加载的 revision 分别记录，需要重启应用的设置不能提前显示为已生效。

显式 CLI 优先级按各字段实现执行；工具策略仍取交集，session 配置不能扩大宿主授权。当前子代理有效设置已有来源报告，其他 CLI 覆盖的完整桌面展示仍在 P0/P7 范围内，不能以 YAML 草稿代替 daemon 实际值。

## 状态与旧数据

桌面 Go 状态固定在 `user-data/data/go-harness`。直接使用 daemon 时，`--data-dir` 或 `DEERFLOW_GO_DATA_DIR` 可指定位置；没有覆盖则位于配置旁的 `go-harness-state`。旧 Python SQLite/LangGraph 数据不能直接作为 Go 执行数据库打开。

旧配置兼容读取仍识别有限的模型类名和历史字段，并仅映射到 Go 实现；不动态加载类或启动解释器。未知旧字段在无关保存时保留，转换需显式审阅。新模板不生成 `use`、旧 HTTP 服务参数、Python memory storage、goal continuation 或无消费者的模型能力字段。旧 `local_acp.goal_auto_continue=true` 会被拒绝。

旧数据导入功能已于 2026-10-12 移除，包括桌面入口、旧 Agent 角色预览和只读历史归档接口；已有配置、技能源与归档文件不删除。历史实施记录见[实施记录](eino-subagents-skill-evolution-status.md)，格式依据见[固定迁移基线](eino-legacy-migration-baseline.md)。现有历史技能源标识为保证检查点兼容保持稳定，不能仅为移除关键词而重命名。

记忆修复阶段 5 的 schema v5 与安全重新学习不增加配置项。输入来源策略与结构化值身份来自新版程序，不能靠设置文件给旧输入补授权。daily_attempts:0 只暂停新模型认领，不能冻结已保存结果的发布；升级时必须停止准入和所有持库进程，完成调用结算或保留 unknown 后备份。当前本机安装的有效路径以阶段 0 核查为准，不能用上面的默认路径替代。具体副本验收、启动顺序与不覆盖新事实的回退策略见[记忆升级运行手册](memory-upgrade-runbook.md)。
