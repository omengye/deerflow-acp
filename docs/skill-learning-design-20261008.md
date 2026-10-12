# 多轮会话经验学习与技能进化设计

日期：2026-10-08。
状态：P0 typed maintenance、P1 shadow 捕获/提取、P2 有界语义归并、授权 skill 匹配、workspace 双重授权共享、lineage 与传递撤销，以及已有 skill 改进和独立 opt-in 的新能力 `experience_pattern` 自动待审提案链路已实现。P3 已接入默认关闭的学习专用受限 auto_patch、正文定向改善、description 正负元数据选择评测及发布来源/观察/回滚联动。单条忽略/恢复、人工已有目标裁决、显式新技能创建重试与原请求键回执已实现；对应桌面缓存控制和原学习来源/history、逐会话贡献/共享、独立发布开关及触发评测报告已接通源码与隔离逻辑测试。桌面构建/视觉验收、真实模型标注/触发质量验收未完成；description 以外的 frontmatter 变更、新技能和人工裁决来源候选仍人工审批。定向验证及边界详见 [实施记录](skill-learning-status-20261008.md)。
技术约束：保持 Go/Eino 单运行时，复用 SQLite、维护预算、候选评审、不可变发布和回滚，不引入 Python 学习服务。

2026-10-09 收口说明：最新要求以 MVP 产品能力为准，自动总结、生成、人工审核与受限自动采用主链已接通并经过定向验证；不再以桌面完整验收、真实模型标注质量或低收益边界修复阻塞本次交付。启用方法、验收依据与后续项见 [MVP 交付说明](skill-learning-mvp-20261009.md)，本文完整设计保留为后续参考，不扩展本轮测试范围。

## 1. 结论与产品定位

建议采用“自动沉淀经验、自动生成候选、受控发布技能”的分层方案。

设计前的项目已经具备真实技能读取证据、终态 outbox、自动候选发现、独立生成预算、安全审查、宿主行为评测、人工审批、受限 auto_patch、观察与回滚。原先主要缺口位于前半段。此次已接入普通用户多轮对话的有界捕获、脱敏和过程经验提取，以及单独授权的语义归并、已有 skill 匹配、workspace 内共享来源归并与 lineage 去重。另行启用经验发现后，已有 skill 的正文缺口和触发问题可以生成待审候选；再独立授权新能力发现和非内置来源，可创建单文件待审新技能。另行授权学习来源 auto_patch 后，通过可信定向改善/回归评测的已有 managed 技能小型正文补丁可自动发布；description 修改另需宿主 v2 正负元数据选择套件和真实触发断言改善，同改正文须分别改善。发布后继续观察回滚。经验、pattern 与模型 match 仍为模型推断、质量未判定；人工目标裁决另记 `host_adjudicated`，不冒充模型判断或质量确认且其候选必须人工发布。显式创建重试有独立人工授权、非退款上限和原请求回执，不绕过自动 lifetime 创建预留。这条确定性接线不证明真实语义质量；学习 UI 未做桌面构建/视觉验收，真实模型标注验收仍需后续实施。

补齐后，用户无需每次打开反馈表单，系统可以自动识别“为什么失败、后来怎样解决、该方法何时有效”，关联已有技能，并产生更新提案。是否直接生效仍取决于用户选择的发布模式。技能进化改变的是持久化的操作指引，不是在线训练模型权重。

不建议采用“每 N 轮把聊天摘要追加进 SKILL.md”，也不建议以一条主 Agent 提示词作为自动学习的唯一保证。

## 2. 三家官方方案核对

实施范围已按 2026-10-09 最新用户要求收敛为“系统自动沉淀并进化技能，支持自动或人工审核”。用户不需要手工整理经验、选目标、发起生成或切任务边界才能得到待审提案。下面的人工裁决、创建重试属于已有可选管理能力，不是进化主链必需步骤；本轮不继续扩展手工起草入口。

同会话独立任务识别仍保守，不靠时钟或模型拆出成功票。新增独立默认关闭 `discovery.experience_review_drafts_enabled`，允许一个有完整本地观察证据的任务由后台自动起草待审提案；固定 `review_draft=true`，始终人工审核，不能通过后续证据、配置或评测自动升级为可自动发布。原完整独立任务门槛和受限 auto_patch 继续保留。

以下描述以本次获取的官方文档与源码为准，不代表用户当前安装版本一定具备全部能力。Codex 和 Claude Code 文档会持续更新；DeerFlow 源码固定在 main 提交 `89c9f0871432e169a67b7db21984c8d2c3486c21`。

### 2.1 Codex

- 将人工维护的 `AGENTS.md`、生成的 memories、按需加载的 skills 分开管理。必须始终生效的团队规则应进入版本控制的指令文件，不能只依靠生成记忆。[O1][O2][O3]
- 本地 memories 默认关闭。启用后，对符合条件的历史聊天后台提取；跳过活跃或短生命周期会话，等待足够空闲，并受剩余额度门槛约束。生成字段包含秘密脱敏。[O1]
- 分别提供每个聊天“使用已有记忆”和“贡献新记忆”的控制；可以排除使用外部 MCP/web 上下文的聊天。提取模型与整合模型可以分别配置。[O1]
- Skills 使用开放 Agent Skills 格式与渐进式加载。明确支持 `$skill-creator`、手动创建，以及 Record & Replay 从演示流程起草技能。默认倾向 instruction-only。[O2]
- 本次核对的官方资料没有建立“任意聊天结束后自动把 memories 提升为 SKILL.md 并发布”的通用契约。不能把后台 memories 功能宣传成通用自动技能更新。

借鉴：延迟学习、提取与整合分阶段、贡献与使用分开、上下文预算、规则与记忆分层。

### 2.2 Claude Code

- 将作者维护的 `CLAUDE.md` / 支持的 `AGENTS.md` 与 auto memory 区分。Auto memory 在本地会话默认开启，但自托管和某些后台会话的默认与可用性不同。[C1]
- 自动笔记有 `user`、`feedback`、`project`、`reference` 四种类型，会记录用户纠正；并非每次会话都写入。[C1]
- 每个项目有本机 memory 目录；`MEMORY.md` 仅加载前 200 行或 25 KB，详细主题按需读取。官方建议多步骤过程迁入 skill，而不是无限增长常驻指令。[C1]
- 当前有明确的“过程配方学习”实例。`/run-skill-generator` 从干净环境捕获项目启动配方；`/verify` 可以将成功验证配方记录为项目 skill。记录只在配方误导执行时修正，例如命令失败或遗漏步骤，避免每次会话都产生改动。[C2]
- `Stop`、`PreCompact`、`SessionEnd` 等 hooks 提供扩展点，但不是通用技能学习调度器。`Stop` 不等于完整业务任务结束，transcript 异步写入还可能落后于当前消息。[C3]

借鉴：把纠正作为可追踪经验、记录经过验证的配方、出错才修复、保持技能稳定，并区分回合结束与任务结束。

### 2.3 字节 DeerFlow

- `skill_evolution.enabled` 默认 false，开启后向 Agent 提供 `skill_manage`。[D1][D4]
- 主 Agent 的 `Skill Self-Evolution` 提示词要求在任务结束后考虑创建或更新技能，具体信号包括 5 次以上工具调用、非显然坑点、用户纠正后奏效、重复工作流。使用技能遇到缺漏时提示立即 patch；优先 patch，新建前先向用户确认，跳过简单一次性任务。[D2]
- 此处的“5 次以上”是提示词启发，不是确定性队列准入规则。模型是否遵循、自主选择哪些经验，仍受模型行为影响。
- `skill_manage` 对 custom 技能执行静态与模型安全扫描，然后写入存储、记录修改 history，并刷新技能提示缓存。它不是本项目这种先固化候选、行为对比、审批或受限自动发布的事务链路。[D3]
- 记忆层在 Agent 执行后异步入队，debounce 合并；默认 DeerMem 队列是进程内 best-effort 队列，不是 durable outbox。优雅退出有尽力 flush，异常退出未处理项可能丢失。[D5][D6]
- 内置 `skill-creator` 有起草、旧版/无技能基线对比、评测、人工反馈、改进、description 触发优化的完整作者工作流。这不能等同于所有对话后台自动执行该流程。[D7]

