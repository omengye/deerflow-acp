from __future__ import annotations

import importlib
import json
from types import SimpleNamespace
from unittest.mock import Mock

import acp
from acp import schema
import pytest
from langchain_core.messages import ToolMessage

from app.routers.chat import _stream_event_to_agui
from deerflow.acp.event_mapper import ACPEventMapper
from deerflow.client import DeerFlowClient, StreamEvent
from deerflow.community.ragflow import tools as rag_tools
from deerflow.community.ragflow.sources import budget_sources, format_sources, get_sources, source_artifact, sources_from_messages
from deerflow.config.tool_config import ToolConfig
from deerflow.runtime.serialization import serialize
from deerflow.subagents.config import SubagentConfig
from deerflow.subagents.executor import SubagentResult, SubagentStatus


@pytest.fixture
def evidence(tmp_path):
    content, artifact = format_sources(
        {"chunks": [{"dataset_id": "approved", "document_id": "doc", "id": "chunk", "document_keyword": "Policy", "content": "The original supporting passage."}]},
        dataset_names_by_id={"approved": "Approved"}, max_chars_per_chunk=200, max_total_chars=1000, outputs_path=str(tmp_path),
    )
    return ToolMessage(content=content, name="knowledge_search", tool_call_id="lookup", id="tool-message", artifact=artifact)


def _agui(event):
    return _stream_event_to_agui(event, "thread", "run", None, {}, set())


def _no_global_config():
    raise AssertionError("Source transport must not consult application configuration")


@pytest.mark.parametrize("worker_wire", [False, True])
def test_sdk_and_worker_source_events_survive_agui_live_and_snapshot_without_global_config(evidence, monkeypatch, worker_wire):
    monkeypatch.setattr("deerflow.config.get_app_config", _no_global_config)
    if worker_wire:
        live = SimpleNamespace(type="messages-tuple", data=serialize((evidence, {}), mode="messages"))
        state = serialize({"messages": [evidence, evidence]}, mode="values")
    else:
        live = DeerFlowClient._tool_message_event(evidence)
        state = {"messages": [DeerFlowClient._serialize_message(evidence)] * 2}
    expected = get_sources(evidence.artifact)
    for event in (live, SimpleNamespace(type="values", data=state)):
        events = _agui(event)
        sources = [item for item in events if item.get("name") == "deerflow.knowledge_sources"]
        assert len(sources) == 1
        assert sources[0]["value"] == expected
    assert sources_from_messages([{"type": "human", "knowledge_sources": expected}]) == []


@pytest.mark.asyncio
async def test_acp_publishes_source_once_across_live_and_checkpoint_and_recovers_values_only(evidence, monkeypatch):
    monkeypatch.setattr("deerflow.config.get_app_config", _no_global_config)
    source = get_sources(evidence.artifact)[0]
    published = []
    updates = []

    async def publish(path):
        published.append(path)
        return acp.resource_link_block("knowledge.md", "https://artifacts.test/knowledge.md")

    async def send(update):
        updates.append(update)

    mapper = ACPEventMapper("session", send, artifact_resolver=publish)
    await mapper.handle(DeerFlowClient._tool_message_event(evidence))
    checkpoint = SimpleNamespace(type="values", data=serialize({"messages": [evidence, evidence]}, mode="values"))
    await mapper.handle(checkpoint)
    assert published == [source["path"]]
    resources = [item for item in updates if isinstance(item, schema.AgentMessageChunk)]
    assert len(resources) == 1 and resources[0].content.uri == "https://artifacts.test/knowledge.md"
    restored = ACPEventMapper("restored", send, artifact_resolver=publish)
    await restored.handle(checkpoint)
    assert published == [source["path"], source["path"]]
    # An altered path must never be sent to the publisher, even when the ID is valid.
    await restored.handle(StreamEvent(type="values", data={"messages": [{"type": "tool", "knowledge_sources": [{**source, "id": "f" * 32, "path": "/etc/passwd"}]}]}))
    assert len(published) == 2


