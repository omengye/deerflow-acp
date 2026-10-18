# Evaluation records

Keep fixtures and records in a separate evaluation workspace so the deployable skill contains only necessary instructions and resources.

## Cases

```json
{
  "skill_name": "report-review",
  "cases": [
    {
      "id": "citation-check",
      "input": "Check whether the report's conclusions follow from the supplied sources.",
      "fixtures": ["report.md", "sources.md"],
      "expectations": ["Flags the unsupported total and cites the contradicting source."]
    }
  ]
}
```

## Results

```json
{
  "case_id": "citation-check",
  "variant": "candidate",
  "skill_version": "record-the-observed-content-hash",
  "status": "completed",
  "expectations": [
    {"passed": true, "evidence": "review.md identifies the total and links source section 2."}
  ],
  "artifacts": ["review.md"]
}
```

Use `not_run`, `completed`, `failed`, `cancelled` or `blocked` for status. `failed` describes the test execution; give separate expectation evidence before attributing a failure to the skill. Store short evidence and fixture references, not full conversations, credentials or tool payloads. Human-authored result files are review aids, not trusted runtime receipts.

## Candidate review failures

Record the proposal ID, source, candidate hash, submission reason, status, security, quality, behavior, error code and failed check reasons separately. Preserve uncertainty when a field is unavailable. An empty field or a passing security check does not establish a validator outage. For a source comparison, hold the complete candidate, base, submission reason and review configuration constant; identical candidate hashes alone do not identify identical model inputs. Even identical inputs may yield different model judgments.

Record script execution evidence independently from content review and fixture behavior. An unexecuted script remains `not_run`; a real failed test remains failed. Record publication and availability receipts, then runtime readiness and execution results when supplied by the host. Do not invent missing JSON fields, environment checks or isolation receipts. These local evaluation records cannot satisfy a host approval gate.
