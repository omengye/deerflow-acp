# ACP stdio transport baseline

## Wire format and ownership

`internal/acp/protocol` implements ACP's UTF-8 newline-delimited JSON-RPC 2.0
transport. It does not use LSP `Content-Length` headers. The default frame limit
is 64 MiB, excluding the trailing LF; `Options.MaxFrameBytes` can override it.
Both inbound and outbound frames are bounded. Batches are not supported by ACP.

The peer owns its input/output closers. Closing either transport or canceling
`Serve` cancels the connection, closes both streams, and waits for request and
notification handlers to finish their cleanup. Callers must supply closers that
unblock pending I/O. The stdio command must exclusively dedicate stdout to this
peer and send diagnostics to stderr.

`Done` signals a disconnected connection, not finished cleanup. `Serve` returning
is the cleanup boundary. Ordinary EOF and explicit `Close` return nil; malformed
framing, capacity overflow, and parent cancellation retain their error cause.

## Why the candidate SDK is not the transport

The implementation reviewed `github.com/coder/acp-go-sdk v0.13.5`, downloaded by
the Go module tooling with checksum
`h1:LI9jq5xon7xslaYlnoktvTVyDlE37yIk2daT7N9ASYk=`.

The SDK's schema types can be reused inside the ACP adapter. Its generated
`agent_gen.go` includes `CloseSession`, `ResumeSession`, and
`SetSessionConfigOption`; these are not missing methods in this pinned version.
The transport itself cannot preserve the local harness's contract unchanged:

* `agent_gen.go`, `AgentMethodSessionPrompt`, cancels a previous same-session
  prompt context before invoking the application's `Prompt` handler. A busy
  check inside that handler is too late to prevent preemption.
* `connection.go`, `receive`, hardcodes a 10 MiB scanner limit.
* Extension dispatch accepts names starting with `_`; it is not a general
  interception hook ahead of generated standard-method handling.

The local implementation is original, small transport code, not a fork of the
generated SDK or an untracked edit to the module cache. It has no ACP session
state. ACP business methods and schema validation stay in the agent adapter.

## Dispatch contract

1. `Options.Admit` executes synchronously on the reader in incoming request order.
   Use it to reserve a prompt's session before dispatch. It may return a derived
   context carrying the reservation and a release callback. It must not wait for
   a model, client request, database retry loop, or another active request.
2. Requests then execute concurrently. The active request count is bounded.
   Duplicate active JSON-RPC IDs and exhausted request capacity receive explicit
   errors; they do not cancel an existing request.
3. Incoming notifications execute in order on a separate bounded worker. A cancel
   notification signals its already-admitted run and returns promptly. It must
   not wait for that run's final response. `IsNotification(ctx)` lets business
   handlers ignore request-only methods incorrectly sent as notifications.
4. Incoming responses route directly to pending reverse calls. A long prompt,
   pending permission request, or slow notification handler cannot block routing.
5. The release callback runs exactly once after handler cleanup and the final
   response write attempt. A canceled *run* may still return the ACP
   `stopReason: cancelled`; connection cancellation suppresses further writes.

Bounded queues are explicit. Reader-generated errors never wait on output and
overflow closes the connection. Producers using `Notify`/`Call` see backpressure
through their contexts. Cancellation during a generic writer's active write
cannot retract already-sent bytes; closing the peer interrupts that I/O.

## Business adapter obligations

* Session ownership, per-session admission, permissions, allowed ACP methods,
  capability negotiation, input validation, and ACP cancellation semantics are
  business policy; the transport must not infer them from method names.
* `session/load` must synchronously send/await historical updates before returning
  its result. `session/resume` returns restored context without a history replay.
* `Call` responses do not wait for application processing of previously received
  notifications. An outbound ACP client adapter must implement its own history
  application barrier if it uses this peer to consume `session/load`.
* Pending outbound IDs belong only to this connection. A canceled or disconnected
  permission request never carries forward into a reconnected session; reconstruct
  the approval from durable intent and issue a fresh call.
* Do not call the admission hook for a notification. A prompt received as a
  notification must not run a long handler on the notification worker.
* Handlers must return `*protocol.Error` for public errors. Other errors and
  recovered handler panics map to a generic internal error, without leaking
  diagnostic details to clients.

## Verification

`go test ./internal/acp/protocol` exercises real OS pipes, fragmented UTF-8 JSON,
reverse permission responses while a prompt runs, duplicate prompt admission,
immediate cancel ordering, delayed cleanup on EOF, canceled call cleanup, protocol
errors, frame limits including the default 64 MiB reader boundary, serialized
concurrent writes, and blocked-writer shutdown. These transport tests do not
substitute for the full ACP TCK, session-policy tests, real editor interop, or
daemon/Bridge tests.
