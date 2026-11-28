# Channel plugins v1

Channels are trusted child processes speaking bounded JSON-RPC 2.0 over private
stdio. They transport messages; the Go host owns account admission, workspace
selection, sessions, model calls, tools, budgets, approvals, SQLite and outbox
receipts. This is process isolation, **not an OS security sandbox**. Install only
trusted binaries. There is no plugin marketplace or automatic download.

## Desktop

Settings > Channels lists installed instances. The bundled WeChat plugin needs
only an existing workspace directory: enable it and scan the QR. Verification
codes are shown only when requested by the provider. Binding and connection are
separate states. Connected means a successful provider poll, not merely a stored
token. Desktop status polling stops when the page is closed; the host worker stays
running. The host must remain running to receive messages.

Recent conversations shows the title, task state, message count and last activity
for each channel account and session. **Open task** opens the linked desktop
session and its task panel. Rows with only channel records show their status.
Messages without a session have a separate entry.

Disable preserves credentials and history, but cancels owned work and quarantines
pending work rather than replaying it. Logout also clears credentials and disables
the instance, including clearing any retained legacy WeChat credential row. This
is local logout, not a promise of provider-side token revocation. A workspace change preserves history but creates a different session
mapping; old pending work never executes in the new directory. Channel changes do
not restart the ACP daemon, desktop or Buzz adapter.

The provider's authenticated login identity is bound automatically. If absent,
the host displays an expiring `/pair RANDOM_CODE` challenge in the local login
response. The intended user sends it to the bot within three minutes. The first
ordinary message never becomes the owner. New channels accept only their bound
owner; no manual allowlist field exists. Groups are not admitted in v1.

Channel tasks default to session approval `global` and inherit the host's tool
classification, sandbox and budgets on each dispatch, including existing channel
sessions. Global `off` permits ordinary tools; `dangerous` permits built-in reads
(including `skill`, `skill_inspect` and `list_directory`) without approval;
`all` requires approval for every tool. Installed-skill edits still require their
explicit confirmation. Channels never implicitly grant `allow_always`. Native
durable permission waits remain pending for desktop review instead of being
rejected because no interactive channel approver exists. No approval is performed
by a plugin. Legacy WeChat sender/approval policies are preserved until explicit
re-binding. New scheduled jobs default to Full Access only after trusted local
or channel scheduling authorization; legacy schedule grants remain unchanged.

### Channel Full Access

The local `channel.permissions` operation and desktop Channels page can explicitly
authorize `allow_always` (Full Access) for the bound owner/account and workspace.
Login alone does not grant it, and plugin options cannot set it. Ordinary enabled
tools then run without interactive approval; plan/read-only restrictions, disabled
tools, executable policy and budgets still apply. Local commands are not confined
by a filesystem security sandbox. Installed `skill_edit apply` still requires an
exact one-use confirmation. Logout, identity or workspace changes revoke the
grant; same-identity credential refresh and restart preserve it.

Desktop selecting `global` or `read_only` revokes the grant. Chat selecting those
modes lowers the selected policy while retaining the local authorization ceiling,
so `/permission full` may return to that previously authorized mode. Chat cannot
create a Full Access grant. Policy changes are audited and rejected during active
work. Queued messages cannot gain authority from a later upgrade.

## Chat commands and tool feedback

WeChat sends a short `👌 收到` receipt as soon as an ordinary text message passes
owner admission, deduplication and durable inbox storage, including messages queued
behind an active task. This is a separate text message, not a reaction on the
user's message. Host commands and unsupported attachments use their normal reply.
Receipts do not invoke the model or enter its conversation history. They have a
separate durable send key and are attempted at most once: an uncertain receipt
is not retried and does not block the task or its final answer. Receipt batches
have a short timeout so an unavailable provider cannot hold up execution.

Incoming text quotes retain their content, title and original message ID in the
stored model prompt. The user's current text stays separate during command
recognition, so a quoted `/stop` reply still works as a command. Missing quoted
content (including ID-only references), media quotes, partial selections and
overly nested quotes receive an explanation instead of executing an incomplete
instruction. The 16 KiB prompt limit includes quoted content.

