import asyncio
import threading

import pytest

from deerflow.mcp import cache


@pytest.fixture(autouse=True)
def reset_cache_state():
    with cache._init_condition:
        cache._mcp_tools_cache = None
        cache._cache_initialized = False
        cache._initializing_generation = None
        cache._cache_generation = 0
        cache._config_signature = None
        cache._init_condition.notify_all()
    yield
    with cache._init_condition:
        cache._mcp_tools_cache = None
        cache._cache_initialized = False
        cache._initializing_generation = None
        cache._cache_generation = 0
        cache._config_signature = None
        cache._init_condition.notify_all()


def test_contended_initialization_is_cross_loop_safe(monkeypatch):
    calls = 0
    barrier = threading.Barrier(2)

    async def fake_tools():
        nonlocal calls
        calls += 1
        await asyncio.sleep(0.03)
        return ["tool"]

    monkeypatch.setattr("deerflow.mcp.tools.get_mcp_tools", fake_tools)
    monkeypatch.setattr(cache, "_get_config_signature", lambda: ("cfg", 1, 1, "hash"))
    results = []

    def initialize():
        barrier.wait()
        results.append(asyncio.run(cache.initialize_mcp_tools()))

    threads = [threading.Thread(target=initialize) for _ in range(2)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join(timeout=2)

    assert all(not thread.is_alive() for thread in threads)
    assert results == [["tool"], ["tool"]]
    assert calls == 1


@pytest.mark.asyncio
async def test_cancelled_initializer_releases_generation_claim(monkeypatch):
    started = asyncio.Event()
    release = asyncio.Event()
    calls = 0

    async def fake_tools():
        nonlocal calls
        calls += 1
        started.set()
        await release.wait()
        return []

    monkeypatch.setattr("deerflow.mcp.tools.get_mcp_tools", fake_tools)
    monkeypatch.setattr(cache, "_get_config_signature", lambda: None)

    owner = asyncio.create_task(cache.initialize_mcp_tools())
    await asyncio.wait_for(started.wait(), timeout=1)
    owner.cancel()
    with pytest.raises(asyncio.CancelledError):
        await owner
    assert cache._initializing_generation is None

    release.set()
    assert await asyncio.wait_for(cache.initialize_mcp_tools(), timeout=1) == []
    assert cache._cache_initialized is True
    assert calls == 2


def test_config_change_during_load_discards_stale_result(monkeypatch):
    signatures = iter([
        ("cfg", 1, 1, "old"),
        ("cfg", 2, 1, "new"),
        ("cfg", 2, 1, "new"),
        ("cfg", 2, 1, "new"),
    ])
    calls = 0

    async def fake_tools():
        nonlocal calls
        calls += 1
        return ["old"] if calls == 1 else ["new"]

    monkeypatch.setattr("deerflow.mcp.tools.get_mcp_tools", fake_tools)
    monkeypatch.setattr(cache, "_get_config_signature", lambda: next(signatures))

    assert asyncio.run(cache.initialize_mcp_tools()) == []
    assert cache._cache_initialized is False
    assert asyncio.run(cache.initialize_mcp_tools()) == ["new"]
    assert cache._mcp_tools_cache == ["new"]
