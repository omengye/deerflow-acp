"""Deterministic version gate for file-modifying tools."""

from __future__ import annotations

import asyncio
import hashlib
import logging
import posixpath
import threading
import weakref
from collections.abc import Awaitable, Callable
from typing import Any, override

from langchain.agents.middleware import AgentMiddleware
from langchain_core.messages import ToolMessage
from langgraph.prebuilt.tool_node import ToolCallRequest
from langgraph.types import Command

from deerflow.sandbox.tools import read_current_file_content

logger = logging.getLogger(__name__)

READ_MARK_KEY = "deerflow_read_mark"
WRITE_BLOCK_KEY = "deerflow_write_block"
_READ_TOOLS = frozenset({"read_file"})
_WRITE_TOOLS = frozenset({"write_file", "str_replace"})
_UNINSPECTABLE_PREFIX = "Error:"
_BLOCK_MESSAGE = (
    "Error: {tool} blocked — {path} already exists and its current version "
    "has not been read. Re-read the file after every modification, inspect "
    "the current content, then retry the write."
)

_GATE_LOCKS: weakref.WeakValueDictionary[
    tuple[str, str], threading.Lock
] = weakref.WeakValueDictionary()
_GATE_LOCKS_GUARD = threading.Lock()


def _gate_lock(scope: str, path: str) -> threading.Lock:
    key = (scope, posixpath.normpath(path))
    with _GATE_LOCKS_GUARD:
        lock = _GATE_LOCKS.get(key)
        if lock is None:
            lock = threading.Lock()
            _GATE_LOCKS[key] = lock
        return lock


def _content_hash(content: str) -> str:
    return hashlib.sha256(content.encode("utf-8")).hexdigest()


async def _await_off_thread(task: asyncio.Task[Any]) -> Any:
    """Drain dispatched thread work before propagating task cancellation."""
    first_cancel: asyncio.CancelledError | None = None
    while True:
        try:
            result = await asyncio.shield(task)
        except asyncio.CancelledError as exc:
            if task.cancelled():
                raise first_cancel or exc
            first_cancel = first_cancel or exc
            if not task.done():
                continue
        except BaseException:
            if first_cancel is None:
                raise
        else:
            if first_cancel is None:
                return result

        if first_cancel is not None:
            if task.done() and not task.cancelled():
                task.exception()
            raise first_cancel


async def _acquire_lock(lock: threading.Lock) -> None:
    task = asyncio.create_task(asyncio.to_thread(lock.acquire))
    try:
        await _await_off_thread(task)
    except asyncio.CancelledError:
        if task.done() and not task.cancelled() and task.exception() is None:
            lock.release()
        raise


