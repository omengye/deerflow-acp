# Documents, goals, schedules and WeChat

These features use the existing Go/Eino harness, SQLite state, ACP session
ownership, permissions and task budgets. No Python runtime, HTTP application
backend or RAG service is added. Rebuild the daemon and desktop package before
using them. Only legacy WeChat channel state is migrated automatically; unrelated
personal configuration is left unchanged.

## Documents and artifacts

- Desktop attachments accept UTF-8 text plus PDF, DOCX and XLSX. Limits are eight
  files per prompt; text 1 MiB/file and 4 MiB total; documents 8 MiB/file and
  16 MiB total. Images retain their existing limits. Large mixed-media prompts
  are also subject to the ACP 64 MiB frame limit.
- `list_attachments` lists opaque, session-scoped IDs. `read_attachment` returns
  at most 4096 UTF-8 bytes with `nextOffset`. Originals remain immutable snapshots;
  extracted Office/PDF text is cached by asset ID, SHA-256 and converter version.
- `analyze_attachment` accepts `{ "id": "...", "header": true }` for CSV, TSV
  and XLSX. Results include per-column nonempty/missing/numeric counts and
  min/max/mean/sum. Non-numeric values are not coerced. Data can be exported with
  existing `write_file` and `present_files` tools as CSV/JSON/Markdown artifacts.
- DOCX extracts paragraph text. XLSX preserves sheet names and sparse columns,
  uses cached formula results, and does not calculate formulas or convert Excel
  date serials. Embedded objects, macros and external references are not run.
  Archives are bounded to 2048 entries, 4 MiB per XML entry, 32 MiB expanded XML,
  64 sheets and 100000 cells. Analysis allows at most 256 columns per sheet.
- PDF requires an operator-installed **Poppler `pdftotext`** on PATH, or
  `DEERFLOW_PDFTOTEXT` set to an absolute executable path. The daemon never
  downloads it. Extraction is limited to the first 200 pages, 20 seconds and
  4 MiB text. Encrypted/malformed PDFs and image-only PDFs return explicit errors.
  OCR is not included. Keep Poppler patched because it parses untrusted files.
- After saving an artifact, the task panel offers a passive text preview capped
  at 8 KiB. PDF/DOCX/XLSX preview uses the bundled `deerflow-control-go` helper;
  PDF still needs Poppler. This is extracted-text preview, not Office/PDF page
  rendering. HTML/SVG are never loaded into a privileged browser origin.
- Preview reads run off the UI thread and stale responses are generation-fenced.
  Artifact transfers retain their existing complete SHA-256 verification.

## Goal evaluation

Send these as whole messages in the desktop or a connected WeChat conversation:

```text
/goal Implement the requested change and provide passing test evidence
/goal
/goal clear
```

Setting a goal does not invoke a model. The next ordinary prompt runs the task.
Goals persist across daemon restarts and are injected into the native agent's
instruction, including after context compaction. Evaluation uses a separate
**metered call to the same configured model** and the last answer plus bounded
tool evidence. It is not an independent correctness guarantee.

Automatic continuation is opt-in host YAML:

```yaml
local_acp:
  goal_auto_continue: true
```

There are at most three continuations per logical run, sharing its existing
token/call/time limits, tool policy and approval boundary. Only
`goal_not_met_yet` with a concrete next action can continue. Missing evidence,
invalid evaluator output, user input, external waits, failed/denied tools,
cancellation and repeated absence of new tool evidence stop continuation.
Native retries do not replay completed tools. An achieved verdict becomes an
achieved goal only after the containing run commits successfully. Failed cleanup,
uncertain effects or waiting approval do not complete the goal. Use `/goal clear`
to remove it or set a new objective to replace it; suspended runs must first be
resolved/cancelled through existing task controls.

## Local control CLI

`deerflow-control-go` is included by the Windows desktop and Windows/Linux Go ACP
packaging scripts. It connects to
the authenticated local DFACP/1 management endpoint. It reads the local token
internally; do not copy endpoint tokens into requests. Requests are JSON on stdin
or UTF-8 files, up to 64 KiB. `--runtime-dir DIR` selects a nondefault daemon.

```powershell
'{"operation":"schedule.list"}' | .\deerflow-control-go.exe
.\deerflow-control-go.exe --request .\schedule-request.json
```

For a portable desktop package, run commands from its directory with
`--runtime-dir .\user-data\runtime\acp-go`. The default is instead
`DEERFLOW_GO_RUNTIME_DIR`, or `%LOCALAPPDATA%\DeerFlow\go-acp` on Windows.
The control command must select the same runtime directory as the running daemon.

