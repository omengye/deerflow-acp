# 2026-09-20 独立适配更新

本次以 [上游评估](upstream-review-20260920.md) 中确认适用的行为为依据，在当前 FastAPI / ACP harness 上独立实现。没有 merge 或 cherry-pick 字节 DeerFlow，未修改用户的 `config.yaml`，未构建或替换已发布的桌面便携包。

## 默认生效的修复

- 上传删除保留请求路径，不跟随符号链接删除同目录的目标文件；API 和 SDK 复用同一实现。
- `read_file` 的循环检测按准确行窗口判断重复。输出优先在完整行边界截断，续读提示使用文件绝对行号；超长单行明确提示可完成的单行读取或字符切片，避免重复前缀。
- 流式输出跟踪每条消息已发送的正文，补发同 ID 消息后处理追加的内容及新增诊断信息；历史回答、工具调用和 usage 不重复发送。
- 子 Agent 显式关闭异步流后再关闭模型资源。服务 shutdown 在重复取消下完成清理，保留原有 memory flush 超时和“后台写入未结束就不关闭 memory manager”的边界。
- 正常终止却无可见回复的模型响应有一次受运行范围约束的恢复机会，耗尽后返回明确失败。工具调用、媒体及 length/safety 终止不误触发此恢复。补充 HTTPX 超时分类；DeepSeek adapter 补齐 thinking 工具历史边界。
- DeerMem 查询注入保留 BM25 召回顺序。无查询时仍按原 confidence 规则排序；数据格式不变。

## 技能按意图发现

```yaml
skills:
  discovery_mode: auto
  discovery_threshold: 20
```

`discovery_mode` 支持 `"off"`、`auto`、`"on"`，默认 `"off"`。`auto` 在当前 Agent 可见的启用技能达到阈值后，用简短名称索引代替全量描述，并提供 `describe_skill`。该工具按中英文任务关键词检索技能元数据；`select:name1,name2` 明确选择技能，不会被普通搜索结果数限制截断。发现范围服从 Agent 的 `skills`，子任务继承父级限制。

主链复用技能缓存，发布、回滚及启停后按已有失效机制刷新。SDK 工厂使用显式技能配置，保持其配置注入方式。YAML 中的 `off` / `on` 要加引号，避免被 YAML 1.1 解释成布尔值。

SDK 可传 `skills_config=SkillsConfig(path="/your/skills", discovery_mode="on")` 和 `available_skills=["research"]`。显式技能目录及允许列表随原生 `task` 子任务传递，每轮 metadata 只能进一步缩小范围；仅指定名称而未提供目录时，子任务不会回退读取全局同名技能。SDK 子任务的模型及其他工具仍使用该项目现有运行配置，工厂装配本身不隐式读取全局 YAML。

显式 SDK 目录提供技能元数据，返回路径由 `container_path` 决定；调用者需提供与该路径匹配的文件读取工具或挂载。它不会自动把 SDK 目录挂载进应用的全局 sandbox，也不会因同名而使用全局技能正文。

## 每个 Agent 选择 MCP 服务

在自定义 Agent 的配置中设置 `mcp_servers`，值为 `extensions_config.json` 的服务键名。

```yaml
mcp_servers:
  - openviking
  - filesystem
```

省略或 `null` 继承启用服务；`[]` 不加载 MCP 工具；列表只选择指定服务。不存在或已禁用的服务不会提供工具，配置中的选择仍然保留。主 Agent、子任务和延迟工具发现使用同一限制，不修改全局 MCP 工具缓存。该选择控制工具装配，不授予额外访问权限。ACP 客户端提供的临时 MCP 服务也按其可信服务名称过滤。

## 记忆多样性排序

```yaml
memory:
  backend_config:
    retrieval_mmr_enabled: true
    retrieval_mmr_lambda: 0.7
```

MMR 默认关闭。开启后在本地 BM25 候选中兼顾相关性和内容差异，减少多条相似事实占据注入预算；`lambda` 越大越偏向相关性。本次不增加向量服务、不迁移记忆数据，也不替换 mem0 后端。

## RAGFlow 范围与来源

原有 `knowledge_search` 工具配置继续生效。工具结果增加调用级来源 ID、文档/分块定位、实际返回的摘录与哈希；有可靠字段时附带页码。这里的引用记录的是检索证据快照，不代表系统独立验证了文档内容的真实性。

