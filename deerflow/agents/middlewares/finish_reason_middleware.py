"""Repair or annotate provider-capped model responses before persistence."""

from __future__ import annotations

import logging
from typing import Any, override

from langchain.agents import AgentState
from langchain.agents.middleware import AgentMiddleware
from langchain_core.messages import AIMessage
from langgraph.runtime import Runtime

from deerflow.agents.middlewares.finish_reason_detectors import (
    FinishReasonDetector,
    FinishReasonTermination,
    length_detectors,
    safety_detectors,
)
from deerflow.agents.middlewares.tool_call_metadata import (
    clone_ai_message_with_tool_calls,
)

logger = logging.getLogger(__name__)

_LENGTH_NOTICE = (
    "The model provider stopped this response because its output-token limit was reached. "
    "The answer above may be incomplete; ask the assistant to continue if needed."
)
_LENGTH_TOOL_NOTICE = (
    "The model provider stopped this response because its output-token limit was reached. "
    "Any tool calls from this turn were suppressed because their arguments may be incomplete. "
    "Ask the assistant to continue or retry with a narrower request."
)
_SAFETY_EMPTY_NOTICE = (
    "The model provider stopped this response with a safety-related signal and returned no "
    "content. Please rephrase the request."
)
_SAFETY_TOOL_NOTICE = (
    "The model provider stopped this response with a safety-related signal. Any tool calls "
    "from this turn were suppressed because their arguments may be incomplete. Please "
    "rephrase or narrow the request."
)


def _visible_content(message: AIMessage) -> bool:
    content = message.content
    if isinstance(content, str):
        return bool(content.strip())
    if isinstance(content, list):
        for block in content:
            if isinstance(block, str) and block.strip():
                return True
            if isinstance(block, dict):
                text = block.get("text")
                if isinstance(text, str) and text.strip():
                    return True
    return False


def _append_text(content: Any, text: str) -> str | list[Any]:
    if content is None or content == "" or content == []:
        return text
    if isinstance(content, list):
        return [*content, {"type": "text", "text": f"\n\n{text}"}]
    if isinstance(content, str):
        return f"{content}\n\n{text}"
    return f"{content}\n\n{text}"


def _detect(
    message: AIMessage,
    detectors: list[FinishReasonDetector],
) -> FinishReasonTermination | None:
    for detector in detectors:
        try:
            termination = detector.detect(message)
        except Exception:
            logger.warning("Finish-reason detector failed", exc_info=True)
            continue
        if termination is not None:
            return termination
    return None


def _stamp_stop_reason(runtime: Runtime | None, reason: str) -> None:
    context = getattr(runtime, "context", None)
    if isinstance(context, dict):
        context.setdefault("stop_reason", reason)


class ModelLengthFinishReasonMiddleware(AgentMiddleware[AgentState]):
    """Append a visible notice when a terminal answer hit its output limit."""

    def __init__(self, detectors: list[FinishReasonDetector] | None = None) -> None:
        super().__init__()
        self._detectors = list(detectors) if detectors is not None else length_detectors()

    def _apply(self, state: AgentState, runtime: Runtime | None) -> dict[str, Any] | None:
        messages = list(state.get("messages") or [])
        if not messages or not isinstance(messages[-1], AIMessage):
            return None
        message = messages[-1]
        termination = _detect(message, self._detectors)
        if termination is None:
            return None
        raw_kwargs = dict(message.additional_kwargs or {})
        if raw_kwargs.get("model_length_termination"):
            return None
        tool_calls = list(message.tool_calls or [])
        invalid_tool_calls = list(getattr(message, "invalid_tool_calls", None) or [])
        has_raw_tool_payload = bool(raw_kwargs.get("tool_calls") or raw_kwargs.get("function_call"))
        suppressed_count = len(tool_calls) + len(invalid_tool_calls)
        if has_raw_tool_payload and suppressed_count == 0:
            suppressed_count = 1
        base = message.model_copy(update={"invalid_tool_calls": []})
        patched = clone_ai_message_with_tool_calls(
            base,
            [],
            content=_append_text(message.content, _LENGTH_TOOL_NOTICE if suppressed_count else _LENGTH_NOTICE),
        )
        kwargs = dict(patched.additional_kwargs or {})
        kwargs["model_length_termination"] = {
            "detector": termination.detector,
            "reason_field": termination.reason_field,
            "reason_value": termination.reason_value,
            "suppressed_tool_call_count": suppressed_count,
        }
        patched = message.model_copy(
            update={
                "content": patched.content,
                "tool_calls": patched.tool_calls,
                "invalid_tool_calls": patched.invalid_tool_calls,
                "additional_kwargs": kwargs,
                "response_metadata": patched.response_metadata,
            }
        )
        _stamp_stop_reason(runtime, "model_length_capped")
        return {"messages": [patched]}

    @override
    def after_model(self, state: AgentState, runtime: Runtime) -> dict[str, Any] | None:
        return self._apply(state, runtime)

    @override
    async def aafter_model(self, state: AgentState, runtime: Runtime) -> dict[str, Any] | None:
        return self._apply(state, runtime)


