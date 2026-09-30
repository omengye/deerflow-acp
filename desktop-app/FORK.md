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
`deerflow-config-go.exe` handles Settings, `deerflow-acp.exe` is the ACP Bridge,
and `deerflow-acpd.exe` runs the Go harness. All sit beside the desktop
executable. The desktop uses the Go daemon for ACP and does not bundle or launch
the DeerFlow Python environment. Existing Python ACP session databases are not
migrated automatically; keep an earlier package to access that history.

All desktop state lives in `user-data/desktop`, including `app.db`, `app.json`,
`state.json`, daemon `settings.json`, blobs, model-cache, projectless workspaces,
worktrees and the optional WebView2 profile. DeerFlow configuration and execution
state use the adjacent `user-data/config`, `data`, `runtime`, `logs` and `skills`
folders. `DEER_FLOW_PORTABLE_ROOT` overrides the complete package root when set
before launch. That directory must contain resources and executables; a
data-only profile directory is not supported. Debug builds use the executable
directory when it contains the bundled Go config CLI and daemon. Unpackaged
Debug builds default to `desktop-app/temp`, so they do not read or write the
existing Waku or ACP installation's preferences and history.

The product/OS application identifiers are `DeerFlow Desktop` and
`app.deerflow.desktop` (with separate Debug identities). Internal crate names
are retained to keep the upstream source structure recognizable.
Windows Debug and Release desktop builds use the GUI subsystem, so launching
the desktop does not open a console window. Developers who need a console with
`cargo run` can enable the `dev-console` Cargo feature.

## Build and distribution

From the repository root run:

```powershell
./scripts/build-deerflow-desktop-go.ps1 -Configuration Debug -CreateZip
```

The script builds the native desktop, internal daemon, ACP Bridge, Go ACP daemon,
Go configuration CLI and direct stdio agent. It copies the default template and
bundled Skills, but no Python runtime, dependencies or existing `user-data`.
It refuses to replace an output directory. `-DesktopTargetDirectory` selects
Cargo output (default `desktop-app/target`); `-SkipBuild` packages binaries
already present there. `-CreateZip` makes the binary ZIP and matching source ZIP.
Some bundled Skills contain Python scripts; running those specific scripts
requires a separately installed Python interpreter.

After extracting a new package, run
`python scripts/test-deerflow-desktop-smoke.py --package PATH --backend go`
to exercise the Go package. Python runs this test on the build host; it is not
needed to launch the packaged desktop. The smoke script copies the package into
an isolated build-cache directory.

The desktop-derived code and its changes are GPL-3.0-only. DeerFlow's separate
source tree retains its existing license. Binary distribution must include the
GPL notice and make the complete corresponding desktop source and build scripts
available under GPL-3.0-only. The `-CreateZip` option generates a companion
source ZIP from the build commit; distribute it alongside the matching binary
ZIP. The binary ZIP also includes license notices. Build outputs
must not be presented as official Waku releases.
