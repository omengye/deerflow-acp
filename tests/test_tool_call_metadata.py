from __future__ import annotations

from langchain_core.messages import AIMessage

from deerflow.agents.middlewares.subagent_limit_middleware import (
    SubagentLimitMiddleware,
)
from deerflow.agents.middlewares.tool_call_metadata import (
    clone_ai_message_with_tool_calls,
)


def _call(call_id: str, name: str = "task") -> dict:
    return {"id": call_id, "name": name, "args": {"prompt": call_id}}


def test_clone_syncs_responses_blocks_by_call_id() -> None:
    message = AIMessage(
        content=[
            {
                "type": "function_call",
                "id": "fc-1",
                "call_id": "call-1",
                "name": "task",
                "arguments": "{}",
            },
            {
                "type": "function_call",
                "id": "fc-2",
                "call_id": "call-2",
                "name": "task",
                "arguments": "{}",
            },
        ],
        tool_calls=[_call("call-1"), _call("call-2")],
    )

    patched = clone_ai_message_with_tool_calls(message, [_call("call-1")])

    assert [block["call_id"] for block in patched.content] == ["call-1"]


def test_subagent_limit_syncs_structured_raw_and_anthropic_calls() -> None:
    message = AIMessage(
        content=[
            {
                "type": "tool_use",
                "id": call_id,
                "name": "task",
                "input": {"prompt": call_id},
            }
            for call_id in ("call-1", "call-2", "call-3")
        ],
        tool_calls=[_call("call-1"), _call("call-2"), _call("call-3")],
        additional_kwargs={
            "tool_calls": [
                {"id": call_id, "type": "function"}
                for call_id in ("call-1", "call-2", "call-3")
            ]
        },
    )

    result = SubagentLimitMiddleware(max_concurrent=2)._truncate_task_calls(
        {"messages": [message]}
    )

    assert result is not None
    patched = result["messages"][0]
    assert [call["id"] for call in patched.tool_calls] == ["call-1", "call-2"]
    assert [block["id"] for block in patched.content] == ["call-1", "call-2"]
    assert [call["id"] for call in patched.additional_kwargs["tool_calls"]] == [
        "call-1",
        "call-2",
    ]
