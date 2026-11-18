# 记忆修复阶段 1 实施报告

日期：2026-11-16

结论：阶段 0 报告支持原计划的 F0 → F4 → 后续校验/预算/Flush 顺序，无需纠正阶段 1 的实施方向。现已完成阶段 1 的代码与定向验收；阶段 2–5 未实施。没有部署、重启当前本机安装或修改其配置、数据库。

## 1. 对阶段 0 证据的判断

- 本机确实启用了 single_user 与自动提取，因此来源快照和删除屏障仍应优先实施。
- 最终队列快照没有积压，只能排除当时的积压现象，不能否定已固化的队列、预算和 Flush 缺陷。
- 真实库当时没有无 slot 事实和 tombstone，隔离测试已证明 Create、ImportLegacy、Promote 可以产生这些状态，必须保留兼容处理。
- 删除后的旧输入尚未复活事实，是原 tombstone 的保护效果；阶段 1 固定输入来源，并继续禁止自动恢复 tombstone，没有提前开放 F6。
- 保留既定的版本、迁移及旧 checkpoint 兼容边界。计划补充了分阶段版本含义和确定拒绝的工具回执语义，未调整阶段依赖。

## 2. 实现内容

### 来源快照与恢复

新增专用 schema 账本，首个迁移为 version=1、name=memory-source-delete-v1，校验迁移 checksum 和未知新版本。新增来源清单、scope 删除代次、操作来源及旧 job 处置表，未改写历史事实、revision、slot、回执或旧 request_sha。入口见[来源协议](../go-harness/internal/memory/source.go)和[接受事务接线](../go-harness/memory_source.go)。

输入快照在真实 BeginRun 接受事务中，与 input、用户事件、run 和预算根一起提交。清单固定 run/input/session/profile、root、授权 scope、scope revision、删除代次、含 tombstone 的 slot revision 及提取资格。正文不复制到清单或新 job，读取时根据原始 input/event 与摘要验证；决定和 job 关联独立存储，避免重试改写来源。

同一输入暂停、恢复及重开客户端后继续读取原快照。普通提取也使用接受时的 scope revision。子任务、后台 continuation 和前台通知 continuation 不调用人工输入捕获入口；新的真实用户输入才建立新快照。

新 job 以 version=2 引用来源，落库时重新读取数据库中的 job 和清单，不信任调用者传来的 text/profile/versions。该版本尚不包含阶段 3 的专用预算 member、独立 worker attempt 和 lease；提取内容仍使用旧格式，阶段 2 再升级校验。禁止仅凭 job.version=2 宣称预算已闭环。

### 删除、回执与显式动作

核心实现见[原子变更与回执](../go-harness/internal/memory/personal.go)、[队列来源与显式动作检查](../go-harness/internal/memory/jobs.go)和[模型工具接线](../go-harness/personal_memory.go)。

- forget_memory 支持 key 或 fact_id 二选一；按 ID 可以删除无 slot 事实，同时保留其他 scope 和 Promote 的原始副本。
- 事实撤销、版本、向量移除、删除屏障、操作回执和 run/input 显式动作记录在同一事务中提交。Store.Delete、Clear 与带回执的删除复用删除逻辑，现有 SDK/ACP owner/scope 检查保留。
- 本次真实删除返回 forgotten；已删除返回 already_forgotten；未知目标返回 not_found。未知 key 仍创建空 tombstone；相同操作重放返回原回执，不重复增加 revision。
- 按 ID 删除有 slot 的事实保留 slot 链；无 slot 删除推进 scope 删除代次。Clear 即使没有事实也推进删除代次及普通 scope revision，阻止旧输入创建当时尚不存在的 key。
- 新工具使用 memory-op-v2 域区分 input/action/scope/目标类型/目标 ID/预期 revision/请求内容，避免同版本的两个 fact_id 碰撞；旧操作摘要和回执编码保持兼容。
- get_memory_state 和 CAS 冲突只返回 scope/key/revision/deleted/exists，不返回删除正文。读取新 revision 不会给旧输入增加恢复权限。
- unchanged、not_found、already_forgotten 及两种显式写入策略都记录显式动作；入队、认领、ApplyJob 均检查它，ApplyJob 的检查与落库共用事务。
- 验收时补获并修复了“来源已过期的确定拒绝被记为 uncertain”的边界。参数/授权/来源/CAS 拒绝标记为未执行或结构化冲突；存储及提交错误不做这种转换。

### 旧数据和执行兼容

旧 checkpoint 沿用原工具 schema 和执行资源状态，不将新字段强写入旧资源指纹。缺失新版来源的显式工具拒绝新写入；自动提取记录 legacy_source_snapshot_missing，主任务可继续。

恢复 worker 时，无法验证的旧 queued/running job（包括已保存 result 的任务）标记 discarded/legacy_unverifiable，并记录 legacy_usage_unavailable；保留 completed job、原 result 和历史回执，不补拍今天的快照、不补请求、不修改预算 usage。代码只在隔离数据库中验证，真实库升级仍要求受控停写和一致性备份。

已复用当前工作区的记忆投影实现：删除/更正影响模型看到的记忆块，不修改 checkpoint 的固定资源状态，也不清除历史用户/工具消息。该实现及其原有测试在本任务中保持原样。

## 3. 验证结果

