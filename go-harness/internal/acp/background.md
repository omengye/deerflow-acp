# Background tasks over ACP

The native Eino backend advertises `_meta.deerflow.background` at initialize.
These are DeerFlow extensions to ACP version 1. They do not add an HTTP service.
Custom SDK engines do not automatically advertise or acquire background support.

## Creation and ownership

The model creates work through `background_agent`; there is no public task-submit
or raw native-resume RPC. Creation binds an actual originating tool receipt to
the parent run, inherited policy, immutable resource specification and existing
budget root. Child models and tools spend that root's remaining budget.

The child has isolated conversation state and receives explicit text instructions.
Parent history, attachment capabilities and connection credentials are not copied.
Client MCP resources are pinned by configuration/schema digests and borrowed
through per-attempt leases. Reattaching a session alone does not authorize an
old task to use its new connections; see resource recovery below. Disabled
subagents, plan and read_only hide submission. Parent
`task_status` and `task_wait` remain available as read tools; `task_cancel` is
available outside plan/read_only.

Every management request requires the currently attached root `sessionId`.
Knowing a task or child-session ID is insufficient. Child sessions cannot be
opened or prompted through ordinary foreground APIs. Reconnect with standard
`session/resume` or `session/load` before using these extensions.

The root owner can inspect the complete background task tree. Foreground and background
roles may create descendants within the configured role/depth policy and shared
root limits. Worker tools can manage only their own descendants. Cancellation
records a durable stop for the selected subtree before delivering cancellation
to each task; an already completed parent can still have active descendants.

## Methods

| Method | Additional parameters | Result |
| --- | --- | --- |
| `_deerflow/tasks/list` | Optional `after` task ID and `limit` (1..100) | `{ "tasks": [...] }` |
| `_deerflow/tasks/get` | `taskId` | Current task snapshot |
| `_deerflow/tasks/wait` | `taskId`, `afterVersion` | Snapshot after a change or at most 10 seconds |
| `_deerflow/tasks/cancel` | `taskId` | Current snapshot after requesting cancellation |
| `_deerflow/tasks/resume` | `taskId`, positive integer `version` | Snapshot after releasing a suspended task |
| `_deerflow/tasks/resources/authorize` | `taskId`, positive integer `taskVersion` and `resourceVersion` | Snapshot after explicitly granting the inspected task compatible reattached MCP resources |
| `_deerflow/tasks/permission/get` | `taskId`, `interactionId`, `taskVersion`, `intentId` | Exact saved tool request for human review |
| `_deerflow/tasks/approve` | `taskId`, `approval` | Snapshot after approval commits |
| `_deerflow/notifications/list` | Optional numeric `after` sequence and `limit` (1..100) | `{ "notifications": [...] }` |
| `_deerflow/notifications/ack` | `notificationId` | `{}` |
| `_deerflow/notifications/process` | `notificationId` | Prompt-style stop reason and `_meta.deerflow.execution` or `_meta.deerflow.backgroundTask` |

Omitted/zero limits use 100. A wait timeout returns a current snapshot. Disconnect
or request cancellation stops that wait, not the task. Explicit task cancellation
waits for the native execution lifecycle to stop its work before reaching a safe
terminal state; the immediate response can still say `running`.

`resumeMethod` is advertised only when the controller can release suspensions.
This method resumes a safe `suspended` checkpoint, including one saved during
clean shutdown. It can also settle `waiting_input` with
`interaction.kind: "cancelled_roles"`: all pending permission branches belong
to explicitly stopped roles, so no tool grant is created. It cannot answer an
ordinary permission wait or restart failed/uncertain tasks. The version is
checked inside the native transition transaction. Startup does not release
suspended tasks automatically. Use `/approve` for remaining real permissions.

## Complete logical task tree

Controllers with the optional task-tree interface advertise
`_meta.deerflow.taskTree` with `version: 1`, `listMethod`, `getMethod`, and an
optional `cancelMethod`, `artifactReadMethod` and `artifactListMethod`. Every call requires the
attached root `sessionId`.

