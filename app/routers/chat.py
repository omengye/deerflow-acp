"""Chat endpoints — streaming and AG-UI."""
import asyncio
import hashlib
import json
import mimetypes
import re
import uuid
from pathlib import PurePosixPath
from typing import Any, cast
from urllib.parse import quote

import logging

from fastapi import APIRouter, Body, HTTPException, Request
from fastapi.responses import StreamingResponse
from sse_starlette.sse import EventSourceResponse

from app.dependencies import get_client_manager
from app.middleware import get_request_id

logger = logging.getLogger(__name__)
from app.schemas import AguiRunAgentInput, ChatRequest
from deerflow.client import StreamEvent, StreamEventType
from deerflow.runtime import ConflictError, END_SENTINEL, HEARTBEAT_SENTINEL, RunStatus

router = APIRouter(tags=["chat"])

_SSE_RESPONSE_HEADERS = {
    "Cache-Control": "no-cache, no-transform",
    "Connection": "keep-alive",
    "X-Accel-Buffering": "no",
}

_IDEMPOTENCY_KEY_RE = re.compile(r"^[A-Za-z0-9._~:-]{1,128}$")


def _idempotency_actor(request: Request) -> str:
    """Return a non-secret stable API actor identifier."""
    authorization = request.headers.get("authorization") or request.headers.get("Authorization") or ""
    if authorization:
        return "bearer:" + hashlib.sha256(authorization.encode("utf-8")).hexdigest()
    return "anonymous"


def _chat_payload_fingerprint(req: ChatRequest, kwargs: dict[str, Any]) -> str:
    payload = {
        "message": req.message,
        "kwargs": kwargs,
        "on_disconnect": req.on_disconnect or "cancel",
        "multitask_strategy": req.multitask_strategy or "reject",
    }
    encoded = json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


@router.post("/chat/stream")
async def chat_stream(request: Request, req: ChatRequest = Body()):
    """Streaming chat — Server-Sent Events with token-level deltas."""
    manager = get_client_manager()

    thread_id = req.thread_id or str(uuid.uuid4())
    kwargs = _chat_kwargs_from_request(req)
    idempotency_key = request.headers.get("idempotency-key") or request.headers.get("Idempotency-Key")
    if idempotency_key is not None and not _IDEMPOTENCY_KEY_RE.fullmatch(idempotency_key):
        raise HTTPException(status_code=400, detail="Invalid Idempotency-Key; use 1-128 URL-safe ASCII characters")

    try:
        record = await manager.start_client_stream_run(
            thread_id=thread_id,
            message=req.message,
            kwargs=kwargs,
            request_id=get_request_id(),
            on_disconnect=req.on_disconnect or "cancel",
            multitask_strategy=req.multitask_strategy or "reject",
            idempotency_actor=_idempotency_actor(request) if idempotency_key is not None else None,
            idempotency_key=idempotency_key,
            payload_fingerprint=_chat_payload_fingerprint(req, kwargs) if idempotency_key is not None else None,
        )
    except ConflictError as exc:
        raise HTTPException(status_code=409, detail=str(exc)) from exc

    if record.metadata.get("replay_expired"):
        raise HTTPException(
            status_code=410,
            detail={
                "code": "REPLAY_EXPIRED",
                "message": "The idempotent run is retained but its replay data has expired",
                "run_id": record.run_id,
                "thread_id": thread_id,
            },
        )

    last_event_id = request.headers.get("last-event-id")

    async def event_generator():
        try:
            async for entry in manager.stream_bridge.subscribe(record.run_id, last_event_id=last_event_id):
                if entry is HEARTBEAT_SENTINEL:
                    yield {"event": "heartbeat", "data": "{}"}
                    continue
                if entry is END_SENTINEL:
                    break

                yield {
                    "id": entry.id,
                    "event": entry.event,
                    "data": json.dumps(entry.data, ensure_ascii=False, default=str),
                }

                # Simplified text event for easier client consumption
                if entry.event == "messages-tuple" and entry.data:
                    msg = entry.data
                    # worker.py serializes messages-mode as [chunk_dict, metadata_dict]
                    if isinstance(msg, list) and len(msg) >= 1:
                        msg = msg[0]
                    # Normalize LangChain type names
                    if isinstance(msg, dict) and msg.get("type") in ("AIMessageChunk", "AIMessage"):
                        msg = {**msg, "type": "ai"}
                    if isinstance(msg, dict) and msg.get("type") == "ai" and msg.get("content") and not msg.get("tool_calls"):
                        yield {
                            "event": "text",
                            "data": json.dumps(
                                {
                                    "content": msg.get("content", ""),
                                    "thread_id": thread_id,
                                    "run_id": record.run_id,
                                },
                                ensure_ascii=False,
                            ),
                        }
        except asyncio.CancelledError:
            if (req.on_disconnect or "cancel") == "cancel":
                await manager.cancel_run(record.run_id)
            raise
        except Exception:
            logger.exception("Unhandled error in /chat/stream (thread=%s)", thread_id)
            yield {
                "event": "error",
                "data": json.dumps({"error": "Internal server error"}),
            }

    return EventSourceResponse(event_generator())


