# DeerFlow Go ACP package

Aggregate task token spending is unlimited by default. Set host YAML
`local_acp.max_run_tokens` to a positive value to opt into a shared cost budget;
`0` disables it. Direct Go executables also accept `--max-tokens`. Model/tool call
counts, per-request output caps and execution timeouts remain enforced.
Automatic context compaction follows each agent's selected model window, using
75% after reserving output tokens and a 2% estimate margin. Only models without
a configured window use a 32,000-token fallback. Explicit thresholds still work.
Set `summarization.enabled: false` to opt out. Complete tool output remains in
private snapshots; bounded previews and recent complete exchanges enter model
context. Existing paused tasks retain their original immutable budget policy.

This package contains five local executables:

- `deerflow-acp` is the stdio Bridge for ACP clients. It discovers and starts
  the sibling `deerflow-acpd` when needed. Add `.exe` to these names on Windows.
- `deerflow-acpd` owns the Go harness state and serves the Bridge through
  authenticated loopback IPC.
- `deerflow-acp-go` runs the Go ACP agent directly over stdio without a daemon.
- `deerflow-skill-go` initializes, validates and packages native skill content.
- `deerflow-state-go` backs up, verifies and restores offline runtime state.
  See [backup and restore](go-runtime-backup.md). Restore always uses a new directory.

The Bridge starts the Eino Go daemon from `--daemon PATH`, then
`DEER_FLOW_ACP_DAEMON`, or the sibling `deerflow-acpd` file, in that order.
Explicit relative paths are resolved against the Bridge invocation directory.
A missing selected file fails immediately; there is no PATH or interpreter
fallback. `--python` is rejected with migration guidance, and the obsolete
`DEER_FLOW_ACP_PYTHON` environment variable has no effect.

The Windows x64 package also includes the standalone Linux executable
`deerflow-wsl-helper-linux-amd64`, its `.sha256` checksum and `.json` build
manifest. It is used only by the explicitly selected WSL2 command provider.
The helper requires WSL2 with pidfd, subreaper and `/proc` support; it does not
require Python or Go installed inside the distribution. Normal local command
execution does not require WSL.

Configure a model before starting the Bridge. For an OpenAI-compatible model,
set `DEERFLOW_MODEL`, `DEERFLOW_MODEL_API_KEY`, and optionally
`DEERFLOW_MODEL_BASE_URL`. `DEERFLOW_MODEL_PROVIDER` defaults to `openai`; `claude`
and `ark` are also supported. Set `DEERFLOW_GO_DATA_DIR` to a private directory
dedicated to this Go implementation. Keep it separate from Python DeerFlow
state. The Bridge and daemon must use the same dedicated `--runtime-dir`.

Windows PowerShell example:

```powershell
$env:DEERFLOW_MODEL = "your-model-id"
$env:DEERFLOW_MODEL_API_KEY = "your-api-key"
$env:DEERFLOW_GO_DATA_DIR = Join-Path $env:LOCALAPPDATA "DeerFlow\go-acp-state"
$runtime = Join-Path $env:LOCALAPPDATA "DeerFlow\go-acp-runtime"
.\deerflow-acp.exe --runtime-dir $runtime --start-daemon
.\deerflow-acp.exe --runtime-dir $runtime --status
# Configure the editor's ACP stdio command as:
# .\deerflow-acp.exe --runtime-dir $runtime
.\deerflow-acp.exe --runtime-dir $runtime --stop-daemon
```

Linux example:

```sh
export DEERFLOW_MODEL=your-model-id
export DEERFLOW_MODEL_API_KEY=your-api-key
export DEERFLOW_GO_DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/deerflow/go-acp"
runtime="${XDG_RUNTIME_DIR:-$HOME/.cache}/deerflow-go-acp"
./deerflow-acp --runtime-dir "$runtime" --start-daemon
./deerflow-acp --runtime-dir "$runtime" --status
# Configure the editor's ACP stdio command as:
# ./deerflow-acp --runtime-dir "$runtime"
./deerflow-acp --runtime-dir "$runtime" --stop-daemon
```

The default stdio protocol is stable ACP v1. Add `--protocol v2` to the Bridge
command only for clients that require the draft v2 lifecycle. The direct Go
stdio binary speaks stable v1. Model requests and configured MCP servers may
make outbound network requests; this package does not expose an HTTP API.

Legacy Python sessions and checkpoints cannot be resumed by Go. Supported legacy
configuration and data use explicit Go importers; retain the source data until
its conversion has been checked. Package builds are described in
`scripts/build-go-acp-portable.ps1` and `scripts/build-go-acp-portable.sh` in the
source tree.

