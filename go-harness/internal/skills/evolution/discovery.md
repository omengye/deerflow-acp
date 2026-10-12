# Automatic skill candidate discovery

This worker is an explicit opt-in to candidate generation. It does not establish
skill-quality causality or publish a skill. The metadata collector and feedback
submission remain separate operations. Only the existing generation worker
makes provider calls, using its independent maintenance limits and receipts.

## Eligible evidence

The selector groups the current `correction` report for the same source identity,
skill name and immutable content version across distinct tasks. Reports
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
input/job, consumed episode/target/version digests, UTC day reservation and
source/name cooldown. Accepted learning lineage defines independence where
available; otherwise the host conservatively uses root identity. The root-to-key
mapping remains pinned after source withdrawal. All new triggers share this
consumption key, and legacy root reservations still block reuse, including
another known root alias from the same episode. Revised
feedback cannot be spent again for the same task/target/version, including
after generation failure or cancellation. Selection counters are separate from
provider call/token accounting and do not refund on failure.

The pending cap includes active skill-maintenance jobs and unfinished proposals,
not extraction/consolidation/matching jobs with no skill target.
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
episodes (or roots when no learning lineage exists). Every root and descendant
must be completed and collected, with no
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

## Multi-turn experience patterns

`experience_patterns_enabled` defaults false and requires generation plus all
three explicitly configured learning model stages. A rotating cursor examines
at most eight completed current matches per pass. The only admitted relations
are `body_gap` and `trigger_mismatch` with one precise authorized existing enabled
target. Covered, ambiguous and irrelevant matches cannot generate. New-capability
matches require the further explicit creation opt-in described next.

Admission verifies the saved match/result, completed consolidation and its whole
shortlist, current pattern revision, every source consent and live redacted task
view, host access/catalog/head/overlay, and completed nontruncated local tool
observations actually cited by the member experiences. The configured repeat
threshold counts host independent episode keys with observations in the window.
Completion is unassessed evidence, never a quality vote or proof of skill causality.

Selection atomically reserves the existing shared pending/daily/cooldown capacity,
generation job, unified episode consumption and match/pattern dependency fence.
Only references/digests are stored in discovery; a bounded redacted procedure is
reconstructed transactionally before use. It is passed to the existing tool-free
generator as ordered UTF-8-safe fragments. This bridge allows only SKILL.md
changes, preserving every existing support file byte-for-byte. All content checks
and applicable trusted behavior suites still run. No suite means `not_run`.

Evidence is revalidated before paid calls, candidate/result commits, review and
manual approval. Disabling discovery permits manual approval of prior valid
proposals only while their learning source authority remains enabled/configured.
Changing consent, source receipts, current pattern or matcher/consolidator/access
identity fences old work. There is no implicit rebase. Saved candidate/proposal
recovery reuses generation output rather than charging for generation again.

Ordinary accepted-user activity in a captured episode is not evidence withdrawal.
Queued work waits through the shared metadata-only readiness hook. In-flight work
returns `source_active` only after all pinned source bytes, members, consolidation
shortlist, consent, catalog/head and saved projections still validate. Pure
transaction-scoped readers record activity without hiding permanent failures on
other sources. Terminal inputs missing their outbox are evidence damage, not a
temporary wait. Native adapters retain the host cause without provider details.
The original job retries within its three-attempt limit, first-claim deadline and
shared paid budget; readiness holds its next claim until sources settle. Prior
candidate, content and completed-case checkpoints are reused. Outputs rejected
before a legal checkpoint can need another charged call; no exactly-once model
I/O, refund, implicit rebase or revival of exhausted/stale work is promised.

