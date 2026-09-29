# Shared durable budget ledger

## Authority and lifecycle

A logical foreground execution, its synchronous agents, background tasks and
retries consume one immutable host policy. An unrelated prompt creates another
root. The runtime creates a root and begins the first attempt in the transaction
that accepts the run. A delegated task binds to an existing origin member in
the task-creation transaction. Neither model arguments nor checkpoints create
or increase quota. Every production Eino invocation receives a trusted context
scope; configuring a ledger without that scope fails closed.

The engine joins all model/tool I/O before returning. The runtime/task manager
then ends its attempt in the transaction committing the terminal business
state. Failed attempts still consume quota. Native checkpoint bytes may remain
unchanged after a failed resume, while ledger usage continues advancing.

## API and persistence

`internal/budget.New(db, Config)` migrates independent tables. `Config` accepts
a clock, a lease duration and `CheckEffectTx(ctx, tx, Scope)` for additional
background/native-task fencing. Root, member, attempt and reservation records
are durable. All `*Tx` methods exclusively use their supplied transaction, so
they work with the application's single-connection SQLite pool.

Lifecycle methods are `CreateRootTx`, `BindMemberTx`, `BeginAttemptTx`,
`EndAttemptTx` and `ReconcileInterruptedTx`. Public outcomes are completed,
failed, cancelled, waiting_input and interrupted. A scope binds root, member,
session, attempt and monotonic fence. Origin/group mismatches and stale fences
reject new effects. Existing owned reservations may settle after a fence ends.

## Admission and accounting

Each host-generated operation ID and request digest admits at most one logical
operation. A model reservation atomically increments its call count and token
hold before returning the capped output allowance. Tool admission increments
the call count before approval, preserving existing semantics. Dispatch has a
second durable fence check before external I/O. Reusing an operation ID with
different arguments fails. A previously dispatched grant never authorizes
automatic reexecution of an external effect.

Actual final provider usage replaces an estimate after clean completion. Clean
completion without final usage uses the observed estimate. Failure/cancellation
without final usage charges at least the entire reservation. Actual provider
overshoot is recorded and closes new token admissions. Availability occupied
by concurrent holds is temporary denial, not permanent exhaustion. Hitting a
call count denies new calls without cancelling calls already admitted.

Unjoined or indeterminate work remains held and blocks fresh effects. Lease
expiry and process restart do not prove absence of provider charges. Startup
reconciliation preserves holds and marks interrupted attempts/reservations
unknown. No automatic refund occurs. Settlement retries are idempotent only
when their content is identical. Persistence failures leave holds intact and
are execution failures, including SQL cancellation/deadline failures. The
`PersistenceFailure() bool` marker prevents cancellation filters swallowing
such errors.

## Time

Elapsed time is the union of active root intervals. Concurrent attempts count
the same second once. Queued and fully quiescent waiting-input work do not add
time; synchronous approval waiting and cleanup still count while an attempt
is active. Heartbeats, admissions and lifecycle transitions advance the clock
transactionally. Restart conservatively charges the outstanding issued lease
tail, marks it unknown and blocks new work. Long machine-off time is not
charged as actual execution. Clock regression fails closed. Lease heartbeats
must continue through cleanup, until I/O has really joined.

## Checkpoints and limitations

Ledger-backed checkpoint envelopes contain root/policy/revision identity;
local counters remain only for the no-ledger compatibility path. A newer
ledger than the checkpoint is expected, while an older ledger or mismatched
policy is rejected. Legacy local checkpoints cannot initialize production
quota because failed retry spending cannot be reconstructed from them.

The ledger provides atomic host admission, not a strict provider currency cap.
Input/image estimates and provider-internal HTTP retries can differ from actual
billing. Charge observed provider usage even if it exceeds the reservation.
There is no agent-facing quota-reset or unknown-work-forgiveness tool.

## Verification

Tests use local SQLite, injected clocks and fake models: concurrent ledger
instances, idempotency/conflicting retries, stale fences, root/session/origin
mismatches, settlement after failed/cancelled execution, persistence errors,
unknown restart holds, parallel interval union and actual-usage overshoot.
