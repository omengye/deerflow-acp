# Gentoo skill learning recovery, 2026-10-28

This release derives from the existing `connection-step1-20261028` server image and replaces four Go binaries. It preserves Waku protocol 10, its authentication timeout fix, the existing entrypoint, the memory/runtime wrapper and production bind mounts.

The configuration enables extraction, consolidation, matching, discovery, generation and new-skill review drafts. Existing managed skills can publish automatically only when the trusted host suite and the original evidence/quality gates permit it. New skills and single-task drafts still require human approval. The bundled suite covers only `managed-evolution/searxng-web-search`; enabling its worker does not mean other skills qualify for automatic publication.

Existing learning and shared maintenance budgets are preserved. Cross-session sharing stays disabled; existing per-chat consent records are untouched. Only `skills` and `skill_evolution` YAML sections are rewritten; all other configuration bytes and semantic values survive. Credentials are never included in reports.

## Operator sequence

Run `release.py` on Gentoo from the deployment owner account. It uses `/home/gen/deerflow-deploy`, the existing Compose project and only its `deerflow` service.

1. Stage the four Linux binaries in `bin/`, and validate the pinned base image identity before building. `.dockerignore` excludes candidate configuration, backups and other files from the image context.
2. Run `python3 release.py prepare`. This writes the candidate configuration, manifest and aggregate preflight report. It does not change production configuration.
3. Build the image and run the Linux acceptance tests. `linux-tests.json` must have `passed: true`, the tested `image_id`, and the exact four-file `binaries` SHA-256 mapping.
4. Run `python3 release.py stage`. The original entrypoint starts against an empty isolated profile, a synthetic model endpoint and `--network none`. It uses exactly two mounts: isolated stage data and a read-only copy of the verified production runtime wrapper. The copy preserves the production wrapper's explicit DB location; the older image wrapper is not substituted for it. No production mount sources, channels, credentials or business inputs enter this container. This verifies startup, binary identities, worker state and the diagnostics operation contract; Linux tests provide the deterministic automatic-flow coverage.
5. Run `python3 release.py apply`. It requires unchanged production image/configuration, valid acceptance evidence and no active frontend or maintenance work. It drains the daemon, backs up configuration/environment/Compose/wrapper, stops only DeerFlow with a 90-second timeout, takes a SQLite backup and skills archive, applies the pinned new image and verifies the running service.
6. Run `python3 release.py verify` for another read-only aggregate health/worker/diagnostics check.

The actual production DB is `data/user-data/config/go-harness-state/harness.db`. The unrelated older `data/user-data/data/go-harness/harness.db` is never used. Historical `needs_reconciliation` runs are preserved and are not treated as active work.

## Recovery

`python3 release.py rollback` restores the original image/configuration selection, then starts only DeerFlow. It never restores the old business DB or skills snapshot over newer writes. Retain `manifest.json` and `backup/` as durable recovery material; rollback does not depend on temporary build/test artifacts. Queued/retry maintenance jobs survive the restart. Running/reserved calls and active foreground work must settle first.

If verification fails, automatic rollback checks live work before stopping the candidate. Busy work or unavailable management yields `rollback_deferred`, leaving the service for operator recovery. A stopped candidate can be rolled back without a management connection. `recovery-attention.json` records an unconfirmed resume rather than hiding the initial failure.

The image supplies a 60-second native daemon stop timeout. This release uses `docker compose stop -t 90`; the existing Compose file is intentionally preserved. Future manual maintenance should also use `-t 90`, since its existing 45-second grace period is shorter.

## Local verification

`python -m unittest discover -s deploy/docker/skill-learning-recovery-20261028 -p test_release.py -v` requires PyYAML. The offline tests cover configuration preservation, consent/budget boundaries, private-data exclusion from staging, image/test identity binding, diagnostics error envelopes, the actual DB path, active-work recovery checks and rollback without business-data restoration.

## Deployed result

Gentoo was updated on 2026-10-28 to `deerflow-server:skill-learning-recovery-20261028`, pinned in `.env` to `sha256:535bfcc89aebc94561e4d8a31fd6624f00c4aaddc10394a49df875f7ac6198ce`. The running native build ID is `skill-learning-recovery-20261028`. The service is healthy and all intended workers are enabled; learning idle time remains 300 seconds. Its shared daily budget remains 30 model calls / 100,000 tokens, including the learning sublimit of 10 calls / 50,000 tokens.

The eight Linux test groups passed (463 test results including nested cases), as did 13 deployment-script tests and the isolated image startup. The Linux suites include deterministic automatic extraction, consolidation, matching, generation, review and guarded publication, including historical unfinished-task isolation. Internal and public Waku protocol 10 connections passed, including a 10-second delayed authentication test.

After startup, the real discovery worker automatically generated three candidates from existing signals: one completed and awaits review; two passed security review but failed quality review (`quality_rejected`). No candidate was automatically published. The generic job-level `invalid_candidate` for those two failures is disambiguated by the saved proposal validation code. Extraction is enabled but has no newly captured production inputs yet; this is distinct from the successful production discovery/generation check. No existing installed skill currently satisfies the automatic-publication eligibility checks, so the status correctly reports `no_eligible_targets` and `some_targets_require_review`.

All 66 sessions, 135 run records (including seven `needs_reconciliation` records), model credentials, web proxies, unrelated configuration and other containers were preserved. The new learning source and trusted suite are provisioned by the configuration initializer. No cross-session sharing or per-chat consent override was added. Backups are retained at `/home/gen/deerflow-deploy/skill-learning-recovery-20261028/backup`; the old image remains available. Reports in `reports/` contain metadata only.

Build inputs are frozen in `dist/gentoo-skill-learning-20261028/source.tar.gz` and `source-manifest.json`, including uncommitted source files. The source snapshot also includes the current model-error handling, skill catalog filtering and configuration-read fixes; it is not represented solely by Git HEAD. The four Linux binaries were built with Go 1.26.8 and CGO disabled.

Pre-existing observation outside this rollout: the configuration references `memory-embedding:8080`, but no embedding container was present during the baseline audit. This rollout preserved that configuration and did not create or remove that service.
