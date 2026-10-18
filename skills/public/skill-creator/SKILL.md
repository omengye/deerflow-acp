---
name: skill-creator
description: Use when creating a reusable DeerFlow skill, correcting an existing skill, or preparing a skill candidate for review.
license: Complete terms in LICENSE.txt
---

# Skill Creator

Write focused instructions and test them against the actual task. A skill contains UTF-8 `SKILL.md` with `name` and `description` frontmatter, plus only its necessary support files. Use UTF-8 without BOM and lowercase letters, digits and single hyphens for names, at most 64 characters; the directory name must match.

Python source and text dependency declarations are supported skill resources. Python is an optional execution dependency, not a dependency of the Go host. Content review, publication, availability, runtime readiness and script execution are separate facts.

## Create or improve

1. Identify the requested workflow and a representative failing or missing case. Preserve the user's scope, implementation choice and permissions. Inspect the current host tool schema before using host-specific features.
2. For an existing skill, read the session-selected version. Keep its source and base version attached to the change; retain unrelated support files.
3. State when the skill applies, its inputs, decisions, outputs and verification steps. Keep references package-relative. For Python resources, read [Python runtime guidance](references/python-runtime.md).
4. Exercise representative cases using [the evaluation workflow](references/workflows.md). Include a negative trigger and incomplete input when relevant. Record observed outcomes, not invented test success.
5. Before submission, retry or publication, read [the managed lifecycle](references/managed-lifecycle.md). Submit the complete package and report the host's actual state.

## Managed DeerFlow skills

`skill_manage` proposes changes; it does not authorize publication. Use host-provided destinations and the returned proposal ID. For multi-file creation, submit the complete `files` map, including `SKILL.md`, scripts and dependencies. Keep drafts outside configured skill roots. Never substitute a live directory write, database edit or source switch for failed review.

After valid safety and quality review, script-bearing packages can enter operator review without execution evidence. Mark unexecuted scripts as `not_run`; do not fabricate isolated execution. They require manual approval, never automatic publication. Publication grants no additional execution permission. Any candidate file change invalidates the old approval.

| Claim | Required evidence |
| --- | --- |
| Published | Host `published=true` for the approved package |
| Available next turn | `availability_verified=true` or `skill.verify` with `available=true` for the intended session/version |
| Runtime ready | Host runtime check for that snapshot/environment |
| Script succeeded | Actual authorized execution result, including exit status |

Availability checks load instructions; they do not execute scripts. A local echo, package validation or `ok` response proves neither publication nor runtime success.

## Local draft tooling

Use the actual installed `deerflow-skill-go.exe` path on Windows, or `deerflow-skill-go` on Linux. These commands do not change the live registry:

```text
deerflow-skill-go init --path EXISTING_PARENT --name report-review --description "Use when reviewing a report against its cited evidence."
deerflow-skill-go validate --path EXISTING_PARENT/report-review
deerflow-skill-go package --path EXISTING_PARENT/report-review --output OUTPUT_DIRECTORY/report-review.skill
```

Complete the scaffold first. Validation checks frontmatter, paths, references and candidate limits: 20 files, 128 KiB per file, 512 KiB total. Keep credentials, interpreter installations, caches and binaries outside the package. `validate --mode installed` checks installation structure only. Neither mode runs scripts, installs dependencies or approves publication.

Packaging writes a new ZIP-format `.skill` outside the source directory from the validated snapshot and refuses overwrite. `behavior: not_run` is intentional. Use UTF-8-no-BOM writers, such as PowerShell `[System.Text.UTF8Encoding]::new($false)`.

For evaluation case and result formats, read [the schemas](references/schemas.md). For output examples, read [output patterns](references/output-patterns.md).
