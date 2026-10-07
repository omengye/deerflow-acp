# Eino harness 三轮迭代范围与验收

起点：`6efe7f1b`。用户要求实施前三轮，提前固定范围，不持续扩大测试。

## 第一轮：长任务可靠性

- 统一模型预算与摘要压缩的 token 估算，考虑中文、工具 schema 和图片。
- 已配置模型窗口时，压缩阈值按窗口和输出预留收紧；主角色、子角色和后台执行使用同一策略。调用前拒绝明显超限输入，不花费一次无效请求。
- 中文记忆使用有界 n-gram 召回；自动注入与显式查询批量读取当前事实，移除 10,000 条事实导致运行失败的限制，保持范围、版本和快照一致性。
- 不引入 embedding 服务，不更换数据库，不修改审批、回执与共享根预算的语义。

## 第二轮：兼容性与诊断

- 保留原文本连接测试，增加显式 Agent 能力测试：流式文本、工具调用与实际 usage；总超时和请求数固定，失败只展示脱敏分类，不自动重试或自动更改能力配置。
- 网页网络、HTTP 限流与权限失败采用类型化错误，保留取消和不确定效果边界。
- 基于现有持久事件、执行状态和预算提供有界会话诊断；Waku 后台加载并缓存展示等待/暂停/失败原因、用量及模型/工具耗时。没有原始提示词、工具参数或凭据。
- 不接外部遥测平台，不扩大 Provider，不加入自动模型 fallback。

## 第三轮：业务验收与交付

- 为两项现有常用技能提供对应的宿主行为 suite，仍由既有审查/评测入口运行，维持自动发布默认关闭。
- 增加排空并停服后的本地运行时数据备份与校验恢复流程，恢复到新目录，使用隔离目录演练，不接触个人工作数据库；不尝试在活跃资产回收期间复制数据。
- 一次真实默认模型能力验收；一次现有桌面协议 smoke（隔离 user-data，包含审批恢复、设置应用、产物/回执）；构建最终 Windows 桌面开发包并记录源码身份。
- 更新当前能力文档与手测清单。干净 Windows OS、原生 Linux 构建、远端 CI 只按实际可用环境记录，不以本机模拟结果替代。若环境不可用，明确保留未验收状态。

## 固定测试范围

1. 第一轮：Eino 的 `TestCompaction*`、`TestNativeCompaction*`、新增窗口/token 用例；memory 的 `TestSearch*` 与新增中文/大规模快照用例。
2. 第一轮边界回归仅选现有 `TestNativePermissionPauseResumeUsesOneToolAdmission`、`TestMiddlewareModelCallsUseOriginalDurableBudget` 和 `TestReceiptRecoveryAfterDatabaseReopen`。
3. 第二轮：新增能力测试的成功/错误/取消本地 HTTP fixture，新增网络错误分类及会话诊断；Waku 仅新增相关协议/状态测试。
4. 第三轮：两项 suite 的格式校验和已有行为评测集成一个用例；备份恢复一个隔离集成；一次桌面协议 smoke、一次真实模型能力测试。
5. 最后一次 Go 全模块 build、一次 Waku 编译/打包、`git diff --check`。不跑全量测试、不跑全量 race、不增加 Provider × 参数测试矩阵。

仅在新增改动、失败或明确边界风险出现时重跑受影响的选择器。追加检查须在此记录原因。每轮记录结果，三轮完成后统一完整桌面构建，避免重复大型构建。

## 执行记录

