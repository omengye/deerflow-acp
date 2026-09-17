"""Persistent scheduled task support for DeerFlow API."""

from __future__ import annotations

import asyncio
import json
import logging
import sqlite3
import uuid
from contextlib import contextmanager
from dataclasses import dataclass
from datetime import UTC, datetime, time, timedelta, timezone as fixed_timezone
from pathlib import Path
from typing import Any, Callable, Iterator, Literal
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

logger = logging.getLogger(__name__)

ScheduleType = Literal["once", "interval", "daily"]

_scheduler_service: "SchedulerService | None" = None


def _utc_now() -> datetime:
    return datetime.now(UTC)


def _dt_to_iso(dt: datetime) -> str:
    return dt.astimezone(UTC).isoformat()


def _parse_dt(value: str, timezone: str) -> datetime:
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    dt = datetime.fromisoformat(text)
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=_get_tzinfo(timezone))
    return dt.astimezone(UTC)


def _get_tzinfo(timezone: str):
    try:
        return ZoneInfo(timezone)
    except ZoneInfoNotFoundError:
        normalized = timezone.strip().upper()
        if normalized in {"UTC", "ETC/UTC", "Z"}:
            return UTC
        if timezone in {"Asia/Shanghai", "Asia/Chongqing", "Asia/Harbin"}:
            return fixed_timezone(timedelta(hours=8), "Asia/Shanghai")
        if timezone == "Asia/Urumqi":
            return fixed_timezone(timedelta(hours=6), "Asia/Urumqi")
        raise


def _parse_hhmm(value: str) -> time:
    parts = value.strip().split(":")
    if len(parts) not in (2, 3):
        raise ValueError("time_of_day must be HH:MM or HH:MM:SS")
    hour = int(parts[0])
    minute = int(parts[1])
    second = int(parts[2]) if len(parts) == 3 else 0
    return time(hour=hour, minute=minute, second=second)


