# 记忆修复审查项复核报告

日期：2026-11-17。复核对象：[原审查文档](memory-fix-review-findings.md)的 B1–B6。对照当前工作区和已交付快照 `86e83d6bf4e5260935cdc1b361efc0325838ce60`；本轮仅复核和记录，**没有修复这些实现，也没有因此再次部署**。

结论：**B1–B4、B6a、B6b 仍有对应问题；B5 描述的标签行为存在，但它是现有策略和提示词含义的设计问题，不能当作已经确认的安全漏洞。** 上轮模型修复只调整提取与显式工具指引、保护 V3/V4 固定契约，没有覆盖这些位置。现有回归全部通过不代表以下边界不存在；尤其 B1 仍是 F5 的 P1 缺陷，必须重新列入发布阻塞项。

## 复核方法

用 Go `-overlay` 注入两个临时测试文件，运行根包和 `internal/memory` 的 7 个顶层探针、**36 个叶子用例，全部得到预期观察结果，无跳过**。这里的“通过”表示成功复现当前行为及其对照，不表示缺陷已修复。测试只用临时 SQLite、合成输入与进程内假模型，不访问真实用户数据或模型服务。探针、命令、摘要与结果见[机器记录](reports/memory-review-recheck-20261117.json)，归档于桌面交付目录的 `acceptance/review`。

## 逐项结论与修正建议

| 项目 | 是否仍存在 | 证据与边界 |
|---|---|---|
| **B1 / P1：跳过 PostRun 后 Flush 永久失败** | **确认，真实 SDK 路径复现** | `max_tokens` 和空回答 `end_turn` 均产生 completed run、pending 决策、空 job_id；连续两次 Flush 都为 Failed=1/decision_failed，旧接口返回 ErrMemoryFlushFailed，Close/Open 后依旧如此 |
| **B2 / P2：遗忘语句匹配** | **确认** | 原文六个例子的结果逐一复现：中文“我”被吞掉、英文 `that` 未处理、`忘掉` 不支持。不同目标的请求仍拒绝。按 ID 的管理删除和已支持的属性别名仍可用 |
| **B3 / P2：活动被当作地点** | **确认** | `我在写代码`、`我现在在开会`、`我在想` 在显式与自动校验中全部被接受为 location；`我住在北京`、`记住我在上海` 正对照也接受。此证据是宿主 validator 允许，不是声称真实模型一定选择该 key |
| **B4 / P2：整段扫描降低召回** | **确认其行为** | 原文五条混合消息，在两种写入路径中均被 `quoted_ambiguous_instruction_or_secret` 拒绝，即便 evidence 只指向完整姓名句。属于保守策略带来的有效输入误拒，不是越权写入漏洞 |
| **B5：external_context 标签范围** | **行为确认，需设计决策** | 新鲜输入为 user_supported；作用域已有事实、同会话有上一轮、或已有工具回执时均变为 external_context。无需真的读取第三方文件。现有文档本就把工具/记忆/历史视为可能的外部上下文；提示词却将其解释为“读过第三方内容”并要求确认，含义过强 |
| **B6a / P3：部分可选错误直接返回** | **确认代码分支与故障路径** | 三处 raw return 仍在。删除合成输入的 pinned scope 后，PostRun 返回 ErrInvalidInput；它不属于 memoryCriticalError 的核心错误，决策仍 pending。不是健康数据库常规请求的复现；另两处按代码确认，未伪称均已做瞬时 SQL 故障注入 |
| **B6b / P3：坏任务阻塞队首** | **确认，需缩小原描述** | 删除队首任务的 harness_inputs 行、保留 memory_inputs 后，两次 ClaimJob 都返回 sql.ErrNoRows，两条任务持续 queued。对照：删除 memory_inputs 行会被既有 legacy 处置处理，健康任务可认领；不能说所有缺失快照/坏 payload 都会堵塞 |

### B1：需要修复，但不能照搬“所有 pending/started 改 skipped”

关键位置为 [engine.go](../go-harness/internal/engine/eino/engine.go) 第 633 行的 PostRun 条件、[staging.go](../go-harness/internal/memory/staging.go) 第 304 行只在有 staging 条目时写决策，以及 [flush.go](../go-harness/internal/memory/flush.go) 第 22、192 行的固定集合与失败分类。[execution_service.go](../go-harness/internal/runtime/execution_service.go) 第 378 行之后没有给未经过 PostRun 的正常终态补齐决策。

修复应明确区分“没有调用可选提取”与“提取已开始但失败/状态丢失”。前者在终态事务内写可解释的 skipped 原因；后者不能被统一 UPDATE 掩盖。`waiting_input`、可恢复执行、已保存响应、预算 unknown 和 hold 必须保持原语义；failed/cancelled 也应与当前 discarded 分类一致，而不是一律改成成功跳过。

历史修复只处理能够证明属于该缺陷的已终态行，保留真正失败。补充预算停止、空回答、连续 Flush、重开、正常后续轮、等待输入恢复，以及真实失败不被吞掉的测试。不要通过缩小 Flush 的历史集合来隐藏坏决策。

