import asyncio

import pytest
from langchain_core.messages import AIMessage, ToolMessage

import deerflow.subagents.executor as executor_module
from deerflow.subagents.config import SubagentConfig
from deerflow.subagents.executor import SubagentExecutor, SubagentResult, SubagentStatus


def _executor(monkeypatch, stream, events, **kwargs):
    class Agent:
        def astream(self, *_args, **_kwargs):
            return stream

    executor = SubagentExecutor(
        SubagentConfig(name="worker", description="worker", system_prompt="work", skills=[]),
        tools=[],
        **kwargs,
    )

    async def initial(_task):
        return {"messages": []}

    async def close_model(_model):
        events.append("model_closed")

    monkeypatch.setattr(executor, "_create_agent", lambda stream_callback=None: (Agent(), object()))
    monkeypatch.setattr(executor, "_build_initial_state", initial)
    monkeypatch.setattr(executor_module, "aclose_chat_model", close_model)
    return executor


async def test_cooperative_cancel_closes_stream_on_owner_task_before_model(monkeypatch):
    events = []
    holder = SubagentResult(task_id="cancel", trace_id="trace", status=SubagentStatus.RUNNING)
    closing = asyncio.Event()
    finish = asyncio.Event()
    consuming_task = None

    async def chunks():
        nonlocal consuming_task
        consuming_task = asyncio.current_task()
        try:
            holder.cancel_event.set()
            yield "values", {"messages": []}
        finally:
            assert asyncio.current_task() is consuming_task
            closing.set()
            await finish.wait()
            events.append("stream_closed")

    stream = chunks()
    executor = _executor(monkeypatch, stream, events)
    task = asyncio.create_task(executor._aexecute("work", holder))
    await asyncio.wait_for(closing.wait(), 2)
    try:
        assert holder.status is SubagentStatus.CANCELLED
        assert not task.done()
        assert not events
    finally:
        finish.set()
    assert await task is holder
    assert events == ["stream_closed", "model_closed"]
    assert stream.ag_frame is None


@pytest.mark.parametrize("stream_failure,close_failure", [(False, False), (True, False), (True, True), (False, True)])
async def test_stream_cleanup_on_completion_or_error(monkeypatch, stream_failure, close_failure):
    events = []

    class Stream:
        def __init__(self):
            self.yielded = False

        def __aiter__(self):
            return self

        async def __anext__(self):
            if stream_failure:
                raise ValueError("stream failed")
            if self.yielded:
                raise StopAsyncIteration
            self.yielded = True
            return "values", {"messages": [AIMessage(content="done")]}

        async def aclose(self):
            events.append("stream_closed")
            if close_failure:
                raise RuntimeError("close failed")

    executor = _executor(monkeypatch, Stream(), events)
    result = await executor._aexecute("work")
    assert events == ["stream_closed", "model_closed"]
    if stream_failure or close_failure:
        assert result.status is SubagentStatus.FAILED
        assert result.error == ("stream failed" if stream_failure else "close failed")
    else:
        assert result.status is SubagentStatus.COMPLETED
        assert result.result == "done"


async def test_async_caller_cancellation_closes_stream_before_model(monkeypatch):
    entered = asyncio.Event()
    events = []

    class Stream:
        def __aiter__(self):
            return self

        async def __anext__(self):
            entered.set()
            await asyncio.Event().wait()

        async def aclose(self):
            await asyncio.sleep(0)
            events.append("stream_closed")

    executor = _executor(monkeypatch, Stream(), events)
    task = asyncio.create_task(executor._aexecute("work"))
    await entered.wait()
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    assert events == ["stream_closed", "model_closed"]


async def test_knowledge_scope_is_detached_from_parent_and_between_runtime_channels(monkeypatch):
    scope = {"dataset_ids": ["allowed"]}
    observed = {}

    async def chunks():
        yield "values", {"messages": [AIMessage(content="done")]}

    stream = chunks()
    executor = _executor(monkeypatch, stream, [], knowledge_scope=scope)
    scope["dataset_ids"].append("parent-late")

    class Agent:
        def astream(self, *_args, context, config, **_kwargs):
            observed["context"] = context["knowledge_scope"]
            observed["metadata"] = config["metadata"]["knowledge_scope"]
            context["knowledge_scope"]["dataset_ids"].append("child-local")
            return stream

    monkeypatch.setattr(executor, "_create_agent", lambda stream_callback=None: (Agent(), object()))
    await executor._aexecute("work")
    assert observed["metadata"] == {"dataset_ids": ["allowed"]}
    assert observed["context"] == {"dataset_ids": ["allowed", "child-local"]}
    assert executor.knowledge_scope == {"dataset_ids": ["allowed"]}


@pytest.mark.parametrize("cancel", [False, True])
async def test_knowledge_sources_survive_as_executed_snapshot_without_changing_cancel_status(monkeypatch, cancel):
    source = {"id": "a" * 32, "document_name": "guide", "page": [1]}
    holder = SubagentResult(task_id="sources", trace_id="trace", status=SubagentStatus.RUNNING)
    artifact = {"type": "deerflow.knowledge_sources", "sources": [source, source, {}, "bad"]}
    message = ToolMessage(content="visible result", tool_call_id="call", artifact=artifact)

    async def chunks():
        if cancel:
            holder.cancel_event.set()
        yield "values", {"messages": [message, AIMessage(content="done")]}

    executor = _executor(monkeypatch, chunks(), [])
    result = await executor._aexecute("work", holder)
    expected = SubagentStatus.CANCELLED if cancel else SubagentStatus.COMPLETED
    assert result.status is expected
    assert result.knowledge_sources == [source]
    source["page"].append(2)
    result.update_knowledge_sources([{"id": "late"}])
    assert result.knowledge_sources == [{"id": "a" * 32, "document_name": "guide", "page": [1]}]


async def test_sources_removed_from_executed_state_are_not_published(monkeypatch):
    artifact = {"type": "deerflow.knowledge_sources", "sources": [{"id": "a" * 32}]}

    async def chunks():
        yield "values", {"messages": [ToolMessage(content="result", tool_call_id="call", artifact=artifact)]}
        yield "values", {"messages": [AIMessage(content="condensed answer")]}

    executor = _executor(monkeypatch, chunks(), [])
    result = await executor._aexecute("work")
    assert result.knowledge_sources == []
