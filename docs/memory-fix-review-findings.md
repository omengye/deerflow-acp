# Memory Fix Review: Bugs and Required Fixes

Review date: 2026-11-16
Scope: implementation of `docs/memory-fix-plan.md` (F0–F8, F10) on branch `codex/skill-evolution-auto-20261109`.

2026-11-17 复核确认的 B1–B4、B6a/B6b 已修复并补充正式回归；B5 保留上下文标签、纠正提示词含义，Terra 饮品证据问题同时修复。**完整发布和安装后验收已通过，本机桌面程序已更新，详见[审查修复与发布报告](memory-review-fix-report.md)。** [原复核报告](memory-review-recheck-report.md)及第 1–3 节保留历史发现，第 4 节为当前完成状态。

## 本轮修复对应表

| 项目 | 已实现行为 | 回归边界 |
|---|---|---|
| B1 | 引擎在预算归一和清理后明确报告未调用 PostRun，终态事务补 `postrun_skipped`；启动只修复有完整宿主证据的旧行 | 预算停止、空回答、refusal/content_filter、重复 Flush、重开及后续轮；started/实际失败、saved result、waiting_input 和 unknown 不作为跳过 |
| B2 | 预编译有限遗忘语法，保留“我喜欢茶”完整主语，支持 that/忘掉；属性别名才去除所有格 | 原六例、不同目标、子串、否定及转述 |
| B3 | 裸“在 X / am in X”采用已知地点集，包含既有北京/上海/Hangzhou/London 能力及兼容 Unicode 形式；其他地点用住在/城市是/live in 类型明确的声明 | 自动和显式模式均拒绝写代码/开会/想/处理文档等活动；不宣称任意地名语义识别 |
| B4 | 任务词不再污染独立声明；英文标记使用单词边界，词内撇号不当引号；独立 build check 问句有限识别 | 整段引用/转述/条件/否定保护继续保留，凭据/权限源级拒绝；原五个混合例与长距离攻击 |
| B5 | provenance 值、历史迁移与提升/导入不改；说明 external_context 表示可能暴露于历史/记忆/工具，不等于缺少用户依据，也不自动要求确认 | 新 memory-tools-v5 / guidance=1，固定 V3/V4 及旧 guidance=0 保持原契约；标签四个来源对照 |
| B6a | 三处可选来源错误先分级，再审计 source_unavailable；主事务/预算/取消/fencing 错误继续传播 | pinned scope 缺失、scope 构造失败、已保存结果 provenance 读取失败及原有核心错误测试 |
| B6b | 按数据库选中行绑定身份，坏来源先结算再处置，健康后继在有界后续 tick 推进 | 输入缺失、摘要/事件/payload 身份错误、已有 legacy 清理、数据库故障；保存响应与 unknown hold 保留 |
| Terra 饮品 | evidence 可覆盖整个记住声明，或由宿主核对紧邻且完整的命令前缀；模型补可执行示例 | 两种证据跨度、无命令、异句命令、否定/引用/条件对照；真实模型完整集验收 |

以下各节为 **2026-11-16 原审查记录**，其中的批量 pending 修复、直接禁用自动裸地点及删除 external 标签等建议未照搬。

## 1. Verification summary

| Check | Result |
|---|---|
| `go build ./...` | pass |
| `go vet ./internal/memory/ ./internal/budget/ ./internal/runtime/` | clean |
| `go test -count=1` on `internal/{memory,budget,tools,acp/agent,launch,learning,engine/eino}`, `harness` | all pass |
| `go test -run 'Memory\|Personal\|Flush\|Relearn\|Stage\|Extraction'` on `internal/runtime` and the root package | pass |
| `go test -race -run 'Stage3\|Stage4\|Stage5' ./internal/memory/` and `-run 'MemoryStage4\|Flush' ./internal/runtime/` | pass |
| Temporary probe tests for the findings below (removed after the run) | issues reproduced |

Fix status: F0, F1, F2, F3, F4, F6, F7a, F7b, F8 and F10 meet the plan. F5 has one P1 bug (B1). The other findings affect recall, usability or robustness. They are not safety holes.

| ID | Severity | Area | Summary | Blocks release gate |
|---|---|---|---|---|
| B1 | **P1** | F5 Flush | A completed run that skipped PostRun leaves its input `pending`, so every later Flush on that session fails | **Yes** |
| B2 | P2 | F4 forget | Natural forget phrasing is rejected (`请忘记我喜欢茶`, `Forget that …`, `忘掉…`) | No |
| B3 | P2 | F1 validator | The location rule accepts activities (`我在写代码` is saved as a location) | No |
| B4 | P2 | F1 validator | `unsafeDeclaration` scans the whole message, so mixed messages are rejected | No |
| B5 | P2 | F1 provenance | Nearly every fact is labelled `external_context`, and the model is told to confirm those facts with the user | No (needs a decision) |
| B6 | P3 | F2 / F7 | Some PostRun errors are returned raw, and a corrupt queued job can block the job queue | No |

