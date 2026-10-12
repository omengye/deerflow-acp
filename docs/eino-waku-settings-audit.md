# Waku DeerFlow 设置字段清理清单

2026-10-10 更新：独立 Ark 适配器已移除，当前模型请求统一使用流式传输；下文 Ark 字段说明为历史记录，现行约定见[模型流式审计](model-streaming-audit-20261010.md)。

审计日期：2026-10-02。工作分支：`codex/eino-harness-acp-refactor`。

这是 P0/P7 的字段审计清单。**当前原生设置已贯通共享 schema、保存校验和 Go 消费路径，P4 自动发现/行为评测与 P5 自动补丁/观察回滚已接线；实际 Waku GUI、干净环境与最终发行包验收另行记录。** 本次增量以静态字段映射核对为依据，不代替运行验证。字段路径是 Waku draft JSON 的路径，`*` 代表数组元素。同一格内列出的每个字段均适用该决定。

## 当前进度（2026-10-02）

- `deerflow-settings-v2` 覆盖 runtime、memory、models、subagents、tools、sandbox、skills 与 evolution。Waku 使用配置工具与运行 daemon 的字段交集；不兼容字段只读，草稿保留。
- 子代理全局与角色表单、原生工具参数、技能源管理、候选生成与审核、三类自动发现、行为样例评测及受限自动发布均有 Go 消费者。P4/P5 的逐字段映射见第 4 节。
- `deerflow-applied-settings-v1` 已提供主要运行值及 YAML/CLI/default 来源，并补入已启用 P4/P5 worker 的数值限额、已加载角色的稀疏覆盖和非敏感工具参数，Waku 已接只读展示。子代理实际开关及并发另有既有状态契约；未返回的有效字段不猜测值。
- 显式 Go 导入入口已支持旧 Agent 预览后加入角色草稿，以及旧技能进化目录的只读历史归档。导入不执行旧脚本、不恢复旧作业/审批/观察期，也不将旧内容直接发布。
- 分支采用 Eino Go 单运行时；旧 Python 源码删除/替换以 [清理清单](eino-python-removal-inventory.json) 为准。用户已有配置中的未知键和历史数据继续按显式转换处理，普通设置保存不批量删除。
- 本次静态核对修复 `validatedEvolution` 的嵌套合并遗漏：`auto_patch` 与其他进化组一致，在关闭状态下无关保存保留旧未知字段，显式确认转换时才移除。其余列出的可编辑字段未发现缺少 Go 消费者；这不是全量行为验收结论。

## 历史实施记录

下列“增量”段落保留当时的阶段状态；其中的“待完成”不能用于判断当前功能。当前进度以上述摘要、第 4 节和第 7 节为准。

最新技能增量：`skills_enabled` 与人工候选审查字段已纳入 v2 schema；启用、review 模式、内置覆盖、两路模型和审查限额使用原生控件，移除整段进化 JSON 编辑。无关保存保留关闭状态下的旧数据；移除旧字段需确认当前 revision，转换保持启用状态且要求安全审查通过。原快照的文件技能枚举及设置保存器的 extensions 启停写入已删除，管理入口统一使用 Registry。托管来源和导出路径已接通 YAML、原生来源编辑器及保存/启动共同校验；全自动进化、旧历史导入及真实桌面验收仍待完成。

后续增量：共享 schema 已扩到模型和子代理。模型页改为原生 provider 选择、明确的 context_window，移除 use_path、无效 thinking/reasoning 标记和任意高级 JSON。测试连接正确选择 Ark 适配器。旧主 Agent 导航内容、增删/重命名、SOUL 编辑及 Go snapshot/save/backup/rollback 的目录读写均已移除；历史目录保留待显式导入。子代理页独立于旧目录，全局及逐角色字段已有表单；开关/并发控制已统一，迁移契约见下文。模型凭据仍按原始身份恢复，修改 provider/endpoint 时旧凭据及不透明扩展不随迁；消费中的旧 api.model_name 经保存规范到 runtime.model_name，避免遮蔽模型清空/重命名。

