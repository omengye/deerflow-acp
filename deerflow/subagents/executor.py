"""Subagent execution engine."""

import asyncio
import logging
import threading
import time
import uuid
from collections.abc import Callable
from concurrent.futures import Future, ThreadPoolExecutor
from concurrent.futures import TimeoutError as FuturesTimeoutError
from copy import deepcopy
from dataclasses import dataclass, field
from datetime import UTC, datetime
from enum import Enum
from typing import TYPE_CHECKING, Any, cast

from langchain.agents import create_agent
from langchain.agents.middleware import AgentMiddleware
from langchain.tools import BaseTool
from langchain_core.messages import AIMessage, HumanMessage, SystemMessage, ToolMessage
from langchain_core.runnables import RunnableConfig

from deerflow.agents.date_context import append_current_date
from deerflow.agents.middlewares.loop_detection_middleware import (
    AGENT_TERMINATION_KEY,
    TOOL_CALL_LIMIT_STOP_REASON,
)
from deerflow.agents.thread_state import AgentContext, SandboxState, ThreadDataState, ThreadState
from deerflow.config import get_app_config
from deerflow.models import aclose_chat_model, create_chat_model
from deerflow.subagents.config import SubagentConfig

if TYPE_CHECKING:
    from deerflow.tools.builtins.tool_search import DeferredToolRegistry

logger = logging.getLogger(__name__)


def _utcnow() -> datetime:
    """Return the timestamp convention used by subagent lifecycle metadata."""
    return datetime.now(UTC)


class SubagentStatus(Enum):
    """Status of a subagent execution."""

    PENDING = "pending"
    RUNNING = "running"
    COMPLETED = "completed"
    FAILED = "failed"
    CANCELLED = "cancelled"
    TIMED_OUT = "timed_out"
    LIMIT_REACHED = "limit_reached"

    @property
    def is_terminal(self) -> bool:
        return self in {
            type(self).COMPLETED,
            type(self).FAILED,
            type(self).CANCELLED,
            type(self).TIMED_OUT,
            type(self).LIMIT_REACHED,
        }


@dataclass
class SubagentResult:
    """Result of a subagent execution.

    Attributes:
        task_id: Server-generated identifier that owns this execution.
        external_task_id: Optional provider correlation ID. Provider tool-call
            IDs may repeat across parent runs and therefore never own registry
            state.
        trace_id: Trace ID for distributed tracing (links parent and subagent logs).
        status: Current status of the execution.
        result: The final or partial result message, when available.
        error: Error or incomplete-termination message, when applicable.
        termination_reason: Structured reason when execution stopped incomplete.
        started_at: When execution started.
        completed_at: When execution completed.
        ai_messages: List of complete AI messages (as dicts) generated during execution.
    """

    task_id: str
    trace_id: str
    status: SubagentStatus
    external_task_id: str | None = None
    result: str | None = None
    error: str | None = None
    termination_reason: str | None = None
    started_at: datetime | None = None
    completed_at: datetime | None = None
    ai_messages: list[dict[str, Any]] = field(default_factory=list)
    tool_receipts: list[dict[str, Any]] = field(default_factory=list)
    acceptance_verdict: dict[str, Any] | None = None
    cancel_event: threading.Event = field(default_factory=threading.Event, repr=False)
    _state_lock: threading.Lock = field(default_factory=threading.Lock, init=False, repr=False)

    def try_mark_running(self) -> bool:
        """Move a pending execution to running without reviving terminal work."""
        with self._state_lock:
            if self.status != SubagentStatus.PENDING:
                return False
            self.status = SubagentStatus.RUNNING
            self.started_at = _utcnow()
            return True

    def try_set_terminal(
        self,
        status: SubagentStatus,
        *,
        result: str | None = None,
        error: str | None = None,
        termination_reason: str | None = None,
        ai_messages: list[dict[str, Any]] | None = None,
        tool_receipts: list[dict[str, Any]] | None = None,
        acceptance_verdict: dict[str, Any] | None = None,
    ) -> bool:
        """Publish a terminal outcome once; late workers cannot overwrite it."""
        if not status.is_terminal:
            raise ValueError(f"Status {status!r} is not terminal")
        with self._state_lock:
            if self.status.is_terminal:
                return False
            self.status = status
            self.result = result
            self.error = error
            self.termination_reason = termination_reason
            # Always detach the published terminal snapshot from any list a
            # caller or worker may still hold, even when no replacement list
            # was supplied explicitly.
            self.ai_messages = list(ai_messages if ai_messages is not None else self.ai_messages)
            self.tool_receipts = list(
                tool_receipts if tool_receipts is not None else self.tool_receipts
            )
            self.acceptance_verdict = (
                dict(acceptance_verdict)
                if acceptance_verdict is not None
                else self.acceptance_verdict
            )
            self.completed_at = _utcnow()
            return True

    def update_tool_receipts(self, receipts: list[dict[str, Any]]) -> None:
        """Publish the latest immutable receipt snapshot while a task runs."""
        with self._state_lock:
            if not self.status.is_terminal:
                self.tool_receipts = [dict(receipt) for receipt in receipts]


# Global storage for background task results
_background_tasks: dict[str, SubagentResult] = {}
_background_tasks_lock = threading.Lock()