| Method | Additional parameters | Result |
| --- | --- | --- |
| `_deerflow/tasks/tree` | Optional `rootId`, `cursor`, `limit` (zero/omitted uses 100; max 100) | `{sessionId,nodes,hasMore,nextCursor?,consistency:"live_keyset"}` |
| `_deerflow/tasks/tree/get` | `rootId`, `nodeId` | Logical node snapshot |
| `_deerflow/tasks/tree/cancel` | `rootId`, `nodeId`, positive integer `expectedVersion` | Snapshot after persistently stopping the node and its descendants |
| `_deerflow/tasks/tree/artifacts/read` | `rootId`, `nodeId`, `artifactId`, integer `offset`; optional `limit`, `expectedSha256` | Authorized immutable artifact chunk |
| `_deerflow/tasks/tree/artifacts/list` | `rootId`, `nodeId`; optional `cursor`, `limit` (zero/omitted uses 32; max 100) | `{rootId,nodeId,artifacts,hasMore,nextCursor?,consistency:"live_keyset"}` |

Root nodes use `root/<budget-root-id>`; role nodes retain their logical invocation
identity across native background continuations. A node's `taskId` is the current
execution reference. Pages are live keyset pages, not snapshots. Finish a
traversal before pruning cached nodes, then restart with an empty cursor to find
newly inserted nodes that sort earlier. Private child-session IDs grant no access.

Nodes include role/model, foreground/background/root mode, parent identity,
depth, observed state/version, wait details and supported actions. Optional
results contain final parent-facing answers, never private model history or
reasoning. A missing `usage` means attribution is unavailable. `root_total` usage
already includes descendants and must not be added to child usage. `invocation`
usage includes only calls belonging to that logical role across its attempts,
not its descendants. Artifact
entries are metadata only and do not grant content access.

An optional `execution: {runId,version,status,resumable}` is an authoritative
foreground resume reference. Use its run ID and execution version with the
existing execution methods; neither a tree root ID nor a node version may be
substituted. Role references are supplied only when the saved checkpoint has a
trusted association. Historical nodes without such evidence omit the reference.
An inner foreground role paused inside a native background task can instead
have `backgroundExecution: {taskId,version,status,interactionId?,resumable}`.
This points to the real native host. Its version is the native task version,
not the logical node version. Refresh that task to inspect surviving permissions
or use its version to finish a `cancelled_roles` checkpoint without approval.

Artifact reads authorize the live root owner and the exact logical node
on every request, including artifacts committed by a private child session. The
server looks up the committed asset; caller-supplied paths, child sessions and
asset references are rejected. `offset` starts at zero; omitted/zero `limit`
uses 256 KiB, with a maximum of 1 MiB. The response is
`{rootId,nodeId,artifact:{id,name,mimeType,size},sha256,encoding:"base64",data,
offset,nextOffset,eof}`. For subsequent chunks, send the returned SHA-256 as
`expectedSha256`; any supplied digest is checked even at offset zero. The first
read verifies the full asset hash and records fixed 1 MiB block hashes in a
bounded cache. Later reads verify the complete blocks covering the requested
range, and every read checks the file size. Changes to another block are
detected when it is requested. The file limit is 200 MiB. Clients should
read only on demand, use bounded buffers and validate offsets, final size and
digest before exposing a completed local file. An empty final chunk uses the
empty base64 string. A display name is never authority for a destination path.
The current read operation reserves the root session like ordinary asset reads;
an active prompt returns a request-local busy error.

`present_files` still accepts only paths. The host captures the actual root,
budget run/attempt/fence and role invocation/attempt at tool execution. These
relationships commit with the successful tool receipt and immutable asset.
Root, foreground and background outputs are projected only under that saved
node, including completed and checkpointed attempts after restart. A newer
physical continuation does not replace historical ownership. Legacy outputs
without role provenance retain their exact native background container binding;
no foreground/root attribution is inferred for them.

Each tree node contains at most 32 artifact summaries. Use `artifacts/list` to
enumerate all snapshots for that node. Its opaque live keyset cursor binds the
root owner, session, root and node. New outputs with earlier IDs require a fresh
traversal. Waku keeps a separate 32-entry selected-node page with first/next
controls, and its existing bounded, SHA-verified Save As flow accepts metadata
from either the node summary or this authenticated page.

Cancelling a root stops the whole tree, including descendants not loaded by the
client. Other nodes cancel their subtree; there is no node-only operation that
leaves descendants running. The durable stop precedes I/O cancellation and
blocks new work. The immediate result can remain `cancelling` while cleanup or
saved checkpoint handling is pending; clients must not synthesize completion.
Task-management RPC errors affect that request, not the chat's running prompt.

