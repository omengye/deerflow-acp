# DeerFlow Go harness

This module is the Go/Eino implementation on `codex/eino-harness-acp-refactor`.
It is under active development. The complete V1 scope and acceptance gates are
tracked in [the implementation plan](../docs/eino-subagents-skill-evolution-plan.md)
and [implementation status](../docs/eino-subagents-skill-evolution-status.md).
The latest bounded reliability, diagnostics and delivery work is recorded in
[the three-iteration acceptance record](../docs/eino-three-iterations-20261004.md).

The portable configuration now supports 13 migrated native Go tools and uses
`models[].provider`, `sandbox.provider` and tool names without Python `use`
class paths. See [native tool configuration and limits](../docs/go-native-tools.md).

## Build and start

Use `GOTOOLCHAIN=go1.26.8` to select the tested toolchain explicitly, including
when the system Go version is newer. The module requires Go 1.26 because the
pinned pure Go SQLite driver requires it. No system Go installation or global
Go environment setting needs to be changed.

```powershell
cd go-harness
$env:GOTOOLCHAIN = "go1.26.8"
go build -p=2 -o bin/deerflow-acp-go.exe ./cmd/deerflow-acp-go
$env:DEERFLOW_MODEL = "your-model-id"
$env:DEERFLOW_MODEL_API_KEY = "your-api-key"
# Optional for an OpenAI-compatible provider:
$env:DEERFLOW_MODEL_BASE_URL = "https://your-provider.example/v1"
./bin/deerflow-acp-go.exe --data-dir C:/path/to/separate-go-state
```

On Linux, build the same command without `.exe`. Supported provider adapters are
`openai` and `claude` (`--provider`, or `DEERFLOW_MODEL_PROVIDER`). Provider
keys may also come from `OPENAI_API_KEY` or `ANTHROPIC_API_KEY`.

All model requests use streaming transport, including memory extraction,
summaries, connection probes and skill maintenance. Synchronous `Generate`
calls collect a stream rather than requesting non-streaming completions; there
is no non-streaming fallback. See the [transport audit](../docs/model-streaming-audit-20261010.md).
Model credentials are accepted through the process environment, never command-line flags.

Configure your editor's ACP command to launch this executable with `--data-dir`
and `--model`. The transport is newline-delimited JSON-RPC over stdio. stdout is
reserved for protocol messages; logs and startup errors go to stderr.

Use a separate data directory from Python. Only one process may own it. Build
`./cmd/deerflow-acpd-go` for shared multi-window access through the existing Rust
Bridge. See [daemon setup and compatibility](internal/localhost/README.md).

## Skill evolution

Native launch enables automatic skill evolution when skills are enabled and
inherits the primary model. There is no `skill_evolution` configuration block,
manual review setting or daily maintenance quota.

`skill_manage` submits exact candidate packages to the durable maintenance
queue. The host independently checks safety, quality, behavior, improvement,
triggering and holdout coverage, then automatically admits eligible candidates
into scoped observation. Real task outcomes determine stabilization or
withdrawal. Scripts require actual isolated execution. Missing evidence or
runtime capability waits for those conditions. An explicit native user
confirmation can publish an exact candidate through `skill_manage.publish`,
with source permissions, ownership, content identity and version checks still
enforced. Command execution permissions remain independent.

Learned patterns and agent-authored packages share the same validation and
adoption pipeline. Root/session/role ownership, authorized source identities,
workspace visibility, exact base/content bindings and finite retries are checked
at submission and adoption. Retired jobs remain history and do not replay.

See the [candidate and management contract](internal/acp/skill-evolution.md),
[discovery contract](internal/skills/evolution/discovery.md),
[behavior evaluation](internal/skills/evolution/behavior.md) and
[migration and acceptance record](../docs/skill-evolution-final-plan-20261109.md).

## Current execution path

Native file writes now require the version returned by `read_file` before
overwriting existing content. A bounded no-progress guard stops repeated identical
successful observations as incomplete; ordinary tool failures are left to agent
recovery without a fixed retry count. The lead agent has session-local `task_note`,
`history_search` and `history_read` tools. These add no Goal mode or auxiliary
model requests. See [contracts and limits](../docs/harness-reliability-20261012.md),
including the compatibility boundary for older paused executions.

