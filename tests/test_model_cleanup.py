import weakref
from typing import Any

import pytest

from deerflow.models import aclose_chat_model


class _FakeClient:
    def __init__(self) -> None:
        self.closed = False
        self.close_count = 0

    async def aclose(self) -> None:
        self.closed = True

    def close(self) -> None:
        self.closed = True
        self.close_count += 1


class _FakeModel:
    def __init__(self, **attrs: Any) -> None:
        for name, value in attrs.items():
            setattr(self, name, value)


@pytest.mark.asyncio
async def test_aclose_chat_model_only_closes_injected_http_client() -> None:
    root_client = _FakeClient()
    http_client = _FakeClient()
    model = _FakeModel(root_async_client=root_client, http_async_client=http_client)

    await aclose_chat_model(model)

    assert http_client.closed is True
    assert root_client.closed is False


@pytest.mark.asyncio
async def test_aclose_chat_model_closes_root_client_without_injected_http_client() -> None:
    root_client = _FakeClient()
    model = _FakeModel(root_async_client=root_client)

    await aclose_chat_model(model)

    assert root_client.closed is True


@pytest.mark.asyncio
async def test_aclose_chat_model_closes_only_factory_owned_sync_client() -> None:
    owned_sync = _FakeClient()
    caller_sync = _FakeClient()
    model = _FakeModel(
        http_client=caller_sync,
        http_async_client=_FakeClient(),
    )
    model._deerflow_owned_http_client_finalizer = weakref.finalize(model, owned_sync.close)

    await aclose_chat_model(model)
    await aclose_chat_model(model)

    assert owned_sync.close_count == 1
    assert caller_sync.close_count == 0
