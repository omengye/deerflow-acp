# Skill security review and publication

The native host uses a single, tool-free review on the primary model. The input contains the complete candidate package and its base. The reviewer checks instructions, scripts, credentials, permissions and external effects. The host binds the response to the exact candidate, base and validator identity. Missing, malformed or negative security decisions prevent installation.

The generation worker publishes a passing create or update in the same job. A saved successful review survives an interrupted installation. Source grants, origin bindings, current head and package digests are checked again before publication. Discovered skills become available immediately within their source workspace. Shared source files keep their existing contents for other workspaces.

Discovery can use past tasks to decide which candidate to generate. Once submitted, the native worker requests only security review. It does not create behavioral cases, invoke a quality model, execute scripts, sample future tasks or wait for a confirmation message.

## Historical records and SDKs

Previously recorded comparisons, isolated executions and observation decisions remain inspectable with their original identities. On upgrade, eligible unfinished generation jobs retain their package and get a fresh security review. Sampling ends for previously published trials. A version with a passing review and an active source becomes stable in its originally authorized workspace. Superseded versions and quarantined routes keep their restrictions. Standalone SDK hosts can explicitly configure fixture evaluation of their own drafts.

See [the model tool and publication contract](../../acp/skill-evolution.md).
