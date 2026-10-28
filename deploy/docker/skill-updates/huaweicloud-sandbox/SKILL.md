---
name: huaweicloud-sandbox
description: Manage the existing Anthropic Sandbox Runtime on the user's Huawei Cloud Linux host. Use for explicitly requested remote sandbox health checks, sandboxed commands, policy inspection, or approved network/filesystem allowlist changes. Do not use for unrelated cloud administration or automatic SSH trust enrollment.
---

# Huawei Cloud Sandbox

Use the packaged [Python client](scripts/huaweicloud_sandbox.py) through `skill_run`.
The connection defaults are host `1.92.70.163`, port `2233`, user `root`.
In the deployed Gentoo DeerFlow container, private connection files live outside
the skill package and image:

- Key: `/var/lib/deerflow/user-data/private/skills/huaweicloud/huaweicloud_ed25519`
- Verified known-hosts: `/var/lib/deerflow/user-data/private/skills/huaweicloud/known_hosts`

These paths are not evidence of current reachability or isolation. Never read
the private key into the conversation, the skill package or logs. For desktop
use, explicitly pass that machine's external key and verified known-hosts paths;
the script's portable defaults remain `~/Documents/ssh_info/huaweicloud_ed25519`
and `~/.ssh/known_hosts`.

## Readiness and authorization

1. Run `skill_run` with `{"action":"check","name":"huaweicloud-sandbox"}`.
   Python 3.10+ and Paramiko metadata are required. The [runtime declaration](skill-runtime.json)
   and [requirements](requirements.txt) are data, not installation instructions.
   If missing, report runtime-unready and ask before preparing dependencies.
2. Use a separately verified SSH known-hosts file. Unknown or changed host keys
   fail closed. Do not use AutoAddPolicy, disable host-key checking, or accept a
   fingerprint obtained only from the connection being verified.
3. Match the operation to the user's authorization. Publication and a successful
   dependency check do not authorize remote commands or policy expansion.

## Execute pinned source

Use `skill_run` action `run`, name `huaweicloud-sandbox`, path
`scripts/huaweicloud_sandbox.py`. Pass connection paths explicitly because the
command backend does not inherit arbitrary host environment values.

Read-only health example:

```json
{"action":"run","name":"huaweicloud-sandbox","path":"scripts/huaweicloud_sandbox.py","args":["--key","/var/lib/deerflow/user-data/private/skills/huaweicloud/huaweicloud_ed25519","--known-hosts","/var/lib/deerflow/user-data/private/skills/huaweicloud/known_hosts","status"],"timeout_seconds":90}
```

Replace the final `status` argument with the operation below, retaining connection
arguments before the operation:

- `get-config` reads `/root/.srt-settings.json`; missing/invalid access is a failure,
  never an empty-policy success. Treat returned paths and domains as private data.
- `run`, `--`, then separate command arguments executes the exact argv through
  `srt -c`. For shell operators, pass `sh`, `-c`, and the complete shell expression
  only when the user explicitly requested that expression. `run -s /absolute/policy.json -- ...`
  selects an existing policy; this does not validate the policy's protection.
- `allow-domain`, exact DNS name, `--confirm-policy-change` adds one network entry.
- `allow-write`, specific absolute Linux directory, `--confirm-policy-change` adds
  one writable directory. Confirm the exact expansion and consequences first.

Policy updates require remote Linux Python 3 and a readable valid existing JSON
policy. They preserve unrelated fields, retain a private backup, check for
concurrent modification and atomically replace the file. Failure never constructs
a replacement policy from defaults. External writers must coordinate with the
same lock; no filesystem atomicity claim extends to arbitrary concurrent editors.

## Verify and report

Report the pinned version, process exit status and actual returned results.
For `changed:true`, inspect the returned backup/digest and read the policy again.
`changed:false` means no mutation occurred and has no backup/digest; confirm the
existing entry read-only. Do not claim that a dependency check proves SSH connectivity or
that version output proves effective isolation. Test isolation only with an
explicitly authorized probe. This script does not add isolation to the local
PowerShell/WSL process or to root SSH administration.

Timeout, lost SSH connection or output-limit failure cannot confirm remote process
termination. Do not automatically repeat a mutating command; inspect remote state
under fresh authorization. This client has no original-request status or lost-receipt
recovery API. Finding the requested policy entry proves only current state, not
which call wrote it; absence does not prove the old process has ended. Confirm
execution state and obtain explicit authorization before a new mutation.
Never run a mutable installation path to bypass a
session-pinned version or use direct file copies to claim managed publication.
