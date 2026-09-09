"""Tests for summarization model resolution and failure degradation.

Covers the two defects behind the summarization hardening work:

1. The summarization model used to resolve unconditionally to ``models[0]``
   even though the run's model is chosen per request, so a broken provider on
   ``models[0]`` made every compression fail regardless of which model was
   actually answering the run.
2. LangChain's ``SummarizationMiddleware`` catches summarization invocation
   errors and returns ``f"Error generating summary: {e!s}"`` as if it were a
   real summary. That string was then used to replace the conversation
   history, silently destroying the messages it had failed to summarize.
"""

import logging
import time
from types import SimpleNamespace
from unittest.mock import patch

import pytest
from langchain_core.messages import AIMessage, HumanMessage, ToolMessage
from pydantic import ValidationError

from deerflow.agents.lead_agent import agent as lead_agent
from deerflow.agents.middlewares.summarization_middleware import DeerFlowSummarizationMiddleware
from deerflow.config.summarization_config import ContextSize, SummarizationConfig


class _FakeModel:
    """Chat model stand-in that either returns a summary or raises."""

    def __init__(self, *, text: str = "a summary", error: Exception | None = None) -> None:
        self._text = text
        self._error = error
        self.calls = 0
        self.prompts = []

    def invoke(self, _prompt, config=None):  # noqa: ANN001, ARG002
        self.calls += 1
        self.prompts.append(_prompt)
        if self._error is not None:
            raise self._error
        return SimpleNamespace(text=self._text)

    async def ainvoke(self, _prompt, config=None):  # noqa: ANN001, ARG002
        self.calls += 1
        self.prompts.append(_prompt)
        if self._error is not None:
            raise self._error
        return SimpleNamespace(text=self._text)


def _token_counter(messages) -> int:  # noqa: ANN001
    """Deterministic counter so triggers never depend on real tokenization."""
    return 100 * len(list(messages))


def _middleware(model: _FakeModel, **kwargs) -> DeerFlowSummarizationMiddleware:
    """Build the middleware with message-count triggers and no trimming."""
    return DeerFlowSummarizationMiddleware(
        model=model,
        trigger=("messages", 6),
        keep=("messages", 2),
        token_counter=_token_counter,
        trim_tokens_to_summarize=None,
        **kwargs,
    )


def _state(count: int = 8) -> dict:
    """Alternating human/AI history long enough to trip the trigger."""
    messages = []
    for i in range(count):
        if i % 2 == 0:
            messages.append(HumanMessage(content=f"question {i}"))
        else:
            messages.append(AIMessage(content=f"answer {i}"))
    return {"messages": messages}


def _tool_heavy_state(*, include_internal_reminder: bool = False) -> dict:
    """A single active user turn whose request falls behind a tool-heavy cutoff."""
    messages = [HumanMessage(content="CURRENT REQUEST", id="user-current")]
    if include_internal_reminder:
        messages.append(HumanMessage(content="internal reminder", name="todo_reminder", id="reminder"))
    for index in range(3):
        call_id = f"call-{index}"
        messages.extend(
            [
                AIMessage(
                    content="",
                    id=f"assistant-{index}",
                    tool_calls=[{"name": "search", "args": {"query": str(index)}, "id": call_id}],
                ),
                ToolMessage(content=f"result {index}", tool_call_id=call_id, id=f"tool-{index}"),
            ]
        )
    messages.append(AIMessage(content="working", id="assistant-final"))
    return {"messages": messages}


def _runtime() -> SimpleNamespace:
    return SimpleNamespace(context={"thread_id": "thread-1", "agent_name": None})


def _replacements(update: dict) -> list:
    """The messages the middleware substituted for the summarized span.

    Everything between the RemoveMessage and the preserved tail.
    """
    return [msg for msg in update["messages"][1:] if msg.additional_kwargs.get("lc_source")]


# --------------------------------------------------------------------------
# Change 1: which model summarizes a run
# --------------------------------------------------------------------------