Permission branches carry trusted role provenance. Cancelling a paused role
uses an internal cancellation resume value, never a fabricated approval or
rejection. Stopped branches settle as `not_executed` and release their paused
budget; unaffected siblings still require their real permissions. For a native
background batch, the effective interaction ID changes deterministically with
the persisted stopped-intent set. An approval racing after that stop must refresh
the batch. Grants already committed before the stop remain valid only for the
unaffected siblings and original attempt; grants for stopped roles are revoked.

## Client MCP resource recovery

The optional `resourceAuthorizeMethod` is advertised under
`_meta.deerflow.background`. Task snapshots may include `resources` with its own
`version`, `status`, `reason`, saved `descriptor`, and `canAuthorize`. Resource
status is separate from the native task status: `ready`, `waiting_connection`,
`waiting_authorization`, `incompatible`, or `unreconstructable`.

Disconnect drains each affected attempt before releasing its parent connections.
Queued work waits without spending another model call or execution attempt.
After reconnecting the original root session with compatible MCP configuration,
read `/tasks/get` and explicitly authorize its current `taskVersion` and
`resources.version`. Both versions and current ownership are checked. A
terminal source task may need resource authorization for a later continuation;
this never reruns the finished task.

The grant only authorizes the connection. A safely suspended task still uses
`/tasks/resume`; a permission wait still needs its reviewed `/tasks/approve`;
unknown external effects still require receipt reconciliation. No reconnect,
grant, or resume automatically replays an uncertain tool call. Old checkpoints
that contain only a process-local generation cannot reconstruct a descriptor
and expose `unreconstructable` with `canAuthorize: false`.

Descriptors persist only digests and public server/tool names. Credentials,
commands, URLs, environment and headers remain in the connection owner. This
implementation requires the client to recreate the connection set; it does not
provide a persistent host recipe registry for unattended MCP reconstruction.
These extensions target the current ACP v1 connection; the separate draft-v2
facade must not be assumed to forward them.

Task snapshots carry `id`, `sessionId`, `childSessionId`, `rootSessionId`,
`parentTaskId`, `parentInvocationId`, `invocationId`, `continuationOf`, `depth`, `status`, `version`,
`attempt`, timestamps and optional result/error/blocked reason. In SDK/ACP JSON,
opaque `result` and notification `data` byte fields use standard Go JSON base64
encoding. Model tools render task results as readable JSON or text.

For v5 tasks `depth` includes foreground roles. `parentTaskId` identifies the
nearest background ancestor and `parentInvocationId` the immediate role parent;
they can differ when a foreground role sits between two background tasks.

## Durable descendant waits

A role executing inside a background attempt can pause `task_wait` with `status: waiting_input`,
`interaction.kind: task_wait`, `interaction.waitingTasks` containing descendant
IDs, an empty `waitingInputs`, and `resumable: false`. The host resumes this saved
execution when the descendants finish without unresolved effects. No user
approval is needed for the wait itself; descendants may still need approval for
their actual tools. Clients must not offer `/approve` for a pure task wait.

This durable model tool releases the worker, allowing three levels to complete
with one worker and one active role slot, including mixed foreground/background
delegation. Restart preserves saved waits and role
versions. The ten-second public `/tasks/wait` contract is unchanged. Root
foreground runs also have durable waits and attached-owner automatic wakeup.
Terminal inbox notifications can separately create a new continuation under
the saved opt-in root policy described below.

## Approvals

A task paused for permission includes `interaction.id`, `interaction.taskVersion` and
descriptive `waitingInputs`. Lists contain parameter names/types and a digest,
not the argument values. Use the advertised `permissionMethod` to retrieve each
pending intent's exact saved tool request for the currently attached owner:

```json
{
  "sessionId": "parent-session-id",
  "taskId": "task-id",
  "interactionId": "saved-interaction-id",
  "taskVersion": 3,
  "intentId": "intent-id"
}
```

The preview is read-only and rejects stale versions. Display its `toolName` and
`arguments` before collecting a decision. Native checkpoint addresses and
execution grants remain private. Then submit either one decision for the batch:

```json
{
  "sessionId": "parent-session-id",
  "taskId": "task-id",
  "approval": {
    "id": "saved-interaction-id",
    "taskVersion": 3,
    "decision": "allow_once"
  }
}
```

Or use `decisions` in place of `decision`, supplying every pending intent:

```json
{
  "id": "saved-interaction-id",
  "taskVersion": 3,
  "decisions": [
    { "intentId": "intent-id", "version": 1, "decision": "reject_once" }
  ]
}
```

