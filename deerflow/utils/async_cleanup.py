"""Helpers for async cleanup whose lifetime belongs to the calling operation."""

from __future__ import annotations

import asyncio
from collections.abc import AsyncIterable, AsyncIterator, Coroutine
from contextlib import asynccontextmanager
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


class _OwnedAsyncIterator(AsyncIterator[_T]):
    """Drive every iterator step and its close on one persistent task."""

    def __init__(self, source: AsyncIterable[_T]) -> None:
        self._iterator = aiter(source)
        self._requested = asyncio.Event()
        self._reply: asyncio.Future[tuple[str, Any]] | None = None
        self._source_error: BaseException | None = None
        self._error_delivered = False
        self._started = False
        self._finalizing = False
        self._exhausted = False
        self._close_requested = False
        self._driver = asyncio.create_task(self._drive())

    async def _drive(self) -> None:
        self._started = True
        try:
            while True:
                await self._requested.wait()
                self._requested.clear()
                reply = self._reply
                assert reply is not None
                try:
                    value = await anext(self._iterator)
                except StopAsyncIteration:
                    self._exhausted = True
                    reply.set_result(("stop", None))
                    return
                except BaseException as exc:
                    self._exhausted = True
                    self._source_error = exc
                    reply.set_result(("error", exc))
                    return
                reply.set_result(("value", value))
        finally:
            self._finalizing = True
            close = getattr(self._iterator, "aclose", None)
            if close is not None:
                await close()

    async def __anext__(self) -> _T:
        if self._exhausted or self._close_requested:
            raise StopAsyncIteration
        if self._reply is not None and not self._reply.done():
            raise RuntimeError("Concurrent reads from an owned stream are unsupported")
        self._reply = asyncio.get_running_loop().create_future()
        self._requested.set()
        try:
            kind, value = await asyncio.shield(self._reply)
        except asyncio.CancelledError as cancellation:
            try:
                await self.aclose()
            except BaseException as cleanup_error:
                raise cancellation from cleanup_error
            raise
        if kind == "stop":
            raise StopAsyncIteration
        if kind == "error":
            self._error_delivered = True
            raise value
        return value

    async def aclose(self) -> None:
        if not self._close_requested:
            self._close_requested = True
            if not self._driver.done() and not self._finalizing:
                # Forward cancellation once. Repeated caller cancellation must
                # not interrupt an iterator/LangGraph context already exiting.
                self._driver.cancel()

        async def finish() -> None:
            try:
                await self._driver
            except asyncio.CancelledError:
                pass
            if self._source_error is not None and not self._error_delivered:
                self._error_delivered = True
                if not isinstance(self._source_error, asyncio.CancelledError):
                    # Cancellation may win the race with an error reply. Keep
                    # source cleanup failures attached to the caller's cancel.
                    raise self._source_error
            if not self._started:
                close = getattr(self._iterator, "aclose", None)
                if close is not None:
                    await close()

        await await_drained(finish())


@asynccontextmanager
async def owned_async_iterator(source: AsyncIterable[_T]) -> AsyncIterator[AsyncIterator[_T]]:
    """Keep stream cleanup owned until it drains, including consumer-body cancel.

    The driver holds at most one requested item and never reads ahead. A single
    task enters, advances and exits the source, preserving task-bound contexts.
    Consumers must use this context around the entire ``async for`` body.
    """
    stream = _OwnedAsyncIterator(source)
    try:
        yield stream
    except BaseException as original:
        try:
            await stream.aclose()
        except BaseException as cleanup_error:
            raise original from cleanup_error
        raise
    else:
        await stream.aclose()
