"""Stream ownership across consumer callbacks, graph tasks, and workspace guards."""
from __future__ import annotations

import asyncio
import threading
from types import SimpleNamespace

import pytest
from langchain.tools import ToolRuntime
from langgraph.config import get_stream_writer
from langgraph.graph import END, START, StateGraph

from app.dependencies import ClientManager
from deerflow.client import DeerFlowClient, StreamEvent
from deerflow.sandbox.command import CommandResult
from deerflow.sandbox.tools import bash_tool
from deerflow.utils.async_cleanup import owned_async_iterator


async def test_owned_iterator_keeps_one_task_context_and_does_not_read_ahead():
    tasks = []
    steps = []

    async def source():
        tasks.append(asyncio.current_task())
        try:
            for item in range(3):
                steps.append(item)
                tasks.append(asyncio.current_task())
                yield item
        finally:
            tasks.append(asyncio.current_task())

    async with owned_async_iterator(source()) as stream:
        assert await anext(stream) == 0
        await asyncio.sleep(0)
        assert steps == [0]
        assert [value async for value in stream] == [1, 2]
    assert len(set(tasks)) == 1
    assert tasks[0] is not asyncio.current_task()


@pytest.mark.parametrize("source_error", [False, True])
async def test_owned_iterator_does_not_interrupt_already_started_close(source_error):
    closing, finish = asyncio.Event(), asyncio.Event()
    closed = []

    class Source:
        def __aiter__(self):
            return self

        async def __anext__(self):
            if source_error:
                raise ValueError("source failed")
            raise StopAsyncIteration

        async def aclose(self):
            closing.set()
            await finish.wait()
            closed.append(True)

    async def consume():
        async with owned_async_iterator(Source()) as stream:
            return [item async for item in stream]

    consumer = asyncio.create_task(consume())
    await closing.wait()
    await asyncio.sleep(0.01)
    assert not consumer.done()
    finish.set()
    if source_error:
        with pytest.raises(ValueError, match="source failed"):
            await consumer
    else:
        assert await consumer == []
    assert closed == [True]


async def test_early_break_closes_source_before_context_returns():
    closed = []

    async def source():
        try:
            yield 1
            pytest.fail("Early break must not consume another item")
        finally:
            closed.append(True)

    async with owned_async_iterator(source()) as stream:
        async for _ in stream:
            break
    assert closed == [True]


async def test_cancelled_read_preserves_unconsumed_source_cleanup_failure():
    started = asyncio.Event()

    async def source():
        try:
            started.set()
            await asyncio.Event().wait()
            yield 1
        finally:
            raise RuntimeError("source cleanup failed")

    async def consume():
        async with owned_async_iterator(source()) as stream:
            await anext(stream)

    consumer = asyncio.create_task(consume())
    await started.wait()
    consumer.cancel("original cancellation")
    with pytest.raises(asyncio.CancelledError, match="original cancellation") as error:
        await consumer
    assert isinstance(error.value.__cause__, RuntimeError)
    assert str(error.value.__cause__) == "source cleanup failed"


@pytest.mark.parametrize("publish_boundary", [False, True])
@pytest.mark.parametrize("repeat_cancel", [False, True])
async def test_chat_cancellation_keeps_workspace_guard_until_real_graph_tool_drains(monkeypatch, publish_boundary, repeat_cancel):
    started, cancelling, finish, completed = (threading.Event() for _ in range(4))
    publishing = asyncio.Event()
    node_waiting = asyncio.Event()

    def execute(command, *, cancel_event):
        started.set()
        assert cancel_event.wait(5)
        cancelling.set()
        assert finish.wait(5)
        completed.set()
        return CommandResult("cancelled", None, "cancelled", True)

    runtime = ToolRuntime(state={}, context={}, config={}, stream_writer=lambda _: None,
                          tool_call_id="cmd", store=None)
    call = {"name": "bash", "id": "cmd", "type": "tool_call", "args": {
        "runtime": runtime, "description": "audit", "command": "fake command",
    }}

    async def node(state):
        invocation = asyncio.create_task(bash_tool.ainvoke(call))
        while not started.is_set():
            await asyncio.sleep(0.001)
        if publish_boundary:
            get_stream_writer()({"type": "audit-running"})
        node_waiting.set()
        await invocation
        return state

    graph = StateGraph(dict)
    graph.add_node("work", node)
    graph.add_edge(START, "work")
    graph.add_edge("work", END)
    client = object.__new__(DeerFlowClient)
    client._agent = graph.compile()
    client._agent_name = "audit"
    thread_id = f"guard-audit-{publish_boundary}-{repeat_cancel}"
    client._prepare_stream_invocation = lambda *args, **kwargs: ({"configurable": {"thread_id": thread_id}}, {}, {})

    async def noop(*args, **kwargs):
        pass

    client._ensure_checkpoint_compatible = noop
    client._seed_seen_ids_from_checkpoint = noop
    client._events_from_stream_item = lambda item, state: [StreamEvent("custom", item[-1])] if item[-2] == "custom" else []
    client._end_event = lambda *args, **kwargs: StreamEvent("end", {})

    class Bridge:
        async def publish(self, run_id, event_type, data):
            if event_type == "custom" and data.get("type") == "audit-running":
                publishing.set()
                await asyncio.Event().wait()

        publish_end = noop

    manager = object.__new__(ClientManager)
    manager.run_manager = SimpleNamespace(set_status=noop)
    manager.stream_bridge = Bridge()
    manager._thread_lock = threading.Lock()
    manager._running_threads = {thread_id}
    manager._running_thread_counts = {thread_id: 1}
    manager._schedule_completed_run_retention = lambda _: None

    async def get_client(**kwargs):
        return client

    manager.get_async_client = get_client
    record = SimpleNamespace(run_id="run-audit", thread_id=thread_id, metadata={}, abort_event=asyncio.Event())
    config = SimpleNamespace(sandbox=SimpleNamespace(bash_output_max_chars=1000))
    monkeypatch.setattr("deerflow.sandbox.tools.ensure_sandbox_initialized", lambda _: SimpleNamespace(execute_command_result=execute))
    monkeypatch.setattr("deerflow.sandbox.tools.is_local_sandbox", lambda _: False)
    monkeypatch.setattr("deerflow.sandbox.tools.ensure_thread_directories_exist", lambda _: None)
    monkeypatch.setattr("deerflow.sandbox.tools.get_app_config", lambda: config)
    producer = asyncio.create_task(manager._produce_client_stream(record=record, message="audit", kwargs={}, request_id=None))
    try:
        if publish_boundary:
            await asyncio.wait_for(publishing.wait(), 3)
        else:
            await asyncio.wait_for(node_waiting.wait(), 3)
        producer.cancel("first cancellation")
        assert await asyncio.to_thread(cancelling.wait, 3)
        if repeat_cancel:
            producer.cancel("second cancellation")
            await asyncio.sleep(0.01)
        assert not producer.done()
        assert manager.is_thread_running(thread_id)
        assert not completed.is_set()
    finally:
        finish.set()
    with pytest.raises(asyncio.CancelledError, match="first cancellation"):
        await asyncio.wait_for(producer, 3)
    assert completed.is_set()
    assert not manager.is_thread_running(thread_id)
