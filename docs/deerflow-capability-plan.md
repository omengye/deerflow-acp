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

## Initial implementation constraints (2026-10-17)

Keep the Go/Eino execution path and authenticated local DFACP management. Do not
modify personal configuration or databases. Existing uncommitted skill/thinking
work is preserved. Background services must cancel and join before storage closes.
No visual testing or desktop process restarts were part of that initial change.
The 2026-10-18 follow-up explicitly includes local testing, release packaging and
replacement of the installed programs with a user-data backup.

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

## Initial deployment checks (2026-10-17)

- [ ] Rebuild and deploy the daemon, desktop and sibling control helper together.
- [ ] Install a maintained Poppler executable and verify real PDF extraction.
  The optional live parser test is skipped on this host because it is absent.
- [ ] Pair a real iLink account, configure the workspace and explicit sender
  allowlist, then verify private text receive/send and scheduled notifications.
- [ ] Confirm Goal evaluations with the chosen real model and opt into automatic
  continuation if wanted; tests use deterministic offline models.

The implementation deliberately provides extracted-text previews rather than
Office/PDF page rendering, and text-only WeChat rather than media/groups/WeCom.
Native Agent schedule tools now share the host scheduler across desktop, WeChat
and Buzz, with trusted source binding, ownership checks and durable notifications.
Native Settings pages now manage existing schedules and channels; time edits
preserve task permissions and history, and newly created schedules default to
Full Access only through the trusted host interface or explicitly authorized
Agent/channel tools. Remote messages never establish their own authorization.
Schedules require a running daemon; no OS wake-up or scheduler is installed.
See `docs/deerflow-capabilities.md` for configuration and operating commands.

## Conversational scheduling follow-up (2026-10-18)

The previous settings-only implementation exposed no scheduling tools to the
model. The follow-up adds native `schedule_read` and `schedule_manage` tools to
foreground conversations, with host-authored source identity and authorization.
Desktop, WeChat and Buzz share the scheduler and settings cards. Creation is
idempotent; edits use revision fences; new schedules default to Full Access.
Read-only sessions, child agents and unattended runs cannot create schedules.
Channel access is scoped to account, sender and workspace; cross-channel owner
links require an explicit local-admin operation. Notifications have their own
durable delivery state and never cause task execution to be repeated.

Buzz requires the local `schedule_full_access` setting and a restricted sender
allowlist or verified owner. ACP v2 approvals read the actual tool-call subject,
not its display title. Lease refresh and result delivery run independently of
foreground model turns. A stale or revoked grant prevents unattended dispatch.
The retained Bridge resume fix also avoids unnecessary long-history replay.

Release acceptance covers native tool receipts, channel ownership, revocation,
retry/deletion/notification safety, Windows and Linux regressions, the packaged
desktop smoke scenario and the real Buzz-to-Bridge-to-daemon integration using
deterministic local model and transport fixtures. Real provider delivery remains
a separate check; tests must not send messages or model requests to real users.