`ACP → session coordinator → durable accepted input → Eino TurnLoop/DeepAgent →
permission-protected tools → session/checkpoint stores → ACP updates`.

The executable supports initialize, new, list, load, resume, close, delete, prompt,
cancel, session config options, and default/plan modes. Load replays history before its response; resume
restores without replay. A second active prompt is rejected before the first
can be canceled. Cancellation retains session ownership until cleanup and final
persistence finish.

Both executables run a session retention sweep at startup and every hour by
default. Detached sessions explicitly closed with `session/close` are archived
after 30 days; other detached sessions are archived 30 days after their last activity.
`--closed-session-retention-days`, `--inactive-session-retention-days`, and
`--session-cleanup-interval` set those limits; `--session-cleanup-enabled=false`
disables automatic archival and cache maintenance. Zero closed days permits
archival at the next sweep. Archival preserves conversation history, attachments,
artifacts, execution evidence and budgets; only regenerable artifact digest
caches are evicted. Desktop host configuration defaults to five days.
Attached or running sessions, nonterminal background task graphs, unhandled
background notifications, active execution attempts, unsettled budget work,
pending/started receipts and storage faults still prevent archival. An `uncertain`
receipt belonging to a terminal run does not require human review before archival
or explicit deletion; the session and its task graph must still satisfy the
terminal-state, activity and accounting checks. Archival retains that evidence.
A successful `session/load` reopens a
closed or archived session and restarts its inactivity clock without executing
a model or replaying tools. Embedded SDK clients leave
retention disabled unless `Config.Retention` is explicitly configured; hosts
can call `Client.ArchiveExpiredSessions` for a single sweep. The legacy
`Client.CleanupExpiredSessions` name remains an archival alias. Explicit
`session/delete` remains a permanent, guarded deletion operation. Branch and
rewind keep their stricter history checks: pending, started or uncertain receipts
can still block those operations even after ordinary conversation can continue.

Native workspace tools provide read, list, search, write, and edit. Plan and
read_only modes expose trusted local read tools without prompts. Other modes
follow the configured ask/allow_always/reject_always approval policy. An
`allow_always` decision applies only to identical tool arguments in that session
and connection. `os.Root` constrains file operations including symlink traversal.
Command execution is disabled by default. Explicit command providers include
local argv, PowerShell, WSL2 and Docker; both executables share
`--sandbox-provider`, allowlists and resource limits. Local shells and WSL2 have
host capabilities; they are not security isolation. See
[command lifecycle and providers](internal/sandbox/README.md) and
[shared CLI flags](internal/launch/sandbox.md).

Every external tool has a durable receipt. Its `started` transition commits before
the tool runs. Ordinary tool failures return available output and explicit outcome
uncertainty to the agent, which chooses whether to investigate, retry, change
approach or clearly end the turn. Recovery has no fixed retry count; existing
budgets, permissions, user cancellation and storage/accounting protections still
apply. If information or confirmation is needed, the agent asks in ordinary chat
and ends the turn. The user's reply starts a new run in the same conversation.
Old calls and unsafe checkpoints are never automatically replayed.

Desktop execution records are read only, with error details and copying. Unknown
receipts retain their original evidence and are not relabeled as success or no
effect. ACP advertises only `_deerflow/tool_receipts/list`; the former reconcile
RPC is retired and returns MethodNotFound. The SDK's historical reconciliation
method remains for compatibility but is not required by desktop conversation.
See [read-only receipts and agent recovery](internal/acp/receipts.md).

`Client.HistoryPage` and ACP `_deerflow/history/list` provide bounded event pages
with a stable snapshot cursor. Standard `session/load` still replays the complete
conversation and now reads it in pages. See [history pagination](internal/acp/history.md).

ACP client MCP servers are scoped to the attached session. Use repeatable
`--mcp-allow-command` with an absolute executable to allow client stdio servers.
Outbound MCP HTTP/SSE need `--mcp-allow-http` / `--mcp-allow-sse`. These flags do
not create an HTTP server. See [MCP lifecycle and policy](internal/mcp/README.md).

