# Host-defined skill behavior evaluation

Behavior evaluation executes bounded tasks through the Eino model and fixture
tools, then checks their outputs in Go. Content reviewers cannot supply or
approve a behavior report. The evaluator does not publish a skill.

## Configuration

Native configuration uses the existing `evaluation_model_name`, with fresh
contexts separate from security and quality review. Enable evolution and
generation with their existing model routes, source grants and independent
maintenance budget before enabling this block:

```yaml
skill_evolution:
  behavior:
    enabled: true
    suite_path: behavior-suite.json
    timeout_seconds: 60
    max_steps: 6
    max_input_bytes: 131072
    max_output_tokens: 1024
```

`suite_path` is relative to the configuration file, or absolute. Save/validate
and launch use the same parser and source authorization. Native configuration
loads one JSON suite, at most 1 MiB, once at startup; changing the file requires
applying configuration again. SDK callers can configure up to 32 target suites
with `Config.SkillBehavior` and `Config.SkillBehaviorEvaluator`. Duplicate
targets are rejected. Defaults leave behavior evaluation disabled.

The per-variant timeout is 1–300 seconds, model step cap 1–32, serialized input
cap 1 KiB–2 MiB and output cap 128–8192 tokens. Native configuration limits each
variant to 24 tool calls. The job deadline, daily call/token budget and worker
count remain those of `skill_evolution.budget`; a case cannot extend them.

## Suite contract

The host owns the suite, its task inputs, simulated responses and assertions.
Candidates cannot provide or edit their own suite. `source_id` must match an
authorized source and `name` the skill in that source. See
[`docs/fixtures/skill-behavior-v1.json`](../../../../docs/fixtures/skill-behavior-v1.json)
for a copy-file example; select the actual source ID before using it.

A suite has version `skill-behavior-v1`, an ID and 1–16 cases. Each case has:

- A unique ID, `kind` (`targeted` or `regression`), task prompt and 1–3 repetitions.
- Up to 64 initial text files at portable relative paths.
- Up to 8 simulated tools, each with primitive parameter definitions and an
  ordered list of exact JSON argument/response exchanges.
- 1–32 host assertions: `output_equals`, `output_contains`, `file_equals`,
  `file_absent` or `tool_calls`. At least one suite case must be targeted.

`skill-behavior-v2` preserves those execution cases and adds metadata-only trigger
cases. It requires at least one `trigger_positive` and one `trigger_negative`,
as well as a targeted execution case. The existing 16-case/3-repetition cap is
shared, not doubled. See
[`docs/fixtures/skill-behavior-v2.json`](../../../../docs/fixtures/skill-behavior-v2.json).

Each trigger case has no task files or fixture tools and exactly one
`skill_selected` assertion: `count=1` for a positive and `count=0` for a negative.
`trigger_skills` may contain up to eight host-fixed competing name/description
entries, never an override of the evaluated target. Only the target description
comes from the immutable baseline/candidate package, parsed with the runtime's
installation parser. The fixed catalog is sorted and digested. It is not a live
inventory scan and proves only that suite's selection boundaries.

The evaluator uses the pinned native Eino Skill middleware's prompt, reminder and
tool schema in a fresh context, then observes the first model response. An exact
valid Skill call selects its name; a final answer without calls means no skill.
No tool is invoked, no body is loaded, no workspace is created, and no simulated
selection becomes a runtime read. Expected labels, case IDs/kinds and assertion
values remain outside model messages. Unknown tools/names, malformed arguments,
multiple calls, truncation and timeouts are operational failures, not evidence
that the old description was wrong. Calls use the original job's `behavior`
maintenance receipts and budget; no extra worker or paid retry loop is added.

JSON duplicate keys, unknown fields, explicit nulls, executable assertions,
invalid paths and oversized data are rejected. Builtin fixture tools are
`read_skill`, `read_file`, `write_file` and `list_files`. They access only the
immutable skill package or the current temporary task workspace. Simulated
external tools return host fixture data; they never call the real service.

Each execution base/candidate/case/repetition has a fresh temporary workspace and model
context. Expected answers and assertion paths are absent from model messages
and tool-readable files. The Go host inspects actual task output and file/tool
effects. Creating a skill uses a real no-skill baseline; deleting a skill uses
a real no-skill candidate. The absent variant has an empty version and package.

No shell, Python, OpenCLI, MCP or arbitrary script runs in this evaluator.
Only the configured model provider makes network requests. `IsolatedScripts`
stays false: fixture execution does not authorize executable support files.

## Jobs, results and approval

Generated candidates continue evaluation within their original maintenance job.
When behavior is enabled, manual submissions atomically create a `validating`
proposal and a dedicated behavior job (`behavior_job_id`), without inventing a
generation request. Rejection cancels an unfinished manual behavior job.

Content review and completed variant results are persisted. Retries reuse
completed work; an interrupted call without a committed result may run again
under the remaining budget. Each provider request reserves durable usage first;
unknown usage keeps its conservative charge. Receipts include all attempted
calls, so their total can exceed the cost of completed results in the report.

Work is bound to the complete source/target/head/overlay, candidate version,
reviewer/evaluator identities, suite digest and comparison rule version. Source,
configuration, head, evidence or claim changes prevent stale work from advancing.
The evolution service recomposes reports from stored results before approval.

All targeted and trigger candidate repetitions must pass and no previously passing baseline
case may regress. The report includes comparisons, improvements, regressions,
digests, calls, token charges and elapsed time. Passing evaluation still produces
only a proposal for explicit review unless an independently authorized auto-patch
policy admits it. Enabling behavior or using a v2 suite does not grant publication.

V1 saved reports retain `skill-behavior-comparison-v1` and their original digest.
V2 reports use `skill-behavior-comparison-v2`, separate positive/negative counts
and paired selection observations. Approval reconstructs checks from host labels
and observed names, and reconstructs each catalog from immutable variant bytes.
The Eino evaluator identity is now `eino-skill-behavior-v2/<config digest>`;
unfinished v1-evaluator work is fenced after upgrade rather than silently reused.
Completed old evidence remains explicitly manually reviewable; automatic admission
still requires today's exact evaluator/suite/configuration identity.

A target without a matching suite records `not_run` and needs manual review.
An applicable evaluation that fails or cannot complete cannot claim `not_run`
or success. Failed and partial evaluation state remains inspectable. A failed
maintenance task does not fail the user's conversation or replay real tools.

## Inspection

Go SDK methods are `SkillBehaviorEvaluation(ctx, proposalID)` and
`SkillBehaviorReceipts(ctx, proposalID, after, limit)`. Authenticated MANAGE:

```json
{"operation":"proposal.behavior","proposal_id":"<proposal-id>"}
{"operation":"proposal.behavior.receipts","proposal_id":"<proposal-id>","after_sequence":0,"limit":50}
```

Evaluation detail contains job state, attempts, deadlines, content-review status,
up to 96 variant results and the compact report. Receipts use sequence pagination,
with limit 1–100. Queries enforce the proposal's current source authorization,
including when generation is disabled. Responses do not expose suite prompts,
assertion values, fixture files, model messages or provider credentials.

Waku loads evaluation details and receipts in the background and renders cached
state with local result pagination. This source integration and fixture tests
do not replace actual desktop interaction and Release acceptance.
