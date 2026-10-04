> 历史记录：本文描述旧 Python 实现或当时的桌面设计，命令和能力声明不作为当前分支的使用说明。原文与源码参见[固定历史提交](https://github.com/omengye/deerflow-acp/tree/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9)。当前 Eino Go 入口与验收范围见[项目说明](../README.md)。

# Checkpoint 与记忆优化说明

## 对齐的上游更新

本实现核对了字节 DeerFlow 2026 年 7–8 月的相关提交，主要包括：

- [可插拔 MemoryManager（#4326）](https://github.com/bytedance/deer-flow/commit/01a89f23)：统一记忆后端接入点。
- [mem0 HTTP 后端（#4528）](https://github.com/bytedance/deer-flow/commit/352f247a)：远程记忆写入、检索和身份映射。
- [delta checkpoint 并发写线性化（#4460）](https://github.com/bytedance/deer-flow/commit/244ce773)：同一线程的 checkpoint 变更串行执行。
- [可配置 delta 快照频率（#4516）](https://github.com/bytedance/deer-flow/commit/c48de5e7)：在存储占用与冷读取延迟之间调节。
- [mem0 按完整条目截断（#4600）](https://github.com/bytedance/deer-flow/commit/c8607144)：不把半条记忆注入提示词。
- [阻止 task-scoped 数据进入长期记忆（#4604）](https://github.com/bytedance/deer-flow/commit/cccda35c)：过滤工具和内部子任务数据。
- [checkpoint history cache（#4638）](https://github.com/bytedance/deer-flow/commit/c8cf1bf2)：缓存不可变 delta 历史，降低重复回放成本。

## Checkpoint 配置

```yaml
api:
  checkpointer_type: sqlite
  checkpointer_path: ./data/checkpoints.db
  checkpoint_channel_mode: delta
  checkpoint_delta:
    snapshot_frequency: 10
  checkpoint_cache:
    type: memory       # memory 或 redis
    max_entries: 128   # 0 表示关闭
    redis_url: null    # null 时读取 DEER_FLOW_CHECKPOINT_CACHE_REDIS_URL / REDIS_URL
    ttl_seconds: 86400
    key_prefix: ""
```

`checkpoint_channel_mode` 与 `checkpoint_delta.snapshot_frequency` 会在进程内冻结。共享数据库的 API worker、嵌入式客户端和后台 worker 必须配置一致；热切换会直接报错。delta 线程带有持久化模式标记，全量模式读取时会在使用状态前拒绝不兼容线程。

delta 模式必须使用 `langgraph-checkpoint-sqlite>=3.1.0,<3.2`。历史缓存只缓存带明确 checkpoint ID 的不可变祖先链，不缓存“最新 checkpoint”解析；Redis 故障会退化为源数据库读取，不影响正确性。

## DeerMem 配置

```yaml
memory:
  enabled: true
  injection_enabled: true
  manager_class: deermem
  mode: middleware
  shutdown_flush_timeout_seconds: 30
  backend_config:
    storage_path: memory.json
    storage_class: deerflow.agents.memory.storage.FileMemoryStorage
    debounce_seconds: 30
    max_facts: 100
    max_injection_tokens: 2000
    retrieval_enabled: true
    retrieval_top_k: 12
```

旧版 `memory.storage_path`、`memory.debounce_seconds` 等扁平字段仍可使用，加载时会自动复制到 `backend_config`。当扁平字段与嵌套字段同时存在时，嵌套值优先。

`mode: middleware` 会在每次模型调用前自动召回并注入相关记忆。`mode: tool` 不自动注入，而是给主 Agent 注册 `memory_search` 工具，让模型按需检索；两个内置后端仍通过 `MemoryMiddleware` 在对话结束后异步写入。自定义 manager 使用 tool 模式时必须实现结构化 `search()`，配置校验会 fail-fast。

## mem0 配置

```yaml
memory:
  enabled: true
  injection_enabled: true
  manager_class: mem0
  mode: middleware
  shutdown_flush_timeout_seconds: 30
  backend_config:
    api_key_env: MEM0_API_KEY
    base_url: https://api.mem0.ai
    allow_insecure_http: false
    default_user_id: deerflow
    top_k: 8
    score_threshold: 0.1
    max_injection_chars: 12000
    timeout_seconds: 10
    startup_policy: fail_fast       # fail_fast 或 best_effort
    failure_policy:
      read: fail_open               # fail_open 或 fail_closed
      write: log_and_drop           # log_and_drop 或 raise
```

身份映射为：运行时 `user_id`（缺失时使用 `default_user_id`）对应 mem0 `user_id`，DeerFlow `agent_name` 对应 `agent_id`，`thread_id` 对应 `run_id`。HTTP 默认强制 HTTPS；只有明确设置 `allow_insecure_http: true` 才能连接本地 HTTP 服务。

写入使用 `POST /v3/memories/add/`，列表/召回使用 `POST /v3/memories/`，搜索使用 `POST /v3/memories/search/`，清理使用 `DELETE /v1/memories/`。远程后端不提供 DeerMem 的事实 CRUD、导入或导出能力，调用这些接口会返回明确的 unsupported 错误。

退出时会等待当前记忆写入完成并继续排空 debounce 队列，最长等待 `shutdown_flush_timeout_seconds`。若超时，不会提前关闭仍被写入线程使用的 mem0 HTTP 客户端。

## Admin 管理页面

启用 API 鉴权后访问 `/management/`，可在“长期记忆”区域配置以下内容：

- 通用开关、后端、`middleware`/`tool` 模式、自动注入开关和退出排空超时。
- DeerMem 的模型、存储类/路径、debounce、事实数量与置信度、注入上限、FTS5 召回及索引路径。
- mem0 的环境变量名、服务 URL、用户映射、召回/截断/超时参数及启动、读写故障策略。
- 受信任的自定义 `MemoryManager` 类路径和 `backend_config` JSON。

页面提供“校验”“测试后端”“保存并应用”三个独立动作。后端测试会隔离构造候选 manager、执行严格 `probe()` 并关闭候选实例，不修改当前全局 manager；自定义 manager 的构造和 `probe()` 是应用代码，仍可能访问外部资源或产生其自行定义的副作用，因此只应配置受信任的类。

保存使用 `PATCH /api/admin/memory`，请求携带页面读取到的 `expected_revision`。若其他管理员已经改过 Memory 配置，会返回 HTTP 409，页面保留当前表单并要求刷新后重试。后端切换要求以 `replace` 模式提交 `backend_config`，且只切换配置，不迁移 DeerMem、mem0 或自定义后端中的既有数据。兼容端点 `GET/PUT /api/admin/memory` 仍保留；自动化管理建议优先使用带 revision 的 PATCH。

默认保存流程会先校验并探测候选后端，再原子更新 `config.yaml`。选择立即应用时，系统会暂停 timer 驱动的记忆写入，以旧 manager 排空已经入队的任务，然后切换配置和 manager，最后恢复切换期间新进入的任务；候选 reload 失败时会尝试恢复文件和运行时。将 `reload` 或 `probe` 显式设为 `false` 只适合受控自动化，管理页面正常保存不关闭这些保护。

mem0 的实际 Key 必须由服务进程环境变量提供，例如 `MEM0_API_KEY`；配置和 Admin API 只保存/返回 `api_key_env` 及“环境变量是否存在”，不会返回实际值。HTTP 默认禁止，只有可信内网自建服务才应同时使用 `http://` 和 `allow_insecure_http: true`。

## Linux 生产部署清单

1. 备份 `config.yaml`、DeerMem JSON 数据文件和 FTS5 索引；后端切换前另行制定数据迁移/回滚方案。
2. 使用 `uv sync --frozen --inexact` 按仓库中的 `uv.lock` 同步生产虚拟环境；不要因为旧 `.venv` 已存在就跳过依赖升级。核对现有 `config.yaml` 与 `config.example.yaml` 的 `config_version`，合并新增字段但不要覆盖生产密钥和路径。
3. 确保服务运行用户可写 `config.yaml`、DeerMem JSON/FTS5 文件及其父目录。原子保存会在同目录创建临时文件；Linux 多进程锁会持久使用 `.config.yaml.lock`、`.memory.json.lock`（以及按 Agent 数据文件生成的同名隐藏锁文件）。
4. 单机多 worker 必须指向相同的 `config.yaml`、DeerMem 数据和锁文件目录，且底层文件系统要支持 POSIX `flock`。DeerMem 的读改写事务会通过锁文件串行化，避免同机 worker 丢失更新。不要依赖跨主机/NFS 锁来协调多个独立部署实例；跨主机部署优先使用 mem0 等具备服务端并发控制的后端。
5. Admin 热重载只作用于接收该请求的 worker。多 worker 部署保存成功后应滚动重启全部 worker，确保每个进程读取同一版本；重启期间不要继续从另一 worker 修改配置。
6. 先在 staging 用与生产一致的 Linux 用户和目录权限执行“校验”和“测试后端”。mem0 Key 必须注入所有 worker 的进程环境，而不是只存在于登录 shell。
7. 部署后检查 `GET /health/ready`，再在 Admin 页面执行一次后端测试；同时观察待处理写入数是否回落到 0，并验证一次真实记忆写入和召回。

配置写入使用文件内容 SHA256 做 compare-and-swap，并在 Linux 上结合进程内锁、POSIX lock file、同目录临时文件、文件 `fsync`、原子替换及目录 `fsync`，可防止常见并发覆盖和半文件。DeerMem JSON 写入同样采用 POSIX 锁、临时文件 `fsync` 和原子替换。锁文件常驻是正常现象，不应由定时清理任务删除。
