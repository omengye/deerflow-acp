# Managed skill lifecycle

## Submit the package

Use `skill_manage` with the host-provided destination. For creation, supply the complete package, including `SKILL.md` and required support files. For updates, start from the session-selected version and preserve unrelated files. Keep local drafts outside installed skill roots.

The host records the exact candidate, base version and authorized source. Its background worker reviews the entire package for safety, including scripts. An affirmative review automatically publishes that version. An ordinary creation or update request is sufficient. Use `get` with the returned `proposal_id` to check progress.

| Status | What to report |
| --- | --- |
| `evaluating` | Security review is queued, running or retrying. |
| `awaiting_publication`, `publishing` | The safety check passed and the host is installing the reviewed version. |
| `published` | Installation completed. Check `availability_verified` for the requesting session. |
| `rejected` | Inspect the saved security verdict, cancellation or failure before revising the package. |
| `superseded` | Read the current version before preparing an update. |

## Review and recovery

The reviewer checks for credential exposure, unsafe instructions, permission changes and unexpected effects. It reads scripts as text. Runtime checks, dependency preparation and script execution follow their own permissions when a task needs them.

Publication requires a valid package, an authorized destination and the same installed base that was reviewed. The worker retries transient failures within its maintenance limits. If installation is interrupted after admission, the host can finish the original journal using the existing review and candidate digest.

## Report availability

Use `published=true` for a completed publication. Use `availability_verified=true` or a successful `skill.verify` for the exact version to report next-turn availability. Running tasks and interrupted resumes keep their pinned versions. Report script success only from an actual authorized execution result.
