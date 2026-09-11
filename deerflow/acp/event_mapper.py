"""Map embedded DeerFlow stream events to ACP session updates."""

from __future__ import annotations

import asyncio
import inspect
import json
import logging
import mimetypes
import uuid
from collections.abc import Awaitable, Callable
from pathlib import PurePosixPath
from typing import Any, cast

import acp
from acp import schema

from deerflow.config.paths import get_paths
from deerflow.sandbox.output_paths import resolve_outputs_virtual_path

from .policy import tool_kind as _tool_kind

SendUpdate = Callable[[Any], Awaitable[None]]
ArtifactResolver = Callable[
    [str],
    schema.ResourceContentBlock
    | None
    | Awaitable[schema.ResourceContentBlock | None],
]
logger = logging.getLogger(__name__)
_MESSAGE_NAMESPACE = uuid.UUID("d0af913a-b872-4e87-9b31-3fc58f03b3f8")
_OUTPUT_PREFIX = "/mnt/user-data/outputs/"
_MAX_TOOL_TEXT = 20_000
_ACP_STOP_REASONS: dict[str, schema.StopReason] = {
    "tool_call_limit": "max_turn_requests",
    "recursion_limit": "max_turn_requests",
    "model_length_capped": "max_tokens",
    "safety_capped": "refusal",
}


def _message_uuid(kind: str, raw_id: Any) -> str:
    value = str(raw_id or "")
    try:
        return str(uuid.UUID(value))
    except (ValueError, AttributeError):
        return str(uuid.uuid5(_MESSAGE_NAMESPACE, f"{kind}:{value}"))


def _safe_json(value: Any) -> Any:
    return json.loads(json.dumps(value, ensure_ascii=False, default=str))


def _display_text(value: Any) -> str:
    if isinstance(value, str):
        return value
    if value is None:
        return ""
    if isinstance(value, list):
        pieces: list[str] = []
        for item in value:
            if isinstance(item, str):
                pieces.append(item)
            elif isinstance(item, dict):
                text = item.get("text") or item.get("content")
                if isinstance(text, str):
                    pieces.append(text)
        if pieces:
            return "\n".join(pieces)
    return json.dumps(value, ensure_ascii=False, default=str)


def _limited_text(value: Any) -> str:
    text = _display_text(value)
    if len(text) <= _MAX_TOOL_TEXT:
        return text
    return text[:_MAX_TOOL_TEXT] + "\n… (truncated)"


