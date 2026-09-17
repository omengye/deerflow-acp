from __future__ import annotations

import asyncio
import json
import sqlite3
from datetime import UTC, datetime, timedelta
from types import SimpleNamespace

import pytest

import deerflow.runtime.scheduler as scheduler_module
from deerflow.runtime.runs.manager import ConflictError
from deerflow.runtime.scheduler import (
    SchedulerService,
    SchedulerStore,
    compute_next_run_at,
    set_scheduler_service,
)
from deerflow.runtime import RunStatus
from deerflow.tools.builtins.scheduled_task_tools import (
    _delivery_metadata_for_thread,
    _require_task_owned_by_current_thread,
    create_scheduled_task_tool,
    list_scheduled_task_runs_tool,
    list_scheduled_tasks_tool,
)


def test_compute_next_run_at_daily_uses_timezone():
    next_run = compute_next_run_at(
        schedule_type="daily",
        schedule_expr={"time_of_day": "09:00"},
        timezone="Asia/Shanghai",
        after=datetime(2026, 5, 17, 0, 30, tzinfo=UTC),
    )

    assert next_run == datetime(2026, 5, 17, 1, 0, tzinfo=UTC)


def test_interval_schedule_jumps_over_a_distant_legacy_anchor():
    next_run = compute_next_run_at(
        schedule_type="interval",
        schedule_expr={"every_seconds": 1, "start_at": "2000-01-01T00:00:00+00:00"},
        timezone="UTC",
        after=datetime(2026, 1, 1, tzinfo=UTC),
    )

    assert next_run == datetime(2026, 1, 1, 0, 0, 1, tzinfo=UTC)


def test_urumqi_fallback_uses_iana_utc_plus_six(monkeypatch):
    def missing_zoneinfo(_name):
        raise scheduler_module.ZoneInfoNotFoundError

    monkeypatch.setattr(scheduler_module, "ZoneInfo", missing_zoneinfo)

    tz = scheduler_module._get_tzinfo("Asia/Urumqi")

    assert datetime(2026, 1, 1, tzinfo=tz).utcoffset() == timedelta(hours=6)


def test_delivery_metadata_for_feishu_thread():
    assert _delivery_metadata_for_thread("feishu_chat-1") == {
        "delivery": {"channel": "feishu", "chat_id": "chat-1"}
    }
    assert _delivery_metadata_for_thread("thread-1") == {}


@pytest.mark.asyncio
async def test_scheduler_store_persists_tasks(tmp_path):
    db_path = tmp_path / "scheduled_tasks.db"
    store = SchedulerStore(db_path)
    store.setup()

    task = await store.create_task(
        thread_id="thread-1",
        prompt="run report",
        schedule_type="daily",
        schedule_expr={"time_of_day": "09:00"},
        timezone="Asia/Shanghai",
    )

    reloaded = SchedulerStore(db_path)
    reloaded.setup()
    tasks = await reloaded.list_tasks(thread_id="thread-1")

    assert len(tasks) == 1
    assert tasks[0].id == task.id
    assert tasks[0].prompt == "run report"
    assert tasks[0].enabled is True


