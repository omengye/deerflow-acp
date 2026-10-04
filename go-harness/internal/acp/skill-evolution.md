# Local skill proposal management

The Go SDK and authenticated DFACP/1 `MANAGE` channel use the same SQLite-backed
application service. These are operator operations, not standard ACP methods or
model tools. No HTTP server or Python runtime is used. The daemon's existing
token authentication is required before the management request is accepted.

## Available operations

Responses retain the desktop envelope `{ "ok": true, "data": ... }`; errors
use `{ "ok": false, "code": "...", "error": "..." }`. Expected errors include
`conflict`, `not_found`, `permission_denied`, `validation_failed` and
`invalid_request`. Unexpected internal/provider errors are not sent verbatim.

| Operation | Request fields in addition to `operation` | Response data |
| --- | --- | --- |
| `proposal.list` | Optional `source_id`, `name`, `status`, `cursor`, `limit` | `proposals`, `catalog_version`, `next_cursor`, `truncated` |
| `proposal.history` | List fields plus optional `revision_cursor` | List data plus `revisions`, `revision_next_cursor`, `revision_catalog_version`, `signals`, `probations`, `capabilities` |
| `proposal.get` | `proposal_id` | Full proposal including `diff`, `base`, `base_sha256`, `current_head`, `state_version`, `evaluation`, `evaluation_sha256`, `target`, optional `overlay_base` |
| `proposal.approve` | `proposal_id`; optional `expected_state_version`, `expected_base_sha256`, `note` | `proposal`, `catalog_version` |
| `proposal.reject` | Same review fields | `proposal`, `catalog_version` |
| `proposal.rollback` | `name`, integer `version`; optional `source_id`, `expected_head`, `request_key`, `note`, full `target` | `revision`, `catalog_version` |
| `skill.list` | Optional `source_id`, `name`, `status`, `cursor`, `limit` | `skills`, configured `sources`, `catalog_version`, optional `next_cursor` |
| `skill.get` | Full `target` | `skill`, `catalog_version` |
| `skill.set_enabled` | Full `target`, full `expected_head`, boolean `enabled`, stable `request_key` | `skill`, `catalog_version`, original `applied_head`, `replayed` |
| `skill.imports.refresh` | Full `target`, full `expected_head` | `skill`, `catalog_version` after one advisory filesystem check |
| `skill.export.retry` | Full `target`, full `expected_head`, `expected_export_token`, stable `request_key` | `skill`, `catalog_version`, original `applied_head`, `replayed` |
| `skill.signals.list` | Optional `source_id`, `name`, `cursor`, `limit` | Root/version `signals`, `catalog_version`, optional `next_cursor` |
| `skill.signals.get` | `signal_id`; optional `cursor`, `limit` | `signal`, bounded `reads`, optional `next_cursor` |
| `skill.feedback.get` | `signal_id` | `signal`, optional current `feedback`, `evidence_current` |
| `skill.feedback.list` | `signal_id`; optional `cursor`, `limit` | Bounded immutable `revisions`, `current_revision`, optional `next_cursor` |
| `skill.feedback.submit` | `signal_id`, `expected_signal_state_version`, `expected_feedback_version`, `request_key`, `kind`, `summary`, `read_ids` | Applied `feedback`, latest `current_revision`, `replayed` |
| `skill.missing.submit` | `session_id`, `run_id`, `expected_report_version`, `request_key`, `kind`, `summary` | Applied `report`, latest `current_revision`, `replayed` |
| `skill.missing.get` | `session_id`, `run_id` | Derived `root_id`, `root_session_id`, optional current `report` |
| `skill.missing.list` | `session_id`; optional `cursor`, `limit` | Current `reports`, `catalog_version`, optional `next_cursor` |
| `skill.missing.history` | `session_id`, `run_id`; optional `cursor`, `limit` | Immutable `revisions`, `current_revision`, optional `next_cursor` |
| `skill.missing.targets` | `session_id`; optional `cursor`, `limit` | Real root-task `targets`, optional `next_cursor`; includes tasks with no report or skill reads |
| `skill.collector.status` | No additional fields | Worker state, pending/completed/failed counts, first 100 unresolved jobs, `truncated` |
| `skill.collector.jobs` | Optional positive `after_outcome_id`, `limit` | Unresolved `jobs`, optional `next_outcome_id` |
| `skill.collector.retry` | Positive `outcome_id`, `expected_state_version` | `queued: true` after a failed job's CAS reset |
| `skill.generation.status` | No additional fields | `version: skill-generation-v1`, `enabled`, granted `sources`, optional configured `budget` |
| `skill.generation.prepare` | Full `target` | Observed `target`, full `base` (including absent targets), optional `overlay_base` |
| `skill.generation.enqueue` | Full `target`, full `expected_head`, stable `request_key`, nonempty `reason` | Generation job; author is fixed to `desktop` |
| `skill.generation.list` | Optional `source_id`, `name`, `cursor`, `limit` | `jobs`, optional `next_cursor` |
| `skill.generation.get` | `job_id` | Generation job and saved phase/proposal reference |
| `skill.generation.cancel` | `job_id`, positive `expected_state_version` | `cancelled: true` after a job/proposal transaction |
| `skill.generation.receipts` | `job_id`; optional positive `after_sequence`, `limit` | `receipts`, `last_sequence` |

