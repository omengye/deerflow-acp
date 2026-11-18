# 模型记忆写入协议 v2

本页描述阶段 2–5 及后续模型指引修正后的写入、预算、Flush、索引与安全重新学习协议，本轮审查补修的发布状态见[修复与发布报告](memory-review-fix-report.md)。管理端 CRUD、显式 JSON 导入仍由操作者审阅提交；它们不代表模型可以绕过本协议。实现入口为 [write_validation.go](../go-harness/internal/memory/write_validation.go)、[extraction_v2.go](../go-harness/internal/memory/extraction_v2.go) 和 [relearn.go](../go-harness/internal/memory/relearn.go)，最新验收见[真实模型修复报告](memory-model-fix-report.md)，历史阶段结果见[阶段 5 报告](memory-stage5-report.md)。

## 写入字段与支持范围

模型提交 key、category、原文 value、完整 evidence 和 evidence_start/evidence_end。位置从原始已接受用户文本起算，单位为 Unicode code point，结束位置不包含在证据内；不能先 trim 再计数。多段文本按接受记录以换行连接。宿主校验来源、完整子句、取值和字段类型后生成 content；模型可省略 content，若提供则必须与宿主模板完全相同。

例如用户输入“请记住，我在北京”，显式工具参数为：

~~~json
{"scope":"global","key":"profile.last_reported_location","category":"profile","value":"北京","evidence":"我在北京","evidence_start":4,"evidence_end":8,"expected_revision":0}
~~~

| key / category | 支持的完整声明示例 | 宿主生成的正文 |
|---|---|---|
| profile.name / profile | 我叫小鹿；我的名字是小鹿；My name is Li | 用户姓名是小鹿。 |
| profile.last_reported_location / profile | 我在北京；我住在北京；I live in London；I am in London | 用户最近告知所在地是北京。 |
| preference.language / preference | 以后请用中文回答；我偏好中文回答；Please answer in Chinese | 用户偏好使用中文回答。 |
| preference.answer_style / preference | From now on, please answer concisely in Chinese；Please answer concisely in Chinese；Continue to answer concisely in Chinese | User prefers concise Chinese answers |
| profile.hardware / profile | 我的硬件是 NEW-BOARD；My hardware is NEW-BOARD | The user's hardware is NEW-BOARD. |
| project.database / project | 项目数据库使用 SQLite；The project database is SQLite | 项目数据库采用 SQLite。 |
| project.path / project | 请记住项目路径是 src/app/main.go | 项目路径是 src/app/main.go。仅限显式写入 |

语言规则同时支持中文、英文/英语、Chinese、English；英文大小写不敏感，但 value 必须保留用户原值。name/city 等兼容别名仍归一到现有 canonical key。名称/地点只允许有界的字母、地点数字、空格、点和连字符；硬件/数据库值只允许有界的标识符。完整规则以实现和测试为准，未支持的自然语言表达会拒绝，不猜测同义改写。

通用 dotted key 只接受“记住/更正/remember/correct”后的有限描述语法，value 为整个事实片段。首版支持：

- 饮品：茶、咖啡、牛奶、水 / tea、coffee、milk、water。
- 颜色：红、蓝、绿、黑、白对应的“红色”等完整词及 red、blue、green、black、white。
- 编辑器：VS Code、Vim、Emacs。
- 项目采用的语言或数据库：Go、Python、Java、TypeScript、Rust、SQLite、PostgreSQL。

例如“请记住：我喜欢茶”可写 preference.drink，value 为“我喜欢茶”。任意文本并不会因放在“我偏好”后面就成为可支持事实。项目/资料/偏好必须使用对应 key 前缀。引用、代码、否定、转述、歧义、秘密、权限与执行指令拒绝；自动提取另拒绝路径、URL 和 markup。全源检查保守，混合陈述可能整体不写；不支持的表达可以重述或通过管理入口人工审阅提交。

