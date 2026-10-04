# Windows Waku 与 Eino Go 构建

[Build Windows Desktop](../.github/workflows/build-desktop.yml) 使用 `scripts/build-deerflow-desktop-go.ps1`，构建 Rust Waku、Waku daemon、ACP Bridge 和 Go harness/config/stdio 程序，以及 Go WSL helper。构建链路不安装或打包 Python、uv、pip。

## 本地构建

需要 Git、Go 1.26.8、兼容当前 Cargo.lock 的 Rust、MSVC x64 Build Tools、Windows SDK、CMake 和 shader 编译器 `fxc.exe`。CI 使用 Rust 1.96.0；当前本地已有 Rust 1.95.0 的构建缓存，可沿用通过检查的本机工具链。Go 模块通过 `toolchain go1.26.8` 选择版本，工作区外的 `go version` 可能显示另一个全局版本。

```powershell
# 本地开发目录，每次使用新的输出位置。
.\scripts\build-deerflow-desktop-go.ps1 -Configuration Debug

# Waku 发行目录与对应源码；必须从干净、已提交的工作树执行。
.\scripts\build-deerflow-desktop-go.ps1 -Configuration Release -CreateZip
```

`-DesktopTargetDirectory` 可选择 Cargo 输出位置。源码身份检查在编译前进行，并在复制和归档时再次核查；`-SkipBuild -CreateZip` 被拒绝。`BUILD_COMMIT.txt` 和 `BUILD_SOURCE.json` 记录来源，详见[归档要求](go-acp-portable.md#desktop-builds-and-corresponding-source)。

### 未提交工作树的本地交付

需要先让用户手动体验当前修改时，可以生成 **Release 便携目录**，省略 `-CreateZip`。这仍会真实编译所有 Rust/Go 程序，`BUILD_SOURCE.json` 将标注 `development-dirty`；不会把当前工作树冒充为 HEAD 的源码归档。待修改提交后，再使用 `-CreateZip` 生成可分发的二进制及对应源码 ZIP。

从仓库根目录在 PowerShell 中执行：

```powershell
$vsWhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio/Installer/vswhere.exe'
$vsInstall = & $vsWhere -latest -products '*' -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
if (-not $vsInstall) { throw 'Visual Studio C++ x64 Build Tools were not found' }
& (Join-Path $vsInstall 'Common7/Tools/Launch-VsDevShell.ps1') -Arch amd64 -HostArch amd64 -SkipAutomaticLocation
$env:GPUI_FXC_PATH = (Get-Command fxc.exe -ErrorAction Stop).Source
$env:CARGO_BUILD_JOBS = '2'
$buildStamp = Get-Date -Format 'yyyyMMdd-HHmmss'
.\scripts\build-deerflow-desktop-go.ps1 -Configuration Release `
    -DesktopTargetDirectory 'desktop-app/target' `
    -OutputDirectory "dist/desktop/DeerFlow-Desktop-Go-$buildStamp"
```

Visual Studio 开发环境会使用实际安装位置，不要求 SDK 位于 C 盘。输出中的 `deerflow-desktop.exe` 是 Waku 桌面入口。每次选择新的目录；构建不会复制旧包的 `user-data`、配置或密钥。代码和资源修改完成后再启动构建，避免产生 `development-source-changed` 来源记录。

两个旧脚本 `build-deerflow-desktop.ps1` 与 `build-deerflow-portable.ps1` 仅保留带弃用提示的 Go 包装入口，旧 Python 参数不再受支持。

## CI 触发与产物

在 **Actions → Build Windows Desktop → Run workflow** 选择分支运行。发布 GitHub Release（含预发布）也触发对应标签构建；手动运行生成 artifact，不创建 Release。

桌面 artifact `DeerFlow-Desktop-windows-x64` 包含：

- `DeerFlow-Desktop-Go-windows-x64.zip`。
- `DeerFlow-Desktop-Go-source.zip`。
- 两个 ZIP 对应的 SHA-256 文件。

解压后运行 `DeerFlow-Desktop-Go/deerflow-desktop.exe`。程序包与源码使用同一固定提交，并保留 Waku GPL 和 DeerFlow 许可证。成功的发布作业将经 smoke 的附件上传至现有 Release。当前生成未签名的 Windows x64 便携包，不生成安装器。

工作流缓存 Rust/Go 构建依赖，不复用发行目录或用户数据。普通 Go/Bridge 回归由 [Go harness](../.github/workflows/go-harness.yml) 执行，Waku 配置/协议及构建脚本检查由 [Desktop contracts](../.github/workflows/ci.yml) 执行。独立 ACP 的 [Release Windows Portable ACP](../.github/workflows/build-portable.yml) 只构建 Go ACP 与 Bridge 包，不包含 Waku。

## 协议 smoke 与验收边界

原生 `go-harness/cmd/deerflow-desktop-smoke` 用隔离配置、本地假模型和内置 Go MCP fixture 验证打包的 Waku daemon/Bridge/Go harness 协议。工作流运行普通模式和 `--legacy-empty-mcp-policy` 模式；日志位于 `.build-cache/desktop-smoke/*/logs/`，失败阻止上传桌面包。迁移后的实跑结果在[实施记录](eino-subagents-skill-evolution-status.md)中单列。

这类 smoke 检查模型切换、聊天与工具、审批/取消、MCP 策略及会话恢复，不测试 GUI 绘制、键盘操作或真实在线服务。Waku 真实交互与未安装 Python/uv/pip 的干净环境构建仍是独立发布验收项；GitHub runner 没有执行安装命令本身不能证明该环境未预装解释器。

完整完成门槛见[实施方案 P7 与验收标准](eino-subagents-skill-evolution-plan.md)。本次修改工作流不表示 GitHub Actions 或最终桌面发行包已验证通过。
