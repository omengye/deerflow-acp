# 记忆修复阶段 2 实施报告

日期：2026-11-16

结论：阶段 1 的报告支持既定的 F1 + F3 + F7a + F2 方向，后续阶段顺序无需纠正。阶段 2 的代码、协议文档与定向验收已完成；阶段 3–5 仍待实施。本次没有部署、重启本机安装、修改其配置或数据库，也没有创建 commit。

## 1. 计划复核与实施边界

阶段 1 已固定真实输入、授权 scope、删除代次和显式动作记录，阶段 2 继续以这些持久化来源为依据，不在 PostRun 或恢复时重新获取较新版本。补充了以下执行细节，未改变阶段依赖：

- 保留既有批次原子事务。批内第一条处理后发生数据库故障，整批事实、回执和条目状态回滚；已保存的原始响应仍可用于重放。无需为“部分恢复”拆开提交边界。
- 提取 schema、写入 policy 与预算执行协议分开版本化。新 job 的来源 version=2 不能证明阶段 3 的 member、attempt、lease、预留与 usage 已实现。
- F1 采用计划允许的兼容入口方案：记忆使用新的 UntrustedMemoryContentTool，将所有工具内容默认视为不可信；技能学习保留 ExternalContentTool 的既有分类。扩大回归发现直接修改共享分类会阻止依赖工具执行证据的技能自动发布，因此已拆分入口并复测相关发布与回滚路径。

阶段 3 继续完成后台预算闭环，阶段 4 处理 Flush 与索引，阶段 5 才开放受保护的重新学习。阶段 2 的每日上限属于附加限流，不是计费修复。

## 2. 实现内容

### F1：写入支持关系与来源

新增公共[写入校验器](../go-harness/internal/memory/write_validation.go)。模型显式写入及两条自动提取管线均验证已接受用户输入中的完整声明、Unicode 证据位置、原始字段值和 key/category；content 由宿主模板生成，模型提供的正文必须与模板一致。forget 单独验证用户要求的目标，不能借一个无关证据删除其他事实。

支持的中英文姓名、地点、语言/回答风格、硬件、数据库、显式项目路径及有限通用事实语法已写入[模型记忆写入协议](memory-write-policy.md)。引用、转述、否定、权限/执行指令、秘密、当前任务要求及无法确认的自由文本保守拒绝；自动提取保持路径/URL/markup 限制。这是受限语法的确定性校验，不宣称理解所有自然语言或彻底解决提示词注入。

provenance 与 policy 分列保存；本地文件、命令、历史、旧记忆、未知工具及失败回执均不能提供可信豁免。Promote、迁移、读取与重新注入保留来源级别；历史来源不明的事实保持 unknown。恢复只更新模型看到的记忆投影，不改写固定 checkpoint 资源状态。技能学习保持既有准入规则，本次不声称同步解决其全部本地内容风险。

### F3：逐条校验、原始序号与重放

新增[提取 schema v2](../go-harness/internal/memory/extraction_v2.go)，区分顶层格式错误和单条字段/内容错误。合法 envelope 中的坏条目单独记录拒绝原因与原始 ordinal；合法兄弟项继续处理，全拒绝终态 no_facts，不再次请求模型。同 scope/key 的冲突值全部拒绝，单条 superseded 不阻断其他独立 key。

普通模式按 input 持久化合法原始响应、摘要、schema/policy 和 provenance，再按 attempt staging，最终仍与业务终态及预算同事务发布。个人模式按 job 保存响应，整个 ApplyJob 批次及回执原子提交。过滤不重排 ordinal，操作身份不随重试变化；恢复已保存结果不新增提取请求。单条错误不会触发模型重试，普通模式的整包格式重试最多两次。

新增 version=2 / memory-write-policy-v2 schema 迁移，保留 version=1 checksum 和旧事实/回执。新工具为 memory-tools-v3；旧 V1/V2 checkpoint 继续使用其原 schema，但拒绝产生新记忆效果。旧 job/result、旧 staging 缺少新策略时可解释地丢弃，不补拍来源、不按新规则重新编号旧操作。来源协议、写入 policy 与预算协议的边界详见协议页。

### F7a：入队资格与每日上限

single_user PostRun 在 modelAllowed=false 时记录 quota_skip，不入队；后续恢复不能重新入队。入队、认领及落库继续核对同输入的显式动作，保留阶段 1 的 no-op 回执保护。

增加宿主 memory.daily_attempts / SDK MemoryDailyAttempts，默认 200，范围 0–1000000，0 暂停新的后台调用认领。UTC 日期桶在认领事务中持久化，重启不清零；已保存结果重放不占额度。认领后未调用即中断仍保守计一次，升级首日从旧 attempts 保守初始化。已同步配置模板和加载验证；实际安装配置未修改。

### F2：可选故障与核心事务分界

独立的 Stage、Enqueue、保存结果或审计事务，只有明确成功回滚后才能作为可选失败降级。审计失败采用固定原因码日志与计数兜底，不输出原文或 SQL 错误，不递归审计，并尽量保留输入决定。

取消/超时、预算 unknown/持久化错误、执行 fencing 失效、提交结果不明和终态事务失败继续传播。尤其 Stage 检查预算 attempt 时，SQL 失败保留预算持久化错误语义，缺失或失效的 attempt 保留执行冲突语义，不能通过可选包装吞掉。前台回答生成并不等于 run 已成功提交。

## 3. 验证结果

基线提交为 68753f15e51ef285260bca52388c44c786ff0f41，测试针对含既有改动的未提交工作区。固定 Go 1.26.8 windows/amd64、GOROOT 与 GOTOOLCHAIN=local；新增测试使用临时 SQLite 数据库、进程内假 provider 和 SQL 故障注入，没有调用实际安装的模型服务。

