"""Tests for adding sync wrappers only to async StructuredTool instances."""

from typing import Annotated

from langchain_core.tools import InjectedToolArg, StructuredTool
from langchain_core.messages import AIMessage
from langgraph.graph import END, START, MessagesState, StateGraph
from langgraph.prebuilt import ToolNode
from langgraph.prebuilt.tool_node import _get_all_injected_args

from deerflow.tools.tools import _ensure_sync_invocable_tool


async def _async_echo(value: str) -> str:
    return value


def test_async_structured_tool_gets_sync_wrapper() -> None:
    tool = StructuredTool.from_function(
        coroutine=_async_echo,
        name="async_echo",
        description="Return the provided value.",
    )
    assert tool.func is None

    resolved = _ensure_sync_invocable_tool(tool)

    assert resolved is tool
    assert tool.func is not None
    assert tool.invoke({"value": "hello"}) == "hello"


def test_sync_wrapper_preserves_toolruntime_injection_metadata() -> None:
    seen = {}

    async def adapter_tool(
        value: str,
        runtime: Annotated[object | None, InjectedToolArg()] = None,
    ) -> str:
        seen["runtime"] = runtime
        return value

    tool = StructuredTool.from_function(
        coroutine=adapter_tool,
        name="adapter_tool",
        description="Adapter-shaped async tool.",
    )
    _ensure_sync_invocable_tool(tool)

    assert tool.func is not None
    assert tool.func.__wrapped__ is adapter_tool
    assert _get_all_injected_args(tool).runtime == "runtime"

    graph = StateGraph(MessagesState, context_schema=dict)
    graph.add_node("tools", ToolNode([tool]))
    graph.add_edge(START, "tools")
    graph.add_edge("tools", END)
    graph.compile().invoke(
        {
            "messages": [
                AIMessage(
                    content="",
                    tool_calls=[
                        {"name": "adapter_tool", "args": {"value": "ok"}, "id": "call-1"}
                    ],
                )
            ]
        },
        context={"thread_id": "thread-1", "user_id": "alice"},
    )
    assert seen["runtime"] is not None
    assert seen["runtime"].context["user_id"] == "alice"
