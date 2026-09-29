# Go local daemon transport

This package implements the Rust Bridge's existing `DFACP/1` authenticated
loopback TCP protocol. It does not start an HTTP server. `endpoint.json` retains
the six existing fields: `host`, `port`, `token`, `pid`, `build_id`, `config_path`.
The token is random (256 bits), compared in constant time, and never logged.

## Launch through the existing Bridge

Build `./cmd/deerflow-acpd-go`, then set `DEERFLOW_MODEL`,
`DEERFLOW_MODEL_API_KEY`, and optionally `DEERFLOW_MODEL_PROVIDER` (default
`openai`), `DEERFLOW_MODEL_BASE_URL`, and `DEERFLOW_GO_DATA_DIR`. Invoke the
existing Bridge with:

```text
deerflow-acp --daemon /absolute/path/deerflow-acpd-go --runtime-dir /private/go-runtime
```

On Windows use the `.exe` filenames. Always choose a Go-specific runtime
directory. Existing Python/Rust launch defaults are unchanged. The Go daemon's
own default is `%LOCALAPPDATA%/DeerFlow/go-acp` on Windows, or
`$XDG_RUNTIME_DIR/deerflow-go-acp` / the user cache `deerflow-go/acp` on Unix.
`DEERFLOW_GO_RUNTIME_DIR` overrides this default.

The daemon also accepts the shared runtime flags for budgets, model selection,
subagents and MCP allowlists (`--max-model-calls`, `--max-tool-calls`,
`--max-tokens`, `--max-output-tokens`, `--run-timeout`, `--allow-model`,
`--disable-subagents`, `--mcp-allow-command`, `--mcp-allow-http`,
`--mcp-allow-sse`). To customize flags that the Bridge does not forward, start
the daemon directly with those flags and connect the Bridge using
`--no-auto-start --runtime-dir /private/go-runtime`.

The Go daemon accepts the Bridge's `--config` for the documented bounded YAML
mapping; see [configuration compatibility](../../../docs/eino-go-config-bridge.md).
`MANAGE` implements daemon status/drain/resume, session list/delete, and memory
get/delete. Unsupported operations return
`{"ok":false,"code":"unsupported_operation",...}`.

`session.delete` requires a detached foreground session. It refuses active or
uncertain background task graphs and unresolved tool receipts. Terminal task
graphs, their child sessions and private records are removed in one database
transaction, then internal asset snapshots are reclaimed. Workspace
files and workspace/user memory remain. The operation is idempotent so a client
can retry after a lost response. `cleanup_eligible` becomes true when the
configured retention age is reached, the session is detached, and no active
task, unhandled background notification or unresolved tool receipt blocks
deletion. This inventory field is a
point-in-time hint; the sweep repeats checks before deleting.

Automatic cleanup is enabled by default in both Go executables. It runs once
at startup and then every hour, retaining closed and inactive sessions for 30
days by default. `--session-cleanup-enabled=false` disables it; the two
`--*-session-retention-days` flags and `--session-cleanup-interval` adjust the
policy. The daemon also reads the corresponding `local_acp` YAML fields when
launched with `--config`; explicit Go flags take precedence.

## Lifetime and capacity

`Host.Start` binds only `127.0.0.1`, acquires `daemon.lock`, and atomically
publishes the endpoint. A separate `deerflow.Open` lock owns the data directory,
so changing only the endpoint directory cannot create a second database writer.
The runtime directory is protected with mode `0700` and its files with `0600`
on Unix. Windows uses protected DACLs restricted to the current user and
LocalSystem; permissive inherited ACLs are removed. Use a directory dedicated
to this daemon, because the runtime directory's permissions are tightened.

Handshake lines are bounded to 4096 bytes and 3 seconds. `MANAGE` requests are
bounded to 64 KiB and 10 seconds. After an accepted `ACP` handshake, the deadline
is cleared and pipelined ACP bytes are retained. `--max-connections` defaults to
32 and limits ACP streams independently from model execution capacity.
Authenticated `STATUS` and `STOP` bypass that limit. `STATUS` reports accepted
ACP streams as `connections=N`, including idle streams.

`STOP` acknowledges, cancels the host, closes sockets, joins all ACP handlers,
then calls `Config.Cleanup` (the CLI uses `Client.Close`). Only after cleanup
finishes does it remove its own endpoint and release the runtime lock. A
successor endpoint with a different PID or token is preserved. The existing
Bridge force-stops a daemon after its default 10-second shutdown timeout; set
`DEER_FLOW_ACP_DAEMON_STOP_TIMEOUT_MS` if application cleanup needs longer.

`ServeACP` callbacks must honor context cancellation, close their streams, and
join their owned model/tool resources before returning. Normal connection
errors close that connection; they do not stop other clients. `Host.Wait`
reports listener, final cleanup, endpoint and lock errors.

## Verification

`go test ./internal/localhost ./cmd/deerflow-acpd-go` covers real TCP
authentication, bounded handshakes, pipelined frames, capacity/control
independence, permissions, endpoint ownership and shutdown ordering. A child
process fixture runs the actual daemon entry point with a real Eino OpenAI SSE
provider, checks cancellation/cleanup and persistence after restart, and
rejects a second process using the same data directory.

Set `DEERFLOW_TEST_BRIDGE` to an existing native Rust Bridge executable to also
exercise its `--status`, `--manage`, ACP stdio proxy and `--stop-daemon` paths.
This optional test neither builds nor changes the Rust Bridge.

The same variable enables `TestExistingRustBridgeV2Lifecycle` in
`cmd/deerflow-acpd-go`. It launches the real Go daemon and the Rust Bridge with
`--protocol v2`, using a local SSE model fixture to check prompt state order,
permission forwarding, cancellation, session isolation, resume and replay.
Build the Bridge with `cargo build --locked` in `bridge/` before running it.
