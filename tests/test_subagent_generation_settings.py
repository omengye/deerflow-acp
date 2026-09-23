"""Tests for per-agent generation/sampling settings (model_settings, thinking_enabled,
reasoning_effort) across the subagent config chain: config schema -> registry merge ->
executor kwargs -> factory graceful degrade.

All model construction is mocked; no real HTTP/LLM calls are made.
"""

import logging
import subprocess
import sys
from pathlib import Path
from typing import ClassVar

import pytest
from pydantic import ValidationError

import deerflow.config.subagents_config as subagents_config_module
import deerflow.models.factory as factory_module
import deerflow.subagents.executor as executor_module
from deerflow.config.app_config import AppConfig
from deerflow.config.model_config import ModelConfig
from deerflow.config.sandbox_config import SandboxConfig
from deerflow.config.subagents_config import (
    CustomSubagentConfig,
    ModelSettingsConfig,
    SubagentOverrideConfig,
    SubagentsAppConfig,
)
from deerflow.subagents.config import SubagentConfig
from deerflow.subagents.executor import SubagentExecutor
from deerflow.subagents.registry import get_subagent_config

# ---------------------------------------------------------------------------
# 1. SubagentsAppConfig getters' semantics
# ---------------------------------------------------------------------------


def test_subagents_app_config_generation_getters() -> None:
    config = SubagentsAppConfig(
        agents={
            "general-purpose": SubagentOverrideConfig(
                model_settings=ModelSettingsConfig(temperature=0.2, max_tokens=100),
                thinking_enabled=True,
                reasoning_effort="high",
            ),
            "bash": SubagentOverrideConfig(),  # override entry exists but sets nothing
        }
    )

    assert config.get_model_settings_for("general-purpose") == ModelSettingsConfig(temperature=0.2, max_tokens=100)
    assert config.get_thinking_enabled_for("general-purpose") is True
    assert config.get_reasoning_effort_for("general-purpose") == "high"

    # Override entry present but fields unset -> None (inherit)
    assert config.get_model_settings_for("bash") is None
    assert config.get_thinking_enabled_for("bash") is None
    assert config.get_reasoning_effort_for("bash") is None

    # No override entry at all -> None (inherit)
    assert config.get_model_settings_for("unknown") is None
    assert config.get_thinking_enabled_for("unknown") is None
    assert config.get_reasoning_effort_for("unknown") is None


# ---------------------------------------------------------------------------
# 2. ModelSettingsConfig validation (extra="forbid", ge/le ranges)
# ---------------------------------------------------------------------------


def test_model_settings_config_accepts_valid_values() -> None:
    settings = ModelSettingsConfig(temperature=0.7, max_tokens=2048)
    assert settings.temperature == 0.7
    assert settings.max_tokens == 2048

    # All-None is the "no override" default.
    empty = ModelSettingsConfig()
    assert empty.temperature is None
    assert empty.max_tokens is None


def test_model_settings_config_rejects_unknown_field() -> None:
    # extra="forbid": a typo (e.g. "tempurature") must fail loudly at config-load
    # time instead of silently being dropped, unlike per-model settings which use
    # extra="allow" for provider-specific kwargs.
    with pytest.raises(ValidationError):
        ModelSettingsConfig(tempurature=0.5)


@pytest.mark.parametrize(
    "kwargs",
    [
        {"temperature": 2.1},
        {"temperature": -0.1},
        {"max_tokens": 0},
        {"max_tokens": -1},
    ],
)
def test_model_settings_config_rejects_out_of_range_values(kwargs) -> None:
    with pytest.raises(ValidationError):
        ModelSettingsConfig(**kwargs)


# ---------------------------------------------------------------------------
# 3. Registry merge: per-agent overrides applied on top of builtin + custom agents
# ---------------------------------------------------------------------------