# Thread pool for background task scheduling and orchestration
_scheduler_pool = ThreadPoolExecutor(max_workers=3, thread_name_prefix="subagent-scheduler-")

# Python cannot force-stop a thread that is blocked in a synchronous tool.  A
# fixed execution pool is therefore unsafe here: three timed-out workers would
# permanently occupy all three slots.  Execute each subagent in its own daemon
# thread and quarantine timed-out workers instead.  The cap bounds the leak
# while still leaving replacement capacity for the normal three-way fan-out.
MAX_CONCURRENT_SUBAGENTS = 3
MAX_QUARANTINED_SUBAGENTS = 6
_execution_threads_lock = threading.Lock()
_active_execution_threads: set[threading.Thread] = set()
_quarantined_execution_threads: set[threading.Thread] = set()


def _start_isolated_execution(
    func: Callable[[], SubagentResult],
    *,
    execution_id: str,
) -> tuple[Future[SubagentResult], threading.Thread] | None:
    """Start one daemon worker, refusing work once the quarantine is full.

    Active workers reserve a quarantine slot up front.  Consequently, even if
    every active worker times out simultaneously, the number of live detached
    threads can never grow beyond ``MAX_QUARANTINED_SUBAGENTS``.
    """
    future: Future[SubagentResult] = Future()

    def worker() -> None:
        try:
            future.set_result(func())
        except BaseException as exc:
            future.set_exception(exc)
        finally:
            current = threading.current_thread()
            with _execution_threads_lock:
                _active_execution_threads.discard(current)
                _quarantined_execution_threads.discard(current)

    thread = threading.Thread(
        target=worker,
        name=f"subagent-exec-{execution_id[:8]}",
        daemon=True,
    )
    with _execution_threads_lock:
        live_count = len(_active_execution_threads) + len(
            _quarantined_execution_threads
        )
        if live_count >= MAX_QUARANTINED_SUBAGENTS:
            return None
        _active_execution_threads.add(thread)
    try:
        thread.start()
    except BaseException:
        with _execution_threads_lock:
            _active_execution_threads.discard(thread)
        raise
    return future, thread


def _quarantine_execution_thread(thread: threading.Thread) -> bool:
    """Detach a still-running timed-out worker from supervised capacity."""
    with _execution_threads_lock:
        if thread not in _active_execution_threads:
            return False
        _active_execution_threads.remove(thread)
        _quarantined_execution_threads.add(thread)
        return True


def _filter_tools(
    all_tools: list[BaseTool],
    allowed: list[str] | None,
    disallowed: list[str] | None,
) -> list[BaseTool]:
    """Filter tools based on subagent configuration.

    Args:
        all_tools: List of all available tools.
        allowed: Optional allowlist of tool names. If provided, only these tools are included.
        disallowed: Optional denylist of tool names. These tools are always excluded.

    Returns:
        Filtered list of tools.
    """
    filtered = all_tools

    # Apply allowlist if specified
    if allowed is not None:
        allowed_set = set(allowed)
        filtered = [t for t in filtered if t.name in allowed_set]

    # Apply denylist
    if disallowed is not None:
        disallowed_set = set(disallowed)
        filtered = [t for t in filtered if t.name not in disallowed_set]

    return filtered


def _get_model_name(config: SubagentConfig, parent_model: str | None) -> str | None:
    """Resolve the model name for a subagent.

    Args:
        config: Subagent configuration.
        parent_model: The parent agent's model name.

    Returns:
        Model name to use, or None to use default.
    """
    if config.model == "inherit":
        return parent_model
    return config.model