@pytest.mark.parametrize("status", [SubagentStatus.COMPLETED, SubagentStatus.LIMIT_REACHED, SubagentStatus.FAILED, SubagentStatus.CANCELLED, SubagentStatus.TIMED_OUT, SubagentStatus.RUNNING])
@pytest.mark.asyncio
async def test_task_poller_preserves_sources_and_incomplete_outcomes(evidence, monkeypatch, status):
    module = importlib.import_module("deerflow.tools.builtins.task_tool")
    config = SubagentConfig(name="worker", description="Worker", system_prompt="Work", timeout_seconds=1)
    holder = SubagentResult(task_id="execution", trace_id="trace", status=status, result="Useful finding", error="Stopped", knowledge_sources=get_sources(evidence.artifact))
    executor = SimpleNamespace(execute_async=lambda *_args, **_kwargs: "execution")
    cleanup = Mock()
    finalizer = Mock()
    monkeypatch.setattr(module, "get_available_subagent_names", lambda: ["worker"])
    monkeypatch.setattr(module, "get_subagent_config", lambda _name: config)
    monkeypatch.setattr(module, "SubagentExecutor", lambda **_kwargs: executor)
    monkeypatch.setattr(module, "get_background_task_result", lambda _id: holder)
    monkeypatch.setattr(module, "cleanup_background_task", cleanup)
    monkeypatch.setattr(module, "finalize_cancelled_background_task", finalizer)
    monkeypatch.setattr("deerflow.tools.get_available_tools", lambda **_kwargs: [])

    async def assemble(function, *args, **kwargs):
        return function(*args, **kwargs)

    async def no_delay(_seconds):
        return None

    monkeypatch.setattr("deerflow.runtime.assembly.run_in_assembly_executor", assemble)
    monkeypatch.setattr(module.asyncio, "sleep", no_delay)
    runtime = SimpleNamespace(state={}, context={"thread_id": "thread"}, config={"metadata": {}})
    result = await module._task_tool_impl(runtime, "Read evidence", "Investigate", "worker", "task-call")
    assert isinstance(result, ToolMessage)
    assert get_sources(result.artifact) == get_sources(evidence.artifact)
    incomplete = status is not SubagentStatus.COMPLETED
    assert result.additional_kwargs["incomplete"] is incomplete
    assert result.status == ("error" if incomplete else "success")
    assert ("Task Succeeded" in result.content) is not incomplete
    assert holder.status is status
    wire = DeerFlowClient._tool_message_event(result)
    assert wire.data["knowledge_sources"] == get_sources(evidence.artifact)
    assert wire.data["status"] == result.status
    assert any(event.get("name") == "deerflow.knowledge_sources" for event in _agui(wire))
    cleanup.assert_called_once_with("execution")
    if status is SubagentStatus.RUNNING:
        assert "polling timed out" in result.content
        finalizer.assert_called_once()
    else:
        finalizer.assert_not_called()


def test_omitting_all_sources_returns_reserved_budget_to_summary():
    artifact = source_artifact([{"id": "a" * 32, "excerpt": "evidence" * 1000}])
    summary = "Detailed finding. " * 7 + "Keep the final conclusion."
    artifact["summary"] = summary
    result, remaining = budget_sources("", artifact, 240)
    assert summary in result
    assert len(result) <= 240 and not get_sources(remaining)


@pytest.mark.asyncio
async def test_escaped_api_key_echo_is_redacted_before_snapshot_json(monkeypatch, tmp_path):
    secret = 'secret"with\\escapes'
    config = ToolConfig(name="knowledge_search", group="knowledge", use="test:tool", datasets=["approved"], api_key=secret)

    class Provider:
        async def list_datasets(self, **_kwargs):
            return [{"id": "approved", "name": "Dataset " + secret, "embedding_model": "embed", "chunk_count": 1}]

        async def retrieve(self, *_args, **_kwargs):
            return {"chunks": [{"dataset_id": "approved", "document_id": "doc", "id": "chunk", "document_keyword": "Document " + secret, "content": "Evidence " + secret}]}

    monkeypatch.setattr(rag_tools, "get_app_config", lambda: SimpleNamespace(get_tool_config=lambda _: config))
    monkeypatch.setattr(rag_tools, "_build_client", lambda _: Provider())
    runtime = SimpleNamespace(context={}, config={}, state={"thread_data": {"outputs_path": str(tmp_path)}})
    text, artifact = await rag_tools._search_result("question", runtime=runtime, citations=True)
    assert get_sources(artifact)
    saved = next(tmp_path.glob("knowledge-*.md")).read_text(encoding="utf-8")
    for output in (text, json.dumps(artifact), saved):
        assert secret not in output
        assert json.dumps(secret)[1:-1] not in output
    assert "[REDACTED]" in saved
