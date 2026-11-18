# Gentoo skill_edit 参数修复发布（2026-11-17）

Gentoo 的 DeerFlow 已更新为 `skill-edit-arguments-20261117-68753f15`，
镜像为 `deerflow-server:skill-edit-arguments-20261117-68753f15`，
镜像 ID 为 `sha256:e639a3b8ca245fa69061b097cc27d1aca66c30abae82b435ac33e41ed00cd7eda`。
清理后的检查为 healthy、零重启、未处于排空状态，数据库 quick_check 为 ok。

此次在服务器实际运行的 `68753f15e51ef285260bca52388c44c786ff0f41` 上应用
与桌面修复相同的 8 个文件补丁，通过独立 worktree 使用 Go 1.26.8 构建 Linux/amd64 程序。
补丁 SHA-256 为 `9ed971d80016b3d767549c03b91a89d89610f5cee85ee0b6789ad289b73b003c`。
镜像标签明确记录基线提交加补丁，以及实际源码清单的摘要。
Windows checkout 的 CRLF 与已审核 LF 文件逐一归一化比较，逻辑内容一致；
清单同时记录归档文件摘要和已审核 LF 摘要。

修复明确了 prepare、apply、status 的不同参数要求。apply/status 多传 name 时，
错误提示说明应删除 name/edits 并复用原 edit_id，回执分类为
`invalid_skill_edit_arguments`。审批要求和拒绝记录的参数脱敏保持原有约束。

- Linux 共 71 个顶层测试、165 项测试及子测试通过，零失败、零跳过。
  包含 apply/status 多传 name 后纠正重试、错误脱敏、技能编辑、回执分类及 daemon/bridge 检查。
  见 [构建报告](reports/build-report.json) 和 [Linux 测试日志](reports/linux-tests.log)。
- 隔离容器使用独立配置和数据库副本，网络为 none，未挂载生产数据。
  启动、重启、两个原有会话恢复、WebSocket 配置应用及管理接口均通过。
  见 [隔离验证](reports/stage-report.json)。
- 生产验证通过 WebSocket、原生会话恢复及管理接口，未提交模型提示或发送渠道消息。
  92 个会话、158 条运行和输入、10,066 条事件、309 条工具回执、3,512 条 Eino 事件、
  49 条资源记录和全部候选/维护记录保留，所核验历史内容无变化。
  原有 7 条 needs_reconciliation 记录保留。
  见 [部署报告](reports/deployment-report.json)。
- 18 个其他容器的身份、状态和启动时间均保持一致。
  见 [最终审计](reports/final-audit.json) 和 [本机 Tailscale 连通性](reports/tailscale-connectivity.json)。

配置更新仅将 `/home/gen/deerflow-deploy/.env` 的 `DEERFLOW_IMAGE` 固定到新镜像 ID。
此次无需新增配置项或数据库迁移；模型、搜索凭据、渠道、Compose、wrapper、端口及技能文件保持一致。
主库继续绑定 `/var/lib/deerflow/user-data/config/go-harness-state`。
已有 skill-evolution-v2 迁移日志和输入水位 33873 保持一致。

发布目录为 `/home/gen/deerflow-deploy/skill-edit-arguments-20261117-68753f15/`。
其 `backup/` 保存停机后一致性数据库备份、完整 user-data/skills 归档，以及原配置、环境和 Compose。
含私有数据的备份仅保留在服务器。回退镜像 `deerflow-server:git-68753f15e51e` 保留。
回退只切换程序及兼容配置，不将旧数据库覆盖到业务数据上。

已清理本次构建容器、隔离容器、build/、stage/、image-context/、上传归档、
Go 程序归档及临时镜像别名，共 1,478,291,446 字节（约 1.48 GB）。
清理前核对了路径边界、容器 ID、退出状态、镜像与挂载；清理后生产容器和 daemon 均未重启。
备份、源码归档、补丁、发布记录、当前和回退镜像保留，未执行全局 prune。
见 [清理报告](reports/cleanup-report.json)。

本目录保存实际发布脚本与不含凭据的报告。release.py/build.py/cleanup.py 是此次发布的状态相关脚本，
不应直接用于另一版本或再次初始化。服务器可运行 `python3 -B final-audit.py` 复核清理后的状态。
