# DeerFlow Eino Go 便携版

## Waku 桌面包

解压新的桌面包，运行 `deerflow-desktop.exe`，在 DeerFlow 设置中填写模型并“保存并应用”。首次运行创建 `user-data`。

| 内容 | 路径 |
| --- | --- |
| DeerFlow 配置 | `user-data/config/config.yaml` |
| 桌面历史与偏好 | `user-data/desktop` |
| Go harness 状态 | `user-data/data/go-harness` |
| ACP runtime / endpoint | `user-data/runtime/acp-go` |
| 客户端 MCP 定义 | `user-data/config/client-mcp-servers.json` |

包内运行链路为 Rust Waku → Rust ACP Bridge → Eino Go daemon，不含 Python 运行时。Go daemon 缺失时直接报告错误。`host_opencli` 可调用用户安装的 OpenCLI/Node.js；需要浏览器功能时配置其浏览器桥接。

## 独立 ACP 包

将 `deerflow-acp.exe` 的绝对路径设为客户端的 stdio 命令，按需要传入 `--config` 与 `--runtime-dir`。包内相邻的 `deerflow-acpd.exe` 提供 Go 后端。默认 ACP v1，Bridge 可显式选择 `--protocol v2`。

独立包的构建、环境变量、命令和 MCP 配置见 [Go ACP 使用说明](docs/go-acp-portable.md)。Waku 发行构建及源码对应要求见[桌面构建说明](docs/windows-desktop-actions.md)。

## 升级与历史数据

关闭正在使用的程序后，将 `user-data` 备份并复制到新包；不要用发行包覆盖原配置。旧 Python 的会话与 LangGraph 检查点不能直接作为 Go 执行状态恢复。其他旧数据的显式迁移进度见[实施记录](docs/eino-subagents-skill-evolution-status.md)。

本分支仍在实施，当前源码和协议测试结果不表示最终 Release 或无 Python 干净环境验收已经完成。分发桌面二进制时，附上同一固定提交的源码 ZIP 与许可证。
