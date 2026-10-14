# Windows Desktop MVP Candidate, 2026-10-14

## Product Decision

The product scope is a local, single-user Windows x64 desktop MVP for informed
users. The user explicitly confirmed these skill-publication rules:

- The first publication of a newly created skill requires human approval.
- Changes outside the automatic-maintenance scope require human approval.
- Eligible automatic changes to existing managed skills may publish after
  independent security and behavior checks, demonstrated improvement, no
  regression, source/head validation and bounded publication quotas.

Automatic first publication of arbitrary skills is NOT an MVP requirement.
Do not add that feature or weaken review gates to declare this release ready.
The current implementation covers this functional contract and is suitable for
submission as an MVP candidate. Distribution still requires a successful
Windows Actions build for the exact committed source. This is not a declaration
of general stable-release, unattended-operation or multi-user readiness.

## Included Changes

- Simplified Chinese for new desktop preferences. Existing explicit language
  choices and existing user configuration survive upgrades.
- Model-only initial setup with preconfigured memory, subagents and bounded
  skill learning/generation/automatic maintenance. Host suite provisioning is
  local and does not make a model request.
- Maintenance ceilings of 1000 calls / 6000000 tokens per UTC day, with learning
  sublimits of 50 calls / 1000000 tokens; 600 seconds per job and 5 workers.
  Existing-skill automatic patches remain at 40 changed lines and 2 daily
  publications. These ceilings are not a currency spending guarantee.
- Skill Center in Settings, four main tabs, host-owned coverage and attention
  counts, and separate background-wait versus operator-action states.
- Bounded multi-target trusted suite registry, policy-bound decision hints,
  fresh-evidence draft successors, downstream scheduling with FIFO service,
  and continued observation of adopted versions when new publication is off.
- Actionable duplicate-skill conflicts and tool-effect reconciliation UI.
  Unknown tool outcomes remain paused; confirmation does not replay tools.
- Native templates and startup recovery fixtures match the shipped defaults.
  A valid native `auto_patch` configuration no longer requests legacy migration.

## Scope And Security Disclosure

New desktop profiles enable local commands, host tools and `dangerous` mode.
This is a trusted local-user agent, not a security sandbox for untrusted skills
or prompts. Optional browser integrations and external search credentials are
not included. Model and maintenance activity may incur provider charges.

Only the independently prepared two-case
`managed-evolution/searxng-web-search` assessment is bundled. The package does
not install the operator's personal skill, endpoint, credential or history.
Other skills need their own trusted coverage after initial approval. A fresh
profile may legitimately report partial automation with no eligible target.

No new-skill automatic-publication policy, enforced per-skill runtime isolation
or general semantic coverage is claimed. Publication observation uses existing
operator-confirmed evidence; it is not a new autonomous post-release quality
oracle. These are explicit product boundaries, not hidden enabled capabilities.

## Evidence And Release Gates