These are host-management commands. Do not expose the endpoint or CLI with
arbitrary requests to remote users. The Agent uses the separate, scoped
`schedule_read` and `schedule_manage` tools for conversation-based scheduling.
Settings > Scheduled tasks manages the same persisted schedules. Channel
administration has a native Settings > Channels page.

## Durable schedules

### Conversations, ownership and authorization

Desktop, WeChat and Buzz share the native scheduling engine. Ask the Agent to
create a reminder/recurrence, list tasks, change timing, pause/resume/delete, or
read execution history. Success requires a persisted receipt with task ID,
timezone, next fire and notification target, not an optimistic natural-language
promise. The model receives native capability guidance and a clock/status tool.

- `schedule_read`: `list`, `get`, `history`, `status`, `preview`.
- `schedule_manage`: `create`, `reschedule`, `pause`, `resume`, `delete`.
  Creation requires a stable `request_id`; retries within the source message
  reuse the task, and changing parameters under the same key is rejected.
- Workspace, source identity, recipients and Full Access grant are host-bound,
  never model arguments. Read-only/plan mode cannot mutate schedules. Native
  scheduled work and delegated/background agents cannot create more schedules.
- Channel users see only their own tasks in their workspace, across conversations
  and restarts. Local desktop settings remain administrative. Shared Buzz
  channels now use sender-scoped Agent sessions; old shared sessions remain in
  history but are not reused. A message admitted before authorization cannot
  acquire a later grant merely by waiting in the inbox.
- WeChat requires the existing local Full Access channel authorization. Buzz
  additionally requires `[deerflow] schedule_full_access = true` and a restricted
  sender allowlist (an attested owner is supported). `permission_policy=allow_once`
  alone does not authorize persistent Full Access jobs. The adapter must remain
  running to renew its local scheduling lease and deliver results; if the lease
  expires at dispatch, the task pauses for attention. Revocation/account/workspace
  changes invalidate old grants; reauthorization does not silently upgrade old
  tasks. Existing legacy schedule permissions are preserved.
- Notifications default to the source conversation. WeChat uses its existing
  durable channel outbox; Buzz uses a claimed, acknowledged outbox. Unknown send
  outcomes are quarantined rather than resent. Notification failure never reruns
  the task. The desktop card/history and `schedule_read` show notification state
  separately from execution state. Delete retains already-produced results.
- Cross-channel identity is never inferred. A local administrator can explicitly
  link source principals using `schedule.owner.link` with `sources` (each has
  `kind`, `account`, `sender`, `workspace`) and an opaque local `owner` string.
  An empty owner unlinks those principals. This is a local-only operation, not
  an Agent tool; linked users still need current authorization to mutate tasks.

No chat window needs to stay open. The daemon and relevant channel transport
must remain running; there is no OS wakeup or promise of delivery after shutdown.

### Local management

Example `schedule-request.json`:

```json
{
  "operation": "schedule.create",
  "spec": {
    "name": "Morning workspace report",
    "workspace": "D:\\Work\\project",
    "prompt": "Read the workspace and summarize changes. Do not modify files.",
    "kind": "cron",
    "cron": "0 9 * * 1-5",
    "timezone": "Asia/Shanghai",
    "approval": "read_only"
  }
}
```

Other types use `kind: "once"` with `at: "2026-11-01T09:00:00+08:00"`, or
`kind: "interval"` with `every_seconds: 3600`. Only fields for the selected kind
are accepted. Cron has five fields (minute/hour/day/month/weekday); IANA timezones
are bundled. Intervals must be 60 seconds to one year. At most 200 schedules exist.

```json
{"operation":"schedule.status"}
{"operation":"schedule.list"}
{"operation":"schedule.snapshot","id":"OPTIONAL_HISTORY_SCHEDULE_ID"}
{"operation":"schedule.preview","timing":{"kind":"cron","cron":"0 9 * * 1-5","timezone":"Asia/Shanghai"}}
{"operation":"schedule.reschedule","id":"SCHEDULE_ID","expected_revision":1,"timing":{"kind":"interval","every_seconds":3600,"timezone":"Asia/Shanghai"}}
{"operation":"schedule.pause","id":"SCHEDULE_ID","expected_revision":2,"paused":true}
{"operation":"schedule.pause","id":"SCHEDULE_ID","expected_revision":3,"paused":false}
{"operation":"schedule.history","id":"SCHEDULE_ID"}
{"operation":"schedule.history"}
{"operation":"schedule.ack","id":"OCCURRENCE_ID"}
{"operation":"schedule.delete","id":"SCHEDULE_ID","expected_revision":4}
```

