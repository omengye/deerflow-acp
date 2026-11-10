# Skill maintenance jobs and accounting

This internal package supplies the durable queue and independent model budget
for the P4 generation host in `internal/skills/evolution`. SDK hosts explicitly
enable it with `Config.SkillGeneration` and a versioned validator; SDK startup
starts the workers and shutdown cancels and joins them before closing storage.
Authenticated MANAGE exposes operator enqueue/query/cancel operations. YAML and
Waku native settings now wire the explicit generation opt-in, model and budget
through the shared schema/save/launch decoder to SDK startup. The desktop job
request/history/receipt panel is connected; real desktop acceptance is pending.
The collector remains metadata-only. A separately enabled
[repeated-correction selector](../skills/evolution/discovery.md) can enqueue
automatic generation using the same maintenance host.
`RunOne` requires an explicit host handler; constructing this store alone never
starts a worker.

## Persistence contract

- `df_skill_maintenance_jobs` binds an immutable producer reference/input digest
  to source identity, skill name and request key. Repeating the same request
  returns the existing job; changing its input conflicts. The producer uses
  `EnqueueTx` to commit its immutable input and job in one transaction, then
  verifies that input and current source authorization before dispatch. This
  package does not decide skill attribution or source permissions.
- Jobs pin positive, bounded policy values. At most 100 unfinished jobs are
  admitted. Claims respect shared database concurrency and the job's own worker
  cap. Each job has at most three attempts, persisted exponential backoff,
  15-second renewable leases, and one absolute deadline set at its first claim.
  Lease recovery never resets attempts or that deadline.
- Claims have opaque random tokens and a state version. Old workers cannot
  admit calls, settle them, renew leases, finish or cancel a replacement.
  Operator cancellation uses the observed version and invalidates the claim.
- `RunOne` binds the job deadline and budget broker to the context, renews its
  lease every five seconds, and joins the handler and heartbeat on cancellation.
  Handlers must join model I/O before returning. Error records use fixed codes;
  raw provider/storage error text does not belong in job metadata.
- `GuardTx` fences producer stage commits against the exact active claim.
  Admission hooks run inside every reservation transaction before provider I/O;
  terminal hooks close unfinished proposals inside job failure transactions,
  including exhausted expired claims. The evolution host also reconciles old
  interrupted metadata without model calls.

## Usage and execution contract

Automatic maintenance uses an independent durable usage ledger, internal
concurrency/time limits and finite retries. It has no customer daily call/token
quota. Reservations and settlement bind the exact active claim; unfinished calls
cannot be reported as completed, stale workers cannot refund charges, and paid
phase outputs survive restart. Receipts contain stage, digests, attempt and
usage, never credentials or raw provider errors.

`skill_manage` only queues the candidate in the foreground. Content and behavior
model calls run later through the maintenance broker, independently of the
conversation budget. Source authorization is checked before exposing job data.

## Model adapters and generation host

`internal/engine/eino/skill_generation.go` generates a complete, text-only
candidate from pinned input and bounded evidence. It requires the maintenance
broker and rejects changed target/base, malformed/duplicate JSON fields,
incomplete output and tool calls. It has no publication or filesystem API.
The evolution host applies package/path/Python and credential checks, stages
the immutable candidate and its digest atomically, and checkpoints a proposal
before independent content and behavior validation. Trusted suites compare
baseline and candidate through fixture tools and host assertions. Completed
content and variant results survive retries. Missing trusted evidence waits as
`blocked_evidence`; missing script isolation waits as `blocked_runtime`.
Successful validation enters automatic adoption, observation and withdrawal.
The legacy storage state `pending_review` does not request human approval. See [behavior contract](../skills/evolution/behavior.md).

The existing Eino security/quality reviewer recognizes the same broker. All
content stages and behavior calls commit reservations and settle receipts at the provider boundary.
Skill model constructors disable provider SDK retries; retry decisions belong
to the durable job. OpenAI and Claude HTTP failure fixtures verify this.
Ordinary conversation model constructors keep their previous retry policy.

`df_skill_generation_inputs` pins the full target/source/workspace identity,
complete publication head, builtin overlay base, operator reason/request key,
and generator/reviewer identities. The queue input digest binds that snapshot;
a separate progress digest binds candidate version/reason and proposal ID.
Source grants, head and configuration are checked before calls and at stage
commits. Explicit builtin-overlay authorization is required.

Retries and restart reuse a saved candidate and proposal, without regenerating
completed stages. A committed review can be acknowledged without new calls even
if an operator published it before job finalization recovered. An unfinished
generated proposal can only be reviewed within its owned maintenance context.
Cancellation uses the observed job version and rejects a still-reviewable or
validating proposal in the same transaction; published proposals are not undone.
Completed jobs use the ordinary proposal review controls.

History and receipt reads remain available when generation is disabled, limited
to currently configured source identities. Removing or rebinding a source
revokes access. Lists use revision/filter-bound cursors; receipts use bounded
sequence pagination. A failed maintenance job does not create or fail a user
run, replay a tool, or publish a skill. An interrupted request retains its
conservative charge across shutdown and restart.

See [SDK setup and MANAGE operations](../acp/skill-evolution.md#operator-requested-generation).
Waku has source-level job submission, explicit refresh, bounded job/receipt
pagination and observed-version cancellation, using the authenticated MANAGE
adapter. Read-only status/prepare operations expose current grants and observe
complete heads, including absent skills, without starting model work. A prepared
head is checked again on enqueue. Generated candidates use the existing manual
proposal review controls. Real desktop interaction/release acceptance is pending.
Operator feedback is persisted separately against actual signal/read identities
and does not assert skill causality. When explicitly enabled, repeated current
corrections across distinct settled roots may enter generation through the
selector's atomic daily/cooldown/dedup admission. Automatic snapshots add a
discovery ID/digest and current evidence is rechecked at every paid stage and
before publication approval. Queue-capacity backpressure does not consume a
discovery reservation or permanently quarantine healthy evidence.
Manual candidates with behavior enabled use a separate durable producer in the
same maintenance queue, without a fake generation input. Their content/behavior
costs use the independent budget. Proposal rejection cancels an active manual
evaluation job. P4 still needs other discovery triggers and trusted quality
attribution. Explicit jobs supply the submitted reason/pinned base;
automatic jobs also supply verified, sanitized correction summaries as untrusted
evidence. No automatic publication capability is enabled here.
