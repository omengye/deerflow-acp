# Durable execution over ACP

The native Eino engine advertises `_meta.deerflow.executions` during initialize.
Custom engines without durable continuation do not advertise this extension.
All operations require attachment to the parent session on the current connection.

| Method | Parameters | Result |
| --- | --- | --- |
| `_deerflow/executions/get` | `sessionId`, optional `runId` | `ExecutionState`; omitting the run selects the latest execution |
| `_deerflow/executions/resume` | `sessionId`, `runId`, `expectedVersion` | Standard prompt response, including `_meta.deerflow.execution` |
| `_deerflow/executions/cancel` | `sessionId`, `runId`, `expectedVersion` | Final `ExecutionState` for an idle waiting execution |

The extension rejects replacement prompts, checkpoint keys, native targets,
unknown fields, duplicate keys, and stale versions. Resume reserves the session
on the transport reader before dispatch, just like `session/prompt`.

## Approval and reconnection

During a prompt, a protected tool first persists its intent and a real Eino
checkpoint. After all execution resources join, the run becomes `waiting_input`
and its budget attempt ends. The agent then sends `session/request_permission`.
Approval or denial resumes that same run in a new attempt. All parallel pending
interactions must receive an answer before any of them can execute.

A cancelled permission response leaves the run waiting. Closing the transport
also preserves a committed wait. Reconnect with `session/resume` or `session/load`,
query the execution, then explicitly request execution resume. A fresh permission
request is sent to the new client; an old JSON-RPC ID is not reusable authority.
`session/resume` itself never executes a saved tool call.

`session/cancel` stops an active prompt or resume, including one currently
waiting on a permission response. Use the execution cancel extension for an
idle saved wait. SDK context cancellation has the same explicit-cancel meaning.
Tool effects whose outcome is unknown still require receipt reconciliation.

New prompts and session configuration changes are blocked by a waiting run.
Permission response time does not consume the active execution time budget.
Each actual tool call is charged once across its pause and continuation.

## Questions in the desktop conversation

A desktop client advertises
`clientCapabilities._meta.deerflow.userInput` as
`{"version":1,"requestMethod":"_deerflow/ask_user_question"}`. Only an attached
connection advertising this capability supplies the native `ask_user_question`
tool. The v1 bridge forwards this bidirectional extension; clients without the
capability, including the v2 bridge facade, retain ordinary text questions.

Model arguments contain `questions` (1–4), each with `question`, optional
`header`, optional `options` (`label` and optional `description`, at most 8), and
optional `multiSelect`. Omitting options permits free text. The host assigns
question IDs and sends `sessionId`, `runId`, `toolCallId`, and the normalized
questions in `_deerflow/ask_user_question`. The desktop returns either
`{"outcome":"cancelled"}` or
`{"outcome":"accepted","answers":[{"questionId":"q1","answers":["value"]}]}`.
Every question must have exactly one answer entry; single-choice questions
accept one value, and multiple-choice questions accept up to eight values.

The existing desktop question panel supports choices and custom text. Each
connection serializes question forms, and responses bind to the original
question IDs. Answers are conversation data and never authorize another tool
or alter approval policy. Only the trusted native question tool skips a separate
permission prompt. Tool-policy filtering and tool-call budgets still apply.

Questions are live root-conversation interactions: delegated agents return
missing information to the root agent, which owns the question form. Questions
use the active turn's time budget and do not create an approval checkpoint that
survives reconnection.
Stop, disconnect, or prompt completion closes the pending form; a new message
can continue the conversation without replaying the old question. Questions
outside an active desktop prompt are cancelled, so automatic-only continuation
can fall back to ordinary text. Completed answers remain in normal tool history.
Other unavailable tool names still return corrective feedback to the model.

## Recovery errors

| Code | Meaning |
| --- | --- |
| `-32012` | A saved execution is waiting for input |
| `-32013` | Execution version or identity conflicts with the request |
| `-32014` | The saved checkpoint cannot safely continue with current evidence/resources |
| `-32028` | The execution checkpoint exceeds its raw or stored byte budget |
| `-32029` | The saved checkpoint frame or native envelope is invalid |
| `-32030` | The execution checkpoint could not be committed |

Error data includes the discovery/resume/cancel method names. Unknown internal
errors use the transport's generic error response; raw provider or database
errors are not serialized. Ordinary ownership, busy, and receipt error codes
remain applicable.

New harness turn checkpoints use a versioned `DFCP` binary frame. Small JSON
metadata and opaque Eino native bytes are stored separately, without base64.
Native state of at least 64 KiB uses Gzip at the fastest level when it reduces
size; smaller or incompressible state stays raw. Both the stored frame and its
uncompressed header/metadata/native total are limited to 64 MiB. Compression
cannot bypass the decoded-state budget. The frame binds its version, codec,
declared lengths, metadata and stored payload with SHA-256. Decoding rejects
invalid lengths, unknown codecs, checksum mismatches, truncated streams, extra
streams/trailing bytes and output beyond the declared decoded length.

Legacy v1/v2 JSON envelopes remain readable with their original local/durable
budget semantics. New writes keep those envelope metadata versions inside the
frame. A previous executable cannot read new binary frames; finish or cancel
waiting executions before rolling back the application. This codec covers
`harness/turn/v1/` foreground and automatic-continuation checkpoints; native
background executor checkpoints keep their existing format.

The host checks frame metadata without inflating native state while holding a
SQLite transaction. The manifest hashes the exact stored frame and still binds
native history, permissions, task waits, resources and receipt frontiers. The
`waiting_input` execution audit includes raw/stored/native/metadata byte counts
and codec. Size failures retain the byte count and limit in local diagnostics
and safe error metadata. Public errors contain a short explanation; desktop
transcripts do not display the attached execution JSON or private diagnostics.

Eino remains on the alpha.35 API via a source-pinned local replacement. The
replacement ports upstream #1232's single canonical nested checkpoint payload
and reference reconstruction; see `third_party/eino/PATCH.md`. It is independent
of compression and preserves old inline native checkpoints. Checkpoint writes,
manifest publication and budget settlement retain their existing transaction
boundary. A codec failure does not publish a partial checkpoint or replay tools.

Malformed JSON from the model response is reported as `-32026` with
`kind: model_response_invalid` and a safe explanation. The original cause
remains available in local diagnostics. This error does not retry the model or
replay tools: `automaticReplay` remains false, and attached execution metadata
still determines whether the saved run can resume. Partial output is not a
successful result; inspect completed tool results before continuing.

Model HTTP 429 errors are reported as `-32027` with `kind: model_rate_limited`
and a safe Chinese explanation. The native agent retries a model request only
when the provider rejects it before opening a response stream: at most three
additional attempts, waiting 1, 2, and 4 seconds. Each attempt uses the shared
model-call/token budget and settles before another is admitted. Cancellation
interrupts the wait; budget or persistence failures stop further requests.
Provider SDK retries are disabled for these models to avoid unmetered attempts.
Auxiliary models keep their caller-owned retry policies. A stream that has
already opened is never replayed, and the agent run and completed tools are
never restarted by this retry policy. `automaticReplay` remains false, with
execution metadata preserved if the requests still fail.

Checkpoints pin accepted input, session configuration, resource versions,
native history, business event cursor, tool receipts, and budget identity. A
nonempty client MCP connection has a process-local generation; reconnecting it
invalidates execution resource matching. Task-owned MCP rebinding is future work.

Verification uses real Eino tools, a local model fixture, SQLite close/reopen,
and bidirectional ACP pipes. These tests do not substitute for real editor or
ACP TCK interoperability tests.
