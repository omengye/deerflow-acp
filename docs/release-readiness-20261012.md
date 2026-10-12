# Windows release candidate assessment, 2026-10-12

## Decision

The current source is a release candidate, NOT a verified stable release.
Submit on the existing `codex/eino-harness-acp-refactor` branch and use its
Windows Actions run as a mandatory clean-checkout validation gate. Do not
create a release tag, merge into main, or publish release assets from this
assessment. A dispatched workflow is not evidence of successful packaging.

## Candidate scope

- Streaming model transport for synchronous and asynchronous model APIs,
  output-token controls, output continuation, cancellation/follow-up recovery,
  and session reasoning settings.
- Version-checked native file mutations, bounded no-progress detection,
  session task notes and scoped historical event lookup.
- Opt-in, consent-bound skill learning, source provenance, review-only drafts,
  evaluation and constrained publication/rollback; no general autonomous
  improvement or real-model quality guarantee is claimed.
- Private text attachments, local readiness checks, unlimited default task
  lifetimes, desktop settings/approval improvements and removal of obsolete
  runtime backup and legacy import UI operations.

The accumulated changes are interdependent. Commits are grouped by backend,
desktop and delivery concerns rather than pretending the recent three harness
features are the entire pending change. Local configuration, credentials,
databases, installed packages, logs and build caches are not source inputs.

## Verified evidence

- Go module verification passes.
- The three updated Go executables built successfully and were installed into
  the existing development runtime with an orderly restart. Configuration was
  unchanged; SQLite quick_check passes and session/run/event/execution counts
  and pending execution states were preserved.
- The isolated binary-level desktop/ACP smoke passes 84 checks with no cleanup
  errors. All 56 recorded mock-provider model requests use streaming; offered
  schemas include the continuity tools and file-write version preconditions.
- Focused race tests for file versions, notes/history, progress detection,
  delegated-role isolation and ACP failure reporting pass in four packages.
- Source identity protection passes 28 fixtures; Windows package failure,
  archive, checksum and environment protection passes 30 fixtures. Build
  entry-point and source/skill input checks pass.
- Candidate credential-pattern scanning found six matches, all reviewed
  synthetic credential-rejection test literals. No real credential was found
  by that scan; this is not a guarantee that arbitrary secrets are detectable.

## Blocking and remaining checks

The initial all-module local regression failed with 40-second deadlines in
mixed foreground/background role restart and automatic wakeup tests. The first
Windows Actions candidate also failed. Neither run is release evidence. After
the Windows fixes described below, a complete serial local Windows regression
passes, including those mixed-role tests, without changing product timeouts or
weakening workspace checks. Clean-checkout Actions packaging is still required.

The Windows workflow must run Go regression, Rust Bridge tests, source identity
checks, both desktop protocol smoke modes, packaged Bridge verification,
desktop settings-state tests and desktop ACP contracts before uploading ZIPs.
Failed builds retain diagnostics but must not upload distributable packages.
Binary/source ZIPs and checksum files must identify the same clean commit.

Real GUI interaction, online provider quality, multi-day soak and an environment
that actually has no Python/uv/pip installed remain unverified. Stock hosted
runner execution and no interpreter installation step do not prove the latter.
Do not promote the candidate to a stable release merely because compilation or
fixture-based protocol tests pass.

## Windows CI correction and local revalidation

The failed Windows run used an 8.3 user-directory alias (`RUNNER~1`) in TEMP.
`filepath.EvalSymlinks` expanded that alias, exposing two separate problems:

- Internal test fixtures bypassed session attachment but supplied raw temporary
  paths where the production contract requires a canonical host workspace.
  Shared canonical fixtures now preserve that contract. Assertions about
  published configuration, session and executable identities use the canonical
  values, including the Buzz TOML workspace expectation.
- Real attachment paths, OpenCLI managed-shim package paths and skill archive
  source/output comparisons mixed long and short spellings. These entry points
  now normalize consistently. Attachment spelling expansion does not follow
  symlinks, and `os.Root` still confines the final open. The strict saved-CWD
  comparison, directory identity checks and scoped skill authority are unchanged.