The SDK and both executables default to unlimited model/tool calls, unlimited
aggregate tokens, 4,096 output tokens per request, and no total execution time
limit. Native child roles and foreground queue waits also default to unlimited
time; healthy multi-day tasks are not stopped by a default run deadline.
Tool calls, connection setup, worker leases and cleanup retain their separate
timeouts and fencing. Model calls remain cancellable; per-request transport
timeouts depend on the provider. The corresponding flags are `--max-model-calls`,
`--max-tool-calls`, `--max-tokens`, `--max-output-tokens`, and `--run-timeout`;
zero disables an individual limit. Token usage is estimated until reported by
the provider, so these limits cannot guarantee exact billing. Provider-internal
transport retries are not counted as separate logical calls.

Aggregate token spending is an optional cost policy, separate from a model's
context window: replayed history is charged again on every model call. Opt in
with `--max-tokens` or host YAML `local_acp.max_run_tokens` (default `0`, disabled);
an explicit CLI flag takes precedence. Existing durable tasks keep their original
budget policy; start a new request to use a changed policy.

`local_acp.max_output_tokens` is the single host per-response allowance (default
4096); `--max-output-tokens` overrides it. It is distinct from the aggregate
`max_run_tokens` spending limit. Reasoning can consume this allowance too.
Each model route accepts `output_token_parameter: auto|max_tokens|max_completion_tokens`.
`auto` uses `max_tokens` for Claude and the verified `copilot.tencent.com`
OpenAI-compatible endpoint, and `max_completion_tokens` for other OpenAI-compatible
chat routes. An explicit route setting overrides this compatibility rule;
`--output-token-parameter` configures the default route. Only one wire field
is sent after the host, role, caller and context-window caps have been applied.
Thinking settings do not switch that field. `auto` selects a wire parameter,
not a guessed model output size. OpenAI Responses is not implemented here.

A provider `length` / Claude `max_tokens` finish now triggers native automatic
continuation rather than immediately ending the task. Already streamed text is
preserved; the next request rechecks compaction and makes a fresh shared-budget
reservation. Entire truncated tool-call groups are discarded, even if one call
has valid JSON; only newly generated complete calls can execute. Root agents,
configured roles and prepared background agents share this behavior. Continued
final answers are consolidated for history, role results and post-run consumers
without emitting duplicate UI text. Recovery permits four additional requests
per interrupted response and stops earlier on repeated empty/repeated output,
cancellation, explicit spending limits or an error. Exhaustion reports an
actionable incomplete-output notice, never successful completion. A normal
`stop`, refusal or transport failure is not guessed into a continuation.

Both executables and bundled YAML enable automatic context compaction by default.
Each agent uses about 75% of its selected model's `context_window`, after reserving
the output cap and a 2% estimate margin. Large-window models therefore compact
later; a 32,000-token fallback applies only when the window is unknown. Explicit
token/message thresholds remain supported, with token thresholds clamped for
smaller windows. Eight recent messages and complete tool call/result groups are
retained. `summarization.enabled: false` or `--context-compaction=false` opts out.
Summaries preserve the active user request even during a long first turn; earlier
exchanges can be summarized without keeping the whole turn in model context.
Full tool results remain in receipts and private snapshots; model previews are
bounded to 8 KiB per result and 16 KiB per run, with `read_tool_output` for details.
SDK embedders enable compaction with `Config.Compaction.Enabled`.

Hidden summaries use a concise factual handoff, not the main turn's reasoning
effort or role temperature. A declared thinking switch is disabled only for the
summary; models without a declared switch receive no guessed provider option.
Summary requests remain bounded by the shared budget, role output cap and a
4,096-token ceiling. A truncated, empty or oversized summary is regenerated
once with a shorter instruction, with a fresh reservation in the same budget.
Provider errors, invalid output structure and cancellation are not retried.
Neither rejected summary is inserted into history, and the 16 KiB acceptance
limit remains enforced. Exhausted validation returns ACP `context_compaction`
with safe reason/finish/size/attempt metadata, not summary text. Completed tool
effects are not replayed. A failed old checkpoint may be unresumable even when
all receipts are settled. A new message lets the agent assess the existing
evidence in a new run; it does not force checkpoint replay or ask the user to
certify tool outcomes. The desktop persists terminal errors
even after partial assistant output.

