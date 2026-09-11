# DeerFlow ACP Portable

这是 DeerFlow ACP 的 Windows x64 便携版。解压 ZIP 后无需安装。

## 首次使用

1. 运行 `deerflow-config.exe`。
2. 在“模型”页面配置 Provider、模型 ID、Base URL 和 API Key。
3. 按需配置 Custom Agent、Skills 与 Runtime / ACP 参数。
4. 点击“保存并应用”，首次启动后复制概览页生成的客户端配置，在自己的 ACP 客户端中验收。

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

“Skills”页面可以查看待审批 Proposal 的详情、Diff、安全扫描和评估结果，并执行批准发布或拒绝。“数据与恢复”页面可查看 Signal、Probation、归档 Proposal 与修订历史，并执行版本回滚。

## 长期记忆

“记忆”页面用于配置便携版内置的本地 DeerMem。可以启停长期记忆，选择自动召回（`middleware`）或模型主动搜索（`tool`），并调整提取模型、写入延迟、事实数量、置信度、注入 Token、FTS5 检索和关闭刷新超时。

“ACP 记忆作用域”控制不同客户端会话之间如何共享记忆：`global` 全局共享，`workspace` 按项目目录隔离，`session` 按 ACP 会话隔离。便携版默认使用 `workspace`。

global 共享记忆保存在 `user-data/data/deerflow/memory.json`，workspace/session 记忆保存在 `user-data/data/deerflow/memory-scopes`；可重建的检索索引默认位于 `user-data/data/deerflow/memory-fts5.sqlite3`。建议保持相对路径，以便移动整个便携目录时同时迁移记忆。本便携版本的配置工具只支持本地 DeerMem，不提供远程 Mem0 配置。

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

便携 ACP 默认每小时自动清理一次过期会话及其 checkpoint。已关闭会话由 `closed_session_retention_days` 控制；客户端未发送 `session/close` 时，未连接且长期无活动的会话由 `inactive_session_retention_days` 控制。可在 `local_acp` 中调整 `session_cleanup_interval_seconds`，或将 `session_cleanup_enabled` 设为 `false` 关闭自动清理。启动阶段实际删除了过期会话时会自动压缩 checkpoint 数据库；运行期间删除出的空闲页则供后续写入复用，避免在线 `VACUUM` 阻塞活跃任务。

模型的字面量密钥在界面读取时会被脱敏。保存时密钥输入框留空会保留原值；只有勾选“清除已保存的 API Key”才会删除它。


## 配置工具的新工作流

- 普通“保存配置”不停止 Daemon。运行实例保留启动时配置，保存后提示待应用。
- “保存并应用”先验证并保存，然后暂停接收新任务，等待已接收的任务结束后重启。可取消等待，也可在确认影响后立即重启。
- “刷新状态”每五秒自动执行，不丢弃编辑内容；诊断页“重新加载配置”会在存在草稿时要求确认。关闭窗口同样保护草稿。
- 高级配置位于对应页面的折叠区，包括模型、工具、会话、记忆和自进化。各页独立展开，本次使用期间记住状态；收起不会清空配置或草稿。隐藏字段出错时保持展开，可点击“查看错误配置”定位。
- 模型页顶部展示全局默认模型、新 ACP 会话预计使用的模型及其来源。选择列表项只切换编辑对象；点击“设为默认”才修改默认值，保存并应用后生效。删除默认模型需明确选择替代项，被其他配置引用的模型须先解除引用。
- ACP 新会话模型可选择“自动选择（Agent 优先，否则全局默认）”，或指定固定模型。固定选择不跟随全局默认变化；Agent、记忆和自进化的模型选项明确提供“继承全局默认”。已有会话的显式模型选择保持不变。
- 模型页面提供服务商预设和手动测试。只有点击“测试模型”才发送一次最小文本请求，可能产生少量费用；默认不进行付费探测。
- 概览中的能力预览基于当前编辑配置，客户端的会话模型/Profile 选择仍可覆盖默认值。
- 诊断页可查看存储占用、日志尾部并复制脱敏报告。本地路径仍会显示，分享前请自行检查。

## 数据管理与兼容

“数据与恢复”提供配置备份预览与恢复、会话清理预览、记忆事实查看和删除、自进化历史与版本回滚。会话和记忆操作需要 Daemon 运行；正在连接客户端的会话不能删除。清理会话会删除会话元数据及 checkpoint，保留 uploads/outputs 等产物文件。

配置恢复只恢复配置工具管理的配置部分与 Agent 设置，恢复前自动备份当前配置。会话、记忆事实和产物不随配置回滚。恢复后仍需应用配置。默认自动清理保留期限未改变。

ACP 的 workspace/session 记忆现在实际保存到 `user-data/data/deerflow/memory-scopes` 的独立目录。旧版共享记忆不会自动复制到项目作用域，也不会删除；这是为了避免把未标注来源的旧事实分配给错误项目。global 作用域仍使用原存储位置。项目目录更换后产生新的 workspace 记忆作用域。

`queue_timeout_seconds`（默认 600 秒）限制排队等待；`run_timeout_seconds` 在取得运行槽位后计时，仍覆盖该轮自动续跑和权限等待。客户端会收到排队和开始执行的状态提示，取消会及时释放等待槽位。

所有 ACP 客户端中的连接、权限呈现和会话体验，请在交付便携包后由使用者进行验收。
