# Immutable skill registry

> Current authority contract (2026-10-15): the bound source directory, not a
> database overlay, defines the current skill. New turns/startup/explicit refresh
> reindex files; invalid or missing sources do not fall back to history. New
> bodies live in content-addressed files. Publication/rollback writes the real
> directory, with a durable journal and explicit recovery. Builtins are read-only.
> See `../../../docs/skill-direct-edit-20261015.md` for the current protocol and
> migration rules. Historical descriptions of managed SQL overlays and advisory
> import-only refresh below describe the old design and are superseded.

This package provides an explicit local-source registry and an Eino
`adk/middlewares/skill.Backend`. It uses independent `df_skill_*` tables in the
caller's SQLite database. It owns its source-directory handles, never the DB.

## Integration

```go
registry, err := skills.NewRegistry(ctx, harness.SkillsConfig{
    Sources: []harness.SkillSource{{
        ID: "project", Root: absoluteSkillRoot,
        Scope: harness.SkillScopeWorkspace, Workspace: absoluteWorkspace,
    }},
}, db)
// Handle err; close registry before the caller closes db.

record, err := registry.Install(ctx, "project", "research")
// Inspect record.Findings and handle err. New installations start disabled.
err = registry.SetEnabled(ctx, "project", "research", true)

selection := harness.SkillSelection{Workspace: absoluteWorkspace}
snapshot, err := registry.Snapshot(ctx, selection)
handler, err := skill.NewMiddleware(ctx, &skill.Config{
    Backend: snapshot,
    BuildContent: snapshot.BuildContent,
})
// Include snapshot.ReadFileTool() in the same agent's tools.
// Store snapshot.Refs() atomically with run/checkpoint configuration.
```

The host assembles the handler and tool per run and applies its usual budget,
tool permission and subagent policies. `model`, `agent`, `context`, `tools`,
`allowed-tools`, and `permissions` frontmatter fields are rejected in this V1
profile, including null/empty declarations. Enabling inline skills cannot
automatically change the model or create a fork. Future support requires an
explicit allowed model/agent resolver sharing the parent budget and permissions.

## Sources and scopes

- There is no implicit HOME, cwd, environment, global, or repository scanning.
  `Sources` must be explicitly configured; `Install` is an explicit operation.
- `Install(sourceID, relativeDir)` installs exactly one package directory. `.`
  can be used when the source root itself is a single package. Otherwise the
  directory basename must match the frontmatter name.
- A workspace source must be inside its configured canonical workspace.
  Global sources are visible only with `IncludeGlobal: true`.
- Nil selection names means all enabled visible skills. An empty non-nil slice
  means none. Missing explicitly selected names fail visibly.
- Workspace names shadow global names, including when the workspace installation
  is disabled. Duplicate names in the same selected scope fail as ambiguous.
- Source identity hashes the source ID, normalized root, scope, and workspace.
  Reusing an ID for a different root/scope cannot restore its previous refs.
- Source roots and files reject symlinks, nonregular files, unsafe relative paths,
  case collisions, reserved device names, excessive depth/entries, and VCS
  metadata. Reads use pinned `os.Root` handles and opened-file identity checks.
  Installation sources should be quiescent; this is not a filesystem transaction
  over concurrently edited source trees or an OS sandbox.

## Versions and lifecycle

Installation reads files under bounded limits, validates `SKILL.md`, scans local
content, and commits an immutable version plus the current pointer in one SQL
transaction. The version is the SHA-256 of a sorted manifest containing each
relative path, byte count, and file SHA-256. Identical content is idempotent.
Reinstallation preserves the existing enabled flag.

`List` returns management metadata, including disabled installations. `SetEnabled`
and `Delete` affect future snapshots. Deletion removes the installed pointer and
keeps old versions for checkpoint recovery. `Restore(selection, refs)` loads the
exact saved versions after checking current source identity, selection and scope;
it never silently uses the latest version or a different same-named source.
Old snapshot refs deliberately survive disable/delete. For a host-level immediate
revocation, remove the source/skill from the current restoration selection policy.

There is no implicit version garbage collection. Retained versions count toward
the storage/version limits; checkpoint-aware pruning is a future management API.
`Snapshot.Refs`, `List`, `Files`, and file reads return independent values.

## Reviewed publications and managed installations

`PrepareCandidate` validates in-memory packages and owns a deep copy. It rejects
ambiguous file/directory and case collisions as well as the regular package
limits. It neither runs support scripts nor approves content for publication.

`WithPublication` owns a SQLite transaction and a configured source identity.
`Stage`, `Current`, `ReadVersion`, `Publish` and `CatalogVersion` share that
transaction with proposal and publication audit writes through `SQL()`. Do not
reenter the registry/database connection pool or commit the transaction inside
the callback. Rollback is automatic on any callback error. `ReadVersion` verifies
the stored metadata, exact manifest and all file hashes before using a version.