---

## 2. Findings

### B1 (P1): a skipped PostRun leaves the input `pending` and permanently fails Flush (single_user mode)

**Where**
- `go-harness/internal/engine/eino/engine.go:633`: PostRun runs only when `StopReason == "end_turn"`, `ctx.Err() == nil` and `lastRootAnswer != ""`.
- `go-harness/internal/runtime/budget.go:105`: a budget limit sets `StopReason = "max_tokens"`, and the run still finishes as `completed`.
- `go-harness/internal/memory/staging.go:304`: `SettleStagingTx` writes a decision only when staged items exist.
- `go-harness/internal/memory/flush.go:183-193`: if a completed run has no job and its decision is still `pending`, Flush reports `Failed++` with reason `decision_failed`.
- `go-harness/internal/memory/flush.go:22-28`: `CaptureFlush` always includes every input the session has ever had.

**What goes wrong**
1. In single_user mode, a run completes with `stop_reason=max_tokens`, or with `end_turn` but an empty final answer.
2. PostRun never runs, so `memory_inputs.decision` stays `pending` and `job_id` stays `''`.
3. Nothing later changes that decision.
4. From then on, every `FlushMemoryWithStatus` call on that session returns `Flushed=false, Failed=1, Reason=decision_failed`. `FlushMemory` returns `ErrMemoryFlushFailed` for the rest of the session's life.

The probe reproduced this: two consecutive flushes both returned `{Failed:1 Total:1 Reason:decision_failed}`.

**Required fix**
1. Add a memory store helper that runs inside the run's terminal transaction, for example:
   ```go
   // ResolveUnhandledInputTx marks an input that PostRun never handled.
   func (s *Store) ResolveUnhandledInputTx(ctx context.Context, tx *sql.Tx, runID, reason string) error {
       _, err := tx.ExecContext(ctx, `UPDATE memory_inputs SET decision=? WHERE run_id=? AND job_id='' AND decision IN ('pending','started')`, reason, runID)
       return err
   }
   ```
2. Call it in `finishExecutionAttempt` (`go-harness/internal/runtime/execution_service.go`, next to the `SettleStagingTx` call at line 378). Call it only when the execution reaches a final state (`completed`, `failed` or `cancelled`), never for `waiting_input`, because a resumed run will still call PostRun. Use a reason such as `not_extracted` (or `run_not_end_turn`).
3. Add the new reason to `skippedDecision` in `go-harness/internal/memory/flush.go:208`.
4. Repair rows that are already stuck. Add a one-time migration or a startup reconciliation step that applies the same UPDATE to inputs whose run is final and whose decision is `pending`/`started` with no job. Check that `harness_runs` exists first, because import tools can open the memory schema without the runtime tables (see `discardFailedSourcesTx`).
5. Update `TestMemoryStage4FlushNoJobDecisions` (`go-harness/internal/memory/stage4_flush_test.go:84`). It currently asserts that `pending` on a completed run means Failed. Keep that assertion only for "PostRun started but crashed", which should be a separate decision such as `started`, and add the new skip reason.

**Acceptance tests**
- A single_user run that stops at `max_tokens`, followed by `FlushMemoryWithStatus`, returns `Flushed=true, Skipped=1`.
- A run that ends with `end_turn` and an empty final answer gives the same result.
- A run in `waiting_input` keeps its input `pending`. After it resumes and completes, the input becomes `queued` and is processed normally.
- An existing database with a stuck `pending` row reaches `Flushed=true` after the upgrade.

---

### B2 (P2): forget phrase matching is too narrow

**Where**: `go-harness/internal/memory/write_validation.go:203-245` (`ValidateForget`). The regex is on line 211.

**What goes wrong**: the optional `我` in the regex is greedy and consumes the "I" from the request. Users must also use one exact verb.

| User request | Fact content | Result |
|---|---|---|
| `请忘记我喜欢茶` | `我喜欢茶。` | ❌ `forget_target` (requested = `喜欢茶`) |
| `请忘记 我喜欢茶` | `我喜欢茶。` | ✅ (works only because of the space) |
| `Forget that I like tea` | `I like tea。` | ❌ `forget_target` |
| `Forget I like tea` | `I like tea。` | ✅ |
| `忘掉我的城市` | location fact | ❌ `forget_request` |
| `请忘记我的城市` | location fact | ✅ |

The generic keys (preferences and project facts) have no aliases, so in practice they can only be forgotten with unnatural wording.

