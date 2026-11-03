# Gentoo 持久数据库绑定发布

本次发布把启动、容器重启和 Waku 配置 apply 统一到现有的
`/var/lib/deerflow/user-data/config/go-harness-state/harness.db`。
数据库选择依据是发布前在线 daemon 返回的 `runtime.data_dir`。

发布使用 `6586c1f074141908913344411d20ad516901ae86` 的源码，保留 Gentoo
已经部署的 Ready 缓存修复。该覆盖仅涉及
`desktop-app/crates/waku-core/src/daemon.rs`，SHA256 为
`527541a4cc6a5065b07aedfdb0ceae92c2d491f48c0453af75253389261d3084`。
其他未提交的本地源码及用户技能未加入镜像。完整文件清单和源码归档保存在服务器发布目录。

## 镜像与数据

- 发布目录：`/home/gen/deerflow-deploy/storage-binding-20261103`。
- 原镜像：`sha256:90e46bb01bd79c2d79d330190e419b798417e630f21f606079d9a5defa210cf9`。
- 候选标签：`deerflow-server:storage-binding-20261103-6586c1f0`。
- 已部署镜像：`sha256:475c63fa112b79806c1679b0ded9f90b8dc347ffc83f38cdf5207c5b9ff9a65b`。
- 原生构建标识：`storage-binding-20261103-6586c1f0`。
- Waku protocol 保持 12，现有 FoloToy 客户端无需更新。
- 更新原生 daemon、Go stdio、Rust Bridge、Waku daemon 和 wrapper；继承原镜像的依赖及其他工具。
- 宿主机 `runtime/deerflow-acpd-wrapper.sh` 是只读挂载，须与镜像内 wrapper 同时更新。
- `.env` 仅更改 `DEERFLOW_IMAGE`，使用验证后的不可变镜像 ID。
- 配置内容、挂载、端口和其他容器保持原状。

`data/user-data/config/runtime-data.json` 保存：

```json
{"data_dir":"config/go-harness-state"}
```

两份数据库均保留原位置。发布不迁移、合并、删除数据库，不以新会话代替历史会话。

## 执行与验证

这些脚本对应上述固定部署，不能直接作为其他版本的通用更新命令。

1. 准备固定提交的源码快照（含 `desktop-app/db/migrations`、locales、`docs/fixtures` 和嵌入资源），
   加入上述已部署覆盖，并离线 vendor Rust 依赖。清单记录每个源码文件及归档 SHA256，
   `executable_files` 记录该提交 `git ls-tree` 中的可执行文件，避免 Windows 归档丢失执行权限。
2. 使用 Go 1.26.8、`GOOS=linux GOARCH=amd64 CGO_ENABLED=0` 生成原生程序及
   `runtimepaths`、`launch`、`desktopconfig`、daemon 测试程序；清单记录各程序 SHA256。
3. 上传源码、Go 归档、清单、候选 wrapper 和本目录脚本。清单另记录仓库中
   `skill-learning-recovery-20261028/release.py` 及候选 wrapper 的 SHA256。
   该辅助脚本提供既有的备份、健康检查及管理协议原语。
4. 运行 `python3 build.py`，在固定 Debian Rust 1.96 镜像中离线构建，限制 CPU 和内存。
   Go 集成测试使用本次真实构建的 Bridge、daemon 和 stdio 程序。
5. 依次执行 `python3 release.py prepare`、`build-image`、`stage`。
   stage 使用无网络、无生产数据挂载的合成 profile，验证同一会话经过容器重启及配置 apply
   后仍可恢复，并检查另一数据库的标记字节不变。
6. `python3 release.py apply`：检查空闲与后台维护任务、排空、备份、仅停止
   Compose 服务 `deerflow`（90 秒宽限期），保存绑定并用只读挂载预检候选程序后更新。
7. `python3 release.py restart`：在空闲时执行普通容器重启并复验。
   `python3 release.py verify` 可单独检查已部署结果，不发送模型提示。

生产验收检查实际目录、程序哈希、健康状态、protocol 12、配置 apply、原生历史会话恢复，
并逐表检查原记录主键。历史输入及事件内容也逐条比对；备用目录的存在状态、完整文件集合
和哈希必须不变。其他容器的 ID 和启动时间必须不变。

本地与 Gentoo 均运行 `python -m unittest discover -s . -p test_release.py -v`，
覆盖目录越界、挂载身份、备用库意外新建、历史丢失/篡改、空恢复检查、忙碌重启及回退。
历史抽查使用原生表的空字符串时间戳，排除已关闭、已归档和后台子会话；该筛选也有回归测试，
并在停服前确认有可恢复的历史会话。

## 备份与回退

备份位于发布目录 `backup/`：原 `.env`、Compose、配置、wrapper、完整 `user-data`
与 skills 归档，以及通过 SQLite backup API 生成并检查完整性的原生库和桌面库。
这些文件留在服务器，验收报告不包含密码、令牌或业务文本。

`python3 release.py rollback` 只恢复原镜像选择和 wrapper，保留已验证的目录绑定。
**回退不会用旧数据库或 skills 归档覆盖更新后的数据。** 如果存在活动任务则推迟回退并恢复接收请求。
隔离测试失败会保留日志和 profile，便于排查。

若已经完成回退，`python3 release.py retry-prepare` 会核对原镜像、配置、挂载、绑定和记录保留情况，
将上次发布备份与元数据归档到 `previous-attempt-*`，再建立新的发布基线。它只移动本次发布目录中的文件。

本次第一次生产验收因抽查脚本将空字符串误作 SQL NULL 而回退。原镜像恢复健康，业务数据未恢复自旧备份。
修正并测试筛选后，先在原服务上恢复两个原生 ID，再完成重新部署。第一次完整备份仍保留于
`previous-attempt-1lj08pak/backup/`，最终部署备份位于 `backup/`。

## 最终结果

2026-11-03 已完成部署、实际配置 apply 和一次普通容器重启，状态为 healthy，未排空停留。
两次恢复检查都使用原有的两个原生会话 ID；验证未提交模型提示。

- Rust：188 通过、0 失败、3 忽略（原有外部认证工具测试）。
- Go：150 个顶层测试通过，含真实 Linux Bridge、daemon、stdio 及 Shell 启动脚本联合验证。
- 发布保护：Windows 和 Gentoo 上各 9 项通过。
- 原始记录保留：72 个会话、146 条执行记录、9,011 条事件、146 条输入、285 条工具回执、
  3,260 条 Eino 会话事件；桌面 1 个会话、4 条消息。历史输入、事件和桌面消息内容比对无变化。
- 备用数据库目录、原挂载/端口和其他容器的身份及启动时间保持不变。

脱敏证据：[构建](reports/build-report.json)、[隔离验证](reports/stage-report.json)、
[线上更新及配置应用](reports/deployment-report.json)、[普通重启](reports/restart-report.json)、
[首次备份至最终状态的核验](reports/final-audit.json)、[程序来源](reports/source-provenance.json)。
仓库报告将原生会话 ID 替换为计数和摘要；服务器保留完整本地验收记录。