def _chat_kwargs_from_request(req: ChatRequest) -> dict[str, Any]:
    kwargs: dict[str, Any] = {}
    if req.model_name:
        kwargs["model_name"] = req.model_name
    if req.thinking_enabled is not None:
        kwargs["thinking_enabled"] = req.thinking_enabled
    if req.subagent_enabled is not None:
        kwargs["subagent_enabled"] = req.subagent_enabled
    if req.plan_mode is not None:
        kwargs["plan_mode"] = req.plan_mode
    if req.max_concurrent_subagents is not None:
        kwargs["max_concurrent_subagents"] = req.max_concurrent_subagents
    return kwargs


@router.post("/chat/agui")
async def chat_agui(request: Request, req: AguiRunAgentInput = Body()):
    """AG-UI compatible chat stream.

    The wire contract follows AG-UI HTTP SSE: each chunk is a `data:` JSON
    object whose payload contains a standard AG-UI `type` field.
    """
    manager = get_client_manager()
    thread_id = req.thread_id
    run_id = req.run_id

    last_event_id = request.headers.get("last-event-id")
    requested_kwargs = _chat_kwargs_from_agui(req)
    record = manager.run_manager.get(run_id)
    if record is not None:
        if record.thread_id != thread_id:
            raise HTTPException(status_code=409, detail=f"Run {run_id} belongs to a different thread")
        if record.metadata.get("replay_expired"):
            raise HTTPException(
                status_code=410,
                detail={
                    "code": "REPLAY_EXPIRED",
                    "message": f"Replay data for run {run_id} has expired",
                    "run_id": run_id,
                    "thread_id": thread_id,
                    "status": record.status.value,
                },
            )
        kwargs = dict(record.kwargs)
    else:
        kwargs = requested_kwargs
        if last_event_id:
            raise HTTPException(status_code=410, detail=f"Run {run_id} is no longer available for reconnect")
        user_message = _latest_user_message(req)
        if user_message is None:
            raise HTTPException(status_code=400, detail="No user message found in request")
        try:
            record = await manager.start_client_stream_run(
                thread_id=thread_id,
                run_id=run_id,
                message=user_message,
                kwargs=kwargs,
                request_id=get_request_id(),
                entrypoint="chat_agui",
                on_disconnect=req.on_disconnect or "cancel",
                multitask_strategy=req.multitask_strategy or "reject",
            )
        except ConflictError as exc:
            raise HTTPException(status_code=409, detail=str(exc)) from exc

    client = await manager.get_async_client(**kwargs)
    agent_name: str = client.agent_name

    async def event_generator():
        attached = False
        open_text_message_id: str | None = None
        # Maps tool_call_id → serialized args JSON already sent as delta.
        # Used to compute incremental deltas across streaming chunks.
        tool_call_args_state: dict[str, str] = {}
        subagent_names_by_id: dict[str, str] = {}
        open_subagent_text_message_ids: set[str] = set()
        open_subagent_reasoning_message_ids: set[str] = set()
        # Tool calls that have been started (TOOL_CALL_START emitted) but not yet ended.
        open_tool_call_ids: set[str] = set()
        # Signature of the last emitted MESSAGES_SNAPSHOT (list of message ids).
        # LangGraph fires a values event after every middleware node even when
        # messages haven't changed, so we deduplicate here.
        last_snapshot_sig: str | None = None
        # Reasoning messages that have been started but not yet ended.
        # Track by id because providers can stream separate reasoning blocks.
        open_reasoning_message_ids: set[str] = set()
        # Provider adapters may expose either raw reasoning deltas or cumulative
        # reasoning text. Store what each id has already emitted so AG-UI always
        # receives incremental REASONING_MESSAGE_CONTENT.delta values.
        reasoning_content_state: dict[str, str] = {}

        def emit(agui_event: dict[str, Any], name: str | None = None, *, event_id: str | None = None) -> str:
            return _agui_sse(
                _annotate_event_name(agui_event, name if name is not None else agent_name, subagent_names_by_id),
                event_id=event_id,
            )

        try:
            attach = getattr(manager, "attach_run_stream", None)
            if attach is not None:
                await attach(record.run_id)
                attached = True
            yield emit({"type": "RUN_STARTED", "threadId": thread_id, "runId": run_id, **({"parentRunId": req.parent_run_id} if req.parent_run_id else {})})
            async for entry in manager.stream_bridge.subscribe(record.run_id, last_event_id=last_event_id):
                if entry is HEARTBEAT_SENTINEL:
                    yield emit({"type": "CUSTOM", "name": "deerflow.heartbeat", "value": {}})
                    continue
                if entry is END_SENTINEL:
                    break
                if entry.event not in {
                    "values",
                    "messages-tuple",
                    "custom",
                    "end",
                    "tool_call_chunk",
                    "tool_result_chunk",
                    "task_started",
                    "task_running",
                    "task_completed",
                    "task_limit_reached",
                    "task_failed",
                    "task_cancelled",
                    "task_timed_out",
                    "subagent_started",
                    "token_chunk",
                    "thinking_chunk",
                    "turn_complete",
                }:
                    continue
                event_agent_name = entry.data.get("_agent_name") if isinstance(entry.data, dict) else None
                event = StreamEvent(type=cast(StreamEventType, entry.event), data=entry.data)
                events_for_entry: list[tuple[dict[str, Any], str | None]] = []
                if event.type == "end":
                    for reasoning_event in _close_reasoning_messages(open_reasoning_message_ids):
                        events_for_entry.append((reasoning_event, event_agent_name))
                    for reasoning_event in _close_subagent_reasoning_messages(open_subagent_reasoning_message_ids):
                        events_for_entry.append((reasoning_event, event_agent_name))
                    if open_text_message_id:
                        events_for_entry.append(({"type": "TEXT_MESSAGE_END", "messageId": open_text_message_id}, event_agent_name))
                        open_text_message_id = None
                    for text_event in _close_subagent_text_messages(open_subagent_text_message_ids):
                        events_for_entry.append((text_event, event_agent_name))
                    for tc_id in list(open_tool_call_ids):
                        events_for_entry.append(({"type": "TOOL_CALL_ARGS", "toolCallId": tc_id, "delta": tool_call_args_state.get(tc_id, "{}")}, event_agent_name))
                        events_for_entry.append(({"type": "TOOL_CALL_END", "toolCallId": tc_id}, event_agent_name))
                    open_tool_call_ids.clear()
                for agui_event in _stream_event_to_agui(
                    event,
                    thread_id,
                    run_id,
                    open_text_message_id,
                    tool_call_args_state,
                    open_tool_call_ids,
                    open_reasoning_message_ids,
                    reasoning_content_state,
                    subagent_names_by_id,
                    open_subagent_text_message_ids,
                    open_subagent_reasoning_message_ids,
                ):
                    event_type = agui_event.get("type")
                    if event_type == "MESSAGES_SNAPSHOT":
                        sig = ",".join(m.get("id", "") for m in agui_event.get("messages", []))
                        if sig == last_snapshot_sig:
                            continue
                        last_snapshot_sig = sig
                    elif event_type == "TEXT_MESSAGE_START":
                        message_id = str(agui_event["messageId"])
                        if message_id.startswith("subagent:"):
                            open_subagent_text_message_ids.add(message_id)
                        else:
                            open_text_message_id = message_id
                    elif event_type == "TEXT_MESSAGE_END":
                        message_id = str(agui_event["messageId"])
                        if message_id.startswith("subagent:"):
                            open_subagent_text_message_ids.discard(message_id)
                        else:
                            open_text_message_id = None
                    elif event_type == "REASONING_MESSAGE_START":
                        message_id = str(agui_event["messageId"])
                        if message_id.startswith("subagent:"):
                            open_subagent_reasoning_message_ids.add(message_id)
                        else:
                            open_reasoning_message_ids.add(message_id)
                    elif event_type == "REASONING_MESSAGE_END":
                        message_id = str(agui_event["messageId"])
                        if message_id.startswith("subagent:"):
                            open_subagent_reasoning_message_ids.discard(message_id)
                        else:
                            open_reasoning_message_ids.discard(message_id)
                    events_for_entry.append((agui_event, event_agent_name))
                for index, (agui_event, agui_event_agent_name) in enumerate(events_for_entry):
                    yield emit(agui_event, agui_event_agent_name, event_id=entry.id if index == len(events_for_entry) - 1 else None)
            for tc_id in list(open_tool_call_ids):
                yield emit({"type": "TOOL_CALL_ARGS", "toolCallId": tc_id, "delta": tool_call_args_state.get(tc_id, "{}")})
                yield emit({"type": "TOOL_CALL_END", "toolCallId": tc_id})
            open_tool_call_ids.clear()
            for reasoning_event in _close_reasoning_messages(open_reasoning_message_ids):
                yield emit(reasoning_event)
            for reasoning_event in _close_subagent_reasoning_messages(open_subagent_reasoning_message_ids):
                yield emit(reasoning_event)
            if open_text_message_id:
                yield emit({"type": "TEXT_MESSAGE_END", "messageId": open_text_message_id})
            for text_event in _close_subagent_text_messages(open_subagent_text_message_ids):
                yield emit(text_event)
            final_record = manager.run_manager.get(record.run_id)
            if final_record is not None and final_record.status in {RunStatus.error, RunStatus.timeout, RunStatus.interrupted}:
                yield emit({"type": "RUN_ERROR", "message": final_record.error or final_record.status.value})
            else:
                yield emit({"type": "RUN_FINISHED", "threadId": thread_id, "runId": run_id})
        except asyncio.CancelledError:
            if record.on_disconnect.value == "cancel":
                await manager.cancel_run(record.run_id)
            raise
        except Exception:
            logger.exception("Unhandled error in /chat/agui (thread=%s)", thread_id)
            for tc_id in list(open_tool_call_ids):
                yield emit({"type": "TOOL_CALL_ARGS", "toolCallId": tc_id, "delta": tool_call_args_state.get(tc_id, "{}")})
                yield emit({"type": "TOOL_CALL_END", "toolCallId": tc_id})
            for reasoning_event in _close_reasoning_messages(open_reasoning_message_ids):
                yield emit(reasoning_event)
            for reasoning_event in _close_subagent_reasoning_messages(open_subagent_reasoning_message_ids):
                yield emit(reasoning_event)
            if open_text_message_id:
                yield emit({"type": "TEXT_MESSAGE_END", "messageId": open_text_message_id})
            for text_event in _close_subagent_text_messages(open_subagent_text_message_ids):
                yield emit(text_event)
            yield emit({"type": "RUN_ERROR", "message": "Internal server error"})
        finally:
            if attached:
                detach = getattr(manager, "detach_run_stream", None)
                if detach is not None:
                    await detach(record.run_id)

    return StreamingResponse(event_generator(), media_type="text/event-stream", headers=_SSE_RESPONSE_HEADERS)


