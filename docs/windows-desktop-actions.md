# Windows 桌面版 GitHub Actions 构建

仓库根目录的 [Build Windows Desktop](../.github/workflows/build-desktop.yml) 工作流调用
`scripts/build-deerflow-desktop.ps1`，构建 GPUI 桌面程序、Waku daemon、ACP bridge 和
嵌入式 Python runtime。它与旧的 `Release Windows Portable ACP` 工作流独立，产物名称也不同。

## 触发与下载

将工作流和相关源码提交到 GitHub 默认分支后，在 **Actions → Build Windows Desktop →
Run workflow** 选择分支运行。成功后下载 `DeerFlow-Desktop-windows-x64` artifact，其中包含：

- `DeerFlow-Desktop-windows-x64.zip`：解压后运行 `DeerFlow-Desktop/deerflow-desktop.exe`。
- `DeerFlow-Desktop-source.zip`：与程序包一起构建的对应源码，包含重建使用的 `BUILD_VERSION.txt`。
- 两个 ZIP 各自的 `.sha256` 校验文件。

发布 GitHub Release（包括预发布）也会自动构建对应标签，验证通过后将这四个文件附到该
Release。手动运行只生成 Actions artifact，不创建或修改 Release。Artifact 和诊断日志保留
14 天；Release 附件不受此保留期影响。普通 push 和 PR 不触发这项耗时构建。

## 构建与验证

工作流使用 `windows-2025` x64 runner、Rust 1.96.0、uv 0.9.18 和脚本默认的嵌入式
Python 3.12.10。MSVC 环境初始化后检查 C++、资源、shader 编译器及 CMake。保留完整
Git 历史和标签，供 Python 包的 hatch-vcs 计算版本；Rust 和 Python 依赖沿用锁文件。
构建需要从 GitHub、Cargo、Python.org 和 Python 包索引下载依赖，无需配置模型 API Key。

Rust 缓存覆盖 `desktop-app` 的 `.build-cache/desktop-build` 以及 `bridge/target`、
`desktop/target`；uv 单独缓存 Python 下载和依赖。运行时每次重新组装，不缓存程序分发目录
或用户数据。桌面脚本目前复用旧 portable 脚本准备 runtime，因此也会编译旧 Iced 配置程序。

打包后运行 `scripts/test-deerflow-desktop-smoke.py`。它使用包内 Python，在独立目录启动
daemon 和 ACP bridge，并通过本地假模型检查配置、会话、权限和聊天流程，不启动桌面 GUI，
也不验证图形渲染。冒烟检查失败会阻止程序包上传及 Release 附件发布；诊断信息保存在
`DeerFlow-Desktop-smoke-logs` artifact。首次构建没有缓存，耗时可能较长；构建步骤限时
120 分钟，冒烟测试限时 30 分钟，整个构建 job 限时 180 分钟。

本流程生成未签名的 Windows x64 便携 ZIP，不生成安装器。源码 ZIP 随程序一起发布，保留
桌面 fork 所需的许可证和源码。构建 job 只有仓库读取权限，单独的发布 job 使用 GitHub
自动提供的 `GITHUB_TOKEN` 附加文件，无需配置个人访问令牌。

提交时应一并包含打包链依赖的 `scripts/cargo-windows-static-crt.toml` 及打包脚本改动，
避免本地存在但未提交的文件导致干净 checkout 构建失败。
