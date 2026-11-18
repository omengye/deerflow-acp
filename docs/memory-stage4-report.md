# 记忆修复阶段 4 实施报告

日期：2026-11-16

结论：阶段 3 的预算、结果持久化与恢复协议支持原有 F5/F8 方向，无需改变阶段顺序。阶段 4 已完成固定集合 Flush、任务处置、独立索引与持久重试，确定性验收和相关回归通过。阶段 5 尚未实施，自动恢复 tombstone 保持关闭。本次没有部署、重启本机安装、修改其配置或数据库，也没有创建 commit。

## 1. 对阶段 3 报告与后续计划的复核

阶段 3 的独立 member/attempt、预算准入、unknown 保护、已付费结果重放提供了 Flush 必须读取的真实状态。实施前重新运行剩余两个复现，Flush 提前成功与坏 embedding 样本阻塞均因目标断言失败；不是环境错误。现已修复并移入默认测试套件。

原计划无需方向性纠正，实施中补齐了三处细节：有 job 的 quota_skip 与无 job 的跳过决定统一计数；会话删除沿用现有在途/未知费用检查，通过后才事务性处置剩余任务；拆批优先到达单条，防止整批坏样本反复耗尽拆分额度却永远没有失败次数进展。阶段 5 增补了新 head revision 与索引失败状态的衔接要求，不改变安全重新学习的来源、值身份和 CAS 门槛。

## 2. 实现内容

### 固定集合 Flush 与兼容接口

新 SDK `FlushMemoryWithStatus` 在总计最多 10 秒内验证会话 owner、等待前台槽，在短事务中固定本会话已有输入决定与 job ID 集合，再释放槽。等待只查询该集合；其他会话、之后接受的输入不能扩大集合，不持有前台槽或数据库事务等待模型。

返回 flushed、pending、failed、skipped、discarded、blocked、completed、total、reason，以及集合 barrier、必要时 nextAt/waitReason。计数互斥，单位是输入决定/任务，不是候选或新事实数。只有 pending/blocked/failed 均为零、同步屏障成功时 flushed=true；不承诺向量已建立。历史失败、意外丢失的行、查询失败不能伪装成空队列成功。

| 状态 | 处理 |
|---|---|
| queued/running | pending；可区分 retry_delay、daily_limit、budget_held，保留可用时间 |
| blocked/budget_unknown | blocked；不释放 hold，不自动重发 |
| 来源 waiting_input/仍可恢复 | blocked/run_unresolved；不因等待时间长而丢弃 |
| 来源缺失、无法解释 | blocked/orphan_source |
| failed job/失败输入决定 | failed；无当前排队任务也不能隐藏历史失败 |
| completed/quota_skip、无 job 的 quota_skip、no_facts、显式动作 | skipped；不把跳过说成已发布事实 |
| worker 未运行 | 保留 pending 计数，reason=worker_not_running，waitReason 保留队列原因 |
| 超时 | 返回最后已观察统计；reason=timeout，等待前台超时为 foreground_timeout，此时尚无固定集合 |
| 显式取消、状态或协议错误 | 返回 error；不在取消后另开查询伪造最终统计 |

旧 `FlushMemory(ctx, sessionID) error` 保留，未完成时返回可通过 errors.As 读取统计的 MemoryFlushError；errors.Is 可区分 pending、blocked、failed，超时兼容 context.DeadlineExceeded。ACP 保留 version=1 和 flushed，声明 flushStatusVersion=1 并增补字段；测试覆盖旧客户端只读取 flushed 和新客户端完整状态。普通模式保留同步屏障，不提升失败 staging。

入口见[SDK](../go-harness/memory.go)、[运行时等待](../go-harness/internal/runtime/memory.go)、[固定集合与分类](../go-harness/internal/memory/flush.go)和[ACP](../go-harness/internal/acp/agent/memory.go)。

### 来源恢复与会话删除

恢复时将 failed/cancelled 来源的 queued 任务持久标记 discarded/run_not_completed，并记录 disposition，即使提取 worker 未启动也能执行。可恢复来源继续等待；仍有未知计费时保持预算阻塞。

会话删除先经过既有 active/reserved/dispatched/unknown/held 检查，拒绝销毁尚未核对的调用；通过后，将剩余关联任务和输入决定在同一删除事务内标记 discarded/session_deleted，清除租约并保留 no_active_call 处置记录。该标签只说明删除时无活动调用，不表示过去没有付费。故障注入证明删除失败会回滚任务状态与 disposition；unknown 的 hold 不会消失。历史 completed/failed 结果保持可解释。

### 独立索引与有界重试

只要记忆开启并配置 embedding URL/model，就启动独立索引 worker，不依赖 single_user、自动提取或是否使用自定义 Engine。memory.enabled=false 时不安装 embedder，不发起索引或查询 embedding；已有事实保留。提取 worker 不再调用 IndexPending。

新增 schema version=4 / memory-index-retry-v1，保留前三次迁移 SQL/checksum。失败状态按 `(fact_id, revision, model)` 保存 attempts、next_at、固定错误码和 terminal；模型整体退避另行持久化，重开 Store/客户端初始化不会清零。

每轮选取最多 16 个当前 head，最多发出 15 次请求，embedding 调用预算 15 秒，超时后最多再用 5 秒持久化退避。输入相关失败优先拆到单条；服务故障停止本轮剩余请求，按模型整体退避，避免后续批次立即再次调用。单个无效向量不影响同批合法向量提交。自定义 embedder 用 ErrEmbeddingInput 标注输入错误；HTTP 400/413/422 归为输入拒绝，其余 HTTP/网络/身份或数量错误按服务故障处理。

