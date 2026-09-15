"""Tests for ``ExtensionsConfig.middlewares`` and its dynamic loader.

Covers the declarative-middleware config option: a list of
"module.path:ClassName" strings on ``ExtensionsConfig.middlewares`` that
``load_configured_middlewares()`` resolves and instantiates for
``_build_middlewares()`` to append to the lead agent's middleware chain
(see ``deerflow/agents/middlewares/configured_extensions.py`` and
``config.example.yaml``).
"""

from __future__ import annotations

import logging
from unittest.mock import patch

import pytest
from langchain.agents.middleware import AgentMiddleware

from deerflow.agents.middlewares.clarification_middleware import ClarificationMiddleware
from deerflow.agents.middlewares.configured_extensions import (
    load_configured_middlewares,
)
from deerflow.agents.middlewares.loop_detection_middleware import (
    LoopDetectionMiddleware,
)
from deerflow.config.app_config import AppConfig, reset_app_config, set_app_config
from deerflow.config.extensions_config import ExtensionsConfig
from deerflow.config.sandbox_config import SandboxConfig

# pytest imports this module by its bare basename (no `tests.` package
# prefix, since tests/ has no __init__.py and is inserted directly onto
# sys.path under the default "prepend" import mode) — the dotted path must
# match that, or resolve_class() re-imports a second, distinct copy of this
# module and isinstance() checks against it fail.
_DUMMY_PATH = "test_configured_extensions_middleware:_DummyMiddleware"
_NOT_A_MIDDLEWARE_PATH = "test_configured_extensions_middleware:_NotAMiddleware"
_MISSING_ATTR_PATH = "test_configured_extensions_middleware:DoesNotExist"


class _DummyMiddleware(AgentMiddleware):
    """Middleware used to exercise both declaration forms."""

    def __init__(self, label: str | None = None, enabled: bool = True):
        self.label = label
        self.enabled = enabled


class _NotAMiddleware:
    """Not an ``AgentMiddleware`` subclass; used to exercise the type check."""


def test_extensions_config_middlewares_defaults_to_empty_list():
    config = ExtensionsConfig()
    assert config.middlewares == []


def test_extensions_config_middlewares_parses_from_dict():
    config = ExtensionsConfig.model_validate({"middlewares": [_DUMMY_PATH]})
    assert config.middlewares == [_DUMMY_PATH]


def test_configured_middleware_accepts_json_safe_constructor_kwargs():
    config = ExtensionsConfig.model_validate(
        {
            "middlewares": [
                {
                    "class": _DUMMY_PATH,
                    "kwargs": {"label": "configured", "enabled": False},
                }
            ]
        }
    )

    middlewares = load_configured_middlewares(config)

    assert len(middlewares) == 1
    assert middlewares[0].label == "configured"
    assert middlewares[0].enabled is False


def test_configured_middleware_rejects_non_finite_kwargs():
    from pydantic import ValidationError

    with pytest.raises(ValidationError):
        ExtensionsConfig.model_validate(
            {
                "middlewares": [
                    {"class": _DUMMY_PATH, "kwargs": {"threshold": float("nan")}}
                ]
            }
        )


def test_load_configured_middlewares_returns_empty_list_by_default():
    assert load_configured_middlewares(ExtensionsConfig()) == []


def test_load_configured_middlewares_instantiates_valid_entries_in_order():
    config = ExtensionsConfig(middlewares=[_DUMMY_PATH, _DUMMY_PATH])

    middlewares = load_configured_middlewares(config)

    assert len(middlewares) == 2
    assert all(isinstance(middleware, _DummyMiddleware) for middleware in middlewares)


def test_load_configured_middlewares_skips_missing_attribute_and_keeps_valid_entries(caplog):
    config = ExtensionsConfig(middlewares=[_MISSING_ATTR_PATH, _DUMMY_PATH])

    with caplog.at_level(logging.WARNING):
        middlewares = load_configured_middlewares(config)

    assert len(middlewares) == 1
    assert isinstance(middlewares[0], _DummyMiddleware)
    assert any("DoesNotExist" in record.message for record in caplog.records)


def test_load_configured_middlewares_skips_wrong_type_entry(caplog):
    config = ExtensionsConfig(middlewares=[_NOT_A_MIDDLEWARE_PATH])

    with caplog.at_level(logging.WARNING):
        middlewares = load_configured_middlewares(config)

    assert middlewares == []
    assert any("_NotAMiddleware" in record.message for record in caplog.records)


def test_load_configured_middlewares_uses_process_wide_config_by_default(monkeypatch):
    config = ExtensionsConfig(middlewares=[_DUMMY_PATH])
    monkeypatch.setattr(
        "deerflow.agents.middlewares.configured_extensions.get_extensions_config",
        lambda: config,
    )

    middlewares = load_configured_middlewares()

    assert len(middlewares) == 1
    assert isinstance(middlewares[0], _DummyMiddleware)


def test_build_middlewares_appends_configured_middlewares_before_clarification():
    # Full integration through `_build_middlewares()`: mock the loader (rather
    # than routing an actual ExtensionsConfig through disk I/O) to prove the
    # declared middlewares really land in the chain that the lead agent runs,
    # in the documented slot — after the built-ins/custom middlewares and
    # LoopDetectionMiddleware, but still before the mandatory-last
    # ClarificationMiddleware.
    from deerflow.agents.lead_agent.agent import _build_middlewares

    dummy = _DummyMiddleware()
    set_app_config(AppConfig(sandbox=SandboxConfig(use="test")))
    try:
        with (
            patch("deerflow.agents.lead_agent.agent._create_summarization_middleware", return_value=None),
            patch("deerflow.agents.lead_agent.agent.load_configured_middlewares", return_value=[dummy]),
        ):
            middlewares = _build_middlewares({}, model_name=None)
    finally:
        reset_app_config()

    assert dummy in middlewares
    assert isinstance(middlewares[-1], ClarificationMiddleware)
    dummy_index = middlewares.index(dummy)
    loop_detection_index = next(i for i, m in enumerate(middlewares) if isinstance(m, LoopDetectionMiddleware))
    assert loop_detection_index < dummy_index < len(middlewares) - 1
