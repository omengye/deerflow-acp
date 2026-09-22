import asyncio
from types import SimpleNamespace

import httpx
import pytest
from langchain.agents.middleware.types import ModelResponse
from langchain.agents import create_agent
from langchain_core.language_models.fake_chat_models import FakeMessagesListChatModel
from langchain_core.messages import AIMessage
from langchain_core.tools import StructuredTool

from deerflow.agents.middlewares.llm_error_handling_middleware import LLMErrorHandlingMiddleware


@pytest.fixture
def recovery(monkeypatch):
    middleware = LLMErrorHandlingMiddleware()
    middleware.retry_max_attempts = 3
    middleware.retry_base_delay_ms = 0
    middleware.retry_cap_delay_ms = 0
    monkeypatch.setattr(middleware, "_emit_retry_event", lambda *_args: None)
    monkeypatch.setattr(middleware, "_emit_failure_event", lambda *_args, **_kwargs: None)
    return middleware


@pytest.mark.parametrize("async_mode", [False, True])
async def test_empty_completed_response_recovers_once(recovery, async_mode):
    responses = iter([AIMessage(content="", response_metadata={"finish_reason": "stop"}), AIMessage(content="Recovered")])
    calls = 0

    def handler(_request):
        nonlocal calls
        calls += 1
        return next(responses)

    async def ahandler(request):
        return handler(request)

    response = await recovery.awrap_model_call(None, ahandler) if async_mode else recovery.wrap_model_call(None, handler)
    assert response.content == "Recovered"
    assert calls == 2
    assert recovery._circuit_failure_count == 0


def test_empty_response_failure_retains_provider_diagnostics_and_reasoning(recovery):
    original = AIMessage(
        id="provider-id",
        content=[{"type": "thinking", "thinking": "reasoning", "signature": "signed"}],
        additional_kwargs={"reasoning_content": "reasoning", "provider_trace": "trace-id"},
        response_metadata={"finish_reason": "stop", "model_name": "test"},
        usage_metadata={"input_tokens": 10, "output_tokens": 3, "total_tokens": 13},
    )
    response = recovery.wrap_model_call(None, lambda _: ModelResponse(result=[original]))
    assert response.id == original.id
    assert response.usage_metadata == original.usage_metadata
    assert response.response_metadata == original.response_metadata
    assert response.content[0] == original.content[0]
    assert "empty response" in response.content[-1]["text"]
    assert response.additional_kwargs["error_reason"] == "empty_response"
    assert response.additional_kwargs["deerflow_error_fallback"] is True
    assert response.additional_kwargs["provider_trace"] == "trace-id"
    assert original.additional_kwargs.get("error_reason") is None
    assert recovery._circuit_failure_count == 0


@pytest.mark.parametrize("async_mode", [False, True])
async def test_retry_budget_is_per_run_and_resets_for_next_run(recovery, async_mode):
    context = {"thread_id": "thread", "run_id": "run-1"}
    request = SimpleNamespace(runtime=SimpleNamespace(context=context))
    calls = 0

    def handler(_request):
        nonlocal calls
        calls += 1
        return AIMessage(content=" \n", response_metadata={"stop_reason": "end_turn"})

    async def ahandler(request):
        return handler(request)

    async def invoke():
        return await recovery.awrap_model_call(request, ahandler) if async_mode else recovery.wrap_model_call(request, handler)

    await invoke()
    assert calls == 2
    await invoke()
    assert calls == 3
    context["run_id"] = "run-2"
    await invoke()
    assert calls == 5


@pytest.mark.parametrize("response", [
    AIMessage(content="", tool_calls=[{"id": "tool-1", "name": "read_file", "args": {}}]),
    AIMessage(content="", invalid_tool_calls=[{"id": "tool-1", "name": "read_file", "args": "{", "error": "invalid json"}]),
    AIMessage(content=[{"type": "tool_use", "id": "tool-1", "name": "read_file", "input": {}}]),
    AIMessage(content=[{"type": "image_url", "image_url": {"url": "https://example.com/image.png"}}]),
    AIMessage(content="", response_metadata={"finish_reason": "length"}),
    AIMessage(content="", response_metadata={"stop_reason": "max_tokens"}),
    AIMessage(content="", response_metadata={"finish_reason": "content_filter"}),
    AIMessage(content="", response_metadata={"stop_reason": "refusal"}),
    AIMessage(content="", response_metadata={"finish_reason": "SAFETY"}),
])
def test_tools_media_and_provider_terminations_are_not_empty_retried(recovery, response):
    calls = 0

    def handler(_request):
        nonlocal calls
        calls += 1
        return response

    assert recovery.wrap_model_call(None, handler) is response
    assert calls == 1


