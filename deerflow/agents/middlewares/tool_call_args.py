"""Rewrite tool-call arguments consistently on every provider representation.

The graph checkpoint remains untouched.  Callers use these helpers only on a
``ModelRequest`` copy, keeping structured LangChain calls, raw provider calls,
and provider-native content blocks consistent with one another.
"""

from __future__ import annotations

import json
from collections import Counter, defaultdict, deque
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, replace
from typing import Any

from langchain_core.messages import AIMessage, ToolMessage

ArgsReplacements = Mapping[str, dict[str, Any]]
ReplacementSelector = Callable[
    [AIMessage, dict[str, Any]],
    dict[str, Any] | None,
]


def rewrite_messages_tool_call_args(
    messages: list[Any],
    replacement_for: ReplacementSelector,
) -> list[Any] | None:
    """Return a rewritten message list, or ``None`` when nothing matched."""
    updated: list[Any] = []
    changed = False
    for message in messages:
        patched = message
        if isinstance(message, AIMessage) and message.tool_calls:
            replacements: dict[str, dict[str, Any]] = {}
            duplicated = _duplicated_call_ids(message.tool_calls)
            ambiguous_idless_names = _ambiguous_idless_names(message)
            for tool_call in message.tool_calls:
                if not isinstance(tool_call, dict):
                    continue
                call_id = tool_call.get("id")
                if (
                    not isinstance(call_id, str)
                    or not call_id
                    or call_id in duplicated
                    or tool_call.get("name") in ambiguous_idless_names
                ):
                    continue
                replacement = replacement_for(message, tool_call)
                if replacement is not None:
                    replacements[call_id] = replacement
            if replacements:
                patched = rewrite_tool_call_args(message, replacements)
        changed = changed or patched is not message
        updated.append(patched)
    if not changed:
        return None
    # Rewritten history cannot safely chain to a provider-side copy that still
    # contains the original payload.
    return [_without_response_chain_id(message) for message in updated]


def _duplicated_call_ids(tool_calls: Sequence[Any]) -> set[str]:
    counts = Counter(
        call_id
        for tool_call in tool_calls
        if isinstance(tool_call, dict)
        and isinstance(call_id := tool_call.get("id"), str)
        and call_id
    )
    return {call_id for call_id, count in counts.items() if count > 1}


def _ambiguous_idless_names(message: AIMessage) -> set[str]:
    """Names whose ID-less provider surface cannot be paired unambiguously."""
    name_counts = Counter(
        name
        for call in message.tool_calls or ()
        if isinstance(call, dict)
        and isinstance(name := call.get("name"), str)
        and name
    )
    duplicated_names = {name for name, count in name_counts.items() if count > 1}
    if not duplicated_names:
        return set()
    structured_ids = {
        call_id
        for call in message.tool_calls or ()
        if isinstance(call, dict)
        and isinstance(call_id := call.get("id"), str)
        and call_id
    }
    idless_names: set[str] = set()

    def record(entry: Any, *id_keys: str, nested_name: str | None = None) -> None:
        if not isinstance(entry, dict):
            return
        for key in id_keys:
            identifier = entry.get(key)
            if isinstance(identifier, str) and identifier:
                # Provider surfaces have an ordered ID contract (for example,
                # Responses call_id takes precedence over the item id).  An
                # ID-bearing block must never fall back to its name merely
                # because that identifier is not one of our structured calls.
                return
        name = nested_name if nested_name is not None else entry.get("name")
        if isinstance(name, str) and name in duplicated_names:
            idless_names.add(name)

    for block in message.content if isinstance(message.content, list) else ():
        if not isinstance(block, dict):
            continue
        block_type = block.get("type")
        if block_type == "function_call":
            record(block, "call_id", "id")
        elif block_type == "custom_tool_call":
            # Responses item ids (ctc_*) identify the content item, not the
            # structured tool call.  Only call_id may be used for pairing.
            record(block, "call_id")
        elif block_type in {"tool_use", "tool_call", "tool_call_chunk"}:
            record(block, "id")

    additional = message.additional_kwargs or {}
    record(additional.get("function_call"))
    raw_calls = additional.get("tool_calls")
    if isinstance(raw_calls, list):
        for raw in raw_calls:
            function = raw.get("function") if isinstance(raw, dict) else None
            nested_name = (
                function.get("name") if isinstance(function, dict) else None
            )
            record(raw, "id", "call_id", nested_name=nested_name)
    return idless_names