固定使用 Go 1.26.8，并在测试进程内同时设置 GOROOT 和 GOTOOLCHAIN。新增测试全部使用临时 SQLite 数据库和进程内假 provider，没有调用实际安装的模型服务。

| 集合 | 结果 |
|---|---|
| 最终根包、memory、runtime 的 Memory/PersonalReceipt 回归 | **71 个叶子用例通过，1 个跳过**；包含全部 **37 个阶段 1 专项** |
| runtime 输入接受、执行恢复、continuation、后台任务及预算相关回归 | **281 个叶子用例通过** |
| 首次扩展整包回归 | memory、ACP agent、Eino、tools、learning、harness 六个包通过；根包及 runtime 达到 240 秒整包时限，未计为通过 |
| 根包延长至 600 秒的整包复测 | **364 个叶子用例通过、3 个跳过**，用时 514.6 秒，退出码 0 |
| 剩余 memory_fix_repro | **7 个用例仍因既定目标断言失败**，退出码 1；不计为通过 |
| race / 真实 embedding | race 所需 CGO/编译器环境未就绪；真实 embedding 未配置，跳过测试不计为验收通过 |

完整命令、包结果、跳过/超时及目标诊断见[阶段 1 测试记录](reports/memory-stage1-tests-20261116.json)。不同集合有重叠，不累加为独立用例总数。

根包整包复测开始于最终工具拒绝分类修正之前；该修正及新增断言完成后，又运行了上表的最终记忆回归。runtime 全包初次在技能容量用例处达到整包时限，未宣称全包通过；本阶段相关的 281 个用例另行全部通过。git diff --check 通过。

阶段 0 的 4 个顶层、8 个叶子复现已移入默认测试：未知 key 遗忘回执（3）、按 ID 删除（1）、旧输入恢复版本（2）、显式动作优先（2）。恢复测试增加了重开客户端变体。默认专项还覆盖：

- 接受事务失败时 input/run/event/来源/预算全部回滚；两个会话跨空 Clear 接受输入并固定不同删除代次。
- 更正、删除、无 slot 删除、Clear、显式恢复、伪造事件和篡改输入摘要；按 ID 删除后的 Get/List/search、注入及 slot 链保护。
- 回执响应丢失后的重放、变更目标/请求/scope 冲突、两个 ID 连续删除、回执及显式动作记录写入故障。
- no-op 显式动作在认领前及认领后的落库边界阻止自动解释。
- 无 job 的 disabled/no_facts/quota_skip/promoted 决定可查询；quota_skip 此处验证普通同步模式，个人后台模式的配额问题仍待 F7a。
- 旧来源缺失时真实 Run/Resume 可继续、构造旧协议的工具 schema/资源状态保持一致；迁移重复执行、失败回滚、旧回执保留、旧 job 处置及未知 schema 拒绝。

旧 checkpoint 兼容测试包含构造的旧协议状态，不等于已经完成真实安装数据库副本上的完整升级/回滚演练。

专项测试位于[存储/迁移](../go-harness/internal/memory/source_test.go)、[阶段 0 复现转默认回归](../go-harness/internal/memory/stage1_test.go)、[SDK/真实恢复路径](../go-harness/memory_stage1_test.go)和[人工来源与 continuation 隔离](../go-harness/internal/runtime/memory_source_test.go)。

剩余失败准确对应 P1-1、P1-3、P1-4、P1-5、P2-9、P2-10、P2-12，即内容支持关系、Flush、个人后台配额、权限指令过滤、可选审计降级、索引批次阻塞和逐条过滤；没有将这些缺陷误标为阶段 1 已修复。

## 4. 复现命令

在 go-harness 目录运行：

~~~powershell
$env:GOROOT = 'D:\GO_ENV\GOPATH\pkg\mod\golang.org\toolchain@v0.0.1-go1.26.8.windows-amd64'
$env:GOTOOLCHAIN = 'local'
$stage1Go = Join-Path $env:GOROOT 'bin\go.exe'

& $stage1Go test ./internal/memory . ./internal/runtime -run 'Memory|PersonalReceipt' -json -count=1 -timeout=180s
& $stage1Go test ./internal/runtime -run 'Memory|Execution|Continuation|Background|RunQueue|Budget' -json -count=1 -timeout=240s
& $stage1Go test . -json -count=1 -timeout=600s

# 阶段 2–4 尚未修复的 7 项目标断言，当前应退出 1。
& $stage1Go test -tags memory_fix_repro ./internal/memory . -run '^TestMemoryFixRepro' -json -count=1 -timeout=180s
~~~

## 5. 范围和后续门槛

已更新[修复计划](memory-fix-plan.md)的实施状态和版本边界。本次改动集中于来源捕获、记忆存储/删除、工具接线与测试；没有提交 commit 或执行部署。核对阶段 0 记录的已有代码文件，除本阶段需要修改的 extensions.go 外，其余 17 个已有代码/测试文件的 SHA256 均保持一致。

阶段 1 完成不等于完整修复已可发布：阶段 2 的内容校验和入队/错误边界、阶段 3 的实际预算协议、阶段 4 的 Flush/索引、阶段 5 的重新学习仍待实施。F6 保持关闭；race、真实模型/embedding 评测、安装数据库副本的完整 T10、备份恢复和二进制可追溯构建继续作为发布门槛。
