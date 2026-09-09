"""Task tool for delegating work to subagents."""

import asyncio
import concurrent.futures
import logging
import uuid
from collections.abc import Callable, Coroutine
from copy import deepcopy
from dataclasses import replace
from functools import wraps
from pathlib import Path
from typing import Annotated, Any

from langchain.tools import InjectedToolCallId, ToolRuntime
from langchain_core.tools import StructuredTool
from pydantic import BaseModel, Field

from deerflow.agents.thread_state import AgentContext, ThreadState
from deerflow.sandbox.security import LOCAL_BASH_SUBAGENT_DISABLED_MESSAGE, is_host_bash_allowed
from deerflow.subagents import SubagentExecutor, get_available_subagent_names, get_subagent_config
from deerflow.subagents.executor import (
    SubagentStatus,
    cleanup_background_task,
    finalize_cancelled_background_task,
    get_background_task_result,
)

logger = logging.getLogger(__name__)


class _TaskToolInput(BaseModel):
    """Input schema for the task tool."""
    description: str = Field(description="A short (3-5 word) description of the task for logging/display. ALWAYS PROVIDE THIS PARAMETER FIRST.")
    prompt: str = Field(description="The task description for the subagent. Be specific and clear about what needs to be done. ALWAYS PROVIDE THIS PARAMETER SECOND.")
    subagent_type: str = Field(description="The type of subagent to use. ALWAYS PROVIDE THIS PARAMETER THIRD.")
    max_turns: int | None = Field(default=None, description="Optional maximum number of agent turns. Defaults to subagent's configured max.")
    acceptance_criteria: list[str] | None = Field(
        default=None,
        description=(
            "Optional bounded checklist. Deterministic forms are: "
            "file:<path> exists, file:<path> non-empty, "
            "file_written:<path>, and tests_passed:<exact command>."
        ),
    )


_TASK_TOOL_DESCRIPTION = """Delegate a task to a specialized subagent that runs in its own context.

Subagents help you:
- Preserve context by keeping exploration and implementation separate
- Handle complex multi-step tasks autonomously
- Execute commands or operations in isolated contexts

Built-in subagent types:
- **general-purpose**: A capable agent for complex, multi-step tasks that require
  both exploration and action. Use when the task requires complex reasoning,
  multiple dependent steps, or would benefit from isolated context.
- **bash**: Command execution specialist for running bash commands. This is only
  available when host bash is explicitly allowed or when using an isolated shell
  sandbox such as `AioSandboxProvider`.

Additional custom subagent types may be defined in config.yaml under
`subagents.custom_agents`. Each custom type can have its own system prompt,
tools, skills, model, and timeout configuration. If an unknown subagent_type
is provided, the error message will list all available types.

When to use this tool:
- Complex tasks requiring multiple steps or tools
- Tasks that produce verbose output
- When you want to isolate context from the main conversation
- Parallel research or exploration tasks

When NOT to use this tool:
- Simple, single-step operations (use tools directly)
- Tasks requiring user interaction or clarification
"""


def _merge_skill_allowlists(parent: list[str] | None, child: list[str] | None) -> list[str] | None:
    """Return the effective subagent skill allowlist under the parent policy."""
    if parent is None:
        return child
    if child is None:
        return list(parent)

    parent_set = set(parent)
    return [skill for skill in child if skill in parent_set]


def _snapshot_uploaded_files(state: object) -> list[dict[str, Any]] | None:
    """Validate and copy the parent upload boundary, failing closed."""
    if not isinstance(state, dict) or "uploaded_files" not in state:
        return None
    uploaded_files = state.get("uploaded_files")
    if not isinstance(uploaded_files, list):
        return None
    if not all(
        isinstance(entry, dict)
        and isinstance(entry.get("filename"), str)
        and bool(entry["filename"])
        and Path(entry["filename"]).name == entry["filename"]
        for entry in uploaded_files
    ):
        return None
    return deepcopy(uploaded_files)


