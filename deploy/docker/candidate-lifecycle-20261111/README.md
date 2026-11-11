# Gentoo 候选生命周期更新（2026-11-11）

已将 Gentoo 的 `deerflow-deploy-deerflow-1` 更新至提交
`3722ca94e939e5b67e28957e705caad07a51b05c`。镜像标签为
`deerflow-server:git-3722ca94e939`，镜像 ID 为
`sha256:8394da1e6d8beab4883f4268189bf80bd79ea23e514af1238ade8dbb7f796bbc`。
运行时构建标识为 `git-3722ca94e939`；发布和清理后的检查均为 healthy，
最终审计为零重启、未处于排空状态。

本次更新使管理端的候选列表、历史和详情返回生命周期、状态版本、取消资格和观察信息，
修正已取消或失败候选仍被显示为活动状态的问题，列表不再包含完整 diff。
生产源码相对上次部署仅变更 `candidate_progress.go` 和 `management_proposals.go`。
从干净提交通过 `git archive` 打包，使用 Go 1.26.8、CGO 关闭构建 Linux/amd64
的 daemon、ACP、配置和技能四个 Go 程序。未变动的 Rust 程序、控制工具、
渠道插件、资源和运行依赖沿用上次镜像。

## 验证结果

- Linux 回归共 189 项通过，零跳过：evolution 164、management 13、daemon 12，
  各测试清单均核对完成，见[构建报告](reports/build-report.json)。
- 隔离容器使用独立配置和数据库副本，无网络、无生产目录挂载；重启、
  原生会话恢复、Waku WebSocket 配置应用及生命周期接口均通过。
- 生产原生管理接口与实际 Waku WebSocket `manage` 请求均验证
  `proposal.list`、`proposal.history` 和 `proposal.get`。
  9 个候选分别为 7 个 historical、2 个 rejected，状态版本一致，列表无完整 diff。
- 两个原有会话恢复通过；生产验收未提交模型提示或发送渠道消息。
  本机经 Tailscale 连接 18787 端口成功。
- 74 个会话、146 条运行及输入、8,130 条事件、286 条工具回执、3,266 条 Eino 事件、
  9 个候选、53 条维护任务及 68 条维护调用完整保留；历史输入与事件内容一致。
  桌面库的 1 个会话和 4 条消息保留。原有 7 条 `needs_reconciliation` 运行保留。
  以上为本次更新前后的实际基线。
- 数据库完整性为 `ok`，19 个其他容器的 ID、状态及启动时间保持一致。

## 配置、备份和回退

主库仍绑定 `/var/lib/deerflow/user-data/config/go-harness-state`。
配置、搜索凭据、wrapper、Compose、数据库绑定、挂载和端口均保持一致；
环境文件仅调整镜像选择。本次无需数据库或配置迁移；既有
`skill-evolution-v2` 迁移日志保持不变，输入水位仍为 33873。

发布目录为 `/home/gen/deerflow-deploy/candidate-lifecycle-20261111-3722ca94e939/`。
其 `backup/` 保留原配置、环境、Compose、wrapper、绑定、搜索配置、
完整 `user-data.tar.gz`、`skills.tar.gz` 及一致性 SQLite 备份。
既有非活动数据库归档仍位于
`/home/gen/deerflow-deploy/code-update-20261110-a7a030f95550/backup/inactive-go-harness`，
逐文件哈希一致。含私有数据的备份仅保留在服务器。

`release.py` 复用固定哈希的历史备份和数据校验工具，排空后停止并备份服务，
仅重建 Compose 的 `deerflow`。首次尝试在停机前检测到短暂活动，自动恢复接收请求；
脚本增加最多 45 秒的排空等待后重试成功，见[排空报告](reports/drain-report.json)。
回退恢复旧镜像及兼容配置，不用旧备份覆盖业务数据库。回退镜像
`deerflow-server:git-a7a030f95550` 保留。

## 临时文件清理

已删除本次退出的构建容器、`build/`、`stage/`、`go-artifacts.tar.gz` 和
`image-context/`，合计 871,187,614 字节（约 831 MiB）。隔离验证容器也已移除，
发布目录无 `__pycache__/`。临时镜像别名 `deerflow-server:before-3722ca94e939` 已移除。

清理前核对路径边界、容器完整 ID、退出状态、镜像和挂载；清理后生产容器及 daemon
均未重启。配置、备份、非活动库归档、源码归档、发布日志、当前及回退镜像保持完整。
共享镜像和缓存保留，未执行全局 prune。详见[清理报告](reports/cleanup-report.json)
及[完成核对](reports/completion-report.json)。

本目录保存实际执行的发布脚本及不含凭据的报告；本次记录提交不改变已部署运行代码。
`source-manifest.json` 的 SHA-256 与运行镜像标签一致。
清理后的服务器仍可执行 `python3 -B final-audit.py` 或 `python3 -B release.py verify`
重新审计。`build.py`、发布和清理动作依赖当次目录状态，不应作为可重复初始化脚本重跑。

详细证据：[预检](reports/prepare-report.json)、[隔离验证](reports/stage-report.json)、
[生产数据保留](reports/deployment-report.json)、[最终审计](reports/final-audit.json)、
[Tailscale 连通性](reports/tailscale-connectivity.json)。