def compute_next_run_at(
    *,
    schedule_type: ScheduleType,
    schedule_expr: dict[str, Any],
    timezone: str,
    after: datetime | None = None,
) -> datetime | None:
    """Compute the next UTC run time for a supported schedule."""
    base = (after or _utc_now()).astimezone(UTC)

    if schedule_type == "once":
        run_at = schedule_expr.get("run_at")
        if not isinstance(run_at, str) or not run_at.strip():
            raise ValueError("schedule_expr.run_at is required for once schedules")
        dt = _parse_dt(run_at, timezone)
        return dt if dt > base else None

    if schedule_type == "interval":
        seconds = int(schedule_expr.get("every_seconds") or 0)
        if seconds < 1:
            raise ValueError("schedule_expr.every_seconds must be >= 1 for interval schedules")
        start_at_raw = schedule_expr.get("start_at")
        if isinstance(start_at_raw, str) and start_at_raw.strip():
            candidate = _parse_dt(start_at_raw, timezone)
        else:
            candidate = base + timedelta(seconds=seconds)
        if candidate <= base:
            # Jump directly to the first future slot.  Iterating one interval
            # at a time can hold the SQLite write transaction for minutes when
            # a legacy anchor is years behind (especially for 1-second tasks).
            elapsed_seconds = (base - candidate).total_seconds()
            elapsed_intervals = int(elapsed_seconds // seconds) + 1
            candidate += timedelta(seconds=elapsed_intervals * seconds)
        return candidate

    if schedule_type == "daily":
        tod_raw = schedule_expr.get("time_of_day")
        if not isinstance(tod_raw, str) or not tod_raw.strip():
            raise ValueError("schedule_expr.time_of_day is required for daily schedules")
        tz = _get_tzinfo(timezone)
        local_base = base.astimezone(tz)
        tod = _parse_hhmm(tod_raw)
        candidate = datetime.combine(local_base.date(), tod, tzinfo=tz)
        if candidate <= local_base:
            candidate += timedelta(days=1)
        return candidate.astimezone(UTC)

    raise ValueError(f"Unsupported schedule_type: {schedule_type}")


@dataclass(frozen=True)
class ScheduledTask:
    id: str
    thread_id: str
    prompt: str
    schedule_type: ScheduleType
    schedule_expr: dict[str, Any]
    timezone: str
    enabled: bool
    next_run_at: str | None
    created_by: str
    created_at: str
    updated_at: str
    metadata: dict[str, Any]
    kwargs: dict[str, Any]
    multitask_strategy: str


class SchedulerStore:
    """SQLite-backed scheduled task repository."""

    def __init__(self, db_path: str | Path) -> None:
        self.db_path = Path(db_path)

    def setup(self) -> None:
        self.db_path.parent.mkdir(parents=True, exist_ok=True)
        with self._connect() as conn:
            conn.executescript(
                """
                CREATE TABLE IF NOT EXISTS scheduled_tasks (
                    id TEXT PRIMARY KEY,
                    thread_id TEXT NOT NULL,
                    prompt TEXT NOT NULL,
                    schedule_type TEXT NOT NULL,
                    schedule_expr TEXT NOT NULL,
                    timezone TEXT NOT NULL,
                    enabled INTEGER NOT NULL DEFAULT 1,
                    next_run_at TEXT,
                    created_by TEXT NOT NULL,
                    created_at TEXT NOT NULL,
                    updated_at TEXT NOT NULL,
                    metadata TEXT NOT NULL DEFAULT '{}',
                    kwargs TEXT NOT NULL DEFAULT '{}',
                    multitask_strategy TEXT NOT NULL DEFAULT 'reject'
                );

                CREATE INDEX IF NOT EXISTS idx_scheduled_tasks_due
                    ON scheduled_tasks(enabled, next_run_at);

                CREATE TABLE IF NOT EXISTS scheduled_task_runs (
                    id TEXT PRIMARY KEY,
                    task_id TEXT NOT NULL,
                    scheduled_at TEXT NOT NULL,
                    started_at TEXT,
                    finished_at TEXT,
                    run_id TEXT,
                    status TEXT NOT NULL,
                    error TEXT,
                    attempt_count INTEGER NOT NULL DEFAULT 0,
                    available_at TEXT,
                    lease_expires_at TEXT,
                    execution_status TEXT,
                    execution_error TEXT,
                    delivery_status TEXT,
                    delivery_error TEXT,
                    created_at TEXT NOT NULL,
                    updated_at TEXT NOT NULL,
                    FOREIGN KEY(task_id) REFERENCES scheduled_tasks(id)
                );

                CREATE INDEX IF NOT EXISTS idx_scheduled_task_runs_task
                    ON scheduled_task_runs(task_id, created_at DESC);

                """
            )
            # CREATE TABLE IF NOT EXISTS does not evolve databases created by
            # older releases.  Add scheduler-run columns in place so existing
            # task definitions and history remain usable after upgrade.
            columns = {row["name"] for row in conn.execute("PRAGMA table_info(scheduled_task_runs)")}
            migrations = {
                "attempt_count": "INTEGER NOT NULL DEFAULT 0",
                "available_at": "TEXT",
                "lease_expires_at": "TEXT",
                "execution_status": "TEXT",
                "execution_error": "TEXT",
                "delivery_status": "TEXT",
                "delivery_error": "TEXT",
            }
            for column, declaration in migrations.items():
                if column not in columns:
                    conn.execute(f"ALTER TABLE scheduled_task_runs ADD COLUMN {column} {declaration}")
            conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_scheduled_task_runs_claimable
                ON scheduled_task_runs(status, available_at, lease_expires_at)
                """
            )

    @contextmanager
    def _connect(self) -> Iterator[sqlite3.Connection]:
        conn = sqlite3.connect(str(self.db_path), check_same_thread=False)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA busy_timeout = 5000")
        conn.execute("PRAGMA foreign_keys = ON")
        try:
            yield conn
            conn.commit()
        except Exception:
            conn.rollback()
            raise
        finally:
            conn.close()

    async def create_task(
        self,
        *,
        thread_id: str,
        prompt: str,
        schedule_type: ScheduleType,
        schedule_expr: dict[str, Any],
        timezone: str,
        created_by: str = "agent",
        metadata: dict[str, Any] | None = None,
        kwargs: dict[str, Any] | None = None,
        multitask_strategy: str = "reject",
    ) -> ScheduledTask:
        normalized_expr = dict(schedule_expr)
        if schedule_type == "interval" and not normalized_expr.get("start_at"):
            seconds = int(normalized_expr.get("every_seconds") or 0)
            if seconds < 1:
                raise ValueError("schedule_expr.every_seconds must be >= 1 for interval schedules")
            normalized_expr["start_at"] = _dt_to_iso(_utc_now() + timedelta(seconds=seconds))

        next_run_at = compute_next_run_at(
            schedule_type=schedule_type,
            schedule_expr=normalized_expr,
            timezone=timezone,
        )
        if next_run_at is None:
            raise ValueError("The schedule has no future run time")
        task_id = str(uuid.uuid4())
        now = _dt_to_iso(_utc_now())

        def _write() -> ScheduledTask:
            with self._connect() as conn:
                conn.execute(
                    """
                    INSERT INTO scheduled_tasks (
                        id, thread_id, prompt, schedule_type, schedule_expr,
                        timezone, enabled, next_run_at, created_by,
                        created_at, updated_at, metadata, kwargs, multitask_strategy
                    )
                    VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        task_id,
                        thread_id,
                        prompt,
                        schedule_type,
                        json.dumps(normalized_expr, ensure_ascii=False),
                        timezone,
                        _dt_to_iso(next_run_at),
                        created_by,
                        now,
                        now,
                        json.dumps(metadata or {}, ensure_ascii=False),
                        json.dumps(kwargs or {}, ensure_ascii=False),
                        multitask_strategy,
                    ),
                )
                row = conn.execute("SELECT * FROM scheduled_tasks WHERE id = ?", (task_id,)).fetchone()
                return self._row_to_task(row)

        return await asyncio.to_thread(_write)

    async def list_tasks(
        self,
        *,
        thread_id: str | None = None,
        include_disabled: bool = False,
        limit: int = 50,
    ) -> list[ScheduledTask]:
        limit = max(1, min(limit, 200))

        def _read() -> list[ScheduledTask]:
            clauses = []
            params: list[Any] = []
            if thread_id:
                clauses.append("thread_id = ?")
                params.append(thread_id)
            if not include_disabled:
                clauses.append("enabled = 1")
            where = "WHERE " + " AND ".join(clauses) if clauses else ""
            with self._connect() as conn:
                rows = conn.execute(
                    f"""
                    SELECT * FROM scheduled_tasks
                    {where}
                    ORDER BY next_run_at IS NULL, next_run_at ASC, created_at DESC
                    LIMIT ?
                    """,
                    (*params, limit),
                ).fetchall()
                return [self._row_to_task(row) for row in rows]

        return await asyncio.to_thread(_read)

    async def get_task(self, task_id: str) -> ScheduledTask | None:
        def _read() -> ScheduledTask | None:
            with self._connect() as conn:
                row = conn.execute("SELECT * FROM scheduled_tasks WHERE id = ?", (task_id,)).fetchone()
                return self._row_to_task(row) if row else None

        return await asyncio.to_thread(_read)

    async def set_enabled(self, task_id: str, enabled: bool) -> ScheduledTask | None:
        now = _dt_to_iso(_utc_now())

        def _write() -> ScheduledTask | None:
            with self._connect() as conn:
                row = conn.execute("SELECT * FROM scheduled_tasks WHERE id = ?", (task_id,)).fetchone()
                if row is None:
                    return None
                next_run_at = row["next_run_at"]
                if enabled and (
                    next_run_at is None
                    or datetime.fromisoformat(next_run_at).astimezone(UTC) <= _utc_now()
                ):
                    task = self._row_to_task(row)
                    next_dt = compute_next_run_at(
                        schedule_type=task.schedule_type,
                        schedule_expr=task.schedule_expr,
                        timezone=task.timezone,
                    )
                    if next_dt is None:
                        raise ValueError("The schedule has no future run time")
                    next_run_at = _dt_to_iso(next_dt) if next_dt else None
                conn.execute(
                    "UPDATE scheduled_tasks SET enabled = ?, next_run_at = ?, updated_at = ? WHERE id = ?",
                    (1 if enabled else 0, next_run_at, now, task_id),
                )
                updated = conn.execute("SELECT * FROM scheduled_tasks WHERE id = ?", (task_id,)).fetchone()
                return self._row_to_task(updated) if updated else None

        return await asyncio.to_thread(_write)

    async def pause_task(
        self,
        task_id: str,
        *,
        reason: str = "Scheduled task paused",
    ) -> tuple[ScheduledTask | None, list[dict[str, Any]]]:
        """Disable a task and terminalize every outstanding occurrence."""
        now = _dt_to_iso(_utc_now())

        def _write() -> tuple[ScheduledTask | None, list[dict[str, Any]]]:
            with self._connect() as conn:
                conn.execute("BEGIN IMMEDIATE")
                row = conn.execute("SELECT * FROM scheduled_tasks WHERE id = ?", (task_id,)).fetchone()
                if row is None:
                    return None, []
                active = [
                    dict(item)
                    for item in conn.execute(
                        """
                        SELECT * FROM scheduled_task_runs
                        WHERE task_id = ?
                          AND status IN ('pending', 'retry', 'claimed', 'running', 'launched')
                        """,
                        (task_id,),
                    ).fetchall()
                ]
                conn.execute(
                    "UPDATE scheduled_tasks SET enabled = 0, updated_at = ? WHERE id = ?",
                    (now, task_id),
                )
                conn.execute(
                    """
                    UPDATE scheduled_task_runs
                    SET status = 'cancelled', error = ?,
                        execution_status = COALESCE(execution_status, 'cancelled'),
                        execution_error = COALESCE(execution_error, ?),
                        available_at = NULL, lease_expires_at = NULL,
                        finished_at = COALESCE(finished_at, ?), updated_at = ?
                    WHERE task_id = ?
                      AND status IN ('pending', 'retry', 'claimed', 'running', 'launched')
                    """,
                    (reason[:4000], reason[:4000], now, now, task_id),
                )
                updated = conn.execute("SELECT * FROM scheduled_tasks WHERE id = ?", (task_id,)).fetchone()
                return self._row_to_task(updated), active

        return await asyncio.to_thread(_write)

    async def delete_task(self, task_id: str) -> bool:
        def _write() -> bool:
            with self._connect() as conn:
                conn.execute("DELETE FROM scheduled_task_runs WHERE task_id = ?", (task_id,))
                cur = conn.execute("DELETE FROM scheduled_tasks WHERE id = ?", (task_id,))
                return cur.rowcount > 0

        return await asyncio.to_thread(_write)

    async def delete_tasks_for_thread(self, thread_id: str) -> int:
        """Delete all task definitions/history when no scheduler is active."""
        def _write() -> int:
            with self._connect() as conn:
                conn.execute("BEGIN IMMEDIATE")
                task_ids = [
                    row["id"]
                    for row in conn.execute(
                        "SELECT id FROM scheduled_tasks WHERE thread_id = ?",
                        (thread_id,),
                    ).fetchall()
                ]
                if not task_ids:
                    return 0
                placeholders = ",".join("?" for _ in task_ids)
                conn.execute(
                    f"DELETE FROM scheduled_task_runs WHERE task_id IN ({placeholders})",
                    task_ids,
                )
                conn.execute(
                    "DELETE FROM scheduled_tasks WHERE thread_id = ?",
                    (thread_id,),
                )
                return len(task_ids)

        return await asyncio.to_thread(_write)

    async def claim_due_tasks(
        self,
        *,
        now: datetime,
        limit: int = 10,
        lease_seconds: float = 120.0,
        max_attempts: int = 3,
    ) -> list[tuple[ScheduledTask, str, str, int]]:
        """Materialize due occurrences and lease runnable rows atomically.

        A task definition may advance as soon as its occurrence is persisted,
        but the occurrence itself remains recoverable.  Expired ``claimed`` or
        ``running`` leases are retried after a process crash.
        """
        limit = max(1, min(limit, 100))
        max_attempts = max(1, max_attempts)
        now = now.astimezone(UTC)
        now_iso = _dt_to_iso(now)
        lease_expires_at = _dt_to_iso(now + timedelta(seconds=max(1.0, lease_seconds)))

        def _claim() -> list[tuple[ScheduledTask, str, str, int]]:
            claimed: list[tuple[ScheduledTask, str, str, int]] = []
            with self._connect() as conn:
                conn.execute("BEGIN IMMEDIATE")

                # First turn due schedule definitions into durable occurrences.
                due_rows = conn.execute(
                    """
                    SELECT * FROM scheduled_tasks
                    WHERE enabled = 1 AND next_run_at IS NOT NULL AND next_run_at <= ?
                    ORDER BY next_run_at ASC
                    LIMIT ?
                    """,
                    (now_iso, limit),
                ).fetchall()
                for row in due_rows:
                    run_row_id = str(uuid.uuid4())
                    scheduled_at = row["next_run_at"] or now_iso
                    try:
                        task = self._row_to_task(row)
                        schedule_expr = dict(task.schedule_expr)
                        # Legacy interval rows may not have a persisted anchor.
                        # Anchor them to the occurrence being consumed so polling
                        # latency does not accumulate as schedule drift.
                        if task.schedule_type == "interval" and not schedule_expr.get("start_at"):
                            schedule_expr["start_at"] = scheduled_at
                        next_dt = compute_next_run_at(
                            schedule_type=task.schedule_type,
                            schedule_expr=schedule_expr,
                            timezone=task.timezone,
                            after=now,
                        )
                    except Exception as exc:
                        detail = f"Invalid schedule quarantined: {type(exc).__name__}: {exc}"[:2000]
                        conn.execute(
                            "UPDATE scheduled_tasks SET enabled = 0, next_run_at = NULL, updated_at = ? WHERE id = ?",
                            (now_iso, row["id"]),
                        )
                        conn.execute(
                            """
                            INSERT INTO scheduled_task_runs (
                                id, task_id, scheduled_at, status, error,
                                execution_status, execution_error,
                                attempt_count, created_at, updated_at, finished_at
                            ) VALUES (?, ?, ?, 'dead_letter', ?, 'error', ?, 0, ?, ?, ?)
                            """,
                            (run_row_id, row["id"], scheduled_at, detail, detail, now_iso, now_iso, now_iso),
                        )
                        logger.error("Scheduled task %s was quarantined: %s", row["id"], detail)
                        continue

                    if task.schedule_type == "once":
                        enabled = 0
                        next_run_at = None
                    else:
                        next_run_at = _dt_to_iso(next_dt) if next_dt else None
                        enabled = 1 if next_run_at is not None else 0
                    conn.execute(
                        """
                        UPDATE scheduled_tasks
                        SET enabled = ?, next_run_at = ?, schedule_expr = ?, updated_at = ?
                        WHERE id = ?
                        """,
                        (enabled, next_run_at, json.dumps(schedule_expr, ensure_ascii=False), now_iso, task.id),
                    )
                    conn.execute(
                        """
                        INSERT INTO scheduled_task_runs (
                            id, task_id, scheduled_at, status, attempt_count,
                            available_at, created_at, updated_at
                        ) VALUES (?, ?, ?, 'pending', 0, ?, ?, ?)
                        """,
                        (run_row_id, task.id, scheduled_at, now_iso, now_iso, now_iso),
                    )

                claimable_sql = """
                    (
                        status IN ('pending', 'retry')
                        AND COALESCE(available_at, scheduled_at) <= ?
                    ) OR (
                        status IN ('claimed', 'running')
                        AND (lease_expires_at IS NULL OR lease_expires_at <= ?)
                    )
                """
                # Exhausted stale/pending work is retained as a dead letter
                # instead of being silently stranded forever.
                conn.execute(
                    f"""
                    UPDATE scheduled_task_runs
                    SET status = 'dead_letter',
                        error = COALESCE(error, 'Maximum scheduler attempts exhausted'),
                        finished_at = COALESCE(finished_at, ?),
                        lease_expires_at = NULL,
                        updated_at = ?
                    WHERE attempt_count >= ? AND ({claimable_sql})
                    """,
                    (now_iso, now_iso, max_attempts, now_iso, now_iso),
                )

                candidates = conn.execute(
                    f"""
                    SELECT runs.id, runs.task_id, runs.scheduled_at, runs.attempt_count
                    FROM scheduled_task_runs AS runs
                    JOIN scheduled_tasks AS tasks ON tasks.id = runs.task_id
                    WHERE (tasks.enabled = 1 OR tasks.schedule_type = 'once')
                      AND runs.attempt_count < ? AND ({claimable_sql})
                    ORDER BY COALESCE(runs.available_at, runs.scheduled_at), runs.created_at
                    LIMIT ?
                    """,
                    (max_attempts, now_iso, now_iso, limit * 4),
                ).fetchall()
                # Include leases acquired by every scheduler process, not only
                # rows selected in this transaction.  Conversations share a
                # checkpoint namespace and must be serialized across workers.
                claimed_threads = {
                    row["thread_id"]
                    for row in conn.execute(
                        """
                        SELECT DISTINCT tasks.thread_id
                        FROM scheduled_task_runs AS runs
                        JOIN scheduled_tasks AS tasks ON tasks.id = runs.task_id
                        WHERE runs.status = 'launched'
                           OR (
                              runs.status IN ('claimed', 'running')
                              AND runs.lease_expires_at IS NOT NULL
                              AND runs.lease_expires_at > ?
                           )
                        """,
                        (now_iso,),
                    ).fetchall()
                }
                for run_row in candidates:
                    if len(claimed) >= limit:
                        break
                    task_row = conn.execute("SELECT * FROM scheduled_tasks WHERE id = ?", (run_row["task_id"],)).fetchone()
                    if task_row is None:
                        conn.execute(
                            "UPDATE scheduled_task_runs SET status = 'dead_letter', error = ?, finished_at = ?, updated_at = ? WHERE id = ?",
                            ("Scheduled task definition no longer exists", now_iso, now_iso, run_row["id"]),
                        )
                        continue
                    try:
                        task = self._row_to_task(task_row)
                    except Exception as exc:
                        detail = f"Invalid task data: {type(exc).__name__}: {exc}"[:2000]
                        conn.execute(
                            "UPDATE scheduled_tasks SET enabled = 0, next_run_at = NULL, updated_at = ? WHERE id = ?",
                            (now_iso, run_row["task_id"]),
                        )
                        conn.execute(
                            "UPDATE scheduled_task_runs SET status = 'dead_letter', error = ?, execution_status = 'error', execution_error = ?, finished_at = ?, updated_at = ? WHERE id = ?",
                            (detail, detail, now_iso, now_iso, run_row["id"]),
                        )
                        continue
                    # No process may safely run two occurrences against the
                    # same conversation/checkpoint concurrently.
                    if task.thread_id in claimed_threads:
                        continue
                    claimed_threads.add(task.thread_id)
                    conn.execute(
                        """
                        UPDATE scheduled_task_runs
                        SET status = 'claimed', attempt_count = attempt_count + 1,
                            available_at = NULL, lease_expires_at = ?, run_id = NULL,
                            started_at = NULL, finished_at = NULL, error = NULL,
                            execution_status = NULL, execution_error = NULL,
                            delivery_status = NULL, delivery_error = NULL,
                            updated_at = ?
                        WHERE id = ?
                        """,
                        (lease_expires_at, now_iso, run_row["id"]),
                    )
                    claimed.append(
                        (
                            task,
                            run_row["id"],
                            run_row["scheduled_at"],
                            int(run_row["attempt_count"] or 0) + 1,
                        )
                    )
            return claimed

        return await asyncio.to_thread(_claim)

    async def mark_task_run_started(self, task_run_id: str, run_id: str, *, attempt_count: int) -> bool:
        now = _dt_to_iso(_utc_now())

        def _write() -> bool:
            with self._connect() as conn:
                cur = conn.execute(
                    """
                    UPDATE scheduled_task_runs
                    SET run_id = ?, status = 'running', started_at = COALESCE(started_at, ?), updated_at = ?
                    WHERE id = ? AND attempt_count = ? AND status IN ('claimed', 'running')
                      AND EXISTS (
                          SELECT 1 FROM scheduled_tasks
                          WHERE scheduled_tasks.id = scheduled_task_runs.task_id
                            AND (
                                scheduled_tasks.enabled = 1
                                OR scheduled_tasks.schedule_type = 'once'
                            )
                      )
                    """,
                    (run_id, now, now, task_run_id, attempt_count),
                )
                return cur.rowcount > 0

        return await asyncio.to_thread(_write)

    async def renew_task_run_lease(
        self,
        task_run_id: str,
        *,
        attempt_count: int,
        lease_seconds: float,
    ) -> bool:
        now = _utc_now()
        lease_expires_at = _dt_to_iso(now + timedelta(seconds=max(1.0, lease_seconds)))
        now_iso = _dt_to_iso(now)

        def _write() -> bool:
            with self._connect() as conn:
                cur = conn.execute(
                    """
                    UPDATE scheduled_task_runs
                    SET lease_expires_at = ?, updated_at = ?
                    WHERE id = ? AND attempt_count = ? AND status IN ('claimed', 'running', 'launched')
                    """,
                    (lease_expires_at, now_iso, task_run_id, attempt_count),
                )
                return cur.rowcount > 0

        return await asyncio.to_thread(_write)

    async def reconcile_expired_launched_runs(
        self,
        *,
        now: datetime,
        orphan_grace_seconds: float = 3600.0,
    ) -> int:
        """Terminalize expired launch fences without replaying the prompt."""
        now = now.astimezone(UTC)
        now_iso = _dt_to_iso(now)
        orphaned_before = _dt_to_iso(
            now - timedelta(seconds=max(0.0, orphan_grace_seconds))
        )
        reason = "Scheduler owner disappeared after managed run launch; prompt was not retried"

        def _write() -> int:
            with self._connect() as conn:
                cur = conn.execute(
                    """
                    UPDATE scheduled_task_runs
                    SET status = 'interrupted', error = ?,
                        execution_status = 'unknown', execution_error = ?,
                        lease_expires_at = NULL,
                        finished_at = COALESCE(finished_at, ?), updated_at = ?
                    WHERE status = 'launched'
                      AND (lease_expires_at IS NULL OR lease_expires_at <= ?)
                      AND updated_at <= ?
                    """,
                    (reason, reason, now_iso, now_iso, now_iso, orphaned_before),
                )
                return cur.rowcount

        return await asyncio.to_thread(_write)

    async def mark_task_run_launched(
        self,
        task_run_id: str,
        run_id: str,
        *,
        attempt_count: int,
    ) -> bool:
        """Fence an occurrence once a managed run has actually been created.

        ``launched`` rows are intentionally not part of ``claim_due_tasks``'
        expired-lease recovery set.  After this boundary, automatic prompt
        replay is unsafe because the managed run may already have committed
        external side effects.  Explicit retry decisions (shutdown after
        cancellation or a classified transient LLM failure) still use
        ``reschedule_task_run`` below.
        """
        now = _dt_to_iso(_utc_now())

        def _write() -> bool:
            with self._connect() as conn:
                cur = conn.execute(
                    """
                    UPDATE scheduled_task_runs
                    SET run_id = ?, status = 'launched',
                        started_at = COALESCE(started_at, ?), updated_at = ?
                    WHERE id = ? AND attempt_count = ?
                      AND status IN ('claimed', 'running', 'launched')
                    """,
                    (run_id, now, now, task_run_id, attempt_count),
                )
                return cur.rowcount > 0

        return await asyncio.to_thread(_write)

    async def get_task_run(self, task_run_id: str) -> dict[str, Any] | None:
        def _read() -> dict[str, Any] | None:
            with self._connect() as conn:
                row = conn.execute("SELECT * FROM scheduled_task_runs WHERE id = ?", (task_run_id,)).fetchone()
                return dict(row) if row else None

        return await asyncio.to_thread(_read)

    async def reschedule_task_run(
        self,
        task_run_id: str,
        *,
        error: str,
        delay_seconds: float,
        max_attempts: int,
        attempt_count: int,
        execution_status: str | None = None,
        execution_error: str | None = None,
        delivery_status: str | None = None,
        delivery_error: str | None = None,
    ) -> bool | None:
        """Retry a leased occurrence, or dead-letter it when exhausted.

        Returns ``True`` when another attempt was scheduled, ``False`` when
        exhausted, and ``None`` when this caller no longer owns the lease.
        """
        now = _utc_now()
        now_iso = _dt_to_iso(now)
        available_at = _dt_to_iso(now + timedelta(seconds=max(0.0, delay_seconds)))

        def _write() -> bool | None:
            with self._connect() as conn:
                retry = attempt_count < max(1, max_attempts)
                status = "retry" if retry else "dead_letter"
                cur = conn.execute(
                    """
                    UPDATE scheduled_task_runs
                    SET status = ?, error = ?, available_at = ?, lease_expires_at = NULL,
                        execution_status = ?, execution_error = ?,
                        delivery_status = ?, delivery_error = ?,
                        finished_at = ?, updated_at = ?
                    WHERE id = ? AND attempt_count = ? AND status IN ('claimed', 'running', 'launched')
                    """,
                    (
                        status,
                        error[:4000],
                        available_at if retry else None,
                        execution_status,
                        execution_error,
                        delivery_status,
                        delivery_error,
                        None if retry else now_iso,
                        now_iso,
                        task_run_id,
                        attempt_count,
                    ),
                )
                return retry if cur.rowcount > 0 else None

        return await asyncio.to_thread(_write)

    async def mark_task_run_finished(
        self,
        task_run_id: str,
        status: str,
        error: str | None = None,
        *,
        attempt_count: int | None = None,
    ) -> bool:
        return await self.mark_task_run_finished_detailed(
            task_run_id,
            status=status,
            error=error,
            execution_status=status,
            execution_error=error,
            delivery_status="not_requested",
            delivery_error=None,
            attempt_count=attempt_count,
        )

    async def mark_task_run_finished_detailed(
        self,
        task_run_id: str,
        *,
        status: str,
        error: str | None,
        execution_status: str | None,
        execution_error: str | None,
        delivery_status: str | None,
        delivery_error: str | None,
        attempt_count: int | None = None,
    ) -> bool:
        now = _dt_to_iso(_utc_now())

        def _write() -> bool:
            with self._connect() as conn:
                attempt_clause = (
                    ""
                    if attempt_count is None
                    else " AND attempt_count = ? AND status IN ('claimed', 'running', 'launched')"
                )
                params: list[Any] = [
                    status,
                    error,
                    execution_status,
                    execution_error,
                    delivery_status,
                    delivery_error,
                    now,
                    now,
                    task_run_id,
                ]
                if attempt_count is not None:
                    params.append(attempt_count)
                cur = conn.execute(
                    f"""
                    UPDATE scheduled_task_runs
                    SET status = ?, error = ?, execution_status = ?, execution_error = ?,
                        delivery_status = ?, delivery_error = ?, lease_expires_at = NULL,
                        finished_at = ?, updated_at = ?
                    WHERE id = ?{attempt_clause}
                    """,
                    params,
                )
                return cur.rowcount > 0

        return await asyncio.to_thread(_write)

    async def list_task_runs(
        self,
        task_id: str,
        *,
        limit: int = 20,
        offset: int = 0,
        status: str | None = None,
    ) -> list[dict[str, Any]]:
        # The public history tool allows 100 and requests one sentinel row to
        # compute has_more, so this internal reader accepts 101.
        limit = max(1, min(limit, 101))
        offset = max(0, offset)

        def _read() -> list[dict[str, Any]]:
            with self._connect() as conn:
                where = "WHERE task_id = ?"
                params: list[Any] = [task_id]
                if status is not None:
                    where += " AND status = ?"
                    params.append(status)
                params.extend((limit, offset))
                rows = conn.execute(
                    f"""
                    SELECT * FROM scheduled_task_runs
                    {where}
                    ORDER BY created_at DESC, id DESC
                    LIMIT ? OFFSET ?
                    """,
                    params,
                ).fetchall()
                return [dict(row) for row in rows]

        return await asyncio.to_thread(_read)

    async def cleanup_task_runs(self, *, older_than: datetime, max_per_task: int) -> int:
        """Delete old terminal history while preserving live/retry occurrences."""
        cutoff = _dt_to_iso(older_than)
        max_per_task = max(1, max_per_task)
        terminal = ("success", "error", "timeout", "interrupted", "cancelled", "dead_letter")

        def _write() -> int:
            deleted = 0
            placeholders = ",".join("?" for _ in terminal)
            with self._connect() as conn:
                cur = conn.execute(
                    f"DELETE FROM scheduled_task_runs WHERE status IN ({placeholders}) AND finished_at IS NOT NULL AND finished_at < ?",
                    (*terminal, cutoff),
                )
                deleted += max(0, cur.rowcount)
                task_ids = [
                    row["task_id"]
                    for row in conn.execute("SELECT DISTINCT task_id FROM scheduled_task_runs").fetchall()
                ]
                for task_id in task_ids:
                    stale = conn.execute(
                        f"""
                        SELECT id FROM scheduled_task_runs
                        WHERE task_id = ? AND status IN ({placeholders})
                        ORDER BY created_at DESC
                        LIMIT -1 OFFSET ?
                        """,
                        (task_id, *terminal, max_per_task),
                    ).fetchall()
                    if stale:
                        ids = [row["id"] for row in stale]
                        id_placeholders = ",".join("?" for _ in ids)
                        cur = conn.execute(
                            f"DELETE FROM scheduled_task_runs WHERE id IN ({id_placeholders})",
                            ids,
                        )
                        deleted += max(0, cur.rowcount)
            return deleted

        return await asyncio.to_thread(_write)

    @staticmethod
    def _row_to_task(row: sqlite3.Row) -> ScheduledTask:
        return ScheduledTask(
            id=row["id"],
            thread_id=row["thread_id"],
            prompt=row["prompt"],
            schedule_type=row["schedule_type"],
            schedule_expr=json.loads(row["schedule_expr"] or "{}"),
            timezone=row["timezone"],
            enabled=bool(row["enabled"]),
            next_run_at=row["next_run_at"],
            created_by=row["created_by"],
            created_at=row["created_at"],
            updated_at=row["updated_at"],
            metadata=json.loads(row["metadata"] or "{}"),
            kwargs=json.loads(row["kwargs"] or "{}"),
            multitask_strategy=row["multitask_strategy"],
        )


