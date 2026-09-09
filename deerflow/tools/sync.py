"""Utilities for invoking async tools from synchronous agent paths.

When the deerflow client streams synchronously (or when LangChain dispatches
a tool through a sync code path), async-only tools — including those served
by MCP or ACP adapters — must be re-entered via a thread-pool bridge.

This wrapper additionally:

* Detects whether the wrapped coroutine declares a ``RunnableConfig``
  parameter (so we can forward LangChain's runtime-injected ``config``
  kwarg under whatever name the function chose, e.g. ``config`` or
  ``runnable_config``).
* Copies the current ``contextvars`` context into the worker thread so
  LangGraph runtime context (``thread_id``, ``run_id``, ...) propagates
  to the inner coroutine instead of being lost at the thread boundary.
"""

import asyncio
import atexit
import concurrent.futures
import contextvars
import functools
import logging
import typing
from collections.abc import Callable
from typing import Any, get_type_hints

from langchain_core.runnables import RunnableConfig

logger = logging.getLogger(__name__)

_SYNC_TOOL_EXECUTOR = concurrent.futures.ThreadPoolExecutor(max_workers=10, thread_name_prefix="tool-sync")

atexit.register(lambda: _SYNC_TOOL_EXECUTOR.shutdown(wait=False))


def _get_runnable_config_param(coro: Callable[..., Any]) -> str | None:
    """Return the parameter name (if any) that expects a ``RunnableConfig``.

    LangChain dispatches tools by injecting a ``config`` kwarg when the
    callable accepts one. Tool authors may name the parameter differently
    (``runnable_config``, ``cfg``, ...) — inspect type hints to discover the
    canonical name. ``functools.partial`` wrappers are unwrapped so the
    inspection sees the original signature.
    """
    target = coro.func if isinstance(coro, functools.partial) else coro
    try:
        hints = get_type_hints(target)
    except Exception:
        return None
    for name, annotation in hints.items():
        if name == "return":
            continue
        if annotation is RunnableConfig:
            return name
        # Handle ``RunnableConfig | None`` / ``Optional[RunnableConfig]`` /
        # ``Union[RunnableConfig, ...]`` — common in optional-config tools.
        if RunnableConfig in typing.get_args(annotation):
            return name
    return None


def make_sync_tool_wrapper(coro: Callable[..., Any], tool_name: str) -> Callable[..., Any]:
    """Build a synchronous wrapper for an asynchronous tool coroutine.

    The wrapper:
      * Forwards any LangChain-injected ``config`` kwarg to the parameter
        whose annotation is ``RunnableConfig`` (if any).
      * Preserves ``contextvars`` across the thread-pool boundary so the
        coroutine sees the same runtime context as the caller.
      * Runs the coroutine on a fresh event loop inside a worker thread
        when invoked from inside a running loop, falling back to
        ``asyncio.run`` otherwise.
    """

    config_param = _get_runnable_config_param(coro)

    def _run_coroutine(args: tuple[Any, ...], kwargs: dict[str, Any]) -> Any:
        injected_config = kwargs.pop("config", None)
        if config_param is not None and injected_config is not None and config_param not in kwargs:
            kwargs[config_param] = injected_config
        return asyncio.run(coro(*args, **kwargs))

    # LangGraph inspects ``tool.func`` (before ``tool.coroutine``) for injected
    # arguments such as ToolRuntime.  Preserve the coroutine's annotations and
    # ``__wrapped__`` chain when this wrapper is assigned to ``tool.func``.
    @functools.wraps(coro)
    def sync_wrapper(*args: Any, **kwargs: Any) -> Any:
        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            loop = None

        try:
            if loop is not None and loop.is_running():
                ctx = contextvars.copy_context()
                future = _SYNC_TOOL_EXECUTOR.submit(ctx.run, _run_coroutine, args, kwargs)
                return future.result()
            return _run_coroutine(args, kwargs)
        except Exception as exc:
            logger.error("Error invoking tool %r via sync wrapper: %s", tool_name, exc, exc_info=True)
            raise

    return sync_wrapper