@dataclass(frozen=True, slots=True)
class ToolCallOccurrence:
    index: int
    message: AIMessage
    tool_call: dict[str, Any]
    result: ToolMessage | None

    @property
    def call_id(self) -> str:
        return self.tool_call["id"]

    @property
    def name(self) -> str:
        name = self.tool_call.get("name")
        return name if isinstance(name, str) else ""

    @property
    def args(self) -> dict[str, Any]:
        args = self.tool_call.get("args")
        return args if isinstance(args, dict) else {}


def pair_tool_call_results(messages: Sequence[Any]) -> list[ToolCallOccurrence]:
    """Pair calls with results per AI-message occurrence, not globally by ID."""
    occurrences: list[ToolCallOccurrence] = []
    open_calls: dict[str, deque[int]] = defaultdict(deque)
    for index, message in enumerate(messages):
        if isinstance(message, AIMessage):
            open_calls = defaultdict(deque)
            for tool_call in message.tool_calls or ():
                if not isinstance(tool_call, dict):
                    continue
                call_id = tool_call.get("id")
                if not isinstance(call_id, str) or not call_id:
                    continue
                open_calls[call_id].append(len(occurrences))
                occurrences.append(
                    ToolCallOccurrence(index, message, tool_call, None)
                )
        elif isinstance(message, ToolMessage):
            queue = (
                open_calls.get(message.tool_call_id)
                if isinstance(message.tool_call_id, str)
                else None
            )
            if queue:
                position = queue.popleft()
                occurrences[position] = replace(
                    occurrences[position],
                    result=message,
                )
    return occurrences


def _without_response_chain_id(message: Any) -> Any:
    if not isinstance(message, AIMessage):
        return message
    metadata = message.response_metadata or {}
    response_id = metadata.get("id")
    if not (
        isinstance(response_id, str)
        and response_id.startswith("resp_")
    ):
        return message
    return message.model_copy(
        update={
            "response_metadata": {
                key: value for key, value in metadata.items() if key != "id"
            }
        }
    )


def rewrite_tool_call_args(
    message: AIMessage,
    replacements: ArgsReplacements,
) -> AIMessage:
    """Clone ``message`` with replacement args synchronized across surfaces."""
    if not replacements:
        return message
    update: dict[str, Any] = {}
    tool_calls = message.tool_calls or []
    name_replacements = _unambiguous_name_replacements(tool_calls, replacements)

    rewritten_calls = [
        dict(tool_call, args=new_args)
        if isinstance(tool_call, dict)
        and (
            new_args := _replacement_for_id(tool_call.get("id"), replacements)
        )
        is not None
        else tool_call
        for tool_call in tool_calls
    ]
    if _any_replaced(rewritten_calls, tool_calls):
        update["tool_calls"] = rewritten_calls

    chunks = getattr(message, "tool_call_chunks", None)
    if isinstance(chunks, list):
        rewritten_chunks = [
            dict(chunk, args=_serialize(new_args))
            if isinstance(chunk, dict)
            and (new_args := _replacement_for_id(chunk.get("id"), replacements))
            is not None
            else chunk
            for chunk in chunks
        ]
        if _any_replaced(rewritten_chunks, chunks):
            update["tool_call_chunks"] = rewritten_chunks

    additional_kwargs = message.additional_kwargs or {}
    raw_calls = additional_kwargs.get("tool_calls")
    if isinstance(raw_calls, list):
        rewritten_raw = [
            _rewrite_raw_tool_call(entry, replacements, name_replacements)
            for entry in raw_calls
        ]
        if _any_replaced(rewritten_raw, raw_calls):
            update["additional_kwargs"] = {
                **additional_kwargs,
                "tool_calls": rewritten_raw,
            }
    raw_function_call = additional_kwargs.get("function_call")
    if isinstance(raw_function_call, dict):
        rewritten_function_call = _rewrite_raw_tool_call(
            raw_function_call,
            replacements,
            name_replacements,
        )
        if rewritten_function_call is not raw_function_call:
            base = update.get("additional_kwargs", additional_kwargs)
            update["additional_kwargs"] = {
                **base,
                "function_call": rewritten_function_call,
            }

    if isinstance(message.content, list):
        rewritten_content = [
            _rewrite_content_block(block, replacements, name_replacements)
            for block in message.content
        ]
        if _any_replaced(rewritten_content, message.content):
            update["content"] = rewritten_content

    return message.model_copy(update=update) if update else message


