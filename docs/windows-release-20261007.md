# Superseded scope (2026-10-11)

This is a historical release record. Runtime backup/restore and the state CLI
have since been removed from the MVP; references to the old backup manual below
describe that historical package only. See `mvp-closure-20261011.md` for current
scope. Existing user data and backups are not deleted.

# Windows x64 Release 记录

日期为 2026-10-07，交付范围为本地单用户 Go/Eino harness、Rust ACP Bridge 和 Waku 桌面便携包。

## 稳定性修复

预算监控的只读任务树停止查询与业务事务共享 SQLite 单连接。此前该查询的 1 秒 deadline 超时会直接取消健康任务，即使外层任务时限尚未到期。新连接占用回归测试稳定复现了 `context deadline exceeded` 误取消。

现在仅在监控查询自身 deadline 到期且调用者尚未取消时，等待下一轮查询。实际任务树停止、调用者取消、数据库故障和预算心跳错误仍保留取消与错误处理；模型和工具的预算授权仍通过事务核验，没有延长生产超时或放宽预算限制。回归测试等待第二次连接池查询，以证明监控已实际经历一次超时，并验证连接释放后仍能观察到持久化任务树停止。

工具失败回执的持久化白名单补齐 `rate_limit` 和 `remote`，保持与 Eino 工具分类及技能质量归因一致。测试通过实际回执写入和历史查询确认这两种环境故障保留分类，同时验证未知分类不会进入回执。该修复没有改变工具效果证据或重试规则。

## 构建与验证

此工作区仍有未提交功能。本地交付使用独立冻结源码仓库及逐文件 SHA-256，将包含必要的未提交源码；不会提交或清理用户工作区。构建使用原工作区缓存，编译后再次核对所有输入字节、文件集合和基础提交，确保对应源码 ZIP 包含相同构建输入。未参与 Go/桌面构建的旧 `integrations/` 适配器不进入源码快照。

本次交付须依次通过以下检查，任何失败均阻止归档：

- Go 依赖校验与完整 `go test -count=1 -p=2 -json -timeout=15m ./...`。
- 原续跑失败选择器重复 20 次，保留原有断言与时限。
- Rust Bridge 的锁定依赖 Release 测试。
- Windows 打包失败门禁和源码身份 fixture。
- Windows 桌面、daemon、Bridge、五个 Go 工具和 WSL helper 的实际 Release 构建。
- 包内 Bridge 测试及普通、旧 MCP 策略两种无界面桌面协议 smoke。
- ZIP 中每个交付文件的 SHA-256，以及私有配置和用户数据路径检查。

最终实际结果、工具链和来源身份以包内 `BUILD_PACKAGE.json`、`BUILD_SOURCE.json` 和 `MANUAL_SOURCE_SNAPSHOT.json` 为准。无界面协议 smoke 使用隔离数据和本地模型 fixture；真实线上模型、GUI 绘制及另一台干净 Windows 环境的验收单独记录。

## 使用与升级

完整解压到新目录后运行 `deerflow-desktop.exe`，在设置中配置模型及 API Key。首次启动创建 `user-data`，ZIP 不包含个人配置、密钥和历史数据库。

更新已有便携安装时，先关闭程序并按 `resources/go-runtime-backup.md` 备份，再将 `user-data` 复制到新目录。保留旧安装及备份以便回退。自动技能进化、命令执行和子代理沿用当前默认策略，需要相应能力时显式启用。

本地生成 ZIP 和配套源码、校验文件，不会创建 Git 标签、推送源码或发布 GitHub Release。