Ordinary messages reuse the same session for the same instance, account,
conversation, sender and workspace. Send exactly `/new` (surrounding whitespace
is allowed) to create and switch to a fresh session without calling the model.
The next message uses that new session and the channel's configured approval
policy. Old history and pending executions remain intact; `/new` does not approve,
cancel or replay them. The command goes through normal owner admission, durable
deduplication and workspace fencing. An uncertain outbound send still requires
desktop review; `/new` does not bypass that recovery fence.

Additional host commands (never sent to the model):

| Command | Behavior |
| --- | --- |
| `/help` | Show help |
| `/status` | Policy, session, task state and available confirmation options |
| `/permission full` | Full Access within an existing local grant |
| `/permission global` / `/permission read_only` | Lower channel policy |
| `/resume` | Continue the existing checkpoint without resubmitting the prompt |
| `/approve CODE` / `/deny CODE` | Decide the exact displayed pending call |
| `/stop` | Cancel owned running/waiting work and earlier queued prompts, even if the last run already finished |

Codes expire after ten minutes and bind identity, conversation, workspace, session,
execution/intent/config versions, argument digest and policy revision. Complete
decision sets are consumed durably before resume; crashes never replay them.
Bounded host-redacted installed-skill diffs can be confirmed in chat. Oversized or
unsupported previews require desktop review, not blind approval. Uncertain effects
still require reconciliation, even under Full Access. A downgraded Full Access
task cannot resume with its original broader authority: cancel and start again.

One model execution remains active per channel, while polling and status/stop
commands continue. WeChat polls through a separate plugin process: queued work
cannot starve incoming controls, and a long poll or its retry backoff cannot hold
up a completed answer. Incoming batches and completed runs wake the sending
worker. Active feedback uses a third, disposable plugin process. All processes
are cancelled and joined on disable, logout or update.
Status/stop/help/permission controls can pass an uncertain-delivery fence but
cannot authorize replay of quarantined work.

WeChat defaults to native tool progress: `TOOL_CALL_START` / `TOOL_CALL_RESULT`
items are sent during execution, associated with a run and an opaque tool-call
ID. `tool_start` and `tool_execute` share one start notification; permission waits
send a `blocked` result. A tool with no terminal event when execution returns is
closed as `unknown`, without claiming success. Checkpoint resumes start a fresh
feedback attempt without replaying tools. Labels and statuses are host-authored.
Raw tool arguments, outputs, URLs, commands and errors never enter progress or
summaries; unknown tools have a generic label.

The channel's optional `options.progress_mode` setting accepts `native` (the
default, including an empty setting) or `summary`. Summary mode sends the previous
bounded text summary (up to the last eight calls) with the answer/permission
notice. Native mode falls back to that same summary when the plugin lacks the
feedback capability, fails to start or acknowledge progress, exceeds the bounded
queue (64 items / 256 distinct tool calls per dispatch), or cannot finish sending
within a 200 ms end-of-execution grace period. Successfully acknowledged native
progress is not repeated as a final text summary. Provider acknowledgements do
not establish that every version of the WeChat client rendered the native items;
choose `summary` if the client's display is incompatible.

`options.typing` accepts `true` (default/empty) or `false`, independently of the
progress mode. An actual model run starts the provider's typing indicator and
refreshes it every five seconds. Normal completion, permission waits, failure and
cancellation attempt to clear it. Status/help/new and follow-ups to an already
waiting session do not start typing. Tickets are scoped to the account/recipient
and held only in memory for that dispatch; ticket preparation precedes starting
typing so an uncertain start can still be cancelled using a fresh process.

Feedback RPCs have a two-second budget and run on a separate transport from
polling, receipts and final replies. The event callback only enqueues; optional
failure never cancels model/tool work, revokes credentials or creates a delivery
fence. Progress is attempted at most once and is not replayed after restart.
Typing failures do not disable otherwise working native tool progress. Final
replies wake the sending worker before optional typing cleanup has finished;
cleanup is joined before starting another execution or changing channel identity.