Every source/name head has a monotonic state version in addition to its content
digest. Publication CAS therefore detects an intervening disable, deletion or
rollback even when the content digest returns to its old value. A publication
marks the installation managed. `InstallConfigured` honors the durable managed
pointer, enabled state and tombstone on restart, including when the old import
package has disappeared. Explicit `Install` refuses to overwrite a managed
installation. Prior checkpoint snapshots still restore their pinned content.

`internal/skills/evolution` uses these same version tables for candidates and
revisions. It implements complete create/edit candidates, main-file content
edits, exact patches, support-file writes/removal, deletion, validation, review,
conflict/stale handling, integer release history and rollback. The first edit
captures the imported baseline so it is also a rollback target. Input intent,
evaluation digest and displayed diff are checked against immutable versions.
Duplicate submissions, reviews and keyed rollbacks do not publish twice.

The public operator SDK and authenticated MANAGE channel share that service;
see [skill management protocol](../acp/skill-evolution.md). Model tools do not
inherit these operator APIs. Discovery, maintenance budgets,
automatic publication and Waku integration
are still separate work; the operator service does not enable those capabilities.

`deerflow.NewSkillModelValidator` supplies explicit Eino security and quality
content reviews, each with fresh context and no bound tools. Input/output/time
limits are enforced and provider errors are sanitized. Reports retain model,
prompt/config version, input digest and usage evidence. Behavior remains
`not_run` and scripts remain ineligible unless a separate trusted host validator
actually performs isolated behavioral checks. A nil validator fails closed.

The SDK exposes candidate submission through the native `skill_manage` tool only
for explicit `Config.SkillProposalSources`. It checks workspace/global source
scope, current attempt identity, role tool/skill subsets and pinned content.
Candidate authors and idempotency keys are host-generated. Model reads are limited
to the same author; the separate authenticated operator service manages reviews.
Review calls inside a run share that run's durable model budget and usage events.
No model tool changes the live publication pointer. Ordinary reviewer failures
produce failed proposals; budget-storage errors remain runtime failures.

The daemon and desktop config validator share strict parsing for the implemented
manual-review YAML subset. See [the protocol and configuration guide](../acp/skill-evolution.md).
Discovery, automatic publication and Waku review UI
integration remain separate pending work.

## Managed sources and recoverable exports

`SkillsConfig.ManagedSources` declares SQLite-only sources using existing
explicit source roots/scopes. It does not change identity hashes and rejects
overlap with builtin policies. Such sources never permit `Install` or
`InstallConfigured`, so neither old directories nor an exported package can
silently create or resurrect a live skill. Creation uses reviewed candidates.
An explicit, existing `ExportRoot` is required for managed sources.

All managed heads (including builtin overlays and tombstones) enqueue export
state atomically with pointer movement when an export root is configured.
`ReconcileExports` reconstructs missing entries and rechecks previously
synced views. `StartExportWorker` is owned by the SDK; `Close` cancels and
joins it before closing directory handles. Standalone registry hosts may call
`SyncExports` explicitly instead.

The worker reads and verifies immutable SQL content, performs bounded filesystem
work outside the transaction, and CAS-attaches a completed directory to the
same head. Superseded work cannot attach its old version to a newer publication.
Source and export-root identities qualify all records. Readers use
`ListExports` (which does no file I/O) and the returned current head/status.
There is no mutable filesystem current pointer competing with SQLite.

Materialization uses pinned `os.Root` directories, refuses links and writes
only newly created random views with exclusive files. Every file is synced and
the complete manifest/package is verified before status becomes `synced`.
Views contain `publication.json` plus `files/` for live heads. Deleted
heads export only the publication tombstone. Filesystem errors persist a
sanitized code and bounded retry state; three failed attempts require explicit
retry. Restart preserves exhausted retries and verifies successful views.
Changed views are retained while a new one is built from SQL.

Database/outbox failures within publication roll back the whole publication;
later filesystem errors never undo it. Post-publication database or immutable
content failures remain errors, and the background worker logs a sanitized
diagnostic with backoff. Exports do not invoke scripts, make model calls or
certify behavior. Old/crash-orphaned views are retained pending explicit future
cleanup. See [startup configuration](../launch/skills.md) for SDK/CLI wiring.

## Builtin overlays and source changes

`SkillsConfig.BuiltinSources` marks configured distribution sources without
changing their identity hash. `PublicationTx.OverlayBase` resolves the verified
original imported version. Proposals retain that reference, and `Publish`
persists the relation atomically with the managed head. The overlay uses the
existing immutable source/name namespace; distribution files are never rewritten.
Rollback and tombstones retain provenance and historical snapshots. `List`
includes builtin/managed flags, the head state version and optional overlay base.