借鉴：完成复杂任务后捕获坑点和配方，使用技能管理边界，先小 patch，结合旧版基线评测。保留我们项目更强的持久化和发布治理，不照搬直接覆写或进程内定时队列。

### 2.4 综合选择

| 问题 | 推荐做法 | 主要参考 |
| --- | --- | --- |
| 什么值得学习 | 用户纠正、已验证配方、真实缺口，跳过一次性任务 | Claude Code、DeerFlow |
| 什么时候学习 | 终态记录来源，空闲后后台提取，避免打断主任务 | Codex、DeerFlow |
| 如何避免杂乱 | 经验分型、语义整合、先匹配已有技能 | Codex、Claude Code |
| 如何持久化 | 领域事件 frontier、durable queue、幂等与版本校验 | 当前项目 |
| 如何更新技能 | 最小候选差异、基线评测、受控发布、观察回滚 | Claude 配方修复、当前项目 |

这是一套结合官方机制与本项目治理能力的设计，不是三家都已实现的统一“自进化标准”。

## 3. 当前项目基础与缺口

| 当前能力 | 代码入口 | 本次设计的处理 |
| --- | --- | --- |
| 实际读取技能与终态 outbox | `go-harness/internal/runtime/skill_evidence.go` | 保留，不往 metadata collector 中塞聊天全文 |
| 结构化 operator 纠正发现 | `go-harness/internal/skills/evolution/discovery.go` | 保留，新建 experience trigger，不伪装 operator 反馈 |
| 缺失报告、重复工具形状发现 | `go-harness/internal/skills/evolution/discovery_derived.go` | 保留，接入统一目标与去重裁决 |
| 受预算约束的候选生成 | `go-harness/internal/engine/eino/skill_generation.go` | 接收脱敏过程经验与精确来源引用 |
| 维护队列、租约、调用计费 | `go-harness/internal/skillmaintenance` | 扩展 typed producer，统一分发而非另起模型 goroutine |
| 宿主行为评测 | `go-harness/internal/skills/evolution/behavior_evaluation.go`、`go-harness/internal/engine/eino/skill_behavior.go` | 保留可信 fixture 与 baseline 对比 |
| 受限发布和回滚 | `go-harness/internal/skills/evolution/auto_patch_store.go` | 保留并对新来源增加证据准入约束 |
| 描述性事实提取 | `go-harness/memory_extraction.go` | 第一期保持现有语义，不混入过程学习 |

需要补齐的关键点：

1. 普通聊天的用户纠正不会自动变成当前 Discovery 使用的结构化反馈。
2. 当前重复 workflow 指纹只包含工具名、参数键和类型，不包含目标、成功参数、坑点、关键结果，因此难以形成真正可执行的经验配方。
3. 当前纠正按同一个技能版本聚合，没有对纠正含义做语义归并。相同技能上的两种不同问题不应草率合成一个 patch。
4. 当前 derived workflow 新建目标使用摘要名；若已有目标或 tombstone，会跳过，并非自动找到和改进语义相关的已有技能。
5. 每次用户 prompt 通常产生新 root。多轮纠错虽有多个 root，却可能只是一个任务的修复链，不能当成多个独立成功样本。

现有同步 memory 提取仅接收本轮用户输入和最终回答、提取 durable descriptive facts，并在主 run 终态事务中提升。不要把它直接改成异步技能改写器，也不要让技能学习继承主任务临时授权。

## 4. 四层知识模型

| 层次 | 内容 | 自动写入边界 |
| --- | --- | --- |
| Policy / Instructions | 团队规则、工具权限、审批、预算 | 仍由宿主与用户管理，学习不能提权 |
| Declarative Memory | 用户偏好、长期项目事实、参考入口 | 继续受 memory 的来源、范围和 revision 约束 |
| Episodic Experience | 某次任务的目标、尝试、纠正、解决、验证 | 本次新增，可自动提取，默认不作为权威规则注入 |
| Procedural Skill | 泛化后的步骤、适用条件、坑点、验证方法 | 通过既有候选评审与发布链路进入运行时 |

“回答用中文”属于偏好；“此项目生产部署需要人工审批”属于规则；“读 JSON 时曾因逐行输入失败，改用完整文件文本成功”属于经验；“该环境的 JSON 配置检查流程”才可能是技能。

生成 memory、归并 experience、发布 skill 使用不同的 authority。任何模型输出都不能自动成为 operator-confirmed 质量反馈。

## 5. 目标链路

```text
真实用户输入 + 已提交回答 + 工具回执 + 实际 skill reads
                    |
       终态事务写学习 outbox / 来源 frontier
                    |
       筛选、脱敏、任务链关联、空闲与预算检查
                    |
       ExperienceExtractor：结构化过程经验
                    |
       Consolidator：去重、合并、冲突、适用范围
                    |
       SkillMatcher：已有技能 / 触发问题 / 新能力
                    |
       ExperienceDiscovery：完整独立证据 / 单任务人工待审草案
                    |
       既有 Generator -> 安全/内容/行为评测
                    |
       review 或受限 auto_patch -> 不可变发布
                    |
       真实读取观察 -> 显式质量判断 -> 必要时回滚
```

主对话只提交来源索引，不等待学习模型。自动学习不能重放原工具、读取整台电脑、增加审批权限或重新执行已经产生的副作用。

### 5.1 触发和调度

