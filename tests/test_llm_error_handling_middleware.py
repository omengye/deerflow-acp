import asyncio
from types import SimpleNamespace

import pytest
from langchain_core.messages import AIMessage

from deerflow.agents.middlewares.llm_error_handling_middleware import LLMErrorHandlingMiddleware
from deerflow.agents.middlewares import llm_error_handling_middleware as llm_middleware_module


class FakeProviderError(Exception):
    status_code = 400
    body = {
        "error": {
            "message": "Input data may contain inappropriate content.",
            "type": "data_inspection_failed",
            "code": "data_inspection_failed",
        }
    }

    def __str__(self) -> str:
        return "Error code: 400 - {'error': {'code': 'data_inspection_failed'}}"


class FakeUpstreamProviderError(Exception):
    status_code = 400
    body = {
        "error": {
            "message": "Error from provider (Console Go): Upstream request failed",
            "type": "invalid_request_error",
            "code": "invalid_request_error",
        }
    }

    def __str__(self) -> str:
        return (
            "Error code: 400 - {'error': {'message': "
            "'Error from provider (Console Go): Upstream request failed', "
            "'type': 'invalid_request_error', 'code': 'invalid_request_error'}}"
        )


def test_content_policy_error_returns_user_message_without_raising():
    middleware = LLMErrorHandlingMiddleware()

    def handler(_request):
        raise FakeProviderError()

    response = middleware.wrap_model_call(None, handler)

    assert isinstance(response, AIMessage)
    assert "content safety policy" in response.content


@pytest.mark.asyncio
async def test_async_content_policy_error_returns_user_message_without_raising():
    middleware = LLMErrorHandlingMiddleware()

    async def handler(_request):
        raise FakeProviderError()

    response = await middleware.awrap_model_call(None, handler)

    assert isinstance(response, AIMessage)
    assert "content safety policy" in response.content


def test_wrapped_upstream_failure_retries_despite_400_status():
    middleware = LLMErrorHandlingMiddleware()
    middleware.retry_base_delay_ms = 0
    attempts = 0

    def handler(_request):
        nonlocal attempts
        attempts += 1
        if attempts < 3:
            raise FakeUpstreamProviderError()
        return AIMessage(content="recovered")

    response = middleware.wrap_model_call(None, handler)

    assert attempts == 3
    assert isinstance(response, AIMessage)
    assert response.content == "recovered"


@pytest.mark.asyncio
async def test_async_wrapped_upstream_failure_retries_despite_400_status():
    middleware = LLMErrorHandlingMiddleware()
    middleware.retry_base_delay_ms = 0
    attempts = 0

    async def handler(_request):
        nonlocal attempts
        attempts += 1
        if attempts < 3:
            raise FakeUpstreamProviderError()
        return AIMessage(content="recovered")

    response = await middleware.awrap_model_call(None, handler)

    assert attempts == 3
    assert isinstance(response, AIMessage)
    assert response.content == "recovered"


def test_other_bad_requests_are_not_retried():
    middleware = LLMErrorHandlingMiddleware()
    attempts = 0

    class InvalidRequestError(Exception):
        status_code = 400

    def handler(_request):
        nonlocal attempts
        attempts += 1
        raise InvalidRequestError("Invalid tool schema")

    response = middleware.wrap_model_call(None, handler)

    assert attempts == 1
    assert isinstance(response, AIMessage)
    assert "Invalid tool schema" in response.content


class _NoHeaderError(Exception):
    """A retriable-shaped exception with no Retry-After response headers."""


def test_retry_delay_uses_full_jitter_range(monkeypatch):
    middleware = LLMErrorHandlingMiddleware()
    middleware.retry_base_delay_ms = 1000
    middleware.retry_cap_delay_ms = 8000
    calls = []

    def fake_uniform(low, high):
        calls.append((low, high))
        return high / 2

    monkeypatch.setattr(
        "deerflow.agents.middlewares.llm_error_handling_middleware.random.uniform",
        fake_uniform,
    )

    delay = middleware._build_retry_delay_ms(3, _NoHeaderError())

    # attempt=3 -> uncapped backoff = base * 2**(3-1) = 4000, capped at min(4000, 8000) = 4000.
    # Full jitter samples uniformly from [0, capped_backoff], so the lower bound must be 0
    # (never base/2 or similar) and the upper bound must equal the capped backoff exactly.
    assert calls == [(0, 4000)]
    assert delay == 2000


