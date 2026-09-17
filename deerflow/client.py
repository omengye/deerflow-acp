"""DeerFlowClient — Embedded Python client for DeerFlow agent system.

Provides direct programmatic access to DeerFlow's agent capabilities
without requiring LangGraph Server or Gateway API processes.

Usage:
    from deerflow.client import DeerFlowClient

    client = DeerFlowClient()
    response = client.chat("Analyze this paper for me", thread_id="my-thread")
    print(response)

    # Streaming
    for event in client.stream("hello"):
        print(event)
"""

import asyncio
import json
import logging
import mimetypes
import tempfile
import uuid
from collections.abc import AsyncGenerator, Generator, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Literal

from langchain.agents import create_agent
from langchain.agents.middleware import AgentMiddleware
from langchain_core.messages import AIMessage, HumanMessage, SystemMessage, ToolMessage
from langchain_core.runnables import RunnableConfig
from langgraph.checkpoint.base import BaseCheckpointSaver
from langgraph.errors import GraphRecursionError
from langgraph.graph.state import CompiledStateGraph

from deerflow.agents.date_context import get_current_date
from deerflow.agents.image_inputs import (
    INPUT_IMAGES_KEY,
    normalize_input_image_metadata,
)
from deerflow.agents.lead_agent.agent import _build_middlewares
from deerflow.agents.lead_agent.prompt import apply_prompt_template
from deerflow.agents.middlewares.subagent_limit_middleware import clamp_subagent_limit
from deerflow.agents.thread_state import (
    AgentContext,
    get_thread_state_schema,
    normalize_middleware_state_schemas,
)
from deerflow.config.agents_config import AGENT_NAME_PATTERN, load_agent_config
from deerflow.config.app_config import get_app_config, reload_app_config
from deerflow.config.checkpointer_config import (
    DEFAULT_CHECKPOINT_SNAPSHOT_FREQUENCY,
    CheckpointChannelMode,
)
from deerflow.config.extensions_config import (
    ExtensionsConfig,
    get_extensions_config,
    get_extensions_config_lock,
    load_extensions_config_data,
    reload_extensions_config,
    write_extensions_config_data,
)
from deerflow.config.paths import get_paths
from deerflow.models import create_chat_model
from deerflow.skills.installer import install_skill_from_archive
from deerflow.uploads.manager import (
    copy_file_exclusive,
    delete_file_safe,
    enrich_file_listing,
    ensure_uploads_dir,
    get_uploads_dir,
    list_files_in_dir,
    upload_artifact_url,
    upload_virtual_path,
)

logger = logging.getLogger(__name__)

from deerflow.runtime.checkpoint_lock import (
    checkpoint_thread_lock_async,
    checkpoint_thread_lock_sync,
)

# Shown to the user when a run exhausts the graph ``recursion_limit``. The
# loop-detection middleware normally forces a clean wrap-up well before this,
# so reaching it means a genuinely long/diverse run. We surface a meaningful
# final message instead of letting the bare ``GraphRecursionError`` propagate
# (which discards the whole turn and leaves dangling tool calls).
_RECURSION_LIMIT_NOTICE = (
    "⚠️ 已达到本轮对话的最大步数限制，无法继续调用更多工具。"
    "以上是我在限制内完成的部分结果。如需继续，请补充说明或将任务拆分后重试。"
)


StreamEventType = Literal["values", "messages-tuple", "custom", "end"]


@dataclass
class StreamEvent:
    """A single event from the streaming agent response.

    Event types align with the LangGraph SSE protocol:
        - ``"values"``: Full state snapshot (title, messages, artifacts).
        - ``"messages-tuple"``: Per-message update (AI text, tool calls, tool results).
        - ``"end"``: Stream finished.

    Attributes:
        type: Event type.
        data: Event payload. Contents vary by type.
    """

    type: StreamEventType
    data: dict[str, Any] = field(default_factory=dict)