- 在终态事务中记录 `session_id`、`input_id`、`root_id`、事件 frontier、结果版本与 consent epoch。outbox 不复制未脱敏的聊天、工具输出或 provider 错误。
- 学习 worker 使用独立消费位置，不能争用或提前 ACK 当前技能 collector 的 `processed` 状态。
- 先 debounce 合并同会话待处理引用，再等待空闲。建议初始空闲窗口为 5 分钟；这是本项目建议值，不是三家共同默认。
- 不能依赖时钟不断重置导致长期聊天永远不提取。已完成 episode 可以在后续任务开始后单独排队；持续活跃且无明确边界时只做有界 capture，等任务链 settled 后再用于技能生成。
- 自动生成技能前要求相关任务树 settled、无 uncertain effects、无未决审批或待回收资源。正常回合结束不是完整业务任务成功证明。
- 对失败任务可以保存诊断经验，但仅在有后续纠正与验证支持时标记为“解决”。网络、权限、配额、取消不会自动归因到技能。
- 上下文压缩前只提交 capture/flush hint 或已提交来源的引用，不在压缩钩子里做昂贵学习，也不把未完成任务当成功案例。当前完整领域历史并未随原生摘要删除，提取应读领域事件而非摘要文本。
- 用户无需显式要求“整理为技能”。启用单任务自动待审草案后，满足来源、匹配和执行证据条件即可由后台起草；独立证据不足的草案只走人工审核，不授予自动发布。
- 重启扫描未消费 outbox；停止时取消并 join worker。关闭后台学习不影响主回答和下一轮 prompt。
- 已入队但未领取的提取、归并、匹配和经验候选生成，如果原 episode 出现新一轮活跃真人任务，先通过共享维护队列的 metadata-only readiness hook 等待，不把暂时活跃等同于来源撤销。等待不增加 attempt，不启动或延长原 deadline，不退款或隐式修改旧快照；只等待原捕获 episode 和必要的归并 shortlist，后续无关 episode 不阻塞。每轮最多扫描原全局 100 个 pending 槽，忙碌来源不挡住其他可执行任务。领取后的模型准入和提交仍完整重核验；已付费作业仍受原有限尝试和截止时间约束。
- 领取后或模型执行中出现普通追问时，只有完整核验原捕获字节、全部成员/归并 shortlist、授权、目录/head 和固定投影均有效、仅存在已接受真人任务活跃，才返回宿主 `source_active`，进入原作业的有限重试。只读核验 scope 在同一 SQL 事务内先记录活跃、继续检查其他固定来源，永久错误优先，不把一般 permission failure 当作临时状态。缺失终态 outbox、变化来源、撤销贡献/共享、策略/目录/head/revision 变化仍终结旧工作。所有原生维护适配器保留该原因而不泄露来源或 provider 诊断。最多 3 个原 claim attempts、首次领取的原 deadline、共享预算和不退款消费不变；等待期间 readiness 不再领取。已有候选、内容报告和完成行为用例复用，尚未合法提交的付费输出可以丢弃并再次计费重做，不承诺模型调用 exactly-once 或超时/额度耗尽后自动恢复。
- 审核结果已提交、仅缺 maintenance ACK 时保留原终态确认语义，不因后续对话重做生成或审核。人工/自动批准遇到临时来源活跃时拒绝本次发布、保留待审状态；来源结束后再次走原完整授权和版本校验。永久变更、撤销、目录/head/策略变化仍使旧候选失效，不自动 rebase 或重新生成。
- 自动目录重新评估仅发生在当前 pattern 尚未预留任何 generation、没有人工控制和创建 lifetime 预留、原匹配已完成或明确因宿主 `inventory_changed` 终结时。共享 learning producer 扫描原 pending 后，每轮最多考虑一个 metadata 候选；原 pattern、来源成员/贡献/共享、归并及全部 shortlist、模型和宿主访问身份均保持有效，且正式 catalog revision 确实改变，才重建当前授权 inventory 和有界 view，创建全新 match/job/digest，不重试或改写旧结果。每个 pattern 生命周期最多两次额外评估，一分钟冷却，从首次 match 起三十天内；均使用原全局/learning 槽与 paid budget，不退款旧尝试。只读 history 保留旧结果，不是新生成权。新结果 `already_covered` 不生成候选，符合条件的结果仍走原单任务人工草案或独立证据受限自动发布。
- 人工取消、忽略/恢复、裁决、禁用产生的取消、配置/access identity 变化、损坏/缺失 input/result、来源撤销和预算/截止等一般终态失败不作为重新评估入口；已经有生成预留的 pattern 也不因目录更新递归产生新提案。真实后续 accepted task 经归并产生新 revision 时，使用原自带 stage input 自动匹配，不将旧 revision 的输出 rebase。首次 generation 已耗费的来源/目标/版本及新能力 lifetime 创建消费继续保持，不靠重新评估重新选名称、来源或退消费。

### 5.2 多轮任务 episode 与独立性

新增 `episode`，表示一个用户目标及其尝试、纠正、完成链。它和现有预算 root 不同，不改变 root 计费、子任务归属或恢复协议。

例如“先检查配置 -> 不对，改用完整 JSON 读取 -> 再确认全部字段”，三次 prompt 可以产生三个 root，但只算一个 episode。重试、通知续跑、子代理与会话 fork 也不增加独立票数。

episode 关联优先使用真实用户输入来源、被纠正的前序回答/工具、共同产物或操作对象、明确的任务边界。模型可以建议合并已有 episode，但不能凭自己的判断拆出多个独立票。模糊情况保守合并，等待后续独立任务或用户确认。

`independence_key` 必须由宿主生成并保存，不能使用模型输出的“成功次数”。跨会话复制和恢复使用 lineage/reference 去重。一次 epoch/frontier 的重复提取不能增加 pattern 支持数量。

当前实现补充采用附加 lineage 表，不改原 episode 的 UNIQUE 键；相同 workspace 与完整初始 accepted input 摘要保守合票，显式新任务使用新键。可信 SDK/MANAGE 可在新会话没有 run/episode 前关联同 workspace、双方 shared consent 的来源 episode，改写复制与后续恢复沿用原独立键。当前无原生 copy/fork 接口；未来实现必须接入该宿主边界，未声明的改写复制不能靠模型可靠检测。

### 5.3 ExperienceExtractor

使用无工具模型，输出严格、有界 JSON。首先构建经过授权和脱敏的输入视图：真实用户指令、最终回答、允许的工具参数和关键结果片段、失败码、实际技能读取身份。不提供隐藏推理、不扫描全部历史、不信任网页或工具结果内的指令。

后台委派与通知 continuation 的结果已接入同一自动主链。启动时将 runtime 的 `LearningRootObservationsTx` 固定接入 learning；同一 SQLite 事务从 accepted foreground root 重建物理成员及 ancestry、原双份 binding/spec、parent/child policy、实际 native task/attempt/终态与预算结算、角色和委派回执，通知则沿用原 source/role-context provenance reader。模型或管理请求不能提供 root 成员列表。只有整个原任务树安全 settled 后才捕获；一次仍最多八个真人 turn，加总最多八个 delegated/continuation run，原 528 references、输入字节、每 run 512 events/32 receipts/32 reads 和归并来源缓存 32 runs 上限保持不变。

委派 run 只追加实际 skill-read 身份和本地工具的有界脱敏回执，网络/MCP/未知工具仍仅 hints。子代理的合成输入、私有对话/回答、隐藏推理、native result 和通知正文均不进入提取或来源检查视图；`task`、`background_agent`、`skill_manage` 也不成为学习文本。新快照固定 `procedural-root-observations-v1`、每个 accepted source 的精确 sorted 成员 metadata/host digest，并计入 extraction identity；付费预留、结果提交、来源读取、生成及审批时实时重建，新增/缺失/改变后代不能静默扩展或 rebase。原前台-only 快照仍保留原范围，不回填子任务。Eino extractor prompt 升到 v2，尚未完成的旧原生 prompt 作业按模型 identity conflict 保守终结，不自动迁移或退款。

子任务不是新的 episode、独立成功票或 operator 反馈。被经验真实引用的、完整本地完成回执可以归回原真人任务的独立键；其前台 source 与所引子任务都必须 completed，时间窗口采用两者结束时间的较晚者。failed 子任务可以提供诊断观察，但不能授权成功 promotion。一任务仍只能自动生成人工待审草案；受限自动发布仍要求原完整独立任务、评测和独立发布 grant。原 source 被撤销、parent/child policy 或 descendant/notification 改变、blocked/reconciliation 状态都拒绝旧证据，而不是获得 `source_active` 重试授权。

建议经验记录包含以下字段：

| 字段 | 含义 |
| --- | --- |
| `kind` | `correction_chain`、`proven_recipe`、`capability_gap`、`trigger_mismatch` |
| `goal` / `applicability` | 用户目标、环境与前置条件、适用和不适用范围 |
| `problem` / `resolution` | 原方案问题、最小修复和原因 |
| `steps` / `verification` | 可泛化步骤、实际执行结果、可验证完成条件 |
| `user_evidence_refs` | 用户纠正或认可的不可变输入/事件引用 |
| `receipt_refs` / `skill_read_refs` | 实际工具回执与读取版本，不能凭空构造 |
| `episode_id` / `scope` | 宿主确定的 episode 和 user/workspace/agent 边界 |
| `origin` / `confidence` | `model_extracted` 与提取不确定性，不代表效果质量 |
| `outcome` | `observed_execution`、`user_reported`、`unresolved` 等来源区别 |
| `policy_version` / `source_digest` | 提取策略、脱敏版本及输入视图摘要 |

