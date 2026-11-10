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

network:
  proxy:
    enabled: true
    url: http://127.0.0.1:7890
    no_proxy: [localhost, 127.0.0.1, "::1"]

search:
  providers:
    brave:
      api_key: ${BRAVE_API_KEY}

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
    max_results: 5
  - name: web_fetch
    group: web
    timeout: 10
  - name: image_search
    group: web
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

## 统一网络代理

在桌面“设置 → 常用”的“网络代理”区块中启用统一代理并填写地址，通过页面底部的
“保存更改”应用。支持 `http://`、`https://`、`socks5://` 和 `socks5h://`；
绕过列表用于本地或内网目标，默认包含 `localhost`、`127.0.0.1`、`::1`。
代理连接测试使用当前草稿，经代理访问 `https://example.com` 验证 HTTPS 连接；
该测试忽略绕过列表，不保存配置，也不调用模型。

统一代理覆盖模型请求、模型连接测试、子代理和后台维护模型，以及 `web_search`、
本地 `web_fetch`、DeepSeek Fetch 和 `image_search`。工具或搜索服务已配置的
独立 `https_proxy` 优先于统一代理。DeepSeek Fetch 使用 DeepSeek 搜索服务的代理设置。
启用统一代理时，绕过列表同样适用于独立代理，匹配的目标直接连接。
关闭统一代理保留已保存地址，并恢复原有连接策略：模型可使用进程环境变量代理，
网页工具只使用其独立代理，否则直连。该设置不修改 Windows 系统代理或 PAC，
也不会替外部浏览器、OpenCLI、shell 命令配置网络。

统一设置保存到 `config.yaml` 的 `network.proxy`，搜索服务的独立覆盖继续保存在
`search-providers.json`。应用配置沿用现有等待活动任务结束、重建后端的流程；
已保存与已生效状态分别显示。设置页以明文显示代理地址，重新打开页面时回填已保存地址；
诊断输出仍会隐藏地址。留空保留已有地址，清除需使用显式清除操作。

## 实现与边界

| 工具 | Go 实现 |
| --- | --- |
| `web_search` | Brave / DeepSeek / SearXNG 搜索插件；在设置 → 工具 → `web_search` 中配置并即时切换，保留统一结果及来源链接，详见[搜索插件](search-plugins.md) |
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
| `skill_run` | 对当前会话固定的技能版本检查 Python 环境或执行 `.py` 入口；复用命令权限、超时和回执，不自动安装依赖 |
| `ask_user_question` | 仅在当前客户端声明提问能力时提供；支持选项与自由文本，回答不代替工具授权，停止或断连会取消问题 |

文件工具接受工作区相对路径、工作区内的绝对路径及 `/mnt/acp-workspace/...`。
文本附件单文件最多 1 MiB，每条消息最多 8 个、合计 4 MiB；支持 TXT、Markdown、CSV、TSV、JSON、YAML、LOG，不支持 PDF、Office、目录或 OCR。附件快照与工作区文件工具是两条独立授权路径，附件不授予任意宿主目录访问权。
所有文件操作使用固定的 `os.Root`，无法通过 `..` 或符号链接父目录越出工作区。
搜索跳过 `.git` 和符号链接，最多遍历 2000 个条目；`grep` 跳过二进制和超过
512 KiB 的文件，扫描总字节上限 16 MiB。返回截断标记和跳过文件数。
SDK 原有的 `list_directory/search_files/edit_file/execute` 工具仍可用。

技能可以携带 Python 源码和文本依赖声明，宿主本身仍不依赖 Python。
`skill_run` 使用 `action: check` 检查解释器/发行包元数据，使用 `action: run`
执行 `name`、包内相对 `path` 和独立 `args`。运行文件来自会话的不可变快照，
不是当前安装目录。Windows PowerShell 模式可发现 PATH 中的 Python；其他模式
需显式允许对应解释器，操作员也可用命令环境中的 `DEERFLOW_SKILL_PYTHON` 指定。
计划/只读模式不提供执行能力。手工发布含脚本候选需确认未执行验证的风险，
这不授予额外运行权限，也不代表本地/PowerShell/WSL 安全隔离。完整契约见
[Python 技能说明](skill-python-support-20261018.md)。