`ObserveImports` checks only known, previously imported managed packages. It
includes deleted publications and packages no longer present in the startup
profile. Bounded source reads yield saved unchanged/changed/missing/invalid/
unavailable observations; changes contain at most 100 sorted paths and hashes.
The baseline is the last imported version, independent of the current overlay.
Reads happen outside the SQL transaction; persistence rechecks the import
identity and never changes the publication head or catalog revision. Concurrent
older checks cannot replace a newer saved observation. Context cancellation and
storage or immutable-content corruption are returned as errors.

`ListImportObservations` reads these saved reports with the same source scope
selection as `List`, including reports for managed tombstones. SDK startup calls
the observer once; explicit refresh is available. No background watch, automatic
source import or Waku source-diff view is implied. Generated skills without an
import origin have no source report; their disk views are reconstructed through
the separate export mechanism above.

## Operator catalog and commands

`QueryCatalog`/`GetCatalogSkill` read bounded metadata projections over configured
source identities, including tombstones, overlays, import reports and export
state. Queries use one SQLite snapshot and never read skill bodies or source
directories. Keyset pages order by name/source identity; cursors pin configured
source policies, export root, filters and catalog revision. Publication changes
invalidate the cursor; advisory worker updates do not.

`SetEnabledChecked` compares the complete observed target/head and records its
request key, actor, intent and applied head atomically with durable managed
enablement and export queuing. An import profile cannot undo that decision on
restart. This flag-only operation does not create builtin content overlays.
Historical SDK `SetEnabled` retains its original import-profile semantics;
desktop callers use the checked operation.

`RefreshCatalogImport` checks one stored managed origin outside SQL and returns
the saved observation without changing publication. `RetryExportChecked` binds
the head plus a token covering export-root identity and persisted worker state.
Its reset and audit commit together. A repeated request returns the original
applied head and current projection without repeating the mutation. Export worker
settlement also checks its observed timestamp, so a late attempt cannot overwrite
a newer queued retry with the same head and attempt count.

These are host/operator APIs; they do not grant model publication permission.
The authenticated local MANAGE operations are documented in
[`skill-evolution.md`](../acp/skill-evolution.md). Waku has source-level catalog,
review and generation controls; real desktop/release acceptance remains separate.

## Observed use and operator feedback

The run-owned snapshot observer records actual validated skill/support-file reads.
The durable collector groups them by root task, source identity and immutable
skill version. It never treats metadata listing or a completed run alone as proof
of skill quality. The evolution feedback service binds an operator judgment to
those linked reads, with independent immutable revisions, two version checks and
request-key replay. Source authorization is rechecked for queries and retries.

Feedback text is bounded and scrubbed for common credential patterns, without
persisting the raw input in retry metadata. Judgments remain `unassessed` and do
not publish or trigger model calls. See [feedback semantics](evolution/feedback.md)
and [runtime evidence](../runtime/skill-evidence.md). Automatic attribution,
selection and behavioral evaluation remain separate work.

## Progressive, read-only projection

Eino `List` returns only name/description metadata with no body query. `Get` loads
and verifies only the pinned `SKILL.md`; `ReadFile` verifies the requested pinned
support file. Source edits, reinstallation and process restarts cannot change an
existing snapshot. The metadata/file manifests also contain no real source paths.

`BaseDirectory` is a `skill://` virtual identifier, not an OS directory. Always
wire `Snapshot.BuildContent` and `ReadFileTool` together so models use
`read_skill_file(skill, path)` for references. The tool only renders UTF-8 text;
the host `ReadFile` API can retrieve binary assets. No write, execute, arbitrary
source path, network fetch, archive extraction, or script autorun capability is
provided. A real read-only container mount can be added by a later sandbox adapter.

## Validation and scan limits

Frontmatter requires a valid lowercase skill name, a nonempty string description,
and nonempty instructions. Only name, description, license, compatibility and a
bounded string metadata map are accepted. Aliases, duplicate keys, unknown fields
and policy/model/fork overrides are rejected. The frontmatter maximum is 16 KiB.

The deterministic scanner emits review hints for a few instruction-override,
download-to-shell and credential-literal patterns, plus binary files it did not
scan. It never includes matched secret values in findings. These patterns are
incomplete and can produce false positives; absence of findings does not establish
safety. The scanner does not remove secrets, interpret program behavior or prove
that prose is free of malicious instructions. Installed content remains untrusted
input subject to the host's instruction hierarchy and execution permissions.

Default limits are 128 installed skills, 256 files per skill, 32 retained versions
per source/name, 1 MiB per file, 8 MiB per skill, and 256 MiB across all retained
versions in this DB. Enumeration is streamed in bounded batches, capped at four
times the file count and 16 path levels. Limits can be configured explicitly.

## Verification

`go test -race -p=2 ./internal/skills` covers real SQLite close/reopen, immutable
versions after source edits/deletion, enable/disable/delete, metadata-only loading,
source ID reuse, scope/name filtering, path and symlink isolation, integrity
checks, injected transaction rollback, count/size/version bounds, static findings,
binary-tool rejection, and concurrent idempotent installation.