def _chat_kwargs_from_agui(req: AguiRunAgentInput) -> dict[str, Any]:
    kwargs: dict[str, Any] = {}
    if req.model_name:
        kwargs["model_name"] = req.model_name
    if req.agent_name:
        kwargs["agent_name"] = req.agent_name
    if req.thinking_enabled is not None:
        kwargs["thinking_enabled"] = req.thinking_enabled
    if req.subagent_enabled is not None:
        kwargs["subagent_enabled"] = req.subagent_enabled
    if req.plan_mode is not None:
        kwargs["plan_mode"] = req.plan_mode
    if req.max_concurrent_subagents is not None:
        kwargs["max_concurrent_subagents"] = req.max_concurrent_subagents
    return kwargs


def _latest_user_message(req: AguiRunAgentInput) -> str | None:
    for message in reversed(req.messages):
        if message.role == "user" and message.content:
            return message.content
    return None


def _agui_sse(event: dict[str, Any], *, event_id: str | None = None) -> str:
    lines: list[str] = []
    if event_id:
        safe_event_id = str(event_id).replace("\r", "").replace("\n", "")
        lines.append(f"id: {safe_event_id}")
    lines.append(f"data: {json.dumps(event, ensure_ascii=False, default=str)}")
    return "\n".join(lines) + "\n\n"


