# Go 原生工具与桌面配置

Go/Eino harness 可以直接消费便携版的 `config.yaml`。新配置不需要 Python
`use` 类路径，也不会加载 Python 模块。

```yaml
models:
  - name: primary
    provider: openai  # openai 或 claude
    model: your-model-id
    api_key: ${MODEL_API_KEY}
    base_url: https://your-provider.example/v1
default_model: primary

local_acp:
  enable_bash: true
  permission_mode: dangerous

sandbox:
  provider: local
  allow_host_bash: true
  allow_host_tools: true

tool_groups:
  - name: web
  - name: file:read
  - name: file:write
  - name: host:opencli
  - name: bash

tools:
  - name: web_search
    group: web
    api_key: ${BRAVE_API_KEY}
    https_proxy: http://127.0.0.1:11808
    max_results: 5
  - name: web_fetch
    group: web
    https_proxy: http://127.0.0.1:11808
    timeout: 10
  - name: image_search
    group: web
    https_proxy: http://127.0.0.1:11808
    max_results: 5
  - name: ls
    group: file:read
  - name: read_file
    group: file:read
  - name: glob
    group: file:read
    max_results: 200
  - name: grep
    group: file:read
    max_results: 100
  - name: write_file
    group: file:write
  - name: str_replace
    group: file:write
  - name: move_path
    group: file:write
  - name: delete_path
    group: file:write
  - name: host_opencli
    group: host:opencli
    executable: C:\Users\your-user\AppData\Local\Microsoft\WindowsApps\opencli.cmd
    timeout: 120
    allowed_sites: [web, twitter, xiaohongshu, xiaoyuzhou, weixin, douyin, bilibili]
  - name: bash
    group: bash
```

## 实现与边界

| 工具 | Go 实现 |
| --- | --- |
| `web_search` | Brave Search API；保留独立 API key、代理、结果上限和 day/week/month/year 时间过滤 |
| `web_fetch` | Go HTTP、显式代理、超时、2 MiB 响应上限，HTML 正文和 Markdown 提取；默认输出 4096 字符 |
| `image_search` | DuckDuckGo 图片搜索，失败时使用 Bing；返回原图和缩略图 URL，支持尺寸、类型、布局过滤 |
| `ls` | 两层目录列表，有界分页和目录版本校验 |
| `read_file` | UTF-8 文本、行范围、字节分页和文件内容版本校验；单次最多 1 MiB，文件最多 16 MiB |
| `list_attachments` | 列出当前会话已接受的文件附件，每页最多 32 项；不遍历宿主路径 |
| `read_attachment` | 根据不透明附件 ID 分块读取会话私有 UTF-8 快照，每次最多 4096 字节；不接受路径，重启后仍可读取 |
| `glob` | 支持 `**` 的路径搜索；默认最多 200 个结果 |
| `grep` | RE2 正则或字面量搜索、大小写选项、glob 过滤；默认最多 100 条 |
| `write_file` | 原子写入、追加；已有文件必须传入 `expected_version`；总内容最多 512 KiB |
| `str_replace` | 必须传入 `expected_version`；默认要求唯一匹配，可显式 `replace_all` |
| `move_path` | 工作区内移动、重命名；显式覆盖文件，不隐式覆盖目录 |
| `delete_path` | 删除文件、符号链接、空目录；显式递归删除；禁止删除工作区根 |
| `host_opencli` | Go 参数数组调用已安装的 OpenCLI，带站点白名单、权限、输出上限和进程回收 |
| `bash` | 复用 Go shell 执行器；Windows 默认采用 PowerShell 语法，WSL/Docker 可显式配置 |

文件工具接受工作区相对路径、工作区内的绝对路径及 `/mnt/acp-workspace/...`。
文本附件单文件最多 1 MiB，每条消息最多 8 个、合计 4 MiB；支持 TXT、Markdown、CSV、TSV、JSON、YAML、LOG，不支持 PDF、Office、目录或 OCR。附件快照与工作区文件工具是两条独立授权路径，附件不授予任意宿主目录访问权。
所有文件操作使用固定的 `os.Root`，无法通过 `..` 或符号链接父目录越出工作区。
搜索跳过 `.git` 和符号链接，最多遍历 2000 个条目；`grep` 跳过二进制和超过
512 KiB 的文件，扫描总字节上限 16 MiB。返回截断标记和跳过文件数。
SDK 原有的 `list_directory/search_files/edit_file/execute` 工具仍可用。