class SchedulerService:
    """Lease-based scheduler with at-least-once occurrence execution.

    Retrying an occurrence reruns its full prompt.  Scheduled prompts and tools
    that mutate external systems should therefore be designed to be idempotent.
    """

    def __init__(
        self,
        *,
        store: SchedulerStore,
        manager: Any,
        poll_interval_seconds: float,
        default_timezone: str = "Asia/Shanghai",
        recursion_limit_resolver: Callable[[], int] | None = None,
        max_concurrent_runs: int = 4,
        max_attempts: int = 3,
        retry_base_seconds: float = 15.0,
        claim_lease_seconds: float = 120.0,
        shutdown_grace_seconds: float = 10.0,
        run_retention_days: int = 30,
        max_runs_per_task: int = 1000,
    ) -> None:
        self.store = store
        self.manager = manager
        self.poll_interval_seconds = max(0.5, poll_interval_seconds)
        self.default_timezone = default_timezone
        self.recursion_limit_resolver = recursion_limit_resolver
        self.max_concurrent_runs = max(1, max_concurrent_runs)
        self.max_attempts = max(1, max_attempts)
        self.retry_base_seconds = max(0.0, retry_base_seconds)
        self.claim_lease_seconds = max(5.0, claim_lease_seconds)
        self.shutdown_grace_seconds = max(0.0, shutdown_grace_seconds)
        self.run_retention_days = max(1, run_retention_days)
        self.max_runs_per_task = max(1, max_runs_per_task)
        self._task: asyncio.Task | None = None
        self._stopping = asyncio.Event()
        self._dispatch_tasks: set[asyncio.Task] = set()
        self._dispatch_tasks_by_task_id: dict[str, set[asyncio.Task]] = {}
        self._last_cleanup_at = 0.0

    async def start(self) -> None:
        if self._task is not None and not self._task.done():
            return
        self.store.setup()
        reconciled = await self.store.reconcile_expired_launched_runs(now=_utc_now())
        if reconciled:
            logger.warning(
                "Reconciled %d orphaned launched scheduler occurrence(s) without replay",
                reconciled,
            )
        set_scheduler_service(self)
        self._stopping.clear()
        self._task = asyncio.create_task(self._run_loop(), name="deerflow-scheduler")
        logger.info("Scheduler service started")

    async def stop(self) -> None:
        self._stopping.set()
        if self._task is not None:
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
            self._task = None
        active = {task for task in self._dispatch_tasks if not task.done()}
        if active and self.shutdown_grace_seconds > 0:
            _done, active = await asyncio.wait(active, timeout=self.shutdown_grace_seconds)
        if active:
            for task in active:
                task.cancel()
            await asyncio.gather(*active, return_exceptions=True)
        self._dispatch_tasks.clear()
        self._dispatch_tasks_by_task_id.clear()
        set_scheduler_service(None)
        logger.info("Scheduler service stopped")

    async def _run_loop(self) -> None:
        while not self._stopping.is_set():
            try:
                await self.tick()
            except asyncio.CancelledError:
                raise
            except Exception:
                logger.exception("Scheduler tick failed")
            await asyncio.sleep(self.poll_interval_seconds)

    async def tick(self) -> None:
        loop = asyncio.get_running_loop()
        if loop.time() - self._last_cleanup_at >= 3600:
            deleted = await self.store.cleanup_task_runs(
                older_than=_utc_now() - timedelta(days=self.run_retention_days),
                max_per_task=self.max_runs_per_task,
            )
            if deleted:
                logger.info("Cleaned up %d old scheduled-task run records", deleted)
            self._last_cleanup_at = loop.time()

        reconciled = await self.store.reconcile_expired_launched_runs(now=_utc_now())
        if reconciled:
            logger.warning(
                "Reconciled %d expired launched scheduler occurrence(s) without replay",
                reconciled,
            )

        active_count = sum(1 for task in self._dispatch_tasks if not task.done())
        capacity = self.max_concurrent_runs - active_count
        if capacity <= 0:
            return
        claimed = await self.store.claim_due_tasks(
            now=_utc_now(),
            limit=min(10, capacity),
            lease_seconds=self.claim_lease_seconds,
            max_attempts=self.max_attempts,
        )
        for task, task_run_id, scheduled_at, attempt_count in claimed:
            dispatch_task = asyncio.create_task(
                self._dispatch(task, task_run_id, scheduled_at, attempt_count),
                name=f"scheduled-dispatch-{task.id}-{task_run_id}",
            )
            self._dispatch_tasks.add(dispatch_task)
            self._dispatch_tasks_by_task_id.setdefault(task.id, set()).add(dispatch_task)
            dispatch_task.add_done_callback(
                lambda completed, task_id=task.id: self._on_dispatch_done(task_id, completed)
            )

    def _on_dispatch_done(self, task_id: str, task: asyncio.Task) -> None:
        self._dispatch_tasks.discard(task)
        by_task = self._dispatch_tasks_by_task_id.get(task_id)
        if by_task is not None:
            by_task.discard(task)
            if not by_task:
                self._dispatch_tasks_by_task_id.pop(task_id, None)
        if task.cancelled():
            return
        exc = task.exception()
        if exc is not None:
            logger.error("Scheduled dispatch task escaped with an error", exc_info=exc)

    async def set_task_enabled(self, task_id: str, enabled: bool) -> ScheduledTask | None:
        """Change task state while coordinating outstanding occurrences."""
        if enabled:
            return await self.store.set_enabled(task_id, True)
        task, active = await self.store.pause_task(task_id)
        if task is None:
            return None
        await self._cancel_active_task_work(task_id, active, reason="Scheduled task paused")
        return task

    async def delete_task(self, task_id: str) -> bool:
        """Fence/cancel active work before deleting the durable definition."""
        task, active = await self.store.pause_task(
            task_id,
            reason="Scheduled task deleted",
        )
        if task is None:
            return False
        await self._cancel_active_task_work(task_id, active, reason="Scheduled task deleted")
        return await self.store.delete_task(task_id)

    async def delete_tasks_for_thread(self, thread_id: str) -> int:
        """Delete every enabled or disabled schedule owned by one thread."""
        deleted = 0
        while True:
            tasks = await self.store.list_tasks(
                thread_id=thread_id,
                include_disabled=True,
                limit=200,
            )
            if not tasks:
                return deleted
            for task in tasks:
                if await self.delete_task(task.id):
                    deleted += 1

    async def _cancel_active_task_work(
        self,
        task_id: str,
        active_rows: list[dict[str, Any]],
        *,
        reason: str,
    ) -> None:
        run_ids = {
            str(row["run_id"])
            for row in active_rows
            if row.get("run_id")
        }
        for run_id in run_ids:
            try:
                await self.manager.cancel_run(run_id, action="interrupt")
            except Exception:
                logger.warning("Could not cancel scheduled managed run %s", run_id, exc_info=True)

        dispatches = {
            task
            for task in self._dispatch_tasks_by_task_id.get(task_id, set())
            if not task.done()
        }
        for dispatch in dispatches:
            dispatch.cancel(msg=reason)
        if dispatches:
            await asyncio.gather(*dispatches, return_exceptions=True)

    def _retry_delay_seconds(self, attempt_count: int) -> float:
        return min(self.retry_base_seconds * (2 ** max(0, attempt_count - 1)), 300.0)

    async def _renew_lease(self, task_run_id: str, attempt_count: int) -> None:
        interval = max(1.0, min(self.claim_lease_seconds / 3, 30.0))
        while True:
            await asyncio.sleep(interval)
            renewed = await self.store.renew_task_run_lease(
                task_run_id,
                attempt_count=attempt_count,
                lease_seconds=self.claim_lease_seconds,
            )
            if not renewed:
                return

    async def _reschedule_failure(
        self,
        task_run_id: str,
        *,
        attempt_count: int,
        error: str,
        execution_status: str | None = None,
        execution_error: str | None = None,
        delivery_status: str | None = None,
        delivery_error: str | None = None,
        immediate: bool = False,
    ) -> bool:
        retry = await self.store.reschedule_task_run(
            task_run_id,
            error=error,
            delay_seconds=0.0 if immediate else self._retry_delay_seconds(attempt_count),
            max_attempts=self.max_attempts,
            attempt_count=attempt_count,
            execution_status=execution_status,
            execution_error=execution_error,
            delivery_status=delivery_status,
            delivery_error=delivery_error,
        )
        if retry is None:
            logger.warning(
                "Ignored stale scheduler attempt %d for occurrence %s",
                attempt_count,
                task_run_id,
            )
        elif retry:
            logger.warning(
                "Scheduled occurrence %s will retry after attempt %d/%d: %s",
                task_run_id,
                attempt_count,
                self.max_attempts,
                error,
            )
        else:
            logger.error(
                "Scheduled occurrence %s moved to dead letter after %d attempt(s): %s",
                task_run_id,
                attempt_count,
                error,
            )
        return bool(retry)

    async def _dispatch(
        self,
        task: ScheduledTask,
        task_run_id: str,
        scheduled_at: str,
        attempt_count: int | None = None,
    ) -> None:
        if attempt_count is None:
            run_row = await self.store.get_task_run(task_run_id)
            attempt_count = int((run_row or {}).get("attempt_count") or 1)
        managed_run_id = f"{task_run_id}-a{attempt_count}"
        heartbeat: asyncio.Task | None = None
        delivery_task: asyncio.Task | None = None
        record: Any | None = None
        launch_succeeded = False
        delivery_status = "not_requested"
        delivery_error: str | None = None
        try:
            owns_lease = await self.store.mark_task_run_started(
                task_run_id,
                managed_run_id,
                attempt_count=attempt_count,
            )
            if not owns_lease:
                logger.warning(
                    "Skipped stale scheduler attempt %d for occurrence %s",
                    attempt_count,
                    task_run_id,
                )
                return
            heartbeat = asyncio.create_task(
                self._renew_lease(task_run_id, attempt_count),
                name=f"scheduled-lease-{task_run_id}-a{attempt_count}",
            )
            run_kwargs = dict(task.kwargs)
            if self.recursion_limit_resolver is not None:
                # Resolve at dispatch time so an Admin hot update applies to the
                # next scheduled occurrence without restarting the scheduler.
                run_kwargs["recursion_limit"] = int(self.recursion_limit_resolver())
            record = await self.manager.start_client_stream_run(
                thread_id=task.thread_id,
                message=task.prompt,
                kwargs=run_kwargs,
                run_id=managed_run_id,
                entrypoint="scheduled_task",
                on_disconnect="continue",
                multitask_strategy=task.multitask_strategy,
            )
            # From this point onward a managed run exists.  A later scheduler
            # bookkeeping failure must never turn this occurrence back into a
            # retry while that run may still execute.
            launch_succeeded = True
            launch_recorded = await self.store.mark_task_run_launched(
                task_run_id,
                record.run_id,
                attempt_count=attempt_count,
            )
            if not launch_recorded:
                logger.warning(
                    "Managed run %s launched after scheduler attempt %d lost ownership of occurrence %s",
                    record.run_id,
                    attempt_count,
                    task_run_id,
                )
            if isinstance(task.metadata.get("delivery"), dict):
                try:
                    delivery_task = self._start_delivery(task, record.run_id)
                    delivery_status = "running"
                except Exception as exc:
                    delivery_status = "error"
                    delivery_error = f"Delivery unavailable: {exc}"[:2000]
                    logger.warning("Scheduled task %s could not start delivery: %s", task.id, exc)
            if record.task is not None:
                try:
                    await record.task
                except asyncio.CancelledError:
                    raise
                except Exception:
                    logger.warning("Scheduled run task raised unexpectedly", exc_info=True)
            if delivery_task is not None:
                try:
                    await delivery_task
                    delivery_status = "success"
                except asyncio.CancelledError:
                    raise
                except Exception as exc:
                    delivery_status = "error"
                    delivery_error = f"Delivery failed: {exc}"
                    logger.warning("Scheduled task %s delivery failed", task.id, exc_info=True)
            current = self.manager.run_manager.get(record.run_id)
            execution_status = current.status.value if current is not None else "unknown"
            execution_error = current.error if current is not None else "Managed run metadata disappeared"
            # Do not rerun a full agent prompt for arbitrary execution errors:
            # tools may already have committed external side effects.  Only a
            # terminal LLM event explicitly classified as transient opts into
            # occurrence retry (which still has at-least-once semantics).
            llm_retriable = False
            if current is not None:
                metadata = getattr(current, "metadata", {}) or {}
                llm_retriable = bool(metadata.get("llm_failure_retriable", False))

            if execution_status != "success":
                error = execution_error or f"Execution ended with status {execution_status}"
                if llm_retriable:
                    await self._reschedule_failure(
                        task_run_id,
                        attempt_count=attempt_count,
                        error=error,
                        execution_status=execution_status,
                        execution_error=execution_error,
                        delivery_status=delivery_status,
                        delivery_error=delivery_error,
                    )
                else:
                    await self.store.mark_task_run_finished_detailed(
                        task_run_id,
                        status="error",
                        error=error,
                        execution_status=execution_status,
                        execution_error=execution_error,
                        delivery_status=delivery_status,
                        delivery_error=delivery_error,
                        attempt_count=attempt_count,
                    )
                return

            overall_status = "error" if delivery_status == "error" else "success"
            overall_error = delivery_error if delivery_status == "error" else None
            await self.store.mark_task_run_finished_detailed(
                task_run_id,
                status=overall_status,
                error=overall_error,
                execution_status=execution_status,
                execution_error=execution_error,
                delivery_status=delivery_status,
                delivery_error=delivery_error,
                attempt_count=attempt_count,
            )
            logger.info("Scheduled task %s fired at %s as run %s", task.id, scheduled_at, record.run_id)
        except asyncio.CancelledError:
            if delivery_task is not None and not delivery_task.done():
                delivery_task.cancel()
                await asyncio.gather(delivery_task, return_exceptions=True)
            # Scheduler shutdown cancels the managed task before requeueing the
            # interrupted occurrence.  This keeps the existing at-least-once
            # recovery contract without allowing the old and new attempts to
            # run concurrently.
            if launch_succeeded and record is not None:
                managed_task = getattr(record, "task", None)
                if managed_task is not None and not managed_task.done():
                    managed_task.cancel()
                    await asyncio.gather(managed_task, return_exceptions=True)
            await self._reschedule_failure(
                task_run_id,
                attempt_count=attempt_count,
                error="Scheduler shutdown interrupted the occurrence",
                execution_status="interrupted",
                execution_error="Scheduler shutdown",
                immediate=True,
            )
            raise
        except Exception as exc:
            logger.exception("Scheduled task %s dispatch failed", task.id)
            if launch_succeeded and record is not None:
                await self._reconcile_launched_run(
                    task=task,
                    task_run_id=task_run_id,
                    attempt_count=attempt_count,
                    record=record,
                    delivery_task=delivery_task,
                    delivery_status=delivery_status,
                    delivery_error=delivery_error,
                    bookkeeping_error=str(exc) or type(exc).__name__,
                )
            else:
                await self._reschedule_failure(
                    task_run_id,
                    attempt_count=attempt_count,
                    error=str(exc) or type(exc).__name__,
                    execution_status="error",
                    execution_error=str(exc) or type(exc).__name__,
                )
        finally:
            if heartbeat is not None:
                heartbeat.cancel()
                await asyncio.gather(heartbeat, return_exceptions=True)

    async def _reconcile_launched_run(
        self,
        *,
        task: ScheduledTask,
        task_run_id: str,
        attempt_count: int,
        record: Any,
        delivery_task: asyncio.Task | None,
        delivery_status: str,
        delivery_error: str | None,
        bookkeeping_error: str,
    ) -> None:
        """Finish bookkeeping after launch without ever re-running the prompt.

        The normal dispatch path can fail after ``start_client_stream_run`` has
        returned (for example while correcting a run ID or writing the terminal
        occurrence row).  At that point retrying the occurrence is unsafe: the
        managed run may already have produced external side effects.  Wait for
        the already-launched work and make one best-effort terminal write.
        """
        logger.error(
            "Scheduled task %s launched as run %s but scheduler bookkeeping failed: %s",
            task.id,
            getattr(record, "run_id", None),
            bookkeeping_error,
        )
        try:
            try:
                launch_recorded = await self.store.mark_task_run_launched(
                    task_run_id,
                    record.run_id,
                    attempt_count=attempt_count,
                )
                if not launch_recorded:
                    logger.warning(
                        "Could not persist launch fence for stale scheduler attempt %d on occurrence %s",
                        attempt_count,
                        task_run_id,
                    )
            except Exception:
                logger.exception(
                    "Could not persist launch fence for run %s; continuing reconciliation without requeue",
                    record.run_id,
                )

            managed_task = getattr(record, "task", None)
            if managed_task is not None:
                try:
                    await managed_task
                except asyncio.CancelledError:
                    if asyncio.current_task() is not None and asyncio.current_task().cancelling():
                        raise
                    logger.warning("Launched scheduled run %s was cancelled", record.run_id)
                except Exception:
                    logger.warning("Launched scheduled run task raised during reconciliation", exc_info=True)

            if delivery_task is not None:
                try:
                    await delivery_task
                    delivery_status = "success"
                except asyncio.CancelledError:
                    if asyncio.current_task() is not None and asyncio.current_task().cancelling():
                        raise
                    delivery_status = "error"
                    delivery_error = "Delivery was cancelled"
                except Exception as exc:
                    delivery_status = "error"
                    delivery_error = f"Delivery failed: {exc}"

            current = self.manager.run_manager.get(record.run_id)
            execution_status = current.status.value if current is not None else "unknown"
            execution_error = current.error if current is not None else "Managed run metadata disappeared"
            if execution_status == "success":
                overall_status = "error" if delivery_status == "error" else "success"
                overall_error = delivery_error if delivery_status == "error" else None
            else:
                overall_status = "error"
                overall_error = execution_error or f"Execution ended with status {execution_status}"

            applied = await self.store.mark_task_run_finished_detailed(
                task_run_id,
                status=overall_status,
                error=overall_error,
                execution_status=execution_status,
                execution_error=execution_error,
                delivery_status=delivery_status,
                delivery_error=delivery_error,
                attempt_count=attempt_count,
            )
            if not applied:
                logger.warning(
                    "Could not reconcile stale scheduler attempt %d for occurrence %s",
                    attempt_count,
                    task_run_id,
                )
        except asyncio.CancelledError:
            raise
        except Exception:
            # Crucially, do not call _reschedule_failure here.  The running row
            # and lease are safer than knowingly duplicating launched work.
            logger.exception(
                "Failed to reconcile launched scheduled run %s; occurrence %s was not requeued",
                getattr(record, "run_id", None),
                task_run_id,
            )

    def _start_delivery(self, task: ScheduledTask, run_id: str) -> asyncio.Task | None:
        delivery = task.metadata.get("delivery")
        if not isinstance(delivery, dict):
            return None
        if delivery.get("channel") != "feishu":
            raise ValueError(f"unsupported delivery channel: {delivery.get('channel')!r}")

        chat_id = delivery.get("chat_id")
        if not isinstance(chat_id, str) or not chat_id:
            raise ValueError("invalid Feishu delivery metadata")

        feishu_channel = getattr(self.manager, "feishu_channel", None)
        if feishu_channel is None:
            raise RuntimeError("Feishu channel is unavailable")

        return asyncio.create_task(
            feishu_channel.render_run_to_chat(
                run_id=run_id,
                thread_id=task.thread_id,
                chat_id=chat_id,
            ),
            name=f"scheduled-feishu-delivery-{task.id}",
        )


def set_scheduler_service(service: SchedulerService | None) -> None:
    global _scheduler_service
    _scheduler_service = service


def get_scheduler_service() -> SchedulerService:
    if _scheduler_service is None:
        raise RuntimeError("Scheduler service is not available")
    return _scheduler_service


def task_to_dict(task: ScheduledTask) -> dict[str, Any]:
    return {
        "id": task.id,
        "thread_id": task.thread_id,
        "prompt": task.prompt,
        "schedule_type": task.schedule_type,
        "schedule_expr": task.schedule_expr,
        "timezone": task.timezone,
        "enabled": task.enabled,
        "next_run_at": task.next_run_at,
        "created_by": task.created_by,
        "created_at": task.created_at,
        "updated_at": task.updated_at,
        "metadata": task.metadata,
        "kwargs": task.kwargs,
        "multitask_strategy": task.multitask_strategy,
    }