### Operator-requested generation

SDK hosts and the daemon enable durable generation explicitly. Configuring
manual review alone does not enable this worker. Given a `deerflow.Config` with a configured Registry
source `project` and resolved model routes, set these fields before `Open`:

```go
validator, err := deerflow.NewSkillModelValidator(ctx, harness.SkillModelReviewConfig{
    Moderation: moderationRoute,
    Evaluation: evaluationRoute,
    Timeout: time.Minute,
    MaxInputBytes: 131072,
    MaxOutputTokens: 1024,
})
if err != nil {
    return err
}
cfg.SkillValidator = validator
cfg.SkillGeneration = &harness.SkillGenerationOptions{
    Model: harness.SkillGenerationModelConfig{
        Route: generationRoute,
        Timeout: time.Minute,
        MaxInputBytes: 131072,
        MaxOutputTokens: 8192,
    },
    Budget: harness.SkillMaintenanceBudget{
        MaxDailyModelCalls: 30,
        MaxDailyTokens: 500000,
        MaxJobSeconds: 300,
        Workers: 1,
    },
    Sources: []string{"project"},
    AllowBuiltinOverlays: false,
}
```

The routes are `harness.ModelRoute` values, with credentials resolved by the
host for the selected provider. Do not put model
credentials in requests, reasons or job metadata. The generator uses a bounded
tool-free context. Custom validators must implement `SkillVersionedValidator`;
the Eino reviewer already does. Source grants are explicit and independent of
model `skill_manage` grants. Builtin changes require an explicit overlay grant.

All budget fields are required and positive: daily calls 1–100000, daily tokens
1–1000000000, job seconds 1–3600 and workers 1–16. Limits cover generation and
both reviews across all sources in the same database. There is no unlimited
zero value. Each provider request reserves its estimated input and output cap
before I/O; missing usage, interrupted requests and provider failures retain a
conservative charge. Restart does not clear budget or attempt counts.

First read `skill.generation.status` for the running host's explicit source
grants and configured budget. These are limits, not remaining usage; disabled
generation returns an empty source list and no budget. Each source is a full
target identity with an empty name. Set the desired name and pass that target
to `skill.generation.prepare`. Preparation validates the grant and exact
source/workspace identity and returns an observed head without enqueueing or
calling a model. Builtin sources require the overlay grant and an imported
origin; create new skills in a custom or managed source.

Enqueue uses the prepared `target` and `base` as `expected_head`. The head must
include `version`, `state_version`, `exists`, `enabled` and `managed`, including
their zero/false values for an absent skill. `skill.get` is for catalog entries
and returns `not_found` for a genuinely absent skill; do not synthesize its head.
Preparation does not reserve the head: enqueue checks it again transactionally.
The request key binds this target/head and operator intent. Identical retries
return the original job even after publication; changed intent conflicts.
Queue capacity is bounded and can return `resource_limit`.

Input and queue insertion are atomic. The job pins source/workspace identity,
head and overlay base, generator identity and reviewer identity. It saves the
complete immutable candidate and then a proposal reference before review.
Retries reuse those committed stages. Each new model call rechecks source,
configuration and head within budget admission. A stale worker cannot commit
progress or release reserved usage. A committed review may be acknowledged
without model calls after an interrupted job finalization, including when the
operator already published that proposal.

Job states are `queued`, `running`, `retry`, `completed`, `failed` and
`cancelled`. `phase` identifies `generating`, `validating` or `review_complete`;
it is not the publication status. Successful checks leave the proposal
`pending_review`. Quality failure blocks approval. Configured host behavior
suites compare both versions in this same job. Without an applicable suite,
behavior remains `not_run`; automatic publication is not enabled.
Use `proposal.get` and existing approve/reject/rollback operations for releases.

Cancellation requires the observed job version, invalidates its claim and
rejects an associated validating/pending-review proposal atomically. It cannot
undo a published proposal or cancel an already completed job. For completed
jobs, manage the proposal directly. Generation failure does not fail a user
conversation, create a user run or replay a tool. SDK shutdown cancels and joins
active generation/model work before closing the collector and database.

