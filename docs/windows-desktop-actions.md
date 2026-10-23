# Windows Waku 与 Eino Go 构建

[Build Windows MVP](../.github/workflows/build-windows.yml) 使用 `scripts/build-windows.ps1`，可选择 Desktop、ACP 或 All。已有 [Build Windows Desktop](../.github/workflows/build-desktop.yml) 和 [Release Windows Portable ACP](../.github/workflows/build-portable.yml) 复用该工作流，保留手动运行和发布 Release 触发。构建链路不安装或打包 Python、uv、pip。

## 本地构建

推荐使用 PowerShell 7。需要 Git、Go 1.26.8、兼容当前 Cargo.lock 的 Rust、MSVC x64 Build Tools。桌面还需要 Windows SDK、CMake 和 shader 编译器 `fxc.exe`。CI 使用 Rust 1.96.0；当前本地已有 Rust 1.95.0 的构建缓存，可沿用通过检查的本机工具链。Go 模块通过 `toolchain go1.26.8` 选择版本，工作区外的 `go version` 可能显示另一个全局版本。

从已配置 MSVC 的 PowerShell 执行统一入口：

```powershell
# 正式 ZIP、配套源码和校验文件；需要干净、已提交的工作树。
.\scripts\build-windows.ps1 -Package Desktop
.\scripts\build-windows.ps1 -Package ACP
.\scripts\build-windows.ps1 -Package All -OutputDirectory 'dist/desktop/mvp-0.1.0'

# 同时生成未签名的 EXE 安装程序（需要 Inno Setup 6）。
.\scripts\build-windows.ps1 -Package Desktop -CreateInstaller

# 未提交开发构建，仍编译并检查包内协议，仅生成开发目录。
.\scripts\build-windows.ps1 -Package ACP -Development
```

默认输出为新的时间戳目录。Desktop/All 的自定义输出必须位于 `dist/desktop` 下，ACP 可在 `dist` 下选择目录；现有目录会被拒绝。正式打包不能跳过测试。`-Development -SkipTests` 只省略源码回归，包内协议检查仍执行，构建记录标记回归未运行。`-TestTimeoutMinutes` 默认 30，控制单个 Go 包的测试总时限，不改变 Agent 的生产执行超时。

统一入口依次运行 Go 依赖校验、全量 Go 回归、Bridge 测试与来源 fixture，再调用下层 assembler 编译。实际包内 Bridge/daemon 测试使用本地 fixture；Desktop 额外运行两种 MCP 策略的协议 smoke。全部通过后写入 `BUILD_PACKAGE.json`、`BUILD_SOURCE.json`、`BUILD_COMMIT.txt` 与 `PACKAGE_FILES.sha256`，随后创建 ZIP 与 `.sha256`。二进制和源码 ZIP 固定到同一个提交。

原下层脚本仍可用于仅构建程序；其 `-CreateZip` 不包含统一入口的完整测试门禁：

```powershell
# 本地开发目录，每次使用新的输出位置。
.\scripts\build-deerflow-desktop-go.ps1 -Configuration Debug

# Waku 发行目录与对应源码；必须从干净、已提交的工作树执行。
.\scripts\build-deerflow-desktop-go.ps1 -Configuration Release -CreateZip
```

