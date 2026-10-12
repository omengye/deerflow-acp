# Native harness reliability and task continuity

This change adds three bounded capabilities to the Go/Eino harness. It does not
add Goal mode, automatic completion evaluation, background retries, or paid
auxiliary model calls. Provider transport stays streaming, including synchronous
result APIs. Eino's Generate-only middleware endpoint is preserved internally;
the terminal provider wrapper collects its result from Stream.

## Version-checked file writes

Both default and configured `read_file` return a JSON object with `version` and
`content`. Configured reads retain their range/continuation fields. Version is a
SHA-256 of the entire file, including when only a range is returned. Metadata is
placed before content so an inline preview retains the version when possible.

`write_file`, append, `edit_file` and `str_replace` require `expected_version`
when a file already exists. Omitted version or `"absent"` means create-only.
Successful writes return the new version. Conflicts are certified pre-execution
failures: the agent must read again, then submit a new operation under the normal
approval policy. A saved approval includes the original expected version and
cannot authorize overwriting an intervening edit.

```json
{"path":"notes.txt","content":"new text","expected_version":"<version from read_file>"}
```

A process-wide native mutation lock covers version checks, replacement, append,
move and delete across factories/agents, including overlapping workspace roots.
This deliberately favors correctness over native write throughput. It does not
lock external editors, shell commands, other processes, or MCP servers. It is
not an OS-wide atomic compare-and-swap and cannot prevent every external race.
Move/delete keep their existing permissions and overwrite contracts; they do
not acquire content-version preconditions in this iteration. Versions prove
content equality, not that the model semantically understood or fully read it.

Existing scripts that overwrite without a version must be updated. New-file
creation remains compatible. The 512 KiB native mutation limit remains.

## Conservative no-progress guard

The guard observes completed model-visible tool results before context
compaction. It retains a bounded 20-entry fingerprint window in native Eino
run-local checkpoint state. Identical argument/result cycles of length 1..4
produce a change-strategy hint after three repetitions, and stop the run with
an explicit incomplete-task error after five, but only after a prior model call
has been offered the warning. It does not report successful task completion or
create an automatically resumable approval. The user may send a new instruction.

Successful commands and mutations reset this evidence because their identical
text may conceal real changes. Task wait/status calls reset it as well. Different
file contents, query parameters and pagination arguments do not count as the
same observation. The initial read allowlist covers file/list/search/web reads;
tool-output and attachment chunk readers are excluded. Certified failed calls
can contribute regardless of name. No model is used to judge progress.

The state survives native interrupt/resume and compaction, and new agent runs
start fresh. Root and configured child agents have separate state. This is a
conservative exact-pattern guard, not semantic stagnation detection: changing
arguments, dynamic output IDs, or successful no-op commands can avoid detection.

## Session task notebook and history tools

The standard SDK/ACP engine exposes `task_note`, `history_search`, `history_read`
subject to the host tool policy. A custom injected Engine is unchanged. Native
delegated roles do not receive the parent's notebook/history capabilities;
background role construction does not expose them either.

`task_note` has `list` and `set` actions. A set uses `expected_revision` (zero for
a new key); empty content clears the note and retains a revision tombstone.
There are at most eight active notes of 3000 UTF-8 bytes each and 64 named slots
per session. Reuse named slots after deletion. Updates commit to SQLite and
survive restart. They remain observed model notebook mutations even when a
later part of the run fails; they are not accepted user facts or task-success
evidence. Plan/read-only mode rejects updates. Normal permission policy still
applies; dangerous mode treats this private notebook tool as non-dangerous.

Notes enter only the next model request, clearly labeled untrusted model reports,
not the persisted user-message list. The wrapper runs before model input
accounting so note tokens count against normal context and spending limits.
Notes survive compaction. There is no automatic task-boundary detector: the
agent should clear obsolete entries when the user changes tasks. No long-term
memory extraction or cross-session sharing is added.

History search uses case-sensitive literal substring matching, including Chinese,
over the current session's committed business events, not model checkpoints.
It therefore sees source text removed by compaction, including earlier completed
observations in the current run. Only visible user text, lead assistant text,
and session-owned tool-end text are projected; reasoning, control metadata,
explicitly child-attributed text and recursive notebook/history tool results are
excluded. Delegated tool receipts already recorded in the parent session can
remain readable there; a child cannot read the parent's history. Results are historical
data and do not grant current permission or establish successful execution.

Each search scans at most 256 events (with a 2 MiB soft batch budget, allowing
one existing larger source event) and returns up to eight 600-character excerpts.
`next_before` continues backwards; `through` identifies the query's high-water
mark. An empty result with `next_before` is not an exhaustive miss. No index or
semantic ranking is claimed. Sources are immutable event fragments, so a streamed
answer may span multiple source IDs and a query crossing fragments will not match.
`history_read` reads one source in 4000 Unicode-code-point pages. Returned offsets
apply to that exact source. Full oversized tool snapshots remain available through
the separate `read_tool_output` tool.

Run/session identity and live execution fencing are checked at the storage boundary;
model input never selects a session. Session deletion removes its notebook too.
There is no separate desktop notebook editor in this iteration.

## Compatibility and verification

The engine execution contract version changes. Previously paused executions with
an older contract must not silently resume under changed tool semantics; cancel
them and start a fresh user turn. Existing chat history remains readable.

Tests cover default/configured file conflicts, concurrent factories, native guard
warning/stop, permission checkpoint restoration, notebook CAS/capacity/deletion,
scope isolation, Unicode paging, historical and current-run reads, native
compaction/reopen, tool policy, and streaming-only synchronous model wrappers.
Real-provider quality, long-running soak tests and packaged desktop GUI acceptance
remain separate from these deterministic regressions.

### Verification recorded on 2026-10-12

Commands ran from `go-harness`; final reruns used `GOMAXPROCS=2` to avoid
oversubscribing subprocess/deadline-sensitive tests on this Windows host.

- `go build -mod=readonly -p 2 ./...` passed.
- Full `internal/tools`, `internal/engine/eino`, and `internal/acp/agent`
  package tests passed on the final implementation (`-p 1 -count=1 -timeout 10m`).
- Focused race tests passed in all four affected packages:
  `go test -mod=readonly -race -p 1 ./internal/tools ./internal/runtime ./internal/engine/eino ./internal/acp/agent -run 'Test(VersionedWorkspace|TaskNotes|TaskHistory|TaskContext|ProgressGuard|NoProgress)' -count=1 -timeout 5m`.
  This includes the final concurrent notebook CAS and delegated-role isolation
  tests, as well as a provider fixture that rejects non-streaming calls.
- The initial `go test -mod=readonly -p 2 ./... -count=1 -timeout 30m`
  did NOT pass. Two background-role regressions were fixed by returning
  `written: true` rather than a generic `status` field in file-write receipts;
  both failing cases passed a targeted rerun. Skill-learning and MCP cases
  encountered deadlines/initialization failures; the previously failing cases
  and the complete MCP package subsequently passed separate reruns.
- The initial runtime package hit its aggregate 30-minute timeout while running
  the skill-matching tests. Its auto-patch regression case and all four cases
  of the interrupted skill-matching test passed separately.
  A complete final-source all-module green run has NOT been established, and
  isolated successful reruns do not establish long-run stability.
- `git diff --check` passed. No desktop bundle was built or deployed, and no
  live paid-provider acceptance run was performed.

Logs are under `.build-cache/` at the repository root, notably
`harness-continuity-final-packages.log`, `harness-continuity-final-race-extended.log`,
`harness-continuity-final-build.log`, and `harness-continuity-regression-20261012.log`.