最新角色增量：两个角色 map 的 JSON 编辑器已替换为逐角色字段，增加内置/自定义选择、增删/重命名和内置覆盖重置；重命名原子更新委派引用，被引用角色需先解除引用才能删除。嵌套角色与 model_settings 字段均有共享能力声明，字段契约变化限制编辑及保存，整角色操作要求完整字段支持。工具/技能以每行一个名称输入，空白继承、`[]` 不授予，bash 的空白仍沿用内置 bash 工具集；thinking 使用继承/开启/关闭。选择角色不修改稀疏配置，未知旧字段展示诊断且保留原值。真实桌面验收仍待完成。

最新控制迁移：字段契约为 `deerflow-settings-v2`，表单仅提供 `subagents.enabled` 和 `subagents.max_concurrent`。新文件标识为 `config_version: eino-harness-v1`；旧文件按原两个开关的 AND 提供草稿，并发保留原值。明确展示 CLI 优先级差异并确认读取 revision 后，保存才会备份并删除两个旧 local_acp 键；源文件变化、格式未知或新旧字段混用时拒绝写入。运行状态另行提供子代理实际开关/并发、配置或 CLI 来源与 task 工具策略限制，界面只读显示。其他字段的有效值/CLI 来源及历史类路径迁移仍待完成。

此前运行/记忆增量：`internal/configschema` 已提供 runtime/memory 两组的共享字段定义、校验及 snapshot/daemon 状态声明。Waku 对这两组按配置工具和运行服务的字段定义取交集；缺失或不兼容时只读并保留草稿。记忆页面仅保留 enabled/mode/injection/retrieval；运行页删除 agent_name、thinking/plan 和 goal_*，并发范围统一为 1–64，运行/排队超时统一为 0.001–86400 秒，指令长度按 Unicode 字符计算。global 不再作为可选值，关闭记忆时允许保留旧值。全页面 schema 与实际生效验证继续实施。

对应 snapshot/defaults/save 已停止生成旧记忆存储类、无效数值参数、假定的 memory.json/FTS 路径及废弃运行默认值；记忆保存仅更新四项原生字段，并将实际消费的 retrieval 别名规范到根字段。原文件中其他未知键和凭据仍保留，待显式迁移删除；不能将“没有重新生成”描述为旧配置和 Python 源码均已清空。

检查入口：

- `desktop-app/src/app/deerflow_settings.rs`：`fields_for` 的全部分组及默认模型按钮。
- `go-harness/internal/desktopconfig/document.go`：snapshot、runtime/memory defaults；`save.go`：保存校验和 YAML 映射。
- `go-harness/internal/launch/config.go`：`ApplyConfig`、`applySubagents`、`applySkillEvolution`、`applyBuiltinTools`，即 daemon 实际读取的入口。
- `go-harness/extensions.go`：实际技能/记忆/工具装配；`harness/builtin_tools.go`：工具及 provider 契约。
- `desktop-app/crates/waku-core/src/deerflow_config.rs`：本地配置程序、MCP 文档与服务应用操作。

“保留”表示具有实际消费路径；“移除”包含普通页、高级页、默认值和保存投影；“替换”要求先实现对应的新契约。旧值只能经明确迁移保留为历史数据，不能在新版表单中继续被表示为有效设置。

## 1. 模型

| 字段 | 当前消费依据 | 实施决定 |
| --- | --- | --- |
| `/default_model` | `ApplyConfig` 选择默认路由，模型默认按钮写入 | 保留；使用配置名称引用 |
| `/models/*/name` | `modelDocument.Name`、模型/角色/审查器名称解析 | 保留；重命名事务更新所有有效引用 |
| `/models/*/display_name` | `cfg.Models` 的显示名称 | 保留 |
| `/models/*/description` | 配置保存/列表描述，未作为运行参数解析 | 作为可选描述元数据保留，不描述为模型指令 |
| `/models/*/model`、`base_url`、`api_key` | provider 路由、环境变量解析 | 保留；API key 脱敏和稳定身份恢复 |
| `/models/*/clear_api_key` | `validatedModels` 明确清除凭据 | 保留为界面操作，不写成 provider 参数 |
| `/models/*/use_path`（已移除），现 `/models/*/provider` | Go 保存及启动的原生 provider | 已替换为 openai/ark/claude 枚举；旧 source 的 use 仅读取规范化，保存写 provider |
| `/models/*/supports_vision` | `cfg.Media.VisionModels` | 保留；仍受 provider/所选模型真实能力约束 |
| `/models/*/supports_thinking`、`supports_reasoning_effort` | 启动器不读取这两个标记 | 已从输入、snapshot/defaults 移除；不重新生成，原文件中的旧值待显式迁移 |
| `/models/*/advanced` | 原任意对象与实际消费脱节 | 编辑入口和透传保存已移除；context_window 作为原生数值字段校验/保存/启动。原 provider 的未知数据在无关保存中保留，更换后端时不转移 |

