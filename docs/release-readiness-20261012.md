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

The new all-module local regression is NOT green. It reproduced 40-second
deadline failures in mixed foreground/background role restart and automatic
wakeup tests. Concurrent local Rust compilation was present; that observation
does not establish the cause or excuse failures. Do not increase product
timeouts, weaken assertions or skip tests to make a release pass. Earlier
targeted successes and the 2026-10-11 baseline green run do not replace a
complete passing run of this candidate.

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

The GitHub run result and artifact identity must be checked after dispatch;
they are not predeclared in this document.
