# Tool receipt recovery extension

These methods run over the existing ACP JSON-RPC connection. They create no
HTTP endpoint. `initialize._meta.deerflow.toolReceipts` advertises version 1 and
the two method names.

## Query

```json
{"jsonrpc":"2.0","id":10,"method":"_deerflow/tool_receipts/list","params":{"sessionId":"SESSION"}}
```

The result is `{ "receipts": [...] }`. Each receipt includes `runId`,
`toolCallId`, `state`, `version`, `configVersion`, argument digest and a summary
of parameter names/types, bounded result evidence, and any prior human review.
Tool session updates carry the corresponding identity, state and version in
`update._meta.deerflow.receipt`.

Hosts advertising `pagination: "live_keyset"` also accept `limit` (1–20),
optional `cursor`, and `uncertainOnly` (default false). The result includes
`nextCursor`, `hasMore`, and `consistency: "live_keyset"`. A cursor binds the
session and filter; changing either requires restarting from the first page.
Pages replace the desktop's bounded window. `uncertainOnly: true` ensures that
earlier successful calls cannot hide blocking receipts. The original unpaged
request remains available for existing SDK/ACP clients.

A `failed` receipt records a native command that ended with an unsuccessful
result after the host confirmed termination and cleanup, or a normal MCP
`IsError` response with bounded, credential-checked evidence. Its tool card remains
failed, and the model receives the bounded output and error so it can inspect
and correct the command. Partial effects may have occurred; this state asserts
neither success nor rollback and never causes automatic replay. Cancellation,
unconfirmed termination, lost transport, and persistence failures retain their
existing stopping/reconciliation behavior. Old uncertain receipts are not
silently reclassified by upgrading the executable.

Command snapshots carry `notExecuted` only when the native operation failed
before launching a process (including a rejected `cmd.Start`). A missing exit
code alone is insufficient; wait/cleanup failures retain uncertain handling.
Waku's “任务与工具核对” panel queries and reviews receipts on the owning ACP
connection. Review requires a reviewer, actual verification notes and explicit
`completed`/`no_effect` choice, followed by confirmation. It never replays a call.

The desktop shows a direct “确认工具结果” button above the DeerFlow conversation
only when that session is known to need receipt review. It is hidden for normal
sessions and after a fresh first-page query confirms no uncertain receipts remain.
The management panel retains its manual receipt browser. The direct button
opens a dedicated recovery panel and queries uncertain receipts
without sending a prompt. Recovery errors are shown as plain-language guidance
instead of a JSON-RPC dump. The panel explains common failures and what to check;
technical evidence remains available for inspection and copying. A permission
error is not proof of no effect. Users can keep the session paused when the
outcome is unknown or only partial. After a confirmed review the panel refreshes
the first uncertain page; only an empty first page clears its recovery warning.
It never automatically resends the blocked message or changes filesystem permissions.

This private query returns harness receipt records, not standard ACP content
blocks. Results from `view_image` or `present_files` may contain a stable
`deerflow-asset:` URI and internal `asset` metadata (session, digest and size)
for audit. They contain no inline image bytes. Those fields cannot be submitted
as ACP prompt or reconciliation capabilities. Standard session content updates
and the artifact listing use the normal media projection described in
[media.md](media.md).

The connection must own the session, and the session must be idle. After a
reconnect, first use `session/resume` or `session/load` with its original
workspace. Neither re-executes an interrupted tool.

## Explicit review

Inspect the actual external destination or artifact before submitting a review.
An execution error alone cannot establish that the operation had no effects.

```json
{
  "jsonrpc": "2.0",
  "id": 11,
  "method": "_deerflow/tool_receipts/reconcile",
  "params": {
    "sessionId": "SESSION",
    "review": {
      "runId": "RUN",
      "toolCallId": "CALL",
      "expectedVersion": 3,
      "outcome": "completed",
      "reviewer": "operator",
      "note": "Verified the destination contains the intended result.",
      "result": [{"type":"resource_link","uri":"file:///workspace/result.txt"}]
    }
  }
}
```

The result is `{ "receipt": {...} }`. `outcome` must be `completed` or
`no_effect`. Only an `uncertain` receipt can be reviewed. `expectedVersion`
must match the latest queried receipt. The original error and partial result
remain available alongside the review. The operation writes an audit record;
it never invokes the model or tool. Subsequent work requires a separate prompt.

Both methods are requests. Sending either as a notification has no effect.
Parameters reject unknown/case-aliased fields, duplicate keys, nulls and wrong
types. Requests are limited to 128 KiB. Optional result evidence is limited to
128 text/resource-link parts and 64 KiB of field values; inline binary data is
not accepted. Evidence references are recorded without fetching them.
SDK reconciliation applies the same evidence limits and rejects internal asset
references, image data, size/description fields and unsupported content kinds.

## Errors

| Code | Meaning | Next action |
| --- | --- | --- |
| `-32010` | Uncertain effects block a new prompt | Query, inspect the real outcome, then explicitly review |
| `-32011` | Receipt state/version or execution evidence conflicts | Query again; do not replay the operation |
| `-32000` | Session is busy or owned elsewhere | Wait for cleanup or use the owning connection |
| `-32602` | Invalid parameters or session is not attached | Correct the request or attach the session |
| `-32603` | Internal persistence/provider failure | Query before retrying a review to establish its committed state |

Recovery errors include safe machine-readable `data.kind`, `listMethod`,
`reconcileMethod`, and `automaticReplay: false`. Internal diagnostic errors are
not copied into public JSON-RPC error messages.

## Go SDK equivalent

After reopening the harness, call `LoadSession(ctx, sessionID, cwd, false, nil)`
to attach the original session, then:

```go
receipts, err := client.ListToolReceipts(ctx, sessionID)
// Handle err, select the uncertain receipt and inspect the real outcome.
r := receipts[index]
reviewed, err := client.ReconcileToolReceipt(ctx, sessionID, harness.ToolReconciliation{
    RunID: r.RunID, ToolCallID: r.ToolCallID, ExpectedVersion: r.Version,
    Outcome: harness.ReceiptCompleted, Reviewer: "operator",
    Note: "Verified the destination contains the intended result.",
})
// Handle err. A separate client.Run can proceed once all uncertain receipts
// in the session have been reviewed. Review itself never resumes execution.
```

Use `errors.Is(err, harness.ErrReconciliationRequired)` to detect the blocked
run, `harness.ErrReceiptConflict` to detect a stale review, and `harness.ErrBusy`
for an active session. If the external outcome cannot be established, keep the
receipt uncertain and leave that session blocked.
