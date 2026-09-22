from langchain_core.messages import AIMessage, AIMessageChunk, ToolMessage

from deerflow.client import DeerFlowClient, _StreamProcessingState


def _events(client, state, item):
    return [e.data for e in client._events_from_stream_item(item, state) if e.type == "messages-tuple"]


def test_streamed_answer_gets_guard_suffix_once_and_metadata():
    client = object.__new__(DeerFlowClient)
    state = _StreamProcessingState()
    first = _events(client, state, ("messages", (AIMessageChunk(id="a", content="Answer"), {})))
    assert first[0]["content"] == "Answer"
    assert not _events(client, state, ("values", {"messages": [AIMessage(id="a", content="Answer")]}))
    changed = AIMessage(id="a", content="Answer\n[FORCED STOP]", additional_kwargs={"stop_reason": "loop_capped"})
    extra = _events(client, state, ("values", {"messages": [changed]}))
    assert [e["content"] for e in extra] == ["\n[FORCED STOP]"]
    assert extra[0]["additional_kwargs"]["stop_reason"] == "loop_capped"
    assert not _events(client, state, ("values", {"messages": [changed.model_copy(deep=True)]}))


def test_values_only_rewrites_append_without_replaying_replacements_or_history():
    client = object.__new__(DeerFlowClient)
    state = _StreamProcessingState(seen_ids={"history"})
    assert not _events(client, state, ("values", {"messages": [AIMessage(id="history", content="Old")]}))
    output = []
    for text in ["A", "AB", "different", "ABC"]:
        output.extend(_events(client, state, ("values", {"messages": [AIMessage(id="new", content=text)]})))
    assert "".join(e["content"] for e in output) == "ABC"


def test_tool_calls_and_results_still_emitted_once_after_snapshot_rewrite():
    client = object.__new__(DeerFlowClient)
    state = _StreamProcessingState()
    call = AIMessage(id="a", content="", tool_calls=[{"name": "read_file", "args": {"path": "a"}, "id": "tc"}])
    output = _events(client, state, ("values", {"messages": [call]}))
    assert len(output) == 1 and output[0]["tool_calls"][0]["args"] == {"path": "a"}
    updated = call.model_copy(update={"content": "Stopped"})
    extra = _events(client, state, ("values", {"messages": [updated]}))
    assert len(extra) == 1 and extra[0]["content"] == "Stopped"
    result = ToolMessage(id="t", tool_call_id="tc", content="ok")
    assert len(_events(client, state, ("messages", (result, {})))) == 1
    assert not _events(client, state, ("values", {"messages": [updated, result]}))


def test_late_usage_and_metadata_without_text_are_not_lost_or_double_counted():
    client = object.__new__(DeerFlowClient)
    state = _StreamProcessingState()
    _events(client, state, ("values", {"messages": [AIMessage(id="a", content="Done")]}))
    final = AIMessage(id="a", content="Done", usage_metadata={"input_tokens": 5, "output_tokens": 2, "total_tokens": 7}, additional_kwargs={"token_usage_attribution": {"model": "x"}})
    extra = _events(client, state, ("values", {"messages": [final]}))
    assert len(extra) == 1 and extra[0]["content"] == ""
    assert extra[0]["usage_metadata"]["total_tokens"] == 7
    assert not _events(client, state, ("values", {"messages": [final]}))
    assert state.cumulative_usage["total_tokens"] == 7
