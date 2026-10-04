# Eino Go skill management

The DeerFlow settings skill page queries the running Go registry. Individual
enable/disable buttons no longer edit the filesystem-derived config snapshot.
The global `skills_enabled` setting still belongs to the configuration draft.

## Catalog and commands

- `skill.list` displays one page of at most 20 entries, including disabled skills
  and deletion tombstones. Separate source identities disambiguate equal names.
- Selecting an entry uses `skill.get`. Details show scope, state version,
  builtin/managed source status, builtin overlay provenance, saved source changes
  and current export status. Reads and rendering do not open source directories.
- Enable/disable sends the complete observed target/head and a new request key.
  A successful reply is persisted and applies to new runs. Existing snapshots
  retain their selection. Deleted skills have no enable action.
- Source refresh stores a report without importing or publishing source changes.
  Export retry sends the observed state token and only queues asynchronous work;
  the user refreshes details to observe completion. An edited old export view is
  retained when the backend reconstructs a new one.

All requests use the existing background executor, request generation and daemon
IPC. Rendering reads cached projections. Cursor navigation replaces the displayed
page rather than appending an unbounded row collection. Publication changes mark
catalog navigation stale; exporter progress alone does not invalidate its cursor.

## Reviews and rollback

Pending and all-proposal lists use `proposal.list`; publication history uses the
independent `revision_cursor` from `proposal.history`. Each displays one bounded
page. Detail includes the diff, base/candidate digests, content review, behavioral
status, reviewer identity, model checks and reported usage. `not_run` is displayed
as unexecuted behavior evaluation, never as a passing test.

Approve/reject carry `expected_state_version`; approve also keeps the legacy
base digest. Rollback first reads `skill.get` for the historical entry's complete
target, then asks the user to confirm the observed current state and chosen
revision. The command includes target, source ID, full expected head and a stable
request key. A duplicate result does not publish another revision.

## Conflicts, retries and service changes

The internal Waku `manage-envelope` command preserves the authenticated MANAGE
`ok/data/code/error` envelope; older internal `manage` callers keep their existing
data/error behavior. A transport failure or malformed success response retains
the exact mutation request. Other reads may inspect current state, but cannot
discard that request or replace it with a new mutation. Retry reuses the original
key and preconditions; the user can explicitly end retry and refresh instead.

Conflicts and changed source authorization invalidate cached action state and
require a fresh read. Service stop/reapplication invalidates selection; a reply
from a superseded applied generation is not accepted as current state. Config
drafts remain intact. Unsupported backends do not fall back to old per-file
enablement writes.

## Remaining integration work

The shared v2 settings contract covers manual review, generation, repeated-correction
discovery, behavior evaluation, maintenance budgets and source editing. The
generation operation and behavior inspection panels are implemented in source.
Other discovery triggers, automatic publication and old history import remain
separate work. Source code tests and compilation do not prove a real
desktop interaction: end-to-end pointer/keyboard flows, actual service restart,
packaged daemon compatibility and release smoke remain P7 acceptance gates.

## Generation configuration

The native form adds a separate opt-in (off by default), configured generation
model selector, timeout/input/output limits and positive daily call/token,
job-duration and worker limits. A model name alone does not enable work.
Renaming a model updates all three evolution references. Parent and child field
contracts must match the config service and applied daemon; incompatible fields
cannot be edited/saved, and drafts are retained. Rendering reads cached values.

Settings take effect through save/apply; unsaved changes cannot modify the live
worker. Generation uses independent UTC daily maintenance accounting and the
same authorized skill destinations as candidate submission. Disabling it stops
new model work when applied and retains history. Successful generation/content
review still requires operator approval to publish. Applying generation alone
does not enable discovery; its separate opt-in is described below.

## Repeated-correction discovery configuration

The native form exposes `discovery.operator_corrections_enabled` (default false),
repeat threshold/window, cooldown, daily selections and pending-work limit.
Enabling it requires evolution and generation enabled, authorized destinations,
valid model routes and the existing positive maintenance budget. The shared
parent/child field contract gates edits and save. Applied settings control the
worker; unsaved drafts do not start work. The old Python `discovery.enabled`
field is migration-only and cannot activate this worker.

