# Automatic skill evolution and candidate management

The native host reviews the safety of the complete candidate package and automatically publishes it on success. Conversation submissions and background-generated packages use the same publication policy. The configured primary model performs the security review. [Security review contract](../skills/evolution/behavior.md)

## Model candidate tool

`skill_manage` accepts `create`, `edit`, `patch`, `write_file`, `remove_file` and
`get`, plus `publish` for inspecting or resuming an already admitted publication. It does not accept
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
package, proposal and generation job. Its worker reviews the saved bytes without regenerating them. Content and scripts are reviewed statically. Package structure, source authorization, current base and candidate digests are verified at admission. An affirmative verdict publishes this exact version without another user message. Discovered versions are immediately available within their source workspace.

The validated package and decision are durable. Retry resumes installation of an admitted version without repeating a successful review or writing a second revision. An unsafe package, withdrawn source or changed head cannot acquire a publication receipt.

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

For `mode=security_only`, `get` reports the proposal ID, candidate digest, job state, security checks and publication status. Quality and behavior remain `not_run`. `automatic_retry` reports a scheduled retry.

| Status | Meaning |
| --- | --- |
| `evaluating` | Security review is queued, running or retrying. |
| `awaiting_publication`, `publishing` | The security decision is saved and installation is in progress. |
| `published` | The exact reviewed version was installed. |
| `rejected` | Review failed or the candidate was cancelled. |
| `superseded` | A conflicting change invalidated the candidate. |

`published` reports completed installation. `availability_verified` checks the candidate in the caller's next-turn snapshot. Older decisions retain their recorded provenance. Script execution and external actions require their ordinary runtime permissions.

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

The evolution tests cover safety rejection, exact-version publication, retries, conflicts, historical queue migration and workspace visibility. The native desktop configuration test exercises learned creates and updates against a deterministic provider and checks its call ledger.
