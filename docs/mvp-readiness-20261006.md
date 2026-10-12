# Scope update (2026-10-11)

This dated review is retained as evidence, not the current feature contract.
Runtime backup/restore has been removed from MVP scope. Text attachment reads,
local readiness and confirmed review-only learning setup are tracked in
`mvp-closure-20261011.md`; old acceptance statements do not validate those changes.

# Windows 本地 Agent MVP 评估

2026-10-10 更新：独立 Ark 适配器已移除，当前模型请求统一使用流式传输；下文 Provider 范围保留评估当日状态，现行约定见[模型流式审计](model-streaming-audit-20261010.md)。

评估日期为 2026-10-06。范围是当前工作树的 Go/Eino harness、ACP Bridge、Waku 桌面和 Windows x64 便携交付，不以历史规划文件中的功能列表代替实现验证。

当前功能足以冻结为面向开发者和专业用户的本地单用户内测 MVP。其核心优势是任务执行、权限、持久状态与工具效果的完整闭环。本次全量回归仍有一个续跑 deadline 失败，因此质量门禁尚未通过，不建议标记为公开稳定发行。正式分发还须完成真实模型、首次配置和桌面操作验收。它目前提供 SDK、ACP 和认证本地 IPC，没有远程 HTTP 产品接口或多租户控制面；若 MVP 的目标是多人托管平台，现有交付范围还不够。

## 已形成的产品闭环

| 维度 | 实现依据 | MVP 判断 |
| --- | --- | --- |
| Agent 执行 | `go-harness/deerflow.go` 装配 SDK；`internal/engine/eino` 使用原生 DeepAgent、TurnLoop 和公开 middleware；OpenAI、Claude、Ark 适配 | 已具备可替换 Provider 的实际执行链，不只是模型调用封装 |
| 工作空间与权限 | `internal/tools/workspace.go` 使用 `os.Root`；`harness/permission_mode.go` 区分读操作与需要批准的操作；命令后端默认关闭 | 能完成读取、编辑与授权命令执行；local/PowerShell/WSL 权限仍属于当前 OS 用户，隔离要求应选择已有 Docker 后端 |
| 状态与恢复 | SQLite WAL/事务、会话 ownership、原生检查点、不可变执行协议；`internal/runtime/execution_service.go` 在恢复前核对来源和策略 | 会话恢复、等待审批、取消和重启路径具备明确语义；会话 resume 与工具执行 resume 分开处理 |
| 工具效果 | `internal/runtime/receipts.go` 先提交 started，再执行；未知效果需要人工核对；已确认失败保留失败回执与部分输出 | 这是专业 harness 的关键价值，避免把断线或重启当成安全重试依据 |
| 长任务和子代理 | 根预算持久化；队列、模型/工具调用限额、动态窗口压缩；前后台角色、持久等待、续跑和 MCP 资源授权 | 足够支撑有界编排；累计 token 上限默认关闭，需要成本硬限制时显式开启；估算不能保证准确账单 |
| 技能与记忆 | 不可变技能版本、候选审查/发布/回滚、隔离行为评测、中文记忆检索和作用域策略 | MVP 可优先交付人工管理技能与记忆。自动发现和自动补丁已有实现，但真实质量尚不足以作为默认承诺 |
| 用户入口与运维 | Waku 设置、任务/回执管理；配置工具、脱敏诊断、离线状态备份与新目录恢复工具 | 首次使用与日常运维入口完整；实际桌面可用性仍需用户验收 |

随包配置默认关闭命令、OpenCLI、子代理与付费技能生成。体验高级能力时需要显式启用对应能力，并配置 Provider；解压后没有 API Key 的首次启动不能直接完成线上任务。OpenCLI 还需要用户自己的 OpenCLI/Node.js 与浏览器桥接。WSL 和 Docker 都是可选后端，不作为普通聊天和文件工具的安装前提。

## 当前应优先处理的事项

1. **让当前源码和交付门禁保持一致。** 初次 `go test -count=1 -p=2 ./... -timeout=5m` 暴露三处过时断言、一次混合角色 FFB 超时及 SDK 根包整体五分钟超时。三处断言分别是续跑来源损坏的错误类型、误匹配记忆说明文字、命令确认失败应允许 Agent 继续。修正只涉及测试，不更改生产权限、恢复校验或失败回执。FFB 单用例重复三次通过，仍需全量复查，不能用单测结果消除并发时序风险。新打包入口将每个 Go 包的测试总时限设为 15 分钟，不改变生产超时，也不自动重试失败测试。
2. **完成真实使用验收。** 上一次记录的默认模型能力探测中，两次请求均失败，未获得 usage，见[三轮迭代记录](eino-three-iterations-20261004.md)。这只能说明当次端点验收未通过，不能据此断言模型不支持工具。此次不会自动使用个人密钥发起付费探测。发布前至少用实际选定模型完成一次“流式回答 → 工具调用 → 人工审批 → 产物 → 重启恢复”，再检查一个子代理任务。
3. **以两个业务任务判断价值。** 先用现有代码文档和资讯简报场景建立真实输入、产物正确性、耗时和成本的基线。当前 `resources/skill-behavior` 的 suite 能验证受限 fixture 中的行为，不能证明开放网络或任意仓库任务的质量。冻结自动进化默认关闭的策略，优先收集真实用户结果。
4. **完成干净 Windows 首次启动。** 本地 fixture 和协议 smoke 不检查 GUI、驱动、系统依赖或线上服务。发行前在新的 Windows x64 环境解压、配置模型、执行和备份恢复一次。当前便携 ZIP 无签名、无安装器，这两项可以在 MVP 后根据实际分发需要补充。

