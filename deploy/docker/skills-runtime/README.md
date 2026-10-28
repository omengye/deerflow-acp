# Gentoo 技能运行环境

本目录在现有 `deerflow-server:pilot-20261024` 镜像上增加 Python 3.11、独立 Python 环境中的 Paramiko、Node/npm 和 curl。原 Go/Rust 可执行程序保留；启动 wrapper 只补充 Go 已支持的容器内命令运行参数。

构建：

```bash
docker build --pull=false -t deerflow-server:skills-20261024 deploy/docker/skills-runtime
```

国内服务器可通过 `DEBIAN_MIRROR`、`DEBIAN_SECURITY_MIRROR`、`PIP_INDEX_URL` 构建参数选择软件源。依赖安装发生在构建阶段，加载技能不会自动运行 pip 或 npm。

Compose 的独立挂载为：

```yaml
volumes:
  - ./skills:/var/lib/deerflow/user-data/skills
```

先创建宿主机目录并赋予容器 UID/GID 1000:1000 读写权限，迁移已有内容后再添加挂载，避免空目录覆盖原技能。配置继续使用 `skills.path: ../skills`。普通导入目录为 `public` 和 `custom`；受管技能需取当前已同步发布包再作为普通包导入，不应复制整个桌面数据库。

在服务器 `.env` 中选择 `DEERFLOW_IMAGE=deerflow-server:skills-20261024` 后执行 `docker compose up -d`。配置、会话、记忆及桌面 WS 地址保留。首次迁移后应检查原生 `skill.list`，并以新会话验证技能快照和 `skill_run`。

Wrapper 使用 Go 的 `local` 命令后端及 CLI 白名单，为 Python `skill_run` 配置明确的解释器。`local` 后端本身不提供隔离；容器是运行边界。`allow-shell=false` 关闭 Script 请求通道，但白名单中的 Bash、Python 和 Node 仍可运行脚本，不能据此宣称禁止任意脚本。此部署不挂载 Docker socket。

RAG 运行凭据位于 `/var/lib/deerflow/user-data/private/skills/fanren-rag/endpoint.json`，由 `FANREN_RAG_CONFIG` 指定路径，文件模式 0600。凭据不进入镜像或技能包；普通技能的 endpoint 模板不含 Token。

Windows 专用的 `instabox-exec-windows` 在 Linux 停用。SSH 类技能的依赖检查和 `--help` 可验证本地运行环境；连接外部主机仍需单独提供对应私钥及 known_hosts。

## 固定已有会话目录

Gentoo 的已有会话位于 `/var/lib/deerflow/user-data/config/go-harness-state`。WS 管理器启动时会提供另一默认数据目录；wrapper 现在在传入参数之后显式追加 `--data-dir`，让普通连接、管理器 apply 和容器重启使用同一目录。此变更不迁移、合并或删除数据库。其他部署可通过独立的 `DEERFLOW_PERSISTENT_DATA_DIR` 选择自己的绝对容器路径，不应使用管理器会重写的 `DEERFLOW_GO_DATA_DIR`。

新构建的技能镜像已经包含该 wrapper。对现有 `skills-20261024` 镜像，可只更新宿主机 wrapper 并添加只读挂载，无需重新安装运行依赖或编译 Go/Rust：

```bash
python3 install-runtime-override.py \
  --root /home/gen/deerflow-deploy \
  --wrapper-source /path/to/deerflow-acpd-wrapper.sh --apply
cd /home/gen/deerflow-deploy
docker compose up -d --no-build --force-recreate deerflow
```

安装器只写 wrapper 和 Compose，保留原文件的私有备份，并以 `docker compose config --quiet` 检查结果。它不会停止请求或重建容器；部署时需先停止 Folotoy 的新请求并确认已有任务结束，再运行上述重建。首次添加挂载需要重建容器，单独 `docker compose restart` 不能给已有容器增加挂载。后续普通 Compose 命令会保留挂载。`compose.override.yaml` 提供同一个挂载的可选模板；若已有 override 文件，应合并，不能覆盖现有配置。

验收应查看 `user-data/runtime/acp-go/endpoint.json` 的 `data_dir`（不要输出认证字段），并确认旧原生会话可通过 `session.diagnostics` 访问；随后重启一次、管理 apply 一次并重复检查。仅检查 `daemon.status` 的活动计数不足以验证目录，因为该操作不返回 `data_dir`。验证后恢复 Folotoy，并用独立会话测试 WS 工具执行。
