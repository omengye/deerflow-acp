"""Middleware that enforces a per-result budget on tool outputs."""

from __future__ import annotations

import asyncio
import logging
import os
import posixpath
import shlex
import uuid
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import replace as dc_replace
from typing import TYPE_CHECKING, Any, override

from langchain.agents import AgentState
from langchain.agents.middleware import AgentMiddleware
from langchain.agents.middleware.types import (
    ModelCallResult,
    ModelRequest,
    ModelResponse,
)
from langchain_core.messages import AIMessage, ToolMessage
from langgraph.prebuilt.tool_node import ToolCallRequest
from langgraph.types import Command

from deerflow.agents.middlewares.tool_output_synopsis import build_synopsis
from deerflow.agents.middlewares.tool_call_args import (
    ToolCallOccurrence,
    pair_tool_call_results,
    rewrite_messages_tool_call_args,
)
from deerflow.config.tool_output_config import ToolOutputConfig
from deerflow.sandbox.sandbox_provider import get_sandbox_provider

if TYPE_CHECKING:
    from deerflow.sandbox.sandbox import Sandbox

logger = logging.getLogger(__name__)

_VIRTUAL_OUTPUTS_BASE = "/mnt/user-data/outputs"
_EXT_MAP: dict[str, str] = {
    "bash": "log",
    "bash_tool": "log",
    "web_fetch": "log",
}


def _default_config() -> ToolOutputConfig:
    return ToolOutputConfig()


def _message_text(content: Any) -> str | None:
    if isinstance(content, str):
        return content
    if content is None:
        return None
    if isinstance(content, list):
        pieces: list[str] = []
        for part in content:
            if isinstance(part, str):
                pieces.append(part)
            elif isinstance(part, dict) and isinstance(part.get("text"), str):
                pieces.append(part["text"])
            else:
                return None
        return "\n".join(pieces) if pieces else None
    return None


def _snap_to_line_boundary(text: str, pos: int) -> int:
    if pos <= 0 or pos >= len(text):
        return pos
    half = pos // 2
    nl = text.rfind("\n", half, pos)
    return nl + 1 if nl >= 0 else pos


def _sanitize_tool_name(name: str) -> str:
    base = os.path.basename(name)
    safe = base.replace("..", "").replace("/", "_").replace("\\", "_")
    return safe or "unknown"


def _build_externalized_filename(*, tool_name: str, tool_call_id: str) -> str:
    safe_name = _sanitize_tool_name(tool_name)
    ext = _EXT_MAP.get(tool_name, "txt")
    short_id = uuid.uuid4().hex[:12]
    return f"{safe_name}-{short_id}.{ext}"


def _externalize(
    content: str,
    *,
    tool_name: str,
    tool_call_id: str,
    outputs_path: str,
    storage_subdir: str,
) -> str | None:
    if os.path.isabs(storage_subdir) or ".." in storage_subdir:
        return None
    storage_dir = os.path.join(outputs_path, storage_subdir)
    try:
        os.makedirs(storage_dir, exist_ok=True)
    except OSError:
        return None

    filename = _build_externalized_filename(tool_name=tool_name, tool_call_id=tool_call_id)
    filepath = os.path.join(storage_dir, filename)
    if not os.path.abspath(filepath).startswith(os.path.abspath(storage_dir)):
        return None

    try:
        with open(filepath, "w", encoding="utf-8") as f:
            f.write(content)
    except OSError:
        return None

    return f"{_VIRTUAL_OUTPUTS_BASE}/{storage_subdir}/{filename}"