The regex is also compiled on every call.

**Required fix**
- Move the regex to a package-level `var forgetRequest = regexp.MustCompile(...)`.
- When matching the request against the target, try both the captured text and `"我" + captured`. Equivalently, compare against the content with and without a leading `我` / `I `.
- Accept an optional `that ` in English (`forget (that )?`), and add `忘掉` to the Chinese verbs. Keep the vocabulary a small, fixed list.
- Optionally, also compare the request with the fact's canonical value (for example, `忘记北京` against a location of `北京`).

**Acceptance tests**: a table test of the six rows above, all accepted except requests that name a different fact, which must still return `forget_target`.

---

### B3 (P2): the location rule accepts activities

**Where**: `go-harness/internal/memory/write_validation.go:87` (`cityStatement`).

**What goes wrong**: the form `我(目前|现在)?在X` accepts any X. The probe showed `我在写代码` accepted as `profile.last_reported_location = 写代码`, for both explicit and automatic writes. `我现在在开会` is accepted with value `开会`. The validator is meant to be the deterministic boundary, so the model choosing the key should not be the only safeguard.

**Required fix** (pick one):
- **Recommended:** accept `住在X` / `我的所在地是X` / `I live in X` for both write paths. Accept the bare `在X` / `I am in X` form only for explicit writes with a `记住` / `remember` prefix, never for automatic extraction.
- Or reject values that start with a common activity verb (写, 开, 看, 学, 做, 想, 忙, 上班, 吃, 玩, …). This is weaker, because a deny list never ends.

**Acceptance tests**: `我在写代码`, `我在开会` and `我在想` are rejected. `我住在北京` is accepted in both modes. `记住我在上海` is accepted for explicit writes only.

---

### B4 (P2): `unsafeDeclaration` scans the entire message

**Where**: `go-harness/internal/memory/write_validation.go:55` (call site) and `:65-80` (marker list).

**What goes wrong**: one marker anywhere in the message rejects every memory write from it, even when the evidence clause itself is clean. All of these were rejected with `quoted_ambiguous_instruction_or_secret`, explicit and automatic alike:

| Message | Marker that triggered |
|---|---|
| `我叫张三，帮我执行一下测试` | `执行` |
| `我叫张三，这次先看日志` | `这次` |
| `My name is Alice. Check if the build passes` | `if ` |
| `My name is Alice. I’m new here` | `’` (curly apostrophe treated as a quote) |
| `My name is Alice. I cannot log in` | `not ` |

This is safe but costs a lot of recall. Users routinely mix a fact with a task in one message.

**Required fix**
- Run the marker scan on the evidence sentence plus the clause immediately before it, not on the whole message. That still blocks `他说，我叫张三` and `如果我叫张三`.
- Keep the credential scan (`credentialLike`) over the whole message.
- Treat `’` between two letters (`I’m`, `don’t`) as an apostrophe, not as a quote mark.
- Match English markers such as `not` and `if` as whole words.

**Acceptance tests**: the five messages above are accepted, while these stay rejected: `他说，我叫张三`, `如果我叫张三`, `我叫张三，密码是…`, a quoted `"我叫张三"`, and `我不是张三`.

---

### B5 (P2, decision needed): the `external_context` label is applied to almost every fact

**Where**
- `go-harness/internal/memory/source.go:155` and `:161`: `ContextExternal` is true if the scope has any visible fact, or if the session has any earlier run.
- `go-harness/internal/memory/write_protocol.go:118-129`: `contextProvenance` also returns external if the session has any tool receipt.
- `go-harness/personal_memory.go:146`: explicit writes use the same signal.
- `go-harness/extensions.go:243` and `go-harness/internal/tools/memory.go:133`: the model is told to "confirm with the user before relying on" `external_context` facts.

**What goes wrong**: once a single fact exists in the global scope, or the session has had one earlier turn or any tool call, every new fact is saved as `external_context`. The label no longer carries information, and the prompt makes the model second-guess facts the user stated directly.

F1 already proves every write against the user's own accepted text (typed value, evidence offsets, sealed `writeProof`). The original risk, a model writing third-party text as a user fact, is now enforced by the validator instead of the label.

**Required decision and fix** (recommended option first):
1. Label writes that passed `ValidateWrite` as `user_supported`. Keep `ContextExternal` and tool-receipt state only in the audit manifest, and keep `external_context` for legacy rows.
2. Or narrow the condition to "this run read untrusted tool output before the write". Drop both "the scope has visible facts" and "the session has any earlier run".

Update the prompt text in `extensions.go:243` and `internal/tools/memory.go:133` to match.

