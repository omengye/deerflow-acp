# 真实模型记忆写入修复与复验报告

日期：2026-11-17。基线提交：`68753f15e51ef285260bca52388c44c786ff0f41`；验收针对未提交工作区。本机范围与[首次发布验收](memory-release-acceptance-report.md)相同。

结论：**自动空候选与显式项目路径未写入已修复，16 项真实模型质量、对抗及删除后重新学习验收全部通过**。此次修正模型指引并保护旧执行契约，没有放宽宿主校验、改变删除/预算规则或新增数据库迁移。未部署、未提交，没有修改当前安装配置或数据库。

## 1. 定位与修复

### 自动提取空候选

首次报告中的四个正样本均有完整 usage 和已完成 job，持久结果为零候选，且无逐项拒绝。因此问题发生在模型提出候选之前，不是数据库没有保存合法候选或宿主将返回的事实误删。

两条自动管线实际发送 `{"user":"已接受原文"}`（普通模式可另带 assistant 上下文），旧指引只要求不提取 quoted text，未解释 JSON 运输包装、解码后的原文和原文内部引用的区别。它也没有列清各 typed key 的映射及可执行正例。最新指引明确：

- 仅 decoded user 是事实与证据来源；JSON 定界引号/转义不是用户的引用，offset 从解码后的原始 Unicode code point 起算，assistant 不能提供用户事实。
- 给出 canonical key 映射，明确最近报告地点和长期回答语言可保存，普通受支持声明不需要先说“记住”。
- 加入不同于验收输入的五个正例；scope 随普通/单用户模式选择，证据位置由原文计算。回归测试将实际提示词中的示例重新交给 ParseExtraction，两个模式均通过宿主校验。

只修改模型指引后，原地点输入产生合法候选；完整轮的姓名、地点、语言、数据库和通用饮品事实均保存成功。证据将故障定位在未完整说明的模型输入契约；不声称已经通过实验单独分离 JSON 引号、key 映射和正例各自的贡献。引用、代码、权限、凭据等宿主拒绝规则保持原样。

### 显式项目路径

先在原实现上增加脱敏参数诊断并复跑：模型选对 `project.path`、workspace、project，evidence 是完整当前输入，offset 为 0–24 且 content 省略，但 value 不等于 `src/app/main.go`。持久工具回执为 **not_executed / unsupported_value_or_content**，不是版本冲突或权限审批未完成；本轮基线花费 3 次调用、4186 token，全部结算。

旧模型指引没有说明已登记的 project.path，并要求通用 key 使用整个事实子句。新指引明确 project.path 是 typed 字段，value 只取原文路径，并给出通过宿主校验的完整参数例子。宿主仍拒绝把“项目路径是 src/app/main.go”作为路径值；补充 SDK 回归同时验证正确路径写入与错误整句值的 not_executed 回执，没有用自动截断值或放宽验证掩盖问题。

### 固定执行契约

直接改已有 V3 指引会改变恢复执行看到的资源契约，因此新执行使用 **memory-tools-v4** 指引版本；工具参数 schema 仍是 V3。恢复 V3 checkpoint 时保留旧 V3 指引、工具 schema 和原写入路径，状态不重编码。V1/V2 的既有兼容限制保持。回归验证新执行取得新指引、固定 V3 状态与工具 schema 不变，以及实际记忆工具后的多次执行恢复与重开。

写入策略 memory-write-v2、提取 schema=2、数据库 schema=5 均不变。新请求摘要和预算估算包含新提示词；已付费结果重放、原始 ordinal、CAS、删除代次与值身份规则未修改。

生产改动集中在 [extraction_v2.go](../go-harness/internal/memory/extraction_v2.go)、[personal_memory.go](../go-harness/personal_memory.go)和[extensions.go](../go-harness/extensions.go)。

## 2. 真实模型完整复验

沿用当前安装配置的默认 `gpt-6.1-sol` / `responses` 路线，只读配置获取模型和网络参数。所有输入、攻击文件、workspace、profile 和数据库均为合成临时数据，不访问安装历史。每个 run 保持最多 4 次模型调用、4 次工具调用、40000 token、单次输出 1024 token 上限。

| 类别 | 用例与结果 |
|---|---|
| 自动正样本 | 中文姓名、短英文姓名、地点、长期中文回答偏好、workspace 数据库、通用饮品偏好均实际保存 |
| 显式路径 | 只保存 workspace 项目路径，global 无项目事实；remember_memory 成功，重开后 ID/revision/content 一致 |
| 不可信/不支持输入 | 引用、代码块、否定、权限注入、合成凭据请求、只走自动提取的路径请求均无事实 |
| 外部文件攻击 | 成功读取含伪造 SYSTEM OVERRIDE、伪造姓名和越权偏好的 README，global/workspace 均无污染 |
| 用户事实与攻击同轮 | 原文真实姓名保存，恶意文件中的姓名/权限不保存，保留 external_context；此例仅开放 read_file，验证自动提取路径 |
| 自动重新学习 | 保存北京→SDK 删除→同值候选被删除屏障拒绝→上海恢复同一事实 ID、revision 增长→重开保留 |
| 显式同值恢复 | 真实模型显式保存北京→SDK 删除→新的显式请求恢复北京，仍为同一 ID、revision 增长→重开保留 |

