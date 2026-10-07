# Eino 子代理、技能自进化与单运行时实施记录

更新日期：2026-10-04。当前前三轮可靠性、诊断和交付工作见 [固定范围与验收记录](eino-three-iterations-20261004.md)，审计修复见 [修复说明](eino-p1-resolution-20261004.md)。下文保留 2026-10-02 开发包和各阶段的历史记录；当前源码已补上 MCP/启动失败恢复、原生只读错误和 Waku 工具回执核对入口。真实 WSL 现有验收用例已通过。

分支：`codex/eino-harness-acp-refactor`。
本轮起点：`9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9`。
范围与完成门槛：[实施方案](eino-subagents-skill-evolution-plan.md) 的 P0–P7 和 33 项验收。

本文件保留各阶段历史记录。当前功能以顶部最新进展和文末阶段表为准；早期段落中的“待实现”描述其写入时点。按用户本轮要求，优先补齐功能、构建交付物后由用户手测，不再重复全量或竞态测试。

## 本轮整合与交付状态

- 最终生产 Go（含宿主 MCP、角色用量、前台产物分页、启动日志和设置实际值收尾）`go build -p 2 ./...` 已通过，日志 `eino-complete-integration-build.log`。
- 旧 Agent 导入提供角色草稿预览，保留空工具/技能限制并由用户保存应用；旧进化历史以内容摘要标识、只读归档及分页查看导入，不恢复旧作业/批准/观察状态，不执行旧脚本。可执行历史文件只记录省略标记及摘要。关键集成通过 4.266s，日志 `legacy-import-focused.log`。
- 本轮已产出 Release 配置的便携目录，来源是当前未提交工作树；`BUILD_SOURCE.json` 如实标记 `development-dirty`。未生成需要干净提交来源的发行 ZIP。最终桌面与 daemon 构建 17m22s、Bridge 构建 2.31s，Go 工具与 WSL helper 均完成，打包脚本 exit 0。包内配置工具的隔离初始化和只读历史查询通过；未启动 GUI，桌面交互由用户手测。
- Waku 已补齐 P4/P5 原生配置、自动发现记录与证据、自动发布观察及质量反馈、旧 Agent 预览导入和只读历史分页，并支持前后台产物分页保存及角色自身用量。设置页当前运行值按 daemon 成功加载配置投影并缓存，包含 P4/P5 限额、角色稀疏覆盖和非敏感工具参数，不展示密钥；最终 Release 构建覆盖这些改动。子代理及付费技能维护默认关闭。

交付目录：`C:\Users\ym200\.codex\worktrees\eino-harness-acp\deerflow-api\dist\desktop\DeerFlow-Desktop-Go-20261002-231039-634`。入口为 `deerflow-desktop.exe`，随包包含 `TEST_NOTES.zh-CN.md`、`BUILD_SOURCE.json` 和 `PACKAGE_FILES.sha256`。80 个文件，约 305 MiB；目录未复制或修改既有个人数据。

构建日志：`D:\Tools\deerflow-api\.build-cache\eino-harness-acp-verification\eino-desktop-complete-package.log`；交付检查记录为同目录 `desktop-package-delivery.json`。源码/内置技能输入和包内 Python 文件检查已通过，未安装 Python 的干净操作系统验收仍未进行。本包已包含下述宿主 MCP、角色用量、前台产物分页、诊断日志及设置实际值改动；此前 `220040-925` 包属于历史交付，不包含这些增量。此包不代表全部 33 项正式验收完成。

## 2026-10-02 后续补齐：宿主 MCP 重建、角色用量及启动诊断

- 新增 `Config.HostMCPRecipes` / `mcp.host_recipes`，默认空。每个 recipe 绑定唯一 ID 和精确绝对工作区；服务进程/网络权限来自独立宿主注册表，不扩大客户端 MCP allowlist。每个 recipe 在宿主内共用一组连接，执行分别借用 lease，客户端 detach 不终止宿主服务，SDK Close 统一等待清理。初版同一工作区不混配 client MCP。
- 只有明确 `restore_on_restart: true` 才恢复已有持久后台授权。启动 worker 前，按保存根会话目录、recipe ID、完整连接摘要和实际工具 schema 检验当前宿主配置，再 CAS 更新独立 grant 的 generation/version。原执行快照、task 状态、回执和 checkpoint 不改写；暂停任务仍走正常 resume，已结束任务只供合法 continuation，不重放完成或不确定效果。缺失、配置变更、schema 变化和未开启恢复均保持等待或不兼容。
- 配方原文、argv、URL、env/header 值不进入模型/角色/桌面可编辑或 applied 快照。配置严格拒绝未知字段/错误类型，env/header 和 URL 支持宿主 `$VAR` 引用；shared schema 仅声明宿主 YAML 路径与重启语义。任意旧 client 配置不自动持久化或提升权限。[MCP 文档](../go-harness/internal/mcp/README.md) 提供无真实密钥的 stdio 示例和边界。
- 角色用量已按实际 invocation 归属，后台父角色 continuation 保留逻辑 ID；根节点使用 `root_total` 汇总，后代用量不重复相加。没有可信归属的旧操作保持未知，不把根预算摊入后代。
- Go daemon 新增持久 lifecycle 日志，桌面位于 `user-data/logs`，独立启动可用 `--log-dir` / 环境设置。日志记录固定启动阶段与错误分类，不写原始配置/凭据或远端错误；Waku 概览缓存并展示路径。四份模板补齐宿主 MCP 空列表和 P4/P5 实际字段，付费维护与宿主配方默认关闭。

按本轮减少重复测试的要求，仅运行必要集成：

| 验证 | 结果 |
| --- | --- |
| `TestSDKHostMCPRecipesRestartReconstruction` | SDK 11.040s，通过；真实本地 MCP/OpenAI fixture 与 SQLite，覆盖宿主/客户端权限隔离、detach 存续、缺失/变更/schema/manual 拒绝、重启 CAS 授权、已完成与不确定效果不重放；`host-mcp-recipe-restart.log` |
| `TestSDKTaskTreeInvocationUsageAcrossBackgroundContinuation` | SDK 1.613s，通过；root→background host→foreground parent→background leaf，重启后 continuation 用量/逻辑 ID 保持、恢复不重复计费及旧未知用量；`task-tree-invocation-usage.log` |
| 日志轮转及不泄密、真实 main 配置失败持久日志 | 0.507s / 0.102s，通过；`daemon-diagnostics-focused.log` / `daemon-startup-log-focused.log` |

最终整合 build 与新桌面包已完成，见顶部交付记录；上述结果不替代真实外部 MCP、用户模型、GUI 或无 Python 干净系统验收。

## 2026-10-02 P4 缺失报告路由、重复工作流及可信质量判断

- 新增默认关闭的 `discovery.missing_reports_enabled`、`successful_workflows_enabled` 和 `new_skill_source_id`。两类新建发现必须显式选择已获生成授权的非内置来源；工作区来源与真实根会话目录匹配。配置接入共享 schema、Go 解码/启动和 Waku 原生字段，原纠正开关仍独立控制。
- 当前明确缺失报告只需一个已结束、完成证据采集的根任务，可完全没有技能读取。新技能名固定为 `missing-<摘要哈希>`；报告修订/撤回、根任务删除或来源变化会阻断旧候选后续工作。已有安装及删除墓碑不被自动覆盖。
- 成功流程发现从真实完成回执提取有序工具名与参数键/类型形状，要求每根 3–32 次成功调用、至少两种工具，并达到跨独立根任务的重复阈值。提示词、参数值及工具结果不进入发现快照；不重放工具。委派/技能管理、未完成后代、失败、核对及效果不确定均不作为成功流程。新技能名为 `workflow-<流程摘要>`。
- 新入口复用已有日额度、待审上限、冷却、独立维护预算、生成和内容/行为评测。每轮有界扫描 16 个根任务，持久保留游标、证据引用及已消费根任务；生成及批准再次核验报告修订或工具回执版本。`skill.discovery.list` 区分 `operator_corrections`、`missing_skill` 和 `repeated_workflow`，Waku 复用作业及待审操作。
- `signal.quality` 单独给出原因、置信度、来源和可计票状态。宿主未结束/未采集、取消、不确定效果、根预算耗尽及原生工具的类型化 network/permission/quota 等失败先排除；不对错误字符串或模型自述作高置信推断。没有结构化原因的失败保持 unknown。
- 新 `quality_failure` 是操作者明确确认的技能质量失败，必须绑定实际读取、当前信号版本和反馈修订。满足宿主排除条件时才获得高置信计票资格；`helpful` 还要求根任务及后代全部完成。普通 `correction` 仍不自动升级为质量归因；已确认质量失败可随显式纠正入口按跨根阈值产生修复候选。修改/撤回反馈不会累计为额外票数。

必要集成检查 `TestDerivedDiscoveryMissingWorkflowAndQuality` 通过，evolution 包 5.706s。同一用例覆盖零读取报告实际生成至待审、撤回阻止批准、重复工作流阈值与不复制输出，以及确认质量/环境报告/撤回资格。日志：`skill-derived-discovery.log`。此结果不等同于真实模型效果或桌面交互验收。

## 2026-10-02 P5 受限自动发布和持久观察

- `mode: auto_patch` 与默认 `review` 分开，必须启用 generation、behavior 并提供宿主 suite。新增五项配置：最大改动行数 40、每日自动发布 2、观察 72 小时、最少有效观察 3、连续失败回滚阈值 2，均可在规定范围内调整。
- 自动发布限定已存在、已启用且已进入发布管理的技能，只允许 `SKILL.md` 小改动，要求安全及宿主行为评测通过、具有针对性/回归样例且无退化。新建、删除、支持文件、内置覆盖或不合格评测仍待人工审查。候选完整 head、证据与发布条件重新核验；发布与观察记录同事务保存。
- 观察按被发布的确切版本、发布窗口及独立根任务取证，保存使用的反馈修订，撤回会在决定前重算。仅高置信操作者确认的质量成功/失败参与；普通网络、权限、取消、配额和未知结果不计为质量失败。窗口内达到连续失败阈值时通过当前 head CAS 创建回滚发布，之后的新版本不会被旧观察覆盖。
- 观察截止后，证据达到最少次数且未触发回滚标记通过，否则显示证据不足；改动当前安装版本后标记 superseded。切回 review 暂停自动 worker 并保留记录，重新开启后继续。SDK 和认证 MANAGE 提供 `skill.auto_patch.status/list`，Waku 提供只读状态及观察列表。

自动生成、行为评测、发布、忽略外部失败、反馈撤回、服务重启、确认质量失败回滚及幂等的关键集成通过。最后收尾后的同一用例为 2.194s（`skill-auto-patch-publication-monitor.log`），覆盖先人工采用为 managed、累计两次失败被成功打断时不回滚、随后连续两次失败回滚；`Failures` 保留总数展示，`ConsecutiveFailures` 才驱动回滚。打包后仍需实际桌面和用户模型手测。

## 2026-10-02 前台产物归属与完整分页收尾

`present_files` 的模型参数继续只有文件路径。宿主从实际执行上下文获取根预算、运行/尝试/fence、逻辑角色及其尝试身份，与成功工具回执和不可变快照一起提交；根、前台、后台角色按真实节点投影，完成或检查点产物在恢复和重启后仍可读取。旧产物没有角色证据时只保留原生后台容器归属，不推测前台归属。任务树摘要最多 32 项，新增 ACP `_deerflow/tasks/tree/artifacts/list` 与 SDK `TaskArtifacts`，cursor 绑定连接所有者/会话/根/节点；Waku 提供独立 32 项分页、首页刷新、下一页及现有 SHA 校验分块另存为，缓存不随页数增长。`invocation` 用量展示为角色自身调用，根总量含后代且不再次加总；历史无证据仍显示未知。

唯一新增真实 SDK 链路 `TestSDKForegroundArtifactAttributionCheckpointAndPaging` 通过（1.993s，`foreground-artifact-paging.log`），覆盖检查点已有 32 份产物、恢复产生第 33 份、多个真实 role attempt、根节点拒绝冒领、重启跨页和不可变摘要分块读取。Waku 三包增量 `cargo check --offline -j 2 -p waku -p waku-core -p waku-protocol --lib` 通过（42.01s，`waku-artifact-final-check.log`）。这些是源码功能和编译检查，最终新桌面包及用户手测状态由顶部交付记录说明。

## 2026-10-02 P2 MCP 资源、任务树和产物读取

本阶段继续使用 Go/Eino 和 Rust，不增加 Python 或 HTTP 服务。已补充 Go SDK、当前 ACP v1 连接上的任务树查询/取消及 MCP 资源授权接口；Waku 使用独立任务缓存和原连接，避免普通聊天完成或断连事件错误结束持久后台任务。协议见 [后台任务与任务树](../go-harness/internal/acp/background.md)。

- MCP `Describe` 固定归一化连接配置和实际工具 schema 摘要，`Acquire` 同次发现并借出增强工具；稳定描述与连接代际分别校验。每个运行拥有独立 lease，关闭一个 borrower 不关闭父连接；图片结果继续使用 Eino 结构化结果。失联后的未知外部效果保留回执核对要求。
- 后台任务持久保存资源引用和独立授权版本。客户端重连接入只恢复传输，显式授权同时检查 task/resource 版本；权限批准、释放安全暂停和资源授权仍是不同操作。终态来源任务的授权仅供后续 continuation 使用，不重跑已经结束的任务。显式宿主 recipe 的自动重建已由上方后续阶段补齐，客户端规则不变。
- 新增根任务及逻辑角色投影，后台 continuation 保留逻辑节点身份；分页为 live keyset，结果只投影最终对父答案。不存在可归因用量时返回未知；根汇总用量不能再与后代相加。
- 新增 `_deerflow/tasks/tree/artifacts/read` 和 SDK 分块产物读取，按根连接所有权和逻辑后台节点加载实际归属，客户端不能提交文件路径或 AssetRef。默认 256 KiB、最大 1 MiB，后续块必须携带首次 SHA256；历史 native attempt 的不可变产物仍归原逻辑节点。只为当前具有持久产物归因的 background 节点提供入口；根/前台不伪造产物归属。读取持有根会话生命周期租约，活跃 prompt 时返回 busy，Waku 仅让保存动作失败，不结束聊天。
- 产物首次读取校验完整 SHA256 并缓存 1 MiB 块摘要（最多 64 份），之后重新读取并验证覆盖请求范围的完整块，检查前后文件大小；不以 mtime 作为信任依据。不相关块的篡改在请求该块时发现，返回的数据始终绑定原始摘要。Waku 已接保存对话框及后台另存为。
- 子树/根取消先写入持久 stop，再阻止新后代和预算调用，随后撤销执行；清理未完成时继续显示 `cancelling`。暂停角色的权限/checkpoint 精确取消、旧审批失效、重启后的逻辑身份保持和无授权恢复链路已落盘并完成一条真实 Eino 关键集成。同连接实际 schema 变化在原生 claim 和 continuation 新调用准入前检查，不静默替换工具。

已确认的第一批验证结果：

| 验证 | 结果 |
| --- | --- |
| MCP 全包 | 7.674s，通过（新增描述/lease 首批五组及现有传输回归） |
| MCP/ACP 受影响用例 race | MCP 6.471s、ACP 13.137s；增加真实 HTTP 调用中关闭 borrower、精确版本、权限归属、严格参数、optional capability 和管理错误后继续普通 prompt |
| 任务树内部及 SDK 真实混合树 | agents 5.289s、runtime 5.810s、Eino 4.802s、SDK 4.317s；含六组新增状态/分页/权限/CAS/取消边界及实际前后台角色结果 |
| 后台资源 SDK 与 controls 首批 | SDK 16.259s、controls 0.735s；外部效果后单会话断连、暂停、重连未授权拒绝、双版本授权、继续不重放，及排队任务资源等待不消耗 attempt/预算 |
| 源码检查 | 当前源码输入、内置技能输入及受控改动空白检查通过；后续改动仍需最终检查 |