## 2. 智能体与子代理

| 字段 | 当前消费依据 | 实施决定 |
| --- | --- | --- |
| `/agents/*/name`、`display_name`、`description`、`model`、`memory_enabled`、`soul`、`tool_groups`、`skills` | Go 不选择旧主 Agent | 整个编辑器、增删及其目录读写已移除；旧 Agent 可经显式预览转换到角色草稿，再用当前角色校验/保存 |
| `/subagents` | `agents.DecodeConfig`、`applySubagents`、原生角色 Factory/controller | 已独立为子代理页，全局及逐角色字段结构化；嵌套 schema、真实严格解码及 provider 校验拒绝未知字段/凭据/循环。角色增删/重命名与恢复内置配置操作已接线，真实桌面验收仍待完成 |
| `/runtime/subagent_enabled` | 旧文件的 ACP 开关 | 编辑入口已移除，显式迁移后只写 `subagents.enabled`；原双开关按 AND 合并，CLI 优先级变化需确认 |
| `/runtime/max_concurrent_subagents` | 旧文件的根并发数 | 编辑入口已移除；保存器及启动器共用 `subagents.max_concurrent` 1–64。旧值显式迁移并删除影子键 |

新角色区覆盖名称、description、system_prompt、model、temperature、thinking/reasoning、工具/技能白名单及拒绝列表、max_turns、timeout_seconds、delegate_to；全局区覆盖 enabled、max_depth、max_concurrent、max_tasks_per_root、background_workers、auto_continue、max_auto_continuations。字段定义以 `harness/agents.go` 和 `internal/agents/config.go` 为依据；界面保持 `null` 继承和 `[]` 不授予的区别。同名旧内置角色仅经迁移映射，不能静默丢弃。

## 3. 长期记忆

| 字段 | 当前消费依据 | 实施决定 |
| --- | --- | --- |
| `/memory/enabled` | `MemoryEnabled`、`MemoryExtraction` | 保留 |
| `/memory/injection_enabled` | `injectMemory` | 保留 |
| `/memory/retrieval_enabled` | `SnapshotSearch` / bounded List 分支，tool 模式要求开启 | 保留；说明关闭检索不等同于关闭全部记忆 |
| `/memory/mode` | `MemoryMode`，middleware/tool | 保留两个实际值 |
| `/memory/model_name` | 当前启动器不读取，抽取使用已有运行设施 | 移除旧独立“记忆处理模型”入口 |
| `/memory/max_facts`、`max_injection_tokens`、`retrieval_top_k` | 旧保存器校验，但 Go 装配仍采用固定边界，例如 4096 bytes、6 条检索结果 | 移除无效可编辑项；将来接入 Go 原生限制时重新通过能力 schema 开放 |
| `/memory/fact_confidence_threshold`、`fact_dedup_enabled`、`fact_dedup_similarity_threshold` | 启动器未读取这些旧 backend 配置 | 移除 |
| `/memory/debounce_seconds`、`shutdown_flush_timeout_seconds` | 未映射到 Go 的事务/清理逻辑 | 移除 |
| `/memory/storage_path`、`retrieval_index_path` | Go 使用 SQLite，旧 memory.json/FTS5 路径不属于当前存储参数 | 旧路径输入及伪造默认值已移除；实际数据目录只用状态返回的值展示 |
| `/memory/storage_class`、隐含的 `manager_class` | 旧 Python 实现选择，非 Go 动态装载入口 | 从表单、默认值和保存结果删除；实现名称可作为只读说明 |
| `/memory/advanced`、`backend_advanced` | 当前保存未知参数，实际运行不消费 | 删除任意 JSON 编辑；不保留一个可绕过清理的高级入口 |

## 4. 技能与进化

