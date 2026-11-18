# 记忆修复阶段 3 实施报告

日期：2026-11-16

结论：阶段 2 的证据支持继续实施 F7b，无需改变后续阶段方向或依赖顺序。阶段 3 的预算执行与恢复协议已完成，定向验收与相关回归通过。本次没有部署、重启本机安装、修改其配置或数据库，也没有创建 commit。

## 1. 对阶段 2 报告的复核

阶段 2 的写入验证、原始响应与 ordinal 持久化、批次原子事务、quota_skip 和错误边界，提供了阶段 3 所需前提。每日认领上限仍不能代替来源 root 的预算，剩余两个 memory_fix_repro 也不能作为预算已完成的依据。

实施前固化了两个阶段 3 复现：认领后台任务没有独立预算 attempt；入队后 root 额度耗尽仍能认领新的调用。两者在实施前均因目标断言失败，现在均通过。

本阶段补充了执行细节：记忆恢复必须先于通用预算中断恢复；只有结束标志、最终 usage 与 EOF 都完整才能按实际用量结算；已计费结果的落库同样受 root 停止状态约束。原计划已经要求预算、恢复与取消保护，因此这些属于落实细节，不改变阶段 4、5 的目标或顺序。

## 2. 实现内容

### 身份、准入与租约

新增 schema version=3 / memory-job-budget-v1，保留前两次迁移的 SQL 与 checksum。新 job 持久化 budget_policy 和专用 member_id，宿主从已接受输入确定 run/input/session/root；入队时验证来源 attempt，并将 job、member 绑定、租约记录及输入决定同事务提交。

worker 使用独立 memory_job member、worker attempt/fence、操作 ID 和 30 秒 job 租约。预算预留和分派的事务内检查当前租约、真实完成的来源运行、会话、输入绑定、显式动作、删除代次与 root 停止状态。旧 fence、其他 root/session、未登记 member、取消任务以及没有预算账本的路径不能发起新效果。认领后没有分派的中断也会关闭 attempt；queued/退避不保留活动调用 attempt。

核心入口见[任务预算协议](../go-harness/internal/memory/job_budget.go)、[预算恢复事务](../go-harness/internal/budget/memory_job.go)和[worker](../go-harness/memory_worker.go)。

### 每次调用的预算与 usage

新 worker 只有取得 ReserveModel 和 MarkDispatched 的首次许可后才能请求模型。请求摘要绑定模型身份与消息；从实际消息序列化形式估算输入，输出上限为 1024，再受原 root 的单次输出及剩余 token 限制。并发的前台/其他成员与记忆 worker 共享 root 的调用数、held 和 spent，不能各自使用最后一份额度。

provider 的内部重试关闭，重试统一由持久任务调度。流式读取继续到 usage 尾部和 EOF，不能在获得正文后提前返回；早期 usage 不能冒充最终 usage。响应超限仍尽量读取使用量，内存中正文保持上限。格式错误在消耗已知时结算后有界重试，每次重试重新预留；HTTP 故障、缺失最终 usage、断流、超时和取消无法证明消耗时进入 blocked/budget_unknown，保留 hold，不自动重发或按零费用结算。

永久预算不足记录 quota_skip；临时占用记录 budget_held 和 available_at。明确没有获得预留的临时占用不耗尽三次提取机会，但仍保守消耗当日认领额度。每日上限继续跨重启持久化，限流记录 daily_limit 与下一 UTC 日期；已保存结果不占新请求额度。

模型与 usage 适配见[提取器](../go-harness/internal/engine/eino/memory_extractor.go)。旧的仅凭原始文本 SaveJobResult、仅凭 ID FinishJob 路径拒绝产生新效果，不保留无计费 fallback。

### 恢复、结算与发布

响应正文、usage/完整性、错误码与操作身份先持久化。随后在同一事务中结算预算、关闭 attempt，并保存合法提取结果或安排已知失败的重试。事实与回执继续由既有 ApplyJob 批次事务原子发布，completed 状态不能先于事实提交。

| 中断位置 | 恢复行为 |
|---|---|
| 已认领、尚未预留 | 关闭旧 attempt，任务可再次认领 |
| 已预留、确定未分派 | 释放 token hold；保守保留已占用的调用/每日认领计数 |
| 已分派、没有可靠响应 | 标记 unknown，保留 hold，阻止自动重发 |
| 响应与完整 usage 已保存、尚未结算 | 按原身份结算并结束 attempt，恢复原始结果 |
| 已结算、事实尚未提交 | 取得新的 job 租约后仅重放落库，不新建调用预留 |
| 事实、回执及 completed 已提交 | 重放保持幂等，不增加事实或重复扣费 |

宿主持有数据库进程锁后，先恢复这些调用，再运行通用预算恢复。只消解当前记忆 attempt 的已知状态，不能清除同 root 其他成员的 unknown。结果保存、结算或事实发布故障各自保持事务边界；无法持久化已返回的 usage 时保守保留 unknown。

Close 取消并等待 worker；租约失效的旧 worker 不能更新响应、续租或发布事实。后台失败不改写已提交的前台 completed。已付费结果也会在 root 停止后被拒绝发布，费用仍保留。