Lists accept 1–100 entries (MANAGE defaults to 50), filter by source/name, and reject
proposal status filters. A cursor is bound to the query, current configured
source identities and generation catalog revision; refresh after `conflict`.
Receipt pages use the last sequence as the next `after_sequence`, omitting it
on the first page. Receipts contain digests, stage, attempt and usage, without
prompts, endpoints or credentials. Queries/cancellation remain available when
generation is disabled, for currently configured source identities only.
Removing or rebinding a source revokes access to its old jobs.

SDK equivalents are `SkillGenerationStatus`, `PrepareSkillGeneration`,
`QueueSkillGeneration`, `GetSkillGeneration`,
`ListSkillGenerations`, `CancelSkillGeneration` and `SkillGenerationReceipts`.
`proposal.history.capabilities.generation` reports explicit host configuration;
it is false by default. `discovery` reports the separate repeated-correction
opt-in described below; `probation` remains false. Explicit jobs use the submitted
reason and pinned base. Automatic jobs also use current, sanitized correction
evidence and report `discovery_id`. Trusted attribution remains pending.

#### Host-defined behavior evaluation

Optional `skill_evolution.behavior` loads a trusted JSON suite separately from
candidate files. It requires evolution and generation enabled and uses the
existing evaluation model in independent contexts. Six native settings control
enablement, suite path, per-variant timeout, steps, input bytes and output tokens.
The same independent maintenance budget covers all content and behavior calls.
See the [full contract and suite example](../skills/evolution/behavior.md).

Generated candidates use their original maintenance job. Manual submissions
return `validating` with `behavior_job_id`; their dedicated job has no generated
candidate request. Rejecting an unfinished manual proposal cancels that job.
Passing comparisons still require explicit publication approval. Failed
applicable evaluation cannot become `not_run`; no matching suite can.

| Operation | Request fields | Response |
| --- | --- | --- |
| `proposal.behavior` | `proposal_id` | Evaluation/job identity and state, attempts, deadline/retry time, content-review status, bounded variant results and compact report |
| `proposal.behavior.receipts` | `proposal_id`, optional `after_sequence` (nonnegative), `limit` (1–100, default 50) | `proposal_id`, `receipts`, `last_sequence` |

These authenticated read-only operations enforce current source authorization,
including after evaluation is disabled. They expose digests and host checks,
without fixture inputs, expected answers or model messages. SDK equivalents are
`SkillBehaviorEvaluation` and `SkillBehaviorReceipts`. At most 96 variant results
are returned; receipt pages use the last sequence for continuation.

#### Automatic selection from repeated corrections

`Config.SkillDiscovery` adds bounded `harness.SkillDiscoveryOptions` to an SDK
host with generation already configured. In YAML and Waku, the corresponding
opt-in is `skill_evolution.discovery.operator_corrections_enabled`:

```yaml
skill_evolution:
  # Also enable evolution and generation and configure their models/budget.
  discovery:
    operator_corrections_enabled: false
    repeat_threshold: 2
    repeat_window_days: 30
    cooldown_hours: 24
    max_daily_proposals: 5
    max_pending_proposals: 20
```

All five numbers are integers. Defaults are shown above; ranges are respectively
2–16, 1–365, 1–8760, 1–100 and 1–1000. Explicit numeric null and fractions are
rejected. The flag defaults false and requires both evolution and generation
enabled. The old Python `discovery.enabled` field never enables this worker;
it is retained only for explicit migration/diagnostics. Saving and launch share
validation, and Waku exposes native controls gated by the same field schema.

Only current `correction` feedback on actual reads from distinct settled root
tasks qualifies. Source/name/content version must match an enabled installed
head. Cancelled, unsettled, uncollected or effect-uncertain work is ineligible.
Revisions within a root do not add votes; attribution remains `unassessed`.
Selection fixes source, full head, overlay, feedback/read-set revisions and
options; job creation, consumed-root digests, daily reservation and cooldown
commit together. It uses the existing independent generation budget. Failure or
cancellation does not refund daily selection or consumed roots. The pending cap
counts all active generation and unfinished proposals, counting an active job
and its linked proposal once.

Evidence is checked again before each paid stage, candidate commit, proposal
attachment/review and explicit approval. Withdrawal, revision or deletion
cannot authorize later calls from stale references. An already in-flight call
may finish, but its stale output cannot advance. Options/model changes fence
new paid work. Disabling automatic work still permits explicit approval of a
fully reviewed candidate whose evidence, head and authorization remain valid.
Already published approval replay remains idempotent. Generation never publishes
automatically; separately configured behavior suites also apply to these jobs.

| Operation | Request fields | Response |
| --- | --- | --- |
| `skill.discovery.status` | None | Applied enable/running state, supported triggers, options, source grants, UTC day, daily reservations, pending count, bounded diagnostic counters/code |
| `skill.discovery.list` | Optional `source_id`, `name`, `cursor`, `limit` | `discoveries` and optional `next_cursor` |

