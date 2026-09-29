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
| 会话协调 / stdio | 双向传输、同步准入、占用、取消、new/list/load/resume/close 已写入 | 黑盒互操作、官方 TCK、真实编辑器 |
| 执行与工作区工具 | Eino TurnLoop/DeepAgent、前台委派、文件工具、plan/read_only、主/子共享预算；执行回执与命令工具已接线 | 长期会话循环、后台委派和更完整的工具集 |
| 权限 / 恢复 | 原生 durable HITL、审批/检查点/回执/预算联合恢复、SDK/ACP 显式执行查询与恢复已接入 | 后台任务 broker；真实编辑器与 MCP 重新绑定恢复 |
| 领域事件 / 历史 | 持久事件、文本/工具/产物 updates、分页历史与 load 重放 | 有界异步发送、计划/usage 投影 |
| MCP | ACP client 配置、stdio/出站 HTTP/SSE、官方工具适配、会话代际替换、凭据隔离已接入 | 真实编辑器互操作、与后台任务生命周期组合 |
| 会话配置 | 模型白名单、subagent、ask/allow_always/reject_always/read_only、版本与审批缓存撤销已持久化 | thinking/profile 仅在真实能力落地后开放 |
| 图片 / 附件 / 产物 | 不可变资产快照、引用持久化、模型前临时加载、SDK/ACP 图片输入、view_image 与产物登记已验证 | MCP/模型生成媒体导入、可选对象存储发布 |
| 后台子任务 / 长命令 | 原生 Manager、有界 worker、child 租约、fenced stores、事务化 checkpoint 与通知 inbox 已通过 fixture 验证 | 真实模型/工具 factory、审批、SDK/ACP 与父会话输入接线 |
| Skills / memory / 压缩 | Skills 不可变注册表、原生 middleware、SDK 管理、显式 CLI 启动配置与逐步加载已接入 | memory、压缩及其共享预算 |
| Docker / Windows shell | 默认禁用；可选 local/PowerShell/WSL2/Docker 命令后端；进程树、输出、环境与资源限制已接入 | Docker 真实运行验收，跨 run 后台命令生命周期 |
| daemon / Bridge / draft v2 | Go daemon 的 DFACP/1、认证 endpoint、STATUS/STOP、多窗口、现有 Rust Bridge 实际二进制互操作已验证 | draft v2 对照、Python --config 迁移、MANAGE 诊断子集 |
| 可选外部 ACP Agent | 待实现 | 白名单、反向权限、预算和取消链 |
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

新增 GitHub Actions Windows/Linux 验证定义尚未推送或在远端执行。真实模型联网测试、Linux 运行期验证、ACP TCK 和编辑器实测仍待后续阶段。本阶段尚不满足完整 V1 发布条件。

## 下一阶段

历史查询现提供 SDK `HistoryPage` 与 ACP `_deerflow/history/list`，游标绑定会话并固定首次查询的事件上限。`session/load` 持有会话租约分页读取并完整重放，`session/resume` 仍不重放。验证覆盖分页间新增事件、全量重放、跨会话游标、其他连接访问、繁忙会话与大事件边界，runtime 与真实 ACP 管道限定测试通过 race。

继续装配长期 TurnLoop、公开 durable HITL 恢复、Memory、压缩及后台任务 host。后台 Manager 与原生 fixture 已通过测试，实际模型/工具 factory、审批、SDK/ACP 与父会话通知输入尚需接线。

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
