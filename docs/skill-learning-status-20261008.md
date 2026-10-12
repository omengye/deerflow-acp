# 会话经验学习实施记录

日期：2026-10-08。
最近验证：2026-10-09。
依据：[设计方案](skill-learning-design-20261008.md)。

2026-10-09 MVP 收口：按用户最新要求，已验证的自动经验沉淀、候选生成、人工审核和已有技能受限自动采用满足本次 MVP 交付。停止继续重复测试及低收益修复；下列历史“目标继续进行”描述属于当时的完整设计验收范围，不表示 MVP 仍被这些事项阻塞。最终功能范围、启用方式和剩余项见 [MVP 交付说明](skill-learning-mvp-20261009.md)。
当前状态：P0、P1 shadow，以及 P2 的有界语义归并、授权 skill 匹配、工作区显式共享/lineage、已有 skill 改进与独立 opt-in 新能力 `experience_pattern` 自动待审提案链路已接通。P3 已接入默认关闭的学习专用受限 auto_patch、正文定向改善、description 正负元数据选择评测、来源撤销标识与原观察/回滚链路。单条建议忽略/恢复、人工已有目标裁决、显式新技能创建重试与原 key 回执已实现，桌面对应控制和原学习来源/history、逐会话贡献/共享、新发布开关和触发报告已接通源码及隔离逻辑测试；桌面构建/视觉验收和真实语义/触发质量验收未完成，目标继续进行。

## 1. 当前能做什么

显式开启 `skill_evolution.learning.enabled` 后，真实用户多轮任务在终态、空闲且任务树安全结束后，自动进入持久队列，由无工具 Eino 模型提取有界、脱敏的过程经验。主对话不等待学习模型，不重放原工具，也不使用业务 root 的预算或临时权限。

另行开启 `learning.consolidation_enabled` 后，已提交经验自动进入 typed consolidation 作业。宿主做有界词法召回，无工具模型判断等价、不同、冲突或歧义；宿主维护不可变 pattern revision 与来源成员。同会话同一 episode 的多批次/多 root 只算一个支持，显式的新 episode 才能增加独立计数。默认只归并本会话；再启用 `learning.cross_session_enabled` 且每个来源会话显式同意 `workspace_shared` 后，才归并同一规范化工作区内的共享经验。普通会话 pattern/match 查询不返回共享文本，跨会话历史仅提供给可信 SDK 宿主和认证 MANAGE。

同一任务的早期未验证经验可以在后续普通纠正中自动解除阻塞。宿主要求等价归并、同一真实 session/episode/epoch/lineage，后续经验明确引用早期全部真人输入、更晚真人输入及更晚 turn 的完整本地执行回执，保存有界、可重建的原成员到验证成员 link。原历史、outcome、来源和撤销边保留；一个任务仍只计一票。旧 receipt、其他会话的同源复制、不同 episode、较晚新问题、冲突和歧义不能覆盖。旧 job/result 原语义不自动升级，后来真实输入的新 revision 才应用新策略。

同一工作区、相同完整初始 accepted input 保守视为同一任务 lineage；不同初始输入的独立会话可形成不同任务。宿主可在新会话尚无 run/episode 时显式关联已授权来源 episode，以覆盖改写输入的复制任务。恢复沿用原 episode，显式 `episode.start` 建立新独立任务。当前没有会话复制/fork 功能；将来增加此功能必须在首次输入前调用 lineage 接口，不能只靠模型或新 session ID 声称独立。相同初始输入的真实独立任务可能被少计，此时需显式新任务边界；未声明的改写复制无法保证识别。

在提取和归并都已开启的前提下，再单独开启 `learning.matching_enabled`，无工具 Eino 模型比较当前会话可见技能。实际读过的来源优先，然后按名称/描述做词法召回；最多比较 8 个有界、脱敏的 `SKILL.md`。宿主固定来源身份、scope、完整 publication head 和 overlay，模型不能指定路径或新来源。匹配结果区分 `body_gap`、`trigger_mismatch`、`already_covered`、`new_capability`、`irrelevant`、`ambiguous`；matching 本身不创建 generation/proposal 或发布。一个独立 episode 默认只得到 shadow 分类；另启单任务自动待审草案可由 discovery 起草人工审核提案，但不满足自动发布独立门槛。

在任何 generation 预留之前，如果当前未受人工控制的 pattern 来源及原模型/访问身份仍有效，正式技能目录/head 改变可自动建立全新匹配评估，不改写或复活原 paid job/result。每个 pattern 生命周期最多两次额外评估、一分钟冷却、首次 match 起三十天窗口，仍使用原共享队列、学习子预算和 paid admission；旧记录保留为 history，新模型可判断已经覆盖而停止起草。取消、忽略/恢复、人工裁决、来源撤销/损坏、配置变化和一般失败不触发；已有生成预留不递归重做。新的真人任务经正常归并形成新 revision 后，继续走原自动匹配输入，不要求人工重新匹配。

提取结果固定为 `origin=model_extracted`、`quality=unassessed`。引用存在与执行回执完整，并不证明方法有效或证明某个技能导致成功。不会调用 `SubmitFeedback` 伪造 operator 反馈。

另行开启 generation 和 `discovery.experience_patterns_enabled` 后，默认要求至少达到配置门槛的独立 episode 有完整、未截断、被经验真实引用的本地完成回执，且当前 pattern 无冲突/歧义、匹配精确授权的已有启用 skill，才自动生成 instruction-only 待审提案。另启 `discovery.experience_review_drafts_enabled` 可从一个满足其余相同守卫的观察任务自动起草，仅供人工审核，不降低自动发布门槛。已有目标只允许 `body_gap` / `trigger_mismatch`；`new_capability` 需下面独立的新能力授权。候选仅可修改 `SKILL.md`，已有支持文件必须逐字节保持。仍走既有结构/安全/内容审查和适用的宿主行为 suite，没有 suite 则 `not_run`。这些学习候选默认人工发布，外层既有 auto_patch 不隐式授予学习发布权。

再独立启用 `auto_patch.experience_patterns_enabled`，并保持 `mode=auto_patch`、完整学习/当前 experience discovery 和可信宿主 behavior suite，已有启用 managed 技能的小型补丁才可自动发布。正文修改至少一个 targeted 比较必须从 baseline `assertion_failed` 改善为 candidate passed，另需 regression case、无回归、无 timeout/model/工具/额度等操作性评测故障；正文-only 的整个 YAML frontmatter 必须保持不变（仅归一化 CRLF）。

description 修改另需可信 `skill-behavior-v2` 正负触发 suite。它保留 targeted/regression 正文覆盖，新增至少一正一负 metadata-only 首次 Skill 工具选择；所有候选 case 通过、至少一个触发断言真实改善，若同改正文还须正文 targeted 改善。未匹配 v2、无选择改善或有操作故障都不自动发布。只 description YAML span 可以变化，其余名称/compatibility/license/metadata 必须字节不变；纯格式改写、其他 frontmatter、新技能、首次导入接管、builtin overlay、支持文件、大改动仍人工。配额/目标观察锁/原 worker 不变，没有额外模型任务层。

触发模型只看名称/描述及宿主 prompt，使用固定 Eino 原生 Skill middleware prompt/reminder/schema。期望正负标签、case ID/kind 和 assertions 不发送；不加载正文、不创建 workspace、不执行任何工具、不生成 runtime read 或质量票。可有最多 8 个固定宿主竞争描述，目录按名称排序并固化 SHA-256，目标 metadata 经原安装 parser 读取。历史/审批从不可变 variant 与宿主 oracle 重算选择 check、目录和报告。它只证明当前固定 suite，不证明 live 全目录或真实任务准确率。v1 suite/report 向后兼容；Eino evaluator identity 升为 v2，未完成旧任务被 configuration fence，已完成旧证据可人工审阅，自动发布仍要当前 exact config。

发布前在原审批事务中重建学习来源和固定 generation/behavior/discovery 配置；发布与观察来源引用原子提交。`learning_source` 只存 generation/discovery/pattern revision/match/policy/evidence 摘要，`valid_at_publication` 不是持续授权。发布后撤销学习来源保留已采用资产，活动观察下次扫描标记 `withdrawn`，不制造失败票；终态观察保留决定当时状态，不声称持续核验。即使重启时 learning/学习发布 gate 关闭，只要整体 auto_patch 保留，仍按原保存观察限额处理明确的 operator-confirmed 质量反馈与回滚。无反馈仍为 `insufficient_evidence`，整体 auto_patch 关闭则暂停原 worker。

还需单独开启 `discovery.experience_new_skills_enabled` 并明确非内置、generation-authorized `new_skill_source_id`，才能对 `new_capability` 自动起草新技能。它沿用独立 episode 门槛；另启单任务自动待审草案可用一个合格观察任务起草，新技能在两种情况下均须人工审核。Matcher v2 只给可读名称建议，最多 56 ASCII 字节、至少两个含字母的 kebab-case 段，不接受路径/纯数字段/长摘要；旧或自定义 matcher 无名称的结果保持 shadow。宿主必须有完整匹配目录和无限定 Names selection，核对源在当前会话 workspace/global 可见，拒绝已安装、禁用、tombstone 和未完成同名提案。不选默认目录、不改名规避冲突、不把 builtin overlay 授权用于创建。新候选只可含 `SKILL.md`，同一 pattern 生命周期仅一次创建预留，后续 revision、换名/来源或失败/取消/驳回不退款重试。结构合格不证明名称、能力缺失或配方质量语义正确。

审批前重建实时来源，核验提取、归并完整 shortlist、match result、当前 pattern revision、所有来源 consent/workspace、host access/catalog/head/overlay。关闭 discovery 停止新工作，但在 learning source 仍有效时可人工采用此前完成的提案；关闭 learning、模型/access 改变或 receipt/revision 改变则拒绝。撤销贡献/共享或删除来源清理未发布提案正文、生成/行为派生文本与独占学习 staged bytes，不退费用，不删除已采用或独立保留的不可变版本。

模型 Match 固定为 `origin=model_matched`、`quality=unassessed`。语义匹配不会补造 skill read，旧版读取不会被算作当前 head 的读取。当前将同 pattern 各提取视图里的真实读取作为召回线索，并不宣称被引用的 skill 对成功存在因果贡献。

本轮增加单条 pattern 忽略/恢复和人工目标裁决。控制版本独立于 pattern revision 和 consent epoch，首次为 0，每次成功修改递增；同时要求精确当前 pattern revision。忽略持续覆盖该 pattern 之后的修订，不删除原经验，也不产生质量驳回票；取消相关匹配、撤销并清理未发布派生提案，保留费用、已消费 episode、单 pattern 新技能创建预留和已采用技能。恢复不重发旧匹配、不退消费，不自动重新启用旧人工裁决。

显式创建重试独立于自动扫描。先用可信 SDK/MANAGE prepare 核对当前 pattern revision / review CAS、原 generation 终态及提案版本、原完整来源与精确不存在的目标，然后由用户确认带请求键的完整条件 enqueue。只接受 `failed` / `cancelled`，或 `completed` 且提案 `failed` / `rejected`；仍在执行、待审核、已发布或 `stale` 的原任务拒绝。原 pattern 修订、名称和来源不能更换，不隐式 rebase。自动 lifetime 创建预留永久保留，每个 pattern 另有最多 3 次非退款人工重试额度，同一原作业最多一个后继。重试复用原来的 episode 消费 identity 与完整 evidence，不插入新独立票或删除/转移旧消费；重新占用共享每日/待处理提案、cooldown 和原 maintenance 调度/模型额度。新 generation/discovery 与人工授权、CAS、额度同事务提交，旧输入和候选不改写。

人工重试授权固定原请求、操作会话、原 generation/proposal 版本、目标、原 evidence reservation、ordinal、新 generation/discovery 输入摘要，生成 snapshot 另绑定 authorization digest。结果由固定授权重建，重新哈希单条回执不能改归因。原 key replay 在学习/生成/discovery 关闭、后继已完成或 pattern 之后忽略时可返回历史 ID，但不恢复被清理的 producer，不新入队、不更新当前 review；贡献/共享撤销或删来源会清除授权和回执，保留 digest-only 重试消费、自动创建预留及费用。workspace 非成员操作会话也进入传递来源撤销。Paid call、输出、review 和最终人工 approve 重核完整来源/匹配/目标、原预留及当前 review；仍只允许 `SKILL.md`，新技能永不自动发布。

人工裁决只允许当前 runtime 授权目录中的已有启用技能，明确选择 `body_gap`、`trigger_mismatch` 或 `already_covered`。目标可以在原模型 shortlist 之外，但仍受完整来源身份、scope、Names/工具访问策略、head/overlay、目录与输入字节边界约束；模型原匹配不改写，新建 `origin=host_adjudicated`、`quality=unassessed` 的独立记录。核验当前归并和全部 consolidation shortlist/执行来源，无冲突、歧义或未解决证据；一个 episode 默认仅保存 shadow 人工分类，另行启用单任务草案后可由系统起草人工待审提案，但裁决本身不授予生成权或补造独立票。人工分类本身无模型调用，无新技能/名称/来源 retry grant。后续仍走原独立 discovery/generation 开关、共同配额、冷却、target busy 和 episode/target/version 消费，同事务撤销旧候选；人工裁决来源固化 `host_adjudicated`，自动补丁守卫明确拒绝，即使可信行为评测通过仍须人工发布。

可信 SDK/MANAGE 明确区分 `scope=session` 与 `scope=workspace`。共享范围检查操作会话及全部来源会话的同工作区贡献/共享授权；非成员操作会话的人工裁决也建立可撤销依赖，撤销该操作会话时清除传递派生 pattern/提案但保留其他会话原始经验。单会话 ACP 和模型工具不增加这些管理权限。来源删除会级联清除人工裁决正文及其回执，不能凭旧请求复活内容。

桌面技能中心新增“学习来源”，读取宿主运行状态、当前 DeerFlow 原生会话贡献设置、过程经验、归并、匹配和学习作业。归并/匹配另有“此聊天私有”和“同工作区已授权共享”范围，后者必须有运行 host cross-session gate 和当前会话共享同意，始终使用认证本机 MANAGE；不是 owner-bound ACP 权限扩展。仅缓存一页 20 条及一个选中详情，预先校验并格式化，不在 render 内做 I/O。贡献/共享变更、显式新目标和作业取消均需确认并使用已观察 epoch/状态版本；没有自动保存/开启任何配置。

历史显示模型提取/匹配、质量未评估、`source_state=not_revalidated`、独立支持/本地回执数量和冲突；固定匹配 head 与实际读取版本分开。会话或同 native session 的 local view 切换、scope/pane 切换及服务 generation 变化会使旧响应不能更新新选择。贡献变更在发送前保守清除相关 discovery/generation/proposal/behavior 缓存；生成表单草稿保留。自动发现与待审入口复用既有来源 pins、生成作业、diff 和审批，不从历史匹配伪造关联提案。

来源正文检查已新增可信 SDK `SkillLearningSource` / MANAGE `skill.learning.source.get`，不新增 ACP 扩展或模型工具。经验详情或归并详情可显式选择一个实际成员读取，固定 current revision、完整 kind/id/scope/experience_id；服务同事务重建该成员原 extraction 的最多 8 个 captured turn、真实输入/事件 frontier/settled root/回执/skill read，以及保存的提取 job/input/result identity，只返回该经验实际引用的脱敏视图，最多 32 条 / 512 KiB。真人输入、主代理回答、本地完成/失败等回执和实际版本读取分开显示；网络/MCP/未知工具仅参数形状提示，不返回外部结果正文、原始错误、隐藏推理或子代理正文。`checked_at_read` 是该成员此次读取时核验，不保证缓存持续有效，不重新裁决整条归并、匹配质量、成功次数或因果贡献。关闭付费 learning 后仍可读取 retained 私有来源，跨会话源仍受 host gate 和各会话共享/epoch/workspace 检查；withdraw/forget 清除源，无保存正文副本、入队、消费或质量票。桌面最多一份已格式化详情，行/成员/分页/会话/服务/范围变动与来源写操作在发送时清缓存；迟到响应不得回填新选择，失败不生成 mutation retry。

原 contribute/sharing、cancel、episode.start 写操作仍没有 request-key 回执，因此未知网络结果不直接重发。保留原请求/服务目录，阻止新修改；control/cancel 可读原会话当前状态，但不将其当作原操作回执或自动解锁。`episode.start` 没有当前边界查询，不能假装其他列表能确认是否执行。用户可二次确认结束界面待确认状态，明确不重发、不撤销、也不证明原请求失败，然后刷新当前会话条件。新增忽略/人工裁决单独保存有界原始结果回执；同会话/请求键必须绑定完整原请求摘要，只有完整原请求可在原服务目录重放核对。回执先重新检查来源读取 scope，再返回原结果，不等于当前状态、不重新执行旧修改；之后 pattern revision、人工版本或 learning host 改变不篡改历史回执。桌面选中归并、已观察 review version 和新鲜 catalog target/head 缓存共同决定新裁决按钮，未知结果保留原服务/key并阻断新写；切换会话后晚到结果不更新新选择。

全部示例和打包默认配置保持关闭。没有改写用户的有效配置、个人数据库或已发布技能，没有打开真实付费模型调用。

## 2. 已接通的工程边界

