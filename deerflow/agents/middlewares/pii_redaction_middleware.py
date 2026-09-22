"""Redact temporary model views without changing stored messages or artifacts.

This is a deterministic text filter, not a general data-loss prevention system.
It covers ordinary model calls plus explicitly configured title/summary calls.
Memory extraction, arbitrary extension model calls, image/audio data, and tool
execution are outside this boundary. Retrieval artifacts retain original text
for the user; only their model-visible message text is redacted.
"""

from __future__ import annotations

import re
from collections.abc import Awaitable, Callable
from copy import copy
from datetime import date
from typing import Any

from langchain.agents.middleware import AgentMiddleware
from langchain.agents.middleware.types import ModelCallResult, ModelRequest, ModelResponse
from langchain_core.messages import AIMessage, BaseMessage

from deerflow.config.pii_redaction_config import PiiRedactionConfig

_EMAIL = re.compile(r"(?<![A-Za-z0-9._%+\-])[A-Za-z0-9._%+\-]+@[A-Za-z0-9](?:[A-Za-z0-9.\-]*[A-Za-z0-9])?\.[A-Za-z]{2,63}(?![A-Za-z0-9_\-])")
_CN_ID = re.compile(r"(?<![A-Za-z0-9])[1-9][0-9]{16}[0-9Xx](?![A-Za-z0-9])")
_BANK_CARD = re.compile(r"(?<![A-Za-z0-9])[0-9](?:[ -]?[0-9]){12,18}(?![A-Za-z0-9])")
_CN_PHONE = re.compile(r"(?<![A-Za-z0-9])(?:\+?86[ -]?)?1[3-9][0-9][ -]?[0-9]{4}[ -]?[0-9]{4}(?![A-Za-z0-9])")
_INTL_PHONE = re.compile(r"(?<![A-Za-z0-9])\+[1-9](?:[ -]?[0-9]){7,14}(?![A-Za-z0-9])")
_API_KEY = re.compile(r"(?<![A-Za-z0-9_])(?:sk-(?:proj-|ant-api[0-9]{2}-)?[A-Za-z0-9_\-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|AKIA[A-Z0-9]{16})(?![A-Za-z0-9_\-])")
_LABELED_KEY = re.compile(r"(?i)(\b(?:api[_-]?key|access[_-]?token|secret[_-]?key)['\"]?\s*[:=]\s*['\"]?)([A-Za-z0-9._~+/=\-]{12,})")
_SECRET_FIELD = re.compile(r"(?i)^(?:api[_-]?key|access[_-]?token|secret[_-]?key)$")
_KEY_VALUE = re.compile(r"[A-Za-z0-9._~+/=\-]{12,}")
_BEARER = re.compile(r"(?i)(\bBearer\s+)([A-Za-z0-9._~+/=\-]{16,})")
_PLACEHOLDER = re.compile(r"(\[(?:REDACTED_)?(?:EMAIL|PHONE|BANK_CARD|CREDIT_CARD|CN_ID|CHINESE_ID|API_KEY)(?:_[0-9]+)?\])")


def _valid_chinese_id(value: str) -> bool:
    try:
        born = date(int(value[6:10]), int(value[10:12]), int(value[12:14]))
    except ValueError:
        return False
    if born.year < 1800 or born > date.today() or value[14:17] == "000":
        return False
    weights = (7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2)
    checksum = sum(int(char) * weight for char, weight in zip(value[:17], weights, strict=True)) % 11
    return value[-1].upper() == "10X98765432"[checksum]


def _valid_bank_card(value: str) -> bool:
    digits = [int(char) for char in value if char.isascii() and char.isdigit()]
    if len(set(digits)) < 2:
        return False
    total = 0
    for index, digit in enumerate(reversed(digits)):
        if index % 2:
            digit *= 2
            if digit > 9:
                digit -= 9
        total += digit
    return total % 10 == 0


def redact_text(text: str, config: PiiRedactionConfig | None) -> str:
    """Use irreversible type markers; existing markers are never renumbered."""
    if config is None or not config.enabled:
        return text

    def redact_part(part: str) -> str:
        # IDs must precede both cards and mobile numbers to avoid partial
        # redaction or misclassification of their embedded digit sequences.
        if config.chinese_id:
            part = _CN_ID.sub(lambda match: "[REDACTED_CN_ID]" if _valid_chinese_id(match[0]) else match[0], part)
        if config.api_key:
            part = _API_KEY.sub("[REDACTED_API_KEY]", part)
            part = _LABELED_KEY.sub(lambda match: match[1] + "[REDACTED_API_KEY]", part)
            part = _BEARER.sub(lambda match: match[1] + "[REDACTED_API_KEY]", part)
        if config.bank_card:
            part = _BANK_CARD.sub(lambda match: "[REDACTED_BANK_CARD]" if _valid_bank_card(match[0]) else match[0], part)
        if config.phone:
            part = _CN_PHONE.sub("[REDACTED_PHONE]", part)
            part = _INTL_PHONE.sub("[REDACTED_PHONE]", part)
        if config.email:
            part = _EMAIL.sub("[REDACTED_EMAIL]", part)
        return part

    return "".join(part if _PLACEHOLDER.fullmatch(part) else redact_part(part) for part in _PLACEHOLDER.split(text))


