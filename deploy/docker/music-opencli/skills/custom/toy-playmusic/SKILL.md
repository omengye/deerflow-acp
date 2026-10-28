---
name: toy-playmusic
description: 把共享目录中的本地音乐、歌曲或音频排队播放到 FoloToy 玩具。用于用户要求播放已下载音频；搜索和下载歌曲分别使用 song-download 或 qq-music。
---

# FoloToy 音乐播放

通过 `skill_run` 执行 `scripts/toy-playmusic.py`，调用已部署的 FoloToy 播放队列。它会在本轮语音回复后播放音乐，并与新录音取消行为协调；不要绕过队列单独发 MQTT 播放命令。

音乐共享根目录是 `/workspaces/folotoy/music`。下载技能返回的 `path` 可直接传入；不要使用其他容器的内部路径或旧的 `~/Documents/folotoy/folotoy-server/audio`。

## 播放

用户要求“播放本地音乐/验证音乐”等但未给出文件路径时，先列出共享目录，选择与用户描述匹配的已存在文件；不要猜路径，也不要因此下载新歌曲：

```json
{"action":"run","name":"toy-playmusic","path":"scripts/toy-playmusic.py","args":["list","--limit","20"],"timeout_seconds":20}
```

`list --match 验证音乐` 可筛选文件名，默认包含子目录。把返回的 `files[].path` 原样交给 `play`。

```json
{"action":"run","name":"toy-playmusic","path":"scripts/toy-playmusic.py","args":["play","歌曲名 - 歌手.mp3","--device-key","本轮设备标识","--request-id","本轮请求标识"],"timeout_seconds":25}
```

- 语音请求必须使用 provider 在本轮上下文中给出的 `device_key`、`request_id`，不猜测或沿用历史轮次的标识；语音请求不能使用 `--default-device` 回退其他设备。
- 独立桌面/管理会话没有有效语音请求标识时，省略 `--request-id`，使用用户指定的 `--device-key`；用户明确选择已配置默认设备时加 `--default-device`。没有任何设备/请求选择会报错，不能把该报错当成已播放。
- 文件必须已存在于共享根内。目录参数可配合 `--match TEXT`、`--recursive`、`--random` 选曲。
- 返回 `job.id`、`job.state` 和 `idempotency_key`。`queued/waiting_turn/preparing/ready` 表示已接收或准备中；`published` 只表示已发 MQTT 指令，不能说扬声器已播放或歌曲已播完。

需要跟踪本次任务时查询一次：

```json
{"action":"run","name":"toy-playmusic","path":"scripts/toy-playmusic.py","args":["status","返回的job.id"],"timeout_seconds":20}
```

`--dry-run` 只准备音频，不下发播放。`play ... --wait 30` 可有界等待准备/下发结果。网络超时或失联后不要自动重播；已有 `job.id` 时先查询状态，需要重试时保留同一 `--idempotency-key`。

## 运行配置

私有文件 `/var/lib/deerflow/user-data/private/skills/music/playback.json` 保存服务基址与 Token，不属于技能包或镜像。脚本只读取该文件，不把 Token 放入参数或输出。`health` 子命令检查服务；若运行器要求环境检查，可先调用 `skill_run` 的 `action=check`。