总计 **16 个叶子用例全部通过、无跳过**，包耗时 168.046 秒。攻击读取均由 completed 持久回执证明，断言实际事实库，不只检查模型回答。

自动生命周期的三个 run 均产生 1 个候选并有 1 个已结算 worker 调用：首次 saved；同值为 requires_explicit/same_deleted_value、可见事实数 0；不同新值为 updated/relearned_new_value。删除后搜索为空，新值恢复后的 fact ID 不变且 revision 为初始值+2，Close/Open 后一致。同值步骤不能用模型返回空候选冒充删除屏障通过。删除通过 SDK 的真实删除接口执行；此轮不把它称为真实模型 forget_memory 工具验收。

显式同值恢复与显式路径均关闭自动提取，证明事实来自真正的模型工具调用。两条恢复路径分开验证，没有把新值自动恢复等同于同值自动恢复。

完整轮 **43 次模型调用、53078 token**，其中 16 次为已结算 worker 调用；hold=0、未结算调用=0。加上修复前显式诊断的 3 次/4186 token、修复后两场景预验的 9 次/11269 token，本轮合计 **55 次/68533 token**，均已结算。token 数不是货币费用；不与上一轮发布验收的调用数混算。

## 3. 确定性回归、race 与编译

| 验证集合 | 结果 |
|---|---|
| 修复前指引契约复现 | 自动指引与新执行指引各 1 个目标断言失败；正确/错误路径值的宿主边界保护已通过 |
| 新增回归 | 5 个叶子用例覆盖双模式提示词示例、显式路径示例、V3/V4 固定契约及两种 SDK 路径值，均纳入最终集合通过 |
| memory、launch、tools 整包 | **351 通过、8 跳过、0 失败** |
| SDK/Eino/ACP/runtime 的 Memory/Personal/Extension/相关执行恢复集合 | **175 通过、无跳过** |
| 指引和固定执行恢复 race 专项 | **10 通过、无跳过、无 data race 报告**；包含记忆变更后的恢复/重开、旧 V2 兼容及新 V3/V4 指引检查 |
| daemon 编译 | Go 1.26.8、`go build -trimpath -p=2 ./cmd/deerflow-acpd-go` 成功，产物仅写临时验收目录 |

集合存在重叠，不累加成独立测试总数。普通整包的 8 项跳过包括单独运行的真实模型/embedding opt-in、无关真实服务和 Windows 符号链接权限。普通测试不是 race，当前受限样本不是全部模型或任意自然语言的质量保证。命令、叶子结果、跳过原因和 SHA256 见[机器可读记录](reports/memory-model-fix-20261117.json)。

本轮修改测试：[模型指引回归](../go-harness/internal/memory/model_guidance_test.go)、[SDK/恢复回归](../go-harness/memory_model_guidance_test.go)、[真实模型验收](../go-harness/internal/launch/memory_live_acceptance_test.go)。新增结构诊断只记录固定原因码、布尔值与位置，不记录任意工具错误正文、模型原始响应或凭据。

## 4. 发布状态与工作区保护

[修复计划](memory-fix-plan.md)、[写入协议](memory-write-policy.md)与[运行手册](memory-upgrade-runbook.md)已同步。模型质量、对抗和重新学习的原发布阻塞已消除；剩余顺序为：审阅形成干净源码提交→重建包含此次修复的正式交付包并跑完整发布回归/双模式冒烟/包内 Bridge→按手册执行目标安装备份、受控升级和安装验收。

上一轮开发包及其 manifest 对应旧源码，不包含本轮修正；本轮 daemon 编译不替代完整包验收。embedding、索引、迁移和预算实现未改，保留此前对应证据，不重复启动 embedding 服务或操作安装数据。

开始时保存 126 个既有修改/未跟踪文件的摘要（排除既有 `.deerflow` 运行目录）：118 个不变、8 个按任务更新、无丢失；另新增两个回归测试文件及本报告/机器记录。最终对照及源码摘要见机器记录。Go 格式、git diff --check、52 个文档本地链接和 JSON 检查通过；既有无关修改及历史机器验收记录保留。安装配置与只读迁移副本摘要未变。没有提交、部署或重启当前安装。
