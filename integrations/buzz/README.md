# Buzz DeerFlow Adapter

This standalone sidecar connects Buzz channels and DMs to DeerFlow Portable.
It uses the bridge's ACP v2 facade while DeerFlow's existing Python agent and
persistent daemon continue to own execution and session state.

```text
Buzz Relay
    <-> official buzz CLI (JSON over the relay HTTP bridge)
    <-> buzz-deerflow-adapter
    <-> ACP v2 JSON-RPC over stdio
    <-> deerflow-acp.exe --protocol v2
    <-> v2 facade <-> persistent ACP v1 deerflow-acpd
```

Normal messages, profile metadata, channel names, presence, and status use the
official `buzz` CLI. The CLI owns their Nostr signing, authentication, relay URL
conversion, and ambiguous-delivery classification. Native ACP activity uses a
separate persistent WebSocket because the CLI does not expose NIP-AO observer
publishing. Polling defaults to four seconds.

## Prerequisites

1. Extract and configure DeerFlow Portable. Start it once from
   `deerflow-config.exe` so its model configuration exists.
2. Install the official Buzz CLI and ensure `buzz` is on `PATH`.
3. Create a dedicated Buzz Nostr identity, add its public key to the relevant
   community and channels as a bot/member, and retain its private key securely.
4. If the community uses owner attestation, obtain the four-element NIP-OA auth
   tag as well.

The adapter never accepts the private key in TOML. Before starting it, set the
secret only in the process environment:

```powershell
$env:BUZZ_PRIVATE_KEY = "nsec1..."
# Optional for owner-attested agents:
$env:BUZZ_AUTH_TAG = '["auth","<owner-pubkey>","<conditions>","<signature>"]'
```

Do not commit these values or paste them into logs.

Startup derives the public key from `BUZZ_PRIVATE_KEY` and compares it with
`buzz.agent_pubkey`. A mismatch is rejected before profile, presence, channel,
or message writes, preventing the sidecar from accidentally renaming or posting
as the owner's personal Buzz identity.

On Windows, `start.bat` can instead load a local `.env` file from this
directory. Copy `.env.example` to `.env`, then replace the placeholder:

```dotenv
BUZZ_PRIVATE_KEY=nsec1...
# Optional:
# BUZZ_AUTH_TAG=["auth","<owner-pubkey>","<conditions>","<signature>"]
```

Only those two names are loaded. An already-set process environment variable
takes precedence over `.env`. The real `.env` is ignored by Git; do not add it
with `git add -f` or share its contents.

## Configure

Copy `adapter.example.toml` to `adapter.toml`, then set:

- `buzz.relay_url`, for example `wss://buzz.sprwhisp.cc`;
- `buzz.agent_pubkey` to the public key matching `BUZZ_PRIVATE_KEY`;
- `deerflow.command` to the absolute `deerflow-acp.exe` path;
- `deerflow.protocol = "v2"` and `deerflow.args = ["--protocol", "v2"]`;
- `deerflow.workspace` to an existing local working directory.

The portable build must include the ACP v2 entry point. A legacy executable
that does not recognize `--protocol v2` cannot be used with this configuration.
Set `deerflow.protocol = "v1"` and remove those arguments only as a temporary
fallback; v1 remains supported but is no longer the sidecar default.

Point `deerflow.command` at the same portable directory whose
`deerflow-config.exe` you use. Both programs then discover the same
`user-data/config/config.yaml`, bundled Python, and `user-data/runtime/acp`
endpoint automatically. A daemon already started from that configuration UI is
reused by both ACP v1 clients and this ACP v2 sidecar; no second daemon is
required. Mixing a repository Bridge with another portable directory requires
explicit matching `--config` and `--runtime-dir` arguments and is intended only
for development.

With `auto_discover_channels=true`, the adapter asks Buzz for every channel in
which its identity is a member. `include_dms=true` also discovers direct-message
conversations. Shared-channel events must contain the agent's signed Nostr
`p`-tag by default; DMs do not need a mention. `allowed_pubkeys` can further
restrict who may invoke DeerFlow.

### Display names and lifecycle

`[buzz.profile]` sets the dedicated identity's display name, avatar, bio, and
optional NIP-05 identifier at startup. This changes how the identity appears in
messages and member lists; the Nostr public key remains its internal identity.

Use the explicit `buzz.channel_names` UUID-to-name table to rename regular
channels. The adapter never renames every discovered channel implicitly:

```toml
[buzz]
channel_names = { "11111111-1111-1111-1111-111111111111" = "DeerFlow 工作区" }
```

The identity must have permission to update that channel. DMs do not use this
regular-channel name mechanism.

`[buzz.lifecycle]` publishes `online` only after ACP initialization succeeds,
refreshes presence every 60 seconds, uses the configured ready/working/error
profile status, and publishes `offline` on a graceful exit. Presence represents
the externally launched identity's relay liveness. It cannot change the
Start/Stop value of a local managed-Agent record in Buzz Desktop; that value is
owned by Desktop's process manager.

### Real-time tool activity

DeerFlow ACP v2 `tool_call`, `tool_call_update`, state, and thought updates are
observed without blocking the ACP reader. `[observability]` offers two outputs:

- `enabled=true` enables native Buzz NIP-AO (`kind:24200`) frames. Frames are
  ephemeral, NIP-44 encrypted, and visible only to the registered agent owner.
  This requires either `BUZZ_AUTH_TAG` or `observability.owner_pubkey`, plus an
  agent-owner relationship already known to the relay. Merely setting an owner
  pubkey does not create that relationship.