These are authenticated read-only MANAGE operations, not model tools or standard
ACP methods. There is no management `skill.discovery.run`. Lists accept 1–100
entries (default 50), reject unknown fields, explicit null, wrong types and
status filters. Cursors bind current source identities/filter and the last
discovery ID; results use live keyset ordering by ID, not a frozen chronological
snapshot. Refresh from the first page for newly inserted lower IDs. SDK methods
are `SkillDiscoveryStatus` and `ListSkillDiscoveries` (explicit limit required).

Each discovery includes its ID, target, full base, generation ID, creation time,
`kind=operator_corrections`, `attribution=unassessed`, evidence references and
the live job state/proposal ID. Snapshots store references/digests, not feedback
text. Sanitized summaries are resolved for dispatch from current feedback.
Source removal/rebinding revokes old records; disabling selection retains
history for still-authorized sources. `last_error_code` is the last recorded
diagnostic, not proof of a currently failing worker. See the
[discovery implementation contract](../skills/evolution/discovery.md).

#### YAML and Waku generation settings

The config service and daemon share the native decoder and field schema. Waku
has native model/number/boolean controls with the usual save/apply lifecycle;
no unbounded JSON editor is used. To enable generation in a profile with configured
skill sources and the three named model routes:

```yaml
skill_evolution:
  enabled: true
  mode: review
  allow_builtin_overlays: false
  generation_model_name: generator
  moderation_model_name: safety
  evaluation_model_name: quality
  security_fail_closed: true
  generation:
    enabled: true
    timeout_seconds: 60
    max_input_bytes: 131072
    max_output_tokens: 8192
  validation:
    timeout_seconds: 60
    max_input_bytes: 131072
    max_output_tokens: 1024
  budget:
    max_daily_model_calls: 30
    max_daily_tokens: 100000
    max_job_seconds: 300
    workers: 1
```

`generation.enabled` defaults to false. A historical `generation_model_name`
alone does not activate work. Generation requires evolution and skills enabled;
the model name must resolve to the named configuration even if two configurations
share a provider model ID. The generation timeout/input/output fields accept 0
for native defaults (60 seconds / 131072 bytes / 8192 tokens), otherwise the
ranges are 1–300 / 1024–2097152 / 128–32768. The positive `budget` fields must
all be supplied when generation is enabled in YAML. Templates/draft defaults
show the bounded example values above. 0 is never an unlimited budget.

The source grant is the existing candidate destination set: custom imports and
explicit managed sources, plus builtin sources only with `allow_builtin_overlays`.
Generation permits at most 32 such sources. Workspace identity and overlay checks
remain in the service. Disabling evolution or generation prevents new model work
after the configuration is applied, retaining jobs/history and current sources.
Old jobs retain their pinned policy and are also bounded by stricter current
daily limits. Resuming an unfinished job with changed model identity fails its
configuration check rather than silently switching models.

Save/validate reject new invalid values even while disabled, and launch rejects
invalid active configuration. Unchanged dormant legacy values remain available
for explicit conversion. Unknown nested keys are preserved as migration input,
never treated as effective controls. Model renaming updates all three references.
Waku checks matching parent and child field descriptors from both config tool and
running daemon, preserves invalid drafts, and performs no render-time I/O.

The desktop generation panel uses these MANAGE operations through its existing
background executor. The operator refreshes capability, selects an explicitly
granted source, enters a skill name and reason, checks the current head and
submits generation. Editing the draft invalidates that preparation. Service
changes revoke cached action state while preserving input text. Submission is
disabled until configuration changes have been saved and applied.

Job and receipt lists show at most 20 rows per page with explicit refresh and
navigation. Detail shows state/phase, attempts, retry/deadline and fixed error
codes. Cancellation carries the observed job version; completed jobs link to
the existing proposal review flow. Transport failure retains the original
mutation and request key, allowing inspection and exact retry. Receipt views
show reserved/charged usage and estimated versus actual accounting, never
prompts, endpoints or credentials. History remains available when generation
is disabled. Enabling the setting does not automatically select signals or
generate a proposal. Real desktop interaction and release acceptance remain
separate from the source-level tests.

### Observed skill signals

Signals aggregate actual validated reads within one root task and one immutable
skill version. `attribution` is currently `unassessed`; success/failure/cancel and
uncertainty counts are not quality decisions or authorization to publish. The
collector never invokes a model. `collection_pending` is a live hint about
unfinished root runs or outcomes, not a generation-job status.