Both executables allow two concurrent foreground runs across sessions by default.
`--max-active-runs` changes this limit; `--queue-timeout` defaults to `0`, with
no queue deadline. An explicit positive value enables one, including multi-day
waits. Waiting for a slot does not consume the
run's execution timeout or persist a new input. A queue timeout returns an ACP
server-busy error, and cancellation removes the waiting run. Background child
attempts have a separate `--background-workers` limit.

Set `--context-window <tokens>` for the default model when its context size is
known. ACP then reports `usage_update` from the last main-agent model call's
reported input and output tokens. The SDK accepts `Config.ContextWindow` and
per-model `Config.ContextWindows`; the Bridge config maps model
`context_window`. Without a configured size or provider usage, ACP omits this
context indicator. It is separate from aggregate run token usage.
Configured windows now constrain compaction thresholds and preflight estimates
for all model roles. Chinese text, tool schemas and images share the same estimate
used for admission. These estimates remain approximate; provider usage is
authoritative. Changed windows/estimator policy invalidate incompatible paused
execution contracts; cancel the old execution and start a new turn.

Desktop settings offer an explicit two-request Agent capability probe and cached
session diagnostics, including slot waits, pause reasons, root budget counters
and bounded model/tool timing metadata. Diagnostics omit conversation content
and credentials. Runtime backup/restore is no longer part of the MVP. Session
persistence, execution recovery and skill version rollback remain supported.

Use repeatable `--allow-model` to expose additional model IDs in session config;
they use the same configured provider and credentials. Model, subagent and
approval settings persist across restarts. `--disable-subagents` disables the
delegation capability globally.

The SDK supports explicitly installed, scoped Skills through Eino's native skill
middleware. Configure `Config.Skills.Sources`, call `InstallSkill`, inspect its
findings, then call `SetSkillEnabled`. New installations are disabled. Workspace
selection follows the session, global sources require
`Config.SkillSelection.IncludeGlobal`, and no HOME paths are scanned.
`ListSkills` and `DeleteSkill` provide management. Bodies and reference files load
on demand from immutable database versions. See [Skills](internal/skills/README.md).
Both executables accept `--skills-config <file>` for explicit source, installation
and selection configuration; see [the startup profile](internal/launch/skills.md).
Each listed installation commits independently. New SDK installations remain
disabled until enabled; a startup profile can explicitly enable a reviewed package.

Internal execution checkpoints pin Skills versions, resource policy and durable
budget identity. The SQL ledger records reservations and actual/estimated usage
independently of checkpoint success. Failed retries cannot reset spent quota;
unknown interrupted work retains its holds. Production checkpoint envelopes use
version 2, containing native Eino state and the ledger identity; older local
counter checkpoints cannot authorize production resume. ACP `session/resume` reattaches a
conversation; it does not resume suspended tool execution.

Protected tools use native Eino interrupt/resume with persisted permission intent.
The prompt asks for approval after a waiting checkpoint commits and the active
budget attempt ends. Online answers continue that same run. A cancelled permission
response or disconnected approval channel preserves the wait. The SDK provides
`Execution`, `ResumeExecution`, and `CancelExecution`; ACP advertises
`_deerflow/executions/get`, `/resume`, and `/cancel`. Reconnection requires fresh
permission and exact run/version matching. See [execution recovery](internal/acp/executions.md).
Eino's built-in `task` orchestration emits subagent lifecycle events; the child
model and external tools share the original budget and individual tool receipts.
Eino's built-in `write_todos` provides structured lead-agent plans; successful
updates become ACP `plan` updates and replay from durable history. Child-agent
todo lists do not replace the lead agent's plan.
`Config.ToolPolicy` is a host-owned exact-name allow/deny boundary. A nil
allowlist leaves tools available unless denied; an empty non-nil allowlist
disables all tools. It also controls Eino's native `task` and `write_todos` and
the Skills `skill` tool. Session config cannot restore a host-denied tool, and
execution checkpoints pin the effective policy. Bridge `--config` maps
`local_acp.tool_allowlist`, `tool_denylist` and the native permission modes
`off`, `dangerous` and `all` through the same Go configuration contract.
The default Eino backend also exposes durable `background_agent`, `task_status`,
`task_wait` and `task_cancel` tools. Isolated child agents inherit the workspace,
policy and existing budget; only explicit text instructions are copied. Both
launchers accept `--background-workers` (default 4, maximum 64). Saved child
approvals survive disconnect/restart and can be resolved through the SDK or ACP.
Client-owned MCP configurations currently disable background submission. See
[background task management](internal/acp/background.md).