def _app_config():
    models = [SimpleNamespace(name="run-model"), SimpleNamespace(name="default-model"), SimpleNamespace(name="cheap-model")]
    by_name = {model.name: model for model in models}
    return SimpleNamespace(
        models=models,
        default_model="default-model",
        get_model_config=by_name.get,
        get_default_model_name=lambda: "default-model",
        skills=SimpleNamespace(container_path="/mnt/skills"),
    )


def _build_middleware_kwargs(*, config_model_name: str | None, run_model_name: str | None) -> tuple[list, dict]:
    """Run the factory with everything stubbed; return (create_chat_model names, middleware kwargs)."""
    summarization_config = SummarizationConfig(
        enabled=True,
        model_name=config_model_name,
        trigger=ContextSize(type="messages", value=50),
    )
    created_names: list = []
    captured: dict = {}

    def _capture(**kwargs):
        captured.update(kwargs)
        return "middleware"

    with (
        patch.object(lead_agent, "get_summarization_config", return_value=summarization_config),
        patch.object(lead_agent, "get_app_config", return_value=_app_config()),
        patch.object(lead_agent, "get_memory_config", return_value=SimpleNamespace(enabled=False)),
        patch.object(lead_agent, "create_chat_model", side_effect=lambda name=None, **_: created_names.append(name)),
        patch.object(lead_agent, "DeerFlowSummarizationMiddleware", _capture),
    ):
        lead_agent._create_summarization_middleware(run_model_name=run_model_name)

    return created_names, captured


def test_summarization_follows_run_model_when_not_configured() -> None:
    """model_name=None must summarize with the run's model, not models[0]."""
    created_names, captured = _build_middleware_kwargs(config_model_name=None, run_model_name="run-model")

    assert created_names == ["run-model"]
    # Retrying on the same model would be pointless, so no tier-2 fallback.
    assert captured["fallback_model_name"] is None


def test_summarization_prefers_explicit_config_model_over_run_model() -> None:
    """An explicit config model_name still wins, with the run model as fallback."""
    created_names, captured = _build_middleware_kwargs(config_model_name="cheap-model", run_model_name="run-model")

    assert created_names == ["cheap-model"]
    assert captured["fallback_model_name"] == "run-model"


def test_summarization_ignores_unconfigured_run_model() -> None:
    """An unresolvable run model degrades to the default instead of being trusted."""
    created_names, captured = _build_middleware_kwargs(config_model_name=None, run_model_name="not-in-config")

    assert created_names == [None]  # create_chat_model(None) -> configured default
    assert captured["fallback_model_name"] is None


def test_summarization_skips_fallback_when_run_model_is_the_default() -> None:
    """Explicitly configuring the default model while running it must not self-retry."""
    created_names, captured = _build_middleware_kwargs(config_model_name="default-model", run_model_name="default-model")

    assert created_names == ["default-model"]
    assert captured["fallback_model_name"] is None


def test_summarization_invalid_config_model_falls_back_to_run_model(caplog: pytest.LogCaptureFixture) -> None:
    """A hand-edited config.yaml naming an unknown model must degrade, not hard-fail agent construction.

    `admin.py` validates model names on writes through the API, but
    config.yaml can still be edited by hand with a stale or typo'd name.
    Tier 1 (config.model_name) must be validated the same way tier 2
    (run_model_name) already is, instead of handing an unknown name straight
    to `create_chat_model`, which raises.
    """
    with caplog.at_level(logging.WARNING):
        created_names, captured = _build_middleware_kwargs(config_model_name="not-in-config", run_model_name="run-model")

    assert created_names == ["run-model"]
    assert captured["fallback_model_name"] is None
    assert any("not-in-config" in record.message and "run-model" in record.message for record in caplog.records)


