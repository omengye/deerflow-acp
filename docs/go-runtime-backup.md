# Offline Go runtime backup and restore

`deerflow-state-go` preserves `harness.db` (sessions, memory, checkpoints, budgets,
receipts and installed skill versions) and immutable files under `assets/`.
The tool acquires the native host's `runtime.lock` and the asset store lock,
uses SQLite `VACUUM INTO` to include committed WAL state, and writes SHA-256
checksums. Verification checks every file and SQLite integrity. A failed operation
may leave an incomplete new directory; it is never treated as a valid backup.

This is a runtime-state snapshot. Back up `user-data/config`, `user-data/skills`,
managed skill source directories and `user-data/desktop` separately if needed.
Those directories hold credentials or personal history and are never added to
the distributable package. External ACP agents' private state and workspace
files also remain outside this snapshot.

## Windows desktop

Stop accepting work, wait for active runs/operations and uncertain effects to be
resolved, then stop the ACP service in settings. Quit the desktop app to prevent
automatic restart. For CLI hosts, drain through the authenticated management
channel and stop through the Bridge. An active runtime lock rejects backup.

Run from the portable package directory; choose new output paths whose parent
directories already exist:

```powershell
.\deerflow-state-go.exe backup --data-dir .\user-data\data\go-harness --output .\user-data\backups\before-upgrade
.\deerflow-state-go.exe verify --backup .\user-data\backups\before-upgrade
.\deerflow-state-go.exe restore --backup .\user-data\backups\before-upgrade --data-dir .\user-data\data\go-harness-restored
```

Restore refuses existing destinations, linked files, tampered snapshots and
destinations inside the backup directory. Backup also refuses destinations inside
the source data directory. Output is a short JSON result; no prompt, key or asset
content is printed. Each command has a five-minute deadline.

The desktop host uses the fixed `user-data/data/go-harness` path. With the desktop
and ACP service still stopped, preserve the old directory and move the restored
directory into that location (the `go-harness-original` destination must not exist):

```powershell
Move-Item -LiteralPath .\user-data\data\go-harness -Destination .\user-data\data\go-harness-original
Move-Item -LiteralPath .\user-data\data\go-harness-restored -Destination .\user-data\data\go-harness
```

Start the desktop, load a known session and inspect its history/assets before
resuming work. CLI hosts can point `DEERFLOW_GO_DATA_DIR` or `--data-dir` directly
at the new directory. Preserve the original until acceptance succeeds.
A snapshot retains workspace/source
identity bindings: keep the original config and installed source locations.

## Upgrade compatibility

A snapshot does not rewrite execution contracts. Changing context windows,
model policy or the estimator version can prevent older paused executions from
resuming. Cancel an incompatible execution and start a fresh turn; do not relax
its checkpoint fences. A restored directory should have only one harness owner.
The isolated restore test checks session/event and asset preservation, rejects
an active lock, refuses overwrite and detects a modified asset.

## Business behavior suites

The package includes `skill-behavior/code-documentation.json` and
`skill-behavior/newsletter-generation.json` under resources (ACP packages put
them at the top level). Each contains one targeted and one regression case with
one repetition, supplied local facts and declarative assertions.

Select **one** suite via `skill_evolution.behavior.suite_path`, using a path
relative to the configuration file or an absolute path. Both target the existing
`python-public` source ID; this is a compatibility identifier used by the Go
registry. Built-in candidate evaluation requires explicitly permitting built-in
overlays in the existing evolution settings. Keep automatic publication disabled
while reviewing candidates. Suite format and the existing fixture tool-loop are
validated in this iteration; passing them does not certify live model quality.