`project.path` 是已登记的 typed 字段，不是通用 dotted key。用户说“请记住项目路径是 src/app/main.go”时，value 必须为 `src/app/main.go`，不能包含“项目路径是”或记住请求。2026-11-17 的模型指引修正补齐了该区别及可执行示例；宿主仍以 `unsupported_value_or_content` 拒绝错误整句值，没有通过放宽校验来修复模型调用。

forget 不接受 value/content。当前用户必须明确要求删除目标，可以使用已登记字段别名、精确 fact_id 或精确事实正文；按 ID 的别名匹配由宿主读取目标核对，未知 ID 不能凭一个不相干别名删除。后续变更仍校验 scope 和 revision。get_memory_state 不返回已删除正文。

## 两条自动提取管线

普通模式允许宿主选择的 workspace/session/user，禁止 global。single_user 模式允许个人 profile/preference 写 global，project 写 workspace；保留渠道表达风格排除。

两条提取管线都把已接受原文放在宿主构造的 JSON envelope 的 `user` 字段中；可选 `assistant` 仅作上下文。提示词明确要求先解码 `user`，JSON 定界引号和转义不是用户自身的引用，证据位置也不从 JSON envelope 起算。解码后用户文本内部的引用、代码及指令仍受原校验约束。指引提供 canonical key 映射、最近报告地点/长期回答语言的含义及真实可解析的正例；示例的原文、范围和证据位置通过同一个宿主解析器测试，不能把示例事实写入实际用户库。

~~~json
{"schema_version":2,"facts":[{"scope":"global","key":"profile.last_reported_location","category":"profile","value":"北京","evidence":"我在北京","evidence_start":0,"evidence_end":4,"durability":"durable","authority":"descriptive","confidence":0.9}]}
~~~

来源正文最多 8192 字节，响应最多 16384 字节，envelope 最多 8 个候选，最多接受 4 条。schema/顶层字段、截断、超限错误是整包格式错误；候选未知字段、重复字段、缺少或非法 metadata、证据不成立等只拒绝该项。confidence 必须存在且为 0.75–1，但不替代来源验证。相同 scope/key 的不同值全部拒绝，相同值保留首项，其余记录 duplicate。

原始合法 envelope、摘要、schema/policy、provenance 与原始 ordinal 持久化。普通模式按 input 保存结果，再按 attempt staging，终态与预算/业务同事务发布；single_user 按 job 保存结果，ApplyJob 将整个有效批次及回执原子提交。批内故障整体回滚，恢复复用原始结果与序号；完整提交后的重复 Apply 不增加事实或回执。内容拒绝与 superseded 可逐项处置，数据库故障不能伪装成内容拒绝。

普通模式最多两次格式尝试，均调用 tracked model；single_user 保留最多三次提取尝试，每次实际分派均独立预留与计费。全拒绝/空结果是 no_facts 终态，不再次请求模型。格式失败在 usage 完整时结算后有界重试；usage 不明进入 blocked/budget_unknown，不自动重发。每日上限仍是附加限流，不能替代来源 root 的预算。

## 来源、恢复与故障

provenance 与 policy_version 分列：user_supported 表示通过受限用户支持规则且未标记外部上下文；external_context 表示上下文可能含工具/记忆/历史内容，不能据此认定事实缺少当前用户证据；unknown 表示历史来源无法验证。external 标签本身不要求每次确认，证据缺失、冲突、陈旧或影响当前决策时再确认。三者都只是描述性数据，不能授权工具。记忆入口 UntrustedMemoryContentTool 将所有工具名默认视为不可信，包括本地文件、命令、历史、记忆、未知工具及失败/不确定回执。

技能学习继续使用兼容入口 ExternalContentTool，保留其既有网络内容分类。记忆不再用这个旧分类作为信任边界。扩大回归发现直接改共享函数会使所有依赖工具证据的技能自动发布失去原准入路径，因此按 F1 的兼容选项拆分入口，并增加技能学习分类回归；本次不声称同时修复技能学习的全部本地内容风险。