Signal/evidence pages accept 1–100 entries (default 50). Source filters match
current Registry identities; removing or rebinding a source revokes these query
results. Cursors are bound to the query and recorded revision; refresh from page
one after conflict. Evidence contains only host identities, read kind, relative
path, hash/size, observation time and outcome flags. It omits skill contents,
prompts, tool arguments and private provider/database errors.

Queue pages use the returned positive `next_outcome_id` as `after_outcome_id`.
They represent live unresolved work, so completed/deleted jobs may disappear.
Failed count is a subset of pending count. A failed page can be retried only
with its current version; duplicate/stale retry conflicts and requires refresh.
This requeues local evidence processing and never replays a user tool or prompt.

SDK equivalents are `ListSkillSignals`, `GetSkillSignalEvidence`,
`SkillCollectorStatus`, `ListSkillCollectionJobs` and `RetrySkillCollection`.
The legacy `proposal.history.signals` slot stays empty until its desktop
presentation is migrated; use these versioned evidence queries. Discovery,
behavioral evaluation and probation are not advertised as available merely
because the collector is running. Operator generation has its own explicit
configuration and capability above.

### Operator feedback

Read `skill.signals.get` for actual read references and `skill.feedback.get`
for the current judgment. A submit includes all seven fields explicitly;
missing/null fields are rejected. `expected_feedback_version` is 0 for the
first judgment. Non-withdrawal feedback requires 1–16 actual read IDs and a
nonempty summary of at most 2000 UTF-8 bytes. The service derives target,
immutable skill version and root identities; clients cannot supply an actor or
target. Kinds are `helpful`, `correction`, `network`, `quota`, `permission`,
`cancelled`, `other`, and `withdrawn`. Withdrawal needs an existing judgment,
empty summary and explicit empty read list.

Both signal and feedback versions are checked for new writes. Exact request-key
replay is checked after authorization and before version CAS. A replay can return
an older applied revision alongside a newer `current_revision`; refresh current
state before presenting it as current. Revision history is bounded and paged,
with the final capacity slot reserved for withdrawal. Source rebind/removal
revokes reads and retries. Feedback changes do not alter the read-set version.

Summaries are scrubbed for common credential forms and URLs before storage;
`summary_redacted` reports this. The scrubber is not an arbitrary-secret detector;
do not paste raw conversations or credentials. Raw summaries are not persisted
in request payloads or errors. Feedback retains `origin: operator`, and signal
attribution remains `unassessed`. Neither external-error reports nor negative
judgments alone count as skill degradation. `evidence_current` refers only to
the read-set version, not complete root outcomes or quality approval.

SDK equivalents are `SubmitSkillFeedback`, `GetSkillFeedback` and
`ListSkillFeedback`. See the [feedback persistence contract](../skills/evolution/feedback.md)
for idempotency, history, deletion, text handling and remaining attribution work.

### Missing skill reports

`skill.missing.*` records an operator's judgment that a task needed a reusable
capability. No skill read, installed target or generated candidate is required.
A task that read other skills can also report a different missing capability.
Reports retain `origin: operator` and `attribution: unassessed`. They do not
create read signals, establish a skill failure, run models or publish skills.

The six submit fields are required and cannot be null. `session_id` must be
the existing foreground root session. `run_id` identifies a real saved root
run, background descendant or continuation; the service derives its budget
root and verifies the session assertion in the same transaction. Private child
session IDs and forged session/run pairs are rejected. Foreground role invocation
IDs are not run IDs. The authenticated local management token remains the
authority, so reports can be managed after an ACP connection has disconnected.
These operations are not exposed as standard ACP methods or model tools.

Use `skill.missing.targets` to discover a task without entering internal run
IDs. It returns root run creation time, root run status and current report
revision, with no prompt or tool contents. Tasks are ordered by root run
creation time descending and then root ID descending. Later child activity or
continuations do not reorder an older root. This is a live keyset-paged list,
not a frozen snapshot: refresh from the first page to see newly created tasks.
Its cursor is bound to the foreground session, and each returned identity is
verified against the saved run/budget relationship.

Each root task has one current report with immutable revisions. Submit
`kind: missing_skill`, a nonempty summary and `expected_report_version: 0` for
the first report. Use the current revision for a correction, or
`kind: withdrawn` with an empty summary to withdraw an existing report. Root
and descendant submissions revise the same judgment and must not be counted
as independent votes. The report's `run_id` retains the run referenced by that
revision; its root identity is derived by the host.

`request_key` is an opaque stable ID using the feedback key format. After
validating current task membership, an identical request replays the original
revision and returns the latest `current_revision`, even after later changes.
Reusing a key for different intent conflicts. New requests require version CAS.
The report history has at most 1000 revisions, with the final slot reserved for
withdrawal. Summaries use the same 2000-byte validation and credential/URL
scrubbing as observed feedback; raw summaries are not stored in replay data.

