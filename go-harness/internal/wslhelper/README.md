# Go WSL command helper

This is a standalone Linux process, never an in-process daemon plugin. It uses
Go and Linux syscalls only. The Windows backend owns the WSL transport; this
helper owns guest children as a subreaper and returns a receipt after cleanup.

`probe <absolute-workspace>` reports protocol version, executable SHA-256,
architecture, kernel, canonical workspace, pidfd and subreaper capability.
`run` reads one newline-terminated `Request` (at most 4 MiB) from stdin. It
rejects incompatible version/digest/workspace before spawning a child. Only the
explicit request environment reaches the child. The remaining stdin accepts
`cancelled\n` or `timed_out\n`; EOF and invalid control also cancel the command.

Each stdout packet is a 4-byte unsigned big-endian length followed by a JSON
`Frame` of at most 64 KiB. Output frames contain at most 32 KiB of base64-encoded
child bytes. A single final receipt follows both child cleanup and output drain.
Every frame carries the protocol version and request ID. Raw stderr belongs
to helper/transport diagnostics and cannot confirm a command outcome. The host
requires a valid terminal frame and confirmed successful transport termination.

The helper sets `PR_SET_CHILD_SUBREAPER`, starts a new child session, and reaps
children using a single wait owner. Cleanup repeatedly enumerates all thread
child lists, opens a pidfd, verifies the process parent, signals through that
pidfd and reaps until no children remain. It does not use a bare PID or process
group ID as termination authority. A stalled output pipe has a bounded write
deadline; SIGPIPE is handled so cleanup can run after transport loss.

This lifecycle protocol does not provide OS isolation from programs running as
the same user. The WSL provider retains its existing host/distro permission
boundary. Missing proof, failed cleanup or malformed transport leaves the host
command `uncertain`; it does not authorize automatic replay.
