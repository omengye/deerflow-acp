# Explicit Skills startup configuration

Both `deerflow-acp-go` and `deerflow-acpd-go` accept `--skills-config <file>`.
The file is a bounded JSON object. Source paths must be absolute. A workspace
source must be inside its named workspace. No directories are scanned implicitly.

```json
{
  "sources": [
    {
      "id": "project",
      "root": "C:/work/project/skills",
      "scope": "workspace",
      "workspace": "C:/work/project"
    }
  ],
  "install": [
    {"sourceId": "project", "directory": "research", "enabled": true}
  ],
  "includeGlobal": false,
  "names": ["research"]
}
```

`install` is an explicit startup instruction to snapshot those packages and
apply the specified enabled state. Review the package before setting `enabled`
to true. Each installation commits independently; if a later package fails,
startup stops but earlier successful installations remain. Correcting the
configuration and restarting is safe: unchanged packages reuse their version.
Omitting `install` uses only existing installations and does not update source
contents. Omitting `names` selects all enabled visible skills; `names: []`
selects none. Global sources require both an explicit source and `includeGlobal`.

These same fields are available in `Config.Skills` and `Config.SkillSelection`.
The SDK's Install/SetEnabled/Delete/List methods support live administration.
Disabling or deleting affects future runs. Suspended runs keep their pinned
versions; removing the source from host configuration revokes their restoration.

## Managed sources and export views

The JSON profile also accepts `builtinSources`, `managedSources` and
`exportRoot`. These reference the explicitly declared source IDs:

```json
{
  "sources": [
    {
      "id": "generated",
      "root": "C:/work/project/managed-skills",
      "scope": "workspace",
      "workspace": "C:/work/project"
    }
  ],
  "managedSources": ["generated"],
  "exportRoot": "C:/work/project/managed-skills",
  "includeGlobal": false
}
```

Both directories must already exist and reject links. A managed source uses
SQLite publications exclusively; do not include it in `install`. Files under
its root are not scanned or imported, including previously generated exports.
The root continues to bind source identity/scope. Builtin and managed policies
cannot apply to the same source.

Native launch configures automatic skill evolution from the primary model when
skills are enabled. Explicit managed sources are preferred candidate destinations;
existing custom import sources can receive authorized updates. If no writable
destination exists, the host provisions managed storage beside the configuration.
Builtin sources do not accept in-place model candidates.

There is no `skill_evolution` YAML block, manual approval setting, independent
review route or daily maintenance quota. The host owns worker concurrency,
timeouts and finite retries. Candidate validation, automatic admission,
observation and withdrawal use independent evidence and scoped source authority.
The `skill_manage` submission call queues the exact package without performing
foreground model review. Command execution permissions remain independent.
See the [candidate contract](../acp/skill-evolution.md).

The SDK owns a local export worker. Every managed publication, enablement
change or tombstone queues the exact head in the same SQLite transaction.
The worker writes a verified view below `exportRoot/.deerflow-exports/`.
Each view contains `publication.json` and a `files/` package for a live
head; a tombstone has no package. Select a view using `ListSkillExports`
and its `synced` status, not by picking a directory with the newest name.

File failures preserve the published skill and are retried at most three
times. Failure/retry state survives restart; `RefreshSkillExports` explicitly
retries failed views. Startup checks previously synced views and rebuilds
missing or changed ones into a fresh directory. It does not overwrite old disk
edits or change the publication. Old/crash-orphaned views are currently retained;
exports are not a history backup or a garbage-collection facility.

## YAML managed sources

The ordinary configuration accepts managed sources directly, without a separate
CLI profile. Paths resolve relative to the YAML file; directories must already
exist. For a configuration under `project/config/`:

```yaml
skills:
  enabled: true
  managed_sources:
    - id: generated
      root: ../managed-skills
      scope: workspace
      workspace: ..
  export_root: ../managed-skills
  include_global: false
```

`managed_sources` is an array of strict source definitions (`id`, `root`,
`scope`, optional `workspace`); IDs must be unique and cannot reuse the legacy
`python-public` / `python-custom` identities. Scope and directory/link checks
reuse the Registry implementation. Workspace roots must stay inside their
workspace. Global sources omit `workspace`; managed-only YAML defaults to
excluding global skills, which require `include_global: true` to select.
The combined number of import and managed sources is limited to 32.

An explicit `skills.path` can coexist with managed sources; its public/custom
import behavior and source identities remain unchanged. Import configurations
keep their historical global selection unless `include_global` is specified.
Managed directories are never scanned or installed from disk. The explicit
`--skills-config` profile still takes precedence over YAML sources. Disabling
skills clears source selection and removes candidate submission tools while
preserving stored publications.

Desktop save/validate uses the same source resolution and proposal destination
policy as native launch. It no longer writes filesystem skill enablement into
extensions JSON; Registry operations own that state. MANAGE/Waku support export
status/retry and native import/export directory fields, global selection and
per-source id/root/scope/workspace editing. Source forms preserve relative paths;
an unchanged source definition is not rewritten. Directories must exist before
validation, and removing a source does not delete its files or published history.
Retired evolution settings and jobs remain migration/history concerns and do not
reintroduce manual candidate approval.
