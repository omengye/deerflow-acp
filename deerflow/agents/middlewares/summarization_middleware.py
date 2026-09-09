"""Summarization middleware extensions for DeerFlow."""

from __future__ import annotations

import logging
import threading
import time
from collections.abc import Collection
from dataclasses import dataclass
from typing import Any, Protocol, runtime_checkable

from langchain.agents import AgentState
from langchain.agents.middleware import SummarizationMiddleware
from langchain.chat_models import BaseChatModel
from langchain_core.messages import AIMessage, AnyMessage, HumanMessage, RemoveMessage, ToolMessage
from langchain_core.messages.utils import get_buffer_string, trim_messages
from langgraph.config import get_config
from langgraph.graph.message import REMOVE_ALL_MESSAGES
from langgraph.runtime import Runtime

from deerflow.agents.middlewares.input_sanitization_middleware import is_genuine_user_message

logger = logging.getLogger(__name__)


class _SummarizationFailed(Exception):
    """Raised internally when a summarization model invocation fails.

    Distinguishes a genuine invocation failure (network/provider error) from a
    successful-but-uninteresting summary, so callers can never mistake an
    error string for real summary content — the bug this middleware exists to
    avoid: the upstream `SummarizationMiddleware` catches invocation errors
    and returns `f"Error generating summary: {e!s}"` as if it were a valid
    summary, which then gets used to replace (i.e. destroy) the conversation
    history it failed to summarize.
    """


@dataclass(frozen=True)
class SummarizationEvent:
    """Context emitted before conversation history is summarized away."""

    messages_to_summarize: tuple[AnyMessage, ...]
    preserved_messages: tuple[AnyMessage, ...]
    thread_id: str | None
    agent_name: str | None
    runtime: Runtime


@runtime_checkable
class BeforeSummarizationHook(Protocol):
    """Hook invoked before summarization removes messages from state."""

    def __call__(self, event: SummarizationEvent) -> None: ...


def _resolve_thread_id(runtime: Runtime) -> str | None:
    """Resolve the current thread ID from runtime context or LangGraph config."""
    thread_id = runtime.context.get("thread_id") if runtime.context else None
    if thread_id is None:
        try:
            config_data = get_config()
        except RuntimeError:
            return None
        thread_id = config_data.get("configurable", {}).get("thread_id")
    return thread_id


def _resolve_agent_name(runtime: Runtime) -> str | None:
    """Resolve the current agent name from runtime context or LangGraph config."""
    agent_name = runtime.context.get("agent_name") if runtime.context else None
    if agent_name is None:
        try:
            config_data = get_config()
        except RuntimeError:
            return None
        agent_name = config_data.get("configurable", {}).get("agent_name")
    return agent_name


def _tool_call_path(tool_call: dict[str, Any]) -> str | None:
    """Best-effort extraction of a file path argument from a read_file-like tool call."""
    args = tool_call.get("args") or {}
    if not isinstance(args, dict):
        return None
    for key in ("path", "file_path", "filepath"):
        value = args.get(key)
        if isinstance(value, str) and value:
            return value
    return None


def _clone_ai_message(
    message: AIMessage,
    tool_calls: list[dict[str, Any]],
    *,
    content: Any | None = None,
) -> AIMessage:
    """Clone an AIMessage while replacing its tool_calls list and optional content."""
    update: dict[str, Any] = {"tool_calls": tool_calls}
    if content is not None:
        update["content"] = content
    return message.model_copy(update=update)


@dataclass
class _SkillBundle:
    """Skill-related tool calls and tool results associated with one AIMessage."""

    ai_index: int
    skill_tool_indices: tuple[int, ...]
    skill_tool_call_ids: frozenset[str]
    skill_tool_tokens: int
    skill_key: str


