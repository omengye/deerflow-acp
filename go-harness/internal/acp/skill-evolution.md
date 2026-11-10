# Automatic skill evolution and candidate management

The native host uses one automatic pipeline for learned improvements and
agent-authored candidates: immutable package, independent validation,
automatic admission, scoped observation, stabilization or withdrawal. Explicit
native user confirmation also permits direct publication of an exact candidate
without the automatic evidence gates. Command execution and external
effects retain their own permission policy.

Native launch configures the pipeline from the primary model. The former
`skill_evolution` YAML block and manual review settings have been removed.
Historical records remain readable; they do not authorize replay of retired
work. See [discovery](../skills/evolution/discovery.md) and
[behavior evaluation](../skills/evolution/behavior.md) for evidence, isolation,
observation and recovery contracts.

## Model candidate tool

`skill_manage` accepts `create`, `edit`, `patch`, `write_file`, `remove_file` and
`get`, plus `publish` for explicit user confirmation. It does not accept
`approve`, `review_retry` or `delete`.
Deletion is a separate catalog administration operation. Direct installed-file
editing is also separate from candidate evolution.

`Config.SkillProposalSources` grants the candidate destinations. Plan/read-only
runs and tool-policy denials omit the tool. Existing skills must be in the
agent's pinned visible set. Role tool and skill subsets, source identities,
workspace scope and global visibility are checked at invocation. Builtin sources
do not accept in-place automatic candidates. New skills require an authorized
writable destination and an absent target.

The tool calls `SubmitAutomatic`, which atomically persists the exact supplied
package, proposal, generation job and behavior binding. Submission does not call
review models, execute scripts or change the installed head. The generation
worker validates the saved bytes without regenerating them. Validation uses the
independent maintenance ledger and finite retry policy. Enqueue failure rolls
back the proposal and job together.

The job pins the source, base, content, validation identities and originating
workspace, including when the destination is a global managed source. Source
revocation, a changed base or changed evaluation configuration cannot silently
reuse old authority. Safety, behavior, improvement, holdout, trigger and script
execution gates apply before automatic adoption. Missing evidence or runtime
waits for those dependencies unless the native user explicitly confirms
publication via the dedicated path below.

## Explicit user confirmation

The lead agent calls `get`, then `publish` with `proposal_id`,
`candidate_sha256` and `state_version`. The host reads the current native input
and recognizes direct Chinese/English confirmation (for example “确认发布这个技能，
跳过证据门槛” or “Please publish this skill now”). It rejects quoted examples,
questions, conditions, negation, feature requests and ordinary editing requests.
Model arguments cannot carry or manufacture the confirmation. A generic
confirmation selects the latest candidate for a single session-owned skill;
multiple skills require its name, and an older candidate requires its proposal ID.

Ownership, authorized source visibility, native session/workspace, candidate
hash, proposal version and the full base CAS are enforced. A durable approval
records the native run/input/digest and exact candidate. Decision, cancelled
validation jobs, publication history and the recoverable file journal commit
atomically. User-confirmed content is installed for ordinary subsequent tasks
without the automatic observation wait. Actual checks are unchanged, including
failures and unrun scripts. Replays resume the same decision and never approve
different bytes. Existing automatic content cannot be imported at another target.

Root ownership follows the session across turns and restarts. Role ownership
follows its logical invocation. Host-derived request keys bind the run, tool
call, author and canonical arguments. An identical retry returns the same
candidate and job. `get` accepts only the returned `proposal_id` and never
starts another evaluation. Another session or role cannot inspect the candidate
through this tool.

The trusted builtin candidate tool needs no tool-approval prompt, even under
permission mode `all`. External tools named `skill_manage` do not receive this
exemption. Command tools, `skill_run`, direct file edits and external effects
retain their independent permission checks.

## Candidate results

