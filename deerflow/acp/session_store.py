"""Small persistent metadata store for local ACP sessions."""

from __future__ import annotations

import asyncio
import base64
import binascii
import builtins
import json
import sqlite3
import uuid
from dataclasses import asdict, dataclass
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any, Literal, cast


SessionApprovalMode = Literal["ask", "allow_always", "reject_always"]
SESSION_APPROVAL_MODES = frozenset({"ask", "allow_always", "reject_always"})
DEFAULT_SESSION_APPROVAL_MODE: SessionApprovalMode = "ask"


def normalize_session_approval_mode(value: object) -> SessionApprovalMode:
    """Return a safe approval mode for persisted or legacy session data."""
    if isinstance(value, str) and value in SESSION_APPROVAL_MODES:
        return cast(SessionApprovalMode, value)
    return DEFAULT_SESSION_APPROVAL_MODE


def utc_now() -> str:
    return datetime.now(UTC).isoformat().replace("+00:00", "Z")


@dataclass(slots=True)
class LocalACPSession:
    session_id: str
    cwd: str
    title: str | None
    updated_at: str
    model_name: str | None
    thinking_enabled: bool
    subagent_enabled: bool
    plan_mode: bool
    max_concurrent_subagents: int
    recursion_limit: int
    agent_name: str | None = None
    approval_mode: SessionApprovalMode = DEFAULT_SESSION_APPROVAL_MODE
    closed: bool = False

    def runtime_key(self) -> tuple[Any, ...]:
        return (
            self.model_name,
            self.thinking_enabled,
            self.subagent_enabled,
            self.plan_mode,
            self.max_concurrent_subagents,
            self.recursion_limit,
            self.agent_name,
        )

    def config_dict(self) -> dict[str, Any]:
        data = asdict(self)
        for key in ("session_id", "cwd", "title", "updated_at", "closed"):
            data.pop(key, None)
        return data


