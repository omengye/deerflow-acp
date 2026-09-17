"""Offline coverage for model request admission and factory integration."""

import asyncio
import threading
from concurrent.futures import ThreadPoolExecutor
from unittest.mock import patch

import pytest
from pydantic import ValidationError

from deerflow.config.model_config import RequestAdmissionConfig
from deerflow.models import request_admission as admission


@pytest.fixture
def clock(monkeypatch):
    now = [0.0]
    monkeypatch.setattr(admission, "monotonic", lambda: now[0])
    return now


@pytest.fixture
def registry(monkeypatch):
    monkeypatch.setattr(admission, "_registry", {})


def test_pacing_has_no_catch_up_burst(clock):
    limiter = admission.RequestAdmission(
        RequestAdmissionConfig(requests_per_minute=60)
    )

    assert limiter.acquire(blocking=False)
    assert not limiter.acquire(blocking=False)
    clock[0] = 0.999
    assert not limiter.acquire(blocking=False)
    clock[0] = 1
    assert limiter.acquire(blocking=False)
    clock[0] = 100
    assert limiter.acquire(blocking=False)
    assert not limiter.acquire(blocking=False)


def test_high_rpm_wait_tracks_next_admission(clock):
    limiter = admission.RequestAdmission(
        RequestAdmissionConfig(requests_per_minute=6000)
    )
    limiter.acquire()

    assert limiter._delay(300) == pytest.approx(0.01)
    clock[0] = 0.009
    assert limiter._delay(300) == pytest.approx(0.001)
    clock[0] = 0.02
    assert 0 < limiter._delay(300) <= 0.01


@pytest.mark.asyncio
async def test_fifo_cancellation_and_queue_capacity(clock):
    limiter = admission.RequestAdmission(
        RequestAdmissionConfig(requests_per_minute=60, max_queue_size=2)
    )
    await limiter.aacquire()
    first = asyncio.create_task(limiter.aacquire())
    second = asyncio.create_task(limiter.aacquire())
    await asyncio.sleep(0)

    try:
        with pytest.raises(admission.AdmissionError, match="queue is full"):
            await limiter.aacquire()
        assert not limiter.acquire(blocking=False)

        first.cancel()
        with pytest.raises(asyncio.CancelledError):
            await first

        clock[0] = 1
        assert await asyncio.wait_for(second, 1)
        assert not limiter._waiters
    finally:
        for task in (first, second):
            task.cancel()
        await asyncio.gather(first, second, return_exceptions=True)


@pytest.mark.asyncio
async def test_wait_deadline_does_not_spend_admission(clock):
    limiter = admission.RequestAdmission(
        RequestAdmissionConfig(requests_per_minute=1, max_wait_seconds=1)
    )
    await limiter.aacquire()
    waiter = asyncio.create_task(limiter.aacquire())
    await asyncio.sleep(0)

    clock[0] = 2
    with pytest.raises(admission.AdmissionError, match="timed out"):
        await asyncio.wait_for(waiter, 1)
    assert not limiter._waiters

    clock[0] = 60
    assert limiter.acquire(blocking=False)


def test_sync_timeout_removes_waiter(clock, monkeypatch):
    limiter = admission.RequestAdmission(
        RequestAdmissionConfig(requests_per_minute=1, max_wait_seconds=1)
    )
    limiter.acquire()
    monkeypatch.setattr(admission.time, "sleep", lambda _: clock.__setitem__(0, 2))

    with pytest.raises(admission.AdmissionError, match="timed out"):
        limiter.acquire()
    assert not limiter._waiters


def test_sync_and_independent_event_loops_share_one_budget(clock):
    limiter = admission.RequestAdmission(
        RequestAdmissionConfig(requests_per_minute=1)
    )
    barrier = threading.Barrier(8)

    def attempt(index):
        barrier.wait(timeout=5)
        if index % 2:
            return limiter.acquire(blocking=False)
        return asyncio.run(limiter.aacquire(blocking=False))

    with ThreadPoolExecutor(max_workers=8) as executor:
        assert sum(executor.map(attempt, range(8))) == 1


