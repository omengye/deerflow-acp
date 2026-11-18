# 记忆修复阶段 0 执行报告

日期：2026-11-16

结论：阶段 0 完成。已固化缺陷复现、保留保护性回归，并确定后续 schema/API 版本与迁移边界；阶段 1–5 尚未实施。

范围按用户确认限定为**当前本机桌面安装**。本次新增测试与核查记录，更新修复计划的实施状态；没有修改生产实现、安装配置或运行数据库，也没有重启、部署、执行迁移或调用真实模型。

## 1. 测试基线与结果

源码基线为分支 `codex/skill-evolution-auto-20261109`、HEAD `68753f15e51ef285260bca52388c44c786ff0f41`，包含任务开始前已有的工作区改动。测试对象是该工作区，不是安装中的二进制。结束时对任务开始前记录的 18 个已有代码/测试文件核对 SHA256，均未改变；计划文档的状态更新单独保留。

新增文件：

- [存储层复现](../go-harness/internal/memory/fix_plan_repro_test.go)：内容校验、无 slot 遗忘回执、逐条过滤、显式动作优先及索引阻塞。
- [运行路径复现](../go-harness/memory_fix_repro_test.go)：真实 Run/Resume、额度耗尽、SDK Flush、按 ID 遗忘工具及可选审计失败。
- [公共测试夹具与终态事务保护](../go-harness/memory_stage0_test.go)：临时数据库、进程内流式假 provider；在输入前取消并等待记忆 worker，控制队列时序。新增终态事务失败的默认回归测试。

缺陷用例使用 `//go:build memory_fix_repro`，断言**修复后应有的行为**，在旧实现上显式失败。默认测试不加载这两份文件。后续每项修复通过后，应将对应测试移入默认套件，避免永久依赖人工运行标签。不能以“这是已知缺陷”为由忽略新的编译、夹具或其他断言错误。

| 测试集合 | 本次结果 | 记录 |
|---|---|---|
| `internal/memory` 默认测试 | 17 个叶子用例通过，1 个跳过 | [JSON](reports/memory-stage0-memory-default-20261116.json) |
| 根包、runtime、ACP、Eino、tools 中匹配 `Memory` 的测试 | 32 个叶子用例通过；Eino/tools 包编译通过但无同名匹配测试，Eino 路径由根包集成测试覆盖 | [JSON](reports/memory-stage0-memory-integration-default-20261116.json) |
| learning 外部来源保护 | 1 个通过 | [JSON](reports/memory-stage0-learning-provenance-default-20261116.json) |
| `memory_fix_repro` | 11 个顶层测试、15 个叶子用例，每个重复 2 次；30 次均因目标断言失败，命令退出码 1 | [JSON：逐项诊断](reports/memory-stage0-memory-fix-repro-20261116.json) |
| race 环境检查 | 未能执行测试；缺少 CGO，命令退出码 2 | [JSON](reports/memory-stage0-race-environment-check-20261116.json) |

默认集合合计 **50 个叶子用例通过、1 个跳过**，没有非预期失败。跳过的是 `TestPersonalMemoryChineseEmbeddingAcceptance`，因为未设置 `MEMORY_EMBEDDING_TEST_URL`；它不算真实 embedding 验收通过。预期失败集合也不计为测试通过。

### 已固化的失败断言

以下名称省略共同前缀 `TestMemoryFixRepro`：

