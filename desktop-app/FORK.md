# DeerFlow Desktop fork

This directory contains the GPL-3.0-only DeerFlow Desktop derivative of Waku.

- Upstream project: https://github.com/egoist/waku
- Imported local fork: https://github.com/omengye/waku (`D:\Tools\waku`)
- Source revision: `e53746c1f58b44ae523829b25c9341bb03fc91d3`
- Imported on: 2026-09-17
- Waku's `LICENSE` and original copyright notices are retained.

The desktop uses Waku's GPUI conversation workbench, native components and theme
system. DeerFlow configuration is implemented with the same GPUI components and
connects to DeerFlow's configuration service. This fork defaults to the bundled
DeerFlow ACP provider. Other provider implementations remain in the source but
are disabled by default. Waku's upstream updater and product analytics are
unconditionally disabled; the upstream release/signing/website assets are not
DeerFlow publishing endpoints.

## Portable layout

Run `deerflow-desktop.exe` from an extracted Windows package. `waku-daemon.exe`
remains the internal companion executable; its name is not a separate product.
The runtime and `deerflow-acp.exe` sit beside the desktop executable.

All desktop state lives in `user-data/desktop`, including `app.db`, `app.json`,
`state.json`, daemon `settings.json`, blobs, model-cache, projectless workspaces,
worktrees and the optional WebView2 profile. DeerFlow configuration and execution
state use the adjacent `user-data/config`, `data`, `runtime`, `logs` and `skills`
folders. `DEER_FLOW_PORTABLE_ROOT` overrides the complete package root when set
before launch. That directory must contain runtime, resources and executables;
a data-only profile directory is not supported. Debug builds default to `desktop-app/temp`, so they do not read or
write the existing Waku or ACP installation's preferences and history.

The product/OS application identifiers are `DeerFlow Desktop` and
`app.deerflow.desktop` (with separate Debug identities). Internal crate names
are retained to keep the upstream source structure recognizable.

## Build and distribution

From the repository root run:

```powershell
./scripts/build-deerflow-desktop.ps1
```

The script builds the native desktop, internal daemon and ACP bridge, then copies
a clean embedded runtime from `dist/portable/DeerFlow` when available (or builds
one in a fresh staging directory). It never copies the source package's
`user-data`, and it refuses to replace an existing output directory. Each build
uses a new output folder by default. `-PortableRuntimeDirectory` selects another
runtime source; `-RebuildRuntime` forces a fresh locked Python runtime build.
`-DesktopTargetDirectory` selects Cargo output (default
`.build-cache/desktop-build` beneath the repository, or `CARGO_TARGET_DIR` when
set). `-SkipBuild` packages binaries already present there. Templates and bundled
skills always come from the current repository; the existing runtime contributes
Python/dependencies and its Python license only.

The desktop-derived code and its changes are GPL-3.0-only. DeerFlow's separate
source tree retains its existing license. Binary distribution must include the
GPL notice and make the complete corresponding desktop source and build scripts
available under GPL-3.0-only. The script generates a companion source ZIP containing the desktop changes,
DeerFlow sources and build scripts; distribute it alongside the matching binary
ZIP. The binary ZIP also includes license notices. Build outputs
must not be presented as official Waku releases.
