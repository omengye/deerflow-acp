from __future__ import annotations

import asyncio
from types import SimpleNamespace

import pytest
from langchain_core.messages import ToolMessage

from deerflow.agents.middlewares.read_before_write_middleware import (
    READ_MARK_KEY,
    WRITE_BLOCK_KEY,
    ReadBeforeWriteMiddleware,
)
from deerflow.agents.middlewares.tool_error_handling_middleware import (
    build_lead_runtime_middlewares,
    build_subagent_runtime_middlewares,
)


def _request(name: str, state: dict, *, path: str = "/mnt/user-data/workspace/a.txt"):
    return SimpleNamespace(
        tool_call={"name": name, "id": f"{name}-1", "args": {"path": path}},
        state=state,
        runtime=SimpleNamespace(context={"thread_id": "thread-1"}),
    )


def _result(name: str, content: str = "OK") -> ToolMessage:
    return ToolMessage(content=content, tool_call_id=f"{name}-1", name=name)


def test_existing_file_write_is_blocked_without_current_read() -> None:
    middleware = ReadBeforeWriteMiddleware(lambda _runtime, _path: "current")
    called = False

    def handler(_request):
        nonlocal called
        called = True
        return _result("write_file")

    result = middleware.wrap_tool_call(_request("write_file", {"messages": []}), handler)

    assert isinstance(result, ToolMessage)
    assert result.status == "error"
    assert WRITE_BLOCK_KEY in result.additional_kwargs
    assert called is False


def test_read_mark_allows_one_write_then_becomes_stale() -> None:
    files = {"/mnt/user-data/workspace/a.txt": "before"}
    middleware = ReadBeforeWriteMiddleware(lambda _runtime, path: files[path])
    read = middleware.wrap_tool_call(
        _request("read_file", {"messages": []}),
        lambda _request: _result("read_file", "before"),
    )
    assert isinstance(read, ToolMessage)
    assert READ_MARK_KEY in read.additional_kwargs
    state = {"messages": [read]}

    def write(_request):
        files["/mnt/user-data/workspace/a.txt"] = "after"
        return _result("write_file")

    first = middleware.wrap_tool_call(_request("write_file", state), write)
    second = middleware.wrap_tool_call(_request("write_file", state), write)

    assert isinstance(first, ToolMessage) and first.status != "error"
    assert isinstance(second, ToolMessage) and second.status == "error"
    assert files["/mnt/user-data/workspace/a.txt"] == "after"


def test_new_or_uninspectable_file_fails_open_to_tool_handler() -> None:
    def reader(_runtime, path):
        if path.endswith("new.txt"):
            raise FileNotFoundError(path)
        return "Error: remote sandbox unavailable"

    middleware = ReadBeforeWriteMiddleware(reader)
    for path in (
        "/mnt/user-data/workspace/new.txt",
        "/mnt/user-data/workspace/remote.txt",
    ):
        result = middleware.wrap_tool_call(
            _request("write_file", {"messages": []}, path=path),
            lambda _request: _result("write_file"),
        )
        assert isinstance(result, ToolMessage)
        assert result.status != "error"


@pytest.mark.asyncio
async def test_cancelled_async_write_releases_path_lock() -> None:
    content = "current"
    middleware = ReadBeforeWriteMiddleware(lambda _runtime, _path: content)
    read = middleware.wrap_tool_call(
        _request("read_file", {"messages": []}),
        lambda _request: _result("read_file", content),
    )
    state = {"messages": [read]}
    entered = asyncio.Event()

    async def waiting_handler(_request):
        entered.set()
        await asyncio.Event().wait()
        return _result("write_file")

    task = asyncio.create_task(
        middleware.awrap_tool_call(_request("write_file", state), waiting_handler)
    )
    await entered.wait()
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task

    async def succeeding_handler(_request):
        return _result("write_file")

    result = await asyncio.wait_for(
        middleware.awrap_tool_call(
            _request("write_file", state),
            succeeding_handler,
        ),
        timeout=1,
    )
    assert isinstance(result, ToolMessage)
    assert result.status != "error"


def test_runtime_builders_install_read_before_write_gate() -> None:
    assert any(
        isinstance(middleware, ReadBeforeWriteMiddleware)
        for middleware in build_lead_runtime_middlewares()
    )
    assert any(
        isinstance(middleware, ReadBeforeWriteMiddleware)
        for middleware in build_subagent_runtime_middlewares()
    )