def test_missing_model_result_produces_explicit_failure(recovery):
    response = recovery.wrap_model_call(None, lambda _: ModelResponse(result=[]))
    assert "empty response" in response.content
    assert response.additional_kwargs["error_reason"] == "empty_response"


def test_config_can_disable_empty_retry(recovery):
    recovery.retry_max_attempts = 1
    calls = 0

    def handler(_request):
        nonlocal calls
        calls += 1
        return AIMessage(content="")

    assert "empty response" in recovery.wrap_model_call(None, handler).content
    assert calls == 1


@pytest.mark.parametrize("error_class", [httpx.ReadTimeout, httpx.ConnectTimeout, httpx.WriteTimeout, httpx.PoolTimeout, httpx.TimeoutException])
def test_native_httpx_timeouts_have_bounded_recovery(recovery, error_class):
    calls = 0

    def handler(_request):
        nonlocal calls
        calls += 1
        raise error_class("deadline exceeded")

    response = recovery.wrap_model_call(None, handler)
    assert calls == recovery.retry_max_attempts
    assert "temporarily unavailable after multiple retries" in response.content


async def test_empty_retry_cancellation_releases_half_open_probe(recovery, monkeypatch):
    recovery._circuit_state = "half_open"
    retry_entered = asyncio.Event()
    monkeypatch.setattr(recovery, "_build_retry_delay_ms", lambda *_args: 30000)
    monkeypatch.setattr(recovery, "_emit_retry_event", lambda *_args: retry_entered.set())

    async def handler(_request):
        return AIMessage(content="")

    task = asyncio.create_task(recovery.awrap_model_call(None, handler))
    await asyncio.wait_for(retry_entered.wait(), timeout=2)
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    assert recovery._circuit_probe_in_flight is False
    assert recovery._circuit_failure_count == 0


@pytest.mark.parametrize("context", [None, {}, {"thread_id": "graph-thread", "run_id": "graph-run"}])
async def test_graph_tool_turns_share_the_run_empty_retry_budget(recovery, context):
    class ToolModel(FakeMessagesListChatModel):
        def bind_tools(self, tools, **kwargs):
            return self

    executed = []

    def probe() -> str:
        executed.append("probe")
        return "ok"

    model = ToolModel(responses=[
        AIMessage(content=""),
        AIMessage(content="", tool_calls=[{"id": "probe-1", "name": "probe", "args": {}}]),
        AIMessage(content=""),
        AIMessage(content="must not spend another retry"),
    ])
    agent = create_agent(
        model=model,
        tools=[StructuredTool.from_function(probe, description="Probe one deterministic tool invocation")],
        middleware=[recovery],
        context_schema=dict,
    )
    result = await agent.ainvoke(
        {"messages": [{"role": "user", "content": "probe"}]},
        context=context,
    )
    assert executed == ["probe"]
    assert model.i == 3
    assert result["messages"][-1].additional_kwargs["error_reason"] == "empty_response"


async def test_contextless_sdk_runs_release_their_retry_budget(recovery):
    from deerflow.agents.middlewares.llm_error_handling_middleware import _EMPTY_RETRY_ANCHORS

    before = len(_EMPTY_RETRY_ANCHORS)
    model = FakeMessagesListChatModel(responses=[AIMessage(content=""), AIMessage(content="recovered")])
    agent = create_agent(model=model, middleware=[recovery])
    for _ in range(3):
        result = await agent.ainvoke({"messages": [{"role": "user", "content": "continue"}]})
        assert result["messages"][-1].content == "recovered"
    assert len(_EMPTY_RETRY_ANCHORS) == before