宿主核对引用存在性、顺序、归属、版本和文本对应关系。记录“用户确实说过”和判断“技能确实导致结果改善”是两回事。模型输出的 confidence 不可进入 `operator_confirmed` 分支。

第一期专注 procedural experience。已有 memory 继续独立提取事实；Extractor 分类出的纯偏好不写 skill，不重复向现有 memory API 塞一遍。同源提取的合并和 memory 后台化另做迁移，避免这一功能偷偷改变 `FlushMemory`、计费和终态提升语义。

### 5.4 语义归并与 SkillMatcher

归并以目标、问题、前置条件、解决动作和验证方法为准。工具形状指纹只用于低成本召回，不能证明经验等价。同一个 skill 的不同错误应保留不同 pattern；Windows 和 Bash 配方、开发和生产操作、不同版本 API 不混成“通用方案”。

第一期用有界关键词/词法召回，加无工具模型比较，暂不依赖向量数据库。Embedding 后续可以作为召回优化，不能作为授权或最终匹配依据。

早期 `unresolved` / `user_reported` 成员不能永久阻塞同任务后来完成的纠正，也不能简单被任意成功覆盖。新归并快照固定 `procedural-same-task-resolution-v1`；只有模型已经判定等价、原和后续经验同 session / episode / consent epoch / host independence key、不同 extraction，后续经验明确引用原经验的全部真人输入及更晚真人输入，且引用更晚 turn 的完整本地 completed 回执，宿主才能在新 pattern revision 保存原成员到验证成员的 resolution link。时间顺序使用原 accepted event sequence、全部 foreground/child frontier 和 outbox sequence，不使用模型声称的时间或经验 created_at；所引前台和子任务都必须 completed，旧 receipt、assistant claim、网络/MCP、截断引用或复制 lineage 不能解除原问题。

原成员、原 outcome、全部引用和传递撤销边保留；resolution 只改变该 pattern 的未验证阻塞状态，不新增 episode、成功票或 operator feedback，不认证技能因果贡献。后出现的未验证问题、其他 episode 未验证成员、冲突和歧义继续阻塞。每次最多 32 个成员及原 32 source-run 核验上限，link 从固定捕获视图按最早合法后续 accepted sequence / ID 确定性重建；history 仅检查保存投影和捕获 identity，付费准入、promotion、生成与发布仍逐来源实时核验。Promotion 保留并核验历史成员，但只用实际 observed 成员形成独立观察。

新策略是快照中的可选字段，纳入原固定归并 ID/input digest 和 pattern SHA；未知策略拒绝。旧未完成 job 与已保存 output 不自动升级、修改或重生成，仍按原全成员 unresolved 语义回放。后来真实输入产生新的正常归并 revision 时才可以采用新策略。Eino extractor prompt v3 明确要求纠正链引用早期输入、后续纠正与当轮回执；与此前 prompt identity 不同的未完成原生作业保守 conflict，不迁移或退款。一个已解决任务仍只形成单任务人工待审草案；受限自动发布至少需两个实际独立任务及原发布授权和可信 suite。

匹配顺序：

1. 本次实际读过的相同 scope 技能，检查经验是否真与其步骤有关。
2. 当前有权使用的其他技能的 name、description、必要的有界正文。
3. 判断是正文步骤缺漏、description 触发不准、纯环境事实，还是新的能力。
4. 明确已有技能时生成最小 patch；有多个接近目标时保持歧义，交人工选目标。
5. 无合适目标时提出单一职责的新技能，给出可读名称和边界；不继续批量创造 `workflow-<digest>` 近重复技能。

Matcher 只能在宿主提供的授权 shortlist 中建议目标。`source_id`、source identity、scope、head、overlay base 最终由宿主解析和固定。未实际读过的技能可以作为潜在改进目标，但不得制造该次任务“已经使用该技能”的读取证据。

同名不等于同一个技能；builtin、workspace 和 global 必须继续按 source identity 区分。scope 默认保持项目/用户私有，跨项目或跨用户推广必须另行授权。

跨会话归并必须同时通过独立 host gate 和每会话 `workspace_shared` consent/epoch，并固定在同一私有数据库/profile 的规范化 workspace。Private/shared 目录互不召回；普通 owner-bound ACP 的 pattern/match 列表不返回共享文本，workspace 历史与 lineage 只向认证 MANAGE/可信 SDK 宿主开放。对 seed、整个 shortlist 而非仅最终选中目标保存来源依赖；撤销时递归清除传递派生 snapshot/result/revision/match，不保留未选中比较的模型摘要。Host gate 关闭取消 shared claims，但不能把历史降级为普通会话可见。

### 5.5 自动候选准入

新增 `experience_pattern` 发现类型和独立引用 schema，不通过现有 `SubmitFeedback` 伪造 operator correction / helpful / quality_failure，也不把模型猜出的缺口当作 operator missing report。

| 情况 | 推荐动作 |
| --- | --- |
| 单次偏好或一次性需求 | 保留已有 memory/当前会话语义，不改 skill |
| 一次已解决的复杂任务 | 存一条经验，默认不自动改正式 skill |
| 同一 pattern 在至少 2 个独立 episode 中成立 | 自动生成已有技能更新提案 |
| 重复已验证流程，没有相关技能 | 自动生成新技能提案，待人工审批 |
| 开启单任务自动待审草案且已有核验的本地执行来源 | 一个 episode 可自动起草，只走人工审核，不需手工触发 |
| 无验证、相互矛盾、目标匹配有歧义 | 等待证据或进入人工处理，不自动扩大适用范围 |

“2 个 episode”是建议的初始门槛，后续用真实模型样本校准。调用次数、assistant 自报成功、用户继续聊天，均不替代验证。

Discovery 选择事务固定 pattern revision、episode refs、source/head，并原子提交去重消耗、每日提案额度、目标冷却和 generation job。多个发现入口针对同一 episode、目标和版本只允许消费一次；已有 pending 提案先阻止重复生成，新增证据可形成后续修订而非并排发布。

当前已有、启用且精确授权目标的 `body_gap` / `trigger_mismatch` 可自动生成待审提案；`already_covered`、`ambiguous`、`irrelevant` 不生成。`new_capability` 还需独立 `discovery.experience_new_skills_enabled` 和明确的 generation-authorized 非内置 `new_skill_source_id`。Matcher 仅建议可读名称，宿主核对完整授权目录、无限定 Names selection、workspace/global 可见性，并拒绝已安装、禁用、tombstone 或未完成同名提案。新候选只能包含 `SKILL.md`，不退失败/取消/驳回票，不自动发布。同一 pattern 生命周期仅保留一个自动创建预留，不能靠后续 revision、换名或换来源重复新建；显式人工重试另行授权且保持原 pattern 修订、原名称和来源，最多 3 次、原作业只一个后继。消费 identity 统一为 episode/target/version，人工重试复用原 evidence reservation 不增票、不退消费；旧 trigger/root 记录和已知同 episode root 别名仍阻止重复消费。各阶段重建真实来源，历史列表不是准入凭据。名称结构验证不证明命名语义准确，完整目录也不证明模型判缺准确。