class LocalACPSessionStore:
    """SQLite-backed session metadata, independent from LangGraph checkpoints."""

    def __init__(self, path: str | Path):
        self.path = Path(path)
        self._connection: sqlite3.Connection | None = None
        self._lock = asyncio.Lock()

    def setup(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        connection = sqlite3.connect(self.path, timeout=30, check_same_thread=False)
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA journal_mode=WAL")
        connection.execute("PRAGMA busy_timeout=30000")
        connection.execute(
            """
            CREATE TABLE IF NOT EXISTS acp_sessions (
                session_id TEXT PRIMARY KEY,
                cwd TEXT NOT NULL,
                title TEXT,
                config_json TEXT NOT NULL,
                updated_at TEXT NOT NULL,
                closed INTEGER NOT NULL DEFAULT 0
            )
            """
        )
        connection.execute(
            "CREATE INDEX IF NOT EXISTS idx_acp_sessions_updated ON acp_sessions(closed, updated_at DESC)"
        )
        connection.commit()
        self._connection = connection

    def close(self) -> None:
        if self._connection is not None:
            self._connection.close()
            self._connection = None

    def _conn(self) -> sqlite3.Connection:
        if self._connection is None:
            raise RuntimeError("ACP session store is not open")
        return self._connection

    @staticmethod
    def _from_row(row: sqlite3.Row) -> LocalACPSession:
        config = json.loads(row["config_json"])
        config["approval_mode"] = normalize_session_approval_mode(
            config.get("approval_mode")
        )
        return LocalACPSession(
            session_id=row["session_id"],
            cwd=row["cwd"],
            title=row["title"],
            updated_at=row["updated_at"],
            closed=bool(row["closed"]),
            **config,
        )

    async def create(self, *, cwd: str, defaults: dict[str, Any]) -> LocalACPSession:
        session = LocalACPSession(
            session_id=str(uuid.uuid4()),
            cwd=cwd,
            title=None,
            updated_at=utc_now(),
            closed=False,
            **defaults,
        )
        await self.save(session)
        return session

    async def save(self, session: LocalACPSession) -> None:
        session.updated_at = utc_now()
        async with self._lock:
            connection = self._conn()
            connection.execute(
                """
                INSERT INTO acp_sessions(session_id, cwd, title, config_json, updated_at, closed)
                VALUES (?, ?, ?, ?, ?, ?)
                ON CONFLICT(session_id) DO UPDATE SET
                    cwd=excluded.cwd,
                    title=excluded.title,
                    config_json=excluded.config_json,
                    updated_at=excluded.updated_at,
                    closed=excluded.closed
                """,
                (
                    session.session_id,
                    session.cwd,
                    session.title,
                    json.dumps(session.config_dict(), ensure_ascii=False, sort_keys=True),
                    session.updated_at,
                    int(session.closed),
                ),
            )
            connection.commit()

    async def get(self, session_id: str, *, include_closed: bool = False) -> LocalACPSession | None:
        async with self._lock:
            row = self._conn().execute(
                "SELECT * FROM acp_sessions WHERE session_id = ?",
                (session_id,),
            ).fetchone()
        if row is None:
            return None
        session = self._from_row(row)
        if session.closed and not include_closed:
            return None
        return session

    async def list(
        self,
        *,
        cwd: str | None,
        cursor: str | None,
        limit: int,
    ) -> tuple[list[LocalACPSession], str | None]:
        offset = self.decode_cursor(cursor)
        where = "WHERE closed = 0"
        params: list[Any] = []
        if cwd is not None:
            where += " AND cwd = ?"
            params.append(cwd)
        params.extend((limit + 1, offset))
        async with self._lock:
            rows = self._conn().execute(
                f"SELECT * FROM acp_sessions {where} ORDER BY updated_at DESC LIMIT ? OFFSET ?",  # noqa: S608
                params,
            ).fetchall()
        has_more = len(rows) > limit
        sessions = [self._from_row(row) for row in rows[:limit]]
        next_cursor = self.encode_cursor(offset + limit) if has_more else None
        return sessions, next_cursor

    async def mark_closed(self, session_id: str) -> bool:
        async with self._lock:
            connection = self._conn()
            cursor = connection.execute(
                "UPDATE acp_sessions SET closed = 1, updated_at = ? WHERE session_id = ?",
                (utc_now(), session_id),
            )
            connection.commit()
            return cursor.rowcount > 0

    async def list_for_management(self) -> builtins.list[LocalACPSession]:
        async with self._lock:
            rows = self._conn().execute(
                "SELECT * FROM acp_sessions ORDER BY updated_at DESC LIMIT 1000"
            ).fetchall()
        return [self._from_row(row) for row in rows]

    async def purge_closed(self, *, retention_days: int) -> builtins.list[str]:
        """Delete closed session metadata older than the configured retention."""

        session_ids = await self.expired_session_ids(
            closed_retention_days=retention_days,
            inactive_retention_days=None,
        )
        await self.delete_sessions(session_ids)
        return session_ids

    async def expired_session_ids(
        self,
        *,
        closed_retention_days: int,
        inactive_retention_days: int | None,
        limit: int = 256,
    ) -> builtins.list[str]:
        """Return oldest sessions eligible for checkpoint cleanup."""

        now = datetime.now(UTC)
        closed_cutoff = (
            (now - timedelta(days=closed_retention_days))
            .isoformat()
            .replace("+00:00", "Z")
        )
        conditions = ["(closed = 1 AND updated_at <= ?)"]
        params: list[Any] = [closed_cutoff]
        if inactive_retention_days is not None:
            inactive_cutoff = (
                now - timedelta(days=inactive_retention_days)
            ).isoformat().replace("+00:00", "Z")
            conditions.append("(closed = 0 AND updated_at <= ?)")
            params.append(inactive_cutoff)
        params.append(limit)
        async with self._lock:
            rows = self._conn().execute(
                "SELECT session_id FROM acp_sessions WHERE "
                + " OR ".join(conditions)
                + " ORDER BY updated_at ASC LIMIT ?",
                params,
            ).fetchall()
        return [str(row["session_id"]) for row in rows]

    async def delete_sessions(self, session_ids: builtins.list[str]) -> None:
        """Delete session metadata after its checkpoint state was removed."""

        if not session_ids:
            return
        async with self._lock:
            connection = self._conn()
            connection.executemany(
                "DELETE FROM acp_sessions WHERE session_id = ?",
                ((session_id,) for session_id in session_ids),
            )
            connection.commit()

    @staticmethod
    def encode_cursor(offset: int) -> str:
        return base64.urlsafe_b64encode(str(offset).encode()).decode().rstrip("=")

    @staticmethod
    def decode_cursor(cursor: str | None) -> int:
        if not cursor:
            return 0
        try:
            padded = cursor + "=" * (-len(cursor) % 4)
            offset = int(base64.urlsafe_b64decode(padded).decode())
        except (binascii.Error, ValueError, UnicodeDecodeError) as exc:
            raise ValueError("Invalid session cursor") from exc
        if offset < 0:
            raise ValueError("Invalid session cursor")
        return offset