class ReadBeforeWriteMiddleware(AgentMiddleware):
    """Require a retained read mark matching an existing file's current hash."""

    def __init__(
        self,
        content_reader: Callable[[Any, str], str] | None = None,
    ) -> None:
        super().__init__()
        self._content_reader = content_reader or read_current_file_content

    @override
    def wrap_tool_call(
        self,
        request: ToolCallRequest,
        handler: Callable[[ToolCallRequest], ToolMessage | Command],
    ) -> ToolMessage | Command:
        name = request.tool_call.get("name")
        path = self._path(request)
        if path is None or name not in _READ_TOOLS | _WRITE_TOOLS:
            return handler(request)
        with self._lock(request, path):
            if name in _WRITE_TOOLS:
                blocked = self._check_write(request, path)
                return blocked if blocked is not None else handler(request)
            result = handler(request)
            self._stamp_read(request, path, result)
            return result

    @override
    async def awrap_tool_call(
        self,
        request: ToolCallRequest,
        handler: Callable[[ToolCallRequest], Awaitable[ToolMessage | Command]],
    ) -> ToolMessage | Command:
        name = request.tool_call.get("name")
        path = self._path(request)
        if path is None or name not in _READ_TOOLS | _WRITE_TOOLS:
            return await handler(request)
        lock = self._lock(request, path)
        await _acquire_lock(lock)
        try:
            if name in _WRITE_TOOLS:
                check = asyncio.create_task(
                    asyncio.to_thread(self._check_write, request, path)
                )
                blocked = await _await_off_thread(check)
                return blocked if blocked is not None else await handler(request)
            result = await handler(request)
            stamp = asyncio.create_task(
                asyncio.to_thread(self._stamp_read, request, path, result)
            )
            await _await_off_thread(stamp)
            return result
        finally:
            lock.release()

    @staticmethod
    def _path(request: ToolCallRequest) -> str | None:
        args = request.tool_call.get("args") or {}
        if not isinstance(args, dict):
            return None
        path = args.get("path")
        return path if isinstance(path, str) and path else None

    @staticmethod
    def _scope(request: ToolCallRequest) -> str:
        context = getattr(request.runtime, "context", None)
        if isinstance(context, dict):
            thread_id = context.get("thread_id")
            if isinstance(thread_id, str) and thread_id:
                return thread_id
        state = request.state
        sandbox = state.get("sandbox") if isinstance(state, dict) else None
        if isinstance(sandbox, dict) and isinstance(sandbox.get("sandbox_id"), str):
            return sandbox["sandbox_id"]
        return "global"

    def _lock(self, request: ToolCallRequest, path: str) -> threading.Lock:
        return _gate_lock(self._scope(request), path)

    def _check_write(
        self,
        request: ToolCallRequest,
        path: str,
    ) -> ToolMessage | None:
        try:
            current = self._content_reader(request.runtime, path)
        except FileNotFoundError:
            return None
        except Exception:
            logger.warning(
                "Read-before-write gate could not inspect %r; allowing tool error handling",
                path,
                exc_info=True,
            )
            return None
        if current.startswith(_UNINSPECTABLE_PREFIX):
            return None
        if self._latest_hash(request.state, posixpath.normpath(path)) == _content_hash(current):
            return None
        tool = str(request.tool_call.get("name") or "write")
        return ToolMessage(
            content=_BLOCK_MESSAGE.format(tool=tool, path=path),
            tool_call_id=str(request.tool_call.get("id") or ""),
            name=tool,
            status="error",
            additional_kwargs={
                WRITE_BLOCK_KEY: {"path": posixpath.normpath(path), "tool": tool}
            },
        )

    @staticmethod
    def _latest_hash(state: Any, path: str) -> str | None:
        messages = (
            state.get("messages")
            if isinstance(state, dict)
            else getattr(state, "messages", None)
        )
        for message in reversed(messages or []):
            if not isinstance(message, ToolMessage):
                continue
            mark = (message.additional_kwargs or {}).get(READ_MARK_KEY)
            if isinstance(mark, dict) and mark.get("path") == path:
                value = mark.get("hash")
                return value if isinstance(value, str) else None
        return None

    def _stamp_read(
        self,
        request: ToolCallRequest,
        path: str,
        result: ToolMessage | Command,
    ) -> None:
        message = self._tool_message(result)
        if message is None or message.status == "error":
            return
        try:
            content = self._content_reader(request.runtime, path)
        except Exception:
            logger.debug("Read mark skipped for %r", path, exc_info=True)
            return
        if content.startswith(_UNINSPECTABLE_PREFIX):
            return
        message.additional_kwargs[READ_MARK_KEY] = {
            "path": posixpath.normpath(path),
            "hash": _content_hash(content),
        }

    @staticmethod
    def _tool_message(result: ToolMessage | Command) -> ToolMessage | None:
        if isinstance(result, ToolMessage):
            return result
        if isinstance(result, Command) and isinstance(result.update, dict):
            messages = result.update.get("messages") or []
            candidates = [
                message for message in messages if isinstance(message, ToolMessage)
            ]
            return candidates[-1] if candidates else None
        return None