These proposals default to human approval; old operator auto-patch alone cannot
publish them. The independent default-off `auto_patch.experience_patterns_enabled`
requires current experience discovery, host learning authority and behavior suites.
It admits only an existing enabled managed skill's small SKILL.md patch. Body
changes need targeted baseline assertion improvement and regression coverage.
Description edits need a trusted v2 positive/negative trigger suite, genuine
selection improvement and passing candidate execution cases; combined edits need
both improvements. Other frontmatter changes remain manual. All paths require
no regressions and no operational fixture errors. New capabilities and first import
adoption remain manual. Publication reuses normal approval's live source guard,
exact generator/suite/discovery configuration, shared daily quota and observation
lock. No parallel publication path or paid worker is introduced.

Revocation or session deletion clears transitively affected unpublished
generation/discovery/proposal and behavior text. It removes staged bytes only
when exclusively learning-created and not independently retained or adopted.
Accounting, consumed episode reservations and revoked dependency fences survive;
published/adopted skills remain user assets. Learning auto-patch observation stores
only pinned source references/digests, never procedure text or quality votes. Its
`valid_at_publication` source state is historical, not live matching authorization;
withdrawal after adoption is marked separately and never causes rollback by itself.
Monitoring and confirmed-quality rollback continue with saved limits when the
learning-specific gate is off but overall auto_patch remains on. Cached learning
history/consent UI is connected in source, not desktop-build or visually accepted.
Metadata-only trigger evaluation is implemented against a fixed host-owned
catalog; real semantic/trigger quality acceptance remains pending.

### Host suggestion review

Trusted SDK/MANAGE can ignore/restore one pattern or explicitly adjudicate an
existing runtime-visible target. Review CAS is independent of pattern revision
and consent epoch; both review version and current pattern revision are required.
Ignore persists across later revisions, cancels pending matching and withdraws
unpublished derived candidates. Raw experiences, costs, episode/target/version
consumption, lifetime creation reservations and adopted skills survive. Restore
neither requeues old matching work nor reactivates an old manual decision.

Adjudication fixes the full source/target/head/overlay after rebuilding live
authorized evidence and inventory. It can select a runtime-visible skill outside
the original eight-choice shortlist, without widening Names/tool/source policy.
Original model records remain immutable; `ladj_` records have
`origin=host_adjudicated`, `quality=unassessed` and no model job or fabricated read.
Conflicting, ambiguous and unresolved evidence stays blocked. Discovery retains
its separate opt-in and the independent episode threshold, except for separately
enabled human-review-only drafts described below. Shared capacity, cooldown,
busy checks and nonrefundable consumption remain unchanged. The immutable `host_adjudicated` flag
is bound to live provenance; even a passing behavior suite cannot auto-publish
these manually redirected candidates. They require explicit human approval.

Session-private and authorized workspace-sharing scopes are explicit. The acting
session's sharing grant is also revocable even if it is not an evidence member.
Revocation clears derived adjudication/receipt text with the existing transitive
forget mechanism, without deleting other sessions' raw experiences. These are
not ACP owner tools or new-capability/name/source retry grants. Original-key
receipts support only full-request replay after scope authorization; they are
historical outcomes, never current admission evidence.

### Automatic review-only experience drafts

`experience_review_drafts_enabled` is a separate default-off discovery grant,
requiring the complete learning/matching/generation chain. It lets ordinary
conversations produce proposals without `episode.start`, operator feedback,
manual target selection or a manual generation request. The source tree must
still be settled and authorized, with an observed local completion receipt,
no unresolved/conflicting/ambiguous pattern and an exact authorized match.
Covered/irrelevant skills do not generate work. New capabilities additionally
require the existing new-skill grant and non-builtin source.

Discovery first attempts the original repeated-episode admission. Only insufficient
observed support can fall back to review-only evidence. It consumes the real
episode/target/version keys, shared daily/pending/cooldown capacity and existing
maintenance budget atomically, never another vote or an extra worker. The fixed
discovery record has `review_draft=true` and fewer evidence entries than its pinned
repeat threshold. Auto-patch explicitly rejects it even when all evaluations
pass; no later evidence or configuration change upgrades that stored candidate.
Original fully qualified candidates keep their original evidence policy identity.

