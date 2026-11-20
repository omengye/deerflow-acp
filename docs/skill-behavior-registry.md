# Skill publication and historical behavior suites

DeerFlow publishes skill creations and updates automatically after security review of the complete package. The native runtime uses the primary model and requires no fixture registry, prior uses, sandbox execution or publication phrase. The worker records the exact package and base version it reviewed. A safety rejection or changed destination prevents installation.

The `skill-behavior-v1` and `skill-behavior-v2` formats describe comparisons produced by older hosts or by SDKs that explicitly configure a behavior evaluator. Historical comparisons and their actual usage remain available for inspection. Adding suite JSON beside the desktop configuration does not affect native publication. [SDK suite example](fixtures/skill-behavior-v2.json)

Use `skill_manage.get` to read the security result, publication receipt and verified next-turn availability for a conversation candidate. [Candidate lifecycle](../go-harness/internal/acp/skill-evolution.md)