| 范围 | 当前实现 |
| --- | --- |
| 维护队列 | typed kind/owner、无目标学习作业、namespaced 幂等键；旧 generation/manual behavior 根据真实关联迁移，保留租约、deadline、receipt 和消费 |
| 预算与调度 | extract/consolidate 与生成/评审/行为调用共享数据库维护预算；学习调用/Token 子额度同时固定到 snapshot，按当前与固定额度的较小值准入；skill 作业优先，学习 pending 上限 20，总作业上限 100 |
| 真实输入 | 仅 `runtime.BeginRun` 的 accepted input 进入 capture；child 指令、通知或名称为 `user_message` 的普通事件不能建立真人来源 |
| 终态事务 | SQLite trigger 固化来源 frontier、终态、consent epoch、uncertain 标志；outbox 不复制原聊天、工具输出或错误正文 |
| 多轮 episode | 同会话保守归为一个任务；显式 SDK/管理操作开始新 episode，模型不能拆出独立成功次数；后续新任务不会阻塞已结束的旧 episode |
| 有界积压 | 每会话最多 128 条 pending 来源；每批最多 8 轮，后续批次带先前上下文；事件/receipt 原始读取有字节上限，超大单轮跳过 |
| 来源视图 | 不发送隐藏推理、附件二进制或 child assistant 文本；只提取带 lead AgentName 的回答；本地工具允许脱敏参数/结果，网络、MCP 和未知工具仅参数形状提示 |
| 委派观察 | accepted foreground root 的后台/通知物理 run 同事务核验；仅实际读取和脱敏回执，八个子 run 总上限；不引入真人输入、独立票、质量反馈或私有对话正文；来源/审批实时重建固定后代集合 |
| 引用 guard | 调用预留前与结果提交前重建脱敏视图，校验真实输入、frontier、终态、receipt 版本/参数摘要、skill read、任务树与授权 epoch |
| 恢复幂等 | snapshot/job/outbox 原子入队；结果/来源 ACK 原子提交；丢失作业 ACK 后复用已保存结果，并校验结果摘要和经验内容，不再次调用模型 |
| 有界语义归并 | 同会话最多 128 个 pattern；词法召回后最多 8 个候选；每个 pattern 最多 32 个经验成员，单事务最多核验 32 个不同 source turn；超限或失效来源明确跳过，active 来源延后，不以删减验证换取通过 |
| 跨会话授权 | 独立 host gate + 每会话 sharing consent/epoch + 同一规范化 workspace；共享空间最多 128 个 pattern，按 workspace 串行归并；private/shared 目录互不召回，跨工作区和单会话 ACP 查询不授予共享文本访问 |
| Lineage | 额外持久独立键表，不破坏原 episode UNIQUE 键；旧来源一次性迁移且不重建已删除键；初始精确输入摘要保守去重、宿主 linked copy 继承键、显式 task boundary 新键 |
| 传递撤销 | 保存 seed 和全部 shortlist 的 session/epoch、pattern 依赖；撤销同事务递归清理所有依赖 revision、归并 snapshot/result、matcher payload/result，连未选中的比较也清理；保留其他会话原始经验、已发生费用和正式技能 |
| Pattern 状态 | 独立 supports/observed episode 由宿主计数；未解决、歧义与双向冲突保持阻断；保留已提取代表经验，不由归并模型创造步骤或验证结果 |
| 归并来源 | snapshot 固定经验/提取摘要、consent epoch、任务独立键与 pattern revision；预留调用和输出提交重新核验；来源缓存只在相同 SQLite transaction 内复用 |
| 授权匹配 | startup host selection 与已配置 source identity 固化到 `MatchAccess`；事务内复现 workspace/global、disabled shadow、同 scope 重名失败和精确 Names 策略，不使用管理 List 作为授权 |
| 匹配边界 | 可见 inventory 上限 128、跨来源最多扫描 256 条且 metadata 总量有界，shortlist 上限 8；只读不可变 SQL `SKILL.md`，单文档 32 KiB，不扫描脚本、支持文件或源目录；无词法命中保留稳定 fallback |
| 匹配 guard | queue、调用预留和输出事务核对 pattern/current revision、实时来源/consent、会话 workspace、模型/access identity、catalog revision、完整 head/overlay 和重建脱敏视图；来源缓存不跨事务 |
| 匹配恢复 | 同 pattern revision 单作业，snapshot/job/input 原子入队，result/ACK 原子提交；保存结果损坏拒绝并避免重复调用；取消和撤销清除 unpublished matcher 正文，消费不退款 |
| 经验提案准入 | 独立 discovery opt-in；每 pass 最多 8 个完成的当前 match；精确授权目标、无未解决/冲突/歧义来源和完整本地回执；默认至少 repeat_threshold 个独立未消费 episode，另启单任务草案允许至少 1 个且固定人工审核；hint 仅用于便宜的 head/quota 检查，不是 authority |
| 新能力创建 | 再独立 opt-in + 明确非内置可见来源；完整目录/无限定 Names、可读 name 建议、fresh absent head；已存在/禁用/tombstone/未完成同名均阻断；仅 SKILL.md，同 pattern 跨 revision/name/source 一次 digest-only 预留，与入队/依赖/消费/配额同事务 |
| 原子依赖与去重 | job/discovery/dependency/episode 消费/daily/cooldown 同事务；所有入口共享 episode/target/version identity 和目标 busy lock；root-to-key 固定，旧 root 消费和已知同 episode 别名保留防重，不退已消费票 |
| 生成与发布 | 使用现有维护 dispatcher，不新增付费 worker；只改变 SKILL.md，理由脱敏，独占学习新版本标记；保存 candidate/proposal 重启复用，审批实时 guard；旧 auto_patch 不能隐式采用学习候选 |
| 受限学习 auto_patch | 默认关闭的独立授权、已有 managed 小补丁；正文需 targeted 改善和 frontmatter 不变，description 另需 v2 正负首次选择改善，同改须分别改善；同审批事务固定 live provenance/观察摘要，共享旧额度/目标锁，来源撤销不当质量票 |
| Description trigger | 宿主 v2 suite、固定竞争目录、原生 Skill middleware metadata-only 首次选择；不发 oracle、不执行工具，重算目录/标签/报告；原 16 cases/96 results 上限与 behavior budget，v1 向后兼容 |
| 提案撤销 | 先关闭 claim/provenance，再清理未发布 generation/discovery/proposal/behavior 内容；只删独占且未被 install/overlay/publication/其他提案等保留的版本；adopted 资产与费用不动 |
| 贡献撤销 | CAS epoch 与相同设置幂等；撤销或删会话取消 unpublished job 并删除派生文本/来源，过期 worker 不可写回；消费记录不退款，正式技能不变 |
| 管理接入 | SDK、认证本地 MANAGE、owner-bound ACP；有界经验/pattern/match/作业列表、贡献设置、显式任务边界、CAS 取消三种学习作业；match 作业归属 pattern revision，不伪称单一 episode |
| 配置与桌面 | Go schema、save/validate/launch/applied settings 同步；桌面原生字段和功能探测同步，模型重命名保留 extraction/consolidation/matching 引用；尚未构建或做桌面视觉验证 |
| 学习来源 UI | 已接通状态/贡献/共享/显式任务/取消、20 条历史分页及推断/实际读取区分；原生 cursor 绑定、generation fencing、非幂等未知结果不重发、原服务只读核对；仅源码及隔离投影/管理通道验证 |
| 来源正文检查 | host-only SDK/MANAGE；固定 revision/scope/一个真实成员，仅返回被引用的脱敏视图；原 job/input/result、live source bytes/consent 同事务核验；最多 32 引用 / 512 KiB，私有源可在 paid stage 关闭后读取；一份缓存、无模型调用或新质量票 |

正常的原生审批恢复会使旧工具预留成为 `continued`。学习的 settled 检查核对 `budget_tool_continuations` 的真实后继，再要求整个 root 安全结束。`paused`、`unknown`、孤立 `continued`、未决 child、uncertain effects 与 blocked root 仍阻止模型调用。

## 3. 当前配置与控制

### 最新范围与单任务自动待审草案（2026-10-09）

按用户最新要求，主链必须由系统自动沉淀和起草，人工仅审核系统生成结果，不要求手工整理技能、选目标、请求生成或建立第二个 episode。本轮停止扩展手工起草入口，撤掉该入口尚未验证的本轮代码；已有可选管理能力保留，不作为进化前置步骤。

新增默认 false 的 `discovery.experience_review_drafts_enabled`。依赖完整 learning / consolidation / matching / generation 和 experience discovery 链，不依赖原 operator feedback。先尝试原重复任务准入；已核验观察证据不足时，可从一个完整本地执行任务自动起草 instruction-only 提案。冲突、歧义、未解决、已覆盖、无授权目标或已撤销来源继续阻断。`review_draft=true` 和原完整 evidence / policy / options 固定进 discovery，少于原 `repeat_threshold` 的候选只允许人工审核，即使评测通过且 auto_patch 开启也不能自动发布。完整独立证据仍走原自动补丁准入，不因新开关改变已满足门槛的 evidence policy。

沿用同一 episode/target/version 消费、pending/daily/cooldown、维护模型预算和生成 worker，不新增手工生成 API、ACP 扩展、工具或付费 worker。新能力还需原新技能/source gate，且始终人工审核；单 pattern lifetime 创建预留不变，驳回后换 revision 不再创建。配置 schema / save / launch / applied settings、四份默认模板及桌面字段/缓存发现行已接通；默认模板与有效配置区分，本机实际配置未变。

```yaml
skill_evolution:
  discovery:
    experience_review_drafts_enabled: true
```

这是一段合并设置，不是完整配置。须先显式启用前述完整自动链和授权技能来源；开关授权的是系统起草待审候选，不是降低自动发布门槛或把一个任务当两个独立票。

以下第一段仅开启经验提取，不会生成技能；要让系统自动进化，应继续合并后面的完整归并、匹配和生成设置。在已有原生配置中合并，不用它覆盖模型、来源或其他演化设置。`skills.enabled` 与外层 evolution 必须允许该能力；只提取时 generation 可以保持关闭。模型为空时继承全局默认模型，不继承某会话的临时模型选择。

```yaml
skill_evolution:
  enabled: true
  mode: review
  generation:
    enabled: false
  learning:
    enabled: true
    extraction_model_name: null
    consolidation_enabled: false
    consolidation_model_name: null
    cross_session_enabled: false
    matching_enabled: false
    matching_model_name: null
    idle_seconds: 300
    timeout_seconds: 60
    max_input_bytes: 131072
    max_output_tokens: 4096
    max_daily_model_calls: 10
    max_daily_tokens: 50000
    external_context_policy: hints_only
  budget:
    max_daily_model_calls: 30
    max_daily_tokens: 100000
    max_job_seconds: 300
    workers: 1
```

以上显式开启会授权后台模型消费。子额度位于共享总额内，不是额外预算。SDK 同时启用 generation 与 learning 时，两者共享 budget 必须完全一致。

需要语义归并时，再显式将 `consolidation_enabled` 设为 true。归并模型为空时继承全局模型；提取与归并共用 learning 的 timeout、input/output limits 和每日子额度。SDK 对应 `SkillLearningOptions.Consolidation` 与可选可信 `Config.SkillExperienceConsolidator`；无选项时，仅提供 adapter 也不能开启付费工作。

跨会话归并另需 `cross_session_enabled=true`，且每会话 `workspace_shared=true`。SDK 对应 `SkillLearningOptions.CrossSession` 和 `SkillLearningControlRequest.WorkspaceShared`。共享设置改变会提高同一个 consent epoch，清除旧学习来源和全部传递派生内容；重新授权只捕获之后输入，不回填旧聊天。该边界是同一私有数据库/profile 加规范化工作区，并非多租户共享服务的用户授权。Host 关闭跨会话能力会取消 shared claims，保留历史与 accounting；不把共享历史降级为普通会话历史。

需要 skill 匹配时，再将 `matching_enabled` 显式设为 true，要求 `enabled` 和 `consolidation_enabled` 同时 true；模型为空时继承全局默认模型，共享同一 learning 模型限制和子额度。SDK 对应 `SkillLearningOptions.Matching` 与可选可信 `Config.SkillExperienceMatcher`。Matching 不要求 generation。关闭 host skill/read_skill_file 工具访问会使匹配 inventory 为空，不从读取历史恢复被撤销的授权。这里的 `new_capability` 只是分类，既不选择新技能源，也不授权创建新技能。

要为已有 skill 自动生成待审候选，在既有模型/技能来源已授权的配置里再合并以下字段。它授权 generation/security/quality 和适用行为评测消耗共享维护额度；并非将 learning 子额度扩大为额外预算。不要直接覆盖其他模型、来源或 settings。

```yaml
skill_evolution:
  enabled: true
  mode: review
  generation:
    enabled: true
  learning:
    enabled: true
    consolidation_enabled: true
    matching_enabled: true
  discovery:
    operator_corrections_enabled: false
    experience_patterns_enabled: true
    repeat_threshold: 2
    repeat_window_days: 30
    cooldown_hours: 24
    max_daily_proposals: 5
    max_pending_proposals: 20
```

SDK 对应 `SkillDiscoveryOptions.ExperiencePatternsEnabled`，要求配置 `SkillGeneration` 和完整 learning 链。普通多轮会话无需第二个独立 episode 即可起草时，另行合并 `experience_review_drafts_enabled: true`，SDK 对应 `SkillDiscoveryOptions.ExperienceReviewDraftsEnabled`；不添加手工生成请求。现有目标桥接不要求 `new_skill_source_id`，不能把该参数当作新能力授权。服务启动时在 generation+matching 已配置的情况下绑定可信学习来源，即使 discovery 后来关闭，也可核验并人工审批合法旧提案。状态/list/proposal/diff 复用现有管理接口；`skill.learning.status` 报告 `proposal_discovery_enabled`，默认 `mode=review_candidates`。另行授权学习发布后显示 `auto_patch_enabled=true` / `mode=guarded_auto_patch`，不宣称某个候选已经评测或发布。

仅对已由人工采用的 managed 技能开启受限学习补丁，可在完整提案配置上再合并以下内容。`behavior.suite_path` 必须替换为可信宿主制定并维护的 fixture suite，候选模型不能自授权期望答案。description 修改需要 v2 suite，可参考 `docs/fixtures/skill-behavior-v2.json`，但宿主仍须人工确定真实目标及正负标签；不自动安装、启用或把示例视为真实质量验收。该例不是独立完整配置。

```yaml
skill_evolution:
  mode: auto_patch
  behavior:
    enabled: true
    suite_path: fixtures/approved-skill-behavior.json
  auto_patch:
    experience_patterns_enabled: true
```

SDK 对应 `SkillAutoPatchOptions.ExperiencePatternsEnabled`。缺省/false 不继承旧 auto_patch 学习发布权；null/数值不接受。关闭 experience discovery 后须同时关闭学习发布开关，合法旧提案仍可按 live learning guard 人工审批。`skill.auto_patch.status.options` 与观察历史保存该 opt-in；`skill.auto_patch.list.learning_source.source_state` 区分当时核验、撤销和链接不可用。后两种状态不产生质量失败或自动删除已发布内容。

新能力创建需在以上 experience discovery 已启用的基础上，另外设置下列字段；示例 `project` 必须替换为已配置且授权的非内置来源，不由系统自动注册。推荐显式 workspace managed 源，global 源还需 IncludeGlobal 权限。

```yaml
skill_evolution:
  discovery:
    experience_new_skills_enabled: true
    new_skill_source_id: project
```

SDK 对应 `SkillDiscoveryOptions.ExperienceNewSkillsEnabled` 和既有 `NewSkillSourceID`。这两行不是完整有效配置，需合并而非覆盖。`SkillExperienceMatchDecision.SuggestedName` / match 历史 `suggested_name` 只是名称建议，Target/Head 仍为空；宿主 discovery 裁定后才固定目标。创建仍是同一个 `experience_pattern` 类型，查看 `base.exists=false` 和 proposal `action=create` 区分。关闭 discovery 仍允许严格来源有效的旧提案人工审批，关闭 learning 或改变 matcher/access/catalog 则拒绝。

| 认证 MANAGE 操作 | 必填字段 | 返回 |
| --- | --- | --- |
| `skill.learning.status` | 无 | `enabled`、`mode=shadow` / `review_candidates` / `guarded_auto_patch`、`proposal_discovery_enabled`、`auto_patch_enabled`、各 stage 开关和数量 |
| `skill.learning.control.get` | `session_id` | `contribute`、`workspace_shared`、`consent_epoch` |
| `skill.learning.control.set` | `session_id`、`control` | `{contribute, expected_epoch}` 必填，可选 boolean `workspace_shared`；省略保留当前共享设置，贡献撤销则清除共享 |
| `skill.learning.experiences` | `session_id` | `experiences`、`next_cursor`；可选 `cursor`、`limit`，默认 50，上限 100 |
| `skill.learning.source.get` | `session_id`、完整 `source` | `source={kind,id,expected_revision,scope,experience_id}`；kind=experience 时 session scope 且 id=experience_id，kind=pattern 时选择当前修订真实成员；返回来源会话/episode/epoch、extraction/input/result 摘要、policy、checked_at_read/unassessed 和最多 32 个 cited 脱敏 references，非完整原始聊天或成功认证 |
| `skill.learning.patterns` | `session_id` | `patterns`、`next_cursor`；同样有界，只包含本会话经验；历史读取 `source_state=not_revalidated`，不重新认证实时来源 |
| `skill.learning.matches` | `session_id` | `matches`、`next_cursor`；可选 `cursor`/`limit`，目标/head/overlay 与实际 reads 分开；不返回候选 skill 正文，不重新认证实时来源 |
| `skill.learning.workspace.patterns` | `session_id` | 认证宿主查询该已同意共享会话的 workspace pattern；有界分页，核对所有成员共享授权，不认证任务树为实时证据 |
| `skill.learning.workspace.matches` | `session_id` | 同授权 workspace 的 matching 历史；不返回候选正文，所有来源共享授权仍须有效 |
| `skill.learning.lineage.link` | `session_id`、`source_session_id`、`source_episode_id`、`expected_epoch`、`expected_source_epoch` | 新 `episode_id`；两边 foreground/shared 且同 workspace，目标尚无 run/episode，继承宿主来源独立键 |
| `skill.learning.jobs` | `session_id` | `jobs`、`next_cursor`；含 `state_version`，不返回 snapshot 正文 |
| `skill.learning.episode.start` | `session_id` | `episode_id`；显式建立新用户目标边界 |
| `skill.learning.cancel` | `session_id`、`job_id`、`expected_state_version` | `cancelled`；取消未完成提取、归并或匹配，不等于删除已保存学习历史 |
| `skill.learning.pattern.ignore` | `session_id`、完整 `review` | `review`；含 pattern_id、expected_pattern_revision、expected_state_version、scope、request_key、ignored，保留原结果回执 |
| `skill.learning.pattern.adjudicate` | `session_id`、完整 `review` | `review`、`match`；前五字段加完整 target/expected_head、明确 overlay_base（无则 null）、relation、reason，人工目标来源，不授予发布 |
| `skill.learning.creation_retry.prepare` | `session_id`、完整 `retry` | `retry` 含 pattern_id、expected_pattern_revision、expected_state_version、scope、previous_generation_id；返回固定完整 enqueue request、原 generation/proposal 状态及 retry number / evidence reservation / max_retries=3，不入队、不调用模型 |
| `skill.learning.creation_retry.enqueue` | `session_id`、完整 `retry` | 前述条件加 request_key、expected_generation_state_version、previous_proposal_id、expected_proposal_state_version、完整 target/expected_head、明确 null overlay_base、evidence_reservation_id、expected_retry_number；返回原授权 ID/review/new generation/discovery/retry，保存完整原条件回执 |

