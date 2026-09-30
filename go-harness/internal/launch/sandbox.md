# Command execution configuration

The stdio executable `deerflow-acp-go` and loopback daemon
`deerflow-acpd-go` use the same runtime flags. Command execution is disabled
by default. An operator can select
`--sandbox-provider=local|powershell|wsl2|docker`.
Setting an allowlist, resource limit, or `--sandbox-allow-shell` by itself does
not enable the `execute` tool. `--sandbox-provider=disabled` leaves it off.

The Windows desktop `--config` adapter also accepts the two existing settings
`local_acp.enable_bash: true` and `sandbox.allow_host_bash: true`, only when
`sandbox.use` is `deerflow.sandbox.local:LocalSandboxProvider`. Together they
select the Go PowerShell script backend. Either switch alone leaves commands
disabled or reports the missing host consent. An explicit Go sandbox provider
remains authoritative; if `enable_bash` is set, it must support scripts and
also have `--sandbox-allow-shell` (an explicit disabled provider therefore
rejects that configuration). On other platforms, use explicit Go flags
for a command provider.

SDK users configure the equivalent fields on `deerflow.Config.Sandbox` and set
both `Enabled: true` and the chosen `Provider`. The configuration belongs to
the host operator. The model supplies only executable/arguments or a script
and an optional shorter timeout.

## Providers and authorization

| Provider | Required configuration | Execution boundary |
| --- | --- | --- |
| `local` | Repeatable `--sandbox-allow-command` with absolute host executable paths | Host process with argv; scripts are rejected |
| `powershell` | `--sandbox-allow-shell`; optional absolute `--sandbox-shell` path | Host PowerShell script; argv-only requests are rejected |
| `wsl2` | Explicit `--sandbox-wsl-distribution`; guest executable allowlist or `--sandbox-allow-shell` | Selected WSL2 distribution; capability and guest cleanup probes must pass |
| `docker` | `--sandbox-docker-image` also present in repeatable `--sandbox-docker-allow-image`; guest executable allowlist or `--sandbox-allow-shell` | One constrained container per command |

`local`, PowerShell and WSL2 use host capabilities and are **not security
isolation boundaries**. An executable allowlist restricts the initial argv
executable; allowing an interpreter grants its code execution capabilities.
`--sandbox-allow-shell` explicitly grants arbitrary scripts, whose child
programs are not restricted by the argv allowlist. WSL2 has host integrations
and does not isolate hostile commands from the host.

Every command still passes through the harness's session approval policy and
durable execution receipt before its process starts. Plan mode and the
`read_only` approval setting omit `execute`. Provider configuration does not
silently grant tool permission.

Example startup arguments, appended to either executable's model/data options:

```text
--sandbox-provider=local --sandbox-allow-command="C:\Program Files\Git\cmd\git.exe"
--sandbox-provider=powershell --sandbox-allow-shell --sandbox-shell="C:\Program Files\PowerShell\7\pwsh.exe"
--sandbox-provider=wsl2 --sandbox-wsl-distribution=Ubuntu-22.04 --sandbox-allow-command=/usr/bin/git
```

Docker requires an explicitly selected image and matching image allowlist,
for example the same operator-approved digest in `--sandbox-docker-image`
and `--sandbox-docker-allow-image`. Runtime validation happens when a run opens
its command backend. Unavailable or invalid providers fail the run; they do
not fall back to host execution.

## Limits and environment

| Shared flag | Default when zero |
| --- | --- |
| `--sandbox-timeout` | `2m`, and a tool request may only shorten it |
| `--sandbox-output-bytes` | 262144 retained bytes separately for stdout/stderr |
| `--sandbox-max-concurrent` | 4 running commands per run backend |
| `--sandbox-max-retained` | 64 running/completed records per run backend |

`--sandbox-env=NAME=VALUE` supplies explicit environment values and is
repeatable. A small platform environment is preserved; ambient credentials
are not inherited. Values can contain `=` and may be empty.

Windows local/PowerShell providers additionally support
`--sandbox-memory-bytes`, `--sandbox-max-processes`, and
`--sandbox-cpu-percent` (1..100). Zero disables each optional Job Object limit.
Providers reject limits they cannot enforce.

WSL configuration also exposes `--sandbox-wsl-executable` and
`--sandbox-wsl-user`. Docker configuration exposes:

- `--sandbox-docker-executable`
- `--sandbox-docker-network` (`none` by default; explicit `bridge` supported)
- `--sandbox-docker-cpus` (1 by default)
- `--sandbox-docker-memory-bytes` (512 MiB by default)
- `--sandbox-docker-pids` (64 by default)
- `--sandbox-docker-user` (host UID:GID on Unix; `1000:1000` on Windows)
- `--sandbox-docker-workspace-read-only`

Docker uses a read-only root filesystem, dropped capabilities and
`no-new-privileges`, with only the selected workspace mounted. Host networking
and implicit credential mounts are not enabled. `--sandbox-shell` selects an
absolute guest shell path for Docker/WSL2; their default is `/bin/sh`.

## Completion, cancellation and uncertain outcomes

The tool is foreground-only. Each run owns and closes its command backend;
this interface does not create persistent background jobs. Output is bounded
and the JSON result records command state, exit code, truncation counts and
whether termination was confirmed.

Nonzero exits and timeouts are failed tool executions; the process JSON is
retained in the failed receipt. Canceling a wait alone does not terminate the
process, so the tool explicitly cancels and joins the owned command before
returning. Confirmed terminal processes can be released even when they failed;
their durable tool receipt remains available.

Every command result has `ProcessLocal: true`. A persisted receipt is evidence,
not a process handle that can be restored after restart. A command with
`uncertain` state or unconfirmed termination returns `ErrCommandUncertain`,
keeps its backend record, and must not be automatically retried. Partial side
effects may also exist after a nonzero exit or cancellation. Review durable
receipts through `ListToolReceipts`; `ReconcileToolReceipt` records an explicit
operator assessment and never replays a command.

Local/PowerShell/WSL2 runtime acceptance has been exercised on Windows. Docker
argument and policy validation has tests; actual Docker execution acceptance
still requires an available Docker daemon and the operator's approved image.