A separate Windows DACL assertion depended on SDDL printing a numeric SID.
Hosted Administrator accounts may print `LA` instead. The test now checks the
actual allowed ACEs, full-access masks and user/System SIDs; production ACL
creation is unchanged.

Local results for the corrected source:

- Reproduced all user-reported failures before editing by setting process-local
  TEMP/TMP to a real 8.3 alias. No system environment or installed configuration
  was changed.
- The 12 affected packages pass their complete suites under that alias.
- `GOMAXPROCS=2 CGO_ENABLED=0 go test -mod=readonly -count=1 -p=1 -json
  -timeout=30m ./...` passes: 34 tested packages, 3110 passing test/subtest events,
  zero failures, from 18:21 to 18:57 China time. This is an uncached full run,
  not a targeted-test substitute. Module verification also passes.
- Seven affected/security/streaming packages pass `-race`; the originally
  reported cases pass three repeated iterations. Buzz unit tests, `go vet` and
  full race tests pass with the same short TEMP/TMP.
- Build-entrypoint checks, 28 source-identity fixtures, 30 packaging fixtures
  and workflow `actionlint` validation pass.
- Rust Bridge release tests pass (17 unit and 2 process tests). Rebuilt native
  Bridge/Go binaries pass all four Bridge integration tests plus Buzz's native
  permission allow/deny integration. These checks cover the four Bridge cases
  skipped during the generic Go run.
- Desktop release tests pass: 50 ACP contracts (3 authenticated external-agent
  tests remain ignored) and 144 settings tests, including both startup/failure
  configuration-state assertions. Local Rust is 1.95.0; Actions uses its pinned
  1.96.0 toolchain, so local success does not replace that gate.
- An isolated development assembly passes both desktop protocol smoke modes
  (91 and 89 recorded checkpoints, no cleanup errors). All 107 recorded mock
  model requests use streaming. This assembly uses rebuilt Go/Bridge binaries
  and cached desktop binaries; it is explicitly not a distributable source-
  matched archive. Actions must rebuild the clean commit and both smoke modes.

The full Go run records 18 environment-dependent skips. They include unavailable
symlink privileges, real Docker/WSL/online services and separately configured
Rust Bridge integration. Short-path attachment and archive tests ran; the
OpenCLI and workspace normalization alias assertions ran before their symlink
sections skipped. Skips are not successes, and no privilege or security check
was disabled to eliminate them. New Windows-specific regression tests explicitly
exercise real aliases, outside resources and archive-output containment.

All model transport remains streaming by default; no model-call implementation
or user configuration changed in this correction. Live desktop executables,
configuration, session databases and in-flight tasks were not modified.

## Follow-up Windows cumulative test timeout

The subsequent Actions runs on `4567b6a5` also failed and are NOT release
evidence. Desktop run `38244979273` exhausted the runtime package's cumulative
30-minute test deadline. The reported
`TestSkillLearningExtractionChunkingPreservesSingleEpisode` had been running
for only two seconds; its stack was in SQLite WAL `FlushFileBuffers` during
commit. The log does not establish a deadlock in that test. The preceding full
local run completed that case in 1.52 seconds. Harness run `38244975275` also
reported source-publication polling, cancelled-turn follow-up and maintenance
generation deadlines on Windows; Ubuntu passed. Slow durable I/O is consistent
with these failures but is not proof that every deadline has the same cause.

The existing artifact-size configuration changes were committed first as
`dd816f65`, separately from this test infrastructure correction, as requested.

Both Windows packaging and Go CI now use `scripts/test-go-harness.ps1`:

- Discover the actual Test/Example/Fuzz inventory and execute all top-level
  cases and their subtests in batches of at most 32 using ordinary `go test`.
  Each batch preserves the 30-minute normal / 40-minute race ceiling and runs
  uncached. Compilation, vet, package working directories and helper processes
  retain Go's standard behavior. No test is silently omitted or retried.
- Execute packages serially with `-p=1`, rather than competing SQLite suites
  writing concurrently. Normal and race suites have independent Actions jobs;
  neither shares a runner disk or a cumulative job budget with the other.
