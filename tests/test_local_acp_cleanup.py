from __future__ import annotations

import sqlite3
from pathlib import Path

import pytest

from deerflow.acp.config import LocalACPConfig
from deerflow.acp.session_cleanup import cleanup_expired_sessions
from deerflow.acp.session_coordinator import (
    ACPSessionCoordinator,
    SessionBusyError,
)
from deerflow.acp.session_store import LocalACPSessionStore


def _defaults() -> dict[str, object]:
    return {
        "model_name": None,
        "thinking_enabled": True,
        "subagent_enabled": False,
        "plan_mode": False,
        "max_concurrent_subagents": 2,
        "recursion_limit": 100,
        "agent_name": None,
    }


class _Runtime:
    def __init__(self, *, fail_checkpoint_purge: bool = False) -> None:
        self.session_coordinator = ACPSessionCoordinator()
        self.released: list[str] = []
        self.purge_attempts: list[str] = []
        self.compactions = 0
        self.fail_checkpoint_purge = fail_checkpoint_purge

    async def release_session(self, session_id: str) -> None:
        self.released.append(session_id)

    async def purge_checkpoints(self, session_ids: list[str]) -> list[str]:
        self.purge_attempts.extend(session_ids)
        return [] if self.fail_checkpoint_purge else list(session_ids)

    async def compact_checkpoints(self) -> bool:
        self.compactions += 1
        return True


def _age_session(path: Path, session_id: str) -> None:
    with sqlite3.connect(path) as connection:
        connection.execute(
            "UPDATE acp_sessions SET updated_at = ? WHERE session_id = ?",
            ("2000-01-01T00:00:00Z", session_id),
        )


@pytest.mark.asyncio
async def test_cleanup_removes_expired_unattached_sessions(tmp_path: Path) -> None:
    store = LocalACPSessionStore(tmp_path / "sessions.db")
    store.setup()
    stale = await store.create(cwd=str(tmp_path), defaults=_defaults())
    active = await store.create(cwd=str(tmp_path), defaults=_defaults())
    recent = await store.create(cwd=str(tmp_path), defaults=_defaults())
    closed = await store.create(cwd=str(tmp_path), defaults=_defaults())
    await store.mark_closed(closed.session_id)
    for session in (stale, active, closed):
        _age_session(store.path, session.session_id)

    runtime = _Runtime()
    runtime.session_coordinator.attach(active.session_id, "connection-1")
    config = LocalACPConfig(
        config_path=tmp_path / "config.yaml",
        checkpointer_path=tmp_path / "checkpoints.db",
        session_store_path=store.path,
        inactive_session_retention_days=30,
        closed_session_retention_days=0,
    )

    purged = await cleanup_expired_sessions(config, store, runtime, compact=True)

    assert set(purged) == {stale.session_id, closed.session_id}
    assert await store.get(stale.session_id, include_closed=True) is None
    assert await store.get(closed.session_id, include_closed=True) is None
    assert await store.get(active.session_id) is not None
    assert await store.get(recent.session_id) is not None
    assert set(runtime.released) == set(purged)
    assert runtime.compactions == 1
    store.close()


@pytest.mark.asyncio
async def test_cleanup_keeps_metadata_when_checkpoint_purge_fails(
    tmp_path: Path,
) -> None:
    store = LocalACPSessionStore(tmp_path / "sessions.db")
    store.setup()
    stale = await store.create(cwd=str(tmp_path), defaults=_defaults())
    _age_session(store.path, stale.session_id)
    runtime = _Runtime(fail_checkpoint_purge=True)
    config = LocalACPConfig(
        config_path=tmp_path / "config.yaml",
        checkpointer_path=tmp_path / "checkpoints.db",
        session_store_path=store.path,
        inactive_session_retention_days=30,
    )

    assert await cleanup_expired_sessions(config, store, runtime) == []
    assert await store.get(stale.session_id) is not None
    store.close()


def test_cleanup_reservation_blocks_session_attach() -> None:
    coordinator = ACPSessionCoordinator()
    assert coordinator.reserve_cleanup("session-1") is True
    assert coordinator.reserve_cleanup("session-1") is False
    with pytest.raises(SessionBusyError, match="being cleaned up"):
        coordinator.attach("session-1", "connection-1")
    coordinator.release_cleanup("session-1")
    assert coordinator.attach("session-1", "connection-1") is True
    assert coordinator.reserve_cleanup("session-1") is False
