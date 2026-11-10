# 启动、重启与更新的数据目录约定

便携安装的数据库选择由 `user-data/config/runtime-data.json` 保存。所有本地启动器、桌面配置应用及维护脚本读取这份绑定。启动路径不再各自推导不同的历史数据库目录。

```json
{"data_dir":"config/go-harness-state"}
```

`data_dir` 是相对于 `user-data` 的路径。绑定必须指向已有的 `harness.db`，不能逃出该目录。移动整个便携目录后，相对绑定仍然有效。绑定文件大小限制为 8192 字节；未知字段、重复字段和无效 JSON 在原生启动器中被拒绝。

新建、未绑定的便携配置统一使用 `user-data/config/go-harness-state`。首次成功创建数据库时，Go 宿主在运行锁内原子写入 `runtime-data.json`。只读路径预检不创建数据库或绑定。

无绑定但任一已知运行位置已有数据库时拒绝启动；有绑定但数据库缺失时同样拒绝，不能新建替代库。若绑定目录之外的另一已知运行位置仍有 `harness.db`，启动、配置应用和维护预检均报告第二份运行库，要求先将非活动库整体归档到运行目录之外，再从归档恢复技能登记。备份目录不参与运行库发现。Go、Rust 桌面、Bridge 和 PowerShell 使用同一规则。

绑定不负责迁移、合并、恢复或删除数据库。已有绑定继续选择原运行库，升级不得改选另一份库。2026-11-10 单库收敛只恢复经核验的历史已发布技能安装登记；旧会话、消息、执行记录、工具回执和检查点不迁移。当前运行库的数据保留，历史库先退出运行目录并归档，再用只读归档作为技能恢复的证据。不能根据目录名、修改时间或会话数量自动选库。


## 单库技能登记恢复（2026-11-10）

受管来源目录只读盘点发现磁盘包但无安装记录时，目录页和会话提示报告 `installation_missing`；诊断不会生成安装绑定、加载指令或执行脚本。删除记录与候选创建保护优先，不能通过刷新复活技能。`skill_inspect` 和 `skill_manage` 对已授权来源返回明确的可用性原因，其他工作区的文件不向模型暴露。

宿主管理接口 `skill.recovery.plan` 接受完整 `target` 和绝对 `history_database` 路径。历史库使用 SQLite `mode=ro`，校验当前发布头、已应用文件回执、已发布提案及文件清单；当前目录必须完整匹配发布版本。该接口不适用于需要恢复原观察范围和自动采用状态的自动候选。

`skill.recovery.apply` 接受同一目标/历史路径，以及计划中的 `expected_version`、`expected_receipt_sha256`、`enabled` 和唯一 `request_key`。缺失登记只能恢复一次；源文件变化、目标已有记录或存在删除/创建保护时拒绝。登记、来源审计和幂等回执原子提交，文件内容不重写，旧库会话和后台任务不读取或迁入。重复请求可在历史库已归档后返回原回执；后续删除或修改不会被旧请求覆盖。

恢复后通过 `skill.verify` 核对目标会话的下一轮版本。运行中的快照不变，普通新一轮才获得恢复后的技能。旧库和对应不可变对象目录作为整体归档，禁止只删除主数据库而留下 WAL 或重新启用备用路径。

2026-11-10 本机已完成单库更新，运行构建为 `deerflow-single-runtime-20261110`。绑定继续指向 `user-data/config/go-harness-state`，第二份运行库整体移至 `.build-cache/desktop-deploy-backups/single-runtime-20261110-history/go-harness`，不参与启动发现。当前安装和程序的完整回退备份位于 `.build-cache/desktop-deploy-backups/single-runtime-20261110`。

宿主依据归档中的已发布版本及现有文件，恢复了 `hugo-publisher`、`huaweicloud-sandbox`、`show-me` 的登记，技能总数由 23 增至 26。恢复请求跨重启重放保持幂等；原失败会话的下一轮已通过精确版本及正文可读性核验。此操作没有发布图片技能草稿，也没有改变 98 个既有技能文件或运行配置。