def _externalize_to_sandbox(
    content: str,
    *,
    tool_name: str,
    tool_call_id: str,
    storage_subdir: str,
    sandbox: Sandbox,
) -> str | None:
    if os.path.isabs(storage_subdir) or ".." in storage_subdir:
        return None
    filename = _build_externalized_filename(tool_name=tool_name, tool_call_id=tool_call_id)
    virtual_dir = f"{_VIRTUAL_OUTPUTS_BASE}/{storage_subdir}"
    virtual_path = f"{virtual_dir}/{filename}"
    try:
        sandbox.execute_command(f"mkdir -p {shlex.quote(virtual_dir)}")
        sandbox.write_file(virtual_path, content)
        check = sandbox.execute_command(f"test -s {shlex.quote(virtual_path)} && echo OK || echo MISSING")
        if not isinstance(check, str) or check.strip() != "OK":
            logger.warning("Sandbox externalize validation failed: path=%s, check=%r", virtual_path, check)
            return None
    except Exception:
        logger.exception("Failed to externalize %s output to sandbox (call_id=%s)", tool_name, tool_call_id)
        return None
    return virtual_path


def _build_preview(
    content: str,
    *,
    tool_name: str,
    virtual_path: str,
    head_chars: int,
    tail_chars: int,
    synopsis_enabled: bool = False,
    synopsis_max_chars: int = 0,
) -> str:
    total = len(content)

    synopsis = build_synopsis(content, tool_name=tool_name, max_chars=synopsis_max_chars) if synopsis_enabled and synopsis_max_chars > 0 else None
    if synopsis is not None:
        # Structured payloads (JSON/JSON Lines) get a schema-level synopsis
        # instead of a raw head/tail slice: half of a JSON document is
        # usually unparseable syntax that tells the model nothing about the
        # data's shape, whereas the synopsis conveys keys/types/lengths
        # directly. The synopsis therefore replaces head/tail rather than
        # being appended alongside them.
        ref = f"\n\n[Full {tool_name} output saved to {virtual_path} ({total} chars, ~{total // 4} tokens). Use read_file with start_line and end_line to access specific sections. Structured synopsis shown above in place of a raw preview.]\n\n"
        return f"{synopsis}{ref}"

    head_end = _snap_to_line_boundary(content, min(head_chars, total))
    tail_start = max(head_end, total - tail_chars)
    tail_start_snapped = _snap_to_line_boundary(content, tail_start)
    if tail_start_snapped > head_end:
        tail_start = tail_start_snapped

    head = content[:head_end]
    tail = content[tail_start:] if tail_start < total else ""
    omitted = total - len(head) - len(tail)
    ref = f"\n\n[Full {tool_name} output saved to {virtual_path} ({total} chars, ~{total // 4} tokens). Use read_file with start_line and end_line to access specific sections. {omitted} chars omitted from this preview.]\n\n"
    return f"{head}{ref}{tail}"


def _build_fallback(content: str, *, tool_name: str, max_chars: int, head_chars: int, tail_chars: int) -> str:
    total = len(content)
    if max_chars <= 0 or total <= max_chars:
        return content

    marker_template = "\n\n[... {n} chars omitted from {tn} output. Persistent storage unavailable. Consider narrowing the query or using more specific parameters.]\n\n"
    marker_overhead = len(marker_template.format(n=total, tn=tool_name))
    if marker_overhead >= max_chars:
        return content[:max_chars]

    budget = max_chars - marker_overhead
    effective_head = min(head_chars, budget)
    effective_tail = min(tail_chars, max(0, budget - effective_head))
    head_end = _snap_to_line_boundary(content, min(effective_head, total))
    tail_start = max(head_end, total - effective_tail)
    tail_start_snapped = _snap_to_line_boundary(content, tail_start)
    if tail_start_snapped > head_end:
        tail_start = tail_start_snapped

    head = content[:head_end]
    tail = content[tail_start:] if tail_start < total else ""
    marker = marker_template.format(n=total - len(head) - len(tail), tn=tool_name)
    return f"{head}{marker}{tail}"


def _resolve_outputs_path(request: ToolCallRequest) -> str | None:
    runtime = getattr(request, "runtime", None)
    state = getattr(runtime, "state", None)
    if not isinstance(state, dict):
        return None
    thread_data = state.get("thread_data")
    if not isinstance(thread_data, dict):
        return None
    outputs_path = thread_data.get("outputs_path")
    return outputs_path if isinstance(outputs_path, str) else None