Only current explicit corrections with actual reads from distinct settled root
tasks qualify. Revisions on one root do not count as extra votes. Selection fixes
the skill head, source identity, evidence revisions and generation settings.
Withdrawing/changing feedback or deleting its task stops subsequent stages and
blocks publication from stale evidence. Completed reviews may still be explicitly
approved after automatic generation is disabled, provided the evidence and skill
head remain valid. Already published operations retain idempotent replay.

Automatic jobs appear in the existing job/receipt panel with their discovery ID;
their successful proposals use the existing manual review panel. The pending cap
counts all queued/running generation and unfinished proposals, without counting
the same active job and its proposal twice. Daily selection reservations and
consumed roots are not refunded after failure or cancellation. Source changes
and service changes follow the same cache invalidation rules as explicit jobs.
Detailed selector status and evidence references are available through
`skill.discovery.status/list`; a dedicated desktop decision browser is not yet
implemented. Attribution remains unassessed. Separately configured host behavior
suites apply to these jobs; without an applicable suite behavior remains
`not_run`. This stage does not authorize automatic publication.

## Behavior evaluation settings and inspection

The native form adds an explicit opt-in, host suite path, timeout, step cap,
input-byte cap and output-token cap. Both evolution and generation must be
enabled; the existing evaluation model uses independent contexts and the
maintenance budget. Saving and applying remain separate. Candidate files cannot
provide their own host suite. See the [Go behavior contract](../../go-harness/internal/skills/evolution/behavior.md).

Proposal detail includes the host-verified comparison report. Background MANAGE
queries load `proposal.behavior` and `proposal.behavior.receipts`; rendering reads
cached projections. Detailed variant outcomes use local pagination and receipts
use sequence pagination. Selection or service changes invalidate old responses.
Missing records are presented as unavailable history, without implying success.

Manual candidates return `validating` with a dedicated behavior job. The reject
action is available while validating and cancels unfinished work; approval is
available only after successful review. Failed or partial results and charged
usage remain inspectable. Create/delete reports identify the empty no-skill
control with an empty version. Evaluation never runs scripts or real external
tools and does not certify executable skill support files.

## Generation requests, jobs and receipts

The skill page has a separate operator form; its name/reason inputs are not
configuration fields and do not make the settings draft dirty. All operations
use the existing background MANAGE executor and cached projections.

1. Refresh generation capability from the running service. Its explicit source
   grants and configured budget determine available submission controls. The
   budget display describes limits, not remaining calls or tokens.
2. Select a source, enter the skill name and a bounded reason, then check the
   target version. `skill.generation.prepare` returns the full observed head,
   including an absent target, and builtin overlay provenance when applicable.
   The check cannot create a job or call a model. Builtin targets require an
   imported origin and an explicit overlay grant.
3. Submit candidate generation using that target/head and a fresh request key.
   Changing name, reason or source invalidates the prepared head. An in-flight
   check is bound to the editor generation. Unsaved/unapplied config prevents
   submission. A transport failure retains the exact original mutation for retry.
4. Refresh the all-jobs or current-skill page and select a job for detail. Each
   page holds at most 20 jobs. Service/capability changes mark navigation and
   actions stale; refresh restores them. Input text survives invalidation and
   a rebound source must be explicitly selected again.
5. Cancel an active job using its observed state version and confirmation, or
   open the associated proposal in the existing review panel. Completed jobs
   have no cancel action; successful generation still requires publication review.

Receipts use sequence pagination with at most 20 rows per page. They identify
the job/attempt, generation/security/quality stage and UTC accounting day,
reserved/charged/input/output tokens and whether usage is estimated. Response
validation rejects foreign jobs, malformed identities and inconsistent cursors.
No prompts, endpoint URLs or credentials are displayed. Refresh is explicit;
rendering never polls, starts a subprocess or reads files.

After generation is disabled and applied, refresh capability to browse retained
jobs/receipts for currently configured source identities. New automatic selection
also requires the separate applied discovery opt-in.

## Use evidence and operator feedback

