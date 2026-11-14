# Host-owned automatic skill evaluation

The native harness always enables automatic skill evolution and inherits the
primary model. There is no `skill_evolution` configuration block, independent
review route or daily call/token quota. Concurrency,
timeouts and finite retries are internal policy; usage remains recorded.

Evidence gates govern automatic adoption. `skill_manage.publish` can publish
an exact conversation-owned candidate after a native user confirmation without
behavior, holdout, isolation or observation evidence. It preserves real checks,
records the input digest and cancels queued/running/waiting validation jobs.
Structural validity, source authority and base CAS remain mandatory. A copied
automatic package cannot acquire this authority by being written to a source.

The host derives trusted cases from new independent tasks with explicit exact
output or artifact requirements. At least two independent roots support a
candidate, and a separate holdout is required before adoption. Missing trusted
coverage waits as `blocked_evidence`. Synthetic tasks cannot count as online
successes. Candidates never receive expected answers or editable grading rules.

Optional host-maintained regression files live in the `skill-behavior` directory
beside the configuration file. Each regular JSON file contains one strictly
validated suite, capped at 1 MiB. Files load 32 at a time and suites persist as
content-addressed case manifests. Sixteen cases and 32 suites are not lifetime
limits. SDK hosts can supply `SkillBehaviorOptions.Load` to stream suites; this
is a host API, not a customer setting. Duplicate targets are rejected.

## Cases and comparison

`skill-behavior-v1` supports targeted and regression cases. The v2 contract also
requires positive and negative metadata-only selection checks. A case has one
to three repetitions, a task prompt, at most 64 text files, eight fixture tools
and 32 host assertions. Assertions inspect output, files or tool calls. Trigger
cases compare the pinned target description with up to eight competitors per
case; additional competitor groups become additional cases.

Each baseline/candidate repetition runs in a clean context and workspace. A new
skill uses the current catalog without that skill as its baseline. Content
review cannot mint behavior or script-execution evidence. Mandatory comparisons
are persisted and paged; all must finish before the host assesses improvement,
regression, triggering and holdout coverage. Operational failures are distinct
from assertion failures. Transient measurements have at most three persisted
attempts, and successful comparisons are reused after restart.

## Script execution

`run_script` executes package Python or shell scripts using the managed OCI
runner: fixed image identity, temporary storage, disabled network, process
limits and real exit/output evidence. Host credentials and grading files are
never mounted. `read_skill`, `read_file`, `write_file` and `list_files` operate
only on immutable package files or task fixtures. Simulated external tools
return host-owned fixture data. Missing OCI capability is `blocked_runtime`;
static inspection cannot substitute for execution or authorize publication.

## Adoption and recovery

Evaluation identities bind package, baseline, suite, catalog, model, validator,
runner and tool contract. Changes invalidate pending verdicts and schedule
reassessment of published content without regenerating candidates. The host
checks all bindings and current source authority again at publication CAS.
Automatic packages stay in immutable storage; scoped routing selects observation
candidates without overwriting public source files. Normal catalogs retain the
stable baseline, and a new skill stays hidden outside observation scope.

Observations last 72 hours and require five independent successful roots,
critical-behavior coverage and no unresolved failures. Source withdrawal,
confirmed regression or inconclusive expiry withdraws the candidate. A manual
head change supersedes the old observation. Historical observations finish at
their original deadline; retired learning and generation work never replays.

## External provenance

Learning never receives result bodies from network, MCP or attachment tools,
but injected content in those results can still steer the assistant text and
later local actions that are learned. When more than half of a learned
candidate's supporting experiences come from episodes that consumed such
content (`harness.ExternalContentTool`), automatic adoption stops with
`external_provenance_confirmation`. Behavior evaluation cannot detect a dormant
injected instruction, so the proposal stays pending and needs attention until
the user publishes it once with `skill_manage.publish`. Unreadable evidence
counts as external. Explicit memory writes follow the same rule: after a
session consumes external content they must pass the automatic-extraction
content filters and are labelled `external_context` wherever memory is shown
to a model.