class SubagentExecutor:
    """Executor for running subagents."""

    def __init__(
        self,
        config: SubagentConfig,
        tools: list[BaseTool],
        parent_model: str | None = None,
        sandbox_state: SandboxState | None = None,
        thread_data: ThreadDataState | None = None,
        uploaded_files: list[dict[str, Any]] | None = None,
        thread_id: str | None = None,
        trace_id: str | None = None,
        thinking_enabled: bool = False,
        deferred_registry: "DeferredToolRegistry | None" = None,
        middlewares: list[AgentMiddleware] | None = None,
        acceptance_criteria: list[str] | None = None,
    ):
        """Initialize the executor.

        Args:
            config: Subagent configuration.
            tools: List of all available tools (will be filtered).
            parent_model: The parent agent's model name for inheritance.
            sandbox_state: Sandbox state from parent agent.
            thread_data: Thread data from parent agent.
            uploaded_files: Validated snapshot of current-run parent uploads.
            thread_id: Thread ID for sandbox operations.
            trace_id: Trace ID from parent for distributed tracing.
        """
        self.config = config
        self.parent_model = parent_model
        self.sandbox_state = sandbox_state
        self.thread_data = thread_data
        self.uploaded_files = deepcopy(uploaded_files) if uploaded_files is not None else None
        self.thread_id = thread_id
        self.thinking_enabled = thinking_enabled
        self.deferred_registry = deferred_registry
        self.middlewares = list(middlewares or [])
        self.acceptance_criteria = list(acceptance_criteria or [])
        # Generate trace_id if not provided (for top-level calls)
        self.trace_id = trace_id or str(uuid.uuid4())[:8]

        # Graph recursion budget for the run, set by ``_create_agent`` from the
        # assembled chain's real per-turn cost. ``None`` until then.
        self._run_recursion_limit: int | None = None

        # Filter tools based on config
        self.tools = _filter_tools(
            tools,
            config.tools,
            config.disallowed_tools,
        )

        logger.info(f"[trace={self.trace_id}] SubagentExecutor initialized: {config.name} with {len(self.tools)} tools")

    def _create_agent(self, stream_callback: Callable[[dict[str, Any]], None] | None = None):
        """Create the agent instance.

        Returns:
            (agent, model) tuple. The caller owns ``model`` and must close it
            via :func:`aclose_chat_model` before the surrounding event loop
            terminates (otherwise the openai httpx pool will leak SSL
            transports bound to the dying loop).
        """
        model_name = _get_model_name(self.config, self.parent_model)

        # thinking_enabled: explicit per-agent override (config.yaml
        # subagents.agents.*.thinking_enabled) takes precedence over the
        # parent's value inherited via self.thinking_enabled. None means "no
        # override configured", so it's a true three-state precedence, not a
        # simple `or` -- `False` must be able to override a `True` parent.
        effective_thinking_enabled = self.config.thinking_enabled if self.config.thinking_enabled is not None else self.thinking_enabled

        # model_settings/reasoning_effort are override-only (no parent
        # inheritance path exists for subagents today, same as model/skills)
        # so we just forward whatever is configured as extra kwargs.
        # factory.py:create_chat_model already merges **kwargs over the
        # model's own config.yaml settings, so no factory changes were
        # needed to support per-agent overrides here.
        model_kwargs: dict[str, Any] = dict(self.config.model_settings) if self.config.model_settings else {}
        if self.config.reasoning_effort is not None:
            model_kwargs["reasoning_effort"] = self.config.reasoning_effort

        model = create_chat_model(
            name=model_name,
            thinking_enabled=effective_thinking_enabled,
            disable_keepalive=True,
            **model_kwargs,
        )

        from deerflow.agents.middlewares.loop_detection_middleware import (
            LoopDetectionMiddleware,
            calibrate_loop_detection,
            count_steps_per_turn,
        )
        from deerflow.agents.middlewares.tool_error_handling_middleware import build_subagent_runtime_middlewares

        # Reuse shared middleware composition with lead agent, forwarding stream_callback
        # so TokenUsageMiddleware and LoopDetectionMiddleware can push events in the
        # isolated subagent thread (where get_stream_writer() is not available).
        middlewares = build_subagent_runtime_middlewares(lazy_init=True, stream_callback=stream_callback)
        from deerflow.agents.middlewares.runtime_headers_middleware import RuntimeHeadersMiddleware

        app_config = get_app_config()
        resolved_model_config = app_config.get_model_config(model_name or app_config.get_default_model_name())
        if resolved_model_config is not None and resolved_model_config.runtime_headers:
            middlewares.append(RuntimeHeadersMiddleware(resolved_model_config.runtime_headers))
        from deerflow.agents.middlewares.tool_receipt_middleware import (
            ToolReceiptMiddleware,
        )

        # Outermost tool wrapper so receipts observe converted errors and
        # short-circuit ToolMessages produced by inner policy middleware.
        middlewares.insert(0, ToolReceiptMiddleware())
        middlewares.extend(self.middlewares)
        if self.deferred_registry is not None:
            from deerflow.agents.middlewares.deferred_tool_filter_middleware import DeferredToolFilterMiddleware

            middlewares.append(DeferredToolFilterMiddleware())
        middlewares.append(LoopDetectionMiddleware(stream_callback=stream_callback))
        from deerflow.agents.middlewares.finish_reason_middleware import build_finish_reason_middlewares

        middlewares.extend(build_finish_reason_middlewares())

        from deerflow.subagents.report_contract import (
            build_acceptance_criteria_system_note,
            build_report_contract_section,
        )

        system_parts = [self.config.system_prompt, build_report_contract_section()]
        if self.acceptance_criteria:
            system_parts.append(build_acceptance_criteria_system_note())

        # recursion_limit counts graph super-steps, not turns. One tool-calling
        # turn costs ``count_steps_per_turn`` super-steps — model + tools + one
        # node per before/after_model middleware (currently 4 for this chain),
        # NOT a fixed 3. Size the budget to the real per-turn cost so max_turns
        # actually maps to max_turns turns, then calibrate the loop backstop to
        # the same value so it stops cleanly before the recursion limit.
        steps_per_turn = count_steps_per_turn(middlewares)
        self._run_recursion_limit = self.config.max_turns * steps_per_turn
        calibrate_loop_detection(middlewares, self._run_recursion_limit)

        agent = create_agent(
            model=model,
            tools=self.tools,
            middleware=cast(Any, middlewares),
            system_prompt=append_current_date("\n\n".join(system_parts)),
            state_schema=ThreadState,
            context_schema=AgentContext,
        )
        return agent, model

    async def _load_skills(self) -> list[Any]:
        """Load only enabled skill metadata; bodies remain lazy until read_file."""
        if self.config.skills is not None and len(self.config.skills) == 0:
            logger.info(f"[trace={self.trace_id}] Subagent {self.config.name} skills=[] — skipping skill discovery")
            return []

        try:
            from deerflow.skills.loader import load_skills

            # Use asyncio.to_thread to avoid blocking the event loop (LangGraph ASGI requirement)
            all_skills = await asyncio.to_thread(load_skills, enabled_only=True)
            logger.info(f"[trace={self.trace_id}] Subagent {self.config.name} loaded {len(all_skills)} enabled skills from disk")
        except Exception:
            logger.warning(f"[trace={self.trace_id}] Failed to load skills for subagent {self.config.name}", exc_info=True)
            return []

        if not all_skills:
            logger.info(f"[trace={self.trace_id}] Subagent {self.config.name} no enabled skills found")
            return []

        # Filter by config.skills whitelist
        if self.config.skills is not None:
            allowed = set(self.config.skills)
            skills = [s for s in all_skills if s.name in allowed]
        else:
            skills = all_skills

        return skills

    async def _build_initial_state(self, task: str) -> dict[str, Any]:
        """Build the initial state for agent execution.

        Args:
            task: The task description.

        Returns:
            Initial state dictionary.
        """
        # Expose only the skill catalog. The subagent reads a matching SKILL.md
        # through read_file when needed, avoiding eager prompt growth and stale
        # authority from unrelated skills.
        skills = await self._load_skills()
        skill_names = {skill.name for skill in skills}
        skill_section = ""
        if skill_names:
            from deerflow.agents.lead_agent.prompt import get_skills_prompt_section

            skill_section = await asyncio.to_thread(get_skills_prompt_section, skill_names)
        from deerflow.tools.builtins.tool_search import get_deferred_tools_prompt_section

        deferred_section = get_deferred_tools_prompt_section(self.deferred_registry)

        messages: list = []
        if skill_section:
            messages.append(SystemMessage(content=skill_section))
        if deferred_section:
            messages.append(SystemMessage(content=deferred_section))
        # Acceptance criteria remain untrusted task data; the system prompt
        # carries only the framework-owned authority note.
        from deerflow.subagents.report_contract import render_acceptance_criteria_block

        criteria_block = render_acceptance_criteria_block(self.acceptance_criteria)
        task_content = f"{task}\n\n{criteria_block}" if criteria_block else task
        messages.append(HumanMessage(content=task_content))

        state: dict[str, Any] = {
            "messages": messages,
        }

        # A subagent may have a narrower Skill allowlist than its parent.  Do
        # not reuse the parent's sandbox snapshot in that case; lazy sandbox
        # acquisition below will bind the subagent's own projection revision.
        inherited_skills = (
            self.sandbox_state.get("available_skills")
            if self.sandbox_state is not None
            else None
        )
        same_skill_policy = (
            (inherited_skills is None and self.config.skills is None)
            or (
                inherited_skills is not None
                and self.config.skills is not None
                and set(inherited_skills) == set(self.config.skills)
            )
        )
        if self.sandbox_state is not None and same_skill_policy:
            state["sandbox"] = self.sandbox_state
        if self.thread_data is not None:
            state["thread_data"] = self.thread_data
        if self.uploaded_files is not None:
            state["uploaded_files"] = deepcopy(self.uploaded_files)

        return state

    async def _aexecute(
        self,
        task: str,
        result_holder: SubagentResult | None = None,
        stream_callback: Callable[[dict[str, Any]], None] | None = None,
    ) -> SubagentResult:
        """Execute a task asynchronously.

        Args:
            task: The task description for the subagent.
            result_holder: Optional pre-created result object to update during execution.
            stream_callback: Optional callback invoked with each LLM token chunk as it
                arrives.  The callback receives a dict with ``type="token_chunk"`` and
                a ``content`` field containing the raw chunk content (str or list).
                This allows the caller (task_tool) to forward tokens to the SSE stream
                in real time without waiting for the next polling cycle.

        Returns:
            SubagentResult with the execution result.
        """
        if result_holder is not None:
            # Use the provided result holder (for async execution with real-time updates)
            result = result_holder
        else:
            # Create a new result for synchronous execution
            task_id = str(uuid.uuid4())[:8]
            result = SubagentResult(
                task_id=task_id,
                trace_id=self.trace_id,
                status=SubagentStatus.RUNNING,
                started_at=_utcnow(),
            )

        # Build AI-message output off-object and publish it together with the
        # terminal state.  A timed-out/cancelled worker may continue briefly;
        # mutating result.ai_messages in that window would otherwise make an
        # already-terminal result keep changing underneath pollers.
        captured_ai_messages = list(result.ai_messages)

        chat_model: Any = None
        deferred_registry_set = False
        try:
            if self.deferred_registry is not None:
                from deerflow.tools.builtins.tool_search import set_deferred_registry

                set_deferred_registry(self.deferred_registry)
                deferred_registry_set = True
            agent, chat_model = self._create_agent(stream_callback=stream_callback)
            state = await self._build_initial_state(task)

            # Budget sized to the chain's real per-turn super-step cost in
            # ``_create_agent`` (max_turns × steps_per_turn) so max_turns maps to
            # actual turns. Fall back defensively if the agent wasn't built here.
            run_config: RunnableConfig = {
                "recursion_limit": self._run_recursion_limit or self.config.max_turns * 4,
            }
            context: AgentContext = {}
            if self.thread_id:
                run_config["configurable"] = {"thread_id": self.thread_id}
                context["thread_id"] = self.thread_id
            context["available_skills"] = (
                list(self.config.skills)
                if self.config.skills is not None
                else None
            )

            logger.info(f"[trace={self.trace_id}] Subagent {self.config.name} starting async execution with max_turns={self.config.max_turns}")

            # Notify caller that execution has truly begun (not just queued).
            if stream_callback is not None:
                try:
                    stream_callback({
                        "type": "subagent_started",
                        "name": self.config.name,
                        "trace_id": self.trace_id,
                    })
                except Exception:
                    logger.debug(f"[trace={self.trace_id}] stream_callback raised on subagent_started, ignoring", exc_info=True)

            # Use stream_mode=["values", "messages"] so we get both:
            #   - "values" chunks: full state snapshots used for ai_messages aggregation
            #   - "messages" chunks: individual LLM token deltas for real-time streaming
            final_state = None

            # Pre-check: bail out immediately if already cancelled before streaming starts
            if result.cancel_event.is_set():
                logger.info(f"[trace={self.trace_id}] Subagent {self.config.name} cancelled before streaming")
                result.try_set_terminal(
                    SubagentStatus.CANCELLED,
                    error="Cancelled by user",
                    ai_messages=captured_ai_messages,
                )
                return result

            async for mode, chunk in agent.astream(  # type: ignore[arg-type]
                state,
                config=run_config,
                context=context,
                stream_mode=["values", "messages"],
            ):
                # Cooperative cancellation: check if parent requested stop.
                # Note: cancellation is only detected at astream iteration boundaries,
                # so long-running tool calls within a single iteration will not be
                # interrupted until the next chunk is yielded.
                if result.cancel_event.is_set():
                    logger.info(f"[trace={self.trace_id}] Subagent {self.config.name} cancelled by parent")
                    result.try_set_terminal(
                        SubagentStatus.CANCELLED,
                        error="Cancelled by user",
                        ai_messages=captured_ai_messages,
                    )
                    return result

                if mode == "messages":
                    # chunk is a tuple (message_chunk, metadata) from LangGraph messages mode
                    msg_chunk = chunk[0] if isinstance(chunk, tuple) else chunk
                    if stream_callback is not None:
                        try:
                            # LLM token stream — content may be str or list of blocks
                            if isinstance(msg_chunk, AIMessage) and msg_chunk.content:
                                content = msg_chunk.content
                                if isinstance(content, list):
                                    # Split thinking blocks from text blocks
                                    for block in content:
                                        if isinstance(block, dict):
                                            if block.get("type") == "thinking" and block.get("thinking"):
                                                stream_callback({"type": "thinking_chunk", "thinking": block["thinking"]})
                                            elif block.get("type") == "text" and block.get("text"):
                                                stream_callback({"type": "token_chunk", "content": block["text"]})
                                        elif isinstance(block, str) and block:
                                            stream_callback({"type": "token_chunk", "content": block})
                                else:
                                    stream_callback({"type": "token_chunk", "content": content})
                            # Tool call invocation (args may arrive incrementally across chunks)
                            if isinstance(msg_chunk, AIMessage) and msg_chunk.tool_calls:
                                for tc in msg_chunk.tool_calls:
                                    stream_callback({"type": "tool_call_chunk", "tool_call": tc})
                            # Tool execution result
                            if isinstance(msg_chunk, ToolMessage):
                                tool_result_event: dict[str, Any] = {
                                    "type": "tool_result_chunk",
                                    "tool_call_id": msg_chunk.tool_call_id,
                                    "name": getattr(msg_chunk, "name", None),
                                    "content": msg_chunk.content,
                                    "status": getattr(msg_chunk, "status", None),
                                }
                                from deerflow.tools.builtins.present_file_tool import (
                                    PRESENTED_ARTIFACTS_KEY,
                                )

                                presented = (msg_chunk.additional_kwargs or {}).get(
                                    PRESENTED_ARTIFACTS_KEY
                                )
                                if (
                                    msg_chunk.name == "present_files"
                                    and isinstance(presented, list)
                                    and all(isinstance(path, str) for path in presented)
                                ):
                                    tool_result_event["presented_artifacts"] = presented
                                stream_callback(tool_result_event)
                        except Exception:
                            logger.debug(f"[trace={self.trace_id}] stream_callback raised, ignoring", exc_info=True)
                    # Don't update final_state from messages chunks
                    continue

                # mode == "values": full state snapshot — one per completed graph node
                final_state = chunk

                from deerflow.agents.middlewares.tool_receipt import (
                    extract_tool_receipts,
                )

                result.update_tool_receipts(
                    extract_tool_receipts(list(chunk.get("messages", [])))
                )

                # Emit turn_complete so callers can track agent progress
                if stream_callback is not None:
                    try:
                        messages_so_far = chunk.get("messages", [])
                        stream_callback({
                            "type": "turn_complete",
                            "message_count": len(messages_so_far),
                        })
                    except Exception:
                        logger.debug(f"[trace={self.trace_id}] stream_callback raised on turn_complete, ignoring", exc_info=True)

                # Extract AI messages from the current state
                messages = chunk.get("messages", [])
                if messages:
                    last_message = messages[-1]
                    # Check if this is a new AI message
                    if isinstance(last_message, AIMessage):
                        # Convert message to dict for serialization
                        message_dict = last_message.model_dump()
                        # Only add if it's not already in the list (avoid duplicates)
                        # Check by comparing message IDs if available, otherwise compare full dict
                        message_id = message_dict.get("id")
                        is_duplicate = False
                        if message_id:
                            is_duplicate = any(msg.get("id") == message_id for msg in captured_ai_messages)
                        else:
                            is_duplicate = message_dict in captured_ai_messages

                        if not is_duplicate:
                            captured_ai_messages.append(message_dict)
                            logger.info(f"[trace={self.trace_id}] Subagent {self.config.name} captured AI message #{len(captured_ai_messages)}")

            logger.info(f"[trace={self.trace_id}] Subagent {self.config.name} completed async execution")

            final_result: str
            last_ai_message: AIMessage | None = None
            if final_state is None:
                logger.warning(f"[trace={self.trace_id}] Subagent {self.config.name} no final state")
                final_result = "No response generated"
            else:
                # Extract the final message - find the last AIMessage
                messages = final_state.get("messages", [])
                logger.info(f"[trace={self.trace_id}] Subagent {self.config.name} final messages count: {len(messages)}")

                # Find the last AIMessage in the conversation
                for msg in reversed(messages):
                    if isinstance(msg, AIMessage):
                        last_ai_message = msg
                        break

                if last_ai_message is not None:
                    content = last_ai_message.content
                    # Handle both str and list content types for the final result
                    if isinstance(content, str):
                        final_result = content
                    elif isinstance(content, list):
                        # Extract text from list of content blocks for final result only.
                        # Concatenate raw string chunks directly, but preserve separation
                        # between full text blocks for readability.
                        text_parts = []
                        pending_str_parts = []
                        for block in content:
                            if isinstance(block, str):
                                pending_str_parts.append(block)
                            elif isinstance(block, dict):
                                if pending_str_parts:
                                    text_parts.append("".join(pending_str_parts))
                                    pending_str_parts.clear()
                                text_val = block.get("text")
                                if isinstance(text_val, str):
                                    text_parts.append(text_val)
                        if pending_str_parts:
                            text_parts.append("".join(pending_str_parts))
                        final_result = "\n".join(text_parts) if text_parts else "No text content in response"
                    else:
                        final_result = str(content)
                elif messages:
                    # Fallback: use the last message if no AIMessage found
                    last_message = messages[-1]
                    logger.warning(f"[trace={self.trace_id}] Subagent {self.config.name} no AIMessage found, using last message: {type(last_message)}")
                    raw_content = last_message.content if hasattr(last_message, "content") else str(last_message)
                    if isinstance(raw_content, str):
                        final_result = raw_content
                    elif isinstance(raw_content, list):
                        parts = []
                        pending_str_parts = []
                        for block in raw_content:
                            if isinstance(block, str):
                                pending_str_parts.append(block)
                            elif isinstance(block, dict):
                                if pending_str_parts:
                                    parts.append("".join(pending_str_parts))
                                    pending_str_parts.clear()
                                text_val = block.get("text")
                                if isinstance(text_val, str):
                                    parts.append(text_val)
                        if pending_str_parts:
                            parts.append("".join(pending_str_parts))
                        final_result = "\n".join(parts) if parts else "No text content in response"
                    else:
                        final_result = str(raw_content)
                else:
                    logger.warning(f"[trace={self.trace_id}] Subagent {self.config.name} no messages in final state")
                    final_result = "No response generated"

            termination_reason = context.get("stop_reason")
            termination_message: str | None = None
            if last_ai_message is not None:
                marker = (last_ai_message.additional_kwargs or {}).get(
                    AGENT_TERMINATION_KEY
                )
                if isinstance(marker, dict):
                    termination_reason = termination_reason or marker.get("reason")
                    marker_message = marker.get("message")
                    if isinstance(marker_message, str) and marker_message:
                        termination_message = marker_message

            limit_reasons = {
                TOOL_CALL_LIMIT_STOP_REASON,
                "model_length_capped",
                "recursion_limit",
            }

            from deerflow.agents.middlewares.tool_receipt import (
                extract_citing_turn_receipts,
                extract_tool_receipts,
            )
            from deerflow.subagents.acceptance_checks import (
                check_acceptance_criteria,
            )

            final_messages = list((final_state or {}).get("messages", []))
            terminal_receipts = extract_citing_turn_receipts(final_messages)
            if terminal_receipts is None:
                terminal_receipts = extract_tool_receipts(final_messages)
            acceptance_verdict = (
                check_acceptance_criteria(
                    self.acceptance_criteria,
                    thread_data=(final_state or {}).get("thread_data")
                    or self.thread_data,
                    messages=final_messages,
                )
                if self.acceptance_criteria
                else None
            )
            if termination_reason in limit_reasons:
                result.try_set_terminal(
                    SubagentStatus.LIMIT_REACHED,
                    result=final_result,
                    error=termination_message
                    or f"Subagent stopped before completion ({termination_reason})",
                    termination_reason=str(termination_reason),
                    ai_messages=captured_ai_messages,
                    tool_receipts=terminal_receipts,
                    acceptance_verdict=acceptance_verdict,
                )
            else:
                result.try_set_terminal(
                    SubagentStatus.COMPLETED,
                    result=final_result,
                    ai_messages=captured_ai_messages,
                    tool_receipts=terminal_receipts,
                    acceptance_verdict=acceptance_verdict,
                )

        except Exception as e:
            logger.exception(f"[trace={self.trace_id}] Subagent {self.config.name} async execution failed")
            result.try_set_terminal(
                SubagentStatus.FAILED,
                error=str(e),
                ai_messages=captured_ai_messages,
            )
        finally:
            if deferred_registry_set:
                from deerflow.tools.builtins.tool_search import reset_deferred_registry

                reset_deferred_registry()
            # execute() runs us inside ``asyncio.run`` on a worker thread; the
            # loop closes immediately after we return. Drain the model's httpx
            # pool first so no SSL transport survives into the post-close GC
            # sweep — that is the documented trigger for the
            # ``RuntimeError: Event loop is closed`` chain seen during the
            # parent agent's later LLM streaming on the main loop.
            await aclose_chat_model(chat_model)

        return result

    async def aexecute(
        self,
        task: str,
        result_holder: SubagentResult | None = None,
        stream_callback: Callable[[dict[str, Any]], None] | None = None,
    ) -> SubagentResult:
        """Execute the subagent on the caller's event loop.

        Prefer this entry point from async code: it avoids the worker-thread
        ``asyncio.run`` round-trip used by :meth:`execute`, which otherwise
        leaves an isolated event loop on the stack and is the documented
        trigger for cross-loop ``Event loop is closed`` failures in the
        parent agent's httpx connection pool.
        """
        return await self._aexecute(task, result_holder, stream_callback)

    def execute(
        self,
        task: str,
        result_holder: SubagentResult | None = None,
        stream_callback: Callable[[dict[str, Any]], None] | None = None,
    ) -> SubagentResult:
        """Execute synchronously via ``asyncio.run``.

        Intended for callers that have no running event loop (e.g. CLI
        scripts and the isolated daemon worker invoked by
        :meth:`execute_async`).  Async callers must use :meth:`aexecute`
        instead — calling this from a running loop would either deadlock or
        force a fresh isolated loop, which is precisely what previously
        poisoned the parent's httpx pool.
        """
        try:
            running_loop = asyncio.get_running_loop()
        except RuntimeError:
            running_loop = None

        if running_loop is not None and running_loop.is_running():
            raise RuntimeError(
                "SubagentExecutor.execute() cannot be called from a running "
                "event loop; use `await executor.aexecute(...)` instead."
            )

        try:
            return asyncio.run(self._aexecute(task, result_holder, stream_callback))
        except Exception as e:
            logger.exception(f"[trace={self.trace_id}] Subagent {self.config.name} execution failed")
            # Create a result with error if we don't have one
            if result_holder is not None:
                result = result_holder
            else:
                result = SubagentResult(
                    task_id=str(uuid.uuid4())[:8],
                    trace_id=self.trace_id,
                    status=SubagentStatus.RUNNING,
                    started_at=_utcnow(),
                )
            result.try_set_terminal(SubagentStatus.FAILED, error=str(e))
            return result

    def execute_async(
        self,
        task: str,
        task_id: str | None = None,
        stream_callback: Callable[[dict[str, Any]], None] | None = None,
    ) -> str:
        """Start a task execution in the background.

        Args:
            task: The task description for the subagent.
            task_id: Optional external correlation ID for logs and client
                events. It is never used as the process-wide registry key.
            stream_callback: Optional callback for real-time LLM token chunks.

        Returns:
            Server-generated execution ID used to control this execution.
        """
        execution_id = str(uuid.uuid4())

        # Create initial pending result
        result = SubagentResult(
            task_id=execution_id,
            trace_id=self.trace_id,
            status=SubagentStatus.PENDING,
            external_task_id=task_id,
        )

        logger.info(
            "[trace=%s] Subagent %s starting async execution, execution_id=%s, external_task_id=%s, timeout=%ss",
            self.trace_id,
            self.config.name,
            execution_id,
            task_id,
            self.config.timeout_seconds,
        )

        with _background_tasks_lock:
            _background_tasks[execution_id] = result

        # Submit to scheduler pool
        def run_task():
            with _background_tasks_lock:
                if _background_tasks.get(execution_id) is not result:
                    logger.debug("Execution %s was removed before it could start", execution_id)
                    return
                result.try_mark_running()

            try:
                # A dedicated daemon thread prevents a timed-out blocking tool
                # from poisoning a shared ThreadPoolExecutor slot forever.
                started = _start_isolated_execution(
                    lambda: self.execute(task, result, stream_callback),
                    execution_id=execution_id,
                )
                if started is None:
                    result.try_set_terminal(
                        SubagentStatus.FAILED,
                        error=(
                            "Subagent execution capacity is exhausted because "
                            f"{MAX_QUARANTINED_SUBAGENTS} worker threads are still "
                            "running. Restart the service or wait for blocked tools "
                            "to return before retrying."
                        ),
                    )
                    return
                execution_future, execution_thread = started
                deadline = time.monotonic() + self.config.timeout_seconds
                while True:
                    if result.cancel_event.is_set() and result.status.is_terminal:
                        _quarantine_execution_thread(execution_thread)
                        return
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        logger.error(f"[trace={self.trace_id}] Subagent {self.config.name} execution timed out after {self.config.timeout_seconds}s")
                        result.try_set_terminal(
                            SubagentStatus.TIMED_OUT,
                            error=f"Execution timed out after {self.config.timeout_seconds} seconds",
                        )
                        result.cancel_event.set()
                        if _quarantine_execution_thread(execution_thread):
                            logger.warning(
                                "[trace=%s] Quarantined timed-out subagent worker %s; "
                                "in-process synchronous tools cannot be force-stopped",
                                self.trace_id,
                                execution_thread.name,
                            )
                        return
                    try:
                        exec_result = execution_future.result(
                            timeout=min(0.25, remaining)
                        )
                    except FuturesTimeoutError:
                        continue
                    # The normal path returns ``result`` itself. Keep this
                    # defensive copy for custom executors, while the one-shot
                    # terminal transition prevents a late worker from replacing
                    # a timeout/cancellation outcome.
                    if exec_result is not result and exec_result.status.is_terminal:
                        result.try_set_terminal(
                            exec_result.status,
                            result=exec_result.result,
                            error=exec_result.error,
                            ai_messages=exec_result.ai_messages,
                        )
                    return
            except Exception as e:
                logger.exception(f"[trace={self.trace_id}] Subagent {self.config.name} async execution failed")
                result.try_set_terminal(SubagentStatus.FAILED, error=str(e))

        try:
            _scheduler_pool.submit(run_task)
        except Exception:
            # Registration happens before submission so a very fast worker can
            # always find its result. If the executor is shutting down or
            # broken, the caller never receives this execution ID; do not leave
            # an unreachable PENDING entry in the process-wide registry.
            with _background_tasks_lock:
                if _background_tasks.get(execution_id) is result:
                    _background_tasks.pop(execution_id, None)
            raise
        return execution_id