# Core async implementation of the task tool.
async def _task_tool_impl(
    runtime: ToolRuntime[AgentContext, ThreadState],
    description: str,
    prompt: str,
    subagent_type: str,
    tool_call_id: Annotated[str, InjectedToolCallId],
    max_turns: int | None = None,
    acceptance_criteria: list[str] | None = None,
) -> str:
    available_subagent_names = get_available_subagent_names()

    # Get subagent configuration
    config = get_subagent_config(subagent_type)
    if config is None:
        available = ", ".join(available_subagent_names)
        return f"Error: Unknown subagent type '{subagent_type}'. Available: {available}"
    if subagent_type == "bash" and not is_host_bash_allowed():
        return f"Error: {LOCAL_BASH_SUBAGENT_DISABLED_MESSAGE}"

    # Build config overrides
    overrides: dict[str, Any] = {}

    # Skills are loaded by SubagentExecutor per-session (aligned with Codex's pattern:
    # each subagent loads its own skills based on config, injected as conversation items).
    # No longer appended to system_prompt here.

    if max_turns is not None:
        overrides["max_turns"] = max_turns

    # Extract parent context from runtime
    sandbox_state = None
    thread_data = None
    uploaded_files = None
    thread_id = None
    parent_model = None
    trace_id = None
    metadata: dict[str, Any] = {}

    if runtime is not None:
        sandbox_state = runtime.state.get("sandbox")
        thread_data = runtime.state.get("thread_data")
        uploaded_files = _snapshot_uploaded_files(runtime.state)
        ctx = runtime.context if isinstance(runtime.context, dict) else None
        thread_id = ctx.get("thread_id") if ctx else None
        if thread_id is None:
            thread_id = runtime.config.get("configurable", {}).get("thread_id")

        # Try to get parent model from configurable
        metadata = runtime.config.get("metadata", {})
        parent_model = metadata.get("model_name")
        parent_thinking_enabled = bool(metadata.get("thinking_enabled", False))

        # Get or generate trace_id for distributed tracing
        trace_id = metadata.get("trace_id") or str(uuid.uuid4())[:8]

    parent_available_skills = metadata.get("available_skills")
    if parent_available_skills is not None:
        overrides["skills"] = _merge_skill_allowlists(list(parent_available_skills), config.skills)

    if overrides:
        config = replace(config, **overrides)

    prompt_overlay = metadata.get("subagent_system_prompt_overlay")
    if isinstance(prompt_overlay, str) and prompt_overlay.strip():
        config = replace(
            config,
            system_prompt=f"{config.system_prompt}\n\n{prompt_overlay.strip()}",
        )

    # Get available tools (excluding task tool to prevent nesting)
    # Lazy import to avoid circular dependency
    from deerflow.tools import get_available_tools

    # Inherit parent agent's tool_groups so subagents respect the same restrictions
    parent_tool_groups = metadata.get("tool_groups")

    # Subagents should not have subagent tools enabled (prevent recursive nesting)
    tools = get_available_tools(
        model_name=parent_model,
        groups=parent_tool_groups,
        subagent_enabled=False,
        include_upload_tool=uploaded_files is not None,
    )
    excluded_tool_names = set(metadata.get("subagent_excluded_tool_names") or [])
    # Never rely solely on the global registry honoring subagent_enabled=False:
    # recursive delegation is a hard boundary for internal ACP subagents.
    excluded_tool_names.update({"task", "task_status"})
    allowed_tool_names_raw = metadata.get("subagent_allowed_tool_names")
    allowed_tool_names = (
        set(allowed_tool_names_raw) if allowed_tool_names_raw is not None else None
    )
    tools = [tool for tool in tools if tool.name not in excluded_tool_names]
    if allowed_tool_names is not None:
        tools = [tool for tool in tools if tool.name in allowed_tool_names]
    deferred_registry = None
    try:
        from deerflow.tools.builtins.tool_search import clone_deferred_registry_for_tools, get_deferred_registry

        if any(tool.name == "tool_search" for tool in tools):
            deferred_registry = clone_deferred_registry_for_tools(
                get_deferred_registry(), tools
            )
    except Exception:
        logger.debug("Failed to clone deferred registry for subagent", exc_info=True)

    # Create executor
    executor = SubagentExecutor(
        config=config,
        tools=tools,
        parent_model=parent_model,
        sandbox_state=sandbox_state,
        thread_data=thread_data,
        uploaded_files=uploaded_files,
        thread_id=thread_id,
        trace_id=trace_id,
        thinking_enabled=parent_thinking_enabled,
        deferred_registry=deferred_registry,
        middlewares=list(metadata.get("subagent_middlewares") or []),
        acceptance_criteria=acceptance_criteria,
    )

    # Resolve the live_event_callback from config metadata.
    # This callback writes directly into the SSE bridge, bypassing LangGraph's
    # per-node custom-stream buffer which is only flushed when the node returns.
    # Without this, all writer() calls would be buffered until the tool coroutine
    # exits, causing 30+ second delays for long-running task polls.
    _live_cb: Callable[[dict[str, Any]], Coroutine[Any, Any, None]] | None = None
    if runtime is not None:
        _live_cb = runtime.config.get("metadata", {}).get("live_event_callback")

    # Capture the main event loop so that _emit can schedule coroutines on it
    # from any thread (including the subagent's isolated ThreadPoolExecutor thread).
    _main_loop: asyncio.AbstractEventLoop | None = None
    try:
        _main_loop = asyncio.get_running_loop()
    except RuntimeError:
        pass

    def _emit(event: dict[str, Any]) -> None:
        """Push an event to the SSE bridge (non-blocking fire-and-forget).

        Safe to call from any thread: uses call_soon_threadsafe when invoked
        from outside the main event loop thread.
        """
        if _live_cb is None or _main_loop is None:
            return
        cb = _live_cb
        loop = _main_loop
        try:
            if loop.is_running():
                def schedule_event() -> None:
                    loop.create_task(cb(event))

                loop.call_soon_threadsafe(schedule_event)
        except Exception:
            pass

    # Polling interval (seconds). 1s balances responsiveness with CPU overhead.
    _POLL_INTERVAL = 1

    # Forward LLM token chunks from the subagent thread to the SSE stream in real
    # time.  The callback is invoked synchronously from the subagent's isolated
    # event loop thread; _emit schedules the async bridge publish on the main loop,
    # so it is safe to call across thread boundaries.
    # Client events remain correlated with the provider's tool-call ID. The
    # process-wide task registry uses a separate server-generated execution ID.
    def _token_stream_callback(event: dict[str, Any]) -> None:
        _emit(
            {
                "type": "task_token_chunk",
                "task_id": tool_call_id,
                "subagent_type": subagent_type,
                **event,
            }
        )

    execution_id = executor.execute_async(
        prompt,
        task_id=tool_call_id,
        stream_callback=_token_stream_callback,
    )

    # Send Task Started message
    _emit({"type": "task_started", "task_id": tool_call_id, "description": description})

    # Poll for task completion in backend (removes need for LLM to poll)
    poll_count = 0
    last_status = None
    last_message_count = 0  # Track how many AI messages we've already sent
    # Polling timeout: execution timeout + 60s buffer, checked every _POLL_INTERVAL seconds
    max_poll_count = (config.timeout_seconds + 60) // _POLL_INTERVAL

    logger.info(
        "[trace=%s] Started background task %s (execution_id=%s, subagent=%s, timeout=%ss, polling_limit=%s polls)",
        trace_id,
        tool_call_id,
        execution_id,
        subagent_type,
        config.timeout_seconds,
        max_poll_count,
    )

    try:
        while True:
            result = get_background_task_result(execution_id)

            if result is None:
                logger.error(f"[trace={trace_id}] Task {tool_call_id} execution {execution_id} not found in background tasks")
                _emit({"type": "task_failed", "task_id": tool_call_id, "error": "Task disappeared from background tasks"})
                cleanup_background_task(execution_id)
                return f"Error: Task {tool_call_id} disappeared from background tasks"

            # Log status changes for debugging
            if result.status != last_status:
                logger.info(f"[trace={trace_id}] Task {tool_call_id} execution {execution_id} status: {result.status.value}")
                last_status = result.status

            # Check for new AI messages and send task_running events
            current_message_count = len(result.ai_messages)
            if current_message_count > last_message_count:
                # Send task_running event for each new message
                for i in range(last_message_count, current_message_count):
                    message = result.ai_messages[i]
                    _emit(
                        {
                            "type": "task_running",
                            "task_id": tool_call_id,
                            "message": message,
                            "message_index": i + 1,  # 1-based index for display
                            "total_messages": current_message_count,
                        }
                    )
                    logger.info(f"[trace={trace_id}] Task {tool_call_id} sent message #{i + 1}/{current_message_count}")
                last_message_count = current_message_count

            # Check if task completed, failed, or timed out
            if result.status == SubagentStatus.COMPLETED:
                from deerflow.agents.middlewares.receipt_verification import (
                    render_citation_verdict,
                    verify_receipt_citations,
                )
                from deerflow.subagents.acceptance_checks import (
                    render_acceptance_section,
                )

                receipt_verdict = verify_receipt_citations(
                    result.result or "",
                    list(getattr(result, "tool_receipts", []) or []),
                )
                acceptance_verdict = getattr(result, "acceptance_verdict", None)
                _emit(
                    {
                        "type": "task_completed",
                        "task_id": tool_call_id,
                        "result": result.result,
                        "receipt_verdict": receipt_verdict,
                        "acceptance_verdict": acceptance_verdict,
                    }
                )
                logger.info(f"[trace={trace_id}] Task {tool_call_id} completed after {poll_count} polls")
                cleanup_background_task(execution_id)
                sections = [f"Task Succeeded. Result: {result.result}"]
                citation_summary = render_citation_verdict(receipt_verdict)
                if citation_summary:
                    sections.append(citation_summary)
                if isinstance(acceptance_verdict, dict):
                    sections.append(render_acceptance_section(acceptance_verdict))
                return "\n\n".join(sections)
            elif result.status == SubagentStatus.LIMIT_REACHED:
                reason = result.termination_reason or "limit_reached"
                termination_message = (
                    result.error or f"Subagent stopped before completion ({reason})"
                )
                if runtime is not None and isinstance(runtime.context, dict):
                    runtime.context.setdefault("stop_reason", reason)
                _emit(
                    {
                        "type": "task_limit_reached",
                        "task_id": tool_call_id,
                        "reason": reason,
                        "error": termination_message,
                        "result": result.result,
                        "incomplete": True,
                    }
                )
                logger.warning(
                    "[trace=%s] Task %s stopped incomplete: %s",
                    trace_id,
                    tool_call_id,
                    termination_message,
                )
                cleanup_background_task(execution_id)
                partial_result = (
                    f" Partial result: {result.result}" if result.result else ""
                )
                return (
                    f"Task incomplete. Reason: {termination_message}."
                    f"{partial_result}"
                )
            elif result.status == SubagentStatus.FAILED:
                _emit({"type": "task_failed", "task_id": tool_call_id, "error": result.error})
                logger.error(f"[trace={trace_id}] Task {tool_call_id} failed: {result.error}")
                cleanup_background_task(execution_id)
                return f"Task failed. Error: {result.error}"
            elif result.status == SubagentStatus.CANCELLED:
                _emit({"type": "task_cancelled", "task_id": tool_call_id, "error": result.error})
                logger.info(f"[trace={trace_id}] Task {tool_call_id} cancelled: {result.error}")
                cleanup_background_task(execution_id)
                return "Task cancelled by user."
            elif result.status == SubagentStatus.TIMED_OUT:
                _emit({"type": "task_timed_out", "task_id": tool_call_id, "error": result.error})
                logger.warning(f"[trace={trace_id}] Task {tool_call_id} timed out: {result.error}")
                cleanup_background_task(execution_id)
                return f"Task timed out. Error: {result.error}"

            # Still running, wait before next poll
            await asyncio.sleep(_POLL_INTERVAL)
            poll_count += 1

            # Polling timeout as a safety net (in case thread pool timeout doesn't work)
            # Set to execution timeout + 60s buffer, in _POLL_INTERVAL-second intervals
            # This catches edge cases where the background task gets stuck
            if poll_count > max_poll_count:
                timeout_minutes = config.timeout_seconds // 60
                logger.error(f"[trace={trace_id}] Task {tool_call_id} execution {execution_id} polling timed out after {poll_count} polls (should have been caught by thread pool timeout)")
                _emit({"type": "task_timed_out", "task_id": tool_call_id})
                finalize_cancelled_background_task(
                    execution_id,
                    status=SubagentStatus.TIMED_OUT,
                    error="Parent task polling safety timeout",
                )
                cleanup_background_task(execution_id)
                return f"Task polling timed out after {timeout_minutes} minutes. This may indicate the background task is stuck. Status: {result.status.value}"
    except asyncio.CancelledError:
        # Signal the background subagent thread to stop cooperatively.
        # Without this, the thread (running in ThreadPoolExecutor with its
        # own event loop via asyncio.run) would continue executing even
        # after the parent task is cancelled.
        finalize_cancelled_background_task(
            execution_id,
            status=SubagentStatus.CANCELLED,
            error="Parent task was cancelled",
        )
        cleanup_background_task(execution_id)
        raise
    except Exception as exc:
        # The poller owns the registry entry after execute_async succeeds.
        # Unexpected failures (for example a status lookup or event bridge
        # failure) must not leave that background execution running and
        # permanently registered after this tool invocation unwinds.
        logger.error(
            "[trace=%s] Task %s poller exited unexpectedly (%s)",
            trace_id,
            tool_call_id,
            type(exc).__name__,
        )
        finalize_cancelled_background_task(
            execution_id,
            status=SubagentStatus.CANCELLED,
            error="Parent task stopped polling unexpectedly",
        )
        cleanup_background_task(execution_id)
        raise