ACP 的 `_meta.deerflow.skillLearning` 广告以下非标准扩展，参数使用 camelCase，control.set 可选 `workspaceShared` 且广告 `workspaceSharingControl=true`。该扩展仍描述 owner-bound shadow 历史/控制，不授予 generation/proposal 或跨会话 authority。ACP 不提供跨会话列表或 lineage.link，因为单会话 owner 不是其他聊天的读取 authority。可信 SDK 宿主可调用 `WorkspaceSkillExperiencePatterns`、`WorkspaceSkillExperienceMatches`、`LinkSkillLearningEpisode`。

```text
_deerflow/learning/control/get
_deerflow/learning/control/set
_deerflow/learning/experiences
_deerflow/learning/patterns
_deerflow/learning/matches
_deerflow/learning/jobs
_deerflow/learning/episode/start
_deerflow/learning/cancel
```

ACP 操作要求当前连接拥有且空闲的前台会话。认证 MANAGE 由本机管理 authority 检查会话身份，可以撤销正在进行的后台学习。关闭 host learning 停止 capture/extract/consolidate/match，并取消待执行作业，但保留历史与计费；关闭某会话 contribute 则清除其未发布学习来源和派生文本。重新打开 contribute 只捕获之后的真实输入，不回填历史聊天；只切换全局 host 开关不会删除先前合法捕获的 pending 来源，重新开启后仍可能处理这些来源。

归并完成不表示配方质量合格。`observed_execution` 仅表示至少两个宿主独立 episode 含已核验的本地完成回执，且没有未解决、冲突或歧义成员；不是用户确认或行为评测通过。Pattern 查询只校验已存内容和成员投影，不在查询内重读整个任务树；后续 matcher/discovery/generation 必须另做实时 guard。关闭全局 learning 后仍可查看这份历史，撤销 contribute 或删会话会同事务删除 pattern、revision、members、归并 snapshot/result 等派生文本，保留计费且不动正式技能。

Matcher 对 unresolved/conflicting/ambiguous pattern 明确跳过；对无明确目标或多个近似技能只保留未定分类。Inventory 超过 shortlist 时 `inventory_complete=false`，不允许从部分目录宣称缺少能力。Head、overlay、catalog 或 selection 变化不会隐式 rebase；当前该作业终止，待新 pattern revision 或显式人工已有目标裁决处理。Match 历史同样是 `source_state=not_revalidated`，已接入 discovery/approve 另做实时来源认证，不拿历史列表绕过授权。

显式重新裁决已有目标现在可通过可信 SDK `SetSkillLearningPatternIgnored` / `AdjudicateSkillLearningPattern` 和上表 MANAGE 操作完成；ACP 广告不新增。Pattern 与 match 历史附带当前 `review` 投影，匹配历史按 ID 有界合并原模型记录与 `ladj_` 人工记录，不伪造 matching job 或计费。新 MANAGE 对象拒绝多余/重复/错误类型、缺少 bool/head/overlay 等不完整字段。

创建重试 SDK 为 `PrepareSkillLearningNewSkillRetry` / `RetrySkillLearningNewSkill`。原 key 必须保持完整条件；unknown transport outcome 重发只用于读取原授权结果，不是再次支付一次。新的请求需要新鲜准备条件、用户确认和新的 key，仍不能从其他 conversation 的私有 pattern 或其他 workspace 创建。桌面在生成页选中并刷新终态原作业，回到已显示的归并条目 prepare 后确认 enqueue；只缓存和显示这些条件，后台执行 MANAGE，不在 render 读数据库/文件/网络。发现历史显示原作业/原 evidence reservation/人工 retry number，retry authorization 不是额外独立任务票。

来源检查 SDK 为 `SkillLearningSource(ctx, sessionID, SkillLearningSourceRequest)`。MANAGE source 的五个字段均必填，严格大小写、类型和重复字段检查，拒绝 null/额外字段；外层只有 operation/session_id/source。请求不用 request key，因为它只读，不产生原结果回执或准入 grant。`source_session_id` 表示成员真实来源，不冒充 workspace 操作者；private/共享授权、源变化或删除、old revision 都不会静默返回缓存或换成员。history 的 `not_revalidated` 保持不变；单独 source 检查返回 `checked_at_read`，仅覆盖该成员原 capture 视图与结果。

## 4. 定向验证

只运行下列新增及直接受影响的选择器。没有 `go test ./...`、全包扫描、race、桌面构建/视觉测试或远程付费模型评测。模型集成使用 loopback httptest 或 trusted fixture extractor。学习 UI 使用 `.build-cache/skill-learning-ui-state` 隔离 Rust 测试 harness，直接引用生产 `learning_state.rs` / `skills_contract.rs` / `settings_contract.rs`，提取未改写的 SkillBrowser/GenerationBrowser/MissingBrowser/AutoPatchBrowser 非 GPUI 方法，以及 evolution 表单字段和直接受影响的 tests；其他无关 projection collaborators 用 inert fixtures 替代。这验证缓存状态/请求守卫，不证明 GPUI 类型集成、后台 daemon 真实往返、布局或视觉行为。

```powershell
go test ./internal/runtime ./internal/acp/agent ./internal/learning ./internal/engine/eino . -run '^TestSkillLearning' -count=1
go test ./internal/skillmaintenance -run 'Test(TypedMaintenance|DurableMaintenanceSharedQuotaAndRestart|MaintenanceUnknownCallsFencingAndDeadline|MaintenanceRunOneJoinsCancellation)' -count=1
go test ./internal/skills/evolution -run 'Test(SkillLearningMaintenanceDispatcherPriorityAndAccounting|GenerationQueueReviewAndPublishAreSeparate|GenerationRestartsFromSavedCandidateAndProposal|BehaviorManualQueuedBudgetedReviewedAndPublishedSeparately|BehaviorGeneratedCandidateUsesOriginalJob|DiscoveryDistinctRootsAndActualGenerationEvidence)$' -count=1
go test ./internal/engine/eino -run '^TestPreparedObserverNativeBackgroundTextAndLateStreamError$' -count=1
go test ./internal/configschema ./internal/launch ./internal/desktopconfig -run '^Test(SkillLearning|EvolutionNativeFractionsCannotBeCoercedToIntegers|EvolutionNullAndZeroDefaultsAreValidAndDormantExceptionsAreNarrow|EvolutionTemplatesHaveOnlyNativeReviewDefaults|EvolutionLegacyConversionIsExplicitAndRevisionBound|GenerationSettingsSaveLaunchAndProviderConsumption)$|^TestSkillLearning' -count=1
go test ./internal/runtime ./internal/learning . -run '^TestSkillLearningConsolidationMigrationDoesNotRecreateDeletedStageInputs$|^TestSkillLearningConsolidationInactiveHistoryAndSessionIsolation$|^TestSkillLearningConsolidationSDKLifecycleManagementAndSharedDispatcher$' -count=1
go test ./internal/runtime ./internal/learning ./internal/skills ./internal/engine/eino ./internal/acp/agent . ./internal/configschema ./internal/launch ./internal/desktopconfig -run '^TestSkillLearningMatching' -count=1
go test ./internal/runtime ./internal/learning ./internal/acp/agent . ./internal/configschema ./internal/launch ./internal/desktopconfig -run '^TestSkillLearningWorkspace|^TestSkillLearningACPOwnerConsentAndStrictFields$|^TestSkillLearningCapture(DefaultOffConsentAndEpisode|TerminalAtomicityAndEventSpoof|DeletionCancelsUnpublishedJobs)$|^TestSkillLearningExtractionJobListingAndCancellationCAS$|^TestSkillLearningConsolidation(DistinctEpisodesAndUnresolvedEvidence|GuardRevocationChangedSourceAndRevision|SavedResultRestartReuseAndCorruption|InactiveHistoryAndSessionIsolation|ResultAndMembersCommitAtomically|MigrationDoesNotRecreateDeletedStageInputs|SDKLifecycleManagementAndSharedDispatcher)$|^TestSkillLearningMatching(PinsTargetAndDoesNotFabricateReadsOrGeneration|ResultAtomicRecoveryAndForget|IndependentOptInAndDisabledHistory|SDKLifecycleSharedDispatcherAndBudget)$|^TestEvolutionTemplatesHaveOnlyNativeReviewDefaults$' -count=1
go test ./internal/runtime ./internal/skills/evolution ./internal/configschema ./internal/desktopconfig ./internal/engine/eino . -run '^TestSkillLearningExperienceDiscovery|^TestDiscovery(DistinctRootsAndActualGenerationEvidence|ConcurrentReservationRestartAndLifetimeRootDedup|DisabledAfterReviewAllowsExplicitApprovalAndTerminalAck)$|^TestDerivedDiscoveryMissingWorkflowAndQuality$|^TestGeneration(QueueReviewAndPublishAreSeparate|RestartsFromSavedCandidateAndProposal)$|^TestBehavior(ManualQueuedBudgetedReviewedAndPublishedSeparately|GeneratedCandidateUsesOriginalJob)$|^TestSkillGenerationAndBothReviewsShareMaintenanceBudget$|^TestSkillCandidateResponseRejectsAmbiguity$|^TestEvolutionTemplatesHaveOnlyNativeReviewDefaults$|^TestSkillLearningWorkspace(RevocationCleansDistinctShortlistOutputs|SessionDeletionCleansForeignDerivedRows)$|^TestSkillLearningMatchingResultAtomicRecoveryAndForget$' -count=1
go test ./internal/runtime ./internal/learning ./internal/skills ./internal/skills/evolution ./internal/configschema ./internal/desktopconfig ./internal/engine/eino . -run '^TestSkillLearningExperienceNewSkill|^TestSkillLearningExperienceDiscovery(OptInIndependentEpisodesAndManualPublication|MatchingRelationsDoNotGrantNewTargets|RevocationPreservesIndependentRetention|SDKSharedDispatcherLoopbackAndDisabledRecovery)|^TestSkillLearningMatching(CapabilityNameIsOnlyAReadableSuggestion|DecisionsCannotInventTargetsOrCapabilities|ProjectionKeepsAttributionSeparate|NewCapabilityTargetUsesHostScopeAndProtectsNames|EinoIsolatedStrictAndBudgeted|EinoParserRejectsTypesCaseAndExtraJSON|EinoCapabilityNameCannotChooseSourceOrPath|SDKLifecycleSharedDispatcherAndBudget)$|^TestDiscovery(DistinctRootsAndActualGenerationEvidence|ConcurrentReservationRestartAndLifetimeRootDedup|DisabledAfterReviewAllowsExplicitApprovalAndTerminalAck)$|^TestDerivedDiscoveryMissingWorkflowAndQuality$|^TestGeneration(QueueReviewAndPublishAreSeparate|RestartsFromSavedCandidateAndProposal)$|^TestSkillCandidateResponseRejectsAmbiguity$|^TestEvolutionTemplatesHaveOnlyNativeReviewDefaults$' -count=1
go test ./internal/runtime ./internal/skills/evolution ./internal/configschema ./internal/desktopconfig ./internal/engine/eino -run '^TestSkillLearningExperienceDiscovery(ApprovalRevalidatesLiveAuthority|HeadReceiptAndGenerationOutputGuards|SavedCandidateAndProposalRestartReuse|SharedQuotaCrossTriggerDedupAndAtomicity)$|^TestSkillLearningExperienceDiscoveryNeedsHostAuthorityAndRemainsManualOnly$|^TestSkillLearningExperienceDiscovery(RequiresExplicitCompleteChain|SettingsSaveLaunchAndDefaultOff)$|^TestSkillLearningMatchingEinoCapabilityNameCannotChooseSourceOrPath$|^TestSkillGenerationAndBothReviewsShareMaintenanceBudget$|^TestEvolutionTemplatesHaveOnlyNativeReviewDefaults$' -count=1
go test ./internal/runtime -run '^TestSkillLearningExperienceNewSkillOptInOutputAndRevocationGuards/renamed-revision$' -count=1
go test ./internal/runtime -run '^TestSkillLearningExperienceNewSkillOptInOutputAndRevocationGuards/pending-(before|after)$' -count=1
go test ./internal/desktopconfig -run '^TestSkillLearningExperienceNewSkillSettingsSaveLaunchAndDefaultOff$' -count=1
go test . -run '^TestSkillLearning(SDKShadowLifecycleAndManagement|MatchingSDKLifecycleSharedDispatcherAndBudget|WorkspaceSDKManagementConsentLineageAndDispatcher)$' -count=1
go test ./internal/runtime ./internal/skills/evolution ./internal/configschema ./internal/desktopconfig . -run '^TestSkillLearningAutoPatch|^TestAutoPatchPublicationObservationAndRollback$|^TestSkillLearningExperienceDiscoveryNeedsHostAuthorityAndRemainsManualOnly$|^TestEvolutionTemplatesHaveOnlyNativeReviewDefaults$' -count=1
go test . -run '^TestSkillLearningAutoPatchSDKIndependentStartupGrantAndManagementStatus$|^TestSkillLearningExperienceDiscoverySDKSharedDispatcherLoopbackAndDisabledRecovery$' -count=1
go test ./internal/runtime ./internal/skills/evolution ./internal/configschema ./internal/desktopconfig -run '^TestSkillLearningAutoPatchAdmissionAndRevocation/new-skill$|^TestSkillLearningExperienceDiscoveryOptInIndependentEpisodesAndManualPublication$|^TestBehaviorApprovalReconstructsPersistedEvidence$|^TestGenerationQueueReviewAndPublishAreSeparate$|^TestEvolutionNullAndZeroDefaultsAreValidAndDormantExceptionsAreNarrow$|^TestSkillLearningExperienceNewSkillSettingsSaveLaunchAndDefaultOff$' -count=1
go test ./internal/skills/evolution ./internal/desktopconfig -run '^TestSkillLearningAutoPatchRetainsManualPackageBoundaries$|^TestEvolutionTemplatesHaveOnlyNativeReviewDefaults$' -count=1
go test ./internal/runtime -run '^TestSkillLearningAutoPatchPendingRestartRechecksPublicationPolicy$' -count=1
go test ./internal/skills/evolution -run '^TestSkillLearningAutoPatchSourceRecordIsBoundedAndHistorical$' -count=1
go test ./internal/skillbehavior ./internal/engine/eino ./internal/skills/evolution -run '^TestSkillTrigger|^TestBehaviorSuiteStrictParsingAndDigest$|^TestSkillBehaviorActualToolLoopVariantIsolationAndReceipts$|^TestBehaviorApprovalReconstructsPersistedEvidence$|^TestSkillLearningAutoPatchRequiresTargetedAssertionImprovement$' -count=1 -timeout=60s
go test ./internal/runtime ./internal/skills/evolution -run '^TestSkillTrigger|^TestSkillLearningAutoPatchAdmissionAndRevocation/(on|description|baseline-timeout)$|^TestSkillLearningAutoPatchFrontmatterRemainsManual$|^TestBehaviorApprovalReconstructsPersistedEvidence$|^TestBehaviorGeneratedCandidateUsesOriginalJob$' -count=1
go test ./internal/launch ./internal/runtime -run '^TestSkillTriggerNativeSuiteLoadingUsesExplicitHostFileAndSourceGrant$|^TestSkillTriggerLearningApprovalReconstructsPersistedCatalogAndLabels$' -count=1
go test ./internal/runtime -run '^TestSkillTriggerLearningDescriptionPublicationRequiresIndependentEvidence/negative-improvement$' -count=1
go test ./internal/desktopconfig . -run '^TestBehaviorSettingsSaveValidateLaunchAndPinnedSuite$|^TestSkillBehaviorSDKManualAndGeneratedCandidates$' -count=1
go test ./internal/runtime . -run '^TestSkillLearningReview' -count=1 -timeout=90s
go test ./internal/runtime ./internal/skills/evolution -run '^TestSkillLearningMatchingResultAtomicRecoveryAndForget$|^TestSkillLearningWorkspaceSessionDeletionCleansForeignDerivedRows$|^TestSkillLearningExperienceDiscoveryOptInIndependentEpisodesAndManualPublication$|^TestSkillLearningExperienceNewSkillOptInOutputAndRevocationGuards/renamed-revision$|^TestSkillLearningAutoPatchAdmissionAndRevocation/on$|^TestGenerationQueueReviewAndPublishAreSeparate$' -count=1 -timeout=90s
go test ./internal/runtime . -run '^TestSkillLearningReviewPersistedAdjudicationReconstructsProjection$|^TestSkillLearningReviewRestartDisabledReceiptAndIgnoredRevisions$|^TestSkillLearningReviewIgnoreIsAtomicScopedAndDoesNotRefund$|^TestSkillLearningReviewSDKAndManagementManualControls$' -count=1 -timeout=60s
go test ./internal/runtime . -run '^TestSkillLearningCreationRetry' -count=1 -timeout=90s
go test ./internal/runtime . -run '^TestSkillLearningSource' -count=1 -timeout=60s
go test ./internal/runtime -run '^TestSkillLearningSourceActualSkillReadUsesSavedVersionAndInvalidatesChangedRead$' -count=1 -timeout=45s
go test ./internal/runtime -run '^TestSkillLearningSourceReadOnlyRedactedCitationsAndDisabledRecovery$|^TestSkillLearningExtractionCompletedEpisodeDoesNotWaitForNewTask$|^TestSkillLearningExtractionUnsafeAndUnsettledDoNotCall/waiting-child$' -count=1 -timeout=45s
go test ./internal/runtime . -run '^TestSkillLearningExtractionDebounceRestartAndRedaction$|^TestSkillLearningExtractionUnsafeAndUnsettledDoNotCall/(uncertain|waiting-child|blocked-budget)$|^TestSkillLearningConsolidationGuardRevocationChangedSourceAndRevision/changed-event$|^TestSkillLearningSourceSDKManagementCitedHintsDisabledAndWithdrawal$|^TestSkillLearningSourceToolReceiptLiveBytesAndIdentityGuards$|^TestSkillLearningExperienceDiscoveryOptInIndependentEpisodesAndManualPublication$' -count=1 -timeout=75s
go test ./internal/runtime -run '^TestSkillLearningCreationRetryFailedOutputRestartsAsFreshManualCandidate$' -count=1 -timeout=45s
go test ./internal/runtime -run '^TestSkillLearningCreationRetryCancelledAtomicReceiptAndManualPublish$|^TestSkillLearningCreationRetryFailedOutputRestartsAsFreshManualCandidate$' -count=1 -timeout=60s
go test ./internal/runtime -run '^TestSkillLearningCreationRetryReceiptCannotRehashNewReviewVersion$' -count=1 -timeout=45s
go test ./internal/runtime ./internal/skills/evolution -run '^TestSkillLearningCreationRetryWorkspaceNonmemberActorRevokesDerivedChain$|^TestSkillLearningReviewIgnoreIsAtomicScopedAndDoesNotRefund$|^TestSkillLearningMatchingResultAtomicRecoveryAndForget$|^TestSkillLearningWorkspaceSessionDeletionCleansForeignDerivedRows$|^TestSkillLearningExperienceNewSkillOptInOutputAndRevocationGuards/renamed-revision$|^TestSkillLearningExperienceDiscoveryOptInIndependentEpisodesAndManualPublication$|^TestGenerationQueueReviewAndPublishAreSeparate$|^TestSkillLearningAutoPatchAdmissionAndRevocation/new-skill$' -count=1 -timeout=90s
```