- 第一轮：压缩/原生摘要、权限恢复和共享预算选择器通过（Eino 7.192s、memory 7.126s）；新增窗口/中文与 10,001 条事实快照/回执重启选择器通过（Eino 6.356s、memory 1.447s、runtime 9.704s）。
- 第二轮：诊断有界/脱敏和网页错误分类通过；能力测试发现重复 Close，修复后只重跑该选择器。取消与不确定效果保持优先于普通故障分类。
- 第二轮新增 rate_limit/remote 分类会影响质量判断，因此同步排除这两种环境失败，并追加现有 derived-discovery 单个用例；不扩展到整个技能测试包。
- 第二轮收尾选择器通过：runtime 8.127s、Eino 6.171s、memory 8.739s、evolution 7.349s。中文批量 SQL 保留 Unicode 大小写匹配；排队诊断、取消、环境失败因果边界均在原定选择器内检查。
- 第三轮 suite 格式校验通过 5.986s，已有 Eino 行为工具循环隔离用例通过。离线备份/校验/新目录恢复通过 1.354s；校正数据库权限和跨磁盘目标处理后仅重跑备份用例 2.729s，同时计时范围修正后只重跑共享预算用例 6.507s。
- 一次 Go 全模块 `go build -p 2 ./...` 已通过。没有运行全量 Go 测试或 race。
- 网页响应体读取中断保持上下文取消错误；改动后仅重跑原网页分类用例，通过 1.738s。
- Rust 的最初 Cargo bin 入口没有包含 UI 单元测试（0 tests，不能计为通过）；定向状态文案移到无 GPUI 依赖模块，直接编译该实际源码测试，避免继续扩大 GUI 测试编译。
- 无界面 Rust 状态用例实际执行 1 条并通过；测试通过 `#[path]` 引用生产源码，使用已有 serde_json 和 thin LTO 缓存，没有复制被测逻辑。
- 收尾检查补齐已约定的全文索引批量读取，避免非中文索引路径仍逐条读取事实；只重跑 `TestSearchRebuildsIndexAndExcludesChangedHeads`，通过 2.376s。桌面缓存按管理接口实际 `session_id` 字段核对会话。
- 真实默认模型为根目录配置的 `deepseek-v4-flash`，仍使用旧 `langchain_deepseek:ChatDeepSeek` 标识。只在隔离验收适配中使用现有 openai 适配器，不修改个人配置。两次实际请求均为 `request_failed`（流式 1309ms、工具调用 345ms）；未报告 usage，外部能力验收未通过，不重试、不自动更换模型。
  只读确认使用的是已配置 `opencode.ai` 端点；没有请求其他默认端点。失败代码是脱敏结果，不能据此断言模型不支持这些能力。
- 最终 Windows Release 开发包构建通过（跨日完成于 2026-10-05）：Waku/daemon 15m43s、Bridge 5m05s，Go daemon/config/stdio/skill/state 工具及 Linux WSL helper 均成功构建。`BUILD_SOURCE.json` 标记 `development-dirty`，没有发行 ZIP，也没有将本地改动冒充 HEAD 的源码归档。
- 一次既有桌面协议 smoke 通过，清理错误为 0。覆盖 WebSocket/ACP 会话、模型切换、审批写文件及审批缓存清理、配置应用后会话恢复、内置技能读取、本地 MCP fixture 调用与释放、删除与服务停止语义。实际 GUI 未启动，不把协议结果当成视觉验收。
- 本轮不修改已有 `integrations/` 内容或个人运行数据库；`git diff --check` 通过。没有扩大到全量测试或 race，完成上述检查后停止验证。

## 本次交付

- 开发包：`dist/desktop/DeerFlow-Desktop-Go-20261004-Iterations1-3`，入口 `deerflow-desktop.exe`。
- 包内新增 `deerflow-state-go.exe`、两项 `resources/skill-behavior/*.json`、备份说明和本记录；附 `TEST_NOTES.zh-CN.md`、源码来源记录和 `PACKAGE_FILES.sha256`。
- 构建日志：`.build-cache/eino-three-iterations/desktop-package-build.log`。
- 协议记录：`.build-cache/eino-three-iterations/desktop-smoke/20261005-002448-ab046539-db23-421d-a1d1-e7761e6445a2/logs/report.json`。
- 外部模型记录：`.build-cache/eino-three-iterations/real-model-probe.json`，保留失败结果，不追加请求。

## 手测清单与边界

1. 使用最终开发包，模型设置中的“测试 Agent 能力”应分别显示流式、工具调用和 usage 状态；不自动改写能力开关。
2. 在“记忆与数据”的会话列表点击“查看诊断”，检查采样时间、排队/执行阶段、暂停/停止原因、根任务累计计数和最近模型/工具计时；再次点击刷新快照。
3. 中文查询应召回数据库/偏好等现有事实；小窗口模型对明显超限输入给出可处理错误，摘要保留当前用户轮次和工具配对。
4. 按随包 `go-runtime-backup.md` 在隔离目录备份与恢复，验证完成后再手动切换停服的数据目录。

这份清单供用户验收，不额外启动 GUI 测试。干净 Windows 无 Python 环境、原生 Linux 构建、远端 CI 和真实外部 MCP 服务没有在本轮执行；协议 smoke 只验证隔离的本地 MCP fixture。Token 是估算，provider usage 是结算依据。旧暂停任务的窗口/估算策略不兼容时应取消后新建任务，不能放宽恢复校验。
