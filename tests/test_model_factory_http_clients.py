"""ACP cold-start HTTP client ownership and caller override tests."""

import gc
import ssl
import weakref
from unittest.mock import patch

import httpx
import langchain_openai.chat_models.base as openai_base
import pytest

from deerflow.config.app_config import AppConfig
from deerflow.config.model_config import ModelConfig
from deerflow.config.sandbox_config import SandboxConfig
from deerflow.models import aclose_chat_model, factory as factory_module


class _SyncClient:
    def __init__(self) -> None:
        self.close_count = 0

    def close(self) -> None:
        self.close_count += 1


class _AsyncClient:
    def __init__(self) -> None:
        self.close_count = 0

    async def aclose(self) -> None:
        self.close_count += 1


class _FakeChatOpenAI:
    def __init__(self, **kwargs) -> None:
        self.init_kwargs = kwargs
        self.callbacks = None
        self.profile = None


def _configured_factory(monkeypatch: pytest.MonkeyPatch) -> None:
    config = AppConfig(
        sandbox=SandboxConfig(use="test"),
        models=[ModelConfig(name="test-model", use="langchain_openai:ChatOpenAI", model="test")],
    )
    monkeypatch.setattr(factory_module, "get_app_config", lambda: config)
    monkeypatch.setattr(factory_module, "resolve_class", lambda *_: _FakeChatOpenAI)
    monkeypatch.setattr(factory_module, "build_tracing_callbacks", lambda: [])
    monkeypatch.setattr(openai_base, "BaseChatOpenAI", _FakeChatOpenAI)


@pytest.mark.asyncio
async def test_deepseek_uses_shared_ssl_context_with_environment_proxy(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """ChatDeepSeek is a BaseChatOpenAI subclass, not ChatOpenAI itself."""
    from langchain_deepseek import ChatDeepSeek

    config = AppConfig(
        sandbox=SandboxConfig(use="test"),
        models=[ModelConfig(name="deepseek-test", use="langchain_deepseek:ChatDeepSeek", model="deepseek-chat", api_key="test-key")],
    )
    monkeypatch.setattr(factory_module, "get_app_config", lambda: config)
    monkeypatch.setattr(factory_module, "resolve_class", lambda *_: ChatDeepSeek)
    monkeypatch.setattr(factory_module, "build_tracing_callbacks", lambda: [])
    monkeypatch.setenv("HTTPS_PROXY", "http://proxy.example:8080")
    monkeypatch.setenv("NO_PROXY", "")
    monkeypatch.delenv("SSL_CERT_FILE", raising=False)
    monkeypatch.delenv("SSL_CERT_DIR", raising=False)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)

    with (
        patch.object(openai_base, "global_ssl_context", context),
        patch.object(httpx, "create_ssl_context") as create_context,
    ):
        model = factory_module.create_chat_model(
            name="deepseek-test",
            disable_keepalive=True,
            share_ssl_context_for_http_clients=True,
        )

    try:
        create_context.assert_not_called()
        assert isinstance(model, ChatDeepSeek)
        assert isinstance(model.http_client, httpx.Client)
        assert isinstance(model.http_async_client, httpx.AsyncClient)
        assert model.http_client._transport._pool._ssl_context is context
        assert model.http_async_client._transport._pool._ssl_context is context
        assert any(
            pattern.pattern == "https://" and transport is not None
            for pattern, transport in model.http_client._mounts.items()
        )
        assert any(
            pattern.pattern == "https://" and transport is not None
            for pattern, transport in model.http_async_client._mounts.items()
        )
    finally:
        await aclose_chat_model(model)

    assert model.http_client.is_closed
    assert model.http_async_client.is_closed


def test_dual_client_builder_honors_explicit_ca_override(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("SSL_CERT_FILE", "custom-ca.pem")
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    with (
        patch.object(httpx, "create_ssl_context", return_value=context) as create_context,
        patch.object(httpx, "Client", return_value=_SyncClient()) as sync_class,
        patch.object(httpx, "AsyncClient", return_value=_AsyncClient()) as async_class,
    ):
        factory_module._build_no_keepalive_http_clients()

    create_context.assert_called_once_with(verify=True, trust_env=True)
    assert sync_class.call_args.kwargs["verify"] is context
    assert async_class.call_args.kwargs["verify"] is context
    assert sync_class.call_args.kwargs["limits"].max_keepalive_connections == 0
    assert async_class.call_args.kwargs["limits"].max_keepalive_connections == 0
    assert sync_class.call_args.kwargs["timeout"].read is None
    assert async_class.call_args.kwargs["timeout"].read is None


def test_acp_opt_in_owns_sync_client_until_model_is_collected(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _configured_factory(monkeypatch)
    sync_client, async_client = _SyncClient(), _AsyncClient()
    monkeypatch.setattr(
        factory_module,
        "_build_no_keepalive_http_clients",
        lambda: (sync_client, async_client),
    )

    model = factory_module.create_chat_model(
        name="test-model",
        disable_keepalive=True,
        share_ssl_context_for_http_clients=True,
    )

    assert model.init_kwargs["http_client"] is sync_client
    assert model.init_kwargs["http_async_client"] is async_client
    assert sync_client.close_count == 0
    model_ref = weakref.ref(model)
    del model
    gc.collect()
    assert model_ref() is None
    assert sync_client.close_count == 1


def test_explicit_sync_client_and_proxy_keep_priority(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _configured_factory(monkeypatch)
    async_client = _AsyncClient()
    monkeypatch.setattr(factory_module, "_build_no_keepalive_async_client", lambda: async_client)
    with patch.object(factory_module, "_build_no_keepalive_http_clients") as build_dual:
        supplied_sync = _SyncClient()
        with_sync = factory_module.create_chat_model(
            name="test-model",
            disable_keepalive=True,
            share_ssl_context_for_http_clients=True,
            http_client=supplied_sync,
        )
        with_proxy = factory_module.create_chat_model(
            name="test-model",
            disable_keepalive=True,
            share_ssl_context_for_http_clients=True,
            openai_proxy="http://proxy.example:8080",
        )
        supplied_async = _AsyncClient()
        with_async = factory_module.create_chat_model(
            name="test-model",
            disable_keepalive=True,
            share_ssl_context_for_http_clients=True,
            http_async_client=supplied_async,
        )

    build_dual.assert_not_called()
    assert with_sync.init_kwargs["http_client"] is supplied_sync
    assert with_sync.init_kwargs["http_async_client"] is async_client
    assert "http_client" not in with_proxy.init_kwargs
    assert "http_async_client" not in with_proxy.init_kwargs
    assert with_async.init_kwargs["http_async_client"] is supplied_async
    assert "http_client" not in with_async.init_kwargs
    assert supplied_sync.close_count == 0


def test_failed_model_construction_closes_both_factory_clients(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _configured_factory(monkeypatch)

    class FailingChatOpenAI(_FakeChatOpenAI):
        def __init__(self, **kwargs) -> None:
            raise ValueError("bad model settings")

    monkeypatch.setattr(factory_module, "resolve_class", lambda *_: FailingChatOpenAI)
    sync_client, async_client = _SyncClient(), _AsyncClient()
    monkeypatch.setattr(
        factory_module,
        "_build_no_keepalive_http_clients",
        lambda: (sync_client, async_client),
    )

    with pytest.raises(ValueError, match="bad model settings"):
        factory_module.create_chat_model(
            name="test-model",
            disable_keepalive=True,
            share_ssl_context_for_http_clients=True,
        )

    assert sync_client.close_count == 1
    assert async_client.close_count == 1