默认与配置版 `read_file` 均返回含 `version`、`content` 的 JSON，版本对应完整文件。
覆盖、追加和 `edit_file/str_replace` 必须使用最近读取的 `version` 作为
`expected_version`；不传或传 `"absent"` 只允许创建新文件。冲突时不写入，必须重新
读取再提交，已批准的旧操作也不能覆盖中途发生的修改。该检查协调同一进程内的
原生写入，无法锁住外部编辑器、shell 或 MCP 写入；移动、删除尚不要求内容版本。

标准 SDK/ACP 的主代理另有 `task_note`、`history_search`、`history_read`，无需加入
上述 `tools` 配置；仍受工具策略控制。笔记按会话持久化，历史检索仅覆盖本会话可见
事件，不开放隐藏推理或跨会话内容。重复观察保护、笔记上限和恢复兼容性见
[harness 可靠性说明](harness-reliability-20261012.md)。

Windows 的 PowerShell `execute` 只向模型提供 `script/timeout_seconds` 参数，
例如 `{"script":"& 'curl.exe' '-s' 'https://example.com'"}`。该后端不接受
`executable/args`；这些字段只适用于允许指定可执行文件的其他后端。

网页抓取不执行 JavaScript，也不复刻 Scrapling 的 TLS 浏览器指纹。需要登录、
动态渲染或浏览器挑战的页面应使用 OpenCLI 的浏览器能力。图片来源可能受站点
风控限制；失败会明确返回错误。所有 HTTP 调用响应有界，尊重取消，搜索凭据
不会随跨来源重定向发送。

## OpenCLI

OpenCLI 本身仍需预先安装，它使用 Node.js，不需要 Python。Windows OpenCLIApp
受控 `.cmd` shim 和标准 npm `.cmd` 安装会解析到 `node.exe` 与 npm bin 的固定参数。
模型不能指定可执行文件或拼接 shell 字符串；其他不认识的 `.cmd` 会返回配置错误。
`OPENCLI_BIN` 宿主环境变量优先于配置路径。默认仅继承用户目录、OpenCLI 配置目录
和明确的浏览器超时设置，不继承模型密钥或 Go daemon token。

浏览器命令需要用户的 OpenCLI 浏览器 bridge 和浏览器登录态。默认 site 白名单
与旧配置一致，可在 `allowed_sites` 显式调整。工具接口保留
`description/site/command/arguments`；未指定输出格式时追加 `--format json`。
输出包含 `exit_code/stdout/stderr/state`、截断标记和终止确认；stdout/stderr
各保留最多 1,000,000 字节。非零退出、超时和取消均保留回执。

宿主必须同时配置工具条目及 `sandbox.allow_host_tools: true`。Go 的工具策略和
会话审批继续适用。计划及只读模式不会提供 OpenCLI、shell 或文件写操作。
Windows 子进程使用隐藏窗口和 Job Object；取消或超时会终止并等待整个进程树。
执行策略摘要进入 checkpoint，配置变化时会拒绝恢复旧执行；密钥不进入 checkpoint。

## 工具失败与回执恢复

原生工具的 JSON 解码错误、参数校验拒绝和命令启动前拒绝会记录为
`not_executed`。原生只读工具正常返回的失败记录为 `no_effect`，不会阻止后续提示。
外部命令（含 OpenCLI）非零退出或达到单次工具超时后，如果进程已确认终止且
清理成功，会记录为 `failed`。错误、退出码和输出返回模型，模型可以在同一轮
检查结果、修正命令并继续回复，下一条提示也不会因此被拦截。失败可能已经产生
部分效果，模型应先检查受影响的状态；运行时不会自动重放命令或假定已经回滚。
文件操作缺少终态证据、外部命令未确认终止、通信或清理失败仍保留 `uncertain`，
需要核实实际结果。用户取消和会话级超时继续停止当前执行。
新增只读工具在进程崩溃后没有终止证据时，仍保留 `uncertain`；SDK 扩展工具
不会仅因使用这些新增工具的名称就获得“无效果”的判定。

遇到 `reconciliation_required` 时，可通过当前 ACP 连接调用
`_deerflow/tool_receipts/list`，核实目标文件或外部操作，再调用
`_deerflow/tool_receipts/reconcile`，提交最新版本及核实说明。
重连时先以原工作目录 `session/resume`。核对只记录审查结果，不会重放工具。
完整参数见源码 `go-harness/internal/acp/receipts.md`。
升级不会自动改写历史 `uncertain` 回执，历史记录仍需按实际证据完成审查。

## 更新桌面包

关闭旧版 Desktop 后，将旧目录的整个 `user-data` 复制到新版目录，再启动新版
`deerflow-desktop.exe`。配置中的模型和工具密钥只存在本地，不包含在分发 ZIP 中。
旧版 Go daemon 不支持新的配置结构，应使用本次重建的 Go-only 包。