List and history pages are limited to 1–100 records (MANAGE defaults to 50).
List cursors bind the foreground session and catalog version; history cursors
bind the root report and its revision. Refresh after `conflict`. Root-session
deletion removes its private reports and history transactionally. Report write
failures do not change run outcomes or block subsequent prompts.

SDK equivalents are `SubmitMissingSkillReport`, `GetMissingSkillReport`,
`ListMissingSkillReports`, `ListMissingSkillReportHistory` and
`ListMissingSkillReportTargets`. Waku's task-selection, report and history form
is connected in source. Trusted failure classification, automatic selection
and generation from these judgments remain separate work. A report does not authorize a future
candidate's destination; generation must validate the current target grant.

### Catalog and durable enablement

`skill.list` status is `enabled`, `disabled` or `deleted`; proposal status names
are not accepted here. Tombstones remain manageable. Entries expose `target`,
`head`, optional live `record`, `builtin`, `managed_source`, `overlay_base`,
`import_observation`, `export` and `can_refresh_import`. Sources describe current
configured policy, including sources with no installed skills. Bodies, raw source
file paths and export-root paths are not returned. Export directories are relative
views, not installation paths. Detail/list queries only access SQLite.

Catalog cursors bind query filters, source identities/policies, export-root
identity and the publication catalog revision. Any changed publication revision
returns `conflict`; restart from page one. Advisory import/export progress does
not change that revision. Page size is 1–100 (MANAGE defaults to 50); metadata
pages are bounded to approximately 2 MiB, and clients must follow `next_cursor`.

These new commands require explicit full preconditions; missing, null or unknown
fields are rejected. `target` has `source_id`, `source_identity`, `name`, `scope`
and, for workspace sources, `workspace`. `expected_head` includes all five fields
shown in the rollback example below. MANAGE assigns actor `desktop`; clients
cannot inject an actor. An authenticated operator can manage configured sources;
this is not exposed as a model tool or standard ACP method.

Enablement becomes durable managed state in the same transaction as its audit
row and export queue. This preserves the content version and original import
baseline; changing a builtin enable flag does not create a content overlay.
Startup imports cannot reset that flag. Existing execution snapshots retain
their pinned version and selection. Deleted heads cannot be enabled.

Request keys are scoped to source/name and bind the complete operation intent.
A duplicate after restart or a later command returns the original `applied_head`
alongside the current `skill`; it never re-applies the old command. Reusing a key
with different fields conflicts. Export retries additionally require the observed
`export.stateToken` as `expected_export_token`; worker progress or export-root
rebinding invalidates old tokens. A replay does not reset worker attempts again.
Retries queue local asynchronous work, not a completed export or publication.

Import refresh reads only the saved managed import origin and stores a bounded,
sanitized report. It does not import, publish or execute observed files. Generated
skills without an import origin report `can_refresh_import: false`.

SDK equivalents are `QuerySkillCatalog`, `GetCatalogSkill`,
`SetSkillEnabledChecked`, `RefreshSkillImport` and `RetrySkillExport`.

### Proposal pagination

Limits default to 50 and accept 1–100. List entries omit the full diff. A cursor
is opaque, binds its query filters and configured source identities, and cannot
be reused for another list kind. A caller refreshes from the first page to see
new proposals. History has independent proposal/revision cursors and catalog
epochs, since these two pages need not be from the same transaction. Responses
are bounded; clients must follow cursors instead of assuming one page is complete.

Source identity includes its canonical root, scope and workspace. Removing or
rebinding a configured source prevents old records from being listed or managed
under the new source. An omitted rollback source is accepted only when exactly
one currently configured source has history for that skill name.

New clients should submit the proposal's observed `state_version`. Rollback
should submit the complete observed `current_head` and a stable request key:

```json
{
  "operation": "proposal.rollback",
  "source_id": "project",
  "name": "research",
  "version": 1,
  "request_key": "user-rollback-request-id",
  "expected_head": {
    "version": "<observed content SHA-256>",
    "state_version": 5,
    "exists": true,
    "enabled": true,
    "managed": true
  }
}
```

Waku first obtains the current target/head through `skill.get`, then presents the
rollback confirmation. It also sends the full `target` on rollback. When supplied,
the server verifies it against the configured source identity and requires all
expected-head fields plus a request key; a reused source ID cannot redirect the
old dialog to a different source. Legacy requests without `target` retain their
previous compatibility behavior.

Legacy requests without these fields capture the current state on the server
and still publish with CAS. They cannot detect an already stale UI observation.
An explicit request key plus expected head provides retry idempotency across
restarts. Reusing that key for a different request fails with `conflict`.
The integer revision selects content; it is distinct from a monotonic head state
version and the content SHA-256. Rollback creates a new revision.

## SDK candidate creation and review