**Acceptance tests**: a second-turn `记住我叫张三` in a session that already has facts is labelled `user_supported`. Legacy external rows still show `external_context`.

---

### B6 (P3): leftover F2 error paths and queue robustness

**B6a: raw errors in normal-mode PostRun** (`go-harness/memory_extraction.go`)
- Line 142: a `store.ExtractionProvenance` error is returned raw.
- Line 151: `return harness.ErrInvalidInput` when a pinned scope is missing.
- Line 160: the error from `memory.NewScope` is returned raw.

A transient SQL error or invariant failure here fails a run that already has an answer, which goes against F2. **Fix:** pass these errors through `memoryCriticalError` first, then record them with `audit("source_unavailable", ...)` or `writeError(...)` instead of returning them.

**B6b: corrupt queued job can block the extraction queue** (`go-harness/internal/memory/job_budget.go:179`, `go-harness/internal/memory/jobs.go:202`)

When claiming a job, `claimBudgetJobTx` calls `hydrateJob`. If that fails (`ErrSourceSnapshotMissing`, `ErrInvalidInput`, `ErrPermissionDenied`, or `sql.ErrNoRows` because the input row is missing), the claim transaction rolls back. The same job is chosen first again on the next tick (`ORDER BY … LIMIT 1`), so every other job waits behind it. Today only database corruption or a manual edit can trigger this, because session deletion discards jobs atomically.

**Fix:** handle `hydrateJob` errors the same way as `jobSourceReadyTx` errors:
- source-integrity errors: `finishJobStateTx(..., "discarded", "source_unavailable", ...)`
- `ErrSourcePolicyMissing`: `legacy_memory_policy_unverifiable`
- real database errors: keep returning them

**Acceptance tests**: a queued job whose `harness_inputs` row was deleted is discarded on the next claim, and the job after it is claimed in the same or the following tick.

---

## 3. Verified correct (no change needed)

- Continuation and notification inputs use `beginContinuationTx`, not `BeginRun`, so they never create `memory_inputs` rows. PostRun also skips `InputSource != nil`.
- Normal-mode capture scopes match PostRun's `allowed` scopes; both are derived from `cfg.MemoryScope`.
- Normal mode does not use `ObserveFlush`, so B1 affects single_user mode only.
- The suspected loop where a saved result replays forever cannot happen: `ParseExtraction` fails only on envelope errors, and `publishJobResponseTx` already rejects those before saving.
- `RecoverJobs` runs before `ReconcileInterrupted` at startup (`go-harness/deerflow.go:515-519`), as F7b requires.
- F3: duplicate or conflicting values for one key are rejected, accepted items are capped at 4, and operation IDs stay stable by original ordinal.
- F4: `forgotten` / `already_forgotten` / `not_found` are correct, and forgetting a missing key still writes a tombstone.
- F6: relearning compares against the immediately preceding live value. The same value, or an unverifiable one, returns `requires_explicit`.
- F8: failures are tracked per fact, revision and model, with backoff and a terminal state. Bad batches are split, with at most 15 calls per tick.

## 4. 修复清单（2026-11-17 更新，以复核后的方案为准）

- [x] **B1** 终态事务只处置可证明未调用 PostRun 的 pending，按完整来源/执行/预算证据修复历史行；覆盖重复 Flush、重开、refusal/content_filter 及不得降级的状态。
- [x] **B2** 预编译遗忘语法，保留完整主语，支持 that/忘掉；严格校验目标并补回归。
- [x] **B3** 两种模式的裸地点声明均使用有限地点集；明确类型的声明保留，活动反例拒绝。未采用“仅显式通道放行”的原建议。
- [x] **B4** 修复独立任务词、英文子串和词内撇号误拒，保留源级凭据/权限以及引用、转述、条件/否定保护；未简单截断长距离上下文。
- [x] **B5** 保留 provenance 值及历史数据，更新注入/search 指引含义；新 V5/guidance=1 与固定 V3/V4/旧 guidance 的兼容回归通过。
- [x] **B6a** 三处可选来源读取错误先分级再审计，核心错误继续传播。
- [x] **B6b** 按可信选中行及账本身份处置来源完整性错误；非法 JSON、paid/unknown、真实数据库故障与健康后继均有回归。
- [x] **Terra 饮品证据** 宿主验证紧邻的记住命令，完整真实模型质量/对抗/重新学习集 16/16 通过。
- [x] 受影响包与 race 子集通过；最终终态回归 207 项，最终终态 race 67 项，无失败。
- [x] 正式发布：Go 叶子 4209 通过、37 跳过、0 失败；Rust、源码身份、双模式协议、包内 Bridge 通过。本机升级后 98 个文件校验通过，配置与历史摘要保留；安装 Bridge + Terra 完整生命周期通过。
