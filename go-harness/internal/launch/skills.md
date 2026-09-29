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
