import asyncio
import threading
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

from app.dependencies import ClientManager


@pytest.fixture
async def shutdown_context(monkeypatch):
    events = []
    manager = ClientManager()
    manager.stop_feishu_channel = AsyncMock()
    monkeypatch.setattr(
        "deerflow.mcp.session_pool.get_session_pool",
        lambda: SimpleNamespace(close_all=AsyncMock()),
    )
    monkeypatch.setattr("deerflow.mcp.session_pool.reset_session_pool", lambda: None)
    monkeypatch.setattr(
        "deerflow.sandbox.sandbox_provider.shutdown_sandbox_provider",
        lambda: events.append("sandbox_closed"),
    )
    monkeypatch.setattr(
        "deerflow.config.memory_config.get_memory_config",
        lambda: SimpleNamespace(enabled=True, shutdown_flush_timeout_seconds=1),
    )

    class Context:
        async def __aexit__(self, *_args):
            events.append("checkpoint_closed")

    manager._async_checkpointer_cm = Context()
    manager._service_loop = asyncio.get_running_loop()
    return manager, events


@pytest.mark.parametrize("cancel_phase", ["flush", "close"])
async def test_shutdown_finishes_memory_and_remaining_resources_after_repeated_cancel(
    monkeypatch, shutdown_context, cancel_phase
):
    manager, events = shutdown_context
    entered = threading.Event()
    finish = threading.Event()

    def pause():
        entered.set()
        assert finish.wait(5), "test did not release fake memory worker"

    def flush():
        if cancel_phase == "flush":
            pause()
        events.append("memory_flushed")
        return True

    def close():
        if cancel_phase == "close":
            pause()
        events.append("memory_closed")

    monkeypatch.setattr("deerflow.agents.memory.get_memory_manager", lambda: SimpleNamespace(shutdown_flush=flush))
    monkeypatch.setattr("deerflow.agents.memory.reset_memory_manager", close)
    task = asyncio.create_task(manager.shutdown())
    try:
        assert await asyncio.to_thread(entered.wait, 2)
        for reason in ("first", "second"):
            task.cancel(reason)
            await asyncio.sleep(0)
            assert not task.done()
        assert "memory_closed" not in events
        assert "checkpoint_closed" not in events
    finally:
        finish.set()
    with pytest.raises(asyncio.CancelledError, match="first"):
        await task
    assert events == ["memory_flushed", "memory_closed", "sandbox_closed", "checkpoint_closed"]
    assert manager._service_loop is None


async def test_flush_timeout_leaves_busy_backend_open_and_cleans_other_resources(monkeypatch, shutdown_context):
    manager, events = shutdown_context
    entered = threading.Event()
    finish = threading.Event()
    exited = threading.Event()

    def flush():
        entered.set()
        try:
            assert finish.wait(5)
            return True
        finally:
            exited.set()

    monkeypatch.setattr(
        "deerflow.config.memory_config.get_memory_config",
        lambda: SimpleNamespace(enabled=True, shutdown_flush_timeout_seconds=0),
    )
    monkeypatch.setattr("deerflow.agents.memory.get_memory_manager", lambda: SimpleNamespace(shutdown_flush=flush))
    monkeypatch.setattr("deerflow.agents.memory.reset_memory_manager", lambda: events.append("memory_closed"))
    try:
        await asyncio.wait_for(manager.shutdown(), 2)
        assert entered.is_set()
        assert not exited.is_set()
        assert events == ["sandbox_closed", "checkpoint_closed"]
    finally:
        finish.set()
        assert await asyncio.to_thread(exited.wait, 2)


async def test_memory_config_failure_does_not_skip_remaining_teardown(monkeypatch, shutdown_context):
    manager, events = shutdown_context

    def broken_config():
        raise ValueError("invalid config")

    monkeypatch.setattr("deerflow.config.memory_config.get_memory_config", broken_config)
    await manager.shutdown()
    assert events == ["sandbox_closed", "checkpoint_closed"]


async def test_incomplete_memory_flush_leaves_backend_open(monkeypatch, shutdown_context):
    manager, events = shutdown_context
    monkeypatch.setattr("deerflow.agents.memory.get_memory_manager", lambda: SimpleNamespace(shutdown_flush=lambda: False))
    monkeypatch.setattr("deerflow.agents.memory.reset_memory_manager", lambda: events.append("memory_closed"))
    await manager.shutdown()
    assert events == ["sandbox_closed", "checkpoint_closed"]
