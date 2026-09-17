import asyncio
from types import SimpleNamespace

from langchain_core.messages import AIMessage

from deerflow.agents.middlewares.clarification_middleware import ClarificationMiddleware


def test_clarification_flattens_cleans_and_deduplicates_dict_options() -> None:
    middleware = ClarificationMiddleware()
    options = {
        "item": {
            "item": "Move the section earlier</item>",
            "$text": ["Merge shared patterns</item>", "Move the section earlier"],
        },
        "$text": 2,
        "ignored": None,
    }

    assert middleware._normalize_options(options) == [
        "Move the section earlier",
        "Merge shared patterns",
        "2",
    ]


def test_clarification_formats_json_encoded_dict_options() -> None:
    middleware = ClarificationMiddleware()
    message = middleware._format_clarification_message(
        {
            "question": "Choose",
            "clarification_type": "approach_choice",
            "options": '{"item": ["A</item>", "B", "A"]}',
        }
    )

    assert "1. A" in message
    assert "2. B" in message
    assert "3." not in message


def test_clarification_drops_parallel_sibling_tools_and_provider_metadata() -> None:
    message = AIMessage(
        id="assistant-1",
        content=[
            {"type": "text", "text": "I need confirmation."},
            {
                "type": "tool_use",
                "id": "clarify-1",
                "name": "ask_clarification",
                "input": {},
            },
            {
                "type": "tool_use",
                "id": "write-1",
                "name": "write_file",
                "input": {},
            },
        ],
        tool_calls=[
            {
                "id": "clarify-1",
                "name": "ask_clarification",
                "args": {"question": "Proceed?", "clarification_type": "risk_confirmation"},
            },
            {"id": "write-1", "name": "write_file", "args": {"path": "x"}},
        ],
        additional_kwargs={
            "tool_calls": [
                {"id": "clarify-1", "type": "function"},
                {"id": "write-1", "type": "function"},
            ]
        },
        response_metadata={"finish_reason": "tool_calls"},
    )

    result = ClarificationMiddleware().after_model(
        {"messages": [message]},
        None,  # type: ignore[arg-type]
    )

    assert result is not None
    patched = result["messages"][0]
    assert [call["name"] for call in patched.tool_calls] == ["ask_clarification"]
    assert [block.get("name") for block in patched.content if block.get("type") == "tool_use"] == [
        "ask_clarification"
    ]
    assert [call["id"] for call in patched.additional_kwargs["tool_calls"]] == ["clarify-1"]
    assert patched.response_metadata["finish_reason"] == "tool_calls"


def test_malformed_clarification_still_blocks_executable_sibling() -> None:
    message = AIMessage.model_construct(
        id="assistant-2",
        content=[
            {"type": "function_call", "name": "ask_clarification", "args": "{bad"},
            {"type": "function_call", "name": "bash", "args": {}},
        ],
        type="ai",
        tool_calls=[{"id": "bash-1", "name": "bash", "args": {"command": "echo unsafe"}}],
        invalid_tool_calls=[
            {
                "id": "clarify-bad",
                "name": "ask_clarification",
                "args": None,
                "error": "invalid JSON",
            }
        ],
        additional_kwargs={
            "tool_calls": [{"id": "bash-1", "type": "function"}],
        },
        response_metadata={"finish_reason": "tool_calls"},
    )
    middleware = ClarificationMiddleware()

    result = asyncio.run(
        middleware.aafter_model(
            {"messages": [message]},
            None,  # type: ignore[arg-type]
        )
    )

    assert result is not None
    patched = result["messages"][0]
    assert patched.tool_calls == []
    assert [call["name"] for call in patched.invalid_tool_calls] == ["ask_clarification"]
    assert [block.get("name") for block in patched.content] == ["ask_clarification"]
    assert "tool_calls" not in patched.additional_kwargs
    assert patched.response_metadata["finish_reason"] == "stop"


def test_non_clarification_tool_batch_is_unchanged() -> None:
    message = AIMessage(
        content="",
        tool_calls=[{"id": "read-1", "name": "read_file", "args": {"path": "x"}}],
    )

    assert (
        ClarificationMiddleware().after_model(
            {"messages": [message]},
            None,  # type: ignore[arg-type]
        )
        is None
    )


def test_clarification_tool_message_marks_pending_human_input() -> None:
    request = SimpleNamespace(
        tool_call={
            "id": "clarify-1",
            "name": "ask_clarification",
            "args": {"question": "Proceed?"},
        }
    )

    command = ClarificationMiddleware()._handle_clarification(request)
    message = command.update["messages"][0]

    assert message.artifact["human_input"]["kind"] == "human_input_request"
    assert message.artifact["human_input"]["request_id"] == "clarify-1"
