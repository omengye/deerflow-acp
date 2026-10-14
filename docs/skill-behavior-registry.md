# Trusted Skill Behavior Registry

The desktop `skill_evolution.behavior.suite_path` accepts either an existing
single `skill-behavior-v1` / `skill-behavior-v2` suite or a registry document:

```json
{
  "version": "skill-behavior-registry-v1",
  "suites": [
    {
      "version": "skill-behavior-v1",
      "id": "example-target-regression-v1",
      "source_id": "managed-skills",
      "name": "example-skill",
      "cases": []
    }
  ]
}
```

This is a shape illustration, **not a runnable or valid assessment**: replace
`cases` with host-reviewed targeted and regression cases for the real target.
See the existing `fixtures/skill-behavior-v2.json` fixture for
the full v2 schema including positive and negative trigger selection checks.
That fixture is test data, not coverage for an arbitrary installed skill.

## Bounds And Trust

- One regular file, at most 1 MiB total, with 1 through 32 embedded suites.
- Each source must already be authorized for generation; the target is the
  source/name pair. Duplicate targets and duplicate JSON keys are rejected.
- Unknown registry fields, nested path lists and generated trust references
  are not accepted. Each suite is validated by the existing strict parser.
- Save validation and startup use the same loader. Startup retains an immutable
  suite snapshot; changing the file requires the ordinary save/apply flow.
- Candidates cannot install their own assertions or expected answers as trusted
  coverage. A registry only wires independently maintained host assessments.

## Automatic Adoption

Existing managed, enabled, non-builtin SKILL.md patches need passing security
and behavior reports, real targeted improvement, no regressions, current head
and source checks, and the original bounded publication budget. Description
changes require trusted v2 positive/negative selection comparisons. New skills,
support-file changes and other metadata changes still require review.

The host's `skill.auto_patch.status.readiness` reports installed and eligible
targets, missing suites, and pending/attention/waiting counts. It is a capability
projection, never permission to publish. A partially covered catalog stays
`partial`; do not label an enabled switch as full coverage.

`proposal.list` optionally accepts `needs_attention: true` together with
`status: "pending_review"`. False selects background checks/waits instead;
omitting it returns both. Filtered cursors bind the current host policy and
source identities. Each pending row contains a read-only `disposition` with
`needs_attention` and a reason code. Stored decision hints are fenced by proposal
state version and policy identity; publication always revalidates live guards.

Turning off new automatic publication retains non-paid observation of already
adopted versions while the service is running. Quality votes still require the
existing operator-confirmed evidence; this implementation does not introduce
host-evaluated post-publication tests or new-skill runtime isolation.

## Desktop First Run

New desktop profiles use Simplified Chinese, automatic maintenance, inherited
default-model routes and the selected maintenance limits: 1000 calls / 6000000
tokens daily, 600 seconds per job and 5 workers. Learning shares those limits
with a 50-call / 1000000-token sublimit. Publication remains limited to 40 changed
lines and 2 releases per day. These are ceilings, not startup charges.

The Go configuration service embeds the independently prepared two-case SearXNG
suite in `internal/desktopconfig/defaults/searxng-web-search.json`. When the
portable profile selects `./skill-behavior/searxng-web-search.json`, initialization
creates that missing host file without network or model calls. Existing suites,
other suite paths and existing user configuration are not replaced. Candidate
skills cannot provision a different assessment through this mechanism.

This bundle covers only `managed-evolution/searxng-web-search`; it does not
install the user's skill content or search credentials, and it does not grant
coverage to arbitrary new skills. A fresh profile may have no eligible target
and report partial automation while normal chat, learning and generation remain
available after model-only setup. New skills and uncovered targets still need
review. Missing search credentials and optional browser dependencies do not
block basic model setup. Changing defaults does not rewrite existing profiles.
