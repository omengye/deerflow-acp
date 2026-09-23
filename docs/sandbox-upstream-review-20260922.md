# 沙箱能力专项适配评估

核查日期：2026-09-22。接续 [最近两天上游评估](D:/Tools/deerflow-api/docs/upstream-review-20260922.md)，重点核对当前 WSL、Local 和 Docker AIO 的实际实现，另补查上周相关沙箱提交。上游快照仍为 `ef21855b8dc43f88fbe5eb40316058457531d27e`，没有把这次专项检查描述为新的正式版本发布。

本次只有审查、临时合成数据/替身复现和文档，无运行代码或配置变更。建议独立实现，不直接合并上游代码。

## 结论与实施顺序

当前配置是 WSL。最近沙箱提交以正确性、超时和生命周期修复为主，没有必要为追随上游引入新的 provider。建议分两批：

1. **当前 WSL/桌面直接受益**：可靠的命令结果与验收、统一超时和取消控制、搜索完整性及错误语义。
2. **启用 AIO 前补齐**：失败命令不重放、执行占用保护、可靠回收与异常后的资源核对。

另有一项适合后续独立增强：有界读取、大文件续读和搜索覆盖元信息。它不是本周上游已经实现的功能，需按本项目接口设计。

## 1. 命令结果需要保留机器可判定的终态