单条 pattern 控制有独立 `state_version`，写入需要当前 pattern revision、控制 CAS、明确 session/workspace scope 及原请求键。忽略覆盖之后修订，取消匹配并撤销未发布候选，不删原始经验、不产生失败票、不退消费或费用、不撤销已采用技能；恢复不重新执行旧作业或重启旧人工裁决。人工重新裁决要求当前授权目录中的精确已有技能/head/overlay，可重查原 shortlist 外目标，但不放宽 Names、scope 或来源权限。保留模型原记录，新建 `host_adjudicated / unassessed`；阻断原匹配准入并撤销旧未发布候选。方法来源仍需 live 全链授权与独立执行证据。后续只能在原 discovery/generation 开关、共享额度/冷却/消费约束内生成，人工裁决来源即使评测通过也不自动发布。一个独立 episode 默认仅是 shadow 分类，另行启用单任务草案后可由系统起草人工待审提案；裁决本身不增加生成权或独立票。新技能/换名/换源重试不能借此绕过 lifetime 创建预留。

显式新技能创建重试使用独立 prepare/enqueue。原 generation 已 failed/cancelled 或 completed 且原提案 failed/rejected 才可准备；仍活跃、待审核、已发布或 stale 拒绝。完整条件包含当前 pattern/review CAS、原 generation/proposal state version、原精确不存在的 target/head/overlay、原 evidence reservation 和 retry ordinal，用户确认后加原 request key。重新校验所有来源、目录、完整 consolidation shortlist、本地回执及当前时间窗口，不自动换名/来源/修订，不隐式 rebase。新的 generation/discovery、generation-bound authorization、review CAS 与共享 daily/pending/cooldown 同事务提交；模型调用仍走原维护预算和有限尝试。人工 retry spent 使用独立 digest-only lifetime cap，永不删除自动创建预留，也不删除/转移 episode 消费。原 key 可返回历史 ID 而不重新执行，即使后来忽略或关闭付费开关；来源撤销清除授权回执正文但保留消费和已采用技能。workspace 操作者即使不是 evidence 成员也必须可撤销。新技能始终人工发布。

当前 old-head 经验不能无检查应用到 new head。版本改变后重新匹配与检查问题是否已解决，不能隐式 rebase 或重写已批准结果。

### 5.6 生成、评测与发布

复用现有 Generator，只改变经过宿主核实的 evidence 供应。生成器仍无工具、无发布权，不增加 Python 依赖、权限、凭证或无关文件。

技能内容优先保留目标、适用条件、步骤、关键坑点和验证方式。具体用户身份、临时路径、一次性 API 参数和原始聊天不进入 SKILL.md。自动学习第一期仅生成 instruction-only；需要 scripts/support files 时转人工作者流程。

学习生成的 `SKILL.md` 在落库前增加宿主确定性凭据检查，独立于安全审查模型。对检查副本先移除隐藏字符、规范化控制字符，再有界展开嵌入 JSON 的转义，拒绝具体敏感赋值、Bearer、常见凭据前缀、完整或未闭合私钥以及 URL 凭据。允许完整且明确的环境变量、占位符和 `[redacted]`，不会把一般文档 URL 或通用临时路径当作凭据。检查只读原候选，不脱敏改写待审正文或内容哈希。此规则仅用于宿主绑定的 `experience_pattern` 生成结果，不能靠请求的 origin/trigger 获得例外；其他候选及支持文件保留原静态规则。保存、提案提交、重启恢复、内容/行为评测入口和人工/自动发布都复核；旧待审候选失败后释放待审容量但不退证据、额度或费用，不自动重新付费生成。已发布或已拒绝结果仍按原终态幂等确认，不借安全升级改写已采用内容。已保存旧正文不是自动清理或追溯撤销对象，敏感内容仍应显式撤销来源；这是有界模式检查，不代表任意凭据或编码均可识别。

评测分开处理：

- 结构与安全：当前解析、静态检查、模型安全审查；安全扫描通过不代表方法有效。
- 行为：使用旧版本作为 baseline；新建使用无技能 baseline。匹配宿主可信 fixture suite，验证目标问题和已有正常流程，不能执行真实发布、支付、发消息或生产命令。
- 触发：正文行为评测不会自动证明 description 匹配正确。已增加独立的宿主正例/负例元数据选择评测，无可信 v2 套件则 description 变更保持人工审核。使用当前固定 Eino Skill middleware 原生 prompt/reminder/tool schema，仅观察首次 Skill 工具选择，不执行工具、不加载正文、不伪造真实 reads；期望标签不发送给模型。
- 泛化：样例与留出任务分开；模型可以起草测试建议，但不能自己提供期望答案、自行批准 suite 并据此通过自动发布。

默认沿用人工审核。`experience_pattern` 不能借用旧 operator auto_patch 的授权，必须另行开启 `auto_patch.experience_patterns_enabled`；该字段缺省为 false，要求当前启用 auto_patch 模式、完整学习/经验 discovery 链和宿主 behavior suite。未匹配行为 suite 明确显示 `not_run`，人工可以决定 instruction-only 提案是否采用，但不能冒充评测通过。

明确授权的可信项目可开启已有 `auto_patch` 及独立学习发布 gate，保留全部现有限制：已启用的 managed 正式 head、automatic 来源、只改 SKILL.md、默认最多 40 changed lines、安全与行为通过、至少一个 targeted 和一个 regression case、无 regression、不改 builtin overlay、不新建/删除/改支持文件。所有学习来源与旧发布共用每日发布额度和同目标观察锁，不新增维护模型 worker 或发布路径。

已实现的 experience 正文自动 patch 额外要求至少一个 targeted 比较从 baseline `assertion_failed` 到 candidate passed；只改善 regression case 或全部无差异不自动发布。description-only 自动 patch 使用宿主 `skill-behavior-v2` 套件，至少一正一负触发 case、targeted 和 regression 正文覆盖；全部候选通过，至少一正/负选择从 baseline `assertion_failed` 改善。若同时改正文，另需正文 targeted 改善。任一样例有 timeout、model_error、额度/工具等非断言故障也不视为质量改善。

目标名称/description 来自不可变 variant 的原安装 parser；每个 trigger case 可放最多 8 个宿主固定竞争名称/描述，不能由候选覆盖、不能读取 live 全目录。排序后的目录 SHA-256 与原 suite、case、variant/repetition 固定；提交、历史和审批重算所选名称对应的宿主断言与目录，而非接受 `passed` 自证。总计仍最多 16 cases / 3 repetitions / 96 variant results，模型调用沿原 job 的 `behavior` 维护额度，不新增 worker。它只证明这个固定宿主目录的首次选择，不证明所有真实任务或实际执行质量。

发布时重新校验完整 provenance、固定 generation/behavior identities 和当前 discovery policy；关闭 discovery 后旧提案仅可人工审批。正文-only 要求整个 frontmatter 字节不变；description 改动只可改变 YAML description span，其余元数据按 CRLF 归一化后逐字保持。纯格式变化、compatibility、license、metadata、名称/权限变更仍人工。原 v1 suites/reports 保持兼容和摘要不变；升级后的 Eino evaluator v2 identity 拦住未完成旧评测，不偷偷重用，已完成旧证据仍可人工审阅。

新技能、导入技能首次接管、大修改、执行脚本和 builtin overlay 保持人工审批。发布后新 run 使用新 snapshot，正在执行或恢复的旧 run 保持 pin，不热替换。

观察沿用明确的 operator-confirmed 质量反馈。自动提取的自然语言赞许可以展示为 quoted evidence，但不自动算高置信质量票。无质量反馈保留 `insufficient_evidence`；不能以“未报错”宣称进化有效。

自动发布与来源引用固定在同一个现有 PublicationTx 中提交。观察记录保存 generation/discovery/pattern revision/match/policy/evidence 摘要，不复制聊天或配方。`valid_at_publication` 仅表示当时核验，不是持续授权；已采用后撤销来源保留技能，活动观察下次扫描标记 `withdrawn`，不制造失败票。终态观察保留决定当时状态，不持续重写来源授权。重启时关闭 learning 和学习发布 gate，但保留整体 auto_patch，可继续按已保存限额观察/回滚；关闭整体 auto_patch 则暂停该既有 worker。

