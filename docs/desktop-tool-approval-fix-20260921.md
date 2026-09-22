# DeerFlow Desktop 工具审批修复记录

本轮将桌面的权限选择接入 DeerFlow 的真实会话审批策略，避免界面显示 Ask，但后端仍按自动批准执行工具。新安装、无历史偏好的会话默认使用 Full Access；用户选择过的模式继续保留。恢复已有会话时，以后端返回的 `tool_approval` 为准，旧桌面状态不会覆盖后端策略。

输入区常驻权限入口，提供两个选项。

- **Full Access** 对应 `allow_always`，自动批准已启用的工具。工具禁用规则和文件访问范围仍然有效。
- **Ask** 对应 `ask`，按照服务配置，对受控操作请求确认。显式选择 Ask 会清除该会话先前针对工具保存的“始终允许”和“始终拒绝”缓存，即使当前模式已经是 Ask。

已有会话只允许在空闲时切换。切换经桌面、Waku daemon 和 ACP 的 `session/set_config_option` 请求完成，并核对后端实际返回值；收到确认后才更新界面与本地状态。等待确认期间禁止继续提交任务，失败会提示，后端状态未确认时不会放行新提示词。没有运行时的历史会话会先恢复连接，无需发送额外聊天消息。

服务返回 `off` 时，入口显示服务关闭审批，两项切换均不可用，并提供“工具与权限”设置入口。恢复的 `reject_always` 显示为“拒绝受控操作”，用户可在空闲时显式选择 Ask 或 Full Access。DeerFlow 的规划模式与工具审批保持独立。

桌面与 Waku daemon 的内部协议升级至 **v9**，新增 `setToolApproval` 请求、`toolApproval` 确认响应及 `toolApprovalChanged` 状态事件。DeerFlow 对外使用的 **ACP v1 保持不变**。

本次交付为**完整的新便携目录** `dist/desktop/DeerFlow-Desktop-20260921-approval-fix`。桌面、Waku daemon、ACP bridge 和 Python 运行时随整包交付；Windows x64 便携包与对应源码包的文件名列于下方。

迁移时先结束任务、停止旧目录的 ACP 服务，并退出旧桌面及其 Waku daemon。保留旧目录作为备份，将旧目录中完整的 `user-data` 复制到新解压的便携目录，再从新目录启动 `deerflow-desktop.exe`。这样迁移配置、会话历史、记忆和桌面偏好；用户原有工作目录保持原位。新旧桌面与 daemon 不应混用，也不要仅替换旧目录中的 EXE。

本轮新增与扩展的回归用例覆盖默认模式、后端真实策略恢复、确认失败与延迟响应、Ask 缓存清理及跨会话隔离。便携包 smoke 已验证真实 `write_file` 的自动批准、拒绝不写、允许一次写入和重启后恢复。该 smoke 使用真实 daemon、bridge 与 Python 运行时，模型服务由本机模拟，测试数据写入仓库内隔离目录。

已核对通过的验证如下。

- Python 回归合计 **145 passed**，由 **119 + 26** 两组组成。
  - `.venv/Scripts/python.exe -m pytest tests/test_local_acp_agent.py tests/test_local_acp_policy.py tests/test_local_acp_runtime.py -q`，**119 passed in 27.08s**。
  - `.venv/Scripts/python.exe -m pytest tests/test_local_acp_daemon.py tests/test_portable_management.py tests/test_local_acp_cleanup.py -q`，**26 passed in 20.22s**。
- `waku-protocol` **74 tests passed**。
- `export_types --check` 通过。
- 最终桌面与 daemon 的 Release 构建成功，耗时 **24m 47s**。在 `desktop-app` 目录执行的实际命令为 `cargo build --offline --locked --release --target-dir ../.build-cache/desktop-build -p waku --bin waku -p waku-daemon --bin waku-daemon`。新便携目录中的 `deerflow-desktop.exe`、`waku-daemon.exe` 已更新，文件 hash 与本轮生成的 `waku.exe`、`waku-daemon.exe` 分别一致。
- 新便携目录中的 wheel 版本为 **`0.0.4.dev28+g019997c5e.d20260921`**。包内 `deerflow/acp/agent.py`、`runtime.py`、`permission.py` 三个 Python 模块的文件 hash 均与当前源码一致。
- 已检查新便携目录中**没有 `user-data`**，正式包不包含用户配置和历史。

- `waku-core` 的 ACP 回归 **35 passed、0 failed、3 ignored**。三项跳过的测试依赖外部已安装、已登录的 Cursor、Grok、Kimi CLI；本轮 DeerFlow 权限用例全部通过。实际命令为 `cargo test --offline --locked --release --config 'profile.release.package.waku-core.opt-level=1' --config 'profile.release.package.waku-core.codegen-units=16' --target-dir ../.build-cache/desktop-build -p waku-core --lib driver::acp`。该命令仅对本包单测临时覆盖优化参数，交付 EXE 使用上述默认 Release 设置。Rust 两组测试合计 **109 passed**。
- 完整便携包隔离 smoke **通过**，退出码为 **0**，报告 `ok=true`、`cleanup_errors=[]`。实际命令为 `.venv/Scripts/python.exe scripts/test-deerflow-desktop-smoke.py --package dist/desktop/DeerFlow-Desktop-20260921-approval-fix`。报告位于 `.build-cache/desktop-smoke/20260921-163848-71639875/logs/report.json`。
- smoke 使用真实 Waku daemon、ACP bridge、Python 运行时及 `write_file`，通过本机模拟模型触发操作，核对批准后的文件内容和拒绝后的文件不存在。验证涵盖 Full Access、Ask 拒绝/允许一次、两类 always 缓存清理、配置应用后相同会话与历史恢复、旧 Full Access 偏好不覆盖后端 Ask、删除持久化，以及停止服务后模型发现不会重启服务。

交付文件位于 `dist/desktop/`。

- 完整便携目录 `DeerFlow-Desktop-20260921-approval-fix/`，入口为 `deerflow-desktop.exe`。
- Windows x64 便携包 `DeerFlow-Desktop-20260921-approval-fix-windows-x64.zip`。
- 对应源码包 `DeerFlow-Desktop-20260921-approval-fix-source.zip`，包含本轮修改、构建与测试脚本，以及与已安装 wheel 一致的 `BUILD_VERSION.txt`。

关键原生文件 SHA256 如下。

- `deerflow-desktop.exe`：`B7650D00EA3EEC78544B4C3CFC15C0A149020A0E9A8CC7E5EACCE3645F86FCE1`。
- `waku-daemon.exe`：`FAFE34AAFB1B73CFBE67FB32267CDBCEE45DB5043E5D7897A901969079A198D9`。

本轮未进行 GUI 视觉测试，也未向真实外部模型服务发送测试请求。
