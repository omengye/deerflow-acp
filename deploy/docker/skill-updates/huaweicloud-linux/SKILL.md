---
name: huaweicloud-linux
description: 操作用户的华为云 Linux 主机（默认 1.92.70.163:2233，root）。用于明确请求的系统状态、服务日志、命令执行、SFTP 文件传输和应用维护；通过外部 SSH 私钥和已验证 known_hosts 连接。
---

# 华为云 Linux 主机管理

使用 `skill_run` 执行随技能发布的 `scripts/huaweicloud_ssh.py`。
`skill://` 是只读快照标识，不是工作区或 shell 路径；不要把它拼入 `python` 命令。

## 运行检查与连接配置

先调用：

```json
{"action":"check","name":"huaweicloud-linux"}
```

需要 Python 3.10+ 和 Paramiko。依赖就绪只证明运行环境，不证明 SSH 已连接。
连接默认值为主机 `1.92.70.163`、端口 `2233`、用户 `root`。

Gentoo DeerFlow 容器使用以下外部私有文件，均不属于技能包或镜像：

- 私钥：`/var/lib/deerflow/user-data/private/skills/huaweicloud/huaweicloud_ed25519`
- 已单独验证的主机密钥记录：`/var/lib/deerflow/user-data/private/skills/huaweicloud/known_hosts`

通过全局参数 `--key`、`--known-hosts` 明确传入路径，放在 `status` 等操作之前。
可选全局参数为 `--host`、`--port`、`--user`。
不要读取私钥内容、把凭据放入技能目录或在回答中输出凭据。
只使用已验证的 known_hosts；未知或变化的主机密钥必须连接失败，不能自动信任。

## 只读状态检查

```json
{"action":"run","name":"huaweicloud-linux","path":"scripts/huaweicloud_ssh.py","args":["--key","/var/lib/deerflow/user-data/private/skills/huaweicloud/huaweicloud_ed25519","--known-hosts","/var/lib/deerflow/user-data/private/skills/huaweicloud/known_hosts","status"],"timeout_seconds":90}
```

检查系统版本、运行时间、负载、内存、磁盘和网络接口。必须根据实际工具退出状态和输出报告结果。

## 其他操作

保留上面的连接参数，替换最后的操作参数；操作范围以用户本次请求为准。

- 查看服务：`["exec","systemctl","status","nginx"]`
- 执行用户明确请求的远程命令：`["exec", ...命令参数]`。`exec` 的参数按旧接口组成远程 shell 表达式；不要把不可信内容直接拼入命令。
- 上传：`["upload","容器内本地文件路径","远程目标路径"]`
- 下载：`["download","远程文件路径","容器内保存路径"]`

使用 `skill_run` 的 `args` 数组传递每个参数，不把整个调用拼成一个字符串。
超时或断线不能证明远程进程已终止，不自动重放有副作用的操作。

## 桌面与环境变量兼容

脚本保留原有外部私钥默认位置 `~/Documents/ssh_info/huaweicloud_ed25519`，
known_hosts 默认 `~/.ssh/known_hosts`。可兼容环境变量：

`HUAWEICLOUD_SSH_KEY`、`HUAWEICLOUD_SSH_KNOWN_HOSTS`、
`HUAWEICLOUD_SSH_HOST`、`HUAWEICLOUD_SSH_PORT`、`HUAWEICLOUD_SSH_USER`。

DeerFlow 命令后端不继承任意宿主机环境变量，因此容器中的技能调用应使用上面的显式参数。