def _unambiguous_name_replacements(
    tool_calls: Sequence[Any],
    replacements: ArgsReplacements,
) -> dict[str, dict[str, Any]]:
    """Map unique call names for provider blocks that omit tool-call IDs."""
    name_counts = Counter(
        name
        for call in tool_calls
        if isinstance(call, dict)
        and isinstance(name := call.get("name"), str)
        and name
    )
    by_name: dict[str, dict[str, Any]] = {}
    for call in tool_calls:
        if not isinstance(call, dict):
            continue
        name = call.get("name")
        replacement = _replacement_for_id(call.get("id"), replacements)
        if (
            isinstance(name, str)
            and name_counts[name] == 1
            and replacement is not None
        ):
            by_name[name] = replacement
    return by_name


def _replacement_for_id(
    identifier: Any,
    replacements: ArgsReplacements,
) -> dict[str, Any] | None:
    return replacements.get(identifier) if isinstance(identifier, str) else None


def _replacement_for_block(
    block: dict[str, Any],
    replacements: ArgsReplacements,
    name_replacements: Mapping[str, dict[str, Any]],
    *id_keys: str,
) -> dict[str, Any] | None:
    for key in id_keys:
        identifier = block.get(key)
        if isinstance(identifier, str) and identifier:
            # The first populated provider ID is authoritative.  Do not try a
            # lower-priority item ID when the call ID exists but is not being
            # rewritten; doing so can pair this block with another call.
            return _replacement_for_id(identifier, replacements)
    name = block.get("name")
    return name_replacements.get(name) if isinstance(name, str) else None


def _any_replaced(rewritten: Sequence[Any], original: Sequence[Any]) -> bool:
    return any(
        new is not old
        for new, old in zip(rewritten, original, strict=True)
    )


def _serialize(args: dict[str, Any]) -> str:
    return json.dumps(args, ensure_ascii=False)


def _rewrite_raw_tool_call(
    entry: Any,
    replacements: ArgsReplacements,
    name_replacements: Mapping[str, dict[str, Any]],
) -> Any:
    if not isinstance(entry, dict):
        return entry
    function = entry.get("function")
    function_name = function.get("name") if isinstance(function, dict) else None
    has_authoritative_id = any(
        isinstance(entry.get(key), str) and entry.get(key)
        for key in ("id", "call_id")
    )
    new_args = _replacement_for_block(
        entry,
        replacements,
        name_replacements,
        "id",
        "call_id",
    )
    if (
        new_args is None
        and not has_authoritative_id
        and isinstance(function_name, str)
    ):
        new_args = name_replacements.get(function_name)
    if new_args is None:
        return entry
    if isinstance(function, dict):
        return {
            **entry,
            "function": {**function, "arguments": _serialize(new_args)},
        }
    if isinstance(entry.get("arguments"), str):
        return {**entry, "arguments": _serialize(new_args)}
    if isinstance(entry.get("args"), dict):
        return {**entry, "args": new_args}
    if isinstance(entry.get("input"), str):
        return {**entry, "input": _serialize(new_args)}
    return entry


def _rewrite_content_block(
    block: Any,
    replacements: ArgsReplacements,
    name_replacements: Mapping[str, dict[str, Any]],
) -> Any:
    if not isinstance(block, dict):
        return block
    block_type = block.get("type")
    if block_type == "tool_use":
        new_args = _replacement_for_block(
            block,
            replacements,
            name_replacements,
            "id",
        )
        if new_args is None:
            return block
        rewritten = {
            key: value
            for key, value in block.items()
            if key != "partial_json"
        }
        rewritten["input"] = new_args
        return rewritten
    if block_type == "function_call":
        new_args = _replacement_for_block(
            block,
            replacements,
            name_replacements,
            "call_id",
            "id",
        )
        if new_args is None:
            return block
        if "args" in block:
            return {**block, "args": new_args}
        return {**block, "arguments": _serialize(new_args)}
    if block_type == "custom_tool_call":
        new_args = _replacement_for_block(
            block,
            replacements,
            name_replacements,
            "call_id",
        )
        if new_args is None:
            return block
        if "input" in block:
            custom_input = new_args.get("__arg1")
            return {
                **block,
                "input": (
                    custom_input
                    if isinstance(custom_input, str)
                    else _serialize(new_args)
                ),
            }
        return {**block, "arguments": _serialize(new_args)}
    if block_type in ("tool_call", "tool_call_chunk"):
        new_args = _replacement_for_block(
            block,
            replacements,
            name_replacements,
            "id",
        )
        if new_args is None:
            return block
        rewritten = {
            **block,
            "args": (
                new_args
                if block_type == "tool_call"
                else _serialize(new_args)
            ),
        }
        extras = block.get("extras")
        if isinstance(extras, dict) and "arguments" in extras:
            rewritten["extras"] = {
                **extras,
                "arguments": _serialize(new_args),
            }
        return rewritten
    return block
