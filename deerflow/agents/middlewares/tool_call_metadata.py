"""Helpers for keeping every AIMessage tool-call representation in sync."""

from __future__ import annotations

from collections import Counter
from typing import Any

from langchain_core.messages import AIMessage


_CONTENT_TOOL_CALL_ID_KEYS: dict[str, tuple[str, ...]] = {
    "tool_use": ("id",),
    "function_call": ("call_id", "id"),
    "custom_tool_call": ("call_id",),
    "tool_call": ("id",),
    "tool_call_chunk": ("id",),
}


def _raw_tool_call_id(raw_tool_call: Any) -> str | None:
    if not isinstance(raw_tool_call, dict):
        return None
    raw_id = raw_tool_call.get("id")
    return raw_id if isinstance(raw_id, str) and raw_id else None


def _content_block_call_id(
    block: dict[str, Any],
    id_keys: tuple[str, ...],
) -> str | None:
    for key in id_keys:
        value = block.get(key)
        if isinstance(value, str) and value:
            return value
    return None


def _sync_content_tool_call_blocks(
    content: Any,
    retained_calls: list[dict[str, Any]],
) -> Any:
    """Remove provider-native call blocks that no longer have a retained call."""
    if not isinstance(content, list):
        return content

    retained_ids = {
        call["id"]
        for call in retained_calls
        if isinstance(call.get("id"), str) and call["id"]
    }
    entries: list[tuple[Any, tuple[str, ...] | None, str | None]] = []
    for block in content:
        block_type = block.get("type") if isinstance(block, dict) else None
        id_keys = (
            _CONTENT_TOOL_CALL_ID_KEYS.get(block_type)
            if isinstance(block_type, str)
            else None
        )
        call_id = (
            _content_block_call_id(block, id_keys)
            if isinstance(block, dict) and id_keys is not None
            else None
        )
        entries.append((block, id_keys, call_id))

    matched_ids = {
        call_id
        for _, id_keys, call_id in entries
        if id_keys is not None and call_id in retained_ids
    }
    idless_budget = Counter(
        call["name"]
        for call in retained_calls
        if isinstance(call.get("name"), str)
        and not (
            isinstance(call.get("id"), str)
            and call["id"] in matched_ids
        )
    )

    synced: list[Any] = []
    for block, id_keys, call_id in entries:
        if id_keys is None:
            synced.append(block)
        elif call_id is not None:
            if call_id in retained_ids:
                synced.append(block)
        elif isinstance(block, dict):
            name = block.get("name")
            if isinstance(name, str) and idless_budget[name] > 0:
                idless_budget[name] -= 1
                synced.append(block)

    return content if len(synced) == len(content) else synced


def clone_ai_message_with_tool_calls(
    message: AIMessage,
    tool_calls: list[dict[str, Any]],
    *,
    content: Any | None = None,
) -> AIMessage:
    """Clone an AIMessage and synchronize structured, raw and content calls."""
    kept_ids = {
        call["id"]
        for call in tool_calls
        if isinstance(call.get("id"), str) and call["id"]
    }
    invalid_tool_calls = [
        call
        for call in (getattr(message, "invalid_tool_calls", None) or [])
        if isinstance(call, dict)
    ]
    source_content = message.content if content is None else content
    synced_content = _sync_content_tool_call_blocks(
        source_content,
        [*tool_calls, *invalid_tool_calls],
    )

    update: dict[str, Any] = {"tool_calls": tool_calls}
    if content is not None or synced_content is not message.content:
        update["content"] = synced_content

    additional_kwargs = dict(getattr(message, "additional_kwargs", {}) or {})
    raw_tool_calls = additional_kwargs.get("tool_calls")
    if isinstance(raw_tool_calls, list):
        retained_raw = [
            raw
            for raw in raw_tool_calls
            if _raw_tool_call_id(raw) in kept_ids
        ]
        if retained_raw:
            additional_kwargs["tool_calls"] = retained_raw
        else:
            additional_kwargs.pop("tool_calls", None)
    if not tool_calls:
        additional_kwargs.pop("function_call", None)
    update["additional_kwargs"] = additional_kwargs

    response_metadata = dict(getattr(message, "response_metadata", {}) or {})
    if not tool_calls and response_metadata.get("finish_reason") == "tool_calls":
        response_metadata["finish_reason"] = "stop"
    update["response_metadata"] = response_metadata

    return message.model_copy(update=update)