Permission notices identify known pending tools, session and available chat/desktop
review actions. A follow-up while the session is waiting repeats the pending
tool explanation without invoking the model, changing config or granting approval.
Normal model answer text is still delivered as before.
This change does not stream answer text or change its existing length bounds.

## Outbound images

The bundled WeChat plugin (1.2.0+) sends generated images as native image messages.
The host collects committed `image_delta` events, successful image-producing tool
results and image artifacts published by `present_files` during the current turn,
including a resumed permission wait. Repeated references to the same image bytes
are deduplicated within that turn. `view_image`, arbitrary paths in model text and
old session artifacts are not automatically forwarded. A tool that creates a file
should publish it with `present_files` after writing it under `.deerflow/outputs`.

Each reply may contain up to 16 images, each at most 20 MiB, in PNG, JPEG, GIF or
WebP format. These are host limits. Unsupported, unavailable or oversized images
produce a text notice; remaining valid images and the text answer still deliver.
Image-only answers work without a placeholder text response. Scheduled channel
notifications use the same image outbox.

Only durable asset references are stored in `harness_channel_images`, atomically
with the ready reply. At delivery the host rechecks the session, workspace and
snapshot digest, then passes a local immutable snapshot descriptor through stdio.
The plugin checks its size, SHA-256 and actual MIME type before uploading. Image
bytes do not enter the 4 MiB JSON frame. No model-provided URL is fetched.

WeChat upload uses `getuploadurl`, AES-128-ECB with PKCS#7 padding, a CDN POST and
then `sendmessage` with an `image_item`. Bot credentials are never sent to the CDN;
HTTPS Weixin origins are required and redirects are refused. Transient CDN uploads
may be retried up to three times with the same ciphertext. The final message send
is attempted once with a stable key per image. Each image has its own delivery
receipt. A pre-send failure permits a safe text notice; an ambiguous send stops
the reply in `unknown` and requires review, including after restart. Successful
parts are not automatically replayed. Image RPCs have a two-minute overall budget.

Protocol v1 adds optional descriptor `outbound_images` and `send.image` fields.
`send.image` contains `path`, `sha256`, `size` and `mime_type`, and is sent separately
from text. Older plugins receive text plus a notice. Error `-32002` means the plugin
failed **before** attempting a provider image message; it must never describe an
ambiguous message-send outcome. Incoming attachments remain unsupported.

## Local administration

Use the authenticated local management endpoint via `deerflow-control-go`.
Portable installations must select `--runtime-dir .\user-data\runtime\acp-go`.
Do not expose arbitrary management requests to chat users or model tools.

```json
{"operation":"channel.list"}
{"operation":"channel.status","id":"wechat"}
{"operation":"channel.configure","id":"wechat","revision":1,"enabled":true,"workspace":"D:\\Work\\project","options":{}}
{"operation":"channel.permissions","id":"wechat","revision":2,"approval":"allow_always"}
{"operation":"channel.login.start","id":"wechat"}
{"operation":"channel.login.poll","id":"wechat","login_id":"LOGIN_ID"}
{"operation":"channel.login.poll","id":"wechat","login_id":"LOGIN_ID","verify_code":"123456"}
{"operation":"channel.login.cancel","id":"wechat"}
{"operation":"channel.logout","id":"wechat"}
{"operation":"channel.sessions","id":"wechat"}
{"operation":"channel.history","id":"wechat","account":"BOUND_BOT_ID","session_id":"SESSION_ID","before":0}
{"operation":"channel.history","id":"wechat"}
{"operation":"channel.review","id":"wechat","message_id":"UNKNOWN_ID"}
```

Read the current revision before configuration/update. A conflict requires a
refresh; never retry a stale write blindly. Review only acknowledges an uncertain
message and does not rerun tools or resend a reply. Login responses contain QR
PNG base64 and optionally a one-time pairing code, never provider credentials.

