# Local desktop MVP closure

## Scope

Windows single-user desktop Agent: configure a model, attach UTF-8 text or images,
run tools under approval, obtain artifacts, stop and continue chatting, retain
history across restart, and optionally draft skills for human review.

No runtime backup/restore product, PDF/Office/OCR parsing, remote HTTP service,
multi-tenancy, cloud sync or new provider integrations. Existing user data,
backups and running installations are not removed. Session persistence,
checkpoints, receipt reconciliation and skill rollback remain. Configuration
save rollback remains an internal write-safety mechanism, not a restore product.

## Implementation stages

1. Remove runtime backup CLI, implementation, dedicated tests and package inputs.
   Move unrelated business suite documentation out of the retired manual.
2. Snapshot explicitly attached text under session ownership and provide bounded
   model reads. Do not grant access to arbitrary host paths. Preserve images.
3. Add local capability readiness and guided review-only learning setup, without
   network requests or silently granting paid work, sharing or auto-publication.
4. Freeze source and verify affected paths, Go regression/build, desktop build
   and isolated package smoke. Record failures and limits without claiming GUI
   or real-provider acceptance from deterministic fixtures.

## Progress

Additional user requirement: default task, native child-role and foreground
queue time limits are disabled (0). Multi-day work keeps context compaction,
manual cancellation, tool/connection timeouts and lease fencing. Model calls
remain cancellable; per-request timeouts depend on the provider. Explicit existing
configuration and durable budget identities are preserved, not silently reset.

- Stage 1: runtime backup sources and package dependencies removed; historical
  records are retained with supersession notices. User data is untouched.
- Stage 2: text snapshots, session-scoped list/read tools and desktop ACP
  embedded text resources implemented. Focused Go, native local-provider
  round-trip/reopen and desktop ACP tests pass.
- Stage 3: cached local readiness mounted in common settings; confirmed
  review-only learning preset and prerequisite/stage guidance implemented.
  Configuration/source changes invalidate readiness. No automatic network test,
  source grant, budget increase, cross-session sharing or publication is added.
- Stage 4: final-source full regression, native desktop release build and
  isolated development package verification completed. Both protocol smoke
  modes and packaged Bridge tests pass. GUI, real-provider and multi-day soak
  checks still require separate acceptance.

## Verification notes

- Unlimited-time tests cover absent/zero/72-hour configuration, save/launch
  agreement, parent-role bounds, lease fencing, cancellation, and fake-clock
  multi-day accounting plus reconstruction. These are not a multi-day soak.
- External ACP setup has a separate two-minute deadline; it is cancelled before
  prompt dispatch and never becomes the lifetime of the delegated task.
- Desktop ACP release tests: 17 passed. The initial Waku `--bin` invocation
  compiled the UI but discovered zero tests; it is not counted as UI test
  coverage. The correct `--lib` settings regression passes all 132 tests.
  The preset fixture was corrected to include the nested settings objects
  present in real saved snapshots, and verifies behavior settings are preserved.
- The initial 15-minute Go run timed out in the expanded runtime suite. An
  imported-skill fixture also included fake credential markers that correctly
  prevented generation before its intended unmanaged-publication assertion;
  the fixture was repaired without weakening the production credential guard.
- A later full run exposed two obsolete tests that rejected zero timeouts.
  They now reject negative values. Complete launch and desktopconfig package
  reruns pass, alongside the positive unlimited-time tests.
- Repeated full regression exposed a real maintenance heartbeat shutdown race:
  successful handler completion cancelled an in-flight renewal, which could be
  misclassified as a storage failure and retried. A deterministic connection-pool
  test reproduces it. The fix ignores only cancellation inherited from work
  shutdown; a renewal timeout while work is live remains a storage failure.
  Both cases pass three repetitions. Final-source regression is tracked below.
- Isolated package protocol smoke passes in normal and legacy-empty-MCP modes,
  including text snapshot reads after deleting both the original and desktop
  copy and restarting the service. Packaged Bridge tests pass. The first smoke
  run exposed an obsolete non-streaming-only extraction assertion, fixed to
  accept the existing streaming extraction transport. The final Go binaries
  include the heartbeat fix and pass both smoke modes again.
- The Windows build entry point, README and CI use a 30-minute per-package test
  runner timeout for the expanded regression suite (CI race tests: 40 minutes).
  This test-runner guard is not a product/session timeout.

## Final verification

Completed on 2026-10-11, using local deterministic providers and isolated data.
No paid provider calls or GUI/visual testing were performed.

- Go: `go test -mod=readonly -count=1 -p=2 -timeout=30m ./...` passes all
  34 tested packages; 8 packages have no test files. The following
  `go build -mod=readonly -p=2 ./...` also succeeds. Both commands ran from
  `go-harness` with `GOMAXPROCS=2`, after the heartbeat fix.
  Log: `.build-cache/mvp-closure-go-verified.log`.
- Desktop: 17 ACP tests and 132 settings tests pass; Waku and waku-daemon
  release builds succeed. Logs: `.build-cache/mvp-closure-rust-core.log`,
  `.build-cache/mvp-closure-rust-settings-final.log` and
  `.build-cache/mvp-closure-rust-desktop-final.log`.
- Bridge: 17 unit tests and 2 integration tests pass. Packaging source identity
  tests pass all 28 fixtures. Logs: `.build-cache/mvp-closure-bridge-tests.log`
  and `.build-cache/mvp-closure-source-identity-tests.log`.
- Final package: normal and legacy-empty-MCP protocol smoke each return
  `ok: true` with no cleanup errors. Packaged Bridge tests pass. Fresh-package
  snapshots confirm task, queue and child-role timeout defaults are zero.
  Log: `.build-cache/mvp-closure-package-verified.log`.
- Package inspection: all 83 original manifest entries pass SHA-256 checks;
  the retired backup executable/manual and `user-data` are absent. Packaged
  credentials are environment placeholders, not copied user secrets.
  Added `VERIFICATION_NOTES.md`, regenerated the manifest without including
  the manifest itself, and verified all 84 final entries. `git diff --check`
  passes.

## Delivery and acceptance boundary

New isolated development package:
`dist/desktop/mvp-final-20261011/DeerFlow-Desktop-Go`.
Launch `deerflow-desktop.exe` from that directory for manual acceptance.

`BUILD_SOURCE.json` correctly records `development-dirty`, not an exact clean
release identity. `BUILD_PACKAGE.json` records `regression_tests` as
`skipped_development` because packaging used `-SkipTests` after the independent
final-source regression above. It separately records both smoke modes and
packaged Bridge tests as passed. No formal release ZIP or commit was created.

The existing `dist/desktop/DeerFlow-Desktop-Go-20261010-103944` installation,
running application, user configuration, history and prior backups were not
replaced or removed.

Task timeout zero removes the cumulative execution deadline; it does not
disable cancellation, provider errors, configured quotas, tool/connection
timeouts or stale-worker lease fencing. Existing explicit positive time limits
and persisted task budgets are preserved. Set the applicable settings to zero
and use a new request to adopt the unlimited policy for an older installation.
External ACP initialization/session setup remains bounded independently of the
unlimited prompt lifetime. Context compaction remains enabled by default.

Fake-clock multi-day accounting, persistence/reopen and cancellation tests do
not certify a live multi-day soak, automatic recovery from every power loss,
or uninterrupted provider/network availability. These and GUI/real-provider
acceptance remain outside the completed deterministic verification.
