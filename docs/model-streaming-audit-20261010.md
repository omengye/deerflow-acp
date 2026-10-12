# Model streaming audit (2026-10-10)

## Finding

The configured provider rejects non-streaming chat requests with HTTP 400:
`Non-stream chat request is currently not supported`.
The main conversation already streamed, but memory extraction and several
auxiliary operations called the provider's synchronous `Generate` endpoint.
The inspected memory database had no saved facts; its 13 extraction audits
contained 11 `model_error` and 2 `no_facts` results. This explains why subsequent
turns could not retrieve automatically extracted facts.

This fix concerns the native SQLite-backed memory system. Writing a workspace
`MEMORY.md` file does not automatically import it into that system. File-memory
discovery and the truncated recursive-glob behavior are separate issues, not
fixed by changing the model transport.

## Transport contract

- `internal/engine/eino/models.go` constructs only OpenAI-compatible and Claude
  models. Every constructor returns the shared provider wrapper, including
  models used by isolated maintenance workers and connection probes.
- `outputTokenModel.Generate` calls `internal/modelstream.Generate`, which calls
  `Stream`, collects deltas through EOF, and returns one complete message. It
  never falls back to a non-streaming HTTP request.
- Tool binding preserves the wrapper. SDK-injected models also pass through
  this boundary, without changing their provider-specific token options.
- The collector retains reasoning, tool-call fragments, finish metadata and
  usage-only trailers. It normalizes split usage without repairing explicitly
  inconsistent counters, so maintenance accounting remains conservative.
- Collection is bounded to 16 MiB of serialized chunks and 65,536 chunks. On
  error, cancellation or overflow, partial content is discarded and cooperative
  providers are cancelled and drained before returning. No automatic replay or
  non-streaming fallback is introduced.
- Direct tracked auxiliary calls observe streaming usage and retain their
  existing budget, lifecycle and optional-failure policy. Eino supplies a
  Generate-only middleware endpoint to synchronous runners, so that endpoint
  keeps its `Generate` call; its terminal model is still the stream-only wrapper.

## Call-site audit

| Operation | Model path | Wire transport |
| --- | --- | --- |
| Foreground chat, subagents, background execution and continuations | Tracked model / common model factory | Stream |
| Memory extraction | `memory_extraction.go` -> tracked `Generate` -> stream collector | Stream |
| Native context compaction | Tracked summary model supplied to Eino middleware | Stream |
| Connectivity probe | `models.go:ProbeModel` -> wrapped `Generate` | Stream |
| Streaming and tool capability probe | `model_probe.go` -> wrapped model, including `WithTools` | Stream |
| Skill generation, behavior evaluation and security/quality review | Common factory / `generateSkillMaintenance` | Stream |
| Skill experience extraction, consolidation, matching and discovery | Isolated maintenance model / common factory | Stream |
| Desktop configuration model tests | Launch/probe paths into the same factory | Stream |

The remaining `Generate` calls are synchronous *result* APIs, not non-streaming
provider requests. `generation_worker.go` calls the domain-level skill generator,
which uses the same maintenance model boundary. The vendored Claude SDK retains
its upstream `Generate` implementation for API compatibility, but application
constructors do not expose it directly. Desktop UI streaming-state booleans are
not model HTTP options. The desktop smoke fixture is an inbound fake provider,
not an outgoing model call.

## Provider cleanup

The standalone Ark adapter, special thinking/token option branches, native
provider selection, desktop dropdown entry, CLI key fallback, and Ark/Volcengine
Go dependencies have been removed. The supported native providers are `openai`
and `claude`; existing `openai-compatible` and `anthropic` aliases remain valid
where already supported.

To use a compatible service, explicitly configure `provider: openai`, its
OpenAI-compatible Base URL, model ID and API key. `provider: ark` is now rejected,
not silently reinterpreted. CLI credentials use `DEERFLOW_MODEL_API_KEY` (or the
supported provider-specific variable). Live configuration and credentials were
not migrated or modified by this change.

Historical implementation records retain their original provider/test claims
with a supersession note; they are not the current provider contract.

## Regression coverage

Loopback fixtures now reject every model request lacking `stream: true`. Tests
cover both providers, bound tools, connectivity/capability probes, native
compaction, synchronous Eino runners, token option translation and maintenance
stages. Collector tests include content/reasoning/tool deltas, usage trailers,
split or inconsistent counters, nil/empty/error streams, cancellation and limits.

Memory tests cover extraction, persistence/reopen, retrieval on the next turn,
scope isolation, quota skip, concurrent scope changes, failed terminal commit,
and failed/cancelled extraction streams. Valid JSON received before a stream
error must not publish facts; an optional extraction failure must not fail a
completed main answer.

Validation results:

- Full package suites passed for `internal/engine/eino`, `harness`,
  `internal/configschema`, `internal/desktopconfig`, `internal/modelstream` and
  `internal/launch` across the recorded runs. The initial launch failure was an
  old JSON-only review fixture; it was converted to strict SSE and the full
  launch suite then passed.
- 54 focused memory, learning, provider-schema, launch and synchronous-runner
  tests passed; 10 native runtime learning/matching tests passed. A later
  33-test collector/provider/accounting regression run also passed.
- `go test -race ./internal/modelstream -count=1 -timeout=120s` passed (12 tests).
- `go test ./... -run '^$' -timeout=180s` passed as an all-package compile check.
- `cargo check -p waku --locked --offline` passed with no errors and 9 dead-code
  warnings. The two edited Rust files passed targeted `rustfmt --check`.
- The broader SDK suite, run alongside the desktop build, exceeded its 15-minute
  timeout and reported background continuation/restart deadline failures. The
  affected cases and memory extraction/retrieval were rerun without the build;
  all 15 selected tests passed. This is not a claim that the full SDK suite
  completed successfully.
- `git diff --check` passed. The module graph contains no Ark/Volcengine
  dependency; the only Ark name in runtime/test source is a rejection test.

Validation is against source and local fixtures. The running desktop release
was not replaced or restarted, and its memory database was not altered. A new
build is required for the running application to use these changes.
