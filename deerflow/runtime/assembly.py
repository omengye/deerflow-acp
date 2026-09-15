"""Bounded off-loop execution for synchronous agent/tool assembly."""

from __future__ import annotations

import asyncio
import contextvars
import logging
import os
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from typing import Any, Callable, TypeVar

logger = logging.getLogger(__name__)

_T = TypeVar("_T")
_ASSEMBLY_WORKERS = max(2, min(4, os.cpu_count() or 2))
_ASSEMBLY_CAPACITY = _ASSEMBLY_WORKERS * 4
_ASSEMBLY_EXECUTOR = ThreadPoolExecutor(
    max_workers=_ASSEMBLY_WORKERS,
    thread_name_prefix="deerflow-assembly",
)
_ASSEMBLY_SLOTS = threading.BoundedSemaphore(_ASSEMBLY_CAPACITY)
_ASSEMBLY_STATE_LOCK = threading.Lock()
_ASSEMBLY_PENDING = 0
_ASSEMBLY_ACTIVE = 0
_STARVATION_LOG_SECONDS = 0.25


def assembly_snapshot() -> dict[str, int]:
    """Return lightweight queue metrics for diagnostics and tests."""
    with _ASSEMBLY_STATE_LOCK:
        return {
            "workers": _ASSEMBLY_WORKERS,
            "capacity": _ASSEMBLY_CAPACITY,
            "pending": _ASSEMBLY_PENDING,
            "active": _ASSEMBLY_ACTIVE,
        }


async def _acquire_slot() -> float:
    started = time.monotonic()
    logged = False
    while not _ASSEMBLY_SLOTS.acquire(blocking=False):
        waited = time.monotonic() - started
        if not logged and waited >= _STARVATION_LOG_SECONDS:
            snapshot = assembly_snapshot()
            logger.warning(
                "Agent/tool assembly executor is saturated "
                "(pending=%d active=%d capacity=%d)",
                snapshot["pending"],
                snapshot["active"],
                snapshot["capacity"],
            )
            logged = True
        await asyncio.sleep(0.01)
    return time.monotonic() - started


async def run_in_assembly_executor(
    func: Callable[..., _T],
    /,
    *args: Any,
    **kwargs: Any,
) -> _T:
    """Run blocking assembly without stalling the caller's event loop.

    Capacity is reserved before submission and released by the worker itself,
    so cancelling an awaiter cannot leak a slot or create an unbounded executor
    queue.  The caller's context variables are copied into the worker.
    """

    global _ASSEMBLY_PENDING, _ASSEMBLY_ACTIVE

    waited = await _acquire_slot()
    if waited >= _STARVATION_LOG_SECONDS:
        logger.info("Agent/tool assembly acquired a slot after %.3fs", waited)

    context = contextvars.copy_context()
    with _ASSEMBLY_STATE_LOCK:
        _ASSEMBLY_PENDING += 1

    def invoke() -> _T:
        global _ASSEMBLY_PENDING, _ASSEMBLY_ACTIVE
        with _ASSEMBLY_STATE_LOCK:
            _ASSEMBLY_PENDING -= 1
            _ASSEMBLY_ACTIVE += 1
        try:
            return func(*args, **kwargs)
        finally:
            with _ASSEMBLY_STATE_LOCK:
                _ASSEMBLY_ACTIVE -= 1
            _ASSEMBLY_SLOTS.release()

    try:
        future = asyncio.get_running_loop().run_in_executor(
            _ASSEMBLY_EXECUTOR,
            context.run,
            invoke,
        )
    except BaseException:
        with _ASSEMBLY_STATE_LOCK:
            _ASSEMBLY_PENDING -= 1
        _ASSEMBLY_SLOTS.release()
        raise

    # Shield leaves queued/running assembly alive when a request is cancelled.
    # The worker's finally block owns accounting and releases capacity.
    return await asyncio.shield(future)
