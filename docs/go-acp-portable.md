# DeerFlow Go ACP package

This package contains three local executables:

- `deerflow-acp` is the stdio Bridge for ACP clients. It discovers and starts
  the sibling `deerflow-acpd` when needed. Add `.exe` to these names on Windows.
- `deerflow-acpd` owns the Go harness state and serves the Bridge through
  authenticated loopback IPC.
- `deerflow-acp-go` runs the Go ACP agent directly over stdio without a daemon.

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

The Go runtime does not import Python sessions or checkpoints. Existing Python
installations should keep their own binaries and data paths until migration
validation is complete. Package builds are described in
`scripts/build-go-acp-portable.ps1` and `scripts/build-go-acp-portable.sh` in the
source tree.

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
