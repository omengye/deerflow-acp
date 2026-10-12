# Output truncation investigation - 2026-10-10

## Scope

Investigated the running portable desktop build
`dist/desktop/DeerFlow-Desktop-Go-20261010-103944` using read-only SQLite
connections, source inspection, offline Go tests, and six synthetic requests
to the configured model endpoint. No application source, live configuration,
session state, or running process was changed. The live probes contained no
conversation history and had no tools.

## Incident evidence

- Harness session: `J2PKOZ7UEJUD4ODFH2HGEVVTEO`.
- Run: `TPYSHPAZ2AJZHWW3GHNXI7XM2Y`.
- Desktop message: `d8d4a447-d590-4348-9d65-50ae70116085`.
- Model route: `deepseek-v4.1-flash` at `https://copilot.tencent.com/v2`,
  configured as OpenAI-compatible, with reasoning effort `max`.
- The final model request ran from approximately 11:54:29 to 11:54:41
  Asia/Shanghai. The complete run took approximately 254 seconds.
- Native session event 489 contains the final assistant message. Its last
  characters are `\u4f46\u5b83\u4f4d\u4e8e\u865a`.
- This native message is byte-for-byte identical to the desktop message:
  948 Unicode characters, 1775 UTF-8 bytes, SHA-256
  `8939b9b780063331aa2c81b4e04437e55d46ba22e62648d13b8c6fb49c4e58df`.
- Harness delta event 48521 contains the same final character. No subsequent
  text delta exists for this run.
- Both native message metadata and model-request-end event 490 record
  `finish_reason=stop`. The native model span has `status=ok`.
- Response ID: `cmb-4dd5a704c45e11f18b783a3e6080e291`.
- Usage: 23574 input tokens, 2431 completion tokens, including 1921 reasoning
  tokens. The reported non-reasoning difference is 510 tokens.
- The durable model reservation granted an output cap of 4096. The task-wide
  token/model/tool count limits were disabled, and the run timeout was 7200
  seconds. The root ledger has no blocked reason or held tokens.
- Both harness run and execution finished as `completed` / `end_turn`.

This rules out desktop-only clipping, lost trailing ACP deltas, and a host
budget/cancellation/timeout termination for this incident. There were 50
model calls across the task tree, but this is not evidence of a 50-call cap:
no such cap was active and there is no limit termination event.

The original raw HTTP/SSE response was not retained. The pinned OpenAI
adapter copies the provider finish reason directly into message metadata;
the current harness does not synthesize `stop` there. The evidence therefore
points upstream of the desktop/harness event bridge, but cannot distinguish
the model generating an early stop from gateway-side truncation or finish
reason rewriting. The synthetic probes did not reproduce this exact early
`stop` defect.

## Why the original UI considered it finished

- `go-harness/internal/engine/eino/engine.go` initializes the result to
  `end_turn`; `consumeMessage` changes it for `length` or `content_filter`.
- `go-harness/internal/engine/eino/turn_loop.go` stops the accepted turn
  after the native agent iterator completes.
- `desktop-app/crates/waku-core/src/driver/acp.rs` treats `EndTurn` as a
  successful completion without an error summary.

The client follows the recorded `stop` correctly at the protocol level. It
does not check whether the text fulfilled a promised list or stopped inside
a word. Automatically retrying every punctuation-free answer would not be
safe: valid headings, code, and lists often have no terminal punctuation.

## Separately confirmed output-limit incompatibility

In the incident build, with reasoning enabled, `role_model_options.go` removed the common
`MaxTokens` option for OpenAI-compatible routes and substitutes
`openai.WithMaxCompletionTokens`. This applies to the Tencent route too.

Live probes on 2026-10-10 used the same configured endpoint/model and
`reasoning_effort=max`, without conversation data or tool definitions.
The test requested numbered English lines plus an explicit final marker.
All six streams returned HTTP 200 and a `[DONE]` marker.

| Requested lines | Limit field | Value | Actual completion tokens | Finish | Full marker |
| --- | --- | ---: | ---: | --- | --- |
| 120 | `max_completion_tokens` | 4096 | 2021 | `stop` | yes |
| 120 | `max_tokens` | 4096 | 2303 | `stop` | yes |
| 600 | `max_completion_tokens` | 256 | 8409 | `stop` | yes |
| 600 | `max_tokens` | 256 | 256 | `length` | no |
| 600 | `max_completion_tokens` | 4096 | 9805 | `stop` | yes |
| 600 | `max_tokens` | 4096 | 4096 | `length` | no |

On this tested route, `max_completion_tokens` does not enforce the requested
cap, while `max_tokens` does. The 256-token `max_tokens` request spent its
entire cap on reasoning and emitted no visible text. The 4096-token request
stopped partway through numbered line 272 and correctly reported `length`.

This is a real compatibility defect, not proof of the cause of the original
2431-token `stop`. Increasing only the existing configured cap cannot be
assumed to fix it, since that cap currently goes into the ignored field for
this route. Neither should the field be globally switched back for every
OpenAI-compatible provider; output-limit semantics need a per-route policy.

