from __future__ import annotations

import json

from langchain_core.messages import AIMessage

from deerflow.agents.middlewares.tool_call_args import (
    rewrite_messages_tool_call_args,
)


def test_rewrite_updates_all_provider_surfaces_including_idless_gemini() -> None:
    original_args = {"path": "/tmp/a", "content": "secret"}
    replacement = {"path": "/tmp/a", "content": "[elided]"}
    message = AIMessage(
        content=[
            {
                "type": "tool_use",
                "id": "call-1",
                "name": "write_file",
                "input": original_args,
                "partial_json": "old",
            },
            {
                "type": "function_call",
                "id": "fc-1",
                "call_id": "call-1",
                "name": "write_file",
                "arguments": json.dumps(original_args),
            },
            {
                "type": "custom_tool_call",
                "id": "ctc-1",
                "call_id": "call-1",
                "name": "write_file",
                "input": "secret",
            },
            {
                "type": "function_call",
                "name": "write_file",
                "args": original_args,
            },
        ],
        tool_calls=[
            {
                "id": "call-1",
                "name": "write_file",
                "args": original_args,
            }
        ],
        additional_kwargs={
            "tool_calls": [
                {
                    "id": "call-1",
                    "type": "function",
                    "function": {
                        "name": "write_file",
                        "arguments": json.dumps(original_args),
                    },
                }
            ],
            "function_call": {
                "name": "write_file",
                "arguments": json.dumps(original_args),
            },
        },
        response_metadata={"id": "resp_123"},
    )

    rewritten = rewrite_messages_tool_call_args(
        [message],
        lambda _message, call: replacement if call["id"] == "call-1" else None,
    )

    assert rewritten is not None
    patched = rewritten[0]
    assert message.tool_calls[0]["args"] == original_args
    assert patched.tool_calls[0]["args"] == replacement
    assert json.loads(
        patched.additional_kwargs["tool_calls"][0]["function"]["arguments"]
    ) == replacement
    assert patched.content[0]["input"] == replacement
    assert "partial_json" not in patched.content[0]
    assert json.loads(patched.content[1]["arguments"]) == replacement
    assert json.loads(patched.content[2]["input"]) == replacement
    assert patched.content[3]["args"] == replacement
    assert json.loads(
        patched.additional_kwargs["function_call"]["arguments"]
    ) == replacement
    assert "id" not in patched.response_metadata


def test_idless_name_fallback_is_disabled_for_duplicate_names() -> None:
    message = AIMessage(
        content=[
            {"type": "function_call", "name": "write_file", "args": {"x": 1}},
            {"type": "function_call", "name": "write_file", "args": {"x": 2}},
        ],
        tool_calls=[
            {"id": "one", "name": "write_file", "args": {"x": 1}},
            {"id": "two", "name": "write_file", "args": {"x": 2}},
        ],
    )

    rewritten = rewrite_messages_tool_call_args(
        [message],
        lambda _message, call: {"x": 9} if call["id"] == "one" else None,
    )

    assert rewritten is None


def test_custom_tool_call_uses_single_string_argument() -> None:
    message = AIMessage(
        content=[
            {
                "type": "custom_tool_call",
                "call_id": "custom-1",
                "name": "shell",
                "input": "old command",
            }
        ],
        tool_calls=[
            {
                "id": "custom-1",
                "name": "shell",
                "args": {"__arg1": "new command"},
            }
        ],
    )

    rewritten = rewrite_messages_tool_call_args(
        [message],
        lambda _message, call: call["args"],
    )

    assert rewritten is not None
    assert rewritten[0].content[0]["input"] == "new command"


def test_responses_item_id_cannot_override_authoritative_call_id() -> None:
    message = AIMessage(
        content=[
            {
                "type": "function_call",
                "id": "call-a",
                "call_id": "call-b",
                "name": "write_file",
                "arguments": '{"value":"B"}',
            },
            {
                "type": "custom_tool_call",
                "id": "call-a",
                "call_id": "call-b",
                "name": "write_file",
                "input": "B",
            },
        ],
        tool_calls=[
            {"id": "call-a", "name": "write_file", "args": {"value": "A"}},
            {"id": "call-b", "name": "write_file", "args": {"value": "B"}},
        ],
    )

    rewritten = rewrite_messages_tool_call_args(
        [message],
        lambda _message, call: (
            {"value": "A2", "__arg1": "A2"}
            if call["id"] == "call-a"
            else None
        ),
    )

    assert rewritten is not None
    assert rewritten[0].tool_calls[0]["args"]["value"] == "A2"
    assert rewritten[0].content == message.content


def test_raw_tool_call_with_id_cannot_fall_back_to_nested_name() -> None:
    raw_call = {
        "id": "call-b",
        "type": "function",
        "function": {"name": "tool_a", "arguments": '{"value":"B"}'},
    }
    message = AIMessage(
        content="",
        tool_calls=[
            {"id": "call-a", "name": "tool_a", "args": {"value": "A"}},
            {"id": "call-b", "name": "tool_b", "args": {"value": "B"}},
        ],
        additional_kwargs={"tool_calls": [raw_call]},
    )

    rewritten = rewrite_messages_tool_call_args(
        [message],
        lambda _message, call: (
            {"value": "A2"} if call["id"] == "call-a" else None
        ),
    )

    assert rewritten is not None
    assert rewritten[0].tool_calls[0]["args"] == {"value": "A2"}
    assert rewritten[0].additional_kwargs["tool_calls"] == [raw_call]
