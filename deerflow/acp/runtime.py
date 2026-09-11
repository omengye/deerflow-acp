"""Embedded DeerFlow runtime used by the local ACP adapter."""

from __future__ import annotations

import asyncio
import hashlib
import inspect
import logging
from collections.abc import AsyncGenerator, Awaitable, Callable
from contextlib import AbstractAsyncContextManager, asynccontextmanager
from dataclasses import dataclass
from typing import Any

from langchain_core.messages import HumanMessage
from langgraph.checkpoint.sqlite.aio import AsyncSqliteSaver

from deerflow.agents.goal_state import GoalEvaluation, GoalState
from deerflow.client import DeerFlowClient, StreamEvent
from deerflow.models import aclose_chat_model
from deerflow.runtime.goal import (
    GoalCheckpointSnapshot,
    GoalWriteConflict,
    attach_goal_evaluation,
    build_goal_state,
    compute_no_progress_count,
    create_goal_evaluator_model,
    evaluate_goal_completion,
    goal_instance_matches,
    goal_stand_down_reason,
    goal_thread_lock,
    latest_visible_assistant_signature,
    make_goal_continuation_message,
    read_goal_snapshot,
    read_thread_goal,
    should_continue_goal,
    visible_conversation_signature,
    write_thread_goal,
)

from .client_mcp import ClientMCPBinding
from .config import LocalACPConfig
from .permission import ACPPermissionBroker, ACPPermissionMiddleware, PermissionHandler
from .policy import LocalACPCapabilityPolicy
from .session_coordinator import ACPSessionCoordinator
from .session_store import LocalACPSession

LiveEventCallback = Callable[[dict[str, Any]], Awaitable[None]]

logger = logging.getLogger(__name__)


@dataclass(slots=True)
class _GoalTurnResult:
    continuation: HumanMessage | None
    status_event: StreamEvent | None


