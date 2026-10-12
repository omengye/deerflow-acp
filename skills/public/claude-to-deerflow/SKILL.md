---
name: claude-to-deerflow
description: Use when connecting an ACP-capable client to the Eino-based DeerFlow runtime, delegating an authorized task through that client, or diagnosing a DeerFlow ACP connection and session.
---

# Connect to DeerFlow through ACP

This branch runs DeerFlow on the Eino Go harness. Use an existing ACP-capable client or configured connector to send tasks. Keep the historical skill name for installed references.

## Choose the connection

Read the client's installed agent command and the actual package path before changing configuration. Use its supported configuration format; there is no universal editor JSON schema.

- Packaged Waku uses `deerflow-acp.exe`, a stdio Bridge, with the sibling `deerflow-acpd.exe` Go daemon. Its desktop settings supply model and runtime configuration.
- An external ACP client can use the same Bridge and an explicitly chosen runtime directory. The sibling daemon is discovered automatically. On Linux omit `.exe`.
- For a direct standalone process, use `deerflow-acp-go.exe`. It serves stable ACP v1 over stdin/stdout and does not accept a desktop `--config` argument.

The Bridge defaults to stable ACP v1. Select its draft v2 option only when the actual client requires it. Keep protocol stdout free of shell banners and progress messages; read process diagnostics from stderr.

## Configure a standalone client

Use absolute executable and private data paths. Configure the child environment with `DEERFLOW_MODEL`, `DEERFLOW_MODEL_PROVIDER` (`openai` for OpenAI-compatible services or `claude`), and the credential supplied by the user's existing secure configuration. The common credential variable is `DEERFLOW_MODEL_API_KEY`. `DEERFLOW_MODEL_BASE_URL` is optional. Use `DEERFLOW_GO_DATA_DIR` for the state directory. Do not print credentials or store them in this skill.

A direct-process client command can be represented as these separate arguments:

```text
command: ABSOLUTE_PACKAGE_PATH/deerflow-acp-go.exe
args: --data-dir ABSOLUTE_PRIVATE_STATE_PATH --provider openai --model CONFIGURED_MODEL_ID
```

A Bridge client command can be represented as:

```text
command: ABSOLUTE_PACKAGE_PATH/deerflow-acp.exe
args: --runtime-dir ABSOLUTE_PRIVATE_RUNTIME_PATH
```

Use the same runtime directory for the Bridge's `--status`, `--start-daemon`, and `--stop-daemon` operations. Stopping a shared daemon interrupts its connected sessions; diagnose through status before taking an action with that effect. Keep the desktop's existing process ownership and paths when diagnosing Waku.

## Run an authorized task

1. Have the ACP client perform `initialize` and inspect the negotiated capabilities.
2. Create a session with `session/new`, using the user's actual workspace as `cwd`. Pass only host-authorized client MCP server configurations. Preserve the returned `sessionId`.
3. Send the requested text through `session/prompt`. Process `session/update` events for text, plans, tools and usage; answer permission requests through the client's normal controls.
4. Report the final prompt result and inspect the requested artifacts. A successful connection alone does not prove the delegated task succeeded.
5. For a follow-up, reuse the session. After reconnecting, use negotiated session load/resume support and the same workspace. Do not treat replayed history as a new execution.
6. To cancel, send the `session/cancel` notification through the client and wait for the resulting status. Do not automatically repeat writes after a connection failure.

Use model/configuration choices returned by the active session. Skill review, memory management and host settings belong to the capabilities actually exposed by the connected client; their availability must be checked. This skill does not grant access to a daemon's authenticated management methods.

## Diagnose failures

- A missing binary or invalid model is a startup/configuration problem. Check the actual package files, configured child environment and stderr without exposing secrets.
- A daemon status response and an ACP initialization response are different checks. Verify both when the client uses the Bridge.
- A normal tool error should be reported to the agent as a tool result. Follow the session's actual stop reason before deciding that the session itself failed.
- For `reconciliation_required`, inspect the supplied receipt-list method, verify the external outcome, and explicitly reconcile through an authorized client. Do not fabricate success or bypass the requirement by creating a fresh session and replaying the action.
- If the current environment has no ACP client or connector, prepare the command/environment configuration and report that interactive execution remains unverified. Do not invent a health endpoint or emulate a full client with an ad hoc shell pipeline.

Completion evidence should identify the command, negotiated protocol, session/workspace, final status and inspected outputs. Include no credentials or private prompt contents beyond what the user requested.