### 旧任务与后续阶段

缺少 budget_policy 的旧 queued/running job，包括阶段 2 已有 result 的任务，均按 legacy_unverifiable 处置，保留 legacy_usage_unavailable；不补预算身份、不推测旧 usage、不重新调用模型。已有事实、回执和 completed job 保留。

[修复计划](memory-fix-plan.md)补充了阶段 4 的状态映射：有 job 的 completed/quota_skip 也应计为 skipped；blocked/budget_unknown 不能当作已 Flush；daily_limit、budget_held 仍属于固定集合内待处理项。Flush 不能通过额外请求或释放未知消耗来取得成功状态。自动恢复 tombstone 继续保持关闭，留待阶段 5。

## 3. 验证结果

基线提交为 68753f15e51ef285260bca52388c44c786ff0f41，测试针对未提交工作区。固定使用 Go 1.26.8 windows/amd64、GOROOT 和 GOTOOLCHAIN=local。新增测试使用临时 SQLite、进程内流式假 provider、同步通道和 SQL 故障注入，没有访问实际安装的模型服务。

| 集合 | 结果 |
|---|---|
| 阶段 3 专项 | **49 个叶子用例全部通过**，包含响应持久化失败、无账本拒绝及已计费结果的 root 停止保护 |
| 最终记忆、预算、任务树及 Eino 相关回归 | **319 个叶子用例通过、1 个真实 embedding 测试跳过** |
| budget、memory、Eino、learning、tools、launch、ACP agent 整包回归 | **1393 个通过、7 个跳过** |
| runtime 记忆、执行、continuation、后台任务、预算及技能自动发布相关回归 | **302 个叶子用例通过** |
| 根包整包回归 | **423 个叶子用例通过、3 个跳过**，516.8 秒，退出码 0 |
| 剩余 memory_fix_repro | **2 个预期目标断言失败**：Flush 提前成功、IndexPending 坏样本阻塞；属于阶段 4，退出码 1，未计为通过 |

完整命令、包结果、时间、专项名单、失败/跳过原因与原始日志摘要见[阶段 3 测试记录](reports/memory-stage3-tests-20261116.json)。各集合有重叠，不累加为独立测试总数。

受影响包整包及 runtime 回归启动后，又完善了最终 usage 判定、临时预算占用退避和已付费结果的 root 停止保护；根包整包测试开始时，前两项已完成，最后的 root 停止保护尚未加入。这些分支随后由最终相关回归及专项重新验证。最后新增的响应持久化故障用例和无账本断言包含在最终专项中。旧阶段 1/2 测试夹具已改为经过实际预算身份、预留、分派和结算后再验证原来的删除、CAS、重放与原子性断言；未通过放宽业务断言绕过新门槛。git diff --check 与本阶段 Go 格式检查通过；两个已有且未改动文件的格式差异保留原样。

专项还验证：model_calls/token 最后额度竞争、原 root usage 归属、请求输出上限、各中断窗口、整客户端重开时的恢复顺序、其他成员 unknown 保留、旧 lease 与伪造身份拒绝、临时 hold 不使任务永久滞留、取消前后释放/保留额度、迁移失败回滚，以及 Close 等待实际 provider 取消。

race 未执行：本机 CGO/编译器前提仍不具备。真实模型质量/对抗评测、真实 embedding、安装数据库副本的完整升级/回滚与发布构建未执行；跳过项不计为通过。没有宣称 runtime 全包或全仓库全部通过。

## 4. 复现命令

在 go-harness 目录运行：

~~~powershell
$env:GOROOT = 'D:\GO_ENV\GOPATH\pkg\mod\golang.org\toolchain@v0.0.1-go1.26.8.windows-amd64'
$env:GOTOOLCHAIN = 'local'
$stage3Go = Join-Path $env:GOROOT 'bin\go.exe'

& $stage3Go test . ./internal/memory ./internal/engine/eino -run '^TestMemoryStage3' -timeout=180s -json -count=1
& $stage3Go test . ./internal/memory ./internal/budget ./internal/engine/eino -run 'Memory|Personal|TaskTree|Budget|Reservation|Attempt' -timeout=240s -json -count=1
& $stage3Go test ./internal/runtime -run 'Memory|Execution|Continuation|Background|RunQueue|Budget|SkillLearningAutoPatch' -timeout=360s -json -count=1
& $stage3Go test . -timeout=900s -json -count=1

# 阶段 4 的两个既知缺陷，当前应退出 1。
& $stage3Go test -tags memory_fix_repro . ./internal/memory -run '^TestMemoryFixRepro' -timeout=180s -json -count=1
~~~

## 5. 工作区与发布边界

开始前记录的 85 个已有修改/未跟踪文件中，71 个 SHA256 保持一致，14 个在本阶段按需更新。阶段 0–2 报告和测试记录、已有独立引擎/传输/桌面改动等保持原样。这是已记录文件的对比，不代表整个 git diff 都由本阶段产生。

阶段 3 完成不等于完整修复可部署。阶段 4 的异步 Flush 与索引、阶段 5 的安全重新学习、race、真实安装迁移及发布构建仍须分别完成。本次按要求止于工作区实现、测试和文档。