@dataclass
class _StreamProcessingState:
    """Mutable bookkeeping shared by sync and async stream consumers."""

    seen_ids: set[str] = field(default_factory=set)
    streamed_ids: set[str] = field(default_factory=set)
    counted_usage_ids: set[str] = field(default_factory=set)
    cumulative_usage: dict[str, int] = field(
        default_factory=lambda: {"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
    )
    # usage_metadata of the LAST top-level (lead) model call. input_tokens of
    # the most recent call approximates the current thread context occupancy;
    # subagent calls (non-empty LangGraph namespaces) are excluded on purpose.
    last_lead_usage: dict[str, int] | None = None


class DeerFlowClient:
    """Embedded Python client for DeerFlow agent system.

    Provides direct programmatic access to DeerFlow's agent capabilities
    without requiring LangGraph Server or Gateway API processes.

    Note:
        Multi-turn conversations require a ``checkpointer``. Without one,
        each ``stream()`` / ``chat()`` call is stateless — ``thread_id``
        is only used for file isolation (uploads / artifacts).

        The system prompt (including date, memory, and skills context) is
        generated when the internal agent is first created and cached until
        the configuration key changes. Call :meth:`reset_agent` to force
        a refresh in long-running processes.

    Example::

        from deerflow.client import DeerFlowClient

        client = DeerFlowClient()

        # Simple one-shot
        print(client.chat("hello"))

        # Streaming
        for event in client.stream("hello"):
            print(event.type, event.data)

        # Configuration queries
        print(client.list_models())
        print(client.list_skills())
    """

    def __init__(
        self,
        config_path: str | None = None,
        checkpointer=None,
        *,
        model_name: str | None = None,
        thinking_enabled: bool = True,
        subagent_enabled: bool = False,
        plan_mode: bool = False,
        max_concurrent_subagents: int = 3,
        agent_name: str | None = None,
        available_skills: set[str] | None = None,
        middlewares: Sequence[AgentMiddleware] | None = None,
        additional_mcp_tools: Sequence[Any] | None = None,
        excluded_tool_names: set[str] | None = None,
        allowed_tool_names: set[str] | None = None,
        system_prompt_overlay: str | None = None,
        subagent_system_prompt_overlay: str | None = None,
        subagent_middlewares: Sequence[AgentMiddleware] | None = None,
        recursion_limit: int = 200,
        checkpoint_channel_mode: CheckpointChannelMode | None = None,
        checkpoint_snapshot_frequency: int | None = None,
    ):
        """Initialize the client.

        Loads configuration but defers agent creation to first use.

        Args:
            config_path: Path to config.yaml. Uses default resolution if None.
            checkpointer: LangGraph checkpointer instance for state persistence.
                Required for multi-turn conversations on the same thread_id.
                Without a checkpointer, each call is stateless.
            model_name: Override the default model name from config.
            thinking_enabled: Enable model's extended thinking.
            subagent_enabled: Enable subagent delegation.
            plan_mode: Enable TodoList middleware for plan mode.
            max_concurrent_subagents: Maximum parallel subagent tool calls per model response.
            agent_name: Name of the agent to use.
            available_skills: Optional set of skill names to make available. If None (default), all scanned skills are available.
            middlewares: Optional list of custom middlewares to inject into the agent.
            excluded_tool_names: Tool names to omit from this embedded client.
            allowed_tool_names: Optional hard allowlist applied after all tools are assembled.
            system_prompt_overlay: Server-owned instructions appended to the generated prompt.
            subagent_system_prompt_overlay: Server-owned instructions appended to subagent prompts.
            subagent_middlewares: Guardrail middlewares inherited by internal subagents.
        """
        if config_path is not None:
            reload_app_config(config_path)
        self._app_config = get_app_config()

        if agent_name is not None and not AGENT_NAME_PATTERN.match(agent_name):
            raise ValueError(f"Invalid agent name '{agent_name}'. Must match pattern: {AGENT_NAME_PATTERN.pattern}")

        self._checkpointer = checkpointer
        self._model_name = model_name
        self._thinking_enabled = thinking_enabled
        self._subagent_enabled = subagent_enabled
        self._plan_mode = plan_mode
        self._max_concurrent_subagents = max_concurrent_subagents
        self._agent_name = agent_name
        self._agent_config = load_agent_config(agent_name) if agent_name is not None else None
        self._memory_enabled = (
            getattr(self._agent_config, "memory_enabled", True)
            if self._agent_config is not None
            else True
        )
        profile_skills = self._agent_config.skills if self._agent_config is not None else None
        if available_skills is None:
            self._available_skills = set(profile_skills) if profile_skills is not None else None
        elif profile_skills is None:
            self._available_skills = set(available_skills)
        else:
            self._available_skills = set(available_skills).intersection(profile_skills)
        self._tool_groups = self._agent_config.tool_groups if self._agent_config is not None else None
        self._profile_model_name = self._agent_config.model if self._agent_config is not None else None
        self._middlewares = list(middlewares) if middlewares else []
        self._subagent_middlewares = list(subagent_middlewares or [])
        self._additional_mcp_tools = list(additional_mcp_tools or [])
        self._excluded_tool_names = frozenset(excluded_tool_names or ())
        self._allowed_tool_names = (
            frozenset(allowed_tool_names) if allowed_tool_names is not None else None
        )
        self._system_prompt_overlay = (system_prompt_overlay or "").strip()
        self._subagent_system_prompt_overlay = (
            subagent_system_prompt_overlay
            if subagent_system_prompt_overlay is not None
            else self._system_prompt_overlay
        ).strip()
        self._recursion_limit = recursion_limit
        configured_checkpointer = getattr(self._app_config, "checkpointer", None)
        self._checkpoint_channel_mode: CheckpointChannelMode = (
            checkpoint_channel_mode
            or getattr(configured_checkpointer, "channel_mode", "full")
        )
        self._checkpoint_snapshot_frequency = (
            checkpoint_snapshot_frequency
            or getattr(
                getattr(configured_checkpointer, "delta", None),
                "snapshot_frequency",
                DEFAULT_CHECKPOINT_SNAPSHOT_FREQUENCY,
            )
        )

        # Lazy agent — created on first call, recreated when config changes.
        self._agent: CompiledStateGraph[Any, Any, Any, Any] | None = None
        self._agent_config_key: tuple | None = None
        # Effective checkpointer used by the current agent (may differ from
        # self._checkpointer when get_checkpointer() was called as fallback).
        self._effective_checkpointer: Any = None

    @property
    def agent_name(self) -> str:
        return self._agent_name or "agent"

    def reset_agent(self) -> None:
        """Force the internal agent to be recreated on the next call.

        Use this after external changes (e.g. memory updates, skill
        installations) that should be reflected in the system prompt
        or tool set.
        """
        self._agent = None
        self._agent_config_key = None

    def warmup(self) -> None:
        """Build the configured agent graph without sending a model request."""

        config = self._get_runnable_config("__deerflow_warmup__")
        self._ensure_agent(config)

    # ------------------------------------------------------------------
    # Internal helpers
    # ------------------------------------------------------------------

    @staticmethod
    def _atomic_write_json(path: Path, data: dict) -> None:
        """Write JSON to *path* atomically (temp file + replace)."""
        fd = tempfile.NamedTemporaryFile(
            mode="w",
            dir=path.parent,
            suffix=".tmp",
            delete=False,
        )
        try:
            json.dump(data, fd, indent=2)
            fd.close()
            Path(fd.name).replace(path)
        except BaseException:
            fd.close()
            Path(fd.name).unlink(missing_ok=True)
            raise

    def _get_runnable_config(self, thread_id: str, **overrides) -> RunnableConfig:
        """Build a RunnableConfig for agent invocation."""
        configurable = {
            "thread_id": thread_id,
            "model_name": overrides.get(
                "model_name",
                self._model_name or getattr(self, "_profile_model_name", None),
            ),
            "thinking_enabled": overrides.get("thinking_enabled", self._thinking_enabled),
            "is_plan_mode": overrides.get("plan_mode", self._plan_mode),
            "subagent_enabled": overrides.get("subagent_enabled", self._subagent_enabled),
            "max_concurrent_subagents": overrides.get("max_concurrent_subagents", self._max_concurrent_subagents),
        }
        metadata: dict[str, Any] = {}
        if "live_event_callback" in overrides:
            metadata["live_event_callback"] = overrides["live_event_callback"]
        # Mirror key context fields into metadata so tools (e.g. task_tool) can read
        # parent-agent context from runtime.config["metadata"] at call time.
        # _get_runnable_config puts model_name/thinking_enabled into configurable for
        # LangGraph checkpoint keying, but ToolRuntime only exposes metadata to tools.
        effective_model = (
            overrides.get("model_name")
            or self._model_name
            or getattr(self, "_profile_model_name", None)
        )
        if effective_model:
            metadata["model_name"] = effective_model
        metadata["thinking_enabled"] = bool(overrides.get("thinking_enabled", self._thinking_enabled))
        metadata["agent_name"] = self._agent_name or "agent"
        metadata["tool_groups"] = getattr(self, "_tool_groups", None)
        metadata["available_skills"] = (
            sorted(self._available_skills) if self._available_skills is not None else None
        )
        subagent_middlewares = getattr(self, "_subagent_middlewares", [])
        if subagent_middlewares:
            metadata["subagent_middlewares"] = subagent_middlewares
        metadata["subagent_system_prompt_overlay"] = getattr(
            self, "_subagent_system_prompt_overlay", ""
        )
        metadata["subagent_excluded_tool_names"] = sorted(self._excluded_tool_names)
        metadata["subagent_allowed_tool_names"] = (
            sorted(self._allowed_tool_names)
            if self._allowed_tool_names is not None
            else None
        )
        if overrides.get("user_id") is not None:
            metadata["user_id"] = str(overrides["user_id"])
        result = RunnableConfig(
            configurable=configurable,
            metadata=metadata,
            recursion_limit=overrides.get("recursion_limit", self._recursion_limit),
        )
        from deerflow.runtime.checkpoint_mode import inject_checkpoint_mode

        inject_checkpoint_mode(result, self._checkpoint_channel_mode)
        return result

    @staticmethod
    def _get_memory_signature(agent_name: str | None = None) -> str | None:
        """Return a stable signature for prompt-injected memory content."""
        try:
            from deerflow.agents.memory import memory_manager_lease
            from deerflow.config.memory_config import get_memory_config

            memory_config = get_memory_config()
            if (
                not memory_config.enabled
                or not memory_config.injection_enabled
                or memory_config.mode in {"middleware", "tool"}
            ):
                return None
            with memory_manager_lease() as manager:
                return manager.cache_signature(agent_name)
        except Exception:
            logger.debug("Failed to build memory signature for agent cache key", exc_info=True)
            return None

    @staticmethod
    def _get_skill_catalog_version() -> int:
        """Return the global published Skill catalog version."""
        try:
            from deerflow.skills.evolution import get_evolution_store

            return get_evolution_store().get_catalog_version()
        except Exception:
            logger.debug("Failed to read Skill catalog version for agent cache key", exc_info=True)
            return 0

    def _ensure_agent(self, config: RunnableConfig):
        """Create (or recreate) the agent when config-dependent params change."""
        cfg = config.get("configurable", {})
        app_config = get_app_config()
        requested_model_name = cfg.get("model_name") or getattr(
            self, "_profile_model_name", None
        )
        model_name = requested_model_name or app_config.get_default_model_name()
        model_config = app_config.get_model_config(model_name) if model_name else None
        current_date = get_current_date()
        memory_enabled = getattr(self, "_memory_enabled", True)
        memory_signature = (
            self._get_memory_signature(self._agent_name)
            if memory_enabled
            else None
        )
        skill_catalog_version = self._get_skill_catalog_version()
        skill_evolution_config = getattr(app_config, "skill_evolution", None)
        skill_evolution_enabled = bool(getattr(skill_evolution_config, "enabled", False))
        skill_evolution_mode = getattr(skill_evolution_config, "mode", "review")
        checkpoint_mode = getattr(self, "_checkpoint_channel_mode", "full")
        checkpoint_frequency = getattr(
            self,
            "_checkpoint_snapshot_frequency",
            DEFAULT_CHECKPOINT_SNAPSHOT_FREQUENCY,
        )
        key: tuple[Any, ...] = (
            model_name,
            getattr(model_config, "supports_vision", False),
            cfg.get("thinking_enabled"),
            cfg.get("is_plan_mode"),
            cfg.get("subagent_enabled"),
            getattr(app_config.subagents, "enabled", True),
            cfg.get("max_concurrent_subagents"),
            memory_signature,
            skill_catalog_version,
            skill_evolution_enabled,
            skill_evolution_mode,
            self._agent_name,
            memory_enabled,
            frozenset(self._available_skills) if self._available_skills is not None else None,
            checkpoint_mode,
            checkpoint_frequency,
            getattr(self, "_excluded_tool_names", frozenset()),
            getattr(self, "_allowed_tool_names", None),
            getattr(self, "_system_prompt_overlay", ""),
            getattr(self, "_subagent_system_prompt_overlay", ""),
            current_date,
            tuple(
                type(middleware).__qualname__
                for middleware in getattr(self, "_subagent_middlewares", [])
            ),
        )

        if self._agent is not None and self._agent_config_key == key:
            return

        thinking_enabled = cfg.get("thinking_enabled", True)
        subagent_enabled = bool(cfg.get("subagent_enabled", False)) and getattr(app_config.subagents, "enabled", True)
        max_concurrent_subagents = clamp_subagent_limit(cfg.get("max_concurrent_subagents", 3))

        from deerflow.runtime.checkpoint_mode import (
            freeze_checkpoint_channel_mode,
            freeze_checkpoint_snapshot_frequency,
        )

        freeze_checkpoint_channel_mode(checkpoint_mode)
        freeze_checkpoint_snapshot_frequency(checkpoint_frequency)
        middlewares = _build_middlewares(
            config,
            model_name=model_name,
            agent_name=self._agent_name,
            custom_middlewares=self._middlewares,
            recursion_limit=self._recursion_limit,
            memory_enabled=memory_enabled,
        )
        middlewares = normalize_middleware_state_schemas(
            middlewares,
            checkpoint_mode,
            checkpoint_frequency,
        )
        tools = self._get_tools(model_name=model_name, subagent_enabled=subagent_enabled)
        effective_tool_names = {tool.name for tool in tools}
        effective_subagent_enabled = subagent_enabled and "task" in effective_tool_names
        kwargs: dict[str, Any] = {
            # disable_keepalive: the lead agent's ChatOpenAI is cached per
            # config and reused across requests; opting out of keep-alive
            # ensures the httpx pool never carries SSL transports that
            # could later be torn down on a foreign loop.
            "model": create_chat_model(name=model_name, thinking_enabled=thinking_enabled, disable_keepalive=True),
            "tools": tools,
            "middleware": middlewares,
            "system_prompt": apply_prompt_template(
                subagent_enabled=effective_subagent_enabled,
                max_concurrent_subagents=max_concurrent_subagents,
                agent_name=self._agent_name,
                available_skills=self._available_skills,
                available_tool_names=effective_tool_names,
                system_prompt_overlay=getattr(self, "_system_prompt_overlay", ""),
                current_date=current_date,
                memory_enabled=memory_enabled,
            ),
            "state_schema": get_thread_state_schema(
                checkpoint_mode,
                checkpoint_frequency,
            ),
            "context_schema": AgentContext,
        }
        checkpointer = self._checkpointer
        if checkpointer is None:
            from deerflow.agents.checkpointer import get_checkpointer

            checkpointer = get_checkpointer()
        if checkpointer is not None:
            kwargs["checkpointer"] = checkpointer

        self._agent = create_agent(**kwargs)
        self._effective_checkpointer = checkpointer
        self._agent_config_key = key
        logger.info("Agent created: agent_name=%s, model=%s, thinking=%s", self._agent_name, model_name, thinking_enabled)

    def _get_tools(self, *, model_name: str | None, subagent_enabled: bool):
        """Lazy import to avoid circular dependency at module level."""
        from deerflow.tools import get_available_tools

        tools = list(
            get_available_tools(
                model_name=model_name,
                groups=getattr(self, "_tool_groups", None),
                subagent_enabled=subagent_enabled,
                include_memory_tool=getattr(self, "_memory_enabled", True),
            )
        )
        existing_names = {tool.name for tool in tools}
        for tool in getattr(self, "_additional_mcp_tools", []):
            if tool.name in existing_names:
                logger.warning(
                    "Skipping client MCP tool %r because DeerFlow already exposes that name",
                    tool.name,
                )
                continue
            tools.append(tool)
            existing_names.add(tool.name)
        filtered = [
            tool
            for tool in tools
            if tool.name not in getattr(self, "_excluded_tool_names", frozenset())
        ]
        allowed = getattr(self, "_allowed_tool_names", None)
        if allowed is not None:
            filtered = [tool for tool in filtered if tool.name in allowed]

        try:
            from deerflow.tools.builtins.tool_search import (
                clone_deferred_registry_for_tools,
                get_deferred_registry,
                reset_deferred_registry,
                set_deferred_registry,
            )

            source_registry = get_deferred_registry()
            if source_registry is not None:
                filtered_names = {tool.name for tool in filtered}
                if "tool_search" not in filtered_names:
                    # Allowed deferred tools become ordinary bound tools when
                    # discovery itself is unavailable.
                    reset_deferred_registry()
                else:
                    filtered_registry = clone_deferred_registry_for_tools(
                        source_registry, filtered
                    )
                    if filtered_registry is None:
                        filtered = [tool for tool in filtered if tool.name != "tool_search"]
                        reset_deferred_registry()
                    else:
                        set_deferred_registry(filtered_registry)
        except Exception:
            logger.debug("Failed to apply deferred tool policy", exc_info=True)
        return filtered

    @staticmethod
    def _serialize_tool_calls(tool_calls) -> list[dict]:
        """Reshape LangChain tool_calls into the wire format used in events."""
        return [{"name": tc["name"], "args": tc["args"], "id": tc.get("id")} for tc in tool_calls]

    @staticmethod
    def _ai_text_event(msg_id: str | None, text: str, usage: dict | None, reasoning_content: str | None = None) -> "StreamEvent":
        """Build a ``messages-tuple`` AI text event, attaching usage and reasoning when present."""
        data: dict[str, Any] = {"type": "ai", "content": text, "id": msg_id}
        if reasoning_content:
            data["reasoning_content"] = reasoning_content
        if usage:
            data["usage_metadata"] = usage
        return StreamEvent(type="messages-tuple", data=data)

    def _recursion_limit_event(self) -> "StreamEvent":
        """Build a synthetic AI text event used when the recursion limit is hit.

        Surfaces a graceful final answer to the user. The text is *not* written
        back to the checkpoint (the graph already aborted); any dangling tool
        calls left in history are repaired by ``DanglingToolCallMiddleware`` on
        the next turn.
        """
        return self._ai_text_event(
            f"recursion-limit-{uuid.uuid4().hex[:12]}",
            _RECURSION_LIMIT_NOTICE,
            None,
        )

    @staticmethod
    def _ai_tool_calls_event(msg_id: str | None, tool_calls) -> "StreamEvent":
        """Build a ``messages-tuple`` AI tool-calls event."""
        return StreamEvent(
            type="messages-tuple",
            data={
                "type": "ai",
                "content": "",
                "id": msg_id,
                "tool_calls": DeerFlowClient._serialize_tool_calls(tool_calls),
            },
        )

    @staticmethod
    def _tool_message_event(msg: ToolMessage) -> "StreamEvent":
        """Build a ``messages-tuple`` tool-result event from a ToolMessage."""
        from deerflow.tools.builtins.present_file_tool import PRESENTED_ARTIFACTS_KEY

        presented = (msg.additional_kwargs or {}).get(PRESENTED_ARTIFACTS_KEY)
        data = {
            "type": "tool",
            "content": DeerFlowClient._extract_text(msg.content),
            "name": msg.name,
            "tool_call_id": msg.tool_call_id,
            "id": msg.id,
            "status": getattr(msg, "status", "success"),
        }
        if (
            msg.name == "present_files"
            and isinstance(presented, list)
            and all(isinstance(path, str) for path in presented)
        ):
            data["presented_artifacts"] = presented
        return StreamEvent(
            type="messages-tuple",
            data=data,
        )

    @staticmethod
    def _serialize_message(msg) -> dict:
        """Serialize a LangChain message to a plain dict for values events."""
        if isinstance(msg, AIMessage):
            d: dict[str, Any] = {"type": "ai", "content": msg.content, "id": getattr(msg, "id", None)}
            reasoning = msg.additional_kwargs.get("reasoning_content")
            if reasoning:
                d["reasoning_content"] = reasoning
            if msg.tool_calls:
                d["tool_calls"] = DeerFlowClient._serialize_tool_calls(msg.tool_calls)
            if getattr(msg, "usage_metadata", None):
                d["usage_metadata"] = msg.usage_metadata
            return d
        if isinstance(msg, ToolMessage):
            return {
                "type": "tool",
                "content": DeerFlowClient._extract_text(msg.content),
                "name": getattr(msg, "name", None),
                "tool_call_id": getattr(msg, "tool_call_id", None),
                "id": getattr(msg, "id", None),
                "status": getattr(msg, "status", "success"),
            }
        if isinstance(msg, HumanMessage):
            serialized = {
                "type": "human",
                "content": msg.content,
                "id": getattr(msg, "id", None),
            }
            input_images = normalize_input_image_metadata(
                (msg.additional_kwargs or {}).get(INPUT_IMAGES_KEY)
            )
            if input_images:
                serialized["input_images"] = input_images
            return serialized
        if isinstance(msg, SystemMessage):
            return {"type": "system", "content": msg.content, "id": getattr(msg, "id", None)}
        return {"type": "unknown", "content": str(msg), "id": getattr(msg, "id", None)}

    @staticmethod
    def _extract_text(content) -> str:
        """Extract plain text from AIMessage content (str or list of blocks).

        String chunks are concatenated without separators to avoid corrupting
        token/character deltas or chunked JSON payloads. Dict-based text blocks
        are treated as full text blocks and joined with newlines to preserve
        readability.
        """
        if isinstance(content, str):
            return content
        if isinstance(content, list):
            if content and all(isinstance(block, str) for block in content):
                chunk_like = len(content) > 1 and all(isinstance(block, str) and len(block) <= 20 and any(ch in block for ch in '{}[]":,') for block in content)
                return "".join(content) if chunk_like else "\n".join(content)

            pieces: list[str] = []
            pending_str_parts: list[str] = []

            def flush_pending_str_parts() -> None:
                if pending_str_parts:
                    pieces.append("".join(pending_str_parts))
                    pending_str_parts.clear()

            for block in content:
                if isinstance(block, str):
                    pending_str_parts.append(block)
                elif isinstance(block, dict):
                    flush_pending_str_parts()
                    text_val = block.get("text")
                    if isinstance(text_val, str):
                        pieces.append(text_val)

            flush_pending_str_parts()
            return "\n".join(pieces) if pieces else ""
        return str(content)

    async def _seed_seen_ids_from_checkpoint(
        self, stream_state: "_StreamProcessingState", thread_id: str
    ) -> None:
        """Pre-populate seen_ids with historical message IDs from the checkpoint.

        Prevents re-emitting AI text / tool calls / tool results from previous
        conversation turns when LangGraph fires a values snapshot that includes
        the full thread state loaded from the checkpointer.
        """
        checkpointer = self._effective_checkpointer
        if checkpointer is None:
            return
        try:
            check_config = {"configurable": {"thread_id": thread_id, "checkpoint_ns": ""}}
            if self._checkpoint_channel_mode == "delta" and self._agent is not None:
                from deerflow.runtime.checkpoint_mode import inject_checkpoint_mode

                inject_checkpoint_mode(check_config, "delta")
                snapshot = await self._agent.aget_state(check_config)
                for msg in (getattr(snapshot, "values", {}) or {}).get("messages", []):
                    if (msg_id := getattr(msg, "id", None)):
                        stream_state.seen_ids.add(msg_id)
                return
            aget_tuple = getattr(checkpointer, "aget_tuple", None)
            if aget_tuple is not None:
                cp_tuple = await aget_tuple(check_config)
            else:
                get_tuple = getattr(checkpointer, "get_tuple", None)
                cp_tuple = get_tuple(check_config) if get_tuple is not None else None
            if cp_tuple is None:
                return
            for msg in cp_tuple.checkpoint.get("channel_values", {}).get("messages", []):
                if (msg_id := getattr(msg, "id", None)):
                    stream_state.seen_ids.add(msg_id)
        except Exception:
            logger.debug("Failed to seed seen_ids from checkpoint (thread=%s)", thread_id, exc_info=True)

    def _seed_seen_ids_from_checkpoint_sync(
        self, stream_state: "_StreamProcessingState", thread_id: str
    ) -> None:
        """Sync variant of _seed_seen_ids_from_checkpoint for the sync stream() path."""
        checkpointer = self._effective_checkpointer
        if checkpointer is None:
            return
        try:
            check_config = {"configurable": {"thread_id": thread_id, "checkpoint_ns": ""}}
            if self._checkpoint_channel_mode == "delta" and self._agent is not None:
                from deerflow.runtime.checkpoint_mode import inject_checkpoint_mode

                inject_checkpoint_mode(check_config, "delta")
                snapshot = self._agent.get_state(check_config)
                for msg in (getattr(snapshot, "values", {}) or {}).get("messages", []):
                    if (msg_id := getattr(msg, "id", None)):
                        stream_state.seen_ids.add(msg_id)
                return
            get_tuple = getattr(checkpointer, "get_tuple", None)
            if get_tuple is None:
                return
            cp_tuple = get_tuple(check_config)
            if cp_tuple is None:
                return
            for msg in cp_tuple.checkpoint.get("channel_values", {}).get("messages", []):
                if (msg_id := getattr(msg, "id", None)):
                    stream_state.seen_ids.add(msg_id)
        except Exception:
            logger.debug("Failed to seed seen_ids from checkpoint (thread=%s)", thread_id, exc_info=True)

    def _ensure_checkpoint_compatible_sync(self, config: RunnableConfig) -> None:
        from deerflow.runtime.checkpoint_mode import ensure_checkpoint_mode_compatible

        ensure_checkpoint_mode_compatible(
            getattr(self, "_effective_checkpointer", None),
            config,
            getattr(self, "_checkpoint_channel_mode", "full"),
        )

    async def _ensure_checkpoint_compatible(self, config: RunnableConfig) -> None:
        from deerflow.runtime.checkpoint_mode import aensure_checkpoint_mode_compatible

        await aensure_checkpoint_mode_compatible(
            getattr(self, "_effective_checkpointer", None),
            config,
            getattr(self, "_checkpoint_channel_mode", "full"),
        )

    def _prepare_stream_invocation(
        self,
        message: str | HumanMessage,
        thread_id: str | None,
        **kwargs: Any,
    ) -> tuple[RunnableConfig, dict[str, Any], dict[str, Any]]:
        """Build the common LangGraph invocation payload for streaming."""
        if thread_id is None:
            thread_id = str(uuid.uuid4())

        config = self._get_runnable_config(thread_id, **kwargs)
        self._ensure_agent(config)
        if self._agent is None:
            raise RuntimeError("Agent was not initialized")

        input_images = normalize_input_image_metadata(kwargs.get("input_images"))
        if isinstance(message, HumanMessage):
            additional_kwargs = dict(message.additional_kwargs or {})
            if input_images:
                additional_kwargs[INPUT_IMAGES_KEY] = input_images
            human_message = message.model_copy(
                deep=True,
                update={"additional_kwargs": additional_kwargs},
            )
        else:
            additional_kwargs = {INPUT_IMAGES_KEY: input_images} if input_images else {}
            human_message = HumanMessage(
                content=message,
                additional_kwargs=additional_kwargs,
            )
        state: dict[str, Any] = {
            "messages": [human_message]
        }
        context = {
            "thread_id": thread_id,
            "loop_detection_scope_id": f"{thread_id}:{uuid.uuid4().hex}",
            "available_skills": (
                sorted(self._available_skills)
                if self._available_skills is not None
                else None
            ),
        }
        if self._agent_name:
            context["agent_name"] = self._agent_name
        if kwargs.get("user_id") is not None:
            context["user_id"] = str(kwargs["user_id"])
        if kwargs.get("workspace_path") is not None:
            context["workspace_path"] = str(kwargs["workspace_path"])
        return config, state, context

    @staticmethod
    def _lead_usage_from_metadata(usage: Any) -> dict[str, int]:
        """Normalize one usage_metadata snapshot into a lead-context usage dict.

        LangChain's ``usage_metadata.input_tokens`` already includes cached
        tokens (``input_token_details`` is a subset breakdown), so
        ``input_tokens + output_tokens`` is the full prompt + completion size
        of that model call — a good proxy for current thread context usage.
        """
        input_tokens = int(usage.get("input_tokens", 0) or 0)
        output_tokens = int(usage.get("output_tokens", 0) or 0)
        return {
            "input_tokens": input_tokens,
            "output_tokens": output_tokens,
            "total_tokens": input_tokens + output_tokens,
        }

    def _model_context_window(self) -> int | None:
        """Resolve the configured context window for this client's model.

        Mirrors make_lead_agent's resolution order (requested -> agent config ->
        default) so the reported window matches the model actually in use.
        Returns None when the model or its ``context_window`` is unconfigured;
        callers must treat that as "do not report usage percentage".
        """
        try:
            from deerflow.agents.lead_agent.agent import _resolve_model_name

            agent_model = (
                self._agent_config.model
                if self._agent_config is not None and self._agent_config.model
                else None
            )
            model_name = _resolve_model_name(self._model_name, agent_model)
            model_config = self._app_config.get_model_config(model_name)
            window = getattr(model_config, "context_window", None) if model_config else None
            return int(window) if window else None
        except Exception:
            logger.debug("Failed to resolve model context window", exc_info=True)
            return None

    def _end_event(
        self,
        stream_state: "_StreamProcessingState",
        *,
        stop_reason: str | None = None,
    ) -> "StreamEvent":
        """Build the terminal stream event with usage and context telemetry."""
        data: dict[str, Any] = {"usage": stream_state.cumulative_usage}
        if stop_reason:
            data["stop_reason"] = stop_reason
        # Context-window telemetry for the lead thread. ``last_usage`` is the
        # final top-level model call's usage snapshot; ``context_window`` is
        # resolved from the model config. Consumers (e.g. the ACP event
        # mapper) report a usage percentage only when both are present.
        if stream_state.last_lead_usage is not None:
            data["last_usage"] = stream_state.last_lead_usage
        context_window = self._model_context_window()
        if context_window is not None:
            data["context_window"] = context_window
        return StreamEvent(type="end", data=data)

    @staticmethod
    def _account_usage(
        stream_state: _StreamProcessingState,
        msg_id: str | None,
        usage: Any,
    ) -> dict[str, int] | None:
        """Add usage to cumulative totals if this message id was not counted."""
        if not usage:
            return None
        if msg_id and msg_id in stream_state.counted_usage_ids:
            return None
        if msg_id:
            stream_state.counted_usage_ids.add(msg_id)
        input_tokens = usage.get("input_tokens", 0) or 0
        output_tokens = usage.get("output_tokens", 0) or 0
        total_tokens = usage.get("total_tokens", 0) or 0
        stream_state.cumulative_usage["input_tokens"] += input_tokens
        stream_state.cumulative_usage["output_tokens"] += output_tokens
        stream_state.cumulative_usage["total_tokens"] += total_tokens
        return {
            "input_tokens": input_tokens,
            "output_tokens": output_tokens,
            "total_tokens": total_tokens,
        }

    def _events_from_stream_item(
        self,
        item: Any,
        stream_state: _StreamProcessingState,
    ) -> Generator[StreamEvent, None, None]:
        """Convert one LangGraph stream item into DeerFlow stream events."""
        mode: str
        chunk: Any
        is_top_level = True
        if isinstance(item, tuple) and len(item) == 3:
            # subgraphs=True: LangGraph yields (namespace_tuple, mode, chunk)
            # Namespace identifies which subgraph emitted the event; we strip it
            # so the rest of the handler treats subgraph events identically to
            # top-level events.  This is intentional — callers see a flat stream.
            namespace, mode, chunk = item
            mode = str(mode)
            is_top_level = not namespace
        elif isinstance(item, tuple) and len(item) == 2:
            mode, chunk = item
            mode = str(mode)
        else:
            mode, chunk = "values", item

        if mode == "custom":
            yield StreamEvent(type="custom", data=chunk)
            return

        if mode == "messages":
            # LangGraph ``messages`` mode emits ``(message_chunk, metadata)``.
            if isinstance(chunk, tuple) and len(chunk) == 2:
                msg_chunk, _metadata = chunk
            else:
                msg_chunk = chunk

            msg_id = getattr(msg_chunk, "id", None)

            if isinstance(msg_chunk, AIMessage):
                text = self._extract_text(msg_chunk.content)
                reasoning = msg_chunk.additional_kwargs.get("reasoning_content")
                counted_usage = self._account_usage(stream_state, msg_id, msg_chunk.usage_metadata)
                if is_top_level and msg_chunk.usage_metadata:
                    stream_state.last_lead_usage = self._lead_usage_from_metadata(
                        msg_chunk.usage_metadata
                    )

                if text or reasoning:
                    if msg_id:
                        stream_state.streamed_ids.add(msg_id)
                    evt = self._ai_text_event(msg_id, text, counted_usage, reasoning_content=reasoning)
                    evt.data["is_delta"] = True
                    yield evt

                # Tool calls are intentionally NOT emitted here. Individual streaming
                # chunks have incomplete data: only the first chunk carries name/id
                # (args still empty), and only the final chunk has complete args (but
                # name and id are empty strings that fall back to unusable defaults).
                # The values-mode handler emits tool calls once, with complete data,
                # after the node finishes.

            elif isinstance(msg_chunk, ToolMessage):
                if msg_id:
                    stream_state.streamed_ids.add(msg_id)
                yield self._tool_message_event(msg_chunk)
            return

        # mode == "values"
        if not isinstance(chunk, dict):
            return
        messages = chunk.get("messages", [])

        for msg in messages:
            msg_id = getattr(msg, "id", None)
            if msg_id and msg_id in stream_state.seen_ids:
                continue
            if msg_id:
                stream_state.seen_ids.add(msg_id)

            # Text was already streamed via ``messages`` mode.  Still emit tool
            # calls here because streaming chunks carry incomplete name/id data
            # and are skipped in the messages handler; the values snapshot has
            # the fully-populated AIMessage with correct name, id, and args.
            if msg_id and msg_id in stream_state.streamed_ids:
                if isinstance(msg, AIMessage):
                    self._account_usage(stream_state, msg_id, getattr(msg, "usage_metadata", None))
                    if is_top_level and msg.usage_metadata:
                        stream_state.last_lead_usage = self._lead_usage_from_metadata(
                            msg.usage_metadata
                        )
                    if msg.tool_calls:
                        yield self._ai_tool_calls_event(msg_id, msg.tool_calls)
                continue

            if isinstance(msg, AIMessage):
                counted_usage = self._account_usage(stream_state, msg_id, msg.usage_metadata)
                if is_top_level and msg.usage_metadata:
                    stream_state.last_lead_usage = self._lead_usage_from_metadata(
                        msg.usage_metadata
                    )
                reasoning = msg.additional_kwargs.get("reasoning_content")

                if msg.tool_calls:
                    yield self._ai_tool_calls_event(msg_id, msg.tool_calls)

                text = self._extract_text(msg.content)
                if text or reasoning:
                    yield self._ai_text_event(msg_id, text, counted_usage, reasoning_content=reasoning)

            elif isinstance(msg, ToolMessage):
                yield self._tool_message_event(msg)

        # Emit a values event for each state snapshot.
        yield StreamEvent(
            type="values",
            data={
                "title": chunk.get("title"),
                "messages": [self._serialize_message(m) for m in messages],
                "artifacts": chunk.get("artifacts", []),
                "todos": chunk.get("todos", []),
            },
        )

    # ------------------------------------------------------------------
    # Public API — threads
    # ------------------------------------------------------------------

    def list_threads(self, limit: int = 10) -> dict:
        """List the recent N threads.

        Args:
            limit: Maximum number of threads to return. Default is 10.

        Returns:
            Dict with "thread_list" key containing list of thread info dicts,
            sorted by thread creation time descending.
        """
        checkpointer = self._checkpointer
        if checkpointer is None:
            from deerflow.agents.checkpointer.provider import get_checkpointer

            checkpointer = get_checkpointer()

        thread_info_map = {}

        if not isinstance(checkpointer, BaseCheckpointSaver):
            return {"thread_list": []}

        for cp in checkpointer.list(config=None, limit=limit):
            if self._checkpoint_channel_mode == "full":
                from deerflow.runtime.checkpoint_mode import checkpoint_tuple_uses_delta

                if checkpoint_tuple_uses_delta(cp):
                    from deerflow.runtime.checkpoint_mode import (
                        CheckpointModeMismatchError,
                    )

                    raise CheckpointModeMismatchError(
                        "Checkpoint database contains delta threads; restart with "
                        "checkpoint_channel_mode=delta before listing them."
                    )
            cfg = cp.config.get("configurable", {})
            thread_id = cfg.get("thread_id")
            if not thread_id:
                continue

            ts = cp.checkpoint.get("ts")
            checkpoint_id = cfg.get("checkpoint_id")

            if thread_id not in thread_info_map:
                channel_values = cp.checkpoint.get("channel_values", {})
                thread_info_map[thread_id] = {
                    "thread_id": thread_id,
                    "created_at": ts,
                    "updated_at": ts,
                    "latest_checkpoint_id": checkpoint_id,
                    "title": channel_values.get("title"),
                }
            else:
                # Explicitly compare timestamps to ensure accuracy when iterating over unordered namespaces.
                # Treat None as "missing" and only compare when existing values are non-None.
                if ts is not None:
                    current_created = thread_info_map[thread_id]["created_at"]
                    if current_created is None or ts < current_created:
                        thread_info_map[thread_id]["created_at"] = ts

                    current_updated = thread_info_map[thread_id]["updated_at"]
                    if current_updated is None or ts > current_updated:
                        thread_info_map[thread_id]["updated_at"] = ts
                        thread_info_map[thread_id]["latest_checkpoint_id"] = checkpoint_id
                        channel_values = cp.checkpoint.get("channel_values", {})
                        thread_info_map[thread_id]["title"] = channel_values.get("title")

        threads = list(thread_info_map.values())
        if self._checkpoint_channel_mode == "delta" and threads:
            for item in threads:
                runnable_config = self._get_runnable_config(item["thread_id"])
                self._ensure_agent(runnable_config)
                if self._agent is not None:
                    from deerflow.runtime.checkpoint_state import (
                        CheckpointStateAccessor,
                    )

                    snapshot = CheckpointStateAccessor(
                        self._agent, checkpointer, "delta"
                    ).get(runnable_config)
                    item["title"] = (getattr(snapshot, "values", {}) or {}).get(
                        "title"
                    )
        threads.sort(key=lambda x: x.get("created_at") or "", reverse=True)

        return {"thread_list": threads[:limit]}

    def get_thread(self, thread_id: str) -> dict:
        """Get the complete thread record, including all node execution records.

        Args:
            thread_id: Thread ID.

        Returns:
            Dict containing the thread's full checkpoint history.
        """
        checkpointer = self._checkpointer
        if checkpointer is None:
            from deerflow.agents.checkpointer.provider import get_checkpointer

            checkpointer = get_checkpointer()

        config: RunnableConfig = {"configurable": {"thread_id": thread_id}}
        checkpoints: list[dict[str, Any]] = []

        if not isinstance(checkpointer, BaseCheckpointSaver):
            return {"thread_id": thread_id, "checkpoints": checkpoints}

        if self._checkpoint_channel_mode == "delta":
            runnable_config = self._get_runnable_config(thread_id)
            self._ensure_agent(runnable_config)
            if self._agent is None:
                return {"thread_id": thread_id, "checkpoints": checkpoints}
            timestamp_by_id = {
                (cp.config.get("configurable", {}) or {}).get("checkpoint_id"):
                cp.checkpoint.get("ts")
                for cp in checkpointer.list(config)
            }
            from deerflow.runtime.checkpoint_state import CheckpointStateAccessor

            accessor = CheckpointStateAccessor(self._agent, checkpointer, "delta")
            for snapshot in accessor.history(runnable_config):
                values = dict(getattr(snapshot, "values", {}) or {})
                if "messages" in values:
                    values["messages"] = [
                        self._serialize_message(message)
                        if hasattr(message, "content")
                        else message
                        for message in values["messages"]
                    ]
                snapshot_config = getattr(snapshot, "config", {}) or {}
                parent_config = getattr(snapshot, "parent_config", {}) or {}
                cfg = snapshot_config.get("configurable", {})
                parent_cfg = parent_config.get("configurable", {})
                checkpoints.append(
                    {
                        "checkpoint_id": cfg.get("checkpoint_id"),
                        "parent_checkpoint_id": parent_cfg.get("checkpoint_id"),
                        "ts": timestamp_by_id.get(cfg.get("checkpoint_id")),
                        "metadata": getattr(snapshot, "metadata", {}) or {},
                        "values": values,
                        "pending_writes": [],
                    }
                )
            checkpoints.reverse()
            return {"thread_id": thread_id, "checkpoints": checkpoints}

        from deerflow.runtime.checkpoint_mode import ensure_checkpoint_mode_compatible

        ensure_checkpoint_mode_compatible(checkpointer, config, "full")

        for cp in checkpointer.list(config):
            channel_values = dict(cp.checkpoint.get("channel_values", {}))
            if "messages" in channel_values:
                channel_values["messages"] = [self._serialize_message(m) if hasattr(m, "content") else m for m in channel_values["messages"]]

            cfg = cp.config.get("configurable", {})
            parent_cfg = cp.parent_config.get("configurable", {}) if cp.parent_config else {}

            checkpoints.append(
                {
                    "checkpoint_id": cfg.get("checkpoint_id"),
                    "parent_checkpoint_id": parent_cfg.get("checkpoint_id"),
                    "ts": cp.checkpoint.get("ts"),
                    "metadata": cp.metadata,
                    "values": channel_values,
                    "pending_writes": [{"task_id": w[0], "channel": w[1], "value": w[2]} for w in getattr(cp, "pending_writes", [])],
                }
            )

        # Sort globally by timestamp to prevent partial ordering issues caused by different namespaces (e.g., subgraphs)
        checkpoints.sort(key=lambda item: str(item.get("ts") or ""))

        return {"thread_id": thread_id, "checkpoints": checkpoints}

    # ------------------------------------------------------------------
    # Public API — conversation
    # ------------------------------------------------------------------

    def stream(
        self,
        message: str,
        *,
        thread_id: str | None = None,
        **kwargs,
    ) -> Generator[StreamEvent, None, None]:
        """Stream a conversation turn, yielding events incrementally.

        Each call sends one user message and yields events until the agent
        finishes its turn. A ``checkpointer`` must be provided at init time
        for multi-turn context to be preserved across calls.

        Event types align with the LangGraph SSE protocol so that
        consumers can switch between HTTP streaming and embedded mode
        without changing their event-handling logic.

        Token-level streaming
        ~~~~~~~~~~~~~~~~~~~~~
        This method subscribes to LangGraph's ``messages`` stream mode, so
        ``messages-tuple`` events for AI text are emitted as **deltas** as
        the model generates tokens, not as one cumulative dump at node
        completion.  Each delta carries a stable ``id`` — consumers that
        want the full text must accumulate ``content`` per ``id``.
        ``chat()`` already does this for you.

        Tool calls and tool results are still emitted once per logical
        message.  ``values`` events continue to carry full state snapshots
        after each graph node finishes; AI text already delivered via the
        ``messages`` stream is **not** re-synthesized from the snapshot to
        avoid duplicate deliveries.

        Why not reuse Gateway's ``run_agent``?
        ~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
        Gateway (``runtime/runs/worker.py``) has a complete streaming
        pipeline: ``run_agent`` → ``StreamBridge`` → ``sse_consumer``.  It
        looks like this client duplicates that work, but the two paths
        serve different audiences and **cannot** share execution:

        * ``run_agent`` is ``async def`` and uses ``agent.astream()``;
          this method is a sync generator using ``agent.stream()`` so
          callers can write ``for event in client.stream(...)`` without
          touching asyncio.  Bridging the two would require spinning up
          an event loop + thread per call.
        * Gateway events are JSON-serialized by ``serialize()`` for SSE
          wire transmission.  This client yields in-process stream event
          payloads directly as Python data structures (``StreamEvent``
          with ``data`` as a plain ``dict``), without the extra
          JSON/SSE serialization layer used for HTTP delivery.
        * ``StreamBridge`` is an asyncio-queue decoupling producers from
          consumers across an HTTP boundary (``Last-Event-ID`` replay,
          heartbeats, multi-subscriber fan-out).  A single in-process
          caller with a direct iterator needs none of that.

        So ``DeerFlowClient.stream()`` is a parallel, sync, in-process
        consumer of the same ``create_agent()`` factory — not a wrapper
        around Gateway.  The two paths **should** stay in sync on which
        LangGraph stream modes they subscribe to; that invariant is
        enforced by ``tests/test_client.py::test_messages_mode_emits_token_deltas``
        rather than by a shared constant, because the three layers
        (Graph, Platform SDK, HTTP) each use their own naming
        (``messages`` vs ``messages-tuple``) and cannot literally share
        a string.

        Args:
            message: User message text.
            thread_id: Thread ID for conversation context. Auto-generated if None.
            **kwargs: Override client defaults (model_name, thinking_enabled,
                plan_mode, subagent_enabled, recursion_limit).

        Yields:
            StreamEvent with one of:
            - type="values"          data={"title": str|None, "messages": [...], "artifacts": [...], "todos": [...]}
            - type="custom"          data={...}
            - type="messages-tuple"  data={"type": "ai", "content": <delta>, "id": str}
            - type="messages-tuple"  data={"type": "ai", "content": <delta>, "id": str, "usage_metadata": {...}}
            - type="messages-tuple"  data={"type": "ai", "content": "", "id": str, "tool_calls": [...]}
            - type="messages-tuple"  data={"type": "tool", "content": str, "name": str, "tool_call_id": str, "id": str}
            - type="end"             data={"usage": {"input_tokens": int, "output_tokens": int, "total_tokens": int}}
        """
        config, state, context = self._prepare_stream_invocation(message, thread_id, **kwargs)
        agent = self._agent
        if agent is None:
            raise RuntimeError("Agent was not initialized")
        stream_state = _StreamProcessingState()

        actual_thread_id = config.get("configurable", {}).get("thread_id")
        lock_id = str(actual_thread_id or uuid.uuid4())
        with checkpoint_thread_lock_sync(lock_id):
            self._ensure_checkpoint_compatible_sync(config)
            if actual_thread_id:
                self._seed_seen_ids_from_checkpoint_sync(
                    stream_state, actual_thread_id
                )
            try:
                for item in agent.stream(
                    state,
                    config=config,
                    context=context,
                    stream_mode=["values", "messages", "custom"],
                    subgraphs=True,
                ):
                    yield from self._events_from_stream_item(item, stream_state)
            except GraphRecursionError:
                logger.warning(
                    "Recursion limit reached (thread=%s) — emitting graceful final answer",
                    actual_thread_id,
                )
                context.setdefault("stop_reason", "recursion_limit")
                yield self._recursion_limit_event()

        yield self._end_event(stream_state, stop_reason=context.get("stop_reason"))

    async def astream(
        self,
        message: str | HumanMessage,
        *,
        thread_id: str | None = None,
        **kwargs,
    ) -> AsyncGenerator[StreamEvent, None]:
        """Asynchronously stream a conversation turn, yielding events incrementally.

        This mirrors :meth:`stream` but uses LangGraph's native ``astream()``
        path, making it suitable for FastAPI SSE endpoints and other asyncio
        servers that must not block the event loop while waiting for model or
        tool output.
        """
        config, state, context = self._prepare_stream_invocation(message, thread_id, **kwargs)
        agent = self._agent
        if agent is None:
            raise RuntimeError("Agent was not initialized")
        stream_state = _StreamProcessingState()

        actual_thread_id = config.get("configurable", {}).get("thread_id")
        lock_id = str(actual_thread_id or uuid.uuid4())
        async with checkpoint_thread_lock_async(lock_id):
            await self._ensure_checkpoint_compatible(config)
            if actual_thread_id:
                await self._seed_seen_ids_from_checkpoint(
                    stream_state, actual_thread_id
                )
            try:
                async for item in agent.astream(
                    state,
                    config=config,
                    context=context,
                    stream_mode=["values", "messages", "custom"],
                    subgraphs=True,
                ):
                    for event in self._events_from_stream_item(item, stream_state):
                        yield event
            except GraphRecursionError:
                logger.warning(
                    "Recursion limit reached (thread=%s) — emitting graceful final answer",
                    actual_thread_id,
                )
                context.setdefault("stop_reason", "recursion_limit")
                yield self._recursion_limit_event()

        yield self._end_event(stream_state, stop_reason=context.get("stop_reason"))

    def chat(self, message: str, *, thread_id: str | None = None, **kwargs) -> str:
        """Send a message and return the final text response.

        Convenience wrapper around :meth:`stream` that accumulates delta
        ``messages-tuple`` events per ``id`` and returns the text of the
        **last** AI message to complete.  Intermediate AI messages (e.g.
        planner drafts) are discarded — only the final id's accumulated
        text is returned.  Use :meth:`stream` directly if you need every
        delta as it arrives.

        Args:
            message: User message text.
            thread_id: Thread ID for conversation context. Auto-generated if None.
            **kwargs: Override client defaults (same as stream()).

        Returns:
            The accumulated text of the last AI message, or empty string
            if no AI text was produced.
        """
        # Per-id delta lists joined once at the end — avoids the O(n²) cost
        # of repeated ``str + str`` on a growing buffer for long responses.
        chunks: dict[str, list[str]] = {}
        last_id: str = ""
        for event in self.stream(message, thread_id=thread_id, **kwargs):
            if event.type == "messages-tuple" and event.data.get("type") == "ai":
                msg_id = event.data.get("id") or ""
                delta = event.data.get("content", "")
                if delta:
                    chunks.setdefault(msg_id, []).append(delta)
                    last_id = msg_id
        return "".join(chunks.get(last_id, ()))

    # ------------------------------------------------------------------
    # Public API — configuration queries
    # ------------------------------------------------------------------

    def list_models(self) -> dict:
        """List available models from configuration.

        Returns:
            Dict with "models" key containing list of model info dicts,
            matching the Gateway API ``ModelsListResponse`` schema.
        """
        token_usage_enabled = getattr(getattr(self._app_config, "token_usage", None), "enabled", False)
        if not isinstance(token_usage_enabled, bool):
            token_usage_enabled = False

        return {
            "models": [
                {
                    "name": model.name,
                    "model": getattr(model, "model", None),
                    "display_name": getattr(model, "display_name", None),
                    "description": getattr(model, "description", None),
                    "supports_thinking": getattr(model, "supports_thinking", False),
                    "supports_reasoning_effort": getattr(model, "supports_reasoning_effort", False),
                    "supports_vision": getattr(model, "supports_vision", False),
                    "context_window": getattr(model, "context_window", None),
                }
                for model in self._app_config.models
            ],
            "token_usage": {"enabled": token_usage_enabled},
        }

    def list_skills(self, enabled_only: bool = False) -> dict:
        """List available skills.

        Args:
            enabled_only: If True, only return enabled skills.

        Returns:
            Dict with "skills" key containing list of skill info dicts,
            matching the Gateway API ``SkillsListResponse`` schema.
        """
        from deerflow.skills.loader import load_skills

        return {
            "skills": [
                {
                    "name": s.name,
                    "description": s.description,
                    "license": s.license,
                    "category": s.category,
                    "enabled": s.enabled,
                }
                for s in load_skills(enabled_only=enabled_only)
            ]
        }

    def get_memory(self) -> dict:
        """Get current memory data.

        Returns:
            Memory data dict (see src/agents/memory/updater.py for structure).
        """
        from deerflow.agents.memory import memory_manager_lease

        with memory_manager_lease() as manager:
            return manager.get_memory(self._agent_name)

    def export_memory(self) -> dict:
        """Export current memory data for backup or transfer."""
        from deerflow.agents.memory import memory_manager_lease

        with memory_manager_lease() as manager:
            return manager.export_memory(self._agent_name)

    def import_memory(self, memory_data: dict) -> dict:
        """Import and persist full memory data."""
        from deerflow.agents.memory import memory_manager_lease

        with memory_manager_lease() as manager:
            return manager.import_memory(memory_data, self._agent_name)

    def get_model(self, name: str) -> dict | None:
        """Get a specific model's configuration by name.

        Args:
            name: Model name.

        Returns:
            Model info dict matching the Gateway API ``ModelResponse``
            schema, or None if not found.
        """
        model = self._app_config.get_model_config(name)
        if model is None:
            return None
        return {
            "name": model.name,
            "model": getattr(model, "model", None),
            "display_name": getattr(model, "display_name", None),
            "description": getattr(model, "description", None),
            "supports_thinking": getattr(model, "supports_thinking", False),
            "supports_reasoning_effort": getattr(model, "supports_reasoning_effort", False),
            "supports_vision": getattr(model, "supports_vision", False),
        }

    # ------------------------------------------------------------------
    # Public API — MCP configuration
    # ------------------------------------------------------------------

    def get_mcp_config(self) -> dict:
        """Get MCP server configurations.

        Returns:
            Dict with "mcp_servers" key mapping server name to config,
            matching the Gateway API ``McpConfigResponse`` schema.
        """
        config = get_extensions_config()
        return {"mcp_servers": {name: server.model_dump() for name, server in config.mcp_servers.items()}}

    def update_mcp_config(self, mcp_servers: dict[str, dict]) -> dict:
        """Update MCP server configurations.

        Writes to extensions_config.json and reloads the cache.

        Args:
            mcp_servers: Dict mapping server name to config dict.
                Each value should contain keys like enabled, type, command, args, env, url, etc.

        Returns:
            Dict with "mcp_servers" key, matching the Gateway API
            ``McpConfigResponse`` schema.

        Raises:
            OSError: If the config file cannot be written.
        """
        config_path = ExtensionsConfig.resolve_config_path()
        if config_path is None:
            raise FileNotFoundError("Cannot locate extensions_config.json. Set DEER_FLOW_EXTENSIONS_CONFIG_PATH or ensure it exists in the project root.")

        with get_extensions_config_lock():
            config_data = load_extensions_config_data(config_path)
            config_data["mcpServers"] = mcp_servers
            write_extensions_config_data(config_path, config_data)

            self._agent = None
            self._agent_config_key = None
            reloaded = reload_extensions_config()
        try:
            from deerflow.mcp.cache import reset_mcp_tools_cache

            reset_mcp_tools_cache()
        except Exception:
            logger.debug("Failed to reset MCP tools cache after config update", exc_info=True)
        return {"mcp_servers": {name: server.model_dump() for name, server in reloaded.mcp_servers.items()}}

    # ------------------------------------------------------------------
    # Public API — skills management
    # ------------------------------------------------------------------

    def get_skill(self, name: str) -> dict | None:
        """Get a specific skill by name.

        Args:
            name: Skill name.

        Returns:
            Skill info dict, or None if not found.
        """
        from deerflow.skills.loader import load_skills

        skill = next((s for s in load_skills(enabled_only=False) if s.name == name), None)
        if skill is None:
            return None
        return {
            "name": skill.name,
            "description": skill.description,
            "license": skill.license,
            "category": skill.category,
            "enabled": skill.enabled,
        }

    def update_skill(self, name: str, *, enabled: bool) -> dict:
        """Update a skill's enabled status.

        Args:
            name: Skill name.
            enabled: New enabled status.

        Returns:
            Updated skill info dict.

        Raises:
            ValueError: If the skill is not found.
            OSError: If the config file cannot be written.
        """
        from deerflow.skills.loader import load_skills

        skills = load_skills(enabled_only=False)
        skill = next((s for s in skills if s.name == name), None)
        if skill is None:
            raise ValueError(f"Skill '{name}' not found")

        config_path = ExtensionsConfig.resolve_config_path()
        if config_path is None:
            raise FileNotFoundError("Cannot locate extensions_config.json. Set DEER_FLOW_EXTENSIONS_CONFIG_PATH or ensure it exists in the project root.")

        with get_extensions_config_lock():
            config_data = load_extensions_config_data(config_path)
            raw_skills = config_data.setdefault("skills", {})
            if not isinstance(raw_skills, dict):
                raise ValueError("extensions skills must be a JSON object")
            raw_skill = raw_skills.get(name)
            skill_data = dict(raw_skill) if isinstance(raw_skill, dict) else {}
            skill_data["enabled"] = enabled
            raw_skills[name] = skill_data
            write_extensions_config_data(config_path, config_data)

            self._agent = None
            self._agent_config_key = None
            reload_extensions_config()
        try:
            from deerflow.agents.lead_agent.prompt import clear_skills_system_prompt_cache
            from deerflow.skills.evolution import get_evolution_store

            catalog_version = get_evolution_store().bump_catalog(
                actor="admin",
                action="skill.enabled" if enabled else "skill.disabled",
                details={"skill_name": name},
            )
            clear_skills_system_prompt_cache()
        except Exception:
            logger.warning("Failed to update Skill catalog version after changing enabled state", exc_info=True)
            catalog_version = None

        updated = next((s for s in load_skills(enabled_only=False) if s.name == name), None)
        if updated is None:
            raise RuntimeError(f"Skill '{name}' disappeared after update")
        result: dict[str, Any] = {
            "name": updated.name,
            "description": updated.description,
            "license": updated.license,
            "category": updated.category,
            "enabled": updated.enabled,
        }
        if catalog_version is not None:
            result["catalog_version"] = catalog_version
        return result

    def install_skill(self, skill_path: str | Path) -> dict:
        """Install a skill from a .skill archive (ZIP).

        Args:
            skill_path: Path to the .skill file.

        Returns:
            Dict with success, skill_name, message.

        Raises:
            FileNotFoundError: If the file does not exist.
            ValueError: If the file is invalid.
        """
        result = install_skill_from_archive(skill_path)
        try:
            from deerflow.agents.lead_agent.prompt import clear_skills_system_prompt_cache
            from deerflow.skills.evolution import get_evolution_store
            from deerflow.skills.manager import get_custom_skill_dir

            skill_name = str(result.get("skill_name") or "")
            store = get_evolution_store()
            if skill_name:
                store.bootstrap_active_skill(skill_name, get_custom_skill_dir(skill_name), actor="admin")
            result["catalog_version"] = store.bump_catalog(
                actor="admin",
                action="skill.installed",
                details={"skill_name": skill_name},
            )
            clear_skills_system_prompt_cache()
        except Exception:
            logger.warning("Failed to version installed Skill", exc_info=True)
        return result

    # ------------------------------------------------------------------
    # Public API — memory management
    # ------------------------------------------------------------------

    def reload_memory(self) -> dict:
        """Reload memory data from file, forcing cache invalidation.

        Returns:
            The reloaded memory data dict.
        """
        from deerflow.agents.memory import memory_manager_lease

        with memory_manager_lease() as manager:
            return manager.reload_memory(self._agent_name)

    def clear_memory(self) -> dict:
        """Clear all persisted memory data."""
        from deerflow.agents.memory import memory_manager_lease

        with memory_manager_lease() as manager:
            return manager.clear_memory(self._agent_name)

    def create_memory_fact(self, content: str, category: str = "context", confidence: float = 0.5) -> dict:
        """Create a single fact manually."""
        from deerflow.agents.memory import memory_manager_lease

        with memory_manager_lease() as manager:
            return manager.create_fact(
                content=content,
                category=category,
                confidence=confidence,
                agent_name=self._agent_name,
            )

    def delete_memory_fact(self, fact_id: str) -> dict:
        """Delete a single fact from memory by fact id."""
        from deerflow.agents.memory import memory_manager_lease

        with memory_manager_lease() as manager:
            return manager.delete_fact(fact_id, agent_name=self._agent_name)

    def update_memory_fact(
        self,
        fact_id: str,
        content: str | None = None,
        category: str | None = None,
        confidence: float | None = None,
    ) -> dict:
        """Update a single fact manually, preserving omitted fields."""
        from deerflow.agents.memory import memory_manager_lease

        with memory_manager_lease() as manager:
            return manager.update_fact(
                fact_id=fact_id,
                content=content,
                category=category,
                confidence=confidence,
                agent_name=self._agent_name,
            )

    def get_memory_config(self) -> dict:
        """Get memory system configuration.

        Returns:
            Memory config dict.
        """
        from deerflow.config.memory_config import get_memory_config

        config = get_memory_config()
        return config.model_dump()

    def get_memory_status(self) -> dict:
        """Get memory status: config + current data.

        Returns:
            Dict with "config" and "data" keys.
        """
        return {
            "config": self.get_memory_config(),
            "data": self.get_memory(),
        }

    # ------------------------------------------------------------------
    # Public API — file uploads
    # ------------------------------------------------------------------

    def upload_files(self, thread_id: str, files: list[str | Path]) -> dict:
        """Upload local files into a thread's uploads directory.

        For PDF, PPT, Excel, and Word files, they are also converted to Markdown.

        Args:
            thread_id: Target thread ID.
            files: List of local file paths to upload.

        Returns:
            Dict with success, files, message — matching the Gateway API
            ``UploadResponse`` schema.

        Raises:
            FileNotFoundError: If any file does not exist.
            ValueError: If any supplied path exists but is not a regular file.
        """
        from deerflow.utils.file_conversion import CONVERTIBLE_EXTENSIONS, convert_file_to_markdown

        # Validate all files upfront to avoid partial uploads.
        resolved_files: list[Path] = []
        has_convertible_file = False
        for f in files:
            p = Path(f)
            if not p.exists():
                raise FileNotFoundError(f"File not found: {f}")
            if not p.is_file():
                raise ValueError(f"Path is not a file: {f}")
            resolved_files.append(p)
            if not has_convertible_file and p.suffix.lower() in CONVERTIBLE_EXTENSIONS:
                has_convertible_file = True

        uploads_dir = ensure_uploads_dir(thread_id)
        uploaded_files: list[dict] = []

        conversion_pool = None
        if has_convertible_file:
            try:
                asyncio.get_running_loop()
            except RuntimeError:
                conversion_pool = None
            else:
                import concurrent.futures

                # Reuse one worker when already inside an event loop to avoid
                # creating a new ThreadPoolExecutor per converted file.
                conversion_pool = concurrent.futures.ThreadPoolExecutor(max_workers=1)

        def _convert_in_thread(path: Path):
            return asyncio.run(convert_file_to_markdown(path))

        try:
            for src_path in resolved_files:
                dest = copy_file_exclusive(src_path, uploads_dir)
                dest_name = dest.name

                info: dict[str, Any] = {
                    "filename": dest_name,
                    "size": str(dest.stat().st_size),
                    "path": str(dest),
                    "virtual_path": upload_virtual_path(dest_name),
                    "artifact_url": upload_artifact_url(thread_id, dest_name),
                }
                if dest_name != src_path.name:
                    info["original_filename"] = src_path.name

                if src_path.suffix.lower() in CONVERTIBLE_EXTENSIONS:
                    try:
                        if conversion_pool is not None:
                            md_path = conversion_pool.submit(_convert_in_thread, dest).result()
                        else:
                            md_path = asyncio.run(convert_file_to_markdown(dest))
                    except Exception:
                        logger.warning(
                            "Failed to convert %s to markdown",
                            src_path.name,
                            exc_info=True,
                        )
                        md_path = None

                    if md_path is not None:
                        info["markdown_file"] = md_path.name
                        info["markdown_path"] = str(uploads_dir / md_path.name)
                        info["markdown_virtual_path"] = upload_virtual_path(md_path.name)
                        info["markdown_artifact_url"] = upload_artifact_url(thread_id, md_path.name)

                uploaded_files.append(info)
        finally:
            if conversion_pool is not None:
                conversion_pool.shutdown(wait=True)

        return {
            "success": True,
            "files": uploaded_files,
            "message": f"Successfully uploaded {len(uploaded_files)} file(s)",
        }

    def list_uploads(self, thread_id: str) -> dict:
        """List files in a thread's uploads directory.

        Args:
            thread_id: Thread ID.

        Returns:
            Dict with "files" and "count" keys, matching the Gateway API
            ``list_uploaded_files`` response.
        """
        uploads_dir = get_uploads_dir(thread_id)
        result = list_files_in_dir(uploads_dir)
        return enrich_file_listing(result, thread_id)

    def delete_upload(self, thread_id: str, filename: str) -> dict:
        """Delete a file from a thread's uploads directory.

        Args:
            thread_id: Thread ID.
            filename: Filename to delete.

        Returns:
            Dict with success and message, matching the Gateway API
            ``delete_uploaded_file`` response.

        Raises:
            FileNotFoundError: If the file does not exist.
            PermissionError: If path traversal is detected.
        """
        from deerflow.utils.file_conversion import CONVERTIBLE_EXTENSIONS

        uploads_dir = get_uploads_dir(thread_id)
        return delete_file_safe(uploads_dir, filename, convertible_extensions=CONVERTIBLE_EXTENSIONS)

    # ------------------------------------------------------------------
    # Public API — artifacts
    # ------------------------------------------------------------------

    def get_artifact(self, thread_id: str, path: str) -> tuple[bytes, str]:
        """Read an artifact file produced by the agent.

        Args:
            thread_id: Thread ID.
            path: Virtual path (e.g. "mnt/user-data/outputs/file.txt").

        Returns:
            Tuple of (file_bytes, mime_type).

        Raises:
            FileNotFoundError: If the artifact does not exist.
            ValueError: If the path is invalid.
        """
        try:
            actual = get_paths().resolve_virtual_path(thread_id, path)
        except ValueError as exc:
            if "traversal" in str(exc):
                from deerflow.uploads.manager import PathTraversalError

                raise PathTraversalError("Path traversal detected") from exc
            raise
        if not actual.exists():
            raise FileNotFoundError(f"Artifact not found: {path}")
        if not actual.is_file():
            raise ValueError(f"Path is not a file: {path}")

        mime_type, _ = mimetypes.guess_type(actual)
        return actual.read_bytes(), mime_type or "application/octet-stream"
