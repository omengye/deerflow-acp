from __future__ import annotations

from types import SimpleNamespace

import pytest

from deerflow.models.invocation import (
    ainvoke_chat_model,
    invoke_chat_model,
    is_stream_only_chat_error,
)


class ProviderError(Exception):
    def __init__(self, message: str, *, status_code: int = 400) -> None:
        super().__init__(message)
        self.status_code = status_code


class SyncModel:
    def __init__(self, error: Exception) -> None:
        self.error = error
        self.calls: list[tuple[object, object, dict[str, object]]] = []

    def invoke(self, input, *, config=None, **kwargs):
        self.calls.append((input, config, kwargs))
        if len(self.calls) == 1:
            raise self.error
        return "ok"


class AsyncModel:
    def __init__(self, error: Exception) -> None:
        self.error = error
        self.calls: list[tuple[object, object, dict[str, object]]] = []

    async def ainvoke(self, input, *, config=None, **kwargs):
        self.calls.append((input, config, kwargs))
        if len(self.calls) == 1:
            raise self.error
        return "ok"


def _stream_only_error() -> ProviderError:
    return ProviderError(
        "Error code: 400 - {'code': 11101, "
        "'msg': 'Non-stream chat request is currently not supported'}"
    )


def test_stream_only_error_requires_specific_message_and_http_400() -> None:
    assert is_stream_only_chat_error(_stream_only_error()) is True
    assert is_stream_only_chat_error(ProviderError("other bad request")) is False
    assert is_stream_only_chat_error(
        ProviderError("Non-stream chat request is currently not supported", status_code=429)
    ) is False


def test_stream_only_error_accepts_status_from_response() -> None:
    error = RuntimeError("Non-stream chat request is currently not supported")
    error.response = SimpleNamespace(status_code=400)  # type: ignore[attr-defined]

    assert is_stream_only_chat_error(error) is True


def test_sync_invocation_retries_once_with_stream_and_preserves_arguments() -> None:
    model = SyncModel(_stream_only_error())

    result = invoke_chat_model(model, "prompt", config={"run_name": "memory"}, temperature=0)

    assert result == "ok"
    assert model.calls == [
        ("prompt", {"run_name": "memory"}, {"temperature": 0}),
        ("prompt", {"run_name": "memory"}, {"temperature": 0, "stream": True}),
    ]


@pytest.mark.asyncio
async def test_async_invocation_retries_once_with_stream() -> None:
    model = AsyncModel(_stream_only_error())

    result = await ainvoke_chat_model(model, "prompt", config={"run_name": "title"})

    assert result == "ok"
    assert model.calls[-1][2] == {"stream": True}


@pytest.mark.parametrize(
    "error",
    [ProviderError("unrelated 400"), ProviderError("Non-stream chat request is currently not supported", status_code=500)],
)
def test_unrelated_errors_are_not_retried(error: Exception) -> None:
    model = SyncModel(error)

    with pytest.raises(type(error), match=str(error)):
        invoke_chat_model(model, "prompt")

    assert len(model.calls) == 1


def test_explicit_stream_call_is_not_retried() -> None:
    model = SyncModel(_stream_only_error())

    with pytest.raises(ProviderError):
        invoke_chat_model(model, "prompt", stream=True)

    assert len(model.calls) == 1


def test_omitted_config_preserves_minimal_sync_model_signature() -> None:
    class MinimalModel:
        def invoke(self, input):
            return input

    assert invoke_chat_model(MinimalModel(), "prompt") == "prompt"


@pytest.mark.asyncio
async def test_omitted_config_preserves_minimal_async_model_signature() -> None:
    class MinimalModel:
        async def ainvoke(self, input):
            return input

    assert await ainvoke_chat_model(MinimalModel(), "prompt") == "prompt"