The host configures `Config.SkillValidator`, either an implementation of
`harness.SkillValidator` or `deerflow.NewSkillModelValidator` with explicit
moderation/evaluation model routes and bounded input/output/timeout. Review calls
have no tools or conversation history. A missing or failed validator leaves the
candidate failed; it cannot be approved. Quality judgments are review evidence,
not behavioral regression results. The provided model reviewer reports behavior
`not_run`, cannot certify scripts, and cannot approve automatic publication.

Operator flow:

1. `GetSkillHead(source, name)` obtains the full expected head.
2. `SubmitSkillProposal` supplies that base, a stable request key, reason and
   complete files for create/edit, or a supported mutation. Submission stores
   immutable candidate content, then runs validation outside the SQL transaction.
3. `GetSkillProposal` / `ListSkillProposals` expose status, diff and evaluation.
4. `ApproveSkillProposal` / `RejectSkillProposal` require a state version, actor
   and optional note. Approval atomically updates the head, proposal and audit.
5. `ListSkillHistory` / `RollbackSkill` preserve all old immutable snapshots.

Mutation semantics:

- `create` with `Content` creates `SKILL.md`; `Files` can instead supply a whole
  package. The expected target must be absent.
- `edit` with `Content` replaces `SKILL.md` and retains support files. `Files`
  means a complete replacement package, including deliberate support-file deletion.
- `patch` requires nonempty exact `Find` and a `Replace` pointer, which may point
  to empty text. An explicit positive `ExpectedCount` must match all occurrences;
  when omitted, the first occurrence is replaced, matching the legacy contract.
- `write_file` and `remove_file` target a canonical relative support path and
  cannot modify `SKILL.md` or escape the package.
- `delete` creates a reviewed tombstone, retaining immutable rollback versions.

Mutation input is materialized against the requested immutable base. Retrying an
already published request reconstructs its original candidate and returns the
saved result without rerunning validation. Mixed complete-file and mutation
arguments are rejected. Python dependencies/files, credential-like literals and
invalid local references fail before model review.

## Model candidate tool and manual review configuration

`Config.SkillProposalSources` is the explicit source-ID allowlist for model
candidate submission. A validator is required. The native `skill_manage` tool
is available to authorized root agents, foreground roles and background roles;
plan/read-only runs and tool-policy denials omit it. Role tool and pinned skill
subsets are checked again at invocation. Workspace sources must match the run's
workspace; global sources also require `SkillSelection.IncludeGlobal`.

The model can propose `create`, `edit`, `patch`, `write_file`, `remove_file` or
`delete`, or `get` its own proposal's status. It cannot approve, reject, roll
back, choose authorship or provide a base version. The host takes the live head
only when it still matches the pinned visible skill version. New skills require
an absent target. Model-generated proposal IDs and permissions cannot grant
access to another session's or role's proposals.

Root proposal ownership follows the session across runs/restarts. Role ownership
follows its logical invocation across continuations; it does not transfer to the
parent or a later invocation of the same role. Host-derived request keys bind
the run, tool call, author and canonical arguments. Duplicate candidate requests
return the durable proposal without another validation. Submission permission
and operator publication review are separate: permitting a tool call never
publishes the skill.

All actual moderation/evaluation model calls inside `skill_manage` share the
run's durable budget, usage sink and cleanup lifecycle. Ordinary validation and
provider failures return a failed candidate result to the agent. Admission or
settlement storage errors still fail the owning run. `live_skill_changed: false`
describes the effect of this tool call, including a `get` of an already published
proposal. It is not the current publication state; consult `status` and operator
history for that.

The daemon and desktop config validator currently support this manual subset:

```yaml
skills:
  enabled: true
  path: ./skills # must contain an ordinary custom directory
skill_evolution:
  enabled: true
  mode: review
  allow_builtin_overlays: false
  moderation_model_name: primary
  evaluation_model_name: quality
  security_fail_closed: true
  validation:
    timeout_seconds: 60
    max_input_bytes: 131072
    max_output_tokens: 1024
```

Both model names must exist in `models`; they resolve by configured name even
when providers share a model ID. This automatically grants the existing custom
source by default. Set `allow_builtin_overlays: true` to also grant reviewed
changes to existing builtin skills; this requires an ordinary `skills/public`
directory. The historical IDs `python-custom` and `python-public` are retained
for stored source/checkpoint identity; no Python is invoked. Disable clears
model submission grants and disables generation/discovery. Active `auto_patch`
and other unimplemented fields are rejected with a config
path instead of silently accepted. Disabled legacy fields remain data awaiting
explicit migration. Waku has a native manual-review form behind the shared
settings contract; generation has the separate opt-in and budget described above.
Real desktop acceptance remains pending.

## Builtin overlays and import observations

