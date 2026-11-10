# Gentoo 已提交代码更新（2026-11-10）

已将 Gentoo 的 `deerflow-deploy-deerflow-1` 更新至提交
`a7a030f95550814842f97848170cbc3472ce51db`。镜像标签为
`deerflow-server:git-a7a030f95550`，镜像 ID 为
`sha256:1db117c1db1e1fbfc689a0e819411ca7d5c588a5129f7b82659348dd43d9c4c5`。
运行时构建标识为 `git-a7a030f95550`，最终检查为 healthy、零重启、未处于排空状态。

源码从干净提交通过 `git archive` 打包，Go 程序使用 Go 1.26.8 交叉编译，
Rust Bridge 与 Waku daemon 在 Gentoo 的无网络构建容器中编译。
镜像更新全部 Go/Rust 运行组件及默认配置，包含微信渠道插件；插件保持未启用状态。
基础镜像中的 Python、Node、OpenCLI 等既有执行环境、入口和健康检查沿用原版本。
模型配置、搜索凭据、挂载、端口及数据库绑定均已核对。

## 检查结果

- 本机提交前检查：Go 57 个包、1,770 项顶层用例通过；Rust Bridge 21 项、
  Waku Core 574 项、桌面 708 项通过，并完成相关脚本和竞态检查。
- Linux 回归按测试清单逐项检查完成：Go 527 项通过、3 项跳过，
  Rust 191 项通过、3 项忽略，见[构建报告](reports/build-report.json)。
- 隔离容器使用数据库与配置副本，无网络、无生产目录挂载。
  升级迁移、容器重启、原生会话恢复及 WebSocket 配置 apply 均通过。
- 生产保留原有 75 个会话、153 条运行与输入、9,865 条事件、289 条工具回执、
  3,312 条 Eino 事件、9 个技能提案及全部维护记录；历史输入和事件内容比较一致。
  桌面库的 2 个会话、18 条消息均保留。
- 原有 7 条 `needs_reconciliation` 运行仍然保留。生产验收未提交模型提示或发送渠道消息。
- 19 个其他容器的 ID、状态和启动时间一致。数据库完整性为 `ok`，
  WebSocket 协议 12 握手及两个原有会话恢复通过，本机经 Tailscale 连接端口 18787 成功。

## 数据迁移和备份

主库仍绑定 `/var/lib/deerflow/user-data/config/go-harness-state`，
`runtime-data.json` 字节保持一致。新版自动迁移旧 `skill_evolution` 配置；
其他配置语义及搜索凭据保持一致。配置迁移日志与数据库的 `skill-evolution-v2`
迁移身份一致，输入水位为 33873，隔离重启和配置应用后保持一致。

新版拒绝在同一 profile 下存在第二份运行数据库。停止服务、完整备份后，
将原非活动目录 `data/user-data/data/go-harness` 整体重命名至发布备份目录，
逐文件核对文件集合和 SHA-256 一致。没有选择或合并非活动数据库。

Gentoo 发布目录：`/home/gen/deerflow-deploy/code-update-20261110-a7a030f95550/`。
其 `backup/` 保存原配置、环境、Compose、wrapper、绑定、搜索配置、
完整 `user-data.tar.gz`、`skills.tar.gz`、一致性 SQLite 备份以及
`inactive-go-harness/` 非活动库归档。备份含私有数据，仅保留在服务器。

## 发布步骤与证据

`build.py` 启动独立的离线构建及回归容器。`release.py` 依次执行
`prepare`、`build-image`、`stage`、`apply`，检查镜像与配置基线、排空并备份后，
仅重建 Compose 的 `deerflow` 服务。发布脚本复用已固定哈希的历史备份与数据校验工具。
`source-manifest.json` 描述打包时的来源和校验值，其中的早期连接状态是历史元数据；
实际发布结果以 `reports/` 为准。

本目录是此次发布的可审查记录，运行代码固定为上方提交；后续的记录提交不改变已部署程序。
重新审计可在服务器发布目录执行 `python3 -B final-audit.py` 或 `python3 -B release.py verify`。
`rollback` 仅在排空且配置符合本次迁移结果时恢复原镜像及兼容配置，保留数据库和归档；
不会用旧备份覆盖业务数据库。执行回退前应重新检查新产生的运行状态是否兼容旧程序。

详细证据：[本机验证](reports/local-validation.json)、[预检](reports/prepare-report.json)、
[Linux 构建](reports/build-report.json)、[隔离验证](reports/stage-report.json)、
[生产数据保留](reports/deployment-report.json)、[最终审计](reports/final-audit.json)。

## 本次临时文件清理

2026-11-10 已按用户要求清理本次构建容器、`build/`、`stage/`、
`go-artifacts.tar.gz`、`image-sg5xyt4g/` 和 `__pycache__/`，
删除 1,454,404,062 字节临时文件（约 1.35 GiB）。
同时移除临时别名 `deerflow-server:before-a7a030f95550`，原回退镜像仍保留。

清理前核对容器完整 ID、退出状态、镜像和挂载，逐个验证删除路径位于本发布目录，
且不与其他容器挂载重叠。运行容器与 daemon 均未重启，服务仍为 healthy；
配置、完整备份、非活动库归档、源码归档和验收日志校验一致。
原有共享构建镜像和缓存保留，未执行全局 prune。
执行脚本见 `cleanup.py`，完整结果见[清理报告](reports/cleanup-report.json)。
