"""Built-in tools for agent-managed scheduled tasks."""

from __future__ import annotations

import json
from typing import Any, Literal

from langchain.tools import ToolRuntime, tool
from langgraph.config import get_config

from deerflow.agents.thread_state import AgentContext, ThreadState
from deerflow.runtime.scheduler import get_scheduler_service, task_to_dict

ScheduleType = Literal["once", "interval", "daily"]
ScheduledTaskRunStatus = Literal[
    "pending",
    "claimed",
    "running",
    "launched",
    "retry",
    "success",
    "error",
    "timeout",
    "interrupted",
    "cancelled",
    "dead_letter",
]


def _get_thread_id(runtime: ToolRuntime[AgentContext, ThreadState] | None) -> str | None:
    if runtime is not None:
        if runtime.context and runtime.context.get("thread_id"):
            return runtime.context.get("thread_id")
        thread_id = runtime.config.get("configurable", {}).get("thread_id")
        if thread_id:
            return thread_id
    try:
        return get_config().get("configurable", {}).get("thread_id")
    except Exception:
        return None


def _get_agent_name(runtime: ToolRuntime[AgentContext, ThreadState] | None) -> str | None:
    """Resolve the custom agent bound to the current run, if any."""
    if runtime is not None:
        if runtime.context and runtime.context.get("agent_name"):
            return str(runtime.context["agent_name"])
        agent_name = runtime.config.get("configurable", {}).get("agent_name")
        if agent_name:
            return str(agent_name)
    try:
        agent_name = get_config().get("configurable", {}).get("agent_name")
        return str(agent_name) if agent_name else None
    except Exception:
        return None


def _json(data: Any) -> str:
    return json.dumps(data, ensure_ascii=False, indent=2)


def _delivery_metadata_for_thread(thread_id: str) -> dict[str, Any]:
    if thread_id.startswith("feishu_"):
        chat_id = thread_id.removeprefix("feishu_")
        if chat_id:
            return {"delivery": {"channel": "feishu", "chat_id": chat_id}}
    return {}


@tool("create_scheduled_task", parse_docstring=True)
async def create_scheduled_task_tool(
    runtime: ToolRuntime[AgentContext, ThreadState],
    prompt: str,
    schedule_type: ScheduleType,
    run_at: str | None = None,
    every_seconds: int | None = None,
    time_of_day: str | None = None,
    timezone: str | None = None,
    multitask_strategy: Literal["reject", "interrupt", "rollback"] = "reject",
) -> str:
    """Create a persistent scheduled task for this DeerFlow service.

    Use this only after the user has confirmed the exact schedule and task
    content. The task survives API service restarts because it is stored in
    SQLite.

    Args:
        prompt: The user-facing instruction to run when the task fires.
        schedule_type: once, interval, or daily.
        run_at: ISO-8601 datetime for once schedules, e.g. 2026-05-18T09:00:00+08:00.
        every_seconds: Interval length in seconds for interval schedules.
        time_of_day: Local time for daily schedules, HH:MM or HH:MM:SS.
        timezone: IANA timezone, e.g. Asia/Shanghai. Defaults to service timezone.
        multitask_strategy: What to do if the target thread already has a run.
    """
    target_thread_id = _get_thread_id(runtime)
    if not target_thread_id:
        raise ValueError("thread_id is required when no current conversation thread is available")

    schedule_expr: dict[str, Any]
    if schedule_type == "once":
        if not run_at:
            raise ValueError("run_at is required for once schedules")
        schedule_expr = {"run_at": run_at}
    elif schedule_type == "interval":
        if not every_seconds:
            raise ValueError("every_seconds is required for interval schedules")
        schedule_expr = {"every_seconds": every_seconds}
        if run_at:
            schedule_expr["start_at"] = run_at
    elif schedule_type == "daily":
        if not time_of_day:
            raise ValueError("time_of_day is required for daily schedules")
        schedule_expr = {"time_of_day": time_of_day}
    else:
        raise ValueError(f"Unsupported schedule_type: {schedule_type}")

    service = get_scheduler_service()
    agent_name = _get_agent_name(runtime)
    task = await service.store.create_task(
        thread_id=target_thread_id,
        prompt=prompt,
        schedule_type=schedule_type,
        schedule_expr=schedule_expr,
        timezone=timezone or service.default_timezone,
        created_by="agent",
        metadata=_delivery_metadata_for_thread(target_thread_id),
        kwargs={"agent_name": agent_name} if agent_name else None,
        multitask_strategy=multitask_strategy,
    )
    return _json({"created": task_to_dict(task)})


