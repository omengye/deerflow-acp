# Optional command backends

`sandbox.New(ctx, harness.SandboxConfig, workspace)` creates a session-scoped
command owner. Command execution is disabled unless both `Enabled` and an
explicit provider are configured. The caller must apply the harness permission
policy to the exact request before calling `Start`; this package does not
auto-approve a tool or implement an ACP permission UI.

## Providers and policy

| Provider | Invocation and policy | Boundary |
| --- | --- | --- |
| `local` | Absolute executable + argv; exact executable allowlist | Host process, not a security sandbox |
| `powershell` | Explicit `AllowShell`; UTF-16 encoded script; no profile, no interactive stdin | Host process, not a security sandbox |
| `wsl2` | Explicit distro; real kernel/Python/wslpath/workspace probes; allowlisted guest executable or explicit `AllowShell` | Shares host filesystem and distro capabilities; not a security sandbox |
| `docker` | Explicit local image and image allowlist; Linux daemon required; allowlisted guest executable or explicit `AllowShell` | One constrained container per command with only the workspace mounted |

Allowlisting an interpreter permits the programs it can execute. `AllowShell`
grants arbitrary scripts; executable allowlists do not parse or restrict the
commands inside those scripts. The harness must still approve each script.
No request may choose a different cwd, environment or Docker options.

The backend holds a directory handle and verifies its identity on every
`Start`, including immediately before execution. On Windows this handle also
prevents renaming/removing the workspace while the backend is open. Local
commands can still access other host paths and services. These checks do not
turn shell execution into filesystem isolation.

Host child environments contain only basic OS/path/temp/locale variables plus
operator-provided `Environment`. Model API keys, SSH agent variables, proxy
variables and arbitrary host environment entries are not implicitly forwarded.
This does not prevent a local process from reading credentials using its normal
OS account. Docker adds no implicit home, credential or daemon-socket mounts;
files already inside the selected workspace remain part of that workspace.

## Limits and ownership

Defaults are a 2-minute timeout, 256 KiB retained per stdout/stderr, 4 concurrent
commands and 64 retained task records. Requests can shorten but cannot enlarge
the timeout. Output is continuously drained after the capture limit, and
snapshots report total bytes and truncation. Captures and record counts are
bounded; completed tasks remain until `Release` or the backend is discarded.

Windows processes start suspended, join a Job Object with kill-on-close, then
resume. Optional job memory, active-process and CPU-percent caps are enforced.
Cleanup kills remaining descendants even when the leader exits normally, waits
for member process handles, and joins pipe readers. Unix uses a command-owned
process group; local CPU/memory/PID caps are rejected there rather than silently
ignored. Deliberately escaping a Unix process group is outside the local
provider's isolation guarantees; use Docker for untrusted execution.

Docker defaults to network `none`, 1 CPU, 512 MiB memory (with no additional
swap), 64 PIDs, dropped capabilities, no-new-privileges, read-only rootfs and a
bounded temporary filesystem. Network `bridge` requires explicit configuration;
host network is rejected. The workspace is mounted at `/workspace`; only the
operator can choose a read-only workspace mount. The default container user is
the host UID:GID on Unix and `1000:1000` on Windows. Images must already be
present; no automatic image pulls occur. Container removal is checked after
completion and cancellation. If Docker is unavailable during cleanup, the task
is `uncertain` and its container name remains in `ResourceID` for reconciliation.

WSL requires Linux `/proc`, a WSL2 kernel, and Python 3.9+ with pidfd support.
Paths come from the selected distro's real `wslpath`, not an assumed `/mnt/c`
mapping. A guest-side subreaper owns one process group, kills/reaps descendants,
and returns a final termination receipt. Cancellation is sent through its stdin;
EOF also cancels it. A missing/negative receipt is `uncertain`, even if the host
`wsl.exe` exited. No operation shuts down or terminates a shared distro. Job
memory/CPU/PID limits are not advertised for WSL; use Docker for those limits.

`HostPath` translates a Docker/WSL workspace coordinate for client display. It
rejects lexical traversal and unrelated remote paths. Callers still apply their
normal symlink/artifact validation when opening the resulting host file.

## Background lifecycle and recovery

```go
backend, err := sandbox.New(ctx, config, session.CWD)
// Approve the exact request through the owning tool's permission policy first.
started, err := backend.Start(sessionLifetime, request)
snapshot, err := backend.Poll(ctx, started.ID)
snapshot, err = backend.Wait(waitContext, started.ID)
snapshot, err = backend.Cancel(cleanupContext, started.ID)
err = backend.Release(ctx, started.ID)
err = backend.Close()
```

`Start` returns a `starting` record; process-start failures are represented by
the final snapshot. Its context owns the task lifetime. `Wait` only limits the
wait and never cancels the task. `Cancel` requests termination then waits, while
`Close` cancels all commands and waits for their cleanup and output readers.
Background tasks that should outlive a foreground tool call need a
session/parent-task lifetime context, with explicit cancellation by the owner.

`Release` refuses running or termination-uncertain tasks. A successful `Wait`
means the record is final; inspect `State`, `ExitCode` and
`TerminationConfirmed` before declaring execution successful.

All V1 tasks are explicitly `ProcessLocal`. Persisting an ID or snapshot in
SQLite does not restore a process handle after restart. Interrupted commands
must be reconciled as unknown/interrupted, with no automatic re-execution of
side effects. Docker containers may survive a host crash; `ResourceID` enables
manual/provider-specific reconciliation. The backend does not claim durable
background execution or exactly-once side effects.

## Tests

`go test ./internal/sandbox` runs actual native processes and PowerShell when
available, including child-tree cleanup, normal-exit orphan cleanup, timeout,
wait cancellation, cwd/argv handling, credential stripping, output limits and
Job resource configuration. Docker/WSL argument and policy tests run without
remote dependencies.

Set `DEERFLOW_TEST_WSL_DISTRO` to an installed distro to run the real WSL2
supervisor/cancellation test. Set `DEERFLOW_TEST_DOCKER_IMAGE` to a pre-pulled
Linux image with `/bin/sh` to run the real Docker lifecycle test. These tests
never install dependencies, pull images or terminate a shared distro.
