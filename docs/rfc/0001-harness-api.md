> 历史记录：本文描述旧 Python 实现或当时的桌面设计，命令和能力声明不作为当前分支的使用说明。原文与源码参见[固定历史提交](https://github.com/omengye/deerflow-acp/tree/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9)。当前 Eino Go 入口与验收范围见[项目说明](../../README.md)。

# RFC 0001 — Harness Public API (v0.1)

| Field | Value |
|---|---|
| Status | Draft |
| Author | Sisyphus |
| Created | 2026-05-05 |
| Targets | `deerflow_sdk` 0.1.0 |
| Supersedes | — |

## 1. Motivation

The current project (`deerflow-api`) is a FastAPI service wrapping an
embedded `deerflow/` harness. It is **not** a framework that a third
party can depend on:

- No public Python SDK (`deerflow/__init__.py` is empty).
- LangGraph types leak through (`StreamEvent.type` is the literal
  `"values" | "messages-tuple" | "custom" | "end"` from langgraph).
- Module-level globals (settings singleton, `_client_manager`, three
  `ThreadPoolExecutor`s, the IPv4 monkey-patch in `app/config.py`)
  prevent two harnesses from coexisting in one process.
- Tools register via yaml entry-point strings; middlewares are
  hardcoded; sub-agents use yaml + a builtins directory. Three
  inconsistent extension mechanisms.
- No `__all__`, no `py.typed`, no SemVer policy, no CHANGELOG.

In short, `deerflow-api` works as a **product** but cannot be consumed
as a **library**. v0.1 of `deerflow_sdk` fixes that.

## 2. Goals

1. A user can build a working agent in **≤ 30 lines of Python** using
   only public symbols from `deerflow_sdk`.
2. Two `Harness` instances in the same process are **fully independent**
   (different models, different sandboxes, different running tasks, no
   crosstalk).
3. The public API has **zero langchain / langgraph imports**. LangGraph
   remains the engine — it is hidden behind `deerflow_sdk._engine`.
4. The legacy `config.yaml` and `DeerFlowClient` continue to work via a
   `deerflow_legacy` shim. Existing tests pass without modification.
5. The contract is enforced by tests: `tests/sdk/test_contract.py`
   fails immediately if a public symbol disappears or a langgraph type
   leaks into the public module.

## 3. Non-Goals (v0.1)

- Multiple engine backends (LangGraph is the only engine). The internal
  layout leaves room for an engine Protocol in v0.2; we do not commit
  to it now.
- Built-in web UI / Studio.
- Distributed checkpointer. v0.1 ships in-memory + sqlite single-host.
- Redesign of guardrails / MCP. Continue using the existing
  `deerflow.guardrails` and `deerflow.mcp` modules underneath.
- Auto schema inference for arbitrary generic types. v0.1 supports
  primitives + pydantic models + literals + unions of those.

## 4. Public API Surface

The complete public surface is everything in `deerflow_sdk.__all__`:

```text
Harness, HarnessConfig, ModelConfig
tool, Tool, ToolContext
subagent, SubagentSpec
Hook, HookContext
Permission, PermissionDecision, PermissionContext
Sandbox, LocalSandbox
StreamEvent, TextDelta, ToolCall, ToolResult,
  SubagentStart, SubagentEnd, RunComplete
DeerFlowError, ToolError, PermissionDenied, SandboxError, ModelError
```

### 4.1 `Harness`

The single entry point.

```python
Harness(
    *,
    model: str | ModelConfig,
    tools: list[Tool] = [],
    subagents: list[SubagentSpec] = [],
    hooks: list[Hook] = [],
    permissions: list[Permission] = [],
    sandbox: Sandbox | None = None,
    system_prompt: str | None = None,
    max_iterations: int = 50,
    config: HarnessConfig | None = None,    # alternative
)

await harness.run(prompt, *, thread_id=None, output_type=None) -> T | str
async for event in harness.stream(prompt, *, thread_id=None): ...
await harness.aclose()
async with harness: ...
```

All state lives on the instance. `Harness.__init__` performs zero I/O
and zero global registration.

### 4.2 `@tool`

```python
@tool
def name(arg: T) -> R: ...
```

Schema is inferred from the signature. Description is taken from the
docstring (raises `ValueError` if neither docstring nor `description=`
is provided). For full control, subclass the `Tool` Protocol.

### 4.3 `@subagent`

Class decorator that returns a `SubagentSpec`. The harness compiles each
spec into a `dispatch_<name>` tool exposed to the lead agent.

### 4.4 `Hook`

Subclass and override any subset of `on_run_start`, `on_user_prompt`,
`pre_tool_use`, `post_tool_use`, `on_run_end`. Hooks **observe**;
they do not gate. To gate, use `Permission`.

### 4.5 `Permission`

Returns `ALLOW`, `DENY`, or `ASK_USER`. Permissions chain with AND
semantics. The first non-`ALLOW` wins. `ASK_USER` falls back to `DENY`
if no `ask_user` callable is configured (secure default).

### 4.6 `Sandbox` / `LocalSandbox`

`Sandbox` is a Protocol. `LocalSandbox` ships in v0.1 with the critical
property:

> `LocalSandbox.execute()` raises `SandboxError` unless
> `allow_host_bash=True` was passed at construction time. The check
> happens **inside** `execute()`, not in some upstream tool registration
> layer. Direct calls cannot bypass the gate.

`DockerSandbox` is planned for v0.2. The Protocol is stable now so
third parties can ship their own.

### 4.7 `StreamEvent`

Framework-owned. The `type` field is a stable string discriminator:

| Class | `type` value |
|---|---|
| `TextDelta` | `"text_delta"` |
| `ToolCall` | `"tool_call"` |
| `ToolResult` | `"tool_result"` |
| `SubagentStart` | `"subagent_start"` |
| `SubagentEnd` | `"subagent_end"` |
| `RunComplete` | `"run_complete"` |

Adding new subclasses is backwards-compatible. Renaming or removing
fields is a breaking change.

## 5. Internal Layout

```
packages/deerflow_sdk/
├── __init__.py        # public API only
├── py.typed
├── _harness.py
├── _tools.py
├── _subagents.py
├── _hooks.py
├── _permissions.py
├── _sandbox/
│   ├── base.py
│   └── local.py
├── _events.py
├── _config.py
├── _errors.py
└── _engine/           # private — LangGraph adapter lives here
    └── (added in Phase 2)
```

Private modules use a leading underscore. Anything reachable only via
a private module may change between patch releases.

## 6. Compatibility Strategy

- The existing `deerflow/` package is **untouched** in v0.1. A future
  `deerflow_legacy/` shim will re-export its `DeerFlowClient` as a thin
  wrapper around `Harness`, with a `DeprecationWarning`.
- The existing `app/` FastAPI service continues to import from the old
  `deerflow/` package. It will be migrated to use `deerflow_sdk` in
  Phase 4 once the engine is functional.
- `config.yaml` keeps working through `deerflow_legacy.config_loader`
  in Phase 4.

## 7. Versioning & Stability

- `deerflow_sdk` follows SemVer.
- `0.1.x` patches: bug fixes only, no API changes.
- `0.x.0` minors: additive only. New public symbols, new event
  subclasses, new `Hook` callbacks (default no-op).
- `1.0.0`: stable major. Breaking changes require a major bump and
  one minor of `DeprecationWarning`.
- The `tests/sdk/test_contract.py` suite enforces the public surface.

## 8. Risks & Open Questions

- **Engine event coverage.** LangGraph's event stream may not map
  cleanly onto the discriminator types we chose. Mitigation: the engine
  adapter (`_engine/event_adapter.py`) buffers and translates; we
  prefer dropping low-value LG events over leaking the LG type system.
- **Sub-agent dispatch in LangGraph.** Today the project uses
  `subagents/executor.py` with module-level thread pools and a global
  task dict. Phase 3 must reimplement dispatch as instance state on the
  engine. If this is more invasive than expected, sub-agents may slip
  to v0.2.
- **`output_type=`.** Requires constrained-decoding support across the
  three providers we ship (DashScope, OpenAI, Anthropic). Each provider
  expresses this differently. Phase 2 will validate feasibility before
  promising this in v0.1.0; if a provider does not support it, the
  feature degrades to "ask the model in the prompt + parse" with a
  warning.

## 9. Phase Plan

| Phase | Weeks | Deliverable | Verification |
|---|---|---|---|
| 1 | 1–2 | Public API stubs, contract tests, RFC | `pytest tests/sdk` green; mypy strict on `packages/deerflow_sdk` |
| 2 | 3–4 | LangGraph engine — `run()`, `stream()`, multi-instance | `examples/01_hello.py` and `05_multi_instance.py` actually run |
| 3 | 5 | Sub-agents, permissions, sandbox enforcement | `examples/02–04` run; sandbox security tests pass |
| 4 | 6 | Legacy shim — `deerflow_legacy.client.DeerFlowClient`, yaml loader, AG-UI extracted from `app/routers/chat.py` | All existing tests pass; old `config.yaml` still works |
| 5 | 7 | 80%+ coverage, mkdocs site, CHANGELOG | CI gates on coverage |
| 6 | 8 | PyPI release, migration guide | `pip install deerflow_sdk==0.1.0` succeeds |

## 10. Decision Required

Approve this RFC to proceed with Phase 2 (engine implementation).