Promote 保留源 provenance；旧 external 策略迁移保留 external_context，其余旧事实和没有可验证上下文的导入保持 unknown。single_user 恢复时只更新模型看到的投影标签并移除已删除/更正项，固定 checkpoint 状态不改写。

schema 账本新增 version=2 / memory-write-policy-v2；来源协议仍为 memory-source-v1，清单另带 memory-write-v2。当前新执行使用 **memory-tools-v5 指引契约、memoryGuidanceVersion=1**，参数 schema 仍为 V3。固定 V3/V4 的执行继续使用原指引与参数 schema；缺少 guidance 字段的旧执行保持旧注入说明。V1/V2 旧 schema 可恢复执行但不产生新记忆效果。本轮不新增 schema 迁移，数据库仍为 5。新 job 仍为 version=2，要求 write_policy、extraction_schema=2、budget_policy，并使用 extract/v3 身份域。历史回执、迁移 checksum 与来源快照不重写。

审查补修的有限语法：裸“我在 / I am in”仅接受源码 `bareLocations` 的已知地名，按 NFKC/大小写比较、保留原 value；其他地名需“住在 / 我的城市是 / I live in”明确类型声明。自动和显式路径都不将“我在写代码/开会/想”视为地点，不承诺任意地名识别。generic evidence 可以包含整个记住请求；若只包含事实，宿主仅在原文中紧邻且完整的命令前缀可证明时补入授权，异句命令、引用/条件/否定不得补入。独立任务词不再整段拒绝干净事实，词内撇号按单词处理；引用、跨句转述、条件和权限/凭据仍保守扫描。

遗忘请求支持 忘记/忘掉/删除 与 forget/delete (that)，完整第一人称事实精确匹配目标正文；只有属性别名比较会去掉 我的/my/the 等有限前缀，不使用子串删除。

终态未调用 PostRun 的输入写 postrun_skipped；等待恢复、实际提取失败和未知预算不改为跳过。历史修复只在完整执行/预算/事件证据证明时进行。队列来源完整性失败按选中数据库行处置，先结算已保存 usage，保留响应与 unknown hold；临时 SQL/核心持久化失败继续返回错误。

阶段 3 新增 schema version=3 / memory-job-budget-v1 与 memory_job_leases，前两次迁移 checksum 不变。新 job 另需 budget_policy=memory-job-budget-v1 和宿主绑定的 member_id；缺少预算协议的阶段 2 queued/running job（即使已有 result）按 legacy_unverifiable 处置，并保留 legacy_usage_unavailable。历史事实、回执及 completed job 不重写。

独立可选 SQL 事务只有显式确认回滚后才允许降级。审计失败以固定原因码日志和计数兜底，不打印原文、SQL 错误或响应，不递归写审计；尽量另存输入决定。取消/超时、预算 unknown/持久化、fencing、提交不明及终态事务失败继续传播。终态发布仍与业务和预算原子提交。

## 附加每日上限

宿主 YAML 的 memory.daily_attempts 或 SDK 的 MemoryDailyAttempts 设置 single_user worker 每个 UTC 日的新调用认领上限，默认 200，范围 0–1000000；0 暂停新调用，已保存结果仍可重放。不影响普通同步提取的 root 预算。

计数在认领事务中持久化，重启不清零，跨日分桶。认领后尚未请求即退出也保守占用一次；这是附加限流额度，不是精确 usage 账本。升级首日依据旧 attempts 做保守初始化。配置属宿主 YAML 设置，重启时读取；本次没有修改实际安装的配置。

## 后台预算与恢复（阶段 3）

