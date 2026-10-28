# OpenCLI host bridge

Run as the existing `gen` desktop user. The host's OpenCLI, HOME and Chrome
extension stay on the host. No browser profile or Docker socket is mounted into
the DeerFlow container.

Host private JSON, mode `0600`, outside the payload:

```json
{
  "token": "GENERATE_A_RANDOM_PRIVATE_TOKEN",
  "executable": "/home/gen/.npm-global/bin/opencli",
  "home": "/home/gen",
  "bind": "172.27.0.1",
  "port": 18990,
  "journal": "/home/gen/deerflow-deploy/host-tools/state/jobs.sqlite3",
  "allowed_sites": ["web", "twitter", "xiaohongshu", "xiaoyuzhou", "weixin", "douyin", "bilibili", "github", "hackernews", "youtube"],
  "allowed_cidrs": ["172.27.0.0/16", "127.0.0.1/32", "192.168.1.190/32"],
  "max_timeout_seconds": 70,
  "workers": 1,
  "max_jobs": 4096
}
```

The last `/32` is intentional for this Gentoo host. A request made locally to
`172.27.0.1` was observed by the server with peer address `192.168.1.190` because
of the host's local network translation. With only the Docker subnet and
loopback allowed, the authenticated host health probe returned `403`. Retain the
exact host `/32`; this does not require allowing the entire LAN. Other hosts
should use their observed local source address rather than copying this IP.

Container private JSON at
`/var/lib/deerflow/user-data/private/host-tools/opencli.json`, mode `0600`:

```json
{
  "url": "http://172.27.0.1:18990",
  "token": "THE_SAME_PRIVATE_TOKEN",
  "timeout_seconds": 70,
  "client_timeout_seconds": 85
}
```

Install `proxy/opencli-proxy` as executable
`/opt/deerflow-host-tools/opencli-proxy`. Set `sandbox.allow_host_tools=true`,
`host_opencli.executable` to that file, `host_opencli.timeout=100`, and explicitly
configure the desired site allowlist. This keeps the existing native tool schema
and approval policy. Python's standard library is sufficient for both bridge and
proxy.

The provided systemd **user** unit targets this deployment path. Enable it as
`gen` if the user manager is available. It does not start a separate browser;
OpenCLI `doctor` can start the local browser daemon and verify the extension.
The fallback `service.py start|stop|status --config PRIVATE_JSON` supports a
user-owned detached process without requiring systemd. For a long-lived fallback,
attach that launcher to the host's existing supervisor or login startup.

All API routes require `Authorization: Bearer TOKEN` and an allowed source CIDR:

- `GET /health`
- `POST /v1/opencli/jobs` with `idempotency_key`, literal `argv`, `timeout_seconds`
- `GET /v1/opencli/jobs/{id}`
- `DELETE /v1/opencli/jobs/{id}`

The private SQLite journal stores receipts and durable idempotency keys. A lost
submission is retried only using the same key and payload. Reusing a key for a
different command returns `409`. Startup marks interrupted jobs `unknown` and
does not replay them. At `max_jobs`, new requests return `507`; retain the journal
until all uncertain commands are reconciled, then rotate it during maintenance.

The proxy forwards normal CLI output and exit code. On connection loss or an
unconfirmed host process state it exits `125` with a structured stderr receipt;
the outer command's termination only proves the proxy ended. Query the returned
job ID before deciding whether a mutating command can be submitted again.

Useful deployment checks are native `host_opencli` version, doctor, list, and one
public read-only site query. Search/read readiness does not authorize posting or
messaging from a logged-in browser account.
