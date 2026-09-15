"""Compatibility helpers for direct chat-model invocations."""

from __future__ import annotations

import logging
from typing import Any

logger = logging.getLogger(__name__)

_STREAM_ONLY_MESSAGE = "non-stream chat request is currently not supported"


def _status_code(exc: BaseException) -> int | None:
    status = getattr(exc, "status_code", None)
    if isinstance(status, int):
        return status
    response = getattr(exc, "response", None)
    status = getattr(response, "status_code", None)
    return status if isinstance(status, int) else None


def is_stream_only_chat_error(exc: BaseException) -> bool:
    """Return whether a provider explicitly rejected a non-stream chat call.

    The check deliberately requires both HTTP 400 and the provider's specific
    error text so unrelated bad requests are never retried with changed
    semantics.
    """
    return _status_code(exc) == 400 and _STREAM_ONLY_MESSAGE in str(exc).casefold()


def invoke_chat_model(model: Any, input: Any, *, config: Any = None, **kwargs: Any) -> Any:
    """Invoke a model, retrying once in stream mode for stream-only gateways."""
    call_kwargs = dict(kwargs)
    if config is not None:
        call_kwargs["config"] = config
    try:
        return model.invoke(input, **call_kwargs)
    except Exception as exc:
        if not is_stream_only_chat_error(exc) or kwargs.get("stream") is True:
            raise
        logger.info("Provider requires streaming; retrying direct chat invocation with stream=True")
        return model.invoke(input, **{**call_kwargs, "stream": True})


async def ainvoke_chat_model(model: Any, input: Any, *, config: Any = None, **kwargs: Any) -> Any:
    """Async counterpart of :func:`invoke_chat_model`."""
    call_kwargs = dict(kwargs)
    if config is not None:
        call_kwargs["config"] = config
    try:
        return await model.ainvoke(input, **call_kwargs)
    except Exception as exc:
        if not is_stream_only_chat_error(exc) or kwargs.get("stream") is True:
            raise
        logger.info("Provider requires streaming; retrying direct chat invocation with stream=True")
        return await model.ainvoke(input, **{**call_kwargs, "stream": True})
