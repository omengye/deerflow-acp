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

The GitHub run result and artifact identity must be checked after dispatch;
they are not predeclared in this document.
