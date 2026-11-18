# Eino harness 记忆与上下文压缩实施设计

状态：设计已核对 pinned Eino alpha.35 和当前 Python DeerFlow；受共享预算计费的模型工厂、结构化事实存储、SDK/ACP 管理、只读注入、显式启用的同步提取与终态提升、显式启用的 Eino 摘要压缩、同步写入屏障 `FlushMemory`，以及显式映射的旧 DeerMem JSON 导入已落地。提取策略与真实模型的校准仍待完成。

2026-11-16 更新：下面同步 staging/终态事务描述适用于普通模式。single_user 使用显式工具和持久队列；阶段 2 已接入[模型记忆写入协议 v2](memory-write-policy.md)，阶段 3 已补齐来源 root 下的独立 worker attempt、预算预留/结算与重启恢复。阶段 4 已实现固定集合异步 Flush 与独立索引，阶段 5 已增加 schema v5 的结构化值身份和删除后新输入策略，见[修复计划](memory-fix-plan.md)；这些改动尚未部署。

安全重新学习只作用于 single_user 的 keyed 管线：新接受人工输入固定 relearn_policy、删除代次和 slot revision，值身份与 fact revision/回执同事务保存。旧 checkpoint/输入不补拍新策略；没有可信值身份的历史记录不自动恢复。新结构化值不同才允许自动恢复，同值或未知身份要求新的明确请求。普通 staging 管线的作用域、终态事务、owner/profile 与只读限制保持原边界，详见[恢复规则](memory-write-policy.md#删除后的安全重新学习阶段-5)和[升级手册](memory-upgrade-runbook.md)。

## 现有语义与边界

Python 默认 DeerMem 保存带 `scope`、`durability`、`authority`、confidence、来源的结构化事实。其写入筛选用户输入与最终助手回复，排除工具调用、一次性任务授权和明显的任务路径；读取按 workspace/session/user 范围约束，FTS5/BM25 检索与可选 MMR 后限定注入额度。`memory` 可设 middleware 或 tool 模式。当前 Python 摘要默认关闭；开启后按消息或 token 阈值触发，保留最近消息和有界的 Skills 读取证据；摘要失败不能伪装为成功摘要。

Eino `v0.10.0-alpha.35` 的 `automemory` 默认使用 `MEMORY.md` 和主题文件。Read 的主题选择、Write 的提取都可能另行调用模型；Write 的同步模式仍直接调用文件后端。其协调 cursor 是 native 消息数量，摘要删减消息后不能作为稳定的跨重启提取位置。默认本地 coordinator 也不能代表 SQLite 中的会话状态。Eino `summarization` 的 `Model` 由中间件直接 `Generate`，若传入原 provider model，会绕过当前 `modelLifecycle` 的预算预留、usage 结算、I/O join 和错误传播。

第一版保留 DeerMem 的事实语义。可以复用 Eino 中间件的注入和摘要时机，不能将默认文件后端、独立模型或原生 cursor 当作持久语义的权威。原始会话事件和工具回执始终完整保存；压缩只改变进入模型的上下文视图。

## 存储与公开操作

新增 `internal/memory` SQLite 事实存储，主库是权威；FTS5 只是可重建索引。表至少包含不可变 fact revision、当前 head、范围键、来源 event/run/input、分类、confidence、正文、创建/撤销时间和提取策略版本。范围键由规范化 workspace、会话和可配置 user/agent 身份构成，不从模型文本推断。每次读取固定 revision/cursor，后续 resume 使用同一注入快照或摘要 checkpoint，不悄悄换成更新的事实。

公开 SDK `MemoryFacts`、`MemoryFact`、`DeleteMemoryFact`、`ClearMemory` 和 `FlushMemory`；ACP 只声明已实际实现的 `_deerflow/memory/*` 扩展，保持 owner、范围和版本校验。删除产生 tombstone revision，FTS 索引可重建，不改写历史来源。全局/用户范围需要显式身份配置；没有身份时默认仅 workspace + session 范围。旧 Python JSON/FTS 数据在迁移命令中显式导入，不在启动时猜测归属。

读取使用保守词法检索作为 FTS 不可用时的回退；查询和 TopK/注入字节数有上限。模型只得到标明为描述性数据的 `<relevant_memory>` 区块；事实不会产生新的工具权限、预算、审批或配置。tool 模式提供只读搜索与查询；写入或删除工具另走现有权限和回执边界。

## 提取与持久事务

一次 foreground run 完成后，用已提交领域事件序列作为提取 frontier，读取本轮真实用户输入与最终助手消息。通知 continuation、后台 child 指令、工具结果、媒体注入占位和中间助手 tool-call 消息不能被当作新的用户偏好。来源 frontier 是 event sequence / input ID，不能依赖 `len(nativeMessages)`；同一来源的重试使用唯一键去重。

提取模型返回 schema_version=2 的受限 JSON，scope 必须属于宿主允许范围，durability=durable、authority=descriptive、confidence>=0.75，并提交原文值、完整证据及 Unicode 位置。宿主登记语法生成最终正文；metadata 与置信度不代替支持关系。候选逐条校验并保留原始序号，合法原始响应持久化供重放。拒绝权限、执行指令、凭据、引用与无法验证的事实；具体语法及模式范围见写入协议。普通模式的 assistant 回答只作上下文。

普通提取使用当前 root 的 budget member 和 attempt，调用前预留、I/O join 后结算；候选进入 attempt staging，只有主 run 终态事务成功才提升，失败/取消不发布。single_user worker 使用来源 root 的独立 memory_job member/attempt，先持久化响应和 usage，再结算并原子落库，不复用已结束的前台 attempt。重启重放已保存结果，不重复请求；已分派且消耗未知则保留 unknown 与预留。Flush 只等待固定集合，不创建模型请求或绕过账本。

## 上下文压缩

新增显式 Go 配置，默认沿 Python 保持关闭；阈值可按消息数或估算 token，保留最近窗口、活动用户输入、未决工具调用与其结果、选中 Skills 的有界内容。摘要模型必须由本次 run 的 `modelLifecycle` 包装，和主模型共享同一预算、取消、usage、I/O join。禁用默认无界重试；失败时保留原 native 状态和事件，不提交“错误文本摘要”。

摘要视图在 Eino checkpoint 中固定，并把来源 event frontier、原 native head、摘要版本、选中 Skills 的 pin 与摘要 SHA 放入 runtime manifest。恢复先校验 frontier、预算 root、资源 pin 和 checkpoint；后续用户 prompt 可以基于已提交摘要继续，但 load/history 仍从完整领域事件重放。摘要不能删除 TaskContext 中的计划、子任务、产物引用、待批交互和回执证明。若上下文已超限而摘要持续失败，返回明确的 context limit，不能悄悄丢弃历史。

## 装配顺序与验收

1. 实现 SQLite scoped facts、revision、查询/删除/清空与 FTS 重建；SDK/ACP owner 和跨 workspace 测试。
2. 加 Eino 只读记忆注入，固定 revision 到 extension/checkpoint；验证 prompt、continuation、重启、不同 session 不串事实。
3. 加受共享预算约束的同步提取、staging 与终态提升；验证取消、审批等待、重启与已用完预算。
4. 加 Eino 摘要 middleware 的受控模型包装和快照；覆盖 1 次触发、失败保持历史、Skills/工具配对、resume 和 usage 计费。
5. 接入 tool 模式、FlushMemory、迁移旧 DeerMem 数据，并做真实 SDK/ACP 管道与 Go race。真实模型与编辑器验收留在完整 V1 发布门槛。

Eino `RunExtensions.ModelHandlerFactory` 与 `PostRunFactory` 能拿到与主 Agent 共用 I/O 生命周期和预算的 `trackedModel`。事实存储保留不可变 revision 和 tombstone；FTS5 可按范围修订号重建，CJK 与索引不可用时使用有界词法回退。SDK 与 ACP 管理接口共享会话归属和前台槽，ACP 只声明实际可用的 scope。Eino DeepAgent 本轮指令注入选中事实；所选内容和范围修订号保存在 extension state，显式恢复不重新检索。

`MemoryExtraction` 默认关闭，可经 SDK 配置或两个 CLI 的 `--memory-extraction` 显式开启。成功的前台父 Agent 回答后，受控模型读取本轮真实用户输入与最终回答并输出受限 JSON；确定性筛选仅接受有界、durable、descriptive 的 workspace/user 候选。候选进入 attempt staging，来源绑定领域事件 sequence、run/input/attempt；只有执行完成且预算、运行终态同事务成功才提升。失败、取消或等待不会发布，scope revision 漂移会舍弃候选，同文重复不新增事实。额度不足时明确记录 `quota_skip`，避免已完成的主回答因可选提取而变成超额失败。后续需要扩充策略评测。不要把默认 `automemory.New` + `WriteModeAsync` 直接加入 `RunExtensions.Handlers`；它的写入与 cursor 不能满足本设计的事务和恢复边界。

`Compaction.Enabled` 默认关闭，CLI 使用 `--context-compaction` 显式开启。消息数和估算 token 任一阈值触发 Eino `summarization`，其模型经原 run 的 `trackedModel` 共享预算、usage 和 I/O 生命周期，不做额外重试。自定义 finalizer 拒绝空白、工具调用和过长摘要，保留活动用户回合及完整工具调用/结果链，最多保留 64 KiB 最近消息，并以最多 8 KiB 的历史片段保留已读取 Skill 文本。超限时显式失败而不丢弃活动回合。摘要成功后 Eino 写入 `messages_replaced` 原生事件；业务历史与工具回执不裁剪。执行与后台任务的恢复 manifest 额外绑定最近原生替换事件的序号、ID 和摘要载荷 SHA。当前实现尚未给完成态摘要单独建立业务索引；提取策略与真实模型的摘要质量仍须校准。

普通模式提取在前台 run 的终态事务中发布。`FlushMemory` 在 SDK/ACP 按会话归属等待前台槽，最多 10 秒，随后读取同一 SQLite 数据库形成持久化屏障；它不会重新提取额度不足的轮次，也不会提升失败或中断 attempt 留下的 staging。single_user 使用 FlushMemoryWithStatus 固定既有输入/job 集合，释放前台槽后等待；pending/blocked/failed 均不能返回 flushed=true，保留 skipped/discarded 与历史失败，状态查询失败不能视为空队列。旧 SDK 保留 error 接口，ACP 保留 flushed 并增补统计。索引独立运行，持久退避、坏样本拆批和版本/删除校验见[全局记忆说明](eino-global-memory.md#flush-与索引阶段-4)。

旧 DeerMem 的 `memory.json` 可用 [显式迁移命令](eino-memory-migration.md) 导入。旧 `acpmem-*` 桶无法反向证明工作区归属，迁移要求明确的既有 Go 会话、工作区与 scope。只导入通过内容筛选的 `facts`，不把旧摘要桶或 FTS 索引当作事实；事务内保持幂等和冲突回滚。Python 源文件不改动。
