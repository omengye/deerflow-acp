# Skill validation feedback repair

Date: 2026-10-10 (Asia/Shanghai).

## Incident

The two `todo-extract` candidates had the same package hash but different
submission reasons. The first proposal, `p_43NUEHBVHHGWEKCD4P254PSAEX`, passed
security and failed quality with an explicit missing output-consistency check.
Its original submit/get receipts contained the generic validation error, not an
empty error. The second candidate used a diagnostic reason, passed review and
was later published by the operator. This was not a controlled source comparison
and does not establish a `managed-evolution` service outage.

The tool projected security/behavior but omitted the quality decision and its
explanation. The reviewer consumes the submission reason and does not route by
source ID. The preferred managed-source label is a storage policy, not a probe.

## Changes

- Submit/get results expose quality, bounded redacted model-check explanations,
  input hashes, error_code, failure_stage, retryable and next_action.
- New proposal failures persist a host-selected classification separately from
  the immutable evaluation digest. Legacy failures can expose explicit rejected
  checks without rewriting old reports or tool receipts. Missing evidence stays
  unknown, and specific existing host errors are retained safely.
- Security/quality rejection, invalid/incomplete response, timeout, cancellation,
  budget exhaustion, permanent request errors and recognized transient failures
  are separate outcomes. Provider error bodies and endpoints are not returned.
- Maintenance review uses classified retry eligibility while keeping the existing
  queue error enum, accounting, three-attempt cap and deadline. Terminal jobs do
  not advertise further retries and retain completed security evidence.
- The tool and skill-creator guidance distinguish validation from publication,
  preserve original requirements in reason, and reject source switching or
  weakened requirements as a repair strategy. Review prose is untrusted evidence.
- The external experiment's results.json note was corrected; its six case records
  and the published skill were not changed.

## Verification

Focused tests exercise both ordinary tool submission/get and queued maintenance:

- Quality rejection with security passed, including useful redacted reasons.
- Failed quality transport, malformed/truncated replies, 401, 429 and 503.
- Cancellation, budget limits and temporary versus permanent DNS errors.
- Identical content/reason across sources yields identical review input hashes;
  changing reason changes the hashes and the deterministic fixture decision.
- Legacy projection, replay without new model calls, reopening persisted results,
  source/ownership boundaries and unchanged publication authority.
- Permanent queued errors stop once; transient errors stop at the existing cap;
  generation is not repeated and partial review evidence is preserved.

The focused test selector covers the root package, internal/skills/evolution and
internal/engine/eino. Tests use local fixtures, not external paid providers.
The ACP client, ACP daemon and local skill-authoring commands also compile.
The updated skill passes the native local package validator (behavior: not_run).

## Deployment boundary

The initial source repair did not replace the running desktop release. The
operator subsequently authorized the minimal deployment recorded below. This
repair does not enable automatic new-skill creation, publish a skill, or migrate
old tool receipts. Cross-source resubmission guidance is not a new registry
deduplication policy or a guarantee of model compliance.

## Minimal deployment

Completed 2026-10-11 00:05 (Asia/Shanghai) in
`dist/desktop/DeerFlow-Desktop-Go-20261010-103944`.

- Rebuilt only `deerflow-acpd.exe` from the frozen development-dirty source
  snapshot, with build ID `deerflow-go-skill-feedback-20261010-234556`.
- Replaced that daemon and the two skill-creator documents (`SKILL.md` and
  `references/schemas.md`) in both bundled and import locations. The desktop,
  Waku, Rust Bridge, other Go command binaries and WSL helper were unchanged.
- Gracefully restarted only the idle Go daemon, preserving the desktop's five
  portable environment variables, including its data and log directories.
- The first attempt omitted the portable data-directory environment variable;
  its publication guard caused rollback. The original database was untouched.
  Restored the original backend against the correct database and archived the
  accidentally created empty fallback state without deleting it.
- A second guard compared an export CAS token that includes a refreshed
  timestamp. Confirmed publication rows were identical, then checked durable
  target/head/record fields and synchronized export status instead of that token.
- Verified all 85 package checksum entries, the five replaced paths, unchanged
  configuration, 26 visible sessions (28 including internal children), session
  events, runs, all 456 tool receipts, proposals and publications. The published
  todo-extract head and content are unchanged. Existing receipts remain historical.
- The rebuilt binary passed isolated loopback-provider smoke tests for quality
  rejection and retryable transport failure. No remote paid model was used.

Final backup and verification records are under
`dist/desktop/backups/20261010-234556-skill-feedback-verified`.
The release's `HOTFIX_SOURCE.json` records the build, source snapshot, replacement
hashes and backup path. Deployment scripts and final verification are retained
under `.build-cache/hotfix-skill-feedback-20261010-234556`.