class LocalACPRuntime:
    """Owns the ACP-only checkpointer and embedded DeerFlow clients."""

    def __init__(self, config: LocalACPConfig):
        self.config = config
        self._pinned_config = None
        self.policy = LocalACPCapabilityPolicy.from_config(config)
        self.session_coordinator = ACPSessionCoordinator()
        self.permission_broker = ACPPermissionBroker(
            self.policy,
            session_owner=self.session_coordinator.owner,
        )
        self.permission_middleware = ACPPermissionMiddleware(self.permission_broker)
        self._checkpointer_cm: AbstractAsyncContextManager[Any] | None = None
        self._checkpointer: Any = None
        self._clients: dict[tuple[Any, ...], DeerFlowClient] = {}
        self._client_mcp_bindings: dict[str, ClientMCPBinding] = {}
        self._client_lock = asyncio.Lock()
        self._run_slots = asyncio.Semaphore(config.max_active_runs)
        self.active_runs = 0
        self.queued_runs = 0

    @asynccontextmanager
    async def run_slot(self, callback: LiveEventCallback):
        async def emit(event: dict[str, Any]) -> None:
            result = callback(event)
            if inspect.isawaitable(result):
                await result

        started = asyncio.get_running_loop().time()
        if self._run_slots.locked():
            await emit({"type": "queue_status", "elapsed_seconds": 0})
        acquire = asyncio.create_task(self._run_slots.acquire())
        self.queued_runs += 1
        acquired = False
        try:
            while not acquire.done():
                done, _ = await asyncio.wait({acquire}, timeout=0 if not self._run_slots.locked() else 5)
                if not done:
                    await emit({"type": "queue_status", "elapsed_seconds": round(asyncio.get_running_loop().time() - started)})
                    await asyncio.wait({acquire}, timeout=5)
            await acquire
            acquired = True
            self.queued_runs -= 1
            self.active_runs += 1
            await emit({"type": "run_started"})
            yield
        finally:
            if acquired:
                self.active_runs -= 1
                self._run_slots.release()
            else:
                self.queued_runs -= 1
                acquire.cancel()
                results = await asyncio.gather(acquire, return_exceptions=True)
                if results == [True]:
                    self._run_slots.release()


    def validate_sandbox_provider(self) -> None:
        """Require the host-local provider supported by portable ACP."""

        from deerflow.config import get_app_config
        from deerflow.sandbox.provider_paths import is_local_sandbox_provider_path

        provider_path = get_app_config().sandbox.use
        if not is_local_sandbox_provider_path(provider_path):
            raise RuntimeError(
                "Portable ACP supports only LocalSandboxProvider; "
                f"configured provider: {provider_path}"
            )

    def bind_permission_handler(
        self, connection_id: str, handler: PermissionHandler
    ) -> None:
        self.permission_broker.bind(handler, connection_id)

    def unbind_permission_handler(self, connection_id: str) -> None:
        self.permission_broker.unbind(connection_id)

    async def open(self) -> None:
        if self._checkpointer is not None:
            return
        cm = AsyncSqliteSaver.from_conn_string(str(self.config.checkpointer_path))
        saver = await cm.__aenter__()
        try:
            await saver.setup()
        except BaseException:
            await cm.__aexit__(None, None, None)
            raise
        self._checkpointer_cm = cm
        self._checkpointer = saver

    async def close(self) -> None:
        client_mcp_sessions = list(self._client_mcp_bindings)
        if client_mcp_sessions:
            results = await asyncio.gather(
                *(
                    self.release_client_mcp(session_id)
                    for session_id in client_mcp_sessions
                ),
                return_exceptions=True,
            )
            for session_id, result in zip(client_mcp_sessions, results, strict=True):
                if isinstance(result, BaseException):
                    logger.warning(
                        "Failed to release client MCP while closing ACP runtime for session %s",
                        session_id,
                        exc_info=(type(result), result, result.__traceback__),
                    )
        self._clients.clear()
        cm = self._checkpointer_cm
        self._checkpointer_cm = None
        self._checkpointer = None
        try:
            if cm is not None:
                await cm.__aexit__(None, None, None)
        finally:
            await self._flush_memory()

    async def _flush_memory(self) -> None:
        try:
            from deerflow.agents.memory import get_memory_manager, reset_memory_manager
            from deerflow.config.memory_config import get_memory_config

            memory_config = get_memory_config()
            if not memory_config.enabled:
                return
            manager = get_memory_manager()
            flushed = await asyncio.wait_for(
                asyncio.to_thread(manager.shutdown_flush),
                timeout=memory_config.shutdown_flush_timeout_seconds + 1.0,
            )
            if flushed is not False:
                await asyncio.to_thread(reset_memory_manager)
            else:
                logger.warning("ACP memory queue did not drain during shutdown")
        except TimeoutError:
            logger.warning("ACP memory shutdown flush timed out")
        except Exception:
            logger.warning("ACP memory shutdown flush failed", exc_info=True)

    async def warmup(self) -> None:
        """Build and cache the default DeerFlow client graph without calling a model."""

        session = LocalACPSession(
            session_id="__deerflow_acp_warmup__",
            cwd="",
            title=None,
            updated_at="",
            model_name=self.config.model_name,
            thinking_enabled=self.config.thinking_enabled,
            subagent_enabled=self.config.subagent_enabled,
            plan_mode=self.config.plan_mode,
            max_concurrent_subagents=self.config.max_concurrent_subagents,
            recursion_limit=self.config.recursion_limit,
            agent_name=self.config.agent_name,
        )
        client = await self._client_for(session)
        await asyncio.to_thread(client.warmup)

    async def _client_for(self, session: LocalACPSession) -> DeerFlowClient:
        if self._checkpointer is None:
            raise RuntimeError("Local ACP runtime is not open")
        binding = self._client_mcp_bindings.get(session.session_id)
        key: tuple[Any, ...]
        if binding is None:
            key = ("shared", *session.runtime_key())
        else:
            key = (
                "client-mcp",
                session.session_id,
                binding.fingerprint,
                *session.runtime_key(),
            )
        async with self._client_lock:
            client = self._clients.get(key)
            if client is None:
                effective_subagents = (
                    session.subagent_enabled and self.policy.subagents_enabled
                )
                kwargs: dict[str, Any] = {
                    "config_path": None if self._pinned_config is not None else str(self.config.config_path),
                    "checkpointer": self._checkpointer,
                    "model_name": session.model_name,
                    "thinking_enabled": session.thinking_enabled,
                    "subagent_enabled": effective_subagents,
                    "plan_mode": session.plan_mode,
                    "max_concurrent_subagents": (
                        session.max_concurrent_subagents if effective_subagents else 1
                    ),
                    "recursion_limit": session.recursion_limit,
                    "agent_name": session.agent_name,
                    "excluded_tool_names": self.policy.excluded_tool_names(
                        enable_bash=self.config.enable_bash
                    ),
                    "allowed_tool_names": (
                        set(self.policy.tool_allowlist)
                        if self.policy.tool_allowlist is not None
                        else None
                    ),
                    "system_prompt_overlay": self.policy.prompt_overlay(),
                    "subagent_system_prompt_overlay": self.policy.prompt_overlay(
                        for_subagent=True
                    ),
                    "middlewares": [self.permission_middleware],
                    "subagent_middlewares": [self.permission_middleware],
                }
                if binding is not None:
                    from deerflow.mcp.tools import get_mcp_tools

                    kwargs["additional_mcp_tools"] = await get_mcp_tools(
                        binding.extensions_config
                    )
                client = DeerFlowClient(**kwargs)
                self._clients[key] = client
            return client

    async def bind_client_mcp(
        self,
        session_id: str,
        binding: ClientMCPBinding | None,
    ) -> None:
        """Attach an in-memory client MCP definition to one ACP session."""

        current = self._client_mcp_bindings.get(session_id)
        if (
            current is not None
            and binding is not None
            and current.fingerprint == binding.fingerprint
        ):
            return
        if current is not None:
            await self.release_client_mcp(session_id)
        if binding is not None:
            self._client_mcp_bindings[session_id] = binding

    async def release_client_mcp(self, session_id: str) -> None:
        """Forget a session's client MCP config and close its MCP processes."""

        async with self._client_lock:
            stale_keys = [
                key
                for key in self._clients
                if len(key) > 1 and key[0] == "client-mcp" and key[1] == session_id
            ]
            for key in stale_keys:
                self._clients.pop(key, None)

        from deerflow.mcp.session_pool import get_session_pool

        await get_session_pool().close_scope(session_id)
        # Keep the binding as a retry marker if close_scope raises. A later
        # session load or daemon shutdown will attempt the cleanup again.
        self._client_mcp_bindings.pop(session_id, None)

    async def release_session(self, session_id: str) -> None:
        try:
            await self.release_client_mcp(session_id)
        finally:
            self.permission_broker.clear_session(session_id)

    async def purge_checkpoints(self, session_ids: list[str]) -> list[str]:
        checkpointer = self._checkpointer
        if checkpointer is None:
            return []
        purged: list[str] = []
        for session_id in session_ids:
            try:
                await checkpointer.adelete_thread(session_id)
            except Exception:
                logger.warning(
                    "Failed to purge ACP checkpoints for session %s",
                    session_id,
                    exc_info=True,
                )
            else:
                purged.append(session_id)
        return purged

    async def compact_checkpoints(self) -> bool:
        """Compact the ACP SQLite database during an idle startup window."""

        checkpointer = self._checkpointer
        conn = getattr(checkpointer, "conn", None)
        lock = getattr(checkpointer, "lock", None)
        if conn is None or lock is None:
            return False
        try:
            async with lock:
                checkpoint_cursor = await conn.execute(
                    "PRAGMA wal_checkpoint(TRUNCATE)"
                )
                await checkpoint_cursor.fetchall()
                await checkpoint_cursor.close()
                vacuum_cursor = await conn.execute("VACUUM")
                await vacuum_cursor.close()
                await conn.commit()
        except Exception:
            logger.warning("Failed to compact ACP checkpoint database", exc_info=True)
            return False
        return True

    def _require_checkpointer(self) -> Any:
        if self._checkpointer is None:
            raise RuntimeError("Local ACP runtime is not open")
        return self._checkpointer

    async def get_goal(self, session_id: str) -> GoalState | None:
        """Return a defensive copy of one ACP session's active goal."""

        return await read_thread_goal(self._require_checkpointer(), session_id)

    async def set_goal(self, session_id: str, objective: str) -> GoalState:
        """Set or replace the durable goal for one ACP session."""

        goal = build_goal_state(
            objective,
            auto_continue=self.config.goal_auto_continue,
            max_continuations=self.config.goal_max_continuations,
            max_no_progress_continuations=(
                self.config.goal_max_no_progress_continuations
            ),
        )
        checkpointer = self._require_checkpointer()
        async with goal_thread_lock(session_id):
            await write_thread_goal(
                checkpointer,
                session_id,
                goal,
                create_if_missing=True,
                as_node="goal_command",
            )
        return goal

    async def clear_goal(self, session_id: str) -> None:
        """Clear an active goal, treating a missing checkpoint as already clear."""

        checkpointer = self._require_checkpointer()
        try:
            async with goal_thread_lock(session_id):
                await write_thread_goal(
                    checkpointer,
                    session_id,
                    None,
                    as_node="goal_command",
                )
        except LookupError:
            return

    def _memory_user_id(
        self, session: LocalACPSession, workspace_path: str
    ) -> str | None:
        if self.config.memory_scope == "global":
            return None
        if self.config.memory_scope == "session":
            return f"acp-session:{session.session_id}"
        digest = hashlib.sha256(workspace_path.encode("utf-8")).hexdigest()[:24]
        return f"acp-workspace:{digest}"

    async def astream(
        self,
        session: LocalACPSession,
        message: str,
        *,
        live_event_callback: LiveEventCallback,
        input_images: list[dict[str, str | int]] | None = None,
    ) -> AsyncGenerator[StreamEvent, None]:
        from .workspace import normalize_workspace_cwd, workspace_paths_equal

        self.validate_sandbox_provider()
        try:
            workspace_path = normalize_workspace_cwd(session.cwd)
        except ValueError as exc:
            raise RuntimeError(f"ACP session workspace is unavailable: {exc}") from exc
        if not workspace_paths_equal(session.cwd, workspace_path):
            raise RuntimeError(
                "ACP session workspace changed after session creation: "
                f"expected {session.cwd}, resolved to {workspace_path}"
            )
        self.permission_broker.set_session_approval_mode(
            session.session_id,
            session.approval_mode,
        )
        client = await self._client_for(session)
        async with self.run_slot(live_event_callback):
            client_kwargs: dict[str, Any] = {
                "thread_id": session.session_id,
                "live_event_callback": live_event_callback,
                "workspace_path": workspace_path,
                "user_id": self._memory_user_id(session, workspace_path),
            }
            if input_images:
                client_kwargs["input_images"] = input_images
            evaluator_model: Any | None = None
            current_message: str | HumanMessage = message
            try:
                while True:
                    turn_failed = False
                    try:
                        async for event in client.astream(
                            current_message,
                            **client_kwargs,
                        ):
                            if (
                                event.type == "custom"
                                and event.data.get("type") == "llm_failure"
                            ):
                                turn_failed = True
                            yield event
                    except asyncio.CancelledError:
                        raise
                    except Exception:
                        if self._goal_checkpointer_available():
                            try:
                                await self._record_failed_goal_turn(
                                    session.session_id,
                                    reason=(
                                        "The Agent run raised an error before the "
                                        "goal could be evaluated."
                                    ),
                                )
                            except Exception:
                                logger.warning(
                                    "Failed to record goal run failure for ACP session %s",
                                    session.session_id,
                                    exc_info=True,
                                )
                        raise

                    # Images belong only to the genuine initial user turn. A
                    # hidden continuation reuses checkpoint context instead.
                    client_kwargs.pop("input_images", None)
                    if turn_failed:
                        if self._goal_checkpointer_available():
                            status = await self._record_failed_goal_turn(
                                session.session_id,
                                reason="The model run failed before the goal could be evaluated.",
                            )
                            if status is not None:
                                yield status
                        break

                    checkpointer = self._checkpointer
                    if not self._goal_checkpointer_available():
                        # Lightweight injected clients in embedders may opt out
                        # of persistence entirely; such clients cannot host a
                        # durable goal and retain the historical single-turn path.
                        break
                    snapshot = await read_goal_snapshot(
                        checkpointer,
                        session.session_id,
                    )
                    if not snapshot.goal or snapshot.goal.get("status") != "active":
                        break
                    if evaluator_model is None:
                        evaluator_model = create_goal_evaluator_model(
                            model_name=session.model_name
                        )
                    result = await self._evaluate_goal_turn(
                        session,
                        evaluator_model=evaluator_model,
                        snapshot=snapshot,
                    )
                    if result.status_event is not None:
                        yield result.status_event
                    if result.continuation is None:
                        break
                    current_message = result.continuation
            finally:
                await aclose_chat_model(evaluator_model)

    def _goal_checkpointer_available(self) -> bool:
        checkpointer = self._checkpointer
        return checkpointer is not None and any(
            callable(getattr(checkpointer, name, None))
            for name in ("aget_tuple", "get_tuple")
        )

    async def _record_failed_goal_turn(
        self,
        session_id: str,
        *,
        reason: str,
    ) -> StreamEvent | None:
        checkpointer = self._require_checkpointer()
        snapshot = await read_goal_snapshot(checkpointer, session_id)
        goal = snapshot.goal
        if not goal:
            return None
        evaluation = GoalEvaluation(
            satisfied=False,
            blocker="run_failed",
            reason=reason,
            evidence_summary="",
        )
        evidence_signature = latest_visible_assistant_signature(snapshot.messages)
        updated = attach_goal_evaluation(
            goal,
            evaluation,
            no_progress_count=compute_no_progress_count(
                goal,
                evaluation,
                evidence_signature=evidence_signature,
            ),
            stand_down_reason="run_failed",
            evidence_signature=evidence_signature,
        )
        try:
            async with goal_thread_lock(session_id):
                current = await read_goal_snapshot(checkpointer, session_id)
                if (
                    not goal_instance_matches(goal, current.goal)
                    or current.checkpoint_id != snapshot.checkpoint_id
                ):
                    return None
                await write_thread_goal(
                    checkpointer,
                    session_id,
                    updated,
                    expected_checkpoint_id=snapshot.checkpoint_id,
                    as_node="goal_evaluator",
                )
        except GoalWriteConflict:
            return None
        return self._goal_status_event(
            updated,
            evaluation,
            status="paused",
            stand_down_reason="run_failed",
        )

    async def _evaluate_goal_turn(
        self,
        session: LocalACPSession,
        *,
        evaluator_model: Any,
        snapshot: GoalCheckpointSnapshot,
    ) -> _GoalTurnResult:
        """Evaluate and atomically commit one post-turn goal decision."""

        goal: GoalState = snapshot.goal
        conversation_signature = visible_conversation_signature(snapshot.messages)
        evidence_signature = latest_visible_assistant_signature(snapshot.messages)
        evaluator_usage: dict[str, int] = {}
        try:
            evaluation = await evaluate_goal_completion(
                goal,
                snapshot.messages,
                model=evaluator_model,
                model_name=session.model_name,
                thread_id=session.session_id,
                usage_callback=evaluator_usage.update,
            )
            evaluator_failed = False
        except asyncio.CancelledError:
            raise
        except Exception:
            logger.warning(
                "Goal evaluator failed for ACP session %s",
                session.session_id,
                exc_info=True,
            )
            evaluator_failed = True
            evaluation = GoalEvaluation(
                satisfied=False,
                blocker="missing_evidence",
                reason="The goal evaluator failed; automatic continuation stopped safely.",
                evidence_summary="",
            )

        checkpointer = self._require_checkpointer()
        current = await read_goal_snapshot(checkpointer, session.session_id)
        if not goal_instance_matches(goal, current.goal):
            return _GoalTurnResult(None, None)
        if (
            current.checkpoint_id != snapshot.checkpoint_id
            or visible_conversation_signature(current.messages)
            != conversation_signature
        ):
            return _GoalTurnResult(None, None)

        if evaluation["satisfied"]:
            try:
                async with goal_thread_lock(session.session_id):
                    latest = await read_goal_snapshot(
                        checkpointer,
                        session.session_id,
                    )
                    if (
                        not goal_instance_matches(goal, latest.goal)
                        or latest.checkpoint_id != snapshot.checkpoint_id
                        or visible_conversation_signature(latest.messages)
                        != conversation_signature
                    ):
                        return _GoalTurnResult(None, None)
                    await write_thread_goal(
                        checkpointer,
                        session.session_id,
                        None,
                        expected_checkpoint_id=snapshot.checkpoint_id,
                        as_node="goal_evaluator",
                    )
            except GoalWriteConflict:
                return _GoalTurnResult(None, None)
            return _GoalTurnResult(
                None,
                self._goal_status_event(
                    goal,
                    evaluation,
                    status="completed",
                    evaluator_usage=evaluator_usage,
                ),
            )

        no_progress_count = compute_no_progress_count(
            goal,
            evaluation,
            evidence_signature=evidence_signature,
        )
        stand_down_reason = (
            "evaluator_failed"
            if evaluator_failed
            else goal_stand_down_reason(
                goal,
                evaluation,
                no_progress_count=no_progress_count,
            )
        )
        continue_goal = not evaluator_failed and should_continue_goal(
            goal,
            evaluation,
            no_progress_count=no_progress_count,
        )
        next_count = int(goal.get("continuation_count", 0)) + 1
        updated = attach_goal_evaluation(
            goal,
            evaluation,
            continuation_count=next_count if continue_goal else None,
            no_progress_count=no_progress_count,
            stand_down_reason=stand_down_reason,
            evidence_signature=evidence_signature,
        )
        try:
            async with goal_thread_lock(session.session_id):
                latest = await read_goal_snapshot(checkpointer, session.session_id)
                if (
                    not goal_instance_matches(goal, latest.goal)
                    or latest.checkpoint_id != snapshot.checkpoint_id
                    or visible_conversation_signature(latest.messages)
                    != conversation_signature
                ):
                    return _GoalTurnResult(None, None)
                await write_thread_goal(
                    checkpointer,
                    session.session_id,
                    updated,
                    expected_checkpoint_id=snapshot.checkpoint_id,
                    as_node="goal_evaluator",
                )
        except GoalWriteConflict:
            return _GoalTurnResult(None, None)

        if not continue_goal:
            return _GoalTurnResult(
                None,
                self._goal_status_event(
                    updated,
                    evaluation,
                    status="paused",
                    stand_down_reason=stand_down_reason,
                    evaluator_usage=evaluator_usage,
                ),
            )
        return _GoalTurnResult(
            make_goal_continuation_message(updated, evaluation),
            self._goal_status_event(
                updated,
                evaluation,
                status="continuing",
                evaluator_usage=evaluator_usage,
            ),
        )

    @staticmethod
    def _goal_status_event(
        goal: GoalState,
        evaluation: GoalEvaluation,
        *,
        status: str,
        stand_down_reason: str | None = None,
        evaluator_usage: dict[str, int] | None = None,
    ) -> StreamEvent:
        data: dict[str, Any] = {
            "type": "goal_status",
            "status": status,
            "objective": goal.get("objective", ""),
            "continuation_count": int(goal.get("continuation_count", 0)),
            "max_continuations": int(goal.get("max_continuations", 0)),
            "blocker": evaluation.get("blocker", "none"),
            "reason": evaluation.get("reason", ""),
            "stand_down_reason": stand_down_reason,
        }
        if evaluator_usage:
            data["evaluator_usage"] = evaluator_usage
        return StreamEvent(type="custom", data=data)

    async def history(self, session_id: str) -> list[dict[str, Any]]:
        """Return the latest full message snapshot for session/load replay."""

        checkpointer = self._checkpointer
        if checkpointer is None:
            raise RuntimeError("Local ACP runtime is not open")
        checkpoint = await checkpointer.aget_tuple(
            {"configurable": {"thread_id": session_id, "checkpoint_ns": ""}}
        )
        if checkpoint is None:
            return []
        messages = checkpoint.checkpoint.get("channel_values", {}).get("messages", [])
        return [
            DeerFlowClient._serialize_message(message)
            if hasattr(message, "content")
            else dict(message)
            for message in messages
            if (hasattr(message, "content") or isinstance(message, dict))
            and not self._hidden_message(message)
        ]

    async def history_state(self, session_id: str) -> dict[str, Any]:
        """Return replayable messages plus plan, artifact, and title state."""

        checkpointer = self._checkpointer
        if checkpointer is None:
            raise RuntimeError("Local ACP runtime is not open")
        checkpoint = await checkpointer.aget_tuple(
            {"configurable": {"thread_id": session_id, "checkpoint_ns": ""}}
        )
        if checkpoint is None:
            return {"messages": [], "todos": [], "artifacts": [], "goal": None}
        values = checkpoint.checkpoint.get("channel_values", {})
        messages = values.get("messages", [])
        return {
            "messages": [
                DeerFlowClient._serialize_message(message)
                if hasattr(message, "content")
                else dict(message)
                for message in messages
                if (
                    hasattr(message, "content") or isinstance(message, dict)
                )
                and not self._hidden_message(message)
            ],
            "todos": values.get("todos", []),
            "artifacts": values.get("artifacts", []),
            "title": values.get("title"),
            "goal": values.get("goal"),
        }

    @staticmethod
    def _hidden_message(message: Any) -> bool:
        kwargs = getattr(message, "additional_kwargs", None)
        if kwargs is None and isinstance(message, dict):
            kwargs = message.get("additional_kwargs")
        return isinstance(kwargs, dict) and kwargs.get("hide_from_ui") is True