从仓库根执行 UI 隔离选择器（缓存 harness 不属于产品或桌面构建）：

```powershell
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib learning_state::tests
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib learning_transport_tests
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib skills_contract::tests
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib deerflow_generation_learning_source_change
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib auto_patch::learning_tests
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib learning_status_separates_auto_publication_grant_from_shadow_and_review
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib deerflow_evolution_requires_complete_matching_nested_contract
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib deerflow_evolution_form_exposes_operator_generation_and_configured_models
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib learning_workspace_history_requires_host_and_explicit_session_sharing
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib skill_trigger
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib deerflow_behavior_report_caches_repeated_case_comparisons_and_bound_identity
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib deerflow_behavior_inspector_partial_failure_keeps_evidence_without_passing_review
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib learning_review
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib learning_creation_retry
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib learning_source
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib learning_read_fencing
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib learning_review_uses_loaded_scope_revision_and_control_version
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib learning_creation_retry_observation_does_not_survive_a_different_pattern_selection
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib learning_transport_fences_unknown_operations_to_original_store_without_replay
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib learning_transport_episode_result_unknown_never_becomes_a_generic_retry
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib learning_current_state_read_does_not_reauthorize_an_older_mutation_receipt
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib learning_transport_success_invalidates_dependent_projections_and_releases_guard
cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib catalog_requires_full_state_and_rejects_rebound_detail
foreach ($case in @('navigation_keeps_pages_bounded_and_filters_bound','proposal_and_history_pagination_are_independent','deerflow_generation_transport_failure_retains_original_mutation','missing_retry_stays_bound_to_original_service_store','service_restart_preserves_original_retry_and_completion_clears_binding','definitive_missing_failure_or_abandon_releases_service_binding','deerflow_generation_service_changes_preserve_draft_and_revoke_old_authority','deerflow_generation_prepare_pins_complete_absent_head_and_draft','deerflow_generation_lists_replace_pages_and_bind_filters')) {
    cargo test --offline --manifest-path .build-cache/skill-learning-ui-state/Cargo.toml --lib $case
}
```

验证包含多 root 单 episode、终态事务故障回滚、真人事件 spoof 拒绝、debounce、重启、chunk overlap、已完成 episode 不被新任务阻塞、敏感值/隐藏推理过滤、取消/忘记/来源变化/伪造引用拒绝、saved-result 复用与损坏拒绝、贡献幂等、owner/严格字段校验、作业 CAS 取消、调用子额度、学习背压与旧 producer 恢复。

原生 SDK fixture 另覆盖真实 Eino TurnLoop、技能加载、实际本地 `read_file` 回执、审批恢复、终态 capture、lead 回答与脱敏投影。它证明接线有效，不证明真实任务配方质量有效。

P2 新增定向验证覆盖同 episode 多批次不增票、独立 episode 增票、未验证证据阻断、双向冲突/歧义、跨会话隔离、foreign 与 adapter 修改 shortlist 拒绝、来源/epoch/revision 改变时晚到输出拒写、CAS 取消、saved-result 的新建/merge 复用和损坏拒绝、输出事务故障原子回滚、共享学习子额度、disabled host 历史可读、active shortlist 延后、一次性 P1 历史经验迁移，以及单事务来源缓存边界。SDK fixture 同时验证 learning-only 与 generation shared dispatcher；loopback Eino fixture 检查严格 JSON、无工具、budget receipt 和失败不退款。配置/save/launch 校验包含独立 consolidation gate、稀疏继承和未知模型拒绝。

Matcher 的新增定向验证覆盖 runtime visibility、workspace/global 和 disabled shadow、同 scope 重名、Names nil/empty、不可变正文及 metadata/body 上限、builtin/overlay 和 ABA head；BodyTx 的私有 grant 固定 access/SQL transaction/原始 descriptor，公开 slice 修改或跨 transaction 复用不能伪造授权。实际读取优先但旧版不计当前版，相关目标不补造 reads，模型改写 shortlist/foreign ID 被拒绝。另覆盖调用前/后 consent 和来源变更、会话 workspace、revision/head/overlay、CAS 取消、输出事务故障、保存结果复用/损坏、重启后 selection/source identity/model 变化、一次性历史 pattern 迁移、学习 pending 背压与恢复。SDK 验证 standalone/shared dispatcher、三阶段共享子额度、工具拒绝和管理接入；Eino 使用 loopback 检查严格 JSON、无工具和失败保守计费。没有真实任务语义准确率结论。

跨会话新增定向验证覆盖未授权同目录隔离、private/shared 互不召回、跨工作区拒绝、两独立任务归并、精确初始输入复制/显式 linked copy/恢复不增票、显式任务增票、lineage 不可重绑与一次迁移、共享归并 workspace 串行、依赖表缺失拒绝、调用前/后 consent/receipt/workspace 改变拒写、saved merge 复用、共享 matcher 正常接线、host 禁用取消旧 claim 且不降级泄露历史。递归撤销同时覆盖已选中与未选中的 shortlist 内容，不删其他会话原始经验，不退款、不动技能。SDK/认证 MANAGE 的两种 dispatcher、严格 sharing 字段与 host-only 查询，以及 schema/save/launch/default template 同步均使用 fixture 验证。

已有目标经验 discovery 的新增定向验证覆盖六种 match 关系的准入、独立 episode 门槛与跨入口消费、旧 root 同任务别名防重、完整本地回执及 live provenance、instruction-only 支持文件保留、无行为 suite 时的 `not_run`、旧 auto_patch 拒绝学习候选，以及审批前 receipt/pattern/model/access/consent/host 变化拒绝。重启复用已保存 candidate/proposal，不再次调用生成模型；撤销会取消 queued claim 且消费不退，独立保留、预先存在或经其他提案采用的版本不误删。SDK loopback 通过真实 capture/共享 dispatcher 验证单任务草案关闭时一个 episode 不生成、两个 episode 只创建一个待审提案，以及关闭 discovery 后合法旧提案仍可人工审批；不证明候选方法或匹配语义质量。

新能力定向验证覆盖独立创建 gate、缺少名称保持 shadow、名称严格 JSON/结构限制、模型不能返回 source/path、非内置来源与 workspace/global 可见性、Names nil/empty/限定选择、已存在/禁用/tombstone/visible global 名称碰撞、独立门槛、单文件输出、审批前新 head、撤销派生正文/独占版本且不退消费、入队故障原子回滚和恢复，以及拒绝后更换名字/新增 revision/两个新 episode 仍不重复创建。SDK loopback 验证真实 accepted capture 和 shared dispatcher、固定可读 name/absent base/仅 SKILL.md 的工具-free Eino 请求、关闭 discovery 后恢复并人工发布；未开启远程模型。Save/launch/applied settings 同步新 gate，effective 字段数量仍在桌面 96 字段包络内。

P3 定向 fixture 验证独立 SDK/native/draft 默认关闭 gate、完整链 prerequisites、旧 auto_patch 不隐式放行、targeted 改善与 regression-only/无改善/模型操作失败区别、frontmatter 不变和导入首次采用/新能力/overlay/支持文件人工边界、发布前 receipt/consent 变化阻断、共用每日配额和目标观察锁、观察写入故障时发布事务回滚且可恢复。发布后撤销不删采用版本、不产生质量票；关闭 learning 发布的重启仍可观察，两个确认质量失败回滚且不重复生成/回滚，无确认反馈观察结束仍 evidence insufficient。配额测试直接布置受外键约束的隔离观测行，未扩大模型/任务执行范围；SDK 新增仅 startup/status/MANAGE，断言模型零调用，原 discovery loopback 仅直接回归。桌面新增 4 个投影状态用例和 3 个既有表单/contract/workspace 直接回归通过；不构建桌面。

P3 description 本轮定向验证覆盖 v2 正负标签/竞争目录/类型边界与 v1 兼容，native suite 显式文件和来源 grant，Eino 原生 Skill schema/reminder 接线、不发送 oracle/正文/支持文件、漏选和过度触发、未知工具/名称/重复参数/多调用/截断/空响应、输入边界、budget/cancel/未知用量保守计费。Runtime fixture 验证描述-only、同改正文双改善、正文无改善拒绝、触发无改善/正负失败/基线操作故障、receipt/consent 撤销，竞争描述/prompt/evaluator/suite 改变后的重启拒绝，原配置恢复不重生成/评测。重算目录/选择/oracle 拒绝重新哈希的伪造持久结果；恢复原结果后可正常发布。另补负例过度触发修复准入。它不包含真实远程模型准确率验收。

首次 runtime 验证发现旧 `reportBoundTo` 仍只接受 v1 comparison rule，使完整 v2 结果不能进入报告；已补齐 v2 绑定并定向复核。取消 fixture 首次未消费 HTTP 请求 body，导致 httptest Close 等待；只终止该已确认的本轮 `eino.test`，修正 fixture 消费请求并以 45/60 秒超时定向复核，未扩大测试选择器或修改产品运行服务。新增 3 个 Rust trigger 报告/inspector 缓存用例和 3 个既有直接回归通过；隔离 harness 改为提取未改写的生产 BehaviorView parse 与 BehaviorInspector 非 GPUI 方法，仍不构建桌面。

人工控制定向覆盖私有/共享/foreign workspace、非来源成员操作会话撤销、exact head/ABA/来源身份/当前修订/review CAS、shortlist 外目标、未解决来源拒绝、完整 MANAGE 类型和 nested 大小写 alias、未知结果原 key/store replay、幂等回执事务故障回滚、模型调用中忽略阻断晚到输出。另验证重启关闭 learning 后历史回执与撤销仍可用、新裁决拒绝，忽略对新 pattern revision 持续生效，历史回执不重启旧决定；原提案清理不退 episode/target/version 消费，人工分类不伪造 reads/operator quality，行为评测合格仍不自动发布，人工采用后再忽略保留技能。持久化人工结果从固定请求/选择重建，重新哈希伪造 model origin 也被拒绝。SDK fixture 新增仅 1 个原生三阶段任务，无远程模型。桌面新增 6 个状态/传输用例与所选旧未知结果/共享/来源缓存直接回归，旧 revision match 不能借当前 review 投影授权修改未显示的新 pattern；仍不是 GPUI 类型或可用性验收。

显式创建重试定向验证覆盖 queued 拒绝、cancelled / rejected / failed 原任务、精确 original job/proposal/target/review/ordinal/evidence 条件、同名 pending、daily/cooldown 拒绝、来源变化拒绝、入队事务故障回滚、3 次上限和原作业不分叉、旧 snapshot 不改写、episode 消费不退不增、固定授权重哈希篡改拒绝、重启后新 generation、未知结果原 key replay、忽略后原 receipt 可读但不复活 producer、workspace 非成员操作会话撤销、贡献撤销保留 digest-only 账目。SDK fixture 两个独立用户输入、原生成和一次人工 retry 使用 loopback Eino，验证 shared dispatcher / 严格 MANAGE / 关闭 discovery 后人工采用 / 关闭全部 paid learning/generation 后仍可读原回执，新 key 不授予工作。桌面新增 5 个隔离缓存/传输/discovery 计数用例；人工授权证据行不计入独立 episode 数量。早期新增 Go fixture 编译参数和 Rust 成员计数错误已修正；一次过期 fixture 改错了 `harness_skill_outcomes` 日期，改为学习实际 pin 的 outbox 后通过，不能把该检查声称为真实时间推进评测。

本轮初次 fixture 编译有 Get/Approve/RunAutoPatch 等既有 API 形状不符，已局部修正；运行中忽略的测试最初把预期 maintenance claim CAS 失效当作成功返回条件，已改成验证取消 fence 和零晚到结果。SDK 首次发现 MANAGE 顶层白名单未包含新增 operation，补齐后定向通过。未据此扩大全包或产品构建。

各阶段最近的定向结果均通过，早期 P1/P2 结果和本次 matcher 结果分开列出：