These are automatically generated SKILL.md proposals, not automatically published
skills. Explicit approval revalidates the same live sources. Withdrawal removes
unpublished derived content, but failed/rejected/withdrawn work never refunds
consumption. New-skill lifetime creation reservations remain unchanged; the
explicit retry API does not silently turn a one-episode draft into repeated
evidence. No new manual-authoring SDK, MANAGE or model tool is added.

### New capability creation

`experience_new_skills_enabled` defaults false and requires experience discovery
plus an explicitly configured generation-authorized, non-builtin
`new_skill_source_id`. The matcher may suggest a specific lowercase kebab-case
name (at most 56 ASCII bytes, at least two letter-bearing components, no opaque
hashes), never a path, source or grant. Legacy/custom results without a suggestion
remain shadow classifications. The Eino matcher v2 pins its updated prompt identity;
old results are not silently reused with a different adapter.

Creation reuses the same live provenance and independent episode threshold,
unless the separate review-draft gate permits a strictly human-reviewed draft. It
requires a complete matcher inventory and unrestricted runtime Names selection.
The host resolves source visibility for the canonical session workspace and its
explicit global-scope setting; it cannot fallback to another source. Installed,
disabled and tombstoned names in any visible source block creation, as do other
unfinished proposals/jobs with that name. It never chooses a new label to evade a
collision. A separate digest-only reservation permits at most one creation per
pattern, regardless of subsequent revisions, suggested labels or source changes.
Job, dependency, creation/episode reservations and quota commit atomically;
failed, cancelled, rejected and forgotten work does not refund consumption.

Generation v3 creates exactly one file, SKILL.md, using the host-fixed name and
absent base. Scripts/support files are rejected before staging. Security/quality
and applicable trusted behavior checks remain unchanged; no suite is `not_run`.
Live source/name/provenance checks run before calls, commit, review and approval;
newly installed names stale old proposals rather than rebasing them. These
proposals always require human approval, including with operator auto-patch on.

### Explicit host creation retry

Trusted SDK/MANAGE can prepare and explicitly enqueue another new-capability
attempt. The original generation must be terminal failed/cancelled, or completed
with a failed/rejected proposal. Active, pending-review, published and stale work
cannot be retried. Exact current pattern/review versions, previous generation and
proposal versions, original name/source, absent zero head and null overlay are
required. New paid work needs all original opt-ins, live authorized provenance,
independent receipts still within the repeat window, name availability, and shared
daily/pending/cooldown/maintenance capacity. There is no rename, source fallback,
revision upgrade, implicit rebase or automatic retry authorization.

The automatic lifetime creation reservation remains untouched. A separate
digest-only lifetime cap permits at most three host-authorized retries per pattern
and one successor per previous job. Original episode reservations remain owned by
the original discovery: retries reuse that evidence, never add votes or refund or
transfer consumption. Fresh generation/discovery inputs, host authorization,
review CAS and quota commit atomically. A generation-bound authorization digest
pins the original request and outcome; historical results are reconstructed.
Original-key replay returns historical IDs even with paid work disabled or an
ignored pattern, but cannot reenqueue or reactivate removed producers. Scope is
rechecked; source deletion removes derived authorization/receipt text while
keeping digest-only accounting. Nonmember workspace actors are also revocable.

Publication always requires human approval, the unchanged instruction-only
constraint, and live source/target/authorization checks. These endpoints are not
ACP extensions or model tools. Waku selects/refetches the previous job, prepares
complete conditions, and confirms a keyed enqueue through the existing cached
learning/MANAGE path. Unknown outcomes can replay only the full original request
against the original persistent service store.

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
existing generation/review panels and a bounded discovery browser. Experience
references are formatted on response acceptance, never with render-time I/O.
Dedicated bounded learning histories, session consent, host suggestion review
and explicit creation-retry controls are connected in source and isolated-state
tests. Desktop build/visual and real-model quality acceptance remain unfinished.