class ACPEventMapper:
    """Stateful, per-prompt ACP event mapper."""

    def __init__(
        self,
        session_id: str,
        send_update: SendUpdate,
        *,
        artifact_resolver: ArtifactResolver | None = None,
        outputs_path: str | None = None,
    ):
        self.session_id = session_id
        self._send_update = send_update
        self._outputs_path = outputs_path
        self._artifact_resolver = artifact_resolver or self._default_artifact_resolver
        self._lock = asyncio.Lock()
        self._reasoning: dict[str, str] = {}
        self._started_tools: set[str] = set()
        self._finished_tools: set[str] = set()
        self._seen_artifacts: set[str] = set()
        self._last_plan: str | None = None
        self._last_title: str | None = None
        self.title: str | None = None
        self.usage: dict[str, int] = {
            "input_tokens": 0,
            "output_tokens": 0,
            "total_tokens": 0,
        }
        self._lead_usage = dict(self.usage)
        self._subagent_usage = dict(self.usage)
        self._goal_evaluator_usage = dict(self.usage)
        self.failure_message: str | None = None
        self.stop_reason: schema.StopReason | None = None
        self._closed = False

    async def _send(self, update: Any) -> None:
        await self._send_update(update)

    def _default_artifact_resolver(self, virtual_path: str) -> schema.ResourceContentBlock | None:
        if not virtual_path.startswith(_OUTPUT_PREFIX):
            return None
        try:
            if self._outputs_path is None:
                host_path = get_paths().resolve_virtual_path(
                    self.session_id,
                    virtual_path,
                ).resolve()
            else:
                host_path = resolve_outputs_virtual_path(
                    self._outputs_path,
                    virtual_path,
                )
        except (OSError, ValueError):
            return None
        name = PurePosixPath(virtual_path).name or "artifact"
        mime_type, _ = mimetypes.guess_type(name)
        try:
            size = host_path.stat().st_size
        except OSError:
            size = None
        return acp.resource_link_block(
            name,
            host_path.as_uri(),
            mime_type=mime_type,
            size=size,
            description="DeerFlow local artifact",
        )

    async def handle(self, event: Any) -> None:
        async with self._lock:
            event_type = str(getattr(event, "type", ""))
            data = getattr(event, "data", {})
            if not isinstance(data, dict):
                return
            if event_type == "messages-tuple":
                await self._handle_message(data)
            elif event_type == "values":
                await self._handle_values(data)
            elif event_type == "custom":
                await self._handle_live_unlocked(data)
            elif event_type == "end":
                internal_stop_reason = data.get("stop_reason")
                if isinstance(internal_stop_reason, str):
                    mapped_stop_reason = _ACP_STOP_REASONS.get(internal_stop_reason)
                    if mapped_stop_reason is not None:
                        self.stop_reason = mapped_stop_reason
                usage = data.get("usage")
                if isinstance(usage, dict):
                    for key in ("input_tokens", "output_tokens", "total_tokens"):
                        self._lead_usage[key] += max(
                            0,
                            int(usage.get(key, 0) or 0),
                        )
                    self._refresh_usage()
                await self._send_usage_update(data)
            else:
                await self._handle_live_unlocked({"type": event_type, **data})

    async def handle_live(self, data: dict[str, Any]) -> None:
        async with self._lock:
            if self._closed:
                return
            await self._handle_live_unlocked(data)

    async def _handle_message(self, data: dict[str, Any]) -> None:
        message_type = data.get("type")
        if message_type == "ai":
            raw_id = data.get("id") or "agent"
            message_id = _message_uuid("agent", raw_id)
            reasoning = data.get("reasoning_content")
            if isinstance(reasoning, str) and reasoning:
                previous = self._reasoning.get(message_id, "")
                delta = reasoning[len(previous) :] if previous and reasoning.startswith(previous) else reasoning
                self._reasoning[message_id] = reasoning if reasoning.startswith(previous) else previous + delta
                if delta:
                    await self._send(
                        schema.AgentThoughtChunk(
                            session_update="agent_thought_chunk",
                            content=acp.text_block(delta),
                            message_id=_message_uuid("thought", raw_id),
                        )
                    )

            content = data.get("content")
            if isinstance(content, str) and content:
                await self._send(
                    schema.AgentMessageChunk(
                        session_update="agent_message_chunk",
                        content=acp.text_block(content),
                        message_id=message_id,
                    )
                )

            tool_calls = data.get("tool_calls")
            if isinstance(tool_calls, list):
                for tool_call in tool_calls:
                    if isinstance(tool_call, dict):
                        await self._start_tool(tool_call)
            return

        if message_type == "tool":
            tool_call_id = str(data.get("tool_call_id") or data.get("id") or uuid.uuid4())
            if tool_call_id not in self._started_tools:
                await self._start_tool(
                    {
                        "id": tool_call_id,
                        "name": data.get("name") or "tool",
                        "args": {},
                    }
                )
            await self._finish_tool(
                tool_call_id,
                status="failed" if data.get("status") in {"error", "failed"} else "completed",
                output=data.get("content"),
            )

    async def _start_tool(self, tool_call: dict[str, Any], *, prefix: str = "") -> str:
        raw_id = str(tool_call.get("id") or uuid.uuid4())
        tool_call_id = f"{prefix}{raw_id}"
        name = str(tool_call.get("name") or "tool")
        args = _safe_json(tool_call.get("args") or {})
        if tool_call_id in self._started_tools:
            return tool_call_id
        self._started_tools.add(tool_call_id)
        await self._send(
            acp.start_tool_call(
                tool_call_id,
                name,
                kind=_tool_kind(name),
                status="in_progress",
                raw_input=args,
            )
        )
        return tool_call_id

    async def _finish_tool(self, tool_call_id: str, *, status: schema.ToolCallStatus, output: Any) -> None:
        if tool_call_id in self._finished_tools:
            return
        self._finished_tools.add(tool_call_id)
        text = _limited_text(output)
        content = [acp.tool_content(acp.text_block(text))] if text else None
        await self._send(
            acp.update_tool_call(
                tool_call_id,
                status=status,
                content=content,
                raw_output=_safe_json(output),
            )
        )

    async def _handle_values(self, data: dict[str, Any]) -> None:
        title = data.get("title")
        if isinstance(title, str) and title and title != self._last_title:
            self._last_title = title
            self.title = title
            await self._send(
                schema.SessionInfoUpdate(
                    session_update="session_info_update",
                    title=title,
                )
            )

        todos = data.get("todos")
        if isinstance(todos, list):
            normalized: list[schema.PlanEntry] = []
            for todo in todos:
                if not isinstance(todo, dict):
                    continue
                content = todo.get("content") or todo.get("description") or todo.get("title")
                if not isinstance(content, str) or not content:
                    continue
                status = str(todo.get("status") or "pending")
                if status not in {"pending", "in_progress", "completed"}:
                    status = "pending"
                priority = str(todo.get("priority") or "medium")
                if priority not in {"low", "medium", "high"}:
                    priority = "medium"
                normalized.append(
                    schema.PlanEntry(
                        content=content,
                        status=cast(schema.PlanEntryStatus, status),
                        priority=cast(schema.PlanEntryPriority, priority),
                    )
                )
            signature = json.dumps(
                [entry.model_dump(mode="json") for entry in normalized],
                ensure_ascii=False,
                sort_keys=True,
            )
            if signature != self._last_plan:
                self._last_plan = signature
                await self._send(acp.update_plan(normalized))

        artifacts = data.get("artifacts")
        if isinstance(artifacts, list):
            for raw in artifacts:
                if not isinstance(raw, str) or raw in self._seen_artifacts:
                    continue
                try:
                    resolved = self._artifact_resolver(raw)
                    block = (
                        await cast(
                            Awaitable[schema.ResourceContentBlock | None],
                            resolved,
                        )
                        if inspect.isawaitable(resolved)
                        else resolved
                    )
                except Exception as exc:
                    message = f"Failed to publish artifact {raw}: {exc}"
                    logger.error("ACP artifact publishing failed: %s", message)
                    self.failure_message = message
                    self._seen_artifacts.add(raw)
                    await self._send(
                        schema.AgentMessageChunk(
                            session_update="agent_message_chunk",
                            content=acp.text_block(message),
                            message_id=_message_uuid("artifact-error", raw),
                        )
                    )
                    continue
                if block is None:
                    continue
                self._seen_artifacts.add(raw)
                await self._send(
                    schema.AgentMessageChunk(
                        session_update="agent_message_chunk",
                        content=block,
                        message_id=_message_uuid("artifact", raw),
                    )
                )

    async def _handle_live_unlocked(self, data: dict[str, Any]) -> None:
        if data.get("type") in {"queue_status", "run_started"}:
            queued = data["type"] == "queue_status"
            await self._send(schema.AgentThoughtChunk(
                session_update="agent_thought_chunk",
                content=acp.text_block(
                    f"排队中，已等待 {data.get('elapsed_seconds', 0)} 秒；可随时取消。\n"
                    if queued else "已开始执行，执行超时从现在计时。\n"
                ),
            ))
            return

        event_type = str(data.get("type") or "")
        if event_type == "llm_failure":
            self.failure_message = str(data.get("message") or data.get("reason") or "Model request failed")
            return

        if event_type == "loop_hard_stop":
            self.stop_reason = "max_turn_requests"
            return

        if event_type == "goal_status":
            evaluator_usage = data.get("evaluator_usage")
            if isinstance(evaluator_usage, dict):
                for key in ("input_tokens", "output_tokens", "total_tokens"):
                    self._goal_evaluator_usage[key] += max(
                        0,
                        int(evaluator_usage.get(key, 0) or 0),
                    )
                self._refresh_usage()
            status = str(data.get("status") or "paused")
            objective = str(data.get("objective") or "")
            reason = str(data.get("reason") or "")
            stand_down = str(data.get("stand_down_reason") or "")
            count = max(0, int(data.get("continuation_count", 0) or 0))
            maximum = max(0, int(data.get("max_continuations", 0) or 0))
            if status == "completed":
                text = f"Goal completed: {objective}"
            elif status == "continuing":
                text = f"Goal not complete; continuing ({count}/{maximum})."
                if reason:
                    text += f" {reason}"
            elif stand_down == "auto_continue_disabled":
                text = "Goal remains active; automatic continuation is disabled."
            elif stand_down == "continuation_limit":
                text = f"Goal remains active; continuation limit reached ({count}/{maximum})."
            elif stand_down == "no_progress_limit":
                text = "Goal remains active; automatic continuation stopped after repeated no-progress evaluations."
            elif stand_down == "needs_user_input":
                text = "Goal remains active and needs user input."
            elif stand_down == "external_wait":
                text = "Goal remains active and is waiting on an external system."
            elif stand_down == "run_failed":
                text = "Goal remains active; the run failed."
            elif stand_down == "evaluator_failed":
                text = "Goal remains active; completion evaluation failed safely."
            else:
                text = "Goal remains active; automatic continuation paused."
            if status != "continuing" and status != "completed" and reason:
                text += f" {reason}"
            await self._send(
                schema.AgentMessageChunk(
                    session_update="agent_message_chunk",
                    content=acp.text_block(_limited_text(text)),
                    message_id=_message_uuid(
                        "goal-status",
                        f"{status}:{stand_down}:{count}:{objective}",
                    ),
                )
            )
            return

        task_id = str(data.get("task_id") or data.get("trace_id") or "")
        if event_type == "token_usage" and task_id:
            for key in ("input_tokens", "output_tokens", "total_tokens"):
                self._subagent_usage[key] += max(0, int(data.get(key, 0) or 0))
            self._refresh_usage()
            return
        subagent_tool_id = f"subagent:{task_id}" if task_id else ""
        if event_type in {"task_started", "subagent_started"} and task_id:
            title = str(data.get("description") or data.get("name") or "Subtask")
            if subagent_tool_id not in self._started_tools:
                self._started_tools.add(subagent_tool_id)
                await self._send(
                    acp.start_tool_call(
                        subagent_tool_id,
                        title,
                        kind="think",
                        status="in_progress",
                        raw_input=_safe_json(data),
                    )
                )
            return

        if event_type in {"token_chunk", "thinking_chunk", "task_running"} and task_id:
            if subagent_tool_id not in self._started_tools:
                await self._handle_live_unlocked({**data, "type": "task_started"})
            if subagent_tool_id in self._finished_tools:
                return
            progress = data.get("content") or data.get("thinking") or data.get("message")
            text = _limited_text(progress)
            if text:
                await self._send(
                    acp.update_tool_call(
                        subagent_tool_id,
                        status="in_progress",
                        content=[acp.tool_content(acp.text_block(text))],
                    )
                )
            return

        if event_type in {
            "task_completed",
            "task_failed",
            "task_cancelled",
            "task_timed_out",
            "task_limit_reached",
        } and task_id:
            if subagent_tool_id not in self._started_tools:
                await self._handle_live_unlocked({**data, "type": "task_started"})
            tool_status: schema.ToolCallStatus = (
                "completed" if event_type == "task_completed" else "failed"
            )
            if event_type == "task_limit_reached":
                self.stop_reason = _ACP_STOP_REASONS.get(
                    str(data.get("reason") or "tool_call_limit"),
                    "max_turn_requests",
                )
            await self._finish_tool(
                subagent_tool_id,
                status=tool_status,
                output=data.get("result")
                if tool_status == "completed"
                else data.get("error") or event_type,
            )
            return

        if event_type == "tool_call_chunk" and task_id:
            tool_call = data.get("tool_call")
            if isinstance(tool_call, dict):
                await self._start_tool(tool_call, prefix=f"subagent:{task_id}:tool:")
            return

        if event_type == "tool_result_chunk" and task_id:
            raw_id = str(data.get("tool_call_id") or uuid.uuid4())
            nested_id = f"subagent:{task_id}:tool:{raw_id}"
            if nested_id not in self._started_tools:
                await self._start_tool(
                    {"id": raw_id, "name": data.get("name") or "tool", "args": {}},
                    prefix=f"subagent:{task_id}:tool:",
                )
            failed = str(data.get("status") or "").lower() in {"error", "failed"}
            await self._finish_tool(
                nested_id,
                status="failed" if failed else "completed",
                output=data.get("content"),
            )

    def _refresh_usage(self) -> None:
        self.usage = {
            key: (
                self._lead_usage[key]
                + self._subagent_usage[key]
                + self._goal_evaluator_usage[key]
            )
            for key in ("input_tokens", "output_tokens", "total_tokens")
        }

    async def _send_usage_update(self, end_data: dict[str, Any]) -> None:
        """Push an ACP usage_update with the lead thread's context occupancy.

        ``size`` comes from the model's configured ``context_window`` and
        ``used`` from the last top-level model call's usage snapshot (its
        input_tokens approximates the prompt size the next turn will carry).
        Skipped when either value is missing — ACP clients then simply keep
        their previous context indicator.
        """
        context_window = end_data.get("context_window")
        last_usage = end_data.get("last_usage")
        if not isinstance(context_window, int) or context_window <= 0:
            return
        if not isinstance(last_usage, dict):
            return
        try:
            used = max(
                0,
                int(last_usage.get("input_tokens", 0) or 0)
                + int(last_usage.get("output_tokens", 0) or 0),
            )
            await self._send(
                schema.UsageUpdate(
                    session_update="usage_update",
                    size=context_window,
                    used=used,
                )
            )
        except Exception:
            logger.debug("Failed to send ACP usage_update", exc_info=True)

    async def close_open_tools(
        self,
        *,
        cancelled: bool,
        failure_message: str | None = None,
    ) -> None:
        async with self._lock:
            for tool_call_id in sorted(self._started_tools - self._finished_tools):
                failed = cancelled or failure_message is not None
                await self._finish_tool(
                    tool_call_id,
                    status="failed" if failed else "completed",
                    output=(
                        "Cancelled"
                        if cancelled
                        else failure_message or "Completed"
                    ),
                )
            self._closed = True
