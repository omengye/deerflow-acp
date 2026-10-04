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
Optional `ExecutionFailure` reports late errors from work that has already
joined. These produce a failed task without retaining shared resources as
uncertain cleanup. Such diagnostics persist with final task transitions, even
when native explicit cancellation determines the terminal `canceled` status.

`ObserveControl` sees actual manager-owned controls before forwarding them to
the native executor. The host uses this attempt-local signal to let native
drain/cancel graph interrupts reach Eino's outer Runner; unknown business
interrupts without a control remain rejected. This observer provides no tool
permission. The forwarding goroutine is stopped and joined before cleanup.

`SubmitNativeSubagent` uses Eino's public submission helper, leaving native
payload and checkpoint bytes opaque. `Submit` supports explicitly configured
executors. Origin identity is `(parent session, origin run, origin tool call)`;
replaying the same intent returns the existing task, while changed input or
policy is rejected. The host must derive origin, workspace, contract, version
and root budget fields from its authenticated run, never model arguments.

New child sessions use a deterministic service-generated ID. Continuation of an
existing child requires the same parent, workspace, execution contract, agent
version and task ancestry. Child sessions cannot equal the parent session.

## Task tree authority

Bindings optionally record `RootSessionID`, `ParentTaskID`, `ParentInvocationID`
and `Depth`; legacy
root bindings omit them to preserve their original serialized hashes. The
transactional `harness_background_tree` index backfills old tasks as depth 1.
The current root attachment can query and manage the complete tree.

`WorkerActor(ctx)` derives a private actor from a live managed attempt. A copied
actor label grants no authority without that context, native lease, child lease
and active local attempt. Workers can manage only their descendants. They
cannot read/cancel ancestors or siblings, approve effects, borrow the root's
attachment, or obtain authority by attaching to a private child session.
Submission derives ancestry from that attempt, checks the shared workspace and
root budget, and repeats the lease/stop checks inside the create transaction.
The infrastructure bounds durable ancestry to three levels. For v5 mixed
delegation, `Depth` counts both foreground and background roles, `ParentTaskID`
is the nearest background ancestor, and `ParentInvocationID` is the immediate
role parent. A root background task created by a foreground role can therefore
have depth 2 or 3. The host's `RolePlacement` callback checks the live role lease
both before submission and within the task-create transaction. Role counts and
the native binding commit together; expired, superseded or finished parents
cannot create new tasks.

Notifications remain owned by the immediate parent invocation. Root tree
visibility does not route grandchild results into root model input. Reusing a
child session does not transfer old invocation notifications to the new worker.

## Transaction boundaries

| Boundary | Atomic contents |
| --- | --- |
| Create | Native task, immutable business binding, validated tree edge, root-budget member binding, native created outbox record |
| Subtree stop | Durable stop intent for every descendant, fenced against concurrent child creation and new effects |
| Claim | Native pending-to-running CAS and attempt increment, child-session lease acquisition |
| Native running update/heartbeat | Native version/lease change and root-budget active clock/lease renewal |
| Child append | Native attempt/lease check, child lease check, session event append |
| Safe pause/complete | Joined attempt, budget settlement gate, staged runner checkpoint changes, native task metadata/state, native outbox, child lease release |
| Approval resume | Broker's durable one-time approval consumption, versioned native waiting-to-pending transition |
| Descendant wait resume | Saved wait targets, descendant readiness and execution frontier validated with native waiting-to-pending CAS |
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

`Cancel` first records durable intent for the complete subtree, including
descendants of an already completed parent. It then requests native cancellation
for each nonterminal task; worker scans retry that delivery after failure or
restart. An undelivered stop already fences new child creation, effects and
permission/suspension resume. It does not release resources or erase uncertain
effect evidence. Completion is observed through task state after every executor
and its injected cleanup return. `DrainAndClose`
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

## Host integration and remaining scope

