# Durable background infrastructure

`Service` is a client-lifetime assembly of Eino `backgroundtask.Manager`, its
SQLite providers, the native durable subagent executor, and bounded workers.
The manager owns native lifecycle/CAS/heartbeat semantics. The harness owns
business authority, budget identity, resource ownership, and durable parent
delivery. This package installs no model-visible tools.

## Host assembly

Construct one `Service` with the shared `sqlite.Store`, a current-session
`Authorizer`, a durable `BudgetLedger`, an `AttemptFactory`, and stable versioned
`AgentNames`. Call `StartWorkers` once. The worker pool discovers persisted
pending tasks after restart and never automatically releases suspended tasks.

The registry registers a stable proxy for each agent name exactly once. Every
attempt calls `AttemptFactory.Open` to rebuild its agent, sandbox, providers and
tool middleware from the immutable `Binding`. The returned agent's name must
match `AgentVersion`. No foreground run closures are captured by the registry.
`Open` must return cleanup ownership for any resources it allocated, including
when it also returns an error. `JoinAndClose` must wait for actual I/O and process
cleanup; returning an error quarantines the task and child session.

`SubmitNativeSubagent` uses Eino's public submission helper, leaving native
payload and checkpoint bytes opaque. `Submit` supports explicitly configured
executors. Origin identity is `(parent session, origin run, origin tool call)`;
replaying the same intent returns the existing task, while changed input or
policy is rejected. The host must derive origin, workspace, contract, version
and root budget fields from its authenticated run, never model arguments.

New child sessions use a deterministic service-generated ID. Continuation of an
existing child requires the same parent, workspace, execution contract and
agent version. Child sessions cannot equal the parent session. Query, wait,
cancel, approval, release and inbox APIs authorize the current owner/session
attachment and scope task IDs to that parent.

## Transaction boundaries

| Boundary | Atomic contents |
| --- | --- |
| Create | Native task, immutable business binding, root-budget member binding, native created outbox record |
| Claim | Native pending-to-running CAS and attempt increment, child-session lease acquisition |
| Native running update/heartbeat | Native version/lease change and root-budget active clock/lease renewal |
| Child append | Native attempt/lease check, child lease check, session event append |
| Safe pause/complete | Joined attempt, budget settlement gate, staged runner checkpoint changes, native task metadata/state, native outbox, child lease release |
| Approval resume | Broker's durable one-time approval consumption, versioned native waiting-to-pending transition |
| Notification intake | Parent inbox entry committed first; native outbox receipt acknowledged afterward |

`TaskStore.WithHooks` produces an immutable decorated provider view; it does not
modify callbacks on the shared base provider. Hooks use only the supplied SQL
transaction. SQLite uses one connection, so calling the database pool from
inside a hook would deadlock.

`Config.OnTransitionTx` lets the host commit child business projections and
permission manifests with the native transition. For an active final transition
it sees promoted runner checkpoints and settled budget, while the child lease
still exists and all execution resources have joined. It also observes idle
cancel/resume and pending-to-running claim transitions; running heartbeats do
not invoke it. It must use the provided transaction and must not invoke
`CheckEffectTx` after execution has joined. An error rolls back native state,
outbox, checkpoint, budget, and business changes together.

`BudgetLedger.BindTaskTx` must resolve the root from the durable origin run and
compare the requested root, then bind the task in the same transaction.
`BeforeAttempt` registers the attempt with the durable ledger. The injected
model/tool middleware must reserve and settle every external operation against
that shared root. `Service.CheckEffectTx` allows the ledger to check the task
attempt and child lease in the reservation/dispatch transaction.
`CommitAttemptTx` must reject unresolved or uncertain effects before publishing
a safe checkpoint or normal terminal outcome.

`NewLedgerAdapter(store, ledger)` implements these gates using `internal/budget`.
Both instances must share the same SQLite database. It verifies that the durable
origin member belongs to both the supplied root and the authenticated parent
session; the task budget member belongs to the child session. `BudgetScope`
produces the stable task/attempt identity for engine middleware. Route background
reservations through `Service.CheckBudgetEffectTx` in the ledger's transaction
callback; foreground reservations need their own host fence.

Configure native `HeartbeatInterval` no greater than the adapter's
`HeartbeatInterval()` (the default background interval of 5 seconds fits the
default 30-second budget lease). The optional `HeartbeatBudgetLedger` hook
renews the budget in native running-update transactions even when quota has been
exhausted, retaining ownership through real cleanup. The optional `BudgetMonitor`
cancels a private factory/provider context when limits or accounting errors are
detected; it leaves native heartbeat ownership intact. A monitor error overrides
an inner executor that incorrectly reports success after cancellation.

Runner checkpoint Set/Delete calls are staged in the attempt. The previous raw
checkpoint is retained until cleanup has joined and the native task transition
commits. Failed/canceled attempts preserve previous bytes. A native safe pause
requires actual runner checkpoint bytes as well as Eino's task metadata.

The native SessionStore factory returns a fresh fenced wrapper per call.
Eino's process-local admission lock alone is insufficient because its identity
includes the wrapper pointer. The durable child-session lease is therefore
mandatory. Both stale attempts and writes after cleanup are rejected.

## Delivery, cancellation and shutdown

Workers and the pending queue are bounded. Dispatch stops filling the queue
when full so long-running tasks cannot starve outbox intake. Inbox delivery is
idempotent by notification ID; an Ack failure safely replays after its lease.
The inbox API does not concurrently append messages to an active parent Runner.
Before acknowledging a model-input notification, the host must durably admit
an idempotent parent input keyed by that notification ID.

`Cancel` records intent; completion of cancellation is observed through task
state after the executor and its injected cleanup return. `DrainAndClose`
requires a deadline, closes submissions, stops dispatch, requests native drain,
and waits for worker cleanup. It never closes the shared SQLite store. After a
deadline error, keep storage/resources alive and call it again to wait. A
cleanup error returns `ErrBackgroundUncertain` and persists a quarantine that
also prevents another task from reusing that child after restart.

Eino alpha.35 stops native heartbeats after explicit user cancellation. Cleanup
must therefore join within the remaining native and budget leases to publish a
normal canceled result. Expiry follows the native recovery policy; retry tasks
remain pending with their cancel intent, and live local cleanup prevents child
reentry. A lost budget lease or uncertain cleanup cannot publish a safe normal
outcome. Quota cancellation uses a private context, so it does not stop the
native heartbeat while cleanup joins.

## Explicit remaining integration

The host must wire the supplied durable budget adapter, full model/tool attempt
factory, durable HITL broker, SDK and ACP ownership checks, and parent input
admission. None is replaced with an in-memory fallback here. Native raw
`task_output`/`task_stop` tools must not be exposed because they lack business
owner checks. A `background_agent` tool should be advertised only after the
real attempt factory and all these gates are connected.

This package does not make local shell processes recoverable. A future command
executor must fail after lease expiry unless its backend can recover the exact
logical operation by task identity. If persistence fails while recording
cleanup uncertainty, the error is surfaced and the durable budget's unresolved
reservation policy must prevent an unsafe restart.

## Verification

`go test -mod=readonly -p=2 ./internal/background ./internal/storage/sqlite`
covers native store conformance, atomic creation and origin replay, owner and
child authority, stale attempt fences, checkpoint fault rollback, outbox
commit-before-Ack replay, bounded workers, restart discovery, actual cleanup
join and quarantine, and native subagent interrupt/resume after reconstruction.
The native smoke test uses a fixture resumable agent, not a live model service.