参考上游 [#5634](https://github.com/bytedance/deer-flow/pull/5634) 对正常完成、硬超时、传输超时和结果未知的区分。上游采用 AIO HTTP SDK，本地采用 `docker exec` 与 `wsl.exe`，能借鉴语义，不能复制 SDK 参数。

本地 [Sandbox.execute_command](D:/Tools/deerflow-api/deerflow/sandbox/sandbox.py:21) 只返回字符串。[WSL](D:/Tools/deerflow-api/deerflow/sandbox/local/wsl_sandbox.py:184) 把超时写成 `Exit Code: -1` 文本，[AIO](D:/Tools/deerflow-api/deerflow/sandbox/aio.py:70) 把非零退出码追加在文本末尾。工具返回字符串后，调用记录缺少稳定的 `exit_code/timed_out/termination_confirmed` 字段。

[子任务验收](D:/Tools/deerflow-api/deerflow/subagents/acceptance_checks.py:325) 依据 ToolMessage.status 和 `Error:` 前缀判断是否成功，再从正文匹配测试通过字样。因此“先有 passing summary、随后命令失败或超时”需要特别修复，不能把部分 stdout 当成功终态。

已用实际工具包装及验收函数复现：替身 WSL 返回超时或退出码 1、stdout 含 `1 passed` 时，生成的 ToolMessage 仍是 `status="success"`，`tests_passed:pytest` 判定 `holds=True`；AIO 退出码 1 同样复现。这里使用的是合成结果，并没有运行真实 pytest/WSL 命令。这项应排在当前配置的第一优先级。

建议引入内部结构化命令结果，保留现有可读文本以兼容界面；失败、超时和未知结果不能生成成功验收回执。状态放在不会被正文截断丢失的结构化元数据中。验收应要求确认完成且退出码为 0，而非仅匹配通过字样。这是本项目独立补强，上游 #5634 没有直接修复本地验收链。

## 2. 统一超时配置，并把取消传给正在运行的命令

- [配置字段](D:/Tools/deerflow-api/deerflow/config/sandbox_config.py:117) 当前明确只承诺 LocalSandbox 的命令超时。
- Local provider 已转发该配置，Windows Local 还有进程树终止与有界输出捕获，见 [local_sandbox_provider.py](D:/Tools/deerflow-api/deerflow/sandbox/local/local_sandbox_provider.py:145)、[local_sandbox.py](D:/Tools/deerflow-api/deerflow/sandbox/local/local_sandbox.py:430)。不能说本项目完全没有超时控制。
- [WSL 执行](D:/Tools/deerflow-api/deerflow/sandbox/local/wsl_sandbox.py:177) 使用固定 600 秒；AIO 实例默认也是 600 秒，[provider 创建实例](D:/Tools/deerflow-api/deerflow/sandbox/aio.py:671) 未传配置期限。
- [bash 工具](D:/Tools/deerflow-api/deerflow/sandbox/tools.py:1430) 是同步工具。取消其异步等待不自动终止工作线程；[run 取消](D:/Tools/deerflow-api/deerflow/runtime/runs/manager.py:136) 会先标记 interrupted，命令生命周期需要另行控制。

建议把配置契约扩展到三类 provider；增加按命令持有的进程/执行句柄，将取消信号传到实际执行端。区分“停止等待”“确认命令已终止”“终态未知”，并在终止或隔离完成前保护相关工作区和资源。避免为停止一个命令而终止整套 WSL 发行版或影响同容器的其他任务。

替身验证配置设为 7 秒，经真实 provider 构建后，WSL/AIO 的 subprocess 参数仍为 600 秒。另用真实 `bash_tool.ainvoke` 与替身 subprocess 验证：调用者已收到 CancelledError 后，工作线程仍会继续完成。该复现证明线程继续，不等于已经测出真实 Linux 进程树存活情况。

Docker CLI 请求超时只证明宿主停止等待，不能据此宣称容器内进程树已经退出。本次不启动 WSL/Docker 验证实际进程树，后续实施需要补对应集成验收。上游的 `hard_timeout` 依赖其 HTTP 服务与兼容镜像；直接升级镜像不会让本地 CLI 自动获得该协议。

## 3. AIO 的 shell fallback 不应重放已经执行的命令

[AioSandbox.execute_command](D:/Tools/deerflow-api/deerflow/sandbox/aio.py:90) 先用 bash 执行，返回 126 或 127 后就把整条命令换成 sh 再跑。126/127 也可能是用户命令执行部分操作后返回的状态，不能唯一证明 bash 不存在。比如先写入文件、再调用不存在的程序，可能重复前面的写入。

替身执行器分别返回 126 和 127，两种情况均记录到 bash 一次、sh 再一次，并且最终只返回第二次输出，确认控制流会重放整条命令。

建议在执行用户命令前独立探测/选择 shell，缓存能力；执行开始后不因这种退出码重放命令。结果未知也不能自动重试有副作用的命令。这个本地缺口与 #5634 的“不盲目重试未知结果”原则相关，不是上游同一段代码的移植。

## 4. AIO 增加执行占用保护与可靠回收

参考 [#5178](https://github.com/bytedance/deer-flow/pull/5178) 防止子 Agent 运行所需会话被淘汰、[#5562](https://github.com/bytedance/deer-flow/pull/5562) 区分资源发现与生命周期变更、[#5498](https://github.com/bytedance/deer-flow/pull/5498) 在取消下完成资源切换收尾。

本地没有上游 `MAX_SHELL_SESSIONS`、E2B warm pool 或异步 lease manager；这些补丁不直接适用。但 [AIO 容量淘汰](D:/Tools/deerflow-api/deerflow/sandbox/aio.py:673) 和 [空闲回收](D:/Tools/deerflow-api/deerflow/sandbox/aio.py:681) 只看记录顺序/last_used，没有执行占用计数。命令运行期间没有持续保护，其他会话申请资源可能淘汰仍在工作的容器。

建议为实际运行和文件操作增加占用计数或租约，只回收确认空闲的资源；满额且全部忙时排队或返回明确容量错误。回收失败保留可重试记录，创建超时后核对资源是否实际存在；旧实例的迟到清理不能删除同名的新实例。维护资源状态比直接搬上游 warm pool 更适合当前规模。

真实 provider 生命周期配合 fake Docker，禁止所有外部进程调用后，确认了以下控制流：

- 容量为 1，A 的命令仍阻塞时申请 B，A 被移除；空闲期限设为 10 秒，模拟 A 执行 11 秒后申请 B，也会移除 A。
- 模拟删除 A 失败，内存记录仍被移除且随后创建 B，出现配置容量 1、模拟实际存活 2、A 已失去追踪。
- 模拟容器被外部删除，再次申请仍复用原对象，没有存活检查。
- provider 冷启动并发可产生两个实例；若整理生命周期，应补 [单例初始化](D:/Tools/deerflow-api/deerflow/sandbox/sandbox_provider.py:103) 的同步保护。

空闲检查只在 acquire 时触发，当前属于机会式回收。可明确记录这个语义，或增加只清理无活跃占用资源的维护任务，不要把检查本身变成续命操作。

此问题针对 AIO。WSL provider 的缓存项是 Python 对象，缓存淘汰不等同于直接销毁 WSL 进程；不应把 AIO 风险扩展为当前 WSL 会话已经会被强制回收。

## 5. 搜索和文件错误采用统一契约

直接适配 [#5534](https://github.com/bytedance/deer-flow/pull/5534)：Local/WSL glob、grep 及 AIO grep 在结果恰好等于上限时误报截断。应额外观察一条符合条件的结果后判断，已在上一轮用临时目录复现。

同时借鉴 [#5677](https://github.com/bytedance/deer-flow/pull/5677)，为本项目三类 provider 建立共享离线验收。该 PR 是测试基础设施，不是新生产功能。本地已有零散覆盖，值得统一以下可观察行为：

- 空结果、根目录不存在、权限不足分别表达；不能把执行失败显示成空目录或文件不存在。
- 非法正则采用统一错误类型。目前 Local 是 `re.error`，AIO 映射为 `ValueError`，工具因而给出不同错误提示。
- 逐文件读取失败不能悄悄表示“全量未命中”。目前 [Local grep](D:/Tools/deerflow-api/deerflow/sandbox/search.py:208) 会跳过 OSError，AIO 则会报告失败，应明确部分扫描策略。
- 特殊字符路径、单文件 grep、忽略目录、大小写、行号、达到上限与超过上限均跨 provider 验收。
- Windows 路径与中文输出应在 Windows 执行，不能沿用上游整套测试跳过 Windows 的做法。

## 6. 后续独立增强：有界读取、续读和覆盖信息

这些能力由本次对照发现本地需求，**不是本周上游的新功能**：

- **provider 内部限量读取**：现在 read_file/ls 大多先完整读取或收集，再在工具层裁剪；Local 无行范围时调用 [f.read](D:/Tools/deerflow-api/deerflow/sandbox/local/local_sandbox.py:606)，AIO 用 cat 并完整 capture stdout。把字节/条目上限下传 provider，才能控制实际内存和传输开销。
- **超长单行也能续读**：行范围、完整行截断和下一行提示已实现，不重做。目前 [超长行提示](D:/Tools/deerflow-api/deerflow/sandbox/tools.py:1396) 要求借助命令工具或提高限额；可新增只读字符/字节窗口，让禁用 bash 的 Agent 也能读取大 JSON、压缩文本。需处理 UTF-8 边界和文件变更。
- **报告搜索覆盖**：当前 grep 会跳过超过 1 MB 的文件、二进制文件和约 2000 字符以上的行，[search.py](D:/Tools/deerflow-api/deerflow/sandbox/search.py:169)。结果可附扫描文件数、跳过原因与读取错误，避免把“未搜索到”误解成“所有内容均检查过”。
- **稳定分页**：若经常碰到结果上限，再增加 `next_cursor`；先定义稳定排序和文件变动处理，不能仅在未排序的 os.walk 上添加 offset。已有 max_results 和硬上限应保留。

## 已具备或无需搬入

| 上游改动 | 本地结论 |
| --- | --- |
| [#5612](https://github.com/bytedance/deer-flow/pull/5612)、[#5676](https://github.com/bytedance/deer-flow/pull/5676) 忽略目录/列表预算 | 本地与 AIO 在收集和递归前过滤，已验证；无需重复改造。 |
| [#5422](https://github.com/bytedance/deer-flow/pull/5422) 不完整目录结果报错 | AIO 先检查返回码再解析输出，遍历成功才打印列表；mock 的“部分 stdout + 非零码”也抛错，已有核心能力。 |
| [#5559](https://github.com/bytedance/deer-flow/pull/5559) 空文件验收 | 本地用 os.lstat/文件模式，没有远端 stat 文本标签问题；已验证。 |
| [#5546](https://github.com/bytedance/deer-flow/pull/5546) 隐式 shell 探测加 subshell | 本地每条 WSL/docker exec 新建进程，没有同型持久 shell。 |
| [#5446](https://github.com/bytedance/deer-flow/pull/5446) Docker Desktop 默认 loopback | 本地 [docker run](D:/Tools/deerflow-api/deerflow/sandbox/aio.py:602) 不用 -p 发布控制 API，不需要引入上游绑定/端口探测结构。 |
| [#5001](https://github.com/bytedance/deer-flow/pull/5001) local bash probes | 此提交沙箱相关部分是测试改进；本地已有 Windows 进程树终止、超时和编码测试，按本地执行器补用例即可。 |
| 精确行范围循环检测、整行截断、已被后续操作替代的 write_file 正文裁减 | 本地已经实现，不重复列为新增能力。 |

无需因上游已有 E2B、BoxLite、OpenSandbox、Apple Container 等实现，就给本项目全量增加这些 provider。远程弹性、macOS 原生容器或浏览器隔离属于不同部署需求，应在有需求时单独选型。

## 证据与验证范围

上游详情在 [审计目录](D:/Tools/deerflow-api/.tmp/upstream-review-20260922)；本轮额外取得 #5498、#5178、#5446、#5422、#5001 的完整提交详情。检索/空文件等既有复现见 [audit_sandbox_and_think_repro.output.json](D:/Tools/deerflow-api/.tmp/upstream-review-20260922/audit_sandbox_and_think_repro.output.json)。

命令状态、验收、超时配置、取消与 shell 重放：[复现脚本](D:/Tools/deerflow-api/.tmp/upstream-review-20260922/audit_command_lifecycle_repro.py)、[结果](D:/Tools/deerflow-api/.tmp/upstream-review-20260922/audit_command_lifecycle_repro.output.json)。AIO 容量、空闲回收、失败删除、失效复用及单例并发：[复现脚本](D:/Tools/deerflow-api/.tmp/upstream-review-20260922/sandbox_boundary_repros.py)、[结果](D:/Tools/deerflow-api/.tmp/upstream-review-20260922/sandbox_boundary_repros.json)。

本轮未执行真实 WSL/Docker 命令、未触碰正在运行的沙箱、未运行全量测试。涉及进程终止、容器生命周期的 mock 证明代码分支与控制流，不替代实施后的平台集成验收。
