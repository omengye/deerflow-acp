# DeerFlow · Eino Go Harness

本分支以 [CloudWeGo Eino](https://github.com/cloudwego/eino) 为唯一 Agent 运行时，提供 Go SDK、ACP stdio、本地 daemon 和 Waku 桌面。Waku 与 ACP Bridge 使用 Rust，模型和工具由 Go harness 执行。本轮不提供 HTTP 服务。

当前版本固定 Eino `v0.10.0-alpha.35`、Go `1.26.8`。已实现子代理、技能审查发布和原生 Go 交付；完整范围及验收结果见[实施方案](docs/eino-subagents-skill-evolution-plan.md)和[实施记录](docs/eino-subagents-skill-evolution-status.md)。当前源码通过的测试不等同于新的桌面发行包已验收。

技能自进化采用主模型自动完成经验提取、技能生成、验证、有限范围采用、观察及撤回，无独立配置、累计 token 额度或每日调用和发布配额。最终契约与验收要求见[技能自进化方案](docs/skill-evolution-final-plan-20261109.md)，历史发版记录保留其原有证据。

## 使用入口

| 入口 | 用途 |
| --- | --- |
| `deerflow-desktop.exe` | Waku 桌面聊天、DeerFlow 设置、任务与技能管理 |
| `deerflow-acp.exe` | ACP v1 stdio Bridge，多窗口共享 Go daemon；`--protocol v2` 提供 Bridge v2 适配 |
| `deerflow-acpd.exe` | 持有 SQLite 状态的 Go daemon，使用认证本地 IPC |
| `deerflow-acp-go.exe` | 直接运行 Go ACP stdio agent |
| `deerflow-config-go.exe` | 原生配置初始化、校验、保存与模型测试，由 Waku 调用 |
| `go-harness/` | 可嵌入的 Go SDK |
| `integrations/buzz/` | 原生 Go Buzz 适配器，使用官方 CLI 与 ACP v2 Bridge，独立 Windows ZIP 与 CI |

Linux 的 ACP 可执行文件省略 `.exe`。客户端启动 Bridge 或直连 stdio 程序；`--start-daemon`、`--status`、`--stop-daemon` 用于管理常驻进程。

## Windows 安装

打开 [Build Windows Desktop](https://github.com/omengye/deerflow-acp/actions/workflows/build-desktop.yml)，选择所需分支最近一次成功的构建。在运行摘要中点击 **Download the Windows x64 installer**，或在 **Artifacts** 下载 **DeerFlow-Windows-Installer-x64**。登录 GitHub 后下载并解压，双击 `DeerFlow-Desktop-<版本>-windows-x64-Setup.exe` 安装。

安装包适用于 Windows 10/11 x64，采用未签名安装器，无需配置 Windows 证书。默认仅为当前用户安装到 `%LOCALAPPDATA%\Programs\DeerFlowDesktop`，支持开始菜单入口和卸载；运行桌面不需要安装 Go、Rust 或 Python。Git、WebView2 及可选工具按[安装说明](docs/windows-desktop-actions.md#exe-安装程序)准备。

下载包同时包含对应源码 ZIP 和 SHA-256 校验文件。Actions 下载保留 14 天；过期后可重新运行工作流。有正式发布时，可从 [Releases](https://github.com/omengye/deerflow-acp/releases) 下载对应安装文件。升级前先退出桌面并停止该安装目录的后台服务；覆盖安装及卸载都会保留 `user-data`。

## 从源码构建

Windows MVP 统一交付入口支持桌面、独立 ACP 或全部包，并在归档前运行回归和包内协议检查：

```powershell
.\scripts\build-windows.ps1 -Package Desktop
# 可选：同时生成未签名的 EXE 安装包（需要 Inno Setup 6）。
.\scripts\build-windows.ps1 -Package Desktop -CreateInstaller
# -Package ACP 或 -Package All
```

正式 ZIP 和安装包要求干净、已提交的工作树；未提交开发构建使用 `-Development`，默认仅输出目录，可加 `-CreateInstaller` 生成文件名带 `development` 的本地测试安装包。GitHub Actions 的 **Build Windows MVP** 可以选择包类型，Desktop/All 自动生成未签名安装包；现有桌面和 ACP Release 工作流复用同一检查流程。当前 MVP 判断及验收边界见[发版检查记录](docs/release-readiness-20261014.md)，完整命令和产物见[Windows 构建说明](docs/windows-desktop-actions.md)。

需要 Git、Go 和 Rust。Windows 桌面还需要 MSVC、Windows SDK、CMake 与 shader 编译器，详见[桌面构建说明](docs/windows-desktop-actions.md)。项目构建和运行不使用 Python、uv 或 pip。

构建独立 ACP 包：

```powershell
.\scripts\build-go-acp-portable.ps1 -Configuration Release
```

```sh
bash ./scripts/build-go-acp-portable.sh Release dist/go-acp/linux-x64
```

构建 Windows Waku 桌面供本地开发：

```powershell
.\scripts\build-deerflow-desktop-go.ps1 -Configuration Debug
```

Waku 桌面发行构建使用 `-Configuration Release -CreateZip`，要求干净、已提交的工作树并重新编译。构建脚本将二进制与对应源码绑定到同一提交；`-SkipBuild` 不能和 `-CreateZip` 同用。每次构建使用新的输出目录，详情见[归档来源要求](docs/go-acp-portable.md#desktop-builds-and-corresponding-source)。完整的无 Python 环境验收仍按实施方案执行。

## 配置与启动

桌面首次启动使用简体中文并创建 `user-data`。在 DeerFlow 设置中填入模型 Provider、模型 ID、Base URL 和 API Key，保存并应用，然后创建会话。支持的 Provider 为 `openai`（含兼容协议服务）和 `claude`，所有模型请求均使用流式传输。记忆、子代理及技能自动维护已预配置，维护模型默认继承所选模型，无需逐项填写；升级不覆盖已有语言和配置。

需要代理时，在“设置 → 常用 → 网络代理”启用并填写代理地址，然后点击“保存更改”。统一代理用于模型、搜索和网页抓取，支持 HTTP、HTTPS、SOCKS5 及绕过地址；工具和搜索服务的独立代理优先。关闭统一代理恢复原有连接配置，详见[网络代理说明](docs/go-native-tools.md#统一网络代理)。

OpenAI 模型可在“设置 → 模型 → 接口协议”选择 Responses。`gpt-6.1-sol` 等要求 Responses 才能调用工具的模型应选择该协议；保留原服务地址（例如 `https://api.openai.com/v1`）和 API Key，输出上限参数使用“自动”。Responses 同样使用统一代理；已有模型的“自动”协议保留原连接方式。
`gpt-6.1-sol` 的推理强度可使用 `low`、`medium`、`high`、`xhigh`、`max`；它不支持 `none`，因此“思考模式接口”应保持未配置或声明不支持切换，不能声明可关闭的思考开关。

新桌面配置默认允许本地命令和宿主工具，并使用 `dangerous` 权限模式，不是隔离沙箱。技能自动维护沿用主模型，可能产生模型费用；不设置每日调用或累计 Token 额度，运行由并发、超时、去重及有限重试约束，实际使用量继续统计。脚本技能的自动验证仍要求真实隔离执行。搜索服务密钥和 OpenCLI 浏览器依赖为可选配置，不影响基本聊天。

源码开发可从 [config.example.yaml](config.example.yaml) 复制配置；示例使用 OpenAI 兼容协议及 `DASHSCOPE_API_KEY` 环境变量。`models[].provider` 和 `sandbox.provider` 使用原生名称，工具按 `name` 选择。相对技能路径以配置文件所在目录解析。

独立 ACP 包也可使用环境变量：

```powershell
$env:DEERFLOW_MODEL = 'your-model-id'
$env:DEERFLOW_MODEL_API_KEY = 'your-api-key'
$env:DEERFLOW_MODEL_BASE_URL = 'https://your-provider.example/v1'
$env:DEERFLOW_GO_DATA_DIR = 'C:/DeerFlow/go-state'
.\dist\go-acp\windows-x64\deerflow-acp.exe --runtime-dir C:/DeerFlow/go-runtime --start-daemon
```

ACP 客户端命令填写 Bridge 的绝对路径，使用相同的环境与 `--runtime-dir`，不要添加 `--start-daemon`。也可通过 `--config` 指定配置文件。完整说明见 [Go ACP 便携包](docs/go-acp-portable.md) 和 [Bridge](bridge/README.md)。

## 当前能力

- 会话持久化、流式响应、模型选择、工具权限、中断恢复、工具回执及效果核对。
- 前后台子代理、角色配置、独立模型与工具/技能范围、共享根任务预算、嵌套等待及结果续跑；桌面[运行状态面板](docs/desktop-runtime-status.md)展示 Todo 和 Agent 执行状态，任务管理保留在详情中。MCP 资源重建及桌面真实交互仍待完成验收。
- Go 原生网页搜索/抓取/图片搜索、文件工具、命令执行和 `host_opencli`。[搜索插件](docs/search-plugins.md)支持 Brave、DeepSeek、SearXNG 在设置 → 工具 → `web_search` 中配置和即时切换，无需重启服务；SearXNG 地址必填、Key 选填。[工具配置说明](docs/go-native-tools.md)列出支持参数。OpenCLI 需要用户安装 OpenCLI/Node.js，浏览器命令还需要其浏览器桥接。
- 技能安装、启停、不可变版本、候选、验证和发布/撤回；真实读取、宿主可验证结果及反馈持久保存。新技能与已有技能通过统一自动验证流程，脚本必须真实隔离执行。72 小时观察要求至少五个独立任务成功、关键行为覆盖且没有确认失败，未达要求自动撤回。前台任务固定版本，使用量继续统计。
- Waku 设置按配置工具与运行 daemon 的能力显示，保存与应用状态分别处理；字段清理范围见[设置审计](docs/eino-waku-settings-audit.md)。

桌面沿用主模型并自动准备受管技能空间。可信宿主套件和新任务的可验证结果用于评测；证据或隔离运行条件不足时等待条件变化。升级一次性删除旧技能进化配置，保留已安装技能和历史审计，不补跑历史阻断工作。

技能自进化采用统一的零配置流程；独立 ACP 的 `resources/default-config.yaml` 和源码 `config.example.yaml` 同样不再包含旧技能进化配置。命令执行、宿主工具及其他功能仍使用各入口的权限配置；无配置文件的 SDK/ACP 默认值见 Go harness 文档。Go daemon 缺失时启动报错；没有 Python 后端回退。

## 数据与升级

桌面配置为 `user-data/config/config.yaml`，聊天与偏好位于 `user-data/desktop`，新建 Go 状态位于 `user-data/config/go-harness-state`，已有状态沿用 `user-data/config/runtime-data.json` 的绑定，daemon endpoint 位于 `user-data/runtime/acp-go`。启动时拒绝同时存在两份运行数据库；非活动库需先归档到运行目录之外。升级使用新的程序目录并保留现有绑定，不要直接编辑运行中的数据库。MVP 不提供运行数据备份/恢复命令；会话持久化、断点与回执核对、技能版本回滚仍保留。

MVP 支持显式附加 UTF-8 TXT、Markdown、CSV、TSV、JSON、YAML 和 LOG 文件，每条消息最多 8 个、单文件 1 MiB、合计 4 MiB。服务保存会话私有快照，模型通过 `list_attachments` / `read_attachment` 分块读取，不会因附件获得任意宿主路径访问权。图片链路保留；PDF、Office、目录及 OCR 不在本版范围。

默认不限制任务总执行时间、前台排队时间或原生子代理的累计执行时间，`run_timeout_seconds: 0` 表示不限时，可持续处理跨天任务；上下文自动压缩仍启用。工具调用、连接握手、后台租约和退出清理保留各自的超时与失效保护；模型请求仍可取消，不额外承诺所有 Provider 均有单次请求时限。已有配置中的显式时限不被静默覆盖，可在设置中改为 0；已有持久任务保留原预算，新请求使用新策略。持续运行需要主机与服务保持可用，这不等于断电后无条件自动完成。

DeerFlow 常用设置提供本地能力就绪检查，不访问网络或消耗模型额度；模型和 OpenCLI 的实际连接能力需要另行主动验证。来源配置、聊天贡献和各阶段作业可在技能中心核对。当前实施范围见[技能自进化方案](docs/skill-evolution-final-plan-20261109.md)，此前验收记录保留在 `docs/mvp-closure-20261011.md`。

桌面服务诊断日志位于 `user-data/logs/deerflow-acpd-<pid>-<唯一标识>.jsonl`，设置概览显示当前日志路径。日志记录启动/退出阶段和错误分类，不记录配置、模型/工具内容或原始错误文本；每次启动独立保存并轮转。独立 daemon 可通过 `--log-dir` 或 `DEERFLOW_GO_LOG_DIR` 指定目录，详见[日志说明](go-harness/internal/diagnostics/README.md)。

旧 Python LangGraph 检查点不能作为 Go 检查点恢复。旧配置、记忆、角色和提案历史按显式 Go 迁移工具处理，未完成的导入能力在实施记录中标明。旧实现及数据格式依据固定到[迁移基线](docs/eino-legacy-migration-baseline.md)，不通过启动旧运行时读取。

## 开发验证

```powershell
cd go-harness
go test -p 2 ./... -timeout 30m
go build ./...
```

Rust Bridge 与 Waku 的构建、协议 smoke 和 CI 入口见[桌面构建说明](docs/windows-desktop-actions.md)。桌面协议测试、真实交互测试及无 Python 环境验收分别记录。

Buzz 的配置迁移、权限策略、故障恢复与独立构建见[适配器说明](integrations/buzz/README.md)。其测试使用模拟 Buzz CLI 和本地模型 fixture；真实 relay 收发由人工验收。

## 许可证

DeerFlow 与 Waku 的许可证分别见 [LICENSE](LICENSE) 和 [desktop-app/LICENSE](desktop-app/LICENSE)。Waku fork 的分发要求见 [FORK.md](desktop-app/FORK.md)，分发桌面二进制时附上对应源码与许可证。
