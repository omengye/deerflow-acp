# DeerFlow capability completion

Scope: document ingestion/preview, session goals, durable schedules, and WeChat
iLink. RAG and a second execution backend are explicitly out of scope.

## Acceptance checklist

- [x] Bounded PDF/DOCX/XLSX/CSV ingestion through opaque session assets; table
  statistics; ACP desktop upload; safe artifact preview.
- [x] Persistent `/goal` controls, metered evidence evaluation, bounded native
  continuation; cancellation, approval, budget and unknown-effect fences intact.
- [x] Once/interval/cron schedules, timezone handling, pause/resume, durable
  occurrences and notifications; no automatic replay of interrupted effects.
- [x] WeChat iLink QR login, explicit sender allowlist, durable input dedupe,
  conversation isolation and uncertain-delivery handling; offline protocol tests.
- [x] Configuration, local management CLI, documentation and focused regression
  tests. Live WeChat and model calls require separate user authorization/setup.

## Constraints

Keep the Go/Eino execution path and authenticated local DFACP management. Do not
modify personal configuration or databases. Existing uncommitted skill/thinking
work is preserved. Background services must cancel and join before storage closes.
No visual testing or desktop process restarts are part of this change.

## Verification (2026-10-17)

- `go build ./...` passes. Full tests pass for assets, documents, automation,
  WeChat, localhost, the control CLI, launch, configschema, tools, ACP agent and
  harness packages. Desktop configuration regression also passes.
- Focused root integration, Goal, execution recovery, output continuation,
  compaction, permissions and session deletion tests pass. Native receipts block
  optimistic Goal completion/continuation; active scheduler shutdown is joined.
- Race tests pass for automation, WeChat and documents. Cases include input
  dedupe, same-batch arrival ordering, uncertain-send fences, logout cancellation,
  retained QR verification across waits/redirects and transactional notifications.
- `cargo check --locked -p waku -p waku-core` passes with existing dead-code
  warnings. `waku-core` DeerFlow unit tests: 47 passed. Artifact unit tests: 4
  passed. No visual test was run.
- Windows packaging scripts parse successfully; Linux packaging script passes
  `bash -n`. `git diff --check` passes. No complete production package was built.

## Deployment checks still required

- [ ] Rebuild and deploy the daemon, desktop and sibling control helper together.
- [ ] Install a maintained Poppler executable and verify real PDF extraction.
  The optional live parser test is skipped on this host because it is absent.
- [ ] Pair a real iLink account, configure the workspace and explicit sender
  allowlist, then verify private text receive/send and scheduled notifications.
- [ ] Confirm Goal evaluations with the chosen real model and opt into automatic
  continuation if wanted; tests use deterministic offline models.

The implementation deliberately provides extracted-text previews rather than
Office/PDF page rendering, text-only WeChat rather than media/groups/WeCom, and
CLI schedule/channel administration rather than a dedicated desktop settings UI.
Schedules require a running daemon; no OS wake-up or scheduler is installed.
See `docs/deerflow-capabilities.md` for configuration and operating commands.
