# Gentoo 音乐与宿主 OpenCLI 接入

本目录实现 `song-download`、`qq-music`、`toy-playmusic` 和 `opencli` 四个技能。
运行结果以本次部署与真实工具回执报告为准；安装成功、HTTP 返回成功和模型文字回答
都不能代替下载文件、浏览器连接或玩具拉取音频的证据。

## 组件与路径

| 组件 | 位置 / 作用 |
|---|---|
| DeerFlow 镜像 | `deerflow-server:music-opencli-20261024`，从原技能镜像增加离线 `requests==2.32.5` 和 OpenCLI 代理 |
| FoloToy 镜像 | `folotoy-deerflow-ws:music-20261024`，在日期修复镜像上覆盖本次修改的 Python 文件 |
| DeerFlow 部署 | `/home/gen/deerflow-deploy`，原配置、技能、会话和工作区持久挂载保留 |
| FoloToy 部署 | `/home/gen/Documents/folotoy/deerflow-ws/src` |
| 下载共享目录 | 宿主 `/home/gen/deerflow-workspaces/folotoy/music`；DeerFlow `/workspaces/folotoy/music`；FoloToy `/music` 只读 |
| 设备音频目录 | FoloToy `/audio`，对应宿主部署目录的 `audio/`；转码结果由现有 HTTP 服务提供 |
| 设备音频地址 | `http://192.168.1.190:8081/文件名.mp3`，玩具需要能从局域网访问 |
| 音乐控制服务 | FoloToy 主进程内的 HTTP 线程，当前私有地址 `http://172.27.0.1:8090` |
| OpenCLI 宿主桥 | `http://172.27.0.1:18990`，按 `gen` 用户运行 `deerflow-opencli-bridge.service` |
| 既有 OpenCLI / 浏览器 | `/home/gen/.npm-global/bin/opencli` 与宿主 Chrome 插件；原 daemon 使用 `127.0.0.1:19825` |
| 原生会话目录 | `/var/lib/deerflow/user-data/config/go-harness-state`，启动 wrapper 继续显式固定此目录 |

桌面与 FoloToy 仍通过 `ws://192.168.1.190:18787/v1` 连接 DeerFlow，使用原来的
WS Token、`fullAccess` 和 `allow_always`。音乐接入没有迁移或合并会话数据库。
FoloToy 本轮工具超时配置为 240 秒，角色与 ASR/TTS 设置保留。

## 私有运行配置

这些文件均不属于技能包、构建上下文或镜像，文件权限为 `0600`，所在私有目录为
`0700`。不要把实际 Token 或 Key 复制到本文、命令参数、日志和报告里。

| 用途 | 宿主位置 | 容器位置 |
|---|---|---|
| 音乐搜索 API Key | DeerFlow 部署目录下 `data/user-data/private/skills/music/api.json` | `/var/lib/deerflow/user-data/private/skills/music/api.json` |
| 音乐技能客户端地址和 Token | 同目录 `playback.json` | `/var/lib/deerflow/user-data/private/skills/music/playback.json` |
| FoloToy 音乐服务配置 | FoloToy 部署目录下 `private/music-control.json` | `/run/deerflow-music-control.json`，只读挂载 |
| OpenCLI 宿主桥配置 | DeerFlow 部署目录下 `private/opencli-bridge.json` | 保留在宿主，由 `gen` 用户服务读取 |
| OpenCLI 容器代理配置 | DeerFlow 部署目录下 `data/user-data/private/host-tools/opencli.json` | `/var/lib/deerflow/user-data/private/host-tools/opencli.json` |

音乐客户端与 FoloToy 服务使用同一份音乐 Token；OpenCLI 宿主桥与代理使用另一份
相互匹配的桥接 Token。`host_opencli.executable` 指向
`/opt/deerflow-host-tools/opencli-proxy`，`sandbox.allow_host_tools=true`，站点通过
`allowed_sites` 显式配置。完整桥接字段与接口见
[host-bridge/CONFIGURATION.md](host-bridge/CONFIGURATION.md)。

本机访问 Docker 网关时，桥接服务实际观察到的来源可能是宿主 LAN 地址。
此次现场需额外允许 `192.168.1.190/32`，因此健康探针出现 `403` 时，应先核对来源
地址和 CIDR，不要把正确的 Token 换成新值或放开整个网段。

## 搜索与下载

自然语音可以直接说“用 QQ 音乐搜索晴天”或“放一首晴天，周杰伦的版本”。需要下载
并播放时，模型应先搜索选择，再下载，最后把工具返回的路径交给 `toy-playmusic`。
搜索结果不明确时先选版本，避免把同名翻唱当成指定歌手。

以下是 `skill_run` 参数示例，适用于管理会话和故障定位：

```json
{"action":"run","name":"song-download","path":"scripts/song-download.py","args":["search","晴天 周杰伦","--limit","3"],"timeout_seconds":45}
{"action":"run","name":"qq-music","path":"scripts/qq_music_api.py","args":["search","晴天 周杰伦","--limit","3"],"timeout_seconds":45}
{"action":"run","name":"qq-music","path":"scripts/qq_music_api.py","args":["download","晴天 周杰伦","--song-index","1"],"timeout_seconds":180}
```

下载结果需要同时有 `ok=true`、有效文件 `path/relative_path`、字节数和摘要。
酷我搜索结果提供 `download_args`，应直接复用已选结果中的标题和歌手，例如
`["download","--song-title","晴天","--artist","周杰伦"]`，避免下载时再次搜索
丢失已选歌曲。旧的查询词和序号调用仍然兼容。
两个下载器均把文件限制在共享音乐根目录内。转码交给 FoloToy 已有的 `ffmpeg`，
输出 MP3、24kHz、单声道、96kbps；源文件即使后缀为 MP3 也会检测后重新编码。

