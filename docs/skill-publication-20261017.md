# Skill publication lifecycle repair (2026-10-17)

## Incident

The running desktop used the expected profile and custom skill root. The
`deepseek-search` directory was structurally valid for an installed skill, but
had no installed registry head. Its create proposal became `stale` during
publication: create admission considered only the registry, while the file
publication stage rejected any existing destination directory.

The daemon started before the directory was created. Runtime refresh updates
known source bindings; the desktop catalog refresh queries registry state. A
new directory on disk is therefore not proof of publication or availability.

The historical candidate contained only `SKILL.md`. The current directory also
contains `assets/config.json` and a different main file. An earlier user decision
cannot authorize these different bytes. Do not delete the configuration to make
the directory match, silently import it, or copy an old approval to a new version.

## Publication contract

1. Candidate creation does not install a skill. Drafts belong outside live
   installation roots. `skill_manage.files` accepts the complete UTF-8 package,
   including `SKILL.md` and all support files, for create/edit.
2. Automatic publication retains the existing policy gates, behavioral evidence,
   quotas and source authorization. Eligible automatic patches publish without a
   user decision. This change does not make all new skills automatically eligible.
3. A required user approval immediately attempts publication. A successful create
   writes or adopts the files, registers the complete version and enables it.
   There is no second import or enable step. Updates preserve existing enablement.
4. A missing destination can be created. An existing destination can be adopted
   without rewriting files only if its entire manifest matches the approved
   version. Different bytes, extra files, unsafe paths or an unstable source block
   publication. Files are verified again before indexing.
5. Approval and publication are independent durable facts. An immutable approval
   binds proposal, target, base, candidate, validation digest and original review.
   It survives later publication failure. Only completion of the file journal and
   registry transaction means `published`.
6. An in-flight execution or checkpoint keeps its pinned skill snapshot. The next
   normal turn in the same session selects the current registry and can discover
   and load a newly published enabled skill, subject to source/workspace scope.

## Recovery and operator feedback

- `df_skill_approvals` stores original decisions independently of mutable proposal
  status. Existing review serialization and idempotency hashes are preserved.
- Startup first resumes admitted file-publication journals, then imports configured
  sources. Once evolution policy is configured, it resumes approved proposals
  interrupted before journal admission. Recovery neither invokes review models nor
  approves new bytes. Removed/unavailable sources do not prevent host startup;
  malformed persisted approval identities fail validation.
- `df_skill_creation_guards` prevents startup import of unpublished create targets.
  Migration derives guards from historical non-published create proposals without
  changing source identities. Successful installation removes the guard atomically.
- Preflight conflicts persist a visible blocked proposal before any paid model
  checks. They do not leave an invisible guard with no recoverable proposal.
- `proposal.publish_retry` requires the original approval version and resumes that
  decision. It never accepts a replacement actor, note or candidate. Automatic
  decisions still pass current policy checks before new publication admission.
- `proposal.source_review` reads only a blocked create's configured source directory
  and submits a NEW complete candidate. The old proposal stays unchanged. Its
  stable request key replays the same candidate, not later disk contents. Content
  and behavior checks run under the existing review pipeline and budget policy.
- Current-directory review refuses credential-like local content before submission
  to model validators or inclusion in a new diff. Secret configuration needs
  explicit local handling and an external credential reference; it is never
  silently removed or transmitted by this recovery action.
- Management responses expose publication state separately from approval. Known
  approval failures include a sanitized durable proposal projection, not an
  ambiguous transport failure. The desktop distinguishes pending, blocked and
  completed publication, offers retry/source review, and can list terminal failures.

## Verification

Regression coverage includes absent/exact/different destinations, complete-package
hashes, non-rewriting adoption, missing adopted sources, startup guards and legacy
migration, durable decisions across audit/storage failure, crash recovery, removed
sources, approval binding integrity, automatic publication and original-actor retry,
localhost failure envelopes, and same-session next-turn skill discovery/body load.

Desktop tests validate publication feedback, original approval version retries,
source-review responses and pending-versus-completed publication. No visual UI test
or desktop restart is part of this change.

Completed checks:

- `go test ./internal/skills/... ./internal/localhost ./internal/launch -count=1 -timeout 300s`: 520 passed.
- `go test . -run 'TestPublication|TestPublishedSkill|TestSkillManageNative|TestSkillEvolution|TestSkillCatalog' -count=1 -timeout 300s`: 56 passed.
- `go test ./internal/desktopconfig -run 'Skill|Sources|Portable' -count=1 -timeout 300s`: 41 passed.
- `go test ./... -run '^$' -timeout 180s`: all packages compiled; this is not a full test run.
- `cargo test -p waku deerflow_settings --offline`: 148 passed, 437 filtered out.
- `git diff --check`: passed.

An earlier broader runtime test attempt timed out in `internal/desktopconfig`.
The focused configuration tests above passed, but a complete repository runtime
suite is not claimed. Fixture tests do not establish real DeepSeek API/network
availability or validate the currently running packaged desktop.

## Deployment boundary

Implementation and tests operate on source and temporary fixtures only. The running
packaged desktop, real profile database, skill configuration and credentials have
not been replaced or migrated by this task. Updating requires a matching desktop
and Go host build through the normal release/deployment flow.

After that update, the existing DeepSeek incident must use current-directory review
for the complete current package (after any required local credential handling).
The resulting new candidate follows normal automatic/manual policy. A successful
publication then becomes available to a subsequent normal session turn.

## Follow-up: source review controls

After deployment, the operator reported disabled publication/rejection controls
after choosing current-directory review. Read-only host inspection showed only
the historical stale proposal, not a new candidate. The UI had placed the second
confirmation at the top of the settings page, away from the action that opened it.
The old proposal's disabled controls were correct but did not explain this state.

The follow-up desktop change places source-review confirmation beside the action,
labels it as submission rather than publication, displays state-specific review
eligibility, keeps source-review failures beside the affected proposal, and adds
an explicit detail refresh. Successful submission immediately selects the new
candidate before fetching detail. No review gates are weakened.

Read-only local credential checks also blocked both files in the current DeepSeek
package. The local config contains a non-placeholder credential value. No secret
values were printed, no source-review mutation was replayed, and no real skill
files were modified. Local credential handling is a separate prerequisite, not a
reason to enable the stale proposal's publish control.