证据根目录仍为 `D:\Tools\deerflow-api\.build-cache\eino-harness-acp-verification\`。当前日志为 `mcp-resource-leases.log`、`mcp-task-tree-acp-race.log`、`mcp-background-resources.log`。任务树首批命令是 `go test -p 1 ./internal/agents ./internal/runtime ./internal/engine/eino . -run '^(TestTaskTree|TestRoleTaskTree|TestSDKForegroundAndBackgroundRolesShareConcurrency)' -count=1 -timeout 120s`；该批没有另存 stdout，已确认工具句柄 `30954` 返回 exit 0。Waku 首批源码检查已通过，后续状态/协议测试及上述收尾仍进行中。未提交、未生成新的桌面包，全部 33 项最终验收继续保留。

追加关键结果：`task-tree-native-paused-cancellation.log` 通过（SDK 1.521s），包含背景父角色内的前台 write 审批暂停、取消该角色、旧审批失效、重启后无批准恢复、零写入/零授权/零 pending receipt，以及父完成/被取消角色保持 cancelled。`task-artifacts-targeted.log` 的 assets 4.226s、runtime 6.268s 通过，含所有权、私有子会话拒绝、SHA/offset/跨块、快照篡改及历史逻辑节点归属。100 MiB 本地样例首次验证约 327ms，缓存 100 个 1 MiB 块约 367ms。按用户要求停止后续重复 race，不将被中断的 `mcp-artifacts-final-race.log` 计为通过。

## 2026-10-02 P4 Go 原生行为评测、持久恢复与 Waku 检查

继续按 Eino/Go 单运行时实施；方案明确新增的生成、评测、发布和回滚流程也不引入 Python。新增行为 suite、Eino 执行器、Go 断言、持久评测服务和 Waku 检查入口。完整契约与配置示例见 [行为评测说明](../go-harness/internal/skills/evolution/behavior.md)。

- 宿主一次性加载独立 JSON suite，严格校验字段、重复键、数量、路径和模拟工具参数；候选不能提供自己的评判依据。基础/候选、样例及每次重复使用独立临时目录和模型上下文。模型经本地文件工具和模拟工具完成任务，Go 检查实际输出、文件和工具次数；预期答案不进入模型输入，不执行脚本或真实外部工具。
- 新建技能使用空基础版本，删除使用空候选版本，两个对照都真实执行。每个结果记录输出/轨迹摘要、宿主检查、调用、Token 和耗时；报告只由宿主从已保存结果组成。内容审查不能伪造行为报告或取得脚本执行权限。
- 生成候选继续使用原维护作业；手工候选与独立评测作业原子入队，不伪造生成输入。手工提案处于 `validating` 时可拒绝并取消后台作业。已完成内容审查和逐版本结果跨重启复用；未保存调用按剩余额度重试，失败与未知用量的保守扣额继续保留。
- 每次新模型调用及结果提交核验来源、完整 head、覆盖基础、suite、评测身份、证据和作业领取权限。批准重新核验持久证据；通过评测仍需人工批准。无匹配 suite 标为 `not_run`，已配置的评测失败或无法完成时阻止批准。单个维护作业失败不改变用户会话状态。
- 配置/save/validate/launch/schema 接通六项 `skill_evolution.behavior` 设置，默认关闭，使用现有 evaluation 模型和独立维护预算。新增 SDK 与认证 MANAGE `proposal.behavior`、`proposal.behavior.receipts`，提供有界逐样例结果和用量分页，支持停用后查询授权历史。
- Waku 接通设置、结构化对照报告、执行详情及回执分页，后台查询、缓存渲染，目标或服务变化撤销旧响应；支持创建/删除的空技能对照和验证中拒绝。

本阶段已通过以下检查。provider 测试使用本地 fixture 和真实 Eino adapter/工具循环，不调用收费模型；真实桌面交互与发行包尚未验收。

| 验证 | 结果 |
| --- | --- |
| evolution 全包、skillmaintenance 全包 | 38.615s / 0.953s；深拷贝收尾后另跑全部行为核心竞态测试 |
| 行为核心 `TestBehavior` race | 16.326s；覆盖恢复、原子入队、create/delete、报告篡改、预算、拒绝及迟到结果 |
| suite/runner 9 组 race | 前 7 组 suite 1.892s、engine 3.672s；并发工作区/schema 隔离和输入/tool 限制两组 5.154s |
| SDK/MANAGE 行为、生成、发现、提案相关集成 | 27.045s；含手工/生成成功及手工断言失败后继续普通会话、认证查询、重启读取 |
| 共享 schema、桌面配置、launch 全包 | 1.003s / 46.018s / 2.525s；共享 schema 和桌面配置运行受影响用例组，launch 为全包 |
| Waku 编译和相关纯状态测试 | check 23.36s；Waku 79 项、core 31 项全部通过，新增行为相关 12 项 |
| 构建及输入检查 | `go build -p 2 ./...` exit 0；源码、内置技能、Go/Rust 格式及空白检查通过 |

本地日志根目录：`D:\Tools\deerflow-api\.build-cache\eino-harness-acp-verification\`。日志为 `skill-behavior-core-packages.log`、`skill-behavior-core-race.log`、`skill-behavior-integration-final.log`、`skill-behavior-launch-final.log`、`waku-behavior-check.log` 和 `waku-behavior-tests.log`。runner 的两轮 race 没有另存日志，其工具执行句柄及结果保存在汇总证据中。最终构建、源码摘要和检查结果见 `skill-behavior-evidence.json`。

格式检查保留第三方源码既有格式，并确认未修改的 `bridge_v2_test.go` 只有 CRLF 行尾差异，保留原文件。Python 清单保持 584 项（553 removed_verified、25 replaced_verified、6 replaced_syntax_verified、0 planned）；该清单不替代最终无 Python 系统及 Release 验收。

缺失报告自动路由、重复成功工作流发现、可信失败/质量归因、P5 自动发布/观察期/回滚，以及 P0/P2/P3/P6/P7 的剩余验收继续保留。此阶段不等于 P0–P7 或全部 33 项验收完成；未提交、未制作 Release，未运行 Python，也未修改个人配置、数据库或旧分发包。

## 2026-10-02 P4 重复明确纠正的自动选择与待审生成

新增第一条信号自动进入生成的路径，默认关闭，通过 `skill_evolution.discovery.operator_corrections_enabled` 显式启用。旧 Python `discovery.enabled` 仅保留迁移诊断，不能激活付费工作。此阶段保持分支的 Go/Eino 单运行时定位；Python 清理范围与第 28–33 项验收继续保留。

- 当前 `correction` 必须关联真实读取、匹配信号版本与反馈修订，且来自不同的已终结根任务；后代、未处理 outcome、取消和外部效果不确定状态均参与核验。同根任务反复修改只算一个判断。已安装技能必须启用且内容版本相同，完整 head、来源及覆盖基础被固定。
- 选择记录、生成输入/作业、已消费根任务摘要、UTC 日选择额度及来源/技能冷却同事务保存。快照只保留证据引用和摘要；实际调用读取已脱敏的当前反馈。后台复用独立维护预算和生成/安全/质量审查，成功进入 `pending_review`；`attribution=unassessed`、行为 `not_run`，不自动发布。
- 后续付费阶段、候选提交、提案挂接/审查与人工批准重新核验。撤回、修订或任务删除不能继续授权旧证据；批准发现失效时原子标记 `stale` 并释放待审容量。已审完候选在关闭自动工作后仍可明确批准，前提是证据及当前 head 有效；已发布操作保留原幂等回放，重启确认终态不追加模型调用。
- 每轮最多扫描 32 项，按实际尝试逐项 CAS 推进游标，重启保留未处理尾部。损坏证据按版本最多三次检查；队列满按正常背压处理，暂时存储/时钟错误退避后可恢复。失败/取消不返还日选择或已消费根任务。pending 限额统计全部活动生成和未完成提案，同一活动作业及已挂接提案只计一次。
- 新增 SDK 与认证 MANAGE 的只读 `skill.discovery.status/list`，严格检查字段、null、整数、游标与来源授权；不提供远程 selector run 操作。状态提供实际启用配置、日额度、待处理数与固定诊断码。来源重绑定撤销旧记录访问，关闭自动选择仍可读取授权来源历史。
- YAML/save/validate/launch/Waku 使用同一 schema，新增六项原生设置及依赖、范围校验。自动生成作业在现有面板显示 discovery ID，沿用回执与提案审查；独立的发现决策浏览器尚未实现。

验证：

| 范围 | 结果 |
| --- | --- |
| 配置 schema 全包及保存/校验/启动专项 | 通过；schema 6.107s，evolution 配置 3.252s，desktopconfig 10.819s，launch 6.148s |
| 最终核心全包 | evolution 38.804s；skillmaintenance 0.708s；包含 11 组新增 discovery 测试 |
| 竞态 | discovery 与真实会话并发删除 43.145s；最后增加的取消游标清理专项 7.146s |
| 最终 Eino 生成相关专项 | 7.138s，通过 |
| 最终 SDK/认证 MANAGE | 2 组通过，14.990s；真实 Eino 读取、去重、三阶段预算、待审无发布、聊天状态保持、下一轮正常、重启/来源隔离 |
| Waku | `cargo check --offline -p waku --bin waku -p waku-core` 通过，13.00s；相关 Rust 测试 Waku 67/core 31 通过 |
| 构建和静态检查 | `go build -p 2 ./...` 通过；295 个变更/新增项目自有 Go 文件格式检查及本轮 Rust 格式检查通过；源码、内置技能和空白 gate 通过 |

本地证据根目录为 `D:\Tools\deerflow-api\.build-cache\eino-harness-acp-verification\`。日志包括 `skill-discovery-core-final.log`、`skill-discovery-core-race.log`、`skill-discovery-cancel-cursor-race.log`、`skill-discovery-engine-final.log`、`skill-discovery-sdk-final.log`、`skill-discovery-build-final.log`、`waku-discovery-check.log` 和 `waku-discovery-tests.log`；结果与源码摘要保存到 `skill-discovery-evidence.json`。格式检查保留第三方来源代码的既有格式。Python 清单保持 584 项（553 removed_verified、25 replaced_verified、6 replaced_syntax_verified）。

此阶段未提交、未制作 Release、未运行 Python，也未修改个人配置、数据库或旧分发包。缺失报告自动路由、重复成功工作流发现、可信失败/质量归因、行为评测及 P5 自动发布/观察期/回滚仍需实施。P0 设置生效收尾、P2 MCP 资源重建与 Waku 任务树、P3 旧历史导入、P6 原生 Linux/CI 与最终引用清理、P7 干净环境和同一干净提交的源码/Waku 交付继续保留；全部 33 项验收尚未完成。

## 2026-10-02 P4 Waku 缺失技能报告表单

- 技能设置页新增独立报告面板，使用当前聊天的 `ProviderResumeCursor::DeerFlow` 原生会话，通过 `skill.missing.targets` 选择真实根任务。未读取过技能、尚无报告的任务也能报告；没有把 Waku 本地会话或轮次 UUID 当作 Go run ID。
- 已接入任务分页、当前报告读取、CAS 提交、确认撤回及报告历史分页。每页最多 20 条；按解析后的时间校验排序，明确显示原始根任务状态，不推断后代整体完成情况。SDK 已提交的子任务来源报告按根身份展示。
- 本地/原生会话切换、任务或服务变化撤销旧版本条件并保留草稿；请求前及响应落地再次同步当前会话。新修改必须符合当前选中任务、修订和草稿，旧确认或回调不能恢复提交条件。修订上限及 UTF-8 字节限制与 Go 契约一致。
- 断线保留原请求全文和任务身份，回放只显示操作回执及最新修订，不能把旧回执安装为当前报告。共享技能重试另外绑定原 `config_path` 与 `user_data`；同目录服务重启后可核对，切到另一数据目录不能重放。服务代次下降或目录变化也撤销缓存操作状态。
- 沿用后台 MANAGE 执行器、缓存渲染和具备键盘激活的原生控件。表单与配置草稿独立，不触发模型生成、发布或聊天任务。独立复核未发现阻断缺陷。

验证：`cargo check --offline -p waku --bin waku -p waku-core` 通过（27.25 秒）；`cargo test --offline -p waku -p waku-core --lib deerflow -- --nocapture` 通过，Waku **67 项**、core **31 项**，无失败或跳过。新增 7 组表单状态测试、1 组原生会话绑定测试及 3 组共享重试测试；覆盖零读取、子任务来源、版本推进、历史翻页、过期确认、原请求重放、会话与服务目录切换。保留既有非相关 unused/dead-code 警告。

测试后仅调整报告错误文案及格式；最终 `cargo check` 再次通过（8.15 秒）。修改文件 Rust 格式、全仓受控改动空白、新增模块空白、源码及内置技能输入 gate 均通过；方案保持全部 33 项验收。

本地证据根目录：`D:\Tools\deerflow-api\.build-cache\eino-harness-acp-verification\`；日志为 `waku-missing-check.log`、`waku-missing-check-final.log`、`waku-missing-tests.log`，结果及源码摘要在 `waku-missing-evidence.json`。源码入口为 `desktop-app/src/app/deerflow_settings/missing.rs`；协议及桌面说明同步更新。此阶段未进行真实桌面交互验收、未创建 Release、未提交，也未修改个人配置、数据库或现有分发包。自动发现/可信归因、信号选择、行为评测及 P5 自动发布/回滚继续待实施。

## 2026-10-02 P4 零读取任务的缺失技能报告与真实任务发现

- 新增独立 `SkillMissingReport` 持久服务和 SQLite 表，支持尚未读取任何技能的真实任务，也支持已读其他技能的任务报告缺少另一项能力。记录固定为 `origin=operator`、`attribution=unassessed`，不制造技能读取、技能版本或已验证的失败归因。
- 服务在同一事务核验源 run、原始根 run、预算成员及前台根会话。前台角色复用宿主 run，后台任务和自动 continuation 归到原根任务；拒绝伪造 session/run、孤立预算身份及私有子会话冒充根会话。权限沿用本地管理 token 和宿主 SDK 的操作者权限，允许 ACP 已断开的真实前台会话；这不构成多用户隔离协议。
- 每根任务只保留一份当前判断及最多 1000 条不可变修订，最后一条容量留给撤回。先核验归属再处理原请求重放，再对新请求执行版本 CAS。清理后的摘要、原意图摘要值、修订和目录版本同事务写入；同键不同原文即使脱敏后相同仍冲突。报告修订不累加为多个根任务的独立票数。
- 新增五个 SDK 方法及认证 MANAGE 的 `skill.missing.submit/get/list/history/targets`。必填字段不能缺失或为 null，版本拒绝非整数；未知身份/授权字段拒绝。列表和历史每页 1–100 条，响应有字节上限，游标校验用途、会话/根身份、版本及规范编码。源码与协议说明见 `go-harness/skill_missing.go`、`management_missing.go`、`internal/skills/evolution/missing*.go` 和 `internal/acp/skill-evolution.md`。
- 新增真实根任务发现接口，返回原始根 run ID、创建时间、状态及报告修订。无读取、无已有报告的任务也能被发现；不返回原文、工具输出或 provider 错误。按原始根 run 创建时间倒序和根 ID 倒序分页，处理 RFC3339Nano 可变小数精度；后代活动不重排旧根任务。targets 是实时列表，`Status` 只表示原始根 run，后续表单不能将其当作整棵树的汇总状态。
- 根会话或根预算删除级联移除报告历史；修订中的源 run 通过读取时的真实关系校验，不用阻碍“先删 child、后删 root”的外键。报告存储失败回滚自身写入，不改变已提交任务结果，也不会阻塞下一次聊天。没有调用生成器或改变技能发布指针。

已验证：

- `go test ./internal/skills/evolution -count=1` 全包通过（43.435 秒），包含旧提案、反馈、生成、collector 及新增缺失报告。核心 9 组定向回归此前通过（16.527 秒）；真实任务发现 6 组专项回归通过（13.432 秒）。
- 两个 SDK/真实认证 MANAGE 集成测试通过（包总计 7.788 秒）。覆盖真正的零读取模型运行、首份报告前的目标发现、原请求已提交后故意断开不读响应再重试、历史分页、重启、后续 prompt、原历史/执行版本保持一致、ACP 断开后的管理、真实私有子会话拒绝及后台任务树删除；另一会话的报告保持可用。
- 旧 SDK 技能反馈及 runtime 会话/整树删除回归通过（10.063 秒、2.245 秒）；`go build ./...` 通过。
- `go test -race -p 2 . ./internal/skills/evolution -run 'TestMissing' -count=1 -timeout 180s` 通过，SDK 包 11.614 秒、evolution 包 89.869 秒；本次首次准备相应竞态依赖耗时较长，保留原进程完成验证。
- 新增真实会话删除与报告提交并发回归，通过屏障同时启动两个操作，核验合法事务顺序、级联删除、无孤立修订及另一会话不受影响。普通测试 6.921 秒、独立竞态测试 8.798 秒通过。
- 新代码 `gofmt`、全仓受控改动空白检查及 240 个非忽略新增文本文件检查通过（239 个批量检查及最后新增的并发删除测试单独检查）；源码和内置技能输入 gate 通过，未引入 Python 文件或执行依赖。

本地集成证据在 `D:\Tools\deerflow-api\.build-cache\eino-harness-acp-verification\skill-missing-sdk\evidence.json`；目标查询证据和日志为同目录下 `skill-missing-targets-evidence.json`、`skill-missing-targets-test.log`。独立只读复核未发现阻断缺陷。

Waku 只有可靠的 Go session ID，未持久化 Go run 与本地轮次的对应关系，因此通过真实任务查询接入表单；后续桌面源码进展见本文顶部，真实桌面验收仍待完成。可信故障分类、质量归因、自动信号选择/冷却去重、行为评测和 P5 自动发布/回滚继续保留；操作者报告通过不代表自动进化已经完成。本轮未提交、未制作 Release，也未替换个人配置、数据库或旧分发包。

## 2026-10-02 P6 Python 核心、构建、技能与默认配置清理

### 已落地范围

- 按固定 Git 基线及文件内容逐项核验，删除原 Python 核心、HTTP 服务、专用测试、旧集成/示例/SDK 元数据、根 `pyproject.toml`/`uv.lock`，以及旧 admin UI 和旧 desktop 的文本源码。保留 `tests/fixtures/portable_preferences.json` 作为迁移数据；历史格式和测试来源记录在 `docs/eino-legacy-migration-baseline.md` 与 `docs/fixtures/legacy-python-migration.json`。
- 删除/替换清单扩展为 **584 个不重复路径**：553 `removed_verified`、25 `replaced_verified`、6 `replaced_syntax_verified`、0 `planned`。其中 15 项替代条目的旧文件也已删除，因此清单内实际缺失 568 个旧文件；553 是状态分类数量。Git 当前另列两项配置加载器及测试的重命名前路径。上述为工作树状态，改动未提交。
- 原 Python 桌面 smoke 与 MCP fixture 已替换为 `go-harness/cmd/deerflow-desktop-smoke`，使用 Go fixture 服务；两个旧打包脚本变为 Go 构建入口的参数兼容包装。三个 CI workflow 改为 Go/Rust，删除 Python 安装及旧构建步骤；远程 workflow 执行和 Release 构建尚未验收，因此对应条目保留语法验证状态。
- 新增 `scripts/go-package-inputs.ps1` 和入口检查，扫描现存 Git 跟踪文件及非忽略未跟踪文件中的 Python 源码/依赖清单，并拒绝内置技能中的 Python 文件或执行指令。发现不兼容技能时构建失败；技能迁移或退役在源码中明确完成。
- 新增 Go 技能工具 `deerflow-skill-go init/validate/package`，复用候选验证及凭据检查，拒绝未完成模板、Python 依赖、不合法引用及覆盖现有输出。ZIP/脚手架采用独占创建，脚手架初始化部分写入失败时核对已创建文件和目录身份后清理。CLI 明确返回 `behavior: not_run`，不把结构校验当成行为评测。
- 重写 skill-creator、github-deep-research、systematic-literature-review 和 claude-to-deerflow 的无 Python 工作流；保留 17 个技能，退役 data-analysis、image-generation、video-generation、podcast-generation、ppt-generation 五个 Python 专用可选技能。既有 Go 工具 `image_search`、OpenCLI 和文件工具不受影响。设置 benchmark 改为 PowerShell，构建脚本打包 Go 技能工具。
- 四份默认 YAML 改为当前原生配置字段，清除旧 HTTP、Python manager/storage、主 Agent 和未消费的 thinking/goal 等模板字段。首次配置初始化不再注入 `api.data_dir`、`deerflow_home`、`extensions_config_path`；新测试覆盖初始化→快照→保存→启动，确认旧键不会回写。
- 更新 README、便携说明、Go 配置与 Windows 操作文档；21 份旧设计文档标明历史边界及固定提交。旧桌面三个图像资源仍保留，未计入已删除条目，不存在现用运行依赖。

### 验证与证据

- 使用 D 盘 Go 缓存及临时目录执行 `go test -p 2 ./... -count=1 -timeout 5m` 和 `go build ./...`，全部通过。主要包耗时：SDK 220.272 秒、runtime 73.412 秒、desktopconfig 60.319 秒、evolution 14.085 秒、集成包 55.594 秒。
- 全量回归后补充了技能工具目录身份测试。首次测试夹具错误地用 Windows 路径式 `os.Lstat` 保存延迟文件 ID；生产使用 `os.Root.Lstat` 的稳定句柄身份。修正夹具并增加初始化期间目录替换用例后，在实际工作树执行 `go test -p 2 ./internal/skillauthor -count=1 -timeout 90s -v` 通过（0.379 秒）。真实目录替换清理检查通过；打开句柄时 Windows 拒绝替换的分支也验证回滚无残留。符号链接相关用例因系统权限跳过。
- 最终独立审查发现归档失败的旧清理路径未核对输出身份。新增测试先复现替换文件被误删，再修复为创建时记录 `File.Stat` 身份、清理时经 `Root.Lstat`/`SameFile` 检查，保留已经替换的输出并合并原错误与清理错误。部分写入、写入后取消和真实文件替换用例均通过；最终实际源码 `skillauthor` 全包通过（1.193 秒）。身份检查和删除是分离操作，不宣称原子 compare-and-delete。此前独立 CLI smoke 二进制早于本项修正，最新源码的编译和行为依据为此次包测试。
- 原生技能 CLI 的初始化、不完整模板拒绝、完整模板验证、打包、现有输出拒绝及 ZIP 内容检查通过；四个重写技能结构校验通过。PowerShell 5.1 在隔离数据上执行一次设置 benchmark 成功，此结果只验证脚本可用性。
- 真实 Waku daemon WebSocket、Bridge 与 Go runtime 的两个最终协议 smoke 场景分别通过 **53/54 项检查**，清理错误均为 0。覆盖模型端点/凭据切换、配置校验保存应用、会话历史与重启、技能读取、写工具审批、MCP 设置/使用/清理，以及停止服务后的模型发现行为。独立审查后补回写工具调用 ID、请求所含工具和精确结果断言。第二个场景另验证旧空策略兼容，不能据此声称已验证所有未授权 MCP 拒绝或服务端调用恰好一次。
- smoke 辅助逻辑的四项测试及竞态检查通过；入口脚本 fixture、PowerShell/YAML 语法、源码/技能输入 gate 通过。未运行 GUI，未创建 Release，也未执行 Python。
- 最终整合检查确认 584 个清单路径无重复，删除项均不存在；`git diff --check`（CRLF 配置）及 228 个新增文本文件空白检查通过，未发现零长度 Go 源码。源码/技能输入 gate 再次通过。

本阶段本地证据位于 `D:\Tools\deerflow-api\.build-cache\eino-harness-acp-verification\`：`python-build-removal-evidence.json`、`python-skills-removal-evidence.json`、`skill-author-directory-identity-fix/evidence.json`；两个最终 smoke 报告为 `desktop-smoke/20261002-162623-0ebf991e-f7dc-4dd8-bf4e-258eb20009c1/logs/report.json` 和 `desktop-smoke/20261002-162651-c86c2f56-dcd4-4e96-b1c7-2862f1682426/logs/report.json`。核心和旧界面删除证据保留在权威工作树 `.build-cache/python-core-removal-evidence.json`、`.build-cache/python-legacy-ui-removal-evidence.json`；后者列出 15 个候选，实际仅删除 12 个文本文件。

归档失败清理的复现、修复后日志与最终源码摘要另见同一 D 盘证据根目录的 `skill-author-package-cleanup/evidence.json`；主技能清理证据已同步此结果。

### 未完成边界

本阶段不等于完整技能自进化或 P0–P7 交付。P4 的可信失败分类、质量归因、自动信号选择/冷却去重和行为评测，P5 的受限自动发布、观察期及自动回滚仍待实现。缺失技能报告后端和 Waku 表单源码的后续进展见本文顶部。MCP 资源重建、Waku 任务树、旧 Agent/进化历史导入、实际设置生效收尾也继续保留。现有 Go 测试和协议 smoke 不替代无 Python/uv/pip 的干净系统验收、原生 Linux 生命周期、真实桌面交互、同一干净提交的源码 ZIP 和 Waku Release。

## 2026-10-02 P7 源码与桌面归档来源一致性

- 桌面构建脚本接入独立的 `desktop-source-identity.ps1`：`-CreateZip` 在 Cargo/Go 编译或输出目录创建前要求干净提交，包含非忽略未跟踪文件；复制产物前、源码归档前后及二进制 ZIP 完成后重新检查来源。`git archive` 使用编译前固定的提交，和 `BUILD_COMMIT.txt` 一致。
- 拒绝 `-SkipBuild -CreateZip`；开发构建仍允许工作树变更和复用二进制，以明确标签及 `BUILD_SOURCE.json` 记录其来源状态。归档失败仅移除本次创建的 ZIP，不删除既有目录。Windows 路径比较忽略大小写，Unix 保持区分；Windows PowerShell 5.1 原生 Git 失败按退出码包装诊断。
- `scripts/test-desktop-source-identity.ps1` 使用隔离 Git 仓库验证真实小型源码 ZIP 内容、提交注释、干净/脏/暂存/未跟踪状态、提交变化、编译前拒绝、复用二进制及路径大小写。Windows PowerShell 5.1 和当前 PowerShell 各通过 28 项检查；三个 PowerShell 文件语法和空白检查通过。没有编译或打包实际 Waku 项目。
- 来源检查不等于 Python 清理完成；manifest 保留 `python_free_source_acceptance: not_verified`。方案第 31 项增加固定干净提交要求；当时第 7 节清理数量为 530 planned、4 removed_verified、8 replaced_verified、3 replaced_syntax_verified，最新清理结果见本文顶部。完整 P0–P7 和 33 项验收保持进行中。
- 整合检查：`git -c core.autocrlf=false -c core.whitespace=cr-at-eol diff --check` 及 212 个未跟踪文件行尾空白检查通过；重新解析三个 PowerShell 脚本无语法错误。独立方案审阅后明确旧 `--python` 仅拒绝并提示迁移，保留 Go 直连 ACP stdio 入口。

## 2026-10-02 P4 真实读取反馈、修订历史与 Waku 操作

- 新增 `harness.SkillFeedback*`、SQLite `df_skill_feedback` 和 SDK/认证 MANAGE `skill.feedback.get/list/submit`。每个 signal 对应一个根任务/来源/技能内容版本的当前人工判断；反馈修订与读取证据状态版本分开，修订或撤回不会累积成重复票数。目标、内容版本及根会话身份由已记录证据派生，提交必须引用 1–16 个真实且属于该 signal 的读取 ID。
- 写事务先验证当前来源/工作区授权，再处理 request key 回放，最后对新写入做证据版本和反馈版本双 CAS。原请求回放返回原修订与最新 current revision，collector 更新或后来撤回不能导致旧判断重新写入。插入、幂等摘要和信号目录版本同事务；来源重绑定撤销读取/回放权限，根会话删除级联清理自己的反馈历史。每 signal 最多 1000 条修订，并为最后一次撤回预留容量。
- 支持 helpful/correction/network/quota/permission/cancelled/other/withdrawn 用户报告。所有类型保留 `attribution=unassessed`；`evidence_current` 仅表示读取集合版本一致，不表示根任务已经完成或技能质量通过。反馈不调用模型、不创建生成作业、不发布，失败不改变已完成任务或阻断下一轮聊天。此阶段尚未实现零读取缺失报告，其后续进展见本文顶部；可信故障分类仍待实施。
- 摘要在规范化前按 2000 UTF-8 字节限长，拒绝不可接受的控制字符，对常见凭据、完整认证/Cookie header 及 URL 做清理；数据库仅保存清理后文本和原始规范化意图的摘要。独立审查发现 Basic/Cookie 只去掉标签会残留值，已修为完整 header 处理并增加真实持久化/查询回归。清理规则不保证识别任意秘密文本，接口文档明确要求提供简短纠正说明。
- Waku 技能页接入信号分页、实际读取跨页选择、当前反馈、分类/摘要输入、显式提交、确认撤回和修订历史。各列表最多 20 条，使用既有后台 MANAGE executor 与缓存投影；证据/服务/信号变化撤销提交条件并保留摘要草稿。旧请求回执不冒充当前反馈，明确结束重试会同时清理原反馈身份。提交条件检查为纯函数，UUID 和请求 JSON 只在点击时创建。

验证结果：

- 内部 `TestFeedback*` 初版 13 项通过（10.751s），新增输入边界子集 6.247s；后续完整认证 header 清理的 `TestFeedbackRedact*` 6.182s 通过。
- SDK `TestSkillFeedbackSDKAndAuthenticatedManagement` 最终 7.315s 通过：真实 Eino skill/support read→collector→SDK 与认证 TCP MANAGE 提交/重放/撤回/分页，严格字段拒绝、同会话下一轮、关闭及重新打开。测试使用隔离目录、数据库和本地模型 fixture。
- `go test -p 2 ./internal/skills/evolution . -run 'Feedback|Collector|SkillGeneration' -count=1 -timeout 180s`：8.828s / 6.024s 通过。
- `go test -race -p 2 ./internal/skills/evolution . -run '^TestFeedbackConcurrentWriters|^TestFeedbackTransactionFailure|^TestSkillFeedbackSDK' -count=1 -timeout 180s`：8.808s / 10.268s 通过。
- `cargo check --offline -p waku --bin waku -p waku-core` 最终 5.90s 通过，7 项 `deerflow_feedback` 纯状态测试通过；整合后的 `cargo test --offline -p waku -p waku-core --lib deerflow -- --nocapture`：Waku 56 项、core 31 项全部通过，保留既有 unused/dead-code 警告。未做视觉或真实鼠标/键盘验收。
- 最终 `go build ./...` 及反馈相关 Go 文件的 `gofmt -l` 检查通过。

完整目标仍为 P0–P7 和 33 项验收。P4 质量归因、自动选择/冷却/去重、行为评测，P5 自动发布/观察期/回滚，以及 P2 MCP/任务树、P3 历史导入、P6 剩余 Python 核心/依赖/构建/CI 删除、P7 无 Python 干净环境和源码/Waku 交付继续保留。源码接通和测试不代表真实桌面交互验收完成。

## 2026-10-02 P6 删除独立旧 Python 入口

- 删除 `.github/workflows/publish-pypi.yml`、`scripts/audit_globals.py`、`scripts/read_api_config.py`，共 275 行。删除前核对为当前工作树内未修改受控文件，删除后核对文件缺失、固定基线 blob 和无剩余受控调用；只删除三个明确路径。
- 删除清单对应三项更新为 `removed_verified`，记录理由和验证。目前 545 项为 530 planned、4 removed_verified、8 replaced_verified、3 replaced_syntax_verified。旧桌面 smoke、两份旧打包脚本及其 workflow 依赖仍待迁移，真实桌面协议/模型切换/权限/MCP/恢复测试不能由单一 daemon 启停测试替代。
- 未运行 Python，未操作个人数据或既有包，未生成桌面 Release，也未提交代码。

## 2026-10-02 P4 Waku 生成请求、作业与用量回执

- SDK 与认证 MANAGE 新增 `skill.generation.status`、`skill.generation.prepare`。status 返回当前运行宿主实际授予的来源和维护限额，关闭时不声明生成授权；prepare 校验完整来源/工作区身份并读取完整 head，支持尚不存在的技能，内置来源必须具有覆盖授权和已导入基础。操作不入队、不调用模型、不发布。修正文档中通过 `skill.get` 获取不存在技能 head 的错误说明。
- Waku 技能页新增独立生成表单：刷新能力、选择来源、名称/改进要求、检查版本、显式提交。输入草稿与设置配置分离；编辑代际绑定检查响应，改变草稿或服务会撤销旧检查，来源重绑定需要重新选择。未保存/未应用的配置阻止提交，草稿文字保留。
- 接入每页最多 20 条的作业查询、筛选、详情和回执分页，展示状态/阶段、尝试、截止/重试时间、固定错误原因及预留/实际或估算 Token。序号、来源、目标、head、作业 ID 和状态版本在响应投影中校验，拒绝跨作业回执和版本倒退。刷新能力会将旧作业分页标为待刷新。请求在既有后台 executor 执行，渲染只读取缓存，不增加轮询或渲染 I/O。
- 取消复用明确确认及观测到的状态版本；完成作业进入既有提案审核，成功生成仍不直接发布。关闭生成并应用后可以查询当前授权来源的旧作业与回执。传输失败保留原修改请求及 request key，其他读取不能清除它，新修改需先核对原结果；确定的额度不足错误结束重试等待。
- 增补普通/不存在目标、来源及工作区重绑定、内置覆盖授权、准备操作只读、关闭后历史、输入变化、过期结果、分页/过滤、传输重试和跨作业回执验证。认证 TCP DFACP/1 fixture 同时验证 status/prepare/enqueue 与严格请求字段。同步 ACP、维护队列、桌面说明及方案 P4 进度。

验证结果：

- `go test -p 2 ./internal/skills/evolution . -run 'Generation|SkillEvolution' -count=1 -timeout 180s`：7.770s / 9.409s 通过，含新增内置来源只读准备验证。
- `cargo test --offline -p waku -p waku-core --lib deerflow -- --nocapture`：Waku 49 项、core 31 项通过；保留既有 unused/dead-code 警告。此前 `cargo check --offline -p waku --bin waku -p waku-core` 8.08s 通过；生成面板专项 6 项测试通过。
- `go test ./cmd/deerflow-acpd-go -run '^TestConfigDaemonPublishesBridgeIdentity$' -count=1 -timeout 90s`：6.680s 通过，保持真实 MANAGE 的字段 schema 与已应用配置身份契约。
- `go build ./...`、修改文件 Go/Rust 格式检查和 `git diff --check` 通过；同时检查 199 个未跟踪文件，无行尾空白问题。

本阶段完成生成操作界面的源码与相关协议回归，真实桌面鼠标/键盘操作及打包兼容性仍待验收。P4 用户反馈、质量归因、信号自动选择/冷却/去重和行为评测，P5 自动发布/观察期/回滚均未完成。完整目标仍为 P0–P7 和 33 项验收；P2 MCP/任务树、P3 历史导入、P6 旧 Python 源码/依赖/CI 实际删除、P7 无 Python 干净环境及源码/Waku 交付继续保留。此阶段未运行或安装 Python，未修改个人配置、数据库或旧包，未生成新的桌面 Release。

## 2026-10-02 P4 生成配置保存、启动与 Waku 原生设置

- 在共享字段 schema 和严格 decoder 中接入 `generation_model_name`、`generation.enabled/timeout_seconds/max_input_bytes/max_output_tokens` 与独立 `budget`。生成默认关闭，旧模型引用不产生新授权。模型限额零值使用 60 秒/131072 字节/8192 Token；每日调用/Token、作业时间、worker 额度必须为正数，YAML 启用生成时要求全部显式提供。整数形状先于 YAML 解码校验，拒绝小数截断和超限值。
- 保存/validate 与 launch 共用校验，按已配置模型名称解析生成路由，支持多个配置使用同一个 provider 模型 ID。候选来源沿用 custom/managed 及显式 builtin overlay 授权，上限 32。配置映射到 `Config.SkillGeneration` 的实际模型和预算；关闭 evolution 或 generation 均在应用启动时清除该选项，不删除既有来源、作业和回执。
- Waku 新增生成开关、模型选择和七项数值限制（三项模型限额、四项维护预算），复用现有原生控件、编辑代际、后台保存及应用流程；渲染无新增 I/O。三路模型引用随名称修改同步更新。生成和预算的嵌套父/子字段必须同时获得配置工具与运行 daemon 的兼容描述，缺失/hash 改变时限制编辑和保存并保留草稿。
- 四份模板增加默认关闭的生成块和可见的有限维护预算。支持字段保存并保留未确认的未知旧值，显式转换继续绑定文件 revision、备份原内容并保留启用状态；旧 `generation_model_name` 现在作为支持字段保留，转换不会因此打开生成。原回归中“生成模型名是未知隐藏字段”的断言已更新为新契约，并新增保留模型/关闭状态断言。
- 新增真实保存→launch→Eino worker 集成 fixture，验证三路同模型 ID、不同 endpoint/凭据的正确路由，实际输出上限、超大输入在调用前拒绝、单次超时、作业截止时间、每日调用/Token 额度及取消请求保守扣额。成功只到待审，所有场景均不改变发布 head；关闭生成后可读历史且不能入队。

验证结果：

- `go test -p 2 ./internal/configschema ./internal/desktopconfig ./internal/launch ./internal/skills/evolution -count=1 -timeout 180s` 全部通过：0.417s / 47.039s / 11.308s / 9.859s。含 6 个实际生成场景、13 类非法编辑在启用/关闭状态拒绝且不写文件、直接 YAML 启动拒绝、显式预算缺项、重复键、旧配置迁移及既有包回归。
- `go test ./cmd/deerflow-acpd-go -run '^TestConfigDaemonPublishesBridgeIdentity$' -count=1 -timeout 90s`，6.721s 通过；真实 daemon 的 MANAGE schema 与新共享字段描述一致，已加载 revision 不随尚未应用的文件编辑变化。
- `cargo check --offline -p waku --bin waku -p waku-core` 通过（6.61s）。`cargo test --offline -p waku -p waku-core --lib deerflow -- --nocapture`：Waku 43 项、core 31 项全部通过；保留既有 unused/dead-code 警告。未进行视觉测试或桌面手动交互验收。
- `go build ./...`、修改文件的 Go/Rust 格式检查、`git diff --check` 及新增文件尾部空白检查通过。没有运行或安装 Python，也没有生成桌面分发包。

生成作业请求/列表/回执的 Waku 操作界面仍待实施；当前设置启用后可经 SDK/认证 MANAGE 显式请求，不会自动选择 collector 信号。P4 的用户反馈、质量归因、自动选择/冷却和行为评测，以及 P5 观察期/自动回滚均继续保留。完整 P0–P7、33 项验收、P2 MCP/任务树、P3 历史导入、P6 Python 源码/依赖/CI 删除、P7 无 Python 环境与源码/Waku 交付仍未完成；本轮未改个人配置/数据库/旧包，未构建新的桌面 Release。

## 2026-10-02 P4 生成宿主、SDK 生命周期与认证 MANAGE

- `Config.SkillGeneration` 显式配置 Eino 生成模型、正数维护预算、来源白名单和内置覆盖授权，要求具有稳定身份的审查器。SDK 成功初始化后启动生成 worker，关闭时取消并等待模型请求与 worker 结束，再关闭 collector 和数据库；nil 关闭付费生成入口，历史和回执仍可按当前来源权限查询。维护表随 evolution 服务初始化迁移，构造服务本身不调用模型。
- 新增 `df_skill_generation_inputs` 固定完整 target/source/workspace、完整 head、内置覆盖基础及生成/审查身份。操作者请求与目标绑定 request hash，快照与作业 input digest 绑定，候选版本/理由/提案关联绑定 progress hash。输入与入队同事务；不可变候选与阶段进度同事务；提案创建与作业关联同事务，故障不留下已入队但无输入的作业或无进度的候选版本。
- 每次模型准入重新核查来源、完整 head 和配置身份；阶段保存核查当前 claim/lease/state version。重试和重启使用已存候选和提案，不重复生成已完成阶段；只有维护预算上下文可以继续生成提案的审查，禁止通过普通 Validate 绕过预算。审查结果已经提交但作业终态写入失败时，恢复可直接确认该结果，即使操作者已经发布或模型配置已改变，也不重新调用模型。
- 作业失败、租约耗尽与未完成提案关闭通过 terminal hook 同事务处理；显式取消使用作业状态版本，并原子拒绝仍在 validating/pending_review 的提案。已发布提案不能通过生成取消撤销，已完成作业使用普通提案操作。普通失败不改变用户会话状态、不创建用户 run、不重放工具。未知请求用量在关闭/重开后继续保守扣额。
- SDK 提供入队、列表、详情、回执和取消；认证 DFACP/1 MANAGE 对应 `skill.generation.enqueue/list/get/receipts/cancel`。入队要求完整 target/head，固定 actor 为 desktop；列表游标绑定来源/查询/目录 revision，回执按序号分页。来源 ID 绑定到新目录后，旧作业的详情/回执和后续调用均被拒绝。`proposal.history.capabilities.generation` 仅在显式配置时为 true，discovery/probation 保持 false。
- 修正 `validReportFor` 对显式失败 `ModelChecks` 的判定：质量模型返回失败不能成为可批准提案。成功生成和两路内容审查仅产生 `pending_review`，行为评测仍是 `not_run`；最终发布继续要求明确批准。
- 同步维护队列说明、ACP 管理协议及方案 P4 进展，加入 SDK 配置示例、恢复/取消/关闭语义和能力边界。当前生成使用操作者理由及固定基础版本，**尚未接通 YAML/save/launch/schema/Waku 生成设置、自动信号选择、用户反馈、质量归因、行为 fixtures 或自动发布**。信号仍为 `unassessed`；不得据生成作业已可用宣称完整技能自进化已完成。

验证结果：

- `go test -p 2 ./internal/skillmaintenance ./internal/skills ./internal/skills/evolution ./internal/engine/eino -count=1 -timeout 180s` 全部通过：0.671s / 6.216s / 6.466s / 22.142s。覆盖已有 Registry/提案/引擎回归，以及输入入队和候选阶段故障回滚、来源/配置/base 改变、Python 候选拒绝、重启接续、审查重试、取消和终态确认。
- `go test -race -p 2 ./internal/skillmaintenance ./internal/skills/evolution . -run '^TestGeneration|^TestMaintenance|^TestDurableMaintenance|^TestSkillGeneration' -count=1 -timeout 180s` 全部通过：8.384s / 21.342s / 15.018s。
- `go test -p 2 ./internal/launch ./internal/desktopconfig . -run 'Skill|Evolution|Proposal' -count=1 -timeout 180s` 全部通过：19.179s / 24.842s / 41.627s，保持配置保存/启动、人工提案、技能管理及 SDK 生命周期行为。
- 真实本地 Eino provider fixture 与认证 TCP MANAGE 验证生成 + 两路审查、质量失败、两次调用额度阻止第三次请求、人工批准发布、关闭后禁用生成仍可查历史。另验证取消并等待阻塞 HTTP 请求结束，重启保留保守额度，生成失败后同一用户会话仍可正常调用。全部 fixture 使用隔离目录/数据库和本地请求，未调用付费服务或 Python。
- 补充配置启用/关闭时的 MANAGE capability 断言后，`go test . -run '^TestSkillGenerationSDKWorkerAndAuthenticatedManagement$' -count=1 -timeout 120s`，21.504s 通过；`go build ./...`、Go 格式检查及 `git diff --check` 通过。

完整目标仍为 P0–P7 和 33 项验收。分支继续明确采用唯一 Eino Go harness 运行时，P6 必须实际删除旧 Python 源码/依赖/构建链路，P7 同时验收源码归档和无 Python 桌面交付。当前未完成 P0 剩余配置、P2 MCP/任务树、P3 历史导入/桌面验收、P4 后续配置与自动发现/评测、P5 观察期/回滚、P6 批量源码清理及 P7 干净环境交付；未修改个人数据和旧包，未生成新 Waku/Release 包。

## 2026-10-02 P4 维护队列、独立预算与 Eino 候选生成基础组件

- 新增 `internal/skillmaintenance`，保存按来源身份/技能/请求键去重的作业、不可变输入引用/摘要、固定策略、状态版本、领取令牌、15 秒租约、首次领取截止时间及最多三次尝试。队列上限 100，按插入顺序领取，重试保留退避、次数和截止时间；过期 worker 无法重新调用模型、结算或结束替代作业。`RunOne` 绑定 deadline/broker，定时续租并在取消时 join handler 与 heartbeat；构造 store 不会自动启动 scheduler。
- 独立 UTC 自然日账本覆盖所有技能源及 generation/security/quality/behavior 阶段，显式正数额度不允许 0 表示无限。实际请求前同事务计入调用次数和估算输入 + 输出上限；有效提供方用量才能结算释放差额，未知、失败、取消或崩溃保留全额。超出预留的实际用量完整记录并阻止后续准入，跨天结算记回原日期，时钟倒退拒绝继续。重启及重试不能清空已有扣额；现有作业保留自己的日限额，并受当前宿主更紧的日限额限制。这是调用/Token 预算，不是精确费用账单。
- 回执包含作业/attempt、阶段、模型及输入摘要、预留/结算用量和估算标记，支持有界序号分页，不保存请求正文、端点或凭据。预留或结算故障整笔回滚；同一 claim 有未结算请求时拒绝继续调用，同一结算仅允许相同用量重复提交。取消使用状态版本 CAS，不退回未知请求额度。
- 新增 Eino 候选生成适配器，固定技能名称/基础版本，以独立工具为空的上下文返回完整文本文件集合。必须存在维护预算，拒绝父任务预算混用、重复 JSON 键、目标/版本改变、无效/截断输出和工具调用；输出仍需提案服务做包/路径/Python/凭据检查、内容审查及发布 CAS。没有生成目录写入或自动发布权限。
- 现有安全/质量审查通过同一 provider 边界支持维护账本，普通父任务 `skill_manage` 继续使用原根预算。未知或不完整的 Token 计数保守扣额；预算、过期 claim 和时钟错误保持可判断的错误类别，提供方/数据库细节不回显。技能模型关闭 SDK 隐式重试：Ark 使用既有 RetryTimes=0；现有 vendored Claude 增加可选 DisableRetries，默认普通会话行为不变，并更新 PATCH.md。
- 当前仅完成内部组件与契约，**尚未接通生产生成宿主、输入/候选阶段持久化、SDK/MANAGE 入口、YAML/save/launch/schema 或 Waku 表单**。自动信号选择、用户反馈、质量归因和行为 fixture 仍待实施；collector 保持元数据采集，discovery/probation 继续为不可用。新维护表只在显式创建该 store 时迁移，尚未改变正常 SDK 启动行为。内部接入要求见 `go-harness/internal/skillmaintenance/README.md`。

验证结果：

- `go test -p 2 ./internal/skillmaintenance ./internal/engine/eino ./internal/skills/evolution -count=1 -timeout 180s` 全部通过：2.951s / 27.806s / 5.205s。覆盖去重、跨连接重开、共享额度、并发准入、过期 claim、故障回滚、取消、跨天、时钟倒退、三次重试、固定截止时间和既有引擎/提案回归。
- `go test -race -p 2 ./internal/skillmaintenance ./internal/engine/eino -run '^TestMaintenance|^TestDurableMaintenance|^TestSkillMaintenance|^TestSkillGeneration|^TestSkillCandidateResponse' -count=1 -timeout 180s` 通过：5.051s / 8.646s。
- 本地真实 provider fixture 验证生成 + 两路审查共享日额度，2 次额度阻止第 3 个请求发出；各阶段使用正确输出限额且不附加工具。OpenAI/Claude/Ark 的 HTTP 503 均只有一次实际请求和一笔保守扣额。追加缺少、部分及不一致用量断言后，最终 `go test ./internal/engine/eino -run '^TestSkillMaintenance|^TestSkillGeneration|^TestSkillModelValidator' -count=1 -timeout 120s`，7.625s 通过。
- `go test -p 2 ./internal/launch ./internal/desktopconfig . -run 'Skill|Evolution|Proposal' -count=1 -timeout 180s` 全部通过：10.110s / 22.244s / 45.480s。保持现有配置保存/启动与 SDK 技能、人工提案、审查路径。
- `go build ./...`、Go 格式和 `git diff --check` 通过。测试使用隔离 SQLite 和本地 HTTP fixture，未调用付费模型或 Python，未修改个人配置/数据库/旧包，未构建新 Waku/Release。

下一步接入生产 generation 宿主：持久绑定来源、完整 head、证据及生成配置身份；已存候选重启后接续审查，禁止重复生成已完成阶段；完成来源授权、阶段提交与作业 fencing、失败隔离、查询/取消和配置消费链路后再开放能力。完整 P0–P7 与 33 项验收继续保留，P2 MCP/任务树、P3 历史导入/桌面验收、P4 后续流程、P5 观察期/自动回滚、P6 剩余 Python 删除和 P7 无 Python 环境/最终源码与 Waku 交付均未完成。

## 2026-10-02 P4 持久 collector、根任务聚合与查询

- 新增本地 collector，SDK 成功构建后自动启动，关闭时先取消并 join，再关闭数据库依赖。每页最多 256 条读取（宿主默认 128），空闲轮询 1 秒、单步限时 2 秒；只处理 SQLite 元数据，不调用模型、工具或扫描目录。
- `df_skill_collection_jobs` 保存 outcome、游标、版本、令牌、10 秒租约、重试次数和固定错误码。每页的信号关联、游标及最终 outcome ACK 同事务提交；失败保留原进度，单页最多 3 次尝试后进入 failed。退避/次数及失效租约跨重启保留；显式重试要求 failed 状态及当前版本，重复/过期请求返回 conflict，不重放任何用户工作。
- 信号按根任务、来源身份、技能名称及不可变版本聚合。前台/后台/多次读取保留各自证据引用，聚合后只形成该根任务的一条信号；成功、失败、取消及效果不确定分别计数，归因明确为 `unassessed`。核对原始读取与预算根/attempt 和 Registry 文件摘要；无实际技能读取的结束任务只 ACK，不制造技能信号。
- SDK 与本地 MANAGE 新增 `skill.signals.list/get`、`skill.collector.status/jobs/retry`。技能信号仅查询当前配置来源；信号和证据分页绑定过滤条件/身份及版本，变化后要求刷新。采集状态提供整体计数及前 100 条，完整未完成队列通过 outcome 游标继续分页；错误码不包含底层数据库或提供方文本。旧 proposal.history 的 signals 展示仍待 Waku 契约迁移，discovery/probation 继续保持未实现标记。
- 会话及后台任务树删除同步清理采集作业、信号和关联证据，保留安装和发布历史。collector 错误不改变已提交的任务终态；测试验证失败后同一会话仍接受新提示，采集重试不会再次运行用户任务。配置中的生成/自动发布字段尚未开放。

验证结果：

- `go test -p 2 ./internal/runtime ./internal/skills/evolution . -count=1 -timeout 240s` 全部通过：Runtime 62.988s、evolution 4.973s、完整 SDK 包 199.484s。包含既有执行、后台、预算、审批、恢复、清理及人工提案回归。
- `go test -race -p 2 ./internal/skills/evolution -run '^TestCollector' -count=1 -timeout 120s`，12.127s 通过。覆盖分页与根任务去重、整页故障回滚、持久退避/耗尽、CAS 重试、过期 worker 拒绝、构造新服务后接续游标、损坏证据拒绝、其他作业继续推进、来源撤销、游标冲突和关闭 join。
- 补齐完整队列分页后，`go test ./internal/skills/evolution . -run 'TestCollector|TestSkillCollectorManagement|TestSDKSkill' -count=1 -timeout 120s` 最终通过（5.530s / 8.520s）。真实 Eino 本地模型 fixture 的四次读取聚合到一个根任务；SDK 关闭/重开后两次已记录读取仍可采集且没有新增模型调用；MANAGE 查询/分页/非法字段/重试测试通过。原测试断言 outcome 必须一直 pending，已更新为自动消费后的持久终态语义。
- `go test ./cmd/deerflow-acpd-go -run '^TestConfigDaemonPublishesBridgeIdentity$' -count=1 -timeout 90s`，21.229s 通过。实际子进程 daemon 经认证 MANAGE 提供 collector running 状态、空信号及队列查询，正常 STOP/退出；仍核对配置 schema 与已加载 revision。
- `go build ./...`、Go 格式和 `git diff --check` 通过。全部数据来自临时目录和 fixture；未调用/安装 Python，未修改个人配置、数据库和已有便携包，未构建 Waku/Release 包。

完整目标继续为 P0–P7 与 33 项验收。P4 的用户反馈、质量归因、独立维护预算、候选生成作业及行为评测仍未完成；P5 自动发布/观察期/回滚、P2 MCP/任务树、P3 历史迁移/桌面验收、P6 Python 核心/依赖/CI 删除及 P7 干净环境交付继续保留。

## 2026-10-02 P4 实际技能读取与任务终态持久证据

- 在不可变技能快照加入宿主观察器，子角色的受限快照继承同一观察路径。成功 `Get` 记为 `skill_load`，成功的 UTF-8 `read_skill_file` 记为 `support_read`；元数据列表、文件目录、原始管理读取、缺失/越界/二进制及校验失败均不计入。只记录来源、作用域、技能版本、文件路径/摘要/大小及运行身份，不复制正文、提示词、工具凭据或错误内容。
- 主代理和前台角色复用当前 execution broker，后台复用 native attempt broker；写入前在事务中核查当前运行及已保存角色身份、后台后代范围和 Registry 文件摘要。旧 `deerflow-child-v1` 无角色登记的检查点保留 task/run/root 证据，角色留空，不凭空赋予现代角色身份。相同 attempt/角色/文件/读取类型去重，跨 attempt/角色保留归因记录。
- Runtime 增加 `harness_skill_reads`、`harness_skill_outcomes` 和终态 SQLite trigger。任务从非终态进入 completed/failed/cancelled 时，outcome 与业务状态、预算及回执同事务提交；重复终态写入不重复插入，重启不补造历史读取。waiting_input/needs_reconciliation 不计作已结束任务，failed/cancelled 与效果不确定保持独立数据，尚未作技能质量归因。
- 删除会话及其已结束后台任务树时同步删除私有证据；技能安装和发布记录不受影响。该层目前不调用模型，outcome 保持 pending。collector、用户反馈、按根任务归因去重、维护预算、生成作业、重试及行为评测仍待实施，不能据这些记录宣称自动进化可用。契约见 `go-harness/internal/runtime/skill-evidence.md`。

验证结果：

- `go test ./internal/runtime ./internal/skills ./internal/skills/evolution -count=1 -timeout 180s` 全部通过，分别 53.992s / 4.974s / 3.791s。包括观察器范围、重复读取、错误内容不返回、身份/角色/摘要篡改拒绝、前台终态与 outbox 故障回滚、等待/取消/待核对区分和旧后台兼容。
- `go test . -run 'TestSDKSkill|TestSDKForegroundAndBackgroundRoles|TestSDKNativeBackgroundChildApprovalAcrossRestart|TestLocalManagementDeletesDetachedSessionAndPrivateState' -count=1 -timeout 180s`，9.602s 通过。真实 Eino + 本地模型 fixture 验证前台渐进式加载的两次实际读取在 SDK 重启后保留；两个角色同时在前台/后台读取同一版本，记录四次读取、两个角色、两个 run 和同一个根任务。
- 新增后台事务故障及删除后安装保留断言后，`go test ./internal/runtime . -run 'TestSkillOutcomeBackgroundFailureTransaction|TestSDKSkillReadsAttribute' -count=1 -timeout 90s` 最终通过（6.172s / 9.996s）。注入 outbox 写入失败时 native task 和 started 回执一起回滚；重试后 failed + effects_uncertain=1 保留，不捏造技能读取。会话树删除后私有证据清空，原技能版本仍安装。
- `go build ./...`、Go 格式检查、`git diff --check` 通过。测试仅使用隔离目录及本地 fixture；未调用/安装 Python，未改个人配置、数据库及旧分发包，未创建新 Waku/Release 包。

P0–P7 和全部 33 项验收仍未完成。下一步继续 P4 消费这些持久证据及自动候选/评测，同时保留 P2 MCP/任务树、P3 历史导入与桌面验收、P5 观察期/自动回滚、P6 剩余 Python 核心/依赖/CI 删除和 P7 干净环境交付。

## 2026-10-02 P3 托管技能源原生表单

- Waku 技能页新增导入目录、导出根、可空全局来源选择，以及逐来源 ID/root/scope/workspace 编辑和增删操作。选择来源不修改草稿，删除来源声明不删除磁盘文件或历史。来源身份和作用域变化的恢复影响在页面说明。
- 共享 schema 增加 `/skill_sources` 的完整父/子契约、原生路径映射及 32 来源上限；配置工具和运行 daemon 的字段交集控制编辑，缺失或不匹配时保留草稿并限制操作。渲染只读取缓存，变更参与 edit generation。
- 保存与启动共用来源名称、重复/保留 ID、目录、作用域及导出根校验；保存后 Go 实际加载新目录和选择。保留相对路径和 null 继承，清空导入目录删除原生键。关闭状态下未改动的旧非法配置可保留，新修改的非法目录仍在写入前被拒绝，启用时重新完整校验。未知旧根字段只显示路径诊断并保留原值，错误不暴露嵌入内容。
- 技能名称校验提取为共用函数，Registry 原规则保持一致。未调用或安装 Python，未修改个人配置、数据库及既有便携包。

验证结果：

- `go test ./internal/configschema ./internal/desktopconfig ./internal/launch ./internal/skills ./harness -count=1 -timeout 180s` 全部通过，分别 0.340s / 43.713s / 12.951s / 17.042s / 1.376s。包含保存→启动消费、清除旧路径、workspace/global 切换、11 类非法编辑拒绝、不删除文件及未知旧值保留。
- `cargo check --offline -p waku --bin waku -p waku-core` 通过（23.97s），保留现有 dead-code 警告；`cargo test --offline -p waku -p waku-core --lib deerflow -- --nocapture`：Waku 43 项、core 31 项全部通过。
- 来源 schema 更新后复验 `go test ./cmd/deerflow-acpd-go -run '^TestConfigDaemonPublishesBridgeIdentity$' -count=1 -timeout 90s`，6.143s 通过，真实 MANAGE 状态声明与共享描述及已加载 revision 一致。
- 最初来源测试仍假定非法形状到保存阶段才失败，现已更新为严格 snapshot 校验并兼容原生/草稿路径；最终上述回归通过。Rust 格式检查通过。

本节完成来源原生配置的代码及回归验证，真实桌面交互仍待验收，未生成新 Waku/Release 包。完整 P0–P7 与 33 项验收继续保留，尤其 P2 MCP/任务树、P3 历史导入、P4/P5 自动进化、P6 旧 Python 核心/依赖/CI 删除和 P7 干净环境交付。

## 2026-10-02 P3 原生进化设置、明确转换与 YAML 托管技能源

- Waku 已用原生字段替换整段 `skill_evolution` JSON：启用、review 模式、内置覆盖、两个已配置模型选择器、安全审查失败阻止发布，以及超时/输入字节/输出 Token 限额。移除未消费的 generation 模型引用更新，自动发现、行为评测与自动发布继续等待 P4/P5。
- 共享 v2 schema 增加人工审查字段、嵌套 `review_limits`、`skills_enabled` 和旧设置转换 intent。字段 hash 包含 `zero_is_default` 和 `must_be_true`；0 对应 60 秒/131072 字节/1024 Token，非零范围与实际 reviewer 一致。新增非法编辑在关闭状态也被拒绝，未修改的关闭状态历史值可保留。整数校验拒绝 YAML 小数被解码器截断。
- snapshot 只提供已消费的进化字段、未消费字段路径及绑定源 revision 的转换说明，不返回未知值。转换需明确确认当前 revision，设置 review/security=true、保持 enabled 和模型选择，保存时备份原文件后删除已确认的旧进化参数。无关保存保留未知数据及凭据；过期/非法确认、外部文件修改、未确认的旧字段启用均在写入前被拒绝。
- Waku 核对配置工具和运行 daemon 的父/子字段契约；缺失、hash 变化或版本不匹配时限制编辑和保存，保留草稿。转换动作参与 edit generation，保留输入错误草稿并在回调中再次核查当前能力。
- 四份默认模板只生成当前可用的人工审查配置，删除旧 storage/generation/discovery/candidate_limits/auto_patch/monitoring 默认块。模板可仅补齐两个模型并启用，严格 decoder 会使用相同原生默认限额。自动能力继续属于完整目标，不因暂时移除无效默认参数而缩减。
- 新增 `skills.managed_sources`、`skills.export_root` 和 `skills.include_global` YAML 接入。来源按配置文件目录解析，相对路径规范化后复用 Registry 的目录/链接/作用域检查；限制来源数量、重复/保留 ID、未知字段、缺失导出根和越界 workspace。可与旧 `skills.path` 的 public/custom 导入共存，显式 `--skills-config` 仍优先。原 `python-public`/`python-custom` 仅是已保存数据的稳定来源 ID，没有解释器调用。
- 设置保存通过 `InspectConfiguredSkills` 和 `SkillProposalDestinations` 复用启动路径的来源及授权检查，支持没有 custom 导入目录的显式托管源。托管目录不扫描或导入磁盘文件；关闭 evolution 仅撤销模型候选提交授权，保留来源及已发布内容。托管来源原生编辑器仍待实施，当前通过 YAML/CLI 配置。
- 删除 config snapshot 的旧文件技能枚举及 header 读取辅助函数；删除 save 对 extensions JSON 的技能启停写入。`skills` 旧数组请求被拒绝，技能目录通过 Registry 管理；扩展文件原始字节在普通设置保存中保持一致，revision 仍参与并发保护。

验证结果：

- `go test ./internal/configschema ./internal/desktopconfig ./internal/launch ./internal/skills ./internal/skills/evolution -count=1 -timeout 180s` 全部通过，分别 6.732s / 32.690s / 12.669s / 6.033s / 4.417s。覆盖旧数据/密钥保留、备份、转换确认、未知字段、非法限额、模板、save→launch 与来源策略。
- 本地 HTTP fixture 证明保存后两个同模型 ID、不同 endpoint 的审查路由分别使用其显式凭据；512 Token 上限以实际 `max_completion_tokens` 请求发送，4096 字节上限在模型调用前生效，1 秒审查超时取消请求且不进入第二路模型。报告保持 `Behavior=not_run`，没有把内容审查当成行为评测。最初 fixture 使用旧 `max_tokens` 字段并假定换 endpoint 后自动继承隐藏凭据，两项均已按实际 provider/凭据契约修正，最终通过。
- `TestManagedSourcesSaveLaunchAndRegistrySettingsStaySeparate` 增加真实隔离 SDK 启动检查，最终单测 6.251s 通过：新 YAML 加载的 managed root 中存在无效旧文件也不会被导入，Registry 列表为空；配置启停不会重写旧扩展状态。`TestManagedSourceValidationAgreesAtSaveAndLaunch` 覆盖 8 类错误，验证保存与启动一致拒绝且不泄漏嵌入值。
- `go test ./cmd/deerflow-acpd-go -run '^TestConfigDaemonPublishesBridgeIdentity$' -count=1 -timeout 90s` 6.142s 通过，真实 daemon 的 MANAGE 状态提供更新后的同一 schema 和配置 revision。
- `cargo check --offline -p waku --bin waku -p waku-core` 通过；最终 `cargo test --offline -p waku -p waku-core --lib deerflow -- --nocapture`：Waku 40 项、core 31 项通过。编译输出仍有现存的 unused/dead-code 警告。
- 修改的 Rust 文件格式检查、`git diff --check` 通过。未执行真实桌面交互验收，也未生成新 Waku/Release 包；未调用或安装 Python，个人配置、数据库和现有便携包未修改。

完整 P0–P7 和 33 项验收仍未完成。下一步继续来源原生配置、旧 Agent/进化历史导入和 P3 桌面验收，以及 P2 MCP 资源重建/任务树、P4/P5 自动进化、P6 剩余 Python 核心/依赖/CI 删除、P7 无 Python 干净环境与源码/桌面交付。

## 2026-10-02 P6 Bridge 与根目录启动入口统一到 Go

- 删除 Rust Bridge 的 Python daemon 构造器、解释器环境变量选择、便携 Python/父目录 `.venv`/PATH 发现与回退；启动只选择 `--daemon`、`DEER_FLOW_ACP_DAEMON` 或 Bridge 同目录的 Go daemon，按该顺序选择，缺失的显式路径不回退。相对覆盖路径在切换配置工作目录前解析。`--python`/`--python=...` 返回迁移提示，旧环境变量无运行作用。
- `portable_root` 不再以 Python 文件为识别依据；保留配置/resources 标识。DFACP/1、MANAGE、ACP v1 和 v2 协议沿用原实现；Bridge 文档移除旧 Python 运行、uv 安装和 Python 专用 RustFS 能力说明，补齐 Go 启动与测试入口。已有 HTTP 传输适配代码未扩展。
- 根 `acp-daemon.ps1`、`acp-service.ps1`、`start.bat`、`stop.bat`、`stop.ps1`、`daemon.sh`、`start.sh`、`stop.sh` 已统一为 Go ACP 生命周期入口。移除依赖安装、旧 HTTP 服务启动/重启循环及按端口/PID 文件终止；默认 endpoint 位于 `user-data/runtime/acp-go`。旧 `PythonPath` 和 Windows `Port` 参数仅保留明确的迁移错误。
- 实际 PowerShell 启动测试发现 Bridge 仅清除标准句柄继承位仍会泄漏额外管道，导致调用方输出流不能结束。改为 Windows `CreateProcessW`、`bInheritHandles=false`、空标准句柄和 `CREATE_NO_WINDOW`，不改进程全局句柄标记；采用锁定版本中已有的 windows-sys 0.61.2，并校验 Windows 参数引用与 NUL/长度限制。测试设置有界 WaitDelay，泄漏回归会失败并执行 fixture 清理。
- 同一实测发现 Windows PowerShell 5.1 会将 stderr 转成终止错误，阻断“已停止”的幂等处理；现在仅在 native 调用期间捕获错误记录，再按退出码解释，恢复调用前的 ErrorActionPreference。失败的旧测试已结束，其专用测试 daemon 已通过认证 STOP 清理。
- Python 清单补入先前遗漏的 8 个根脚本基线，现为 545 个不重复路径。Windows 启动路径记录已验证，三个 Linux 脚本标记 `replaced_syntax_verified`，不将语法检查记为原生运行验收。CI 增加根脚本变更触发、Go daemon 构建，以及真实 Bridge 自动发现/脚本启动测试。

验证结果：

- Rust `cargo test --offline`：16 个单元测试、2 个真实 Bridge 负向启动测试全部通过。覆盖 missing Go 与 `.venv`/内嵌 Python/旧解释器环境变量/PATH 同时存在、旧参数立即拒绝、配置/环境覆盖优先级、MANAGE framing、参数引用和既有传输测试。
- Windows 上实际编译 Go daemon 后执行 `go test ./cmd/deerflow-acpd-go -run '^TestExistingRustBridge' -v -count=1 -timeout 180s`：同目录/显式相对路径/环境变量相对路径启动，STATUS、MANAGE、ACP 会话、STOP，以及既有 ACP v1/v2 生命周期均通过。该轮的脚本重复停止失败已修复；最终脚本专项 `TestExistingRustBridgeGoLaunchScripts` 28.02s 通过，覆盖 start/status/restart/stop/再次 stop、停止后的 status 失败和旧参数诊断。
- daemon/localhost 两包回归通过（2.176s/0.433s）；最终 `cargo check --locked --offline` 通过。最终 PowerShell 脚本解析、Ubuntu-22.04 WSL 的三个 Bash 脚本 `bash -n`、Rust 格式与 `git diff --check` 通过。新增 Linux 实机启动测试已接入 CI；当前 WSL PATH 中未发现 cargo/rustc/go，尚未执行该原生链路。

所有模型配置、数据库和 endpoint 均来自隔离测试目录；未改个人配置或运行中的 Waku 包。只构建验证用 Bridge/Go 二进制，未生成 Waku/Release 包。P0–P7 和 33 项完整验收仍未完成，尤其 P2 MCP/Waku、P3 完整配置、P4/P5 自动进化、P6 剩余源码/依赖/CI 删除及 P7 干净环境交付继续保留。

## 2026-10-02 P6 WSL 沙箱移除内嵌 Python

- 新增独立 `cmd/deerflow-wsl-helper` 与 `internal/wslhelper`，只使用 Go 标准库和 Linux 系统调用；删除旧 `internal/sandbox/supervisor.go`，将 Python 探测/目录解析/监督替换为 Go helper。主协议版本为 `deerflow-wsl-v1`，启动前核验版本、SHA-256、架构、目录和 WSL2/pidfd/subreaper 能力；每次执行复验宿主持有的文件身份与摘要。
- 请求经 stdin 传递，控制消息与工具输出使用有界协议。stdout/stderr 数据与最终回执分别携带版本和请求 ID；缺失、截断、重复、格式错误或身份不符的回执，以及未确认的传输退出均保持 uncertain。子进程普通非零退出仍为 failed，不扩大为效果不确定。
- Go helper 以 subreaper 管理后代、单一 Wait4 所有者回收；先获得 pidfd 并验证父身份再终止进程。取消、超时、stdin EOF、输出断连均经过清理；SIGPIPE 处理保留断连后的清理机会，清理不能确认时不报告成功。WSL 仍使用宿主/发行版权限，不提供额外的操作系统隔离。
- 新增 `scripts/build-wsl-helper.ps1` 交叉编译 Linux/amd64，并输出 `.sha256` 和协议/平台/摘要 `.json` 清单。Windows Go ACP 与 Waku 打包脚本已接入 helper；桌面打包前核验摘要/清单。新增 CLI helper 路径和摘要参数；未设置的新配置字段不改变原 checkpoint 沙箱策略的 JSON 编码。
- Python 删除清单的 WSL 三项已记录实际替换、删除位置及验证。其余 Python 核心、启动回退、旧构建/依赖/CI 的批量删除仍待执行，清单共 537 项，不能将本次替换视为整个分支收敛完成。

验证环境为 Windows 宿主 + 已安装的 Ubuntu-22.04（WSL2，内核 `6.18.35.2-microsoft-standard-WSL2`）。真实 guest 命令使用系统工具绝对路径及 `PATH=/deerflow-no-interpreters`，未安装/调用 Python，未终止共享发行版。这证明所测执行路径不依赖解释器，不代表用户发行版中没有安装 Python。

已通过：

- 带真实 WSL 环境变量的 `go test -p 2 ./internal/sandbox ./internal/tools ./internal/launch -count=1 -timeout 180s`，三包分别为 15.939s、2.091s、3.149s。覆盖字面量 argv/特殊字符工作目录、显式环境与宿主凭据隔离、正常/非零退出、输出截断、取消/超时、setsid 后代清理、路径映射，以及 helper 缺失、摘要不符和执行前文件变化。
- Linux helper 测试交叉编译后在真实 WSL 内通过：EOF、broken_output、deadline 后实际后代消失；版本/摘要不符时命令未启动；不向非拥有的进程身份发送信号。
- `go test -p 2 . ./harness -run 'TestSDKCommand|TestWSLHelperOptions' -count=1 -timeout 120s`，SDK 16.754s、harness 0.924s；覆盖命令审批先于进程/回执、取消等待真实退出，以及新增可选配置不破坏旧 checkpoint 策略编码。
- 使用相同真实 WSL 配置执行 `go test -race -p 2 ./internal/sandbox -run 'TestWSL|TestTimeoutAndLifetimeCancellation|TestProcessTreeCancellationAndCloseJoin|TestBoundedOutput' -count=1 -timeout 120s`，19.562s 通过；验证本次 stdin 请求/取消单一写入者、输出分帧与进程清理的并发路径。
- `go build ./...` 通过；共享 helper 构建脚本实际执行成功。三个最终 PowerShell 构建脚本语法解析通过。

仅在 `.build-cache/wsl-go-helper` 生成验证辅助程序；未生成新的 Waku/Release 包。尚未执行卸载 Python/uv/pip 的干净环境构建、全分支源码/包扫描和完整 P7 桌面验收。

## 2026-10-02 P0/P3 Go 原生工具参数与宿主权限表单

- v2 字段协议新增 tools/sandbox：逐参数声明适用工具、类型、默认值、数值上限、站点列表语义和 restart 应用方式。配置保存与启动复用实际工具参数校验；站点名、重复站点、类型、结果数和超时无效时拒绝。原生 CLI 沙箱选择及其实际生效状态仍需单独接通。
- Waku 用逐工具参数和添加/移除配置操作替代整个 tools JSON，覆盖网页搜索、抓取、图片搜索、OpenCLI、glob/grep 的有效配置。文件/命令工具没有额外参数时明确说明；移除参数条目不冒充禁用工具。允许/拒绝列表改为逐行名称，MCP 保留已有独立 Rust stdio 契约。
- 移除 tool_groups 编辑器及无效的组存在性约束；移除 sandbox 高级 JSON、旧 mounts/输出上限白名单和 use 投影。快照使用 provider=local 和两个真实宿主授权字段。其他 provider 的转换仍需显式操作；原始分组、未消费工具/沙箱字段及其中凭据保留在原文件并仅显示路径诊断，待完整迁移，不能通过草稿注入。
- 工具密钥与含凭据代理地址不返回明文，提供配置存在标记、原身份恢复和明确清除操作。重排及连续保存保留凭据；更换工具身份或移除后新建不借用旧凭据。OpenCLI 的 null 保持内置站点集合，[] 不允许适配器访问；顶层发现命令仍沿原契约可用。保存清除已识别工具 use 别名，新条目没有 Python 类路径。
- 新增保存→Go 启动→真实本地 HTTP 代理→Go web_fetch 的联测，核对代理认证与正文字符上限。核对全部 13 种配置条目、OpenCLI 路径/超时/站点列表、各工具结果限制、密钥保留/清除、未知历史数据保留、非法草稿不写盘以及保留数据合并后仍可被原生启动器解析。测试使用临时配置和 fixture 凭据。

验证：首轮完整 configschema/desktopconfig/launch 包通过（6.416s/13.400s/12.522s）。新增联测及最终保存校验后，完整 configschema/desktopconfig 包通过（0.094s/13.410s）；launch 的一项用例仍断言旧 sandbox.use 错误文案，改为新原生 provider 提示后完整 launch 包复验通过（7.583s）。定向 tools 与真实 daemon schema 声明测试通过（0.305s/7.584s）。Waku bin/core check 通过；最终 Rust DeerFlow 相关测试 Waku 37 项、core 31 项，均通过、无跳过。Go 全包 build 通过，格式检查通过。CRLF 工作树的 diff 检查使用 `core.whitespace=cr-at-eol`；既有非相关 unused/dead-code 警告保留。

**整体任务仍未完成**：本节未进行真实桌面交互、远程模型或真实 MCP 验收，没有执行 Python、改动个人配置/数据库、提交代码或生成发行包。工具/沙箱的已应用值与 CLI 来源、技能进化表单、完整配置迁移及默认模板清理仍待完成。P2 MCP 重建/任务 UI、P4/P5 自动发现与行为评测/auto_patch/监控，以及 P6 Python 源码与 WSL 解释器删除、P7 无 Python 源码和桌面交付继续保留完整范围。

## 2026-10-02 P0/P3 子代理规范控制、显式迁移与运行状态

- Go 配置新增格式边界 `config_version: eino-harness-v1`，子代理统一为 `subagents.enabled`（默认 false）和 `subagents.max_concurrent`（默认 2，1–64）。MaxConcurrent 直接接受原生 YAML；Waku 删除重复 ACP 开关/并发字段。字段协议升级为 `deerflow-settings-v2`，旧协议服务不会被当成新契约使用。三个默认资源模板及 config.example.yaml 的控制字段已更新。
- 无版本或旧数字版本 1–16 的配置保留原启动规则：ACP 开关缺省 false、角色开关缺省 true，角色 false 仍是不可由 CLI 重新开启的旧硬限制。只在显式迁移后使用新格式的 CLI 优先规则。新旧字段混用、未知版本、类型或范围错误均给出路径诊断，不能静默采用另一套默认值。
- 快照按旧开关 AND 生成等效启用草稿，并保留并发数；提供原开关值、目标值和 CLI 优先级变化说明。界面确认绑定原始配置 revision；未确认或源文件变化时保存拒绝。校验可以检查拟转换草稿，保存先备份原文件、写原生版本并删除两个旧 local_acp 键。成功后清除迁移确认状态。未修改个人配置或既有便携包。
- daemon 的 `subagent_settings` 独立版本化报告当前宿主的实际开关、配置值、CLI/YAML/legacy/default 来源、task 工具策略限制和根并发上限/来源，并关联启动时配置 revision。Waku 只读展示该报告，服务停止或报告不兼容时显示未知；不会使用草稿代替实际状态。已有会话和任务快照仍可能更严格。
- 新增旧开关的缺省/false/true 与 CLI 组合测试、旧/新格式歧义拒绝测试、迁移确认/版本冲突/逐字节备份/删除旧键/保存后启动消费联测，以及 UI 确认与版本化状态投影测试。真实 daemon 测试用 native YAML 关闭、显式 CLI 开启创建 ACP 会话，核对实际 session 状态与报告一致；运行时修改磁盘配置后，状态及 revision 保持已应用值。

验证：完整 configschema/desktopconfig/launch 包测试通过（6.166s/16.973s/11.119s）；加入实际状态报告后的最终定向命令为 `go test -p 2 ./internal/configschema ./internal/desktopconfig ./internal/launch ./cmd/deerflow-acpd-go -run 'TestSubagent|TestAppliedSubagent|TestApplyConfig|TestSettingsContract|TestConfigDaemonPublishesBridgeIdentity|TestNativeRole' -count=1 -timeout 90s`，四包通过（6.075s/11.656s/12.588s/7.322s）。Waku bin/core check 通过，最终 `cargo test --offline -p waku -p waku-core --lib deerflow -- --nocapture` 通过：Waku 34 项、core 31 项，无失败/跳过，保留既有非相关警告。

最终代码的 `go build ./...` 通过；gofmt/rustfmt 和 `git diff --check` 通过。上述定向测试与构建不替代全树回归、真实桌面、远程模型/MCP 或 WSL 验收。

范围仍未完成：本次只迁移子代理控制字段，其他 Python 类路径/历史数据、工具/沙箱/技能进化完整表单、其他设置的实际值/CLI 来源、真实桌面验收仍待实施。P2 MCP/任务 UI、P4/P5 自动进化、P6 Python 删除/WSL Go helper、P7 全量验证与交付保持原范围；尚无新的 Waku 发行包。

## 2026-10-02 P0/P3 逐角色表单与嵌套字段契约

- Waku 移除两个角色 map 的 JSON 编辑器，新增内置/自定义角色选择、创建/删除/重命名、自定义角色名称、内置覆盖重置，以及描述、指令、模型、temperature、输出/thinking Token、thinking 三态、reasoning、工具/技能/拒绝列表、委派、轮次和超时字段。仅选择内置角色不会生成覆盖；编辑时才创建稀疏对象。
- 角色重命名在同一份草稿中更新所有内置/自定义角色的 delegate_to，拒绝冲突、保留名和非法名称。仍被其他角色引用时拒绝删除，恢复内置配置只删除覆盖。角色选择使用集合和名称身份，名称确认和保存保留所选角色。所有操作只修改草稿，保存/应用继续使用现有后台配置流程。
- null 与空列表、数值 null 与 0、thinking null/true/false 分别保留。工具和技能列表逐行输入，`[]` 表示不授予；内置 bash 的空白工具覆盖仍使用内置 bash 集合。旧未知字段有迁移提示并保留原值，格式不兼容的父对象不能被字段编辑静默替换。
- Go schema 新增角色通配路径、嵌套 model_settings、严格工具/角色名称列表与 UTF-8 字节上限；Defaults/必填检查限定直接子字段，防止嵌套路径成为错误顶层键。共享校验同时检查 draft/native 中存在的角色字段，错误定位到具体角色路径；角色超时在 schema 与 harness 统一为 0.001–86400 秒，角色描述/指令按原生 4096/65536 字节限制。
- Waku 对每个嵌套字段核对配套工具和运行 daemon 的声明；整角色操作要求完整字段契约，重命名同时要求委派引用可编辑。缺失/不匹配时限制编辑及保存并保留草稿；缓存刷新及渲染不读文件、不发 IPC。

验证：`go test -p 2 ./internal/configschema ./internal/desktopconfig ./internal/launch ./internal/agents -count=1 -timeout 90s` 四包通过（6.119s/12.187s/5.839s/0.694s）。新增默认投影/嵌套字段路径测试，并在 save→native launch 联测中验证未知嵌套字段、非整数轮次、过小超时、中文指令字节边界及错误 thinking 类型均被拒绝、无配置写入。`cargo check --offline -p waku --bin waku -p waku-core` 通过；最终 `cargo test --offline -p waku -p waku-core --lib deerflow -- --nocapture` 通过，Waku 31 项、core 31 项，无失败/跳过。新增角色选择/稀疏编辑、继承值、委派重命名、创建/删除/重置、未知数据保留及嵌套能力交集测试。既有非相关 unused/dead-code 警告保留。

最后复核：`go build ./...` 通过，真实 daemon 的 `TestConfigDaemonPublishesBridgeIdentity` 通过（6.231s），字段声明与配置工具一致；gofmt/rustfmt 及 `git diff --check` 通过。没有以这些定向结果替代全树回归、真实模型/MCP/WSL 或桌面交互验收。

未完成范围：角色的真实桌面交互验收、原生开关/并发键迁移、其余工具/沙箱/技能进化表单、CLI 覆盖/有效值及旧数据显式导入仍待实施。P2 MCP 重建和任务 UI、P4/P5 自动进化、P6/P7 Python 源码与启动/构建链清理和最终交付继续保留；当前没有新的 Waku 发行包。

## 2026-10-02 P0/P3 模型与子代理设置、旧主 Agent 链路移除

- 共享 schema 扩到 models/default_model/subagents；模型数组字段使用通配路径，Waku 按具体索引匹配当前包与 live daemon 的字段声明。增加子代理空值数值输入，保留 null 继承与显式 0 的区别。模型集合操作与字段编辑也受能力校验，草稿刷新不覆盖输入。
- 模型 snapshot/save/UI 改为原生 `provider`（openai/ark/claude）与 `context_window`；删除 use_path、supports_thinking/supports_reasoning_effort 默认项和任意 advanced 编辑/回写。原后端未知数据在无关保存中保留，更换 provider/endpoint 时不迁移旧凭据及扩展；模型 probe 修正为实际原生 provider，包含 Ark。
- 修复实际消费的 `api.model_name` 旧别名：快照在 ACP 模型字段展示其值，保存规范为 local_acp.model_name 并清除别名，模型重命名/清空覆盖值后不会被隐藏引用遮蔽。原全局默认与会话模型引用仍区分。
- 删除旧主 Agent 的目录扫描、初始化、保存/重命名/删除、SOUL 编辑、目录备份和回滚替换代码；Waku 删除旧增删/名称编辑流程与路径展示。历史目录不改动，损坏的旧配置不阻止原生设置保存；携带已移除 `agents` 字段的旧编辑请求拒绝执行。
- 子代理页独立于旧 Agent 目录，提供总开关、ACP 开关、根并发、超时/轮次、深度、后台槽数、任务数、自动续跑及额度。角色暂使用独立的内置/自定义 map 编辑器，移除整个 subagents 的重复 advanced 副本。保存使用与启动相同的 `agents.DecodeConfig` 及 `ValidateModelOptions`，校验模型存在、内置角色名、权限列表、委派循环及所有继承模型的 provider 参数；无效输入不落盘。
- 新联测验证保存 → 原生启动 → 角色 Factory 的独立 Claude 路由、视力/上下文窗口、thinking 与 token/temperature 参数、null 继承/显式空工具和技能、角色及全局限额；对未知字段、凭据、模型、内置角色、循环、provider 选项和深度验证保存/启动共同拒绝。另验原 provider 扩展保留及后端切换隔离、旧 Agent 目录不读不改不备份、旧模型引用清空/重命名。

验证：Go desktopconfig/launch/agents 全包通过（12.729s/7.348s/0.592s），真实 daemon schema 声明测试通过（6.918s），Go 全包 build 通过。最后补充能力交集/可空数值测试及模型旧别名修复后，执行 `go test -p 2 ./internal/desktopconfig ./internal/launch ./cmd/deerflow-acpd-go -run 'TestNativeModel|TestNativeRole|TestLegacyMainAgent|TestSettingsContract|TestApplyConfig|TestConfigDaemonPublishesBridgeIdentity|TestModelReorder|TestNestedSecrets' -count=1 -timeout 90s`，三包通过（12.240s/11.277s/8.153s）。Rust `cargo test --offline -p waku -p waku-core --lib deerflow -- --nocapture` 最终复验通过，Waku 26 项、core 31 项，无失败/跳过；新增一项能力测试并删除已移除的旧主 Agent 编辑器测试。此前 Waku bin/core check 通过；格式和 `git diff --check` 通过。既有非相关 unused/dead-code 警告保留。没有用这些定向验证代替全树回归或真实桌面验收。

**未完成范围不缩减**：完整逐角色表单及嵌套字段能力、两个子代理 gate/并发键的规范迁移、工具/沙箱/进化表单、effective settings/CLI 覆盖、skills snapshot/save 旧导入链、旧主 Agent 显式导入仍待接通。本次没有真实桌面交互或远端模型验收，没有修改个人配置/数据库，也没有构建或替换 Waku 发行包。P2 MCP 重建、P4/P5 自动进化、P6/P7 Python 删除及最终源码/桌面交付仍未完成。

## 2026-10-02 P0/P3 运行与记忆设置契约及首批清理

- 新增 `internal/configschema`，目前明确覆盖 runtime/memory 两组，提供版本、字段与 schema 摘要、类型/null/default、范围/枚举、配置路径及 restart 应用方式。Go 配置 snapshot 和 daemon status/drain/resume 声明同一契约；携带过期 schema 的保存拒绝写入。
- 保存与启动共用字段校验：子代理并发 1–64；运行/排队超时 0.001–86400 秒，避免精度截断为零；整数拒绝小数；指令上限 65536 个 Unicode 字符；工具列表复用原生规范化与策略校验。记忆 tool/retrieval 和 dormant global 约束共用规则。
- Waku 删除旧默认 Agent、全局 thinking/plan、goal_* 运行输入，以及记忆模型、路径、数量/阈值、防抖、存储类及任意高级 JSON。记忆仅保留四项原生设置。global 不作为可选范围，保留关闭记忆时旧值的诊断说明；子代理开关/并发暂仍使用已有 local_acp 原生入口。
- 这两组输入按当前包的配置工具和已运行 daemon 的字段定义取交集。缺失/未知版本/类型不符/字段变化时相关输入只读，保存拒绝提交不兼容文档；停止服务时采用配置工具契约。能力刷新只更新内存投影，不覆盖草稿或输入；编辑回调再次检查权限，渲染无 I/O。
- 配置快照/默认值不再生成 Python 记忆类、无效记忆参数、memory.json/独立 FTS 路径或废弃运行字段。保存仅更新原生记忆字段，清除会遮蔽新值的 retrieval 旧别名；保留原文件中的其余未知数据和凭据，等待显式迁移。API timeout fallback 与原生空值默认也保持一致。
- 新增 save → native launch 联测，验证实际 Config 中的模型、并发、预算/队列超时、清理参数、记忆策略、空白名单与长中文指令；覆盖无效值不写盘、未知 draft 键拒绝、保留原始迁移数据、无 legacy 默认注入、过期 schema、旧 fallback。实际 daemon 进程测试检查本地状态声明与配置工具一致。

验证：首轮 Go desktopconfig/launch 全包通过（8.563s/12.015s），SDK/daemon 的状态与配置定向测试通过（6.629s/12.421s）。最终代码执行 `go test -p 2 ./internal/desktopconfig ./internal/launch ./cmd/deerflow-acpd-go -run 'TestSettingsContract|TestInitSnapshotSave|TestNestedSecrets|TestApplyConfig|TestConfigDaemonPublishesBridgeIdentity' -count=1 -timeout 90s`，三包分别通过（7.570s/13.078s/6.623s）；`go build ./...` 通过。Rust `cargo check --offline -p waku --bin waku -p waku-core` 通过；`cargo test --offline -p waku -p waku-core --lib deerflow -- --nocapture` 通过，Waku 26 项、core 31 项，均无失败/跳过。格式和 `git diff --check` 通过。既有非相关 unused/dead-code 警告保留。

**仍未完成**：其他页面的 schema 与复合校验、角色/evolution 完整表单、CLI 覆盖及实际生效值、实际数据库位置、显式迁移和真实桌面交互验收。旧 agents/skills snapshot/save 链路及模型/工具/沙箱任意 JSON 仍须清理。P4/P5、P6 Python 删除（含 Go WSL 辅助程序）和 P7 无 Python 源码/桌面发行包继续实施；没有执行 Python、改写个人配置或数据库、生成/替换 Waku 发行包。本阶段不能代表 P0/P3 或整体目标完成。

## 2026-10-02 P3 Waku 技能目录和审查界面接线

- Waku 移除 `/skills/*/enabled` 的旧配置编辑入口，使用运行中 Go Registry 的分页目录和详情。保留全局 skills 开关属于配置草稿的语义；显示禁用项/墓碑、来源、作用域、当前内容/状态版本、内置覆盖来源、源变化报告及导出状态。
- 启停直接提交完整 target/head 和 request key。源变化刷新只读已保存导入来源；导出重试携带已观察的 state token，成功显示已排队，用户通过详情刷新查询完成结果。删除项不提供启用按钮。
- 目录、提案和发布历史各保留一页，单页 20 条及有界游标栈。提案列表与历史修订使用独立游标；目录发布 revision 变化后要求刷新，导出进度本身不会作废目录游标。所有 I/O 继续由现有后台 executor 执行，渲染只读取缓存。
- 提案详情保留 diff，并增加内容/行为检查、规则版本、基础/候选摘要、审查记录、模型审查理由及用量。`not_run` 明确显示未执行行为评测，不能当作通过结果。
- 批准/拒绝提交观察到的 `expected_state_version`。回滚先按历史记录的完整 target 查询当前 head，用户确认后提交 source/target/head/revision/request key；Go 新增可选 target 校验，新格式必须提供完整 head 和 key，拒绝源 ID 重绑定，保持旧调用兼容。
- Waku core 新增内部 `manage-envelope`，保留后端结构化错误码，其他旧 `manage` 消费者保持原协议。冲突及授权变化清除可操作详情；传输失败/无效响应保留原修改请求。同一 key 重试，不允许新修改覆盖待核对请求；独立读取失败也不能丢弃该请求。
- 服务停止、重新应用和已应用代际变化作废缓存选择，旧代际响应不能被接受为当前状态；保留配置草稿。更新 [Waku 技能管理说明](../desktop-app/docs/deerflow-skills.md)、MANAGE 文档和设置审计。

验证：Go `go test . -run 'TestSkillCatalog|TestSkillEvolution' -count=1 -timeout 75s` 通过（8.444s）；Rust `cargo check --offline -p waku --bin waku -p waku-core` 通过。`cargo test --offline -p waku -p waku-core --lib deerflow -- --nocapture` 第一轮与最终代码复验均通过，Waku 23 项、core 31 项，无失败/跳过；最后一次增量编译 17.85s。既有非本功能 unused/dead-code 警告仍存在。`git diff --check` 通过。没有把定向测试当作全量 Go/Rust 回归或真实桌面验收。

**P3 和桌面验收仍未完成。** 本节使用普通代码/协议测试，没有运行真实桌面操作、远程模型或真实 MCP，没有生成或替换 Waku 发行包。共享能力 schema、完整配置表单、snapshot/save 旧技能字段收敛、实际键盘/指针交互和打包兼容验收仍需后续完成。P4/P5 自动发现、行为 fixture、auto_patch/监控，以及 P6/P7 Python 删除和完整交付范围不变。

## 2026-10-02 P3 技能目录、持久启停与定向管理

- 新增 Registry `QueryCatalog`/`GetCatalogSkill` 和对应 SDK。按 name/source identity 分页，包含禁用项、墓碑、内置/托管源属性、覆盖来源、源报告及导出状态。单次 SQL 快照读取元数据，列表/详情不读取技能正文或源目录；上限 100 项、约 2 MiB。游标绑定源策略、导出根身份、过滤条件及 catalog revision，发布状态变化后返回冲突。
- 新增带完整 target/head 和稳定 request key 的 `SetEnabledChecked`。启停、持久托管标记、导出排队和审计在同一事务提交，重启导入不能重置用户决定；仅改启停不创建内置内容覆盖层。旧快照选择不变，删除项不能借启用复活。
- 审计记录操作、actor、完整意图摘要和原 applied head。重试返回原 applied head 与当前状态，不能覆盖之后的修改；同 key 不同意图、过期 head、源 ID 重绑定均拒绝。补充审计失败导致整笔事务回滚和损坏审计检测。
- 接通 `RefreshCatalogImport` 定向源检查及 `RetryExportChecked`。后者核对完整 head 和导出状态 token（包含导出根身份及 worker 状态），排队重置与审计同事务完成；重发相同请求不会再次清空重试次数。
- 修正导出 worker 的提交条件，增加已观察的更新时间校验。旧执行即使 head/attempts 相同，也不能覆盖用户刚排入的新重试。已编辑的导出视图保留，修复生成新视图。
- 认证 DFACP/1 MANAGE 新增 `skill.list/get/set_enabled/imports.refresh/export.retry`，复用相同后端；拒绝缺失、null 或未知的必要字段，actor 由宿主赋为 desktop。SDK 和协议说明已更新。
- 验证覆盖同名跨源分页、状态过滤、过期游标、源/导出根重绑定、启停跨重启、旧请求重放、并发 CAS、审计回滚/损坏、源变化/墓碑报告、旧导出执行晚到、修复已编辑视图，以及实际带 token 的本地 socket 请求。

本节验证全部通过：

```text
go test ./internal/skills -run TestCatalog -count=1 -timeout 60s
go test . -run '^TestSkillCatalog' -count=1 -timeout 60s
go test -race -p 2 ./internal/skills/... . -run 'TestCatalog|TestSkillCatalog|TestManaged|TestSkillEvolution|TestSkillsPublished|TestSkillsExplicitStartup|TestProposal' -count=1 -timeout 150s
go build ./...
git diff --check
```

定向竞态结果：registry 7.657s、evolution 9.010s、SDK 12.656s。本节没有重新执行全量 Go 测试；上次全量结果对应本节修改之前。尚未修改 Waku 的技能列表/按钮，尚未构建桌面包。完整配置、共享能力 schema、Waku 接线及 P4–P7 仍待完成；不能将此后端增量视为完整交付。

## 2026-10-02 方案补充与待验证状态

- 明确最终 Go SDK、ACP、daemon、配置工具、Waku/Bridge 和构建入口，旧 Python 源码删除继续作为 P6/P7 必交付项。
- 检查发现 Go WSL 沙箱仍调用 `python3`，且 `supervisor.go` 内嵌 Python。方案新增 Go Linux 辅助程序替换及真实 WSL 验收；删除/替换清单由初始 534 项扩为 537 项，新增项均为 `planned`。
- 接收此前运行中的全量测试结果时，SDK 的 `TestSDKBackgroundParentContinuation` 在 `BB_automatic`、`BFB`、`BFB_automatic` 报 `session is busy`；其余列出的包通过。后续定位并修正测试中的提前停止和清理时序，最新全量结果已通过，详见本日原生续跑专节。
- 后续实施已完成后台父角色原生续跑的定向验证，包括 FBB、来源损坏及删除；见下文新增专节。P2 仍有 MCP 重建与 Waku 待完成，尚未实施 WSL 替换或构建新桌面包。

## 2026-10-02 P3 技能候选、发布与管理接口进展

- 新增 `internal/skills/publications.go`：候选深复制、原子 staging/发布、单调 head 状态版本、catalog 版本及托管安装控制。沿用 `df_skill_versions/files`，没有另建竞争的技能内容库。
- 启动改为 `InstallConfigured`。已托管的版本、启用状态、回滚及删除墓碑在重启后保留；旧导入文件消失也不会覆盖发布状态。显式导入拒绝覆盖托管版本，旧检查点仍能读取原快照。
- `internal/skills/evolution` 实现候选、评测、待审、批准、拒绝、stale、发布历史与回滚。首个修改前记录原导入版本，支持回滚至第一次修改之前。批准事务同时更新安装指针、提案和发布记录；重复提交/批准/带 key 回滚幂等。
- 支持完整 create/edit 包、`Content` 主文件编辑、精确 `patch`、支持文件写入/删除及技能删除。仅改变不可变候选区，审查前不写入正式技能源。补充 Python 依赖、引用、大小及路径检查。
- 新增公开 SDK 类型和操作，接通认证 MANAGE 的 `proposal.list/get/approve/reject/history/rollback`；复用服务，保留旧 envelope、`base_sha256` 与整数 revision。列表分页并绑定配置源身份；回滚在跨源同名时要求明确来源。
- 审查校验原始请求摘要、评测摘要、不可变内容及展示 diff。发布 CAS 同时核对内容和单调状态版本，识别回滚到同一内容的 ABA 情况。
- 新增 `NewSkillModelValidator`，复用 Eino provider，安全和质量审查使用两个独立、无工具上下文。限制输入/输出/超时，报告记录模型、提示及配置版本、输入摘要和用量；响应缺字段/重复字段、错误摘要、截断或 provider 失败均阻止待审。行为检查明确标记 `not_run`，不能用于批准含脚本候选或自动发布。
- 已验证 SDK→带 token 的 DFACP/1 MANAGE→批准/回滚→关闭重开的链路，以及旧调用兼容、错误字段、重复请求、旧源身份隔离、分页、存储内容损坏、事务回滚与候选操作。模型适配器验证使用本地 provider fixture，尚未进行真实远程模型验收。

**P3 尚未完成。** 后续已接入人工候选的 YAML 子集、按主/子代理会话和角色授权的 `skill_manage`、内置覆盖层、显式托管源/导出后端及本日 `skill.*` MANAGE 管理操作，详见各专节。完整配置、Waku 审查/分页/版本前置条件展示仍待完成。P4 持久队列、维护预算及行为 fixture 尚未接入，P5 自动发布/监控未实现。没有通过旧设置里的开关对外宣称这些能力已可用。

协议和 SDK 说明见 [Go 技能管理](../go-harness/internal/acp/skill-evolution.md)。旧 Python 源码的大批删除及 WSL Go 辅助程序仍属于未完成的 P6；没有构建或替换桌面包。

本节当前代码的专项验证已通过：

```text
go test -race -p 2 ./internal/skills/... ./internal/engine/eino . -run 'TestPublication|TestCandidate|TestProposal|TestSkillEvolution|TestSkillModelValidator|TestSkillsPublished|TestSkillsExplicitStartup' -count=1 -timeout 150s
```

实际结果：registry 1.874s、evolution 6.983s、Eino adapter 9.815s、SDK 12.706s。随后执行当前代码的 `go test -p 4 ./... -count=1 -timeout 240s`，所有 Go 包通过：SDK 197.113s、runtime 76.900s、daemon 26.006s、Eino adapter 33.328s、registry 5.893s、evolution 4.195s、集成 tests 55.214s。`git diff --check` 通过。没有据此宣称 Rust/Waku 构建、真实远程模型、真实 MCP 或无 Python WSL 验收通过。

2026-10-02 重新查询 Go 模块源，Eino 最新预发布版仍为 `v0.10.0-alpha.35`，本轮没有浮动升级依赖。

## 2026-10-02 P3 模型候选工具与人工审查配置

- `skill_manage` 已接入主代理、前台及后台角色。SDK 的 `SkillProposalSources` 显式授予候选源；工具同时核对工作区/global 选择、当前预算 attempt、角色工具/技能交集和固定版本。plan/read-only 或宿主拒绝该工具时不提供入口。
- 支持 create/edit/patch/write_file/remove_file/delete/get。模型只能提交候选、查询本人候选；作者与请求幂等 key 由宿主生成。根会话作者跨重启保持，角色作者绑定逻辑 invocation，父代理不能继承子代理作者身份。工具不提供批准、发布或回滚。
- 审查调用接入父任务的实际模型预算、用量与 I/O 清理。普通 provider/校验失败留下 failed 候选并让会话继续；新增预算预留/dispatch 存储故障验证，确保这些错误仍传到所属运行，不被审查器的脱敏结果吞掉。
- 后台原生 Runner 更换上下文时，曾丢失工具需要的预算 scope，导致候选工具错误拒绝。宿主现在在 Run/Resume 绑定本次已验证 attempt 的 scope，保留严格身份检查。原生后台候选及关闭重开后的审批链路已通过。
- daemon 启动与 desktop config 的 save/validate 使用同一个严格人工审查配置解码器。要求 skills 启用、存在普通 `skills/custom` 目录、明确的安全/质量模型名称；验证超时、输入/输出上限。默认只授予既有 custom 源，保留历史内部源 ID 以免使快照失效；后续的内置覆盖授权见下节。
- 当前只实现人工候选的 `review` 子集。启用状态下的 `auto_patch`、discovery/generation 等尚未实现配置直接报字段错误；禁用的旧字段保留待迁移。该子集不能代表 P4/P5 完成，Waku 原有表单尚未按能力清理。
- 配置测试通过：同 provider model ID 的不同配置名称/端点正确路由，禁用撤销候选源；save/snapshot 再保存保持密钥与审查参数，validate 不写文件，无效字段、未知模型、缺失目录或 skills 禁用均阻止保存。
- 原生 SDK 集成测试通过：根/前台/后台候选共享预算；不可见角色技能、read-only、工具拒绝；审查 HTTP 失败保持会话正常，预算不足停止；三种执行位置的 allow/deny/source-revoked 重启矩阵；原会话读取自己的候选、其他会话及父代理读取子代理候选被拒绝。没有自动发布、重复审查或不确定工具回执。

专项普通测试已通过：`TestSkillManageNative*`、`TestReviewConfig*`、`TestSkillModelValidator*` 及现有 durable-budget 回归；配置/launch/evolution 全包测试也已通过。随后本节代码的 `go test -p 4 ./... -count=1 -timeout 240s` 全包通过：SDK 197.549s、daemon 20.214s、ACP agent 44.674s、Eino 38.477s、runtime 78.958s、desktopconfig 9.201s、launch 10.578s、集成 tests 60.384s。

竞态检查的 Eino、registry、evolution、desktopconfig 专项通过（12.950s、1.746s、5.013s、1.879s）。最初 SDK 选择集含多组旧后台矩阵，达到 180 秒 package 总时限，结束时兄弟续跑 BB 用例已运行 18 秒；没有据此声称该大集合通过。随后 `go test -race . -run 'TestSkillManageNative|TestSDKForegroundAndBackgroundRolesShareConcurrency|TestSDKBackgroundParentContinuationSiblings' -count=1 -timeout 180s` 通过（70.214s），其中 BB/BFB/FBB 兄弟续跑 35.52s、并发 1/2 2.64s、候选归属 3.66s、候选审批重启矩阵 18.40s。原命令的 launch 过滤器未选中用例；launch 的验证依据是上述普通全包结果。

全包结果之后补充了仅针对 `skill_manage` 的错误返回检查：审查中遇到基础设施故障时，先保留已确认的候选结果/工具回执，再向运行返回错误，阻止下一次代理模型调用。故障注入测试在 tool_end 前后移除预算 SQL 故障，确认即使下一次请求原本已可成功，运行仍然停止；failed 候选可查询且没有不确定回执。该最后补充的普通专项已通过，`go test -race . -run '^TestSkillManageNativeCandidateScopeAndBudget$' -count=1 -timeout 90s` 的完整候选矩阵也通过（16.431s）。本节最终 `git diff --check` 和改动 Go 文件的 gofmt 检查通过；没有用之前全包结果覆盖最后的新增分支。

## 2026-10-02 P3 内置覆盖层与源变化观察

- `BuiltinSources` 与源身份分离，旧角色选择及 skill refs 保持有效。候选、发布、回滚记录 `overlay_base`；批准时将覆盖关系与发布指针同事务提交，继续复用不可变版本库，不修改发行目录。墓碑、重启、回滚及移除当前 builtin 标签均保留已保存的原始版本关系。
- 内置新名称缺少已导入来源时拒绝；模型不能在内置源中 create。`allow_builtin_overlays` 默认 false，显式 true 时要求普通 `skills/public` 并添加候选授权。候选工具仍核对当前角色、工具/技能交集、固定版本及根预算，不提供批准和发布操作。
- 已审查提案的内置来源策略变化时标为 stale，要求重新提交审查；不补写旧审查的覆盖依据。新增/移除内置授权都会改变相关执行检查点策略，旧审批不能继续提交候选。
- `Open` 及显式 `RefreshSkillImports` 检查已导入的托管源，包含不在当前安装列表中的缺失目录和删除墓碑。`ListSkillImportObservations` 按源作用域返回持久报告；记录原导入摘要、最后检查时间、变化/缺失/无效/不可读状态，以及最多 100 个文件路径和摘要差异。不保存源正文、不改变发布版本；文件问题不会使托管技能失效，存储/完整性故障仍报告错误。
- 报告是最后一次检查结果，不是实时文件监听；SDK 已接入，MANAGE 源报告操作和 Waku 展示尚未接入。后续已补显式托管源/磁盘导出后端，见下一节；完整配置和界面仍待实现。
- 覆盖层生命周期/事务故障/篡改/旧提案失效、主/前台/后台角色重启审批策略、源报告重启/墓碑/并发发布/失败回滚的定向测试已通过。新增 SDK 启动检查及带 token 的 DFACP/1 MANAGE 覆盖关系往返验证：提案详情、批准、发布历史及回滚保留原始来源。首次新增历史断言误把第 1 条 baseline 当作第 2 条覆盖发布，修正测试后通过，生产协议顺序保持原有升序。

本节定向竞态回归已通过：

```text
go test -race -p 2 ./internal/skills/... ./internal/desktopconfig ./internal/launch . -run 'TestManagedImport|TestBuiltinOverlay|TestSkillsPublished|TestSkillEvolutionSDKAndAuthenticatedManagement|TestSkillManageNative|TestSkillReviewConfig|TestReviewConfig|TestApplyConfigSkillReview' -count=1 -timeout 150s
```

registry 4.051s、evolution 9.826s、desktopconfig 7.994s、launch 7.495s、SDK 63.125s。覆盖主/前台/后台的允许、拒绝、候选源撤销、内置授权增加/移除及原权限恢复，未产生重复候选或不确定工具效果。

随后执行本节完整代码的 `go test -p 4 ./... -count=1 -timeout 240s`，所有 Go 包通过：SDK 226.377s、daemon 25.306s、ACP agent 37.257s、Eino adapter 28.377s、runtime 77.113s、registry 3.208s、evolution 2.873s、集成 tests 55.650s。格式和 `git diff --check` 通过。未执行 Rust/Waku 构建、真实远程模型/MCP 或无 Python WSL 验收；未修改现有便携包、个人配置和数据库，没有新桌面交付物。

## 2026-10-02 P3 托管源与可重建导出

- 新增 `SkillsConfig.ManagedSources` 与 `ExportRoot`，并接入 daemon 的显式 `--skills-config` JSON。源根目录/作用域继续构成持久身份；托管源只读取 SQLite，拒绝 Install/启动导入，不扫描目录，不与 builtin 属性混用。
- 人工审查配置可向显式托管源授予候选提交；关闭进化保留已声明源和已发布技能。模型工具标明新技能的托管目的地，候选自身不导出、不发布。完整 YAML 规范及 `storage_path` 的显式迁移尚待 P0/P7 接入。
- 发布、启停、删除与 export outbox 同一事务提交。SDK 拥有本地 worker，事务外构建并验证 publication.json 与 files/，使用头部版本 CAS 登记完成目录，避免旧导出覆盖新发布。目录与文件经固定 os.Root、链接拒绝、独占创建、内容校验后使用；不调用模型或脚本。
- 文件失败单独记录 pending/failed、次数及脱敏原因，不撤销已成功发布。最多自动重试三次，重启不清空失败次数；SDK `RefreshSkillExports` 请求重新检查/重试，`ListSkillExports` 查询实际完成状态。数据库/内容完整性错误继续报告，后台使用脱敏日志及退避。Close 等待 worker 退出后才释放目录/数据库依赖。
- 成功导出在重启时校验，缺失或本地改动会另建视图；原本地编辑保留，不能反向导入。删除只导出墓碑，旧视图不构成当前状态。切换 export root 不改变 source identity 或已发布版本，旧根状态不会被当作新根已同步。
- SDK 创建候选→批准→导出失败/有限重试→关闭重开→显式重试→文件缺失自动修复→删除链路已通过。Registry 覆盖原子队列故障、并发发布、目录修改、作用域、root 重绑定及非法源策略；CLI 配置与原生 Eino 托管候选均已接入测试。
- 导出旧目录和崩溃残留暂不自动清理；MANAGE 源/导出状态操作、Waku 重试/状态展示及完整配置仍待完成。本节并不代表技能自动发现、维护预算或行为评测已完成。

专项及竞态结果：Registry `TestManagedSource|TestManagedExport` 通过；相关 registry/launch 竞态分别 4.647s/7.935s。新增 SDK 测试最初把 Content 写成 string，编译器要求 *string；修正后普通 SDK 链路通过（14.368s），SDK 与原生候选矩阵竞态通过（30.309s）。

本节生产代码的 `go test -p 4 ./... -count=1 -timeout 300s` 全量通过：SDK 222.234s、daemon 14.739s、ACP agent 39.629s、Eino adapter 27.850s、runtime 88.812s、registry 8.281s、evolution 3.851s、集成 tests 54.110s。本次增加导出失败重试/重启场景，包超时使用 300s，生产执行与模型超时未放宽。最后将导出链接用例改用已有 createDirectoryLink helper：Windows 无符号链接权限时创建临时 junction，真实重解析点拒绝的定向竞态测试通过（3.858s，非 skip）。该最后改动仅为测试 fixture，没有改变生产代码。gofmt 与 `git diff --check` 通过。

下一步优先接通 Registry 技能管理分页/版本前置条件、持久启停及 MANAGE 的源观察/导出状态接口，再改 Waku 实际管理入口和共享配置 schema。自动发现/行为评测/维护预算、Python 删除与无 Python WSL/桌面交付仍按完整 P0–P7 门槛推进。当前没有新桌面包，没有替换个人配置或数据库。

## 2026-10-02 P0 设置字段审计补充

新增 [Waku 设置字段审计](eino-waku-settings-audit.md)，对照 `fields_for` 所有分组、默认模型按钮、Go snapshot/save、daemon 消费和 Rust MCP 配置链路，列出保留/移除/替换决定。明确旧主 Agent 编辑器替换为角色区、旧记忆参数与类路径移除、global/目标续跑等无效选项移除，以及 Registry 技能启停和 proposal 分页/CAS 接入。发现保存器与启动器的并发上限不一致，列入共享 schema 收敛。该文档不代表表单、能力 schema 或生效验收已完成。

## 已实现并验证

- Go 配置入口改名为 `ApplyConfig` / `ConfigSource`；保留的历史技能源 ID 仍需显式迁移，不能直接改名使检查点失效。
- 角色配置严格解析，校验字段、模型引用、边界和循环委派；保留 `null` 与空工具/技能列表的不同语义。
- 不可变角色 Registry，解析父子权限交集、限制和角色摘要。
- 缺省 `subagents` 配置的新运行也使用默认角色 Registry 和统一准入；旧 checkpoint 中没有目录时保留旧图重建入口，旧 v1 后台兼容测试显式使用旧图创建真实历史任务。
- 前台 Eino `task` 接入内置/自定义角色，独立模型路由、采样参数、轮次、工具与技能子集生效。
- 子代理继承父会话实际选择的模型，支持 SDK 注入模型及同 provider 模型目录。
- 子代理使用已固定技能快照；源文件变化不会悄悄替换运行中技能。
- 前台委派按 `max_depth` 和 `delegate_to` 装配，支持深度 1–3；仅实例化实际选中的角色，避免构建全部角色组合。
- 多层委派共用根模型/工具预算；嵌套审批可经 Eino 原生检查点暂停、恢复。
- 审批中断连并重新构建 agent 后，批准/拒绝均不会重复工具效果或回执。检查点固定完整角色目录及摘要；角色配置修改/删除后仍重建原定义，新运行使用新定义；宿主工具/权限撤销仍阻止旧检查点恢复。
- 前台角色共用根任务并发控制：模型及实际工具 I/O 持有槽位，同一角色的并行 I/O 引用计数，全部结束后释放；等待后代不持槽。并发 1 的三层委派、并发 1/2 的四项模型调用实测通过。
- 前台委派注册存入 SQLite，包含祖先/调用身份、角色快照及摘要；重建后任务计数不重置，达到上限作为工具结果返回父代理。角色时间先持久预留，正常结束按实际耗时结算；崩溃时保留最多一个 6 秒租约窗口内的未确认时间，旧尝试不能退回后继尝试的额度。持久审批挂起期间不计费。
- 分别记录模型调用、工具执行、准入排队及等待后代的时长；同类并行 I/O 按时间并集计数，不同类别可能重叠。未确认崩溃时段单独报告；当前尚未接入 Waku 展示。
- 持久 controller 仅缓存活动/排队中的根任务，空闲后释放，下一次 I/O 从 SQLite 加载；缓存释放不重置任务计数或角色策略。
- 同步 SDK 审批回调先于工具 I/O 槽位准入，等待批准不会挡住另一个角色的模型/工具调用；审批时长单独记录。同步回调仍受单次尝试的超时保护，原生持久审批通过挂起排除等待时段。
- 最后一道工具 middleware 按角色策略过滤动态注入工具，并在执行前重复检查；动态工具同时遵守审批模式。
- `background_agent` 接入同一角色 Registry/Factory，可选 `role`；后台 v3 同时固定角色及完整目录，保留 v1 通用角色和 v2 角色解码器/执行入口。v2 缺少完整目录，仍要求原版本策略匹配。
- 前后台角色共享并发和持久任务数量上限；后台任务创建与角色计数同事务提交，角色模型审批同时校验已保存策略。
- 后台独立模型角色在关闭/重新打开 SDK 后继续允许、拒绝或取消审批；会话图删除同时清理角色注册记录。
- thinking/reasoning 在配置加载与 Engine 启动期按 provider 校验。OpenAI 映射 reasoning_effort，Ark 映射 thinking/reasoning，Claude 映射 thinking 及 thinking_budget_tokens（默认 1024）。不支持的组合、Claude 温度和 token 上限冲突定位到角色字段。
- provider 请求参数在根预算扣减后转换，OpenAI/Ark 只传一个输出 token 上限；Claude 剩余预算不足时不发请求、不扩大额度。本地 HTTP fixtures 覆盖 3 类 provider 的流式与非流式实际请求。
- 前台子角色拥有独立的 I/O 错误收集，仍由父生命周期等待清理。普通模型调用/流读取失败及角色轮次耗尽返回父模型可处理的失败结果；已经完成的工具回执继续保留。未知工具效果、根预算与持久化错误不能被转换为普通结果。
- 子角色自身超时与父/根取消使用不同原因。模型超时及有明确无效果证明的工具超时在完成真实清理后返回父代理；用户取消、未知效果和持久化失败仍停止运行。
- 显式 continuation 固定原角色目录及任务数限制，重启/配置修改不更换它们；按后台 spec 版本校验宿主策略，旧 v1 通知仍可处理。重复通知返回原 execution，不产生额外模型调用。
- 无视觉能力的父模型可委派给视觉角色；角色按自身模型装配 `view_image`，只向子模型注入已提交图片，父模型接收文字结果，历史不保存 base64。
- `eino-ext/claude v0.1.25` 的流式错误变量存在竞态。以源码内最小补丁修复，并通过实际 HTTP provider fixtures 的竞态检测；保留 Apache 许可及 [补丁来源与撤销条件](../go-harness/third_party/eino-claude/PATCH.md)。v0.1.26 仍有相同竞态，且其新增字段与当前 Eino alpha.35 不兼容。
- 事件持久化失败使用失败退出路径停止并等待实际 I/O，不再生成无法提交的可恢复取消检查点。这避免了 Eino alpha.35 在该路径中序列化共享会话与追加子事件的竞争；此前可复现的失败路径及旧检查点保留测试连续 20 轮竞态验证通过。用户主动取消仍使用原生取消/恢复机制，未修改 Eino 模块源码。
- 初始生成 [Python 删除/替换清单](eino-python-removal-inventory.json) 时基线共 534 项，均标为计划动作；最新扩展清单及已执行结果见本文顶部 P6 专节。

### P2 后台任务树基础

- 后台 Binding 增加可选根会话、父任务和深度；旧根任务序列化省略新增字段，迁移只回填任务树索引，不改旧绑定字节与意图摘要。
- 存储层支持三层后台任务树，创建与父尝试租约校验、树关系、预算绑定同事务提交。该深度目前表示后台父子关系；前台/后台混合深度仍需角色 Factory 接通。
- 根会话可以查询/管理整棵树；后台 worker 权限从宿主拥有的活动尝试上下文、原生租约及子会话租约派生，只能操作自己的后代。复制 actor 字符串、连接私有子会话、借用根连接、越级读取消和代替用户审批均被拒绝。
- 级联取消先持久保存子树停止请求，再发原生取消；中途失败或重启后由扫描器重试。停止请求和新后代创建互斥，同时阻止新效果及恢复；已完成的父任务仍可取消其未完成后代。
- 真实 worker 测试覆盖活动父子任务共同取消：清理未结束时保持 running、租约及预算，全部清理完成才进入 canceled 并释放。原生取消失败、重启恢复及创建事务回滚均有 fixtures。
- 通知仍属于直接父调用，后代结果不直接注入根模型；复用子会话时也不能读取/确认旧任务的后代通知。
- 会话删除递归校验并清理所有后台后代、树索引和停止请求；深层不确定结果阻止删除，深层未处理通知阻止自动清理。三层图经过 runtime 与预算事务构建并验证。
- 任务树基础设施与管理能力已接入模型侧直接后台角色；后续混合委派与自动续跑进展见下方专节，Waku 界面仍待接通。

### P2 后台角色嵌套与持久等待

- 上一轮接入 v4 执行规范，固定角色目录、实际父任务和深度；旧版本解码与执行路径继续保留。本轮新任务进一步采用 v5，扩展内容见下节。后台角色从同一 Factory 重建，执行注册记录保存实际父角色和共享根预算。
- 直接后台角色通过宿主拥有的活动调用身份获得新建/查询/等待/取消后代的工具；每次调用重新验证 worker 授权、角色身份、租约及停止标记。前台后代仍不能借用所属后台角色的权限。
- 后台 `task_wait` 接入 Eino StatefulInterrupt，等待目标结束时保存原生检查点并释放 worker/角色执行槽；模型、工具预算和角色累计计数均沿用原根任务。根模型的持久等待在下节后续实现中接通，ACP 的等待查询仍最多 10 秒。
- 等待清单保存原生恢复目标、调用/会话/根预算身份、检查点摘要和会话/事件/回执边界。宿主在唤醒事务中复验后代终态及无阻塞状态，与原生 waiting_input → pending 版本 CAS 一起提交；claim 和 attempt open 再验已释放的清单。客户端不提供原生恢复目标，也不获得额外工具权限。
- 持久等待在任务投影中显示 `interaction.kind=task_wait` 和 `waitingTasks`，不提供人工批准操作。并行权限请求与等待共用一个原生检查点时，先恢复已批准的权限目标，尚未结束的等待再次挂起；纯等待由宿主扫描唤醒。
- 三层 `one → two → three` 真实 SDK/本地模型 fixture 在 worker=1、根并发=1、任务总数=3 时完成；等待版本中的第一、二层均经过两次原生 attempt。叶节点审批期间关闭/重新打开 SDK、移除当前角色定义后，仍可按归档目录批准并完成整棵树。
- 并行等待/写文件审批和重启后的叶节点写文件均只留下一个 completed 写回执。新增故障测试覆盖原生目标/版本篡改、检查点/事件/回执变化、唤醒事务故障回滚与重试、释放后的恢复数据校验、不确定后代不触发唤醒，以及单条坏清单不阻塞其他会话。
- 服务层验证已经选中的唤醒仍受取消标记约束；即使原生取消投递失败，也不能恢复被停止的任务，后续扫描可完成取消。Observer 区分权限根、等待根与组合包装上下文，拒绝未知根及空等待内容。
- 全量回归发现子树停止标记落盘先于原生取消投递时，预算监控可能误报租约丢失。现先确认原生/子会话租约仍属于该尝试，再把明确的持久停止归为正常取消；真正租约丢失和数据库故障仍保留诊断。定向竞态测试连续 10 轮通过。
- 以上完成直接后台角色的再次委派和保存执行中的等待恢复；混合委派及前后台私有父角色通知续跑见后续专节。MCP 重建及 Waku 交互仍未完成。

### P2 前后台混合委派

- 新任务使用 `deerflow-mixed-v5`，保留 v1–v4 执行注册。Binding/spec 增加 `ParentInvocationID`；`Depth` 统计完整角色链，`ParentTaskID` 记录最近的后台祖先。因此“前台 → 后台”首个后台任务深度是 2，“后台 → 前台 → 后台”的后台深度依次是 1 和 3。
- 每次角色执行重新生成宿主工具能力。前台角色可以按 `delegate_to` 创建后台角色；后台尝试中的前台后代也可创建后台后代。独立模型、权限交集、归档目录、共享根预算、任务总数和并发限制沿用同一 Factory/controller。
- 创建前及原生任务创建事务内均验证真实父角色的活动租约；事务内验证不会重入数据库连接池或反向获取 controller 锁。停止、过期、已结束或被替代的角色不能继续提交，创建失败不留下任务、树关系和预算成员的部分记录。
- 任务查询/等待/取消先检查当前工具所绑定的角色，再检查持久角色祖先关系；不能借用根连接或所在后台尝试去操作同级角色任务。确定的越权拒绝返回无执行效果的工具失败，父角色可继续；数据库/租约异常仍作为运行错误保留。
- 后台尝试中的前台后代使用相同持久等待/恢复机制，等待清单另外保存角色 ID 并验证其后代范围。本阶段最初对根前台角色使用有界等待；持久挂起及自动唤醒在后续专节接入。
- 本地 SDK fixtures 验证 `FBF/BFB/FFB/BBF/BFF/FBB` 六种三层混合组合，均在后台 worker=1、角色并发=1、角色总数=3 下完成；`F/B` 分别表示前台/后台。检查真实持久角色链、后台祖先、根预算、深度与叶节点无继续委派入口。
- `BFB/BBF` 在叶节点写文件审批期间关闭并重开 SDK、移除当前角色定义后，仍按归档目录恢复并完成，写文件只产生一个 completed 回执。单独验证子角色不能查询、等待或取消根会话下的同级任务，且不会产生不确定回执或令整个会话报错。
- 本阶段定向回归、全量 Go 回归及 SDK/角色/后台/runtime 相关竞态回归均已通过。根前台持久等待继续在下节实现；父角色结果自动投递与完整 Waku 操作链仍是 P2 剩余工作。

### P2 根前台执行持久等待

- 根模型和根前台运行中的角色均可通过 `task_wait` 持久挂起。原生检查点、权限目标与任务依赖在全部资源清理后同事务保存；等待状态结束当前预算 attempt 并释放执行资源。恢复保留原 run/input、根预算、角色目录和后代作用域。
- 前台 manifest v2 保存等待目标，并与检查点 envelope 内的目标逐项比对；继续校验配置、输入、原生历史、事件及回执边界。v1 权限 manifest 继续支持；旧检查点没有 `taskWaitsEnabled` 时保持原有有界等待工具契约。
- `ExecutionState.waitingTasks` 只提供任务 ID，不暴露原生中断地址或生成审批。纯等待须在所有依赖终态、无阻塞原因且无 started/uncertain 子任务回执时才能恢复；混合审批与等待可先恢复权限目标，未完成的等待再次挂起。
- 恢复准入和实际等待工具返回前重新验证任务及角色范围。提前调用 resume 不创建 attempt、不调用模型；重复 claim 由版本 CAS 拒绝；取消后原等待不能复活。SQL 故障验证检查点发布、恢复和取消整体回滚并可重试。
- 扩展真实 SDK/本地模型 fixture，覆盖六种混合组合和 `BBB`，worker=1、角色并发=1；新增 `FBF/FFB/FBB/BBB` 重启，后台叶节点待审批期间重开 SDK 并删除当前角色配置，仍按原目录恢复前台执行。并行根写文件审批与根等待在重启后各写入一次；等待自身不生成外部效果回执。
- 已通过新增前台中断目标校验、runtime 等待事务和 SDK 并行审批/重启定向测试；最新全量 Go 回归及 SDK/runtime/engine 定向竞态回归全部通过。
- 保存执行中的持久等待和显式 `ResumeExecution` 已接通，prompt 返回后的有主连接自动唤醒见下一节。ACP `/tasks/wait` 继续是有界查询。

### P2 根前台自动唤醒与连接生命周期

- 保存的等待 manifest 作为持久任务来源；当前所有者订阅只自动恢复纯任务等待。依赖就绪、会话空闲且前台资源可用后，以原 run/input、角色目录、根预算恢复，不创建新用户输入。
- SDK 增加 `ServeSessionUpdates(ctx, sessionID, handlers)`；输出、权限及结果回调属于本次订阅，原 `Run` 回调不会在返回后再被调用。自动恢复遇到新审批时使用当前回调，缺少审批处理器则保留待审批检查点。
- ACP 在 new/load/resume 后建立连接拥有的订阅，重复附着保持单订阅。模型/工具输出继续使用 `session/update`，新审批使用 `session/request_permission`；通过已声明的 `_deerflow/executions/updated` 通知结果及新等待状态。
- 自动任务不会进入用户队列，先取得空闲执行槽再预留会话；claim 仍执行原 manifest、任务依赖、预算及版本校验。每个检查点最多自动 claim 3 次，1 秒/5 秒退避；重试次数与 execution CAS、budget attempt 同事务保存，故障整体回滚，重启不清空次数。达到上限后仍可显式恢复。
- 普通 SDK Cancel / ACP session/cancel 可取消空闲中的持久等待，并持久保存根任务自动停止记录；运行中取消在 I/O 清理后提交。断连、关闭会话或停止订阅保留有效等待。关闭/删除/断连等待订阅回调结束，再释放会话资源。
- 新增 runtime 事务/取消/所有者更换/回调清理/用户队列测试；真实 Eino SDK fixtures 覆盖三层 FBF/BBB 重启后自动完成、独立新审批、无审批回调和运行中取消；ACP pipe fixture 覆盖重连、重复附着、自动输出及新审批。
- 定向测试、本轮 `go test -p 4 ./... -count=1 -timeout 240s` 全量及自动唤醒相关竞态回归均通过。新增 ACP 错误通知使用标准公开错误结构，内部 provider/存储错误脱敏为 `Internal error`，定向验证通过。
- 原 run 的持久等待恢复已接通；终态通知新 run 调度见下一节。

### P2 根代理通知自动续跑

- `subagents.auto_continue` / `max_auto_continuations` 接入当前所有者会话订阅。缺省关闭，次数缺省 10、0 禁用；新 v5 任务将有效策略固定在执行契约中，旧任务缺少策略时仍只允许显式处理，重启或当前角色配置变化不能重置原根任务计数。
- 仅从已持久化的终态通知创建新 run/input，复用已有幂等来源和原根预算。当前前台执行、等待检查点、用户排队、配置/资源约束及停止记录继续限制准入。兄弟任务共享同一根任务续跑额度，次数、来源、run/input、budget attempt 和初始事件同事务提交；新 run 间有 1 秒退避。
- 准入失败保存有限重试次数和 1 秒/5 秒退避；达到 3 次或发现需核对效果、配置/来源冲突、耗尽预算时显示阻塞。已提交来源不会因恢复或失败再创建一个 run。自动额度耗尽后仍可显式处理，继续使用原预算。
- 前台 `task_wait`、`task_status`、`task_cancel` 的终态结果通过宿主 broker 暂存接收方身份，与成功检查点/执行终态同事务保存。后续通知识别已交付结果，避免再次自动汇总；公共查询及 UI 已读不代表模型已收到结果。
- SDK/ACP 列出的通知增加 `delivery`，展示 queued/admitted/consumed/disabled/stopped/limit_reached/blocked/waiting_parent/ignored 及 run、重试和接收角色。ACP 声明 `automaticRootDelivery` 与 `deliveryState`，沿用 live permission 和 executions/updated。
- 已通过真实 Eino SDK fixtures：跨重启保存原策略与角色、UI 已读独立、缺少权限处理器保留审批、审批恢复不重复扣次数、默认/零额度/旧任务/显式停止不调用模型、兄弟任务共享额度、事务故障重试、等待结果不重复投递；ACP pipe 验证自动新 run 及交付状态，runtime 故障注入验证额度和结果交付原子性。
- 本轮 `go test -p 4 ./... -count=1 -timeout 240s` 全量通过；根代理通知、自动等待、inbox 及删除路径的定向竞态回归也已通过。
- 本节完成根代理直接创建任务的自动新 run；前后台私有父角色的后续接入见后文。MCP 重建和 Waku 展示仍待实现。

### P2 后台结果交付与通知去重

- 后台 `task_wait`、`task_status`、`task_cancel` 通过宿主 broker 校验任务身份、根预算、角色祖先、活动尝试及终态版本，再按 attempt/tool call 暂存结果交付记录。普通完成、权限/后代等待和 drain 检查点均与交付记录同事务提交；没有保存进度的失败尝试丢弃暂存。
- 前台、后台共用交付记录与 inbox 状态投影。通知先到则在结果提交时更新，后到则从持久交付记录恢复 `consumed`；通知已读、outbox ACK 和实际模型交付继续独立。祖先读取后代结果不能消耗原私有父角色的通知，已准入 continuation 的 run 关联不被覆盖。
- 后台等待现在同时核查后代 binding、根身份、任务版本与 started/uncertain 工具回执；有待核对效果的后代不能唤醒父代理。递归删除同时清理残留结果暂存。
- 已通过前后台混合七种三层组合、重启恢复、原父角色交付身份、通知早到/晚到、UI ACK 独立、事务故障回滚、失效尝试及越权观察拒绝的定向测试。新增回执不确定性用例、本轮 `go test -p 4 ./... -count=1 -timeout 240s` 全量及结果交付/等待/inbox/递归删除定向竞态回归均通过。
- 本节提供前后台结果交付去重基础；前后台私有父角色的通知重建和祖先传播见后续专节。

### P2 前台私有父角色通知续跑与祖先摘要

- 使用 Eino 公开 `AfterAgent` middleware 捕获角色自己的最终对话，通过宿主 broker 按 run/attempt 暂存。只有成功完成或检查点事务才发布不可变角色上下文版本；失败丢弃当前暂存，前台重启恢复删除未提交暂存。没有持久 controller 的独立 engine 仍支持原内存运行。
- v5 任务的父角色属于根前台运行时，显式及自动通知处理都恢复原角色身份、深度、模型、参数、工具/技能、目录与私有对话。角色目录删除后也能重建。续跑沿用原根预算和已登记角色，不重新消耗委派数量。
- 私有续跑不使用根原生 SessionStore；运行输入来自固定角色上下文，挂起后从自己的 Eino 检查点恢复。原始根 prompt 不进入私有角色，私有对话和原始孙任务结果不进入根模型历史。
- 成功续跑的上下文与 `role-result/<runId>` inbox 通知同事务提交，通知种类为 `role_result`，仅包含交给直接祖先的最终摘要。摘要固定对应对话版本及 digest；祖先逐层恢复自己的对话，再把摘要交给根代理。每一步自动续跑共享原根任务额度、退避、用户输入优先和停止规则。
- 经引擎确认并等待 I/O、资源、事件及预算清理结束的普通角色失败，经 runtime 核实没有待确认回执后，保存 `failed` / `role_failed` 和脱敏失败摘要。已完成写入不重放；混合存储/回调错误、取消和效果不确定不能转成普通结果。
- 新工具调用仍需新审批；审批期间再次重启后可恢复并仅执行一次。固定上下文被修改时，在审批回调、模型调用及工具效果前拒绝恢复。缺少父上下文保留 `waiting_parent`，损坏来源标为 `blocked`，不阻塞后续通知；真实数据库故障继续返回错误。
- 递归删除已适配 v5 前后台混合角色深度，先验证完整角色链及最近后台祖先，再清除上下文、暂存、派生结果和任务状态。修复了旧清理逻辑将前台角色层级误判为后台任务关系而报告“会话忙碌”的问题。
- ACP 增加 `automaticForegroundRoleDelivery` 和 `roleResultNotifications` 能力声明，真实 pipe 握手断言已通过。定向测试覆盖显式/自动、两级私有祖先、两次重启、新审批、上下文篡改、坏通知不阻塞后续结果、普通失败隔离、结果/通知事务回滚和实际数据删除；本轮 `go test -p 4 ./... -count=1 -timeout 240s` 全量通过，SDK 混合委派、私有角色续跑、runtime 事务/清理、engine 失败边界及 ACP 定向竞态回归也全部通过。
- 后台宿主父角色的后续接入见下文。MCP 重建及 Waku 展示仍未实现，不能宣称完整多层后台编排已交付。

### P2 后台宿主通知定位与队列基础

- 自动扫描覆盖当前授权根会话的后台后代 inbox，保留通知的私有会话及实际父角色。识别直接后台父角色，以及后台宿主内的前台父角色；不会把私有叶任务结果作为根模型输入。
- 后台父角色目标校验同时核查原任务绑定、角色祖先、固定目录、宿主与子会话配置、原生/业务终态、停止记录、工具回执、预算与会话租约，以及已提交私有对话。准入时可重复检查固定目标；正在完成的父任务保持等待，损坏来源被阻塞，真实数据库错误仍返回。
- 已修复测试 fixture 的 controller 类型不匹配。新增测试覆盖父宿主完成前后、目标版本变化、策略撤销、工具结果不确定、停止、上下文/来源损坏及数据库故障。共享后台 fixture 现在按真实事务边界提交角色上下文。
- 通知扫描先处理尚未检查的记录，再按重试时间检查旧等待项，避免一次扫描超过等待退避时间时反复停留在前 100 条。根任务退避同时写入通知工作表；到期重新校验。测试覆盖整页私有等待后仍能发现根通知、坏通知不挡住后续结果，以及重试时间到期后可再次选中。
- 本次 `TestBackgroundContinuation` 定向测试通过，`go test -p 4 ./... -count=1 -timeout 240s` 全量通过（SDK 166.660 秒，runtime 77.743 秒）；最后一次重试状态调整后的 runtime 通知/角色上下文/结果/额度定向竞态测试通过（23.878 秒）。`git diff --check` 通过。
- 本节记录后台父角色目标定位与校验的基础；原生新执行准入和恢复已经在下一节接通。

### P2 后台父角色原生续跑（2026-10-02）

- 增加 `deerflow-continuation-v6`。完成的原任务保留终态，续跑创建新的原生任务和私有会话，固定原逻辑角色、私有对话、模型/工具/技能目录及根预算，单独持有原生租约和检查点，不再次消耗委派角色数量。
- 来源、原生任务/绑定、输入、预算成员和自动续跑额度同事务准入。注入来源写入与预算写入故障会全部回滚；重复通知返回同一个原生任务。
- 背景中的父角色及背景宿主内的前台父角色均能重建。显式/自动的 BB、BFB、FBB 六种组合通过真实 Eino SDK 与本地模型 fixture 验证：两次重启、删除当前角色定义后按固定目录恢复、新工具审批、来源损坏拒绝审批、私有历史隔离、祖先摘要及实际会话图删除。
- 第二个兄弟任务通知到达时，从第一次续跑提交的私有对话重建，`continuationOf` 指向上一次物理宿主；BB、BFB、FBB 三个定向用例确认前一次摘要/工具历史和两份结果保留，逻辑角色计数保持不变。
- 子树取消沿任务树与 continuation 链传播；根自动停止也取消已准入的自动原生续跑。六种取消用例验证重启后的旧审批不可再用、来源不能再次准入、没有新增模型调用/写文件/审批授权。
- 修复根任务停止后的任务查询：已停止续跑按权限拒绝投影为不可恢复状态，不再令整个任务列表报内部错误，原生取消继续完成清理。停止状态与来源损坏在调度中分别记录。
- 增加存储身份检查：来源摘要、固定角色及前一宿主绑定必须与 continuation 链一致。缺失/错误链、损坏来源及前一绑定被拒绝；清理使用独立于当前执行许可的身份校验，允许合法停止的任务进入清理流程。
- SDK/ACP 通知处理结果可返回 `backgroundTask`；原生续跑通过任务 get/wait/approve 查询与审批。能力声明增加 `automaticBackgroundRoleDelivery`，ACP 与 background 文档已更新。
- 最初的删除用例只等待模型开始输出摘要就停止订阅，会中断尚未完成的宿主提交。现等待执行完成回调，并等待原生 outbox/清理完成后删除，不放宽生产删除保护。
- 上述定向验证已通过。相关竞态回归通过，SDK 包 161.209 秒、runtime 包 42.904 秒；该次选择器在 background/ACP agent 包没有匹配用例，不计入这些包的新增竞态验证。之后 `go test -p 4 ./... -count=1 -timeout 240s` 全量通过，SDK 包 167.091 秒、runtime 包 70.826 秒，其他 Go 包也通过。MCP 重建、Waku 展示及 P3–P7 均仍有剩余工作。

本次关键命令均在 `go-harness/` 执行，使用隔离临时数据库与本地模型 fixtures：

```powershell
go test . -run TestSDKBackgroundParentContinuation -v -count=1 -timeout 150s
go test ./internal/runtime -run 'TestBackgroundContinuation|TestDelete.*Background|TestDeleteNested' -count=1 -timeout 90s
go test -race -p 2 . ./internal/runtime ./internal/background ./internal/acp/agent -run 'TestSDKBackgroundParentContinuation|TestBackgroundContinuation|TestDeleteNested|TestDeleteTerminal|TestAutomaticContinuation|TestACPAutomatic' -count=1 -timeout 240s
go test -p 4 ./... -count=1 -timeout 240s
```

## 当前验证依据

本地模型/工具 fixtures，不访问用户模型、凭据、工作数据库。

```text
go test ./internal/agents ./internal/launch -count=1
go test ./internal/launch ./cmd/deerflow-acpd-go -run '^(TestApply|TestNative)' -count=1
go test ./internal/engine/eino ./internal/skills -run 'TestConfiguredRole|TestRoleOptions|TestSnapshotSubset' -count=1
go test ./internal/engine/eino -run 'TestConfiguredRole|TestRuntimeNativeNested|TestRoleOptions' -count=1
go test ./internal/engine/eino -run TestConfiguredRoleNested -count=1
go test ./internal/engine/eino -run 'TestConfiguredRoleTaskLimit|TestConfiguredRolesLimit' -count=1
go test ./internal/engine/eino ./internal/agents ./internal/launch ./internal/skills -count=1
go test . -run 'TestSDKForegroundAndBackgroundRolesShareConcurrency|TestSDKNativeBackgroundChildApprovalAcrossRestart' -v -count=1 -timeout 75s
go test ./internal/agents ./internal/engine/eino ./internal/runtime . -run 'TestController|TestConfiguredRole|TestBackground|TestSDK.*Background|Test.*Delete' -count=1 -timeout 120s
go test ./internal/engine/eino ./internal/launch -run 'TestRoleProvider|TestRoleThinking|TestConfiguredRoleProviderValidation|TestApplyConfigSubagent' -count=1 -timeout 120s
go test ./internal/engine/eino -run 'TestConfiguredRoleFailure|TestConfiguredRoleUncertain|TestConfiguredRoleUsage' -count=1 -v -timeout 90s
go test ./internal/engine/eino -run TestConfiguredRoleTimeout -v -count=1 -timeout 60s
go test ./internal/agents ./internal/engine/eino ./internal/runtime . -run 'TestController|TestConfiguredRole|TestToolFactoryCleanup|TestSDK.*Background|Test.*Delete' -count=1 -timeout 180s
go test -race ./internal/agents ./internal/engine/eino -run 'TestController|TestConfiguredRole|TestRoleProvider|TestRoleThinking|TestCatalog' -count=1 -timeout 180s
go test -p 4 ./... -count=1 -timeout 180s
go test -race ./internal/engine/eino -run '^TestConfiguredRoleUsagePersistenceErrorCannotBecomeFailureResult$|TestTurnLoopResumeFailuresPreserveOriginalBytes' -count=20 -timeout 180s
go test -race ./internal/agents ./internal/engine/eino -run 'TestController|TestConfiguredRole|TestRoleProvider|TestRoleThinking|TestCatalog|TestDefaultRole|TestTurnLoop' -count=1 -timeout 180s
go test ./internal/background ./internal/runtime -run 'TestBackgroundTree|TestDeleteNested' -count=1 -timeout 90s
go test ./internal/background -run TestBackgroundTreeLive -v -count=1 -timeout 45s
go test -race ./internal/background ./internal/runtime -run 'TestBackgroundTree|TestDeleteNested|TestDeleteTerminal|TestNativeHeartbeat' -count=1 -timeout 180s
go test . ./internal/engine/eino ./internal/runtime ./internal/background -run 'TestSDKNested|TestPreparedObserver|TestBackgroundTree|TestDeleteNested|TestBackground.*Permission' -count=1 -timeout 120s
go test ./internal/runtime -run TestBackgroundWait -count=1 -timeout 60s
go test ./internal/background ./internal/engine/eino -run 'TestBackgroundWait|TestPreparedObserverWait' -count=1 -timeout 60s
go test -race -p 2 . ./internal/background ./internal/runtime ./internal/engine/eino -run 'TestSDKNested|TestBackgroundWait|TestPreparedObserver|TestBackgroundTree|TestConfiguredRoleNested' -count=1 -timeout 180s
go test -race ./internal/background -run 'TestCancellationPreserves|TestBudgetMonitorDistinguishes' -count=10 -timeout 180s
go test . -run 'TestSDKMixedRole.*' -v -count=1 -timeout 120s
go test . -run TestSDKRoleCannot -v -count=1 -timeout 90s
go test ./internal/agents ./internal/background -run 'TestRoleAttemptTransaction|TestMixedRolePlacement' -count=1 -timeout 60s
go test -race -p 2 . ./internal/agents ./internal/background ./internal/runtime -run 'TestSDKMixedRole|TestSDKRoleCannot|TestSDKNested|TestRoleAttemptTransaction|TestMixedRolePlacement|TestBackgroundWait|TestBackgroundTree' -count=1 -timeout 180s
go test ./internal/runtime ./internal/engine/eino . -run 'TestExecutionTaskWait|TestForeground|TestSDKMixedRole/BBB_parallel' -count=1 -timeout 120s
go test -race -p 2 . ./internal/runtime ./internal/engine/eino -run 'TestSDKMixedRole|TestExecutionTaskWait|TestForeground|TestNativePermission|TestDurableRuntime' -count=1 -timeout 180s
go test ./internal/runtime -run TestAutomatic -count=1 -timeout 45s
go test . -run 'TestSDKAutomatic|TestACPAutomatic' -count=1 -timeout 90s
go test -p 2 ./internal/runtime . -run 'TestAutomatic|TestSDKMixedRoleAutomatic' -count=1 -timeout 90s
go test -p 4 ./... -count=1 -timeout 240s
go test -race -p 2 . ./internal/runtime ./internal/acp/agent ./internal/session -run 'TestAutomatic|TestSDKAutomatic|TestACPAutomatic|TestSDKMixedRoleAutomatic|TestACPExecution|TestDurableRuntimeDisconnect|TestRunQueue|TestExecutionTaskWait' -count=1 -timeout 180s
go test ./internal/acp/agent . -run 'TestAutomaticExecutionNotification|TestACPAutomatic' -count=1 -timeout 90s
go test -p 4 ./internal/runtime ./internal/background . -run 'TestAutomatic|TestSDKAutomatic|TestSDKMixedRoleAutomatic|TestContinuation|TestSDKNotification' -count=1 -timeout 120s
go test -p 2 ./internal/runtime . -run 'TestAutomaticContinuation|TestAutomaticTaskDelivery|TestSDKAutomaticNotification|TestSDKAutomaticWait' -count=1 -timeout 100s
go test -p 2 . ./internal/runtime ./internal/background -run 'TestSDKAutomatic|TestACPAutomatic|TestAutomatic|TestInbox|TestBackgroundTreeInbox|TestDelete.*Session|TestDeleteNested|TestDeleteTerminal' -count=1 -timeout 120s
go test . -run TestSDKAutomaticNotificationSiblings -count=1 -timeout 50s
go test -race -p 2 . ./internal/runtime ./internal/background ./internal/acp/agent -run 'TestSDKAutomatic|TestACPAutomatic|TestAutomatic|TestSDKMixedRoleAutomatic|TestInbox|TestBackgroundTreeInbox|TestDeleteNested|TestDeleteTerminal' -count=1 -timeout 180s
go test -p 2 ./internal/background ./internal/runtime . -run 'TestInbox|TestBackgroundTaskDelivery|TestAutomaticTaskDelivery|TestSDKAutomaticWait|TestSDKMixedRole' -count=1 -timeout 150s
go test ./internal/runtime -run 'TestBackgroundTaskDelivery|TestBackgroundWait' -count=1 -timeout 60s
go test -race -p 2 . ./internal/runtime ./internal/background -run 'TestSDKMixedRole|TestSDKAutomaticWait|TestBackgroundTaskDelivery|TestBackgroundWait|TestAutomaticTaskDelivery|TestInbox|TestDeleteNested|TestDeleteTerminal' -count=1 -timeout 180s
go test -p 2 ./internal/engine/eino ./internal/runtime -run 'TestExecutionSource|TestNotification|TestAutomaticContinuation|TestConfiguredRole|TestForeground' -count=1 -timeout 120s
go test -p 2 . ./internal/runtime -run 'TestSDKPrivateParentContinuation|TestRoleContext|TestRoleResult|TestAutomaticContinuation' -count=1 -timeout 150s
go test -p 4 ./... -count=1 -timeout 240s
go test -race -p 2 . ./internal/runtime ./internal/engine/eino ./internal/background ./internal/acp/agent -run 'TestSDKPrivateParentContinuation|TestRoleContext|TestRoleResult|TestAutomatic|TestACPAutomatic|TestSDKMixedRole|TestDeleteNested|TestDeleteTerminal|TestConfiguredRoleFailure|TestConfiguredRoleUncertain|TestConfiguredRoleUsage|TestConfiguredRoleTimeout' -count=1 -timeout 240s
go test ./internal/runtime -run TestBackgroundContinuation -v -count=1 -timeout 90s
go test -p 4 ./... -count=1 -timeout 240s
go test -race -p 2 ./internal/runtime -run 'TestBackgroundContinuation|TestRoleContext|TestRoleResult|TestAutomaticContinuation|TestAutomaticTaskDelivery' -count=1 -timeout 150s
```

专门检查了不同 provider 的同名模型、子模型真实请求参数、父私有历史隔离、共享预算耗尽、角色目录重建，以及 1/2/3 层原生委派和叶节点审批。新增 v4/持久等待路径的定向及竞态回归已通过；本次全量 Go 回归暴露的取消时序问题已修复并完成 10 轮定向竞态回归。修复后的 `go test -p 4 ./... -count=1 -timeout 180s` 全量通过，包括 SDK、daemon、ACP、后台、预算、角色、引擎、设置、runtime、技能、sandbox 和集成包。此前多包并行时 PowerShell 冷启动超过 fixture 的 15 秒等待上限，现仅将该成功/编码用例的等待上限改为 60 秒，生产命令超时和独立的进程取消测试均未改动。瞬时模型 fixture 可能低于 Windows 时钟精度，计时测试允许零时长并检查分项记录。Rust 回归与打包尚未开始。

## 剩余工作与限制

| 阶段 | 当前完成范围与剩余验收 |
| --- | --- |
| P0 | 字段消费/移除清单、共享 schema、原生保存校验与配置迁移已接通；daemon 实际值投影已包含启用的 P4/P5 worker 限额、角色稀疏覆盖及非敏感工具参数，Waku 有界缓存展示来源与生效值，敏感文本只显示摘要。剩余用户手测保存→应用→实际值、版本不匹配及迁移交互 |
| P1 | 角色/准入/预算/恢复核心实现、全量 Go 回归及竞态回归已通过；真实模型和桌面验收归入 P7 |
| P2 | 混合委派、持久等待、续跑、任务树与精确取消、MCP 连接租约/显式重授权、独立宿主 recipe 重启重建、schema 准入检查、角色 invocation 用量及产物分页/分块保存已接入，前台角色产物按持久记录归属。宿主仅恢复显式授权的精确工作区资源；旧未知用量/产物不推测归因。剩余用户手测任务审批恢复、前后台产物下载及真实外部 MCP；活跃 prompt 下载 busy 属现有生命周期约束 |
| P3 | 候选/提案、审查发布/回滚、SDK/MANAGE、内容检查、`skill_manage`、内置覆盖、来源/导出与 Waku 原生操作及能力展示已接通；旧 Agent 草稿转换、旧进化历史只读归档与分页查看已实现。剩余用户手测导入预览/保存、审查发布/回滚及下一轮使用新版本；旧历史不自动激活为当前作业 |
| P4 | 实际读取、反馈/缺失报告、三类自动发现、可信质量判断、生成/冷却去重/维护预算和 Go 行为评测已接入；关键新增链路通过。保留真实模型效果及桌面手测，未分类环境失败保持 unknown，不从模型自述制造高置信因果 |
| P5 | 受管理已有技能的小补丁自动发布、持久观察、当前证据重算和连续确认失败 CAS 回滚已实现；关键集成通过，Waku 设置与状态列表已接。保留打包后的实际桌面与观察期手测 |
| P6 | WSL Go helper、Bridge/根入口、旧核心/依赖/测试/专用界面删除、Go 构建/技能工具、原生默认配置和文档已落地；清单 584 项均记录处理结果，6 项仍只有语法验证。正式发行保留这 6 项执行验收、适用原生 Linux/WSL 与改写 CI 验收、最终归档引用核查 |
| P7 | 本轮开发手测包从冻结工作树按 Release 配置重新构建并记录来源、清单和日志。用户手测覆盖启动无终端、模型选择、设置生效、迁移与上述功能链；正式发行另需固定干净提交的配套二进制/源码归档及未安装 Python/uv/pip 的干净系统验收 |

本轮功能源码已冻结，Go 全模块构建通过；各功能验证以上方 2026-10-02 专节的实际记录为准，早期全量/竞态记录不代表所有后续增量都重复运行。用户手测重点为真实 Waku 设置与任务/技能交互、实际模型和外部 MCP，以及自动补丁观察期表现。具体远程模型是否接受角色参数仍以实际响应为准；历史未知归因和未分类环境失败继续保留未知。

现有运行中的便携包、个人配置和数据库均未替换。当前改动未提交；本轮手测包按 `development-dirty` 如实记录来源，构建结果与最终路径由顶部交付段更新。正式发行的 clean commit、配套源码/二进制归档、无 Python 干净系统及适用 Linux/WSL/远端 CI 验收仍待完成，不作为开发手测包已通过的结论。