默认每个 revision/model 最多 3 次失败，退避从 60 秒指数增加，上限 3600 秒。新增宿主 YAML `memory.index_max_attempts/index_retry_seconds/index_max_retry_seconds`，SDK 对应 MemoryIndex。下调上限也应用于已有失败次数；提高上限不自动复活既有 terminal。新 revision/model 获得新的索引机会。取消不消耗失败次数，超时计入持久服务退避。

向量提交再次检查当前 head_revision、deleted 和 model/配置代次；迟到结果不能恢复旧版本。当前 head/model 的失败和终态可通过 SDK MemoryIndexFailures 查询。Close 取消并等待提取与索引两个 worker；重复启动与并发 IndexPending 不产生竞争执行者。

实现见[索引协议](../go-harness/internal/memory/index.go)与[独立生命周期](../go-harness/memory_index.go)。配置、状态和运维说明已同步到[全局记忆](eino-global-memory.md)、[上下文设计](eino-memory-context-design.md)、[配置桥接](eino-go-config-bridge.md)、[写入协议](memory-write-policy.md)及配置模板。

## 3. 验证结果

基线提交为 68753f15e51ef285260bca52388c44c786ff0f41，测试针对未提交工作区。固定使用 Go 1.26.8 windows/amd64、GOROOT 与 GOTOOLCHAIN=local。测试使用临时 SQLite、进程内假 provider/embedding、同步通道、可控索引时间与 SQL 故障注入，没有调用实际安装服务或付费模型。

| 集合 | 结果 |
|---|---|
| 实施前剩余复现 | **2 个预期目标断言失败**，退出码 1 |
| 最终阶段 4 验收 | **55 个叶子用例全部通过**：53 个阶段 4 专项＋2 个旧复现，已进入默认套件 |
| 记忆/SDK/ACP/配置/会话删除相关回归 | **270 个通过、1 个真实 embedding 测试跳过** |
| memory、budget、Eino、launch、ACP agent、configschema 整包 | **1277 个通过、2 个跳过** |
| runtime 记忆、执行、预算、后台、技能及会话生命周期相关回归 | **310 个通过** |
| 根包整包 | **434 个通过、3 个跳过**，576.506 秒，退出码 0 |

各集合存在重叠，不累加为独立测试总数。命令、包结果、专项名单、失败/跳过原因、时间及日志 SHA256 见[阶段 4 测试记录](reports/memory-stage4-tests-20261116.json)。

专项覆盖固定集合等待与新前台准入、其他会话隔离、真实 10 秒上限、取消、历史失败、无 job 决定、预算未知、owner、行丢失与查询故障、删除事务回滚、原付费结果发布后可读、SDK/ACP 新旧接口、17 条含毒样本、17 条全部拒绝、整体服务故障、重启退避、版本/模型重置、迟到向量、配置范围及下调上限、并发索引、Close 请求取消与迁移回滚。

整包回归启动后，又修正了仅有记忆表的旧库在无 queued job 时无需查询运行表、已解决的无 job 跳过在会话删除后仍应保持已解决，以及下调索引上限对持久失败状态的约束。旧库兼容由记忆整包重跑及最终相关整包覆盖；状态和上限补充由最终 55 项验收覆盖。这里不将较早启动的整包结果表述为最后每一行修改后的全仓库通过。中间的测试夹具编译、SQL 参数与 HTTP 请求体未消费问题已修正，未靠放宽业务断言通过。

## 4. 复现命令与剩余门槛

在 go-harness 目录运行：

~~~powershell
$env:GOROOT = 'D:\GO_ENV\GOPATH\pkg\mod\golang.org\toolchain@v0.0.1-go1.26.8.windows-amd64'
$env:GOTOOLCHAIN = 'local'
$stage4Go = Join-Path $env:GOROOT 'bin\go.exe'

& $stage4Go test . ./internal/memory ./internal/runtime ./internal/launch ./internal/acp/agent -run '^TestMemoryStage4|^TestMemoryFixRepro' -timeout=180s -json -count=1
& $stage4Go test . ./internal/memory ./internal/runtime ./internal/launch ./internal/acp/agent -run 'Memory|Personal|Session.*(Delete|Archive|Retention|Cleanup)' -timeout=300s -json -count=1
& $stage4Go test ./internal/memory ./internal/budget ./internal/engine/eino ./internal/launch ./internal/acp/agent ./internal/configschema -timeout=300s -json -count=1
& $stage4Go test ./internal/runtime -run 'Memory|Execution|Continuation|Background|RunQueue|Budget|Session.*(Delete|Archive|Retention|Cleanup)|SkillLearningAutoPatch' -timeout=420s -json -count=1
& $stage4Go test . -timeout=900s -json -count=1
~~~

race 未执行：当前 CGO_ENABLED=0，未发现 gcc/clang。真实模型质量/对抗评测、真实 embedding、安装数据库副本的完整升级/回滚及发布构建未执行；跳过不计为通过。未宣称 runtime 整包或全仓库全部通过。

阶段 5 的安全重新学习、完整文档与各项发布门槛仍需完成；当前 terminal 索引失败不会对同 revision/model 自动无限重试，也未新增管理 UI 或人工清除失败状态接口。

## 5. 工作区边界

开始时记录的 88 个已有修改/未跟踪文件中，69 个 SHA256 不变，19 个按需更新。该快照只记录文件，未展开当时折叠显示的未跟踪目录，不代表整个 git diff；之前的阶段报告、测试记录及无关桌面/引擎改动未编辑。新文件与此前干净文件另由测试记录保存最终代码哈希。

修改文件的 Go 格式、git diff --check 与文档链接检查通过；既有两个未修改 Go 文件的格式差异保持原样。工作止于源码、测试和文档，未部署或创建 commit。
