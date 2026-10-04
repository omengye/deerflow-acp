# 旧 Python 数据与行为迁移基线

本文件记录 Eino 单运行时收敛时移除的旧实现。它是历史数据导入和 Go 行为验收的参考，不是当前运行或构建入口。下列源码均固定到提交 `9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9`；当前工作树不保留可执行 Python 副本。

## 查阅方式

在含有该提交的 Git checkout 中，用 `git show 9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9:<path>` 只读查看。发布后历史仓库链接仍指向同一提交。导入器只读取用户明确选择的 YAML/JSON/Markdown/技能资源，不运行旧模块或支持脚本。

| 迁移依据 | 固定基线 |
| --- | --- |
| 进化提案、信号、版本模型 | [models.py](https://github.com/omengye/deerflow-acp/blob/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9/deerflow/skills/evolution/models.py) |
| 进化磁盘布局、树摘要、目录发布 | [store.py](https://github.com/omengye/deerflow-acp/blob/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9/deerflow/skills/evolution/store.py) |
| 发布及观察期数据 | [publisher.py](https://github.com/omengye/deerflow-acp/blob/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9/deerflow/skills/evolution/publisher.py) |
| 自动发布约束与旧评判 | [evaluator.py](https://github.com/omengye/deerflow-acp/blob/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9/deerflow/skills/evolution/evaluator.py) |
| 旧反馈/回滚观察器 | [monitor.py](https://github.com/omengye/deerflow-acp/blob/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9/deerflow/skills/evolution/monitor.py) |
| 自定义 Agent 字段、SOUL 和空列表语义 | [agents_config.py](https://github.com/omengye/deerflow-acp/blob/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9/deerflow/config/agents_config.py) |
| DeerMem 1.0 数据形状和作用域路径 | [storage.py](https://github.com/omengye/deerflow-acp/blob/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9/deerflow/agents/memory/storage.py) |
| 旧 ACP 提案管理 | [proposal_control.py](https://github.com/omengye/deerflow-acp/blob/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9/deerflow/acp/proposal_control.py) |
| HTTP 专用管理，仅历史参考 | [proposal_review.py](https://github.com/omengye/deerflow-acp/blob/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9/app/proposal_review.py) |

## 旧技能进化目录契约

根目录来自旧 `skill_evolution.storage_path`。相对路径原先相对用户数据根目录，不能相对新的进程工作目录猜测。导入时应显式指定源根，读取原始数据并记录来源和摘要，不修改源目录。

| 相对路径 | 数据 |
| --- | --- |
| `state.json` | `schema_version: 1`、`catalog_version`、`active_revisions`、`observations`、`probations`、`last_updated_at` |
| `proposals/<id>/proposal.json` | `SkillProposal`；状态、动作、目标、来源、基线/候选摘要、风险、扫描、评测、审查/发布/归档时间 |
| `proposals/<id>/candidate/<skill_name>/` | 候选技能树，可能含支持文件；只按数据读取，禁止执行 |
| `proposals/<id>/diff.patch` | 审查辅助文本，不作为导入器执行指令 |
| `revisions/<skill_name>/<version>/manifest.json` | `RevisionManifest`；前版本、提案来源、树摘要、删除和回滚元数据 |
| `revisions/<skill_name>/<version>/snapshot/` | 历史技能树；删除版本可以没有快照 |
| `signals/s_<id>.json` | 有限任务摘要、工具错误、使用技能及原信号状态 |
| `audit.jsonl` | `ts`、`actor`、`action`、`details` 的历史审计记录 |

旧树摘要按文件相对 POSIX 路径排序，对每个文件累加 `UTF8(path) + NUL + raw_content + NUL` 后计算 SHA-256；符号链接应拒绝。该摘要不可直接冒充 Go 的内容版本或来源身份。

旧提案状态包含 `generating/validating/pending_review/publishing/published/rejected/failed/stale`；动作包含 `create/edit/patch/delete/write_file/remove_file`。导入状态不能启动旧生成任务、自动批准、重新发布或恢复旧观察器。已发布版本保留为历史；旧待审记录需在当前来源、权限和基线下重新验证。中断状态必须标记为历史中断或需人工处理。

旧观察期含 `revision`、`previous_revision`、`remaining_uses`、`consecutive_failures`、`auto_published`、`status`、`started_at`、`last_observed_at`、`last_failure`。这些旧计数只可作为历史描述。旧观察器会用技能使用后的未恢复工具错误推断失败，不能作为新实现可信归因，也不能直接触发 Go 自动回滚。新验收须区分网络/额度/权限/取消与技能质量。

合成数据样本见 [legacy-python-migration.json](fixtures/legacy-python-migration.json)。样本不来自用户配置、聊天、密钥或数据库；它是字段与布局参考，不代表 Go 历史导入器已经完成。

## Agent 与记忆导入边界

旧 `agents/<name>/config.yaml` 含 `name/display_name/description/model/tool_groups/mcp_servers/skills/memory_enabled`，`SOUL.md` 为单独的可选人格文本。`skills` 与 `mcp_servers` 的省略或 null 表示继承，显式 `[]` 表示关闭；不得因转换丢失三态而扩大访问范围。旧 Agent 名称约束为 `^[A-Za-z0-9-]+$`，显示名长度与控制字符校验有独立规则。新角色身份、模型存在性和父任务权限仍须由 Go 校验，不能将旧 config 当可直接运行的 Go 配置。

DeerMem 文档为 `version: "1.0"`，包含 `facts` 及 `user/history` 摘要区域。现有 `go-harness/internal/memory/legacy.go` 的显式事实导入已独立实现，具有大小/编码、ID 唯一性、内容过滤、事务及幂等校验；它不声称将旧摘要区域全部转换成 Go 记忆。LangGraph 检查点不转换成 Go 执行快照。既有 Go 检查点的兼容测试继续保留。

## 后续行为验收索引

| 功能 | 固定旧测试文件及代表用例 | 当前 Go 对应区域 |
| --- | --- | --- |
| 角色限制、上下文/预算/后台身份 | [subagent policy](https://github.com/omengye/deerflow-acp/blob/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9/tests/test_local_acp_subagent_policy.py)、[context snapshot](https://github.com/omengye/deerflow-acp/blob/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9/tests/test_subagent_context_snapshot.py)、[background identity](https://github.com/omengye/deerflow-acp/blob/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9/tests/test_subagent_background_identity.py) | `go-harness/internal/agents/`、`internal/background/`、`internal/runtime/` 与根 SDK 角色/后台测试 |
| 候选、人工审查、版本冲突与回滚 | [test_skill_evolution.py](https://github.com/omengye/deerflow-acp/blob/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9/tests/test_skill_evolution.py)：`test_review_proposal_does_not_change_active_skill_until_approved`、`test_patch_conflict_and_rollback_are_versioned` | `go-harness/internal/skills/evolution/`、`internal/skills/publications*` |
| 重复信号、worker 恢复与自动发布 | 同上：`test_repeated_task_creates_one_signal_then_respects_cooldown`、`test_worker_recovers_pending_and_interrupted_signals`、`test_eligible_automatic_patch_is_published_with_probation` | `internal/skills/evolution/` 与 `internal/skillmaintenance/`；自动发现、完整评测和 auto_patch 仍以主方案验收为准 |
| 自动回滚与失效观察 | 同上：`test_probation_graduates_after_successful_skill_uses`、`test_auto_published_regression_rolls_back_but_admin_publish_only_alerts` | 新 Go 观察器仍须补齐可信归因与版本匹配，禁止复制旧误归因逻辑 |
| ACP、回执、工具故障 | [test_local_acp_runtime.py](https://github.com/omengye/deerflow-acp/blob/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9/tests/test_local_acp_runtime.py)、[test_host_opencli_tool.py](https://github.com/omengye/deerflow-acp/blob/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9/tests/test_host_opencli_tool.py) | `go-harness/internal/acp/`、`internal/interaction/`、`internal/tools/`、SDK 回执与恢复测试 |
| 文件工具与沙箱 | [test_file_search_contract.py](https://github.com/omengye/deerflow-acp/blob/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9/tests/sandbox/test_file_search_contract.py)、[test_wsl_sandbox.py](https://github.com/omengye/deerflow-acp/blob/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9/tests/sandbox/test_wsl_sandbox.py) | `go-harness/internal/sandbox/`、`internal/wslhelper/` |

删除旧 Python 专用测试不表示所有旧产品功能都被承诺迁移。旧 HTTP 服务、Python SDK、Buzz/Raft Python 适配器和弃用示例在此分支撤下；支持的产品入口是 Eino Go SDK/ACP 与 Rust Waku/Bridge。完整子代理、自进化、配置与单运行时交付仍按主方案 P0–P7 的 33 项逐一验收，不能用本索引代替实测。
