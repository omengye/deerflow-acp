import inspect
import logging
from typing import Any

from .factory import create_chat_model

logger = logging.getLogger(__name__)

__all__ = ["create_chat_model", "aclose_chat_model"]


async def aclose_chat_model(model: Any) -> None:
    """Drain a chat model's owned HTTP clients.

    Call this in ``finally`` blocks around model usage whose surrounding
    event loop is about to be closed (worker-thread ``asyncio.run`` paths
    in particular). Without it, openai/httpx connection pools retain SSL
    transports bound to the dying loop; later GC or pool reuse then
    surfaces as ``RuntimeError: Event loop is closed`` inside an unrelated
    coroutine.

    Safe to call on any model — unknown shapes are silently ignored.
    """
    if model is None:
        return

    seen: set[int] = set()
    # langchain_openai.ChatOpenAI: ``root_async_client`` is the
    # ``openai.AsyncOpenAI`` instance and ``http_async_client`` is a
    # user-supplied ``httpx.AsyncClient`` (when one was injected).
    #
    # When we inject ``http_async_client`` we own that transport directly. Close
    # it without marking the OpenAI root client as closed; some OpenAI-compatible
    # wrappers/cache layers can otherwise surface "client has been closed" on a
    # later model call that reuses the root client object.
    explicit_http_client = getattr(model, "http_async_client", None)
    attrs = ("http_async_client",) if explicit_http_client is not None else ("root_async_client", "_async_client")
    for attr in attrs:
        client = getattr(model, attr, None)
        if client is None or id(client) in seen:
            continue
        seen.add(id(client))

        closer = getattr(client, "close", None) or getattr(client, "aclose", None)
        if closer is None:
            continue
        try:
            result = closer()
            if inspect.isawaitable(result):
                await result
        except Exception:
            logger.debug(
                "Failed to close chat model client attribute %r",
                attr,
                exc_info=True,
            )

    # The ACP cold-start path may also inject a sync httpx.Client. Only close
    # the one created by our factory; a caller-provided client remains theirs.
    owned_sync_finalizer = getattr(model, "_deerflow_owned_http_client_finalizer", None)
    if owned_sync_finalizer is not None:
        try:
            owned_sync_finalizer()
        except Exception:
            logger.debug("Failed to close factory-owned sync HTTP client", exc_info=True)