| 字段 | 当前消费依据 | 实施决定 |
| --- | --- | --- |
| `/skills_enabled` | `skills.enabled` 控制加载 | 已纳入共享 schema 与 Waku 字段交集；与 evolution.enabled 联合校验 |
| `/skill_sources/import_path`、`export_root`、`include_global` | Go 来源解析及 SkillSelection | 原生目录输入和可空全局开关；映射到 skills.path/export_root/include_global，保留相对路径、null 继承及未知原始根字段 |
| `/skill_sources/managed/*/id`、`root`、`scope`、`workspace` | Registry 来源身份与工作区约束，候选授权从来源派生 | 原生逐来源编辑及增删；父/子契约共同控制，32 来源总上限；目录校验复用启动路径。移除声明不删除历史或文件；身份变更可能使旧任务无法恢复 |
| `/skills/*/enabled` | Registry 提供持久启用状态 | 原编辑入口、snapshot 文件枚举和 save 的 extensions 写入均已删除；通过 Go Registry `skill.list/get/set_enabled` 管理，旧客户端提交 skills 数组被拒绝 |
| `/skill_evolution` | `DecodeReviewConfig` → `applySkillEvolution` → SDK evolution store/workers | 原生控件覆盖三路模型、generation、validation、budget 及下表 P4/P5 字段。嵌套父/子契约必须匹配；生成/审查限额的零值使用原生默认，预算必须为正数。新增非法编辑在关闭时也被拒绝；旧模型名不自动启用生成 |

提案按钮对应 Go `proposal.list/get/approve/reject/history/rollback`。已接入有界分页、source、状态版本、完整 head/target 回滚前置条件、评测与 diff 展示；技能目录加入内置覆盖来源、源变化报告和导出重试。MANAGE 请求在后台执行，传输失败保留原命令及幂等 key，冲突要求刷新。批准候选工具调用和批准正式发布保持独立。生成作业、纠正反馈、缺失报告、发现状态、行为报告及自动补丁观察已有各自管理入口，具体能力以服务声明为准。

### P4/P5 新增字段与实际消费者

下表路径均以 `/skill_evolution/` 开头。原生 YAML 使用同名点分路径；Waku `evolution::fields()`、`settings_contract`、Go `configschema/evolution.go`、`desktopconfig.validatedEvolution` 与 `launch.applySkillEvolution` 共用这条保存/启动链路。

| 字段 | Go 实际消费路径 | 语义与边界 |
| --- | --- | --- |
| `discovery/operator_corrections_enabled` | `DiscoveryOptions.DisableOperatorCorrections` → 纠正信号选择器 | 默认关闭；启用要求 evolution 与 generation 同时开启，重复独立根任务的明确纠正才能生成候选 |
| `discovery/missing_reports_enabled`、`successful_workflows_enabled` | `SkillDiscoveryOptions` → 缺失报告/成功流程选择器 | 分别显式启用；仅选择当前授权工作区中的持久证据，成功流程以宿主工具回执为依据 |
| `discovery/new_skill_source_id` | `SkillDiscoveryOptions.NewSkillSourceID` → 新技能目标解析 | 缺失/成功发现要求有效非内置来源；运行时重新核验目标授权，新建技能保持人工审核 |
| `discovery/repeat_threshold`、`repeat_window_days` | discovery store 的证据分组及时间范围 | 默认 2 / 30；范围 2–16 / 1–365。同一根任务的子任务、重试和反馈修订不重复计数 |
| `discovery/cooldown_hours`、`max_daily_proposals`、`max_pending_proposals` | 持久发现准入事务 | 默认 24 / 5 / 20；范围 1–8760 / 1–100 / 1–1000，自动入队还受维护调用/token 预算限制 |
| `behavior/enabled`、`suite_path` | `LoadBehaviorSuite`、`ConfigureBehavior` | 默认关闭；启用要求 generation，读取可信宿主套件；候选不能修改评分规则 |
| `behavior/timeout_seconds`、`max_steps` | `BehaviorModelConfig.Timeout/MaxModelCalls` → 行为评测器 | 默认 60 / 6；范围 1–300 / 1–32，基础/候选分别在 fixture 工具上下文执行，还受维护作业时限约束 |
| `behavior/max_input_bytes`、`max_output_tokens` | `BehaviorModelConfig` → 模型输入/输出限额 | 默认 131072 / 1024；范围 1024–2097152 / 128–8192；调用计入维护预算 |
| `mode` | `AutoPatchOptions` → `ConfigureAutoPatch` / `StartAutoPatch` | `review` 默认；`auto_patch` 要求已启用行为评测。关闭恢复人工审核并保留历史 |
| `auto_patch/max_changed_lines` | 自动发布资格检查 | 默认 40，范围 1–200；仅已托管、存在且启用的技能 `SKILL.md` 小补丁，需真实通过安全/质量/行为报告；新建、删除、覆盖、支持文件或大改动转人工 |
| `auto_patch/max_daily_publications` | 自动发布事务的 UTC 日限额 | 默认 2，范围 1–20；同一目标最多一个活动观察，发布与观察记录原子提交 |
| `auto_patch/observation_hours`、`min_observations` | 持久观察 deadline 与终态判定 | 默认 72 / 3；范围 1–720 / 1–100。有效质量观察不足记为 insufficient_evidence，不以无故障冒充通过 |
| `auto_patch/rollback_failure_threshold` | 观察 worker 的连续失败计数与 head CAS 回滚 | 默认 2，范围 1–100；仅明确归因的质量失败计数，确认质量成功清零，外部故障忽略；反馈撤回/修订重算，较新人工发布不会被旧观察覆盖 |