def _close_reasoning_messages(open_reasoning_message_ids: set[str]) -> list[dict[str, Any]]:
    events = [{"type": "REASONING_MESSAGE_END", "messageId": message_id} for message_id in sorted(open_reasoning_message_ids)]
    open_reasoning_message_ids.clear()
    return events


def _close_subagent_text_messages(open_subagent_text_message_ids: set[str]) -> list[dict[str, Any]]:
    events = [{"type": "TEXT_MESSAGE_END", "messageId": message_id} for message_id in sorted(open_subagent_text_message_ids)]
    open_subagent_text_message_ids.clear()
    return events


def _close_subagent_reasoning_messages(open_subagent_reasoning_message_ids: set[str]) -> list[dict[str, Any]]:
    events = [{"type": "REASONING_MESSAGE_END", "messageId": message_id} for message_id in sorted(open_subagent_reasoning_message_ids)]
    open_subagent_reasoning_message_ids.clear()
    return events


def _close_subagent_reasoning_message(open_subagent_reasoning_message_ids: set[str], reasoning_id: str) -> list[dict[str, Any]]:
    if reasoning_id not in open_subagent_reasoning_message_ids:
        return []
    open_subagent_reasoning_message_ids.discard(reasoning_id)
    return [{"type": "REASONING_MESSAGE_END", "messageId": reasoning_id}]


def _close_reasoning_message(open_reasoning_message_ids: set[str], reasoning_id: str) -> list[dict[str, Any]]:
    if reasoning_id not in open_reasoning_message_ids:
        return []
    open_reasoning_message_ids.discard(reasoning_id)
    return [{"type": "REASONING_MESSAGE_END", "messageId": reasoning_id}]


