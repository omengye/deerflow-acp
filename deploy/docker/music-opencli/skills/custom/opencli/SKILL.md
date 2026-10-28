---
name: opencli
description: 通过宿主机已有 OpenCLI 和浏览器插件读取、搜索网站内容，包括小红书、抖音、B站、X、微信公众号、GitHub 等。使用 host_opencli 工具，复用宿主浏览器登录状态。
---

# 宿主 OpenCLI

使用内建 `host_opencli` 工具。该工具通过容器代理调用 Gentoo 宿主上已安装的 OpenCLI，浏览器和登录状态保留在宿主机。

## 发现与调用

首次使用某个站点或不确定命令时，先读取实际安装版本的帮助。旧说明中的动作名称不保证仍受支持。

```json
{"command":"version","description":"确认当前宿主 OpenCLI 版本"}
{"command":"list","description":"发现当前 OpenCLI 站点与命令"}
{"site":"bilibili","command":"help","description":"查看B站适配器命令"}
{"site":"bilibili","command":"help","arguments":["search"],"description":"查看搜索参数"}
```

随后将具体动作和每个参数分别传入 `site`、`command`、`arguments`。不要拼接 shell 命令，不要直接运行容器中的同名 OpenCLI 或读取宿主浏览器配置。

## 浏览器状态与失败

浏览器站点需要宿主的浏览器插件连接 OpenCLI daemon。`doctor` 是实际连通性检查，可能启动或重启 daemon；文件路径存在或版本输出不等于浏览器已连接。

```json
{"command":"doctor","description":"验证宿主浏览器插件连接"}
```

根据工具返回的实际 stdout、stderr、退出码报告结果。站点登录过期时说明需要用户在宿主浏览器登录，不把其他站点结果冒充该站点数据。

若结果出现 `host_state: unknown`、`host_termination_confirmed: false` 或 HTTP 失联，命令是否执行仍不确定。保留返回的作业 ID / idempotency key，先查作业状态；不要重放发帖、互动、上传等有副作用的动作。

读取与搜索按当前用户请求执行。发布、发消息、关注、删除等操作需要用户对该具体动作的明确指令；不要将“检查 OpenCLI 是否可用”当成这类操作的授权。
