# Eino Harness 全局记忆

实施日期：2026-10-04；2026-11-16 补充阶段 2–5 的写入、后台预算、Flush/索引与安全重新学习。此分支使用 Go 与 Eino，无 Python 运行依赖。修复仍在工作区验证，尚未部署，见[修复计划](memory-fix-plan.md)和[阶段 5 报告](memory-stage5-report.md)。

## 读取和写入范围

| 范围 | 共享边界 | 普通模式自动提取 | single_user 自动提取 |
| --- | --- | --- | --- |
| global | 同一份 Waku user-data，跨项目、会话、模型 | 不自动写入；显式创建、导入或提升 | 允许 profile/preference |
| workspace | 当前项目目录下的会话 | memory_scope 为 workspace/global 时写入 | 只允许 project |
| session | 当前会话 | memory_scope 为 session 时写入 | 不允许选择该模式 |
| user（兼容旧接口） | 旧宿主用户身份＋项目 | 保留原有 SDK 行为，不等同于 global | 使用安装的 global profile |

默认读取全局＋项目。当前用户要求优先于记忆；描述性事实冲突时依次优先使用会话、项目、全局。记忆不能授予权限或修改工具策略。

全局 preference/profile 可以在问题关键词未命中时进入有界自动注入；显式 search_memory 仍要求查询匹配。现有自动注入 4096 字节预算、每范围最多 6 个检索结果及管理列表 1000 行/3 MiB 上限保留。工具模式只在显式检索时读取。checkpoint 的执行资源快照保持固定；single_user 在模型边界投影时移除已删除/更正的旧项并保留来源标签，不改写历史消息。

## 配置

```yaml
local_acp:
  memory_scope: workspace
memory:
  enabled: true
  include_global: true
  mode: middleware
  injection_enabled: true
  retrieval_enabled: true
```

include_global 默认 true；关闭后模型只读取选定的本地范围，管理员仍可管理全局。普通模式 memory_scope 的 global 值表示读取全局＋项目、自动提取写项目；启用记忆时与 include_global:false 冲突会明确拒绝。memory.enabled:false 停止模型访问、提取与 embedding 调用，仍保留事实。single_user 要求共享 global profile，个人事实可自动写 global，所有模型候选都必须通过[字段/证据规则](memory-write-policy.md)。

memory.daily_attempts 是 single_user worker 的附加每日上限（默认 200，0 暂停新调用）。阶段 3 已将后台请求绑定到来源 root 的专用 memory_job member，逐次预留、分派与 usage 结算；保存完整响应后重启只重放落库，未知消耗保留 hold 并阻止重发。阶段 4 已接入固定输入集合的异步 Flush，具体状态与索引行为见下文；本分支尚未部署。

全局身份保存在 harness.db 的 memory_profile 单例表，创建后不随连接 owner、模型、安装目录或项目改变。复制整份便携 user-data 会保留身份和事实；另一独立数据库会生成独立身份。没有云端同步，也不会合并不同便携包的数据库。

旧 session/workspace/user key 原样保留，新 global 使用独立命名空间。开启全局读取不会把旧事实搬入全局。升级时记忆执行策略指纹更新，旧策略固定的中断运行须结束后重新发起，避免以变化的范围继续旧执行。

## 管理入口

Waku 高级设置的记忆页提供全局、项目、会话切换。全局不依赖历史会话；项目和会话通过选中的历史会话定位。非全局事实可点击“提升为全局…”，确认后复制原内容，保留原事实。源版本在用户确认后发生变化时拒绝操作并要求刷新；同类别且内容忽略首尾空白及 SQLite lower 后相同的事实不会重复创建。全局删除只影响后续读取，原项目副本保留。

本地管理支持 memory.get/delete/promote；提升必须传 source scope、fact_id、expected_revision，不能由客户端选择全局身份。修改时暂停新运行准入，并拒绝在已有操作执行中修改。

ACP 支持 scope:"global" 的已有 CRUD/search/list，以及 `_deerflow/memory/promote`。提升参数为 sessionId、scope（源范围）、factId、expectedRevision。所有 ACP 操作仍要求拥有有效附着会话；客户端不能指定 profileId。SDK 对应 `PromoteMemoryFact`。deerflow-memory-migrate 的 --scope global 是显式导入入口，无模型调用。

提升事务原子校验源版本、查重及写入，保留 operator 来源、源 fact ID 和源版本以供追溯。