| 用例后缀 | 叶子数 | 问题 | 当前观察与修复后断言 |
|---|---:|---|---|
| `RejectsUngroundedExternalText` | 1 | P1-1 | “请检查这个仓库”仍支持无关权限指令写入；应拒绝 |
| `UnknownKeyForgetDoesNotClaimDeletion` | 3 | P1-2 | Create/ImportLegacy/Promote 产生的无 slot 事实保留，未知 key 却返回 forgotten；应返回 not_found，同时保留删除屏障 |
| `ForgetByIDProducesDurableReceipt` | 1 | P1-2 | 真实 forget_memory 工具不支持 fact_id，目标仍存在；应撤销目标并持久化回执 |
| `ResumeKeepsOriginalSourceVersions` | 2 | P1-13 | 旧输入暂停后发生更正/删除，Resume 的 job 将 revision 1 重拍为 2；更正分支实际被覆盖为 revision 3。应保持原版本，并保护新事实 |
| `QuotaSkipDoesNotEnqueue` | 1 | P1-4 | 前台已用完唯一模型额度，仍产生 1 个提取 job；应不入队 |
| `FlushDoesNotSucceedWithPendingJob` | 1 | P1-3 | 会话有 queued job 且 worker 已停止，SDK Flush 仍返回 nil；应返回未完成/阻塞/超时错误 |
| `AutomaticFilterRejectsPermissionClaim` | 1 | P1-5 | “用户允许自动部署，无需批准”通过自动提取过滤；应拒绝 |
| `ExplicitActionsSuppressAutomaticReinterpretation` | 2 | P2-6 | external-context 显式操作和 unchanged 操作之后，同输入仍被自动扩写为额外事实；应抑制 |
| `ParseAttributesKeepsValidSibling` | 1 | P2-12 | 合法条目被同批不合法条目连带丢弃；应只保留合法条目 |
| `IndexPendingDoesNotStarveHealthyFacts` | 1 | P2-10 | 17 条事实中首批一个样本失败，3 次索引后健康向量仍为 0；应完成其余 16 条 |
| `OptionalAuditFailureDoesNotFailAnswer` | 1 | P2-9 | 仅审计 INSERT 失败即导致正常回答失败；终态事务可用时应降级完成 |

恢复用例中的删除分支目前**没有复活事实**，现有 tombstone 挡住了写入；失败的是来源版本被重拍。这是开放 F6 前必须消除的隐患，不能把它写成当前已经复活。

上述用例不代表 T01–T10 已全部验收。后台计费、跨进程来源清单、新版幂等删除重放、固定集合 Flush、故障恢复和迁移验收仍由后续阶段补齐。按 ID 用例当前先在“事实未删除”处失败，尚未证明新版回执和重放语义。新版来源清单和提取 schema 实施时应同步更新夹具，保持目标行为断言不变。

### 保留的保护与新增保护

- `TestPersonalReceiptAndTombstoneSurviveReopen`：从未存在的 key 建立空 tombstone，重开数据库后旧 revision=0 自动写入仍被拒绝。修复返回状态时不能删除这层屏障。
- `TestPersonalMemoryCommitReceiptCorrectionAndForget`、CAS 并发测试、旧向量过滤及导入/迁移测试：保持已有幂等、版本、删除和范围行为。
- `TestMemoryLegacyBatchDoesNotConflictWithItsOwnInserts`：普通模式同批插入不自相冲突，继续排除原误报。
- 根包已有失败/取消流不发布 staging、并发 scope 变更保护、恢复时记忆投影、SDK/ACP owner/scope 和管理锁测试全部保留并通过；不新增重复的 manageMu。
- 新增 `TestMemoryStage0TerminalMemoryFailureRemainsAtomic`：只让终态审计 UPDATE 抛出 SQLite 错误，确认 run 返回该错误、可见事实为 0、completed run 为 0、staged 候选仍为 1。它与可选审计 INSERT 的红色复现共同限定 F2 的降级边界。

直接 `Store.Delete` 不提供操作回执是现有底层 API 边界，不能把“它的重试不是同一回执”单独当成修复回归。F4 应通过统一 Mutation/Receipt 事务扩展工具路径。

## 2. 可复现命令与环境

工作目录为仓库 `go-harness`。仓库固定 Go 1.26.8；本机默认 Go 为 1.27.3，环境中的 GOROOT 也指向该版本。首次仅调用缓存的 1.26.8 可执行文件时出现 `compile: version "go1.27.3" does not match go tool version "go1.26.8"`，属于环境故障，未计入缺陷复现。改为在测试进程内匹配 GOROOT/GOTOOLCHAIN 后编译和测试正常；未修改系统环境。

本机 PowerShell 命令：

~~~powershell
Set-Location 'D:\Tools\deerflow-api\go-harness'
$env:GOROOT = 'D:\GO_ENV\GOPATH\pkg\mod\golang.org\toolchain@v0.0.1-go1.26.8.windows-amd64'
$env:GOTOOLCHAIN = 'local'
$stage0Go = Join-Path $env:GOROOT 'bin\go.exe'