def _resolve_sandbox(request: ToolCallRequest) -> Sandbox | None:
    runtime = getattr(request, "runtime", None)
    state = getattr(runtime, "state", None)
    if not isinstance(state, dict):
        return None
    sandbox_state = state.get("sandbox")
    if not isinstance(sandbox_state, dict):
        return None
    sandbox_id = sandbox_state.get("sandbox_id")
    if not sandbox_id:
        return None
    try:
        return get_sandbox_provider().get(sandbox_id)
    except Exception:
        logger.exception("Failed to look up sandbox %s for tool-output externalization", sandbox_id)
        return None


def _budget_content(
    content: str,
    *,
    tool_name: str,
    tool_call_id: str,
    outputs_path: str | None,
    config: ToolOutputConfig,
    sandbox: Sandbox | None = None,
) -> str | None:
    threshold = config.tool_overrides.get(tool_name, config.externalize_min_chars)
    if threshold <= 0 and config.fallback_max_chars <= 0:
        return None
    if len(content) <= threshold and len(content) <= config.fallback_max_chars:
        return None

    if threshold > 0 and len(content) > threshold:
        virtual_path: str | None = None
        if sandbox is not None:
            provider = None
            try:
                provider = get_sandbox_provider()
            except Exception:
                logger.exception("Failed to get sandbox provider for tool-output externalization")
            if provider is not None and getattr(provider, "uses_thread_data_mounts", False):
                if outputs_path:
                    virtual_path = _externalize(content, tool_name=tool_name, tool_call_id=tool_call_id, outputs_path=outputs_path, storage_subdir=config.storage_subdir)
            else:
                virtual_path = _externalize_to_sandbox(content, tool_name=tool_name, tool_call_id=tool_call_id, storage_subdir=config.storage_subdir, sandbox=sandbox)
        elif outputs_path:
            virtual_path = _externalize(content, tool_name=tool_name, tool_call_id=tool_call_id, outputs_path=outputs_path, storage_subdir=config.storage_subdir)

        if virtual_path is not None:
            return _build_preview(
                content,
                tool_name=tool_name,
                virtual_path=virtual_path,
                head_chars=config.preview_head_chars,
                tail_chars=config.preview_tail_chars,
                synopsis_enabled=config.structured_synopsis_enabled,
                synopsis_max_chars=config.structured_synopsis_max_chars,
            )

    if config.fallback_max_chars > 0 and len(content) > config.fallback_max_chars:
        return _build_fallback(content, tool_name=tool_name, max_chars=config.fallback_max_chars, head_chars=config.fallback_head_chars, tail_chars=config.fallback_tail_chars)

    return None


def _patch_tool_message(msg: ToolMessage, config: ToolOutputConfig, outputs_path: str | None, sandbox: Sandbox | None = None) -> ToolMessage:
    tool_name = msg.name or "unknown"
    if tool_name in config.exempt_tools:
        return msg
    text = _message_text(msg.content)
    if text is None:
        return msg
    replacement = _budget_content(text, tool_name=tool_name, tool_call_id=msg.tool_call_id or "", outputs_path=outputs_path, config=config, sandbox=sandbox)
    if replacement is None:
        return msg
    return msg.model_copy(update={"content": replacement})


def _effective_trigger(tool_name: str, config: ToolOutputConfig) -> int:
    candidates: list[int] = []
    externalize = config.tool_overrides.get(tool_name, config.externalize_min_chars)
    if externalize > 0:
        candidates.append(externalize)
    if config.fallback_max_chars > 0:
        candidates.append(config.fallback_max_chars)
    return min(candidates) if candidates else -1


def _tool_message_over_budget(msg: ToolMessage, config: ToolOutputConfig) -> bool:
    if (msg.name or "") in config.exempt_tools:
        return False
    trigger = _effective_trigger(msg.name or "", config)
    if trigger < 0:
        return False
    text = _message_text(msg.content)
    return text is not None and len(text) > trigger


