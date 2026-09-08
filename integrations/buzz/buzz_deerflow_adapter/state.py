"""SQLite-backed inbox, outbox, cursor, and ACP session state."""

from __future__ import annotations

import sqlite3
from pathlib import Path

from .models import BuzzMessage, PendingMessage


class AdapterState:
    def __init__(self, path: Path) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        self._connection: sqlite3.Connection | None = sqlite3.connect(path)
        self._connection.row_factory = sqlite3.Row
        self._connection.execute("PRAGMA journal_mode=WAL")
        self._connection.executescript(
            """
            CREATE TABLE IF NOT EXISTS sessions (
                conversation_key TEXT PRIMARY KEY,
                session_id TEXT NOT NULL,
                workspace TEXT NOT NULL,
                updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
            );
            CREATE TABLE IF NOT EXISTS channel_cursors (
                channel_id TEXT PRIMARY KEY,
                created_at INTEGER NOT NULL,
                updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
            );
            CREATE TABLE IF NOT EXISTS inbox_messages (
                message_key TEXT PRIMARY KEY,
                conversation_key TEXT NOT NULL,
                channel_id TEXT NOT NULL,
                event_id TEXT NOT NULL,
                created_at INTEGER NOT NULL,
                author_pubkey TEXT NOT NULL,
                kind INTEGER NOT NULL,
                content TEXT NOT NULL,
                is_dm INTEGER NOT NULL DEFAULT 0,
                status TEXT NOT NULL DEFAULT 'pending',
                attempts INTEGER NOT NULL DEFAULT 0,
                last_error TEXT,
                response_content TEXT,
                received_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
                completed_at TEXT
            );
            CREATE UNIQUE INDEX IF NOT EXISTS inbox_event_id
                ON inbox_messages(event_id);
            """
        )
        self._connection.commit()

    def close(self) -> None:
        connection, self._connection = self._connection, None
        if connection is not None:
            connection.close()

    def _conn(self) -> sqlite3.Connection:
        if self._connection is None:
            raise RuntimeError("adapter state is closed")
        return self._connection

    def get_cursor(self, channel_id: str) -> int | None:
        row = (
            self._conn()
            .execute(
                "SELECT created_at FROM channel_cursors WHERE channel_id = ?",
                (channel_id,),
            )
            .fetchone()
        )
        return None if row is None else int(row["created_at"])

    def put_cursor(self, channel_id: str, created_at: int) -> None:
        self._conn().execute(
            """
            INSERT INTO channel_cursors (channel_id, created_at)
            VALUES (?, ?)
            ON CONFLICT(channel_id) DO UPDATE SET
                created_at = MAX(channel_cursors.created_at, excluded.created_at),
                updated_at = CURRENT_TIMESTAMP
            """,
            (channel_id, created_at),
        )
        self._conn().commit()

    def enqueue(self, messages: list[BuzzMessage], *, session_scope: str) -> int:
        inserted = 0
        for message in messages:
            cursor = self._conn().execute(
                """
                INSERT OR IGNORE INTO inbox_messages (
                    message_key, conversation_key, channel_id, event_id,
                    created_at, author_pubkey, kind, content, is_dm
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    message.key,
                    message.conversation_key(session_scope),
                    message.channel_id,
                    message.event_id,
                    message.created_at,
                    message.author_pubkey,
                    message.kind,
                    message.content,
                    int(message.is_dm),
                ),
            )
            inserted += max(cursor.rowcount, 0)
        self._conn().commit()
        return inserted

    def pending(self, limit: int = 100) -> list[PendingMessage]:
        rows = (
            self._conn()
            .execute(
                """
            SELECT message_key, conversation_key, channel_id, event_id,
                   created_at, author_pubkey, kind, content, is_dm, attempts,
                   response_content
            FROM inbox_messages
            WHERE status = 'pending'
            ORDER BY created_at, message_key
            LIMIT ?
            """,
                (limit,),
            )
            .fetchall()
        )
        return [
            PendingMessage(
                key=row["message_key"],
                conversation_key=row["conversation_key"],
                channel_id=row["channel_id"],
                event_id=row["event_id"],
                created_at=row["created_at"],
                author_pubkey=row["author_pubkey"],
                kind=row["kind"],
                content=row["content"],
                is_dm=bool(row["is_dm"]),
                attempts=row["attempts"],
                response_content=row["response_content"],
            )
            for row in rows
        ]

    def save_response(self, key: str, response_content: str) -> None:
        self._conn().execute(
            """
            UPDATE inbox_messages
            SET response_content = ?
            WHERE message_key = ? AND status = 'pending'
            """,
            (response_content, key),
        )
        self._conn().commit()

    def mark_done(self, key: str) -> None:
        self._conn().execute(
            """
            UPDATE inbox_messages
            SET status = 'done', completed_at = CURRENT_TIMESTAMP,
                last_error = NULL, response_content = NULL
            WHERE message_key = ?
            """,
            (key,),
        )
        self._conn().commit()

    def mark_failed(self, key: str, error: str, *, max_attempts: int) -> bool:
        self._conn().execute(
            """
            UPDATE inbox_messages
            SET attempts = attempts + 1,
                last_error = ?,
                status = CASE
                    WHEN attempts + 1 >= ? THEN 'failed'
                    ELSE status
                END
            WHERE message_key = ? AND status = 'pending'
            """,
            (error[:2000], max_attempts, key),
        )
        self._conn().commit()
        row = (
            self._conn()
            .execute("SELECT status FROM inbox_messages WHERE message_key = ?", (key,))
            .fetchone()
        )
        return row is not None and row["status"] == "failed"

    def mark_delivery_unknown(self, key: str, error: str) -> None:
        self._conn().execute(
            """
            UPDATE inbox_messages
            SET status = 'delivery_unknown', attempts = attempts + 1,
                last_error = ?
            WHERE message_key = ? AND status = 'pending'
            """,
            (error[:8000], key),
        )
        self._conn().commit()

    def get_session(self, conversation_key: str, workspace: Path) -> str | None:
        row = (
            self._conn()
            .execute(
                "SELECT session_id, workspace FROM sessions WHERE conversation_key = ?",
                (conversation_key,),
            )
            .fetchone()
        )
        if row is None or Path(row["workspace"]) != workspace:
            return None
        return str(row["session_id"])

    def put_session(
        self, conversation_key: str, session_id: str, workspace: Path
    ) -> None:
        self._conn().execute(
            """
            INSERT INTO sessions (conversation_key, session_id, workspace)
            VALUES (?, ?, ?)
            ON CONFLICT(conversation_key) DO UPDATE SET
                session_id = excluded.session_id,
                workspace = excluded.workspace,
                updated_at = CURRENT_TIMESTAMP
            """,
            (conversation_key, session_id, str(workspace)),
        )
        self._conn().commit()
