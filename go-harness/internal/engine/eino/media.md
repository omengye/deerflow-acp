# Media projection

The engine accepts images that the harness has already imported into a session
asset. Raw Base64 images, local paths and remote image URLs are rejected at this
boundary. Input normalization belongs to the runtime's asset service.

## Durable and provider representations

- Native input and tool messages contain a `deerflow-asset://SESSION/ID` URI and
  an `AssetRef` encoded as a JSON string in the part's `deerflow_asset_ref` extra.
  These values survive both Eino's gob checkpoints and JSON session events.
- Every model invocation receives a private serialization copy. The model
  lifecycle resolves image references using the current run's session ID,
  verifies size, SHA-256 and MIME type, and places Base64 only in that copy.
  Original history and checkpoint messages are never hydrated in place.
- The same lifecycle applies to reconstructed history and native subagents.
  `Media.VisionModels` must allow the selected model, including a per-call
  `model.WithModel` override. An empty allowlist disables vision.
- One invocation can hydrate at most 32 distinct images with 40 MiB total source
  bytes. Repeated identical references reuse the encoded value. Each image also
  obeys the harness's 20 MiB image limit.

## Files, tools and usage

Ordinary files and HTTP(S) resource links become bounded metadata references in
the provider input, in their original order. They are never automatically read
or fetched. Remote image references are rejected. Explicit tools are required
to inspect attachment contents.

`ProjectToolContent` converts normalized harness content into native enhanced
tool output. Unary enhanced tools may also return inline PNG, JPEG, GIF or WebP
images. The middleware stages at most eight images and 40 MiB per call, then
replaces their bytes with session assets before native history or durable tool
receipts see them. A successful `tool_end` publishes the snapshots with its
receipt and event in one transaction; failed or uncommitted calls remove the
staged snapshots. Streaming tool chunks still require normalized references.

The official MCP adapter formats call results as JSON text. Its result handler
removes image data from that text, and the enhanced wrapper passes actual MCP
image blocks to the same import path. Other MCP binary content remains
unsupported.

Assistant-generated inline PNG, JPEG, GIF and WebP images use a separate model
importer. The model lifecycle replaces Base64 with a session asset reference
before Eino receives the message. Each `image_delta` event commits the asset
record and event together. ACP hydrates that event only for an authorized live
update or history replay. One model call is limited to eight images, 20 MiB
each and 40 MiB total. Generated audio, video and remote image URLs remain
unsupported. Generated images in prior assistant turns are hydrated only for a
configured vision model immediately before the next model invocation.

Each input or generated image receives an explicit 4,096-token estimate when
the provider omits usage; Base64 length is excluded from text token estimation.
This is a model-independent estimate, not an exact token or cost bound.
Provider-reported usage replaces it during settlement.