## Flush 与索引（阶段 4）

SDK 新入口 `FlushMemoryWithStatus(ctx, sessionID)` 和 ACP `_deerflow/memory/flush` 在总计 10 秒内先取得前台槽、固定本会话已有输入决定/job 集合，再释放槽并等待。后续输入与其他会话不扩大集合，不调用模型或提前重试。返回 `flushed/pending/failed/skipped/discarded/blocked/completed/total/reason`，并附 `barrier`、必要时 `nextAt/waitReason`；计数单位是决定/任务，completed 也不是新增事实数。只有无 pending/blocked/failed 且同步屏障成功才 flushed=true；不保证向量已建立。

超时返回本次已观察统计和 reason=timeout；等待前台超时为 foreground_timeout，尚无固定集合。显式取消与状态查询/协议错误走 error 路径。旧 `FlushMemory` 返回可用 errors.Is/As 检查的 `harness.MemoryFlushError`，区分 pending、blocked、failed，并保留 DeadlineExceeded 兼容。ACP 能力中声明 flushStatusVersion=1，原 version=1 和 flushed 字段保持；旧客户端忽略新增字段即可。历史 failed 不因当前队列为空而消失，quota_skip/no_facts/requires_explicit/显式动作计入 skipped。

只要 memory.enabled 允许且配置了 embedding_url/model，独立索引 worker 就会启动，与 single_user、MemoryExtraction 或自定义 Engine 无关。每轮最多取 16 条、最多 15 次请求、embedding 调用预算 15 秒（超时后另有至多 5 秒持久化清理）；输入失败优先拆到单条，服务故障按模型整体退避。重试按 `(fact_id, revision, model)` 持久化，默认最多 3 次失败、60 秒起指数退避、上限 3600 秒，可用 index_max_attempts/index_retry_seconds/index_max_retry_seconds 配置。HTTP 400/413/422 视为输入拒绝，其他 HTTP/网络/响应身份错误视为服务故障；自定义 embedder 应用 ErrEmbeddingInput 明确标注输入错误。

坏样本达到上限保留 terminal；新 revision/model 获得新机会。SDK `MemoryIndexFailures(ctx, limit)` 可读取当前版本/模型的固定错误码、attempts、nextAt 和 terminal，不返回内容；limit 范围 1–1000。迟到向量必须通过当前 head、deleted、model 校验，Close 取消并等待两个 worker。开启词法读取不依赖向量就绪。

## 删除后的重新学习（阶段 5）

single_user 只对删除后新接受的、通过宿主字段规则的声明开放自动恢复。新值必须与被删除版本的可信结构化值不同，来源删除代次和 slot revision 必须仍匹配；同值换大小写、标点、Unicode 形式或已登记语言别名会保持删除。旧事实、人工自由文本修改、通用属性或空 tombstone 无可靠值身份时要求新的明确记住/更正请求或管理操作。

该行为没有新增 YAML 开关；只在新版本程序中、记忆与自动提取已开启且所有来源/预算条件满足时生效。memory.enabled:false 停止模型记忆访问；daily_attempts:0 仅暂停新调用，已付费结果仍可重放，不能作为冻结写入的开关。回执 requires_explicit 不表示保存成功，工具应解释需要显式请求。恢复保留 fact ID、增加 revision，旧索引失败不会阻挡新版本。字段规则和原因码见[写入协议](memory-write-policy.md#删除后的安全重新学习阶段-5)，上线及回退见[运行手册](memory-upgrade-runbook.md)。

## 初始全局记忆实现验证（2026-10-04）

仅运行受影响的定向用例，覆盖跨项目共享、本地隔离、提升 CAS 与查重、重启身份与事实保留、全局偏好注入/检索/关闭读取、ACP owner 和身份拒绝、配置保存→Go 启动，以及自动提取不得写全局。Rust 用例检查无会话的全局请求、源范围、审查版本及禁止自提升。未进行视觉自动化或真实模型质量验收。

本地结果：8 个 Go 定向用例通过，Rust 记忆管理请求用例 1/1 通过；git diff --check 通过。Release 构建和交付包检查在对应 BUILD_SOURCE.json、SHA256 及外部构建日志中记录。

以上是初始实现的历史结果，不代表阶段 5 的发布验收。阶段 5 当前验证、构建范围和剩余门槛以[本轮报告](memory-stage5-report.md)为准。
