# DeerFlow Desktop 设置与会话删除修复

本次修改针对 Windows 原生桌面设置页。

## 行为

- 技能提案点击“确认发布”后直接提交，不再进入第二次确认。请求期间禁止重复操作，保留基础版本冲突校验。
- 完整移除“诊断与恢复”分类，以及报告、日志查看、备份预览和恢复入口。桌面管理服务不再接受 `inspect` / `restore`。会话与记忆管理保留在“长期记忆”。
- 普通保存改为“保存并应用”。桌面管理服务在同一配置写入锁中保存并安排应用，已有任务完成后重启执行服务，随后刷新模型列表。应用失败返回已保存的文档版本，界面保留“重试应用”；取消等待不会撤销保存。“保存并退出”继续只保存。
- 会话删除确认会滚动到可见区域。本桌面的空闲会话自动关闭对应连接并等待服务释放；运行、加载及其他客户端占用的会话给出明确反馈。
- 后端确认清理历史成功后才删除会话记录。桌面等待两处持久化删除确认后更新列表，失败保留可重试入口。重复删除可安全重试，产物文件保留。
- 左侧会话移除改为等待 daemon 确认后更新界面。

## 修改位置

- `desktop-app/src/app/deerflow_settings.rs`
- `desktop-app/src/app/sessions.rs`
- `desktop-app/crates/waku-core/src/deerflow_config.rs`
- `deerflow/acp/management.py`
- `tests/test_portable_management.py`
- `scripts/test-deerflow-desktop-smoke.py`

## 验证

回归覆盖配置校验失败不应用、应用失败保留新配置版本、等待连接释放、其他客户端占用、清理失败保留记录、删除重试及会话列表持久化。隔离 smoke 测试使用本机模拟模型服务，不读取用户配置或真实会话。

已通过 Python 回归 34 项、桌面设置 Rust 回归 15 项、配置应用服务 Rust 回归 5 项，共 54 项。Rust 采用 `--offline --locked --release` 构建，目标目录为 `.build-cache/desktop-build`。

桌面和 daemon 的 Windows x64 Release 构建通过。整包隔离测试通过，验证了无效保存不启动服务、一次保存自动应用到正确版本、应用后续聊保留原上下文、连接中会话拒绝删除、历史删除可重试、旧状态不能重新写入已删除条目，以及 daemon 重启后条目仍不存在。测试退出无进程清理错误。报告位于 `.build-cache/desktop-smoke/20260918-230053-b1f793ec/logs/report.json`。

```powershell
.venv/Scripts/python.exe -m pytest tests/test_config_tool.py tests/test_portable_management.py tests/test_local_acp_cleanup.py tests/test_acp_proposal_control.py -q
# 以下命令在 desktop-app 目录运行
cargo test --offline --locked --release --target-dir ../.build-cache/desktop-build -p waku --lib deerflow_settings
cargo test --offline --locked --release --target-dir ../.build-cache/desktop-build -p waku-core --lib deerflow_config
cargo build --offline --locked --release --target-dir ../.build-cache/desktop-build -p waku --bin waku -p waku-daemon --bin waku-daemon
```

本次不执行视觉测试；原生界面的显示效果仍需在更新后的程序中验收。

## 更新

设置修复补丁必须应用于 2026-09-18 的完整便携版，不能单独运行。先结束任务，在“ACP 运行”中停止服务并关闭桌面程序，再备份原目录，将补丁中的文件按目录结构覆盖到原目录。补丁同时更新桌面程序、内部 daemon 与会话管理 Python 模块；只替换桌面 EXE 不足以启用这次修复。`user-data` 无需替换。

对应源码包以原完整源码包为基础更新以上文件，附带 GPL 许可证与 fork 说明。