The skill page has a separate cached signal/feedback browser. Refresh the signal
list, select one root/skill-version record and inspect its paged actual reads.
The original completed/failed/cancelled/uncertain counts describe task outcomes;
they do not prove that skill content caused success or failure.

Select 1–16 read references, potentially across pages, refresh the current
feedback revision, choose a report kind and enter a short summary. The form is
separate from configuration edits. Submit carries the observed read-set version,
feedback version and a stable request key. Source, skill content version and
root identity are fixed by the selected signal. Changing the signal, evidence
version or service revokes the selected references and submission state; the
unsubmitted summary text is retained.

Kinds distinguish helpful use/corrections from reports about network, quota,
permissions, cancellation and other issues. All remain operator reports with
unassessed causality. The UI's evidence-version match does not mean all work in
the root is complete; pending collection is displayed separately. The feedback
submission records the judgment; an explicitly enabled background discovery
worker may later use repeated corrections to generate a candidate. Publication
continues to require explicit review.

Current feedback and immutable revision history have separate refresh controls.
History, signal and read pages hold at most 20 entries. Withdrawal requires the
observed versions and confirmation, submits empty text/read references, and
keeps history. An exact retry can return an old applied revision with a newer
current revision; it is presented as a receipt and requires a current-state
refresh. Reads cannot clear a pending mutation. Explicitly ending retry discards
its cached mutation identity before another signal can be edited.

Summaries are limited to 2000 UTF-8 bytes. The service removes common credential
patterns and URLs before persistence and reports when it changed the text;
operators should provide concise corrections rather than paste raw transcripts
or credentials. Rendering uses cached projections and native controls, without
background polling or render-time request creation. Source-level tests do not
replace real desktop pointer/keyboard and release acceptance.

## Missing-capability reports

The skill page also accepts reports for real tasks that have not read a skill.
The selected chat's DeerFlow provider cursor supplies the native session ID;
local Waku turn UUIDs are never used as Go run IDs. Refresh discovers root tasks
through `skill.missing.targets`, then selecting a task reads its current report.
Pages show at most 20 tasks, ordered by original root-run creation time. The
displayed status belongs to that root run, not its entire descendant tree.
Refresh from the first page to discover newer tasks.

Submit sends the selected root run, current report revision (zero for no report)
and a stable request key. Summaries are limited to 2000 UTF-8 bytes. Withdrawal
uses the same observed revision and preserves history; revision 1000 is reserved
for withdrawal. Current reports and paged history may contain source runs from
real descendants reported through the SDK. The backend validates that membership.
Reports remain operator judgments with unassessed attribution and do not start
generation or publication.

Local/native session changes, task changes and service changes revoke cached
submission conditions while retaining the draft. Requests and responses check
the current session binding; stale confirmations cannot submit a prior task.
Transport failures retain the exact mutation for reconciliation, including when
the operator selects another chat. A replay receipt identifies the original task
and applied revision separately from the service's latest revision; it never
becomes the current report. Refresh is explicit and rendering only reads cache.

All skill mutation retries now retain their original config path and user-data
directory. A same-directory service restart may replay the original request;
switching to a different store cannot send it there. Ending retry explicitly
clears that binding. This is a local service identity check, not a multi-user
authorization mechanism. Desktop interaction and packaged delivery remain
separate acceptance gates.

## Source configuration

The cached settings draft has a separate `skill_sources` object, distinct from
the live skill catalog. Native fields edit import directory, export directory,
global selection and one selected managed source's ID/root/scope/workspace.
Add/remove operate only on the draft and follow the normal save/apply lifecycle.
The folder paths are relative to the configuration file unless absolute. All
directories must exist; the form does not create or scan directories at render.

`include_global: null` preserves native inheritance: import-directory profiles
include global skills by default, managed-only profiles do not. The source panel
shows this draft interpretation separately from running state. Scope and source
identity changes can make older tasks unavailable for restoration; removing a
source leaves its publication history and files in place. The backend checks
IDs, count, unknown fields, existing directories, links and workspace containment
before save and again at launch. The desktop requires matching parent/child
capabilities; missing or changed source definitions disable affected edits and
save while preserving the draft. Selection does not mark the draft dirty.