def _needs_budget(result: ToolMessage | Command, config: ToolOutputConfig) -> bool:
    if isinstance(result, ToolMessage):
        return _tool_message_over_budget(result, config)
    update = getattr(result, "update", None)
    if isinstance(update, dict):
        return any(isinstance(msg, ToolMessage) and _tool_message_over_budget(msg, config) for msg in update.get("messages", []))
    return False


def _patch_result(result: ToolMessage | Command, config: ToolOutputConfig, outputs_path: str | None, sandbox: Sandbox | None = None) -> ToolMessage | Command:
    if isinstance(result, ToolMessage):
        return _patch_tool_message(result, config, outputs_path, sandbox)
    update = getattr(result, "update", None)
    if not isinstance(update, dict) or not isinstance(update.get("messages"), list):
        return result

    changed = False
    messages: list[Any] = []
    for msg in update["messages"]:
        patched = _patch_tool_message(msg, config, outputs_path, sandbox) if isinstance(msg, ToolMessage) else msg
        changed = changed or patched is not msg
        messages.append(patched)
    return dc_replace(result, update={**update, "messages": messages}) if changed else result


def _patch_model_messages(messages: list[Any], config: ToolOutputConfig) -> list[Any] | None:
    if not any(isinstance(msg, ToolMessage) and _tool_message_over_budget(msg, config) for msg in messages):
        return None
    changed = False
    updated: list[Any] = []
    for msg in messages:
        patched = _patch_tool_message(msg, config, outputs_path=None) if isinstance(msg, ToolMessage) else msg
        changed = changed or patched is not msg
        updated.append(patched)
    return updated if changed else None


_WRITE_TOOL = "write_file"
_FILE_MODIFYING_TOOLS = frozenset({"write_file", "str_replace"})
_FILE_READING_TOOLS = frozenset({"read_file"})
_ELIDED_WRITE_CONTENT_TEMPLATE = (
    "[content elided: {chars} chars; this write_file call succeeded and the "
    "file was read or modified again afterwards, so the on-disk file is the "
    "current version; call read_file on its path to see it]"
)


def elide_superseded_write_payloads(
    messages: list[Any],
    *,
    min_chars: int,
    keep_recent: int,
) -> list[Any] | None:
    """Elide old successful writes made redundant by a later file touch."""
    if not _has_elidable_write(messages, min_chars):
        return None

    latest_touch: dict[str, int] = {}
    successful_writes: list[tuple[ToolCallOccurrence, str]] = []
    for occurrence in pair_tool_call_results(messages):
        path = _normalized_path_arg(occurrence.args)
        if path is None:
            continue
        if occurrence.name in _FILE_MODIFYING_TOOLS:
            if not _result_succeeded(occurrence.name, occurrence.result):
                continue
            if occurrence.name == _WRITE_TOOL:
                successful_writes.append((occurrence, path))
        elif occurrence.name in _FILE_READING_TOOLS:
            if not _result_succeeded(occurrence.name, occurrence.result):
                continue
        else:
            continue
        # Calls in one AIMessage may execute concurrently and have no defined
        # ordering, so only a strictly later message can supersede a write.
        latest_touch[path] = max(
            latest_touch.get(path, -1),
            occurrence.index,
        )

    replacements: dict[tuple[int, str], dict[str, Any]] = {}
    cutoff = max(0, len(successful_writes) - keep_recent)
    for occurrence, path in successful_writes[:cutoff]:
        content = occurrence.args.get("content")
        if (
            not isinstance(content, str)
            or not content
            or len(content) < min_chars
            or latest_touch.get(path, -1) <= occurrence.index
        ):
            continue
        replacements[(id(occurrence.message), occurrence.call_id)] = {
            **occurrence.args,
            "content": _ELIDED_WRITE_CONTENT_TEMPLATE.format(
                chars=len(content)
            ),
        }
    if not replacements:
        return None

    def replacement_for(
        message: AIMessage,
        tool_call: dict[str, Any],
    ) -> dict[str, Any] | None:
        return replacements.get((id(message), tool_call["id"]))

    return rewrite_messages_tool_call_args(messages, replacement_for)


