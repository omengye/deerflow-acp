"""Inject request-scoped provider headers without mutating shared models."""

from __future__ import annotations

from collections.abc import Awaitable, Callable, Mapping
from typing import Any, Literal, override

from langchain.agents.middleware import AgentMiddleware
from langchain.agents.middleware.types import (
    ModelCallResult,
    ModelRequest,
    ModelResponse,
)
from langgraph.config import get_config

RuntimeHeaderSource = Literal["thread_id", "run_id", "user_id"]


def _runtime_values(
    runtime: object | None = None,
    *,
    runtime_values: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    values: dict[str, Any] = {}
    context = getattr(runtime, "context", None)
    if isinstance(context, Mapping):
        values.update(context)
    config = getattr(runtime, "config", None)
    if not isinstance(config, Mapping):
        try:
            config = get_config()
        except RuntimeError:
            config = {}
    configurable = config.get("configurable", {}) if isinstance(config, Mapping) else {}
    if isinstance(configurable, Mapping):
        for key, value in configurable.items():
            values.setdefault(key, value)
    if runtime_values is not None:
        values.update(runtime_values)
    return values


def resolve_runtime_headers(
    sources: Mapping[str, RuntimeHeaderSource],
    runtime: object | None = None,
    *,
    runtime_values: Mapping[str, Any] | None = None,
) -> dict[str, str]:
    values = _runtime_values(runtime, runtime_values=runtime_values)
    headers: dict[str, str] = {}
    for header_name, source in sources.items():
        value = values.get(source)
        if value is None or value == "":
            raise ValueError(f"Runtime header {header_name!r} requires missing {source!r}")
        rendered = str(value)
        if "\r" in rendered or "\n" in rendered:
            raise ValueError(f"Runtime header {header_name!r} contains CR/LF")
        headers[header_name] = rendered
    return headers


def bind_runtime_headers(
    model,
    runtime: object | None = None,
    *,
    runtime_values: Mapping[str, Any] | None = None,
):
    """Bind dynamic headers for direct model calls such as summarization."""
    sources = getattr(model, "_deerflow_runtime_headers", None)
    if not isinstance(sources, Mapping) or not sources:
        return model
    return model.bind(
        extra_headers=resolve_runtime_headers(
            sources,
            runtime,
            runtime_values=runtime_values,
        )
    )


class RuntimeHeadersMiddleware(AgentMiddleware):
    def __init__(self, sources: Mapping[str, RuntimeHeaderSource]) -> None:
        super().__init__()
        self.sources = dict(sources)

    def _augment(self, request: ModelRequest) -> ModelRequest:
        dynamic = resolve_runtime_headers(self.sources, request.runtime)
        settings = dict(request.model_settings or {})
        extra_headers = dict(settings.get("extra_headers") or {})
        extra_headers.update(dynamic)
        settings["extra_headers"] = extra_headers
        return request.override(model_settings=settings)

    @override
    def wrap_model_call(
        self,
        request: ModelRequest,
        handler: Callable[[ModelRequest], ModelResponse],
    ) -> ModelCallResult:
        return handler(self._augment(request))

    @override
    async def awrap_model_call(
        self,
        request: ModelRequest,
        handler: Callable[[ModelRequest], Awaitable[ModelResponse]],
    ) -> ModelCallResult:
        return await handler(self._augment(request))