SDK hosts mark distribution source IDs with `SkillsConfig.BuiltinSources` and
separately grant model candidate access through `SkillProposalSources`. The
builtin policy is excluded from source identity, preserving historical skill
refs. A builtin proposal records `overlay_base`, the immutable original import.
Approval stores the overlay relation and moves the managed publication pointer
in the same transaction. Source files remain untouched. The active skill keeps
the same source/name/scope, so existing role selections continue to work.

SDK `ListSkills` exposes `builtin`, `managed`, `stateVersion` and `overlayBase`.
Proposal detail/list and release history expose `overlay_base`; a baseline
revision from before overlay publication may have no overlay field. Deletion
keeps both versions and provenance; operator rollback can restore either the
original content or an earlier reviewed overlay. Removing the current builtin
tag does not erase an existing overlay relation. No arbitrary new builtin name
can be created without a previously imported origin.

Changing builtin grants invalidates affected pending execution checkpoints.
Tagging a source builtin after a proposal was reviewed does not silently rewrite
its provenance: approval marks that old proposal stale and requires a new
submission/review. A candidate tool approval only submits the candidate; it does
not activate the overlay.

`Open` records bounded source observations for previously imported managed
skills, including tombstones and packages missing from the current install list.
`RefreshSkillImports` repeats the check; `ListSkillImportObservations` returns
saved reports filtered by workspace/global/name selection. Reports compare
against the original imported version, not the active overlay. Status is
`unchanged`, `changed`, `missing`, `invalid` or `unavailable`, with the check time
and at most 100 sorted changed paths/digests (`truncated` when needed). File
contents and physical import paths are not copied into reports. These are
advisory snapshots, not a filesystem watch or permission to import content.
They do not move the publication pointer or consume publication state versions.
Filesystem problems are reported without disabling the managed skill;
cancellation and database/integrity failures remain errors.

Observation APIs exist in the SDK and `skill.imports.refresh`; Waku displays the
saved observation and offers explicit refresh. Rendering does not scan sources.

## Managed sources and export status

SDK `SkillsConfig.ManagedSources` and the explicit `--skills-config` JSON
profile can register SQLite-only destinations, with a required existing
`ExportRoot`. A source root binds scope and identity but is never imported.
The runtime review configuration grants candidate submission to declared managed
sources; disabling evolution removes grants without deleting those sources or
their releases. The native tool identifies managed sources as preferred
destinations for new skills.

Approval/rollback and managed enablement/deletion enqueue an export in their
publication transaction. The SDK's local worker creates a verified directory
for that head; filesystem failures cannot turn a published proposal back into
a failed/unpublished proposal. `ListSkillExports` exposes `pending`,
`synced` or `failed`, head/state version, relative directory, attempts,
sanitized error code and next retry time. Use only a synced directory for its
reported head. The view contains `publication.json` and a `files/` package
for live skills; tombstones have no package.

Automatic file retries stop after three failures and survive restarts.
`RefreshSkillExports` explicitly requeues verification/failed exports; its
return indicates queuing, not completion. Startup verifies previously successful
views and reconstructs missing/tampered ones from SQL. Old disk edits remain in
their old view; they are never imported as skill updates. Closing the SDK joins
the worker. This does not provide cleanup of old/crash-orphaned views.

See [explicit startup configuration](../launch/skills.md). YAML also supports
`skills.managed_sources`, `skills.export_root` and `skills.include_global`.
MANAGE and Waku expose source observations and export status/retry controls.
The old `skill_evolution.storage_path` is not an active metadata store; historical
data import remains pending and is distinct from configuration conversion.

## Current limits

The operator service, native candidate storage, model-scoped `skill_manage`,
manual-review YAML wiring, builtin overlays, import observations and content
reviewer, managed sources and recoverable export views are available. Durable
operator generation, independent maintenance budgets, metadata collection and
bounded SDK/MANAGE queries are implemented. Waku has catalog/proposal pagination,
source/state preconditions, evaluation display and native manual settings; see
[desktop contract](../../../desktop-app/docs/deerflow-skills.md).

Generation YAML/save/launch/schema/native controls and the desktop generation
request/job/receipt and observed-feedback panels are connected in source.
Operator missing-skill reports have SDK/MANAGE endpoints and a cached desktop
task-selection/report/history form in source. Repeated explicit corrections
have an opt-in automatic selection worker, native settings and provenance in
the existing job panel. Automatic missing-report routing, repeated-workflow
discovery, trusted feedback/quality attribution, probation
and automatic rollback remain pending. History advertises discovery according
to the applied opt-in and probation as false; legacy signal/probation arrays
remain empty. Use explicit signal and discovery queries for those records.

This API does not claim an end-to-end desktop skill-evolution delivery. Existing
portable packages are not replaced by source changes.
