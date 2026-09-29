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
Nonempty client MCP configurations currently disable `background_agent`, because
those connection-owned capabilities cannot be rebuilt safely in a background
attempt. Disabled subagents, plan and read_only also hide submission. Parent
`task_status` and `task_wait` remain available as read tools; `task_cancel` is
available outside plan/read_only.

Every management request requires the currently attached parent `sessionId`.
Knowing a task or child-session ID is insufficient. Child sessions cannot be
opened or prompted through ordinary foreground APIs. Reconnect with standard
`session/resume` or `session/load` before using these extensions.

## Methods

| Method | Additional parameters | Result |
| --- | --- | --- |
| `_deerflow/tasks/list` | Optional `after` task ID and `limit` (1..100) | `{ "tasks": [...] }` |
| `_deerflow/tasks/get` | `taskId` | Current task snapshot |
| `_deerflow/tasks/wait` | `taskId`, `afterVersion` | Snapshot after a change or at most 10 seconds |
| `_deerflow/tasks/cancel` | `taskId` | Current snapshot after requesting cancellation |
| `_deerflow/tasks/resume` | `taskId`, positive integer `version` | Snapshot after releasing a suspended task |
| `_deerflow/tasks/permission/get` | `taskId`, `interactionId`, `taskVersion`, `intentId` | Exact saved tool request for human review |
| `_deerflow/tasks/approve` | `taskId`, `approval` | Snapshot after approval commits |
| `_deerflow/notifications/list` | Optional numeric `after` sequence and `limit` (1..100) | `{ "notifications": [...] }` |
| `_deerflow/notifications/ack` | `notificationId` | `{}` |

Omitted/zero limits use 100. A wait timeout returns a current snapshot. Disconnect
or request cancellation stops that wait, not the task. Explicit task cancellation
waits for the native execution lifecycle to stop its work before reaching a safe
terminal state; the immediate response can still say `running`.

`resumeMethod` is advertised only when the controller can release suspensions.
This method resumes a safe `suspended` checkpoint, including one saved during
clean shutdown; it cannot answer `waiting_input` or restart failed/uncertain
tasks. The version is checked inside the native transition transaction. Startup
does not release suspended tasks automatically. Use `/approve` for permissions.

Task snapshots carry `id`, `sessionId`, `childSessionId`, `status`, `version`,
`attempt`, timestamps and optional result/error/blocked reason. In SDK/ACP JSON,
opaque `result` and notification `data` byte fields use standard Go JSON base64
encoding. Model tools render task results as readable JSON or text.

## Approvals

A `waiting_input` task includes `interaction.id`, `interaction.taskVersion` and
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
approve work. At this stage, inbox consumption as a new durable parent model
input is still being implemented; merely acknowledging an item does not run
the parent agent.

The SDK equivalents are `BackgroundTasks`, `BackgroundTask`,
`WaitBackgroundTask`, `CancelBackgroundTask`, `ResumeBackgroundTask`, `ApproveBackgroundTask`,
`BackgroundNotifications` and `AcknowledgeBackgroundNotification`.
`BackgroundPermission` provides the owner-only argument preview.