def test_summarization_invalid_config_model_without_run_model_falls_back_to_default(caplog: pytest.LogCaptureFixture) -> None:
    """With no valid run model either, an invalid config.model_name must use the configured default."""
    with caplog.at_level(logging.WARNING):
        created_names, captured = _build_middleware_kwargs(config_model_name="not-in-config", run_model_name=None)

    assert created_names == [None]  # create_chat_model(None) -> configured default
    assert captured["fallback_model_name"] is None
    assert any("not-in-config" in record.message and "default" in record.message for record in caplog.records)


@pytest.mark.parametrize(
    ("size_type", "value"),
    [
        ("fraction", 0),
        ("fraction", 1.01),
        ("fraction", float("nan")),
        ("fraction", float("inf")),
        ("tokens", 0),
        ("messages", 1.5),
    ],
)
def test_context_size_rejects_dead_or_crashing_thresholds(size_type: str, value: float) -> None:
    with pytest.raises(ValidationError):
        ContextSize(type=size_type, value=value)


def _capture_fraction_config(
    *,
    trigger: ContextSize | list[ContextSize],
    keep: ContextSize,
    model_profile: dict | None,
) -> tuple[dict, list[logging.LogRecord]]:
    config = SummarizationConfig(
        enabled=True,
        trigger=trigger,
        keep=keep,
    )
    captured: dict = {}
    records: list[logging.LogRecord] = []

    class _CaptureHandler(logging.Handler):
        def emit(self, record: logging.LogRecord) -> None:
            records.append(record)

    handler = _CaptureHandler()
    logger = logging.getLogger(lead_agent.__name__)
    logger.addHandler(handler)
    try:
        with (
            patch.object(lead_agent, "get_summarization_config", return_value=config),
            patch.object(lead_agent, "get_app_config", return_value=_app_config()),
            patch.object(lead_agent, "get_memory_config", return_value=SimpleNamespace(enabled=False)),
            patch.object(
                lead_agent,
                "create_chat_model",
                return_value=SimpleNamespace(profile=model_profile),
            ),
            patch.object(
                lead_agent,
                "DeerFlowSummarizationMiddleware",
                side_effect=lambda **kwargs: captured.update(kwargs) or "middleware",
            ),
        ):
            assert lead_agent._create_summarization_middleware("run-model") == "middleware"
    finally:
        logger.removeHandler(handler)
    return captured, records


def test_fraction_thresholds_survive_when_model_profile_is_available() -> None:
    captured, _ = _capture_fraction_config(
        trigger=ContextSize(type="fraction", value=0.8),
        keep=ContextSize(type="fraction", value=0.25),
        model_profile={"max_input_tokens": 128000},
    )

    assert captured["trigger"] == ("fraction", 0.8)
    assert captured["keep"] == ("fraction", 0.25)


def test_unusable_fraction_thresholds_degrade_without_breaking_agent_build() -> None:
    captured, records = _capture_fraction_config(
        trigger=[
            ContextSize(type="fraction", value=0.8),
            ContextSize(type="messages", value=40),
        ],
        keep=ContextSize(type="fraction", value=0.25),
        model_profile=None,
    )

    assert captured["trigger"] == [("messages", 40)]
    assert captured["keep"] == ("messages", 20)
    assert any("fraction trigger" in record.getMessage() for record in records)


def test_fraction_only_trigger_becomes_never_firing_when_profile_is_missing() -> None:
    captured, records = _capture_fraction_config(
        trigger=ContextSize(type="fraction", value=0.8),
        keep=ContextSize(type="messages", value=20),
        model_profile=None,
    )

    assert captured["trigger"] is None
    assert any("auto-compaction will not fire" in record.getMessage() for record in records)


# --------------------------------------------------------------------------
# Change 2: failure degradation
# --------------------------------------------------------------------------


def test_successful_summary_replaces_history_with_the_summary() -> None:
    middleware = _middleware(_FakeModel(text="the summary"))

    update = middleware.before_model(_state(), _runtime())

    assert update is not None
    (replacement,) = _replacements(update)
    assert replacement.additional_kwargs["lc_source"] == "summarization"
    assert "the summary" in replacement.content