| 选择器 | 包结果 |
| --- | --- |
| 新增 `TestSkillLearning`，P1/P2 定向合并轮 | runtime 93.690s、ACP 18.528s、learning 3.217s、Eino 27.281s、SDK 33.384s |
| P2 最后 migration/inactive/shared 接线选择器 | runtime 10.991s、SDK 16.212s；learning 包无匹配用例 |
| P2 归并优先入队调度后 shared SDK 复核 | SDK 7.047s |
| typed/shared maintenance 定向 | skillmaintenance 4.380s |
| dispatcher 与既有 generation/behavior/discovery 定向 | evolution 16.829s |
| 原生 observer 文本回归 | Eino 0.213s |
| 配置/schema/save/launch/template 定向 | configschema 2.443s、launch 35.250s、desktopconfig 39.906s |
| Matcher 新增 + 所选 consolidation/CAS/config/template 直接回归 | runtime 93.976s、learning 2.140s、skills 11.746s、Eino 13.495s、ACP 1.496s、SDK 47.941s、configschema 1.418s、launch 14.384s、desktopconfig 45.782s |
| BodyTx private grant 最后收紧后，授权读取/真实读取/standalone+shared SDK 定向复核 | skills 7.757s、runtime 11.717s、SDK 24.403s；learning 包无匹配用例 |
| Matcher 接入后 typed/shared quota 与既有 generation/behavior/discovery 直接回归 | skillmaintenance 1.143s、evolution 3.341s |
| Workspace sharing/lineage/传递撤销 + 所选 capture/consolidation/matching/SDK/config/template 直接回归 | runtime 86.185s、learning 3.441s、ACP 10.468s、SDK 59.591s、configschema 0.835s、launch 1.581s、desktopconfig 15.083s |
| 最后补充真实 session purge，跨会话派生/claim 清理定向复核 | runtime 8.863s |
| 已有目标经验 discovery + 所选 provenance/删除/generation/behavior/config/SDK 直接回归（2026-10-09） | runtime 133.279s、evolution 33.151s、configschema 3.007s、desktopconfig 12.211s、Eino 19.830s、SDK 30.835s |
| 新能力创建/name/source/去重/撤销/SDK + 所选既有 discovery/generation/matcher/config 直接回归（2026-10-09） | runtime 73.447s、learning 1.307s、skills 10.809s、evolution 22.736s、configschema 5.228s、desktopconfig 11.846s、Eino 17.341s、SDK 55.317s |
| 新能力接入后已有目标 provenance/head/output/restart/shared quota + 独立 gate/template/模型预算定向补充 | runtime 57.111s、evolution 0.432s、configschema 1.229s、desktopconfig 2.822s、Eino 1.004s |
| 创建消费 hint 提前排除、审核前/后的独立同名待审候选及 effective settings 最后接线定向复核 | runtime 11.615s / 11.412s、desktopconfig 7.681s |
| 学习 UI 隔离投影/授权/未知结果管理（2026-10-09） | 新增 22 个 Rust 用例通过：learning_state 15、learning transport 5、共享分页 1、generation 源撤销缓存 1；所选直接回归 9 个通过；不构建 GPUI app |
| 学习 UI 对接的现有 SDK/MANAGE shadow/matching/workspace 三个用例定向复核 | SDK 43.472s，fixture/临时数据库，无远程模型 |
| P3 新增受限学习 auto_patch + 原观察/默认 template 定向 | runtime 68.796s、evolution 10.317s、configschema 8.211s、desktopconfig 17.725s；SDK 首次 fixture 缺少主 Engine，修正后另行复核 |
| P3 SDK 独立 startup/MANAGE gate + 原经验 discovery loopback 直接回归 | SDK 17.884s，新 gate fixture 断言零模型调用 |
| P3 新能力仍人工 + 原 manual publish/behavior provenance/generation/default-null/save-launch 直接回归 | runtime 13.496s、evolution 1.056s、configschema 0.625s、desktopconfig 6.750s |
| P3 补充 overlay/support/import/new 结构边界与 packaged default gate | evolution 3.295s、desktopconfig 6.727s |
| P3 同一待审候选重启：gate/discovery/suite/discovery-policy 改变拒绝，原配置恢复发布且不重生成/评测 | runtime 9.247s |
| P3 来源记录字段/摘要/版本边界、历史与质量状态严格分开，旧观测兼容 | evolution 5.879s |
| P3 桌面隔离投影与新字段接线 | 新增 4 个 Rust 用例、3 个直接回归通过；仅引用/提取生产非 GPUI 源码，无桌面构建或视觉测试 |
| P3 description suite/parser/Eino 首批及 v1 loop 直接回归 | skillbehavior 7.271s、Eino 7.281s |
| P3 description 准入/重启与原 v1 正文/provenance/generation 直接回归 | runtime 51.108s、evolution 1.986s；此前 v2 report 绑定已修正，最小恢复复核 runtime 13.151s |
| P3 description 持久目录/oracle 重算、native suite 加载 | runtime 4.051s、launch 10.602s；Eino 取消 fixture 首次等待已单独修正，不作为通过结果 |
| P3 trigger 取消/budget/未知用量/操作故障复核 | Eino 7.766s，loopback，45 秒选择器上限 |
| P3 trigger 最后宿主标签/catalog/form span + 原 v1 parser/fixture/provenance/targeted 准入复核 | skillbehavior 8.187s、Eino 8.112s、evolution 0.577s；只新增与直接回归，60 秒选择器上限 |
| P3 负例误触发修复及原 save/launch/SDK v1 直接回归 | runtime 9.199s、desktopconfig 9.559s、SDK 14.305s |
| P3 trigger 桌面缓存逻辑 | 新增 3 个 Rust 用例 + 原 report/partial-inspector/learning-mode 3 个直接回归通过；不构建 GPUI app |
| 人工忽略 / 当前目标裁决 / 原 key 回执 / 重启及来源撤销（2026-10-09） | 最新 runtime 39.592s、SDK 17.223s；仅新增选择器，fixture / 临时数据库，无远程模型 |
| 人工控制所影响的原 matching/删除/discovery/new-name/auto_patch/generation 直接回归 | 最新 runtime 27.948s、evolution 3.178s；SDK 白名单修正后新增选择器 11.160s，nested wire 精确大小写严格检查新增 selector 最后 6.419s |
| 人工来源 summary 对原模型 payload 保持兼容及完整 nested MANAGE 最后复核 | 人工目标裁决生成的候选必须由用户审核发布 + 原 saved generation/proposal 重启选择器 runtime 15.363s；新增原生 SDK/MANAGE 11.170s |
| 人工原结果回执重新哈希篡改、ignore / disabled replay 和 SDK 最后收紧复核 | runtime 14.723s、SDK 12.206s；仅新增直接选择器，人工回执重建原 immutable 结果，不凭哈希承认伪造归因 |
| 人工控制桌面缓存逻辑 | 新增 6 个 Rust 用例通过；原未知结果 2、共享 1、模式 1、generation 来源缓存 1、旧 receipt fence 1、来源 cache invalidation 1、catalog detail 1 个直接回归通过；不构建 GPUI app |
| 显式新技能创建 retry 新增 Go 选择器（2026-10-09） | 首批 7 个 runtime + 2 个 SDK/parser 全部通过，runtime 27.676s、SDK 16.905s；第 8 个 failed 输出后重启 fresh manual candidate 单独通过 9.174s；仅 fixture / loopback / 临时数据库，无真实付费模型 |
| 创建 retry 所影响原 matching/删除/discovery/new-name/ignore/generation/auto_patch 直接回归 | runtime 29.200s、evolution 1.224s；包含 workspace 非成员 actor 新用例，不跑无关包 |
| 创建 retry 固定 request key / 原预留 / generation authorization 最后收紧 | 3 个新增 directly affected 用例 runtime 16.969s；不做 broad scan |
| 创建 retry maintenance owner/kind/key 身份守卫收尾 | cancelled 原任务到人工发布及 failed 输出后重启两个新增直接用例通过 12.445s；原回执重哈希用例补充 owner/kind/key 三项篡改拒绝、恢复后原结果不变，通过 8.733s；不增加测试包或运行其他链路 |
| 创建 retry 桌面缓存 | 新增 5 个 Rust 用例通过；原人工 review 6、unknown control 1、generation source-cache 1 直接回归通过；新增 discovery 投影提取真实生产非render 源码，不构建 GPUI app |
| 来源正文 host-only SDK/MANAGE 定向新增（2026-10-09） | 首批 3 个 runtime + 2 个 SDK/parser 全部通过，runtime 13.791s、SDK 9.947s；仅 deterministic extraction / 临时数据库，无远程模型 |
| 来源实际 skill read 第 4 个 runtime 用例 | 真实 Registry snapshot/read observer 经原 recordSkillReadTx 保存后，检查 exact version/path/file hash 和改变 read 后拒绝，通过 7.601s；初次 fixture 试图重复 Configure 被 startup-only guard 拒绝，改为新 store 一次配置，未放宽生产边界 |
| 来源检查只覆盖原 captured turn，不被之后同 episode 活跃追问遮蔽 | 新增来源 continuation 断言与原 completed-episode / unsettled-child 准入直接回归通过 10.611s；只读核验仍要求每个原 captured root settled，昂贵工作继续要求整个 episode 无活跃输入 |
| 来源检查共用 live evidence guard / 私有 pattern / human publication 直接回归 | runtime 16.264s、SDK 8.619s；仅 extraction 脱敏、uncertain/child/blocked、归并 changed-event、直接来源与经验人工发布选择器，不跑其他包 |
| 来源正文桌面缓存与只读管理 | 新增 6 个 Rust 用例通过，原 generation source-cache 1、read fencing 1、review CAS 1、retry selection 1 直接回归通过；只引用生产 learning source 非 GPUI 模块和原传输提取源码，不构建桌面 |
| 单任务自动待审草案 + 所选独立任务/人工发布/new-skill/retry/auto_patch 直接回归（2026-10-09） | 最后合并选择器 runtime 68.295s、evolution 12.750s；fixture / 临时数据库，无远程模型 |
| 单任务草案配置/save-launch/effective/default template 与 SDK shared-dispatcher 直接回归 | 最后合并选择器 configschema 2.164s、desktopconfig 8.529s、SDK 16.341s；SDK 使用本机 httptest Eino，单任务全自动起草、MANAGE review flag 和关闭 discovery 后人工审核均通过 |
| 单任务草案桌面缓存/合同/表单 | `learning_review_draft`、`deerflow_evolution_form_exposes_operator_generation_and_configured_models`、`deerflow_evolution_requires_complete_matching_nested_contract`、`learning_creation_retry_discovery_does_not_count_authorization_as_an_episode` 各 1 个隔离 Rust 用例通过；提取/引用生产非 render 源码，不构建桌面 |
| 原生全链自动学习/起草、认证 MANAGE 审核、受限自动发布和重启后真实读取（2026-10-09） | 新增 `TestSkillLearningNativePipelineAutomaticReviewAndPublication` 两个分支全部通过：review-draft 7.78s、automatic-existing 11.02s，总测试 18.79s、SDK 包 25.290s；实际 Eino 前台与全部维护适配器，仅本机 httptest 模型响应，不调用真实付费模型 |
| 正式设置保存/启动加载至原生全链、JSON 工具封装脱敏修复及直接重启回归（2026-10-09） | 新增 `TestSkillLearningSettingsNativeAutomaticPipelineAndReview` 两分支通过；最后相同范围精确选择器 runtime 25.898s、SDK 29.107s、desktopconfig 34.310s。设置全链 review-draft 10.19s、automatic-existing 12.78s；没有 GPUI 构建或真实语义质量结论 |
| 结构化/多重转义脱敏、限额 fail-closed 与旧快照重新哈希拒绝 | 新增 3 个 redaction 用例和原文本/候选两个直接回归通过，最后 learning 9.312s；实际 capture/job 的真实 SHA-256 重哈希拒绝及已修正 hash-validator 误用的 source 检查包含于最后 runtime 25.898s。零模型调用/经验；失败运行不计通过 |
| 正式设置原生新能力自动起草/人工批准/重启读取及共享 fixture 直接回归（2026-10-09） | 新增 `TestSkillLearningSettingsNativeNewCapabilityAutomaticallyDraftsAndRequiresReview` 两分支通过：review-draft 8.00s、independent-auto-mode 14.54s；最后 desktopconfig 55.198s、SDK 32.569s，包含原已有技能两分支。新能力原 lifetime 去重及有行为评测仍人工的三个精确 runtime 回归全部通过 11.249s；仅本机 loopback、临时设置/数据库，不改生产准入 |
| 隐藏字符文本脱敏顺序修复、真实捕获/旧快照拒绝及原生发布直接回归（2026-10-09） | 新增规范化/候选输出/真实 capture 三个用例；修复前 9 个秘密负例全部复现失败，修复后通过。最后合并精确选择器 learning 0.368s、runtime 17.351s、SDK 25.976s、desktopconfig 59.848s；正式启动四分支带用户/JSON 字符串隐藏字符秘密仍正常生成/人工或受限自动发布并重启读取。未扩大到其他包、桌面构建或远程付费模型 |

来源正文定向覆盖 private 同目录隔离、当前 pattern revision / 真实成员校验、同工作区双重 shared consent 和撤销、原 input/result/job identity、工具回执和实际 skill read 改变拒绝、rehashed snapshot 不授予读取、只返回实际 cited 引用，以及关闭 paid stage 后 private source 可读但 withdraw 后拒绝。SDK fixture 使用 deterministic Engine 产生本地与 web_fetch 回执和 lead 回答，无网络抓取或远程模型；验证 external hints_only、原始 secret 过滤及 MANAGE/SDK 一致。桌面 6 个新增隔离用例验证完整请求/响应身份、类型与内容上限、外部正文拒绝、actual version 非因果、行/分页刷新/成员/范围/会话/服务/写操作/失败清缓存、晚到结果丢弃和只读操作不生成 pending mutation。首轮 Rust 编译因复用的 SkillRef.validate 返回 bool 而非 Result 已修正；不把这些 fixture 当作 GPUI、daemon 往返或真实语义质量验收。

单任务自动草案覆盖同一 episode 普通两轮输入、不调用手工边界/生成接口、零 operator feedback、原 Promotion 独立门槛不放宽、幂等扫描和一次真实消费、评测通过仍不自动发布、完整独立证据仍可走原 auto_patch、默认关闭/已覆盖/歧义拒绝、额外支持文件拒绝、receipt 改变/来源撤销拒绝、生成入队故障原子回滚、原创建 lifetime 预留，以及重启且 discovery 关闭后的合法人工审核。首次 Go fixture 编译出现 RunAutoPatch 返回值/API 名称错误已修正，PromotionIDs 保留原签名 wrapper；失败运行不计为通过。最后使用以下精确选择器，未运行全包测试、桌面构建或真实付费模型：

```powershell
go test ./internal/runtime ./internal/skills/evolution -run '^TestSkillLearningReviewDraft|^TestSkillLearningExperienceDiscoveryOptInIndependentEpisodesAndManualPublication$|^TestSkillLearningExperienceNewSkillReadableTargetManualOnlyAndRestart$|^TestSkillLearningCreationRetryCancelledAtomicReceiptAndManualPublish$|^TestSkillLearningExperienceDiscoveryNeedsHostAuthorityAndRemainsManualOnly$|^TestSkillLearningAutoPatchAdmissionAndRevocation$/(on|old-grant|new-skill)$' -count=1 -timeout=90s
go test ./internal/configschema ./internal/desktopconfig . -run '^TestSkillLearningReviewDraft|^TestSkillLearningExperienceDiscoveryRequiresExplicitCompleteChain$|^TestEvolutionTemplatesHaveOnlyNativeReviewDefaults$|^TestSkillLearningExperienceDiscoverySDKSharedDispatcherLoopbackAndDisabledRecovery$' -count=1 -timeout=60s
```

新增 Go 文件与本次拥有的修改通过 gofmt 检查，先前桌面小模块与 P3 涉及的九个拥有模块通过 edition 2024 rustfmt 检查；本轮人工控制及 retry 的 learning_state/learning/skills/generation/discovery 和两个拥有的 tests 模块亦定向通过；来源正文的 learning_source_state/learning_state/learning 与两个 tests 模块分别定向格式化。大型 `deerflow_settings.rs` 仅此前局部编辑，使用 rustfmt stdout 做语法检查，不做跨文件格式化。tracked diff 按仓库现有 CRLF 设置通过 whitespace 检查。未格式化或回退用户原有 MCP/session update 修改。

单任务草案收尾重新只读检查准入、live generation/approval 和 auto_patch 守卫：完整门槛优先，fallback 重建同一来源、match、归并 shortlist 和引用，不对未解决/冲突/歧义或失效来源放行；已消费证据不退款，单任务候选不升级为自动发布。同步修正文档中的旧“一个 episode 一律只 shadow”表述与配置示例适用范围。133 个拥有的 changed Go 文件通过 gofmt 只读检查，103 个拥有的 untracked 文本文件通过 trailing whitespace / EOF 检查，本轮 5 个拥有的 Rust 模块通过 skip_children rustfmt 检查，tracked diff whitespace 通过（仅原 CRLF 转换警告）；未重跑无关测试或改动有效配置。

原生全链新增单个 SDK 测试文件 `go-harness/skill_learning_native_pipeline_test.go`，不改生产准入。每个任务提交两轮普通用户输入，真实 Eino 前台调用原生 `skill` 和 `read_file`，原生提取、归并、匹配、生成、安全/内容审查和隔离行为评测全部经过本机 OpenAI-compatible httptest；不使用替代 extractor/consolidator/matcher，不调用手工 episode 或 generation 接口。fixture 仅在临时数据库预置已采用的 managed 基线，不预置经验或进化结果。验证实际读取的版本归因、完整本地回执、脱敏秘密与 host assertion ID 不进入维护模型，以及固定行为 suite 中 baseline 定向断言失败、候选通过且回归均通过。

`review-draft` 分支验证单个观察任务自动产生待审 diff，批准前 head 不变，经真实 loopback TCP 的 DFACP/1 token 认证 `proposal.approve` 才发布；`automatic-existing` 分支在每来源会话同意 workspace sharing 后，以两个独立任务自动发布，第一任务不生成候选，全程没有批准调用。两个分支均经认证 MANAGE 查询运行授权、发现归因与提案 diff/版本。10/13 条维护调用回执全部结算且各计 125 tokens；模型过程不制造 operator feedback，自动发布观察初始 success/failure 均为零。关闭学习/生成/发现/自动发布、重开 Client 并原生 LoadSession 后，新 run 实际读取已发布版本，未重生成或评测，也未改写导入文件。该测试验证原生 Go 链路和认证管理传输，不证明真实模型语义准确率、固定 suite 的代表性或桌面 GPUI/daemon 产品往返；之前未 LoadSession 的首次失败属于 fixture 修正，不计通过。

```powershell
go test . -run '^TestSkillLearningNativePipelineAutomaticReviewAndPublication$' -count=1 -timeout=150s -v
```

正式设置全链新增 `go-harness/internal/desktopconfig/skill_learning_pipeline_test.go`，经真实 desktopconfig `save` 写临时 YAML、`launch.ApplyConfig` 构建原生模型与来源、`deerflow.Open` 启动 worker，不直接注入 SDK 学习选项或替代 Engine。仅将上一轮 loopback 模型 fixture 提取到 test-only import 的 `internal/skilllearningtest` 复用；7 个配置名称故意共用 provider model ID，按不同本机端点和凭据核验 foreground/extract/consolidate/match/generate/security/quality 路由，quality 的隔离行为调用复用正式 launcher 的 evaluation route。保存和初始化模型不发请求；运行状态与 applied settings 对照，sharing/审核/观察通过真实 token 认证 MANAGE。普通两轮输入全自动生成提案；单任务人工审、双独立任务自动发布，与原 SDK 全链保持相同证据、真实读取和 10/13 条共享维护回执。关闭能力也通过再次保存及 fresh ApplyConfig 完成，重启保留已采用 head，新 run 实际读取候选版本，未改导入文件或重新评测。

该正式启动测试首次暴露学习视图脱敏缺口：native configured `read_file` 用 JSON `content` 字符串封装 JSON 文件，转义后的敏感字段未被原文本正则识别，fixture 的 `NATIVE_PRIVATE_TOKEN` 到达提取请求。已在 `internal/learning/redaction.go` 增加有界结构化解码，递归检查对象/数组/嵌套 JSON 字符串和解码后的敏感 key/value，使用 UseNumber 保留大整数；无改变的 JSON 不重排。最多 16 层、4096 个值、每层 2 MiB；Token parser 在树分配前限制层数/元素，重复 decoded key 或脱敏后 key 碰撞同样 fail-closed。只有解码后的文本/键使用文本规则，避免 authorization/cookie 的旧宽正则毁掉相邻非敏感 JSON 证据。非 JSON/带前缀/截断文本补充转义引号赋值过滤，保留原最终 UTF-8 字节限制。没有移除秘密断言、改用简化 read_file 或放宽生产来源守卫。新用例覆盖标准封装、双重编码、Unicode/隐藏字符 key、typed secret、Windows 路径、数值/非敏感字段保持、重复 key/上限、重放稳定性。实际 runtime capture 的旧未脱敏 view 即使更新 payload/job 的真实 SHA-256，仍被重新构造的 live redacted view 拒绝，终态 evidence_changed、零模型用量；不自动修复/重新支付旧作业。直接来源原有重哈希负例也修正了同名 bool hash-validator 误用，现用真实 SHA-256 而不是仅把 false 写进 hash 字段。

初次测试 fixture 的 Page 类型、runtime 不支持字段、review-mode applied settings 稀疏字段与 MANAGE nested control 已局部校正，不修改产品契约；脱敏失败是生产缺陷并已修复。所有失败运行不计为通过。最后只运行新增及受该修复直接影响的来源/提取/saved-result/发布恢复选择器：

