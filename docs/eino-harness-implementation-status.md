# Eino Harness / ACP 实施状态

本文件跟踪完整 V1，不能以当前已实现子集代替原方案的验收范围。

## 基线

- 分支 `codex/eino-harness-acp-refactor`；Python/Bridge 参照 `92b56924989a91e0eae05be6e95a6304f1ee4cd4`。
- 代码位于独立 `go-harness/` 模块，未切换现有 Python 或桌面默认入口。
- 工具链 `go1.26.8`；因 SQLite `modernc.org/sqlite v1.60.0` 要求 Go 1.26，模块最低版本相应提高。
- Eino `v0.10.0-alpha.35`；openai `v0.1.13`、claude `v0.1.25`、ark `v0.1.71`；ACP 类型 `coder/acp-go-sdk v0.13.5`。
- MCP 使用 `officialmcp v0.1.1` + 官方 MCP Go SDK `v1.6.1`，已接入执行入口并通过真实 stdio/HTTP/SSE fixture。

## 工作项

| 原方案范围 | 当前进度 | 剩余验收 |
| --- | --- | --- |
| Go SDK / 版本组合 | 模块、公共类型、嵌入式 Client 和模型适配器已写入 | 全量组合、跨平台构建、配置清单 |
| SQLite 基础和 Eino providers | checkpoint、session events、background task stores；上游 conformance、崩溃恢复、SQL 故障注入测试已通过 | 后续 Manager/工具恢复装配 |
| 会话协调 / stdio | 双向传输、同步准入、占用、取消、new/list/load/resume/close/delete、自动 retention 及终态后台任务图清理已写入；官方 ACP stable v1 TCK 判定 `CONFORMANT` | 真实编辑器互操作、清理策略实机验收 |
| 执行与工作区工具 | Eino TurnLoop/DeepAgent、前台委派、文件工具、plan/read_only、主/子共享预算；执行回执与命令工具已接线 | 长期会话循环、后台委派和更完整的工具集 |
| 权限 / 恢复 | 前台与后台原生 durable HITL、审批/检查点/回执/预算联合恢复；SDK/ACP 查询、批准与取消已接入 | 真实编辑器与 MCP 重新绑定恢复 |
| 领域事件 / 历史 | 持久事件、文本/工具/产物 updates、原生子 Agent 生命周期、主 Agent 计划与真实上下文用量投影、分页历史与 load 重放、有界异步 ACP 更新队列 | 真实编辑器流压验证 |
| MCP | ACP client 配置、stdio/出站 HTTP/SSE、官方工具适配、会话代际替换、凭据隔离已接入 | 真实编辑器互操作、与后台任务生命周期组合 |
| 会话配置 | 模型白名单、subagent、ask/allow_always/reject_always/read_only、版本与审批缓存撤销已持久化 | thinking/profile 仅在真实能力落地后开放 |
| 图片 / 附件 / 产物 | 不可变资产快照、引用持久化、模型前临时加载、SDK/ACP 图片输入、view_image、产物登记及 MCP 工具图片导入已验证 | 流式工具与模型生成媒体导入、可选对象存储发布 |
| 后台子任务 / 长命令 | 原生 Manager、有界 worker、隔离 child、真实 Eino/model/tool factory、后台审批 broker、SDK/ACP 与关机暂停/显式恢复已接线 | 父会话通知输入；跨 run 长命令 |
| Skills / memory / 压缩 | Skills 不可变注册表与逐步加载；记忆 scoped facts/revision、FTS5、SDK/ACP 管理、Eino 固定快照注入、只读检索工具、显式启用的受控提取/终态提升与摘要压缩、Flush 屏障及旧 JSON 显式迁移已接入 | 真实模型策略校准 |
| Docker / Windows shell | 默认禁用；可选 local/PowerShell/WSL2/Docker 命令后端；进程树、输出、环境与资源限制已接入 | Docker 真实运行验收，跨 run 后台命令生命周期 |
| daemon / Bridge / draft v2 | Go daemon 的 DFACP/1、认证 endpoint、STATUS/STOP、MANAGE 状态/排空/恢复/会话清单/会话删除/记忆读取与删除、Python `--config` 有界兼容层、自动 retention、多窗口及 Rust Bridge 二进制互操作已验证 | draft v2 对照、完整配置映射 |
| 可选外部 ACP Agent | 首轮接入：显式白名单、独立 workspace、stdio new/load/prompt、进度、连接存续时的反向权限、取消和进程树回收；外层工具与一次估算模型调用计入父预算 | 断线后的反向权限暂停/恢复、远端真实模型用量约束、产物导入与异常会话对账 |
| 打包 / 默认切换 | 未开始 | Windows/Linux 实机、回退、切换演练 |

## 已确认的实施差异

1. Go ACP SDK 的 v0.13.5 已有 close/resume/config 类型，不能笼统描述为缺少这些接口。但其默认 transport 的 10 MiB 上限和 prompt 自动抢占不满足本项目，故复用类型并实现独立有界 JSON-RPC transport。
2. ACP 版本协商按规范返回所支持的版本 1；客户端请求更高版本时由客户端决定断开。非正版本作为参数错误。
3. 内部 SQLite 与业务 SQLite 表共用数据库但事务仍按真实调用边界分离，不宣称跨 Eino 回调的原子事务。
4. normal prompt 与显式 checkpoint Resume 分开。新 prompt 不自动重放曾被取消的工具。

## 验证记录

2026-09-29 首次组合编译通过，已修复并验证：

- 原生 Eino 流取消后尚未等待 provider 清理完成。
- Eino 默认 JSON serializer 字段顺序不稳定，造成同事件重试冲突。

另经审查补充：运行期间固定目录句柄、拒绝 cwd 被链接重定向、阻止关闭后的晚到会话、限量搜索读取、超时 detach 的最终资源释放、一次性准入令牌，以及取消不吞掉清理失败。

真实可执行文件测试已通过：本地模型 SSE fixture 驱动 Eino 工具调用，ACP 双向审批后实际写文件，进程退出并重新启动后验证 ACP 历史重放和 Eino 原生对话重建。测试不调用真实付费模型。

当前 Windows 验证通过：

```text
go mod verify
go test -count=1 -p=2 -timeout=5m ./...
go test -race -p=2 -timeout=5m ./internal/... ./
```