旧 `discovery.enabled` 及旧 probation 参数没有可编辑入口。它们仅作为显式迁移输入或历史归档；新的观察记录通过 auto_patch 管理接口读取。未启用的进化组可以保存有效参数，但不会据此启动 worker。

## 5. 工具与权限

| 字段 | 当前消费依据 | 实施决定 |
| --- | --- | --- |
| `/runtime/permission_mode` | `PermissionMode.RequiresPermission` | 保留 all/dangerous/off |
| `/runtime/enable_bash`、`/sandbox/allow_host_bash` | Windows Go 命令工具与宿主授权联合映射 | 保留实际联动，命名说明为命令/PowerShell；冲突在保存及启动时同样报错 |
| `/sandbox/allow_host_tools` | `HostToolsAllowed` 门控 host_opencli | 保留；不能借开关安装工具或扩大命令白名单 |
| `/runtime/tool_allowlist`、`tool_denylist` | `cfg.ToolPolicy`，运行装配和执行时再次核验 | 保留；null/空数组语义一致 |
| `/client_mcp_servers` | Rust 对 stdio/绝对命令/宿主白名单与 env 验证，交给 ACP 客户端 | 保留 stdio；展示宿主策略的只读状态；没有宿主授权时给出具体不可用原因 |
| `/tools` | `BuiltinToolConfig`、原生工具工厂 | 已替换为逐工具原生字段及添加/移除配置操作；schema 声明参数适用工具和结果范围，删除 `use` 类路径及任意 JSON 编辑。原文件未知数据保留并诊断；不接受草稿注入 |
| `/tool_groups` | 旧配置元数据，不能控制 Go 角色工具授权 | 编辑器和保存时的分组存在性限制已删除；保留原始分类数据供显式迁移，不在 snapshot 重建；授权按工具名称 |
| `/sandbox/advanced` | daemon 不消费任意 sandbox 对象 | 任意 JSON 编辑和旧本地参数白名单已删除；仅开放实际宿主授权。原未知键保留并诊断，CLI 沙箱 provider/enabled/allow_shell 的生效值只读展示 |
| snapshot 的 `/sandbox/use`（已移除） | 原生 provider 的兼容投影 | 已改为 `/sandbox/provider`；快照规范化本地类名，保存只写 provider；其他执行器需显式转换草稿，未知参数保留供迁移 |

OpenCLI 使用 Go 宿主工具及现有启动/超时/错误回执链路。前台会话的 MCP 配置仍受宿主允许列表约束。后台重建使用显式宿主 `mcp.host_recipes`，属于仅宿主 YAML 可编辑的配置；schema 的 `HostSettings` 说明其应用方式，不向模型或桌面草稿暴露凭据。该链路的独立集成验证进度见实施状态，不能从前台设置保存结果推导后台互操作结论。

工具参数和 sandbox 已加入 v2 schema 及 daemon/Waku 字段交集。工具允许/拒绝列表改为逐行名称，保留 null 与 []；OpenCLI 的 null 使用内置站点，[] 不授予适配器访问（仍能使用顶层发现命令）。工具配置条目的增删与工具授权分开：文件/命令工具仍由工具策略及宿主开关控制。API key 和含凭据的代理地址只显示已配置状态；留空保留同一原始工具的值，另有明确清除操作，重排不丢密钥，重新添加不继承隐藏值。测试覆盖保存→Go 启动→原生 web_fetch 经本地代理实际取回内容并截断；真实 Waku 交互和实际 CLI 覆盖仍待验收。