- Fail on nonzero exit, missing/duplicate results or failing tests. Manifests
  retain the exact inventory, batches, statuses and per-batch JSONL logs.
  Environmental skips remain explicit. Diagnostics upload even on failure.
- Give fresh follow-up turns their own original 10-second test contexts,
  instead of reusing the stopped turn's remaining lifetime. Source inspection
  waits within its existing 25-second context instead of an unrelated six-
  second polling deadline. Generation failures now report persisted job state.

These changes do not relax workspace checks, worker fencing, product deadlines
or SQLite FULL durability. They do not modify model transport, installed EXEs,
live configuration or session databases. Clean-checkout Windows packaging and
the remaining real-environment acceptance checks are still required.

Local follow-up verification (real Windows, process-local short TEMP/TMP):

- Complete uncached normal regression passes through the shared runner, from
  21:16 to 22:01 China time: 43 packages discovered, 34 with tests, 1,319 top-
  level tests in 72 batches; 3,125 passing test/subtest events, zero failures
  and 18 explicit environment-dependent skips. This includes the artifact-
  size changes in `dd816f65`. The longest batch takes 288.50 seconds.
- Runtime's 266 top-level tests pass in nine batches. The chunking case takes
  1.47 seconds, the auto-patch daily-shared case 2.85 seconds, source inspection
  2.50 seconds and cancellation/follow-up 0.77 seconds. Product worker deadlines
  and durable on-disk SQLite fixtures remain unchanged.
- Runner fixtures pass 388 inventory, boundary, anchoring, skip-recording and
  failure-propagation checks. Packaging passes 30 failure/archive/checksum/
  environment checks. Build entry-point syntax and workflow actionlint pass.
- Complete uncached race regression of `./internal/... ./` also passes through
  the same runner, from 22:02 to 23:33: 34 packages discovered, 29 with tests,
  1,293 top-level tests in 63 batches; 3,087 passing test/subtest events, zero
  failures or data-race reports and 13 explicit skips. The longest batch takes
  649.58 seconds. CI partitions this unchanged race scope into runtime, SDK
  and the dynamically discovered remaining internal packages on separate
  runners, avoiding another cumulative whole-job deadline on slower hosts.
- Rebuilt desktop release tests pass: three artifact-transfer tests (including
  host sizes above the former 200 MiB limit), 144 settings tests and 50 ACP
  contracts. Three authenticated external-agent cases remain ignored. The
  Windows packaging workflow now explicitly gates on artifact-transfer tests
  as well. Local Rust remains 1.95.0; Actions still verifies pinned 1.96.0.
- A freshly rebuilt Go daemon passes all four native Rust Bridge integration
  tests (18.33 seconds), covering those skipped by the generic normal suite.
  Go module verification and the disjoint 34-package race partition union pass.

## Upgrade boundaries

Existing file overwrite/append clients must supply `expected_version` from
`read_file`. Old-contract paused executions must be explicitly cancelled and
restarted as a new user turn; history remains available. Unknown-effect receipts
must be reconciled before repeating external operations. Session task notes are
not long-term verified facts and do not authorize tool execution.

## Local evidence locations

These directories are intentionally ignored and must not enter source archives:

- `.build-cache/release-readiness-20261012/`: current audit and regression logs.
- `.build-cache/harness-runtime-update-20261012-161609/`: binary smoke, streaming
  checks, configuration validation and live migration verification.
- `.build-cache/harness-continuity-final-race-extended.log`: targeted race tests.
- `.build-cache/windows-ci-fix-20261012/`: before/after reproduction, complete
  short-TEMP JSON test log and result/count manifests, affected-package tests,
  race tests, Buzz validation and subsequent local build diagnostics.
- `.build-cache/windows-ci-timeout-20261012/`: full failed Actions logs,
  follow-up regression summaries and runner fixture results.
- `.build-cache/go-tests/`: discovered test inventories, per-batch JSONL logs
  and normal/race manifests from the shared regression runner.

The GitHub run result and artifact identity must be checked after dispatch;
they are not predeclared in this document.