另有一处源码层面的明确缺口，应该在开启自动进化前处理。Eino 的 `toolFailureCode` 已返回 `rate_limit`、`remote`，技能质量查询也把它们作为环境失败排除，但 `internal/runtime/receipts.go` 的失败回执持久化白名单只接收 `network/permission/quota/cancelled/uncertain/unknown`。两种新分类会在写入回执时丢失，后续质量查询无法据此排除环境故障；当操作员同时标记质量失败时，可能形成错误的技能质量归因。该发现来自生产代码的生产者、持久层和消费者核对，未在本轮扩大到自动发布实跑或修改其生产语义。自动进化默认关闭，因此它不阻挡本文限定的 MVP，但启用该能力时应先补齐持久化及跨层回归。

Eino 当前固定为 `v0.10.0-alpha.35`，Claude 适配器有本地补丁。MVP 可以保持固定版本并记录补丁；升级时要重新验证检查点、权限、预算和工具失败语义。现阶段继续增加 Provider、远程调度或更多自动化会扩大验收成本，建议先完成上面的交付任务。

## Windows 交付实现

新增[统一打包脚本](../scripts/build-windows.ps1)与[Build Windows MVP 工作流](../.github/workflows/build-windows.yml)。已有桌面和 ACP 发布工作流调用同一个可复用工作流，Release 附件只在构建成功后上传。

正式打包依次检查干净源码、Go 依赖与回归、Rust Bridge 测试、源码身份 fixture，构建实际程序，再检查包内 Bridge/daemon；桌面还运行普通与旧 MCP 策略两种协议 smoke。随后写入来源、验证与文件哈希记录，最后生成二进制/源码 ZIP 和两份 ZIP 校验文件。任一测试失败都会停止打包；测试成功不等于线上模型或 GUI 验收成功。

`-Development` 支持当前未提交工作树的真实编译和协议验证，仅生成开发目录。`-SkipTests` 只允许和 `-Development` 同用，仍校验实际包内 Bridge/daemon，构建记录明确标记回归未运行。这不是正式发行的替代入口。

ACP assembler 同时修正绝对输出路径的解析，并像桌面一样给 Rust Bridge 链接静态 CRT，减少依赖 VC++ redistributable 的启动问题。对应源码 archive 固定同一提交；桌面包继续附 Waku GPL 与 DeerFlow 许可证。

当前工作树已有大量此前修改和未跟踪文件；新增的 state backup 程序、行为 suite 及其文档也是已有未提交功能。GitHub Actions 只会看到提交中的文件。应审查并提交本次及这些必要源码，再从干净 checkout 构建，避免遗漏引用文件。脚本不会自动提交、清理工作树或拷贝个人 `user-data`。

## 本次验证记录

- 三处过时断言修正后的定向测试通过；FFB 选择器重复三次通过。
- Windows 打包 fixture 的 30 项检查通过，覆盖脏源码拒绝、回归失败、Bridge 失败、包内协议失败、环境恢复、源码身份、ZIP 内构建记录和 SHA-256。
- 源码身份 fixture 修正其缺失的依赖输入后，28 项检查通过。
- 所有 PowerShell 构建脚本语法检查通过；五个 GitHub Actions 文件通过 actionlint 检查。
- `go test -count=1 -p=2 ./... -timeout=15m` 完整运行结束，仍失败。失败仅在 SDK 根包的 `TestSDKNotificationContinuationUsesOriginalBudgetAndIndependentAck`，其他带测试的 Go 包均通过。SDK 根包完整运行 637.474 秒，没有再次触发整包超时；该用例在其他验证仍并行执行时的三次重复中再次出现 deadline 失败。待其余任务结束后，再隔离运行三次均通过（总计 3.219 秒）。结果显示负载相关的稳定性问题，需要定位具体 deadline 来源；不能把最终定向成功当成全量通过，也没有放宽生产超时、跳过选择器或修改该用例断言。完整全量日志为 `.build-cache/windows-mvp-go-tests.log`。
- 实际 Windows ACP Release 开发目录构建成功，包内 `TestExistingRustBridge*` 测试通过（54.678 秒），16 个实际文件的 SHA-256 清单逐项核对通过；Bridge PE 依赖没有 VC++ redistributable 的 DLL。该构建明确使用 `-Development -SkipTests`，回归结果由上述独立全量运行记录，不把开发构建标记成正式通过。
- Rust Bridge 的 `cargo test --release --locked --config ../scripts/cargo-windows-static-crt.toml` 通过，共 18 项测试。
- 本次未推送源码、运行远端 GitHub Actions、发起真实模型付费请求或启动桌面 GUI。

本地 ACP 开发目录为 `dist/windows/mvp-acp-20261006-local/DeerFlow-ACP-Go`，其中 `BUILD_PACKAGE.json` 如实记录 `development-dirty` 和 `skipped_development`。本次没有生成正式二进制/源码 ZIP；新统一入口的正式归档路径会因未通过的回归停止。当前最小后续范围是排查续跑 deadline、验证实际模型与 Windows 首次配置，完成后再让已添加的工作流生成正式包。