Only `allow_once` and `reject_once` are accepted. Missing, duplicate or stale
decisions fail; clients cannot supply arbitrary evidence or native target maps.
Approval and the native pending transition commit together. Each one-use grant
is consumed with the matching actual tool dispatch or denial receipt. Repeated
approval never repeats the tool. A disconnected client can discover and approve
the same saved interaction after reconnecting.

## Errors and notifications

`-32020` means background execution is unavailable or closing; `-32021` means
state/version conflict and requires a fresh get; `-32022` means the task cannot
safely continue. Unknown effect/cleanup outcomes are retained for inspection,
not automatically replayed. Unowned IDs and malformed parameters are rejected.

The persistent notification inbox deduplicates native outbox deliveries before
acknowledging the outbox. Listing notifications does not acknowledge them or
approve work. UI acknowledgement does not run the parent model.

### Processing a notification

`background.notifications.processMethod` is advertised only when the host
implements the optional notification processor. Call it with exactly
`sessionId` and `notificationId`. It reserves the same foreground session slot
as prompt and execution resume, emits standard session updates, and uses the
normal permission exchange. An occupied foreground execution blocks a new
admission without consuming the inbox item.

Processing accepts an immutable snapshot as a new parent run/input under the
originating budget. It preserves the original prompt, input and tool receipt;
the model receives a marked notification, including readable task output.
Current host/session configuration must still match the originating task.
Earlier one-use tool approvals do not authorize new continuation tools.

Repeated processing returns the execution already bound to that notification
without calling the model again. A waiting continuation uses
`_deerflow/executions/get|resume|cancel` with its run/version. Approval-channel
disconnect preserves the wait for a newly attached owner. Native checkpoints
and resume targets are never supplied by the client.

Native outbox acknowledgement, UI acknowledgement, and model delivery are
independent: processing does not mark an inbox item read, and a known UI-acked
notification can still be processed. Tasks created with `auto_continue: true`
can also be processed by the current connection's automatic root scheduler.
If the originating budget is exhausted, processing returns `-32015` with the
limited resource and leaves the notification unadmitted.

The SDK equivalents are `BackgroundTasks`, `BackgroundTask`,
`WaitBackgroundTask`, `CancelBackgroundTask`, `ResumeBackgroundTask`, `ApproveBackgroundTask`,
`BackgroundNotifications` and `AcknowledgeBackgroundNotification`.
`BackgroundPermission` provides the owner-only argument preview.
`ProcessBackgroundNotification` performs explicit model delivery; omitting its
SDK permission handler leaves ask-policy tools durably waiting for approval.

### Foreground model waits

The model's `task_wait` can suspend a foreground execution or its foreground
roles. `_deerflow/executions/get` reports `waitingTasks` alongside any separate
`waitingInputs`. A pure task wait has no approval intent and is not resumable
until every dependency is terminal and free of blocked/uncertain effects. A
mixed checkpoint may resume its permission batch first. Use the existing
execution resume method with the same run/version; do not send another prompt
or process the notification while that execution is waiting. It resumes the
saved run/input with its original budget and role catalog.

This does not change the public `/tasks/wait` bounded query. A live ACP session
now automatically resumes a ready pure task wait after the original prompt has
returned. `session/new|load|resume` attaches one connection-owned update pump;
repeated attachment does not add another. Reconnect can resume the saved run.
Disconnect or session close cancels and joins the pump before resource cleanup.
`session/cancel` explicitly cancels even an idle waiting execution and disables
automatic work for that root.

The execution capability adds `automaticTaskWaits: true`,
`lifecycleNotifications: true` and
`updatedNotification: "_deerflow/executions/updated"`. After the execution lease
commits, before any automatic engine work/output, the notification is
`{sessionId,phase:"started",operationId,runId,execution}`. Automatic model/tool
output uses ordinary `session/update`; fresh permissions use
`session/request_permission`. On completion or a newly committed wait, it emits
`{sessionId,phase:"finished",operationId,runId,result:{stopReason,execution?,
backgroundTask?,limit?},error?}`. Match session, opaque operation ID and run ID,
and check monotonic execution versions. One operation may span multiple
permission attempts, so matching attempt IDs is incorrect.

Errors use the JSON-RPC `{code,message,data?}` structure; raw internal diagnostic
errors are not exposed. A failed durable settlement may omit `result.execution`;
the top-level operation/run identity still ends that output operation, while
the client queries durable state. An admission that only enqueues background
work has no operation ID and is a refresh signal, not a new output turn. The
`automatic` execution field reports retry/stop state. No synthetic prompt or
history event is inserted to trigger the resume.