`-DesktopTargetDirectory` 可选择 Cargo 输出位置。源码身份检查在编译前进行，并在复制和归档时再次核查；`-SkipBuild -CreateZip` 被拒绝。`BUILD_COMMIT.txt` 和 `BUILD_SOURCE.json` 记录来源，详见[归档要求](go-acp-portable.md#desktop-builds-and-corresponding-source)。

### EXE 安装程序

DeerFlow 专用安装器使用 `resources/windows/deerflow-desktop.iss`，打包同一套通过协议检查的完整桌面目录，不使用上游 Waku 的产品标识、自动更新或发布地址。安装包内含 Go 后端、Bridge、配置工具、WSL helper、技能及许可证；终端用户运行桌面不需要安装 Go、Rust 或 Python。Git、WebView2 和可选工具的宿主依赖不由安装器安装。

构建机安装 Inno Setup 6 后执行：

```powershell
winget install --id JRSoftware.InnoSetup --exact
.\scripts\build-windows.ps1 -Package Desktop -CreateInstaller

# 编译器不在标准位置时显式指定；版本默认读取 desktop-app/Cargo.toml。
.\scripts\build-windows.ps1 -Package Desktop -CreateInstaller `
    -InstallerCompiler 'D:\Tools\Inno Setup 6\ISCC.exe' -InstallerVersion '0.1.19'

# 未提交修改的本地体验包。仍真实编译并通过两种协议 smoke 和包内 Bridge 测试。
# -SkipTests 仅在 Development 下可选，跳过耗时的全量源码回归，不跳过包内检查。
.\scripts\build-windows.ps1 -Package Desktop -Development -CreateInstaller `
    -DesktopTargetDirectory 'desktop-app/target'
```

正式输出为 `DeerFlow-Desktop-<版本>-windows-x64-Setup.exe` 及 `.exe.sha256`，与便携 ZIP、对应源码 ZIP 放在同一输出目录。必须同时分发对应源码 ZIP 和许可证。开发安装包名称带 `-development`，来源和测试状态保留在包内，不作为正式发行凭证；开发模式不生成源码 ZIP。`-CreateInstaller` 仅支持 Release Desktop/All，不接受 ACP 或 Debug。无需设置 Windows 证书，不做 Authenticode 签名；Windows 可能显示未知发布者或 SmartScreen 警告。

安装行为：

- 默认安装到 `%LOCALAPPDATA%\Programs\DeerFlowDesktop`，仅当前用户，无需管理员权限。可以选择其他当前用户可写目录，不要选只读的 `Program Files`。覆盖安装沿用原安装目录，不自动迁移已有安装。
- 创建开始菜单快捷方式，可勾选桌面快捷方式；可从 Windows 已安装应用中卸载。安装向导目前为英文，应用首次启动仍默认简体中文。
- `user-data` 仍在安装目录内，由应用首次启动创建。覆盖安装保留配置、密钥、历史和技能；卸载只移除安装器管理的程序文件，保留 `user-data`。安装包拒绝包含已使用目录中的个人数据。
- 升级或卸载前先结束任务，退出桌面并停止该目录的后台服务。安装器检测到目标目录仍有进程就中止，不强杀进程；WMI 不可用时也中止。静默安装不会启动桌面或重启机器。
- 不自动迁移已有便携版的用户数据，也不自动刷新微信插件的可执行文件哈希绑定。迁移请在所有服务停止后备份并复制 `user-data`；需要刷新微信绑定的旧包升级继续使用经审核的 `scripts/update-deerflow-desktop-go.ps1 -Restart -RefreshBundledWeChat` 流程。

安装器校验：

```powershell
.\scripts\test-windows-installer.ps1
.\scripts\test-windows-installer.ps1 -Compile # 需要 Inno Setup，实际编译隔离的测试载荷，不安装或启动应用
.\scripts\test-windows-installer.ps1 -Install # 使用独立产品 ID，验证静默安装、覆盖安装、卸载和数据保留
```

这些测试检查私有数据排除、文件校验和、源码及协议门禁、失败清理和实际 ISCC 编译。`-Install` 在隔离目录用测试载荷验证安装生命周期，不启动桌面应用，不修改已有安装；不能替代完整应用在另一台电脑上的界面和功能验收。Actions 的 Desktop/All 构建在正式打包前自动运行 `-Install`，验证静默安装、占用中拒绝升级或卸载、覆盖升级和卸载后保留用户数据。

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

### Go 回归分批执行

本地与 Actions 共用 `scripts/test-go-harness.ps1`。脚本先通过 `go test -list` 获取实际测试清单，再按每批最多 32 个顶层测试串行运行，包含所有子测试。普通回归保留每批 30 分钟上限，race 回归为 40 分钟，不使用测试结果缓存。这样避免大型包的累计耗时耗尽单次 `go test` 的包级超时，同时减少多个 SQLite 测试包并行落盘的争用。单个测试及产品代码内的 deadline 不因此延长。

```powershell
.\scripts\test-go-test-batches.ps1
.\scripts\test-go-harness.ps1
.\scripts\test-go-harness.ps1 -Race -TestTimeoutMinutes 40 -Packages './internal/...', './'
```

脚本逐批校验清单中的测试是否都有结束结果。退出码非零、遗漏、重复或失败均中止；环境相关 skip 单独记录，不算通过。`.build-cache/go-tests/<run>/manifest.json` 保存清单、批次及结果，每批 JSONL 保留实时输出和超时堆栈。Go harness 与 Windows 打包工作流均上传这些诊断，无须等待最终 ZIP 生成。普通回归与 race 在独立 runner 作业执行，不争用同一磁盘，也不共同消耗一个作业的时限。分批执行不是重试，也不改变 SQLite 持久化级别。

race 进一步划分为 runtime、SDK 根包、其余 internal 包三个独立作业。其余 internal 包从 `go list` 动态获取，仅排除已单独运行的 runtime，整体范围仍为 `./internal/... ./`。

在 **Actions → Build Windows MVP → Run workflow** 选择分支与包类型。`All` 在一次作业中生成桌面和 ACP 两套包。**Build Windows Desktop** 与 **Release Windows Portable ACP** 也可手动运行；发布 GitHub Release（含预发布）触发对应标签构建。手动运行只生成 artifact，不创建 Release。新文件提交到对应分支后才能在 GitHub 执行；手动工作流入口通常需要先存在于默认分支。

只需要桌面安装包时，打开 **Actions → Build Windows Desktop → Run workflow**，选择分支后运行。成功后在运行摘要点击 **Download the Windows x64 installer**，或在 **Artifacts** 下载 `DeerFlow-Windows-Installer-x64`。登录 GitHub 后下载并解压，运行其中的 `DeerFlow-Desktop-<版本>-windows-x64-Setup.exe`。该独立下载包含安装器及其 SHA-256 文件、对应源码 ZIP 及其 SHA-256 文件；Desktop 和 All 都生成这一下载入口。Actions 产物保留 14 天，过期后需重新运行工作流；正式 Release 的附件不受该保留期影响。无需配置签名证书或额外的 Repository secrets。

Desktop/All 工作流从 [Inno Setup 官方发布](https://github.com/jrsoftware/issrc/releases/tag/is-6_7_3) 下载固定的 `6.7.3`，校验仓库中固定的 SHA-256 后静默安装到 runner 临时目录，并显式使用该编译器；不依赖 runner 预装版本或 Chocolatey 最新版。ACP 构建跳过 Inno Setup。安装器生命周期日志保存在 `.build-cache/windows-release-logs/installer.txt`，失败时也会尝试上传到构建诊断 artifact。

需要便携版或全部产物时，原有桌面 artifact `DeerFlow-Windows-Desktop-x64` 仍包含：

- `DeerFlow-Desktop-<版本>-windows-x64-Setup.exe` 及其 SHA-256 文件（未签名）。
- `DeerFlow-Desktop-Go-windows-x64.zip`。
- `DeerFlow-Desktop-Go-source.zip`。
- 两个 ZIP 对应的 SHA-256 文件。

ACP artifact `DeerFlow-Windows-ACP-x64` 包含 `DeerFlow-ACP-Go-windows-x64.zip`、`DeerFlow-ACP-Go-source.zip` 及两份校验文件。All artifact 为 `DeerFlow-Windows-All-x64`，包含两套产物。

可运行 `Setup.exe` 安装，或解压 ZIP 后运行 `DeerFlow-Desktop-Go/deerflow-desktop.exe`。程序包与源码使用同一固定提交，并保留 Waku GPL 和 DeerFlow 许可证。成功的发布作业将经回归及 smoke 的安装包、ZIP 和校验文件上传至现有 Release。当前 Windows x64 安装包和便携包均未签名。

工作流缓存 Rust/Go 构建依赖，不复用发行目录或用户数据。普通 Go/Bridge 回归由 [Go harness](../.github/workflows/go-harness.yml) 执行，Waku 配置/协议及构建脚本检查由 [Desktop contracts](../.github/workflows/ci.yml) 执行。独立 ACP 的 [Release Windows Portable ACP](../.github/workflows/build-portable.yml) 只构建 Go ACP 与 Bridge 包，不包含 Waku。

## 协议 smoke 与验收边界

原生 `go-harness/cmd/deerflow-desktop-smoke` 用隔离配置、本地假模型和内置 Go MCP fixture 验证打包的 Waku daemon/Bridge/Go harness 协议。工作流运行普通模式和 `--legacy-empty-mcp-policy` 模式；日志位于 `.build-cache/desktop-smoke/*/logs/`，失败阻止上传桌面包。迁移后的实跑结果在[实施记录](eino-subagents-skill-evolution-status.md)中单列。

这类 smoke 检查模型切换、聊天与工具、审批/取消、MCP 策略及会话恢复，不测试 GUI 绘制、键盘操作或真实在线服务。上传桌面产物前还运行 Waku 设置状态与 ACP 合约测试。Waku 真实交互与未安装 Python/uv/pip 的干净环境构建仍是独立发布验收项；GitHub runner 没有执行安装命令本身不能证明该环境未预装解释器。

完整完成门槛见[实施方案 P7 与验收标准](eino-subagents-skill-evolution-plan.md)。本次修改工作流不表示 GitHub Actions 或最终桌面发行包已验证通过。