Explicit installation executes the selected trusted binary to verify its
descriptor. SHA-256 is a pinned integrity check, not proof of publisher identity.
The bundled executable is discovered only beside the host, never from cwd/PATH.

```json
{
  "operation":"channel.install",
  "id":"my-channel",
  "manifest":{
    "id":"example",
    "command":"D:\\TrustedPlugins\\example\\1.0\\channel.exe",
    "sha256":"64_HEX_CHARACTERS"
  }
}
```

Update with `channel.update`, `id`, current `revision` and a new `manifest`.
Rollback with `channel.rollback`, `id` and current `revision`. Use versioned
directories and retain the old executable: rollback verifies its original hash.
An update probes the new executable before stopping the old instance. Only that
instance is restarted. Missing/incompatible binaries remain errors, not silently
trusted replacements. Installation/update UI is intentionally not included in v1;
the desktop manages already installed instances.

## Protocol and SDK

See `go-harness/channelproto` for the Go SDK. Other languages can implement the
same protocol. Each line is one JSON object, at most 4 MiB. Stdout is exclusively
protocol data; the host discards stderr to prevent credential leakage. Child
processes inherit a minimal OS environment, not model keys or host endpoint tokens.

The host serializes requests per process. Request IDs are positive and responses
must match them. The host kills/reaps a process on cancellation, timeout or invalid
transport. Read-side reconnects reuse the **host-persisted** cursor; sending errors
are never assumed safe to retry. Default call ceiling is 45 seconds, additionally
bounded by the caller's deadline.

The optional descriptor capability `independent_poll` allows a second plugin
process dedicated to polling. It is enabled by WeChat, whose `poll` and `send`
operations use explicit auth/cursor inputs and require no process-local login
state. Other plugins retain the original single-process behavior unless they
advertise this capability. There is only one receiver per instance, and it commits
each batch before requesting the next cursor. Send failures do not restart the
receiver; receive failures do not delay the sender.

Methods:

| Method | Input | Result |
| --- | --- | --- |
| initialize | protocol version | descriptor with ID/version/protocol/capabilities/fields |
| login.start | nonsecret options, transient login fields | login challenge or confirmed auth |
| login.poll | attempt ID, optional verification code | normalized login state |
| login.cancel / logout | no secret input required | acknowledgement |
| poll | current auth, committed cursor, options | normalized messages and next cursor |
| send | auth, destination/context, stable key, text or image, options | `{ "sent": true }` |
| progress | auth, destination/context, unique key, host-authored tool lifecycle | `{ "sent": true }` |
| typing.prepare | auth, destination/context | opaque in-memory typing ticket, or empty when unavailable |
| typing | auth, destination, prepared ticket, active flag | `{ "sent": true }` |

The last three methods are optional protocol-v1 extensions advertised by
`independent_feedback`. Advertising the capability promises that those methods
accept explicit credentials/context and work in a separate process without a
process-local login. The host never probes them on an older plugin. Feedback is
not part of the durable final-answer outbox, and a failed/uncertain optional send
must not be retried as a final answer. `progress_mode` and `typing` are bounded
nonsecret settings exposed in the bundled plugin descriptor.

The plugin itself must validate provider signatures/HTTPS origins and return only
authenticated platform identities. `Auth` is an opaque provider secret plus account
and owner IDs. It crosses only the private host-plugin pipe. On Windows the host
persists it in a user-bound DPAPI envelope. Unix requires a private data directory.
Plugins have no database handle or general host-management capability.

The descriptor's `requires_reply_context` is true for WeChat; other providers can
omit it when proactive sends need no prior conversation context. Provider message
IDs must be unique within an account (prefix chat-local IDs if necessary).
Inbound messages missing required reply context are stored as `ignored` for
deduplication and history. They never invoke the model, attempt a reply or create
an uncertain-delivery fence. Previously queued/ready records are checked again
before dispatch. Existing `unknown` records still require explicit review.