@pytest.mark.asyncio
async def test_fifo_prevents_newcomers_overtaking(clock):
    limiter = admission.RequestAdmission(
        RequestAdmissionConfig(requests_per_minute=60)
    )
    limiter.acquire()
    order = []

    async def wait(index):
        await limiter.aacquire()
        order.append(index)

    first = asyncio.create_task(wait(1))
    second = asyncio.create_task(wait(2))
    await asyncio.sleep(0)
    try:
        clock[0] = 1
        assert not limiter.acquire(blocking=False)
        await asyncio.wait_for(first, 1)
        assert order == [1]

        clock[0] = 2
        await asyncio.wait_for(second, 1)
        assert order == [1, 2]
    finally:
        first.cancel()
        second.cancel()
        await asyncio.gather(first, second, return_exceptions=True)


def test_blocking_waiter_reserves_fifo_position_atomically(clock):
    limiter = admission.RequestAdmission(
        RequestAdmissionConfig(requests_per_minute=60)
    )
    assert limiter.acquire(blocking=False)

    acquired, ticket = limiter._try_or_enqueue(blocking=True)
    assert acquired is False
    assert ticket is limiter._waiters[0]

    clock[0] = 1
    assert limiter.acquire(blocking=False) is False

    limiter._remove(ticket)
    assert limiter.acquire(blocking=False) is True


def test_nonblocking_probe_never_joins_fifo(clock):
    limiter = admission.RequestAdmission(
        RequestAdmissionConfig(requests_per_minute=60)
    )
    assert limiter.acquire(blocking=False)

    acquired, ticket = limiter._try_or_enqueue(blocking=False)

    assert acquired is False
    assert ticket is None
    assert not limiter._waiters


@pytest.mark.parametrize(
    "values",
    [
        {"requests_per_minute": 0},
        {"requests_per_minute": True},
        {"requests_per_minute": 1, "max_wait_seconds": float("inf")},
        {"requests_per_minute": 1, "max_queue_size": 0},
        {"requests_per_minute": 1, "group": "invalid group"},
        {"requests_per_minute": 1, "unknown": True},
    ],
)
def test_invalid_configuration(values):
    with pytest.raises(ValidationError):
        RequestAdmissionConfig(**values)


def test_group_sharing_isolation_and_conflict(registry, clock):
    config = RequestAdmissionConfig(requests_per_minute=60, group="shared")
    shared = admission.get_request_admission("a", config)

    assert admission.get_request_admission("b", config) is shared
    assert shared.acquire(blocking=False)
    assert not admission.get_request_admission("b", config).acquire(blocking=False)

    # Explicit groups and implicit model-name budgets use separate namespaces.
    isolated = admission.get_request_admission(
        "shared",
        RequestAdmissionConfig(requests_per_minute=60),
    )
    assert isolated.acquire(blocking=False)

    with pytest.raises(ValueError, match="restart"):
        admission.get_request_admission(
            "a",
            config.model_copy(update={"requests_per_minute": 30}),
        )


def _make_model(
    monkeypatch,
    *,
    name="a",
    policy=None,
    provider=False,
    resolved_class=None,
    **kwargs,
):
    from langchain_core.language_models.fake_chat_models import FakeListChatModel

    from deerflow.config.app_config import AppConfig
    from deerflow.config.model_config import ModelConfig
    from deerflow.config.sandbox_config import SandboxConfig
    from deerflow.models import factory

    model = ModelConfig(
        name=name,
        use="langchain_openai:ChatOpenAI",
        model="test",
        api_key="offline-test-key",
        request_admission=policy,
    )
    config = AppConfig(
        models=[model],
        sandbox=SandboxConfig(use="test"),
    )
    contexts = [
        patch.object(factory, "get_app_config", return_value=config),
        patch.object(factory, "build_tracing_callbacks", return_value=[]),
    ]
    if resolved_class is not None:
        contexts.append(
            patch.object(factory, "resolve_class", return_value=resolved_class)
        )
    elif not provider:
        contexts.append(
            patch.object(factory, "resolve_class", return_value=FakeListChatModel)
        )

    for context in contexts:
        context.start()
    try:
        return factory.create_chat_model(
            name,
            **({} if provider and resolved_class is None else {"responses": ["ok"]}),
            **kwargs,
        )
    finally:
        for context in reversed(contexts):
            context.stop()