Waku binds these notifications to the attached connection generation, buffers
bounded notifications during attachment, and refreshes the task tree even when
the panel is hidden. An old automatic result cannot end a newer user prompt.
Same-run wakeups retain the original input; automatic terminal notification
delivery creates a new input/run with the originating budget and saved policy.

### Automatic notification delivery

Durable hosts advertise `background.notifications.automaticRootDelivery`,
`automaticForegroundRoleDelivery`, `roleResultNotifications`, and `deliveryState`.
Hosts with native private-role dispatch additionally advertise
`automaticBackgroundRoleDelivery`.
`auto_continue` defaults to false, and
`max_auto_continuations` defaults to 10 (zero disables it). The policy is pinned
when a new background task is created; older specs without a policy remain
explicit-only. A live attached connection is required. User input, pending
executions, resource/policy checks, root budget and explicit root stops gate
automatic admission. New writes require the normal live permission exchange.

Inbox items may include `delivery: {status, detail?, attempts, retryAfter?,
runId?, recipientRole?}`. Status is `queued`, `admitted`, `consumed`, `disabled`,
`stopped`, `limit_reached`, `blocked`, `waiting_parent`, or `ignored` (nonterminal).
`admitted` identifies the unique accepted continuation. For a foreground
continuation, query its execution; for native background dispatch, `runId` is
the accepted task ID and `/tasks/get` reports its outcome. An already delivered
managed-task result becomes `consumed`.
This covers tools executing in foreground and background roles. The delivery is
committed with saved execution progress, and appears consistently whether the
notification arrived before or after that commit. Reading the result from an
ancestor role does not consume the original private parent's notification.
UI read acknowledgement does not affect these decisions. Failed admission is
bounded to three attempts; each root also retains its new-run count across
restart. Automatic results use `_deerflow/executions/updated` and ordinary session
updates. Explicit processing can recover an item after the automatic limit.

For v5 tasks created by a private role in the root foreground execution,
explicit/automatic processing restores that original role's committed private
conversation, model, tools, skills, placement and catalog. New permissions are
requested normally, including after another restart. Private role messages do
not become root native history.

A terminal private continuation atomically publishes `kind: "role_result"`
with ID `role-result/<runId>` for its immediate ancestor. Each hop forwards only
the final role summary, preserves original role IDs and shares the original
root's continuation allowance. Ordinary certified provider failures publish a
generic failed summary with execution status `failed` and stop reason
`role_failed`; uncertain effects and storage failures remain blocked for review.
Successful summaries bind their committed conversation revision and digest.

### Native background parent continuations

Private parents executing in native background hosts use the versioned
`deerflow-continuation-v6` executor. The completed task retains its terminal
record. The host creates a new task and private session with the original
logical `invocationId`, pinned role/model/tool/skill catalog, committed private
conversation and budget root. `continuationOf` identifies the previous physical
host. No new delegated-role quota is charged. Source, task, input, budget member
and automatic allowance commit in the same native creation transaction.

Explicit processing returns `_meta.deerflow.backgroundTask`; automatic admission
includes `result.backgroundTask` in `_deerflow/executions/updated`. Admission is
not completion: query/wait for the returned task and use the normal task approval
methods for its fresh tool permissions. Repeating the same notification returns
the same task. Further sibling notifications continue from the latest committed
private role conversation, including after restart.

The final native result is routed to the original logical parent. Foreground
ancestors publish `role_result` summaries as described above; background ancestors
create their own v6 continuations. Private tool history and raw descendant results
do not enter root history. All automatic hops share the original root allowance.

Cancelling an old parent task includes its later native continuations and their
descendants. Explicit root stop also fences already-admitted automatic native
continuations; old permissions cannot restart them. A stopped pending interaction
is reported as nonresumable while native cancellation settles, so task lists remain
readable. Persisted source and continuation-link identity are checked before
execution; cleanup verifies that identity independently of current stop policy.

`waiting_parent` remains for missing conversations, an active parent continuation
or unsettled parent resources. Corrupt conversations/result sources are `blocked`
and do not starve later notifications. Client MCP recovery and corresponding
Waku authorization/resume actions follow the resource contract above; unattended
host recipe reconstruction remains outside the implemented connection lease.
