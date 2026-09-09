"""Tests for preserving existing model callbacks when tracing is enabled."""

from unittest.mock import patch

from langchain_core.callbacks import BaseCallbackHandler, CallbackManager

from deerflow.config.app_config import AppConfig
from deerflow.config.model_config import ModelConfig
from deerflow.config.sandbox_config import SandboxConfig
from deerflow.models import factory as factory_module


class _FakeChatModel:
    def __init__(self, **kwargs) -> None:
        self.callbacks = kwargs.get("callbacks")
        self.profile = kwargs.get("profile")


def _create_model(existing_callbacks, tracing_callback: BaseCallbackHandler):  # noqa: ANN001
    config = AppConfig(
        sandbox=SandboxConfig(use="test"),
        models=[
            ModelConfig(
                name="test-model",
                use="tests.fake:FakeChatModel",
                model="provider-model",
            )
        ],
    )
    with (
        patch.object(factory_module, "get_app_config", return_value=config),
        patch.object(factory_module, "resolve_class", return_value=_FakeChatModel),
        patch.object(factory_module, "build_tracing_callbacks", return_value=[tracing_callback]),
    ):
        return factory_module.create_chat_model(name="test-model", callbacks=existing_callbacks)


def test_factory_appends_tracing_callback_to_existing_list() -> None:
    existing = BaseCallbackHandler()
    tracing = BaseCallbackHandler()

    model = _create_model([existing], tracing)

    assert model.callbacks == [existing, tracing]


def test_factory_adds_tracing_callback_to_existing_manager() -> None:
    existing = BaseCallbackHandler()
    tracing = BaseCallbackHandler()
    manager = CallbackManager([existing])

    model = _create_model(manager, tracing)

    assert model.callbacks is manager
    assert manager.handlers == [existing, tracing]


def test_factory_excludes_context_window_from_model_kwargs() -> None:
    class _CapturingChatModel:
        def __init__(self, **kwargs) -> None:
            self.init_kwargs = kwargs
            self.callbacks = None
            self.profile = {"tool_calling": True}

    config = AppConfig(
        sandbox=SandboxConfig(use="test"),
        models=[
            ModelConfig(
                name="test-model-with-cw",
                use="tests.fake:CapturingChatModel",
                model="provider-model",
                context_window=131072,
            )
        ],
    )
    with (
        patch.object(factory_module, "get_app_config", return_value=config),
        patch.object(factory_module, "resolve_class", return_value=_CapturingChatModel),
        patch.object(factory_module, "build_tracing_callbacks", return_value=[]),
    ):
        model = factory_module.create_chat_model(name="test-model-with-cw")
        assert "context_window" not in model.init_kwargs
        assert model.init_kwargs.get("model") == "provider-model"
        assert model.profile == {
            "tool_calling": True,
            "max_input_tokens": 131072,
        }


def test_explicit_model_profile_wins_over_context_window_translation() -> None:
    explicit_profile = {
        "max_input_tokens": 4096,
        "structured_output": True,
    }
    config = AppConfig(
        sandbox=SandboxConfig(use="test"),
        models=[
            ModelConfig(
                name="test-model-with-profile",
                use="tests.fake:FakeChatModel",
                model="provider-model",
                context_window=131072,
            )
        ],
    )
    with (
        patch.object(factory_module, "get_app_config", return_value=config),
        patch.object(factory_module, "resolve_class", return_value=_FakeChatModel),
        patch.object(factory_module, "build_tracing_callbacks", return_value=[]),
    ):
        model = factory_module.create_chat_model(
            name="test-model-with-profile",
            profile=explicit_profile,
        )

    assert model.profile == explicit_profile


def test_runtime_headers_stay_out_of_constructor_and_static_headers_remain() -> None:
    class _CapturingChatModel:
        def __init__(self, **kwargs) -> None:
            self.init_kwargs = kwargs
            self.callbacks = None
            self.profile = None

    config = AppConfig(
        sandbox=SandboxConfig(use="test"),
        models=[
            ModelConfig(
                name="opencode-go",
                use="tests.fake:CapturingChatModel",
                model="kimi-k3",
                default_headers={"User-Agent": "deerflow-api/1.0"},
                runtime_headers={"x-opencode-session": "thread_id"},
            )
        ],
    )
    with (
        patch.object(factory_module, "get_app_config", return_value=config),
        patch.object(factory_module, "resolve_class", return_value=_CapturingChatModel),
        patch.object(factory_module, "build_tracing_callbacks", return_value=[]),
    ):
        model = factory_module.create_chat_model(name="opencode-go")

    assert "runtime_headers" not in model.init_kwargs
    assert model.init_kwargs["default_headers"] == {"User-Agent": "deerflow-api/1.0"}
    assert model._deerflow_runtime_headers == {"x-opencode-session": "thread_id"}