### B2：扩大固定语法，保留目标绑定

位置：[write_validation.go](../go-harness/internal/memory/write_validation.go) 第 211 行。修正代词提取、有限的 `forget that` / `忘掉` 语法，并将正则预编译到包级是合理方向；仍须将请求匹配到宿主解析的具体目标。不要通过任意前缀删除、子串命中或不核对目标来提高接受率。原报告的六行表可以作为回归，增加不同目标、引述、否定和无效目标对照。

### B3：原文推荐方案会改变已承诺的自动地点能力

位置：[write_validation.go](../go-harness/internal/memory/write_validation.go) 第 87 行。现有计划、提示词正例、确定性测试和两条真实模型路线均支持自动保存“我在北京”。原建议禁止自动的裸 `在X` 会破坏这一行为，必须作为产品语法变更单独决定，不能称作无兼容影响的修补。

而且允许显式 `记住我在X` 仍会接受“记住我在写代码”，并没有完整解决类型误判。后续应先明确可支持的地点声明范围，对两条写入路径加入活动负样本，并保留已承诺地点正样本；若收窄范围，须同步写入协议、模型示例、版本策略及真实质量测试。有限动词黑名单只能减少误判，不能宣称完成任意地名的语义验证。

### B4：不能仅截取相邻句就放松来源保护

位置：[write_validation.go](../go-harness/internal/memory/write_validation.go) 第 55、65 行。缩小检测范围需要理解 evidence 所处的引述、条件、否定范围；“证据句加前一子句”不是充分安全条件，跨句引用和条件依赖也应覆盖。可以独立修正英文单词边界和单词内部撇号，但需保留完整声明、第三方转述、引号/代码、凭据、权限类拒绝及逐条候选隔离的回归。对召回改善和安全约束分别验收。

### B5：应解决语义混用，而非直接删除标签

位置：[source.go](../go-harness/internal/memory/source.go) 第 155、161 行，[write_protocol.go](../go-harness/internal/memory/write_protocol.go) 第 118 行，[extensions.go](../go-harness/extensions.go) 第 243 行及 [tools/memory.go](../go-harness/internal/tools/memory.go) 第 133 行。

`external_context` 当前表示可能暴露于历史/记忆/工具上下文，而不是当前事实没有用户证据。ValidateWrite 通过并不能证明模型未接触这些上下文。因此将所有新行改成 user_supported 是策略调整，不是“修复错误赋值”。可以考虑分别表达“当前用户声明有依据”和“上下文包含外部内容”，或先纠正提示词的解释；无论采用哪种方案，都要保留所有记忆为不可信描述数据的边界、历史 provenance、迁移/提升/导入行为及固定执行契约。探针没有测量“几乎全部实际事实”的比例，也没有证明模型实际每轮追问，不能把推测当测量结果。

### B6：维持错误分级与账本边界

B6a 的位置是 [memory_extraction.go](../go-harness/memory_extraction.go) 第 142、149、160 行。可选来源错误需先通过 memoryCriticalError 分类，再在主执行仍可提交时记 source_unavailable 等审计/决策。原建议中的 `writeError(...)` 并非通用降级器：它只降级已明确回滚的 OptionalWriteFailure，直接传 raw error 仍然会返回错误。不能为了处理这几处分支吞掉取消、fencing、unknown、账本或核心事务失败。

B6b 的位置是 [job_budget.go](../go-harness/internal/memory/job_budget.go) 第 179 行和 [jobs.go](../go-harness/internal/memory/jobs.go) 第 202 行。当前 hydrateJob 失败直接回滚，不推进队首；现有 legacy 清理已经覆盖部分缺失/旧协议情况。修复应按宿主选中的实际行 ID 记录不可恢复来源错误、终态处置及决策，不能信任坏 payload 内的 ID 去更新另一行；临时数据库故障继续返回错误。已付费结果和不明预算必须先按原恢复协议处理，不能将坏来源作为释放 hold 或重发模型的理由。至少加入缺失 harness_inputs、错误 digest/事件、已有 legacy 处置正对照，以及后续健康任务能够推进的测试。

## 后续顺序与发布结论

1. 先修 B1 的终态决策和历史修复，保留真实失败及 waiting_input/预算边界；这是必须重新关闭的 F5 发布阻塞项。
2. 修 B2 及 B6a/B6b 的局部问题；按实际修改范围补充回归与并发检查。
3. B3/B4 先确定语法和来源语义，再修改 validator；对正样本、攻击样本及两种模式重新验收。
4. B5 作为 provenance/提示词策略决定处理，不与前述事务修复混为一项，也不需要因此放宽来源校验。

已经通过的正式构建、全量回归和协议冒烟记录继续有效，但不能据此宣称所有审查缺陷均已消失。当前安装包含上述未修问题；本轮没有自动回退数据库或修改运行配置。后续修复须重新构建和验证，再更新桌面程序。
