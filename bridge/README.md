# DeerFlow native ACP bridge

The Rust Bridge keeps the Eino Go `deerflow-acpd` process running between ACP
connections. It forwards stable ACP v1 over stdio and provides the existing
draft v2 facade through `--protocol v2`. The daemon owns sessions, tools, model
execution and persistence; the Bridge owns transport and process startup.

## Local Go daemon

Build `go-harness/cmd/deerflow-acpd-go` as `deerflow-acpd.exe` on Windows or
`deerflow-acpd` on Linux, then put it beside the Bridge. An unpacked Go portable
package already uses this layout. Configure a model as described in
[`docs/go-acp-portable.md`](../docs/go-acp-portable.md).

```powershell
.\deerflow-acp.exe --config C:\DeerFlow\config.yaml --runtime-dir C:\DeerFlow\runtime --start-daemon
.\deerflow-acp.exe --config C:\DeerFlow\config.yaml --runtime-dir C:\DeerFlow\runtime --status
# ACP stdio client command:
.\deerflow-acp.exe --config C:\DeerFlow\config.yaml --runtime-dir C:\DeerFlow\runtime
.\deerflow-acp.exe --runtime-dir C:\DeerFlow\runtime --stop-daemon
```

Startup selection is `--daemon PATH`, then `DEER_FLOW_ACP_DAEMON`, then the
Bridge's sibling daemon file. Explicit relative paths are resolved before
changing to the configuration directory. A missing override does not fall back
to another installation; a missing sibling gives an actionable startup error.
The Bridge does not search PATH, virtual environments or interpreter bundles.
`--python` is removed and returns a migration error; `DEER_FLOW_ACP_PYTHON` no
longer affects startup. A portable root is identified by its config/resources,
not an embedded interpreter. Windows daemon startup uses `CREATE_NO_WINDOW`.

`--manage` forwards one JSON request on stdin through authenticated DFACP/1
MANAGE. Host-approved MCP commands can be passed with repeated absolute
`--mcp-allow-command` paths. Config validation, endpoint ownership and existing
ACP/management payloads are preserved.

### Repository launch scripts

The root `acp-daemon.ps1` and `acp-service.ps1` accept `start`, `stop`, `restart`
and `status`. Set `-BridgePath`, `-DaemonPath`, `-ConfigPath` and `-RuntimeDir`
for a custom development layout. `start.bat` and `stop.bat` forward to these
PowerShell entry points. `stop.ps1` uses authenticated daemon shutdown;
the old `-Port` option returns a migration error.

On Linux, `daemon.sh` provides the same actions with `--bridge`, `--daemon`,
`--config` and `--runtime-dir`; `start.sh` and `stop.sh` are convenience aliases.
All root launchers default to `user-data/runtime/acp-go` for the endpoint and
`config.yaml` for configuration. Build the Go daemon beside the release Bridge
or pass its path explicitly. The scripts do not install dependencies or start
the former HTTP API, and no longer kill processes by port or PID files.

## Existing HTTP transport adapters

The existing `--gateway` (authenticated ACP HTTP + SSE to the local Go daemon)
and `--remote URL` (stdio to an ACP HTTP + SSE endpoint) transport adapters
remain available. The Eino harness migration adds no HTTP application API.

### Start the local gateway

Set one stable, random token in the gateway process environment. The external
token is independent from the random token stored in the daemon's local
`endpoint.json` file.

```powershell
$env:DEER_FLOW_ACP_GATEWAY_TOKEN = '<at-least-32-random-characters>'
deerflow-acp --gateway `
  --config D:\Tools\deerflow-api\config.yaml `
  --workspace D:\ACP\workspace `
  --listen 127.0.0.1:8787
```

Every `POST`, SSE `GET`, and `DELETE` request to `/acp` must include:

```http
Authorization: Bearer <token>
```

The gateway replaces the remote `cwd` in ACP session requests with the fixed
local `--workspace` path. It does not synchronize either filesystem.

### Connect from an ACP stdio client

Configure an ACP client to launch the Bridge in remote mode. The adapter owns
the HTTP/SSE routing and forwards the client's stdio protocol.

```powershell
$env:DEER_FLOW_ACP_GATEWAY_TOKEN = '<the-same-gateway-token>'
deerflow-acp --remote http://gateway-host:8787/acp
```

## Verification

```sh
cargo test --locked
```

The Bridge tests exercise literal MCP paths, daemon selection, missing Go
binaries in legacy layouts/PATH, removed flag diagnostics, MANAGE framing and
the existing transport adapter behavior. Legacy files in these negative tests
are inert fixtures; no interpreter is installed or invoked.

Set `DEERFLOW_TEST_BRIDGE` and `DEERFLOW_TEST_GO_DAEMON` to actual built binaries
and run `go test -run TestExistingRustBridge ./cmd/deerflow-acpd-go` from
`go-harness/` for real Go daemon startup, STATUS, MANAGE, ACP session and STOP
verification. CI runs this on Windows and Linux. This is separate from final
Waku packaging and desktop acceptance.
