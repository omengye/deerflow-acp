from types import SimpleNamespace

import pytest
from langchain_core.messages import AIMessage

from deerflow.agents.middlewares.finish_reason_middleware import (
    ModelLengthFinishReasonMiddleware,
    SafetyFinishReasonMiddleware,
)


@pytest.mark.parametrize(
    "metadata",
    [
        {"finish_reason": "length"},
        {"finish_reason": "MAX_TOKENS"},
        {"stop_reason": "max_tokens"},
    ],
)
def test_length_finish_reason_appends_visible_notice(metadata):
    message = AIMessage(content="Partial answer", response_metadata=metadata, id="answer-1")
    runtime = SimpleNamespace(context={})

    result = ModelLengthFinishReasonMiddleware().after_model(
        {"messages": [message]},
        runtime,
    )

    assert result is not None
    patched = result["messages"][0]
    assert patched.id == "answer-1"
    assert "Partial answer" in patched.content
    assert "output-token limit" in patched.content
    assert patched.additional_kwargs["model_length_termination"]
    assert runtime.context["stop_reason"] == "model_length_capped"


def test_length_finish_reason_suppresses_possibly_truncated_tool_calls():
    message = AIMessage(
        content="partial",
        response_metadata={"finish_reason": "length"},
        tool_calls=[{"id": "call-1", "name": "write_file", "args": {"path": "x"}}],
    )

    result = ModelLengthFinishReasonMiddleware().after_model(
        {"messages": [message]},
        SimpleNamespace(context={}),
    )

    assert result is not None
    patched = result["messages"][0]
    assert patched.tool_calls == []
    assert "suppressed" in patched.content
    assert patched.additional_kwargs["model_length_termination"]["suppressed_tool_call_count"] == 1


def test_length_finish_reason_backfills_empty_terminal_message():
    message = AIMessage(content="", response_metadata={"finish_reason": "length"})

    result = ModelLengthFinishReasonMiddleware().after_model(
        {"messages": [message]},
        SimpleNamespace(context={}),
    )

    assert result is not None
    assert "output-token limit" in result["messages"][0].content


@pytest.mark.parametrize(
    "metadata",
    [
        {"finish_reason": "content_filter"},
        {"finish_reason": "SAFETY"},
        {"stop_reason": "refusal"},
    ],
)
def test_safety_finish_reason_backfills_empty_message(metadata):
    message = AIMessage(content="", response_metadata=metadata, id="answer-2")
    runtime = SimpleNamespace(context={})

    result = SafetyFinishReasonMiddleware().after_model({"messages": [message]}, runtime)

    assert result is not None
    patched = result["messages"][0]
    assert patched.id == "answer-2"
    assert "safety-related signal" in patched.content
    assert patched.additional_kwargs["safety_termination"]
    assert runtime.context["stop_reason"] == "safety_capped"


def test_safety_finish_reason_suppresses_structured_and_raw_tool_calls():
    message = AIMessage(
        content=[
            {
                "type": "tool_use",
                "id": "call-1",
                "name": "write_file",
                "input": {"path": "x"},
            }
        ],
        response_metadata={"finish_reason": "content_filter"},
        tool_calls=[{"id": "call-1", "name": "write_file", "args": {"path": "x"}}],
        additional_kwargs={
            "tool_calls": [{"id": "call-1", "function": {"name": "write_file"}}],
            "function_call": {"name": "write_file"},
        },
    )

    result = SafetyFinishReasonMiddleware().after_model(
        {"messages": [message]},
        SimpleNamespace(context={}),
    )

    patched = result["messages"][0]
    assert patched.tool_calls == []
    assert "tool_calls" not in patched.additional_kwargs
    assert "function_call" not in patched.additional_kwargs
    assert [block["type"] for block in patched.content] == ["text"]
    assert "suppressed" in patched.content[0]["text"]


def test_safety_finish_reason_preserves_visible_refusal_without_tools():
    message = AIMessage(
        content="I cannot help with that request.",
        response_metadata={"finish_reason": "content_filter"},
    )

    assert SafetyFinishReasonMiddleware().after_model(
        {"messages": [message]},
        SimpleNamespace(context={}),
    ) is None
