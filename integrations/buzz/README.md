# Buzz → DeerFlow Go 适配器

原生 Go sidecar，通过官方 Buzz CLI 收发频道和私信，通过 Rust `deerflow-acp --protocol v2` 驱动 Eino Go daemon。无需 Python 或 `.venv`。适配器独立于桌面发布包，可单独更新。

## Windows 使用

1. 解压当前 Eino Go 桌面发布包，打开桌面设置并配置模型，确认工作区目录存在。
2. 解压 Buzz 适配器 ZIP，安装支持 `messages get/send/edit`、`media get` 的官方 Buzz CLI。
3. 复制 `adapter.example.toml` 为 `adapter.toml`，设置 `buzz.command`、relay、agent 公钥、`deerflow.desktop_root` 和工作区。
4. 复制 `.env.example` 为 `.env`，填入已有 `BUZZ_PRIVATE_KEY` 和可选 `BUZZ_AUTH_TAG`。私钥必须与 agent 公钥匹配。
5. 配置 `allowed_pubkeys`。留空且存在 owner attestation 时，仅接受该 owner。公共频道仍要求提及 agent 的 Nostr p-tag。显式 `allow_any_sender=true` 才允许所有发送者。
6. 运行 `./start.ps1 -Check`。此模式只验证身份、配置和本地可执行文件，执行本地 `--help`，不连接 relay、不启动 ACP、不打开或迁移数据库。
7. 运行 `start.bat` 或 `./start.ps1`，保持窗口运行，使用 Ctrl+C 退出。启动后会更新配置的 profile、presence 和 status；`-Once` 也会执行任务并发送回复。

`permission_policy` 默认 `deny`，工具权限请求会被取消，并回复中断原因。仅在明确限制发送者后才可设置 `allow_once`，此时允许一次工具审批；不支持自动选择 allow_always。

安装目录自带 `deerflow-acp.exe` 和 `deerflow-acpd.exe`。Buzz 应选择 Rust Bridge `deerflow-acp.exe`。直接运行 `deerflow-acp-go.exe` 使用 ACP v1，无法用于此适配器。启动检查会拒绝旧 Python Bridge。配置了 `desktop_root` 后会读取其 `user-data/config/config.yaml`，连接 `user-data/runtime/acp-go`，使用 `user-data/data/go-harness`，与桌面保持一致。

## 从旧 Python 适配迁移

保留 `.env` 和 `data/adapter.sqlite3`，先停止旧适配器并备份数据库。修改旧 `adapter.toml` 中的 `[deerflow]`，删除旧 Python `command`，添加当前 `desktop_root`。启动时兼容原 SQLite 表，补充 backend 标识；游标、已处理消息和已保存回复继续保留。旧 Python 会话 ID 不会传给 Go daemon，新的对话会建立 Go 会话。数据库独占锁会阻止两个新适配器同时运行；该锁无法约束旧 Python 进程，因此必须先停旧进程。

`replay_existing=false` 对首次发现的频道记录已有消息边界；后续重启沿用已保存游标。保存消息和推进游标使用同一事务，同一秒的消息重叠读取并按 event ID 去重。时间戳分页无法继续或超过 `max_poll_pages` 时保留游标并报告失败。

## 故障恢复

提交模型任务前保存 `processing` 状态，模型回复保存后才进入发送流程。Buzz 暂时不可启动、明确可重试的连接错误会重试；已经生成的回复不会再次调用模型。发送前保存 `delivering` 状态。

断线、进程中断或丢失发送确认可能留下 `uncertain` / `delivery_unknown`。这些记录会隔离，重启也不会自动重放工具或重新发送。日志会给出 event ID；在备份后的 SQLite 数据库中查阅对应状态、`response_content` 和 `sessions`，再对照 ACP 会话与 Buzz 线程核实结果。需要重新执行时，在 Buzz 发一条新的明确请求；核实已成功的旧记录可标为 done。不要批量把隔离记录改回 pending，也不要删除数据库来绕过恢复状态。

## 附件与观测

支持 NIP92 imeta 图片和普通文件输入。只下载配置 relay 原点的 `/media/<sha256>` 文件，校验大小、SHA-256、图片格式并拒绝缓存链接。最多 8 个附件、总计 40 MiB，图片单个 20 MiB，普通文件单个 25 MiB。图片作为 ACP image block，普通文件放在工作区 `.buzz-attachments` 并作为 resource link。音视频暂不支持。回复中的本机文件链接不会自动上传到 Buzz。

公共工具进度仅显示工具标题和状态。可选 NIP-AO 观测需要 owner 身份，使用 NIP44 加密、Nostr 签名和 NIP42 鉴权；默认删除原始工具输入、输出和内容。队列有上限，观测失败不会阻塞最终回复。

## 构建和验证

使用 Go 1.26.8，在本目录执行 `go test ./...`、`go vet ./...`。`go test -race ./...` 验证并发。

Windows 打包执行 `./build.ps1 -DesktopRoot 'D:/Apps/DeerFlow-Desktop-Go'`。发行 ZIP 要求源码已经提交；本地开发包可显式加 `-Development`，构建信息会标明源码未提交状态。脚本先运行测试、vet 和依赖校验，再生成 EXE、ZIP、SHA-256 和记录源码提交的 `BUILD-INFO.json`；包内不包含私钥、个人配置、数据库或 Python 缓存。传入 DesktopRoot 会启用真实 Bridge/daemon 联调，模型仍为本地 HTTP fixture，使用独立临时配置、工作区和数据库，结束后停止测试 daemon。

GitHub Actions 在 Windows/Linux 运行测试、并发检查和真实 Bridge 联调，上传原生可执行文件；Windows 另上传完整 ZIP。测试不需要真实 Buzz relay 或模型 API key。实际 relay 的鉴权、成员资格和收发结果仍需人工验收。
