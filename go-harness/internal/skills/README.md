# Immutable skill registry

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
