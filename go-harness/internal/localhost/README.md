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

**Do not pass Bridge `--config`:** Python YAML configuration is not interpreted
by this binary. The daemon rejects it explicitly and publishes an empty
`config_path`. Management requests currently return
`{"ok":false,"code":"unsupported_operation",...}`. This includes Python
skill proposal, memory and session management operations. Session operations
implemented by the Go harness remain available over ACP.

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
