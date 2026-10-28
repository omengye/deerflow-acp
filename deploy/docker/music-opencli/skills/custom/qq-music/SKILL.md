---
name: qq-music
description: 通过 QQ 音乐来源搜索歌曲、按歌曲 MID 解析音质并下载到共享目录。用于 QQ音乐搜索、点歌、下载；要让 FoloToy 播放，下载后调用 toy-playmusic。
---

# QQ 音乐搜索与下载

通过 `skill_run` 执行 `scripts/qq_music_api.py`，使用部署的私有 API 配置。命令非交互，返回 JSON；运行需要 Python 和 `requests`。

```json
{"action":"run","name":"qq-music","path":"scripts/qq_music_api.py","args":["search","晴天 周杰伦","--limit","5"],"timeout_seconds":45}
```

搜索结果含 `index`、`title`、`artist`、`mid`。根据用户要求选择结果；不清楚版本/歌手时先展示少量结果让用户选择。

```json
{"action":"run","name":"qq-music","path":"scripts/qq_music_api.py","args":["download","晴天 周杰伦","--song-index","1"],"timeout_seconds":180}
```

- `resolve 查询词 --song-index N` 或 `resolve --mid MID` 只解析音质，不下载。
- `download --mid MID` 可直接下载明确指定的歌曲 MID。
- 默认音质 `auto` 优先 standard/较小可播放文件；用户指定音质时用 `--quality standard/low/flac` 或 `resolve` 返回的 **一基**候选 `index`。
- 文件落在 `/workspaces/folotoy/music`，可用 `--output-dir` 选择根内子目录。根据返回 `path`、`relative_path`、字节数和摘要确认下载，不能仅凭模型文字确认。

## 在玩具上播放

用户要求点歌/播放时，下载成功后加载 `toy-playmusic`，传入 `relative_path` 以及本轮 provider 给出的真实 `device_key`、`request_id`。独立桌面会话没有语音请求标识时省略 `request_id`。不要自己编造设备/请求标识，也不要把“已下载”说成“已经听到音乐”。FoloToy 负责最终转码和播放排队。

接口没有可用音频链接或明确失败时如实说明；可在另一个来源已配置时改用 `song-download`，不反复重试同一失败 API。不得从报错或空链接推断已下载。

脚本保留了已验证的原客户端请求标识；该接口可能对不同 User-Agent 返回 HTTP 200 空列表，维护时不要把通用机器人标识替换进去。

Key 只从 `/var/lib/deerflow/user-data/private/skills/music/api.json` 读取，不接受命令行 Key，不输出原始 API 响应或带授权参数的下载 URL。若运行器要求环境检查，先 `skill_run action=check`。
