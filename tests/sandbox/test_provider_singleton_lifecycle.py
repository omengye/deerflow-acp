from __future__ import annotations

import threading
from concurrent.futures import ThreadPoolExecutor
from types import SimpleNamespace

import pytest

import deerflow.sandbox.sandbox_provider as providers


def test_cold_concurrent_callers_share_one_provider(monkeypatch):
    entered, finish = threading.Event(), threading.Event()
    constructed = []

    class FakeProvider:
        def __init__(self):
            constructed.append(self)
            entered.set()
            assert finish.wait(3)

    monkeypatch.setattr(providers, "_default_sandbox_provider", None)
    monkeypatch.setattr(providers, "get_app_config", lambda: SimpleNamespace(sandbox=SimpleNamespace(use="fake")))
    monkeypatch.setattr(providers, "normalize_sandbox_provider_path", lambda value: value)
    monkeypatch.setattr(providers, "resolve_class", lambda *args: FakeProvider)
    with ThreadPoolExecutor(max_workers=2) as workers:
        first = workers.submit(providers.get_sandbox_provider)
        assert entered.wait(2)
        second = workers.submit(providers.get_sandbox_provider)
        finish.set()
        assert first.result(timeout=2) is second.result(timeout=2)
    assert len(constructed) == 1


def test_failed_shutdown_keeps_singleton_for_cleanup_retry(monkeypatch):
    class FakeProvider:
        fail = True

        def shutdown(self):
            if self.fail:
                raise RuntimeError("busy")

    provider = FakeProvider()
    monkeypatch.setattr(providers, "_default_sandbox_provider", provider)
    with pytest.raises(RuntimeError, match="busy"):
        providers.shutdown_sandbox_provider()
    assert providers.get_existing_sandbox_provider() is provider
    provider.fail = False
    providers.shutdown_sandbox_provider()
    assert providers.get_existing_sandbox_provider() is None