默认与配置版 `read_file` 均返回含 `version`、`content` 的 JSON，版本对应完整文件。
覆盖、追加和 `edit_file/str_replace` 必须使用最近读取的 `version` 作为
`expected_version`；不传或传 `"absent"` 只允许创建新文件。冲突时不写入，必须重新
读取再提交，已批准的旧操作也不能覆盖中途发生的修改。该检查协调同一进程内的
原生写入，无法锁住外部编辑器、shell 或 MCP 写入；移动、删除尚不要求内容版本。

标准 SDK/ACP 的主代理另有 `task_note`、`history_search`、`history_read`，无需加入
上述 `tools` 配置；仍受工具策略控制。笔记按会话持久化，历史检索仅覆盖本会话可见
事件，不开放隐藏推理或跨会话内容。重复观察保护、笔记上限和恢复兼容性见
[harness 可靠性说明](harness-reliability-20261012.md)。

`ask_user_question` 无需加入静态工具配置。桌面通过连接能力协商启用它，
没有对应能力的服务器客户端使用普通对话提问。问题属于当前轮次，不作为跨重连
审批检查点；其他不存在的工具调用返回未执行的纠正结果，避免直接中断整个会话。
请求与回答边界见[交互提问协议](../go-harness/internal/acp/executions.md#questions-in-the-desktop-conversation)。

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

## 工具失败、代理恢复与只读执行记录

原生工具的 JSON 解码错误、参数校验拒绝和命令启动前拒绝会记录为
`not_executed`。原生只读工具正常返回的失败记录为 `no_effect`，不会阻止后续提示。
外部命令（含 OpenCLI）非零退出或达到单次工具超时后，如果进程已确认终止且
清理成功，会记录为 `failed`。错误、退出码和输出返回模型，模型可以在同一轮
检查结果、修正命令并继续回复，下一条提示也不会因此被拦截。失败可能已经产生
部分效果，模型应先检查受影响的状态；运行时不会自动重放命令或假定已经回滚。
文件操作缺少终态证据、外部命令未确认终止、通信或清理失败仍保留 `uncertain`。
普通工具失败的可用输出和不确定性返回代理，由代理判断是否调查、修正、重试或
换用方案，直到解决问题或明确结束当前轮次；不设工具失败的固定重试次数。
`agentRecoverable: true` 表示宿主已将终态工具失败交给代理处理，不证明外部效果
已确定，也不保证模型已消费结果。预算、权限、用户停止及存储保护继续适用；
存储失败、未结算账本和审批中断仍由宿主控制流处理。用户取消和会话级超时
继续停止当前执行。
新增只读工具在进程崩溃后没有终止证据时，仍保留 `uncertain`；SDK 扩展工具
不会仅因使用这些新增工具的名称就获得“无效果”的判定。

需要用户补充信息或确认时，代理在普通对话中提问并结束当前轮次。用户在主输入框
回复后，同一会话携带历史开始新一轮执行；宿主不会自动重放旧调用或不安全的
检查点，代理须依据已有证据决定是否发起新的尝试。

工具回执的 ACP 接口仅提供 `_deerflow/tool_receipts/list` 只读查询。重连时以原
工作目录 `session/resume` 或 `session/load` 附着原会话；查询支持分页，桌面可查看
错误详情并复制完整记录。`_deerflow/tool_receipts/reconcile` 已退役，调用返回 MethodNotFound，桌面
不再提供人工核对表单。旧 `reconciliation_required` 错误按执行中断兼容显示，
不会要求用户证明工具成功或未产生效果。完整约定见
[只读工具记录与代理恢复](../go-harness/internal/acp/receipts.md)。

升级不会批量改写历史 `uncertain` 回执。选定的旧故障运行只能经显式宿主恢复，
校验会话、运行及预期版本，确认无活动执行和未结算账本后关闭旧运行并失效其
检查点；原始错误和未知证据保留。归档或删除终态会话不依赖人工核对，仍执行
活动、账本与任务图保护；分支和回退继续保守处理未知回执。

## 更新桌面包

关闭旧版 Desktop 后，将旧目录的整个 `user-data` 复制到新版目录，再启动新版
`deerflow-desktop.exe`。配置中的模型和工具密钥只存在本地，不包含在分发 ZIP 中。
旧版 Go daemon 不支持新的配置结构，应使用本次重建的 Go-only 包。
