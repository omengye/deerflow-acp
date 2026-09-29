# Session MCP resources

`Manager` binds MCP connections to a runtime owner and session. The runtime must
hold its idle session lifecycle lease while binding or changing configuration.
It supplies tools through Eino's official `officialmcp.GetTools` adapter, with
collision-resistant provider-compatible exposed names and fresh metadata copies.

## Lifecycle

- `Bind` validates and clones configuration, connects every candidate server,
  discovers tools, enforces the aggregate limit, and atomically publishes the new
  generation. Identical configuration is idempotent. Failure preserves the active
  generation. Successful publication immediately cancels stale tool wrappers.
- `Release` closes that session's active and pending generations. `ReleaseOwner`
  first retires the owner, preventing late reconnects, then also closes retired
  generations. Cancellation of the caller's wait does not cancel cleanup.
- Cleanup timeout failures remain tracked. `Close` rejects new bindings and
  joins every owned cleanup/direct child until it has actually finished. The
  `Done` channel closes only then; `WaitClosed(ctx)` can stop waiting without
  releasing ownership. The SDK must retain its storage/runtime lock until this
  terminal signal. A failed process kill remains owned until the child exits.
- A setup request ending after successful `Bind` does not end the session. Tool
  calls have their own timeout and follow the binding lifetime.
- Connections are not automatically rebuilt and uncertain tool calls are never
  replayed. The caller can explicitly change/release/rebind configuration after
  dealing with the failed operation.

## Transports and trust

Stdio is the default transport. An empty `AllowedCommands` denies process startup.
Every permitted command must be an absolute, existing executable. The manager
canonicalizes its path and pins file identity when the policy is built. Arguments
remain client-controlled: allowlisting a general-purpose shell or interpreter
grants its corresponding host execution authority. This is not an OS sandbox.
Executables and their parent directories must remain trusted after validation.

Stdio receives the session workspace as its working directory. Only a small
standard OS environment set is inherited; provider keys and unrelated host
credentials are excluded. Explicit `Env` entries are added for that server.
The manager owns both pipes and reaps/terminates the direct subprocess on close.
It does not promise to terminate independently spawned descendant processes.

HTTP and SSE are **outbound MCP transports**, separately disabled unless the
host policy enables them. They do not start an HTTP API server. Each endpoint has
its own HTTP transport and credentials. Redirects and cross-origin SSE endpoints
are rejected. The SDK's detached HTTP operations are tied to the endpoint
lifetime; session DELETE cleanup is bounded and its response body is closed.
Cancelling a request or closing a network transport does not guarantee that the
remote server has cancelled an in-flight side effect. A remote server that refuses
session shutdown can produce a reported cleanup error even after local sockets
have been closed; the manager never replays that operation to infer its outcome.

Connection configuration is not stored in model tool metadata. Public errors
omit executable arguments, URLs, environment values, headers and remote error
strings. Known credential echoes in metadata/results are rejected before
formatting. This is a defensive guard, not a general secret classifier: longer
configured values use substring checks and short values require an exact string
match to avoid treating common flags as secrets. Trusted MCP servers remain
responsible for their output and side effects.

Defaults: 8 servers, 128 tools in total, 20 discovery pages per server, 64 KiB
formatted result characters, 15 seconds setup, 2 minutes per call, and 5 seconds
per cleanup wait. These are configurable through `harness.MCPPolicy`.

## Verification

`go test ./internal/mcp` uses an actual subprocess running the official SDK stdio
server, plus SDK HTTP/SSE fixtures. It covers discovery, invocation, environment
isolation, cancellation, replacement rollback, stale tools, direct-process
cleanup, owner retirement, unknown-result no replay, aggregate limits, and
credential forwarding/echo rejection.
