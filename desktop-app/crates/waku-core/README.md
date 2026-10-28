# waku-core

`waku-core` is Waku's daemon-only runtime. It contains the native session
drivers, provider discovery and model metadata, task persistence, attachment
storage, workspace filesystem and Git services, Computer Use process control,
and daemon-owned settings. It depends on the serializable contract in
[`waku-protocol`](../waku-protocol), but contains no desktop transport or UI.

The transport is an authenticated WebSocket (loopback by default). Requests
have stable UUIDs for idempotency; session events carry monotonically
increasing sequence numbers and runtime-generation IDs. Unauthenticated sockets
have a twenty-second absolute deadline and a separate admission quota. The
daemon's `--authentication-timeout-seconds` option accepts 1 through 60 seconds;
it overrides `WAKU_AUTHENTICATION_TIMEOUT_SECONDS`. Invalid effective values
fail startup. This one deadline covers HTTP upgrade and the
authenticated Hello exchange, including fragmented input and Ping traffic.
Clients should allow a larger total connection budget to account for DNS/TLS
before reaching the daemon. Failed handshakes log their stage and elapsed time;
successful authentication taking at least one second logs its elapsed time.
These diagnostics contain no token or conversation content. A private
temporary disk journal retains each active runtime's replayable events; clients
pull bounded pages at a fixed sequence boundary before consuming live events.
Storage failures are explicit rather than silently truncating replay. Closing
the runtime or daemon removes the journal contents; durable provider history
remains the authority after a daemon restart. Stale events or commands from a
replaced runtime are ignored.

`DaemonClient` lives in [`waku-client`](../waku-client), which is what Waku
Desktop depends on. `serve` and `WakuBackend` are used by the `waku-daemon`
binary.

Configuration ownership is explicit:

- the desktop owns `~/.waku/app.json` in Release and checkout-local
  `temp/app.json` in Debug;
- the daemon owns `~/.waku/settings.json`.

Task SQLite rows and durable attachment materializations are daemon-owned as
well. Client-local attachment paths are upload inputs or caches only; provider
prompts and persisted messages use daemon-issued paths and references.
Projectless task directories are daemon-owned too and live beneath
`~/.waku/projects`.

The protocol types use Serde's tagged JSON representation and are exported by
`waku-protocol`, including checked-in TypeScript bindings.
