# Manual stop and fresh conversation turns

## Confirmed cause

Read-only inspection of the running portable application's local state found
that a manually stopped run was correctly persisted as `cancelled`, with no
held tokens remaining. Its last started command retained an `uncertain` tool
receipt (`context canceled`). The foreground admission gate rejected every
subsequent prompt in that session until receipt reconciliation. Stopping a
command cannot prove that it made no partial changes, but that uncertainty
should not make a fresh user question impossible.

An isolated native-engine regression reproduced a second issue after removing
that admission barrier: native history retained an assistant tool call without
its terminal tool message. A later model request could therefore fail provider
validation even though the new turn had been admitted.

The packaged Waku/ACP smoke also exposed a wire-order race during a permission
wait. Desktop sends `session/cancel` and then answers its pending permission
requests. ACP routes responses immediately but dispatches notifications on a
separate queue, so the permission reply could end the prompt before the stop
notification committed cancellation. An immediate follow-up then saw the old
waiting execution. Permission replies now join already-received notifications
and recheck cancellation before preserving a wait or accepting a grant. The
notification barrier broadcasts completion to every concurrent waiter.

## Fix boundaries

- A receipt from a durably cancelled run remains uncertain and reviewable, but
  does not block a fresh foreground run. Other unresolved failures and process
  interruptions still require their existing reconciliation path.
- Checkpoint validation for a new run excludes uncertainty from older cancelled
  runs. The current run's uncertain/started effects still block resumption.
  Cancelling a durable execution removes its checkpoint and stops automatic
  continuations for that root; no old operation is silently replayed.
- The host supplies fresh turns with a warning that stopped tools may have
  partially executed. The engine closes only missing historical tool results
  before the latest user input, using an explicit interrupted/unconfirmed
  message. It does not fabricate success, alter business receipts, or close
  tools belonging to the current request. This repair runs before compaction.
- The desktop retains a short cancellation-drain state on DeerFlow runtimes.
  A new submission waits for the old prompt to settle instead of racing cleanup
  or accepting the old completion as the new turn's result. Stopping alone
  does not automatically run an already queued message.

The existing portable application, configuration, and database were not changed
by diagnosis or regression tests. Existing cancelled runs use the new admission
rule without a data migration.

## Regression coverage

- Legacy and durable native-engine runs: stop a started tool, ask a new question,
  then execute a fresh approved tool without replaying the cancelled effect.
- Validate that provider history contains complete tool-call/result groups,
  the old uncertain receipt/version remains intact, and the cancelled execution
  cannot be resumed.
- Cancel an active model stream, settle its budget, and start a new turn with
  its own budget root.
- ACP: cancel, reject admission while cleanup remains active, then accept
  multiple prompts on the same connection after cleanup.
- Cancel a saved permission wait and ask a new question without executing the
  cancelled pending tool.
- Enforce stop-before-permission-reply ordering, including a late approval;
  retain resumable waits when only the permission prompt is dismissed. Verify
  concurrent notification waiters all settle without a later notification.
- Preserve reconciliation for failed/interrupted work and mixed unresolved
  receipts; repair historical messages idempotently without changing live work.
- Desktop cancellation state has standalone Rust tests. The packaged protocol
  smoke includes stream-stop and approval-stop followed by fresh prompts on the
  same Waku/ACP runtime, using only isolated local fixtures.

These are nonvisual tests. GUI interaction and real-provider behavior are not
certified by this document.
