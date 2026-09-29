# Eino Harness / ACP 实施状态

本文件跟踪完整 V1，不能以当前已实现子集代替原方案的验收范围。

## 基线

- 分支 `codex/eino-harness-acp-refactor`；Python/Bridge 参照 `92b56924989a91e0eae05be6e95a6304f1ee4cd4`。
- 代码位于独立 `go-harness/` 模块，未切换现有 Python 或桌面默认入口。
- 工具链 `go1.26.8`；因 SQLite `modernc.org/sqlite v1.60.0` 要求 Go 1.26，模块最低版本相应提高。
- Eino `v0.10.0-alpha.35`；openai `v0.1.13`、claude `v0.1.25`、ark `v0.1.71`；ACP 类型 `coder/acp-go-sdk v0.13.5`。
- MCP 适配器将在管理器接入时固定 `officialmcp v0.1.1`，尚未混入当前已编译功能范围。

## 工作项

| 原方案范围 | 当前进度 | 剩余验收 |
| --- | --- | --- |
| Go SDK / 版本组合 | 模块、公共类型、嵌入式 Client 和模型适配器已写入 | 全量组合、跨平台构建、配置清单 |
| SQLite 基础和 Eino providers | checkpoint、session events、background task stores；上游 conformance、崩溃恢复、SQL 故障注入测试已通过 | 后续 Manager/工具恢复装配 |
| 会话协调 / stdio | 双向传输、同步准入、占用、取消、new/list/load/resume/close 已写入 | 黑盒互操作、官方 TCK、真实编辑器 |
| 执行与工作区工具 | Eino DeepAgent/Runner、前台委派、原生文件工具、plan 只读模式；流清理和主/子工具权限测试通过 | token/时间预算、工具回执和更完整的工具集 |
| 权限 / 恢复 | 审批意图先落库、精确参数授权、断连拒绝；未完成运行标记待对账 | durable HITL 恢复和显式对账控制 |
| 领域事件 / 历史 | 持久事件、文本/工具 updates、load 重放 | 有界异步发送、计划/usage/产物投影、分页历史 |
| MCP | 待接入 | stdio 必需配置、HTTP/SSE 出站、scope 和凭据隔离 |
| 图片 / 附件 / 产物 | 引擎图片转换准备中；ACP 暂不声明支持 | 校验、文件持久化、引用和重放 |
| 后台子任务 / 长命令 | SQL provider 已写入，未完成调度接线 | Manager/TurnLoop、取消、租约、通知、重启恢复 |
| Skills / memory / 压缩 | 待接入 | Eino middleware + 项目范围/生命周期/预算 |
| Docker / Windows shell | 文件工具已跨平台，默认无 shell | Docker provider、PowerShell/WSL2、进程树清理 |
| daemon / Bridge / draft v2 | 待实现 | DFACP/1、endpoint、STATUS/STOP、多窗口和 v2 fixture |
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

先接入 scoped MCP 管理器与 ACP 客户端 stdio MCP 配置、会话模型/审批配置，随后装配 TurnLoop/backgroundtask Manager。保留工具注册的权限、资源清理和总预算约束，继续补齐 Skills、Memory、压缩、媒体/产物以及本地 daemon/Bridge。后台任务 SQL provider 已通过测试，但不能据此宣称后台任务已能通过当前可执行文件使用。