## 6. Go 接入与持久化

### 6.1 包与公开契约

- 新增 `go-harness/internal/learning`，负责 episode、extract、consolidate、match、consent 与引用有效性。职责不塞进事实 memory 或 metadata collector。
- 在 harness/SDK 新增显式 `SkillLearningOptions` / `Config.SkillLearning`，装配无工具模型与 worker。第一期不要求主 Agent 调新工具才能学习；当前 `skill_manage` 保持即时提案入口。
- 在 runtime 的真实输入接纳、终态与删除流程接线。要区分用户输入、后台 child 指令和通知 continuation；不能仅看到 `user_message` 字符串就当真人输入。
- Eino 增加受 maintenance broker 包装的 learning 模型适配器。不能用当前 `PostRunFactory` 发起后台未 join 的模型调用，其现有契约属于主 root 生命周期。
- evolution 增加 typed experience discovery 与 live evidence guard，复用候选、内容/行为审查、发布与观察。
- ACP 和本地 MANAGE 提供授权查询、取消、证据撤销与提案确认。工具返回的内容、模型的 actor 标签都不能取得这些管理权限。

### 6.2 维护队列需要显式扩展

设计前的 `skillmaintenance.Request` 强制要求合法 `SourceIdentity` 和 skill `Name`，generation worker 主要分发 generation/manual behavior。此次 P0 已完成兼容扩展，未匹配目标的 experience extraction 使用 workspace owner、摘要身份和独立幂等 namespace，不捏造技能目标。

建议进行兼容的 typed producer 扩展：

- 增加明确 `job_kind`，固定为 `experience_extract`、`experience_consolidate`、`skill_match`、`skill_generation`、`skill_behavior` 等宿主支持值。
- 明确 `owner_kind` / `owner_key`。学习作业绑定授权的 user/workspace scope；生成和评测仍绑定具体技能目标。未知目标保持 absent，不捏造技能名称或 generation input。
- 学习型作业的 skill target 为 absent，新增基于 `job_kind`、owner、request key 的幂等 namespace；保留旧技能作业的身份与唯一键。迁移不能把 nullable 目标当成通用发布权限。
- producer input、身份摘要、授权、readiness hook、admission hook、terminal hook 与结果 guard 按 job kind 分发。readiness 只允许 metadata-only 延后，不能替代付费准入或发布授权。由一个宿主 dispatcher 注册固定 handler，不能让 generation-only worker 误认新任务。
- 老作业迁移保留原请求身份与已使用额度，通过实际 generation/behavior 关联确定旧类型；未知关联 fail closed。不重置租约、已 dispatch 的 receipt、deadline 或消耗。
- extract/consolidate/match/generate/review/eval 的全部模型请求共享既有数据库级维护额度，另设学习子额度和调度优先级，避免低价值提取饿死评测和发布。
- 学习积压上限与 pending 技能提案容量分别约束。同步更新当前 `discoveryPending` 投影，不能让未匹配目标的 capture/extract 作业耗尽原来的提案名额，也不能让多个学习阶段规避总作业上限。
- 每个成功阶段分别固化，恢复复用已提交输出。不承诺外部模型调用 exactly-once；未提交调用在崩溃后可能重发，但保守计费、来源幂等和副作用隔离必须保持。

### 6.3 建议新增的数据投影

| 投影 | 主要内容 |
| --- | --- |
| `harness_learning_controls` | 每个授权 scope/chat 的 contribute/use、consent epoch、撤销状态 |
| `harness_learning_outbox` | 已提交来源 frontier、root/result version、独立消费位置、待处理原因 |
| `df_learning_episodes` | 多 root 的任务链、lineage、宿主 independence key |
| `df_learning_extractions` | 不可变输入引用、policy/model identity、阶段结果、关联 maintenance job |
| `df_learning_experiences` | 脱敏过程记录、scope、revision、状态、引用摘要 |
| `df_learning_patterns` / members | 归并版本、正反证据、episode 支持集合、匹配决定 |
| `df_learning_evidence_refs` | 输出到来源的依赖图，支持撤销、删除和过期失效 |

P1 已建立 controls、outbox、episodes、extractions、experiences，以及 host policy、accepted inputs、scan cursor 投影；P2 已建立 consolidation inputs/scan、不可变 snapshot/result、pattern revisions/members 与 matcher，并固定经验/提取/source digest 和 consent 依赖。已有 skill 提案另用 `df_learning_promotion_dependencies` 保存 job/match/pattern 依赖与撤销 fence，用 `df_learning_owned_versions` 区分独占学习候选和复用版本；同事务撤销未发布派生正文，保留费用与人工采用资产。当前控制只有 contribute，没有新增经验 use 注入；已发布技能沿用现有选择与启停。Evolution 统一 episode 消费、pending/daily/cooldown 边界，不另建发布锁。

提取幂等键包含 scope、session/episode、已提交 frontier、consent epoch、策略版本。游标使用事件序号/ID，不用原生 message 数量。游标、经验输出与当前阶段完成必须同事务提交。

### 6.4 每个昂贵/可见阶段的 guard

模型预留调用前、结果落库、generation 入队、candidate commit、review、approve/auto_patch 均检查：

1. 当前租约与 claim，未取消，worker 仍有 ownership。
2. contribute 仍有效，consent epoch、输入引用和 pattern revision 没有改变。
3. 用户、会话、workspace、agent 和 source 仍在授权范围。
4. 任务树状态、安全结果和 provenance 满足该阶段要求。
5. 目标 head/overlay、模型/策略/suite identity 与固定版本一致。

撤销不能取消已经发生的模型消费，但必须阻止失效结果继续生成、审批或发布。失效 pending 提案变为 stale，释放容量而不退还已消耗额度。

## 7. 配置、隐私与产品控制

### 7.1 配置契约

先验证实际运行配置路径。根 `config.yaml` 当前技能配置存在旧 `storage_path`、`discovery.enabled`、旧 auto_patch 等字段；当前 native decoder 对启用状态使用 KnownFields 校验，不能把这些旧键当作已启用自动发现。桌面也可能使用 user-data 下的配置，本设计不假定根配置就是运行配置。

通过明确迁移和 native schema 修复，不自动以新默认打开付费或发布权限。

下面区分现有配置和后续目标。当前已支持 extraction、独立 opt-in 的 consolidation/matching、workspace gate、模型/子额度、hints_only 与经验发现。实际原生键为 `skill_evolution.discovery.experience_patterns_enabled`；不要使用早期设计中的 `learning.experience_discovery_enabled`。可运行示例见实施记录。