Normalized messages may include `quoted_text` alongside `text`. The host preserves
it in ordinary prompts after command recognition. Plugins must mark incomplete or
unsupported quoted content as `unsupported`, rather than silently omitting it.

Descriptor fields are bounded text inputs. `login:true` marks transient login
fields; `secret:true` additionally masks them and requires `login:true`. Tokens must
be supplied this way, never through persistent `options`. QR and verification-code
flows are implemented; token-login plugins can return confirmed auth directly from
`login.start`. OAuth browser navigation/callback UI, arbitrary HTML, inbound
attachments, general file transfers, group admission and custom plugin UI are not
implemented in v1.

The `poll` contract supports polling, or draining an internal WebSocket/callback
queue. A plugin must not irreversibly advance its provider acknowledgement before
the host supplies the committed cursor on a later poll. Protocol v1 does not
install public webhook listeners, tunnels or firewall rules.

## Persistence and recovery

The host stores channel config, auth and cursors in `harness_channels`; inbox/outbox
records in `harness_channel_messages`; conversation mappings in
`harness_channel_threads`; optional receipt attempts in `harness_channel_receipts`;
outbound image references and delivery states in `harness_channel_images`.
No SQLite/network call runs on the GPUI render thread.
Configuration applies transactionally with a monotonically increasing revision.

Inbound identity is `(instance, account, provider message ID)`. Session identity
also includes conversation, sender and workspace. The host commits admitted
messages and the next cursor together before processing. Limits: 32 instances,
500 messages per poll, 1000 pending messages per instance, 16 KiB text per prompt.
The default worker is serial within each channel; the host run queue controls
cross-channel model concurrency.

WeChat records the run and source message together at admission. A foreground
`task_wait` keeps the reply pending in `waiting_task`. The host checks the
original run after restart and can deliver a result completed by a desktop
owner. The saved checkpoint, current authorization and automatic retry limit
govern continuation. Permission waits still require the corresponding approval.

Before a platform send the message enters `sending`. An interrupted or ambiguous
send becomes `unknown` and fences the conversation until explicit review.
An interrupted dispatch without a durable run binding also requires review.
Scheduled notifications commit in the scheduler transaction:

```json
"notify": {
  "channel":"wechat",
  "account":"BOUND_BOT_ID",
  "conversation":"BOUND_USER_ID",
  "sender":"BOUND_USER_ID"
}
```

Account and sender must match the enabled instance. The old `notify_wechat` field
remains compatible but cannot be combined with `notify`.

## Migration and deployment

The legacy `wechat` YAML is imported once. Existing credentials, cursor, session
mappings and message IDs are copied; legacy tables remain intact. In-flight records
are quarantined, never replayed. Only the new worker runs. Subsequent channel
changes live in the host database and do not rewrite unrelated YAML/model secrets.
Old `wechat.*` management operations remain compatibility aliases.

Build scripts include `deerflow-channel-wechat.exe` on Windows and
`deerflow-channel-wechat` on Linux. A deployment must include the updated desktop,
host binaries and plugin together. Keep a private pre-migration database backup;
do not alternate old/new hosts against migrated live data because the old host does
not understand the new channel records.

Offline tests cover protocol validation, real child-process handshake/timeouts,
identity admission, dedupe, pairing, credential redaction, cancel/disable/logout,
workspace fences, unknown sends, notifications, migration and plugin rollback.
Feedback tests cover native payloads and business-error acknowledgements, typing
start/keepalive/cancel, real child-process failures/timeouts, live delivery before
model completion, bounded queues, fallback/redaction, permission waits/resumes,
and final-answer delivery while typing cleanup is blocked. Image tests cover
runtime-to-stdio delivery, `present_files`, permission resume, files larger than
the stdio frame, byte-exact CDN encryption, upload retries, cancellation, rejected
URLs, text compatibility, atomic queue persistence, and partial-send recovery.
Actual WeChat QR login/send/receive requires a separate live-account acceptance
test. No production account or user database is used by automated tests.