## 播放已有文件

先通过 `list` 找到真实文件；下面的播放路径要替换为下载或列举结果中的值：

```json
{"action":"run","name":"toy-playmusic","path":"scripts/toy-playmusic.py","args":["list","--limit","20"],"timeout_seconds":20}
{"action":"run","name":"toy-playmusic","path":"scripts/toy-playmusic.py","args":["play","下载返回的relative_path","--default-device","--wait","30"],"timeout_seconds":45}
{"action":"run","name":"toy-playmusic","path":"scripts/toy-playmusic.py","args":["status","返回的job.id"],"timeout_seconds":20}
```

`--default-device` 是独立管理会话对配置默认设备的明确选择。也可以使用用户明确
指定的 `--device-key`。语音点歌不使用默认设备回退：provider 会提供本轮真实
`device_key` 和 `request_id`，技能把这两个标识传回服务。服务在本轮 TTS 后追加
音乐，统一发送 `audioplay/audioplay_cmd`，并在新录音抢占时取消旧任务。

音乐 API 均要求 Bearer Token：

- `GET /v1/music/health`
- `POST /v1/music/play`：相对音频路径、幂等标识，以及需要时的本轮设备/请求标识
- `GET /v1/music/jobs/{id}`

`queued/waiting_turn/preparing/ready` 表示接收或准备中；`published` 只说明 MQTT 指令
已发出。`audio_requested` 表示观察到音频 HTTP 请求，`device_audio_requested` 表示
观察到带设备客户端特征的请求；这些都不等于扬声器已经发声或歌曲已经播完。
`delivery_confirmed`、`speaker_playback_confirmed` 不会被 HTTP 200 或模型自述改成真。
实体扬声器是否听到音乐需要实际观察。

`--dry-run` 只准备音频，不下发 MQTT。失联后保留 `job.id` 和 `idempotency_key`，
先查状态；不要自动重复一次播放。服务重启后内存中的音乐任务记录不保留，`404`
也不能证明之前的播放指令没有执行。

## 宿主 OpenCLI

浏览器和插件保留在宿主，不往 DeerFlow 容器复制 Chrome profile。模型通过内建
`host_opencli` 调用容器代理，再由宿主桥固定执行真实 OpenCLI argv。

```json
{"command":"version","description":"确认宿主 OpenCLI 版本"}
{"command":"doctor","description":"检查宿主浏览器插件连接"}
{"site":"bilibili","command":"help","description":"发现当前B站适配器命令"}
{"site":"bilibili","command":"help","arguments":["search"],"description":"读取当前搜索参数"}
```

实际站点动作与参数以当前帮助输出为准。`doctor` 会实时探测浏览器，并可能启动
或重启 OpenCLI daemon；仅有版本或路径不能证明插件已连接。验收浏览器功能还需要
一个成功的只读站点调用。发布、发送消息等操作不属于连通性测试。

宿主桥的 SQLite 私有 journal 保留幂等回执。网络失联、`host_state=unknown` 或
`host_termination_confirmed=false` 时，容器代理结束不代表宿主命令结束；应查询
返回的作业 ID，不自动重放有副作用的操作。

## 运维与回滚

在 Gentoo 的 `gen` 用户下维护宿主桥：

```bash
systemctl --user status --no-pager deerflow-opencli-bridge.service
systemctl --user restart deerflow-opencli-bridge.service
journalctl --user -u deerflow-opencli-bridge.service -n 50 --no-pager
```

服务随用户 manager 启动；退出登录后的存续取决于宿主的 user manager/linger 设置，
浏览器插件也需要该用户的浏览器会话。不要把浏览器启动状态归因于容器健康检查。

`deploy.py` 在失败时自动恢复本次触碰的原文件和旧容器。上线后的手工回滚应先等待
活跃请求和音乐准备任务结束，然后：

1. 停止 FoloToy，停止新建的 OpenCLI 用户服务；先保存未知状态的 OpenCLI 回执。
2. 查看 `/home/gen/deerflow-deploy/music-opencli-20261024/deployment.json` 指向的私有
   `backup-.../index.json`，逐项检查是否有上线后的修改再恢复原文件；不要覆盖后来
   的外部修改。新增文件仅按清单确认后移除。
3. 恢复 Compose 和镜像覆盖字段后，使用保留的原镜像重建：
   `deerflow-server:skills-20261024` 与 `folotoy-deerflow-ws:tools-fix-20261024`。
4. 重新加载 systemd 用户单元，再用两个项目原有 Compose 命令恢复服务，确认健康与
   原会话仍可访问。

```bash
cd /home/gen/deerflow-deploy
docker compose up -d --no-build --force-recreate deerflow
cd /home/gen/Documents/folotoy/deerflow-ws/src
docker compose --env-file .env.ws -p folotoy-ws -f compose.ws.yaml up -d --no-build --force-recreate folotoy
```

不要删除会话数据库、私有配置、共享音乐、宿主桥 journal、运行代码或回滚备份。
检查原生目录时只提取 `endpoint.json` 的 `data_dir` 字段，不打印含认证信息的完整
端点文件。新技能的运行记录不通过直接修改会话数据库清除。

构建传输归档、离线测试目录和诊断脚本在真实验收完成、报告取回后可清理；不要
在仍有验证进程或下载任务运行时删除它们。具体清理目标由本次部署清单确认。