The preceding commit `0cdf16cfe6bc084265379b782a4434f08613a526` passed the
[Windows desktop workflow](https://github.com/omengye/deerflow-acp/actions/runs/38272644530).
That result does not validate the pending changes. Its separate
[Go workflow](https://github.com/omengye/deerflow-acp/actions/runs/38272642011)
failed two Windows runtime race cases during generation validation with
`interrupted (9)` / `storage_error`; normal Windows/Linux regressions and the
other race jobs passed. No `DATA RACE` report was identified in the failed log.
The interrupted storage operations remain relevant evidence, not a successful
run or a proven harmless timeout.

For the current source, the local Release desktop/daemon and four Go binaries
were built and installed with orderly shutdown/restart. Both existing backend
instances retained their distinct data directories and loaded the unchanged
configuration. All 96 installed manifest entries verified; desktop responded.
This is process/configuration evidence, not visual acceptance. The source-hash
audit found no changes in the 1855 recorded source files since that build before
the documentation cleanup in this release review.

First-run template and model-only save checks passed previously (8 Go
test/subtest events), as did two focused Rust language/preference tests.
The earlier SearXNG model assessment passed its two existing cases; it did not
exercise a real search service or establish quality for other skills.

The focused race rerun reproduced `interrupted (9)` while a completed handler
cancelled an in-flight maintenance heartbeat. SQLite does not always wrap
`context.Canceled`, so the former context-only check misclassified that shutdown
as a storage failure. The fix recognizes SQLite's interrupt code only when the
renewal context inherited the worker's cancellation/deadline. A live renewal
timeout, earlier independent renewal timeout, I/O error or other storage failure
still fails closed. Lease durations, job deadlines and publication checks are
unchanged. This fix is in the candidate source, not in the earlier local hot-swap.

Current local review results:

- Eleven named Go tests across eight affected packages pass, including first-run
  defaults, existing configuration preservation, registry authorization, budget
  scheduling, duplicate-skill selection, automatic publication/rollback and
  fresh-evidence draft successors.
- After the heartbeat fix, both previously failing runtime race tests pass
  (119.943 seconds), together with the existing heartbeat shutdown/timeout test
  and a focused eight-case error-classification regression (14.081 seconds).
  This bounded rerun does not replace the full Actions race result.
- Five Rust receipt-recovery tests and one automatic-maintenance preset test
  pass in Release. Existing unused/dead-code warnings remain; no build error.
- Build-entrypoint syntax, delegation and source/skill input rejection checks
  pass. Go formatting and `git diff --check` pass.
- Pending-input review excludes program binaries, local configuration, user
  data, databases and logs. Credential-pattern hits were limited to two existing
  synthetic proxy-redaction fixture lines, not actual credentials. Pattern
  scanning cannot certify the absence of every possible secret.

No new model calls, business-skill cases, visual tests, full local suite or stress
workload were added. Only the reproduced heartbeat defect gained a small
classification regression. Existing Actions regression and packaging gates
remain intact.

## Submission And Distribution

Submit directly to `codex/eino-harness-acp-refactor`, preserving the accumulated
related source changes. Exclude local configuration, credentials, user-data,
databases, binaries, build logs and caches. No deleted personal skill is restored.

Dispatch **Build Windows Desktop** for the submitted branch. It delegates to
the existing Windows 2025 workflow and runs dependency/source checks, Go
regressions, Bridge verification, both packaged protocol smoke modes, desktop
settings/ACP/artifact contracts, and source-matched ZIP/checksum generation.
Only successful gates permit uploading the distribution artifact. Push-triggered
Go CI remains separate and any failure must be reported rather than hidden.

The expected artifact is `DeerFlow-Windows-Desktop-x64`, containing Windows and
corresponding-source ZIPs plus SHA-256 files. A manual workflow dispatch does not
create a GitHub Release. Do not create a tag, merge to main or publish release
assets as part of this request. Report the actual commit, workflow URL and result;
dispatch alone is not build success.

## Remaining Acceptance Boundaries

Real GUI walkthroughs, broad provider quality, multi-day soak, code signing and
a genuinely interpreter-free Windows host are not certified by this review.
The MVP is an unsigned portable package, not an installer. These limitations
must remain visible to early users; they do not change the user-approved
first-publication/manual-exception skill policy.

Local logs are intentionally ignored under `.build-cache/mvp-review-20261014/`.
They must not be included in source archives.

## Failed Candidate And Local Windows Revalidation

The Windows desktop run
[38505623555](https://github.com/omengye/deerflow-acp/actions/runs/38505623555)
for `9219387e` failed the SDK regression gate and produced no verified desktop
package. The separate Go workflow also exposed outdated assertions in the
runtime learning and desktop-template tests. The earlier focused check did not
cover these assertions and must not be cited as full release validation.

Local Windows execution reproduced all three deterministic failures:

- Learning-cap tests expected a claimed, terminal `budget_exhausted` job, but
  the approved scheduler defers an unclaimed job until quota is available.
  Updated assertions require queued state, zero attempts, no deadline, no model
  call, no output and no refund of earlier calls. Per-call hard budget tests are
  unchanged and remain in the complete regression.
- The common template test still required learning auto-publication to be off
  in desktop templates. It now distinguishes desktop `auto_patch` from ACP
  `review`, while checking fail-closed security, no automatic new-skill discovery
  and no cross-session sharing.
- A Windows SDK discovery fixture sometimes exhausted a separate 12-second
  polling timer before its durable background pipeline completed. It now uses
  the existing 30-second scenario context and includes match-state diagnostics
  on failure. Product deadlines, retries, worker leases and assertions about
  exactly one generated pending-review candidate are unchanged.

Only these four test files changed; no runtime policy was relaxed. Before any
push or new Actions dispatch, the local Windows delivery entry point ran with
`-Package Desktop -Development` and without `-SkipTests`. Development mode avoids
claiming that uncommitted test changes are a clean source archive; it does not
skip the regression, compilation or package checks.

Verified on 2026-10-14, Asia/Shanghai:

- Complete uncached normal Go regression passed from 12:34:46 to 13:15:24:
  43 discovered packages, 34 tested packages, 1332 top-level tests in 72 batches,
  3165 passing test/subtest events, zero failures and 19 explicit skips.
- The four affected top-level tests passed both ordinary and race runs across
  SDK, desktopconfig and runtime. The race run took 45.263, 13.002 and 8.662
  seconds respectively; no race was reported. This was not a full race rerun.
- Desktop Release contracts passed: 146 settings tests, 51 ACP tests and three
  artifact-transfer tests. Three authenticated external-agent ACP cases remain
  ignored rather than counted as success.
- Rust Bridge passed 17 unit and two process tests. Dependency verification,
  source identity checks, build entrypoint checks, 388 test-runner fixtures and
  30 packaging failure/archive/checksum/environment checks passed.
- The local Release package passed both desktop protocol smoke modes, with no
  cleanup errors, and all four actual bundled Bridge/daemon integration cases.
  Those four cases were skipped in the generic Go run and covered here.
- All 84 local package manifest entries match; the package contains no
  `user-data`. The output is a development directory, not a clean-source ZIP.

The remaining generic Go skips require symlink privileges, real Docker/WSL,
online services or installed OpenCLI. They are not implicit successes. No real
model calls, new business-skill evaluation suite, visual test or live application
replacement was performed. Local Rust remains 1.95.0; Actions independently
uses pinned Rust 1.96.0. Logs are under the ignored
`.build-cache/windows-mvp-fix-20261014/` and
`.build-cache/go-tests/20261014-123446-360-normal-382510bd/` directories.

After committing these test corrections, push and dispatch the unchanged Windows
workflow for the new commit. Its eventual result must still be checked; the
failed `9219387e` run is not retroactively successful.