## The failed event after the answer

The model call beginning at 11:54:41.603 had an output cap of 384, ran for
252 ms, and failed after the answer's native model span had already ended.
`memory_extraction_audit` identifies this as `model_error` in post-run memory
extraction. `memory_extraction.go` records that optional failure without
replacing the foreground answer. It is not the event that cut the text off.

## Follow-up recommendations

1. Make the output-limit wire field an explicit route capability. Use
   `max_tokens` for this confirmed Tencent route while retaining
   `max_completion_tokens` for routes that require it. Add request-capture
   tests for both policies and preserve reservation caps.
2. Add sanitized terminal diagnostics at the provider boundary: response ID,
   route/model, requested cap and field, actual usage, finish reason, and
   terminal-marker/EOF status. Do not record credentials or hidden reasoning
   text. Missing finish metadata must be distinguished from a real `stop`.
3. Handle `length` as an explicit incomplete response with an actionable
   continuation path. For suspicious `stop` responses, prefer an explicit
   continuation action or an opt-in, bounded, text-only recovery policy; do
   not silently rerun tools or infer failure from punctuation alone.
4. If early `stop` happens again, use the response ID and raw sanitized
   terminal frames to investigate the Tencent gateway/model boundary.

## Validation and local evidence

The following existing offline tests passed (including their subtests):

```powershell
go test ./internal/engine/eino -run '^(TestRoleProviderOptionsReachWireWithinBudget|TestSessionReasoningModelWire|TestContextUsageUsesConfiguredWindowAndLastLeadCall|TestNativeRunnerStreamsAndReconstructsSession)$' -count=1
```

These tests verify current local behavior, not Tencent's compatibility with
the field that the fixtures expect.

Sanitized incident metadata, synthetic output, probe metrics, and the
600-line probes' terminal SSE frames are in the ignored local directory
`.tmp/output-truncation-20261010/`. `incident-evidence.json` contains neither
credentials nor the full conversation or reasoning text. No raw original
incident stream is available there.

## Implemented recovery and parameter policy

The source now uses one provider-neutral `BudgetLimits.MaxOutputTokens` cap,
configured with `local_acp.max_output_tokens` or `--max-output-tokens`. Each
model route's `output_token_parameter` controls only serialization. The `auto`
policy selects legacy `max_tokens` for the exact Tencent endpoint hostname and
Claude, and modern `max_completion_tokens` for other OpenAI-compatible chat routes.
Reasoning effort no longer selects the cap field. Explicit overrides, desktop
round trips, alternate/role/maintenance routes and granted limits have offline
request-capture coverage. This change does not add a Responses adapter.

The previous behavior ended a turn on provider output exhaustion. It now uses
Eino's native `ModelRetryConfig.ShouldRetry` to continue a reported `length`
or Claude `max_tokens`, retaining safe text and checking guarded compaction
before the next request. Context capacity, per-request output allowance and
aggregate cost quotas remain distinct. Compaction cannot refund tokens already
spent, and does not remove an explicitly configured shared spending limit.

Every continuation passes through the tracked reservation and lifecycle stack.
Stream EOF waits for usage settlement before another admission. Truncated tool
groups are rejected rather than executed or spliced into executable arguments.
Continued answers are consolidated for parent/background results and persisted
history without duplicate UI output. Four extra attempts are allowed, with
earlier stops on repeated empty/repeated output, cancellation or errors; final
incomplete text remains available to an explicit follow-up. Truncated summaries
cannot replace intact history. Root, configured role and prepared background
paths have regression coverage.

Normal provider `stop` responses are deliberately unchanged. Consequently this
fix addresses the proven wire-field incompatibility and genuine output-limit
termination, but is not evidence that the original 2431-token early `stop` has
been diagnosed or reproduced. The running portable application has not been
replaced or restarted by this source change.

## Source validation after the fix

- Full engine package: 509 tests/subtests passed.
- Full desktop configuration package: 199 passed (rerun alone with a longer
  timeout after a resource-contended five-minute package timeout).
- Full ACP agent package: 159 passed, including the explicit output-limit notice.
- Continuation regression group repeated ten times: 250 passed. This caught and
  fixed an admission race where cancellation preceded the native monitor.
- Selected durable-runtime / cancellation / role-context tests: 32 passed.
- Selected public SDK background / budget / context tests: 37 passed.
- Harness, launch and configuration-schema packages passed in the wider run.
- ACP, ACP daemon and config CLI commands built successfully.
- Desktop release `cargo check` passed; 10 production settings-contract tests
  passed through the lightweight Rust harness. No visual test was requested.

The initial `go test ./... -count=1` did not finish cleanly: the public SDK and
runtime packages exceeded the default ten-minute timeout while running skill
learning tests. This is not a claim of a fully green whole-repository suite.
No production endpoint or running conversation was used for the fix's tests.