`ProcessBackgroundNotification` delivers a saved notification to the parent
model as a separate durable run/input, charging the originating budget. ACP
advertises `_deerflow/notifications/process` when this capability is available.
Processing is explicit and independent from UI acknowledgement. Repeated calls
return the existing execution; tools needing approval use the same durable
execution recovery methods. With no SDK permission handler, an ask-policy tool
remains waiting for `ResumeExecution`.

ACP accepts text and file references. Images require an explicit
`--vision-model <model-id>` (repeatable), and that model must also be selectable.
The corresponding SDK policy is `Config.Media.VisionModels`. Up to eight images
may be supplied per prompt, at most 20 MiB each and 40 MiB in total. Local file
references must stay within the session workspace; ordinary files are capped at
25 MiB. HTTP(S) links are metadata references and are not downloaded.

Images and attachments are stored as immutable, session-scoped snapshots. Model
calls hydrate image bytes privately; SQLite history/checkpoints contain stable
references. `view_image` lets vision models inspect workspace images, including
in read_only mode. `present_files` snapshots completed files from
`.deerflow/outputs` and commits their references with the tool receipt before
presenting them to ACP. `Client.ListArtifacts` and `ResolveAsset` provide SDK
access; ACP exposes `_deerflow/artifacts/list`. See
[media projection and limits](internal/engine/eino/media.md) and
[ACP media behavior](internal/acp/media.md).

Each output artifact defaults to at most 500 MiB. Set
`local_acp.max_artifact_size_mib: 500` in `config.yaml` to change the limit
(positive integer in MiB; omitted/null uses 500). Restart the backend after
editing it. This host-only setting has no desktop editor and governs snapshot
creation, resolution and chunked downloads. Lowering it also prevents reading
existing artifacts larger than the new limit. Input attachment limits are
unchanged. SDK callers can set `Config.MaxArtifactBytes` (zero uses the default).

MCP image blocks and enhanced tool images use the staged asset importer.
Model-generated media, automatic parent notification scheduling, full draft v2
coverage and external ACP recovery remain open. This build is not the V1
completion or default-launcher switch. Isolated Windows/Linux ACP package
scripts and their startup instructions are in
[the portable Go ACP guide](../docs/go-acp-portable.md).

## Embed

The root package exposes `Open`, `Client.NewSession`, `Run`, `Cancel`,
`LoadSession`, `CloseSession`, `SetMode`, `ConfigOptions`, `SetConfigOption`,
`ServeACP`, receipt/Skills/asset management methods, and `Close`.
New/LoadSession accept optional `harness.MCPServer`
arguments. Public event and permission
types live in `harness/` and do not expose Eino or ACP SDK types. An embedder can
provide `Config.Engine` to supply its own execution backend. Call `Close` after
all work; it cancels attached runs, waits for cleanup and releases the database
ownership lock.

Operator skill evidence is available through `SubmitSkillFeedback` for actual
observed reads and `SubmitMissingSkillReport` for a capability missing from a
real task, including a task with no skill reads. Matching Get/List methods
provide current records and bounded histories. These SDK operations share the
authenticated local management service, preserve task outcomes and do not run
generation or publish skills. See the
[skill evidence management contract](internal/acp/skill-evolution.md).

## Verify

```powershell
go test -count=1 -p=2 ./...
go test -race -p=2 ./internal/session ./internal/runtime ./internal/acp/...
```

The executable integration test uses a local OpenAI-compatible fixture server;
it needs no provider key or internet access once dependencies are downloaded.
Use `-count=1` to rerun it after executable changes, since it builds a subprocess.
It builds and launches the real executable, approves an actual file write, then
restarts the process and checks both ACP history replay and native Eino history.
`go test -short ./...` skips this subprocess test.

Do not upgrade all Eino modules with `@latest`. The alpha core and stable model
extensions have divergent APIs. Dependency changes require the native engine,
checkpoint, store conformance and executable integration tests.
