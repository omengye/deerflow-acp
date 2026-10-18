# GitHub Actions run 39039886777

Run: https://github.com/omengye/deerflow-acp/actions/runs/39039886777

Commit: `9ec44ae40aba8d6ac6f9217da6f18a8b115b1822`

Branch: `codex/eino-harness-acp-refactor`

## Failure classification

Five of eight jobs failed, with two independent causes. The downloaded JSON test
artifacts contain assertion failures, not a race detector report or build failure.

| Jobs | Failing top-level tests | Cause |
| --- | --- | --- |
| Linux normal, Linux runtime race, Windows runtime race | Three skill-learning approval/credential tests, with nine failed subtests per job | Admission durably rejects a candidate but returns an empty proposal |
| Windows normal, Windows SDK race | `TestChannelNewCreatesEmptySessionWithoutModelAndPreservesOld` | Raw temporary directory spelling compared to canonical session CWD |

The two internal race jobs and Linux SDK race job passed. The regression runner
stops at a failed batch, so later batches and build/package steps in that run were
not established as passing. Node.js deprecation warnings were not the failure.

## Approval response regression

`recordApproval` can persist `stale` for invalid discovery evidence or `failed`
for credential-like candidate content, then return the corresponding conflict or
validation error. The committed `Approve` handler immediately returned
`Proposal{}, err`, discarding the durable state from its response.

The failing tests expected both the rejection error and the updated proposal.
Their messages about publishability therefore do not demonstrate that unsafe
content was published: the recorded errors are actual rejection errors. The
bug is the missing authoritative state in the response, also relevant to callers
displaying the reason/state of a failed approval.

The existing uncommitted fix reads the persisted proposal after admission fails
and returns it alongside the original error. Its bounded read uses
`context.WithoutCancel` so cancellation cannot hide an already committed result.
Missing/revoked proposals still retain the original error. No validation guard is
removed or downgraded.

Reproduction uses Go's source overlay against the exact committed `review.go`,
without replacing working files. The receipt and legacy-credential subtests fail
with the same empty-proposal output. Applying only the seven-line durable-state
read to that committed handler makes those tests pass. A new focused unit test
also rejects an incorrect review state, compares returned and stored proposals,
and verifies that no approval or installed-head mutation occurred.

## Windows temporary path fixture

Session attachment intentionally resolves workspace aliases with
`filepath.EvalSymlinks`. The `/new` test supplied raw `t.TempDir()` and then checked
`stored.CWD == rawPath`. On Windows, a TEMP/TMP path with an 8.3 alias resolves to
a different long spelling of the same directory.

A local fixture on C: obtained a real short alias with `GetShortPathName` and used
it as TEMP/TMP. The unmodified test reproduced the CI failure. The test now uses
the existing `testutil.CanonicalTempDir` helper and retains exact equality against
the expected canonical path. A Windows-only regression deliberately supplies the
short alias while checking the long path, model-call count, empty new history,
old history retention and binding failure behavior.

This changes test fixtures only; production workspace normalization remains intact.

## Additional failures found before submission

Latest-source validation on Windows and Linux exposed two additional failures
that the original stopped run did not establish as passing.

### Virtual-environment Python on Linux

The local command backend canonicalized the selected interpreter and also used
that resolved path as its invocation name. A typical Linux venv interpreter is a
symlink to system Python. Losing its original name made both readiness and actual
execution leave the configured virtual environment. Windows venv launchers did
not reproduce this failure.

The backend still validates and launches the canonical allowlisted binary, but
preserves the invocation name in `argv[0]`. A regression verifies that retargeting
the alias to an unapproved executable is still denied. No shell is introduced.

There was also a metadata-probe compatibility issue: Python before 3.14 skips
venv prefix initialization under `-S`. Debian/Ubuntu's `site.getsitepackages`
depends on those prefixes even when supplied an explicit prefix list. The probe
now restores just the venv prefixes, honors the system-site flag, uses the true
base prefixes on newer Python, and preserves venv-first distribution precedence.
It retains `-I -S -B`, never imports dependencies or initializes site hooks.

New tests fail against the pre-fix Linux snapshot, then pass after the fixes.
They cover real venv-only dependency execution, metadata without `.pth` or
customization execution, package precedence and system-site isolation. Fixtures
use the interpreter's actual `sysconfig` installation path rather than the last
entry in a list of possible site paths. No host packages were installed.

### Nested-delegation race test deadline

