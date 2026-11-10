# Trusted skill behavior registry

Skill evolution is automatic and inherits the primary model. Customers do not
configure a skill-evolution block, review mode, budget, daily call limit or
suite path. One-time migration removes old fields and retires old unfinished
work while preserving skills, checkpoints and audit history.

The harness automatically derives host-checked examples from fresh tasks with
explicit exact-output or exact-artifact requirements. Evidence without a
trustworthy oracle waits for new evidence. A candidate needs independent
support, held-out verification, demonstrated improvement, no critical
regression and appropriate trigger/script coverage before automatic adoption.

These evidence requirements apply to automatic adoption. An explicit native
user instruction to publish authorizes `skill_manage.publish` for that user's
exact candidate without those gates. The host binds session/run/input digest,
source, base, candidate and state version, records `user_confirmation` in the
approval, cancels pending validation and uses the normal file publication CAS.
Results distinguish `mode=user_confirmed` and `evidence_gate_bypassed=true`
from test success. Published files are available to ordinary subsequent tasks;
there is no observation wait. Source authority, structural checks and version
conflicts still apply, and script/external execution permissions do not change.

Conversation-submitted updates also bind their native source run, root,
session, workspace and input digest. They do not require a discovery record.
For an existing skill the host can use completed native reads of that exact
installed baseline and user-supplied exact-output/artifact oracles from the
originating session and workspace. Two independent task roots provide support;
a separate task started after submission provides the holdout. Subsequent tasks
can supply all three, so a candidate with no prior evidence can still progress.
The submission task, other conversations, different skill versions, retired
roots and revoked contribution epochs cannot qualify. Task text and expected
answers are not included in diagnostic responses.

The `evidence` object in `skill_manage` results and `proposal.behavior` reports
the source binding, suite readiness, supporting and holdout counts, concrete
`missing` items and `next_action`. A new skill without a baseline, or an SDK
candidate without a native source task, still needs discovery-based matching or
a trusted host suite. Natural-language self-reviews cannot supply an oracle.
These are explicit validation limitations, not a recommendation to resubmit.

Maintainers can add trusted regression suites as regular JSON files in the
`skill-behavior` directory next to the active config. These are optional host
assets, never assertions uploaded by a candidate. Each file contains one
`skill-behavior-v1` or `skill-behavior-v2` suite; see
[the v2 fixture](fixtures/skill-behavior-v2.json) for the contract. A fixture's
assertions only cover its own task and do not validate arbitrary skills.

Files load in pages of 32 with a 1 MiB per-file bound. All cases are stored in
immutable manifests and executed in bounded slices with durable progress.
There is no 32-suite or 16-case lifetime limit. Source/name targets and case IDs
must be unique; duplicate keys, unknown fields, unsafe paths and executable
grading rules are rejected. Reloading assets changes verification identity.

The host records actual, estimated and unknown usage separately. Three total
attempts bound transient failures; completed comparisons survive restart. No
local daily quota participates in admission. Missing evidence and missing OCI
capability remain visible waiting conditions for automatic adoption. New
evidence or a changed environment can resume eligible work; explicit user
confirmation can instead publish the existing candidate.

When a matching host suite is added and loaded on restart, an existing waiting
candidate gets a new behavior evaluation with its old binding and results
archived. Its package, proposal ID and unchanged content review are retained.
An obsolete generation job cannot close the replacement evaluation. Probes
without a dependency change do not reset attempts or consume model calls;
retired candidates are not resumed.

The desktop shows automatic validation, observation, stability, withdrawal and
verification freshness. Adopted candidates are scoped to their source workspace
and sampled into new roots; other tasks retain the stable baseline. Existing
runs retain their pinned version. Five independent successes and critical
coverage are required within 72 hours; unresolved regressions and inconclusive
expiry trigger withdrawal. A source correction affects dependent verdicts.

See [the final implementation plan](skill-evolution-final-plan-20261109.md)
and [the evaluator contract](../go-harness/internal/skills/evolution/behavior.md).
