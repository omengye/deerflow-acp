# Skill access and terminal evidence

This is the persistence foundation for P4 discovery. It does not generate
candidates, judge skill quality, run evaluations or publish skills. The SDK now
starts a durable local collector from `internal/skills/evolution`; it consumes
pending outcomes into root/version signals without model calls. Candidate jobs,
maintenance model budgets and explicit user feedback now use separate evolution
services; automatic attribution, signal selection, behavioral evaluation and
publication monitoring remain outstanding.

## Access observations

Run-owned immutable snapshots observe successful `Get` (`skill_load`) and
UTF-8 `read_skill_file` results (`support_read`). Metadata `List`, file manifests,
raw operator reads, missing files, binary tool results and corrupt content are
not observed. Subsets inherit the observer but cannot expand visibility. Operator
and export snapshots have no observer. A successful access does not prove that
the model followed those instructions or that the skill caused an outcome.

The host broker fences the current foreground execution or native background
attempt and saves only the immutable source/scope/name/version, relative file
path/hash/byte count, root, session, run, attempt, role and first observation time.
It rechecks the file identity against Registry storage. No body, prompt, tool
arguments, credentials or provider error is copied into these tables. Repeated
reads of the same file and kind in one attempt/role share one record. Distinct
attempts and roles retain separate records for attribution. The collector groups
them by root task, source identity, skill name and version; future quality
classifiers must count distinct roots when identifying repeated outcomes.

New role tasks require their saved invocation. Legacy `deerflow-child-v1`
checkpoints can have no role registration; their child run/task, session and
root are still checked, and their role is left empty. Missing modern role
identity is not silently converted to this legacy case.

An evidence storage failure does not return the loaded content as a successfully
recorded read. This is part of the live read's persistence boundary; it is
separate from future generator/evaluator failures, which must not change an
already completed task's result.

## Terminal outbox

`harness_skill_terminal_outbox` is a SQLite trigger on `harness_runs.status`.
Only a transition from a nonterminal state to `completed`, `failed` or
`cancelled` inserts `harness_skill_outcomes`. Foreground completion, idle
cancellation, native background completion/failure/lease expiry all go through
that existing business projection. A failed transaction rolls back both the
task transition and the new outcome. Updating the same terminal state again
does not create another record. Reopening the host does not backfill old runs.

Outcomes store the status and the presence of uncertain effects at the time of
the transition, not a quality verdict. `waiting_input` and
`needs_reconciliation` are not terminal outcome signals. Later receipt
reconciliation must remain separate from the original observation. In
particular, an unclassified provider error, network failure, quota exhaustion,
permission refusal or cancellation cannot alone count as skill degradation.

Session deletion removes reads and pending outcomes in the same transaction,
including the private sessions of its terminal background tree. These private
evidence tables do not own or delete installed skills or publication history.
No source directory scan or filesystem write is involved in recording evidence.

## Collector lifecycle and query contract

Each `df_skill_collection_jobs` row is bound to an outcome and its session. A
page claim saves its token, version, attempt count and 10-second lease before
processing. One page reads at most 256 observations (worker default 128), checks
their root/budget identities and Registry file digests, then commits signal
links, the read cursor and final outbox ACK in one transaction. It closes row
iterators before nested queries, including with a single-connection SQLite pool.

Errors preserve the cursor and previously committed pages. Retries have durable
backoff and a three-attempt limit per page; successful page progress resets that
page's attempts. Lost leases retain their attempt count after restart. Graceful
worker cancellation releases its page without consuming a retry; page deadline
failures still count. After exhaustion an operator must submit the failed job's
current version to retry. A stale worker cannot commit a page or change its
replacement's state. Worker errors never change a committed user task outcome,
and retry never reruns user tools or models.

The SDK owns and joins the worker before closing database dependencies. It polls
idle work once per second, yields between pages, and uses a two-second deadline
per step. Unclaimed jobs and all durable failed/retry states are queryable.
Session deletion also removes collection jobs, aggregate signals and their read
links, while skill installations/publications remain owned by the Registry.

Signals have `attribution=unassessed`. Completed, failed, cancelled and uncertain
run counts are evidence inventory, not quality scores. `collection_pending`
also reflects current unfinished runs/unprocessed outcomes in that root and is
an advisory live field. Neither a read nor a completed run proves causal skill
quality. Signals without any actual skill read are not invented for empty runs.

Operator queries filter by currently configured source identities. Signal list
cursors bind filters and the signal catalog revision; evidence cursors bind the
signal identity and its state version. Updates return conflict for stale pages.
Queue job pages are a live sequence-based inventory; completed/deleted jobs can
disappear between pages. The status overview includes only its first 100 jobs;
`skill.collector.jobs` provides access to subsequent unresolved pages. No model
tool or normal ACP method exposes these operator controls.

## Operator feedback

The evolution service stores versioned operator feedback against these real
signal/read identities. The signal's `feedback_revision` is separate from its
read-set `state_version`; a feedback change invalidates signal list pages without
inventing a new read or changing task outcomes. Revising or withdrawing a judgment
preserves its bounded history and does not add an independent root outcome.
Feedback failures do not affect subsequent prompts.

Targets and content versions are derived from the saved signal, never supplied
by the caller. Feedback may be submitted before the whole root is finished;
`evidence_current` only compares the read-set version and must not be treated as
a complete outcome watermark. User-reported helpfulness, corrections and
network/quota/permission/cancellation causes remain unassessed evidence, not
trusted causal classification. See the [feedback contract](../skills/evolution/feedback.md)
for CAS, exact retries, text redaction limits and lifecycle behavior.