def test_latest_user_request_survives_tool_heavy_summarization() -> None:
    """The live request must not be compressed while its tool turn is still running."""
    seen: list = []
    middleware = _middleware(_FakeModel(), before_summarization=[lambda event: seen.append(event)])

    update = middleware.before_model(_tool_heavy_state(), _runtime())

    assert update is not None
    (event,) = seen
    assert "CURRENT REQUEST" not in [message.content for message in event.messages_to_summarize]
    assert "CURRENT REQUEST" in [message.content for message in event.preserved_messages]
    assert "CURRENT REQUEST" in [message.content for message in update["messages"]]


def test_internal_human_message_is_not_mistaken_for_current_user() -> None:
    """Named middleware reminders must not displace the actual user request."""
    seen: list = []
    middleware = _middleware(_FakeModel(), before_summarization=[lambda event: seen.append(event)])

    update = middleware.before_model(_tool_heavy_state(include_internal_reminder=True), _runtime())

    assert update is not None
    (event,) = seen
    preserved_contents = [message.content for message in event.preserved_messages]
    summarized_contents = [message.content for message in event.messages_to_summarize]
    assert "CURRENT REQUEST" in preserved_contents
    assert "internal reminder" in summarized_contents


def test_ai_tool_only_compaction_keeps_latest_tool_result_in_summary() -> None:
    model = _FakeModel()
    middleware = DeerFlowSummarizationMiddleware(
        model=model,
        trigger=("messages", 6),
        keep=("messages", 2),
        token_counter=_token_counter,
        trim_tokens_to_summarize=200,
    )
    messages = [
        AIMessage(content="", tool_calls=[{"name": "search", "args": {}, "id": "old"}]),
        ToolMessage(content="OLD RESULT", tool_call_id="old"),
        AIMessage(content="", tool_calls=[{"name": "search", "args": {}, "id": "new"}]),
        ToolMessage(content="LATEST RESULT", tool_call_id="new"),
    ]

    assert middleware._create_summary(messages) == "a summary"
    assert "LATEST RESULT" in model.prompts[0]
    assert "Previous conversation was too long to summarize." not in model.prompts[0]


async def test_latest_user_request_survives_tool_heavy_summarization_async() -> None:
    """Async execution must preserve the same active-request invariant."""
    middleware = _middleware(_FakeModel())

    update = await middleware.abefore_model(_tool_heavy_state(), _runtime())

    assert update is not None
    assert "CURRENT REQUEST" in [message.content for message in update["messages"]]


def test_failed_summary_is_not_used_as_summary_content() -> None:
    """A provider error must never be laundered into the history as a summary."""
    middleware = _middleware(_FakeModel(error=RuntimeError("401 invalid api key")))

    update = middleware.before_model(_state(), _runtime())

    assert update is not None
    (replacement,) = _replacements(update)
    # The upstream bug produced "Error generating summary: ..." here.
    assert replacement.additional_kwargs["lc_source"] == "summarization_fallback"
    assert "Error generating summary" not in replacement.content
    assert "401 invalid api key" not in replacement.content
    assert "summarization service was unavailable" in replacement.content


def test_failed_summary_still_bounds_the_context() -> None:
    """Degrading must still drop the old span, or context grows without bound."""
    state = _state(8)
    middleware = _middleware(_FakeModel(error=RuntimeError("boom")))

    update = middleware.before_model(state, _runtime())

    assert update is not None
    # 8 messages in, keep=2: one placeholder plus the preserved tail.
    assert len(update["messages"]) < len(state["messages"])
    assert [msg.content for msg in update["messages"][-2:]] == ["question 6", "answer 7"]


