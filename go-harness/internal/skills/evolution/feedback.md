# Operator feedback on observed skill use

`SubmitFeedback`, `Feedback` and `QueryFeedback` use the same Registry source
authorization and signal/read identities as the collector. SDK and authenticated
MANAGE adapters expose them as operator actions. They do not call models, replay
tools, create generation jobs or publish skills within the feedback operation.
Feedback remains available when generation is disabled. A separately enabled
[discovery worker](discovery.md) can subsequently select repeated current
corrections for candidate generation. Feedback is not a model tool or a standard
ACP method.

## Meaning and evidence

One signal represents one root task, source identity, skill name and immutable
content version. Feedback has one current judgment and immutable revisions for
that signal. Revising a judgment cannot create additional independent votes.
The kinds are `helpful`, `correction`, `quality_failure`, `network`, `quota`, `permission`,
`cancelled`, `other` and `withdrawn`. These are operator reports. They do not
establish that the skill caused the result; `signal.attribution` stays
`unassessed`, including for `helpful` and `correction`.

A new judgment must reference 1–16 distinct, positive IDs of actual reads
linked to that signal. The service checks the read's root, source, scope,
name, content version and outcome/run/session association. Target, content
version and root/session identities are derived on the server. Callers cannot
substitute the current published skill version for the observed version.

The current record reports `evidence_current` only when its observed read-set
version matches the current signal version. That flag does not mean that all
root work has finished, that no later outcome can arrive, or that quality has
been assessed. `collection_pending` is a separate live hint. Future automated
evaluation needs a complete outcome watermark and trusted failure categories;
raw failed-run counts and operator reports alone cannot authorize rollback.

`signal.quality` now exposes the host's current assessment separately. Pending
collection, cancelled work, uncertain effects, exhausted root budget and typed
native tool network/permission/quota failures are excluded from quality votes.
Failure codes come from host error types, never pattern matching model text.
Unclassified provider/tool failures remain unknown.

`quality_failure` is an explicit operator confirmation that the observed skill
caused a quality failure; ordinary `correction` retains unassessed causality.
Only current confirmed feedback with actual linked reads and fully settled
root evidence yields `eligible=true`, `confidence=high`,
`source=operator_confirmed`, and `kind=skill_quality_failure`. `helpful` produces
the corresponding success assessment only when all root work completed.
Withdrawal/revision changes the live assessment. The publication monitor also
checks publication time/version and deduplicates roots before using these votes.

## Transactions, revisions and retries

Submit requires `signal_id`, `expected_signal_state_version`,
`expected_feedback_version` (0 for the first judgment), `request_key`, `kind`,
`summary` and `read_ids`. Read IDs are sorted without changing the caller's
slice; duplicate IDs are rejected. Summary edges are trimmed before hashing.
Request keys accept 1–256 ASCII letters, digits, `_`, `-`, `.` and `:` and
should be opaque generated IDs, never conversation text or credentials.

Inside one SQLite write transaction, the service rechecks source/workspace
authorization, then looks up the original request key. The same normalized
request returns its original revision plus `replayed: true` and the latest
`current_revision`, even if newer feedback or evidence has arrived. Changing
intent with that key conflicts. A revoked or rebound source cannot use replay
to recover old data. Clients must not display an old replayed revision as the
current judgment.

For a new key, both observed versions must match before inserting. Concurrent
edits therefore cannot overwrite each other. The immutable revision, request
digest and signal catalog change commit together. Feedback revision is separate
from the signal's read-set version. History cursors bind signal, current source
identities and feedback revision; signal list cursors expire on feedback change,
while unchanged read pages retain their read-set cursor.

Withdraw by submitting `kind: withdrawn`, an empty summary and explicit `[]`
read IDs against the observed current versions. An initial withdrawal is invalid.
Withdrawal preserves history and never resets the revision to 0. There are at
most 1000 revisions per signal; the final slot is reserved for withdrawal.
Exact old-request replay remains available after that limit. History pages hold
1–100 records (MANAGE defaults to 50) and replace rather than append UI pages.

## Text handling and lifecycle

Feedback summaries are at most 2000 UTF-8 bytes, with NUL, other unsupported
control characters and formatting controls rejected. The service removes common
credential assignments, bearer tokens, private-key blocks, token forms and URLs
before persistence, and reports `summary_redacted`. The transformed text must
also fit the limit. These patterns are conservative and incomplete; they cannot
guarantee that arbitrary prose contains no secret. Operators should enter a short
description of the correction, not paste transcripts, endpoints or credentials.

Only a digest of the normalized original request is stored; the raw summary is
not duplicated in an idempotency payload or an error. Stored sanitized records
carry a content digest and are checked against the signal identity on read.
Root session deletion cascades through its signal into feedback history. If
evidence changes while the signal remains, the old judgment is retained as an
observation of that earlier version and `evidence_current` becomes false.

Database or feedback failures never alter the committed run outcome. They do
not block later user prompts or cause a tool replay. Reports of a missing skill
use the independent `SkillMissingReport` service and
[`skill.missing.*` management contract](../../acp/skill-evolution.md#missing-skill-reports);
they do not relax this feedback API's actual-read requirements. Repeated explicit
corrections now support bounded automatic selection, cooldown and generation
when separately enabled. Trusted attribution, other discovery triggers and
behavioral evaluation remain separate implementation work. Feedback revision,
withdrawal or deletion invalidates a selected reference before subsequent paid
stages and before explicit publication approval; it cannot create another vote
for an already consumed root/skill-version tuple.