| 拟新增设置 | 推荐初始策略 |
| --- | --- |
| `skill_evolution.learning.enabled` | 默认 false；明确启用经验学习 |
| `learning.extraction_model_name` / `consolidation_model_name` | 可继承已授权模型或单独指定；启动验证路由 |
| `learning.consolidation_enabled` | 已支持，默认 false；独立授权同会话后台语义比较，不增加生成/发布权限 |
| `learning.matching_enabled` / `matching_model_name` | 已支持，默认关闭；比较当前授权技能，分类本身不生成或发布 |
| `learning.cross_session_enabled` | 已支持，默认 false；还需各会话 shared consent，只归并同 profile/workspace，不授予单会话 ACP 跨会话读取 |
| `learning.idle_seconds` | 建议 300；与业务终态判断分开 |
| `learning.max_input_bytes` / `max_output_tokens` | 有界输入和输出，超限分段且保留来源 |
| `learning.external_context_policy` | P1 仅支持 hints_only；网络/MCP/未知工具只保留参数形状等元数据，不发送结果正文，不作为用户纠正或学习指令 |
| `learning.max_daily_model_calls` / `max_daily_tokens` | 共享总额度内的学习子额度，不能扩大总预算 |
| `discovery.experience_patterns_enabled` | 已支持，默认 false；同时启用 generation/extract/consolidate/match 后才能为已有目标自动排队待审候选 |
| `discovery.experience_review_drafts_enabled` | 已支持，默认 false；完整自动链下允许一个有核验本地执行来源的任务自动起草 SKILL.md，固定为人工审核，不降低自动发布门槛 |
| `auto_patch.experience_patterns_enabled` | 已支持，默认 false；另需 mode=auto_patch、当前 experience discovery 和可信 behavior suite；正文需定向改善，description 另需可信 v2 正负首次选择改善，同改须分别改善 |
| `discovery.experience_new_skills_enabled` | 已支持，默认 false；还需 experience discovery、完整目录和明确非内置 new_skill_source_id，仅为精确 new_capability 建立单文件人工待审候选 |

learning extraction 可以先单独启用并仅积累经验；自动生成必须同时满足既有 evolution/generation/source 授权和新 experience discovery gate。学习候选默认人工发布；P3 独立发布开关不因旧 auto_patch 已开启而自动生效。可信 v2 trigger suite 不是新授权，必须原独立发布 gate 已开才可准入 description 修改；新技能和其他 frontmatter 变更始终人工审核。

未知配置一律不能悄悄忽略。Go schema、save/validate/launch、SDK、桌面表单同步接线，并做迁移和功能探测。现有 `review` / `auto_patch` 枚举不增加模糊的 `auto` 模式。

### 7.2 用户控制与隔离

- 全局开关和单会话“贡献学习”分开；“使用已发布的学习技能”与“贡献新经验”分开，映射到 source/snapshot 选择，不绕开已有 skill 启停。
- 关闭 contribute 递增 consent epoch，取消相关自动作业，阻止旧输出前进；不会凭关闭动作悄悄删除人工采用的既有技能。
- 入队前视图脱敏覆盖文本、工具参数、关键结果、附件元数据和 provenance 文本。正式配置工具的 JSON content 封装先做有界结构化解码和敏感 key/value 过滤，嵌套/多重转义不能仅靠原文本正则；所有纯文本或解码字符串先移除 Unicode format/非布局控制字符并 trim，再匹配转义赋值、凭据前缀及路径，避免先过滤再去隐藏字符留下可读秘密。超出结构限额 fail-closed。生成内容再做秘密扫描。这不是防泄漏的绝对保证，高敏感会话可选择完全不贡献。
- 来源删除/forget 必须清理派生的未发布经验、摘要和待审正文，不只删除引用；作业和审批 guard 同时失效。缓存不得让已忘记内容重新出现。
- 人工采用的技能是独立用户资产。删除聊天与“删除聊天及学习产物”语义明确区分；若要求清除正式技能，走现有禁用/删除/tombstone/保留策略，不静默改变正在运行的版本 pin。
- 默认不跨用户、项目或 agent 分享。跨项目沉淀用户级通用技能、团队共享、版本控制导出分别授权。
- 能学“如何申请发布审批”，不能学“用户上次批过，所以以后免审批”。任何记忆和 skill 都不能修改宿主权限判断。

### 7.3 UI 与可观测性

在已有技能、生成、评审页面增加“学习来源”视图，不要求先建一套独立知识库 UI。显示：

- 本次沉淀了什么、对应哪些任务/纠正、实际验证是什么。
- 为什么认为属于这个技能，为什么是更新而不是新建。
- 当前是收集、证据不足、待整合、待生成、待审、已发布还是 stale。
- 旧版与候选的最小 diff、评测覆盖/未覆盖项、维护用量与跳过原因。
- 忽略、撤销来源、选其他目标、采用、禁用、回滚。

未产生值得处理的变化时不逐轮打扰用户。只在可操作提案、自动发布、回滚或需要用户处理的失败时提示；后台不可把未完成工作标成“技能已进化”。

当前技能中心“学习来源”已在源码接通宿主状态、原生会话贡献/共享确认、显式新目标、学习作业 CAS 取消，以及每页 20 条经验/pattern/match/job 历史；推断归因、固定 head、旧版实际读取和未重新核验来源分开显示。共享范围仅走认证本机 MANAGE，并要求 host gate 与当前会话共享授权。服务/会话/local view 变化撤销旧界面 action authority；贡献设置或人工控制发送前清掉派生详情，保留生成草稿。原 control/cancel/episode.start 未知写结果不直接重发，当前状态只读核对不冒充回执。新增单条忽略/恢复及从新鲜技能目录缓存显式裁决目标，独立控制版本与确认按钮；它们及显式创建重试可在原服务目录重放完整原请求查询原 key 回执，晚到旧结果不改新选择，回执不声称当前状态。自动发现和提案 diff 复用既有页面。仅有隔离状态/请求守卫定向测试，未构建 GPUI app 或视觉验收。

直接来源检查已通过可信 SDK `SkillLearningSource` / 认证 MANAGE `skill.learning.source.get` 接入经验和归并详情，不扩展 ACP 或模型工具。请求固定 kind/id/current revision、session/workspace scope 和一个成员 experience_id；拒绝旧 revision、非成员及不满足共享授权的读取，不从历史 match 静默跳转到更新后的 pattern。每次只检查一个成员的原 extraction、其全部捕获来源、完整结果与被引用 reference，单响应最多 32 条引用 / 512 KiB，不整条归并展开全部任务。服务在 SQLite 同一事务内核验 contribute/epoch、真实 accepted input、原 root 的固定委派/continuation 成员、事件 frontier、终态/settled root、回执版本/参数摘要、真实 skill read 和原提取 job/input/result identity，然后只返回经验实际引用的原脱敏视图。可以返回被引用子任务的本地脱敏回执/实际读取身份，但不返回其私有 assistant/native result/notification 正文；外部/MCP/未知工具仍 hints_only，不返回原始回执、错误正文、隐藏推理或附件。`checked_at_read` 只表示该成员此次读取时核验，`quality=unassessed`；不认证匹配、归并 shortlist、适用时间窗口、独立成功或技能因果贡献。关闭付费 learning/model stage 后会话私有源仍可查看，贡献撤销则清除源；跨会话源仍需原独立 host gate 和每会话共享授权。界面只保存当前一份预格式化来源详情；刷新、成员/行/会话/范围/服务变化、贡献或人工来源操作、失败与晚到响应均撤销缓存，不建立写操作回执或后台轮询。

## 8. 一次多轮对话会怎样工作

以下是设计示例，不是已经完成的真实执行记录。

1. 用户要求检查项目 JSON 配置。Agent 使用 `project-config-audit`，一次 PowerShell 读取/解析失败。
2. 用户纠正“需要读取整个文件，不要逐行解析；还要验证字段”。下一轮采用 `Get-Content -Raw` 与解析、字段验证，实际回执成功。
3. 用户认可结果。系统关联失败、用户纠正、成功回执和实际技能版本，记录一个 `correction_chain` episode。三个 prompt/root 不算三个成功样本。
4. 单任务自动待审草案关闭且无第二个独立 episode 时，只保留经验；启用后自动起草待审 patch，用户仅审核生成结果，不需要求“以后固化到技能”。
5. 同会话或后续已授权会话出现独立配置检查任务，验证同一修复后自动聚合。达到配置门槛且 episode 尚未被同一 target/version 提案消费时，生成最小更新提案；已生成的单任务草案不退款、不补票，也不自动升级为可自动发布。
6. 若仅开启 review，用户采用后新 run 生效；若已授权 auto_patch 且技能、差异、证据和可信评测全部合格，自动发布并进入观察，否则仍待人工处理。