| 集合 | 结果与边界 |
|---|---|
| 最终记忆、来源、checkpoint、配置回归及两个补充测试 | **165 个叶子用例通过，1 个真实 embedding 测试跳过**；其中全部 **82 个阶段 2 专项通过**。主回归 163 个通过，随后迁移回滚与普通 staging 原始序号各补充 1 个通过 |
| 最终 runtime 记忆、执行、continuation、后台任务、预算及 SkillLearningAutoPatch 回归 | **302 个叶子用例通过**，222.4 秒 |
| SDK 技能自动发布管线定向复测 | **4 个叶子用例通过**，覆盖普通、委托及历史未完成任务场景 |
| 技能 evolution 的 External/AutoPatch 回归 | **59 个叶子用例通过** |
| 较早的受影响包整包回归 | memory、learning、tools、harness、launch、configschema、ACP agent、Eino 共 **1408 个通过、7 个跳过**；发生在最终兼容入口拆分之前，受该拆分影响的记忆/来源/技能路径已另行复测 |
| 剩余 memory_fix_repro | **2 个预期目标断言失败**，退出码 1：Flush 仍提前成功、IndexPending 坏样本仍阻塞健康事实；属于阶段 4，未计为通过 |
| race / 真实服务 / 实际安装迁移 | race 所需 CGO/编译器未就绪；真实模型/embedding、安装数据库副本完整升级回滚未执行。相关跳过和平台限制均未计为通过 |

完整命令、时间、包结果、跳过/失败诊断、原始日志摘要与专项用例名单见[测试记录](reports/memory-stage2-tests-20261116.json)。集合之间有重叠，不将各行相加为独立测试总数。

必须保留的验证边界：较早的根包整包运行在旧共享分类实现下出现 3 个技能自动发布失败，并达到 600 秒时限；较早的 runtime 扩展选择出现 9 个同类失败，并达到 360 秒时限。拆分兼容入口后，上表的 SDK 与 runtime 相关测试全部通过，原技能生命周期测试未删减或放宽断言。最终树没有重新完成根包整包或 runtime 全包测试，因此不宣称全仓库/整包全部通过。git diff --check 通过。

专项覆盖包括：逐项未知/重复字段及 metadata 校验、完整证据与伪造值、同 key 冲突、forget 目标、两条管线原始 ordinal、批内失败整体回滚与结果重放、superseded 兄弟项、跨重启每日限流与免费重放、旧 job/staging/checkpoint、来源提升与迁移回滚、可选 SQL/审计故障及核心错误继续传播。

阶段 1 剩余的 7 个复现中，以下 5 个已移入默认测试：无依据外部内容、权限声明、合法兄弟项保留、个人入队 quota_skip、可选审计故障保持回答。它们与新增专项共同验证本阶段目标；没有把剩余失败数量当作全部未完成工作的数量。

## 4. 复现命令

在 go-harness 目录运行；以下是实际验证命令，完整扩展集合见 JSON 记录：

~~~powershell
$env:GOROOT = 'D:\GO_ENV\GOPATH\pkg\mod\golang.org\toolchain@v0.0.1-go1.26.8.windows-amd64'
$env:GOTOOLCHAIN = 'local'
$stage2Go = Join-Path $env:GOROOT 'bin\go.exe'

& $stage2Go test . ./internal/memory ./internal/learning ./harness ./internal/launch -run 'Memory|Personal|External' -timeout=180s -json -count=1
& $stage2Go test ./internal/memory -run '^TestMemoryStage2MigrationFailureRollsBackNewColumns$' -timeout=60s -json -count=1
& $stage2Go test . -run '^TestMemoryStage2OrdinaryStagingKeepsOriginalOrdinal$' -timeout=60s -json -count=1
& $stage2Go test ./internal/runtime -run 'Memory|Execution|Continuation|Background|RunQueue|Budget|SkillLearningAutoPatch' -timeout=360s -json -count=1
& $stage2Go test . -run '^TestSkillLearningNative(DelegatedPipelineAutomaticReviewAndPublication|PipelineAutomaticReviewAndPublication|AutomaticPipelineSurvivesHistoricalUnfinishedTasks)$' -timeout=240s -json -count=1
& $stage2Go test ./internal/skills/evolution -run 'External|AutoPatch' -timeout=180s -json -count=1

# 尚未实施的阶段 4 两项缺陷；当前应退出 1。
& $stage2Go test -tags memory_fix_repro ./internal/memory . -run '^TestMemoryFixRepro' -timeout=180s -json -count=1
~~~

## 5. 工作区保护与下一阶段

开始前记录的 50 个已有修改/未跟踪文件中，31 个 SHA256 保持一致，另外 19 个为本阶段需要更新的记忆接线、实现、测试及计划。阶段 0/1 报告、已有独立引擎/传输/桌面 smoke 改动等保持原样；未回退用户已有工作。该清单是已记录文件的对比，不代表整个 git diff 都由本阶段产生。

[修复计划](memory-fix-plan.md)已标记阶段 2 完成，并保留阶段 3–5 的门槛。下一阶段需要把后台模型调用接入来源 root 的预算 member/attempt、分派预留、usage 结算及 lease/fence 恢复；当前仍有原后台模型调用路径，不能用于宣称预算受控的完整修复发布。自动恢复 tombstone 继续关闭。

本次按要求止于工作区实现和测试。race、真实模型质量/对抗评测、安装数据库副本完整 T10、备份恢复及可追溯构建仍是后续发布条件。