class DeerFlowSummarizationMiddleware(SummarizationMiddleware):
    """Summarization middleware with pre-compression hook dispatch and skill rescue."""

    def __init__(
        self,
        *args,
        skills_container_path: str | None = None,
        skill_file_read_tool_names: Collection[str] | None = None,
        before_summarization: list[BeforeSummarizationHook] | None = None,
        preserve_recent_skill_count: int = 5,
        preserve_recent_skill_tokens: int = 25_000,
        preserve_recent_skill_tokens_per_skill: int = 5_000,
        fallback_model_name: str | None = None,
        max_consecutive_failures: int = 3,
        circuit_recovery_timeout_sec: int = 60,
        **kwargs,
    ) -> None:
        super().__init__(*args, **kwargs)
        self._skills_container_path = skills_container_path or "/mnt/skills"
        self._skill_file_read_tool_names = frozenset(skill_file_read_tool_names or {"read_file", "read", "view", "cat"})
        self._before_summarization_hooks = before_summarization or []
        self._preserve_recent_skill_count = max(0, preserve_recent_skill_count)
        self._preserve_recent_skill_tokens = max(0, preserve_recent_skill_tokens)
        self._preserve_recent_skill_tokens_per_skill = max(0, preserve_recent_skill_tokens_per_skill)
        # Tier 2 fallback: a model distinct from the primary summarization
        # model (typically the run's own model), tried only once the primary
        # model has failed. Built lazily — most turns never need it.
        self._fallback_model_name = fallback_model_name
        self._fallback_model: BaseChatModel | None = None
        # Tier 3 bookkeeping: once summarization has failed this many times in
        # a row, stop calling any LLM and degrade straight to the
        # deterministic placeholder — implemented as a half-open circuit
        # breaker (mirrors LLMErrorHandlingMiddleware) so the middleware can
        # recover once the provider comes back, instead of latching open
        # forever for the lifetime of this long-lived, cached-per-config
        # instance.
        self._max_consecutive_failures = max(1, max_consecutive_failures)
        self._circuit_recovery_timeout_sec = max(1, circuit_recovery_timeout_sec)
        self._circuit_lock = threading.Lock()
        self._circuit_failure_count = 0
        self._circuit_open_until = 0.0
        self._circuit_state = "closed"
        self._circuit_probe_in_flight = False

    def before_model(self, state: AgentState, runtime: Runtime) -> dict | None:
        try:
            return self._maybe_summarize(state, runtime)
        except Exception:
            logger.exception("Summarization before_model hook failed unexpectedly; skipping compression for this turn")
            return None

    async def abefore_model(self, state: AgentState, runtime: Runtime) -> dict | None:
        try:
            return await self._amaybe_summarize(state, runtime)
        except Exception:
            logger.exception("Summarization abefore_model hook failed unexpectedly; skipping compression for this turn")
            return None

    def _maybe_summarize(self, state: AgentState, runtime: Runtime) -> dict | None:
        messages = state["messages"]
        self._ensure_message_ids(messages)

        total_tokens = self.token_counter(messages)
        if not self._should_summarize(messages, total_tokens):
            return None

        cutoff_index = self._determine_cutoff_index(messages)
        if cutoff_index <= 0:
            return None

        messages_to_summarize, preserved_messages = self._partition_with_skill_rescue(messages, cutoff_index)
        new_messages = self._summarize_with_tiers(messages_to_summarize)
        # Hooks fire only once we know we're committed to returning the
        # deletion instruction below, so a hook that queues messages for
        # memory (e.g. memory_flush_hook) never runs ahead of the decision
        # it's reacting to.
        self._fire_hooks(messages_to_summarize, preserved_messages, runtime)

        return {
            "messages": [
                RemoveMessage(id=REMOVE_ALL_MESSAGES),
                *new_messages,
                *preserved_messages,
            ]
        }

    async def _amaybe_summarize(self, state: AgentState, runtime: Runtime) -> dict | None:
        messages = state["messages"]
        self._ensure_message_ids(messages)

        total_tokens = self.token_counter(messages)
        if not self._should_summarize(messages, total_tokens):
            return None

        cutoff_index = self._determine_cutoff_index(messages)
        if cutoff_index <= 0:
            return None

        messages_to_summarize, preserved_messages = self._partition_with_skill_rescue(messages, cutoff_index)
        new_messages = await self._asummarize_with_tiers(messages_to_summarize)
        # See _maybe_summarize: hooks fire only after the deletion instruction
        # is already decided.
        self._fire_hooks(messages_to_summarize, preserved_messages, runtime)

        return {
            "messages": [
                RemoveMessage(id=REMOVE_ALL_MESSAGES),
                *new_messages,
                *preserved_messages,
            ]
        }

    def _get_fallback_model(self) -> BaseChatModel | None:
        """Lazily construct the tier-2 fallback model, only once tier 1 fails."""
        if self._fallback_model_name is None:
            return None
        if self._fallback_model is None:
            from deerflow.models import create_chat_model

            try:
                self._fallback_model = create_chat_model(name=self._fallback_model_name, thinking_enabled=False, disable_keepalive=True)
            except Exception:
                logger.exception("Failed to build fallback summarization model '%s'", self._fallback_model_name)
                return None
        return self._fallback_model

    def _circuit_check(self) -> bool:
        """Return True if the summarization circuit is OPEN (skip the LLM entirely).

        Half-open breaker, mirroring `LLMErrorHandlingMiddleware`: once
        tripped, calls fast-fail straight to the Tier 3 placeholder until the
        recovery timeout elapses, then exactly one call is let through as a
        probe. A successful probe closes the circuit again (`_circuit_record_success`);
        a failed probe reopens it for another recovery window
        (`_circuit_record_failure`). This is what makes recovery possible — a
        plain "give up after N failures" counter would stay tripped forever
        for the lifetime of this long-lived, cached-per-config instance.
        """
        with self._circuit_lock:
            now = time.time()

            if self._circuit_state == "open":
                if now < self._circuit_open_until:
                    return True
                self._circuit_state = "half_open"
                self._circuit_probe_in_flight = False

            if self._circuit_state == "half_open":
                if self._circuit_probe_in_flight:
                    return True
                self._circuit_probe_in_flight = True
                return False

            return False

    def _circuit_record_success(self) -> None:
        with self._circuit_lock:
            if self._circuit_state != "closed" or self._circuit_failure_count > 0:
                logger.info("Summarization circuit breaker reset (closed); summarization model recovered.")
            self._circuit_failure_count = 0
            self._circuit_open_until = 0.0
            self._circuit_state = "closed"
            self._circuit_probe_in_flight = False

    def _circuit_record_failure(self) -> None:
        with self._circuit_lock:
            if self._circuit_state == "half_open":
                self._circuit_open_until = time.time() + self._circuit_recovery_timeout_sec
                self._circuit_state = "open"
                self._circuit_probe_in_flight = False
                logger.error(
                    "Summarization circuit breaker probe failed (open); will probe again after %ds",
                    self._circuit_recovery_timeout_sec,
                )
                return

            self._circuit_failure_count += 1
            if self._circuit_failure_count >= self._max_consecutive_failures:
                self._circuit_open_until = time.time() + self._circuit_recovery_timeout_sec
                if self._circuit_state != "open":
                    self._circuit_state = "open"
                    self._circuit_probe_in_flight = False
                    logger.error(
                        "Summarization failed %d times in a row (threshold reached); circuit open, skipping LLM summarization for %ds",
                        self._circuit_failure_count,
                        self._circuit_recovery_timeout_sec,
                    )
            else:
                logger.warning(
                    "Summarization failed on both the primary and fallback model (consecutive failure %d/%d); falling back to a placeholder",
                    self._circuit_failure_count,
                    self._max_consecutive_failures,
                )

    def _summarize_with_tiers(self, messages_to_summarize: list[AnyMessage]) -> list[AnyMessage]:
        """Produce replacement messages for a summarization cutoff.

        Never destroys history on model failure: tries the primary model,
        then an optional fallback model, then degrades to a deterministic
        placeholder that keeps the loss visible instead of silently deleting
        messages under the guise of a summary. `_circuit_check` skips straight
        to the placeholder while the models are known to be down, and
        periodically lets one real attempt through so a recovered provider is
        picked back up automatically instead of being locked out forever.

        Once `_circuit_check` admits an attempt (closed, or the single
        half-open probe), this method is on the hook to call exactly one of
        `_circuit_record_success`/`_circuit_record_failure` before returning
        or raising — for *any* outcome, not just a modeled `_SummarizationFailed`.
        A probe that raises an unexpected bug (e.g. inside `_build_new_messages`)
        must still resolve the breaker, or a leaked `_circuit_probe_in_flight`
        would wedge it in "half_open" forever with no further recovery check.
        """
        if self._circuit_check():
            logger.error(
                "Summarization circuit breaker is open; skipping LLM summarization and dropping %d messages behind a placeholder",
                len(messages_to_summarize),
            )
            return self._build_placeholder_messages(messages_to_summarize)

        try:
            try:
                summary = self._create_summary(messages_to_summarize)
            except _SummarizationFailed:
                fallback_model = self._get_fallback_model()
                if fallback_model is None:
                    raise
                summary = self._create_summary(messages_to_summarize, model=fallback_model)
            new_messages = self._build_new_messages(summary)
        except _SummarizationFailed:
            self._circuit_record_failure()
            return self._build_placeholder_messages(messages_to_summarize)
        except Exception:
            # Not a modeled LLM failure — an unexpected bug. before_model's
            # blanket handler will keep this from crashing the run, but the
            # breaker must still be settled before it gets there.
            self._circuit_record_failure()
            raise

        self._circuit_record_success()
        return new_messages

    async def _asummarize_with_tiers(self, messages_to_summarize: list[AnyMessage]) -> list[AnyMessage]:
        """Async counterpart of `_summarize_with_tiers`; behavior must stay in lock-step.

        The circuit-breaker decision (`_circuit_check`/`_circuit_record_success`/
        `_circuit_record_failure`) is shared, non-async bookkeeping used
        identically by both paths — only the final `invoke` vs `ainvoke` call
        differs between this method and its sync counterpart. See
        `_summarize_with_tiers` for why every exit path, including an
        unexpected exception, must resolve the breaker.
        """
        if self._circuit_check():
            logger.error(
                "Summarization circuit breaker is open; skipping LLM summarization and dropping %d messages behind a placeholder",
                len(messages_to_summarize),
            )
            return self._build_placeholder_messages(messages_to_summarize)

        try:
            try:
                summary = await self._acreate_summary(messages_to_summarize)
            except _SummarizationFailed:
                fallback_model = self._get_fallback_model()
                if fallback_model is None:
                    raise
                summary = await self._acreate_summary(messages_to_summarize, model=fallback_model)
            new_messages = self._build_new_messages(summary)
        except _SummarizationFailed:
            self._circuit_record_failure()
            return self._build_placeholder_messages(messages_to_summarize)
        except Exception:
            self._circuit_record_failure()
            raise

        self._circuit_record_success()
        return new_messages

    def _create_summary(self, messages_to_summarize: list[AnyMessage], *, model: BaseChatModel | None = None) -> str:
        """Generate a summary using `model` (defaults to the primary model).

        Mirrors the parent class's trim + get_buffer_string + invoke pipeline,
        but never swallows an invocation error into a fake summary string —
        raises `_SummarizationFailed` instead so `_summarize_with_tiers` can
        distinguish "the LLM call failed" from "the LLM produced a summary".
        """
        if not messages_to_summarize:
            return "No previous conversation history."

        trimmed_messages = self._messages_for_summary(messages_to_summarize)
        if not trimmed_messages:
            return "Previous conversation was too long to summarize."

        # Serialize as XML so URL-based multimodal blocks remain visible in the summary
        # prompt while excluding raw message metadata from the token budget.
        formatted_messages = get_buffer_string(trimmed_messages, format="xml")
        llm = model if model is not None else self.model
        from deerflow.agents.middlewares.runtime_headers_middleware import bind_runtime_headers

        llm = bind_runtime_headers(llm)

        try:
            from deerflow.agents.middlewares.llm_error_handling_middleware import llm_call_slot_sync

            with llm_call_slot_sync():
                response = llm.invoke(
                    self.summary_prompt.format(messages=formatted_messages).rstrip(),
                    config={"metadata": {"lc_source": "summarization"}},
                )
            return response.text.strip()
        except Exception as exc:
            logger.warning("Summarization model invocation failed: %s", exc)
            raise _SummarizationFailed(str(exc)) from exc

    async def _acreate_summary(self, messages_to_summarize: list[AnyMessage], *, model: BaseChatModel | None = None) -> str:
        """Async counterpart of `_create_summary`; see that method for the failure contract."""
        if not messages_to_summarize:
            return "No previous conversation history."

        trimmed_messages = self._messages_for_summary(messages_to_summarize)
        if not trimmed_messages:
            return "Previous conversation was too long to summarize."

        # Serialize as XML so URL-based multimodal blocks remain visible in the summary
        # prompt while excluding raw message metadata from the token budget.
        formatted_messages = get_buffer_string(trimmed_messages, format="xml")
        llm = model if model is not None else self.model
        from deerflow.agents.middlewares.runtime_headers_middleware import bind_runtime_headers

        llm = bind_runtime_headers(llm)

        try:
            from deerflow.agents.middlewares.llm_error_handling_middleware import llm_call_slot_async

            async with llm_call_slot_async():
                response = await llm.ainvoke(
                    self.summary_prompt.format(messages=formatted_messages).rstrip(),
                    config={"metadata": {"lc_source": "summarization"}},
                )
            return response.text.strip()
        except Exception as exc:
            logger.warning("Summarization model invocation failed: %s", exc)
            raise _SummarizationFailed(str(exc)) from exc

    def _messages_for_summary(self, messages: list[AnyMessage]) -> list[AnyMessage]:
        """Keep bounded recent AI/tool history when no human anchor remains.

        Rescuing the active user request can leave the compaction window with
        only assistant and tool messages.  LangChain's inherited trimmer uses
        ``start_on='human'`` and returns an empty list for that valid window.
        Retry without the human anchor while retaining the same token budget
        and tail-first policy, so the latest tool result reaches the summary.
        """
        trimmed = self._trim_messages_for_summary(messages)
        if trimmed or not messages:
            return trimmed
        if any(isinstance(message, HumanMessage) for message in messages):
            return messages[-1:]
        if self.trim_tokens_to_summarize is None:
            return messages
        try:
            fallback = trim_messages(
                messages,
                max_tokens=self.trim_tokens_to_summarize,
                token_counter=self.token_counter,
                strategy="last",
                allow_partial=True,
                include_system=True,
            )
        except Exception:
            logger.debug("AI/tool-only summary trimming failed; keeping bounded tail", exc_info=True)
            return messages[-6:]
        return list(fallback) or messages[-1:]

    @staticmethod
    def _build_placeholder_messages(messages_to_summarize: list[AnyMessage]) -> list[HumanMessage]:
        """Deterministic, model-free degradation for when every summarization attempt failed.

        Drops the old messages (bounding context, same as a real summary
        would) but makes the loss visible in-band instead of masquerading as
        summary content.
        """
        return [
            HumanMessage(
                content=f"[{len(messages_to_summarize)} earlier messages were dropped because the summarization service was unavailable]",
                additional_kwargs={"lc_source": "summarization_fallback"},
            )
        ]

    def _partition_with_skill_rescue(
        self,
        messages: list[AnyMessage],
        cutoff_index: int,
    ) -> tuple[list[AnyMessage], list[AnyMessage]]:
        """Partition like the parent, then rescue the active request and recent skills.

        Long tool-heavy turns can push the latest genuine user message behind the
        parent's cutoff while keeping only tool traffic in the tail. The active
        request must remain verbatim so the model still knows what those tools are
        trying to accomplish. Middleware-generated HumanMessages are excluded by
        ``is_genuine_user_message``.
        """
        to_summarize, preserved = self._partition_messages(messages, cutoff_index)
        latest_user = next((message for message in reversed(messages) if is_genuine_user_message(message)), None)

        def rescue_latest_user() -> tuple[list[AnyMessage], list[AnyMessage]]:
            if latest_user is None or not any(message is latest_user for message in to_summarize):
                return to_summarize, preserved
            remaining = [message for message in to_summarize if message is not latest_user]
            return remaining, [latest_user, *preserved]

        if self._preserve_recent_skill_count == 0 or self._preserve_recent_skill_tokens == 0 or not to_summarize:
            return rescue_latest_user()

        try:
            bundles = self._find_skill_bundles(to_summarize, self._skills_container_path)
        except Exception:
            logger.exception("Skill-preserving summarization rescue failed; falling back to default partition")
            return rescue_latest_user()

        if not bundles:
            return rescue_latest_user()

        rescue_bundles = self._select_bundles_to_rescue(bundles)
        if not rescue_bundles:
            return rescue_latest_user()

        bundles_by_ai_index = {bundle.ai_index: bundle for bundle in rescue_bundles}
        rescue_tool_indices = {idx for bundle in rescue_bundles for idx in bundle.skill_tool_indices}
        rescued: list[AnyMessage] = []
        remaining: list[AnyMessage] = []
        for i, msg in enumerate(to_summarize):
            if msg is latest_user:
                rescued.append(msg)
                continue

            bundle = bundles_by_ai_index.get(i)
            if bundle is not None and isinstance(msg, AIMessage):
                rescued_tool_calls = [tc for tc in msg.tool_calls if tc.get("id") in bundle.skill_tool_call_ids]
                remaining_tool_calls = [tc for tc in msg.tool_calls if tc.get("id") not in bundle.skill_tool_call_ids]

                if rescued_tool_calls:
                    rescued.append(_clone_ai_message(msg, rescued_tool_calls, content=""))
                if remaining_tool_calls or msg.content:
                    remaining.append(_clone_ai_message(msg, remaining_tool_calls))
                continue

            if i in rescue_tool_indices:
                rescued.append(msg)
                continue

            remaining.append(msg)

        return remaining, rescued + preserved

    def _find_skill_bundles(
        self,
        messages: list[AnyMessage],
        skills_root: str,
    ) -> list[_SkillBundle]:
        """Locate AIMessage + paired ToolMessage groups that load skill files."""
        bundles: list[_SkillBundle] = []
        n = len(messages)
        i = 0
        while i < n:
            msg = messages[i]
            if not (isinstance(msg, AIMessage) and msg.tool_calls):
                i += 1
                continue

            tool_calls = list(msg.tool_calls)
            skill_paths_by_id: dict[str, str] = {}
            for tc in tool_calls:
                if self._is_skill_tool_call(tc, skills_root):
                    tc_id = tc.get("id")
                    path = _tool_call_path(tc)
                    if tc_id and path:
                        skill_paths_by_id[tc_id] = path

            if not skill_paths_by_id:
                i += 1
                continue

            skill_tool_tokens = 0
            skill_key_parts: list[str] = []
            skill_tool_indices: list[int] = []
            matched_skill_call_ids: set[str] = set()

            j = i + 1
            while j < n and isinstance(messages[j], ToolMessage):
                j += 1

            for k in range(i + 1, j):
                tool_msg = messages[k]
                if isinstance(tool_msg, ToolMessage) and tool_msg.tool_call_id in skill_paths_by_id:
                    skill_tool_tokens += self.token_counter([tool_msg])
                    skill_key_parts.append(skill_paths_by_id[tool_msg.tool_call_id])
                    skill_tool_indices.append(k)
                    matched_skill_call_ids.add(tool_msg.tool_call_id)

            if not skill_tool_indices:
                i = j
                continue

            bundles.append(
                _SkillBundle(
                    ai_index=i,
                    skill_tool_indices=tuple(skill_tool_indices),
                    skill_tool_call_ids=frozenset(matched_skill_call_ids),
                    skill_tool_tokens=skill_tool_tokens,
                    skill_key="|".join(sorted(skill_key_parts)),
                )
            )
            i = j

        return bundles

    def _select_bundles_to_rescue(self, bundles: list[_SkillBundle]) -> list[_SkillBundle]:
        """Pick bundles to keep, walking newest-first under count/token budgets."""
        selected: list[_SkillBundle] = []
        if not bundles:
            return selected

        seen_skill_keys: set[str] = set()
        total_tokens = 0
        kept = 0

        for bundle in reversed(bundles):
            if kept >= self._preserve_recent_skill_count:
                break
            if bundle.skill_key in seen_skill_keys:
                continue
            if bundle.skill_tool_tokens > self._preserve_recent_skill_tokens_per_skill:
                continue
            if total_tokens + bundle.skill_tool_tokens > self._preserve_recent_skill_tokens:
                continue

            selected.append(bundle)
            total_tokens += bundle.skill_tool_tokens
            kept += 1
            seen_skill_keys.add(bundle.skill_key)

        selected.reverse()
        return selected

    def _is_skill_tool_call(self, tool_call: dict[str, Any], skills_root: str) -> bool:
        """Return True when ``tool_call`` reads a file under the configured skills root."""
        name = tool_call.get("name") or ""
        if name not in self._skill_file_read_tool_names:
            return False
        path = _tool_call_path(tool_call)
        if not path:
            return False
        normalized_root = skills_root.rstrip("/")
        return path == normalized_root or path.startswith(normalized_root + "/")

    def _fire_hooks(
        self,
        messages_to_summarize: list[AnyMessage],
        preserved_messages: list[AnyMessage],
        runtime: Runtime,
    ) -> None:
        if not self._before_summarization_hooks:
            return

        event = SummarizationEvent(
            messages_to_summarize=tuple(messages_to_summarize),
            preserved_messages=tuple(preserved_messages),
            thread_id=_resolve_thread_id(runtime),
            agent_name=_resolve_agent_name(runtime),
            runtime=runtime,
        )

        for hook in self._before_summarization_hooks:
            try:
                hook(event)
            except Exception:
                hook_name = getattr(hook, "__name__", None) or type(hook).__name__
                logger.exception("before_summarization hook %s failed", hook_name)