如果运行环境提供 outputs 目录，会将进入结果预算的摘录保存为 `knowledge-<id>.md`，并返回可打开的产物链接。API/SSE 保留 `knowledge_sources` 元数据，AG-UI 发送 `deerflow.knowledge_sources` 自定义事件，ACP 通过现有产物资源链接展示，因此原生桌面可打开这些来源文件。未提供 outputs 目录时仍返回来源元数据和摘录。

引用和摘录按整条处理输出预算，不保留指向已经移除来源的引用。子任务完成或产生不完整结果时可以传回已经观察到的检索证据，并保留不完整标记；这不会把未完成任务标记为成功。

API 的每轮范围示例：

```json
{
  "message": "根据选中文档回答问题",
  "knowledge_scope": {
    "dataset_ids": ["dataset-id"],
    "documents": [
      {"dataset_id": "dataset-id", "document_id": "document-id"}
    ]
  }
}
```

`/chat/stream` 使用 `knowledge_scope`；AG-UI 使用 `knowledgeScope`。Python SDK 在 `stream` / `astream` / `chat` 每次调用中传 `knowledge_scope=...`。ACP prompt 可通过 `_meta` 的 `deerflow.knowledge_scope` 扩展字段传相同结构。

范围只缩小操作员配置允许的数据集。省略字段使用操作员范围，显式 `dataset_ids: []` 或 `documents: []` 表示不检索；显式 `knowledge_scope: null` 重置本轮限制。直接澄清回复继承原问题范围，连续澄清也不会扩大范围。新普通问题重新使用其本轮选择。范围随子任务传递，不能作为模型的工具参数改写。

文档选择先通过按数据集分页的只读接口验证，避免大批选择产生过长 URL；不可访问的文档使检索失败。调用方需要提供选择控件或已知 ID，本次没有增加桌面专用的知识库选择面板。

## 可选模型输入脱敏

```yaml
pii_redaction:
  enabled: true
  email: true
  phone: true
  bank_card: true
  chinese_id: true
  api_key: true
```

总开关默认关闭。启用后，对传给模型的消息与系统提示副本进行规则脱敏，包括历史工具参数、工具结果和 reasoning 字段；摘要及标题的独立模型调用也使用此配置。身份证和银行卡采用格式/校验位检查。替换使用固定的不可逆标记，没有恢复原值的映射表。

原始会话、实际执行的工具参数、用户可见的检索来源快照保持原内容。记忆提取和其他独立外部集成不在本开关覆盖范围内；该功能不等同于整个系统的数据防泄漏策略。需要真实联系人等信息的任务应按业务需要选择检测项。

SDK 工厂可显式传入 `pii_redaction=PiiRedactionConfig(enabled=True)`，策略也绑定到原生 `task` 子任务。完全接管 middleware 列表的调用者需自行安装脱敏中间件及配置辅助模型。更多边界见 [脱敏说明](pii-redaction.md)。

## 应用与验证

修改配置后按现有流程保存并应用，或重启服务/ACP daemon。原有配置无需新增字段即可启动。新功能的默认关闭项不会自动改写用户设置。

验证覆盖模拟模型、模拟 RAGFlow HTTP、主/子 Agent 范围、API 每轮传递、流式事件、真实异步生成器清理和重复取消。测试不使用真实模型凭据或真实知识库数据。Windows 环境无法创建真实符号链接时，对应用例按平台条件跳过；另有无需该权限的路径保护回归。

2026-09-20 最终验收：

- 全量 `python -m pytest tests -q -ra --tb=short --maxfail=10`：**1425 passed、8 skipped、15 subtests passed**，无失败。
- 跳过项为 7 个当前 Windows 权限不支持的真实符号链接用例，以及 1 个 POSIX `flock` 专用用例。已有一个飞书第三方 protobuf 的时间 API 弃用提示。
- SDK 显式子任务策略、来源预算后模型输入脱敏的同步/异步真实 Agent 图另有定向验收通过；持久化历史和来源 artifact 仍保留原文。
- `python -m compileall -q app deerflow`、`git diff --check` 通过；配置示例通过 `AppConfig` 校验，确认新增可选开关默认关闭。