def request_cancel_background_task(task_id: str) -> None:
    """Signal a running background task to stop.

    Sets the cancel_event on the task, which is checked cooperatively
    by ``_aexecute`` during ``agent.astream()`` iteration.  This allows
    subagent threads — which cannot be force-killed via ``Future.cancel()``
    — to stop at the next iteration boundary.

    Args:
        task_id: The task ID to cancel.
    """
    with _background_tasks_lock:
        result = _background_tasks.get(task_id)
        if result is not None:
            result.cancel_event.set()
            logger.info("Requested cancellation for background task %s", task_id)


def finalize_cancelled_background_task(
    task_id: str,
    *,
    status: SubagentStatus,
    error: str,
) -> bool:
    """Cancel and publish a terminal owner-side outcome in one operation.

    A worker thread cannot be force-killed while it is inside a blocking tool,
    but the caller that owns the registry entry must still be able to stop
    waiting and release that entry.  ``try_set_terminal`` fences all later
    worker writes, so removal is safe even while cooperative cancellation is
    still propagating inside the worker.
    """
    if status not in {SubagentStatus.CANCELLED, SubagentStatus.TIMED_OUT}:
        raise ValueError("Owner-side cancellation must be cancelled or timed_out")
    with _background_tasks_lock:
        result = _background_tasks.get(task_id)
        if result is None:
            return False
        result.cancel_event.set()
        return result.try_set_terminal(status, error=error)


