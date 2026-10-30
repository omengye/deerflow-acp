# Read-only tool execution receipts

`initialize._meta.deerflow.toolReceipts` advertises version 1, `listMethod`, and
optional pagination over the existing ACP JSON-RPC connection. The former
`_deerflow/tool_receipts/reconcile` RPC and `reconcileMethod` capability are
retired. Calling that RPC returns MethodNotFound. There is no manual receipt
review form or review requirement before ordinary conversation can continue.

## Query

```json
{"jsonrpc":"2.0","id":10,"method":"_deerflow/tool_receipts/list","params":{"sessionId":"SESSION"}}
```

The result is `{ "receipts": [...] }`. Records include run and call identities,
state, version, argument digest, bounded output/error evidence, and any historical
review already stored. Historical evidence is retained without reclassifying an
unknown outcome as success or no effect. Tool updates carry the same identity,
state and version in `update._meta.deerflow.receipt`.

Hosts advertising `pagination: "live_keyset"` accept `limit` (1–20), optional
`cursor`, and `uncertainOnly` (default false). Responses include `nextCursor`,
`hasMore`, and `consistency: "live_keyset"`. Cursors bind the session and filter.
Changing either requires a new first page. Desktop execution records are read
only, with bounded pages, error details and complete-record copying.

The owning connection must attach the original session, which must be idle.
After reconnecting, use `session/resume` or `session/load` with its original
workspace. Neither operation re-executes interrupted tools. The list method
requires a request; notifications have no effect. Parameters reject unknown or
case-aliased fields, duplicate keys, nulls and wrong types.

## Agent recovery

Ordinary tool failures, including local timeouts, lost transport and unconfirmed
effects, are returned to the agent with available output and explicit outcome
uncertainty. The agent chooses whether to investigate, correct inputs, retry,
change approach or clearly end the turn. Tool-failure recovery has no fixed retry
count; existing user budgets, permissions and cancellation still apply.

`agentRecoverable: true` means the host admitted a terminal tool failure to this
recovery path. It does not certify external effects or guarantee the model has
already consumed the result. The receipt may remain `uncertain`. A started or
pending call cannot become safe merely by carrying this field. Storage failures,
unsettled budget accounting, user cancellation and approval interrupts remain
host control flow rather than ordinary failure observations.

When information or confirmation is needed, the assistant asks in ordinary chat
and ends the turn. A user reply starts a new run in the same conversation with
its history. The host never automatically replays old calls or unsafe
checkpoints. An agent may initiate a new attempt after assessing evidence;
this is not an exactly-once guarantee for external effects.

A terminal failed or cancelled run does not permanently lock new conversation.
Legacy unrecoverable executions are closed only through explicit host recovery,
not through a blanket history migration. `CloseFaultedExecution` requires the
selected session, run and expected execution version, rejects active attempts or
unsettled budget work, ends the old run and invalidates its checkpoint. Unknown
receipts and original error evidence stay intact.

## Media and compatibility

Receipt records are not standard ACP content blocks. Results from `view_image`
or `present_files` may contain stable `deerflow-asset:` references and internal
asset metadata for audit, without inline image bytes. These are not prompt
capabilities. Normal session updates and artifact listing use the media
projection described in [media.md](media.md).

Legacy `-32010`/`reconciliation_required` and `-32011`/receipt-conflict codes are
recognized for compatibility. Their public messages preserve execution evidence
and explain that old tools were not replayed; they do not request human outcome
certification. Busy and unattached sessions retain normal ownership errors.
The Go SDK's historical reconciliation method remains available for compatibility
with existing integrations; the desktop does not expose or require it.