def _reasoning_delta(reasoning_content_state: dict[str, str], reasoning_id: str, reasoning: str) -> str:
    previous = reasoning_content_state.get(reasoning_id, "")
    if previous and reasoning.startswith(previous):
        delta = reasoning[len(previous):]
    else:
        delta = reasoning
    reasoning_content_state[reasoning_id] = reasoning if reasoning.startswith(previous) else previous + delta
    return delta


def _resolve_name_for_event(
    event: dict[str, Any],
    agent_name: str | None,
    subagent_names_by_id: dict[str, str],
) -> str | None:
    for field in ("messageId", "toolCallId"):
        value = event.get(field) or ""
        if isinstance(value, str) and value.startswith("subagent:"):
            parts = value.split(":", 3)
            if len(parts) >= 2:
                subagent_name = subagent_names_by_id.get(parts[1])
                if subagent_name:
                    return subagent_name

    if event.get("type") == "CUSTOM":
        value = event.get("value")
        if isinstance(value, dict):
            task_id = value.get("task_id")
            if task_id:
                subagent_name = subagent_names_by_id.get(str(task_id))
                if subagent_name:
                    return subagent_name

    return agent_name


def _annotate_event_name(
    event: dict[str, Any],
    agent_name: str | None,
    subagent_names_by_id: dict[str, str] | None = None,
) -> dict[str, Any]:
    """Inject agent/subagent name into an AG-UI event.

    Standard AG-UI events get a top-level ``name`` field. All events get
    the name injected into ``rawEvent.name``.
    """
    name = _resolve_name_for_event(event, agent_name, subagent_names_by_id or {})
    if not name:
        return event
    raw_event = {**(event.get("rawEvent") or {}), "name": name}
    return {**event, "rawEvent": raw_event}


def _stream_event_to_agui(
    event: StreamEvent,
    thread_id: str,
    run_id: str,
    open_text_message_id: str | None,
    tool_call_args_state: dict[str, str],
    open_tool_call_ids: set[str],
    open_reasoning_message_ids: set[str] | None = None,
    reasoning_content_state: dict[str, str] | None = None,
    subagent_names_by_id: dict[str, str] | None = None,
    open_subagent_text_message_ids: set[str] | None = None,
    open_subagent_reasoning_message_ids: set[str] | None = None,
) -> list[dict[str, Any]]:
    event_type = str(event.type)
    if event_type == "messages-tuple":
        # worker.py serializes messages-mode as [chunk_dict, metadata_dict].
        # Unwrap to the chunk dict before delegating.
        raw_data: Any = event.data
        if isinstance(raw_data, list) and len(raw_data) >= 1:
            raw_data = raw_data[0]
        data = raw_data
        if not isinstance(data, dict):
            return []
        # Normalize LangChain type names to the wire format used by client.py
        if data.get("type") in ("AIMessageChunk", "AIMessage"):
            data = {**data, "type": "ai"}
        elif data.get("type") in ("HumanMessage", "HumanMessageChunk"):
            data = {**data, "type": "human"}
        elif data.get("type") in ("ToolMessage", "ToolMessageChunk"):
            data = {**data, "type": "tool"}
        return _message_tuple_to_agui(
            data,
            open_text_message_id,
            tool_call_args_state,
            open_tool_call_ids,
            open_reasoning_message_ids if open_reasoning_message_ids is not None else set(),
            reasoning_content_state if reasoning_content_state is not None else {},
        )
    if event_type == "values":
        events: list[dict[str, Any]] = []
        messages = event.data.get("messages")
        if isinstance(messages, list):
            events.append({"type": "MESSAGES_SNAPSHOT", "messages": [_message_to_agui(m) for m in messages if isinstance(m, dict)]})
        artifact_event = _artifacts_to_agui(thread_id, event.data.get("artifacts"))
        if artifact_event is not None:
            events.append(artifact_event)
        return events
    if event_type == "tool_call_chunk":
        return _subagent_tool_call_chunk_to_agui(event.data, tool_call_args_state, open_tool_call_ids, subagent_names_by_id)
    if event_type == "tool_result_chunk":
        return _subagent_tool_result_chunk_to_agui(event.data, tool_call_args_state, open_tool_call_ids)
    if event_type == "custom":
        return [{"type": "CUSTOM", "name": "deerflow.custom", "value": event.data}]
    if event_type == "subagent_started":
        return _subagent_started_to_agui(event.data, subagent_names_by_id if subagent_names_by_id is not None else {})
    if event_type == "token_chunk":
        return _subagent_token_chunk_to_agui(
            event.data,
            subagent_names_by_id if subagent_names_by_id is not None else {},
            open_subagent_text_message_ids if open_subagent_text_message_ids is not None else set(),
            open_subagent_reasoning_message_ids if open_subagent_reasoning_message_ids is not None else set(),
        )
    if event_type == "thinking_chunk":
        return _subagent_thinking_chunk_to_agui(
            event.data,
            open_subagent_reasoning_message_ids if open_subagent_reasoning_message_ids is not None else set(),
            subagent_names_by_id,
        )
    if event_type in {
        "task_completed",
        "task_limit_reached",
        "task_failed",
        "task_cancelled",
        "task_timed_out",
    }:
        return _subagent_terminal_event_to_agui(
            event_type,
            event.data,
            open_subagent_text_message_ids if open_subagent_text_message_ids is not None else set(),
            open_subagent_reasoning_message_ids if open_subagent_reasoning_message_ids is not None else set(),
            subagent_names_by_id if subagent_names_by_id is not None else {},
        )
    if event_type in {
        "task_started",
        "task_running",
        "turn_complete",
    }:
        return [{"type": "CUSTOM", "name": f"deerflow.subagent.{event_type}", "value": {"eventType": event_type, **event.data}}]
    if event_type == "end":
        return [{"type": "CUSTOM", "name": "deerflow.usage", "value": event.data.get("usage", {})}]
    return [{"type": "RAW", "source": "deerflow", "event": {"threadId": thread_id, "runId": run_id, "type": event_type, "data": event.data}}]


