"""Persistent MCP sessions owned by one long-lived event-loop thread.

The agent runtime exposes both asynchronous tools and synchronous wrappers.
Those wrappers use short-lived ``asyncio.run`` loops, while MCP transports and
their anyio cancel scopes must be entered and exited on a stable owner task.
Returning a raw ``ClientSession`` to arbitrary caller loops therefore cannot
provide persistent state safely.

This pool gives each ``(server_name, scope_key)`` one actor.  The actor enters
the adapter context, initializes the session, executes every tool call, and
exits the context in the same task.  Callers receive a small proxy that can be
awaited from any event loop; requests are marshalled to the owner thread.
"""

from __future__ import annotations

import asyncio
import concurrent.futures
import logging
import threading
from collections import OrderedDict
from dataclasses import dataclass, field
from typing import Any, Coroutine

logger = logging.getLogger(__name__)


@dataclass
class _CreationGuard:
    """Cross-event-loop mutex for one persistent-session key."""

    lock: threading.Lock = field(default_factory=threading.Lock)
    users: int = 0


@dataclass
class _ActorRequest:
    name: str
    arguments: dict[str, Any]
    response: asyncio.Future[Any]


class _SessionActor:
    """Own one adapter context and raw ClientSession in a single Task."""

    def __init__(self, connection: dict[str, Any]) -> None:
        self.connection = connection
        self.ready: asyncio.Future[None] | None = None
        self.queue: asyncio.Queue[_ActorRequest | None] | None = None
        self.task: asyncio.Task[None] | None = None
        self._closing = False

    async def start(self) -> None:
        loop = asyncio.get_running_loop()
        self.ready = loop.create_future()
        self.queue = asyncio.Queue()
        self.task = asyncio.create_task(self._run(), name="mcp-session-owner")
        try:
            await asyncio.shield(self.ready)
        except BaseException:
            # Initialization timeout/cancellation must unwind the adapter
            # context in the actor task that entered it.
            self.task.cancel()
            await asyncio.gather(self.task, return_exceptions=True)
            raise

    async def _run(self) -> None:
        from langchain_mcp_adapters.sessions import create_session

        assert self.ready is not None
        assert self.queue is not None
        cm = create_session(self.connection)
        entered = False
        pending: _ActorRequest | None = None
        try:
            session = await cm.__aenter__()
            entered = True
            await session.initialize()
            if not self.ready.done():
                self.ready.set_result(None)

            while True:
                item = await self.queue.get()
                if item is None:
                    break
                pending = item
                try:
                    result = await session.call_tool(item.name, item.arguments)
                except BaseException as exc:
                    if not item.response.done():
                        item.response.set_exception(exc)
                    if isinstance(exc, asyncio.CancelledError):
                        raise
                else:
                    if not item.response.done():
                        item.response.set_result(result)
                finally:
                    pending = None
        except BaseException as exc:
            if not self.ready.done():
                self.ready.set_exception(exc)
            if pending is not None and not pending.response.done():
                pending.response.set_exception(exc)
            self._fail_queued_requests(exc)
            if isinstance(exc, asyncio.CancelledError):
                raise
        finally:
            if entered:
                try:
                    await cm.__aexit__(None, None, None)
                except BaseException:
                    logger.warning("Error closing owned MCP session", exc_info=True)

    def _fail_queued_requests(self, error: BaseException) -> None:
        if self.queue is None:
            return
        while True:
            try:
                item = self.queue.get_nowait()
            except asyncio.QueueEmpty:
                return
            if item is not None and not item.response.done():
                item.response.set_exception(error)

    async def call_tool(self, name: str, arguments: dict[str, Any]) -> Any:
        if self.task is None or self.task.done() or self.queue is None:
            raise RuntimeError("MCP session owner is no longer running")
        loop = asyncio.get_running_loop()
        response: asyncio.Future[Any] = loop.create_future()
        await self.queue.put(_ActorRequest(name=name, arguments=arguments, response=response))
        # Cancelling one caller must not cancel the shared actor or poison the
        # session for later calls. The in-flight MCP request completes on the
        # owner and its result is simply discarded by that caller.
        return await asyncio.shield(response)

    async def close(self) -> None:
        if self.task is None:
            return
        if not self.task.done() and not self._closing:
            self._closing = True
            assert self.queue is not None
            await self.queue.put(None)
        await asyncio.gather(self.task, return_exceptions=True)


