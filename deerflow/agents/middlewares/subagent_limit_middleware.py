"""Middleware to enforce maximum concurrent subagent tool calls per model response."""

import logging
from typing import override

from langchain.agents import AgentState
from langchain.agents.middleware import AgentMiddleware
from langgraph.runtime import Runtime

from deerflow.agents.middlewares.tool_call_metadata import (
    clone_ai_message_with_tool_calls,
)

logger = logging.getLogger(__name__)

# Valid range for max_concurrent_subagents
MIN_SUBAGENT_LIMIT = 2
MAX_SUBAGENT_LIMIT = 4


def clamp_subagent_limit(value: int) -> int:
    """Clamp subagent limit to valid range [2, 4]."""
    return max(MIN_SUBAGENT_LIMIT, min(MAX_SUBAGENT_LIMIT, value))


def _clamp_subagent_limit(value: int) -> int:
    """Backward-compatible internal alias for the public clamp helper."""
    return clamp_subagent_limit(value)


class SubagentLimitMiddleware(AgentMiddleware[AgentState]):
    """Truncates excess 'task' tool calls from a single model response.

    When an LLM generates more than max_concurrent parallel task tool calls
    in one response, this middleware keeps only the first max_concurrent and
    discards the rest. This is more reliable than prompt-based limits.

    Args:
        max_concurrent: Maximum number of concurrent subagent calls allowed.
            Defaults to deerflow.subagents.executor.MAX_CONCURRENT_SUBAGENTS (3),
            resolved lazily at construction time (see __init__). Clamped to [2, 4].
    """

    def __init__(self, max_concurrent: int | None = None):
        super().__init__()
        if max_concurrent is None:
            # Lazy import: deerflow.subagents.__init__ imports .executor, which
            # imports deerflow.agents.thread_state -- and this module sits on the
            # import path deerflow.agents.__init__ pulls in eagerly while priming
            # the skills cache (agents.__init__ -> lead_agent.__init__ ->
            # lead_agent.agent -> this module, via SubagentLimitMiddleware's
            # import in agent.py). A module-level import of MAX_CONCURRENT_SUBAGENTS
            # here would make deerflow.subagents.executor import itself back,
            # mid-init, and fail with "partially initialized module" whenever
            # deerflow.subagents is imported before deerflow.agents finishes
            # (e.g. `import deerflow.subagents.executor` directly). Deferring to
            # construction time -- reached only once both packages have finished
            # importing -- breaks the cycle.
            from deerflow.subagents.executor import MAX_CONCURRENT_SUBAGENTS

            max_concurrent = MAX_CONCURRENT_SUBAGENTS
        self.max_concurrent = _clamp_subagent_limit(max_concurrent)

    def _truncate_task_calls(self, state: AgentState) -> dict | None:
        messages = state.get("messages", [])
        if not messages:
            return None

        last_msg = messages[-1]
        if getattr(last_msg, "type", None) != "ai":
            return None

        tool_calls = getattr(last_msg, "tool_calls", None)
        if not tool_calls:
            return None

        # Count task tool calls
        task_indices = [i for i, tc in enumerate(tool_calls) if tc.get("name") == "task"]
        if len(task_indices) <= self.max_concurrent:
            return None

        # Build set of indices to drop (excess task calls beyond the limit)
        indices_to_drop = set(task_indices[self.max_concurrent :])
        truncated_tool_calls = [tc for i, tc in enumerate(tool_calls) if i not in indices_to_drop]

        dropped_count = len(indices_to_drop)
        logger.warning(f"Truncated {dropped_count} excess task tool call(s) from model response (limit: {self.max_concurrent})")

        # Keep provider-native tool blocks and raw metadata aligned with the
        # structured calls that the tools node will actually execute.
        updated_msg = clone_ai_message_with_tool_calls(
            last_msg,
            truncated_tool_calls,
        )
        return {"messages": [updated_msg]}

    @override
    def after_model(self, state: AgentState, runtime: Runtime) -> dict | None:
        return self._truncate_task_calls(state)

    @override
    async def aafter_model(self, state: AgentState, runtime: Runtime) -> dict | None:
        return self._truncate_task_calls(state)