入队与 memory_job member 绑定同事务提交；来源 run/input/session/root 从已接受输入读取，并验证前台来源 attempt。worker 认领独立的 attempt/fence 与 30 秒 job 租约，心跳续租；预留及分派在同一预算账本中核对来源、member、租约、显式动作和删除代次。请求摘要包含模型身份与实际消息，按序列化消息估算输入并预留最多 1024 输出 token，再受来源 root 的剩余 token 和单次输出上限收紧。

每次 HTTP 分派必须取得 ReserveModel 和 MarkDispatched 的首次许可。provider 内部重试关闭，格式重试由持久队列调度。worker 读取到结束标志、usage 尾部与 EOF 后才把使用量视为完整；缺少尾部、断流、超时、取消或 HTTP 故障无法证明消耗时保留 unknown 与 token hold。本文不把缺少 usage 的响应计为零，也不靠自动重发进行探测。

响应正文、usage/完整性与操作身份先持久化，再在一笔事务中结算、结束 attempt、保存合法结果或安排格式重试。进程在响应保存后、结算前中断，可从该记录恢复；只有 reserved、从未分派的预留可以释放 token hold。认领和预留计数保持保守，不退回已经使用的每日认领额度或模型调用计数。

宿主持有数据库进程锁后，先恢复记忆调用，再运行通用预算中断恢复。没有可靠结果的 dispatched 调用变为 blocked/budget_unknown，不能重新入队；完整结果先完成结算，再仅重放落库，不新增请求或重复扣费。恢复只能清除本 attempt 已消解的 unknown，不能清除同 root 其他成员的未知状态。结果重放不占每日额度或新调用预留，但仍要求有效 job 租约、当前来源/删除屏障与 root 未停止。

root 永久额度不足终态 quota_skip；并发工作临时占用额度时保留 budget_held 和 available_at，退避期间不持有 active attempt；明确未取得预留的临时占用不耗尽三次提取机会，但仍计入每日认领上限。每日限流保留 daily_limit 与下一 UTC 日期的 available_at。已结算的结果发布失败时重放原结果；事实、回执和 completed 状态仍原子提交。Close 取消并等待 worker，后台失败不会改写已提交的前台 completed。

阶段 4 已实现异步 Flush 与独立索引，阶段 5 已实现下述受限的 tombstone 恢复。源码实现不表示完整修复已经满足部署门槛；本机安装没有更新。

## Flush 与索引恢复（阶段 4）

schema version=4 / memory-index-retry-v1 新增 memory_index_failures 与 memory_index_backoff，不改变前三次迁移 checksum、job budget_policy 或提取 schema=2。每个事实 revision/model 的失败次数、下次时间、终态及模型整体退避跨重启保留；服务故障不拆批，输入错误以每轮 15 次请求为上限优先拆到单条。取消不消耗失败次数，超时持久化服务退避。提交时重新校验 head/deleted/model，旧版本的索引状态不阻止新版本。

Flush 固定集合包含既有输入决定及旧 job，不以队列长度或事实数代替。completed/quota_skip、无 job 的 quota_skip、no_facts、requires_explicit 与显式动作都归入 skipped；预算未知保持 blocked，daily_limit/budget_held 为 pending，并保留可用时间。已保存且结算的响应只有原子发布后才完成；Flush 不驱动重放，不释放 hold，也不发起请求。

恢复将 failed/cancelled 来源的 queued job 事务性标记 discarded 并保留 disposition；可恢复来源保持等待，Flush 解释为 blocked/run_unresolved。仍有在途或未知计费时保留既有预算保护。会话删除先通过现有 active/reserved/dispatched/unknown/hold 检查，再同事务标记剩余任务 discarded/session_deleted，保留 no_active_call 处置证据；该标签不表示过去未付费，也不声称释放了未知费用。删除失败会回滚任务状态与处置记录。

