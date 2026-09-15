from __future__ import annotations

import asyncio
import threading

import deerflow.mcp.session_pool as session_pool_module
from deerflow.mcp.session_pool import MCPSessionPool


async def test_capacity_is_rechecked_when_concurrent_sessions_are_promoted(monkeypatch) -> None:
    both_started = threading.Event()
    release_start = threading.Event()
    actors: list[object] = []
    actors_lock = threading.Lock()

    class FakeActor:
        def __init__(self, connection) -> None:
            self.connection = connection
            self.closed = threading.Event()
            actors.append(self)

        async def start(self) -> None:
            with actors_lock:
                if len(actors) == 2:
                    both_started.set()
            await asyncio.to_thread(release_start.wait, 5)

        async def close(self) -> None:
            self.closed.set()

    monkeypatch.setattr(session_pool_module, "_SessionActor", FakeActor)
    pool = MCPSessionPool()
    pool.MAX_SESSIONS = 1

    first = asyncio.create_task(pool.get_session("one", "scope", {}))
    second = asyncio.create_task(pool.get_session("two", "scope", {}))
    assert await asyncio.to_thread(both_started.wait, 2)
    release_start.set()
    await asyncio.gather(first, second)

    assert len(pool._entries) == 1
    for _ in range(200):
        if sum(actor.closed.is_set() for actor in actors) == 1:
            break
        await asyncio.sleep(0.01)
    assert sum(actor.closed.is_set() for actor in actors) == 1

    await pool.close_all()
    assert all(actor.closed.is_set() for actor in actors)