- `progress_messages=true` is the fallback for a standalone identity without
  owner attestation. It maintains one editable reply per turn containing tool
  names and pending/running/completed/failed status. The reply is visible to
  channel members. Raw inputs and results are never placed in this message.

By default `include_raw_tool_data=false` also removes tool inputs and results
from native observer frames while retaining tool names and lifecycle states.
Set it to true only when the owner explicitly wants encrypted raw tool data in
the activity stream.

`replay_existing=false` is the safe default: a newly discovered channel is
seeded at its newest event, while subsequent cursors are persisted in SQLite.
Set it to `true` only when old mentions should intentionally be processed.
If one poll would exceed `max_poll_pages` (200 events per page), that channel's
cursor is not advanced; raise the limit and restart so older events are not
silently skipped.

### Images and file attachments (ACP v2)

Messages may contain text, images, files, or only attachments. The normal
mention/DM and author allowlist rules still apply. Attach files in Buzz's
composer so the signed message contains NIP-92 `imeta` tags (`url`, `m`, `x`,
`size`, and optional `filename`). Plain URLs in text are not automatically
downloaded as attachments.

The sidecar uses the official `buzz media get` command for authenticated
downloads from the configured relay. Use a Buzz CLI version with that command.
Downloaded size and SHA-256 must match the message metadata. Images also have
their format checked. Failed validation produces an explanatory reply without
running a model turn.

- JPG, PNG, WebP and GIF become native ACP image blocks. The v2 bridge must
  advertise `capabilities.session.prompt.image`, and the current DeerFlow
  session model must have `supports_vision: true` and actually support vision.
- Other files (for example PDF, DOCX, CSV, TXT and ZIP) are saved under
  `<deerflow.workspace>/.buzz-attachments/<session-hash>/<event-hash>/` and sent
  as `file://` resource links. DeerFlow tools can access their real bytes in
  the workspace. This does not automatically convert every file format to text.
- Each message accepts at most 8 attachments totaling 40 MiB; each image is
  limited to 20 MiB and each other file to 25 MiB. DeerFlow's configured
  `local_acp.resource_link_max_size_mb` may impose a smaller file limit.
- Audio and video input are not supported. The legacy v1 sidecar reports that
  attachments require v2 instead of silently sending only the caption.

Attachment tags survive sidecar restarts in SQLite. Verified local files are
reused on retry; modified cache entries are downloaded and checked again.
Files remain available for follow-up turns. After the relevant conversations
are no longer needed, their `.buzz-attachments` directories can be removed
manually. ACP session separation is not a filesystem sandbox: sessions sharing
the same configured workspace can access that workspace.

Upgrade both the native bridge and the portable Python daemon, then restart
the daemon and sidecar to pick up capability negotiation and the larger frame
limit. The sidecar schema migration is automatic. Final replies continue to
deliver text and ACP ResourceLink output as links; this input feature does not
upload generated local files back to Buzz.

## Run

From this directory, reuse the parent project's environment:

```powershell
..\..\.venv\Scripts\python.exe -m buzz_deerflow_adapter --config .\adapter.toml
```

Or install the adapter in its own environment:

```powershell
uv sync
uv run buzz-deerflow-adapter --config .\adapter.toml
```

On Windows, `start.bat` performs the same checks and launch. Stop the foreground
adapter with `Ctrl+C`.

A one-shot discovery and drain is available for connectivity checks:

```powershell
uv run buzz-deerflow-adapter --config .\adapter.toml --once
```

On the first run with replay disabled, `--once` initializes channel cursors and
does not answer historical messages.

## Delivery and session behavior

- The official CLI reads full Nostr events as JSON. The event id is the durable
  inbox key and prevents duplicate ACP turns across overlapping polls.
- Top-level channel mentions open a Buzz thread. With the default
  `session_scope="thread"`, later replies reuse that thread's ACP session.
- DMs always reuse one ACP session per DM conversation.
- DeerFlow's final text and ResourceLink output are captured, saved in SQLite,
  and then sent with `buzz messages send --reply-to <event-id>`.
- Tool progress is best effort and never changes final-reply delivery or inbox
  deduplication. If its first send is ambiguous, edits are disabled for that
  turn while the final reply continues normally.
- ACP v2 prompts acknowledge immediately. The sidecar collects updates only
  after `running` and considers the turn complete on the following `idle`, so a
  session's earlier ready/idle notification cannot prematurely finish a reply.
- Buzz has no interactive ACP approval surface. When DeerFlow requests tool
  permission, the sidecar selects `allow_once` when offered (then
  `allow_always` as a fallback); configure DeerFlow's tool allow/deny policy for
  the trust level of the Buzz users allowed to invoke this identity.
- A failed send retries the already-saved body instead of rerunning DeerFlow.
- If the Buzz CLI reports `delivery_unknown`, the row is quarantined and never
  resent automatically, avoiding duplicate replies. A send-side CLI timeout is
  treated the same way because relay acceptance can no longer be proven either
  way.
- Attachment input is supported on the configured message kinds via `imeta`;
  other Buzz event kinds are not implicitly treated as messages.

## Test

From this directory with the parent development environment available:

```powershell
..\..\.venv\Scripts\python.exe -m pytest -q
```