`ok: true` confirms that the request was handled. It does not certify adoption.
Results contain `mode: automatic` or `mode: user_confirmed`, `proposal_id`, `status`, `reason`, `job_id`,
`job_state`, `automatic_retry`, content/base digests, security/quality/behavior
results, bounded model checks and sanitized validation failure details.
`next_action` describes automatic progress or the missing dependency.
User-confirmed publication additionally reports `evidence_gate_bypassed=true`
and confirmation provenance. `published` and `availability_verified` remain
the authority for the actual outcome; evidence bypass is not a passing test.

| Status | Meaning |
| --- | --- |
| `evaluating` | Queued, running or retrying independent evaluation |
| `blocked_evidence`, `waiting_evidence` | Required trusted evidence is absent |
| `blocked_runtime` | Required isolated execution runtime is unavailable |
| `awaiting_adoption`, `waiting_observation` | Validation finished; automatic admission is waiting |
| `observing` | Adopted within the host's observation scope |
| `stable` | Observation criteria passed |
| `published` | Published by explicit user confirmation; automatic evidence gates were skipped |
| `rejected` | Validation or explicit cancellation rejected the candidate |
| `withdrawn` | Regression or insufficient online evidence withdrew the candidate |
| `superseded` | A newer head displaced this candidate |
| `historical` | A retired candidate retained for inspection |
| `unavailable` | Current progress could not be read reliably |

Storage compatibility retains names such as `pending_review` and internal
admission records. They are not human approval requests; `skill_manage` projects
them into the lifecycle above. It does not emit `requires_operator_review`,
`approval_recorded`, `script_review_required`, `review_retry_available` or
`review_retry_of`.

`live_skill_changed: false` describes the submission/inspection call itself.
A successful `publish` call can report `live_skill_changed: true`.
`published` records whether the background worker published a version;
`availability_verified` separately checks the exact candidate in the caller's
next-turn snapshot. Neither publication nor script validation grants permission
to execute commands in a user task. `retryable` classifies a validation failure;
`automatic_retry` reports an actually scheduled retry.

## Authenticated host management

The SDK and authenticated DFACP/1 `MANAGE` channel share SQLite-backed services.
Management is separate from standard ACP methods and model tools. Responses use
`{ "ok": true, "data": ... }`; bounded errors use
`{ "ok": false, "code": "...", "error": "..." }`.

| Operation | Purpose |
| --- | --- |
| `proposal.list`, `proposal.history`, `proposal.get` | Inspect candidates, exact source/base identities, evidence and publication history |
| `proposal.reject` | Explicitly cancel/reject a candidate with state preconditions |
| `proposal.rollback` | Restore an observed historical revision with current-head preconditions |
| `skill.list`, `skill.get` | Inspect installed catalog entries and configured sources |
| `skill.set_enabled`, `skill.delete` | Administer the exact observed catalog head |
| `skill.imports.refresh`, `skill.export.retry` | Inspect imported source changes or recover an export view |
| `skill.generation.*`, `skill.discovery.*`, `skill.auto_patch.*` | Inspect and manage durable background work within host authority |

`proposal.approve`, `proposal.submit`, `proposal.review_retry`,
`proposal.source_review` and `proposal.publish_retry` return `invalid_request`.
They cannot approve or revive historical candidates. History capabilities report
`review: false` and `automatic: true`. Legacy publication fields in management
history remain decoding compatibility, not an active approval interface.

## YAML and Waku generation settings

Use normal model and skill-source configuration. Automatic maintenance inherits
the primary model; it has no separate customer review route, daily budget,
publication quota or manual fallback setting. Disabling skills removes skill
selection and authoring. Managed storage contains immutable publications and
verified exports; writing files into a source is not a substitute for candidate
submission. See [startup configuration](../launch/skills.md).

## Regression coverage

`submitted_candidate_test.go` covers independent evaluation, adoption,
observation, rejection, missing evidence/runtime, workspace binding, idempotency
and atomic queue persistence. SDK `skill_manage_*_test.go` covers root and role
ownership, restart, tool schema and no-approval submission. Permission tests
cover command policy and external-tool impersonation. The new-session runtime
test verifies that a missing optional executable cannot prevent ordinary chat.
