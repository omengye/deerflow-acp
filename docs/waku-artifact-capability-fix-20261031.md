# Waku v12 产物能力快照修复

普通 DeerFlow prompt 完成时，ACP 驱动在 `finish_prompt` 前发送 `DeerFlowTasks::Invalidated`。
它表示任务树、执行状态或产物数据需要刷新；底层 ACP 连接和已声明的能力仍然有效。
桌面端也只设置 `refresh_needed`。

原 daemon 在收到该事件后删除 `deerflow_ready`，导致存活 runtime 的 `attachSession` 返回
`runtimeId != null` 但 `deerflowReady = null`。Ready 通常只在 ACP 连接创建时发送，普通对话
完成不会补发，因此远端客户端的重新附加可能缺少能力快照。

`desktop-app/crates/waku-core/src/daemon.rs` 现在仅由当前 runtime 的 Ready 更新快照，
任务数据 Invalidated 不删除它。Start 仍清理旧快照，ProcessExited 按 runtime ID 清理，
旧实例的迟到事件不能覆盖或删除新实例的 Ready。协议版本保持 12，线上的命令及字段不变。

新增回归测试在修复前因正常完成后的 Ready 丢失而失败；修复后验证普通 prompt 的
Invalidated 保留快照、旧 runtime 隔离及实际退出清理。验证命令：

```powershell
cargo test -p waku-core --lib
cargo build -p waku-daemon
```

本地验证（Windows，2026-10-31）：核心测试 563 项通过、0 失败、37 项跳过，
daemon 的 dev profile 编译成功，产出 `desktop-app/target/debug/waku-daemon.exe`。
新增的两个 Ready 生命周期测试均已执行；普通 prompt 的 Invalidated 保留能力测试在修复前失败。

Android 2.2.1 同步保留 Invalidated 后的连接、能力与在途查询，并修复旧版控制检查点中的
Ready 丢失。详细 APK 和测试记录位于 deerflow-app 的 `docs/artifact-fix-validation-20261031.json`。
源代码修复需随 daemon 重新构建、部署并重启后才会作用于已有服务进程。