def _has_elidable_write(messages: list[Any], min_chars: int) -> bool:
    for message in messages:
        if not isinstance(message, AIMessage):
            continue
        for tool_call in message.tool_calls or ():
            if (
                not isinstance(tool_call, dict)
                or tool_call.get("name") != _WRITE_TOOL
            ):
                continue
            args = tool_call.get("args")
            content = args.get("content") if isinstance(args, dict) else None
            if (
                isinstance(content, str)
                and content
                and len(content) >= min_chars
            ):
                return True
    return False


def _result_succeeded(name: str, result: ToolMessage | None) -> bool:
    """Use the current runtime's ToolMessage success/error contract."""
    if result is None or getattr(result, "status", None) != "success":
        return False
    text = _message_text(result.content)
    if not isinstance(text, str):
        return False
    stripped = text.strip()
    if name in _FILE_MODIFYING_TOOLS:
        return stripped == "OK"
    return not (
        stripped.startswith("Error:")
        or stripped in {
            "(start_line must be >= 1)",
            "(end_line must be >= 1)",
            "(start_line > end_line — no lines in range)",
        }
    )


def _normalized_path_arg(args: Mapping[str, Any]) -> str | None:
    path = args.get("path")
    return posixpath.normpath(path) if isinstance(path, str) and path else None


class ToolOutputBudgetMiddleware(AgentMiddleware[AgentState]):
    """Enforce per-result budget on tool outputs via externalization or truncation."""

    def __init__(self, config: ToolOutputConfig | None = None) -> None:
        super().__init__()
        self._config = config if config is not None else _default_config()

    @classmethod
    def from_app_config(cls, app_config: Any) -> ToolOutputBudgetMiddleware:
        tool_output = getattr(app_config, "tool_output", None)
        if isinstance(tool_output, ToolOutputConfig):
            return cls(config=tool_output)
        return cls()

    @override
    def wrap_tool_call(self, request: ToolCallRequest, handler: Callable[[ToolCallRequest], ToolMessage | Command]) -> ToolMessage | Command:
        result = handler(request)
        if not self._config.enabled or not _needs_budget(result, self._config):
            return result
        return _patch_result(result, self._config, _resolve_outputs_path(request), _resolve_sandbox(request))

    @override
    async def awrap_tool_call(self, request: ToolCallRequest, handler: Callable[[ToolCallRequest], Awaitable[ToolMessage | Command]]) -> ToolMessage | Command:
        result = await handler(request)
        if not self._config.enabled or not _needs_budget(result, self._config):
            return result
        return await asyncio.to_thread(_patch_result, result, self._config, _resolve_outputs_path(request), _resolve_sandbox(request))

    @override
    def wrap_model_call(self, request: ModelRequest, handler: Callable[[ModelRequest], ModelResponse]) -> ModelCallResult:
        return handler(self._budget_model_request(request))

    @override
    async def awrap_model_call(self, request: ModelRequest, handler: Callable[[ModelRequest], Awaitable[ModelResponse]]) -> ModelCallResult:
        return await handler(self._budget_model_request(request))

    def _budget_model_request(self, request: ModelRequest) -> ModelRequest:
        if not self._config.enabled:
            return request
        original = getattr(request, "messages", None)
        if not isinstance(original, list):
            return request
        messages = original
        patched = _patch_model_messages(messages, self._config)
        if patched is not None:
            messages = patched
        if self._config.elide_superseded_writes:
            elided = elide_superseded_write_payloads(
                messages,
                min_chars=self._config.superseded_write_min_chars,
                keep_recent=self._config.keep_recent_writes,
            )
            if elided is not None:
                messages = elided
        return (
            request
            if messages is original
            else request.override(messages=messages)
        )