def test_registry_merges_generation_overrides_onto_builtin_and_custom(monkeypatch: pytest.MonkeyPatch) -> None:
    app_config = SubagentsAppConfig(
        agents={
            "general-purpose": SubagentOverrideConfig(
                model_settings=ModelSettingsConfig(temperature=0.9),
                thinking_enabled=True,
                reasoning_effort="low",
            )
        },
        custom_agents={
            "my-custom": CustomSubagentConfig(
                description="d",
                system_prompt="p",
                model_settings=ModelSettingsConfig(max_tokens=555),
                thinking_enabled=False,
                reasoning_effort="high",
            )
        },
    )
    monkeypatch.setattr(subagents_config_module, "_subagents_config", app_config)

    builtin = get_subagent_config("general-purpose")
    assert builtin is not None
    assert builtin.model_settings == {"temperature": 0.9}
    assert builtin.thinking_enabled is True
    assert builtin.reasoning_effort == "low"

    custom = get_subagent_config("my-custom")
    assert custom is not None
    assert custom.model_settings == {"max_tokens": 555}
    assert custom.thinking_enabled is False
    assert custom.reasoning_effort == "high"

    # A subsequent agents-section override for the custom agent layers on top,
    # only replacing the fields it explicitly sets (mirrors model/skills).
    app_config.agents["my-custom"] = SubagentOverrideConfig(reasoning_effort="medium")
    custom_overridden = get_subagent_config("my-custom")
    assert custom_overridden is not None
    assert custom_overridden.reasoning_effort == "medium"
    assert custom_overridden.thinking_enabled is False
    assert custom_overridden.model_settings == {"max_tokens": 555}


# ---------------------------------------------------------------------------
# 4 & 5. Executor: kwargs passthrough to create_chat_model + thinking_enabled
#         three-state (None/True/False) precedence over the parent's value.
# ---------------------------------------------------------------------------


class _StopAfterModelCreation(Exception):
    """Raised by the fake create_chat_model to short-circuit _create_agent
    right after capturing its call kwargs, without needing the rest of
    _create_agent's middleware/graph wiring to actually succeed."""


def _make_capturing_create_chat_model(captured: dict):
    def _fake(**kwargs):
        captured.update(kwargs)
        raise _StopAfterModelCreation()

    return _fake


def test_executor_passes_model_settings_and_reasoning_effort_as_kwargs(monkeypatch: pytest.MonkeyPatch) -> None:
    captured: dict = {}
    monkeypatch.setattr(executor_module, "create_chat_model", _make_capturing_create_chat_model(captured))

    config = SubagentConfig(
        name="general-purpose",
        description="d",
        system_prompt="p",
        model="some-model",
        model_settings={"temperature": 0.4, "max_tokens": 500},
        reasoning_effort="high",
        thinking_enabled=None,  # no override -> inherit parent's value
    )
    executor = SubagentExecutor(config=config, tools=[], parent_model="parent-model", thinking_enabled=True)

    with pytest.raises(_StopAfterModelCreation):
        executor._create_agent()

    assert captured["name"] == "some-model"
    assert captured["thinking_enabled"] is True  # inherited from parent
    assert captured["disable_keepalive"] is True
    assert captured["temperature"] == 0.4
    assert captured["max_tokens"] == 500
    assert captured["reasoning_effort"] == "high"


@pytest.mark.parametrize(
    ("override_thinking", "parent_thinking", "expected"),
    [
        (None, True, True),  # no override -> inherit parent's True
        (None, False, False),  # no override -> inherit parent's False
        (True, False, True),  # explicit True overrides parent False
        (False, True, False),  # explicit False overrides parent True
    ],
)
def test_executor_thinking_enabled_three_state_precedence(
    monkeypatch: pytest.MonkeyPatch,
    override_thinking: bool | None,
    parent_thinking: bool,
    expected: bool,
) -> None:
    captured: dict = {}
    monkeypatch.setattr(executor_module, "create_chat_model", _make_capturing_create_chat_model(captured))

    config = SubagentConfig(
        name="general-purpose",
        description="d",
        system_prompt="p",
        model="some-model",
        thinking_enabled=override_thinking,
    )
    executor = SubagentExecutor(config=config, tools=[], thinking_enabled=parent_thinking)

    with pytest.raises(_StopAfterModelCreation):
        executor._create_agent()

    assert captured["thinking_enabled"] is expected