@tool("list_scheduled_tasks", parse_docstring=True)
async def list_scheduled_tasks_tool(
    runtime: ToolRuntime[AgentContext, ThreadState],
    include_disabled: bool = False,
    limit: int = 50,
) -> str:
    """List persistent scheduled tasks.

    Args:
        include_disabled: Include disabled or completed one-time tasks.
        limit: Maximum number of tasks to return.
    """
    thread_id = _get_thread_id(runtime)
    if not thread_id:
        raise ValueError("A current thread is required to list scheduled tasks")
    tasks = await get_scheduler_service().store.list_tasks(
        thread_id=thread_id,
        include_disabled=include_disabled,
        limit=limit,
    )
    return _json({"tasks": [task_to_dict(task) for task in tasks]})


@tool("set_scheduled_task_enabled", parse_docstring=True)
async def set_scheduled_task_enabled_tool(
    runtime: ToolRuntime[AgentContext, ThreadState],
    task_id: str,
    enabled: bool,
) -> str:
    """Enable or disable a persistent scheduled task.

    Args:
        task_id: The scheduled task ID.
        enabled: True to enable the task, false to pause it.
    """
    service = get_scheduler_service()
    await _require_task_owned_by_current_thread(runtime, task_id)
    task = await service.set_task_enabled(task_id, enabled)
    if task is None:
        raise ValueError(f"Scheduled task not found: {task_id}")
    return _json({"updated": task_to_dict(task)})


@tool("delete_scheduled_task", parse_docstring=True)
async def delete_scheduled_task_tool(
    runtime: ToolRuntime[AgentContext, ThreadState],
    task_id: str,
) -> str:
    """Delete a persistent scheduled task.

    Args:
        task_id: The scheduled task ID to delete.
    """
    service = get_scheduler_service()
    await _require_task_owned_by_current_thread(runtime, task_id)
    deleted = await service.delete_task(task_id)
    if not deleted:
        raise ValueError(f"Scheduled task not found: {task_id}")
    return _json({"deleted": task_id})


@tool("list_scheduled_task_runs", parse_docstring=True)
async def list_scheduled_task_runs_tool(
    runtime: ToolRuntime[AgentContext, ThreadState],
    task_id: str,
    limit: int = 20,
    offset: int = 0,
    status: ScheduledTaskRunStatus | None = None,
) -> str:
    """List execution history for a scheduled task.

    Args:
        task_id: The scheduled task ID.
        limit: Maximum number of execution records to return.
        offset: Number of matching execution records to skip.
        status: Optional occurrence status to filter before pagination.
    """
    await _require_task_owned_by_current_thread(runtime, task_id)
    limit = max(1, min(limit, 100))
    offset = max(0, offset)
    rows = await get_scheduler_service().store.list_task_runs(
        task_id,
        limit=limit + 1,
        offset=offset,
        status=status,
    )
    has_more = len(rows) > limit
    return _json(
        {
            "task_id": task_id,
            "runs": rows[:limit],
            "offset": offset,
            "limit": limit,
            "has_more": has_more,
            "status": status,
        }
    )


async def _require_task_owned_by_current_thread(
    runtime: ToolRuntime[AgentContext, ThreadState] | None,
    task_id: str,
):
    thread_id = _get_thread_id(runtime)
    if not thread_id:
        raise ValueError("A current thread is required to manage scheduled tasks")
    task = await get_scheduler_service().store.get_task(task_id)
    if task is None or task.thread_id != thread_id:
        # Do not reveal whether a cross-thread task ID exists.
        raise ValueError(f"Scheduled task not found: {task_id}")
    return task


scheduled_task_tools = [
    create_scheduled_task_tool,
    list_scheduled_tasks_tool,
    set_scheduled_task_enabled_tool,
    delete_scheduled_task_tool,
    list_scheduled_task_runs_tool,
]
