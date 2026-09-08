# DeerFlow ACP Portable

这是 DeerFlow ACP 的 Windows x64 便携版。解压 ZIP 后无需安装。

## 首次使用

1. 运行 `deerflow-config.exe`。
2. 在“模型”页面配置 Provider、模型 ID、Base URL 和 API Key。
3. 按需配置 Custom Agent、Skills 与 Runtime / ACP 参数。
4. 点击“保存配置”，再从概览页启动 Daemon。

首次启动会自动创建 `user-data` 下的配置、数据、Skill、日志、备份和 ACP runtime 目录。所有用户数据都留在解压目录中；移动整个目录后仍可使用。

## ACP 客户端配置

DeerFlow 使用标准 ACP stdio 协议。普通客户端默认使用 ACP v1；将启动命令设置为解压目录下 `deerflow-acp.exe` 的**绝对路径**，参数保持为空。独立 sidecar 可通过 `--protocol v2` 使用 ACP v2 兼容入口。Bridge 会自动发现并使用：

- `user-data/config/config.yaml`
- `runtime/python.exe`
- `user-data/runtime/acp`

不要把 `deerflow-config.exe`、`--start-daemon`、`--status` 或 `--gateway` 配置为 ACP Agent 命令；客户端需要启动的是不带模式参数的 `deerflow-acp.exe`。默认 Bridge 会在需要时自动启动 Daemon，并通过 stdio 与客户端交换 ACP JSON-RPC。

### Zed

打开 Zed 的 `settings.json`，把下面的 `agent_servers` 合并到现有配置中。将示例路径替换为实际解压路径；JSON 中的 Windows 反斜杠必须写成 `\\`：

```json
{
  "agent_servers": {
    "DeerFlow Local": {
      "type": "custom",
      "command": "D:\\Apps\\DeerFlow\\deerflow-acp.exe",
      "args": []
    }
  }
}
```

保存设置后，在 Zed 的 Agent 面板中选择 `DeerFlow Local`。Zed 会把当前项目目录作为 ACP session 的工作目录传给 DeerFlow；无需在参数中配置项目路径。多个 Zed 窗口可以指向同一份便携目录并共享常驻 Daemon，但每个窗口仍使用独立的 ACP session。

### 其他 ACP Client

在 Waku 或其他支持自定义 ACP Agent 的客户端中使用以下设置：

- Transport / 通信方式：`stdio`
- Command / Program：`D:\Apps\DeerFlow\deerflow-acp.exe`（替换为实际绝对路径）
- Arguments / Args：留空
- Working directory：由客户端使用当前项目目录传入
- Environment：通常留空

如果客户端只接受一条命令行，可填写带引号的完整路径，例如：

```text
"D:\Apps\DeerFlow\deerflow-acp.exe"
```

不要通过 `cmd /c` 或 PowerShell 包装该命令，否则包装层可能干扰 ACP stdio、进程退出和取消信号。

### ACP v2 sidecar

独立 sidecar（例如随包提供的 Buzz 适配器）应直接启动同一个可执行文件，并明确传入：

```text
D:\Apps\DeerFlow\deerflow-acp.exe --protocol v2
```

该入口在 Bridge 内把 ACP v2 的 `initialize`、session 新建/列出/恢复/关闭、prompt、cancel、状态更新和权限请求映射到常驻 DeerFlow daemon。`session/prompt` 会先确认接收，再以 `running` 到 `idle` 的状态更新报告完成。现有 ACP v1 客户端无需修改。

Buzz sidecar 还可以在自身配置中设置身份显示名、显式频道 UUID 到名称的映射和 presence/status；ACP v2 的工具更新可发布为 owner-only NIP-AO 活动，没有 owner attestation 时可降级为单条可编辑的频道进度消息。具体配置见 `integrations/buzz/README.md`。

### 图片输入

DeerFlow ACP 支持客户端发送标准 `ImageContentBlock`，也支持引用当前 session 工作目录内图片的本地 `file://` ResourceLink。支持 JPG、PNG、WebP 和 GIF；单张最大 20 MB，每轮最多 8 张且总计不超过 40 MB。图片会保存到对应会话的 uploads 目录，checkpoint 只记录文件元数据，不保存 Base64。

图片输入要求当前会话选择的模型配置了 `supports_vision: true`。如果当前模型不支持视觉，ACP 会拒绝该轮输入并提示可选的视觉模型，不会静默切换模型。HTTP/HTTPS 图片 ResourceLink 暂不自动下载；远程图片请由客户端作为 `ImageContentBlock` 发送。

### `/goal` 长任务

发送 `/goal <完成条件>` 会把目标保存到当前 ACP 会话并立即开始执行。单独发送 `/goal` 可查看状态；发送 `/goal clear`、`/goal reset` 或 `/goal off` 可清除。目标会随会话 checkpoint 恢复，完成后自动清除；带图片或资源链接的消息按普通 prompt 处理，不会触发命令。