def test_executor_injects_current_date_into_system_prompt(monkeypatch: pytest.MonkeyPatch) -> None:
    from datetime import datetime

    import deerflow.agents.date_context as date_context_module
    import deerflow.agents.middlewares.finish_reason_middleware as finish_reason_module
    import deerflow.agents.middlewares.loop_detection_middleware as loop_detection_module
    import deerflow.agents.middlewares.tool_error_handling_middleware as tool_error_module

    class _FrozenDateTime:
        @classmethod
        def now(cls) -> datetime:
            return datetime(2042, 3, 4)

    captured: dict = {}

    def _capture_agent(**kwargs):
        captured.update(kwargs)
        return object()

    monkeypatch.setattr(date_context_module, "datetime", _FrozenDateTime)
    app_config = AppConfig(
        sandbox=SandboxConfig(use="test"),
        models=[ModelConfig(name="test-model", use="test:FakeModel", model="test-model")],
    )
    monkeypatch.setattr(executor_module, "get_app_config", lambda: app_config)
    monkeypatch.setattr(executor_module, "create_chat_model", lambda **_kwargs: object())
    monkeypatch.setattr(executor_module, "create_agent", _capture_agent)
    monkeypatch.setattr(tool_error_module, "build_subagent_runtime_middlewares", lambda **_kwargs: [])
    monkeypatch.setattr(loop_detection_module, "LoopDetectionMiddleware", lambda **_kwargs: object())
    monkeypatch.setattr(loop_detection_module, "count_steps_per_turn", lambda _middlewares: 1)
    monkeypatch.setattr(loop_detection_module, "calibrate_loop_detection", lambda *_args: None)
    monkeypatch.setattr(finish_reason_module, "build_finish_reason_middlewares", lambda: [])

    executor = SubagentExecutor(
        config=SubagentConfig(name="general-purpose", description="d", system_prompt="subagent prompt"),
        tools=[],
    )
    executor._create_agent()

    assert captured["system_prompt"].startswith(
        "subagent prompt\n\n<report_contract>"
    )
    assert "[r1 write_file]" in captured["system_prompt"]
    assert captured["system_prompt"].endswith(
        "<current_date>2042-03-04, Tuesday</current_date>"
    )


# ---------------------------------------------------------------------------
# 6. Factory: unsupported thinking_enabled warns + degrades instead of raising
# ---------------------------------------------------------------------------


