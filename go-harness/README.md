# DeerFlow Go harness

This module is the Go/Eino implementation on `codex/eino-harness-acp-refactor`.
It is under active development. The complete V1 scope and acceptance gates are
tracked in [the implementation plan](../docs/eino-harness-v1-implementation-plan.md)
and [implementation status](../docs/eino-harness-implementation-status.md).

## Build and start

Go automatically selects the pinned `go1.26.8` toolchain. The module requires
Go 1.26 because the pinned pure Go SQLite driver requires it. No system Go
installation or global Go environment setting needs to be changed.

```powershell
cd go-harness
go build -p=2 -o bin/deerflow-acp-go.exe ./cmd/deerflow-acp-go
$env:DEERFLOW_MODEL = "your-model-id"
$env:DEERFLOW_MODEL_API_KEY = "your-api-key"
# Optional for an OpenAI-compatible provider:
$env:DEERFLOW_MODEL_BASE_URL = "https://your-provider.example/v1"
./bin/deerflow-acp-go.exe --data-dir C:/path/to/separate-go-state
```

On Linux, build the same command without `.exe`. Supported provider adapters are
`openai`, `claude`, and `ark` (`--provider`, or `DEERFLOW_MODEL_PROVIDER`). Provider
keys may also come from `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, or `ARK_API_KEY`.
Model credentials are accepted through the process environment, never command-line flags.

Configure your editor's ACP command to launch this executable with `--data-dir`
and `--model`. The transport is newline-delimited JSON-RPC over stdio. stdout is
reserved for protocol messages; logs and startup errors go to stderr.

Use a separate data directory from Python. Only one process may own it. Build
`./cmd/deerflow-acpd-go` for shared multi-window access through the existing Rust
Bridge. See [daemon setup and compatibility](internal/localhost/README.md).

## Current execution path

`ACP → session coordinator → durable accepted input → Eino TurnLoop/DeepAgent →
permission-protected tools → session/checkpoint stores → ACP updates`.

The executable supports initialize, new, list, load, resume, close, prompt,
cancel, session config options, and default/plan modes. Load replays history before its response; resume
restores without replay. A second active prompt is rejected before the first
can be canceled. Cancellation retains session ownership until cleanup and final
persistence finish.

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

Every external tool has a durable receipt. Its `started` transition commits before the
tool runs. Unknown outcomes block another run until the owner explicitly reviews
the receipt. Review never executes a tool. The SDK exposes `ListToolReceipts` and
`ReconcileToolReceipt`; ACP advertises the corresponding namespaced extensions.
See [receipt queries and reconciliation](internal/acp/receipts.md).

`Client.HistoryPage` and ACP `_deerflow/history/list` provide bounded event pages
with a stable snapshot cursor. Standard `session/load` still replays the complete
conversation and now reads it in pages. See [history pagination](internal/acp/history.md).

ACP client MCP servers are scoped to the attached session. Use repeatable
`--mcp-allow-command` with an absolute executable to allow client stdio servers.
Outbound MCP HTTP/SSE need `--mcp-allow-http` / `--mcp-allow-sse`. These flags do
not create an HTTP server. See [MCP lifecycle and policy](internal/mcp/README.md).

The SDK and both executables default to 100 model calls, 200 tool calls,
200,000 aggregate tokens, 4,096 output tokens per request, and 30 minutes per run
shared with child agents. The corresponding flags are `--max-model-calls`,
`--max-tool-calls`, `--max-tokens`, `--max-output-tokens`, and `--run-timeout`;
zero disables an individual limit. Token usage is estimated until reported by
the provider, so these limits cannot guarantee exact billing. Provider-internal
transport retries are not counted as separate logical calls.

Set `--context-window <tokens>` for the default model when its context size is
known. ACP then reports `usage_update` from the last main-agent model call's
reported input and output tokens. The SDK accepts `Config.ContextWindow` and
per-model `Config.ContextWindows`; the Bridge config maps model
`context_window`. Without a configured size or provider usage, ACP omits this
context indicator. It is separate from aggregate run token usage.

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

Raw image outputs from MCP and model-generated media still need importer wiring.
Automatic parent notification scheduling, memory, compression, and full
Bridge compatibility are being integrated. Their incomplete
status is not represented as a capability promise. This build is not the V1
completion or default-launcher switch.

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
