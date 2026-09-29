# 旧 DeerMem JSON 导入

Go harness 不会自动扫描或修改 Python DeerFlow 的记忆目录。旧 ACP 使用 `acpmem-...` 哈希桶，桶名不能可靠反推出工作区或会话。操作者必须为每个 `memory.json` 明确指定目标。

先预览源文件，不打开 Go 数据库：

```powershell
go run ./cmd/deerflow-memory-migrate --source C:\path\to\memory.json
```

确认目标 Go 会话已经存在且已关闭正在运行的 harness，然后导入：

```powershell
go run ./cmd/deerflow-memory-migrate --source C:\path\to\memory.json --data-dir C:\path\to\go-data --workspace C:\path\to\workspace --session-id SESSION_ID --scope workspace --apply
```

`--scope` 只接受 `session`、`workspace`、`user`。`user` 还需 `--user-id`，且该身份必须与后续 Go harness 启动时配置的 `--memory-user-id` 一致。命令使用 Go harness 的独占数据目录锁；正在运行的实例不会被并发改写。不要根据旧哈希桶猜测目标范围。

导入接受 `version: "1.0"` 文档中通过保守内容筛选的事实，最多读取 4 MiB 和 1,000 条。旧 `user`/`history` 摘要桶和 FTS 索引不会转成事实。所有写入在一个 SQLite 事务中完成；相同目标范围与旧事实 ID 的重试可跳过，旧 ID 对应的内容或分类改变会使本次事务失败。报告包含导入、已存在、重复和拒绝数量。原 Python 文件保持原样。
