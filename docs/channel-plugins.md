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
| `/stop` | Cancel owned running/waiting work; an active stop discards queued prompts |

Codes expire after ten minutes and bind identity, conversation, workspace, session,
execution/intent/config versions, argument digest and policy revision. Complete
decision sets are consumed durably before resume; crashes never replay them.
Bounded host-redacted installed-skill diffs can be confirmed in chat. Oversized or
unsupported previews require desktop review, not blind approval. Uncertain effects
still require reconciliation, even under Full Access. A downgraded Full Access
task cannot resume with its original broader authority: cancel and start again.

One model execution remains active per channel, while polling and status/stop
commands continue. Plugin transport remains serialized; provider polling latency
still applies. Status/stop/help/permission controls can pass an uncertain-delivery
fence but cannot authorize replay of quarantined work.

Replies include a bounded tool-call summary (up to the last eight calls), sent
together with the answer or permission notice, not as a live stream. Labels and
statuses are host-authored. Raw tool arguments, outputs, URLs, commands and errors
are never copied into these summaries; unknown tools have a generic label.
Permission notices identify known pending tools, session and available chat/desktop
review actions. A follow-up while the session is waiting repeats the pending
tool explanation without invoking the model, changing config or granting approval.
Normal model answer text is still delivered as before.

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

Methods:

| Method | Input | Result |
| --- | --- | --- |
| initialize | protocol version | descriptor with ID/version/protocol/capabilities/fields |
| login.start | nonsecret options, transient login fields | login challenge or confirmed auth |
| login.poll | attempt ID, optional verification code | normalized login state |
| login.cancel / logout | no secret input required | acknowledgement |
| poll | current auth, committed cursor, options | normalized messages and next cursor |
| send | auth, destination/context, stable key, reply, options | `{ "sent": true }` |

The plugin itself must validate provider signatures/HTTPS origins and return only
authenticated platform identities. `Auth` is an opaque provider secret plus account
and owner IDs. It crosses only the private host-plugin pipe. On Windows the host
persists it in a user-bound DPAPI envelope. Unix requires a private data directory.
Plugins have no database handle or general host-management capability.

The descriptor's `requires_reply_context` is true for WeChat; other providers can
omit it when proactive sends need no prior conversation context. Provider message
IDs must be unique within an account (prefix chat-local IDs if necessary).

Descriptor fields are bounded text inputs. `login:true` marks transient login
fields; `secret:true` additionally masks them and requires `login:true`. Tokens must
be supplied this way, never through persistent `options`. QR and verification-code
flows are implemented; token-login plugins can return confirmed auth directly from
`login.start`. OAuth browser navigation/callback UI, arbitrary HTML, attachment
transfers, group admission and custom plugin UI are not implemented in v1.

The `poll` contract supports polling, or draining an internal WebSocket/callback
queue. A plugin must not irreversibly advance its provider acknowledgement before
the host supplies the committed cursor on a later poll. Protocol v1 does not
install public webhook listeners, tunnels or firewall rules.

## Persistence and recovery

The host stores channel config, auth and cursors in `harness_channels`; inbox/outbox
records in `harness_channel_messages`; conversation mappings in
`harness_channel_threads`. No SQLite/network call runs on the GPUI render thread.
Configuration applies transactionally with a monotonically increasing revision.

Inbound identity is `(instance, account, provider message ID)`. Session identity
also includes conversation, sender and workspace. The host commits admitted
messages and the next cursor together before processing. Limits: 32 instances,
500 messages per poll, 1000 pending messages per instance, 16 KiB text per prompt.
The default worker is serial within each channel; the host run queue controls
cross-channel model concurrency.

Before a model call, the record becomes `running`. Before a platform send, it becomes
`sending`. Crashes, ambiguous sends and cancellation become `unknown` and block that
conversation until explicit review. Exactly-once remote delivery cannot be guaranteed
across providers. Scheduled notifications commit in the scheduler transaction:

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
Actual WeChat QR login/send/receive requires a separate live-account acceptance
test. No production account or user database is used by automated tests.