最后两处修改后，另外重跑了 session/tools/runtime/ACP agent/SDK 的 race 检查，均通过。真实进程测试已关闭测试结果缓存重跑。设置 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0` 后的 `go build -p=2 ./...` 交叉编译通过。

新增 GitHub Actions Windows/Linux 验证定义尚未推送或在远端执行。真实模型联网测试、Linux 运行期验证和编辑器实测仍待后续阶段；官方 ACP TCK 结果见第二十三阶段。本阶段尚不满足完整 V1 发布条件。

## 下一阶段

历史查询现提供 SDK `HistoryPage` 与 ACP `_deerflow/history/list`，游标绑定会话并固定首次查询的事件上限。`session/load` 持有会话租约分页读取并完整重放，`session/resume` 仍不重放。验证覆盖分页间新增事件、全量重放、跨会话游标、其他连接访问、繁忙会话与大事件边界，runtime 与真实 ACP 管道限定测试通过 race。

继续实现父会话通知输入、Memory、压缩及共享预算。前台公开 durable HITL 和后台任务 host 已接线。以下各阶段记录保留当时的实现状态，最新进度以本表及最后一节为准。

## 第三阶段装配

- 工具回执与领域事件同事务。`tool_execute` 是唯一执行边界，提交失败不能触发工具；重复 call ID 不重复执行。流式工具的晚到错误及部分输出保留。
- `pending/started/completed/not_executed/uncertain/no_effect` 分别保存。崩溃后不盲目重放；不确定结果需要 owner 在会话空闲时显式对账。审阅带版本检查与独立审计，不覆盖原始证据。
- ACP 声明 `_deerflow/tool_receipts/list` 和 `_deerflow/tool_receipts/reconcile`。业务错误提供稳定提示与恢复方法，内部 joined error 不直接透传。
- Eino 原生 TurnLoop 已替代直接 Runner 入口。目前每次调用处理一个已持久化的前台输入，尚不是长期会话或后台通知调度器。
- 检查点写入/删除延迟到完整执行、事件落库、I/O 和资源清理之后；拒绝恢复、内层编码损坏或模型/清理失败保留旧检查点。保存预算计数、活动执行时长及 extension state。暂停期间不计执行超时。
- 检查点格式为 `harness/turn/v1/<runID>` 对应的版本化 JSON envelope，内部保存 Eino 原生字节。早期 `harness/run/...` raw Runner 及未包装 TurnLoop 开发格式不迁移、不自动重放。
- Skills 固定不可变版本，由一个 per-run factory 同时装配工具和原生 middleware。只暴露 metadata，正文和引用文件按需加载。SDK 安装/启停/删除/查询已接线；global 必须显式开启。fork/model override 暂不支持。
- MCP 检查点绑定不含凭据的连接代际，重连后旧执行拒绝恢复；这不保证同一远端连接的实现永不变化。命令配置以摘要绑定。当前公开 ACP resume 只做会话重新附着，尚不开放执行检查点 Resume。
- 命令工具前台执行并持久化有界结果。明确结束后释放进程资源；失败、取消或未确认终止保留执行证据。Eino alpha 丢弃 result+error 中的 result，适配器通过错误上的结构化结果接口保留命令证据。
- Windows Job、PowerShell 与本机 WSL2 `Ubuntu-22.04` 的执行/取消已实际验证；Linux 进程组测试在该 WSL 中实际运行。Docker daemon 未运行，目前只有策略与参数测试，不宣称容器实测通过。

公开 durable resume 前还需完成业务 run/event cursor 绑定、工具回执与原生检查点的联合准入，以及断连后的审批重新附着。独立预算账本已在第五阶段落地。保留旧检查点字节并不意味着可以无条件重放副作用。完整 V1 仍未达到发布条件。

本批装配后的验证通过：

```text
GOMAXPROCS=2 go test -mod=readonly -p=2 -count=1 -timeout=5m ./...
GOMAXPROCS=2 go test -mod=readonly -race -p=2 -count=1 -timeout=3m ./ ./internal/runtime ./internal/tools ./internal/launch
```

Engine 的最终 checkpoint 回归、ACP agent/protocol、Skills 和 Sandbox 已分别通过限定包 race；MCP 新增连接代际恢复回归通过 race。整模块测试包含真实可执行文件、MCP、命令进程和本地模型 fixture，未调用付费模型。测试发现过的命令错误丢失结构化输出问题已修复，最终 SDK race 验证通过。

本批 `go mod verify` 通过；`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -mod=readonly -p=2 ./...` 全模块交叉编译通过。

## 第四阶段媒体与 Skills 启动配置

- `--skills-config` 在两个 CLI 中复用同一 JSON profile。仅处理显式来源与安装条目；不扫描 HOME。每个安装单独提交，后续条目失败不回滚此前安装，重试相同内容复用已有版本。
- 媒体资产位于独立数据目录内的只读快照，归属会话。输入图片限 8 张、每张 20 MiB、每轮合计 40 MiB；本地附件限 25 MiB。源文件改变后，已接受输入保留原快照。
- 图片能力按模型显式白名单，默认关闭；ACP 初始化按可选模型交集声明能力，每轮按当前模型校验。`--vision-model` 不会自动把模型加入可选列表。
- 输入资产、accepted input、user_message 同事务关联。原生 session/checkpoint 只保留 AssetRef 和稳定 URI，模型调用复制消息后临时加载图片；不会原地修改历史对象。历史恢复和前台子 Agent 走同一路径。
- `view_image` 只读查看 workspace 图片。`present_files` 只接受 `.deerflow/outputs` 内文件并保存不可变快照，产物索引、tool_end 和 receipt 同事务提交；读取图片不会登记为用户产物。
- SDK 支持 `ListArtifacts`/`ResolveAsset`，ACP 提供标准 resource_link 与 `_deerflow/artifacts/list`。HTTP(S) 普通链接只作引用，远程图片不自动下载。
- 图片预算使用每张 4,096 tokens 的明确估值，provider usage 到达后结算。每次模型调用最多加载 32 个不同图片资产、总计 40 MiB；这项限制覆盖完整待发送历史。
- 原始 MCP 图片输出、模型生成多媒体暂时拒绝，等待工具/模型输出导入器；不能把现有图片输入支持描述成全部多媒体闭环。

本批验证通过真实 SDK+模型 fixture 的图片输入、重启后历史加载、引用持久化与无 Base64 入库，以及 present_files 实际工具调用、源文件修改后保持快照、重启查回而不重放。view_image 在 read_only 下通过真实工具调用验证，不请求写入审批、不登记产物。真实 ACP 可执行文件测试覆盖图片到模型、标准文件链接返回及重启后的图片与产物历史恢复。

assets/runtime/ACP、engine 以及根 SDK/launch/tools 分别通过限定包 race；资产事务回滚、会话隔离、故障清理、Close 并发、SDK 对账拒绝伪造资产等回归通过。真实管道 2 × 20 MiB 图片输入与 load 通过普通测试；大型管道用例在 race 构建下跳过，小图片相同路径参加 race。两个 CLI 的 Linux CGO-disabled 交叉编译通过。以上使用本地模型 fixture，不代表真实付费模型或编辑器互操作验收。

## 第二阶段已落地

- SDK 与两个 CLI 共用预算配置。默认一轮及其子 Agent 共 100 次逻辑模型调用、200 次工具调用、200,000 tokens、单次输出最多 4,096 tokens、30 分钟。显式零值可关闭对应预算。
- Token 限额使用调用前预估与并发预留，收到 provider usage 后结算；这是估计约束，不能保证在进行中的模型调用绝不超过实际计费额度。隐藏在 provider SDK 内的重试不计为新的逻辑调用。无 provider usage 时明确标记 `Estimated`。
- ACP 预算耗尽使用标准 stop reason 和文本 update，额外 `limit` 与汇总 usage 放在 `_meta.deerflow` 中。超时仍等待已启动的 provider/tool 清理，独立清理错误保留。
- `session/set_config_option` 的设置确实进入下一轮引擎。配置/模式切换递增版本并撤销缓存授权；运行期间拒绝冲突变更。权限意图记录配置版本。
- MCP 启动命令需显式允许列表；会话配置不持久化凭据。替换先连接和发现新工具，成功后原子发布，失败保持旧连接。read_only/plan 不暴露 MCP 工具。
- 真实可执行文件测试覆盖 ACP 下发 stdio MCP 配置、Eino 模型发现工具、先审批后副作用、切换模型、read_only、关闭与重新加载，以及凭据没有进入 ACP、模型请求或数据库。
- daemon 使用独立目录及认证 loopback IPC，无 HTTP 服务。已实测现有 Rust Bridge 的 status/manage/ACP proxy/stop；MANAGE 业务操作当前仍明确返回 unsupported，Python `--config` 尚未迁移。Windows endpoint 与锁文件使用受保护 DACL。

以上新增模块分别通过常规测试与 race 检查；最终集成验证以实际执行记录为准。Linux 交叉编译不等于 Linux 运行验收，真实编辑器、真实付费模型和 ACP TCK 仍待进行。

## 第五阶段持久预算

- SDK 默认 Eino 路径启用独立 SQL 预算账本；接受输入、建立根预算和首次 attempt 同事务，运行终态与 attempt 结束同事务。同步子 Agent 共用同一个作用域，后续后台任务通过原始 run 绑定既有预算。
- 模型与工具调用先预留、持久标记 dispatch 后才执行。真实 usage 结算，失败且没有最终 usage 时保守收取预留。失败重试保留旧检查点也不会退回已记账消耗；重启留下的未知执行保留额度并阻止自动续跑。
- Heartbeat 覆盖构建、执行和清理；额度耗尽后仍为清理续租。并行执行按活动时间区间并集计时。SQL 结算/收尾错误具有不可拆的 persistence 标记，不会被取消和预算终止过滤吞掉。
- 生产检查点 envelope 升为 v2，保存 root/policy/revision 身份，以账本为额度权威。无账本的内部测试兼容 v1；旧本地计数检查点不能用于初始化生产预算。checkpoint key 前缀仍为 `harness/turn/v1/`，它不表示 envelope 版本。
- runtime 测试覆盖输入/终态事务失败、heartbeat SQL 故障、额度耗尽后清理超过原租期、重启 unknown；engine 测试覆盖失败恢复继续扣费及结算错误不被额度终止吞掉。
- 真实 SDK + 本地 provider fixture 已验证实际 write_file、下一次模型调用被预算挡住、80 tokens 实际 usage 结算、重启保留账本以及新 prompt 建立新组。SDK/runtime/ACP 组合 race 和 engine 全包 race 通过。

后台基础层另已验证 native subagent interrupt → 重建服务 → broker resume、checkpoint/task/outbox 事务故障回滚、取消与清理 join、跨重启 child 隔离及通知去重。此时尚未向模型开放后台委派，也未开放公共执行恢复；不能据这些基础测试宣称完整 V1 已完成。

第二阶段集成检查中发现并修正：取消与独立故障组成 joined error 时不能整条丢弃；provider 在取消之后排空流时上报的故障仍须返回；MCP Close 必须等本地连接和直接子进程真正清理完成后才允许 SDK 释放数据库锁。对应回归已增加。一次全量测试因 HTTP 测试服务在 middleware 内等待不可取消 context 而超时；该代际替换取消用例已改为可回收的真实 stdio 进程，限定包 race 复测通过。出站 HTTP 取消仍不代表远端副作用已经停止。


## 第六阶段前台 durable HITL

- 真实 Eino StatefulInterrupt 先暂停受保护工具，保存审批意图；资源全部 join 后，同事务提交 checkpoint manifest、waiting 状态与预算 attempt 结束。在线审批继续原 prompt；取消审批对话或审批阶段断连保留等待状态。
- 恢复沿用原 run/input/tool call IDs，创建新 attempt/fence；必须重新审批全部 pending targets。一次性 grant 的消费与 tool_execute 或拒绝回执同事务，恢复不重新计入逻辑工具预算。
- manifest 绑定原输入摘要、配置、checkpoint、原生历史位置、领域事件游标、工具回执和中断地址。审批参数原始字节保留，包括空格与换行；变更/缺失 target 在执行前拒绝。
- SDK 提供 Execution（空 runID 查询最近执行）、ResumeExecution、CancelExecution；ACP 条件声明 _deerflow/executions/get、/resume、/cancel，resume 与 prompt 都在协议 reader 中同步准入。标准 session/resume 继续只做附着。
- 等待执行阻止新 prompt 与配置切换。ACP 断连与显式 session/cancel 分开；SDK context 取消视为显式取消。不确定副作用仍须显式对账。
- Eino 内建 task 是编排节点，记录 subagent_start/suspended/resumed/end。子模型与实际工具继续共用预算，只有实际外部工具生成 effect receipt；启用原生 subagent 时禁止自定义工具占用 task 名称。
- 后台 child business sessions 已从普通 list/load/admit/run 入口隔离，准备接入独立 native task host；此项隔离不代表后台 host 已对外开放。

验证使用真实 Eino、本地模型 SSE fixture、工作区 write_file、SQLite close/open 和双向 ACP pipes。SDK 覆盖暂停后重启的批准/拒绝/取消、原输入与参数保持、最大工具次数为 1 的续跑，以及重复恢复拒绝。ACP 覆盖审批断连后重新附着、fresh permission、latest discovery、owner/CAS/严格参数校验与繁忙准入。运行时覆盖 dispatch/receipt/grant 事务故障回滚和通知失败保留等待；引擎覆盖同级并行与嵌套子 Agent 中断恢复。

限定包完整 race 检查已通过：SDK 47.736s、ACP agent 33.588s、runtime 46.3s、session 1.35s、engine 33.312s、budget 9.872s。这些记录对应当前阶段，尚未执行本阶段的整模块回归、Linux 交叉构建或真实编辑器/TCK 验收。完整 V1 仍在实施中。

## 第七阶段后台任务 SDK / ACP 闭环

- 默认 Eino SDK 启动持久后台 host 和有界 worker。两个 CLI 共用 `--background-workers`，默认 4、最大 64。每个 attempt 从保存的 child input、配置与扩展版本重建模型和工具，不复用父 run 的闭包。
- 模型可用 `background_agent`、`task_status`、`task_wait`、`task_cancel`。提交身份来自真实父工具回执；创建 task、child session/run/input 和共享预算成员在一个事务中完成。子任务只接收显式文本指令，历史隔离；普通会话入口不能附着 child。
- `BackgroundController` 与 SDK 开放任务列表、查询、最长 10 秒等待、取消、审批和通知查询/确认。ACP 对应 `_deerflow/tasks/*`、`_deerflow/notifications/*`，按实际宿主能力协商；不开放任意 native submit、checkpoint 或 resume targets。
- 后台审批使用原生 Eino interrupt、保存批次与当前 task version。grant 消费与工具 dispatch/拒绝回执同事务。权限和关机暂停各有独立 manifest，绑定原始输入、配置、native checkpoint/history、领域事件和回执位置。
- 列表仅含审批摘要；SDK `BackgroundPermission` 与 ACP `_deerflow/tasks/permission/get` 让当前会话持有人读取指定批次、版本、intent 的真实工具参数，用于审批前展示。预览不产生授权、不改变通知或任务状态。公开审批仅允许 `allow_once/reject_once`。
- 子任务继承持久权限配置。`allow_always/reject_always` 通过独立 policy 审计生成当前 attempt 的一次性 grant；不伪造人工批准人。plan/read_only 允许 `task_status/task_wait`，仍禁止提交和取消工具。
- `Client.Close` 排空后台 I/O 后才释放数据库等共享资源。重启保留 `suspended`，SDK `ResumeBackgroundTask` 与 ACP `_deerflow/tasks/resume` 按精确版本显式恢复；旧 grant 不跨暂停复用。native 控制信号在转发前被观测，以区分系统 drain 和普通业务中断。
- 已 join 的模型/事件错误按执行失败处理；真正无法确认的资源清理继续隔离。取消与晚到故障同时发生时保留独立持久诊断。终态事务失败时 worker 保留 joined attempt，避免丢失预算、checkpoint 与错误证据。

真实 SDK 与本地 OpenAI fixture 覆盖后台写文件的批准/拒绝/取消、SQLite close/open 后审批、继承 allow_always、共享预算、child 隔离和重复请求拒绝。真实 ACP pipes 覆盖断连重附着、跨 owner 拒绝、任务管理、参数预览和通知确认。关机恢复测试在已完成写入后暂停下一次模型调用，重启时确认不自动执行，显式恢复后确认磁盘 marker 未被覆盖且只有一条原工具回执。

当前限制：非空 ACP client MCP 配置不向后台复制，因此隐藏后台提交；parent asset 能力和 child 产物不会自动互相导入。父会话 notification inbox 尚未作为持久模型输入消费。Memory、上下文压缩、MCP/模型媒体输出导入、计划/usage 投影、有界异步事件发送、可选外部 ACP agent、MANAGE/Python 配置迁移、打包与默认入口切换仍属于完整 V1 的待办。

本批最终组合验证：整模块普通回归的 SDK、CLI/daemon、runtime、engine、ACP、存储、MCP、进程集成等包通过；后台包的两个通知测试因 fixture 的 20 ms outbox lease 在并发编译时提前到期而失败。测试租约改为 5 秒，过期场景继续显式推进 SQL 中的租约；生产租约与恢复语义未改。后台全包串行普通测试 5.340s、race 11.020s 通过。最终 SDK 后台组合 race 62.354s，权限预览 runtime/ACP race 12.075s/9.629s 通过；真实 ACP 权限预览 race 12.080s、关机恢复 race 17.747s 通过。`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -mod=readonly -p=2 ./...` 通过。

以上仍为本地模型和协议 fixture；未进行真实付费模型、编辑器、官方 TCK 或 Docker 实机验收。下一项按 [通知 continuation 设计](eino-notification-continuation-design.md) 实施，使父 prompt 结束后的后台结果也能进入共享原预算的持久模型输入。

最后补充：SDK Close 仅在自身等待期限真正到期且没有 cleanup uncertainty 时继续等待，避免将底层错误中的 DeadlineExceeded 误当成可重试等待。对应 SDK 后台审批与关机恢复组合 race 再次通过（25.481s）。

## 第八阶段后台通知显式 continuation

- SDK `ProcessBackgroundNotification` 和 ACP 协商扩展 `_deerflow/notifications/process` 接受已保存通知 ID，并同步占用父会话的前台执行槽。通知绑定独立 run/input 与不可变来源快照，复用前台 Eino TurnLoop、持久审批和恢复路径；UI 已读状态与模型投递各自独立。
- 准入事务验证当前持有人、原任务 binding/spec、原始输入与预算成员、父会话配置和宿主策略，再保存来源、run/input、预算成员、attempt、execution 与 `continuation_started` 事件。原预算已用完时准入回滚，不新建预算 root。重复 process 返回原 execution，不重跑模型。
- 模型接收带来源标记、可读任务结果的通知数据。原用户 prompt 和原工具结果仍只在历史中各保留一次。来源种类与 SHA 固定在 execution、manifest 和 TurnLoop checkpoint；首次运行和恢复都重新检查保存的来源、扩展 pin、原始预算与当前策略。新工具的审批不继承原有 `allow_once`。
- 未提供 SDK 审批回调时，ask 策略留下 durable waiting；使用现有 `ResumeExecution` 批准。ACP 断连后也保留待批状态，重新附着可查询原执行并按版本恢复。

真实 Eino + 本地模型服务 + SQLite close/reopen 的 SDK 测试验证父 prompt 已完成、原预算计费、较晚的新 prompt 预算独立、UI 已读后处理、重复处理、无审批等待与恢复、配置漂移、原预算耗尽及来源篡改。真实双向 ACP 管道验证断连后重新附着、待批恢复、旧版本冲突和重复处理。整模块普通测试通过；SDK/真实 ACP continuation 聚焦 race、runtime/budget/ACP/engine 全包 race、Linux amd64 无 CGO 全模块交叉编译均通过。ACP 将原预算准入失败映射为 `-32015`，携带资源名并保留通知。

尚待完整 V1 的工作包括记忆与上下文压缩、MCP/模型媒体输出导入、计划与 usage 投影、有界异步事件发送、可选外部 ACP agent、MANAGE/Python 配置迁移、默认入口切换和真实编辑器/TCK/Docker 验证。自动通知调度可在显式处理接口之上继续实施。

## 第九阶段记忆基础层与只读注入

- SQLite 事实按规范化 workspace、session、显式 user 身份隔离；head 与不可变 revision、来源和 tombstone 持久化。FTS5/BM25 索引可按范围 revision 重建，故障或 CJK 子词命中不足时用有界词法回退。
- SDK 创建、查询、检索、替换、删除、清空事实；ACP `_deerflow/memory/*` 条件声明相应管理方法。所有操作按当前会话归属校验并占用前台槽，写入使用事实或范围版本检查。
- Eino DeepAgent 在模型指令中接收有界、明确标为描述性数据的相关事实。选择过程使用单个读事务取得事实与范围 revision；extension state 固定选中事实，恢复时沿用原快照，并绑定 host memory identity。
- ACP 请求严格校验字段和嵌套 fact；真实双向管道覆盖 owner、跨 session、版本和删除后的检索。真实 Eino 与本地模型服务验证注入及删除后不再注入；extension factory 验证恢复固定快照。
- 协议 reader 现在在 handler 完成后、写响应前释放 admission。客户端收到响应即可连续请求同一会话；原有通知续跑时序用例重复五次通过。整模块普通回归通过。

此阶段的手工管理和只读注入已完成；自动提取的实现与验收见下节。摘要压缩、旧 DeerMem 迁移和真实模型/编辑器验收仍未完成。

## 第十阶段受控提取与终态提升

- 显式开启 `MemoryExtraction` 后，根 Agent 成功答复触发一次由原 run 的 `trackedModel` 执行的有界 JSON 提取；后台 child 与通知 continuation 不视作新的用户偏好来源。默认关闭可选模型调用，两个 CLI 提供 `--memory-extraction` 和显式 `--memory-user-id`。
- 筛选要求 durable、descriptive、workspace 或已配置的 user scope、置信度阈值及有界内容，拒绝权限、当前任务、路径和凭据类候选。提取审计保存响应 SHA 与拒绝原因；模型 usage 仍进入原预算和领域事件。
- 候选写入按 attempt 隔离的 staging；终态 SQL 事务将事实提升与执行、预算结算一起提交。失败、取消、等待和终态事务错误不发布。范围 revision 漂移时舍弃候选，同文重复事实不累积；额度不足记录 `quota_skip`，不损害已完成的主回答。
- 本地模型 fixture 验证真实 Eino 提取、重启保留、来源 event sequence、非法候选拒绝、重复去重、配额跳过、终态 SQL 故障不发布和跨会话范围冲突。

后续阶段按 [记忆与上下文压缩设计](eino-memory-context-design.md) 继续完成只读记忆检索工具及真实模型校准。完整 V1 的媒体输出、异步事件、外部 ACP agent、管理接口及真实编辑器/TCK 验收仍待完成。

## 第十一阶段 Eino 摘要压缩

- SDK `Compaction` 和两个 CLI 的 `--context-compaction` 等参数显式开启，默认关闭。配置检查要求触发阈值为最近窗口留出空间，并纳入 Eino 执行合约。
- 摘要模型经当前 attempt 的 `trackedModel` 计入主/子共享预算、usage 与 I/O join。Eino 原生 `summarization` 负责触发和 `messages_replaced` 事件；自定义 finalizer 保留活动用户回合、工具调用及结果配对、最多 64 KiB 最近消息与有界已加载 Skill 片段。无效、过长摘要或过大的活动回合明确失败，不以错误文本替换上下文。
- 前台执行与后台任务恢复 manifest 额外绑定最近原生替换事件的序号、ID 和载荷 SHA。已有 manifest 无摘要字段时仍可按原有边界恢复。
- 原始业务事件、审批与工具回执继续完整保存。测试覆盖真实 Eino 会话替换、重启恢复、摘要失败不替换、共享模型次数、工具配对和早于 native head 的摘要篡改。

本批 `GOMAXPROCS=2 go test -mod=readonly -p=2 -count=1 -timeout=5m ./...` 通过；摘要、恢复 manifest 与启动参数的聚焦 race 测试通过；`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -mod=readonly -p=2 ./...` 通过。未使用真实付费模型或编辑器。

本阶段不等于完整 V1：媒体输出导入、计划/usage 投影、有界异步事件发送、外部 ACP agent、管理接口和真实编辑器/TCK 验收仍待完成。

## 第十二阶段同步 Memory flush 屏障

- SDK `FlushMemory(ctx, sessionID)` 与 ACP `_deerflow/memory/flush` 已接入。它按会话归属等待前台槽，最多 10 秒，随后读取主 SQLite 数据库；此前同步提取的终态事务已完成才能返回成功。
- 当前没有异步记忆写入队列。flush 不重新调用模型，不重试配额跳过，也不提升失败 attempt 的 staging。ACP 仅接受 `sessionId`，并在初始化能力中声明方法。
- SDK 与双向 ACP 管道定向测试覆盖成功、繁忙等待、超时、参数严格校验和其他连接无权访问。

## 第十三阶段旧 DeerMem 显式迁移

- 新增独立 `deerflow-memory-migrate` 命令。默认仅预览源 JSON；写入需要 `--apply` 以及现有 Go 会话、工作区和 session/workspace/user 范围的显式映射。命令使用 Go 数据目录独占锁，不触发模型或 HTTP 服务。
- 读取 Python DeerMem `version: "1.0"` 中的 `facts`，最多 4 MiB、1,000 条；旧摘要桶和 FTS 索引不自动迁移。内容经过与自动提取相同的保守筛选，来源记录为 `legacy-deermem`。同范围旧事实 ID 重试幂等；同 ID 内容改变时整个事务回滚。
- 存储层与真实 CLI 测试覆盖预览、拒绝危险事实、重复导入、来源冲突回滚和重启后读取。操作方法见 [迁移说明](eino-memory-migration.md)。

本批 SDK/ACP Flush 聚焦 race、迁移存储与 CLI 聚焦 race、整模块普通回归，以及 Linux amd64 无 CGO 全模块构建通过。源数据仅使用测试夹具，未对真实 Python 用户文件执行迁移。

## 第十四阶段 MCP 工具图片导入

- `officialmcp` 的实际结果路径会把 MCP 图片编码进 JSON 文本。现在通过官方结果 handler 在格式化前移除图片数据，执行侧的 enhanced wrapper 将图片块交给 Eino 原生增强工具中间件。工具与会话的连接代际、凭据检查、权限和调用预算沿用原边界。
- 增强工具原始图片限 PNG/JPEG/GIF/WebP、单张 20 MiB、每次最多 8 张及 40 MiB；校验 MIME 与内容，存入会话归属的不可变快照。成功 `tool_end` 在同一事务登记资产、领域事件和工具回执；失败、事务故障及关机丢弃暂存快照。原生历史和 SQLite 不保存图片 Base64。随后模型调用在明确允许的视觉模型上临时加载图片。
- 实际 MCP stdio 进程、Eino、ACP 双向管道和本地模型 fixture 验证图片到达下一次模型请求；存储测试验证发布前不可读取、SQL 事件故障回滚、文本与图片顺序及无内联字节落库。

流式增强工具图片、模型生成媒体和可选对象存储发布仍待实现。MCP 的音频与嵌入式二进制资源现在明确拒绝，避免经官方文本格式化路径将 Base64 带入模型上下文。

## 第十五阶段记忆只读检索工具

- 增加内建 `search_memory`。查询与返回量有界，作用域仅由当前会话工作区、会话 ID 和显式配置的 user 身份构造；模型不能在参数中选择其他作用域。结果仅含描述性事实、分类、revision 和范围，并明确标记为不可信数据。
- `plan` 与 `read_only` 允许此内建工具，命名空间内的 MCP 工具不能借名称获得相同授权。失败回执归为无外部副作用。前台 Eino 本地模型测试确认只读模式可实际调用、同工作区的另一会话看不到 session 事实。

检索结果按当前事实 head 读取；恢复后再次调用可看到新的事实版本。已写入原生工具历史的旧结果不会改写。

## 第十六阶段本机 MANAGE 运行诊断

- 复用 DFACP/1 受保护的本机端点，接入 `daemon.status`、`daemon.drain`、`daemon.resume` 和 `session.list`。排空先在会话协调器锁内阻止新建、附着和新 prompt，再暂停后台新提交与派发；既有后台 worker 继续完成，尚未派发的持久任务留待恢复。`active_operations` 合计前台运行、后台提交中操作和已派发任务，桌面等待其归零才重启；`active_runs` 只计实际运行，`queued_runs` 计已派发但尚未运行的后台任务。状态还提供 ACP 连接数和排空状态。
- 会话清单最多 1,000 条、3 MiB，按更新时间排序并排除后台 child；显示进程内 attached/running/closing 状态。当前 Go 存储尚无持久 close/retention 状态，所以不推断清理资格，`cleanup_eligible` 固定为 false。未迁移的删除和记忆管理仍明确返回 unsupported。
- SDK、真实 daemon 进程与 ACP 管道测试验证状态、清单、排空阻止新会话及新执行、恢复和独占资源；可选 Rust Bridge 二进制测试增加 `--manage daemon.status` 验证。Go daemon 仍未接受 Python `--config`，Bridge 自动启动迁移尚未完成。

本批整模块普通测试、管理与后台排空聚焦 race 测试，以及 Linux amd64 无 CGO 全模块构建通过。后台执行的 drain 回归测试确认活动 worker 完成前活动数不归零，新提交在排空期间被拒绝，恢复后可以继续提交。

## 第十七阶段 Bridge 配置入口兼容

- Go daemon 接受 Bridge 的 `--config`，解析最多 2 MiB 的现有 YAML，按 `local_acp.model_name`、`api.model_name`、`default_model` 顺序选择模型，并映射提供商、凭据、端点、视觉能力、子 Agent、连接数及运行期限。显式 Go 标志覆盖对应值；同提供商、同凭据和端点的模型进入会话选项。
- endpoint 保存规范化配置路径，MANAGE 状态返回原配置文件 SHA-256；Bridge 能校验当前 daemon 的配置身份。Go 数据库仍在独立目录，不读取 Python checkpoint。权限模式、工具名单和自动 goal continuation 未迁移时拒绝启动，不悄悄扩大执行授权。
- 使用仓库当前 `config.example.yaml`、特制 YAML、环境引用及真实 daemon 进程验证加载、配置身份与状态。映射范围和限制见 [Go daemon 配置兼容](eino-go-config-bridge.md)。

## 第十八阶段本机 MANAGE 记忆操作

- `memory.get` 按已有普通会话的工作区解析 session/workspace 和显式 user 事实，最多 1,000 条和 3 MiB。后台 child 不可用作管理作用域；本机管理 token 可以读取已脱离 ACP 客户端的会话。
- `memory.delete` 以事实 ID 查找当前会话可见的作用域，暂时排空前台与后台准入，活动任务不为零时拒绝。写入使用事实 revision 条件；操作结束恢复原排空状态。响应沿用桌面需要的 `memory.facts` 结构。
- SDK 与真实 ACP/daemon/MANAGE 管道测试覆盖脱离会话、跨工作区拒绝、运行中拒绝以及成功删除后状态恢复。`session.delete` 仍待实现，清理资格继续固定为 false。

第十七、十八阶段合并验证：整模块普通测试、管理/配置/真实 daemon 聚焦 race 和 Linux amd64 无 CGO 全模块构建通过。真实付费模型、编辑器、官方 ACP TCK 和 Docker 实机仍未验收；本进度不代表完整 V1 完成。

## 第十九阶段有界异步 ACP 事件发送

- ACP 会话更新先经过原有运行时持久化，再以不等待客户端读取的方式进入同一个协议 writer 队列。异步事件总占用默认不超过 64 MiB，队列最多 64 帧；超过上限立即关闭该连接并取消执行，已保存的事件可通过会话历史重放，不静默丢弃。
- 入队后的事件使用连接生命周期，prompt 取消不会撤销已接受的帧；writer 保持与最终响应的入队顺序。同步的普通通知、权限请求和响应继续等待真实写入。
- 协议测试覆盖慢写入背压、字节上限、prompt 取消后的已接受事件和响应排序。完整 ACP agent/protocol 普通测试、相关聚焦 race、整模块普通回归和 Linux amd64 无 CGO 构建通过。真实编辑器长流仍待验收。

## 第二十阶段原生子 Agent 的 ACP 生命周期投影

- Eino 原生 `subagent_start/suspended/resumed/end` 事件投影为 ACP `tool_call` 与 `tool_call_update`。暂停时保留合法的 `in_progress` 状态，并在 `_meta.deerflow.state` 标明 `waiting_input`；恢复时更新为 `running`，结束时输出 `completed` 或 `failed` 及内容。
- ACP 卡片 ID 包含 RunID 与 Eino CallID，生命周期跟踪同时按会话隔离。跨会话/跨运行复用 CallID 不会覆盖卡片；缺少前序 start 的恢复事件会先补建卡片。会话关闭后清理该连接中的跟踪状态。
- 真实 ACP stdio 管道测试覆盖并发会话、暂停→恢复→完成、响应顺序及持久历史 load 重放。整模块普通测试、聚焦 race 和 Linux amd64 无 CGO 构建已通过；真实编辑器仍待验收。

## 第二十一阶段 ACP 上下文用量投影

- 为每个可选模型显式配置 `context_window`；Go 命令行新增默认模型的 `--context-window`，Bridge YAML 中兼容模型的窗口大小进入对应模型配置。模型切换后按当前模型查找窗口。
- Eino 只从最后一次主 Agent 模型响应读取实际 input/output usage，保存独立的 `context_usage` 领域事件并投影为 ACP `usage_update`。子 Agent、记忆和压缩调用的 token 仍进入原有累计预算，但不冒充主线程上下文占用。缺少窗口或模型 usage 时不发送该更新。
- Eino 单次/多次模型调用、原生子 Agent 隔离、ACP stdio 更新和历史重放、Bridge 配置映射已有定向测试。计划投影在随后确认 Eino 内建 `write_todos` 的结构化输出后接入。

## 第二十二阶段 Eino 原生计划的 ACP 投影

- Eino DeepAgent 内建 `write_todos`，成功执行时返回完整的结构化 TODO 列表。仅从主 Agent 的真实工具结果生成 `plan_update` 领域事件，映射 ACP `plan`；默认优先级为 `medium`，清空列表发送空数组以替换客户端旧计划。
- 子 Agent 的待办不覆盖主会话计划。自定义工具不能占用原生 `write_todos` 名称，以确保投影只解析 Eino 的可信内建结果。领域事件随会话历史持久化，`session/load` 可重放。
- Eino 主/子 Agent 与 ACP stdio 管道定向测试覆盖计划生成、隔离、清空及重放；整模块普通测试、相关 race 和 Linux amd64 无 CGO 构建通过。真实编辑器互操作仍待验收。

## 第二十三阶段官方 ACP TCK

- 官方 `acp-tck v0.2.0`（仓库提交 `b15c7bdb`，stable v1，schema revision `6d08f412a7a1370d3cc9a124e3be3d6acf92641e`）使用真实 Go stdio 可执行文件和本地模型 fixture 完整运行。结果为 `CONFORMANT`：21 项 mandatory 全通过，11 项已启用 capability 全通过，8 项 capability 跳过；10 项 advisory 通过、1 项失败、1 项跳过；4 项 informational 通过。
- 修复 `session/resume` 对 `mcpServers` 的错误强制要求。该字段按 ACP resume 类型允许省略；显式 `null` 仍拒绝，`session/new` 与 `session/load` 仍要求数组。ACP 管道回归和 TCK 中 `ACP-RESUME-001/002` 通过。
- 唯一失败项 `ACP-PROMPT-003` 是 advisory。测试传入工作区外且不存在的 `file:///tmp/tck-example.txt`；当前文件引用策略要求引用可读取且位于授权工作区，故返回参数错误。TCK 自身将此条标为规范文本存在分歧的 advisory，不影响 conformance verdict。以后若调整协议兼容性，仍需保持文件读取边界。
- 官方 TCK 不覆盖真实编辑器互操作、图片输入、MCP 重绑定、后台子任务、持久恢复、Docker 或打包；完整 V1 仍须逐项验收。
- 本批整模块 `go test -count=1 ./...`、ACP/MCP/SQLite 聚焦 race 测试，以及 Linux amd64 无 CGO 全模块构建通过。整模块并行运行曾暴露测试夹具时序问题：MCP fixture 启动限时由 300 ms 放宽至 3 s；上游通知 outbox conformance 的 20 ms 租约改用夹具显式推进的固定时钟，避免调度延迟造成虚假租约丢失。运行时代码未因这两处测试调整而改变。

## 第二十四阶段会话删除

- 本地 `MANAGE session.delete` 和 ACP `session/delete` 清除普通前台会话。协调器在持久清理及资产回收期间阻止重连；ACP 持有者可将空闲附着会话原子转换为删除预留，其他连接不能删除正在附着的会话。未知会话删除幂等成功。
- 删除事务按外键顺序清除会话输入、事件、执行、检查点、回执、预算、会话记忆及内部资产引用。提交后仅回收已无数据库引用的内部快照；工作区文件与 workspace/user 记忆保留。存在关联后台任务或未对账工具回执时拒绝删除，避免留下孤立任务或丢失未知副作用证据。
- SDK、数据库回滚/重试、真实 daemon MANAGE 管道和 ACP stdio 测试覆盖附着拒绝、跨连接隔离、持久行与文件清理。官方 ACP stable v1 TCK 再次判定 `CONFORMANT`：21 项 mandatory、12 项已启用 capability（含 `ACP-DELETE-001`）全通过；11 项 advisory 通过（含 `ACP-DELETE-002`），`ACP-PROMPT-003` 因工作区外不存在的文件引用仍失败。完整回归结果以本阶段最终测试记录为准。
- 自动保留期策略尚未实施，管理清单的 `cleanup_eligible` 仍固定为 false。后台任务图的安全删除和真实编辑器互操作仍待验收。
- Windows 整模块普通测试、会话/runtime/资产/ACP/daemon/SDK 聚焦 race 检查及 Linux amd64 无 CGO 全模块构建通过。额外故障路径验证：资源释放失败后会话保持 `closing`，不能继续 prompt，owner 可重试 close；SQL 删除失败可重新 load 并重试删除。

## 第二十五阶段自动会话保留期

- Go stdio 和 daemon 默认启动时扫描一次，此后每小时扫描；显式关闭的会话和未关闭但已脱离连接的会话默认分别保留 30 天。CLI 可设置开关、两种保留天数与扫描间隔，Bridge `--config` 映射对应 `local_acp` 字段；显式 CLI 标志优先。嵌入式 SDK 的零值仍关闭自动清理，可显式配置策略并调用单次扫描。
- 旧 Go 数据库迁移增加 `closed_at`。显式 `session/close` 在资源释放成功后记录关闭时间；成功 `session/load` 清除关闭状态并刷新活动时间；普通与 durable run 的结束事务也刷新活动时间，避免长执行后立即到期。扫描按 ID 有界分页，删除预留阻止重连，并在事务清理前复核到期时间。
- 已附着/运行/正在关闭的会话、后台任务图及未对账工具回执不会自动清理。`MANAGE session.list` 的 `cleanup_eligible` 结合时间、进程状态与持久阻塞条件计算；它只是即时提示，实际删除再次复核。后台任务图的安全删除留待独立策略。
- SDK、迁移、分页、配置优先级、启动扫描及真实 daemon 重启测试覆盖上述路径。Windows 整模块普通测试、SDK/runtime/launch/daemon 聚焦 race 和 Linux amd64 无 CGO 全模块构建通过。官方 `acp-tck v0.2.0` 使用真实 Go stdio 可执行文件和本地模型夹具再次判定 `CONFORMANT`：21 项 mandatory、12 项已启用 capability 全通过；唯一失败仍是工作区外不存在文件的 `ACP-PROMPT-003` advisory。

## 第二十六阶段终态后台任务图清理

- 会话删除和自动 retention 可以在一个 SQLite 事务中清除已完成、失败或取消的后台任务及其私有 child 会话。一个 child 被多个顺序任务复用时只清理一次；任务快照、宿主绑定、预算归属和 child run 的终态必须一致。清理覆盖 Eino 任务/事件/通知、后台审批与诊断、child 历史、回执、预算和资产引用。内部资产在提交后回收，工作区文件保留。
- 运行中、等待输入、暂停、有执行租约、未送达原生通知、未对账回执、关联 task 的未知预算消耗或持久化故障证据的图继续阻止删除。自动 retention 还等待 inbox 通知被 UI 确认；显式 `session/delete` 可以删除终态图中的未确认 UI 通知。`MANAGE session.list.cleanup_eligible` 按相同的终态与通知条件给出即时提示，删除事务重新检查。
- 数据库故障回滚、历史审批外键顺序、自动通知阻塞和真实 SDK 后台子 Agent 终态删除已加入回归。Windows 整模块普通测试、SDK/runtime 聚焦 race 和 Linux amd64 无 CGO 全模块构建通过。官方 TCK 不覆盖后台任务图；本批未重新运行 TCK，ACP 协议层的后台扩展仍需真实编辑器互操作验收。

## 第二十七阶段可选外部 ACP stdio 委派首轮

- `ACPAgents` 为宿主显式允许列表，两个 Go CLI 可用 `--acp-agents-config` 装入 JSON。没有配置时不暴露 `invoke_acp_agent`。`plan`/`read_only` 不暴露该工具；模型只可选择已配置的名字和发送文本任务，不能更改命令、参数和凭据。Bridge `--config` 如含 Python `acp_agents`，没有显式 Go 允许列表便拒绝启动，避免静默遗漏。
- 每个父会话和外部 Agent 使用独立私有 workspace 与持久 remote session ID。客户端以 stdio 完成 `initialize → session/new` 或 `session/load → session/prompt`；只声明空 client capabilities，不代理尚未实现的客户端 fs/terminal。更新经 Eino `tool_update` 发出；连接仍在时，外部反向权限经所属会话的策略及 ACP 客户端实时批准，不自动批准。断线不会重放旧权限响应，外部进程终止并保留未对账的外层工具回执。外部资源链接只作更新引用，不下载。
- 外层工具调用和一次估算模型调用计入父预算；流式文本增加估算 token，用完即中止。标准 ACP 没有可靠的外部 Agent 内部模型调用/计费总量，因此这还不是完整的远端预算约束。外部可执行文件仍须由宿主信任，独立 cwd 和精简环境不是 OS 沙箱。父会话删除后回收私有 session 映射与 workspace。
- 真实子进程协议测试覆盖 new/load、流式文字和工具更新、默认拒绝反向请求、显式选择、超时及回收；Eino 测试覆盖父模型调用额度和进度，durable runtime 测试覆盖连接内实时反向审批路由。配置说明见 [外部 ACP 委派](eino-external-acp.md)。完整 V1 仍需断线后的反向权限恢复、远端预算/副作用对账、媒体和真实编辑器验收。
- 本批 Windows 整模块普通测试、ACP client/protocol/Eino/runtime/SDK 聚焦 race，以及 Linux amd64 无 CGO 全模块构建通过。资源链接校验限制本地文件须位于外部独立 workspace；HTTP(S) 链接只作为引用。
