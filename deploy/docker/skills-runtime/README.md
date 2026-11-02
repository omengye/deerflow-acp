# Gentoo 技能运行环境

本目录在支持数据库绑定的 DeerFlow 服务端镜像上增加 Python 3.11、独立 Python 环境中的 Paramiko、Node/npm 和 curl。原 Go/Rust 可执行程序保留；启动 wrapper 补充 Go 已支持的容器内命令运行参数，并在启动前检查数据库绑定。旧 `pilot-20261024` 镜像缺少绑定能力，不能直接作为当前 wrapper 的基础镜像。

构建：

```bash
docker build --pull=false --build-arg BASE_IMAGE=deerflow-server:current \
  -t deerflow-server:skills-current deploy/docker/skills-runtime
```

国内服务器可通过 `DEBIAN_MIRROR`、`DEBIAN_SECURITY_MIRROR`、`PIP_INDEX_URL` 构建参数选择软件源。依赖安装发生在构建阶段，加载技能不会自动运行 pip 或 npm。

Compose 的独立挂载为：

```yaml
volumes:
  - ./skills:/var/lib/deerflow/user-data/skills
```

先创建宿主机目录并赋予容器 UID/GID 1000:1000 读写权限，迁移已有内容后再添加挂载，避免空目录覆盖原技能。配置继续使用 `skills.path: ../skills`。普通导入目录为 `public` 和 `custom`；受管技能需取当前已同步发布包再作为普通包导入，不应复制整个桌面数据库。

验收镜像后，在服务器 `.env` 中将 `DEERFLOW_IMAGE` 设为已验证的镜像 ID，排空任务、备份持久卷后只更新 `deerflow` 服务。配置、会话、记忆及桌面 WS 地址保留。首次迁移后应检查原生 `skill.list`，并以新会话验证技能快照和 `skill_run`。

Wrapper 使用 Go 的 `local` 命令后端及 CLI 白名单，为 Python `skill_run` 配置明确的解释器。`local` 后端本身不提供隔离；容器是运行边界。`allow-shell=false` 关闭 Script 请求通道，但白名单中的 Bash、Python 和 Node 仍可运行脚本，不能据此宣称禁止任意脚本。此部署不挂载 Docker socket。

RAG 运行凭据位于 `/var/lib/deerflow/user-data/private/skills/fanren-rag/endpoint.json`，由 `FANREN_RAG_CONFIG` 指定路径，文件模式 0600。凭据不进入镜像或技能包；普通技能的 endpoint 模板不含 Token。

Windows 专用的 `instabox-exec-windows` 在 Linux 停用。SSH 类技能的依赖检查和 `--help` 可验证本地运行环境；连接外部主机仍需单独提供对应私钥及 known_hosts。

## 固定已有会话目录

2026-11 的启动器统一读取持久卷内的 `user-data/config/runtime-data.json`。现有 Gentoo 的历史路径是 `/var/lib/deerflow/user-data/config/go-harness-state`；升级前必须核对运行服务的 `runtime.data_dir`，备份后将确认的相对路径写入绑定，例如 `{"data_dir":"config/go-harness-state"}`。不要根据文件时间、目录名或会话数量猜选数据库，也不要移动、合并或删除任何历史库。

新 wrapper 不再独立默认到某一数据库。遗留的 `DEERFLOW_PERSISTENT_DATA_DIR` 仅作为显式请求传给原生启动器，必须与绑定相符。先部署支持该绑定的 Go 后端、Bridge 和 Waku，再使用以下维护入口。它会在修改文件前检查原生 `--check-paths` 能力和运行服务的数据目录，旧镜像会被拒绝。该命令不会创建绑定或修改数据库：

```bash
python3 install-runtime-override.py \
  --root /home/gen/deerflow-deploy \
  --wrapper-source /path/to/deerflow-acpd-wrapper.sh --apply
cd /home/gen/deerflow-deploy
docker compose stop -t 90 deerflow
docker compose up -d --no-build --force-recreate deerflow
```

安装器只写 wrapper 和 Compose，保留原文件的私有备份，并以 `docker compose config --quiet` 检查结果。它不会停止请求或重建容器；部署时需先停止 Folotoy 的新请求并确认已有任务结束，再运行上述重建。首次添加挂载需要重建容器，单独 `docker compose restart` 不能给已有容器增加挂载。后续普通 Compose 命令会保留挂载。`compose.override.yaml` 提供同一个挂载的可选模板；若已有 override 文件，应合并，不能覆盖现有配置。

验收应核对管理响应的 `runtime.data_dir` 或 `user-data/runtime/acp-go/endpoint.json` 的 `data_dir`（不要输出认证字段），并实际恢复原有原生会话 ID；随后重启一次、管理 apply 一次并重复检查。仅检查活动计数不足以验证目录。镜像更新也必须处理已有的 wrapper 挂载；只替换镜像不会更新宿主机的挂载文件。验证后恢复 Folotoy，并用独立会话测试 WS 工具执行。
