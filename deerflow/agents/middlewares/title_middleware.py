"""Middleware for automatic thread title generation."""

import logging
import re
from collections.abc import Mapping
from pathlib import Path
from typing import NotRequired, override
from unicodedata import category

from langchain.agents import AgentState
from langchain.agents.middleware import AgentMiddleware
from langgraph.runtime import Runtime

from deerflow.agents.middlewares.uploads_middleware import (
    _strip_upload_blocks_from_content,
)
from deerflow.config.title_config import get_title_config
from deerflow.models import aclose_chat_model, create_chat_model

logger = logging.getLogger(__name__)


class TitleMiddlewareState(AgentState):
    """Compatible with the `ThreadState` schema."""

    title: NotRequired[str | None]
    uploaded_files: NotRequired[list[dict] | None]


class TitleMiddleware(AgentMiddleware[TitleMiddlewareState]):
    """Automatically generate a title for the thread after the first user message."""

    state_schema = TitleMiddlewareState

    def _normalize_content(self, content: object) -> str:
        if isinstance(content, str):
            return content

        if isinstance(content, list):
            parts = [self._normalize_content(item) for item in content]
            return "\n".join(part for part in parts if part)

        if isinstance(content, dict):
            text_value = content.get("text")
            if isinstance(text_value, str):
                return text_value

            nested_content = content.get("content")
            if nested_content is not None:
                return self._normalize_content(nested_content)

        return ""

    def _should_generate_title(self, state: TitleMiddlewareState) -> bool:
        """Check if we should generate a title for this thread."""
        config = get_title_config()
        if not config.enabled:
            return False

        # Check if thread already has a title in state
        if state.get("title"):
            return False

        # Check if this is the first turn (has at least one user message and one assistant response)
        messages = state.get("messages", [])
        if len(messages) < 2:
            return False

        # Count user and assistant messages
        user_messages = [m for m in messages if m.type == "human"]
        assistant_messages = [m for m in messages if m.type == "ai"]

        # Generate title after first complete exchange
        return len(user_messages) == 1 and len(assistant_messages) >= 1

    def _build_title_prompt(self, state: TitleMiddlewareState) -> tuple[str, str]:
        """Extract user/assistant messages and build the title prompt.

        Returns (prompt_string, user_msg) so callers can use user_msg as fallback.
        """
        config = get_title_config()
        messages = state.get("messages", [])

        user_msg_content = next((m.content for m in messages if m.type == "human"), "")
        assistant_msg_content = next((m.content for m in messages if m.type == "ai"), "")

        if isinstance(user_msg_content, (str, list)):
            user_msg_content, _ = _strip_upload_blocks_from_content(user_msg_content)
        user_msg = self._normalize_content(user_msg_content)
        assistant_msg = self._strip_think_tags(self._normalize_content(assistant_msg_content))

        prompt = config.prompt_template.format(
            max_words=config.max_words,
            user_msg=user_msg[:500],
            assistant_msg=assistant_msg[:500],
        )
        return prompt, user_msg

    def _strip_think_tags(self, text: str) -> str:
        """Remove <think>...</think> blocks emitted by reasoning models (e.g. minimax, DeepSeek-R1)."""
        return re.sub(r"<think>[\s\S]*?</think>", "", text, flags=re.IGNORECASE).strip()

    def _parse_title(self, content: object) -> str:
        """Normalize model output into a clean title string."""
        config = get_title_config()
        title_content = self._normalize_content(content)
        title_content = self._strip_think_tags(title_content)
        title = title_content.strip().strip('"').strip("'")
        return title[: config.max_chars] if len(title) > config.max_chars else title

    def _fallback_title(self, user_msg: str) -> str:
        if not user_msg.strip():
            return "New Conversation"
        config = get_title_config()
        fallback_chars = min(config.max_chars, 50)
        if len(user_msg) > fallback_chars:
            return user_msg[:fallback_chars].rstrip() + "..."
        return user_msg if user_msg else "New Conversation"

    @staticmethod
    def _clean_attachment_filename(filename: object) -> str | None:
        """Return a basename-only, layout-safe filename for display as a title."""
        if (
            not isinstance(filename, str)
            or not filename
            or Path(filename).name != filename
        ):
            return None
        cleaned = "".join(
            " " if category(character).startswith("C") else character
            for character in filename
        )
        cleaned = re.sub(r"\s+", " ", cleaned).strip()
        return cleaned or None

    def _truncate_attachment_filename(self, filename: str) -> str:
        max_chars = get_title_config().max_chars
        if len(filename) <= max_chars:
            return filename
        ellipsis = "..."
        extension = Path(filename).suffix.lstrip(".")
        remaining = max_chars - len(ellipsis) - len(extension)
        if extension and remaining > 0:
            return filename[:remaining].rstrip() + ellipsis + extension
        return filename[: max_chars - len(ellipsis)].rstrip() + ellipsis

    def _attachment_only_title(self, state: TitleMiddlewareState) -> str | None:
        """Build a local title when the first exchange contains uploads only."""
        _, user_msg = self._build_title_prompt(state)
        if user_msg.strip():
            return None
        files = state.get("uploaded_files")
        if not isinstance(files, list):
            return None

        filenames: list[str] = []
        seen_ids: set[str] = set()
        for file in files:
            if not isinstance(file, Mapping):
                continue
            filename = file.get("filename")
            cleaned = self._clean_attachment_filename(filename)
            if cleaned is None:
                continue
            attachment_id = file.get("path")
            if not isinstance(attachment_id, str) or not attachment_id:
                attachment_id = str(filename)
            if attachment_id in seen_ids:
                continue
            seen_ids.add(attachment_id)
            filenames.append(cleaned)

        if len(filenames) == 1:
            return self._truncate_attachment_filename(filenames[0])
        if len(filenames) > 1:
            for title in (f"{len(filenames)} files uploaded", f"{len(filenames)} files"):
                if len(title) <= get_title_config().max_chars:
                    return title
            return str(len(filenames))
        return None

    def _generate_title_result(self, state: TitleMiddlewareState) -> dict | None:
        """Generate a local fallback title without blocking on an LLM call."""
        if not self._should_generate_title(state):
            return None

        if attachment_title := self._attachment_only_title(state):
            return {"title": attachment_title}

        _, user_msg = self._build_title_prompt(state)
        return {"title": self._fallback_title(user_msg)}

    async def _agenerate_title_result(
        self,
        state: TitleMiddlewareState,
        runtime: Runtime | None = None,
    ) -> dict | None:
        """Generate a title asynchronously and fall back locally on failure."""
        if not self._should_generate_title(state):
            return None

        if attachment_title := self._attachment_only_title(state):
            return {"title": attachment_title}

        prompt, user_msg = self._build_title_prompt(state)
        if not user_msg.strip():
            return {"title": self._fallback_title(user_msg)}

        config = get_title_config()

        model = None
        try:
            if config.model_name:
                model = create_chat_model(name=config.model_name, thinking_enabled=False, disable_keepalive=True)
            else:
                model = create_chat_model(thinking_enabled=False, disable_keepalive=True)
            from deerflow.agents.middlewares.llm_error_handling_middleware import (
                llm_call_slot_async,
            )
            from deerflow.agents.middlewares.runtime_headers_middleware import (
                bind_runtime_headers,
            )

            request_model = bind_runtime_headers(model, runtime)
            async with llm_call_slot_async():
                from deerflow.models.invocation import ainvoke_chat_model

                response = await ainvoke_chat_model(
                    request_model,
                    prompt,
                    config={"run_name": "title_agent"},
                )
            title = self._parse_title(response.content)
            if title:
                return {"title": title}
        except Exception:
            logger.debug("Failed to generate async title; falling back to local title", exc_info=True)
        finally:
            # Drain the per-call httpx pool so connections don't outlive the
            # short-lived ChatOpenAI; consistent with the cleanup pattern used
            # by other one-shot LLM callers (MemoryUpdater, subagents).
            await aclose_chat_model(model)
        return {"title": self._fallback_title(user_msg)}

    @override
    def after_model(self, state: TitleMiddlewareState, runtime: Runtime) -> dict | None:
        return self._generate_title_result(state)

    @override
    async def aafter_model(self, state: TitleMiddlewareState, runtime: Runtime) -> dict | None:
        return await self._agenerate_title_result(state, runtime)
