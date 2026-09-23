"""Tool definition API.

Two ways to define a tool:

  1. ``@tool`` decorator on a function. Schema is inferred from the
     function signature and docstring.

  2. Subclass ``Tool`` for full control over name, description,
     input schema, and execution.

A ``Tool`` is what the harness eventually wires into the underlying engine.
The decorator path is sugar over the class path.
"""

from __future__ import annotations

import inspect
from typing import Any, Callable, Protocol, overload, runtime_checkable

from pydantic import BaseModel, ConfigDict, Field


class ToolContext(BaseModel):
    """Per-call context passed as the first argument to every tool."""

    model_config = ConfigDict(extra="forbid", arbitrary_types_allowed=True)

    run_id: str
    thread_id: str
    user_data: dict[str, Any] = Field(default_factory=dict)
    # ``sandbox`` is forward-typed to avoid an import cycle. Resolved lazily.
    sandbox: Any = None  # Sandbox | None at runtime (forward-typed)


@runtime_checkable
class Tool(Protocol):
    """Public Tool protocol. Implement to expose a callable to the agent."""

    name: str
    description: str
    input_schema: type[BaseModel]

    async def __call__(self, ctx: ToolContext, /, **kwargs: Any) -> Any: ...


class _FunctionTool:
    """Adapter that wraps a plain (async) function as a ``Tool``.

    Created by the ``@tool`` decorator. Not part of the public API.
    """

    def __init__(
        self,
        fn: Callable[..., Any],
        *,
        name: str,
        description: str,
        input_schema: type[BaseModel],
    ) -> None:
        self._fn = fn
        self.name = name
        self.description = description
        self.input_schema = input_schema
        self._is_async = inspect.iscoroutinefunction(fn)
        self._accepts_ctx = "ctx" in inspect.signature(fn).parameters

    async def __call__(self, ctx: ToolContext, /, **kwargs: Any) -> Any:
        validated = self.input_schema(**kwargs)
        fn_kwargs = validated.model_dump()
        if self._accepts_ctx:
            fn_kwargs["ctx"] = ctx
        result = self._fn(**fn_kwargs)
        if self._is_async:
            return await result
        return result

    def __repr__(self) -> str:
        return f"<Tool name={self.name!r}>"


@overload
def tool(
    fn: Callable[..., Any],
    *,
    name: str | None = None,
    description: str | None = None,
    input_schema: type[BaseModel] | None = None,
) -> Tool: ...


@overload
def tool(
    fn: None = None,
    *,
    name: str | None = None,
    description: str | None = None,
    input_schema: type[BaseModel] | None = None,
) -> Callable[[Callable[..., Any]], Tool]: ...


def tool(
    fn: Callable[..., Any] | None = None,
    *,
    name: str | None = None,
    description: str | None = None,
    input_schema: type[BaseModel] | None = None,
) -> Tool | Callable[[Callable[..., Any]], Tool]:
    """Decorate a function as a tool.

    Examples:
        @tool
        def get_weather(city: str) -> str:
            \"\"\"Get current weather for a city.\"\"\"
            ...

        @tool(name="search", description="Web search.")
        async def web_search(query: str, limit: int = 10) -> list[str]: ...

    The resulting object is a ``Tool``. It may be passed to
    ``Harness(tools=[...])`` or registered with a tool group.
    """

    def _wrap(f: Callable[..., Any]) -> Tool:
        actual_name = name or f.__name__
        actual_desc = description or (inspect.getdoc(f) or "").strip()
        if not actual_desc:
            raise ValueError(
                f"tool {actual_name!r}: description required (docstring or "
                "description= kwarg)."
            )
        schema = input_schema or _infer_schema(f, actual_name)
        return _FunctionTool(f, name=actual_name, description=actual_desc, input_schema=schema)

    if fn is None:
        return _wrap
    return _wrap(fn)


def _infer_schema(fn: Callable[..., Any], tool_name: str) -> type[BaseModel]:
    """Build a pydantic model from a function signature.

    v0.1: minimal implementation. Supports primitive types and Pydantic
    models. Complex generics raise. Schema inference is intentionally
    conservative so we can tighten it without breaking users in v0.2.
    """
    sig = inspect.signature(fn)
    fields: dict[str, Any] = {}
    for pname, param in sig.parameters.items():
        if pname == "self" or pname == "ctx":
            continue
        annotation = param.annotation
        if annotation is inspect.Parameter.empty:
            raise TypeError(
                f"tool {tool_name!r}: parameter {pname!r} has no type annotation. "
                "Either annotate it or pass an explicit input_schema=."
            )
        default = ... if param.default is inspect.Parameter.empty else param.default
        fields[pname] = (annotation, default)

    from pydantic import create_model

    return create_model(
        f"{tool_name.title().replace('_', '')}Input",
        __config__=ConfigDict(extra="forbid"),
        **fields,
    )