## Lifecycle logs

The daemon records startup stages, readiness, retention cleanup errors and
shutdown in JSON Lines. With Waku, files are under `user-data/logs` and the
settings overview displays the active path. Standalone daemons accept
`--log-dir` or `DEERFLOW_GO_LOG_DIR`; without either, logs default to
`<runtime-dir>/logs` (or the Go data directory when no runtime path is given).

Each launch owns `deerflow-acpd-<pid>-<unique>.jsonl`, with two 2 MiB rotations.
Records include known error categories, not raw error text, credentials, model
content or tool payloads. Configuration validation returns detailed field errors
through the config service. Logging failure does not prevent daemon operation;
`daemon.status` reports current log availability.

## Desktop builds and corresponding source

The Windows Waku desktop builder is `scripts/build-deerflow-desktop-go.ps1`.
To create a Release directory and corresponding binary/source ZIPs from a clean
committed checkout, run:

```powershell
.\scripts\build-deerflow-desktop-go.ps1 -Configuration Release -CreateZip
```

`-CreateZip` checks tracked files and non-ignored untracked files before
compiling, pins the current commit, and checks source identity again before
copying artifacts and around archiving. Source changes or a changed commit fail
the package operation. The source archive uses that pinned commit, also recorded
in the binary package's `BUILD_COMMIT.txt`. `-SkipBuild -CreateZip` is rejected
because existing binaries have no verified correspondence to the current source.

For local development, omit `-CreateZip`. Dirty-source builds and builds that
reuse existing binaries receive explicit development or unverified labels in
`BUILD_COMMIT.txt` and `BUILD_SOURCE.json`; those labels must not be interpreted
as an exact source commit for distribution. A source change between the initial
and final checks is also recorded.

Source identity is one release check. Legacy Python source removal and building
and running in an environment without Python, uv or pip are still separate
acceptance requirements. `BUILD_SOURCE.json` currently reports
`python_free_source_acceptance: not_verified`; successfully creating matching
archives does not mark those requirements complete. See the implementation plan
in `docs/eino-subagents-skill-evolution-plan.md` for the complete release gates.

## WSL command helper

The Windows PowerShell builders cross-compile the helper with CGO disabled.
For development, build only the helper from the repository root:

```powershell
.\scripts\build-wsl-helper.ps1 -OutputDirectory .build-cache\wsl-go-helper
```

Select the distribution explicitly with `--sandbox-wsl-distribution`. The Go
daemon and direct ACP entry point accept `--sandbox-wsl-helper` for an absolute
host helper path and `--sandbox-wsl-helper-sha256` for its expected digest.
Without overrides, they use the sibling helper and its checksum file. The
backend verifies the helper at startup and before each command; missing or
incompatible helpers fail with a sandbox error. See
`go-harness/internal/launch/sandbox.md` for provider, command allowlist and
permission flags.

Cancellation and timeout wait for guest process cleanup. Malformed or missing
terminal receipts remain uncertain and require explicit reconciliation.

## Desktop ACP client MCP

The Windows desktop Go backend accepts stdio MCP servers supplied by an ACP
client only when the host config explicitly authorizes their executables. Edit
`user-data/config/config.yaml` in the extracted desktop package:

```yaml
local_acp:
  accept_client_mcp_servers: true
  client_mcp_allowed_commands:
    - 'C:/absolute/path/to/mcp-server.exe'
```

Use an existing absolute executable path for each entry. The desktop config
service validates this list before saving or applying the config; the bridge
passes it to the Go daemon at startup. An ACP client can choose arguments and
per-session environment values for an authorized command but cannot add a new
executable to the host list. The default is disabled. HTTP and SSE client MCP
servers are outside this desktop configuration path.

For sessions created by the desktop app itself, configure `client_mcp_servers`
in DeerFlow settings as a JSON array. The app saves the list in
`user-data/config/client-mcp-servers.json` and sends it with ACP session/new,
session/load, and session/resume. For example:

```json
[
  {
    "name": "workspace",
    "type": "stdio",
    "command": "C:/absolute/path/to/mcp-server.exe",
    "args": ["serve"],
    "env": [{"name": "MCP_TOKEN", "value": "your-token"}]
  }
]
```

The `command` must also appear in `client_mcp_allowed_commands`. Environment
values are redacted in settings snapshots and restored when saving an unchanged
server entry. Empty the list to detach these tools from subsequent desktop ACP
sessions.
