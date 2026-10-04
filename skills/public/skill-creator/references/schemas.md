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
