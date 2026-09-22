import pytest
from langchain_core.messages import AIMessage, HumanMessage, ToolMessage
from langchain_deepseek import ChatDeepSeek

from deerflow.models.patched_deepseek import PatchedChatDeepSeek


def _tool_history(*, reasoning=None):
    kwargs = {"reasoning_content": reasoning} if reasoning is not None else {}
    return [
        HumanMessage(content="read it"),
        AIMessage(content="", additional_kwargs=kwargs, tool_calls=[{"id": "call-1", "name": "read_file", "args": {"path": "/tmp/report"}}]),
        ToolMessage(content="file content", tool_call_id="call-1"),
    ]


@pytest.mark.parametrize("thinking", ["enabled", "disabled"])
def test_thinking_tool_history_normalizes_null_content_and_missing_reasoning(thinking):
    model = PatchedChatDeepSeek(model="deepseek-chat", api_key="test", extra_body={"thinking": {"type": thinking}})
    history = _tool_history()
    payload = model._get_request_payload(history)
    assistant = payload["messages"][1]
    assert assistant["content"] == ""
    if thinking == "enabled":
        assert assistant["reasoning_content"] == ""
    else:
        assert "reasoning_content" not in assistant
    assert history[1].additional_kwargs == {}


def test_deepseek_keeps_real_reasoning_and_does_not_expand_ordinary_assistant():
    model = PatchedChatDeepSeek(model="deepseek-chat", api_key="test", extra_body={"thinking": {"type": "enabled"}})
    history = _tool_history(reasoning="inspecting") + [AIMessage(content="done")]
    payload = model._get_request_payload(history)
    assert payload["messages"][1]["reasoning_content"] == "inspecting"
    assert "reasoning_content" not in payload["messages"][-1]


def test_per_call_disabled_thinking_wins_over_model_default():
    model = PatchedChatDeepSeek(model="deepseek-chat", api_key="test", extra_body={"thinking": {"type": "enabled"}})
    payload = model._get_request_payload(_tool_history(), extra_body={"thinking": {"type": "disabled"}})
    assert payload["messages"][1]["content"] == ""
    assert "reasoning_content" not in payload["messages"][1]


def test_compatibility_changes_do_not_patch_base_provider():
    model = ChatDeepSeek(model="deepseek-chat", api_key="test", extra_body={"thinking": {"type": "enabled"}})
    assistant = model._get_request_payload(_tool_history())["messages"][1]
    assert assistant["content"] is None
    assert "reasoning_content" not in assistant