```powershell
go test ./internal/learning ./internal/runtime -run '^TestSkillLearningRedaction(NestedJSONToolEnvelope|StructuredBoundsFailClosed|EscapedTextAndStableReplay|RemovesSecretsAndMachinePaths|RehashedLegacyEnvelopeCannotAuthorizeExtraction)$|^TestSkillLearningCandidateProvenanceAndRedaction$' -count=1 -timeout=60s -v
go test ./internal/runtime -run '^TestSkillLearningRedactionRehashedLegacyEnvelopeCannotAuthorizeExtraction$' -count=1 -timeout=45s -v
go test ./internal/runtime . ./internal/desktopconfig -run '^TestSkillLearning(NativePipelineAutomaticReviewAndPublication|SettingsNativeAutomaticPipelineAndReview|ExtractionDebounceRestartAndRedaction|ExtractionSavedResultReusedAndVerified|SourceReadOnlyRedactedCitationsAndDisabledRecovery|SourceToolReceiptLiveBytesAndIdentityGuards|ExperienceDiscoverySavedCandidateAndProposalRestartReuse)$' -count=1 -timeout=150s -v
```

最后合并同一范围并包含实际旧快照拒绝、新版结构化 parser 的运行全部通过：

```powershell
go test ./internal/learning ./internal/runtime . ./internal/desktopconfig -run '^TestSkillLearningRedaction(NestedJSONToolEnvelope|StructuredBoundsFailClosed|EscapedTextAndStableReplay|RemovesSecretsAndMachinePaths|RehashedLegacyEnvelopeCannotAuthorizeExtraction)$|^TestSkillLearningCandidateProvenanceAndRedaction$|^TestSkillLearning(NativePipelineAutomaticReviewAndPublication|SettingsNativeAutomaticPipelineAndReview|ExtractionDebounceRestartAndRedaction|ExtractionSavedResultReusedAndVerified|SourceReadOnlyRedactedCitationsAndDisabledRecovery|SourceToolReceiptLiveBytesAndIdentityGuards|ExperienceDiscoverySavedCandidateAndProposalRestartReuse)$' -count=1 -timeout=150s -v
```

正式配置新能力全链复用上述保存/启动与 loopback fixture，新增 `TestSkillLearningSettingsNativeNewCapabilityAutomaticallyDraftsAndRequiresReview`。前台只执行真实本地 `read_file`，不加载已有研究技能，也不提交手工经验、目标裁决、episode 或生成请求；原生 matcher 使用完整授权目录，返回 `new_capability` 和 `complete-json-audit` 可读名称建议。创建来源由保存的独立 `experience_new_skills_enabled` / `new_skill_source_id=python-custom` 授权，生成请求固定 absent base、具体名称和仅 `SKILL.md`，security/quality 两项真实适配器报告通过。不注入学习结果或预置 managed 新技能，不为新能力补造 skill read 或行为 baseline；已有研究技能的可信 suite 不适用于新名称，报告明确 `not_run`。

单任务普通两轮输入自动起草人工待审提案；关闭单任务草案的双独立会话分支在第一任务不生成，第二任务自动起草。后者开启完整学习 auto_patch 并留出原两秒 worker 的扫描周期，候选仍保持 `pending_review`、新 head 不存在，必须由认证 MANAGE 批准。人工采用不生成 auto_patch observation，已有研究技能 head 与导入文件不变。6/9 条维护回执全部结算、各 125 tokens，无 operator feedback、采用前零 skill reads；每条 pattern 仅一个 lifetime 创建预留，episode 消费分别为 1/2。再次保存关闭能力、fresh ApplyConfig/Open/LoadSession 后，前台真实加载已采用新技能的候选版本，不重生成或评测。配合原 runtime 显式执行 auto_patch 的合格行为 new-skill 负例和 rejected/renamed-revision 去重回归，验证新技能始终人工及 lifetime 不退款；没有把 `not_run` 或固定 fixture 当作真实语义质量合格。

最后只运行新能力及共用 fixture 所直接影响的原生链路和三个精确 runtime 分支；本轮没有产品运行逻辑变更、桌面构建、全包测试或真实付费模型调用：

```powershell
go test . ./internal/desktopconfig -run '^TestSkillLearningNativePipelineAutomaticReviewAndPublication$|^TestSkillLearningSettingsNative(AutomaticPipelineAndReview|NewCapabilityAutomaticallyDraftsAndRequiresReview)$' -count=1 -timeout=150s -v
go test ./internal/runtime -run '^TestSkillLearningReviewDraftNewCapabilityAutomaticallyDraftsOnce$|^TestSkillLearningExperienceNewSkillOptInOutputAndRevocationGuards$/renamed-revision$|^TestSkillLearningAutoPatchAdmissionAndRevocation$/new-skill$' -count=1 -timeout=60s -v
```

后续只读核对发现 `redactPlain` 先匹配正则、后移除 Unicode `Cf` 和非布局控制字符，导致 `to<zero-width>ken`、`Bea<word-joiner>rer`、隐藏字符分隔的 `sk-`、PEM delimiter 或机器路径避开首轮过滤，移除隐藏字符后反而留下明文秘密。这也影响 JSON 内已经解码的普通 content 字符串，不仅是结构化敏感键。新增 `TestSkillLearningRedactionNormalizesHiddenCharactersBeforeFiltering` 在修复前九个秘密分支全部复现；仅调换生产纯文本规范化顺序，先移除隐藏字符、再 trim、最后应用原正则，不新增权限/付费阶段、不修改来源或 JSON 上限。额外检查隐藏字符位于文本边缘时 trim 幂等，不留下第二次重放才消失的空白。

新增候选输出测试检查 goal/applicability/problem/resolution/steps/verification 均去秘密、非敏感程序保留，重新校验相同结果摘要稳定；新增真实 runtime 两分支使用 accepted user、lead text 与实际本地 receipt 捕获，确认快照/传给 extractor 的视图/被引用来源都不带秘密。把其中工具结果替换为旧 filter-then-normalize 明文、更新快照与实际 job SHA-256 后仍被 live source 重建拒绝，`evidence_changed`、零调用/经验，不靠伪 hash 让负例通过。正式 settings 全链 fixture 再添加 accepted user 的零宽 token 和 configured `read_file` 中 JSON content 的隐藏字符 token，并要求所有维护请求都无这些标识；原已有目标、新能力四分支全通过，生成/审核/自动发布和重启读取未受影响。仅此六个拥有 Go 文件和设计/记录变更，gofmt / trailing whitespace / EOF 与仓库原 CRLF diff check 通过，本机有效配置、个人数据库和正式技能未改。

最后执行的精确范围如下；修复前失败不计通过，确定性覆盖不宣称绝对脱敏或真实语义质量已验收：

```powershell
go test ./internal/learning ./internal/runtime . ./internal/desktopconfig -run '^TestSkillLearningRedaction(NormalizesHiddenCharactersBeforeFiltering|NestedJSONToolEnvelope|StructuredBoundsFailClosed|EscapedTextAndStableReplay|RemovesSecretsAndMachinePaths|HiddenTextCaptureAndLegacyView|RehashedLegacyEnvelopeCannotAuthorizeExtraction)$|^TestSkillLearningCandidate(ProvenanceAndRedaction|HiddenCharactersCannotPersist)$|^TestSkillLearning(NativePipelineAutomaticReviewAndPublication|SettingsNativeAutomaticPipelineAndReview|SettingsNativeNewCapabilityAutomaticallyDraftsAndRequiresReview|ExtractionDebounceRestartAndRedaction|ExtractionSavedResultReusedAndVerified|SourceReadOnlyRedactedCitationsAndDisabledRecovery)$' -count=1 -timeout=150s -v
```

### 生成正文凭据阻断（2026-10-09）

本轮继续实现主链安全边界，没有扩展人工写技能、选目标或发生成请求。确认原 `possible_credential` 仅覆盖部分带引号赋值，不能阻断自动生成正文中的未加引号 token、Bearer、JSON 具体凭据、转义键、隐藏字符和未闭合私钥。新增实际 runtime 自动单任务草案用例，修复前八个输出全部可进入候选及待审提案；不是根据正则推测漏洞。修复后增加完整独立来源且开启 auto_patch 的已有 managed 技能分支，也在落库前拒绝。九个分支均无新候选/owned version/待审提案，零内容审查调用，原 generation receipt 保留、证据/日额度和新技能 lifetime 预留不退款，不改变正式 head，不自动重新生成。

`internal/learning/promotion_credentials.go` 新增学习正文专用只读检查。复用隐藏字符规范化，但不拿原经验脱敏函数改写整个 Markdown：一般文档 URL、通用临时路径和完整环境变量/明确占位符合法保留，具体敏感赋值、Bearer、前缀、私钥和 URL userinfo/percent-encoded 敏感 query 拒绝。有界最多 16 次展开 JSON 转义，支持嵌入 Markdown 和多重编码例子；仍是模式扫描，不宣称任意凭据或编码可识别。保留旧静态扫描的无词边界敏感赋值覆盖，只有完整占位符例外。支持文件、手工/普通 operator 生成候选继续用原规则，caller 自称 automatic/experience trigger 无法获得例外，必须宿主 generation claim、精确 candidate version 和真实 discovery kind 绑定。扫描不改内容字节/哈希，通用例子经原评审和人工采用后逐字核对相同。

同一宿主守卫接入生成保存、submit、saved checkpoint 恢复、内容/行为评测入口，以及人工/自动共用的 Approve。旧待审正文发现具体凭据会原子变为 `failed` 并返回 validation failure，释放待审容量，不退消费；不根据模型“安全通过”放行。四个新增恢复负例按真实 Registry 生成带隐藏字符凭据的合法版本，并更新真实 generation progress、提案 request/evaluation SHA-256 和对应 diff，涵盖 candidate、validating proposal、pending review，以及审核已提交但 job finalize 故障的 terminal-review ack；重启后不重新生成/评测/付费，也不能发布。不靠损坏哈希或失效来源让负例通过。已发布或已拒绝结果保持原终态幂等确认，不升级成追溯撤销；旧已保存正文保留原审计/来源撤销语义，本轮不自动清理。此检查也不替代正文语义或泛化评测。

直接回归修正 auto_patch baseline fixture 先前主动保留的匹配专用秘密，明确采用的可信基线不再带 `PRIVATE_SKILL_MATCH_*`，没有减少生成审查或授权断言。普通占位符首轮被原旧 static finding 拦截，已将 learning `SKILL.md` 的检查集中到宿主专用规则，不放宽其他文件/入口；Bearer 环境变量包装与 lifetime 表名等 fixture 问题亦已修正。失败运行不计通过。

新增六个顶层用例及直接回归通过；只检查 self-evolution/受影响 publication 和原生启动链。未跑全包、桌面构建/视觉、远程付费模型，未修改有效设置/个人数据库/已发布技能。第一次合并选择器全部通过：learning 1.836s、runtime 156.949s、evolution 20.484s、SDK 38.862s、desktopconfig 81.563s；runtime 选择器实际还匹配了直接受影响的 regression / atomic-observation 两分支，记录真实范围。最后保留旧 assignment 覆盖、补充 percent-encoded query 和支持文件例外负例后的新增精确选择器通过：learning 7.208s、evolution 7.144s、runtime 58.981s。最终同源代码的原生 SDK/正式设置启动全链再次精确复核通过：SDK 29.798s、desktopconfig 64.030s；SDK 两分支 22.66s，settings 已有技能两分支 28.04s、新能力两分支 25.87s。最终源文件拥有的 12 个 Go 文件格式/whitespace 检查通过，沿用仓库 CRLF 规则的 tracked diff check 通过。

```powershell
go test ./internal/learning ./internal/skills/evolution ./internal/runtime -run '^TestSkillLearningPromotionInstructions(RejectCredentialLiterals|AllowGenericGuidance)$|^TestSkillLearningCredentialGuardRequiresHostDiscovery$|^TestSkillLearningGenerated(CredentialsRejectedBeforeStaging|GenericInstructionsKeepExactBytes|LegacyCredentialsCannotResumeOrPublish)$' -count=1 -timeout=90s -v
go test . ./internal/desktopconfig -run '^TestSkillLearningNativePipelineAutomaticReviewAndPublication$|^TestSkillLearningSettingsNative(AutomaticPipelineAndReview|NewCapabilityAutomaticallyDraftsAndRequiresReview)$' -count=1 -timeout=150s -v
```

### 连续对话中的自动队列等待与发布保留（2026-10-09）

本轮只处理自进化主链调度，没有扩展人工整理、选目标或手工发起生成入口。先用真实 accepted input 和原终态事务复现三处问题：提取、归并、匹配已入队但还未领取时，同一 episode 的普通下一轮对话开始，worker 将暂时活跃当作失效来源，消耗一次 claim 并永久跳过旧工作。修复前三个分支均明确失败；没有通过删掉活跃检查或改来源数据让用例通过。

共享 `skillmaintenance.Hooks` 新增只在 claim 事务内执行的 `Ready` hook，standalone 学习和正式 `deerflow.Open` 的原 evolution dispatcher 都接入。一个受原全局 pending 上限约束的 100 条 metadata shortlist 可以跳过忙碌来源，让其他 workspace 或普通技能维护继续执行；没有另建 worker、schema、重试入口或调度器。提取只看原 episode，归并同时看 seed 和最多 8 个原 shortlist 的成员，匹配只看原修订成员，经验候选生成还覆盖原归并 shortlist 中未选中的比较来源。缺少或不合法的 hint 不授予工作权，仍由原 Handler / reservation admission / 提交和发布守卫拒绝。等待不改变 job identity、state version、attempt、deadline、receipt 或证据消费；重启重新注册相同宿主 hook 即可继续。原已开始的 retry 到 deadline 后照常终结，不无限延长，不退已付费调用。

实际共享链新增四个分支，覆盖 extract / consolidate / match / experience-backed generation 在连续对话时等待，结束后原快照自动继续到 discovery、instruction-only 候选、原安全/内容审查、人工批准。全链保持 6 个维护调用、90 charged tokens，没有 operator feedback 或新增独立票；第三轮仍独立留在原 pending outbox，不被旧快照吞掉。另两个 shared-member 分支验证 recalled 来源活跃也会等待，但同会话后续显式独立 episode 活跃不阻塞旧来源；这里显式 episode 仅用于测试隔离，不是用户得到待审提案的前提。

审核 ACK 新负例进一步复现：已 `pending_review` 的候选在后续普通对话期间收到批准请求，被旧实现永久标为 `stale`。现在本次批准返回 permission failure，保留原待审版本；任务结束后仍执行原 live evidence/head/权限/凭据检查。已提交内容审查但 maintenance ACK 中断的恢复不等待后续任务、不再调用模型，也不因此获得发布权。新增 auto_patch 分支用两个独立来源和真实 fixture baseline/candidate comparison，活跃期间保留待审，结束后由原自动发布扫描采用同一候选，不需人工重新生成、不重评测，观察成功/失败票仍为零。此次未恢复既往已 failed/stale 作业，也不承诺领取后又变活跃的调用无损恢复；模型执行中仍按原来源重校验和有限预算 fail-closed。

新增 9 个顶层用例通过：maintenance 4 个、runtime 5 个（共 11 个 runtime 分支）。最终新增精确选择器结果 maintenance 1.873s、runtime 35.895s。直接受影响的共享预算/优先级/租约/取消/截止、原生成和行为队列、提取 ACK、归并/匹配撤销与 head 守卫、旧凭据恢复和单任务审核回归全部通过：maintenance 2.435s、evolution 8.072s、runtime 51.980s。批准来源变更、已入队取消、单任务草案永不自动发布和 auto_patch 直接回归通过 runtime 58.916s；其子选择器实际还匹配 `description` / `regression` / `atomic-observation`，均为本轮批准入口直接受影响分支，记录真实范围。首轮新增 fixture 的未使用变量/API 名称错误，以及共享 match scan 先跳过旧修订的 fixture 问题均已修正；失败运行不计通过。

同一最终生产源码的原生 SDK 与正式设置保存/启动全链精确复核全部通过：SDK 34.571s（人工/已有受限自动两分支 27.72s），desktopconfig 85.699s（已有两分支 34.92s，新能力两分支 37.10s）。仍是实际 Eino 与全部正式维护适配器、本机 httptest、临时设置和数据库、认证 MANAGE 与重启读取；不证明真实模型语义质量。未扩大到全包、其他业务链、race、桌面构建/视觉或远程付费模型，未修改有效配置、个人数据库或已采用技能。

本轮拥有的 11 个 Go 文件通过 gofmt 只读检查，两个文档及拥有的 Go 文件通过 trailing whitespace / EOF 检查，沿用仓库原 CRLF 设置的 tracked diff check 通过；未格式化或回退用户原有 MCP/session update 修改。

```powershell
go test ./internal/skillmaintenance ./internal/runtime -run '^TestSkillLearningMaintenanceReadiness(PreservesJobsAndSkipsBusyOwners|KeepsOriginalRetryDeadline|ErrorDoesNotConsumeClaim|CoversGlobalPendingCap)$|^TestSkillLearning(QueuedConversationDefersWithoutLosingWork|SharedQueuedConversationResumesToReview|QueuedSharedMembersWaitOnlyForCapturedEpisodes|QueuedReviewACKDoesNotWaitForLaterConversation|AutoPatchWaitsForConversationThenPublishesOriginalCandidate)$' -count=1 -timeout=100s -v
go test . ./internal/desktopconfig -run '^TestSkillLearningNativePipelineAutomaticReviewAndPublication$|^TestSkillLearningSettingsNative(AutomaticPipelineAndReview|NewCapabilityAutomaticallyDraftsAndRequiresReview)$' -count=1 -timeout=150s -v
```

### 执行中的连续对话有界自动恢复（2026-10-09）