def _message_tuple_to_agui(
    data: dict[str, Any],
    open_text_message_id: str | None,
    tool_call_args_state: dict[str, str],
    open_tool_call_ids: set[str],
    open_reasoning_message_ids: set[str],
    reasoning_content_state: dict[str, str],
) -> list[dict[str, Any]]:
    message_type = data.get("type")
    if message_type == "ai":
        tool_calls = data.get("tool_calls")
        if isinstance(tool_calls, list) and tool_calls:
            events: list[dict[str, Any]] = []
            message_id = str(data.get("id") or uuid.uuid4())
            events.extend(_close_reasoning_message(open_reasoning_message_ids, f"reasoning_{message_id}"))
            if open_text_message_id:
                events.append({"type": "TEXT_MESSAGE_END", "messageId": open_text_message_id})
            events.extend(_tool_calls_to_agui(tool_calls, message_id, tool_call_args_state, open_tool_call_ids))
            return events

        content = data.get("content")
        reasoning = data.get("reasoning_content")
        message_id = str(data.get("id") or uuid.uuid4())
        reasoning_id = f"reasoning_{message_id}"
        events: list[dict[str, Any]] = []

        # --- Reasoning block ---
        if isinstance(reasoning, str) and reasoning:
            reasoning_delta = _reasoning_delta(reasoning_content_state, reasoning_id, reasoning)
            if reasoning_id not in open_reasoning_message_ids:
                open_reasoning_message_ids.add(reasoning_id)
                events.append({"type": "REASONING_MESSAGE_START", "messageId": reasoning_id, "role": "reasoning"})
            if reasoning_delta:
                events.append({"type": "REASONING_MESSAGE_CONTENT", "messageId": reasoning_id, "delta": reasoning_delta})
            # If this chunk is reasoning-only (no content), return now.
            # The caller will track open_reasoning_message_ids and send END later.
            if not (isinstance(content, str) and content):
                return events

        # Close the reasoning block for this message if we are about to emit its content.
        if isinstance(content, str) and content:
            events.extend(_close_reasoning_message(open_reasoning_message_ids, reasoning_id))

        # --- Text content block ---
        if isinstance(content, str) and content:
            if open_text_message_id != message_id:
                if open_text_message_id:
                    events.append({"type": "TEXT_MESSAGE_END", "messageId": open_text_message_id})
                events.append({"type": "TEXT_MESSAGE_START", "messageId": message_id, "role": "assistant"})
            events.append({"type": "TEXT_MESSAGE_CONTENT", "messageId": message_id, "delta": content})
            return events

        return events

    if message_type == "tool":
        tool_call_id = data.get("tool_call_id")
        if isinstance(tool_call_id, str) and tool_call_id:
            events = []
            if tool_call_id in open_tool_call_ids:
                events.append({"type": "TOOL_CALL_ARGS", "toolCallId": tool_call_id, "delta": tool_call_args_state.get(tool_call_id, "{}")})
                events.append({"type": "TOOL_CALL_END", "toolCallId": tool_call_id})
                open_tool_call_ids.discard(tool_call_id)
            events.append({
                "type": "TOOL_CALL_RESULT",
                "messageId": str(data.get("id") or uuid.uuid4()),
                "toolCallId": tool_call_id,
                "content": str(data.get("content") or ""),
                "role": "tool",
            })
            return events
    return []