原有 74 个会话 ID、12 条执行记录、5,036 条事件、215 条工具回执和 995 条会话事件全部保留；旧库独有的 101 个会话均未导入。桌面启动新增了两个没有消息的已关闭会话，并归档了一个既有空会话。数据库完整性检查通过。验收记录位于 `.build-cache/single-runtime-20261110/verification.json`，构建与测试记录位于同目录 `implementation.json`。

本次验证包括 Go `runtimepaths`、`skills`、`launch`、`desktopconfig`，原生技能诊断及编辑测试，16 项桌面目录测试，Bridge 路径测试，PowerShell 路径和更新脚本，以及真实 Bridge/daemon/stdio 的归档后启动、重启和同一会话恢复。本机更新后再次重启，运行库路径和三个恢复技能均保持不变。

完成验收后，清理了 `.build-cache` 内闲置的桌面/Go 编译缓存、本次临时安装包及重复二进制，共释放 27.57 GiB；保留回退备份、旧库归档、源码基线和验收记录，详见同目录 `cleanup.json`。

## 入口检查范围

| 入口 | 目录规则与验证 |
| --- | --- |
| 桌面启动、Waku 自动启动后端、ACP 会话连接 | 读取绑定并向子进程传递同一目录；复用服务前检查运行服务的目录。 |
| 桌面配置 apply | 停止旧服务前验证身份；新服务启动后再次核对，不能通过 apply 改开另一个库。 |
| Rust Bridge 的启动、代理、状态和管理操作 | 从明确的配置/便携根识别 profile，验证绑定及 endpoint 的 `data_dir`；默认 IPC 使用 `runtime/acp-go`。 |
| 直接运行 Go daemon、Go stdio agent | 使用共享 `runtimepaths`；便携启动不依赖当前工作目录；显式目录必须匹配绑定。 |
| `start.bat`、`acp-daemon.ps1`、`acp-service.ps1`、`start.sh`、`daemon.sh` | 委托 Bridge；restart 在停止服务前验证目录，错误时保留正在工作的服务。 |
| `update-deerflow-desktop-go.ps1` | 停服前运行候选 daemon 的 `--check-paths`，拒绝不支持绑定的旧包；检查空闲、排空、完整备份，再替换；重启明确传递目标 profile 并核对实际目录。GUI 不继承更新命令的输出句柄。 |
| `update-deerflow-bundled-wechat.ps1` | 从同一绑定读取目录；更新插件登记前核对运行目录和实例身份。 |
| Windows Inno 安装/覆盖升级/卸载 | 保留原安装位置；不包含、不删除 `user-data`；有运行进程时拒绝升级或卸载。 |
| Windows/Linux 打包入口 | 使用新输出目录，拒绝覆盖已有安装；新包不携带用户数据。生成的说明区分绑定路径与新配置默认路径。 |
| Docker 服务入口和容器内启动 | 继续使用持久卷中的 profile；复制程序不覆盖绑定。Bridge/Go/Waku 使用同一目录规则。 |
| skills-runtime wrapper、wrapper 安装器和镜像构建 | wrapper 不再自行默认到历史路径；原生能力预检失败则不启动。安装器修改文件前验证已有绑定、候选路径和运行目录；镜像构建拒绝不支持绑定的基础镜像。 |

Bridge 的 stop 允许停止目录不匹配的服务，供人工恢复使用；它不会启动任何数据库。显式指定独立配置文件的非便携用法继续保留原有 `config` 同级 `go-harness-state` 默认值及显式目录参数。

桌面上游 Waku 自动更新在本项目 identity 中保持禁用。当前维护入口是 DeerFlow 打包、安装器及上述更新脚本。历史按日期命名的 Docker rollout 脚本对应其当时固定的镜像和配置，不能作为新版本的通用更新入口。

## 历史回归验证（2026-11-03）

