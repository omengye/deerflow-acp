# Native background permission broker

`BackgroundInteractionStore` supplies durable human approval for native Eino
background tasks. It shares the host's SQLite database and native task version
CAS, while keeping its permission tables separate from foreground executions.
The native task manager owns scheduling, attempt numbers, and checkpoints.

## Trusted attempt wiring

The host opens an attempt only after the native task has acquired its child
session lease and the root budget attempt has started. `OpenAttempt` validates
that fence, the durable child input/configuration, and the parent policy. Its
`Hooks` install the broker and trusted native resume targets into the rebuilt
agent. Every model/tool budget reservation and every tool dispatch must use the
same native task/child-session fence.

`Publish` persists child events and tool receipts. Grant consumption occurs in
the same transaction as `tool_execute` or the terminal denial receipt. Resolving
a permission decision is read-only and grants no independent right to invoke a
tool. A receipt write failure rolls back grant consumption. A grant is bound to
one intent version and one native attempt and cannot authorize another task,
tool call, or retry.

The prepared agent observer passes trusted native interruption bindings to
`StageInterrupts`. Individual parallel interruptions are merged and checked for
consistency. Staging alone does not publish resumable work. The final transition
requires exact coverage of every pending intent and tool receipt.

## Native transition transaction

The host calls the broker's `TransitionTx` inside the native task transaction:

1. On `pending -> running`, after taking the child lease, revalidate approved
   manifests and ready grants before permitting a new native attempt.
2. On `running -> waiting_input`, after joining engine resources, committing
   budget/business projections, and promoting the actual native checkpoint,
   publish a manifest before deleting the child lease. Any failure rolls back
   all these SQL changes and the native task transition.
3. On `waiting_input -> pending`, call `CommitResumeTx` before the host transition
   callback. It inserts the next attempt's grants and changes the manifest from
   waiting to approved within the native version CAS transaction.
4. On terminal transitions, revoke ready grants, cancel pending intents, close
   the manifest, and discard staged interrupt bindings. Idle cancellation uses
   the same transaction as business receipt settlement and native cancellation.

`TransitionTx` intentionally performs SQL checks directly. It must not invoke
the active attempt effect hook, since the native host holds its attempt mutex
while committing transitions. The supplied access authorizer must check
process-local session ownership without opening a nested database transaction.

## Manifest and public interaction

The manifest binds the immutable task/parent/child/root-budget identity, native
attempt and waiting version, native runner checkpoint hash, native task
checkpoint hash, accepted child input/configuration hashes, native session head,
business event cursor, receipt frontier, and every native interruption address.
Approval preparation and the next attempt validate these frontiers again.
Changed checkpoints, effects, model history, input, or parent policy prevent
replay. Restoring checkpoint bytes never refunds the root budget ledger.

`BackgroundInteraction` exposes a batch approval ID, waiting task version,
descriptive intent metadata, and resumability. It does not expose raw arguments,
checkpoint data, native interruption addresses, or permission grant IDs. Exact
argument bytes are retained privately using the same versioned permission
envelope as foreground executions, including whitespace inside nested JSON.

The separate owner-only `Permission` preview returns one exact saved tool
request for human review. It requires the task, waiting version, interaction
batch and intent IDs; rechecks the manifest and current attachment in the read
transaction; and never inserts a grant or mutates an inbox item. SDK/ACP clients
use this before asking a user to decide. Raw values remain absent from task
lists, notification metadata and model-visible status tools.

An approval supplies the exposed batch ID and exact task version. Either its
`Decision` applies to all pending intents, or its `Evidence` contains complete
per-intent decisions while `Decision` is empty:

```json
{
  "decisions": [
    {"intentId": "intent-a", "version": 1, "decision": "allow_once"},
    {"intentId": "intent-b", "version": 1, "decision": "reject_once"}
  ]
}
```

Unknown fields, duplicate or missing intents, wrong versions, invalid decisions,
replacement prompts, and client-provided native targets are rejected. Preparing
approval is read-only; durable grants only appear if native resume commits.
Public task approvals accept only `allow_once` and `reject_once`; they do not
establish a reusable approval preference.
Current parent-session ownership is checked both before preparation and inside
the commit path. Connection identities and old JSON-RPC request IDs are not
resume credentials.

## Inherited policy and graceful drain

Background child configuration retains the parent's pinned mode and approval
policy. The optional internal `PolicyPermissionBroker` resolves configured
decisions after ordinary tool budget admission and intent creation. In one
transaction it checks the active attempt and current parent policy, inserts a
one-use grant, and records the exact policy/configuration digest in
`harness_background_policy_grants`. No human actor is manufactured: its human
approver field is empty and the policy audit row supplies authority. Execution
and denial consume these grants through the same receipt transaction as human
grants. An undecided policy follows the usual native interruption path.

`plan` and `read_only` allow the reserved `task_status` and `task_wait` host
queries, while rejecting `task_cancel` and `background_agent` changes. Inherited
`allow_always` and `reject_always` configuration resolves automatically for each
new tool intent, with a fresh attempt-bound grant each time.

A native graceful drain records a separate frontier in
`harness_background_drain_manifests`, including when no human interaction has
occurred. Its commit requires joined cleanup and the promoted native checkpoint,
and captures input/configuration, native history, business events, and receipt
digests. It closes the previous permission manifest and revokes its unused
grants. Completed effects remain completed; pending intents remain pending.

Explicit suspension release validates this frontier before moving it to the
released state. Native task start and attempt open validate it again. Restored
hooks contain no old permission targets or grants, so any still-pending native
permission must interrupt again and obtain fresh authority. Repeated drains
replace their frontier atomically. Terminal or subsequent waiting transitions
close the obsolete drain frontier. Started/uncertain effects and changed
frontiers cannot be resumed by releasing a suspension.

## Eino JSON resume boundary

Eino alpha.35's native background executor decodes pending resume JSON into
maps containing `json.Number`. The engine adapter accepts this representation
only with exactly the permission resume fields and a positive integral version,
and only when it matches a trusted target from the attempt's hooks. The broker
then independently checks the intent, grant, receipt, and active attempt. Native
background continuation thus uses Eino's checkpoint implementation while
permission authority remains in the durable runtime broker.

## Verification

`background_permissions_test.go` drives the native SQL task provider and shared
ledger. It covers parallel interrupt coverage, exact one-use allow/deny grants,
stale owners and attempts, public metadata privacy, malformed approval evidence,
checkpoint/native/input/receipt/policy drift, and idle cancellation. Injected SQL
failures verify rollback of waiting publication, approval/native resume,
grant/receipt dispatch, and terminal cancellation. SDK host tests separately
exercise real Eino child interruptions and process close/open continuation.
