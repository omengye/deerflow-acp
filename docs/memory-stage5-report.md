# 记忆修复阶段 5 实施报告

日期：2026-11-16

结论：阶段 4 的来源、预算、固定集合 Flush 与 revision/model 索引协议支持原有 F6/F10 方向，无需调整阶段顺序。阶段 5 已实现安全重新学习、完整文档和数据库副本验收。当前完成范围是源码与确定性验收；race、真实服务评测及完整发布包验收尚未完成。没有部署、重启本机安装、修改其数据库/配置、调用付费模型或创建 commit。

## 1. 计划复核与实施边界

阶段 4 已提供新 head 的索引隔离及持久任务状态，阶段 3 已提供独立预算与结果重放，因此无需重做后续设计。实施前先增加 6 个复现叶子用例，均以目标断言失败：新结构化值无法恢复，以及同值的五种变化缺少 requires_explicit 处置。实现后通过，随后扩展完整验收。

补充的执行细节不改变既定方向：旧正文不能被当作可信结构化值回填；旧输入没有新恢复策略时不能补授权；模型显式工具恢复还必须验证真实记住/更正请求。源码测试通过与实际安装发布分开报告。相关规则、执行顺序和验收要求已同步到[修复计划](memory-fix-plan.md)。

## 2. 实现结果

schema v5 / memory-relearn-v1 新增 memory_value_identities，将宿主字段验证所得的值身份和正文摘要绑定 fact_id/revision；前四次迁移 SQL/checksum、提取 schema=2、write policy v2、预算协议及操作 ID 域保持不变。新输入清单固定 relearn_policy，旧输入、旧 checkpoint 和已付费旧结果不因升级获得自动恢复权限。

自动恢复要求删除后新接受的人工来源、固定删除代次和 slot CAS 有效、完整声明可验证、旧值身份可信且规范化值确实不同。姓名/地点/硬件/数据库采用 Unicode NFKC、case-fold、去空白/标点；语言与数据库另有有限别名映射。路径和通用文本不自动恢复。规则是受限的字段字面判断，不是任意地名翻译或实体同义词识别。

比较对象为 tombstone 前最近一个 live revision。空 tombstone、历史无身份记录、未结构化的人工更正或正文摘要不匹配都保持删除；不能跳过未知新版本使用更早的 typed 值。校验器的私有证明绑定来源、字段、值、正文及自动/显式模式，防止替换公开参数伪造授权。

同值、未知旧值及不支持的属性返回 requires_explicit 和固定 reason；显式工具的普通陈述也返回 explicit_request_required。删除后新的明确请求或直接授权管理操作可以恢复同值，历史输入读取较新 revision 仍不能越过删除屏障。成功自动恢复沿用 fact ID、增加 revision，回执 updated/relearned_new_value。

值身份、事实、回执、提取条目和完成状态在同一事务提交。故障注入后可重放原付费结果，不重复结算或再请求模型。requires_explicit 是终态，Flush 计 skipped；混合批次其他有效 key 仍可发布。新 revision 不受旧索引 terminal 阻挡，迟到旧向量不能写入，budget_unknown 和 hold 保持有效。

实现入口：[恢复规则](../go-harness/internal/memory/relearn.go)、[原子写入](../go-harness/internal/memory/personal.go)、[逐项结果](../go-harness/internal/memory/jobs.go)、[SDK 工具接线](../go-harness/personal_memory.go)。

## 3. 验证结果

基线提交为 68753f15e51ef285260bca52388c44c786ff0f41，测试针对未提交工作区，固定 Go 1.26.8 windows/amd64、GOROOT 与 GOTOOLCHAIN=local。模型/embedding 验收使用进程内假服务或自定义假 Engine，迁移使用临时 SQLite 和本机数据库的二次副本。

| 集合 | 结果 |
|---|---|
| 实施前阶段 5 复现 | 6 个预期目标断言失败，退出码 1 |
| 最终阶段 5 专项 | **39 个叶子用例全部通过，无跳过**，包括本机副本和 schema4 探针 |
| memory、budget、Eino、launch、ACP agent、configschema、tools、learning、harness 整包 | **1535 个通过、7 个跳过** |
| runtime 记忆、执行、预算、后台、会话生命周期及技能兼容相关回归 | **310 个通过** |
| 根包整包 | **437 个通过、3 个跳过**，557.795 秒，退出码 0 |
| daemon 离线构建 | `go build -trimpath -p=2 ... ./cmd/deerflow-acpd-go` 成功；仅输出到临时目录 |

集合有重叠，不能累加成独立测试总数。完整命令、包结果、叶子清单、跳过原因和日志 SHA256 见[测试记录](reports/memory-stage5-tests-20261116.json)。整包回归覆盖最终生产代码；其后只增加了 SDK 打开安装副本的测试，该项已纳入最终 39 项专项，不把较早整包结果说成包含了后来新增的测试。

