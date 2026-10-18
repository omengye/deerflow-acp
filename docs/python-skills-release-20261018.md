# Python skills release verification - 2026-10-18

## Incident and resolution

The original Huawei Cloud proposal `p_4BXVG6KN5TJS6E7JP3D5YSPMQM` failed
at security review with `review_response_invalid`, state version 3. This means
the host did not receive an acceptable bound review response. It does not prove
a Python prohibition, a content rejection, or a provider outage. The historical
response was not retained, so its exact formatting defect cannot be recovered.

The implementation now accepts reviewable Python sources, requires explicit
operator acknowledgment for unverified scripts, executes session-pinned source
through existing command policy, and exposes bounded review recovery and safe
failure diagnostics. See [the support contract](skill-python-support-20261018.md).

## Test and build evidence

- `go test -p=2 -timeout 30m -json ./...`: exit 0; 41 test packages and
  3,419 test/subtest events passed; 20 tests skipped; zero failed events.
- Skips cover unavailable Windows symlink privileges, optional WSL/Docker,
  Poppler, live web/OpenCLI and explicitly configured built-Bridge tests.
- The four built-Bridge tests were subsequently enabled against this Release
  package and all passed, including the three daemon-discovery variants.
  Two old assertions assumed a `deerflow-go` build-label prefix; they now bind
  status output to the actual native endpoint identity. Credential non-disclosure
  and authenticated lifecycle checks remain enforced. Only these test files
  changed after the program build.
- `cargo test --locked --offline -p waku deerflow_settings`: 158 passed.
  No visual UI test was performed.
- Package-input and full-tree updater PowerShell fixtures passed.
- Huawei client: ten local mock tests and native four-file package validation
  passed. Cross-volume `.skill` packaging (D: input, C: output) passed.
- Release desktop and nine Windows EXEs rebuilt successfully. A Linux WSL helper
  is included. No Python runtime was bundled or installed.
- Two complete real-package smoke runs passed, each with 92 check events and no
  cleanup errors. One has no Python on PATH; the other explicitly selects the
  existing local interpreter. They exercise Waku WebSocket/ACP/Go transport,
  configuration apply/restart, session restoration, tool approvals and Python
  readiness/pinned execution/denial/quoted argv. Model endpoints are local mocks.
- `git diff --check` passed.

The build is deliberately recorded as `development-dirty`; no commit or release
ZIP was created. The executable build ID is `deerflow-skills-20261018-015157-569`.
The source hash manifest and test logs are under `.build-cache`.

## Replacement and preserved state

Fresh package:
`D:\Tools\deerflow-api\dist\desktop\DeerFlow-Desktop-Go-PythonSkills-20261018-0152`

Replaced existing installation:
`D:\Tools\deerflow-api\dist\desktop\DeerFlow-Desktop-Go-20261010-103944`

Full program/profile backup:
`D:\Tools\deerflow-api\.build-cache\python-skills-deploy-20261018-020455`

The host was idle and drained before replacement. All nine installed executable
hashes match the fresh package. The updater verified all 370 backed-up profile
files before the reviewed creator-tree migration. The existing profile's
`config.yaml` hash is unchanged. The updated `skill-creator` is enabled at version
`3e580592e9644b48cd6e73296f3dea38883b7eec1c0a230b09735da35efdfdbf`;
`skill.verify` confirms it is readable and available for an existing session's
next turn, with no claim of script execution.

The restarted daemon reports the new build and a different instance identity.
SQLite read-only `PRAGMA quick_check` returns `ok`; session management lists 114
retained sessions. WeChat remains enabled, authenticated and bound. Its bundled
executable pin was updated through the host to registration revision 7 without
replacing channel credentials or options.

The three-file Huawei package previously written directly into `custom` was
hash-checked and moved, not deleted, to:
`user-data\backups\huaweicloud-unreviewed-20261018-020455`

It is outside configured skill sources, preventing unintended automatic import
on restart. Its contents are also preserved in the full profile backup. No
database rows were edited and no candidate was directly installed.

## Huawei review result and remaining action

A new complete corrected draft was submitted through authenticated
`proposal.submit`, preserving the original managed source, absent base and task
requirements. It does not inherit the failed proposal's identity or approval.

- New proposal: `p_IZF6BALYBSZJX6YBUUHTXPYYJW`.
- Candidate: `b04ca410da1158bf1476225803aeecb27a0b3ddf2159c52dd880754d7bf92d8c`.
- State: `pending_review`, state version 2.
- Real configured model reviews: security passed; quality passed.
- Script evidence: `behavior=not_run`, `isolated_scripts=false`,
  `script_review_required=true`.
- Original proposal remains `failed`, state version 3, with its original hash.

The new candidate is NOT yet published or available as a runtime skill. The
operator must inspect it and explicitly confirm the unverified-script risk in
the desktop's publication action. This implementation authorization was not
treated as that separate risk acknowledgment. No SSH connection, cloud command,
private-key read, dependency installation or real remote isolation test occurred.
