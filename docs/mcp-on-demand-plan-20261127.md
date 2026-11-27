# MCP 工具懒加载与本地服务按需启动最终方案

桌面用户只需在现有 MCP 设置中保存服务定义。Go harness 自动决定何时向模型提供工具的参数定义（schema），并在首次定向发现或实际调用时启动对应的本地 stdio 服务。桌面、ACP 和 Go SDK 共用这条执行路径，MCP 路由仅在 harness 进程内提供。

例如，用户在桌面配置了浏览器、数据库和文档服务。打开会话、恢复聊天历史、提一个普通编程问题，三个进程的启动次数都应为零。首次查询数据库时，即使磁盘目录为空，也能在这条用户消息内启动数据库服务、发现工具、按原工具名审批并完成查询。浏览器和文档服务的启动次数仍应为零。

设计基于 DeerFlow `6ac88642` 和 Magpie `bc62dfd7`。2026-11-27 已实施并更新 Windows 桌面版，运行中的后端构建为 `deerflow-mcp-on-demand-20261127.1`。[实施验证](#实施验证) · [DeerFlow 基线](https://github.com/omengye/deerflow-acp/tree/6ac88642be5f4bdb9af05174da7deb1441e8b0db) · [Magpie 同一提交的设计文档](https://github.com/yetone/magpie/blob/bc62dfd71e11c1b93faabd2eeba3a1edc7c4dca3/docs/subsystems/mcp-router.md)

## Magpie 的实际行为与取舍

| 机制 | Magpie 的实现 | DeerFlow 的选择 |
| --- | --- | --- |
| 启动与并发 | `backendOf` 按服务名查找后端，并用配置和登录身份区分版本。`connect` 复用活连接，同时到达的调用方等待同一个 `ready`。单个调用方取消等待后，有时限的启动可以继续。[后端池与启动](https://github.com/yetone/magpie/blob/bc62dfd71e11c1b93faabd2eeba3a1edc7c4dca3/internal/library/mcp_hub.go#L505-L648) | 复用一次启动与握手的做法。进程归属仍按 DeerFlow 的会话或明确授权的工作区 recipe 划分。 |
| 冷目录 | `initialize` 遇到未知服务卡片时可以用最多 5 秒补目录。`tools/list` 在 40 秒预算内并行列举获准服务。目录未命中时，`listAll` 本身就会建连。[初始化](https://github.com/yetone/magpie/blob/bc62dfd71e11c1b93faabd2eeba3a1edc7c4dca3/internal/gateway/hub_mcp.go#L782-L799) · [并行列举](https://github.com/yetone/magpie/blob/bc62dfd71e11c1b93faabd2eeba3a1edc7c4dca3/internal/library/mcp_hub.go#L342-L379) · [目录读取](https://github.com/yetone/magpie/blob/bc62dfd71e11c1b93faabd2eeba3a1edc7c4dca3/internal/library/mcp_hub.go#L725-L771) | 普通聊天和泛搜索只读本地目录。冷目录通过明确选中的单个服务补齐，避免第一次搜索启动所有程序。 |
| 两级缓存 | 完整工具列表保存在后端内存中。`mcp-hub-servers.json` 持久化配置指纹、标题、简述和工具名称/短描述，采用临时文件替换。进程空闲退出后仍能从内存列目录，网关重启后磁盘卡片足以介绍服务。[卡片格式与写入](https://github.com/yetone/magpie/blob/bc62dfd71e11c1b93faabd2eeba3a1edc7c4dca3/internal/library/mcp_hub_cards.go#L26-L42) · [卡片生成](https://github.com/yetone/magpie/blob/bc62dfd71e11c1b93faabd2eeba3a1edc7c4dca3/internal/library/mcp_hub_cards.go#L117-L172) · [空闲后列举的测试](https://github.com/yetone/magpie/blob/bc62dfd71e11c1b93faabd2eeba3a1edc7c4dca3/internal/library/mcp_hub_test.go#L184-L218) | 持久保存经过筛查的发现信息，完整 schema 放内存。每次新进程握手后核对真实工具身份和 schema，再执行。 |
| 空闲回收与失效 | `use`、`done` 维护使用计数。最后一个使用者离开后设定空闲计时器，回调再次核对计时器和使用数。默认 10 分钟，可按服务调整或常驻。工具列表变更通知触发重列，编辑、移除或关闭可清理旧后端。[空闲回收](https://github.com/yetone/magpie/blob/bc62dfd71e11c1b93faabd2eeba3a1edc7c4dca3/internal/library/mcp_hub.go#L664-L722) · [配置清理](https://github.com/yetone/magpie/blob/bc62dfd71e11c1b93faabd2eeba3a1edc7c4dca3/internal/library/mcp_hub.go#L464-L500) · [变更通知](https://github.com/yetone/magpie/blob/bc62dfd71e11c1b93faabd2eeba3a1edc7c4dca3/internal/library/mcp_hub_watch.go#L90-L103) | 桌面进程首次启动后可在会话空闲时保留浏览器等服务的状态。显式设定空闲时限的宿主 recipe 在回收前须核对审批、检查点和后台执行租约。 |
| 模型工具发现 | 对适用客户端，超过 10 个工具时提供 `find_tools` 和 `call_tool`，搜索优先返回前 5 个完整定义。[按需暴露实现](https://github.com/yetone/magpie/blob/bc62dfd71e11c1b93faabd2eeba3a1edc7c4dca3/internal/gateway/hub_ondemand.go) | 使用 Eino 客户端搜索和原名工具调用。这样审批卡片、凭据接收方、增强结果和回执都能对应到具体工具。 |

## 适用范围与默认值

| 来源 | 启动和所有权 |
| --- | --- |
| 桌面设置保存的 stdio | 新增服务和升级前已保存的服务统一采用程序内部默认值 `startup=on_demand`、`idle_timeout_seconds=0`。首次使用时启动，空闲期间保持运行，随所属会话释放。每个会话持有自己的进程。 |
| 宿主 `mcp.host_recipes` 中的 stdio | 宿主可对单个服务明确选择 `on_demand`。缺省为 `eager`。只有匹配同一规范工作区、同一 recipe 和定义的会话才继承已有共享关系。 |
| 普通 ACP 客户端传入的服务及默认 SDK 绑定 | 保留严格的原子 `Bind`。候选连接或目录发现失败时，原绑定继续可用。可信 SDK 宿主可用工作区 recipe 选择按需启动。ACP 请求本身只提供连接意图。 |
| HTTP 与 SSE | 参加角色内的 schema 懒加载。远端进程由服务端拥有，出站连接、凭据和恢复继续采用原有策略。 |

桌面表单只展示服务名称、程序路径、参数和环境变量等现有连接信息。“启动策略”和“空闲回收”由 Go harness 在匹配桌面授权后自动确定。这两项默认值只在运行时生效。原格式的 `client-mcp-servers.json` 在升级后直接使用，已有条目在新版建立会话绑定时自动生效。

桌面 Rust 配置读写沿用已有字段、版本校验、凭据回填和原子保存，`into_acp` 继续传标准 MCP 配置。独立宿主可在 YAML 的 `mcp.host_recipes` 中为服务保存 `startup`、可选的一行 `description` 和 `idle_timeout_seconds`。宿主的空闲时限默认为 0，可为适合重启的服务填写 600 秒或其他正数。服务移除、授权撤销、会话释放或宿主关闭时按所有权开始清理。[桌面配置与 ACP 投影](../desktop-app/crates/waku-core/src/deerflow_config.rs) · [Go 桌面授权](../go-harness/internal/mcp/desktop.go) · [宿主配置校验](../go-harness/internal/configschema/host_mcp.go) · [工作区归属](../go-harness/internal/mcp/host_recipes.go)

## 发现目录与同一轮调用

1. **绑定时登记服务。** 保留现有路径规范化、授权校验、工作区和服务数限制。`on_demand` 条目只登记已授权定义与服务标识。`Bind`、新前台回合快照和普通历史读取均可在零个按需进程运行时完成。混合配置中，`eager` 条目继续执行原有握手与错误处理。[现有绑定](../go-harness/internal/mcp/manager.go) · [现有快照](../go-harness/internal/mcp/lease.go)
2. **离线搜索。** `tool_search` 在当前角色可见的目录中查询名称、服务用途和短描述，默认取 5 个候选。普通中文查询应能看到可选服务的中文用途，可用英文关键词或精确工具名细化。零命中返回可选服务，歧义让模型缩小范围。此操作的子进程启动数和远端 `tools/list` 数都为零。
3. **定向发现。** 缺少完整 schema 时，搜索结果指出具体候选服务。模型通过宿主内置的 `mcp_discover(server, query)` 选中其中一个，`server` 只能引用本次运行获准的服务标识。宿主重新检查启动授权，启动或复用该服务，完成 `initialize` 与分页 `tools/list`。它只发现工具，并将匹配项发布给下一次模型请求。空目录、缺少程序或握手失败均返回该服务的可解释状态，其他服务和聊天可继续。
4. **以原工具名执行。** 如果已有完整 schema，搜索直接发布命中项。用户不需要再发一条消息。真实调用沿用原 `mcp_…` 名称、参数 schema、审批、凭据作用域、预算、图片/结构化结果和回执。空闲回收后的首次调用会重新握手和校验，校验通过才发出业务 `tools/call`。[工具命名与增强结果](../go-harness/internal/mcp/endpoint.go) · [凭据作用域](../go-harness/internal/mcp/credentials.go) · [审批与回执](../go-harness/internal/engine/eino/tools.go)

**模型可见范围。** 根代理和每个子代理分别在工具策略过滤后构建搜索目录。普通搜索只读本地目录。定向发现会运行程序，因此按具体服务核对宿主的启动授权。现有计划模式和只读模式不给模型 MCP 工具，同样隐藏定向发现入口。只有工具名称白名单、尚无目录能确认对应服务的受限角色，可由已获准的父代理定向发现，再按角色白名单投影结果。缓存、模型给出的名称以及服务器自报的描述都不能扩张权限。[角色筛选](../go-harness/internal/engine/eino/roles.go) · [计划与只读模式](../go-harness/extensions.go)

两个发现工具使用保留名和宿主构造器身份。离线搜索在 `dangerous` 模式按可信只读工具分类，`all` 模式遵守逐项审批。定向发现的审批显示具体服务，真实业务调用另按目标工具及参数判定权限。SDK 同名工具不能继承这些权限。[宿主工具身份](../go-harness/internal/toolidentity/native.go) · [权限模式](../go-harness/harness/permission_mode.go)

**工具数较少时的快路径。** 桌面自动采用 `auto`。当该角色的 MCP 目录完整、所有 schema 已在内存且工具数不超过 10 时，直接声明它们。其他情况提供搜索入口，存在获准的冷服务时提供定向发现入口。独立宿主的 `mcp.tool_exposure` 默认同为 `auto`，也可用 `full` 声明已知完整 schema 的全部授权工具，并与 `eager` 组合复现旧行为。内置工具照原策略声明，目录提示按服务概括用途。10 和 5 是首版起点，按实际 schema 大小和任务总成本校准。[Eino 客户端搜索](../go-harness/third_party/eino/adk/middlewares/dynamictool/toolsearch/toolsearch.go) · [上下文估算](../go-harness/internal/engine/eino/context_window.go)

**同一回合的工程落点。** Eino 的 `toolsearch.New` 在构造时固定 `DynamicTools` 索引，`BeforeAgent` 登记可执行工具，`BeforeModelRewriteState` 控制发给模型的 `ToolInfos`。只追加 `ToolInfos` 会导致冷发现的名称在工具节点找不到。先复用 `UseModelToolSearch=false` 验证已知工具，并消除执行工具列表中的重复注册。接入冷发现后，由 DeerFlow 的发现中间件提供同一个 `tool_search`，每次 `BeforeModelRewriteState` 从当前运行和角色的只读快照投影命中项。快照包含真实 `BaseTool`、schema 与审批/凭据身份。模型步骤和对应工具调用批次固定使用同一版本，发现结果从下一模型步骤生效。[Eino 动态工具注册](../go-harness/third_party/eino/adk/middlewares/dynamictool/toolsearch/toolsearch.go) · [ADK 运行期工具装配](../go-harness/third_party/eino/adk/chatmodel.go)

具体在随仓 Eino 的 `ToolsNode` 增加可选的按批次取工具列表回调，未设置时沿用原路径。`Invoke`、`Stream`、中断恢复统一复用现有列表转换与增强工具中间件。宿主的权限与凭据查询读取同一快照。业务工具通过正常 `ToolsNode` 执行，未知名称继续走原有报错。将这项补丁及回归用例记入现有 Eino 补丁说明。当前 DeerFlow Responses 适配器显式拒绝原生 deferred tools，因此各模型统一先走客户端实现。[工具节点](../go-harness/third_party/eino/compose/tool_node.go) · [补丁记录](../go-harness/third_party/eino/PATCH.md) · [Responses 适配器](../go-harness/internal/engine/eino/models_responses.go)

## 缓存与进程生命周期

**目录的归属。** 桌面和宿主 recipe 的发现摘要存入现有本地 SQLite 状态库，按配置来源、规范工作区、服务身份及配置指纹隔离。普通客户端的目录限于该会话内存。持久摘要只含已筛查的原始工具名、对模型暴露的稳定名称、短描述、schema 摘要和格式版本。原始名称在发现时记录，避免反解析截断过的工具名。完整 schema、适配器和 PID 留在内存。命令参数、环境变量、请求头、令牌和工具结果不进入目录。目录损坏按未发现处理，服务改名、程序身份或配置/凭据变化使旧条目失效。可以复用同一工作区的目录摘要，进程复用仍以会话绑定或明确的 host recipe 为界。[目录元数据筛查](../go-harness/internal/mcp/endpoint.go) · [配置与工作区校验](../go-harness/internal/mcp/policy.go)

一次完整分页发现成功后，按同一版本发布内存目录并事务性更新持久摘要。执行现有 `MaxServers`、聚合 `MaxTools`、`MaxToolPages` 上限，并限制摘要和 schema 总字节数。分页失败或越界时舍弃本次半成品。活连接收到 `notifications/tools/list_changed` 后合并刷新，新目录在下一步骤生效。正等待执行的调用仍校验已固定的工具。每次新建连接都重新校验实际目录。没有变更通知的服务，在固定资源恢复或复用旧目录执行前重新列举核对。同次握手已完成的发现结果可直接用于当前调用。[现有桌面目录刷新](../go-harness/internal/mcp/desktop_process.go) · [schema 身份校验](../go-harness/internal/mcp/lease.go)

| 事件 | 进程管理器的行为 |
| --- | --- |
| 从未启动或已空闲回收 | 保留目录和启动意图。定向发现或业务执行才申请连接。同一所有者、同一服务版本同时收到的请求共用一次启动、握手和首次列举。每个等待者可独立取消。 |
| 正在启动或发现 | 工作受绑定所有者的上下文和连接时限约束。一个等待者取消不影响其余等待者。最后一个等待者在就绪前离开时，取消并收回未完成的启动。所有者释放、撤权或替换定义时也取消旧工作，晚到的进程在发布前回收。 |
| 已就绪或调用中 | 取得操作引用后再执行，取消旧空闲计时器。保留现有独立的握手、调用和关闭时限。单次业务超时按原回执处理，后续独立请求再判断连接是否健康。 |
| 宿主 recipe 的空闲期限到达 | 原子核对定义版本、实际连接代次、当前计时器、在途操作数及固定该代次的租约。全部允许才进入关闭。新请求与关闭竞合时等待旧进程退出，再按需启动新代次。离线目录搜索不会续期。 |
| 崩溃、配置替换或授权撤销 | 失效旧连接和相应租约，终止待发布结果。失败的启动只在后续明确需求下重试，并合并并发重试。关闭时沿用宿主直接子进程的退出等待，清理超时仍跟踪至退出。健康服务继续可用。 |

`keep alive` 由空闲时间为零表达。正数空闲时间也只回收没有在途操作、没有审批或可恢复任务占用实际连接的进程。实现须区别预期空闲关闭、意外断连和所有者撤销。当前普通 endpoint 的 `Wait` 结束会取消整组 binding，延迟服务应拥有自己的连接生命周期，避免回收一个程序连带取消整个会话。客户端释放只结束该会话的资源，宿主 recipe 随宿主关闭。沿用目前直接子进程的清理保证。[现有连接关闭](../go-harness/internal/mcp/endpoint.go) · [直接子进程的关闭与等待](../go-harness/internal/mcp/command.go)

## 执行契约、审批和恢复

- **使用两种身份。** 授权定义的版本标识当前所有者、工作区、配置及可见范围。实际连接代次标识一次成功的进程握手。空闲回收和再次启动产生新连接代次。缓存只提供发现线索，相同的 schema 摘要不能把旧连接的授权转给新进程。[现有连接代次](../go-harness/internal/mcp/manager.go) · [资源描述和授权](../go-harness/harness/resources.go)
- **兼容既有执行。** v1 `MCPResourceRef`、扩展状态和执行契约按原方式读取、重建和比较。启用新方案的运行使用 v2，冻结允许的服务定义、工具策略和发现策略版本。前台输入与后台接管按该定义版本核对，避免无关服务启动或回收使整轮失效。检查点按运行、角色和模型步骤记录已展示的工具名及规范 schema 摘要。已固定实际连接的工具另记连接代次和租约授权，两类记录与检查点一致提交。恢复时重建这一批工具与审批身份，再交给 Eino。下一次搜索不能覆盖已固定项。旧版 `Describe` 和 `Acquire` 的真实资源校验继续有效。[检查点契约](../go-harness/internal/engine/eino/turn_loop.go) · [前台及后台资源恢复](../go-harness/background_resources.go)
- **在副作用前核对。** 在工具中间件进入业务审批前，核对角色和启动授权，必要时完成目标握手，固定实际连接与 schema。宿主从真实工具及参数生成凭据作用域，审批绑定这个目标。从等待审批到实际发出 `tools/call` 一直持有该连接的固定引用，发送前再次核对授权、程序身份、代次和 schema。变更时终止旧调用并让模型重新搜索，必要时重新审批。能确认尚未发送业务调用的失败标为无业务效果。已发送后丢失回复保留效果未知的回执，禁止自动重发。[凭据与目标](../go-harness/internal/mcp/credentials.go) · [工具发出与回执](../go-harness/internal/engine/eino/tools.go) · [租约失效后的调用](../go-harness/internal/mcp/lease.go)
- **后台与断点。** 后台任务继承提交时明确授予的范围。已绑定的目标按原代次和实际 schema 核对。客户端掉线或代次失效进入现有等待授权流程。宿主重启按原有 `restore_on_restart`、精确工作区、配置/schema 校验和版本比较更新授权。v2 只为确有执行恢复需求的目标重建连接，同一 recipe 中其余按需服务继续休眠。查看聊天记录只读历史。已完成的调用直接使用现有回执，尚无目标授权的任务等待授权。升级前已在途的任务按原契约结束或恢复。[宿主重建](../go-harness/internal/mcp/host_recipes.go) · [持久任务资源](../go-harness/background_resources.go)

## 交付顺序与验收

1. **工具声明。** 在根代理、前台和后台子代理接入角色级客户端搜索。用模拟模型记录每次实际请求的工具定义，核对命中后的原名调用、拒绝、凭据作用域、图片结果和回执。[根代理装配](../go-harness/internal/engine/eino/prepared.go) · [子代理装配](../go-harness/internal/engine/eino/roles.go)
2. **零启动聊天和单服务冷发现。** 接入宿主策略、目录存储、进程管理和同一轮动态注册。用真实 stdio 测试子进程记录 PID、`initialize`、`tools/list`、`tools/call` 次数。分别用桌面新建条目和原格式旧条目跑通从普通聊天到首次工具调用的端到端路径。
3. **回收、恢复和发布。** 完成宿主空闲回收、热替换、授权撤销、关停、版本化检查点和后台重建。通过下表后一次发布，桌面对新旧服务统一启用内部默认值。

| 验收场景 | 可观察的通过条件 |
| --- | --- |
| 桌面设置与升级 | 在现有表单保存新服务，并载入一份升级前的桌面配置。表单只显示现有连接字段。旧文件无需重存或迁移，两条路径都自动获得工具发现和按需启动。桌面配置和 ACP 消息均只含原有连接字段。 |
| 普通聊天、全冷或热摘要 | 在桌面配置 8 个 stdio 服务。新建会话、加载普通历史和泛搜索的启动、握手、远端列举次数均为 0。 |
| 第一次查询数据库、目录全冷 | 同一条用户消息内只有数据库产生 1 个 PID，首次握手与发现各 1 次（单页 fixture），下一模型步骤只加入允许的命中工具。真实业务调用和业务回执各 1 次。 |
| 重复使用、并发和释放 | 20 个并发请求只产生 1 个进程与 1 次握手。取消一个等待者，其余仍完成。桌面服务跨空闲期再次使用时保持原 PID，释放会话后进程退出。对设有正数空闲时限的宿主 recipe，用可控时钟验证回收、长调用和审批占用，回收后再次使用须取得新代次。 |
| 变更、故障和退出 | 覆盖程序缺失、慢握手、服务同路径替换、凭据轮换、列表变更、撤权及关停与启动竞合。上一个定义没有迟到发布，健康服务可用，宿主退出后无遗留直接子进程。已产生外部效果后退出的 fixture 只产生一次效果。 |
| 角色和恢复 | 同名服务器跨工作区、同工作区独立会话、共享 recipe、受限子代理、v1 旧断点、v2 审批暂停/恢复和后台重建分别验证。目录和工具声明不泄露无权服务，恢复时的真实工具、schema、授权和回执一致。 |
| 性能与模型效果 | 对全量声明与按需模式使用同组无工具、单工具、多工具及中英文任务，覆盖 0、10、11、128 个 MCP 工具。记录首轮声明字节、整项任务输入与缓存 token、搜索命中率、模型往返数、首个业务工具及任务总延迟 P50/P95，同时统计冷/热启动 PID 数。用基线结果校准工具数量阈值，并单独测量宿主 recipe 启用空闲回收后的重启开销。 |

设计阶段已运行当前版本的 5 项基线测试，覆盖 Eino 搜索后暴露命中项、桌面慢发现、桌面进程断开后的隔离、严格绑定替换失败时回滚，以及清理超时后的所有权。以上表格用于验收新增功能，真实模型的节省量须在实现后测量。[Eino 测试](../go-harness/third_party/eino/adk/middlewares/dynamictool/toolsearch/toolsearch_test.go) · [MCP 管理器测试](../go-harness/internal/mcp/manager_test.go)

## 实施验证

| 场景 | 2026-11-27 实测结果 |
| --- | --- |
| 从原桌面安装升级 | 更新器先确认运行与队列空闲，备份程序和用户数据，再原位更新、重启。后端报告新构建号，使用原数据库目录。升级前后的 MCP 配置及主配置 SHA-256 一致。原有 1 项 MCP 服务继续登记，桌面空闲时该服务的子进程数为 0。 |
| 最终安装包端到端 | 在隔离配置里通过桌面设置接口保存两个 stdio 服务。普通聊天启动 0 个进程。首次定向使用其中一个时，同一条用户消息内拿到原名工具的结果，另一服务保持休眠。审批期间移除目标服务，旧调用没有产生业务效果。[桌面测试](../go-harness/cmd/deerflow-desktop-smoke/mcp_hot.go) |
| 并发、目录与恢复 | 绑定 8 个已保存的本地服务并读取目录，启动数为 0。同一目标的 20 个并发请求共用 1 个进程、1 次握手和 1 次列举，另一个等待者取消不影响它们。跨会话读取磁盘摘要和读取损坏摘要都保持零额外启动。宿主恢复测试只启动检查点需要的服务。[进程与恢复测试](../go-harness/internal/mcp/on_demand_test.go) |
| 模型工具定义 | 用本地 MCP 服务和脚本模型检查每一步模型实际收到的工具名。目录完整时，10 个业务工具直接声明。11 和 128 个时，首步业务工具定义均为 0，精确搜索后只声明命中的 1 个工具，业务审批和执行各 1 次。真实供应商上的整轮 token 消耗、搜索命中率与 P50/P95 延迟仍待测量。[模型测试](../go-harness/internal/engine/eino/mcp_discovery_test.go) |
