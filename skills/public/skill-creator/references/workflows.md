# Evaluation workflow

Use one task that exposes the current weakness, a normal task, and a nearby request that should not trigger this skill. Prefer artifacts that can be inspected without contacting a live service.

1. Save the input fixtures and expected observable outcomes before editing. Record the baseline skill content version, or mark the baseline as no skill.
2. Run baseline and candidate with the same available tools, task input and permission scope. If the host exposes subagents, delegate independent cases within its reported concurrency limits. Otherwise execute them serially. Never invent a successful test run.
3. Examine the resulting artifacts against each expectation. Check content and facts, not just headings or the presence of a phrase. Treat tool/network failures, cancellation and denied permissions separately from skill quality.
4. Save case results and artifact references. Record time, tokens and model identity only when the runtime provides them. Omit unavailable metrics.
5. Present the changed instructions and observed differences to the user. Revise only in response to demonstrated failures or requested changes. Re-run affected cases before proposing the revision.

Use fixture tools for external writes. A skill draft is not permission to send messages, deploy code, spend provider budget or replay historical external operations. Candidate scripts require an explicitly supported isolation mechanism; if unavailable, leave behavior unverified.

The local validation command checks package structure. The native candidate service supplies its own safety and quality reports. Neither automatically imports manually written evaluation JSON as trusted approval evidence.
