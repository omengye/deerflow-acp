from __future__ import annotations

import asyncio
import contextvars
import threading

import pytest

from deerflow.runtime.assembly import assembly_snapshot, run_in_assembly_executor


async def test_assembly_executor_propagates_context_and_uses_dedicated_thread() -> None:
    marker = contextvars.ContextVar("assembly-marker", default="missing")
    marker.set("visible")

    value, thread_name = await run_in_assembly_executor(
        lambda: (marker.get(), threading.current_thread().name)
    )

    assert value == "visible"
    assert thread_name.startswith("deerflow-assembly")


async def test_cancelled_awaiter_does_not_leak_assembly_accounting() -> None:
    started = threading.Event()
    release = threading.Event()

    def blocking_assembly() -> None:
        started.set()
        release.wait(timeout=5)

    task = asyncio.create_task(run_in_assembly_executor(blocking_assembly))
    assert await asyncio.to_thread(started.wait, 2)
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task

    # Cancellation does not pretend the worker stopped or release its slot.
    assert assembly_snapshot()["active"] >= 1
    release.set()
    for _ in range(200):
        if assembly_snapshot()["active"] == 0:
            break
        await asyncio.sleep(0.01)
    assert assembly_snapshot()["active"] == 0
    assert assembly_snapshot()["pending"] == 0