@pytest.mark.asyncio
async def test_factory_invoke_and_stream_share_budget(monkeypatch, registry, clock):
    policy = RequestAdmissionConfig(requests_per_minute=60, group="account")
    first = _make_model(monkeypatch, policy=policy)
    second = _make_model(monkeypatch, name="b", policy=policy)

    assert first.rate_limiter is second.rate_limiter
    assert first.invoke("hello").content == "ok"

    pending = asyncio.create_task(second.ainvoke("hello"))
    for _ in range(100):
        if first.rate_limiter._waiters:
            break
        await asyncio.sleep(0)
    try:
        assert first.rate_limiter._waiters
        assert not pending.done()

        clock[0] = 1
        assert (await asyncio.wait_for(pending, 1)).content == "ok"
        clock[0] = 2
        assert "".join(chunk.content for chunk in first.stream("hello")) == "ok"
        assert not first.rate_limiter.acquire(blocking=False)

        clock[0] = 3
        content = "".join(
            [chunk.content async for chunk in second.astream("hello")]
        )
        assert content == "ok"
        assert not first.rate_limiter.acquire(blocking=False)
    finally:
        pending.cancel()
        await asyncio.gather(pending, return_exceptions=True)


def test_openai_factory_strips_policy_and_disables_sdk_retries(
    monkeypatch,
    registry,
):
    model = _make_model(
        monkeypatch,
        policy=RequestAdmissionConfig(requests_per_minute=30),
        provider=True,
        max_retries=9,
    )

    assert isinstance(model.rate_limiter, admission.RequestAdmission)
    assert model.max_retries == 0
    assert "request_admission" not in model.model_kwargs
    assert "request_admission" not in model._default_params


def test_factory_disables_custom_provider_internal_retries(monkeypatch, registry):
    from langchain_core.language_models.fake_chat_models import FakeListChatModel

    class RetryingProvider(FakeListChatModel):
        retry_max_attempts: int = 3

    model = _make_model(
        monkeypatch,
        policy=RequestAdmissionConfig(requests_per_minute=30),
        resolved_class=RetryingProvider,
        retry_max_attempts=9,
    )

    assert isinstance(model.rate_limiter, admission.RequestAdmission)
    assert model.retry_max_attempts == 1


def test_factory_rejects_custom_rate_limiter_with_policy(monkeypatch, registry):
    custom = admission.RequestAdmission(
        RequestAdmissionConfig(requests_per_minute=120)
    )

    with pytest.raises(ValueError, match="custom rate_limiter"):
        _make_model(
            monkeypatch,
            policy=RequestAdmissionConfig(requests_per_minute=60),
            rate_limiter=custom,
        )


def test_disabled_factory_preserves_default(monkeypatch, registry):
    model = _make_model(monkeypatch)

    assert model.rate_limiter is None
    assert not admission._registry


def test_admission_failures_are_never_retried_as_provider_errors(monkeypatch):
    from deerflow.agents.middlewares import llm_error_handling_middleware as errors

    middleware = errors.LLMErrorHandlingMiddleware()
    for message in (
        "LLM admission queue is full; reduce workload or increase queue capacity.",
        "LLM admission timed out before dispatch; increase max_wait_seconds or reduce workload.",
        "rate limit exceeded locally",
        "provider quota",
        "server busy",
    ):
        retry, reason = middleware._classify_error(admission.AdmissionError(message))
        assert retry is False
        assert reason == "admission"