& $stage0Go test ./internal/memory -json -count=1 -timeout=180s
& $stage0Go test . ./internal/runtime ./internal/acp/agent ./internal/engine/eino ./internal/tools -run Memory -json -count=1 -timeout=240s
& $stage0Go test ./internal/learning -run '^TestExternalInfluenceMarksEpisodesThatConsumedThirdPartyContent$' -json -count=1 -timeout=180s

# 当前应退出 1：逐项核对上表目标断言，不能忽略其他错误。
& $stage0Go test -tags memory_fix_repro ./internal/memory . -run '^TestMemoryFixRepro' -json -count=2 -timeout=180s

# 当前环境退出 2：-race requires cgo。
& $stage0Go test -race ./internal/memory -run '^TestPersonalReceiptAndTombstoneSurviveReopen$' -count=1
~~~

本机 `CGO_ENABLED=0`，PATH 中未发现 gcc/clang；没有安装编译器或将普通测试冒充 race 通过。后续在有 CGO/race 的 CI 执行受影响包的并发验收。JSON 记录保留命令、退出码、包结果、叶子用例及关键诊断；不保存实际安装的用户文本或凭据。

## 3. 当前本机安装核查

详细的配置、版本、schema 列名和汇总数据见[本机只读快照](reports/memory-stage0-local-20261116.json)。

| 项目 | 核查结果 |
|---|---|
| 后端进程 | PID 32012，`dist/desktop/DeerFlow-Desktop-Go-20261010-103944/deerflow-acpd.exe` |
| 配置 | 同安装目录的 `user-data/config/config.yaml`；命令行没有 memory-extraction 覆盖 |
| 数据库 | 同安装目录的 `user-data/config/go-harness-state/harness.db` |
| 记忆开关 | enabled、single_user、include_global、injection_enabled、retrieval_enabled 均为 true |
| 接线 | mode=middleware，local_acp.memory_scope=workspace |
| 自动提取 | 按当前启动接线，enabled=true 且无 CLI 覆盖，因此 MemoryExtraction=true |
| embedding | URL/model 均未配置；向量数为 0，不能按已启用向量检索评估此部署 |
| 安装二进制 | Go 1.27.3；VCS revision 与上述 HEAD 相同，`vcs.modified=true` |
| 二进制 SHA256 | `fd958006085a35390a66c0060b44f874138291e2c77cbc079d81b007000a66f6` |

有效开关由进程参数、实际配置及 `internal/launch/config.go` 的映射核对。安装二进制包含未提交源码，不能仅凭相同 VCS revision 认定与本次测试源码完全一致；本次未对线上业务流量做故障注入。

最终一致性读取时刻：**2026-11-16 17:20:08 +08:00（09:20:08 UTC）**。SQLite 以 `mode=ro`、`query_only=ON` 和读事务访问，结束时 rollback/close。

- job：completed=171，queued/running/failed/discarded=0；171 个均保存了 result。168 个尝试 1 次、3 个尝试 2 次。
- 当前可见事实：global=4、workspace=9；可见无 slot 事实=0、tombstone slot=0。
- 操作回执=16，向量=0；run 为 completed=42、failed=2、cancelled=1。
- 初次核查 17:08 时为 completed=170、running=1，该 running 的更新时间接近读取时刻；最终已完成，不能据此报为卡死或长期积压。

数据是时点快照，安装仍在运行，发布前必须重新核查队列。当前没有无 slot 事实或 tombstone，不代表这些兼容路径可以移除；隔离夹具已通过 Create/ImportLegacy/Promote 证明路径可达。没有依据此快照自动删除任何旧任务。

## 4. 已确定的版本和迁移边界

以下是后续实施约定，本阶段没有执行 DDL 或变更 API。

