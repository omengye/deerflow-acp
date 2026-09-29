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
Secrets are accepted through the process environment, never command-line flags.

Configure your editor's ACP command to launch this executable with `--data-dir`
and `--model`. The transport is newline-delimited JSON-RPC over stdio. stdout is
reserved for protocol messages; logs and startup errors go to stderr.

Use a separate data directory from Python. Only one process may own it. Build
`./cmd/deerflow-acpd-go` for shared multi-window access through the existing Rust
Bridge. See [daemon setup and compatibility](internal/localhost/README.md).

## Current execution path

`ACP → session coordinator → durable accepted input → Eino DeepAgent/Runner →
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
Host shell is not enabled. Workspaces are local directories, not OS sandboxes.

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

Use repeatable `--allow-model` to expose additional model IDs in session config;
they use the same configured provider and credentials. Model, subagent and
approval settings persist across restarts. `--disable-subagents` disables the
delegation capability globally.

The current ACP build accepts text prompts. Media persistence, durable
background task scheduling, Skills, memory, compression, Docker, and full
Bridge compatibility are being integrated. Their incomplete
status is not represented as a capability promise. This build is not the V1
completion or default-launcher switch.

## Embed

The root package exposes `Open`, `Client.NewSession`, `Run`, `Cancel`,
`LoadSession`, `CloseSession`, `SetMode`, `ConfigOptions`, `SetConfigOption`,
`ServeACP` and `Close`. New/LoadSession accept optional `harness.MCPServer`
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
