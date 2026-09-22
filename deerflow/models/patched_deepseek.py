"""Patched ChatDeepSeek that preserves reasoning_content in multi-turn conversations.

This module provides a patched version of ChatDeepSeek that properly handles
reasoning_content when sending messages back to the API. The original implementation
stores reasoning_content in additional_kwargs but doesn't include it when making
subsequent API calls. Thinking-mode tool history also requires string content
and a reasoning_content field, even when no reasoning was emitted.
"""

from typing import Any

from langchain_core.language_models import LanguageModelInput
from langchain_core.messages import AIMessage
from langchain_deepseek import ChatDeepSeek


def _request_thinking_enabled(*sources: Any) -> bool:
    """Honor the most specific explicit setting, including disabled overrides."""
    for source in sources:
        if not isinstance(source, dict):
            continue
        for settings in (source, source.get("extra_body")):
            if not isinstance(settings, dict):
                continue
            thinking = settings.get("thinking")
            if isinstance(thinking, dict) and thinking.get("type") in {"enabled", "disabled"}:
                return thinking["type"] == "enabled"
    return False


class PatchedChatDeepSeek(ChatDeepSeek):
    """ChatDeepSeek with proper reasoning_content preservation.

    Restore emitted reasoning and fill protocol-required fields on thinking
    tool turns. These compatibility rules apply only to this adapter.
    """

    @classmethod
    def is_lc_serializable(cls) -> bool:
        return True

    @property
    def lc_secrets(self) -> dict[str, str]:
        return {"api_key": "DEEPSEEK_API_KEY", "openai_api_key": "DEEPSEEK_API_KEY"}

    def _get_request_payload(
        self,
        input_: LanguageModelInput,
        *,
        stop: list[str] | None = None,
        **kwargs: Any,
    ) -> dict:
        """Get request payload with reasoning_content preserved.

        Overrides the parent method to inject reasoning_content from
        additional_kwargs into assistant messages in the payload.
        """
        # Get the original messages before conversion
        original_messages = self._convert_input(input_).to_messages()

        # Call parent to get the base payload
        payload = super()._get_request_payload(input_, stop=stop, **kwargs)

        # Match payload messages with original messages to restore reasoning_content
        payload_messages = payload.get("messages", [])

        # The payload messages and original messages should be in the same order
        # Iterate through both and match by position
        if len(payload_messages) == len(original_messages):
            for payload_msg, orig_msg in zip(payload_messages, original_messages):
                if payload_msg.get("role") == "assistant" and isinstance(orig_msg, AIMessage):
                    reasoning_content = orig_msg.additional_kwargs.get("reasoning_content")
                    if reasoning_content is not None:
                        payload_msg["reasoning_content"] = reasoning_content
        else:
            # Fallback: match by counting assistant messages
            ai_messages = [m for m in original_messages if isinstance(m, AIMessage)]
            assistant_payloads = [(i, m) for i, m in enumerate(payload_messages) if m.get("role") == "assistant"]

            for (idx, payload_msg), ai_msg in zip(assistant_payloads, ai_messages):
                reasoning_content = ai_msg.additional_kwargs.get("reasoning_content")
                if reasoning_content is not None:
                    payload_messages[idx]["reasoning_content"] = reasoning_content

        thinking_enabled = _request_thinking_enabled(kwargs, payload, {"extra_body": self.extra_body})
        for payload_msg in payload_messages:
            if payload_msg.get("role") != "assistant" or not payload_msg.get("tool_calls"):
                continue
            if payload_msg.get("content") is None:
                payload_msg["content"] = ""
            if thinking_enabled and payload_msg.get("reasoning_content") is None:
                payload_msg["reasoning_content"] = ""

        return payload