def test_fallback_model_retried_once_when_primary_fails() -> None:
    primary = _FakeModel(error=RuntimeError("primary down"))
    fallback = _FakeModel(text="fallback summary")
    middleware = _middleware(primary, fallback_model_name="run-model")

    with patch("deerflow.models.create_chat_model", return_value=fallback):
        update = middleware.before_model(_state(), _runtime())

    assert update is not None
    (replacement,) = _replacements(update)
    assert replacement.additional_kwargs["lc_source"] == "summarization"
    assert "fallback summary" in replacement.content
    assert primary.calls == 1
    assert fallback.calls == 1


def test_summarization_failure_does_not_propagate() -> None:
    """An unexpected error skips compression for the turn instead of failing the run."""
    middleware = _middleware(_FakeModel())

    with patch.object(middleware, "_summarize_with_tiers", side_effect=RuntimeError("unexpected")):
        assert middleware.before_model(_state(), _runtime()) is None


def test_hooks_fire_with_the_messages_that_get_removed() -> None:
    seen: list = []
    middleware = _middleware(_FakeModel(), before_summarization=[lambda event: seen.append(event)])

    update = middleware.before_model(_state(8), _runtime())

    assert update is not None
    (event,) = seen
    assert event.thread_id == "thread-1"
    # Hooks must observe exactly the span that the returned update deletes.
    assert [msg.content for msg in event.messages_to_summarize] == [f"{'question' if i % 2 == 0 else 'answer'} {i}" for i in range(6)]
    assert [msg.content for msg in event.preserved_messages] == ["question 6", "answer 7"]


def test_llm_is_not_called_while_the_circuit_is_open() -> None:
    """An open circuit degrades without burning further LLM calls."""
    model = _FakeModel(error=RuntimeError("down"))
    middleware = _middleware(model, max_consecutive_failures=2, circuit_recovery_timeout_sec=300)

    for _ in range(2):
        middleware.before_model(_state(), _runtime())
    assert model.calls == 2

    update = middleware.before_model(_state(), _runtime())
    assert model.calls == 2
    (replacement,) = _replacements(update)
    assert replacement.additional_kwargs["lc_source"] == "summarization_fallback"


def test_summarization_recovers_after_the_provider_comes_back() -> None:
    """Degradation must be bounded in time, not permanent for the instance's life.

    Regression test for a latch bug: resetting the failure count only on a
    successful summary is unreachable once the breaker short-circuits every
    model call, which pinned the instance to placeholder-dropping forever.
    """
    model = _FakeModel(error=RuntimeError("down"))
    middleware = _middleware(model, max_consecutive_failures=2, circuit_recovery_timeout_sec=300)

    for _ in range(3):  # trip the breaker open
        middleware.before_model(_state(), _runtime())
    assert middleware._circuit_state == "open"

    # Recovery window elapses and the provider is healthy again.
    middleware._circuit_open_until = time.time() - 1
    model._error = None
    model._text = "recovered summary"

    update = middleware.before_model(_state(), _runtime())

    (replacement,) = _replacements(update)
    assert replacement.additional_kwargs["lc_source"] == "summarization"
    assert "recovered summary" in replacement.content
    # A successful probe must fully close the breaker, not leave it half-open.
    assert middleware._circuit_state == "closed"
    assert middleware._circuit_failure_count == 0


def test_only_one_probe_is_admitted_per_recovery_window() -> None:
    """Half-open must admit exactly one probe, and a failed probe must reopen."""
    model = _FakeModel(error=RuntimeError("down"))
    middleware = _middleware(model, max_consecutive_failures=1, circuit_recovery_timeout_sec=300)

    middleware.before_model(_state(), _runtime())
    assert middleware._circuit_state == "open"
    calls_when_opened = model.calls

    middleware._circuit_open_until = time.time() - 1

    # First call after the window is the probe; it fails and reopens the circuit.
    middleware.before_model(_state(), _runtime())
    assert model.calls == calls_when_opened + 1
    assert middleware._circuit_state == "open"

    # Still open, so no further model calls leak through.
    middleware.before_model(_state(), _runtime())
    assert model.calls == calls_when_opened + 1


