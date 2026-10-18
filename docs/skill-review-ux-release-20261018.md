# Skill publication UX and deleted-skill visibility follow-up

## Cause and changes

The previous desktop approval handler sent ordinary proposals immediately, but
script-bearing proposals only populated `confirm_manage` on the first click.
The second, generic confirmation sent the actual approval. This was explicit
unverified-script acknowledgment, not a second model review.

The desktop now displays that risk notice immediately beside a single explicit
risk-and-publication action. Clicking it sends `proposal.approve` directly with
`acknowledge_unverified_scripts=true`. The request binds the displayed proposal
ID, state version and base, including the explicit null base of a new skill.
Backend acknowledgment, stale-version, mutation/retry and automatic-publication
guards remain intact. Source-review, rejection and rollback confirmations are
unchanged. The action does not run scripts or grant execution permissions.

Deleted legacy skills have no install row. The source-warning query previously
treated their null enablement as an unavailable source worth advertising. It now
excludes matching deleted control records while preserving real unavailable or
unbound source warnings. No history rows were removed.

The installed-skills screen, refresh and name search now request `status=current`.
The host excludes deleted records before pagination, retaining disabled skills.
Explicit deleted-history and legacy unfiltered operator queries remain available;
explicit pinned historical resumes retain their prior semantics.

## Validation

- Red regression tests reproduced both the directory-deletion and legacy-only
  source-warning leaks before the fix.
- Complete Go `internal/skills` and `internal/skills/evolution` suites passed.
- Final focused Go regression: 108 tests/subtests passed, zero failed/skipped.
- Go race checks for catalog pagination and file authority passed.
- Rust desktop settings: 162 passed, zero failures. No visual UI test performed.
- Build-input and full-tree updater PowerShell fixtures passed.
- Release desktop, nine Windows executables and the WSL helper rebuilt.
- Four built-Bridge integration tests passed, including discovery subcases.
- Actual-package WebSocket/ACP smoke passed twice: 102 check events without
  Python and 101 with the existing Python interpreter, zero cleanup errors.
  Counts include variable readiness/status polling. Both exercised the current
  catalog filter through Waku and Go management; Python execution used local
  fixtures and local mocked model endpoints.
- All 70 recorded modified/untracked source inputs retained their pre-build
  hashes through deployment. No commit or release ZIP was created; package
  source state remains accurately marked `development-dirty`.
- `git diff --check` passed.

Evidence logs and safe verification results are under `.build-cache/skill-ux-*`.

## Deployment

Build ID: `deerflow-skills-20261018-092256-093`.

Fresh package:
`D:\Tools\deerflow-api\dist\desktop\DeerFlow-Desktop-Go-SkillUX-20261018-0923`

Replaced installation:
`D:\Tools\deerflow-api\dist\desktop\DeerFlow-Desktop-Go-20261010-103944`

Complete program and profile backup:
`D:\Tools\deerflow-api\.build-cache\skill-ux-deploy-20261018-093518`

The daemon was idle and drained before replacement. The updater verified all
383 profile files without a skill-creator migration. All nine installed EXE hashes
match the fresh package. Config hash is unchanged. The restarted daemon reports
the new build/instance, SQLite read-only `quick_check` is `ok`, and WeChat remains
enabled, authenticated and bound after updating its executable pin to revision 8.

Live `todo-extract` verification returns zero current/enabled entries, one explicit
deleted-history entry, `available=false` and `body_readable=false`. The read-only
database check using the production warning predicate confirms that the deleted
record is not included in current agent warnings. No live model prompt was needed.

Before this follow-up deployment, Huawei proposal
`p_IZF6BALYBSZJX6YBUUHTXPYYJW` was already published at state version 3. Deployment
preserves that state and candidate hash
`b04ca410da1158bf1476225803aeecb27a0b3ddf2159c52dd880754d7bf92d8c`.
Next-turn verification confirms available/readable instructions for an existing
session, with `execution_checked=false`. This follow-up did not approve a live
proposal, connect to a cloud sandbox, read a private key or install dependencies.
