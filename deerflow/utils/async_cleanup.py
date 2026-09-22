"""Helpers for async cleanup whose lifetime belongs to the calling operation."""

from __future__ import annotations

import asyncio
from collections.abc import Coroutine
from typing import Any, TypeVar

_T = TypeVar("_T")


async def await_drained(operation: Coroutine[Any, Any, _T]) -> _T:
    """Finish owned cleanup before propagating caller cancellation.

    Repeated cancellation never cancels or detaches the cleanup task. The
    operation must bound its own work where necessary; this helper adds no
    timeout. Do not use it to close a context that must exit on its entry task.
    """
    task = asyncio.create_task(operation)
    cancellation: asyncio.CancelledError | None = None
    while not task.done():
        try:
            # Unlike awaiting the task directly, wait neither forwards caller
            # cancellation nor raises a cleanup failure ahead of that cancel.
            await asyncio.wait({task})
        except asyncio.CancelledError as exc:
            if cancellation is None:
                cancellation = exc

    if cancellation is not None:
        try:
            task.result()
        except (asyncio.CancelledError, Exception) as exc:
            raise cancellation from exc
        raise cancellation
    return task.result()
