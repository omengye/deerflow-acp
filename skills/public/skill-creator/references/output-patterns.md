# Output patterns

Choose only the structure needed by the task. A technical review might contain a short conclusion, actionable findings linked to evidence, and verification results. A data transformation might require a specific JSON schema and an accompanying source manifest.

When exact shape matters, give one complete example and define how missing values are represented. When the task is open-ended, explain the decision criteria instead of imposing a fixed number of sections.

Keep facts, assumptions and unknowns distinguishable. List the file or proposal identifier actually produced. A draft, a structurally valid package, a reviewed candidate and a published skill are different states; report the state that the tools returned.

For a Python skill, report publication, instruction availability, runtime readiness and script execution independently. Example when those receipts actually exist: "Published version `<returned-version>`; available to the intended session. Python is not ready because `paramiko` is missing. Script execution: not run. No dependency installation was attempted." Missing dependencies do not revoke a confirmed publication, and a passing echo test cannot replace publication receipts.
