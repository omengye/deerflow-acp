---
name: song-download
description: 通过酷我音乐来源搜索、选择并下载歌曲到共享音乐目录。用于用户点歌、搜索歌曲或下载音频；要在 FoloToy 上播放，下载后调用 toy-playmusic。
---

# 酷我歌曲搜索与下载

通过 `skill_run` 执行 `scripts/song-download.py`。命令非交互，输出 JSON；需要 Python 和 `requests`，必要时先 `skill_run action=check`。

## 搜索、选择、下载

```json
{"action":"run","name":"song-download","path":"scripts/song-download.py","args":["search","晴天 周杰伦","--limit","5"],"timeout_seconds":45}
```

根据返回的标题、歌手选择用户要的歌曲。每项都给出可直接复用的 `download_args`；选定后直接使用这些参数，不再用泛查询词重复搜索。

```json
{"action":"run","name":"song-download","path":"scripts/song-download.py","args":["download","--song-title","晴天","--artist","周杰伦"],"timeout_seconds":180}
```

默认优先适合播放的较小 MP3；用户明确指定音质时，`--quality N` 使用 `resolve` 返回的 **零基**音质索引。下载目录固定在共享根 `/workspaces/folotoy/music` 内，`--download-dir` 可选择根内子目录。输出 `path`、`relative_path`、文件大小和摘要。

`resolve --song-title 标题 --artist 歌手` 可对同一已选结果检查音质，不下载，也不重复搜索。两个选择参数必须完整给出，不能猜补缺失歌手或在失败时静默换首。旧的 `download 查询词 --song-index N` 和 `resolve 查询词 --song-index N` 仍兼容，但会重新搜索，只用于尚未选定结果的一步调用。

## 点歌与播放

用户要求“播放/放一首”时，下载成功后加载 `toy-playmusic`，把返回的 `relative_path` 交给它播放，并传本轮真实 `device_key`、`request_id`。不要下载完就声称已播放。

最终设备 MP3 转码由 FoloToy 服务负责，容器下载器不依赖 ffmpeg。源文件即使后缀是 MP3 也可能是 AAC/MP4；播放服务会检测并转码。

接口失败、无结果或下载失败时如实报告，可在用户要点歌且另一个来源已配置时改用 `qq-music`。不要循环重试同一失败请求，不以旧下载链接替代新解析结果。

私有 API Key 只从 `/var/lib/deerflow/user-data/private/skills/music/api.json` 读取，不在技能、参数或回答中传递。`resolve` 只展示音质元数据，不输出带授权查询参数的原始音频 URL。
