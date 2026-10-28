# Gentoo 技能挂载与迁移记录

2026-10-24 已完成迁移。Gentoo DeerFlow 注册 **25 个技能，启用 24 个**；Windows 专用 `instabox-exec-windows` 保留文件并停用。

## 挂载与内容

```text
宿主机：/home/gen/deerflow-deploy/skills
容器内：/var/lib/deerflow/user-data/skills
```

Compose 通过 `./skills:/var/lib/deerflow/user-data/skills` 读写挂载，保留原 `skills.path: ../skills`。宿主机目录由服务 UID/GID 1000:1000 管理。原 `data`、工作区、模型配置和 WS 地址保持不变。

来源为本机桌面版 `DeerFlow-Desktop-Go-20261010-103944/user-data/skills`。迁移了 17 个 public 技能与 8 个 custom 技能；其中 5 个采用当前已同步发布包，核对发布 head、文件摘要和启用状态后，转为普通导入包。未复制本机会话、记忆数据库或演化审核历史，也未复活已删除的技能或重复的停用版本。

Gentoo 副本的适配：

- `ulw-plan` 改用 Linux Node 调用和实际容器路径。
- `huaweicloud-sandbox` 的 SSH 路径示例改为 Linux 路径；未迁移本机 SSH 私钥。
- `fanren-rag` 通过 `FANREN_RAG_CONFIG` 读取私有运行配置。技能包内 endpoint 模板不含 Token；经用户单独授权复制的配置位于 `data/user-data/private/skills/fanren-rag/endpoint.json`，文件权限 0600。凭据未进入镜像、清单或测试报告。

详细文件摘要、来源版本及适配项见 [迁移清单](reports/skills-20261024/manifest.json)。

## 运行环境

镜像为 `deerflow-server:skills-20261024`，在原镜像上补充 Python 3.11、独立环境中的 Paramiko 4.0.0、Node/npm、curl。Go/Rust 二进制摘要与原镜像一致；Go 程序通过启动 wrapper 的已有参数启用容器内 argv 命令及 `skill_run`。业务源码未修改。

构建文件与命令策略见 [运行环境说明](skills-runtime/README.md)。加载技能不会自动安装依赖。外部云服务、搜索服务仍使用各自的地址及授权；迁移和验收不等于已登录所有外部服务。

## 验证结果

- 候选与正式容器均成功注册 25 个技能，启用状态正确，无技能诊断错误；图表技能完整保留 28 个文件。
- 通过真实 WS/模型执行 `skill` → `skill_run check` → `skill_run run --help`，验证了 Python/Paramiko 就绪、同一固定技能版本、退出码 0 和进程终止确认；未执行外部 SSH 操作。
- Node 图表及规划脚本语法检查通过。
- RAG 健康接口可达；此项未发起实际检索。
- 桌面原加密凭据仍可认证 `ws://192.168.1.190:18787/v1`。
- Folotoy 在重连后通过真实 ASR → DeerFlow → TTS → MQTT/UDP/HTTP 音频链路复验，返回两段可下载音频。测试使用模拟设备，未观察实体扬声器播放。
- 验收后两个正式容器均为 healthy，重启计数为 0；候选容器、测试 actor、临时 Token 文件及传输归档已清理。

[正式技能执行回执](reports/skills-20261024/skill-ws-production-report.json) · [部署状态](reports/skills-20261024/deployment.json) · [Folotoy 复验](reports/skills-20261024/voice-after-skills-report.json)

## 后续维护与回滚

技能文件可直接维护于宿主机上述 `public/`、`custom/` 目录。新增目录需要启动扫描或显式安装；修改已有包可以通过管理接口刷新。仅改文件不代表运行中的会话已切到新版本，应使用新一轮或新会话验证。

```bash
cd /home/gen/deerflow-deploy
docker compose ps
docker compose restart deerflow
```

本次回滚资料在 `/home/gen/deerflow-deploy/skill-migration-20261024/backup`。需要撤销本次迁移时：

```bash
python3 /home/gen/deerflow-deploy/skill-migration-20261024/rollback_skills.py
```

脚本等待活动任务结束，恢复原挂载及镜像配置，并按原清单恢复技能启用状态；保留会话、记忆和技能文件。它不删除私有运行配置。后续若已另外调整 Compose，应先核对备份与现有配置差异。
