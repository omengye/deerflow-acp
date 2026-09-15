"""Middleware for injecting image details into conversation before LLM call."""

import asyncio
import base64
import hashlib
import logging
from collections.abc import Awaitable, Callable
from pathlib import Path
from typing import override

from langchain.agents.middleware import (
    AgentMiddleware,
    ModelCallResult,
    ModelRequest,
    ModelResponse,
)
from langchain_core.messages import AIMessage, HumanMessage, ToolMessage
from langgraph.runtime import Runtime

from deerflow.agents.image_inputs import (
    INPUT_IMAGES_KEY,
    MAX_INPUT_IMAGE_BYTES,
    detect_image_mime,
    normalize_input_image_metadata,
)
from deerflow.agents.thread_state import ThreadState

logger = logging.getLogger(__name__)

class ViewImageMiddlewareState(ThreadState):
    """Reuse the thread state so reducer-backed keys keep their annotations."""


class ViewImageMiddleware(AgentMiddleware[ViewImageMiddlewareState]):
    """Injects image details as a human message before LLM calls when view_image tools have completed.

    This middleware:
    1. Runs before each LLM call
    2. Checks if the last assistant message contains view_image tool calls
    3. Verifies all tool calls in that message have been completed (have corresponding ToolMessages)
    4. If conditions are met, creates an ephemeral human message containing image data
    5. Adds that message only to the immediate model request, never persisted state

    This enables the LLM to automatically receive and analyze images that were loaded via view_image tool,
    without requiring explicit user prompts to describe the images.
    """

    state_schema = ViewImageMiddlewareState

    def _get_last_assistant_message(self, messages: list) -> AIMessage | None:
        """Get the last assistant message from the message list.

        Args:
            messages: List of messages

        Returns:
            Last AIMessage or None if not found
        """
        for msg in reversed(messages):
            if isinstance(msg, AIMessage):
                return msg
        return None

    def _has_view_image_tool(self, message: AIMessage) -> bool:
        """Check if the assistant message contains view_image tool calls.

        Args:
            message: Assistant message to check

        Returns:
            True if message contains view_image tool calls
        """
        if not hasattr(message, "tool_calls") or not message.tool_calls:
            return False

        return any(tool_call.get("name") == "view_image" for tool_call in message.tool_calls)

    def _all_tools_completed(self, messages: list, assistant_msg: AIMessage) -> bool:
        """Check if all tool calls in the assistant message have been completed.

        Args:
            messages: List of all messages
            assistant_msg: The assistant message containing tool calls

        Returns:
            True if all tool calls have corresponding ToolMessages
        """
        if not hasattr(assistant_msg, "tool_calls") or not assistant_msg.tool_calls:
            return False

        # Get all tool call IDs from the assistant message
        tool_call_ids = {tool_call.get("id") for tool_call in assistant_msg.tool_calls if tool_call.get("id")}

        # Find the index of the assistant message
        try:
            assistant_idx = messages.index(assistant_msg)
        except ValueError:
            return False

        # Get all ToolMessages after the assistant message
        completed_tool_ids = set()
        for msg in messages[assistant_idx + 1 :]:
            if isinstance(msg, ToolMessage) and msg.tool_call_id:
                completed_tool_ids.add(msg.tool_call_id)

        # Check if all tool calls have been completed
        return tool_call_ids.issubset(completed_tool_ids)

    def _image_base64(self, image_path: str, image_data: dict, state: ViewImageMiddlewareState) -> str:
        legacy = image_data.get("base64")
        if isinstance(legacy, str) and legacy:
            return legacy

        thread_data = state.get("thread_data")
        if not isinstance(thread_data, dict):
            raise ValueError("Thread data is unavailable for image injection")
        from deerflow.sandbox.tools import (
            resolve_and_validate_user_data_path,
            validate_local_tool_path,
        )

        validate_local_tool_path(image_path, thread_data, read_only=True)

        source_sandbox_id = image_data.get("source_sandbox_id")
        sandbox_state = state.get("sandbox")
        active_sandbox_id = (
            sandbox_state.get("sandbox_id")
            if isinstance(sandbox_state, dict)
            else None
        )
        source_is_remote = isinstance(source_sandbox_id, str) and not (
            source_sandbox_id in {"local", "wsl"}
            or source_sandbox_id.startswith("local:")
            or source_sandbox_id.startswith("wsl:")
        )

        image_bytes: bytes | None = None
        if source_is_remote and source_sandbox_id == active_sandbox_id:
            from deerflow.sandbox.sandbox_provider import (
                get_existing_sandbox_provider,
            )

            provider = get_existing_sandbox_provider()
            sandbox = provider.get(source_sandbox_id) if provider is not None else None
            if sandbox is not None:
                image_bytes = sandbox.download_file(image_path)

        if image_bytes is None:
            # A replaced remote sandbox is allowed to fall back to a mounted
            # host mirror only when the persisted digest can prove identity.
            if source_is_remote and not isinstance(image_data.get("sha256"), str):
                raise ValueError("Remote image source is no longer active")
            actual_path = Path(resolve_and_validate_user_data_path(image_path, thread_data))
            if not actual_path.is_file():
                raise FileNotFoundError(image_path)
            image_bytes = actual_path.read_bytes()

        size = len(image_bytes)
        if size > MAX_INPUT_IMAGE_BYTES:
            raise ValueError(f"Image exceeds {MAX_INPUT_IMAGE_BYTES} bytes")
        expected_size = image_data.get("size")
        if isinstance(expected_size, int) and expected_size != size:
            raise ValueError("Image size changed after it was viewed")
        detected_mime = detect_image_mime(image_bytes)
        expected_mime = image_data.get("mime_type")
        if detected_mime is None or (
            isinstance(expected_mime, str) and detected_mime != expected_mime
        ):
            raise ValueError("Image contents do not match the expected format")
        expected_sha256 = image_data.get("sha256")
        if isinstance(expected_sha256, str) and (
            hashlib.sha256(image_bytes).hexdigest() != expected_sha256
        ):
            raise ValueError("Image contents changed after the prompt was accepted")
        return base64.b64encode(image_bytes).decode("ascii")

    def _patch_direct_input_images(
        self,
        state: ViewImageMiddlewareState,
        messages: list,
    ) -> list | None:
        """Add checkpoint-safe input images to the latest user turn ephemerally."""

        for index in range(len(messages) - 1, -1, -1):
            message = messages[index]
            if not isinstance(message, HumanMessage) or message.name:
                continue
            images = normalize_input_image_metadata(
                (message.additional_kwargs or {}).get(INPUT_IMAGES_KEY)
            )
            if not images:
                return None

            if isinstance(message.content, list):
                content_blocks: list[str | dict] = list(message.content)
            elif isinstance(message.content, str) and message.content:
                content_blocks = [{"type": "text", "text": message.content}]
            else:
                content_blocks = [
                    {
                        "type": "text",
                        "text": "Analyze the attached image(s) and respond helpfully.",
                    }
                ]

            injected_count = 0
            for image in images:
                image_path = str(image["virtual_path"])
                mime_type = str(image["mime_type"])
                try:
                    base64_data = self._image_base64(image_path, image, state)
                except Exception:
                    logger.warning(
                        "Skipping unavailable direct input image: %s",
                        image_path,
                        exc_info=True,
                    )
                    continue
                content_blocks.append(
                    {
                        "type": "text",
                        "text": f"\nAttached image: {image['name']} ({mime_type})",
                    }
                )
                content_blocks.append(
                    {
                        "type": "image_url",
                        "image_url": {
                            "url": f"data:{mime_type};base64,{base64_data}",
                        },
                    }
                )
                injected_count += 1

            if not injected_count:
                return None
            additional_kwargs = dict(message.additional_kwargs or {})
            additional_kwargs["_input_image_injection"] = True
            patched_message = message.model_copy(
                update={
                    "content": content_blocks,
                    "additional_kwargs": additional_kwargs,
                }
            )
            patched_messages = list(messages)
            patched_messages[index] = patched_message
            logger.debug("Injecting %d direct input image(s)", injected_count)
            return patched_messages
        return None

    def _create_image_details_message(self, state: ViewImageMiddlewareState) -> list[str | dict]:
        """Create a formatted message with all viewed image details.

        Args:
            state: Current state containing viewed_images

        Returns:
            List of content blocks (text and images) for the HumanMessage
        """
        viewed_images = state.get("viewed_images", {})
        if not viewed_images:
            # Return a properly formatted text block, not a plain string array
            return [{"type": "text", "text": "No images have been viewed."}]

        # Build the message with image information. This synthetic user message
        # must carry an explicit task reminder; otherwise some models merely
        # acknowledge the tool result instead of answering the user's request.
        content_blocks: list[str | dict] = [
            {
                "type": "text",
                "text": (
                    "Image input for analysis. Use the attached image(s) to answer "
                    "the user's most recent request. Do not merely confirm that "
                    "the image was read."
                ),
            }
        ]

        for image_path, image_data in viewed_images.items():
            mime_type = image_data.get("mime_type", "unknown")
            try:
                base64_data = self._image_base64(image_path, image_data, state)
            except Exception:
                logger.warning("Skipping unavailable viewed image: %s", image_path, exc_info=True)
                base64_data = ""

            # Add text description
            content_blocks.append({"type": "text", "text": f"\n- **{image_path}** ({mime_type})"})

            # Add the actual image data so LLM can "see" it
            if base64_data:
                content_blocks.append(
                    {
                        "type": "image_url",
                        "image_url": {"url": f"data:{mime_type};base64,{base64_data}"},
                    }
                )

        return content_blocks

    def _should_inject_image_message(self, state: ViewImageMiddlewareState) -> bool:
        """Determine if we should inject an image details message.

        Args:
            state: Current state

        Returns:
            True if we should inject the message
        """
        messages = state.get("messages", [])
        if not messages:
            return False

        # Get the last assistant message
        last_assistant_msg = self._get_last_assistant_message(messages)
        if not last_assistant_msg:
            return False

        # Check if it has view_image tool calls
        if not self._has_view_image_tool(last_assistant_msg):
            return False

        # Check if all tools have been completed
        if not self._all_tools_completed(messages, last_assistant_msg):
            return False

        # Check if we've already added an image details message by looking for the structured marker
        # instead of fragile string matching that breaks when summarization rewrites message text
        assistant_idx = messages.index(last_assistant_msg)
        for msg in messages[assistant_idx + 1 :]:
            if isinstance(msg, HumanMessage):
                if msg.additional_kwargs.get("_view_image_injection"):
                    # Already added, don't add again
                    return False

        return True

    def _ephemeral_messages(self, state: ViewImageMiddlewareState, messages: list) -> list | None:
        """Inject direct and tool-viewed images without checkpointing image bytes."""

        direct_messages = self._patch_direct_input_images(state, messages)
        effective_messages = direct_messages or messages
        if not self._should_inject_image_message(state):
            return direct_messages

        # Create the image details message with text and image content
        image_content = self._create_image_details_message(state)

        # This marker is visible to middleware in the immediate model request,
        # but the message is deliberately never returned as a state update.
        human_msg = HumanMessage(
            content=image_content,
            additional_kwargs={"_view_image_injection": True}
        )

        logger.debug("Injecting ephemeral image details before LLM call")
        return [*effective_messages, human_msg]

    @override
    def wrap_model_call(
        self,
        request: ModelRequest,
        handler: Callable[[ModelRequest], ModelResponse],
    ) -> ModelCallResult:
        patched = self._ephemeral_messages(request.state, request.messages)
        if patched is not None:
            request = request.override(messages=patched)
        return handler(request)

    @override
    async def awrap_model_call(
        self,
        request: ModelRequest,
        handler: Callable[[ModelRequest], Awaitable[ModelResponse]],
    ) -> ModelCallResult:
        patched = await asyncio.to_thread(self._ephemeral_messages, request.state, request.messages)
        if patched is not None:
            request = request.override(messages=patched)
        return await handler(request)

    @override
    def after_model(self, state: ViewImageMiddlewareState, runtime: Runtime) -> dict | None:
        if state.get("viewed_images"):
            return {"viewed_images": {}}
        return None

    @override
    async def aafter_model(self, state: ViewImageMiddlewareState, runtime: Runtime) -> dict | None:
        return self.after_model(state, runtime)
