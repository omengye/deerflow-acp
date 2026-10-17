# File-authoritative installed skills

## Scope and acceptance

The explicitly bound directory defines the current skill. SQLite contains
identity, enablement, indexes, hashes, audit/review and recovery state, not a
hidden current file body that can override that directory. Ordinary new turns
re-read packages twice and compare manifests before pinning them. Missing,
invalid, changing or transaction-owned directories are unavailable, with host
diagnostics; no database or other same-name source fallback is allowed.

Execution snapshots and explicit interrupted resumes remain immutable. New
candidate/snapshot bodies are content-addressed files in `skill-snapshots-v1`
beside the database (or the SDK's explicit `SnapshotRoot`). Historical inline
SQLite bytes remain readable only for backwards-compatible pinned history.
Startup does not restore those historical bytes into user directories.

Builtin sources are read-only. Customize through an explicitly installed custom
copy, never a hidden builtin overlay. Legacy database-only skills without a
verified directory binding are unavailable until explicitly migrated by a host.

## Work plan

- [x] Inspect registry, publication, permission, recovery and snapshot contracts.
- [x] Correct existing-skill source resolution and rejected-tool outcomes.
- [x] Add inspection, bounded multi-file preparation and explicit application.
- [x] Add source write journal, conflict checks, idempotent recovery and reindex.
- [x] Integrate permissions, metadata, next-turn guidance and host APIs.
- [x] Replace hidden publication/rollback overlays with actual file changes.
- [x] Verify focused registry, tool, permission, replay and recovery regressions.
- [x] Run the complete Go regression coverage and packaged protocol smoke.
- [x] Back up the stopped profile, replace affected Go EXEs and verify restart.

## Conversation editing

`skill_inspect` resolves the run-visible source identity, directory, pinned and
current versions. `skill_edit prepare` stages a bounded batch of replacements,
whole-file writes or JSON-pointer field edits on existing files. It does not
write the installation. `skill_edit apply` always requires a host permission
decision for the exact immutable candidate and redacted diff, even under
permission-off or allow-always session policies. The engine injects confirmation;
model JSON cannot assert it. Background/role agents do not receive this authority.

Success requires `status=applied`, `live_matches=true` and `source_matches=true`.
Prepared/rejected/conflicting requests and edits to workspace copies are not
installation success. Tool errors are recorded as failures/not-executed rather
than successful receipts. JSON field edits preserve unrelated credentials;
previews redact detected secrets, but this is not a universal secret scanner.

Installed-mode validation accepts existing Python files, checks structure, and
never executes scripts. The stricter evolution/security review remains separate:

```
deerflow-skill-go validate --path <installed-directory> --mode installed
```

## Publication and recovery

Approval first commits a journal and `publishing` state. Only verified directory
installation plus index commit changes the proposal to `published`. History's
`file_status` distinguishes `pending`, `applied`, `aborted`, and `legacy`.
Exports are derived views, never the authority. Rollback also writes real files.

Direct edits preserve displaced original inodes in `.deerflow-edit-backups`;
whole-package publications move originals to `.deerflow-publication-backups`.
Exclusive rooted renames/links do not overwrite racing targets. Changed bytes
stop recovery. Windows file DACLs are preserved; staging and backups are outside
the installed package and are excluded from discovery. Journals block new-turn
loading until reconciled. Startup retries admitted operations, not prepared drafts.

Authenticated host management APIs, not model tools:

- `skill.edits.list` / `skill.edits.reconcile` (`resume` or `abort`).
- `skill.publications.list` / `skill.publications.reconcile` (`resume` or `abort`).
- `skill.imports.refresh` now reindexes the bound directory, including unmanaged
  imports, with a full target and expected-head check.

Abort requires the operator to restore the exact base package first (absence for
a create); it never overwrites external changes automatically. Backups and orphan
staging files are retained, not recursively deleted. Automatic garbage collection
is not implemented; retained-version/byte limits fail closed instead of falling
back to stale content. Catalog metadata is the last index check, not a watcher;
new turns, startup and explicit refresh recheck existing bindings. New unbound
directories still require explicit installation/startup discovery.

## Constraints

Preserve unrelated working-tree changes and existing source identities. Do not
execute edited scripts or use paid model calls for regression fixtures. Do not
claim behavior tests passed when only structural validation ran.

Deployment requires a passing regression gate. Build the four Go executables
into staging, drain/stop the authenticated daemon without interrupting work,
back up EXEs plus the stopped database/config/skills/object store, then replace
only affected Go EXEs. Preserve Rust/UI binaries and all existing hotfix records.
Restart with the same config/runtime paths; verify build ID, hashes, activity,
fanren-rag source path/current file manifest, and unchanged credential-bearing
files. A binary-only downgrade is not a data rollback: restore the matching
stopped-profile backup, including object files, when rolling back migration.

## Deployment evidence

Installed in `dist/desktop/DeerFlow-Desktop-Go-20261010-103944` on 2026-10-15.
Build ID is `deerflow-go-file-authority-20261015-dev`; replaced only
`deerflow-acpd.exe`, `deerflow-acp-go.exe`, `deerflow-config-go.exe`, and
`deerflow-skill-go.exe`. Rust desktop/daemon/bridge binaries are unchanged.
The package records hashes and details in `SKILL_FILE_AUTHORITY_20261015.json`.
Backup is `dist/desktop/backups/skill-file-authority-20261015-181522`.

Verification is a composite regression gate: the full Go run passed all packages
except an obsolete desktopconfig assertion that publications must not write
source files. After updating that assertion, the entire desktopconfig package
passed. Final core packages passed, and the root SDK suite passed all 138
top-level tests across five batches. The full runtime package passed in 993s;
an extra 32-test batch exceeded its cumulative five-minute timeout and passed
when rerun with a twelve-minute limit. Both default and legacy desktop protocol
smokes passed, with no visual or paid-provider testing. Native Bridge tests passed
using an isolated executable copy (the live package's adjacent config interferes
with config-less test helpers).

The user-authorized searxng migration copied its enabled version to
`user-data/skills/custom/searxng-web-search`, backed up the old same-name package,
enabled that explicit custom source and disabled the database-only source. Its
historical file bodies were externalized as hash-checked objects; `df_skill_files`
contains zero inline body bytes for that skill. Audit metadata/history remain.
Both searxng and fanren are ready, with exact directory versions verified.

Restart must explicitly preserve `--data-dir .../user-data/data/go-harness`.
The old process received that path from its environment. The first readiness
launch omitted it and selected the config-adjacent state directory; no prompts
were run. It was stopped and corrected before final verification. The final
process uses the original database, retaining all 87 sessions. Config and fanren
file hashes match the backup. This operational correction is recorded in the
deployment manifest, not hidden as a successful first attempt.