def _artifacts_to_agui(thread_id: str, artifacts: Any) -> dict[str, Any] | None:
    if not isinstance(artifacts, list):
        return None

    items: list[dict[str, Any]] = []
    for raw in artifacts:
        if not isinstance(raw, str):
            continue
        path = raw.strip()
        if not path.startswith("/mnt/user-data/outputs/"):
            continue
        name = PurePosixPath(path).name or "artifact"
        mime_type, _ = mimetypes.guess_type(name)
        encoded_path = quote(path.lstrip("/"), safe="/")
        items.append(
            {
                "path": path,
                "name": name,
                "url": f"/api/threads/{thread_id}/artifacts/{encoded_path}",
                "mimeType": mime_type,
                "kind": "image" if mime_type and mime_type.startswith("image/") else "file",
            }
        )

    if not items:
        return None
    return {
        "type": "CUSTOM",
        "name": "deerflow.artifacts",
        "value": {
            "threadId": thread_id,
            "artifacts": items,
        },
    }


def _subagent_started_to_agui(data: dict[str, Any], subagent_names_by_id: dict[str, str]) -> list[dict[str, Any]]:
    task_id = str(data.get("task_id") or "")
    name = data.get("name")
    if task_id and isinstance(name, str) and name:
        subagent_names_by_id[task_id] = name
    return [{"type": "CUSTOM", "name": "deerflow.subagent.subagent_started", "value": {"eventType": "subagent_started", **data}}]


def _subagent_text_message_id(task_id: str) -> str:
    return f"subagent:{task_id}:message"


def _subagent_reasoning_message_id(task_id: str) -> str:
    return f"subagent:{task_id}:reasoning"


def _subagent_token_chunk_to_agui(
    data: dict[str, Any],
    subagent_names_by_id: dict[str, str],
    open_subagent_text_message_ids: set[str],
    open_subagent_reasoning_message_ids: set[str],
) -> list[dict[str, Any]]:
    task_id = str(data.get("task_id") or "")
    content = data.get("content")
    events = [{"type": "CUSTOM", "name": "deerflow.subagent.token_chunk", "value": {"eventType": "token_chunk", **data}}]
    if not task_id or not isinstance(content, str) or not content:
        return events

    events.extend(_close_subagent_reasoning_message(open_subagent_reasoning_message_ids, _subagent_reasoning_message_id(task_id)))
    message_id = _subagent_text_message_id(task_id)
    if message_id not in open_subagent_text_message_ids:
        open_subagent_text_message_ids.add(message_id)
        events.append({"type": "TEXT_MESSAGE_START", "messageId": message_id, "role": "assistant"})
    events.append({"type": "TEXT_MESSAGE_CONTENT", "messageId": message_id, "delta": content})
    return events


def _subagent_thinking_chunk_to_agui(
    data: dict[str, Any],
    open_subagent_reasoning_message_ids: set[str],
    subagent_names_by_id: dict[str, str] | None = None,
) -> list[dict[str, Any]]:
    task_id = str(data.get("task_id") or "")
    thinking = data.get("thinking")
    events = [{"type": "CUSTOM", "name": "deerflow.subagent.thinking_chunk", "value": {"eventType": "thinking_chunk", **data}}]
    if not task_id or not isinstance(thinking, str) or not thinking:
        return events

    message_id = _subagent_reasoning_message_id(task_id)
    if message_id not in open_subagent_reasoning_message_ids:
        open_subagent_reasoning_message_ids.add(message_id)
        events.append({"type": "REASONING_MESSAGE_START", "messageId": message_id, "role": "reasoning"})
    events.append({"type": "REASONING_MESSAGE_CONTENT", "messageId": message_id, "delta": thinking})
    return events


def _subagent_terminal_event_to_agui(
    event_type: str,
    data: dict[str, Any],
    open_subagent_text_message_ids: set[str],
    open_subagent_reasoning_message_ids: set[str],
    subagent_names_by_id: dict[str, str],
) -> list[dict[str, Any]]:
    events: list[dict[str, Any]] = []
    task_id = str(data.get("task_id") or "")
    if task_id:
        reasoning_message_id = _subagent_reasoning_message_id(task_id)
        if reasoning_message_id in open_subagent_reasoning_message_ids:
            events.append({"type": "REASONING_MESSAGE_END", "messageId": reasoning_message_id})
        message_id = _subagent_text_message_id(task_id)
        if message_id in open_subagent_text_message_ids:
            events.append({"type": "TEXT_MESSAGE_END", "messageId": message_id})
    events.append({"type": "CUSTOM", "name": f"deerflow.subagent.{event_type}", "value": {"eventType": event_type, **data}})
    return events