The Windows race batch hit a test-only 30-second context deadline at depth three.
Two isolated repeats passed in 21.49 and 21.35 seconds at that depth; a CPU-profiled
repeat passed in 23.60 seconds, with 23.55 seconds of samples across a 23.61-second
profile. The profile shows active race instrumentation and checkpoint JSON work,
not an idle deadlock. Under concurrent local suites the same operation exceeded
the old deadline.

This durable-correctness test now allows two minutes while retaining all effect,
checkpoint lifecycle, budget and tool-boundary assertions. Production deadlines,
role time limits and dedicated cancellation/timeout tests are unchanged. The
complete affected race package is rerun rather than accepting isolated retries.

## Verification scope

Evidence is saved in `.build-cache/actions-39039886777-*`, including downloaded
job logs/artifacts, old/fixed Go overlays, red-test logs and local JSON test output.
Both platforms use Go 1.26.8 and `GOMAXPROCS=2`, with serial package execution and
`-parallel=2`, matching the relevant CI settings. Linux runs inside WSL against a
fresh source snapshot outside the live installation.

The user requested latest-source local validation before submission. Earlier
full-package results are supplemented with post-fix affected-package runs; a
source hash manifest identifies the tested inputs. No remote rerun is used to
test the old commit in place of the corrected sources. No live installation,
operator proposal, cloud resource or user configuration is modified by this
CI diagnosis and verification.

### Local results and evidence

- Ordinary Go regression covers all 51 packages on each platform. Windows first
  passed the complete 83-batch run, then reran the seven affected packages after
  the venv fix. Linux coverage combines the initial clean batches, the resumed
  remainder, and a final five-package affected-scope run. These are not claimed
  as one uninterrupted Linux run: initial missing-fixture/proxy setup failures
  and the subsequently fixed venv failure remain in the diagnostic history.
- Final ordinary coverage, replacing older results for rerun tests, is 3,432
  passed test/subtest events plus 21 skips on Windows, and 3,438 passed plus 13
  skips on Linux, with zero outstanding failures. Counts differ by platform.
- The exact failed runtime batch passes normally and with race instrumentation
  on Linux, and with race instrumentation on Windows (142 test/subtest events
  per run). The Windows SDK failing batch passes normally and with race using
  a real short-path TEMP alias (56 each). The new admission result test also
  fails against an overlay of the original handler and passes with the fix.
- Final Windows affected internal race coverage passes across nine packages:
  1,490 test/subtest events and 12 environmental skips. The matching nine-package
  Linux race run passes with 1,491 events and six skips. Separate post-fix SDK
  Python/command race tests (22 each) and ACP client race tests (12 each) pass
  on both platforms. The full original eight-job remote matrix is not presented
  as having been run locally in its entirety.
- Both platforms pass `go mod verify` and `go vet ./...`. Desktop `cargo check`
  passes; 195 Waku and 47 Waku-core DeerFlow contract tests pass. Rust Bridge
  tests, Buzz ordinary/race/vet, and built-Bridge/Buzz integrations pass.
- The batching runner passes 388 fixture checks, source identity 28, Windows
  packaging 30, plus build-entrypoint and updater migration/preservation tests.
- Fresh Windows and Linux portable packages build and pass start/status/stop.
  Windows Buzz development packaging and the Linux Buzz executable build pass.
  The first Windows Buzz release-package attempt correctly refused the dirty
  source tree; it was not weakened or misreported as a product-test failure.
- A new Windows desktop release build at
  `dist/desktop/DeerFlow-Desktop-Go-CIFix-20261018-1145` passes actual Waku/ACP
  smoke twice: 93 check events without Python and 97 with Python, both with no
  cleanup errors. Check counts include variable status polling. The package is
  accurately marked `development-dirty`; it has not replaced the live install.

Environmental skips include unconfigured live Docker/WSL/web services and
Windows symlink privileges. The alias security regression passes on Linux.
Ordinary suites skip built-Bridge tests until binary paths are supplied; those
tests then pass separately against freshly rebuilt artifacts on both platforms.
No visual UI, real cloud/SSH or real-model verification is claimed.

The source manifest `.build-cache/actions-latest-final-source-manifest.json`
records 80 modified/new inputs (59 under `go-harness`). Source hashes are checked
against both the Windows workspace and Linux copy. Final batch manifests,
per-test JSON, build/smoke logs and coverage summaries are under
`.build-cache/actions-latest-*`; earlier red reproductions remain separately
available. The diagnosis documentation is updated after tests without changing
tested production or test inputs.