def test_unexpected_exception_during_half_open_probe_reopens_the_circuit() -> None:
    """A probe that fails on an unexpected bug (not a modeled LLM failure) must
    still resolve the circuit, or a leaked `_circuit_probe_in_flight` would
    wedge the breaker in "half_open" forever with no further recovery check
    — the open branch never runs again and `_circuit_open_until` is never
    re-checked, so every future call would take the half-open fast path and
    skip the LLM permanently.
    """
    model = _FakeModel(error=RuntimeError("down"))
    middleware = _middleware(model, max_consecutive_failures=1, circuit_recovery_timeout_sec=300)

    middleware.before_model(_state(), _runtime())
    assert middleware._circuit_state == "open"

    # Recovery window elapses; the next call is the admitted half-open probe.
    middleware._circuit_open_until = time.time() - 1
    model._error = None
    model._text = "recovered summary"

    with patch.object(middleware, "_build_new_messages", side_effect=TypeError("bad message shape")):
        # ① The unexpected exception must not bubble past before_model.
        assert middleware.before_model(_state(), _runtime()) is None

    # ② The probe must be fully settled: back to open, not dangling half_open.
    assert middleware._circuit_state == "open"
    assert middleware._circuit_probe_in_flight is False

    # ③ Not a permanent latch: once another recovery window elapses, a fresh
    # probe can still be admitted.
    middleware._circuit_open_until = time.time() - 1
    update = middleware.before_model(_state(), _runtime())

    # ④ The probe-success path still closes the breaker normally.
    assert update is not None
    (replacement,) = _replacements(update)
    assert replacement.additional_kwargs["lc_source"] == "summarization"
    assert "recovered summary" in replacement.content
    assert middleware._circuit_state == "closed"
    assert middleware._circuit_probe_in_flight is False


async def test_unexpected_exception_during_half_open_probe_reopens_the_circuit_async() -> None:
    """Async twin of the sync half-open exception-leak regression test above."""
    model = _FakeModel(error=RuntimeError("down"))
    middleware = _middleware(model, max_consecutive_failures=1, circuit_recovery_timeout_sec=300)

    await middleware.abefore_model(_state(), _runtime())
    assert middleware._circuit_state == "open"

    middleware._circuit_open_until = time.time() - 1
    model._error = None
    model._text = "recovered async summary"

    with patch.object(middleware, "_build_new_messages", side_effect=TypeError("bad message shape")):
        assert await middleware.abefore_model(_state(), _runtime()) is None

    assert middleware._circuit_state == "open"
    assert middleware._circuit_probe_in_flight is False

    middleware._circuit_open_until = time.time() - 1
    update = await middleware.abefore_model(_state(), _runtime())

    assert update is not None
    (replacement,) = _replacements(update)
    assert replacement.additional_kwargs["lc_source"] == "summarization"
    assert "recovered async summary" in replacement.content
    assert middleware._circuit_state == "closed"
    assert middleware._circuit_probe_in_flight is False


async def test_async_path_degrades_like_the_sync_path() -> None:
    """abefore_model must stay in lock-step with before_model."""
    middleware = _middleware(_FakeModel(error=RuntimeError("boom")))

    update = await middleware.abefore_model(_state(), _runtime())

    assert update is not None
    (replacement,) = _replacements(update)
    assert replacement.additional_kwargs["lc_source"] == "summarization_fallback"
    assert "Error generating summary" not in replacement.content


async def test_async_fallback_model_retried_once() -> None:
    primary = _FakeModel(error=RuntimeError("primary down"))
    fallback = _FakeModel(text="async fallback summary")
    middleware = _middleware(primary, fallback_model_name="run-model")

    with patch("deerflow.models.create_chat_model", return_value=fallback):
        update = await middleware.abefore_model(_state(), _runtime())

    assert update is not None
    (replacement,) = _replacements(update)
    assert "async fallback summary" in replacement.content
    assert fallback.calls == 1