def _create_task_tool() -> StructuredTool:
    """Create the task tool with both sync and async invocation support.

    The core implementation is async. A sync wrapper bridges via asyncio.run()
    for contexts where the graph executes synchronously (e.g., agent.stream()).
    This fixes: "StructuredTool does not support sync invocation" — the
    `@tool` decorator on an async-only function left `func=None`, causing
    LangGraph's ToolNode to fail when it calls `tool.invoke()`.
    """

    @wraps(_task_tool_impl)
    def _sync_task_tool(
        runtime: ToolRuntime[AgentContext, ThreadState],
        description: str,
        prompt: str,
        subagent_type: str,
        tool_call_id: str,
        max_turns: int | None = None,
        acceptance_criteria: list[str] | None = None,
    ) -> str:
        """Sync wrapper that delegates to the async implementation."""
        coro = _task_tool_impl(
            runtime=runtime,
            description=description,
            prompt=prompt,
            subagent_type=subagent_type,
            tool_call_id=tool_call_id,
            max_turns=max_turns,
            acceptance_criteria=acceptance_criteria,
        )

        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            loop = None

        if loop is not None and loop.is_running():
            # Already in an async context — schedule on a background thread
            # to avoid nested event-loop issues.
            with concurrent.futures.ThreadPoolExecutor(max_workers=1) as executor:
                future = executor.submit(asyncio.run, coro)
                return future.result()
        else:
            return asyncio.run(coro)

    return StructuredTool(
        name="task",
        description=_TASK_TOOL_DESCRIPTION,
        args_schema=_TaskToolInput,
        func=_sync_task_tool,
        coroutine=_task_tool_impl,
    )


# Export the task tool as a StructuredTool instance.
task_tool = _create_task_tool()