每个有活动目标的回合结束后，DeerFlow 会用关闭 thinking 的独立模型检查可见证据。需要用户输入、运行失败、等待外部系统、证据不足或评估失败时会停止，并保留目标供后续查看或继续。

自动续跑默认关闭。需要时编辑 `user-data/config/config.yaml` 的 `local_acp`：

```yaml
local_acp:
  goal_auto_continue: true
  goal_max_continuations: 3
  goal_max_no_progress_continuations: 2
```

单次目标最多自动续跑 8 次。隐藏续跑仍受原 prompt 的超时、取消和权限审批约束，用量会累计到同一次 ACP 响应。

### 启动与排障

第一次连接前应先运行一次 `deerflow-config.exe` 并保存模型配置。Daemon 可以由 ACP Client 首次连接时自动启动，也可以从配置工具的概览页提前启动。

在解压目录打开 PowerShell，可以检查或控制 Daemon：

```powershell
.\deerflow-acp.exe --status
.\deerflow-acp.exe --start-daemon
.\deerflow-acp.exe --stop-daemon
```

如果客户端连接失败，请依次检查：

1. 客户端中的 `command` 是否为当前解压目录下 EXE 的绝对路径。
2. 是否已通过 `deerflow-config.exe` 保存有效的模型配置。
3. `user-data/runtime/acp/daemon.log` 中是否有启动或模型加载错误。
4. 移动便携目录后，是否同步更新了客户端中的绝对路径。

本工具不会自动修改或测试 Waku、Zed 等 ACP Client 的配置，客户端侧连通性需要手动验证。

## Skill 自进化

“Skills”页面可以启用 Self Improving，并配置 Review / Auto Patch 模式、生成/审核/评估模型、自动发现阈值、候选大小限制和自动回滚阈值。Auto Patch 的创建、支持文件、脚本和删除能力始终由安全锁禁用。

“Skills”页面可以查看待审批 Proposal 的详情、Diff、安全扫描和评估结果，并执行批准发布或拒绝。Signal、Probation、归档和完整历史仍在 DeerFlow Admin 页面中管理。

## 长期记忆

“记忆”页面用于配置便携版内置的本地 DeerMem。可以启停长期记忆，选择自动召回（`middleware`）或模型主动搜索（`tool`），并调整提取模型、写入延迟、事实数量、置信度、注入 Token、FTS5 检索和关闭刷新超时。

“ACP 记忆作用域”控制不同客户端会话之间如何共享记忆：`global` 全局共享，`workspace` 按项目目录隔离，`session` 按 ACP 会话隔离。便携版默认使用 `workspace`。

默认记忆数据保存在 `user-data/data/deerflow/memory.json`，可重建的检索索引保存在 `user-data/data/deerflow/memory-fts5.sqlite3`。建议保持相对路径，以便移动整个便携目录时同时迁移记忆。本便携版本的配置工具只支持本地 DeerMem，不提供远程 Mem0 配置。

## Subagents

“Agent”页面包含与 Admin 一致的 Subagents 全局开关、默认超时、默认最大轮次和模型分配，也可以直接编辑 `agents` 与 `custom_agents` 高级 JSON。Subagent 全局开关和“Runtime / ACP”页面的会话级 Subagents 开关需要同时启用。

页面下半部分的 Custom Agents 是可直接作为 ACP 主 Agent 使用的独立 Agent 目录，与任务执行期间由主 Agent 调用的 Subagent 配置相互独立。

## Sandbox 与本地工具

“Sandbox / Tools”页面固定使用 Local Sandbox，可以配置 Host Bash/Host Tools 安全开关、本地挂载与输出限制、Tool Groups、Tools，以及 ACP 会话的工具 Allowlist/Denylist。便携 ACP 不包含 Agent Sandbox、WSL 或 Docker Provider。Local Provider 共享宿主机文件系统，不是操作系统级隔离边界；Host 权限仅适用于完全可信的本地环境。

Sandbox 与 Tool JSON 中的字面量密钥会显示为 `__DEERFLOW_REDACTED__`。保留占位符会继续使用原值，输入新值则会替换原值。工具名称必须唯一，并且每个工具必须引用已配置的 Tool Group。

## 数据与恢复

- 配置：`user-data/config/config.yaml`
- Custom Agents：`user-data/data/deerflow/agents`
- Skills：`user-data/skills`
- Daemon 日志与端点：`user-data/runtime/acp`
- 配置备份：`user-data/backups`

模型的字面量密钥在界面读取时会被脱敏。保存时密钥输入框留空会保留原值；只有勾选“清除已保存的 API Key”才会删除它。