- Rust Bridge：19 项单元测试及 2 项启动集成测试通过。
- 桌面目录绑定：16 项测试通过；原“本轮已完成”问题的 ACP 生命周期测试为 71 通过、3 忽略。
- Go：`runtimepaths`、`launch`、`desktopconfig`、daemon 及 stdio 编译/测试通过。配置重载测试使用各自隔离的便携目录，并继续验证历史记录保留。
- 真实 Windows 可执行文件：Bridge 创建会话，PowerShell restart、直接 daemon 和直接 stdio 均能恢复同一原生会话 ID；错误的环境目录、伪造 endpoint 目录和丢失绑定均被拒绝，失败的 restart 不会停止原服务，另一数据库的标记字节不变。
- 安装器：实际编译、静默安装、忙碌拒绝、同目录覆盖升级及卸载，共 76 项检查通过；绑定及两份数据库的字节保留。
- 维护脚本：目录校验、旧包拒绝、程序哈希、用户数据保留和构建入口检查通过。
- Linux：原生路径测试与 3 项 wrapper/维护测试通过，包括实际编译 daemon 的预检、冲突目录、丢失绑定及旧二进制拒绝。预检不创建 SQLite 文件或改动两份库。

本机存在用户自行添加的 `skills/custom/oss-attachments`，会改变仓库自带技能数量测试的输入。Go 完整相关套件在隔离的源码快照中运行，保留原断言，未删除用户技能。该快照包含当前已存在的其他受跟踪本地修复；程序来源记录为 working-tree snapshot，不能仅用旧 Git HEAD 代表。

## 本机验收与 Gentoo 部署结果

2026-11-03 本机安装已更新并重启。实际运行目录仍为 `user-data/config/go-harness-state`，原生构建标识为 `data-binding-20261103`。两个当前保留的桌面会话在更新及桌面配置 apply 后，均通过真实 `session/resume` 恢复原生 ID 并加载历史。已由用户完成删除的会话不重建。配置、消息、检查点、执行记录及回执的保留检查通过，备用数据库目录文件的哈希不变。

本机验证材料位于 `.build-cache/entrypoint-audit-20261103/`；完整备份位于 `.build-cache/desktop-deploy-backups/entrypoints-20261103/` 和 `entrypoints-20261103-detached/`。

2026-11-03 通过用户提供的 Tailscale 地址完成 Gentoo 更新。现场核对表明，服务端及 FoloToy
客户端原本都已使用 protocol 12。此次基于现有镜像保留运行依赖，更新为
`deerflow-server:storage-binding-20261103-6586c1f0`，不可变镜像 ID 为
`sha256:475c63fa112b79806c1679b0ded9f90b8dc347ffc83f38cdf5207c5b9ff9a65b`。

实际使用的仍是原 `user-data/config/go-harness-state/harness.db`，并已保存持久绑定。
镜像内 wrapper 与宿主机只读挂载的 wrapper 同时更新；Compose 和业务配置保持原字节，
`.env` 仅更新镜像 ID。只重建 `deerflow` 服务，原挂载、端口及其他容器保持不变。

Linux 验收为 Rust 188 通过、3 忽略，Go 150 通过；发布保护测试 9 项通过。
候选镜像先在无网络、无生产数据挂载的容器中完成重启、配置 apply 和原会话恢复。
上线后，实际配置 apply 和一次普通容器重启均通过；两个原生历史会话 ID 均可恢复。
72 个会话、146 条执行记录、9,011 条事件、285 条工具回执和桌面消息均完整保留；
历史输入及事件内容比对通过，备用库文件集合和哈希不变。

Gentoo 的程序来源为固定提交 `6586c1f0` 加上已经部署的 Ready 缓存修复，
未混入其他未提交源码。完整来源清单、备份和运行日志保存在服务器发布目录
`/home/gen/deerflow-deploy/storage-binding-20261103/`。
最终部署备份为 `backup/`；一次因验收脚本筛选错误触发的回退备份保留于
`previous-attempt-1lj08pak/backup/`。回退只恢复镜像和 wrapper，未用旧备份覆盖业务数据。

发布脚本、回退说明及脱敏验收报告见
[Gentoo 持久数据库绑定发布](../deploy/docker/storage-binding-20261103/README.md)。