class SafetyFinishReasonMiddleware(AgentMiddleware[AgentState]):
    """Suppress safety-truncated tools and backfill blank assistant messages."""

    def __init__(self, detectors: list[FinishReasonDetector] | None = None) -> None:
        super().__init__()
        self._detectors = list(detectors) if detectors is not None else safety_detectors()

    def _apply(self, state: AgentState, runtime: Runtime | None) -> dict[str, Any] | None:
        messages = list(state.get("messages") or [])
        if not messages or not isinstance(messages[-1], AIMessage):
            return None
        message = messages[-1]
        tool_calls = list(message.tool_calls or [])
        invalid_tool_calls = list(getattr(message, "invalid_tool_calls", None) or [])
        raw_kwargs = dict(message.additional_kwargs or {})
        has_raw_tool_payload = bool(raw_kwargs.get("tool_calls") or raw_kwargs.get("function_call"))
        blank = not _visible_content(message)
        if not tool_calls and not invalid_tool_calls and not has_raw_tool_payload and not blank:
            return None
        termination = _detect(message, self._detectors)
        if termination is None:
            return None

        has_tool_payload = bool(tool_calls or invalid_tool_calls or has_raw_tool_payload)
        suppressed_count = len(tool_calls) + len(invalid_tool_calls)
        if has_raw_tool_payload and suppressed_count == 0:
            suppressed_count = 1
        notice = _SAFETY_TOOL_NOTICE if has_tool_payload else _SAFETY_EMPTY_NOTICE
        base = message.model_copy(update={"invalid_tool_calls": []})
        patched = clone_ai_message_with_tool_calls(
            base,
            [],
            content=_append_text(message.content, notice),
        )
        additional_kwargs = dict(patched.additional_kwargs or {})
        additional_kwargs["safety_termination"] = {
            "detector": termination.detector,
            "reason_field": termination.reason_field,
            "reason_value": termination.reason_value,
            "suppressed_tool_call_count": suppressed_count,
            "suppressed_tool_call_names": [call.get("name") or "unknown" for call in tool_calls],
            "extras": dict(termination.extras),
        }
        patched = patched.model_copy(
            update={"additional_kwargs": additional_kwargs}
        )
        _stamp_stop_reason(runtime, "safety_capped")
        logger.warning(
            "Provider safety termination detected; suppressed %d tool call(s)",
            suppressed_count,
        )
        return {"messages": [patched]}

    @override
    def after_model(self, state: AgentState, runtime: Runtime) -> dict[str, Any] | None:
        return self._apply(state, runtime)

    @override
    async def aafter_model(self, state: AgentState, runtime: Runtime) -> dict[str, Any] | None:
        return self._apply(state, runtime)


def build_finish_reason_middlewares() -> list[AgentMiddleware]:
    """Order matters: later after_model middleware executes first in LangChain."""
    return [ModelLengthFinishReasonMiddleware(), SafetyFinishReasonMiddleware()]
