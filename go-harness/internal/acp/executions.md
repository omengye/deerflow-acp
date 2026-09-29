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

## Recovery errors

| Code | Meaning |
| --- | --- |
| `-32012` | A saved execution is waiting for input |
| `-32013` | Execution version or identity conflicts with the request |
| `-32014` | The saved checkpoint cannot safely continue with current evidence/resources |

Error data includes the discovery/resume/cancel method names. Unknown internal
errors use the transport's generic error response; raw provider or database
errors are not serialized. Ordinary ownership, busy, and receipt error codes
remain applicable.

Checkpoints pin accepted input, session configuration, resource versions,
native history, business event cursor, tool receipts, and budget identity. A
nonempty client MCP connection has a process-local generation; reconnecting it
invalidates execution resource matching. Task-owned MCP rebinding is future work.

Verification uses real Eino tools, a local model fixture, SQLite close/reopen,
and bidirectional ACP pipes. These tests do not substitute for real editor or
ACP TCK interoperability tests.
