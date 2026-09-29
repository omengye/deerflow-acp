# Durable history pages

`initialize._meta.deerflow.history` advertises version 1 and the private
`_deerflow/history/list` method. It accepts `sessionId`, optional `cursor` and
`limit` (1..500, default 128). The session must be attached to this connection
and idle. The SDK exposes the same operation as `Client.HistoryPage`.

The response has `events` and, when more remain, `nextCursor`. These are harness
domain events with durable sequence numbers, not ACP `session/update` frames.
Asset references stay as references; this query does not load image bytes or
replay model/tool execution. Cursors are opaque read positions, confer no access,
and are valid only for the same session. Each pagination chain excludes events
committed after its first page. Start again without a cursor to include them.

Pages are capped by count and approximately 2 MiB of stored event data. A single
larger event is returned alone, so it is never silently omitted. Standard ACP
`session/load` still replays the complete conversation before responding; its
implementation now reads these pages internally while holding the session lease.
`session/resume` remains attachment without history replay.