## 6. ACP 运行

| 字段 | 当前消费依据 | 实施决定 |
| --- | --- | --- |
| `/runtime/model_name` | 默认会话模型配置名称解析 | 保留；与全局默认的继承关系可见 |
| `/runtime/agent_name` | save 已拒绝非空值，启动器没有选择实现 | 移除 |
| `/runtime/thinking_enabled`、`plan_mode` | 启动器不读取旧全局默认值 | 输入及 snapshot/defaults 已移除；单会话/角色的实际能力沿独立契约提供 |
| `/runtime/max_active_connections`、`max_active_runs` | daemon 连接及运行准入限制 | 保留 |
| `/runtime/run_timeout_seconds`、`queue_timeout_seconds` | Go 运行/排队超时 | 保留；浮点秒范围和 UI 提示与启动校验统一 |
| `/runtime/memory_scope`、`/memory/include_global` | 2026-10-04 接通 session/workspace/global；全局身份由用户数据库固定 | 保留三个值及共享读取开关；global 读取全局＋项目，自动提取仍写入项目，参见全局记忆实施记录 |
| `/runtime/session_cleanup_enabled`、`inactive_session_retention_days`、`closed_session_retention_days`、`session_cleanup_interval_seconds` | `cfg.Retention` | 保留；等待审批、后台任务、回执等保护条件仍由 runtime 执行 |
| `/runtime/goal_auto_continue`、`goal_max_continuations`、`goal_max_no_progress_continuations` | 旧目标续跑；开启时 Go 明确拒绝 | 移除；不能直接映射到语义不同的子任务通知自动续跑 |
| `/runtime/prompt_overlay` | Go 附加指令，参与运行资源契约 | 保留长度限制和重启应用反馈 |

子代理开关和并发上限移至第 2 节的统一入口，去掉页面中“自定义子智能体尚未接入”的旧提示。新版运行状态只报告 Eino Go，删除双后端选择及 Python 回退文案。

## 7. 已接通范围与剩余验收

共享 schema、字段交集、原生角色/工具/进化表单、Registry/提案管理、显式配置转换、旧数据导入、保存与应用 revision 分离及后台管理请求已实现。本清单是审计产物，运行契约仍以 Go schema 和实际服务声明为准。

本次沿字段映射的静态检查未再发现“可编辑但没有消费者”的项。实际生效状态包含默认模型、运行/排队/清理、权限、记忆、沙箱、技能源、进化 worker 开关与子代理并发。后续补齐的投影使用已成功装配的 Go 配置及同次 launch 解码结果，Waku 缓存解析后沿原有只读布局展示：

- P4/P5 标量限额仅在相应 worker 已启用时返回，不把关闭状态的参数表示为运行中的策略。审查/生成零值已换算为原生默认；预算、发现、行为和观察限额使用已加载值。
- `roles` 单独容纳最多 64 个配置角色，只列已加载的稀疏覆盖，保留显式空列表与省略继承的差异。角色模型、思考、限额、工具/技能/委派名单使用明确 allowlist；指令及说明只给 SHA-256。它们表示宿主新任务配置，具体执行仍受父角色、会话与预算限制。
- `tools` 单独容纳最多 13 个已配置工具，仅展示消费中的超时、结果数、输出长度和 OpenCLI 站点。结果数表示没有调用级覆盖时的默认值；命令入口仅给配置摘要，不将其冒充最终进程 argv。API key、代理 URL、环境变量和参数均不投影。
- 两类明细不挤占原有最多 96 项的标量 `fields`，旧客户端仍可读取已有状态；新客户端只解析明确允许的字段，无法解析的响应不以本地草稿补值。没有改动运行授权或新增配置项。

剩余交付验收包括真实 Waku GUI 的编辑/保存/应用与错误反馈、配套 Release 产物启动、干净无 Python 环境和真实外部 MCP/模型互操作。既有 Go/Rust fixture 的命令和结果集中记录在实施状态；本次不重复运行测试，也不将静态映射核对当作 GUI 或最终发行验收。
