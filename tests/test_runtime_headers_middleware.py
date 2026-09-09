from types import SimpleNamespace

import pytest
from langchain.agents.middleware.types import ModelRequest
from langchain_core.messages import AIMessage

from deerflow.agents.middlewares import (
    runtime_headers_middleware as runtime_headers_module,
)
from deerflow.agents.middlewares.runtime_headers_middleware import (
    RuntimeHeadersMiddleware,
    bind_runtime_headers,
)
from deerflow.config.model_config import ModelConfig


def _request(thread_id: str, *, headers=None):
    runtime = SimpleNamespace(
        context={"thread_id": thread_id, "run_id": "changing-run"},
        config={"configurable": {"thread_id": thread_id}},
    )
    return ModelRequest(
        model=None,
        messages=[],
        runtime=runtime,
        model_settings={"extra_headers": headers or {}},
    )


def test_opencode_session_is_stable_per_thread_and_preserves_static_headers():
    middleware = RuntimeHeadersMiddleware({"x-opencode-session": "thread_id"})
    seen = []

    def handler(request):
        seen.append(request.model_settings["extra_headers"])
        return AIMessage(content="ok")

    middleware.wrap_model_call(_request("thread-a", headers={"User-Agent": "deerflow"}), handler)
    middleware.wrap_model_call(_request("thread-a"), handler)
    middleware.wrap_model_call(_request("thread-b"), handler)

    assert seen[0] == {"User-Agent": "deerflow", "x-opencode-session": "thread-a"}
    assert seen[1]["x-opencode-session"] == "thread-a"
    assert seen[2]["x-opencode-session"] == "thread-b"


@pytest.mark.asyncio
async def test_async_model_call_injects_header():
    middleware = RuntimeHeadersMiddleware({"x-opencode-session": "thread_id"})

    async def handler(request):
        assert request.model_settings["extra_headers"]["x-opencode-session"] == "thread-a"
        return AIMessage(content="ok")

    assert (await middleware.awrap_model_call(_request("thread-a"), handler)).content == "ok"


def test_missing_or_unsafe_session_value_fails_fast():
    middleware = RuntimeHeadersMiddleware({"x-opencode-session": "thread_id"})
    with pytest.raises(ValueError, match="requires missing"):
        middleware.wrap_model_call(_request(""), lambda request: AIMessage(content="bad"))
    with pytest.raises(ValueError, match="CR/LF"):
        middleware.wrap_model_call(_request("thread\r\ninjected"), lambda request: AIMessage(content="bad"))


def test_runtime_header_config_rejects_invalid_names_and_sources():
    base = {"name": "go", "use": "langchain_openai:ChatOpenAI", "model": "kimi"}
    with pytest.raises(ValueError):
        ModelConfig(**base, runtime_headers={"bad header": "thread_id"})
    with pytest.raises(ValueError):
        ModelConfig(**base, runtime_headers={"x-opencode-session": "random"})


def test_direct_model_calls_bind_parent_thread_header(monkeypatch):
    class DirectModel:
        def __init__(self) -> None:
            self._deerflow_runtime_headers = {"x-opencode-session": "thread_id"}

        def bind(self, **kwargs):
            return kwargs

    monkeypatch.setattr(
        runtime_headers_module,
        "get_config",
        lambda: {"configurable": {"thread_id": "parent-thread"}},
    )

    assert bind_runtime_headers(DirectModel()) == {
        "extra_headers": {"x-opencode-session": "parent-thread"}
    }


def test_direct_model_calls_accept_explicit_background_session():
    class DirectModel:
        def __init__(self) -> None:
            self._deerflow_runtime_headers = {"x-opencode-session": "thread_id"}

        def bind(self, **kwargs):
            return kwargs

    assert bind_runtime_headers(
        DirectModel(),
        runtime_values={"thread_id": "background-thread"},
    ) == {"extra_headers": {"x-opencode-session": "background-thread"}}