def test_factory_degrades_unsupported_thinking_instead_of_raising(
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    model_config = ModelConfig(
        name="no-thinking-model",
        display_name=None,
        description=None,
        use="irrelevant.module:Irrelevant",
        model="no-thinking-model-id",
        supports_thinking=False,
        supports_reasoning_effort=False,
    )

    class _FakeAppConfig:
        models: ClassVar[list[ModelConfig]] = [model_config]

        def get_model_config(self, name: str) -> ModelConfig | None:
            return model_config if name == model_config.name else None

    captured_kwargs: dict = {}

    class _FakeChatModel:
        def __init__(self, **kwargs):
            captured_kwargs.update(kwargs)
            self.callbacks = None

    monkeypatch.setattr(factory_module, "get_app_config", lambda: _FakeAppConfig())
    monkeypatch.setattr(factory_module, "resolve_class", lambda path, base=None: _FakeChatModel)

    with caplog.at_level(logging.WARNING):
        model = factory_module.create_chat_model(name="no-thinking-model", thinking_enabled=True)

    assert isinstance(model, _FakeChatModel)
    assert any("does not support" in record.message for record in caplog.records)
    # Degraded to non-thinking: none of the thinking-only settings leak through,
    # and reasoning_effort (unsupported for this model too) is dropped, not raised.
    assert "reasoning_effort" not in captured_kwargs


def test_factory_still_raises_for_unrelated_model_lookup_failure(monkeypatch: pytest.MonkeyPatch) -> None:
    # Sanity check: create_chat_model still raises for genuinely missing models
    # (unrelated to thinking support) -- only the thinking-support guardrail
    # changed from a hard failure to a warn+degrade.
    app_config = AppConfig(sandbox=SandboxConfig(use="test"), models=[])
    monkeypatch.setattr(factory_module, "get_app_config", lambda: app_config)
    with pytest.raises(ValueError, match="Model totally-unknown-model-xyz not found in config"):
        factory_module.create_chat_model(name="totally-unknown-model-xyz")


def test_factory_ignores_codex_max_tokens_kwarg_override_and_warns(
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    """A per-agent max_tokens override arrives via **kwargs, not
    model_settings_from_config. The Codex branch used to only pop max_tokens
    from model_settings_from_config, so kwargs' copy survived the
    `{**model_settings_from_config, **kwargs}` merge (kwargs wins) and would
    still reach the Codex endpoint, which rejects it with a 400. Both places
    must be popped.

    Mocks CodexChatModel itself (not just resolve_class) so the
    `issubclass(model_class, CodexChatModel)` check in factory.py sees a
    genuine subclass relationship without needing real Codex CLI credentials
    (the real class's model_post_init loads/validates those).
    """
    import deerflow.models.openai_codex_provider as codex_provider_module

    class _FakeCodexBase:
        pass

    captured_kwargs: dict = {}

    class _FakeCodexModel(_FakeCodexBase):
        def __init__(self, **kwargs):
            captured_kwargs.update(kwargs)
            self.callbacks = None

    model_config = ModelConfig(
        name="codex-model",
        display_name=None,
        description=None,
        use="irrelevant.module:Irrelevant",
        model="gpt-5.4",
        supports_thinking=True,
        supports_reasoning_effort=True,
    )

    class _FakeAppConfig:
        models: ClassVar[list[ModelConfig]] = [model_config]

        def get_model_config(self, name: str) -> ModelConfig | None:
            return model_config if name == model_config.name else None

    monkeypatch.setattr(factory_module, "get_app_config", lambda: _FakeAppConfig())
    monkeypatch.setattr(factory_module, "resolve_class", lambda path, base=None: _FakeCodexModel)
    monkeypatch.setattr(codex_provider_module, "CodexChatModel", _FakeCodexBase)

    with caplog.at_level(logging.WARNING):
        model = factory_module.create_chat_model(
            name="codex-model",
            thinking_enabled=True,
            max_tokens=4096,  # per-agent override, arrives as a kwarg
        )

    assert isinstance(model, _FakeCodexModel)
    assert "max_tokens" not in captured_kwargs
    assert any("does not support max_tokens" in record.message for record in caplog.records)


# ---------------------------------------------------------------------------
# 7. Import ordering: deerflow.subagents must not require deerflow.agents to be
#    imported first (backlog #15)
# ---------------------------------------------------------------------------


def test_subagents_executor_importable_without_agents_first() -> None:
    """Regression test for a circular-import fragility between deerflow.subagents
    and deerflow.agents.

    deerflow.subagents.__init__ imports .executor, which imports
    deerflow.agents.thread_state, which triggers deerflow.agents.__init__ ->
    prime_enabled_skills_cache() -> lead_agent.__init__ -> lead_agent.agent (the
    package's __init__ imports .agent before .prompt is even reachable) -> a
    chain of module-level imports that used to circle back into
    deerflow.subagents while it was still mid-import, failing with "partially
    initialized module". Two such circle-back points existed:
      - lead_agent.prompt used to import `deerflow.subagents.get_available_subagent_names`
        at module level; fixed by deferring it to inside _build_subagent_section,
        the only place it's used.
      - agents.middlewares.subagent_limit_middleware (imported by lead_agent.agent)
        used to import `deerflow.subagents.executor.MAX_CONCURRENT_SUBAGENTS` at
        module level to use as a default parameter value; fixed by deferring it
        to inside SubagentLimitMiddleware.__init__, resolved only when no explicit
        max_concurrent is passed.

    Runs in a fresh subprocess rather than importing directly in-process: by the
    time this test runs, earlier tests/collection in this session have already
    imported deerflow.agents and/or deerflow.subagents in some order, which masks
    the failure this guards against. Only a clean interpreter reproduces it.
    """
    result = subprocess.run(
        [sys.executable, "-c", "import deerflow.subagents.executor"],
        cwd=str(Path(__file__).resolve().parent.parent),
        capture_output=True,
        text=True,
        timeout=60,
    )
    assert result.returncode == 0, result.stderr