def test_retry_delay_never_exceeds_cap_even_at_worst_case_sample(monkeypatch):
    middleware = LLMErrorHandlingMiddleware()
    middleware.retry_base_delay_ms = 1000
    middleware.retry_cap_delay_ms = 8000

    # Worst case for the "never exceed cap" guarantee: random.uniform returns its
    # upper bound every time.
    monkeypatch.setattr(
        "deerflow.agents.middlewares.llm_error_handling_middleware.random.uniform",
        lambda low, high: high,
    )

    for attempt in range(1, 8):
        delay = middleware._build_retry_delay_ms(attempt, _NoHeaderError())
        assert 0 <= delay <= middleware.retry_cap_delay_ms

    # Once uncapped exponential growth exceeds retry_cap_delay_ms, the sampled
    # upper bound must saturate at the cap rather than keep growing.
    assert middleware._build_retry_delay_ms(6, _NoHeaderError()) == middleware.retry_cap_delay_ms


def test_retry_delay_disperses_across_repeated_calls_for_same_attempt(monkeypatch):
    """Same attempt number must not always produce the same delay (no thundering herd)."""
    middleware = LLMErrorHandlingMiddleware()
    middleware.retry_base_delay_ms = 1000
    middleware.retry_cap_delay_ms = 8000

    sampled_values = iter([100.0, 900.0, 1999.0])
    monkeypatch.setattr(
        "deerflow.agents.middlewares.llm_error_handling_middleware.random.uniform",
        lambda low, high: next(sampled_values),
    )

    delays = [middleware._build_retry_delay_ms(2, _NoHeaderError()) for _ in range(3)]

    assert delays == [100, 900, 1999]
    assert len(set(delays)) == len(delays)


def test_terminal_upstream_failure_emits_machine_readable_event(monkeypatch):
    events = []
    middleware = LLMErrorHandlingMiddleware()
    middleware.retry_max_attempts = 1
    monkeypatch.setattr("langgraph.config.get_stream_writer", lambda: events.append)

    def handler(_request):
        raise FakeUpstreamProviderError()

    response = middleware.wrap_model_call(None, handler)

    assert isinstance(response, AIMessage)
    assert events == [
        {
            "type": "llm_failure",
            "reason": "busy",
            "retriable": True,
            "message": "The configured LLM provider is temporarily unavailable after multiple retries. Please wait a moment and continue the conversation.",
        }
    ]


def _llm_test_config(*, limit: int, timeout: float = 1.0):
    return SimpleNamespace(
        circuit_breaker=SimpleNamespace(failure_threshold=5, recovery_timeout_sec=60),
        llm_call=SimpleNamespace(
            max_concurrent_calls=limit,
            queue_timeout_seconds=timeout,
            retry_max_attempts=3,
            retry_base_delay_ms=0,
            retry_cap_delay_ms=0,
            burst_retry_base_delay_ms=0,
        ),
    )


@pytest.fixture
def reset_process_limiter():
    llm_middleware_module._reset_process_limiter_for_tests()
    try:
        yield
    finally:
        llm_middleware_module._reset_process_limiter_for_tests()


@pytest.mark.asyncio
async def test_process_wide_limiter_bounds_async_model_calls(monkeypatch, reset_process_limiter):
    monkeypatch.setattr(llm_middleware_module, "get_app_config", lambda: _llm_test_config(limit=2))
    middleware = LLMErrorHandlingMiddleware()
    in_flight = 0
    peak = 0
    counter_lock = asyncio.Lock()

    async def handler(_request):
        nonlocal in_flight, peak
        async with counter_lock:
            in_flight += 1
            peak = max(peak, in_flight)
        await asyncio.sleep(0.02)
        async with counter_lock:
            in_flight -= 1
        return AIMessage(content="ok")

    results = await asyncio.gather(
        *(middleware.awrap_model_call(None, handler) for _ in range(8))
    )

    assert peak == 2
    assert all(result.content == "ok" for result in results)
    assert llm_middleware_module._PROCESS_LIMITER.in_flight == 0


