# 字节 DeerFlow 最近两天更新适配评估

核查日期：2026-09-22。此次接续 [09-20 评估](D:/Tools/deerflow-api/docs/upstream-review-20260920.md)，只评估之后的新提交，不重复推荐 [上一批已经实施的能力](D:/Tools/deerflow-api/docs/upstream-implementation-20260920.md)。

- 上次上游快照：`03505ac4e0b916351dba92e4d42942514b01f9a7`，北京时间 09-20 11:40:31。
- 本次上游快照：[`ef21855b8dc43f88fbe5eb40316058457531d27e`](https://github.com/bytedance/deer-flow/commit/ef21855b8dc43f88fbe5eb40316058457531d27e)，北京时间 09-22 17:45:01。
- 区间新增 **50 条 main 提交**。取得全部 50 条提交的详情和 diff，对相关候选核对本地源码，并对关键问题做隔离复现。
- 本地基线：`220f6490`。本次只新增评估文档和忽略目录内的审计材料，未修改运行代码。
- GitHub Releases 查询仍只有 `v2.0.0`，发布时间 2026-06-25。本报告不是新正式版发布说明。

本项目不是上游 fork。以下建议以触发条件、预期行为和回归用例为依据独立实现，不 merge、不整包 cherry-pick。

## 建议优先处理的修复

### 1. 删除上传文件时可能误删别人的 Markdown

上游 [#5673](https://github.com/bytedance/deer-flow/pull/5673)，明确适用，优先级高。

[delete_file_safe](D:/Tools/deerflow-api/deerflow/uploads/manager.py:240) 删除 `report.pdf` 后，仍在第 272 行猜测并删除 `report.md`。但 [转换输出](D:/Tools/deerflow-api/deerflow/utils/file_conversion.py:166) 会遇到重名自动编号，所以该 Markdown 不一定属于当前文件。

隔离复现：先存在 `report.docx` 及其 `report.md`，再存在 `report.pdf` 及其 `report_1.md`；删除 PDF 后，DOCX 的 Markdown 消失，PDF 自己的 Markdown 却留下。也可能误删用户直接上传的同名 Markdown。

建议先取消未经来源证明的配套删除，让 Markdown 可单独删除。若需要自动清理，先持久化真实来源映射，并验证对应关系。09-20 修复的是删除链接时误删目标；这次是另一种已经复现的误删。

### 2. 上传转换应读取稳定快照

上游 [#5611](https://github.com/bytedance/deer-flow/pull/5611)，明确存在同类竞态，建议与上传删除一起处理。

[upload_files](D:/Tools/deerflow-api/deerflow/client.py:1933) 将上传复制到公开的 uploads 目录后，把目标路径交给转换器重新打开。即使复制采用独占创建，发布之后的目标仍可能被另一个有目录写权限的执行者替换。

隔离复现保留源文件内容为 `SYNTHETIC ORIGINAL UPLOAD`，在复制后将目标改为 `SYNTHETIC REPLACEMENT`；实际 SDK 上传流程调用的替身转换器读取到后者，且上传返回成功。这证明输入绑定不稳定；本次没有执行符号链接攻击或读取任何真实外部文件。

建议转换私有快照或持有的文件内容，再通过现有独占写入发布转换结果。HTTP 上传本来就有私有临时源，可复用；SDK 也应明确稳定输入边界。不要直接搬上游 Gateway 的整套上传事务结构。

### 3. 输出达到长度上限时，TODO 不应再次拉起模型

上游 [#5569](https://github.com/bytedance/deer-flow/pull/5569)，部分适用，优先级高。

本地 [finish_reason_middleware](D:/Tools/deerflow-api/deerflow/agents/middlewares/finish_reason_middleware.py:60) 已保留正文、清空被截断的工具调用并添加可见说明。但是 [TodoMiddleware.after_model](D:/Tools/deerflow-api/deerflow/agents/middlewares/todo_middleware.py:119) 不检查 `model_length_termination`，仍可返回 `jump_to=model`，在未完成任务存在时再催促最多两次。

内存复现确认存在这种重新进入模型的行为。当前 API 配置启用 `plan_mode`，所以直接相关；纯 SDK 工厂默认关闭 plan mode 时不经过这条 TODO 路径。

建议让 TODO 尊重本轮长度终止标记。上游同时加入的 `write_file` 输出预算提示也值得借鉴，但必须取实际模型配置的有效预算，并避免修改共享工具实例。已有正文保留和可见提示无需重做。

### 4. 压缩上下文时移除旧 TODO 提醒

上游 [#5614](https://github.com/bytedance/deer-flow/pull/5614)，明确适用。

[同步压缩](D:/Tools/deerflow-api/deerflow/agents/middlewares/summarization_middleware.py:187) 与 [异步压缩](D:/Tools/deerflow-api/deerflow/agents/middlewares/summarization_middleware.py:215) 未过滤旧 `todo_reminder`。旧提醒进入保留尾部后，[TODO 注入判断](D:/Tools/deerflow-api/deerflow/agents/middlewares/todo_middleware.py:83) 会认为提醒已经存在，不再按最新任务状态刷新。

内存复现确认旧提醒阻止更新。建议在确定本轮要压缩后、分割摘要和保留消息前剔除可重建的旧提醒，再以当前 `todos` 生成新提醒。同步、异步及摘要钩子的语义应一致，可与上一项一起实施。

### 5. Windows SVG MIME 别名漏过强制下载

上游 [#5594](https://github.com/bytedance/deer-flow/pull/5594)，小范围修复，建议本轮纳入。

[产物响应](D:/Tools/deerflow-api/app/routers/uploads.py:207) 已对活动文档强制 `attachment`，但 [MIME 分类集合](D:/Tools/deerflow-api/app/routers/uploads.py:26) 包含 `image/svg+xml`，未包含 Windows 可能返回的 `image/svg`。类型来自 [mimetypes.guess_type](D:/Tools/deerflow-api/deerflow/client.py:2026)。

实际分类函数对 `image/svg` 返回 False，对标准类型返回 True。本机当前返回标准类型，因此不是本机默认路径已经触发的攻击；换到 MIME 注册不同的 Windows 电脑时可能暴露缺口，与桌面 ZIP 的跨机器使用相关。

建议补充别名，并验证这两种类型均返回下载头。本次未做浏览器脚本执行验证，不扩大宣称攻击已复现。

### 6. 搜索结果恰好达到上限时误报截断

上游 [#5534](https://github.com/bytedance/deer-flow/pull/5534)，明确适用。

本地 [glob](D:/Tools/deerflow-api/deerflow/sandbox/search.py:126)、[grep](D:/Tools/deerflow-api/deerflow/sandbox/search.py:205) 及 [AIO grep](D:/Tools/deerflow-api/deerflow/sandbox/aio.py:429) 达到 `max_results` 即标记 `truncated=True`。当前配置使用 WSL，而 WSL 继承本地文件搜索实现，也受影响。

临时目录只有 2 条结果、上限也是 2 时，本地及 WSL glob/grep、AIO 嵌入 Python grep 均误报截断；AIO glob 已正确。错误信息会让模型以为还有遗漏，诱发不必要的重复搜索。

建议额外观察一条符合条件的结果后再判定截断，同时保留忽略规则和确定性排序。AIO 验证运行了原有嵌入脚本，用宿主 Python 替代 Docker 调用，没有验证实际容器。

### 7. MCP stdio 配置的 cwd 被静默忽略

上游 [#5643](https://github.com/bytedance/deer-flow/pull/5643)，明确适用，小范围功能修复。

[MCP 配置](D:/Tools/deerflow-api/deerflow/config/extensions_config.py:82) 允许额外字段，能接收 `cwd`，但 [build_server_params](D:/Tools/deerflow-api/deerflow/mcp/client.py:29) 只转发 `command/args/env`。纯函数复现配置 `cwd="D:/example-mcp"` 后，连接参数中没有 cwd；已安装的 adapter 支持它。

建议把 cwd 声明为显式字段，并在非空时传给 stdio transport。发现和连接池共用此配置，修复能覆盖两者。从其他目录、Windows 服务或桌面启动本地 MCP 时，依赖相对路径的入口才会按指定工作目录运行。保留当前缺省行为，不顺带复制上游自动切换线程 workspace 的规则。

### 8. 熔断器需要隔离不同代的请求结果

上游 [#5602](https://github.com/bytedance/deer-flow/pull/5602)，部分机制已有，剩余问题已复现。

[LLM 错误处理](D:/Tools/deerflow-api/deerflow/agents/middlewares/llm_error_handling_middleware.py:501) 已有 probe token，能防止旧请求释放别人的探测资格，但没有请求代际隔离。按确定顺序模拟新旧请求交错时，新探测成功使熔断器恢复 closed 后，上一代请求迟到的失败会再次错误打开熔断器；没有调用真实模型并发请求。

建议在请求获准进入时记录 generation，结算时只允许当前代更新状态。保留本地已有的 token、重试预算及空回复恢复机制。收益是在模型服务恢复后避免旧失败造成多余停顿。

## 条件适配及新增功能

| 上游更新 | 本地结论与建议 |
| --- | --- |
| [#4919 无人值守提示词与工具策略一致](https://github.com/bytedance/deer-flow/pull/4919) | 适合本项目定时任务。[调度入口](D:/Tools/deerflow-api/deerflow/runtime/scheduler.py:1298) 只有 `scheduled_task` 标记，没有统一交互策略；[主提示词](D:/Tools/deerflow-api/deerflow/agents/lead_agent/prompt.py:386) 仍要求有歧义先提问，澄清工具和中断中间件也启用。建议可信调度入口同时决定提示词、工具和中间件行为，让低风险可逆任务采用合理假设继续，缺少必要授权的高风险动作报告阻塞。不能只隐藏按钮或全局关闭确认。本次为源码对照，未运行真实定时任务。 |
| [#5664 同步 checkpoint 变更抵御取消](https://github.com/bytedance/deer-flow/pull/5664) | [goal.py:564](D:/Tools/deerflow-api/deerflow/runtime/goal.py:564) 的同步 fallback 直接 `await asyncio.to_thread`。复现取消后调用与线程锁先退出，后台 put 后完成。适合补强自定义同步/SDK checkpointer；当前 API 和 ACP 的 AsyncSqlite 路径不能据此认定有同型缺陷。同型现象也在 [HTTP 删除会话](D:/Tools/deerflow-api/app/routers/threads.py:158) 的模拟删除复现，建议针对写入、删除等待后台操作收尾后传播取消，不把所有读取都改成不可取消。 |
| [#5622 provider close 完成后再传播取消](https://github.com/bytedance/deer-flow/pull/5622) | [checkpoint cache provider](D:/Tools/deerflow-api/deerflow/runtime/checkpoint_cache/provider.py:65) 和 [stream bridge provider](D:/Tools/deerflow-api/deerflow/runtime/stream_bridge/async_provider.py:56) 裸关闭可被取消中断。主 API shutdown 已整体 `await_drained`，默认 memory 关闭无异步等待点，主要是 Redis/独立 provider 使用的边界补强，优先级低于前述修复。 |
| [#5579 自定义 Agent 默认知识库范围](https://github.com/bytedance/deer-flow/pull/5579) | 上一批已实现每消息 scope；[AgentConfig](D:/Tools/deerflow-api/deerflow/config/agents_config.py:80) 仍无默认 scope。新增后可把不同 Agent 绑定到各自默认数据集/文档。新消息未传 scope 时继承，显式选择优先；恢复/重试沿用原请求快照，并继续与操作员允许范围取交集。当前配置未启用 knowledge_search，适合知识库需求启动时做。 |
| [#5564 DeerMem 范围隔离评测](https://github.com/bytedance/deer-flow/pull/5564) | 属于验证能力，不是新的记忆后端。适合借鉴跨工作区、用户或 Agent 范围的对照用例，按本地 ACP/记忆后端定义隔离边界；不承诺上游数据集、阈值可以直接沿用。 |
| [#5648 Python AST 凭据扫描](https://github.com/bytedance/deer-flow/pull/5648) | 本地 [security_scanner](D:/Tools/deerflow-api/deerflow/skills/security_scanner.py:37) 没有上游那条凭据赋值正则，因此没有同源误报。可新增只检查字面量赋值的静态检测，避免把类型注解、环境变量读取当硬编码密钥；属于 skill 安装/演进审核增强，不是待修现有误报。 |
| [#5634 AIO 命令超时](https://github.com/bytedance/deer-flow/pull/5634) | 本地 AIO 使用 Docker CLI，已有宿主超时，没有上游 AIO HTTP SDK/会话代际。停止宿主 CLI 不等于已经证明容器命令停止。若以后部署 AIO，应专项设计命令内部期限与宿主期限；当前 WSL 配置下不优先搬这套机制。 |
| [#5540 保留代码中的字面 think 标签](https://github.com/bytedance/deer-flow/pull/5540) | 上游修的是前端，本地桌面端没有同型剥离。相邻问题在 [MiniMax adapter](D:/Tools/deerflow-api/deerflow/models/patched_minimax.py:52)：正则会移除行内和围栏代码里的 `<think>literal</think>`，已用合成字符串复现。仅该适配器非流式结果适用，当前配置未使用；若启用，应按推理块的语义边界解析，不能直接搬前端补丁。 |

## 已覆盖或不适合直接迁入的内容

- **[#5578 上传独占写入](https://github.com/bytedance/deer-flow/pull/5578)**：本地 [copy_file_exclusive](D:/Tools/deerflow-api/deerflow/uploads/manager.py:141) 使用 `xb`，Markdown 使用 `x`，遇到已有目的项或链接会自动编号，不覆盖、不写穿。保留现有行为；它不消除前述发布后的转换竞态。
- **[#5630 SDK MCP 选择继承](https://github.com/bytedance/deer-flow/pull/5630)**：上次已实现 Agent 选择、图缓存键、私有工具过滤及子 Agent 透传，不重复建设。额外观察到 SDK `reset_agent()` 只重建图、不重读已保存 Agent 配置；可作为独立生命周期小修，但没有证实管理 API 有同样问题，不能称为主链路选择继承失效。
- **[#5612](https://github.com/bytedance/deer-flow/pull/5612)、[#5676](https://github.com/bytedance/deer-flow/pull/5676) 忽略目录与列表预算**：本地与 AIO 在收集/递归前按名称过滤，临时目录验证通过；没有上游 `find | head` 提前占满预算的问题。
- **[#5559 空文件验收](https://github.com/bytedance/deer-flow/pull/5559)**：本地使用 `os.lstat` 和文件模式，而非 GNU stat 文本标签。空文件 exists 通过、non-empty 失败、有成功写入回执时 file_written 通过，符合预期。
- **[#5546 远程探测 subshell](https://github.com/bytedance/deer-flow/pull/5546)**：本地 AIO/WSL 每次创建独立命令进程，没有同型持久隐式 shell。
- **[#5651 外部 system-role 注入](https://github.com/bytedance/deer-flow/pull/5651)**：本地普通聊天接收字符串；[AG-UI](D:/Tools/deerflow-api/app/routers/chat.py:195) 只提取 [最后一条 user 消息](D:/Tools/deerflow-api/app/routers/chat.py:410)，再交由 client 构造 HumanMessage，不把任意传入消息列表或 state 写入图。当前没有发现上游这一原样注入入口；此结论不等于消除普通文本提示注入。
- **[#5593](https://github.com/bytedance/deer-flow/pull/5593)、[#5669](https://github.com/bytedance/deer-flow/pull/5669) skill allowed-tools 空值语义**：本项目尚未实现对应的 per-skill 工具权限模型或 assembly descriptor，不应描述成现有同路径故障。它与已经支持的 Agent `skills=[]` 选择语义不同，未来引入需要完整设计。
- **[#5596 设置页管理共享模型](https://github.com/bytedance/deer-flow/pull/5596)**：本地已有模型管理、密钥遮罩和配置保存机制，不重做一套上游 Web 页面。
- **[#5555 memory backend_config 报错](https://github.com/bytedance/deer-flow/pull/5555)**：本地 mem0 采用 Pydantic 按字段报告校验错误，不存在上游无字段名的同型 TypeError；也不应为追随上游而改变当前 null 值校验契约。
- **[#5647 插件 API/书签](https://github.com/bytedance/deer-flow/pull/5647)、[#5582 微信扫码登录](https://github.com/bytedance/deer-flow/pull/5582)**：属于新产品能力，依赖其插件/渠道管理结构。本轮没有对应明确需求，优先级低于已确认问题。微信需求应按本项目接入链路另行设计。
- PostgreSQL schema/引擎关闭、E2B warm pool、Live Browser WebSocket 权限、统一 thread change-position 等修复，依赖本项目没有的同型组件，不因提交标题相似而迁入。
- **[#5688 llama.cpp 配置文档](https://github.com/bytedance/deer-flow/pull/5688)**：可作为本地模型配置参考，不代表必须新增模型适配器。

## 验证与下一批范围

建议下一批先做八项：上传误删、稳定转换输入、长度终止与 TODO 协同、压缩后 TODO 刷新、SVG 别名、搜索完整性、MCP cwd、熔断代际。按故障行为编写针对性回归，不引入上游特有的服务和目录结构。

本次只运行隔离复现与静态对照，没有运行全量测试、连接实际模型/MCP、执行 Docker 集成或浏览器利用。复现仅使用临时/合成文件、内存状态和替身，不修改会话数据或配置。

原始提交列表、50 份详情、版本查询和审计材料保存在忽略目录 [`.tmp/upstream-review-20260922`](D:/Tools/deerflow-api/.tmp/upstream-review-20260922)。上传/MIME 复现入口为 [audit_upload_boundaries.py](D:/Tools/deerflow-api/.tmp/upstream-review-20260922/audit_upload_boundaries.py)，输出为 [audit_upload_boundaries_results.json](D:/Tools/deerflow-api/.tmp/upstream-review-20260922/audit_upload_boundaries_results.json)。

TODO、熔断、同步 checkpoint 和取消收尾复现入口为 [local_boundary_repros.py](D:/Tools/deerflow-api/.tmp/upstream-review-20260922/local_boundary_repros.py)，[结果文件](D:/Tools/deerflow-api/.tmp/upstream-review-20260922/local_boundary_repros.json) 同时记录了默认配置、替身、压缩边界固定等验证限制。HTTP 删除复现是主动取消异步任务与模拟 manager，不代表已经验证真实断开 HTTP 会触发相同取消。

搜索、目录忽略、空文件验收和 MiniMax 字面标签的复现入口为 [audit_sandbox_and_think_repro.py](D:/Tools/deerflow-api/.tmp/upstream-review-20260922/audit_sandbox_and_think_repro.py)，结果为 [audit_sandbox_and_think_repro.output.json](D:/Tools/deerflow-api/.tmp/upstream-review-20260922/audit_sandbox_and_think_repro.output.json)。
