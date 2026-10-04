# 2026-10-04 审计修复与交付说明

## 修复范围

- MCP 正常返回 `IsError=true` 时，保存脱敏后的有界文本/图片证据，记录为 `failed`，并将诊断交给模型处理。增强结果通过错误载体跨越 Eino 的适配器，不因适配器丢弃返回值而丢失诊断。
- Windows 命令在启动前失败时，由原生进程边界携带 `notExecuted`。只有等待和清理成功、上下文未取消时才记录 `not_executed`。没有退出码的未知故障、等待/清理失败均不能获得该证明。
- 原生 `list_directory`、`search_files` 的普通读取错误返回模型处理，记录 `no_effect`。
- Waku 对话上方增加“任务与工具核对”入口；无需存在子代理任务即可读取当前会话回执。待核对筛选、按页证据、核对人、必填实际核实说明、完成/未产生效果结论和二次确认均走已有 ACP 连接。核对采用版本 CAS，不调用模型、不重新执行工具，旧错误和部分结果保留供审计。
- 回执查询新增向后兼容的可选 keyset 分页。Waku 每页只保留 4 条，以覆盖转义文本和历史审查证据；断连清除草稿，旧连接响应被拒绝，冲突重新查询。渲染仅访问缓存，查询由后台完成。

取消、连接中断、未知效果和持久化故障仍会停止当前轮并保留核对门禁。
升级不会自动修改旧的 `uncertain` 回执；核实后可在界面记录结果，再发送新消息。

## 必要验证

按用户要求，不运行全量或 race，不增加模型测试矩阵。

| 检查 | 结果 |
| --- | --- |
| 原始三个本地缺陷复现转成修复回归 | 通过；Windows 无效 PE 为 `not_executed`，MCP 业务错误为 `failed` 且后续消息正常，目录读取错误为 `no_effect` 且同轮继续 |
| MCP 模型输入证据 | 通过；模型确实收到业务诊断和可能存在部分效果的说明 |
| 命令启动证明与等待/清理错误边界 | 通过；未知退出码、Wait/Release 故障不能被包装为“未执行” |
| ACP 分页与已有原子核对用例 | 通过；后续页的阻塞调用可达，变更筛选不能复用游标，旧调用不自动重放 |
| Waku 编译检查 | 通过；新增桌面 UI、协议类型与 driver 接线 |
| Waku 单个回执协议用例 | 通过；拥有会话的连接、能力独立于任务树、必填核实说明、版本及结论准确传递 |
| WSL 现有 `TestWSL2RealSupervisorAndCancellation` | 通过；真实 Ubuntu-22.04，Go helper、无解释器 PATH、环境隔离、执行/超时/取消/脱离进程组后代清理 |

本机日志位于 `D:/Tools/deerflow-api/.build-cache/eino-harness-acp-verification`：
`p1-recovery-final-20261004.log`、`p1-receipt-page-20261004.log`、
`p1-mcp-evidence-20261004.log`、`waku-receipt-check-20261004.log`、
`waku-receipt-focused-20261004.log`、`wsl-existing-gate-20261004.log`。
中间日志保留失败和修正过程；分页用例以单独修正后的日志为准。

## 配置与发行

通用发行包保持付费技能维护默认关闭。个人配置可经原生配置工具启用
`review` 模式：生成和三类发现启用，使用已有默认模型，单 worker、每日
8 次模型请求/60000 token、每日最多 2 个提案，发布仍需人工审查。
新增技能使用独立的托管来源；内置覆盖默认关闭。
行为评测需要与目标技能相符的宿主 suite，不能拿无关示例冒充有效验收；
`auto_patch` 在具备该 suite 前不启用。

Windows Release 发行目录使用
`dist/desktop/DeerFlow-Desktop-Go-20261004-P1`。
二进制和对应源码 ZIP 由原有构建入口从同一个干净提交重建并产生，
准确提交身份以 `BUILD_SOURCE.json` 和 `BUILD_COMMIT.txt` 为准。
源码输入和包内 Python 文件分别检查；个人 `user-data` 仅用于本机迁移，
不进入源码、发行 ZIP 或公开哈希清单。

真实线上模型效果、原生桌面操作、未安装 Python 的全新 Windows OS、
远端 GitHub Actions 尚不能由本机 fixture/编译结果替代。
真实 WSL 已执行上述现有用例；Ubuntu 中未安装 Go/Rust，未执行原生
Linux 全项目构建。此前清理清单的 CI 和 shell 入口验收仍应分别记录环境，
不将语法检查改写成远端执行成功。
