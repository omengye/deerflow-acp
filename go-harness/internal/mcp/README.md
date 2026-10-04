# Client and host MCP resources

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
- Client-owned connections are not automatically rebuilt. The caller can
  explicitly change/release/rebind configuration after dealing with the failed
  operation. Explicit host recipes have the separate lifecycle below. Neither
  ownership mode replays uncertain tool calls.

## Borrowed resources

`Describe` returns a stable descriptor of normalized connection intent and the
actual discovered tool schemas, plus an opaque connection generation. The
descriptor contains digests and public server/tool names, never executable
arguments, workspace paths, URLs, environment values or headers. An identical
explicit reconnect retains the descriptor and receives a new generation.

`Acquire` requires both the saved descriptor and the explicitly authorized
current generation. It discovers tools once, fingerprints those exact adapters,
and rechecks binding identity before returning a `Lease`. Changed configuration
or schemas are incompatible; an old generation cannot authorize a replacement.
The manager never persists raw connection recipes or grants durable task
authority itself. The SDK owns independent versioned task resource grants.

Leases expose enhanced Eino tools (including structured image results), `Check`,
and a `Done` channel. Closing a lease cancels only that borrower; releasing or
replacing the binding, or loss of its transport, invalidates its borrowers.
Refusal before dispatch is marked as having no effect. After dispatch, transport
failure keeps the existing uncertain-effect contract and never triggers replay.

## Explicit host recipes and restart reconstruction

`Config.HostMCPRecipes` / YAML `mcp.host_recipes` is a separate host registry.
It defaults to empty. Each recipe has a unique ID, one absolute workspace and
1–8 explicitly configured servers. Workspace selection is exact after path
normalization; there is no ancestor or wildcard match. A recipe is shared by
sessions for that workspace, with independent borrowed execution leases. Client
detach does not stop these host transports; SDK `Close` joins them. A matching
workspace cannot also supply client MCP servers in this first version.

```yaml
mcp:
  host_recipes:
    - id: project-docs
      workspace: 'C:\Work\project'
      restore_on_restart: true
      servers:
        - name: docs
          transport: stdio
          command: 'C:\Tools\docs-mcp.exe'
          args: ['--stdio']
          env:
            DOCS_TOKEN: '$DOCS_MCP_TOKEN'
```

The executable must exist and be trusted; the example paths are placeholders.
The host operator edits YAML and restarts. The launcher resolves `$VAR` or
`${VAR}` references in env/header values and endpoint URLs. Unset references
fail startup without echoing their contents. MCP recipes are not exposed as
desktop editable values, applied settings, role definitions or model input.
The shared schema only advertises the host YAML path and restart requirement.
Unknown fields/type coercions are rejected. Raw credentials remain in trusted
host config/process memory; saved execution input contains only recipe ID,
session ID, origin generation and the existing configuration/schema digests.

Only `restore_on_restart: true` authorizes automatic reconstruction. Before
workers start, the SDK loads existing host resource grants, verifies the saved
session workspace and complete configuration digest, reconnects from current
host config, then checks the actual discovered schema using a lease. Only a
successful match advances the independent grant generation/version by CAS.
The execution snapshot, tool receipts, task status and native checkpoints are
not rewritten. Suspended tasks retain their normal explicit resume flow;
finished tasks are not rerun, and continuation and uncertainty fences remain.
Missing, unavailable or changed recipes leave tasks waiting or incompatible so
the desktop can open for inspection. Credential changes also change the digest.
`restore_on_restart: false` (default) requires explicit task resource authority
after reconnection. Arbitrary client MCP configurations never become recipes,
and recipe executable/network authority does not enlarge client MCP policy.

Live transport loss is not silently retried. Restore the service and restart
the harness or use the normal explicit connection/authorization workflow.
This facility does not restore remote server state or guarantee remote effects.

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

The resource tests additionally cover stable identity versus reconnect authority,
configuration/schema changes, independent borrowers, enhanced image retention,
and a real process that writes an effect then exits without returning a result.

`TestSDKHostMCPRecipesRestartReconstruction` is the focused SDK restart chain
using real local MCP/OpenAI fixtures and SQLite. It passed in 11.040s, covering
host/client authority isolation, detach lifetime, missing/changed/manual recipes,
live schema drift, immutable resource refs, CAS grants, completed-effect no
replay and retained uncertain receipts. It does not replace desktop or external
MCP service acceptance.