本轮继续收敛到“系统自动进化、人工只审核”。先以真实 accepted input、终态 outbox 和已结算维护回执复现提取、归并、匹配三处缺陷：原模型已处理捕获快照，普通下一轮真人输入开始，原输出提交守卫把暂时活跃统一转为 `evidence_changed`，一次 claim 后永久丢弃旧工作。三个修复前分支均实际失败。没有让模型自称来源有效、删掉 live 校验、手工重建经验或自动换目标来绕过。

新增 `internal/learning/source_activity.go`，只读核验 scope 严格绑定当前 SQL 事务。先核对 host/consent/policy 和原捕获 outbox、root settlement、真实字节/回执/引用；只有完整有效但同 episode 存在已接受真人任务活跃才记录临时状态。归并 seed/全部 shortlist、匹配全部 pattern 成员与目录/head/投影、promotion 原 match/归并来源也继续完整检查，其他永久错误优先于活跃。事务内经验/提取缓存保留活跃否决，不能借缓存丢失它。缺失已终态输入的 outbox 单独视为永久证据损坏。经验候选重建保留临时原因至验证完固定 evidence、lifetime reservation、名称/来源和新能力冲突；generation 的当前授权、配置/head/overlay 及候选凭据守卫不被暂时活动遮蔽。保存的归并计划重建不再把底层来源错误无差别改写成 invalid plan。

共享维护队列新增固定错误码 `source_active`，提取/归并/匹配/经验生成进入原有限重试。继续沿用首次 claim 的 deadline、最多 3 attempts、原 2/4 秒 backoff、全局及学习子预算、租约/取消/消费/lifetime 预留，不新增 schema、worker、重试作业或模型调用授权。原 readiness 在追问仍活跃时保留 retry，不消耗下一 attempt；重启在来源结束后自动继续原 job/input/candidate，无需人工发重做请求。未合法提交的模型输出仍可以丢弃并付费重做；已保存候选、内容审查和完成行为用例复用。实际 Eino 的 extract/consolidate/match/generate/content/behavior/trigger 七条适配路径保留宿主临时原因，同时过滤 provider/来源诊断；普通 permission failure 仍不是重试许可。原已终态审核 ACK 不重新授权或重做；人工/自动批准仍走既有 live source 和版本守卫。

新增 7 个顶层定向用例，合计 29 个分支。runtime 五个用例包含提取/归并/匹配付费后及匹配 reservation 前、重启等待、原捕获两轮不 rebase/不吞第三轮、不增加独立票；生成/内容/行为和新能力四阶段继续到同一自动待审提案，内容及已完成行为 case 不重跑，人工采用后正常发布。新能力 retry 保持同名/同源、absent base、仅一个 lifetime 预留，即使打开 auto_patch 仍人工；已有技能 suite 不被借给新能力，明确 `not_run`。单源来源变化、缺 outbox、workspace、catalog/head、pattern revision、贡献撤销保持 failed/cancelled；共享成员活跃同时另一个来源损坏或共享撤销也终结。预算耗尽仍拒绝后续 paid admission。独立 maintenance 用例用 fixture 时钟核对 3 attempts 和原 deadline 耗尽均终结且不退调用。native runtime matcher 使用实际 Eino + httptest，在宿主 broker reservation 前及真实本机响应期间开始实际 accepted continuation，重启后原 job 正常提交，回执逐条保留；没有付费外部模型。

初次生成恢复 fixture 查询所有提案时把此前已采用的 baseline 也算成“多提案”，已改为读取原 generation 的 proposal ID；共享 match 的 cursor 先跳过旧修订，已改成最多八次原旋转 scan 寻找确实 queued 的 stage。第一次把新增与 12 个直接回归合在一个 runtime 命令，190 秒整体 timeout 在后续 workspace fixture 的正常 idle sleep 时触发，没有死锁栈；不计合并命令通过。拆成相同精确范围、未删测试或放宽生产守卫后全部通过。修复前失败、fixture 错误和 timeout 均不计为验收成功。

最终新增 runtime 五个用例完整通过，package 90.140s；maintenance/Eino 新增及直接预算/租约/队列/生成/review checkpoint 回归通过，maintenance 3.142s、evolution 16.676s、Eino 12.545s；12 个直接 runtime 回归单独完整通过，156.408s，覆盖提取/归并/匹配 saved result、来源撤销、完整目录/head、候选恢复、审批 live guard、审核 ACK、只读来源和人工裁决投影。最终同一生产源码的原生 SDK/正式设置全链通过，SDK 64.862s（两分支 52.71s）、desktopconfig 100.108s（已有两分支 53.78s、新能力两分支 34.44s），包括自动已有 managed 补丁、单任务人工待审、新能力永远人工、认证 MANAGE、重启实际读取。仅 self-evolution 和直接受影响回归；未跑全包、race、桌面构建/视觉或付费模型。19 个拥有 Go 文件 ASCII/gofmt/whitespace/EOF clean，沿用仓库 CRLF 规则的 diff check 通过。本机有效设置、个人数据库和正式技能未改。

```powershell
go test ./internal/runtime -run '^TestSkillLearningInFlight(ConversationKeepsOriginalBoundedJob|GenerationAndReviewResumeWithoutManualWork|ActivityCannotHidePermanentSourceChanges|SharedActivityValidatesAllPinnedMembers|NativeMatchingPreservesAdmissionCauseAndPaidResponse)$' -count=1 -timeout=155s -v
go test ./internal/skillmaintenance ./internal/skills/evolution ./internal/engine/eino -run '^TestSkillLearningSourceActivityRetainsOriginalAttemptAndDeadlineLimits$|^TestSkillLearningMaintenanceReadiness(PreservesJobsAndSkipsBusyOwners|KeepsOriginalRetryDeadline|ErrorDoesNotConsumeClaim|CoversGlobalPendingCap)$|^TestMaintenanceUnknownCallsFencingAndDeadline$|^TestMaintenanceRunOneJoinsCancellation$|^TestDurableMaintenanceSharedQuotaAndRestart$|^TestGeneration(QueueReviewAndPublishAreSeparate|RestartsFromSavedCandidateAndProposal|StaleBaseAndConfigurationDoNotCallModels|ReviewRetryPreservesCandidate|CommittedReviewRecoversWithoutNewModelConfiguration|CancelDuringReviewRejectsProposalAtomically)$|^TestBehaviorResumesCompletedCasesAndContentAcrossRestart$|^TestSkillLearningMaintenanceDispatcherPriorityAndAccounting$|^TestSkillLearningNativeAdaptersPreserveTemporarySourceAdmission$|^TestSkillMaintenanceReviewChargesBothStages$|^TestSkillGenerationAndBothReviewsShareMaintenanceBudget$|^TestSkillLearning(ExtractorIsolatedBudgetedAndStrictJSON|ConsolidatorIsolatedBudgetedStrictAndShortlistBound|MatchingEinoIsolatedStrictAndBudgeted)$' -count=1 -timeout=110s -v
go test ./internal/runtime -run '^TestSkillLearning(ExtractionSavedResultReusedAndVerified|ExtractionRevocationRejectsLateAndForgedOutput|ConsolidationGuardRevocationChangedSourceAndRevision|ConsolidationSavedResultRestartReuseAndCorruption|MatchingRejectsStaleConsentSourceHeadAndPattern|MatchingResultAtomicRecoveryAndForget|QueuedReviewACKDoesNotWaitForLaterConversation|ExperienceDiscoverySavedCandidateAndProposalRestartReuse|ExperienceDiscoveryHeadReceiptAndGenerationOutputGuards|ExperienceDiscoveryApprovalRevalidatesLiveAuthority|SourceReadOnlyRedactedCitationsAndDisabledRecovery|ReviewPersistedAdjudicationReconstructsProjection)$' -count=1 -timeout=180s -v
go test . ./internal/desktopconfig -run '^TestSkillLearningNativePipelineAutomaticReviewAndPublication$|^TestSkillLearningSettingsNative(AutomaticPipelineAndReview|NewCapabilityAutomaticallyDraftsAndRequiresReview)$' -count=1 -timeout=155s -v
```

### 生成前目录变更的有界自动重新匹配（2026-10-09）

本轮继续只实现自动技能进化链路。先以两轮普通 accepted 真人输入、原终态 outbox 和自动提取/归并/匹配复现：匹配完成后、generation 尚未预留前，正式技能目录/head 改变，旧 promotion 正确失效，但 producer 永久停在 processed input，没有新匹配，用户必须手工处理。新增 `TestSkillLearningMatchingReassessmentAutomaticallyReachesReview` 在修复前明确失败于自动队列；未改旧证据、降低独立门槛或用人工生成接口绕过。

新增 `internal/learning/matching_reassessment.go`，复用原 learning producer、旋转 cursor、shared dispatcher、全局/学习 pending 槽及 paid budget；原 pending 扫描之后每轮最多考虑一个新评估 hint。只允许当前 pattern、所有成员/贡献/共享/归并及原全部 shortlist 有效、原模型及 runtime access identity 相同、正式 catalog revision 真正改变、没有 generation 依赖/创建 lifetime 预留/任何人工控制时自动创建 fresh match/job/digest。旧 job/output/费用保持原样；活跃来源不排新工作。每个 pattern 生命周期最多两次额外评估、一分钟冷却、从首次 match 起三十天窗口。重新评估成功后，仅当前 input 绑定可作为 discovery hint；历史查询保留旧匹配但不能拿旧结果生成。模型新判断为 `already_covered` 时不再起草。已生成的 pattern 不因自身发布递归生成新候选，也不重选新能力名称/来源绕过 lifetime 预留。

原 matching reader 拆出纯 storage/binding 校验，在 live inventory 分类之前先核对不可变 input digest、由完整快照重建的固定 ID、result hash/decision/projection 和当前 ACK；损坏输出不被包装成可重新评估的目录变化。仅宿主成功重建来源与正式 inventory 后的不一致得到 `inventory_changed`，旧 job terminal failed、不进行原 job retry；实际 Eino 维护错误转换保留该 sentinel 和原 conflict，而不输出私有诊断。创建新评估、ordinal、完整 snapshot/job 和当前绑定同事务提交。原 failed/skipped，以及结果已保存但终态 ACK 中断、再次校验因目录改变变为 failed/processed 的两种路径均能建立新评估，旧 deadline/attempts/result 不改写。一般来源/configuration、budget、deadline、cancel/model failure 不借此恢复；既往笼统 `configuration_changed` 终态也不回填成新许可。

新增七个顶层用例，共三十三个分支，覆盖匹配前/paid reservation 前/付费后、saved-result ACK 故障及重启自动 fresh assessment；冷却/三十天/两次 lifetime、共享 queue 背压和无部分消费、全局 paid budget、transactional rollback、hash 和真实 rehash 篡改、取消、忽略后恢复、人工裁决、selection/model 改变、缺 stage input、已生成待审及 new capability 创建 lifetime 预留拒绝。共享来源两个普通独立会话完成后，目录更新重新匹配继续到完整行为评测和受限自动发布，单个观察任务继续到人工审核；后续普通追问没有变成第三个独立票，零 fabricated operator feedback。共享成员损坏和入队后撤销 sharing 会传递取消/清理全部新评估身份，原费用仍保留。正常后续 accepted task 的新 pattern revision 继续用原 stage input 自动匹配，不人工拆任务或手工发生成请求。原生 matcher 两个 httptest 分支实际验证 reservation 前零 provider call、付费后回执保留、重启后新的单 attempt 新快照，不把旧输入重试。

首轮新否决 fixture 暴露 metadata `json_extract` 对破损 snapshot 报 malformed JSON，已改成有界 hint 的 `json_valid` 防护后由 reader fail-closed，不恢复破损来源。自动发布 fixture 最初用直接 Registry enable/disable 模拟目录更新；这会造成正式 publication history 与 current head 不一致，被原 observation guard 保守拒绝，人工批准则合法。定位后保留原生产守卫不变，自动发布分支改为模拟已审核的真实 baseline publication，验证正常 head 更新可自动继续；直接 enable/disable 仍用于其他匹配/人工审核/否决分支。诊断输出和临时辅助批准已移除，`auto_patch_worker.go` 内容及原换行恢复一致；没有把失败 fixture 当作通过或顺手扩大 publication 行为。

最终完整新增选择器加原 matching ACK/migration 两个直接回归通过 runtime 114.819s；另十二个 directly affected matching/source/discovery/adjudication 回归通过 78.429s，五个临时活跃/共享撤销/重启删除直接回归通过 55.865s。维护预算/有限重试/Eino 原 parser 与 admission 回归通过 maintenance 5.836s、Eino 9.272s。最后相同生产源码的真实 SDK 和正式设置保存/启动全链再次通过：SDK 39.216s（两分支 30.90s），desktopconfig 67.034s（已有两分支 38.39s、新能力两分支 27.07s）。全部为临时数据库/设置、真实 Eino 本机 httptest、认证 MANAGE、原自动/人工发布与重启读取；没有全包测试、race、GPUI build/视觉或外部付费模型。原 grants 默认关闭，有效设置、个人数据库和正式技能未改。新语义匹配质量及可信 suite 代表性仍没有真实标注验收，目标继续进行。

```powershell
go test ./internal/runtime -run '^TestSkillLearningMatching(ReassessmentAutomaticallyReachesReview|ReassessmentFailedInputRestartAndFreshDecision|ReassessmentBoundsCooldownAndQueueBudget|ReassessmentCannotReviveDeniedOrGeneratedWork|ReassessmentSharedSourcesAndAutomaticPublication|NewPatternRevisionUsesOrdinaryAcceptedEvidence|ReassessmentNativeInventoryChangeIsNotModelRetry)$|^TestSkillLearning(MatchingResultAtomicRecoveryAndForget|MatchingMigrationDoesNotResurrectMissingStageInput)$' -count=1 -timeout=120s -v
go test ./internal/runtime -run '^TestSkillLearning(MatchingPinsTargetAndDoesNotFabricateReadsOrGeneration|MatchingActualReadsPrioritizedButOldVersionNotCurrent|MatchingRejectsStaleConsentSourceHeadAndPattern|MatchingResultAtomicRecoveryAndForget|MatchingRestartRejectsChangedHostSelectionAndModel|MatchingMigrationDoesNotResurrectMissingStageInput|MatchingQueueBackpressurePreservesEvidenceForLater|ExperienceDiscoveryOptInIndependentEpisodesAndManualPublication|ExperienceDiscoveryHeadReceiptAndGenerationOutputGuards|ExperienceDiscoverySharedQuotaCrossTriggerDedupAndAtomicity|ExperienceDiscoveryQueuedCancellationNeverRegenerates|ReviewPersistedAdjudicationReconstructsProjection)$' -count=1 -timeout=150s -v
go test ./internal/runtime -run '^TestSkillLearning(InFlightActivityCannotHidePermanentSourceChanges|InFlightSharedActivityValidatesAllPinnedMembers|InFlightNativeMatchingPreservesAdmissionCauseAndPaidResponse|WorkspaceSavedMergeMatchingAndRestartPolicy|WorkspaceSessionDeletionCleansForeignDerivedRows)$' -count=1 -timeout=125s -v
go test ./internal/skillmaintenance ./internal/engine/eino -run '^TestDurableMaintenanceSharedQuotaAndRestart$|^TestMaintenanceUnknownCallsFencingAndDeadline$|^TestSkillLearningSourceActivityRetainsOriginalAttemptAndDeadlineLimits$|^TestTypedMaintenanceLearningBackpressureKeepsSkillSlots$|^TestSkillLearningMatchingEino(IsolatedStrictAndBudgeted|ParserRejectsTypesCaseAndExtraJSON)$|^TestSkillLearningNativeAdaptersPreserveTemporarySourceAdmission$|^TestSkillMaintenanceReviewChargesBothStages$' -count=1 -timeout=80s -v
go test . ./internal/desktopconfig -run '^TestSkillLearningNativePipelineAutomaticReviewAndPublication$|^TestSkillLearningSettingsNative(AutomaticPipelineAndReview|NewCapabilityAutomaticallyDraftsAndRequiresReview)$' -count=1 -timeout=155s -v
```

### 委派任务的自动经验与技能进化（2026-10-09）

本轮仍只收敛自动技能进化。先以真实 accepted foreground input、终态 outbox、native background task 和本地完成回执复现缺口：同步子代理的本地观察本来就在前台 run，独立后台 child 此前却只参与 settled 判断，不进入提取。主 Agent 只负责委派、子任务完成实际验证时，自动经验主链没有可引用的本地执行来源。新增实际 native-host fixture 在生产修改前明确失败于缺经验；没有补造真人输入、人工提交经验或要求用户手工发生成请求。

新增 `internal/learning/root_evidence.go` 与 `internal/runtime/skill_learning_roots.go`。`Open` 启动时固定接入宿主 root reader，包括关闭付费 learning 后的只读来源恢复；不增加 schema、worker、工具或授权开关。原事务内重建 accepted root、全部物理成员 ancestry、双份 binding/spec、parent/child policy、角色/树投影、native terminal task、budget attempt/outcome 与原委派开始/终态回执；通知继续走既有 continuation/role-context provenance reader。固定 sorted 成员、input/frontier/status/ended 和 host evidence hash 写进新 extraction identity，付费预留、输出提交、归并/匹配/生成、来源读取及审批都复用 live snapshot guard。新增或改变 descendant、input、receipt、binding、native result、parent policy、tree parent、notification 或角色来源不能静默扩展/rebase。blocked/reconciliation 原本被误分类为 storage retry，新增分支实际复现后改为永久 `evidence_changed`；费用与已消费证据不退。

一次最多八个 accepted foreground turn，加总最多八个 delegated/continuation run；原 528 references、模型输入字节、每 run 512 events/4 MiB、32 receipts/32 reads，以及归并事务缓存 32 source runs 上限保持。新快照额外序列化上限为原模型 input limit 加 32 KiB；超界在 model I/O 前 `source_bounds`，unsafe root 在入队前 `unsafe_root_evidence`。委派仅贡献实际 skill-read 身份和有界本地脱敏回执；网络/MCP/未知工具仍 hints。私有 child instruction/user/assistant/reasoning、native result/notification 正文以及 `task`/`background_agent`/`skill_manage` 不进入模型或来源正文。原前台-only capture 不扩展范围；Eino extractor prompt 从 v1 升到 v2，未完成旧 native prompt job 按模型 identity conflict 保守终结，不迁移或退款。