def get_background_task_result(task_id: str) -> SubagentResult | None:
    """Get the result of a background task.

    Args:
        task_id: The task ID returned by execute_async.

    Returns:
        SubagentResult if found, None otherwise.
    """
    with _background_tasks_lock:
        return _background_tasks.get(task_id)


def list_background_tasks() -> list[SubagentResult]:
    """List all background tasks.

    Returns:
        List of all SubagentResult instances.
    """
    with _background_tasks_lock:
        return list(_background_tasks.values())


def cleanup_background_task(task_id: str) -> None:
    """Remove a completed task from background tasks.

    Should be called by task_tool after it finishes polling and returns the result.
    This prevents memory leaks from accumulated completed tasks.

    Only removes tasks that are in a terminal state
    to avoid race conditions with the background executor still updating the task entry.

    Args:
        task_id: The task ID to remove.
    """
    with _background_tasks_lock:
        result = _background_tasks.get(task_id)
        if result is None:
            # Nothing to clean up; may have been removed already.
            logger.debug("Requested cleanup for unknown background task %s", task_id)
            return

        # Only clean up tasks that are in a terminal state to avoid races with
        # the background executor still updating the task entry.
        is_terminal_status = result.status.is_terminal
        if is_terminal_status or result.completed_at is not None:
            del _background_tasks[task_id]
            logger.debug("Cleaned up background task: %s", task_id)
        else:
            logger.debug(
                "Skipping cleanup for non-terminal background task %s (status=%s)",
                task_id,
                result.status.value if hasattr(result.status, "value") else result.status,
            )