@pytest.mark.asyncio
async def test_agent_created_scheduled_task_pins_current_agent(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    service = SchedulerService(store=store, manager=SimpleNamespace(), poll_interval_seconds=1)
    runtime = SimpleNamespace(
        context={"thread_id": "thread-1", "agent_name": "researcher"},
        config={},
    )
    set_scheduler_service(service)
    try:
        result = json.loads(
            await create_scheduled_task_tool.coroutine(
                runtime,
                prompt="prepare report",
                schedule_type="daily",
                time_of_day="09:00",
                timezone="UTC",
            )
        )
    finally:
        set_scheduler_service(None)

    task = await store.get_task(result["created"]["id"])
    assert task is not None
    assert task.kwargs == {"agent_name": "researcher"}


@pytest.mark.asyncio
async def test_scheduler_store_delete_removes_task_runs(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    task = await store.create_task(
        thread_id="thread-1",
        prompt="run report",
        schedule_type="daily",
        schedule_expr={"time_of_day": "09:00"},
        timezone="Asia/Shanghai",
    )
    now = datetime.now(UTC).isoformat()
    with store._connect() as conn:
        conn.execute(
            """
            INSERT INTO scheduled_task_runs (
                id, task_id, scheduled_at, status, created_at, updated_at
            ) VALUES (?, ?, ?, ?, ?, ?)
            """,
            ("run-row-1", task.id, now, "success", now, now),
        )

    assert await store.delete_task(task.id) is True
    assert await store.get_task(task.id) is None
    assert await store.list_task_runs(task.id) == []


@pytest.mark.asyncio
async def test_scheduler_run_history_filters_before_pagination(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    task = await store.create_task(
        thread_id="thread-1",
        prompt="history",
        schedule_type="daily",
        schedule_expr={"time_of_day": "09:00"},
        timezone="UTC",
    )
    now = datetime.now(UTC).isoformat()
    with store._connect() as conn:
        for run_id, status in (
            ("success-1", "success"),
            ("error-1", "error"),
            ("error-2", "error"),
            ("error-3", "error"),
        ):
            conn.execute(
                """
                INSERT INTO scheduled_task_runs (
                    id, task_id, scheduled_at, status, created_at, updated_at
                ) VALUES (?, ?, ?, ?, ?, ?)
                """,
                (run_id, task.id, now, status, now, now),
            )

    rows = await store.list_task_runs(task.id, limit=2, offset=1, status="error")
    assert [row["id"] for row in rows] == ["error-2", "error-1"]

    service = SchedulerService(store=store, manager=SimpleNamespace(), poll_interval_seconds=1)
    set_scheduler_service(service)
    try:
        runtime = SimpleNamespace(context={"thread_id": "thread-1"}, config={})
        payload = json.loads(
            await list_scheduled_task_runs_tool.coroutine(
                runtime,
                task_id=task.id,
                limit=2,
                status="error",
            )
        )
    finally:
        set_scheduler_service(None)

    assert [row["id"] for row in payload["runs"]] == ["error-3", "error-2"]
    assert payload["has_more"] is True
    assert payload["offset"] == 0


@pytest.mark.asyncio
async def test_scheduler_run_history_limit_100_detects_next_page(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    task = await store.create_task(
        thread_id="thread-1",
        prompt="history",
        schedule_type="daily",
        schedule_expr={"time_of_day": "09:00"},
        timezone="UTC",
    )
    now = datetime.now(UTC).isoformat()
    with store._connect() as conn:
        conn.executemany(
            """
            INSERT INTO scheduled_task_runs (
                id, task_id, scheduled_at, status, created_at, updated_at
            ) VALUES (?, ?, ?, 'success', ?, ?)
            """,
            [
                (f"run-{index:03d}", task.id, now, now, now)
                for index in range(101)
            ],
        )

    service = SchedulerService(store=store, manager=SimpleNamespace(), poll_interval_seconds=1)
    set_scheduler_service(service)
    try:
        payload = json.loads(
            await list_scheduled_task_runs_tool.coroutine(
                SimpleNamespace(context={"thread_id": "thread-1"}, config={}),
                task_id=task.id,
                limit=100,
            )
        )
    finally:
        set_scheduler_service(None)

    assert len(payload["runs"]) == 100
    assert payload["has_more"] is True


@pytest.mark.asyncio
async def test_scheduler_service_dispatches_due_task(tmp_path):
    db_path = tmp_path / "scheduled_tasks.db"
    store = SchedulerStore(db_path)
    store.setup()
    run_task = asyncio.create_task(asyncio.sleep(0))
    fake_record = SimpleNamespace(
        run_id="run-1",
        task=run_task,
    )
    calls = []

    class FakeRunManager:
        def get(self, run_id):
            return SimpleNamespace(status=RunStatus.success, error=None)

    class FakeManager:
        run_manager = FakeRunManager()

        async def start_client_stream_run(self, **kwargs):
            calls.append(kwargs)
            return fake_record

    await store.create_task(
        thread_id="thread-1",
        prompt="run due task",
        schedule_type="once",
        schedule_expr={"run_at": (datetime.now(UTC) + timedelta(milliseconds=10)).isoformat()},
        timezone="UTC",
        kwargs={"agent_name": "agent-a"},
    )

    await asyncio.sleep(0.02)
    live_limit = {"value": 200}
    service = SchedulerService(
        store=store,
        manager=FakeManager(),
        poll_interval_seconds=1,
        recursion_limit_resolver=lambda: live_limit["value"],
    )
    # The service was already constructed; changing the source before dispatch
    # models an Admin hot update and must affect this occurrence.
    live_limit["value"] = 350
    await service.tick()
    for _ in range(20):
        if calls:
            break
        await asyncio.sleep(0.01)

    assert calls
    assert calls[0]["thread_id"] == "thread-1"
    assert calls[0]["message"] == "run due task"
    assert calls[0]["kwargs"]["recursion_limit"] == 350
    assert calls[0]["kwargs"]["agent_name"] == "agent-a"
    task_id = (await store.list_tasks(include_disabled=True))[0].id
    runs = []
    for _ in range(20):
        runs = await store.list_task_runs(task_id)
        if runs and runs[0]["run_id"] == "run-1":
            break
        await asyncio.sleep(0.01)
    assert runs[0]["run_id"] == "run-1"


@pytest.mark.asyncio
async def test_scheduler_service_starts_feishu_delivery(tmp_path):
    db_path = tmp_path / "scheduled_tasks.db"
    store = SchedulerStore(db_path)
    store.setup()
    delivery_calls = []

    class FakeFeishuChannel:
        async def render_run_to_chat(self, **kwargs):
            delivery_calls.append(kwargs)

    class FakeManager:
        feishu_channel = FakeFeishuChannel()

    task = await store.create_task(
        thread_id="feishu_chat-1",
        prompt="run due task",
        schedule_type="once",
        schedule_expr={"run_at": (datetime.now(UTC) + timedelta(seconds=1)).isoformat()},
        timezone="UTC",
        metadata={"delivery": {"channel": "feishu", "chat_id": "chat-1"}},
    )

    service = SchedulerService(store=store, manager=FakeManager(), poll_interval_seconds=1)
    delivery_task = service._start_delivery(task, "run-1")
    assert delivery_task is not None
    await delivery_task

    assert delivery_calls == [
        {
            "run_id": "run-1",
            "thread_id": "feishu_chat-1",
            "chat_id": "chat-1",
        }
    ]


@pytest.mark.asyncio
async def test_stale_claim_is_recovered_after_lease_expiry(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    task = await store.create_task(
        thread_id="thread-1",
        prompt="recover me",
        schedule_type="once",
        schedule_expr={"run_at": (datetime.now(UTC) + timedelta(milliseconds=10)).isoformat()},
        timezone="UTC",
    )
    await asyncio.sleep(0.02)
    first = await store.claim_due_tasks(
        now=datetime.now(UTC),
        lease_seconds=1,
        max_attempts=3,
    )

    recovered_store = SchedulerStore(store.db_path)
    recovered_store.setup()
    second = await recovered_store.claim_due_tasks(
        now=datetime.now(UTC) + timedelta(seconds=2),
        lease_seconds=1,
        max_attempts=3,
    )

    assert first[0][1] == second[0][1]
    assert first[0][0].id == task.id
    run = await recovered_store.get_task_run(first[0][1])
    assert run is not None
    assert run["status"] == "claimed"
    assert run["attempt_count"] == 2


@pytest.mark.asyncio
async def test_claim_serializes_same_thread_across_store_instances(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    first_task = await store.create_task(
        thread_id="shared-thread",
        prompt="first",
        schedule_type="once",
        schedule_expr={"run_at": (datetime.now(UTC) + timedelta(hours=1)).isoformat()},
        timezone="UTC",
    )
    second_task = await store.create_task(
        thread_id="shared-thread",
        prompt="second",
        schedule_type="once",
        schedule_expr={"run_at": (datetime.now(UTC) + timedelta(hours=1)).isoformat()},
        timezone="UTC",
    )
    now = datetime.now(UTC)
    with store._connect() as conn:
        conn.execute(
            "UPDATE scheduled_tasks SET next_run_at = ? WHERE id = ?",
            ((now - timedelta(seconds=2)).isoformat(), first_task.id),
        )
        conn.execute(
            "UPDATE scheduled_tasks SET next_run_at = ? WHERE id = ?",
            ((now - timedelta(seconds=1)).isoformat(), second_task.id),
        )

    first_claim = await store.claim_due_tasks(now=now, limit=1, lease_seconds=60)
    other_process_store = SchedulerStore(store.db_path)
    other_process_store.setup()
    second_claim = await other_process_store.claim_due_tasks(now=now, limit=10, lease_seconds=60)

    assert len(first_claim) == 1
    assert first_claim[0][0].id == first_task.id
    assert second_claim == []


@pytest.mark.asyncio
async def test_expired_attempt_cannot_overwrite_reclaimed_occurrence(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    task = await store.create_task(
        thread_id="thread-1",
        prompt="fence me",
        schedule_type="once",
        schedule_expr={"run_at": (datetime.now(UTC) + timedelta(hours=1)).isoformat()},
        timezone="UTC",
    )
    now = datetime.now(UTC)
    with store._connect() as conn:
        conn.execute(
            "UPDATE scheduled_tasks SET next_run_at = ? WHERE id = ?",
            ((now - timedelta(seconds=1)).isoformat(), task.id),
        )

    first = (await store.claim_due_tasks(now=now, lease_seconds=1))[0]
    second = (await store.claim_due_tasks(now=now + timedelta(seconds=2), lease_seconds=60))[0]
    task_run_id = first[1]

    old_finish_applied = await store.mark_task_run_finished_detailed(
        task_run_id,
        status="error",
        error="late result",
        execution_status="error",
        execution_error="late result",
        delivery_status="not_requested",
        delivery_error=None,
        attempt_count=first[3],
    )
    old_renewed = await store.renew_task_run_lease(
        task_run_id,
        attempt_count=first[3],
        lease_seconds=60,
    )

    run = await store.get_task_run(task_run_id)
    assert first[3] == 1 and second[3] == 2
    assert old_finish_applied is False
    assert old_renewed is False
    assert run is not None
    assert run["status"] == "claimed"
    assert run["attempt_count"] == 2


@pytest.mark.asyncio
async def test_dispatch_conflict_retries_one_time_occurrence(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    task = await store.create_task(
        thread_id="thread-1",
        prompt="retry me",
        schedule_type="once",
        schedule_expr={"run_at": (datetime.now(UTC) + timedelta(milliseconds=10)).isoformat()},
        timezone="UTC",
    )
    await asyncio.sleep(0.02)
    claimed_task, task_run_id, scheduled_at, attempt_count = (
        await store.claim_due_tasks(now=datetime.now(UTC), max_attempts=3)
    )[0]

    class FakeManager:
        async def start_client_stream_run(self, **_kwargs):
            raise ConflictError("thread is busy")

    service = SchedulerService(
        store=store,
        manager=FakeManager(),
        poll_interval_seconds=1,
        retry_base_seconds=0,
        max_attempts=3,
    )
    await service._dispatch(claimed_task, task_run_id, scheduled_at, attempt_count)

    persisted = await store.get_task(task.id)
    run = await store.get_task_run(task_run_id)
    assert persisted is not None and persisted.enabled is False
    assert run is not None and run["status"] == "retry"
    reclaimed = await store.claim_due_tasks(
        now=datetime.now(UTC) + timedelta(seconds=1),
        max_attempts=3,
    )
    assert reclaimed[0][1] == task_run_id


@pytest.mark.asyncio
async def test_transient_llm_failure_retries_occurrence(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    task = await store.create_task(
        thread_id="thread-1",
        prompt="retry provider failure",
        schedule_type="once",
        schedule_expr={"run_at": (datetime.now(UTC) + timedelta(hours=1)).isoformat()},
        timezone="UTC",
    )
    with store._connect() as conn:
        conn.execute(
            "UPDATE scheduled_tasks SET next_run_at = ? WHERE id = ?",
            ((datetime.now(UTC) - timedelta(seconds=1)).isoformat(), task.id),
        )
    claimed_task, task_run_id, scheduled_at, attempt_count = (
        await store.claim_due_tasks(now=datetime.now(UTC), max_attempts=3)
    )[0]

    class FakeRunManager:
        def get(self, _run_id):
            return SimpleNamespace(
                status=RunStatus.error,
                error="upstream request failed",
                metadata={"llm_failure_retriable": True},
            )

    class FakeManager:
        run_manager = FakeRunManager()

        async def start_client_stream_run(self, **kwargs):
            return SimpleNamespace(
                run_id=kwargs["run_id"],
                task=asyncio.create_task(asyncio.sleep(0)),
            )

    service = SchedulerService(
        store=store,
        manager=FakeManager(),
        poll_interval_seconds=1,
        retry_base_seconds=0,
        max_attempts=3,
    )
    await service._dispatch(claimed_task, task_run_id, scheduled_at, attempt_count)

    run = await store.get_task_run(task_run_id)
    assert run is not None
    assert run["status"] == "retry"
    assert run["execution_status"] == "error"
    assert run["execution_error"] == "upstream request failed"


@pytest.mark.asyncio
async def test_post_launch_bookkeeping_failure_does_not_retry_occurrence(tmp_path, monkeypatch):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    task = await store.create_task(
        thread_id="thread-1",
        prompt="launch once",
        schedule_type="once",
        schedule_expr={"run_at": (datetime.now(UTC) + timedelta(hours=1)).isoformat()},
        timezone="UTC",
    )
    with store._connect() as conn:
        conn.execute(
            "UPDATE scheduled_tasks SET next_run_at = ? WHERE id = ?",
            ((datetime.now(UTC) - timedelta(seconds=1)).isoformat(), task.id),
        )
    claimed_task, task_run_id, scheduled_at, attempt_count = (
        await store.claim_due_tasks(now=datetime.now(UTC), max_attempts=3)
    )[0]

    actual_run_id = "manager-selected-run"

    class FakeRunManager:
        def get(self, run_id):
            assert run_id == actual_run_id
            return SimpleNamespace(status=RunStatus.success, error=None, metadata={})

    launches = []

    class FakeManager:
        run_manager = FakeRunManager()

        async def start_client_stream_run(self, **kwargs):
            launches.append(kwargs)
            return SimpleNamespace(
                run_id=actual_run_id,
                task=asyncio.create_task(asyncio.sleep(0)),
            )

    original_mark_launched = store.mark_task_run_launched
    launch_fence_calls = 0

    async def fail_first_launch_fence(*args, **kwargs):
        nonlocal launch_fence_calls
        launch_fence_calls += 1
        if launch_fence_calls == 1:
            raise RuntimeError("temporary bookkeeping failure")
        return await original_mark_launched(*args, **kwargs)

    monkeypatch.setattr(store, "mark_task_run_launched", fail_first_launch_fence)
    service = SchedulerService(store=store, manager=FakeManager(), poll_interval_seconds=1)
    retries = []

    async def record_retry(*args, **kwargs):
        retries.append((args, kwargs))
        return True

    monkeypatch.setattr(service, "_reschedule_failure", record_retry)

    await service._dispatch(claimed_task, task_run_id, scheduled_at, attempt_count)

    run = await store.get_task_run(task_run_id)
    assert len(launches) == 1
    assert retries == []
    assert run is not None
    assert run["status"] == "success"
    assert run["run_id"] == actual_run_id
    assert run["execution_status"] == "success"


@pytest.mark.asyncio
async def test_expired_launched_occurrence_is_not_automatically_reclaimed(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    task = await store.create_task(
        thread_id="thread-1",
        prompt="do not duplicate",
        schedule_type="once",
        schedule_expr={"run_at": (datetime.now(UTC) + timedelta(hours=1)).isoformat()},
        timezone="UTC",
    )
    now = datetime.now(UTC)
    with store._connect() as conn:
        conn.execute(
            "UPDATE scheduled_tasks SET next_run_at = ? WHERE id = ?",
            ((now - timedelta(seconds=1)).isoformat(), task.id),
        )
    claimed_task, task_run_id, _, attempt_count = (
        await store.claim_due_tasks(now=now, lease_seconds=1)
    )[0]
    assert claimed_task.id == task.id
    assert await store.mark_task_run_started(
        task_run_id,
        "managed-run",
        attempt_count=attempt_count,
    )
    assert await store.mark_task_run_launched(
        task_run_id,
        "managed-run",
        attempt_count=attempt_count,
    )

    reclaimed = await store.claim_due_tasks(
        now=now + timedelta(seconds=2),
        lease_seconds=60,
    )

    run = await store.get_task_run(task_run_id)
    assert reclaimed == []
    assert run is not None and run["status"] == "launched"


@pytest.mark.asyncio
async def test_expired_launched_occurrence_keeps_same_thread_serialized(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    first_task = await store.create_task(
        thread_id="shared-thread",
        prompt="possibly still running",
        schedule_type="once",
        schedule_expr={"run_at": (datetime.now(UTC) + timedelta(hours=1)).isoformat()},
        timezone="UTC",
    )
    second_task = await store.create_task(
        thread_id="shared-thread",
        prompt="must wait for reconciliation",
        schedule_type="once",
        schedule_expr={"run_at": (datetime.now(UTC) + timedelta(hours=1)).isoformat()},
        timezone="UTC",
    )
    now = datetime.now(UTC)
    with store._connect() as conn:
        conn.execute(
            "UPDATE scheduled_tasks SET next_run_at = ? WHERE id = ?",
            ((now - timedelta(seconds=2)).isoformat(), first_task.id),
        )
        conn.execute(
            "UPDATE scheduled_tasks SET next_run_at = ? WHERE id = ?",
            ((now - timedelta(seconds=1)).isoformat(), second_task.id),
        )

    first = (await store.claim_due_tasks(now=now, limit=1, lease_seconds=1))[0]
    assert first[0].id == first_task.id
    assert await store.mark_task_run_started(
        first[1],
        "managed-run",
        attempt_count=first[3],
    )
    assert await store.mark_task_run_launched(
        first[1],
        "managed-run",
        attempt_count=first[3],
    )

    later = await store.claim_due_tasks(
        now=now + timedelta(seconds=2),
        limit=10,
        lease_seconds=60,
    )

    assert later == []
    second_runs = await store.list_task_runs(second_task.id)
    assert len(second_runs) == 1
    assert second_runs[0]["status"] == "pending"


@pytest.mark.asyncio
async def test_pausing_task_cancels_retry_and_prevents_reclaim(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    now = datetime.now(UTC)
    task = await store.create_task(
        thread_id="thread-1",
        prompt="do not retry while paused",
        schedule_type="interval",
        schedule_expr={
            "every_seconds": 60,
            "start_at": (now + timedelta(seconds=1)).isoformat(),
        },
        timezone="UTC",
    )
    claim_at = datetime.fromisoformat(task.next_run_at) + timedelta(milliseconds=1)
    claimed = (await store.claim_due_tasks(now=claim_at, lease_seconds=60))[0]
    await store.reschedule_task_run(
        claimed[1],
        error="retry",
        delay_seconds=0,
        max_attempts=3,
        attempt_count=claimed[3],
    )

    paused, active = await store.pause_task(task.id)
    reclaimed = await store.claim_due_tasks(
        now=datetime.now(UTC) + timedelta(seconds=1),
        lease_seconds=60,
    )

    assert paused is not None and paused.enabled is False
    assert [row["status"] for row in active] == ["retry"]
    assert reclaimed == []
    runs = await store.list_task_runs(task.id)
    assert runs[0]["status"] == "cancelled"


@pytest.mark.asyncio
async def test_service_pause_cancels_managed_run_and_dispatch(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    task = await store.create_task(
        thread_id="thread-1",
        prompt="cancel me",
        schedule_type="daily",
        schedule_expr={"time_of_day": "09:00"},
        timezone="UTC",
    )
    now = datetime.now(UTC).isoformat()
    with store._connect() as conn:
        conn.execute(
            """
            INSERT INTO scheduled_task_runs (
                id, task_id, scheduled_at, run_id, status, attempt_count,
                created_at, updated_at
            ) VALUES (?, ?, ?, ?, 'launched', 1, ?, ?)
            """,
            ("occurrence-1", task.id, now, "managed-1", now, now),
        )

    cancelled_runs: list[str] = []

    class Manager:
        async def cancel_run(self, run_id, **_kwargs):
            cancelled_runs.append(run_id)
            return True

    service = SchedulerService(store=store, manager=Manager(), poll_interval_seconds=1)
    blocker = asyncio.create_task(asyncio.sleep(60))
    service._dispatch_tasks.add(blocker)
    service._dispatch_tasks_by_task_id[task.id] = {blocker}

    paused = await service.set_task_enabled(task.id, False)

    assert paused is not None and paused.enabled is False
    assert cancelled_runs == ["managed-1"]
    assert blocker.cancelled()


@pytest.mark.asyncio
async def test_orphaned_launched_occurrence_is_terminalized_without_replay(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    now = datetime.now(UTC)
    task = await store.create_task(
        thread_id="shared-thread",
        prompt="already launched",
        schedule_type="once",
        schedule_expr={"run_at": (now + timedelta(hours=1)).isoformat()},
        timezone="UTC",
    )
    old = (now - timedelta(hours=2)).isoformat()
    with store._connect() as conn:
        conn.execute(
            """
            INSERT INTO scheduled_task_runs (
                id, task_id, scheduled_at, run_id, status, attempt_count,
                lease_expires_at, created_at, updated_at
            ) VALUES (?, ?, ?, ?, 'launched', 1, ?, ?, ?)
            """,
            ("orphan-1", task.id, old, "managed-old", old, old, old),
        )

    reconciled = await store.reconcile_expired_launched_runs(
        now=now,
        orphan_grace_seconds=3600,
    )

    run = await store.get_task_run("orphan-1")
    assert reconciled == 1
    assert run is not None and run["status"] == "interrupted"
    assert "not retried" in run["error"]


@pytest.mark.asyncio
async def test_delete_tasks_for_thread_removes_enabled_disabled_and_history(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    first = await store.create_task(
        thread_id="delete-thread",
        prompt="first",
        schedule_type="daily",
        schedule_expr={"time_of_day": "09:00"},
        timezone="UTC",
    )
    await store.create_task(
        thread_id="delete-thread",
        prompt="second",
        schedule_type="daily",
        schedule_expr={"time_of_day": "10:00"},
        timezone="UTC",
    )
    await store.create_task(
        thread_id="keep-thread",
        prompt="keep",
        schedule_type="daily",
        schedule_expr={"time_of_day": "11:00"},
        timezone="UTC",
    )
    await store.set_enabled(first.id, False)

    deleted = await store.delete_tasks_for_thread("delete-thread")

    assert deleted == 2
    assert await store.list_tasks(thread_id="delete-thread", include_disabled=True) == []
    assert len(await store.list_tasks(thread_id="keep-thread", include_disabled=True)) == 1


@pytest.mark.asyncio
async def test_invalid_due_task_is_quarantined_without_blocking_batch(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    due = (datetime.now(UTC) - timedelta(seconds=1)).isoformat()
    bad = await store.create_task(
        thread_id="bad-thread",
        prompt="bad",
        schedule_type="daily",
        schedule_expr={"time_of_day": "09:00"},
        timezone="UTC",
    )
    good = await store.create_task(
        thread_id="good-thread",
        prompt="good",
        schedule_type="daily",
        schedule_expr={"time_of_day": "09:00"},
        timezone="UTC",
    )
    with store._connect() as conn:
        conn.execute(
            "UPDATE scheduled_tasks SET schedule_type = 'interval', schedule_expr = '{}', next_run_at = ? WHERE id = ?",
            (due, bad.id),
        )
        conn.execute(
            "UPDATE scheduled_tasks SET next_run_at = ? WHERE id = ?",
            (due, good.id),
        )

    claimed = await store.claim_due_tasks(now=datetime.now(UTC), max_attempts=3)

    assert [item[0].id for item in claimed] == [good.id]
    bad_task = await store.get_task(bad.id)
    bad_runs = await store.list_task_runs(bad.id)
    assert bad_task is not None and bad_task.enabled is False
    assert bad_runs[0]["status"] == "dead_letter"


@pytest.mark.asyncio
async def test_legacy_interval_schedule_does_not_drift_from_poll_delay(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    task = await store.create_task(
        thread_id="thread-1",
        prompt="interval",
        schedule_type="interval",
        schedule_expr={"every_seconds": 60},
        timezone="UTC",
    )
    scheduled_at = datetime(2026, 1, 1, 0, 0, tzinfo=UTC)
    with store._connect() as conn:
        conn.execute(
            "UPDATE scheduled_tasks SET schedule_expr = ?, next_run_at = ? WHERE id = ?",
            ('{"every_seconds": 60}', scheduled_at.isoformat(), task.id),
        )

    await store.claim_due_tasks(
        now=scheduled_at + timedelta(seconds=5),
        max_attempts=3,
    )

    updated = await store.get_task(task.id)
    assert updated is not None
    assert updated.next_run_at == (scheduled_at + timedelta(seconds=60)).isoformat()
    assert updated.schedule_expr["start_at"] == scheduled_at.isoformat()


@pytest.mark.asyncio
async def test_missing_delivery_channel_is_recorded_separately(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    await store.create_task(
        thread_id="feishu_chat-1",
        prompt="deliver",
        schedule_type="once",
        schedule_expr={"run_at": (datetime.now(UTC) + timedelta(milliseconds=10)).isoformat()},
        timezone="UTC",
        metadata={"delivery": {"channel": "feishu", "chat_id": "chat-1"}},
    )
    await asyncio.sleep(0.02)
    claimed_task, task_run_id, scheduled_at, attempt_count = (
        await store.claim_due_tasks(now=datetime.now(UTC), max_attempts=3)
    )[0]

    class FakeRunManager:
        def get(self, _run_id):
            return SimpleNamespace(status=RunStatus.success, error=None, metadata={})

    class FakeManager:
        run_manager = FakeRunManager()
        feishu_channel = None

        async def start_client_stream_run(self, **kwargs):
            return SimpleNamespace(
                run_id=kwargs["run_id"],
                task=asyncio.create_task(asyncio.sleep(0)),
            )

    service = SchedulerService(store=store, manager=FakeManager(), poll_interval_seconds=1)
    await service._dispatch(claimed_task, task_run_id, scheduled_at, attempt_count)

    run = await store.get_task_run(task_run_id)
    assert run is not None
    assert run["status"] == "error"
    assert run["execution_status"] == "success"
    assert run["delivery_status"] == "error"
    assert "unavailable" in run["delivery_error"]


@pytest.mark.asyncio
async def test_execution_and_delivery_errors_are_both_preserved(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    task = await store.create_task(
        thread_id="feishu_chat-1",
        prompt="fail both",
        schedule_type="once",
        schedule_expr={"run_at": (datetime.now(UTC) + timedelta(hours=1)).isoformat()},
        timezone="UTC",
        metadata={"delivery": {"channel": "feishu", "chat_id": "chat-1"}},
    )
    with store._connect() as conn:
        conn.execute(
            "UPDATE scheduled_tasks SET next_run_at = ? WHERE id = ?",
            ((datetime.now(UTC) - timedelta(seconds=1)).isoformat(), task.id),
        )
    claimed_task, task_run_id, scheduled_at, attempt_count = (
        await store.claim_due_tasks(now=datetime.now(UTC), max_attempts=3)
    )[0]

    class FakeRunManager:
        def get(self, _run_id):
            return SimpleNamespace(
                status=RunStatus.error,
                error="provider failed",
                metadata={},
            )

    class FakeManager:
        run_manager = FakeRunManager()

        async def start_client_stream_run(self, **kwargs):
            return SimpleNamespace(
                run_id=kwargs["run_id"],
                task=asyncio.create_task(asyncio.sleep(0)),
            )

    async def fail_delivery():
        raise RuntimeError("chat transport failed")

    service = SchedulerService(store=store, manager=FakeManager(), poll_interval_seconds=1)
    service._start_delivery = lambda _task, _run_id: asyncio.create_task(fail_delivery())  # type: ignore[method-assign]
    await service._dispatch(claimed_task, task_run_id, scheduled_at, attempt_count)

    run = await store.get_task_run(task_run_id)
    assert run is not None
    assert run["status"] == "error"
    assert run["execution_status"] == "error"
    assert run["execution_error"] == "provider failed"
    assert run["delivery_status"] == "error"
    assert "chat transport failed" in run["delivery_error"]


@pytest.mark.asyncio
async def test_agent_task_management_is_scoped_to_current_thread(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    other = await store.create_task(
        thread_id="thread-b",
        prompt="private",
        schedule_type="daily",
        schedule_expr={"time_of_day": "09:00"},
        timezone="UTC",
    )
    service = SchedulerService(store=store, manager=SimpleNamespace(), poll_interval_seconds=1)
    runtime = SimpleNamespace(context={"thread_id": "thread-a"}, config={})
    set_scheduler_service(service)
    try:
        with pytest.raises(ValueError, match="not found"):
            await _require_task_owned_by_current_thread(runtime, other.id)
    finally:
        set_scheduler_service(None)

    assert "thread_id" not in create_scheduled_task_tool.args_schema.model_fields
    assert "all_threads" not in list_scheduled_tasks_tool.args_schema.model_fields


def test_scheduler_store_migrates_existing_run_table(tmp_path):
    db_path = tmp_path / "scheduled_tasks.db"
    conn = sqlite3.connect(db_path)
    conn.executescript(
        """
        CREATE TABLE scheduled_tasks (
            id TEXT PRIMARY KEY, thread_id TEXT NOT NULL, prompt TEXT NOT NULL,
            schedule_type TEXT NOT NULL, schedule_expr TEXT NOT NULL,
            timezone TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1,
            next_run_at TEXT, created_by TEXT NOT NULL, created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL, metadata TEXT NOT NULL DEFAULT '{}',
            kwargs TEXT NOT NULL DEFAULT '{}', multitask_strategy TEXT NOT NULL DEFAULT 'reject'
        );
        CREATE TABLE scheduled_task_runs (
            id TEXT PRIMARY KEY, task_id TEXT NOT NULL, scheduled_at TEXT NOT NULL,
            started_at TEXT, finished_at TEXT, run_id TEXT, status TEXT NOT NULL,
            error TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
        );
        """
    )
    conn.close()

    SchedulerStore(db_path).setup()

    conn = sqlite3.connect(db_path)
    columns = {row[1] for row in conn.execute("PRAGMA table_info(scheduled_task_runs)")}
    conn.close()
    assert {
        "attempt_count",
        "available_at",
        "lease_expires_at",
        "execution_status",
        "execution_error",
        "delivery_status",
        "delivery_error",
    } <= columns


@pytest.mark.asyncio
async def test_scheduler_stop_cancels_and_requeues_tracked_dispatch(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    task = await store.create_task(
        thread_id="thread-1",
        prompt="long run",
        schedule_type="once",
        schedule_expr={"run_at": (datetime.now(UTC) + timedelta(milliseconds=10)).isoformat()},
        timezone="UTC",
    )
    await asyncio.sleep(0.02)
    started = asyncio.Event()

    class BlockingManager:
        async def start_client_stream_run(self, **_kwargs):
            started.set()
            await asyncio.Event().wait()

    service = SchedulerService(
        store=store,
        manager=BlockingManager(),
        poll_interval_seconds=60,
        retry_base_seconds=0,
        shutdown_grace_seconds=0,
    )
    await service.start()
    await asyncio.wait_for(started.wait(), timeout=2)

    await service.stop()

    assert service._dispatch_tasks == set()
    runs = await store.list_task_runs(task.id)
    assert runs[0]["status"] == "retry"
    assert runs[0]["lease_expires_at"] is None


@pytest.mark.asyncio
async def test_scheduler_run_history_cleanup_preserves_live_rows(tmp_path):
    store = SchedulerStore(tmp_path / "scheduled_tasks.db")
    store.setup()
    task = await store.create_task(
        thread_id="thread-1",
        prompt="history",
        schedule_type="daily",
        schedule_expr={"time_of_day": "09:00"},
        timezone="UTC",
    )
    old = (datetime.now(UTC) - timedelta(days=60)).isoformat()
    recent = datetime.now(UTC).isoformat()
    with store._connect() as conn:
        for run_id, status, created_at, finished_at in (
            ("old", "success", old, old),
            ("recent", "success", recent, recent),
            ("retry", "retry", old, None),
        ):
            conn.execute(
                """
                INSERT INTO scheduled_task_runs (
                    id, task_id, scheduled_at, status, created_at, updated_at, finished_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                (run_id, task.id, created_at, status, created_at, created_at, finished_at),
            )

    deleted = await store.cleanup_task_runs(
        older_than=datetime.now(UTC) - timedelta(days=30),
        max_per_task=100,
    )
    rows = {row["id"]: row for row in await store.list_task_runs(task.id, limit=20)}

    assert deleted == 1
    assert set(rows) == {"recent", "retry"}