状态字段、兼容错误与索引配置见[全局记忆说明](eino-global-memory.md#flush-与索引阶段-4)。终态索引失败可由 SDK MemoryIndexFailures 查询，当前不会自动清零相同版本的 terminal；更换模型或产生新 revision 后再建立索引。

## 删除后的安全重新学习（阶段 5）

schema version=5 / memory-relearn-v1 增加 memory_value_identities，按 fact_id/revision 保存宿主验证后的 canonical key、规范化值 SHA256、正文 SHA256 和规则版本。前四次迁移 SQL/checksum 不变。metadata 与事实 revision、操作回执、提取条目和 job 完成在同一事务提交；不另存用户原文、不回填旧值、不重编码旧回执。删除仍是逻辑撤销，历史 revision 保留，不等于物理擦除。

新接受的人工输入清单另带 relearn_policy=memory-relearn-v1；来源协议 memory-source-v1、写入策略 memory-write-v2、提取 schema=2、预算协议和操作 ID 域保持不变。旧输入没有该标识，不因升级、恢复 checkpoint 或重放已付费结果获得自动恢复权限。

自动恢复只限 single_user 的 keyed 写入，同时要求：来源在删除后被接受；固定 scope 删除代次和 slot revision 仍有效；完整当前用户声明通过同一个字段验证器；对应已删除值的可信结构化身份存在；新旧规范化值不同。验证结果通过私有证明绑定原始来源、key/category/value/content 及自动/显式路径，不能靠修改公开参数代替宿主验证。输入含遗忘/删除请求时，自动提取保守拒绝，不从混合指令中选择一个声明恢复。

| 属性 | 值身份规则 |
|---|---|
| 姓名、最后报告地点、硬件、项目数据库 | Unicode NFKC、case-fold，去空白及标点；大小写、全半角或模板变化不能制造新值 |
| 语言、简洁回答语言 | 中文/Chinese → zh；英文/英语/English → en |
| 项目数据库别名 | postgres/pg → postgresql；sqlite3 → sqlite |
| 路径、通用自由文本及其他属性 | 不支持自动恢复，要求显式请求 |

这些是有限的字段字面规则，不能识别任意地名翻译、人物别名或实体同义词。输入仍须先满足受限声明语法；规范化不扩大可接受的语法。

比较对象是 tombstone 之前最近一个 live revision。若该版本没有可信 metadata、正文摘要不符、仅有空 tombstone，或最后一次人工修改没有结构化身份，则继续要求显式恢复；不能跳过该版本去使用更早的 typed 值。旧事实即使正文看起来符合模板也不猜测回填；同内容 unchanged 不为旧版本补签身份。

显式模型工具恢复必须来自删除后新接受的“请记住/更正/remember/correct”请求，且请求作用于当前完整声明。“我在北京”本身不能恢复删除项；“请记住，我在北京”可以。同输入另一个不相干字段的记住请求不提供授权。工具先用 get_memory_state 读取 tombstone revision，再按 CAS 写入；历史输入即使读到新 revision 也不能越过删除屏障。经授权的直接管理操作仍可按当前 revision 恢复。

| 回执/条目状态 | reason 与含义 |
|---|---|
| updated | relearned_new_value：安全自动恢复，沿用 fact ID，新增 revision |
| requires_explicit | same_deleted_value、deleted_value_unverifiable、value_identity_unsupported 或 explicit_request_required；事实保持删除 |
| superseded | 旧来源/CAS 冲突保持既有状态；缺少新恢复来源条件时为 relearn_source_unverifiable |

回执仍为 schema_version=2，只增补可选 reason；旧回执重放先返回持久原值。requires_explicit 是终态，不重试模型；整批未发布且有此结果时输入决定同名，Flush 计入 skipped。同批其他有效 key 仍可提交，整体按 completed 计数，逐条原因保留。新 revision 可重新索引，旧 terminal 和迟到向量不能影响它；unknown 预算也不会被恢复行为清除。

升级、备份与安全回退步骤见[记忆升级运行手册](memory-upgrade-runbook.md)。
