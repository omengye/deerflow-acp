# Durable execution storage and permission broker

This package implements storage, permission brokering, and runtime orchestration
for durable human interaction. The service opts into native continuation only
when the engine advertises `DurableExecutions` and the durable ledger is wired.
The SDK and ACP adapt the service's explicit execution APIs separately.
Standard ACP `session/resume` still means attaching a session.

## Identity and authority

One logical execution keeps its `SessionID`, `RunID`, accepted `InputID`, tool
call IDs, and root budget across every attempt. Each attempt receives a new
`AttemptID` and a strictly increasing fence. The attempt's owner is transient
connection identity; the session coordinator remains the authorization boundary.
Store methods do not authenticate callers. Callers must hold the current
coordinator lease for every read or mutation.

Public requests identify only the run and expected execution version. They do
not accept another prompt, checkpoint key, native interrupt address, grant, or
replacement resume parameters. Public pending interactions expose a description
and argument digest. Raw arguments and native target IDs remain broker-owned.

Permission intents are stable across attempts. Each ready grant belongs to one
attempt and one immutable intent version. A fresh connection must answer a new
reverse permission request. Old JSON-RPC request IDs and previous attempt grants
are never a resume credential. Disconnect is distinct from an explicit cancel
or a reject decision.

## Transaction integration

All mutating execution APIs accept the caller's `*sql.Tx`; they never open a
nested transaction. Return immediately and roll back the transaction on any
error. SQLite uses the runtime's immediate transaction configuration.

1. **Accept an input.** In one transaction, create the run/input, create the
   ledger root, call ledger `BeginAttemptTx`, and call `BeginExecutionTx` with
   the same trusted scope and owner. The original input is serialized once.
2. **Prepare permission.** Persist `tool_start`/the pending receipt, then call
   `RecordPermissionIntentTx`. Exact retries return the stable intent ID.
   Native interruption must not turn that pending receipt into a terminal
   failure or emit a second `tool_start` when resuming.
3. **Publish waiting state.** Wait for all model/tool I/O and cleanup to join.
   In one transaction call `SuspendExecutionTx` with the staged native bytes
   and trusted interrupt bindings, then ledger `EndAttemptTx(WaitingInput)`.
   This atomically promotes the final checkpoint and the execution manifest.
4. **Claim a resume.** Hold coordinator ownership and validate the waiting
   manifest before issuing fresh permission requests. Human permission I/O
   occurs while the previous ledger attempt is ended. Once every answer is
   available, call `ClaimExecutionResumeTx`, ledger `BeginAttemptTx`, and
   `RecordPermissionGrantTx` in the same transaction. Reconstruct the original
   input with `ExecutionRequestTx`. No budget attempt stays active while the
   user is deciding.
5. **Build native targets.** `ExecutionResumeBindingsTx` validates the manifest
   again and requires a fresh ready answer for every pending target. The
   trusted engine adapter builds Eino resume parameters from these bindings.
6. **Dispatch or deny.** `ValidatePermissionGrantTx` is a read-only decision
   lookup for the engine broker. The `tool_execute` transaction must call
   `ConsumePermissionGrantTx`, start the receipt, and persist its event before
   invoking the tool. A rejected grant must be consumed in the same transaction
   as its terminal denial receipt. A failed receipt write rolls back grant and
   intent changes. No standalone consume operation is provided.
7. **Finish an attempt.** After I/O has joined, use `EndExecutionAttemptTx` and
   the ledger's `EndAttemptTx` in one transaction. Completion requires no open
   or uncertain tool receipts. Explicit cancellation settles pending calls as
   not executed, started calls as uncertain, revokes grants, and removes the
   executable checkpoint.

While waiting, reject a new prompt using `requireNoWaitingExecution`; callers
must resume or cancel the existing execution. An idle waiting execution can be
cancelled with `CancelExecutionTx`. Running cancellation first stops and joins
the coordinator's active attempt, then uses the end-attempt path.

## Checkpoint validity and recovery

The manifest stores the checkpoint digest, exact accepted-input digest, pinned
session configuration digest, native session head, receipt frontier, business
event cursor, and every outstanding native interrupt binding. The native head
is the actual `eino_session_events.seq`, event ID, and payload digest. It is
separate from `harness_events.sequence`. Broker bookkeeping goes to private
execution audit rows so collecting a permission answer does not invalidate the
business cursor.

Native session writes can precede checkpoint promotion in separate transactions.
This implementation makes final promotion atomic; it does not claim all native
history writes are atomic with that checkpoint. If a failed resume advances
native history, business events, or receipts, the old checkpoint remains stored
for diagnosis but is not executable. Started or uncertain side effects prevent
automatic replay. A failed attempt may return to waiting only while the original
manifest still validates exactly. All model/tool spending stays in the durable
budget ledger and cannot be refunded by restoring checkpoint bytes.

At startup, call `RecoverExecutionsTx` before legacy run/approval/receipt
settlement. Valid waits are preserved; current grants are revoked. Interrupted
attempts return their scopes for ledger cleanup in the same transaction.
Legacy reconciliation excludes runs with the committed `waiting_input`
business status, including background host projections. Otherwise it would
destroy pending intents. Missing/stale foreground manifests become
`needs_reconciliation`; started effects become uncertain. Native adapter checks
for tool/extension generation compatibility remain necessary in addition to
these storage checks, including the existing MCP reconnect-generation guard.

## Service behavior

`Service.Run` drives native interruption, permission collection, and successive
attempts within the original prompt call. An unanswered/cancelled permission
request leaves the execution waiting. `ResumeExecution` resumes only a precise
run/version; `CancelExecution` cancels an idle wait/reconciliation state.
`Execution` accepts an empty run ID to discover the most recent execution after
reconnection. Every public operation checks current session ownership.

The coordinator distinguishes explicit cancellation from disconnect. ACP marks
its connection-owned request context with `WithTransportCancellation`, because
the peer can cancel request parents before coordinator disconnect executes.
SDK context cancellation is explicit by default. Event-handler errors stop
execution immediately, including before a tool effect, and block subsequent
events in that attempt. Persisting the event does not waive the callback gate.
A failed `tool_start` does not manufacture a new waiting checkpoint. Permission
callback errors occur after waiting checkpoint commit and preserve that valid
checkpoint without creating a resume attempt or grant. A failed resumed attempt
can preserve an earlier checkpoint only if its manifest still validates.

Pending execution blocks new prompts and configuration/mode changes. Background
child sessions are hidden from the ordinary session list and rejected by
foreground load/admission/run APIs; their native task lease controls execution.

## Verification

`executions_test.go` covers stable logical identity, grant dispatch once, exact
input reconstruction, waiting prompt guard, simultaneous resume CAS, old-owner
and forged-identity rejection, config/checkpoint/native/receipt drift, all
pending sibling bindings, disconnect with fresh authorization, startup recovery,
and budget spending after a failed resume. SQL fault injection verifies rollback
of checkpoint promotion, resume claim plus budget attempt, cancellation, grant
consumption plus tool receipt, and zero-row CAS rejection.

`execution_service_test.go` verifies automatic allow and deny, permission I/O
with no active budget attempt, exact whitespace-preserving arguments, callback
effects after grant/receipt commit, receipt-failure dispatch prevention,
disconnect versus SDK cancellation, fresh authorization after owner replacement,
latest-run discovery, event failure before effects, preservation of committed
waits after permission callback failure, and real SQLite close/open recovery.