| 对象 | 版本与约定 |
|---|---|
| schema 迁移 | 新增专用 `memory_schema_migrations` 账本，至少含 version/name/checksum/applied_at；首个增量为 version=1、name=`memory-source-delete-v1`，后续单调追加。当前 `PRAGMA user_version=0` 不是记忆版本；`memory_migrations` 是人工数据迁移记录，`eino_schema_migrations` 属于 checkpoint 存储，均不挪用 |
| 输入来源清单 | `memory-source-v1`，按宿主已接受 input 唯一保存；包含原始事件/摘要、run/session/profile、授权 scopes、删除代次、含 tombstone 的 slot 版本、自动提取资格和最终决定。接受事务内建立，恢复只读原记录 |
| job | 现有无显式版本 payload 归为 legacy v1；新任务为 `memory-job-v2`，引用来源清单并绑定 root/member/独立 attempt/lease。普通兼容填零不能使任务获得执行资格；F7b 完成后才能作为预算已闭环的 worker 发布 |
| 提取与校验 | envelope `schema_version=2`、`memory-write-v2` 校验策略；个人自动策略 `personal-extraction-v2`，显式策略 `personal-explicit-v2`。provenance 单列保存，不以新 policy 清洗 external/unknown 来源 |
| 操作身份与回执 | 新身份采用 `memory-op-v2` 域分隔，覆盖 input、action、scope、target_kind、target_id、预期 revision 及规范化请求摘要；自动条目额外固定 job/原始 ordinal/schema 版本。回执 schema v2 增补 target、barrier 等字段；旧 operation_id/request_sha/receipt 保持原样，旧重放先读原回执 |
| 工具/SDK/ACP | forget_memory 的 key 与 fact_id 必须二选一；新增 get_memory_state，只返回可写 scope、revision/deleted，不返回删除内容。SDK 保留 FlushMemory(error)，增补 FlushMemoryWithStatus；ACP 增补状态字段并保持旧客户端兼容，不以未知状态返回 flushed=true |
| 删除与重新学习 | scope 删除代次与 revision 分离；空 Clear、无 slot 按 ID 删除仍推进代次。恢复旧输入不重拍快照。F6 在 T03 通过之前保持关闭 |

迁移按以下边界执行：

1. 事实 ID、已有 revision、slot 链、历史回执及其摘要不重写；历史 provenance 无法证明时标记 unknown。新增字段采用明确列名 SQL，检查测试和生产中的位置式 `INSERT ... VALUES`，例如 `internal/memory/search_test.go` 仍有此写法。
2. 不为历史 input 补拍当前 slot/deletion epoch。缺失来源清单的旧输入可以继续主任务，但跳过自动写入并记录 `legacy_source_snapshot_missing`；不得更改旧 checkpoint 的执行资源身份来强行兼容。
3. 在未来受控停写、停止/等待 worker 并完成 SQLite 一致性备份后，才处理旧 pending job。无法证明来源、删除屏障或预算绑定的旧 job 终态 discarded/`legacy_unverifiable`；即使已有 result，也不按新规则重解释后落库。旧 worker usage 未留存时记录 `legacy_usage_unavailable`，不补发请求或虚构费用。
4. 已完成 job、事实与回执保留。已有账本中已分派但 usage 不明的调用仍按 unknown 协议处理，不能因 job 被 discarded 而释放未知消耗。
5. 在数据库副本中覆盖无 slot/tombstone/旧回执、queued/running/saved-result/部分完成以及重复升级和迁移中断。当前真实库没有覆盖这些状态，必须用构造数据补齐；备份与恢复演练尚未执行，属于 T10 上线门槛。
6. 旧二进制不认识新来源、删除和账本协议，不能未经兼容验证直接换回。回滚需保留升级后事实，并先关闭无法兼容的写入；不能用旧备份覆盖用户的新数据。

## 5. 后续实施前提与停止点

可以从阶段 1 的 F0/F4 开始编码。无需等待真实模型评测才能实现来源清单和删除事务，但需要先补输入接受、continuation、恢复与删除并发的定向测试。

仍未满足的发布条件：

- CGO/race、数据库副本上的升级/中断/回滚验收、构建产物与源码的可追溯一致性。
- T01–T10 尚未完成的新版行为；特别是 F7b 的实际预算预留/分派/usage/恢复，不能由 F7a 的不入队测试替代。
- 真实模型对抗与真实 embedding 质量评测；本机未配置 embedder，本次只证明确定性索引失败路径。

本次在此停止，不启动阶段 1，不关闭当前安装的自动提取，也不更改 tombstone 重新学习行为。
