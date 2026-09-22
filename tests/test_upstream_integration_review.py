from types import SimpleNamespace

import pytest
from langchain_core.messages import AIMessage, HumanMessage

from deerflow.community.ragflow.scope import scope_from_runtime
from deerflow.community.ragflow.sources import budget_sources, get_sources, source_artifact


def _clarify(index):
    return AIMessage(content="", tool_calls=[{"id": f"ask-{index}", "name": "ask_clarification", "args": {"question": "clarify"}}])


def test_repeated_clarification_does_not_broaden_knowledge_scope():
    scope = {"dataset_ids": ["approved"], "documents": None}
    messages = [
        HumanMessage(content="restricted task", additional_kwargs={"knowledge_scope": scope}),
        _clarify(1), HumanMessage(content="first clarification"),
        _clarify(2), HumanMessage(content="second clarification"),
    ]
    runtime = SimpleNamespace(context={}, config={}, state={"messages": messages})
    assert scope_from_runtime(runtime) == scope


def test_omitting_all_oversized_sources_preserves_task_summary():
    artifact = source_artifact([{"id": "a" * 32, "excerpt": "x" * 2000, "document_name": "Policy"}])
    artifact["summary"] = "The child finished the requested comparison."
    text, remaining = budget_sources("unused", artifact, 300)
    assert "child finished the requested comparison" in text
    assert get_sources(remaining) == []


def test_task_summary_does_not_keep_citations_missing_from_its_artifact():
    first = {"id": "a" * 32, "excerpt": "x" * 2000, "document_name": "Policy A"}
    second = {"id": "b" * 32, "excerpt": "small evidence", "document_name": "Policy B"}
    artifact = source_artifact([first, second])
    artifact["summary"] = f"Result from [Source {first['id']}] and [Source {second['id']}]."
    text, remaining = budget_sources("unused", artifact, 500)
    assert {source["id"] for source in get_sources(remaining)} == {second["id"]}
    assert first["id"] not in text


async def test_agui_route_keeps_per_turn_scope_out_of_cached_client(monkeypatch):
    from app.dependencies import ClientManager
    from app.routers import chat
    from app.schemas import AguiRunAgentInput
    from deerflow.client import StreamEvent

    created = []
    received = []

    class FakeClient:
        agent_name = "test"

        def __init__(self, **kwargs):
            assert "knowledge_scope" not in kwargs
            created.append(kwargs)

        async def astream(self, message, **kwargs):
            received.append(kwargs)
            yield StreamEvent(type="end", data={})

    manager = ClientManager()

    async def checkpointer():
        return None

    async def touch(*_args, **_kwargs):
        return None

    monkeypatch.setattr(manager, "_get_async_checkpointer", checkpointer)
    monkeypatch.setattr(manager, "touch_thread_activity", touch)
    monkeypatch.setattr(manager, "_schedule_completed_run_retention", lambda _: None)
    monkeypatch.setattr("deerflow.client.DeerFlowClient", FakeClient)
    monkeypatch.setattr(chat, "get_client_manager", lambda: manager)
    scopes = [{"dataset_ids": ["a"], "documents": None}, {"dataset_ids": ["b"], "documents": None}, None]
    for index, scope in enumerate(scopes):
        request = AguiRunAgentInput(
            threadId=f"agui-scope-{index}", runId=f"agui-run-{index}", knowledgeScope=scope,
            messages=[{"id": "user", "role": "user", "content": "search"}],
        )
        response = await chat.chat_agui(SimpleNamespace(headers={}), request)
        body = [part async for part in response.body_iterator]
        assert any("RUN_FINISHED" in part for part in body)
    assert [item["knowledge_scope"] for item in received] == scopes
    assert len(created) == 1


@pytest.mark.parametrize("scope", [{"dataset_ids": ["a"], "documents": []}, None])
async def test_manager_forwards_explicit_scope_to_each_stream(monkeypatch, scope):
    from app.dependencies import ClientManager
    from deerflow.client import StreamEvent
    from deerflow.runtime import RunStatus

    received = []

    class FakeClient:
        agent_name = "test"

        async def astream(self, message, **kwargs):
            received.append(kwargs)
            yield StreamEvent(type="end", data={})

    client = FakeClient()
    manager = ClientManager()

    async def get_client(**kwargs):
        assert "knowledge_scope" not in kwargs
        return client

    async def touch(*_args, **_kwargs):
        return None

    monkeypatch.setattr(manager, "get_async_client", get_client)
    monkeypatch.setattr(manager, "touch_thread_activity", touch)
    monkeypatch.setattr(manager, "_schedule_completed_run_retention", lambda _: None)
    record = await manager.start_client_stream_run(thread_id="scope-test", message="search", kwargs={"knowledge_scope": scope})
    await record.task
    assert record.status == RunStatus.success
    assert len(received) == 1
    assert "knowledge_scope" in received[0]
    assert received[0]["knowledge_scope"] == scope