def _tool_calls_to_agui(
    tool_calls: list[Any],
    parent_message_id: str,
    tool_call_args_state: dict[str, str],
    open_tool_call_ids: set[str],
) -> list[dict[str, Any]]:
    events: list[dict[str, Any]] = []
    for tool_call in tool_calls:
        if not isinstance(tool_call, dict):
            continue
        tool_call_id = str(tool_call.get("id") or uuid.uuid4())
        args = tool_call.get("args")
        full_json = json.dumps(args if isinstance(args, dict) else {}, ensure_ascii=False, default=str)

        if tool_call_id not in tool_call_args_state:
            # First chunk — emit START and record state. TOOL_CALL_ARGS is intentionally
            # deferred: intermediate chunks serialize partial dicts to closed JSON objects
            # (e.g. "{}") that are not valid prefixes of the final JSON, so incremental
            # deltas would produce invalid output. The complete args are sent once, just
            # before TOOL_CALL_END, when the tool result or stream-end is reached.
            tool_call_name = str(tool_call.get("name") or "tool")
            tool_call_args_state[tool_call_id] = full_json
            open_tool_call_ids.add(tool_call_id)
            events.append({"type": "TOOL_CALL_START", "toolCallId": tool_call_id, "toolCallName": tool_call_name, "parentMessageId": parent_message_id})
        else:
            # Subsequent chunk — keep the latest (most complete) serialized args.
            tool_call_args_state[tool_call_id] = full_json
    return events


def _subagent_tool_call_chunk_to_agui(
    data: dict[str, Any],
    tool_call_args_state: dict[str, str],
    open_tool_call_ids: set[str],
    subagent_names_by_id: dict[str, str] | None = None,
) -> list[dict[str, Any]]:
    tool_call = data.get("tool_call")
    if not isinstance(tool_call, dict):
        return []
    task_id = str(data.get("task_id") or "subagent")
    parent_message_id = f"subagent:{task_id}"
    tool_name = str(tool_call.get("name") or "tool")
    namespaced_tool_call = {**tool_call, "id": _subagent_tool_call_id(task_id, tool_call.get("id"), fallback_hint=tool_name)}
    return _tool_calls_to_agui([namespaced_tool_call], parent_message_id, tool_call_args_state, open_tool_call_ids)


def _subagent_tool_result_chunk_to_agui(
    data: dict[str, Any],
    tool_call_args_state: dict[str, str],
    open_tool_call_ids: set[str],
) -> list[dict[str, Any]]:
    task_id = str(data.get("task_id") or "subagent")
    tool_name = str(data.get("name") or "tool")
    tool_call_id = _subagent_tool_call_id(task_id, data.get("tool_call_id"), fallback_hint=tool_name)
    events: list[dict[str, Any]] = []
    if tool_call_id in open_tool_call_ids:
        events.append({"type": "TOOL_CALL_ARGS", "toolCallId": tool_call_id, "delta": tool_call_args_state.get(tool_call_id, "{}")})
        events.append({"type": "TOOL_CALL_END", "toolCallId": tool_call_id})
        open_tool_call_ids.discard(tool_call_id)
    events.append(
        {
            "type": "TOOL_CALL_RESULT",
            "messageId": f"subagent:{task_id}:tool-result:{tool_call_id}",
            "toolCallId": tool_call_id,
            "content": str(data.get("content") or ""),
            "role": "tool",
        }
    )
    return events


def _subagent_tool_call_id(task_id: str, raw_tool_call_id: Any, *, fallback_hint: str = "tool") -> str:
    raw_id = str(raw_tool_call_id or fallback_hint)
    if raw_id.startswith(f"subagent:{task_id}:"):
        return raw_id
    return f"subagent:{task_id}:{raw_id}"


def _message_to_agui(message: dict[str, Any]) -> dict[str, Any]:
    role = str(message.get("type") or "assistant")
    if role == "ai":
        role = "assistant"
    result: dict[str, Any] = {
        "id": str(message.get("id") or uuid.uuid4()),
        "role": role,
        "content": str(message.get("content") or ""),
    }
    reasoning = message.get("reasoning_content")
    if reasoning:
        result["reasoning_content"] = str(reasoning)
    tool_calls = message.get("tool_calls")
    if isinstance(tool_calls, list) and tool_calls:
        result["toolCalls"] = [_tool_call_to_agui_message(tc) for tc in tool_calls if isinstance(tc, dict)]
    if tool_call_id := message.get("tool_call_id"):
        result["toolCallId"] = str(tool_call_id)
    if name := message.get("name"):
        result["name"] = str(name)
    return result


def _tool_call_to_agui_message(tool_call: dict[str, Any]) -> dict[str, Any]:
    return {
        "id": str(tool_call.get("id") or uuid.uuid4()),
        "type": "function",
        "function": {
            "name": str(tool_call.get("name") or "tool"),
            "arguments": json.dumps(tool_call.get("args") if isinstance(tool_call.get("args"), dict) else {}, ensure_ascii=False, default=str),
        },
    }
