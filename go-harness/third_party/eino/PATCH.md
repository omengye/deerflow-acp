# Eino checkpoint patch

Base: `github.com/cloudwego/eino v0.10.0-alpha.35`, commit
`3f5ec670e75daeabf14aee741990ebb02f28cefb`.
Module sum: `h1:cK5DoUrrdfmkgZGFzXVhd3Lga4Qz+4O7PndJ1+1F1lc=`.
License: Apache-2.0, retained in `LICENSE` and source headers.

This directory contains the upstream module, including its tests and checkpoint
compatibility fixtures. The upstream `.github` automation and images are omitted.
The module cache is not modified. Keep the root dependency requirement at
`v0.10.0-alpha.35`; replacing the entire module with the stable-branch fix commit
would remove the session/background-task APIs used by this application.

The only production source change is in `adk/interrupt.go`: port the canonical
checkpoint/reference fix from upstream PR #1232, commit
`60e1d9929cb65c8c4814b66fba2854e29b730114`:
https://github.com/cloudwego/eino/pull/1232

Persist matching ChatModelAgent checkpoint bytes once in `InterruptID2State`.
The deprecated `Info.Data` view stores an internal reference and is reconstructed
when loading. Custom or different legacy data remains inline. This preserves old
inline checkpoints while avoiding approximately doubled payloads at each nested
AgentTool boundary. Missing or invalid referenced state fails explicitly.

Original `adk/interrupt.go` SHA256:
`bce3df89985be75aafe8b3de9627d53f72b89df4182623c4a615d1420c64d095`.

The application's `internal/engine/eino/checkpoint_growth_test.go` exercises
three nested Runner boundaries through the public ADK API with a 1 MiB payload.
The patched version must remain below the original payload plus 64 KiB. This
test is included in the normal application suite. Run from `go-harness`:

```sh
go test -mod=readonly ./internal/engine/eino -run '^TestEinoNestedCheckpointSizeDoesNotMultiply$' -count=1
```

`adk/checkpoint_patch_compat_test.go` separately verifies old inline native
checkpoints, noncanonical custom data and rejection of broken references. Run
from this module's directory (also run in CI):

```sh
go test -mod=readonly ./adk -run '^TestCheckpointPatch' -count=1
```

Remove the local replacement only after a compatible upstream version contains
the deduplication and passes native resume, nested permission, session-store,
background-task and checkpoint compatibility regressions. No upstream report,
fork or release has been published as part of this change.