完整且真实引用的 child 本地完成回执归回原真人 source/root，要求前台和所引 child 都 completed；窗口取较晚结束时间。failed child 可保留诊断，但不能成为成功 promotion。子任务、嵌套委派和 notification continuation 均不增加 episode、独立票或 operator feedback。单任务仍系统自动起草、人工只审批；完整独立授权任务才可能在原 trusted suite 与学习 auto_patch grant 下发布已有 managed 小补丁。新技能仍人工，观察/回滚和原消费/冷却/预算保持不变。

新增七个 runtime 顶层用例合计 34 个分支，最终分组全部通过：单任务自动待审、11 项固定快照/实时来源否决、七项 bounds/external/synthetic/failed/unsettled、五项 restart/disabled inspection/consent/approval/read attribution 的完整一组 59.178s；独立共享自动发布/共享撤销/子来源变化、真实 v5 嵌套/v6 后台通知/前台 continuation/通知篡改、实际 Eino reservation 前与付费响应后否决一组 108.200s。没有把早期 Subagents fixture、错误分类、重复 outbox 或共享清理错误的失败运行计为通过。

新增 SDK `TestSkillLearningNativeDelegatedPipelineAutomaticReviewAndPublication` 两分支使用真实 `Open`、Eino 前台和 native background custom worker、实际维护各模型适配器及认证 MANAGE。前台仅委派，child 实际读取原技能和本地 JSON；单任务自动待审、两独立共享任务自动发布，baseline 定向失败到 candidate 通过、回归保持通过。全部维护回执 10/13 条分别结算、每条 125 tokens，零 fabricated feedback。关闭所有学习/生成/评测能力重启仍能检查原脱敏来源，再一次真实委派读取已发布版本；只有正常前台/child 调用增加，维护不重跑，原导入技能文件不覆盖。最初 SDK fixture 未允许原 `task` 主开关，随后默认 SDK approval 又使 child 停在等待；均只改临时 fixture 的明确工具策略与 `PermissionModeOff`，未放宽生产委派或学习准入。

最终同一生产源码的原生 SDK 两个顶层测试通过 84.502s（新委派两分支 38.85s、原多轮两分支 38.38s）；正式设置保存/启动的已有技能和新能力两条自进化链通过 desktopconfig 98.915s（62.95s/25.39s）。十个 extraction/redaction/source 直接回归通过 runtime 77.979s，五个 actual-read/review-draft/activity 直接回归通过 67.923s，transitive shared revocation 两分支单独通过 6.692s；Eino strict parser、agent attribution、原 source admission 七阶段通过 9.944s。后者 runtime 选择器曾包含一个不匹配的旧 workspace 名称，未把它计为通过；随后运行下面明确的当前撤销选择器。没有全包、race、其他后台业务测试、桌面 build/视觉或远程付费模型评估；grants 默认关闭，有效配置、个人数据库和正式技能未改。14 个拥有 Go 文件及两个文档 ASCII/gofmt/whitespace/EOF 检查通过，tracked diff 沿用仓库 CRLF 规则通过。

```powershell
go test ./internal/runtime -run '^TestSkillLearningDelegatedExecution(AutomaticallyReachesReview|SnapshotAndLiveAuthorityGuards|BoundsExternalAndNoSyntheticVotes|RestartReadsAndRevocation)$' -count=1 -timeout=135s -v
go test ./internal/runtime -run '^TestSkillLearningDelegatedExecution(NestedAndNotificationContinuations|NativeExtractorAdmissionAndView|IndependentSharedAutoPublication)$' -count=1 -timeout=120s -v
go test ./internal/runtime -run '^TestSkillLearning(ExtractionDebounceRestartAndRedaction|ExtractionChunkingPreservesSingleEpisode|ExtractionCompletedEpisodeDoesNotWaitForNewTask|ExtractionSavedResultReusedAndVerified|ExtractionUnsafeAndUnsettledDoNotCall|ExtractionRevocationRejectsLateAndForgedOutput|RedactionRehashedLegacyEnvelopeCannotAuthorizeExtraction|RedactionHiddenTextCaptureAndLegacyView|SourceReadOnlyRedactedCitationsAndDisabledRecovery|SourceToolReceiptLiveBytesAndIdentityGuards)$' -count=1 -timeout=130s -v
go test ./internal/runtime -run '^TestSkillLearning(MatchingActualReadsPrioritizedButOldVersionNotCurrent|SourceActualSkillReadUsesSavedVersionAndInvalidatesChangedRead|ReviewDraftNeverAutoPublishesButFullEvidenceStillCan|ReviewDraftAutomaticSingleEpisodeAndDisabledDiscoveryApproval|WorkspaceSourceWithdrawalTransitiveAndIndependentPublishedAsset|InFlightActivityCannotHidePermanentSourceChanges)$' -count=1 -timeout=135s -v
go test ./internal/runtime -run '^TestSkillLearningWorkspaceRevocationCleansTransitiveDerivedTextAndFencesWorker$' -count=1 -timeout=55s -v
go test ./internal/engine/eino -run '^TestSkillLearning(ExtractorIsolatedBudgetedAndStrictJSON|TextProjectionRetainsAgentAttribution|NativeAdaptersPreserveTemporarySourceAdmission)$' -count=1 -timeout=50s -v
go test . ./internal/desktopconfig -run '^TestSkillLearningNative(DelegatedPipelineAutomaticReviewAndPublication|PipelineAutomaticReviewAndPublication)$|^TestSkillLearningSettingsNative(AutomaticPipelineAndReview|NewCapabilityAutomaticallyDraftsAndRequiresReview)$' -count=1 -timeout=195s -v
```

### 同任务后续验证自动解除历史阻塞（2026-10-09）

本轮继续实现“自动进化、人工只审核”。先用实际 accepted user、终态 outbox、正常提取/归并复现阻塞：第一轮未验证形成 unresolved 成员，第二轮真实完整本地验证并引用两轮输入后，原归并虽替换了 representative，却因保留任何未验证成员永久保持 unresolved；匹配不会启动，原 promotion 也拒绝该历史成员。新增 `TestSkillLearningLaterVerificationAutomaticallyResolvesOriginalTask` 在修复前明确失败，没有通过删除原经验、手工选目标、拆 episode 或发生成请求规避。

新增 `internal/learning/resolution.go`。新的原 consolidation snapshot 固定 `procedural-same-task-resolution-v1` 可选策略，纳入原 job identity/input digest；pattern snapshot 保存 bounded prior/verified link。只有已判定 equivalent 的成员，同实际 session/episode/consent epoch/host independence key、不同 extraction，后者真实引用原经验的全部非截断真人输入、更晚真人输入及更晚 turn 的完整本地 completed receipt，才解除该历史阻塞。顺序来自 accepted event sequence、原所有 foreground/child frontier 和 outbox sequence；不使用 model reason 或经验时间。原 observationEnded 继续要求来源前台及所引 child completed。每成员取最早合法 accepted sequence，再按 ID 固定，最多原 32 members / 32 source runs，没有增加 model stage、schema、worker 或授权。

History 读重建固定 capture identity、原成员和 link 投影，不冒充 live source 已认证。完整 pattern 路径继续实时核验每个原 extraction、原 outbox/input/receipt/root、贡献/共享/workspace；promotion 允许保留已解除阻塞的历史成员，但不把它变成 observed 或新增观察，只从实际 observed 成员组成独立证据。原 outcome、引用、所有成员及传递撤销边都不删改；较晚未验证问题、其他 episode、同源复制会话、冲突和歧义仍阻塞。单任务自动待审永远人工，受限自动发布独立任务、trusted suite 和 grant 门槛不变。

兼容使用 omitempty 字段，旧 capture/job/result 原语义与哈希不改。新增实际 runtime 旧协议 fixture 按原字段顺序、原 consolidation ID、真实 job storage key/input digest 建立旧 queued job，验证两轮 legacy merge 仍 unresolved、重启后 saved ACK 无模型重跑；第三轮普通真人输入的新 revision 才采用新策略并正常自动 matching，旧 revision 字节不变。Eino extractor identity 从 v2 升到 v3，提示纠正链引用原输入、后续纠正及当轮回执；未知策略或旧 native identity 不自动迁移、rebase 或退款。

新增两个 learning 顶层用例包含 39 项来源/投影分支，覆盖全部早期输入引用、当前真人引用、旧 receipt、外部/失败/截断、accepted/outbox/child frontier、同 task/epoch/workspace、delegated completion、legacy 精确投影/票数与未知/伪 link。新增八个 runtime 顶层用例覆盖自动待审/人工发布/关闭后来源读取、六项未覆盖或较晚问题、六项 restart/rehash/live source、结构有效但缺真实原引用的重新 hash link、原生 delegated 后续纠正、贡献撤销清理及非退款、共享复制 lineage 与撤销、旧 queued/result 兼容。所有最终精确选择器通过；原旧 unresolved 独立证据、冲突/歧义、归并 saved/revocation、shared restart、matching ACK、人工 draft、auto-patch、来源读取和永久错误优先直接回归也通过。

新增 SDK `TestSkillLearningNativeFollowupResolutionAutomaticReviewAndPublication` 两分支复用实际 `Open` / Eino foreground / 全部维护适配器和认证 MANAGE。第一轮只输出“仍需验证”，真实提取为 unresolved 并归并，等旧阶段完成才接受第二轮普通纠正；第二轮实际读取技能及本地完整 JSON，模型输出引用两轮输入和后一 receipt。人工分支自动生成一个 review-only proposal，自动分支用两个不同初始输入的共享真人任务，在实际 baseline targeted 失败到 candidate 通过、regression 保持通过后发布同一候选。保留 2/4 个原经验、1/2 条 resolution link、1/2 个独立观察；12/17 个维护回执全部结算、每条 125 tokens，无 operator feedback，无工具重放。关闭学习重启后实际读取已采用版本，维护不再运行，原导入文件不改。

最后同一生产源码的 SDK 原多轮和新增后续纠正链全部通过（package 163.155s，新两分支 83.41s），Eino parser/admission 选择器通过。与 SDK 并行的首次正式 settings 合并命令中，已有技能自动分支的原严格两轮 fixture 报 bounded view 不符，整条合并命令未计通过；单独原失败分支随后通过，原已有/新能力两个 settings 顶层的最终完整精确选择器亦通过。没有因该 fixture 失配放宽生产来源/队列或修改固定两轮断言。早期 unit fixture 用字符串 resolution 误匹配 candidate 字段的断言已修正为精确可选 JSON key；一次合并 runtime 的 120 秒 timeout 栈停在后续新临时库 WAL WriteFile，没有单连接 pool 重入栈，拆开同一原精确范围后全部通过。DLL 初始化失败的命令没有启动测试，不计通过。上述失败/超时都没有算作验收成功。

本轮只拥有 11 个 Go 文件及两个文档；最终拥有文件 gofmt/ASCII/whitespace/EOF 检查通过，沿用原 CRLF tracked diff check。不测试全包、race、其他业务、桌面构建/视觉或外部付费模型；没有修改用户有效设置、个人数据库、已采用技能或原 ACP/MCP 变更。真实语义纠正/匹配质量与可信 suite 代表性仍未验收，整体目标保持进行。

```powershell
go test ./internal/learning -run '^TestSkillLearningResolution(RequiresSourceCoveredLaterCompletedTask|ProjectionPreservesLegacyAndOriginalVotes)$|^TestSkillLearningConsolidationPatternCountsHostIndependenceOnly$' -count=1 -timeout=30s -v
go test ./internal/runtime -run '^TestSkillLearningLaterVerificationAutomaticallyResolvesOriginalTask$|^TestSkillLearningResolvedTaskWithdrawalCleansGeneratedDraft$|^TestSkillLearningSharedLineageCannotResolveAnotherConversationsHistory$' -count=1 -timeout=100s -v
go test ./internal/runtime -run '^TestSkillLearningResolvedTaskCheckpointRevalidatesLinksAndLiveSources$' -count=1 -timeout=150s -v
go test ./internal/runtime -run '^TestSkillLearningResolutionLegacyJobsKeepFixedSemanticsUntilNewInput$|^TestSkillLearningLaterVerificationCannotResolveUncoveredOrLaterIssues$' -count=1 -timeout=150s -v
go test ./internal/runtime -run '^TestSkillLearningResolutionRehashedUnsupportedLinkCannotCertifyHistory$' -count=1 -timeout=45s -v
go test ./internal/runtime -run '^TestSkillLearningLaterDelegatedVerificationResolvesOriginalTask$' -count=1 -timeout=60s -v
go test ./internal/runtime -run '^TestSkillLearningConsolidation(DistinctEpisodesAndUnresolvedEvidence|ConflictAmbiguityAndForeignTarget|SavedResultRestartReuseAndCorruption|GuardRevocationChangedSourceAndRevision)$|^TestSkillLearningWorkspaceSavedMergeMatchingAndRestartPolicy$' -count=1 -timeout=180s -v
go test ./internal/runtime -run '^TestSkillLearning(MatchingPinsTargetAndDoesNotFabricateReadsOrGeneration|MatchingResultAtomicRecoveryAndForget|ReviewDraftNeverAutoPublishesButFullEvidenceStillCan|SourcePatternMembershipWorkspaceConsentAndRevision|InFlightActivityCannotHidePermanentSourceChanges)$|^TestSkillLearningAutoPatchAdmissionAndRevocation$/on$' -count=1 -timeout=160s -v
go test ./internal/engine/eino -run '^TestSkillLearning(ExtractorIsolatedBudgetedAndStrictJSON|NativeAdaptersPreserveTemporarySourceAdmission)$' -count=1 -timeout=65s -v
go test . -run '^TestSkillLearningNative(FollowupResolutionAutomaticReviewAndPublication|PipelineAutomaticReviewAndPublication)$' -count=1 -timeout=210s -v
go test ./internal/desktopconfig -run '^TestSkillLearningSettingsNative(AutomaticPipelineAndReview|NewCapabilityAutomaticallyDraftsAndRequiresReview)$' -count=1 -timeout=180s -v
```

## 5. MVP 后续事项

- P1/P2 模型质量验收尚缺真实代表性标注样本；确定性 fixture 不证明纠正识别、语义归并或技能目标匹配准确率。有界结构化/文本脱敏不是防泄漏的绝对保证，高敏感会话应完全不贡献。
- Episode 不自动语义拆分。跨会话 lineage 和精确初始输入保守去重已实现；未声明的改写复制无法保证识别，未来原生 copy/fork 必须接 host lineage。单任务自动待审草案不要求人工拆任务，但自动发布仍需完整 host 独立来源；独立票不是质量验收。
- 已入队和执行中的工作都能针对普通追问的暂时活跃自动等待/原作业有限重试，批准也不会仅因后续对话永久过期。原 3 attempts、首次 deadline 和付费额度耗尽仍终结；未合法 checkpoint 的输出可能重复计费，不承诺无损/无限恢复。生成前目录/head 改变已有最多两次/一分钟冷却/三十天窗口的自动新匹配评估，后续真人任务的新 pattern revision 使用正常 stage input；已生成候选、人工控制和来源/configuration 等永久错误不自动重做，不隐式 rebase 或退消费。直接 Registry enable/disable 导致正式 publication history 与 head 不一致时，原自动发布 observation 守卫仍会保守拒绝；本轮不放宽该原守卫。
- 后台/通知不是独立真人输入；授权 root-level 视图已接入实际子任务读取和本地脱敏回执，保持八个 delegated/continuation run 总上限和固定来源重核验。私有对话/回答、native result/notification 正文仍不进入学习，超界、失败/停止或来源变化不会因委派获得额外独立票或自动发布权限。
- 同任务后续纠正已能通过原引用覆盖和更晚真实回执自动解除历史未验证阻塞；原历史与撤销边保留，不需要手工进化。它不推断无引用语义覆盖、跨 episode/复制会话覆盖或任意成功；原八轮捕获窗口之外的历史输入若未在固定后续视图中真实引用，则继续 unresolved，不静默扩大上下文、补造引用或恢复旧 paid job。
- P2 consolidation、授权 matcher、workspace sharing/lineage/传递撤销、已有 skill 与独立 opt-in 新能力 experience discovery、统一 episode 消费、目标 busy/cooldown、单 pattern 自动创建去重与生成依赖、最多 3 次原来源/名称/修订的显式人工重试已实现。桌面已有 host gate、discovery pins、原提案 diff、学习来源/history、逐会话贡献/共享、单条忽略、人工已有目标裁决及重试 prepare/确认源码；桌面构建/视觉验收仍未完成。人工裁决或重试不退旧消费、不自动或人工换名/来源绕过 lifetime 创建预留。
- 直接来源检查的 SDK/MANAGE 和按成员、只读、单份缓存桌面入口已实现；仅脱敏 cited capture references，不返回完整原始回执或认证归并/匹配质量。实际桌面类型/渲染/daemon 往返验收仍未完成；不得把隔离逻辑测试当作已通过桌面产品验收。
- P3 已接入新学习来源受限发布、正文定向改善/回归、description 正负 metadata-only 首次选择改善、发布前 live provenance 与来源撤销/观察回滚联动。固定 host suite 只证明指定目录和 case，其他 frontmatter 和新技能仍人工。可信 suite 是否代表真实任务、留出样本效果和质量指标仍未验收；默认关闭，不应作为全面自治进化宣传。

本阶段可以表述为“自动提取/归并/匹配经验、生成待审提案；在独立授权和可信正文／正负触发评测合格后受限自动发布已有 managed 技能小补丁并观察回滚”。不能表述为“所有 skill 自主进化”“description 无条件自动采用”“新技能自动发布”或“真实模型质量已验收”；本机有效配置与正式技能仍未改动。按最新 MVP 要求本次实施收口，以上后续事项保留记录，不继续扩大验证范围。
