# 字节 DeerFlow 近一周更新适配评估

核查日期：2026-09-20。范围：2026-09-13 00:00（北京时间）至查询时，字节 `bytedance/deer-flow` 的 `main` 分支。获取了 138 条提交记录，对 16 个候选提交读取了具体 diff，并对照本项目当前工作区源码。上游快照为 `03505ac4e0b916351dba92e4d42942514b01f9a7`，提交时间为北京时间 09-20 11:40:31。

本项目是独立实现，建议以触发条件、预期行为和回归用例为依据适配，不执行 merge 或整包 cherry-pick。本次没有修改运行代码，也没有覆盖已有工作区改动。

GitHub Releases 查询仍只返回 `v2.0.0`；本周虽有 `2.1.0-rc0` 版本号更新提交 #5521，但不能把它当作已经发布的新正式版。以下结论针对 main 已落地的改动。

**建议先做的修复**

| 顺序 | 上游更新 | 本地结论与影响 | 独立实现落点 |
| --- | --- | --- | --- |
| 1 | [#5547 上传删除不跟随符号链接](https://github.com/bytedance/deer-flow/pull/5547)，09-18 | 明确缺口。删除 `alias.pdf → victim.pdf` 时，先 resolve 会误删同一 uploads 目录内的 victim 和 victim.md；目录外目标已有边界检查，不应扩大描述为任意文件删除。 | [uploads/manager.py](D:/Tools/deerflow-api/deerflow/uploads/manager.py:259)。保留请求路径，拒绝符号链接，删除请求项；检查配套 Markdown 清理。API 与 SDK 共用此函数。 |
| 2 | [#5486 精确读取行窗口参与循环检测](https://github.com/bytedance/deer-flow/pull/5486)，09-17；[#5474 按行截断并保证续读进展](https://github.com/bytedance/deer-flow/pull/5474)，09-16 | 明确/部分缺口。200 行分桶把正常连续分段读取当成同一调用；现有截断虽有绝对行号提示，但仍可能切断一行，超长单行续读可能返回相同前缀。 | [loop_detection_middleware.py](D:/Tools/deerflow-api/deerflow/agents/middlewares/loop_detection_middleware.py:152)、[sandbox/tools.py](D:/Tools/deerflow-api/deerflow/sandbox/tools.py:1331)。精确窗口去重，保留频率限制；按完整行截断，为超长单行提供能推进的读取方式。 |
| 3 | [#5479 同 ID 消息追加内容补发](https://github.com/bytedance/deer-flow/pull/5479)，09-17 | 明确缺口。中间件在同 ID 消息上追加停止说明，stream 因已见 ID 跳过正文，可能出现历史里有说明、实时回答没有说明。 | [client.py](D:/Tools/deerflow-api/deerflow/client.py:1023)。按消息记录已发送文本，仅补发追加部分及新增 metadata，避免重复历史正文；另验证 ACP/SSE 映射。 |
| 4 | [#5221 先关闭子 Agent 流再释放资源](https://github.com/bytedance/deer-flow/pull/5221)，09-18；[#5531 shutdown 在取消期间继续收尾](https://github.com/bytedance/deer-flow/pull/5531)，09-18 | 流关闭问题已动态复现；记忆退出存在源码可见的取消缺口。应避免模型先关闭而 stream 尚未清理、或取消跳过后续服务清理。 | [executor.py](D:/Tools/deerflow-api/deerflow/subagents/executor.py:635)、[dependencies.py](D:/Tools/deerflow-api/app/dependencies.py:1251)。流显式 aclose，生命周期清理应在取消下完成。保留本地快速取消、终态隔离和阻塞线程 quarantine 设计。 |
| 5 | [#5080 provider 边界响应恢复](https://github.com/bytedance/deer-flow/pull/5080)，09-18 | 部分缺口。正常 stop/end_turn 的空回复仍被当作成功；原生 HTTPX 超时分类不完整。DeepSeek 已保留 reasoning_content，但缺失字段、null content 等边界还可加强。length/safety 工具抑制已实现，不重复移植。 | [llm_error_handling_middleware.py](D:/Tools/deerflow-api/deerflow/agents/middlewares/llm_error_handling_middleware.py:584)、[patched_deepseek.py](D:/Tools/deerflow-api/deerflow/models/patched_deepseek.py:57)。有限次空回复恢复、明确最终失败及 usage 保留；DeepSeek 改动限定在对应 adapter。 |

建议回归验收围绕真实故障行为展开：同目录链接删除不得影响目标；不同读取窗口不触发相同调用阈值；超长行续读必须推进；同 ID 补文只发送一次；正常完成、异常、取消和反复取消都按正确顺序关闭流及模型；空响应恢复有次数上限，正常文本和正常工具调用不受干扰。

**值得新增或增强的功能**

| 功能 | 本地现状 | 建议 |
| --- | --- | --- |
| [#5251 相关性与多样性记忆排序](https://github.com/bytedance/deer-flow/pull/5251)，09-18 | 已有 FTS5/BM25 检索，也会把当前问题传入记忆注入链；[deermem.py](D:/Tools/deerflow-api/deerflow/agents/memory/backends/deermem.py:114) 先筛候选，但 [prompt.py](D:/Tools/deerflow-api/deerflow/agents/memory/prompt.py:276) 再按 confidence 排序。当前配置已启用 DeerMem 检索。 | 中优先级，适合当前用法。保留相关性排序，结合置信度，并可选加入 MMR，减少相似事实挤占 token 预算。无需替换整个记忆后端或重复建设 query 检索。 |
| [#5369 技能按任务意图延迟发现](https://github.com/bytedance/deer-flow/pull/5369)，09-17 | 当前向模型注入所有选中技能的名称、描述及路径，[prompt.py](D:/Tools/deerflow-api/deerflow/agents/lead_agent/prompt.py:628)。尚无上游同型 SkillCatalog/describe_skill。 | 技能多时价值较高，但属于中等改造。新增受 Agent 技能选择约束的检索/描述接口，再按任务意图排序；用户明确点名的技能必须稳定保留，目录缓存需跟随技能演进发布/回滚失效。不能只复制排序函数。 |
| [#5238 每条消息选择 RAGFlow 检索范围](https://github.com/bytedance/deer-flow/pull/5238)，09-18；[#5551 可验证来源引用](https://github.com/bytedance/deer-flow/pull/5551)，09-19 | 已有 RAGFlow 工具及 `[1]` 片段编号，[formatting.py](D:/Tools/deerflow-api/deerflow/community/ragflow/formatting.py:34)；编号尚不等同于稳定、可点开的来源链。当前实际配置尚未启用 knowledge_search。 | 使用私有知识库时再提高优先级。先实现稳定 document/chunk 来源及引用元数据透传，再接桌面显示；可选的每消息范围只能缩小操作员允许的数据集范围，澄清、重试和子任务需继承相同范围。[#5572](https://github.com/bytedance/deer-flow/pull/5572) 的大批文档选择校验应随 scope 功能一并考虑。 |
| [#5527 模型输入中的 PII 脱敏](https://github.com/bytedance/deer-flow/pull/5527)，09-19 | 本地已有部分输入防护、日志/演进信号脱敏，但不是统一的模型输入 PII 检测器。 | 按场景可选开启。主模型、工具结果、摘要与标题调用都需覆盖；工具结果含 Command 时也要处理。占位符会影响需使用真实联系人/证件信息的任务，不宜默认全面替换。记忆提取另有模型调用，上游本项也没有承诺覆盖它，不能称为全链路隐私保护。 |
| [#5497 统一能力目录、插件配置及 Agent 选择](https://github.com/bytedance/deer-flow/pull/5497)，09-19 | 本地已有技能、MCP、Agent 和原生桌面设置，其存储与管理入口有自己的结构。 | 较低优先级、改造范围较大。可借鉴统一目录与稳定安装 ID，让 Agent 按插件选工具；复用现有配置、密钥脱敏与运行时过滤。目录选择不等同于权限授予，不搬上游 Next.js 管理界面。 |

RAGFlow 引用不仅是格式化工具输出：本地流事件目前没有完整透传 ToolMessage.artifact，工具输出预算和子任务报告也需要保留引用元数据。这是端到端功能，应以当前 FastAPI/ACP/桌面链路设计接口。

**已具备的能力，不重复建设**

- 模型请求 RPM 排队 #5432/#5459，对应 [request_admission.py](D:/Tools/deerflow-api/deerflow/models/request_admission.py:24)；项目另有模型调用并发 gate，不能说完全没有并发控制。
- 工具调用被裁剪时同步 structured/raw/provider 内容块 #5447，对应 [tool_call_metadata.py](D:/Tools/deerflow-api/deerflow/agents/middlewares/tool_call_metadata.py:97)；length 截断时禁止继续执行工具也已存在。
- PowerShell UTF-8 #5440、Windows 路径遮蔽、Agent 记忆开关 #5167、技能安装检查，本地 09-17 提交已经覆盖相关行为。
- 记忆近似去重 #5254、读前写保护、stream-only 模型兼容、图像生成支持等，本地 09-15 已做相关独立实现。
- checkpoint delta、缓存和 mem0 属于更早的已实现能力，不是本周新功能。

**不应直接照搬的上游修复**

- #5454 子 Agent 压缩丢系统提示：本地常规子 Agent 没有相同摘要链，角色契约经 `create_agent(system_prompt=...)` 注入。未来增加子 Agent 压缩时作为回归约束。
- #5488 SDK 工厂 DurableContext/token_budget：本地 RuntimeFeatures、子任务计数和摘要回注机制不同，缺少同名类不构成同型故障。
- #5477 容量 slot 重复取消、#5525 durable batch stop：本地没有上游同型 capacity/batch ownership 服务；本地线程池与隔离机制应保留。
- #5522 MCP 裸文件名 regex 替换：本地没有该路径重写机制。
- 前端重新加入活动 run、Projects 文档架、账号偏好、Nginx/Docker 启动修复等，可以参考行为，但主要依赖上游 Gateway/Web 架构，当前优先级低于已确认的 harness 缺口。
- #5308 checkpoint retention 仅实现受约束的删除服务，上游说明明确未连接生产触发器；本项目已有会话清理与 delta checkpoint，不能直接引入该删除策略。

**验证边界与来源**

- 对真实 `_stable_tool_key` 函数做无副作用验证，`1–40 / 41–80 / 81–120 / 121–160 / 161–200` 全部得到 `example.py:0-0`。
- 使用真实 SubagentExecutor._aexecute、假异步流及假模型，无网络调用。取消返回时清理记录只有 `model_closed`，手动 aclose 保留的流引用后才出现 `stream_closed`，证实清理顺序缺口。
- 上传删除属于源码与上游修复前同型确认；Windows 测试环境不允许创建符号链接，未完成该项动态复现，也未为此申请提升权限。
- 其余为静态代码适配评估，并非全量生产场景测试。本次未修改运行实现、依赖、用户配置或 Git 分支。
- 原始 GitHub API 清单、release 记录和候选 diff 保存在工作区 `.tmp/upstream-review-20260920/`，便于后续按相同快照实现。
- [上游提交历史](https://github.com/bytedance/deer-flow/commits/main/)、[上游 Releases](https://github.com/bytedance/deer-flow/releases)、[本次上游快照](https://github.com/bytedance/deer-flow/commit/03505ac4e0b916351dba92e4d42942514b01f9a7)。