class _SessionOwnerLoop:
    """Lazily started event-loop thread shared by every actor in one pool."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._loop: asyncio.AbstractEventLoop | None = None
        self._thread: threading.Thread | None = None
        self._ready: threading.Event | None = None

    def _ensure_started(self) -> asyncio.AbstractEventLoop:
        with self._lock:
            if self._loop is not None and self._thread is not None and self._thread.is_alive():
                return self._loop
            ready = threading.Event()
            self._ready = ready

            def run() -> None:
                loop = asyncio.new_event_loop()
                asyncio.set_event_loop(loop)
                with self._lock:
                    self._loop = loop
                ready.set()
                try:
                    loop.run_forever()
                finally:
                    pending = asyncio.all_tasks(loop)
                    for task in pending:
                        task.cancel()
                    if pending:
                        loop.run_until_complete(asyncio.gather(*pending, return_exceptions=True))
                    loop.close()

            self._thread = threading.Thread(
                target=run,
                name="mcp-session-owner-loop",
                daemon=True,
            )
            self._thread.start()
        ready.wait()
        with self._lock:
            assert self._loop is not None
            return self._loop

    def submit(self, coro: Coroutine[Any, Any, Any]) -> "_OwnerOperation":
        loop = self._ensure_started()
        operation = _OwnerOperation(loop, coro)
        operation.start()
        return operation

    async def await_submit(self, coro: Coroutine[Any, Any, Any]) -> Any:
        operation = self.submit(coro)
        wrapped = asyncio.wrap_future(operation.completion)
        try:
            # Shield prevents the caller loop from marking the bridge Future
            # cancelled before the owner coroutine has unwound its adapter
            # context. We propagate cancellation explicitly below and wait for
            # the owner-side finally block to finish.
            return await asyncio.shield(wrapped)
        except asyncio.CancelledError:
            operation.cancel()
            try:
                await asyncio.shield(wrapped)
            except BaseException:
                pass
            raise

    def stop(self) -> None:
        with self._lock:
            loop = self._loop
            thread = self._thread
        if loop is None or thread is None:
            return
        if loop.is_running():
            loop.call_soon_threadsafe(loop.stop)
        if thread is not threading.current_thread():
            thread.join(timeout=5.0)
        with self._lock:
            if self._thread is thread:
                self._thread = None
                self._loop = None
                self._ready = None


class _OwnerOperation:
    """Cross-thread operation whose completion follows the actual owner Task.

    ``run_coroutine_threadsafe().cancel()`` marks its public Future cancelled
    before the event-loop Task has executed ``finally``.  MCP initialization
    needs the stronger guarantee that adapter cleanup has completed before a
    timeout is reported, so cancellation and completion are kept separate.
    """

    def __init__(
        self,
        loop: asyncio.AbstractEventLoop,
        coro: Coroutine[Any, Any, Any],
    ) -> None:
        self.loop = loop
        self.coro = coro
        self.completion: concurrent.futures.Future[Any] = concurrent.futures.Future()
        self._lock = threading.Lock()
        self._task: asyncio.Task[Any] | None = None
        self._cancel_requested = False

    def start(self) -> None:
        self.loop.call_soon_threadsafe(self._start_on_owner)

    def _start_on_owner(self) -> None:
        task = self.loop.create_task(self.coro)
        with self._lock:
            self._task = task
            cancel_requested = self._cancel_requested
        task.add_done_callback(self._done_on_owner)
        if cancel_requested:
            task.cancel()

    def _done_on_owner(self, task: asyncio.Task[Any]) -> None:
        try:
            result = task.result()
        except BaseException as exc:
            if not self.completion.done():
                self.completion.set_exception(exc)
        else:
            if not self.completion.done():
                self.completion.set_result(result)

    def cancel(self) -> None:
        with self._lock:
            self._cancel_requested = True
            task = self._task
        if task is not None:
            self.loop.call_soon_threadsafe(task.cancel)

    def result(self, timeout: float | None = None) -> Any:
        return self.completion.result(timeout=timeout)


class OwnedMCPSession:
    """Cross-loop proxy for one actor-owned MCP ClientSession."""

    def __init__(self, owner: _SessionOwnerLoop, actor: _SessionActor) -> None:
        self._owner = owner
        self._actor = actor

    async def call_tool(self, name: str, arguments: dict[str, Any]) -> Any:
        return await self._owner.await_submit(self._actor.call_tool(name, arguments))

    async def aclose(self) -> None:
        await self._owner.await_submit(self._actor.close())

    def close_sync(self, timeout: float) -> None:
        self._owner.submit(self._actor.close()).result(timeout=timeout)


class MCPSessionPool:
    """Manage actor-owned MCP sessions scoped by ``(server_name, scope_key)``."""

    MAX_SESSIONS = 256
    SESSION_CLOSE_TIMEOUT = 5.0

    def __init__(self) -> None:
        self._entries: OrderedDict[tuple[str, str], OwnedMCPSession] = OrderedDict()
        self._creation_guards: dict[tuple[str, str], _CreationGuard] = {}
        self._lock = threading.Lock()
        self._owner = _SessionOwnerLoop()

    async def _acquire_creation_guard(self, key: tuple[str, str]) -> _CreationGuard:
        with self._lock:
            guard = self._creation_guards.get(key)
            if guard is None:
                guard = _CreationGuard()
                self._creation_guards[key] = guard
            guard.users += 1
        try:
            while not guard.lock.acquire(blocking=False):
                await asyncio.sleep(0.01)
            return guard
        except BaseException:
            self._drop_creation_guard_user(key, guard)
            raise

    def _drop_creation_guard_user(self, key: tuple[str, str], guard: _CreationGuard) -> None:
        with self._lock:
            guard.users -= 1
            if guard.users == 0 and self._creation_guards.get(key) is guard:
                self._creation_guards.pop(key, None)

    def _release_creation_guard(self, key: tuple[str, str], guard: _CreationGuard) -> None:
        guard.lock.release()
        self._drop_creation_guard_user(key, guard)

    async def get_session(
        self,
        server_name: str,
        scope_key: str,
        connection: dict[str, Any],
    ) -> OwnedMCPSession:
        key = (server_name, scope_key)
        with self._lock:
            existing = self._entries.get(key)
            if existing is not None:
                self._entries.move_to_end(key)
                return existing

        guard = await self._acquire_creation_guard(key)
        try:
            to_close: list[OwnedMCPSession] = []
            with self._lock:
                existing = self._entries.get(key)
                if existing is not None:
                    self._entries.move_to_end(key)
                    return existing
                while len(self._entries) >= self.MAX_SESSIONS:
                    _oldest_key, oldest = self._entries.popitem(last=False)
                    to_close.append(oldest)

            for session in to_close:
                await session.aclose()

            actor = _SessionActor(connection)
            await self._owner.await_submit(actor.start())
            proxy = OwnedMCPSession(self._owner, actor)
            with self._lock:
                self._entries[key] = proxy
            logger.info("Created actor-owned persistent MCP session for %s/%s", server_name, scope_key)
            return proxy
        finally:
            self._release_creation_guard(key, guard)

    async def _detach_matching(self, predicate) -> list[OwnedMCPSession]:
        with self._lock:
            keys = [key for key in self._entries if predicate(key)]
            sessions = [self._entries.pop(key) for key in keys]
        return sessions

    async def close_scope(self, scope_key: str) -> None:
        sessions = await self._detach_matching(lambda key: key[1] == scope_key)
        for session in sessions:
            await session.aclose()

    async def close_session_if_current(
        self,
        server_name: str,
        scope_key: str,
        session: OwnedMCPSession,
    ) -> bool:
        """Evict a dead session without closing a newer replacement."""
        key = (server_name, scope_key)
        with self._lock:
            if self._entries.get(key) is not session:
                return False
            self._entries.pop(key)
        await session.aclose()
        return True

    async def close_server(self, server_name: str) -> None:
        sessions = await self._detach_matching(lambda key: key[0] == server_name)
        for session in sessions:
            await session.aclose()

    async def close_all(self) -> None:
        sessions = await self._detach_matching(lambda _key: True)
        for session in sessions:
            await session.aclose()
        await asyncio.to_thread(self._owner.stop)

    def close_all_sync(self) -> None:
        with self._lock:
            sessions = list(self._entries.values())
            self._entries.clear()
        for session in sessions:
            try:
                session.close_sync(self.SESSION_CLOSE_TIMEOUT)
            except Exception:
                logger.debug("Error closing actor-owned MCP session", exc_info=True)
        self._owner.stop()


_pool: MCPSessionPool | None = None
_pool_lock = threading.Lock()


def get_session_pool() -> MCPSessionPool:
    global _pool
    if _pool is None:
        with _pool_lock:
            if _pool is None:
                _pool = MCPSessionPool()
    return _pool


def reset_session_pool() -> MCPSessionPool | None:
    """Atomically replace the singleton and return the retired pool."""
    global _pool
    with _pool_lock:
        retired = _pool
        _pool = None
    return retired