专项覆盖新值恢复、同值模板/大小写/全半角/标点/语言和数据库别名、显式请求与无关请求区分、旧/未知/空 tombstone、无策略旧已付费结果、引用/否定/遗忘/外部转述、证明替换、旧输入和两会话 CAS 竞争、混合批次、元数据事务故障恢复、unknown 预算、workspace 隔离、索引旧终态及迟到向量、SDK 真正工具调用和付费 worker/Flush、迁移故障/重入/旧回执及回退拒绝。

中间迭代发现新测试夹具遗漏 Action，以及 SDK 工具夹具未关闭交互审批、未实际执行变更；已修正夹具并增加持久工具回执断言，没有放宽恢复规则。首次构建命令使用了不存在的目录，改用仓库实际 `cmd/deerflow-acpd-go` 后通过。

## 4. 本机数据库副本与回退验收

源库来自阶段 0 核实的本机安装，使用 SQLite `mode=ro`、`query_only=ON` 与 backup API 取得一致性副本，源库没有打开写连接。快照时间为 2026-11-16 14:49:23 UTC；副本 SHA256 为 `8d8482f8fd26b8b6e260a854fe1c54596b625c680cef7a10ce39773911406eb8`，验收后未变。integrity_check=ok、外键违规数为 0。

副本原来没有 memory_schema_migrations，包含 4 个 scope、13 个 fact、16 个历史 revision、13 个 slot、16 个 operation 和 177 个 completed job。迁移至 v5 并再次打开，按迁移前存在的列逐表比较摘要；历史 ID、revision、operation ID、request_sha、原始回执及已完成 job 均不改写。没有为旧事实推断值身份。完整 SDK 使用假 Engine、关闭模型记忆访问，在独立临时副本上连续 Open/Close 两次，历史事实/回执/profile 摘要相同，模型调用为 0。

真实副本没有 queued/running 或未结算调用，因此另用构造库覆盖这些状态：旧 queued/running、已保存结果和部分已提交批次按 legacy_unverifiable/legacy_usage_unavailable 处置；已提交部分事实和回执保留，旧 completed 不改写。v5 journal 插入故障使 DDL 一并回滚，重试及再次升级成功。新协议的 unknown/已付费恢复由阶段 3 和本轮重放测试覆盖，不声称真实安装副本本身包含全部故障形态。

实施前构建的 schema4 存储探针能打开 v4，升级后以退出码 2 拒绝 v5；拒绝前后历史记录及升级后新增事实保持一致。本轮验证的是**旧读库器安全拒绝降级**，没有还原旧备份覆盖新数据，也没有运行实际安装旧二进制写新库。最初安装程序可能不认识 schema 账本，不能据此称它可安全回退。

副本、探针和构建产物位于本机临时目录；仓库报告只保存结构计数、摘要及验证结果，不包含真实用户记忆正文。上线操作顺序及兼容回退要求见[运行手册](memory-upgrade-runbook.md)。

## 5. 文档、环境与剩余门槛

已同步[写入协议](memory-write-policy.md)、[全局记忆](eino-global-memory.md)、[上下文设计](eino-memory-context-design.md)、[配置桥接](eino-go-config-bridge.md)、修复计划与新增运行手册。没有新增 YAML 开关；daily_attempts:0 仍只暂停新调用，不能当作冻结已付费结果发布的开关。

`go test -race ./internal/memory -run '^TestMemoryStage5' -count=1` 在执行测试前退出：`-race requires cgo`。本机 CGO_ENABLED=0，未发现 gcc/clang，race 未执行，仍需支持的 CI 环境补齐。相关包 7 个、根包 3 个跳过涉及真实服务、符号链接权限和命令 provider，测试记录逐项列出，不计为通过。

尚未执行真实模型质量/对抗评测、真实 embedding、完整桌面/Bridge/配置工具发布构建和目标安装冒烟；未声称 runtime 整包或全仓库全部通过。daemon 编译和 SDK 副本打开成功不替代完整交付验收。

开始时记录了 114 个既有修改/未跟踪文件的 SHA256（排除既有 .deerflow 运行目录），其中 99 个不变、15 个按本阶段需要更新、无丢失。前四阶段报告、测试记录及无关桌面/引擎修改保持不变；新增文件、最终源码摘要和基线对照见测试记录。修改文件的 Go 格式、git diff --check 与文档本地链接检查通过，既有两个无关 Go 文件的格式差异未改动。工作止于源码、测试、文档和临时副本，未部署或提交。

在 `go-harness` 目录复跑；本轮固定 `GOROOT=D:\GO_ENV\GOPATH\pkg\mod\golang.org\toolchain@v0.0.1-go1.26.8.windows-amd64`、`GOTOOLCHAIN=local`，副本/探针环境变量见运行手册：

~~~powershell
go test . ./internal/memory -run '^TestMemoryStage5' -timeout=180s -json -count=1
go test ./internal/memory ./internal/budget ./internal/engine/eino ./internal/launch ./internal/acp/agent ./internal/configschema ./internal/tools ./internal/learning ./harness -timeout=420s -json -count=1
go test ./internal/runtime -run 'Memory|Execution|Continuation|Background|RunQueue|Budget|Session.*(Delete|Archive|Retention|Cleanup)|SkillLearningAutoPatch' -timeout=420s -json -count=1
go test . -timeout=900s -json -count=1
~~~