The root SDK now wires the durable budget adapter, rebuilt model/tool attempt
factory, durable HITL broker, SDK and ACP ownership checks. Explicit parent
notification input admission and opt-in automatic root continuation scheduling
are available, including private parents in foreground and native background
hosts. New delegated submissions use the v5 host role factory, which supports
foreground/background combinations using their current invocation identity,
archived role catalog, configured depth and inherited policy. V1–v4 execution
paths remain registered. Model-visible capabilities are recreated for each
role and check durable role ancestry before querying or cancelling a task;
neither a root attachment nor an enclosing worker grants access to a sibling.
Native raw
`task_output`/`task_stop` tools must not be exposed because they lack business
owner checks. The host exposes `background_agent` with these gates, plus bounded
status/wait/cancel tools. See [public task management](../acp/background.md).
`ReleaseSuspensionVersion` checks the owner's observed version inside the native
transition transaction; process startup leaves saved suspensions untouched.

Roles executing inside a background attempt receive a durable `task_wait`: it saves a native
interrupt and releases its worker and active role slot after cleanup joins.
`Config.Waits` checks saved descendant targets and resumes only when all are
terminal, unblocked and free of started/uncertain tool receipts. This resumes the
saved native execution. Terminal notification dispatch separately creates new
parent executions from durable inbox entries.
Root model tools and roles executing inside a root foreground run also support
durable waits. Their joined checkpoint is published through the foreground
execution manifest, and `ResumeExecution` continues the same run/input and
root budget when its dependencies are terminal and unblocked. A mixed
permission/wait checkpoint can resume the permission batch first. Old foreground
checkpoints retain their original bounded tool contract. ACP `/tasks/wait`
remains a bounded query. Attached ACP sessions and explicit SDK
`ServeSessionUpdates` subscriptions automatically resume ready foreground waits
after the prompt returns. They use current-owner callbacks and preserve the
original input, role catalog and budget. With the saved `auto_continue` policy,
terminal inbox notifications for the root can create a unique new run under
the same budget and persistent root continuation allowance. Results committed
through a managed foreground or background task tool are not automatically
delivered again to the same original parent role.

The background broker saves observations in `harness_background_delivery_stages`
under the exact task attempt and tool call. These become `harness_task_deliveries`
only in the native completed/checkpoint transaction; failed attempts discard
uncommitted observations. The same transaction updates existing inbox delivery
state, while late outbox arrivals consult saved deliveries. Neither direction
changes UI-read acknowledgement. Session deletion removes attempt stages along
with the rest of the task tree. V5 private parents owned by the root foreground
session can now resume from their committed role-local conversations; only final
summaries are propagated through `role_result` notifications to immediate
ancestors. All hops use the same root budget and automatic allowance.

Native private-parent dispatch uses `deerflow-continuation-v6`. `SubmitContinuation`
requires current root ownership and a transactional host source validator.
Creation preserves the logical invocation and role quota while allocating a new
task, child session and native lease/checkpoint/approval lifecycle. Source and
automatic quota are committed with the task's budget binding. A continuation
can manage the original logical role's descendants; it cannot gain sibling
authority. Subsequent notifications select the latest committed role context.

`harness_background_continuation_links` connects each new physical host to its
previous host. Subtree stops follow both child edges and these links. Root
automatic stops durably fence already-admitted automatic continuations; the
native cancellation scanner joins effects before terminal settlement. Runtime
source reads validate the link against the saved binding, and graph cleanup
performs the same immutable identity check without requiring execution permission.

The runtime broker binds native targets to the task, attempt, root budget,
checkpoint, session/event/receipt frontier and immutable execution recipe.
Commit rechecks those values inside the native resume transaction; claim and
attempt open validate the released manifest again. Public clients cannot supply
wakeup targets. Tree stop fences apply even to wakeups selected before cancel.
One invalid manifest is reported without discarding other valid wakeups.
Mixed permission/wait checkpoints resume approved permission targets first;
unfinished waits suspend again. No wakeup grants permission to an external tool.

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