def _redact_values(value: Any, config: PiiRedactionConfig) -> Any:
    """Copy model-replayed argument values, preserving argument names and IDs."""
    if isinstance(value, str):
        return redact_text(value, config)
    if isinstance(value, int) and not isinstance(value, bool):
        text = str(value)
        redacted = redact_text(text, config)
        return redacted if redacted != text else value
    if isinstance(value, list):
        return [_redact_values(item, config) for item in value]
    if isinstance(value, dict):
        return {
            key: (
                "[REDACTED_API_KEY]"
                if config.api_key and isinstance(key, str) and _SECRET_FIELD.fullmatch(key)
                and isinstance(item, str) and _KEY_VALUE.fullmatch(item)
                else _redact_values(item, config)
            )
            for key, item in value.items()
        }
    return value


def _redact_content(content: Any, config: PiiRedactionConfig) -> Any:
    if isinstance(content, str):
        return redact_text(content, config)
    if not isinstance(content, list):
        return content
    blocks = []
    for block in content:
        if isinstance(block, str):
            blocks.append(redact_text(block, config))
        elif isinstance(block, dict):
            updated = dict(block)
            for key in ("text", "thinking", "reasoning"):
                if isinstance(updated.get(key), str):
                    updated[key] = redact_text(updated[key], config)
            if block.get("type") in {"tool_use", "tool_call", "function_call"}:
                for key in ("input", "args", "arguments"):
                    if key in updated:
                        updated[key] = _redact_values(updated[key], config)
            if block.get("type") == "reasoning" and isinstance(block.get("summary"), list):
                updated["summary"] = _redact_content(block["summary"], config)
            blocks.append(updated)
        else:
            blocks.append(block)
    return blocks


def redact_messages(messages: list[BaseMessage], config: PiiRedactionConfig | None) -> list[BaseMessage]:
    if config is None or not config.enabled:
        return messages
    result = []
    for message in messages:
        updates = {"content": _redact_content(message.content, config)}
        if isinstance(message, AIMessage):
            for field in ("tool_calls", "invalid_tool_calls"):
                calls = getattr(message, field)
                if calls:
                    updates[field] = [
                        {**call, "args": _redact_values(call.get("args"), config)}
                        for call in calls
                    ]
            additional = dict(message.additional_kwargs)
            if isinstance(additional.get("reasoning_content"), str):
                additional["reasoning_content"] = redact_text(additional["reasoning_content"], config)
            # OpenAI-compatible models may replay the raw wire arguments even
            # when parsed tool_calls are also present in the message.
            if isinstance(additional.get("tool_calls"), list):
                calls = []
                for call in additional["tool_calls"]:
                    if isinstance(call, dict) and isinstance(call.get("function"), dict):
                        function = dict(call["function"])
                        if "arguments" in function:
                            function["arguments"] = _redact_values(function["arguments"], config)
                        call = {**call, "function": function}
                    calls.append(call)
                additional["tool_calls"] = calls
            if isinstance(additional.get("function_call"), dict):
                function = dict(additional["function_call"])
                if "arguments" in function:
                    function["arguments"] = _redact_values(function["arguments"], config)
                additional["function_call"] = function
            updates["additional_kwargs"] = additional
        # model_copy retains IDs, status, usage, response metadata and opaque
        # artifact handles. In particular, user-facing knowledge evidence is
        # not rewritten or turned into a different purported source document.
        result.append(message.model_copy(update=updates))
    return result


class PiiRedactionMiddleware(AgentMiddleware):
    def __init__(self, config: PiiRedactionConfig | None = None):
        self.config = (config or PiiRedactionConfig()).model_copy(deep=True)

    def _request(self, request: ModelRequest) -> ModelRequest:
        if not self.config.enabled:
            return request
        system_message = request.system_message
        if system_message is not None:
            system_message = redact_messages([system_message], self.config)[0]
        return request.override(
            messages=redact_messages(request.messages, self.config),
            system_message=system_message,
        )

    def wrap_model_call(self, request: ModelRequest, handler: Callable[[ModelRequest], ModelResponse]) -> ModelCallResult:
        return handler(self._request(request))

    async def awrap_model_call(self, request: ModelRequest, handler: Callable[[ModelRequest], Awaitable[ModelResponse]]) -> ModelCallResult:
        return await handler(self._request(request))


def configure_pii_redaction(middlewares: list[AgentMiddleware], config: PiiRedactionConfig | None) -> list[AgentMiddleware]:
    """Apply an explicit config snapshot to model and known auxiliary callers."""
    if config is None or not config.enabled:
        return middlewares
    from deerflow.agents.middlewares.clarification_middleware import ClarificationMiddleware
    from deerflow.agents.middlewares.summarization_middleware import DeerFlowSummarizationMiddleware
    from deerflow.agents.middlewares.title_middleware import TitleMiddleware

    chain = []
    for middleware in middlewares:
        if isinstance(middleware, PiiRedactionMiddleware):
            continue
        if isinstance(middleware, (TitleMiddleware, DeerFlowSummarizationMiddleware)):
            middleware = copy(middleware)
            middleware.pii_redaction = config.model_copy(deep=True)
        chain.append(middleware)
    # Wrap calls run in list order. Redaction goes after context/tool-output
    # injectors so their temporary additions cannot bypass the filter.
    index = next((i for i, middleware in enumerate(chain) if isinstance(middleware, ClarificationMiddleware)), len(chain))
    chain.insert(index, PiiRedactionMiddleware(config))
    return chain
