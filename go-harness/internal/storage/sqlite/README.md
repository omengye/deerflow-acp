# SQLite Eino providers

`Open(path)` owns the database connection. `Store` implements Eino's
`CheckPointStore`, `CheckPointDeleter`, and
`SessionEventStore[*schema.Message]`. `Store.Tasks()` implements
`backgroundtask.TaskStore`, `TaskEventStore`, `NotificationWriter`, and
`NotificationOutbox`. `DB()` is available to the harness's business repositories.

The SQL tables have an `eino_` prefix. WAL, `synchronous=FULL`, foreign keys,
a five-second busy timeout, and immediate transactions are enabled. The pool
uses a single connection; transactions also fence writes across separately opened
connections. The application still owns daemon process locking and connection
ownership policy.

## Persistence contracts

- A checkpoint Set acknowledgement follows the SQLite commit. Deletion supports
  Eino's rollback cleanup.
- Session event batches commit atomically and use database append positions.
  Byte-identical retries do not duplicate events; conflicting IDs are rejected.
  Session history retains rollback markers rather than erasing physical history.
- Session events use Eino's `HumanReadableSerializer` with canonical JSON key
  ordering, preserving registered extension types and large integers. A custom
  serializer must produce deterministic bytes for retry detection and remain compatible
  across restarts. The event store does not provide a cross-process session run
  lock; the harness session coordinator must enforce one writer per session.
- Task transitions use version CAS, persistent leases and attempt fencing.
  Expiration follows each task's retry/fail policy; pending resume input and
  cancellation intent survive recoverable attempt loss.
- Creating and changing task lifecycle state writes the corresponding outbox
  entry in the same transaction. Application notification replay metadata remains
  after acknowledgement. Receipts are random opaque values whose expiry is
  checked against the persisted current lease.
- Task-event cursors capture an immutable append-position snapshot, scope to the
  database/task/direction, and remain valid after reopening the same database.
- `WaitForTaskVersion` polls committed state every 25 ms; this observes changes
  from other connections and lease expiry without relying on process-local wakeups.

Task lifecycle semantics follow CloudWeGo Eino `v0.10.0-alpha.35`'s provider
contract and reference state machine. The tests invoke its public conformance
suites directly, with additional atomicity, rollback, cross-connection CAS,
reopen and child-process abrupt-exit cases.

The module's SQLite driver version and toolchain are pinned by the repository's
`go.mod`. Run `go test ./internal/storage/sqlite` from `go-harness/`.
