"""Local, token-authenticated desktop operations on the live runtime."""

from __future__ import annotations

import asyncio
from dataclasses import asdict
from typing import Any


async def handle_runtime_management(
    daemon: Any, request: dict[str, Any]
) -> dict[str, Any]:
    try:
        return {"ok": True, "data": await _dispatch(daemon, request)}
    except (ValueError, RuntimeError, OSError) as exc:
        return {"ok": False, "error": str(exc), "code": "invalid_operation"}


async def _dispatch(daemon: Any, request: dict[str, Any]) -> dict[str, Any]:
    operation = request.get("operation")
    runtime = daemon.runtime
    coordinator = runtime.session_coordinator
    if operation in {"daemon.status", "daemon.drain", "daemon.resume"}:
        if operation != "daemon.status":
            coordinator.set_draining(operation == "daemon.drain")
        return {
            **coordinator.activity(),
            "config_revision": daemon.config_revision,
            "warmup": daemon.warmup_state,
            "active_runs": runtime.active_runs,
            "queued_runs": runtime.queued_runs,
            "connections": len(daemon._connections),
        }
    if operation == "session.list":
        sessions = await daemon.store.list_for_management()
        expired = await daemon.store.expired_session_ids(
            closed_retention_days=daemon.config.closed_session_retention_days,
            inactive_retention_days=daemon.config.inactive_session_retention_days,
        )
        return {
            "sessions": [
                {
                    **asdict(s),
                    "phase": coordinator.phase(s.session_id),
                    "cleanup_eligible": s.session_id in expired
                    and coordinator.owner(s.session_id) is None,
                }
                for s in sessions
            ]
        }
    if operation == "session.delete":
        session_id = str(request.get("session_id", ""))
        if not await daemon.store.get(session_id, include_closed=True):
            raise ValueError("会话不存在，请刷新列表")
        if not coordinator.reserve_cleanup(session_id):
            raise ValueError("会话仍连接客户端或正在执行，请先在客户端关闭")
        try:
            await runtime.release_session(session_id)
            purged = await runtime.purge_checkpoints([session_id])
            await daemon.store.delete_sessions(purged)
        finally:
            coordinator.release_cleanup(session_id)
        return {"deleted": purged, "note": "已删除会话和 checkpoint；产物文件保留"}
    if operation in {"memory.get", "memory.delete"}:
        from deerflow.agents.memory import get_memory_manager
        from deerflow.agents.memory.backends.deermem import memory_bucket

        session_id = str(request.get("session_id", ""))
        session = (
            await daemon.store.get(session_id, include_closed=True)
            if session_id != "__legacy__"
            else None
        )
        if not session and session_id != "__legacy__":
            raise ValueError("请选择已有会话，以确定工作区和记忆作用域")
        bucket = (
            memory_bucket(
                session.agent_name, runtime._memory_user_id(session, session.cwd)
            )
            if session
            else None
        )
        manager = get_memory_manager()
        if operation == "memory.delete":
            activity = coordinator.activity()
            if activity["active_operations"]:
                raise ValueError("请等待活动任务结束再删除记忆")
            coordinator.set_draining(True)
            try:
                if not await asyncio.to_thread(manager.shutdown_flush):
                    raise RuntimeError("记忆写入尚未完成，请稍后重试")
                await asyncio.to_thread(
                    manager.delete_fact,
                    str(request.get("fact_id", "")),
                    agent_name=bucket,
                )
            finally:
                coordinator.set_draining(activity["draining"])
        data = await asyncio.to_thread(manager.get_memory, bucket)
        return {
            "workspace": session.cwd if session else "旧版共享记忆",
            "scope": daemon.config.memory_scope if session else "global",
            "bucket": bucket,
            "memory": data,
        }
    raise ValueError(f"Unsupported management operation: {operation}")