- Daemon must remain running; no Windows Task Scheduler or OS cron is installed.
- Each occurrence gets an isolated normal session, saved before the first run.
  The worker serializes dispatch and uses the normal shared run queue. A single
  dispatch has an additional 30-minute ceiling. No desktop-owned session is stolen.
- Newly created schedules default to Full Access (`approval: "allow_always"`).
  Creating a schedule through the trusted host interface or an authorized Agent
  scheduling tool authorizes its enabled
  tools without ordinary per-call approval. Explicit `read_only` and `ask` are
  respected; `ask` may suspend for desktop approval. Existing schedules are never
  upgraded, including legacy omitted approval (read-only). Editing time does not
  change permissions. Tool enable/deny rules and dedicated skill approval gates
  still apply; Full Access does not imply filesystem isolation.
- The native page displays cards, actual permissions, plan status, next planned
  time with timezone offset, and recent execution/history with session links.
  It supports time edits, pause/resume and deletion, not creation or run-now.
  Saving takes effect once without a second confirmation; only deletion asks
  for confirmation. Drafts survive refresh/navigation and cannot overwrite a
  newer revision. Unavailable service is not presented as an empty task list.
- `schedule.snapshot` returns bounded card/history summaries and scheduler
  status (`reschedule_version: 1`). `schedule.list` retains complete specs.
  `schedule.preview` validates without persisting; interval next-fire time is
  recalculated at save/resume time. Paused tasks remain paused after a time edit.
- Pause, delete and reschedule require the current `expected_revision` from a
  fresh list/snapshot. Claim, completion and mutations advance this revision.
  `conflict`, `busy`, `not_found`, and `invalid_request` distinguish failures.
  The desktop also pins the daemon `expected_instance_id`. After a lost reply,
  refresh and inspect rather than retrying blindly (intervals would reset again).
- Claimed/running tasks cannot be edited or deleted. Pausing future recurrence
  is permitted during execution. An ended single-run task can be explicitly
  rearmed using `schedule.reschedule` with `rearm: true` and a future `once` time;
  this preserves its ID and history and enables that new occurrence.
- Durable claim and next-fire advancement happen before execution. Missed fires
  coalesce to one run, not an unbounded catch-up backlog. Pause affects future
  fires, not the active run. An actively running session is owned by the worker;
  the page never steals that lease. Session links become available after the
  run completes or suspends for attention.
- Failure, cancellation, waiting input or crash pauses the recurrence. A crash
  without a terminal receipt is `unknown`, never replayed. Inspect the session
  before explicitly resuming future fires. Expired once schedules cannot resume.
- History is the durable notification inbox (latest 100 occurrences, optionally
  filtered by schedule); `ack` marks an occurrence read. Delete removes scheduling
  but retains history. `schedule.status` reports worker persistence failures.
- Optional generic `notify` selects a bound channel account/conversation/sender;
  the legacy `notify_wechat: "ILINK_USER_ID"` alias still works. For WeChat the user
  must match the bound owner (or preserved legacy policy) and have messaged the bot first, because iLink
  requires a conversation context token. Notification admission and terminal
  occurrence commit share one transaction. If the channel is disabled/logged out,
  the local history remains the delivery destination. Missing/expired context or
  an ambiguous send is not retried automatically; inspect `wechat.history`.

## Channel plugins and WeChat iLink

Channels now use independent trusted plugins managed by the host. Settings >
Channels provides workspace selection, enable/disable, QR login, verification,
binding/connection state, recent tasks and logout. See [channel-plugins.md](channel-plugins.md)
for protocol, security, migration, installation and rollback instructions.

The bundled WeChat plugin accepts one-to-one text only. It automatically binds the
login identity; a local one-time pairing challenge is used when the provider does
not return it. No manual allowlist or separate tool-permission setting is needed.
New channel tasks inherit host global permissions; they never auto-approve tools.
Legacy policies and message history migrate once without replaying unfinished work.

Channel config is stored in the host SQLite database. The legacy `wechat` YAML is
not a live settings source after migration. Windows credentials remain DPAPI protected.
Disabling retains the binding; logout clears it. Changing one channel does not
restart the desktop, shared Go daemon or Buzz adapter.

## Verification and remaining deployment checks

Offline tests cover parser bounds, immutable assets, ACP blob input, goal metering
and termination, schedule restart/dedupe/timezones, iLink protocol/allowlists,
uncertain sends, logout cancellation, and transactional scheduled notifications.
Rust checks and unit tests do not constitute visual validation. No personal
configuration, live WeChat account, running desktop process or production database
is changed by development tests. Live iLink pairing/receive/send and optional
Poppler installation must be verified on the deployment machine.
