# Gentoo 搜索插件发布

本目录将 Brave、DeepSeek 和 SearXNG 的 Go 搜索后端、配置程序与 Waku 配置接口一起更新。
只重建 Compose 服务 `deerflow`，保留现有挂载、端口、凭据、技能和数据库绑定。
本次发布使用现有镜像为基础，保留已部署的 Ready 缓存修复和运行依赖。

源码快照和 Linux 程序位于本机 `.build-cache/gentoo-search-20261103-204617/`。
服务器发布目录为 `/home/gen/deerflow-deploy/search-plugins-20261103-204617/`。
`source-manifest.json` 记录各文件、归档、测试程序和部署脚本的 SHA-256；
来源为固定提交加搜索插件改动及已部署的 Ready 修复，不包含无关的本地删除会话修改或个人技能。

## 顺序

1. 通过 SSH 读取当前镜像、实际 `runtime.data_dir`、绑定、配置哈希、挂载和活动任务。
2. 固定现有镜像及已有离线 builder 的不可变 ID，上传校验过的源码与 Linux 程序。
3. `python3 build.py` 启动受 CPU/内存限制、无网络、无生产数据挂载的独立构建容器。
   运行 headless Rust、Bridge、搜索后端、配置初始化、数据库绑定和主运行时回归。
4. `python3 release.py prepare` 保存部署基线；`build-image` 检查测试结果并生成候选镜像。
5. `stage` 在独立合成数据目录中验证容器重启、配置 apply、原生会话恢复和搜索切换。
   通过 Waku RPC 配置并切换 Brave → DeepSeek → SearXNG → Brave；同一原生会话调用真实
   HTTP adapter，核对实际请求目标，并验证 daemon 实例及容器启动时间不变。
   HTTP 服务和模型均使用回环地址中的合成 fixture，不使用真实凭据或付费模型。
6. `apply` 确认前台及维护任务空闲、排空请求、完整备份，再只停止并重建 `deerflow`。
   沿用既有 `runtime-data.json` 和宿主机 wrapper，不选择、合并或移动任何数据库。
7. `verify` 核对程序哈希、配置迁移、已有凭据、数据库主键及不可变历史内容；
   检查原有会话恢复、备用数据库及其他容器 ID/启动时间不变。

现有旧 `web_search` 的 Key、代理和超时先持久保存到独立搜索配置，再移除旧字段。
主配置的其他内容必须语义一致，原文件另有精确备份；已有搜索配置及显式清除的凭据优先。
生产搜索接口只轮换已经配置且可恢复的服务，并恢复原默认；自动选择模式保持不变。
三家完整的实际请求验证使用隔离实例，生产验证不注入测试 Key、不提交业务或模型提示。

## 回退

`python3 release.py rollback` 恢复原镜像选择与 wrapper。仅当主配置恰好是本次迁移结果时，
才恢复旧后端需要的兼容字段。忙碌时恢复接收请求并中止操作。
回退不恢复旧数据库、不覆盖技能、不删除新的搜索配置。
完整备份留在发布目录的 `backup/`，含原配置、`.env`、Compose、绑定及数据和技能归档。

## 发布保护测试

```sh
python3 -m unittest discover -s . -p test_release.py -v
```

本目录 9 项保护测试覆盖搜索字段迁移、拒绝无关配置更改、凭据和选择保留、忙碌回退及
回退不覆盖业务数据。Linux 容器测试和线上结果以服务器生成的报告为准。

## 升级后清理

部署及验收通过后，用本次 builder 的完整容器 ID 执行 `cleanup.py --expected-builder ID`。
仅删除本次构建容器、构建目录、合成测试 profile、Go 上传归档和临时基础镜像标签。
保留在线镜像、完整备份、报告及源码归档。清理前后检查绑定、运行版本和其他容器身份。
不执行全局 Docker prune，也不删除旧发布目录或其他容器使用的缓存。

构建固定使用 Go 1.26.8 及其对应 GOROOT，避免系统 Go 版本超出 JSON 依赖支持范围。
Linux 验证另修复了自动通知订阅在数据库检查期间取消时的正常退出竞争；
新增数据库连接等待回归在原实现上复现，修复后通过，并重复验证原有两项通知用例。
此修复不改变数据库结构、配置或凭据。

## 部署结果

2026-11-03 已完成上线及 Gentoo 临时文件清理，服务 healthy、未保持排空状态。
运行镜像为 `sha256:95d806ba2e23832d33a732e17a38acfb0ecd8b9bc22daf2523df677272fc5d79`，
构建标识 `search-plugins-20261103-204617`。数据库仍为原
`/var/lib/deerflow/user-data/config/go-harness-state`，绑定文件哈希保持一致。

- Linux Rust 188 项通过、3 项原有外部认证测试忽略；Go 426 次顶层测试通过，包含重复取消回归。
- 原有 74 个会话、147 条执行和输入、11,122 条事件、299 条工具回执及 3,350 条 Eino 事件均保留；
  历史事件、输入及桌面消息内容比较通过，原生会话恢复检查通过。
- 主配置只迁移旧搜索 Key、代理和超时；实际迁入的新配置凭据已与旧值比较，其他配置保持语义一致。
- Brave → DeepSeek → SearXNG → Brave 的请求路由通过 Waku RPC 和同一原生会话在隔离实例实测；
  daemon 实例及容器启动时间不变。凭据及模型均为合成 fixture。
- 生产搜索接口已验收，原 Brave 凭据保留；DeepSeek Key 和 SearXNG 实例地址保持待配置状态。
  本轮未向三家外部服务提交在线搜索，未向生产配置写入测试凭据。
- 19 个原有其他容器的 ID、运行状态及启动时间保持不变，备用数据库文件集合和哈希不变。
- Gentoo 本次构建/测试容器、构建目录、Go 上传归档、合成 profile 和临时镜像别名已清理，
  释放约 1.2 GB。完整备份、验收报告和匹配源码归档保留。
- Windows 本地构建缓存删除被自动审批策略阻止，明确路径重试仍被拒绝，因此保留本地缓存。

备份：`/home/gen/deerflow-deploy/search-plugins-20261103-204617/backup/`。
脱敏证据见 [构建](reports/build-report.json)、[隔离切换](reports/stage-report.json)、
[数据与凭据保留](reports/deployment-report.json)、[清理](reports/cleanup-report.json)、
[最终审计](reports/final-audit.json)及[配置状态](reports/search-readiness.json)。

## 后续 Windows 搜索配置同步

2026-11-03 已通过运行中的管理接口，将 Windows 当前配置的 DeepSeek 与 SearXNG
配置同步到 Gentoo。两家的实际凭据、服务地址及高级选项一致，运行状态均为已配置；
容器及 daemon 实例未重启，原默认搜索选择、Brave 配置、数据库绑定和 19 个其他容器保持不变。
此操作未发送外部搜索请求。

同步前的搜索配置备份：`/home/gen/deerflow-deploy/search-config-sync-20261103-94e9f748b085/search-providers.before.json`。
最新脱敏结果见 [Windows 配置同步](reports/windows-config-sync.json)；
上方首次部署报告中的待配置状态为同步前记录。
