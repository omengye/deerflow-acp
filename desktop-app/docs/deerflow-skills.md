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

The catalog, proposal lists and publication history show the most recent entries
first across pages. Catalog ordering uses the current installation update time,
proposals use update time with creation time as a fallback, and history uses
publication time. Missing timestamps sort last, with stable identity tie-breakers.
The host orders before applying the page limit; the desktop normalizes each
received page once, outside rendering. Old cursors from name/ID ordering must be
refreshed from the first page.

Installed rows and details include a direct Delete action. One click sends the
host-owned `skill.delete` command with the observed source/head and a stable
request key; it performs no model review and opens no confirmation dialog. A
successful reply removes the current row and refreshes pagination under the same
search/filter. Unknown results retain the original request for an idempotent retry.
Deletion removes the installation, including builtins, and persists across source
scans and restarts. Source files and immutable task snapshots remain intact;
deleted installation metadata stays available in the existing history filter.
Model-generated delete proposals continue to use their normal review workflow.

## Reviews and rollback

Pending and all-proposal lists use `proposal.list`; publication history uses the
independent `revision_cursor` from `proposal.history`. Each displays one bounded
page. Current candidates display the exact base, diff, security verdict, model usage
and publication receipt. The host publishes successful creations and updates automatically.
Historical records retain their behavior results and reviewer identity. A pending
candidate may be cancelled using its state version.

Rollback first reads `skill.get` for the historical entry's complete
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

## Native generation and publication

Native launch uses the primary model for generation and security review. The worker
inherits authorized destinations and rechecks the source, current head and exact
package before installation. Discovery uses eligible tasks to decide what to draft.
A candidate enters security review as soon as its package is available. Behavior
suites, quality review, trial runs and task observations are absent from native publication.

The desktop shows security review progress and publication status for current
candidates. Prior comparisons, observations and maintenance receipts remain
available with their original proposals.
[Candidate lifecycle](../../go-harness/internal/acp/skill-evolution.md)

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
   show the final security decision and publication receipt.

Receipts use sequence pagination with at most 20 rows per page. They identify
the job/attempt, generation or security stage and UTC accounting day,
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
submission records the judgment. Background discovery may use repeated
corrections to generate a candidate. The worker publishes the resulting
package as soon as its security review passes.

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

## Learning source inspection

Discovery uses eligible learning sources to decide when to generate a skill.
The worker submits each new package for security review and automatically
publishes passing creations and updates in the originating workspace.
Historical records marked `review_draft=true` retain their original
draft-only authorization.

The native learning-source view has an explicit read action on an experience or
one selected member of a current pattern. The authenticated local MANAGE request
is `skill.learning.source.get` with the native session ID and fixed
`source={kind,id,expected_revision,scope,experience_id}`. It grants no ACP/model
capability, model work, independent vote, review or publication authority. Historical
matches cannot silently rebase to a newer pattern through this view.

The service reconstructs the selected member's original bounded extraction view
and checks current contribution, captured input/frontier, settled tree, tool
receipts, actual skill reads and saved input/result/job identities. Workspace
inspection also requires the acting session and all pattern members' current
shared consent in the same workspace. Private retained sources remain inspectable
when paid learning is off. Desktop chats automatically contribute new tasks whenever
host learning is enabled. Changing workspace sharing or deleting a session clears
its unpublished learning sources.

Responses contain only the experience's cited redacted references, at most 32
entries and 512 KiB. Local observations, real-user input, lead-agent answer and
actual skill versions are distinct. External/MCP/unknown tools expose argument
shape only, never their result body. Hidden reasoning, delegated text, binary
attachments and raw errors are not part of this API. `checked_at_read` means the
selected member was checked then; quality remains unassessed and the cached view
cannot certify continuing authorization, full pattern/match evidence or causality.

The UI caches one preformatted detail, refreshed by explicit keyboard-native
controls. A new selection/read, page/scope/session/service change, source-changing
mutation or failed transport clears it; late responses cannot fill another
selection. Read failures create no pending mutation or replay receipt. Rendering
performs no I/O, and there is no new watcher or polling loop. Isolated state and
transport checks do not replace desktop type/build, daemon or visual acceptance.

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
