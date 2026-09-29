# ACP media and local artifacts

The stdio transport accepts `text`, inline `image`, and `resource_link` prompt
blocks. Embedded resources and audio are not implemented. Image capability is
advertised only when asset storage is configured and an available model appears
in `Media.VisionModels`. The selected session model is checked on every prompt.

## Input

- Images support PNG, JPEG, GIF and WebP, with Base64 and MIME checks. The limits
  are 8 images per turn, 20 MiB per image and 40 MiB combined decoded image bytes.
- Local `file:` resource links must refer to regular files within the session
  workspace. Files are limited to 25 MiB; image links also obey the image limits.
- HTTP(S) resource links are retained as references and are never fetched.
  Remote images, including image MIME, filename or URL extension hints, are
  rejected. Supply inline bytes or a workspace file instead.
- Attachment-only prompts are accepted. The ACP wire never accepts internal
  `asset` metadata or `deerflow-asset:` URLs as a client-supplied capability.
- Validated bytes are snapshotted under the asset directory. Session identity,
  exact metadata, length and SHA-256 are checked whenever bytes are resolved.
  Accepted input, asset metadata and the replay event commit together.

Stored history contains stable references, never inline image Base64. A
`session/load` projects one content block per update: images are hydrated into
inline Base64 and local files use a URI for the immutable snapshot.
`session/resume` attaches without replay. Each projected image fits within the
64 MiB frame limit; a prompt with two 20 MiB images is covered by a real pipe test.

## Tool media and artifacts

`view_image` snapshots a bounded workspace image and commits its reference with
the terminal tool receipt. It does not add the image to the artifact registry.
The tool is made available only for configured vision models.

`present_files` explicitly registers files from `cwd/.deerflow/outputs`. Paths
may be output-relative, workspace-relative `.deerflow/outputs/...`, absolute
within that output root, or use `/mnt/user-data/outputs/...`. An artifact is a
regular file of at most 200 MiB. A call supports up to 32 paths, deduplicated.

Registration, the completed receipt, `tool_end` and an internal audit event are
one transaction. ACP emits the terminal tool card followed by one resource link
per artifact. Audit events are not separately replayed, so live output and load
history show each artifact once. A failed transaction/terminal or cancelled run
removes uncommitted snapshots. Restart cleanup preserves committed snapshots
and cannot race active preparation in another store manager.

The optional query is advertised as
`initialize._meta.deerflow.artifacts.listMethod`:

```json
{"jsonrpc":"2.0","id":3,"method":"_deerflow/artifacts/list","params":{"sessionId":"..."}}
```

It returns `{"artifacts":[...]}` containing ACP resource links. The session must
belong to the requesting connection and be idle; listing and link projection
share that ownership lease. The extension performs no tool execution.

This implementation provides local snapshots. Publishing to S3/RustFS and other
remote artifact destinations is not implemented.
