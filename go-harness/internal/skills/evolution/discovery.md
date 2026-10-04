# Automatic skill candidate discovery

This worker is an explicit opt-in to candidate generation. It does not establish
skill-quality causality or publish a skill. The metadata collector and feedback
submission remain separate operations. Only the existing generation worker
makes provider calls, using its independent maintenance limits and receipts.

## Eligible evidence

The selector groups the current `correction` report for the same source identity,
skill name and immutable content version across distinct root tasks. Reports
must reference actual reads and match the current signal read-set version.
Current feedback revisions, read membership, root ownership and settled outcomes
are checked in the selection transaction. Cancelled or effect-uncertain work,
unfinished descendants, pending collection and out-of-window reports do not
qualify. Raw failed-run counts are not quality judgments. Attribution remains
`unassessed` in evidence and model instructions.

The installed head must exist, be enabled and have the same content version.
The full head and builtin overlay base are fixed with the destination source
identity. Generation and candidate source grants are required; builtin changes
require the existing explicit overlay grant.

## Durable selection

One transaction stores the discovery, reference snapshot/digest, generation
input/job, consumed root digests, UTC day reservation and source/name cooldown.
Consumed identity includes root, source, name and content version. Revised
feedback from that root cannot be spent again for the same version, including
after generation failure or cancellation. Selection counters are separate from
provider call/token accounting and do not refund on failure.

The pending cap includes all active maintenance jobs and unfinished proposals.
An active generation job and its attached proposal count once. Completed jobs
with pending-review proposals still occupy capacity. There is also one active
generation/proposal per destination skill for automatic admission.

Each pass reads at most 32 signals and advances its durable cursor after each
attempted item. Unprocessed tails remain available after interruption. Malformed
evidence is retried at most three times per unchanged signal/feedback revision;
changed revisions may be inspected again. Queue capacity is normal backpressure.
Transient storage or clock errors use a delay without permanent quarantine.

Snapshots contain references and digests, not copies of correction text. The
generator resolves the existing sanitized summaries only after validating the
references. It receives those summaries as untrusted evidence, without tools
or authority to change the target, publish or execute code. Pattern-based
redaction retains the limitations documented in [feedback.md](feedback.md).

## Stage guards and review

Before every new paid stage, candidate commit, proposal attachment/content
review and manual approval, the service verifies evidence revisions and live
read/root membership. Revision, withdrawal, session deletion and source/head
changes invalidate the old authorization. An in-flight call cannot be undone,
but changed evidence prevents its result from advancing.

Automatic spending additionally checks the configured options and generation/
reviewer identity. Disabling discovery prevents further automatic model work.
It does not revoke the operator's ability to approve a fully reviewed candidate
with valid live evidence and head. Approval replays of an already published
proposal remain idempotent. A generation worker recovering an already committed
terminal review acknowledges that result without another provider call.
If explicit approval detects changed/deleted evidence, it marks the pending
proposal `stale` in the same transaction and returns a conflict. That releases
pending/target capacity; consumed roots and daily reservations remain spent.
Transient storage failures retain the proposal for retry.

Successful generation creates a `pending_review` proposal with its discovery
trigger and ID. Existing content checks still run;
configured [behavior suites](behavior.md) compare both versions in the same
maintenance job. Without an applicable suite, behavior stays `not_run`.
Publication follows the separately configured review/auto-patch policy. New
skills always require explicit approval. Candidate generation failure does not change a user run, replay tools
or block the next prompt.

## Missing capabilities and completed workflows

`missing_reports_enabled` and `successful_workflows_enabled` are independent,
default-off triggers. They require generation plus `new_skill_source_id`, an
authorized non-builtin destination. Workspace-scoped sources must match the
actual root session workspace. No automatic source selection or import occurs.

A current explicit missing report can create one candidate without any prior
skill read. Its immutable snapshot references the root/report/revision, and
the sanitized report is loaded only when generation runs. Revisions and
withdrawal invalidate prior in-flight work and pending approval. The destination
name is `missing-` plus a summary digest prefix; only a never-installed target
is eligible. Existing skills and tombstones are not overwritten.

Completed workflows require 3–32 successful committed tool receipts and at
least two distinct tools, repeated across the configured number of independent
roots. Every root and descendant must be completed and collected, with no
uncertain effect, permission rejection, failed receipt or reconciliation.
Delegation and skill management are excluded. The fingerprint contains ordered
tool names and parameter key/type shapes, never argument values, prompts or
tool output. It proposes `workflow-<digest>` guidance, without replaying the
tools or treating completed execution as proven skill-quality success.

A rotating durable cursor examines at most 16 roots per pass. All triggers
share the existing daily/pending limits, per-target cooldown and maintenance
budget. Workflow snapshots retain receipt identity/version and are rechecked
before paid work, candidate commit and approval. Missing and workflow proposals
carry `kind=missing_skill` and `kind=repeated_workflow` in `skill.discovery.list`.
Repeat threshold applies to corrections/workflows; missing reports need one
explicit operator report. Both derived triggers retain `attribution=unassessed`.

## Configuration and inspection

The native flag is `skill_evolution.discovery.operator_corrections_enabled`,
default false. The legacy Python `discovery.enabled` cannot enable spending.
Save, validate, launch and Waku use the shared field schema. See the
[configuration and management reference](../../acp/skill-evolution.md#automatic-selection-from-repeated-corrections)
for bounds and read-only `skill.discovery.status/list` operations.

The SDK starts discovery after collector/generation initialization. Close cancels
and joins discovery before closing generation, collector and the database.
Restart retains selection, consumed roots, cooldowns, daily reservations and
generation progress. Read access follows current source identities even when
discovery is disabled.

Discovery inventory is bounded, live keyset pagination by ID. Feedback deletion
removes its actual text/history; discovery keeps only references/digests and
cannot use them to regenerate deleted text. There is no public selector trigger
operation. Waku exposes settings and automatic job provenance through its
existing generation/review panels; a dedicated discovery browser is still absent.

Other discovery triggers, trusted failure attribution, automatic publication,
probation and automatic rollback remain unfinished.