用户得到的体验是“纠正无需重复填写表单，长期积累的方法能变成技能”，不是“每次聊完系统随意改文件”。

## 9. 分期落地与验收

### P0：配置与契约基线

确认有效配置路径，迁移旧字段；固定来源、actor、终态和删除契约；扩展 typed maintenance producer。此阶段不打开自动学习，不影响原有 generation/behavior 恢复和计费。

### P1：经验捕获，shadow 模式

完成 durable outbox、consent、episode、多轮引用和无工具提取。仅输出经验与“会建议改什么”，不自动创建 generation/proposal。保留现有 memory 流程。使用带人工标注的真实代表性样本，测量纠正识别、验证识别、scope 与 episode 误判。

当前工程落点：捕获、经验查询和独立 opt-in 的归并/授权匹配，以及 workspace 双重授权共享归并与 lineage 已接通；episode 保守保持同会话单任务链，新独立计数仍需真实 host 边界，模型不得自行拆分独立票。启用单任务自动待审草案后，不要求用户手工建立第二个 episode 才能获得候选，但独立证据不足的候选永远不自动发布。每次最多 8 轮、4 条经验，后续批次带先前上下文；单轮过大时明确跳过，尚未实现单轮内分段。归并只比较一个 seed 与最多 8 个 pattern，宿主维护版本和支持集合，不由模型重写配方。Matcher 优先已核验的读取，再以名称/描述词法召回当前 runtime-visible 技能，最多 8 个脱敏正文；宿主固定 source/head/overlay，模型只能返回 ID 和关系分类。只启用这些 stage 仍是 shadow；增加经验 discovery opt-in 才进入 P2 待审提案。确定性协议/事务验证通过不等于真实模型提取、归并或匹配质量通过。

### P2：自动待审提案

完成语义整合、已有技能匹配和 experience discovery；复用 generator/review/eval；接入来源和 diff UI。发布仍 review。先验证提案可采用率、重复提案率和维护成本，再调整 episode 门槛。

当前已有目标与独立 opt-in 新能力桥接已接通并通过定向 fixture/loopback：严格引用、独立门槛、共享消费和容量、可读名称/宿主来源/冲突裁决、单 pattern 创建去重、instruction-only、手工发布、重启复用及传递撤销。单条忽略/恢复、人工已有目标重新裁决、显式新技能创建重试和原 key 回执已定向验证，可信 SDK/MANAGE 与桌面缓存确认控制源码接通；桌面构建/视觉验收及真实质量验收仍未完成。

### P3：受限自动 patch

仅对明确授权项目、managed 技能和宿主可信 suite 开放。已实现独立学习发布 gate、targeted assertion 改善、description 正负元数据首次选择改善、完整 live provenance 和同事务观察引用，复用原观察/回滚及共享发布配额。无可信 v2 trigger suite 的 description 和其他 frontmatter 变更保持人工；新技能、导入技能首次接管和脚本也仍人工审批。当前 fixture/loopback/SDK 配置与隔离桌面投影验证不构成真实模型质量或桌面可用性验收。

### 必须通过的验收

| 场景 | 验收条件 |
| --- | --- |
| 一个任务反复纠正 | 多个 root 聚成一个 episode，不能伪造独立票数 |
| 两个独立任务采用同一修复 | 在授权和阈值满足后只生成一个目标提案 |
| 相同工具形状、不同目标 | 不误合并成一个技能 |
| 纯偏好、一次性需求、临时授权 | 不进入过程技能，不新增权限 |
| user/background/notification 混合历史 | child 指令和工具文本不能伪装真人纠正 |
| 网络、额度、权限、取消 | 不归因到技能质量失败 |
| 安全注入与敏感值 | 网页指令不能改变学习策略；正文和 metadata 无明文秘密 |
| 已有相近技能、多个候选目标 | 优先更新正确 source；歧义不自动写错目标 |
| 重启、重复 outbox、过期 worker | 输出幂等，旧 claim 不能提交，已完成阶段不重复调用 |
| 配额耗尽和队列背压 | 有界延后/跳过、无无限重试、不创建新业务 root、不影响主回答 |
| 关闭 contribute、删除来源 | 旧输出不再前进，派生待审文本清理，不能恢复已忘记内容 |
| source/head/suite/策略改变 | stale 或重新裁决，不隐式 rebase 和审批 |
| 行为/调用评测 | 旧版/无技能基线真实，期望答案不泄漏，原工具副作用不重放 |
| 发布与版本 pin | 新 run 用新版，旧 run/checkpoint 保持原版 |
| 自动观察与回滚 | 推断反馈不当质量票，无反馈显示 insufficient_evidence，回滚幂等 |
| 设置和桌面 | schema/save/validate/launch 一致，旧键不误开付费，新 UI 有状态来源 |

建议产品指标包括每个独立 episode 学习成本、用户纠正识别准确率、候选正确目标率、人工采用/驳回比例、近重复技能数量、无关触发率、定向改善与回归比例。不能只把“新增了多少 skill”作为进化效果。

本设计文档不代表真实模型质量或桌面交互已经验证。实施后仍需确定性单元测试、SDK/ACP 集成测试、崩溃与撤销测试，以及明确授权下的真实模型标注评测。

## 10. 官方资料

- [O1 Codex Memories](https://learn.chatgpt.com/docs/customization/memories)
- [O2 Build Skills](https://learn.chatgpt.com/docs/build-skills)
- [O3 AGENTS.md](https://learn.chatgpt.com/docs/agent-configuration/agents-md)
- [C1 Claude Code Memory](https://code.claude.com/docs/en/memory)
- [C2 Claude Code Skills](https://code.claude.com/docs/en/skills)
- [C3 Claude Code Hooks](https://code.claude.com/docs/en/hooks)
- [D1 DeerFlow SkillEvolutionConfig](https://github.com/bytedance/deer-flow/blob/89c9f0871432e169a67b7db21984c8d2c3486c21/backend/packages/harness/deerflow/config/skill_evolution_config.py)
- [D2 DeerFlow 主 Agent 技能自进化提示词](https://github.com/bytedance/deer-flow/blob/89c9f0871432e169a67b7db21984c8d2c3486c21/backend/packages/harness/deerflow/agents/lead_agent/prompt.py#L278)
- [D3 DeerFlow skill_manage](https://github.com/bytedance/deer-flow/blob/89c9f0871432e169a67b7db21984c8d2c3486c21/backend/packages/harness/deerflow/tools/skill_manage_tool.py)
- [D4 DeerFlow 工具装配](https://github.com/bytedance/deer-flow/blob/89c9f0871432e169a67b7db21984c8d2c3486c21/backend/packages/harness/deerflow/tools/tools.py#L189)
- [D5 DeerFlow MemoryMiddleware](https://github.com/bytedance/deer-flow/blob/89c9f0871432e169a67b7db21984c8d2c3486c21/backend/packages/harness/deerflow/agents/middlewares/memory_middleware.py)
- [D6 DeerFlow DeerMem 队列](https://github.com/bytedance/deer-flow/blob/89c9f0871432e169a67b7db21984c8d2c3486c21/backend/packages/harness/deerflow/agents/memory/backends/deermem/deermem/core/queue.py)
- [D7 DeerFlow skill-creator](https://github.com/bytedance/deer-flow/blob/89c9f0871432e169a67b7db21984c8d2c3486c21/skills/public/skill-creator/SKILL.md)
