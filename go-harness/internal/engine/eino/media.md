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
tool output. Unary and streaming middleware validate structured media before
either native history or durable tool receipts see it. Inline image results from
MCP tools currently fail with a tool receipt error; they require asset-import
wiring before they can be enabled. Generated model multimedia is also rejected
until an output importer is available.

Each image reserves an explicit 4,096-token estimate; Base64 length is excluded
from text token estimation. This is a model-independent estimate, not an exact
token or cost bound. Provider-reported usage replaces it during settlement.