@pytest.mark.asyncio
async def test_limiter_queue_timeout_returns_visible_failure(monkeypatch, reset_process_limiter):
    monkeypatch.setattr(
        llm_middleware_module,
        "get_app_config",
        lambda: _llm_test_config(limit=1, timeout=0.02),
    )
    middleware = LLMErrorHandlingMiddleware()
    entered = asyncio.Event()
    release = asyncio.Event()

    async def blocking_handler(_request):
        entered.set()
        await release.wait()
        return AIMessage(content="first")

    first = asyncio.create_task(middleware.awrap_model_call(None, blocking_handler))
    await entered.wait()
    second = await middleware.awrap_model_call(None, blocking_handler)
    release.set()
    await first

    assert "maximum number of model calls" in second.content
    assert llm_middleware_module._PROCESS_LIMITER.in_flight == 0


@pytest.mark.asyncio
async def test_cancelled_waiter_does_not_leak_process_wide_permit(reset_process_limiter):
    limiter = llm_middleware_module._ProcessWideLimiter(1)
    await limiter.acquire_async()
    waiter = asyncio.create_task(limiter.acquire_async())
    await asyncio.sleep(0)
    waiter.cancel()
    with pytest.raises(asyncio.CancelledError):
        await waiter
    limiter.release()

    assert limiter.in_flight == 0
    assert limiter.queued == 0


@pytest.mark.asyncio
async def test_nested_model_call_reuses_same_task_slot(monkeypatch, reset_process_limiter):
    monkeypatch.setattr(
        llm_middleware_module,
        "get_app_config",
        lambda: _llm_test_config(limit=1, timeout=0.05),
    )
    middleware = LLMErrorHandlingMiddleware()

    async def handler(_request):
        async with llm_middleware_module.llm_call_slot_async():
            return AIMessage(content="nested")

    response = await middleware.awrap_model_call(None, handler)

    assert response.content == "nested"
    assert llm_middleware_module._PROCESS_LIMITER.in_flight == 0


@pytest.mark.asyncio
async def test_cancelled_half_open_probe_releases_only_its_lease(reset_process_limiter):
    middleware = LLMErrorHandlingMiddleware()
    middleware._circuit_state = "open"
    middleware._circuit_open_until = 0
    entered = asyncio.Event()

    async def blocked(_request):
        entered.set()
        await asyncio.Event().wait()

    probe = asyncio.create_task(middleware.awrap_model_call(None, blocked))
    await asyncio.wait_for(entered.wait(), timeout=1)
    owned_token = middleware._circuit_probe_token
    assert owned_token is not None

    # A stale request cannot free the active recovery probe.
    middleware._release_half_open_probe(probe_token=object())
    assert middleware._circuit_probe_in_flight is True
    assert middleware._circuit_probe_token is owned_token

    probe.cancel()
    with pytest.raises(asyncio.CancelledError):
        await probe
    assert middleware._circuit_probe_in_flight is False
    assert middleware._circuit_probe_token is None

    async def healthy(_request):
        return AIMessage(content="recovered")

    response = await middleware.awrap_model_call(None, healthy)
    assert response.content == "recovered"
    assert middleware._circuit_state == "closed"


def test_retry_after_is_bounded_by_configured_cap(monkeypatch, reset_process_limiter):
    monkeypatch.setattr(llm_middleware_module, "get_app_config", lambda: _llm_test_config(limit=0))
    middleware = LLMErrorHandlingMiddleware()
    error = RuntimeError("busy")
    error.response = SimpleNamespace(headers={"retry-after": "3600"})

    assert middleware._build_retry_delay_ms(1, error) == middleware.retry_cap_delay_ms


def test_burst_rate_429_has_tight_retry_budget(monkeypatch, reset_process_limiter):
    monkeypatch.setattr(llm_middleware_module, "get_app_config", lambda: _llm_test_config(limit=0))
    middleware = LLMErrorHandlingMiddleware()
    calls = 0

    class BurstRateError(Exception):
        status_code = 429
        code = "limit_burst_rate"

        def __str__(self):
            return "limit_burst_rate: request rate increased too quickly"

    def handler(_request):
        nonlocal calls
        calls += 1
        raise BurstRateError()

    response = middleware.wrap_model_call(None, handler)

    assert calls == 2
    assert "temporarily throttling requests" in response.content
    assert middleware._circuit_failure_count == 0
