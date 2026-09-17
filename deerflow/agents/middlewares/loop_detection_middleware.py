"""Middleware to detect and break repetitive tool call loops.

P0 safety: prevents the agent from calling the same tool with the same
arguments indefinitely until the recursion limit kills the run.

Detection strategy:
  1. After each model response, hash the tool calls (name + args).
  2. Track recent hashes in a sliding window.
  3. If the same hash appears >= warn_threshold times, queue a
     "you are repeating yourself — wrap up" warning to be injected as a
     plain HumanMessage at the END of the next outgoing message list
     (during ``wrap_model_call``). Queuing — rather than returning the
     warning from ``after_model`` — avoids splitting an ``AIMessage``
     tool_calls from its ToolMessage responses, which would otherwise
     trigger 400s on strict provider validators (OpenAI / Moonshot).
  4. If a hash appears >= hard_limit times, strip all tool_calls from the
     response and publish a structured incomplete termination.
"""

import hashlib
import json
import logging
import threading
import uuid
from collections import Counter, OrderedDict, defaultdict, deque
from collections.abc import Awaitable, Callable
from typing import Any, override

from langchain.agents import AgentState
from langchain.agents.middleware import AgentMiddleware
from langchain.agents.middleware.types import ModelCallResult, ModelRequest, ModelResponse
from langchain_core.messages import HumanMessage
from langgraph.runtime import Runtime

from deerflow.agents.middlewares.tool_call_metadata import (
    clone_ai_message_with_tool_calls,
)

logger = logging.getLogger(__name__)

# Structured termination metadata survives the message rewrite performed by a
# hard stop.  Downstream harness layers must not infer success merely because
# the rewritten assistant message no longer contains tool calls.
AGENT_TERMINATION_KEY = "agent_termination"
TOOL_CALL_LIMIT_STOP_REASON = "tool_call_limit"

# Defaults — can be overridden via constructor
_DEFAULT_WARN_THRESHOLD = 3  # inject warning after 3 identical calls
_DEFAULT_HARD_LIMIT = 5  # force-stop after 5 identical calls
_DEFAULT_WINDOW_SIZE = 20  # track last N tool calls
_DEFAULT_MAX_TRACKED_THREADS = 100  # LRU eviction limit
_DEFAULT_TOOL_FREQ_WARN = 30  # warn after 30 calls to the same tool type
_DEFAULT_TOOL_FREQ_HARD_LIMIT = 80  # force-stop after 80 calls to the same tool type
# Per-run backstop across ALL tool types. A long, genuinely-diverse run
# (every call differs, no single tool type repeats enough) evades both the
# hash and per-tool-type layers and would otherwise only be stopped by the
# graph ``recursion_limit`` — which aborts with an unrecoverable error rather
# than degrading gracefully. The total caps are derived from recursion_limit
# AND the per-turn graph cost (see ``_derive_total_call_limits``) so the two
# stay in lock-step; these constants are the fallback when either is unknown.
_DEFAULT_RECURSION_LIMIT = 200
# How many graph super-steps a single tool-calling agent turn costs. In
# LangChain's ``create_agent`` graph each ``before_model`` / ``after_model``
# middleware hook is compiled as its *own* node, so one turn is
# ``model + tools + (#before_model nodes) + (#after_model nodes)`` super-steps —
# typically 4-5 for our agents, NOT 2. Use ``count_steps_per_turn`` to measure
# the real value from a middleware chain; this is the fallback when unknown.
_DEFAULT_STEPS_PER_TURN = 5
# Of the turns achievable before the recursion limit, force a clean stop at this
# fraction (headroom for the wrap-up step) and nudge to wrap up earlier still.
_TOTAL_CALL_HARD_FRACTION = 0.80
_TOTAL_CALL_WARN_FRACTION = 0.55
_MAX_PENDING_WARNINGS_PER_RUN = 4

type _RunScopeKey = tuple[str, str]


def count_steps_per_turn(middlewares: list) -> int:
    """Graph super-steps consumed by one tool-calling turn for *middlewares*.

    Mirrors ``create_agent``'s graph construction: model + tools nodes (2) plus
    one node per middleware that overrides a ``before_model`` / ``after_model``
    hook. Used to keep the per-run backstop calibrated to the recursion limit.
    """
    before = after = 0
    for m in middlewares:
        cls = m.__class__
        if cls.before_model is not AgentMiddleware.before_model or cls.abefore_model is not AgentMiddleware.abefore_model:
            before += 1
        if cls.after_model is not AgentMiddleware.after_model or cls.aafter_model is not AgentMiddleware.aafter_model:
            after += 1
    return before + after + 2


def _derive_total_call_limits(recursion_limit: int | None, steps_per_turn: int | None = None) -> tuple[int, int]:
    """Derive ``(total_call_warn, total_call_hard_limit)`` from the run budget.

    Keeps the per-run tool-call backstop in lock-step with the graph
    ``recursion_limit``: a run can make at most ``recursion_limit / steps_per_turn``
    tool-calling turns, so we force a stop at a fraction of that ceiling (well
    before the graph aborts with ``GraphRecursionError``). Returns sane, ordered
    values (``1 <= warn < hard``) for any input.
    """
    limit = recursion_limit if isinstance(recursion_limit, int) and recursion_limit > 0 else _DEFAULT_RECURSION_LIMIT
    steps = steps_per_turn if isinstance(steps_per_turn, int) and steps_per_turn > 0 else _DEFAULT_STEPS_PER_TURN
    max_turns = max(2, limit // steps)
    warn = max(1, int(max_turns * _TOTAL_CALL_WARN_FRACTION))
    hard = max(warn + 1, int(max_turns * _TOTAL_CALL_HARD_FRACTION))
    return warn, hard


def calibrate_loop_detection(middlewares: list, recursion_limit: int | None) -> None:
    """Recalibrate any ``LoopDetectionMiddleware`` in *middlewares* in place.

    Call once the full chain is assembled so the per-run backstop reflects the
    actual per-turn graph cost (number of model-phase middleware nodes).
    """
    steps = count_steps_per_turn(middlewares)
    for m in middlewares:
        if isinstance(m, LoopDetectionMiddleware):
            m.set_run_budget(recursion_limit, steps_per_turn=steps)


def _normalize_tool_call_args(raw_args: object) -> tuple[dict, str | None]:
    """Normalize tool call args to a dict plus an optional fallback key.

    Some providers serialize ``args`` as a JSON string instead of a dict.
    We defensively parse those cases so loop detection does not crash while
    still preserving a stable fallback key for non-dict payloads.
    """
    if isinstance(raw_args, dict):
        return raw_args, None

    if isinstance(raw_args, str):
        try:
            parsed = json.loads(raw_args)
        except (TypeError, ValueError, json.JSONDecodeError):
            return {}, raw_args

        if isinstance(parsed, dict):
            return parsed, None
        return {}, json.dumps(parsed, sort_keys=True, default=str)

    if raw_args is None:
        return {}, None

    return {}, json.dumps(raw_args, sort_keys=True, default=str)


def _stable_tool_key(name: str, args: dict, fallback_key: str | None) -> str:
    """Derive a stable key from salient args without overfitting to noise."""
    if name == "read_file" and fallback_key is None:
        path = args.get("path") or ""
        start_line = args.get("start_line")
        end_line = args.get("end_line")

        bucket_size = 200
        try:
            start_line = int(start_line) if start_line is not None else 1
        except (TypeError, ValueError):
            start_line = 1
        try:
            end_line = int(end_line) if end_line is not None else start_line
        except (TypeError, ValueError):
            end_line = start_line

        start_line, end_line = sorted((start_line, end_line))
        bucket_start = max(start_line, 1)
        bucket_end = max(end_line, 1)
        bucket_start = (bucket_start - 1) // bucket_size
        bucket_end = (bucket_end - 1) // bucket_size
        return f"{path}:{bucket_start}-{bucket_end}"

    # write_file / str_replace are content-sensitive: same path may be updated
    # with different payloads during iteration. Using only salient fields (path)
    # can collapse distinct calls, so we hash full args to reduce false positives.
    if name in {"write_file", "str_replace"}:
        if fallback_key is not None:
            return fallback_key
        return json.dumps(args, sort_keys=True, default=str)

    salient_fields = ("path", "url", "query", "command", "pattern", "glob", "cmd")
    stable_args = {field: args[field] for field in salient_fields if args.get(field) is not None}
    if stable_args:
        return json.dumps(stable_args, sort_keys=True, default=str)

    if fallback_key is not None:
        return fallback_key

    return json.dumps(args, sort_keys=True, default=str)


def _hash_tool_calls(tool_calls: list[dict]) -> str:
    """Deterministic hash of a set of tool calls (name + stable key).

    This is intended to be order-independent: the same multiset of tool calls
    should always produce the same hash, regardless of their input order.
    """
    normalized: list[str] = []
    for tc in tool_calls:
        name = tc.get("name", "")
        args, fallback_key = _normalize_tool_call_args(tc.get("args", {}))
        key = _stable_tool_key(name, args, fallback_key)

        normalized.append(f"{name}:{key}")

    normalized.sort()
    blob = json.dumps(normalized, sort_keys=True, default=str)
    return hashlib.md5(blob.encode()).hexdigest()[:12]


_WARNING_MSG = "[LOOP DETECTED] You are repeating the same tool calls. Stop calling tools and produce your final answer now. If you cannot complete the task, summarize what you accomplished so far."

_TOOL_FREQ_WARNING_MSG = (
    "[LOOP DETECTED] You have called {tool_name} {count} times without producing a final answer. Stop calling tools and produce your final answer now. If you cannot complete the task, summarize what you accomplished so far."
)

_HARD_STOP_MSG = "[INCOMPLETE] Repeated tool calls exceeded the safety limit. Partial results were preserved so the task can continue in a new turn."

_TOOL_FREQ_HARD_STOP_MSG = "[INCOMPLETE] Tool {tool_name} was called {count} times and exceeded the per-tool safety limit. Partial results were preserved so the task can continue in a new turn."

_TOTAL_CALLS_WARNING_MSG = (
    "[LOOP DETECTED] You have made {count} tool calls in this run without producing a final answer. Wrap up now and produce your final answer from the results collected so far."
)

_TOTAL_CALLS_HARD_STOP_MSG = "[INCOMPLETE] Total tool calls reached {count} and exceeded the per-run safety limit. Partial results were preserved so the task can continue in a new turn."


class LoopDetectionMiddleware(AgentMiddleware[AgentState]):
    """Detects and breaks repetitive tool call loops.

    Args:
        warn_threshold: Number of identical tool call sets before injecting
            a warning message. Default: 3.
        hard_limit: Number of identical tool call sets before stripping
            tool_calls entirely. Default: 5.
        window_size: Size of the sliding window for tracking calls.
            Default: 20.
        max_tracked_threads: Maximum number of threads to track before
            evicting the least recently used. Default: 100.
        tool_freq_warn: Number of calls to the same tool *type* (regardless
            of arguments) before injecting a frequency warning. Catches
            cross-file read loops that hash-based detection misses.
            Default: 30.
        tool_freq_hard_limit: Number of calls to the same tool type before
            forcing a stop. Default: 50.
        recursion_limit: The graph ``recursion_limit`` this agent runs under.
            When ``total_call_warn`` / ``total_call_hard_limit`` are left as
            ``None`` they are derived from it (see ``_derive_total_call_limits``)
            so the per-run backstop stays in lock-step with the recursion limit.
            Defaults to 200 when unknown.
        steps_per_turn: Graph super-steps one tool-calling turn costs. With the
            recursion limit this bounds the achievable turns. Left ``None`` here
            and refined by ``calibrate_loop_detection`` once the full middleware
            chain is known; defaults to 5 otherwise.
        total_call_warn: Number of tool calls across *all* tool types in a
            single run before injecting a wrap-up warning. Backstop for long,
            diverse runs that evade the hash and per-tool-type layers. When
            ``None`` (default), derived from the run budget.
        total_call_hard_limit: Number of tool calls across all tool types
            before forcing a stop. Kept below the achievable turn count so the
            run ends with a structured incomplete result instead of a
            ``GraphRecursionError``. When ``None`` (default), derived from the
            run budget.
        stream_callback: Optional callback that receives ``{"type": ..., "message": ...}``
            events for ``loop_warning`` / ``loop_hard_stop``. When ``None``,
            falls back to LangGraph's ``get_stream_writer()``.
    """

    def __init__(
        self,
        warn_threshold: int = _DEFAULT_WARN_THRESHOLD,
        hard_limit: int = _DEFAULT_HARD_LIMIT,
        window_size: int = _DEFAULT_WINDOW_SIZE,
        max_tracked_threads: int = _DEFAULT_MAX_TRACKED_THREADS,
        tool_freq_warn: int = _DEFAULT_TOOL_FREQ_WARN,
        tool_freq_hard_limit: int = _DEFAULT_TOOL_FREQ_HARD_LIMIT,
        recursion_limit: int | None = None,
        steps_per_turn: int | None = None,
        total_call_warn: int | None = None,
        total_call_hard_limit: int | None = None,
        stream_callback: Callable[[dict[str, Any]], None] | None = None,
    ):
        super().__init__()
        self.warn_threshold = warn_threshold
        self.hard_limit = hard_limit
        self.window_size = window_size
        self.max_tracked_threads = max_tracked_threads
        self.tool_freq_warn = tool_freq_warn
        self.tool_freq_hard_limit = tool_freq_hard_limit
        # Per-run total backstop derived from the run budget unless overridden.
        # ``calibrate_loop_detection`` refines steps_per_turn once the full chain
        # is known; until then we use the supplied value or a safe default.
        derived_warn, derived_hard = _derive_total_call_limits(recursion_limit, steps_per_turn)
        self.total_call_warn = total_call_warn if total_call_warn is not None else derived_warn
        self.total_call_hard_limit = total_call_hard_limit if total_call_hard_limit is not None else derived_hard
        self.stream_callback = stream_callback
        self._lock = threading.Lock()
        # LangGraph replaces Runtime wrappers across graph nodes but keeps the
        # control object for one invocation. Use it as a fallback run anchor
        # when embedders do not provide run_id.
        self._fallback_run_ids: OrderedDict[int, tuple[object, str]] = OrderedDict()
        self._max_fallback_run_ids = max(1, self.max_tracked_threads * 2)
        # Detection state is run-scoped so a new user run in the same thread
        # never inherits warning or hard-stop counters.
        self._history: OrderedDict[_RunScopeKey, list[str]] = OrderedDict()
        self._warned: dict[_RunScopeKey, set[str]] = defaultdict(set)
        # Per-tool frequency is windowed over individual calls.  Keep the
        # window large enough for the configured hard threshold to be
        # reachable even when the generic hash window is smaller.
        self._tool_frequency_window_size = max(window_size, tool_freq_hard_limit)
        self._tool_name_history: dict[_RunScopeKey, deque[str]] = defaultdict(deque)
        self._tool_freq: dict[_RunScopeKey, Counter[str]] = defaultdict(Counter)
        self._tool_freq_warned: dict[_RunScopeKey, set[str]] = defaultdict(set)
        self._total_calls: dict[_RunScopeKey, int] = defaultdict(int)
        self._total_warned: set[_RunScopeKey] = set()
        # Deferred warnings: queued in ``after_model`` and drained in
        # ``wrap_model_call`` so they land at the end of the message list
        # rather than between an AIMessage tool_calls and its ToolMessage
        # responses. Keyed by (thread_id, run_id).
        self._pending_warnings: dict[_RunScopeKey, list[str]] = defaultdict(list)
        self._pending_warning_touch_order: OrderedDict[_RunScopeKey, None] = OrderedDict()
        self._max_pending_warning_keys = max(1, self.max_tracked_threads * 2)

    def set_run_budget(self, recursion_limit: int | None, steps_per_turn: int | None = None) -> None:
        """Recalibrate the per-run total backstop from the run budget.

        Called by ``calibrate_loop_detection`` after the full middleware chain is
        assembled, so the caps reflect the real per-turn graph cost. Overwrites
        the values derived at construction.
        """
        self.total_call_warn, self.total_call_hard_limit = _derive_total_call_limits(recursion_limit, steps_per_turn)

    def _get_thread_id(self, runtime: Runtime) -> str:
        """Extract the loop-detection scope from runtime context.

        ``thread_id`` is conversation-scoped and can live across many user
        requests. Prefer a per-run scope when the caller provides one so
        normal long conversations do not accumulate tool-frequency counts
        until they trip the hard limit.
        """
        context: dict[str, Any] = runtime.context or {}
        thread_id = context.get("loop_detection_scope_id") or context.get("thread_id")
        if thread_id:
            return str(thread_id)
        return "default"

    def _get_run_id(self, runtime: Runtime) -> str:
        """Return a stable ID for one invocation, even without context run_id."""
        context = getattr(runtime, "context", None)
        if isinstance(context, dict) and context.get("run_id") is not None:
            return str(context["run_id"])

        execution_info = getattr(runtime, "execution_info", None)
        execution_run_id = getattr(execution_info, "run_id", None)
        if execution_run_id is not None:
            return str(execution_run_id)

        control = getattr(runtime, "control", None)
        anchor = control if control is not None else runtime
        anchor_id = id(anchor)
        with self._lock:
            existing = self._fallback_run_ids.get(anchor_id)
            if existing is not None and existing[0] is anchor:
                self._fallback_run_ids.move_to_end(anchor_id)
                return existing[1]
            fallback = f"__invocation__:{uuid.uuid4().hex}"
            self._fallback_run_ids[anchor_id] = (anchor, fallback)
            self._fallback_run_ids.move_to_end(anchor_id)
            while len(self._fallback_run_ids) > self._max_fallback_run_ids:
                self._fallback_run_ids.popitem(last=False)
            return fallback

    def _release_fallback_run_id(self, runtime: Runtime) -> None:
        context = getattr(runtime, "context", None)
        if isinstance(context, dict) and context.get("run_id") is not None:
            return
        execution_info = getattr(runtime, "execution_info", None)
        if getattr(execution_info, "run_id", None) is not None:
            return
        control = getattr(runtime, "control", None)
        anchor = control if control is not None else runtime
        anchor_id = id(anchor)
        with self._lock:
            existing = self._fallback_run_ids.get(anchor_id)
            if existing is not None and existing[0] is anchor:
                self._fallback_run_ids.pop(anchor_id, None)

    def _run_scope_key(self, runtime: Runtime) -> _RunScopeKey:
        return self._get_thread_id(runtime), self._get_run_id(runtime)

    def _pending_key(self, runtime: Runtime) -> _RunScopeKey:
        return self._run_scope_key(runtime)

    def _evict_if_needed(self) -> None:
        """Evict least recently used thread/run scopes if over the limit.

        Must be called while holding self._lock.
        """
        while len(self._history) > self.max_tracked_threads:
            evicted_key, _ = self._history.popitem(last=False)
            self._warned.pop(evicted_key, None)
            self._tool_freq.pop(evicted_key, None)
            self._tool_name_history.pop(evicted_key, None)
            self._tool_freq_warned.pop(evicted_key, None)
            self._total_calls.pop(evicted_key, None)
            self._total_warned.discard(evicted_key)
            self._drop_pending_warning_key_locked(evicted_key)
            logger.debug(
                "Evicted loop tracking for thread/run scope (LRU)",
                extra={"thread_id": evicted_key[0], "run_id": evicted_key[1]},
            )

    def _drop_pending_warning_key_locked(self, key: _RunScopeKey) -> None:
        """Drop all pending-warning bookkeeping for one (thread, run) key.

        Must be called while holding self._lock.
        """
        self._pending_warnings.pop(key, None)
        self._pending_warning_touch_order.pop(key, None)

    def _touch_pending_warning_key_locked(self, key: _RunScopeKey) -> None:
        """Mark a pending-warning key as recently used.

        Must be called while holding self._lock.
        """
        self._pending_warning_touch_order[key] = None
        self._pending_warning_touch_order.move_to_end(key)

    def _prune_pending_warning_state_locked(self, protected_key: _RunScopeKey) -> None:
        """Cap pending-warning state across abnormal or concurrent runs.

        Must be called while holding self._lock.
        """
        overflow = len(self._pending_warning_touch_order) - self._max_pending_warning_keys
        if overflow <= 0:
            return

        candidates = [key for key in self._pending_warning_touch_order if key != protected_key]
        for key in candidates[:overflow]:
            self._drop_pending_warning_key_locked(key)

    def _queue_pending_warning(self, runtime: Runtime, warning: str) -> None:
        """Queue one transient warning for current (thread, run) with caps."""
        pending_key = self._pending_key(runtime)
        with self._lock:
            warnings = self._pending_warnings[pending_key]
            if warning not in warnings:
                warnings.append(warning)
            if len(warnings) > _MAX_PENDING_WARNINGS_PER_RUN:
                del warnings[: len(warnings) - _MAX_PENDING_WARNINGS_PER_RUN]
            self._touch_pending_warning_key_locked(pending_key)
            self._prune_pending_warning_state_locked(protected_key=pending_key)

    def _track_and_check(self, state: AgentState, runtime: Runtime) -> tuple[str | None, bool]:
        """Track tool calls and check for loops.

        Two detection layers:
          1. **Hash-based**: catches identical tool call sets.
          2. **Frequency-based**: catches the same *tool type* being
             called many times with varying arguments (e.g. ``read_file``
             on 40 different files).

        Returns:
            (warning_message_or_none, should_hard_stop)
        """
        messages = state.get("messages", [])
        if not messages:
            return None, False

        last_msg = messages[-1]
        if getattr(last_msg, "type", None) != "ai":
            return None, False

        tool_calls = getattr(last_msg, "tool_calls", None)
        if not tool_calls:
            return None, False

        scope_key = self._run_scope_key(runtime)
        thread_id, run_id = scope_key
        call_hash = _hash_tool_calls(tool_calls)

        with self._lock:
            # Touch / create entry (move to end for LRU)
            if scope_key in self._history:
                self._history.move_to_end(scope_key)
            else:
                self._history[scope_key] = []
                self._evict_if_needed()

            history = self._history[scope_key]
            history.append(call_hash)
            if len(history) > self.window_size:
                history[:] = history[-self.window_size :]

            # Hashes that fall out of the window should be eligible to warn
            # again if they reappear later. Mirror the upstream behavior.
            warned_hashes = self._warned.get(scope_key)
            if warned_hashes is not None:
                warned_hashes.intersection_update(history)
                if not warned_hashes:
                    self._warned.pop(scope_key, None)

            count = history.count(call_hash)
            tool_names = [tc.get("name", "?") for tc in tool_calls]

            # --- Layer 0: per-run total tool-call backstop (hard) ---
            # Counted unconditionally before the early-returning layers below so
            # diverse runs that never trip the hash / per-tool-type layers still
            # stop before the graph recursion_limit aborts the run.
            self._total_calls[scope_key] += len(tool_calls)
            total_count = self._total_calls[scope_key]
            if total_count >= self.total_call_hard_limit:
                logger.error(
                    "Total tool-call hard limit reached — forcing stop",
                    extra={
                        "thread_id": thread_id,
                        "run_id": run_id,
                        "count": total_count,
                        "tools": tool_names,
                    },
                )
                return _TOTAL_CALLS_HARD_STOP_MSG.format(count=total_count), True

            # --- Layer 1: hash-based (identical call sets) ---
            if count >= self.hard_limit:
                logger.error(
                    "Loop hard limit reached — forcing stop",
                    extra={
                        "thread_id": thread_id,
                        "run_id": run_id,
                        "call_hash": call_hash,
                        "count": count,
                        "tools": tool_names,
                    },
                )
                return _HARD_STOP_MSG, True

            # A warning admits the entire batch, so retain it as a candidate
            # until every tool call has been frequency-accounted.  A later
            # hard stop must reject the whole batch regardless of call order.
            warning: str | None = None
            warning_kind: tuple[str, str] | None = None
            if count >= self.warn_threshold and call_hash not in self._warned.get(scope_key, set()):
                warning = _WARNING_MSG
                warning_kind = ("hash", call_hash)

            # --- Layer 2: per-tool-type frequency (sliding window) ---
            freq = self._tool_freq[scope_key]
            name_history = self._tool_name_history[scope_key]
            for tc in tool_calls:
                name = tc.get("name", "")
                if not name:
                    continue
                name_history.append(name)
                freq[name] += 1
                while len(name_history) > self._tool_frequency_window_size:
                    evicted_name = name_history.popleft()
                    freq[evicted_name] -= 1
                    if freq[evicted_name] <= 0:
                        del freq[evicted_name]
                    if freq.get(evicted_name, 0) < self.tool_freq_warn:
                        self._tool_freq_warned[scope_key].discard(evicted_name)
                tc_count = freq[name]

                if tc_count >= self.tool_freq_hard_limit:
                    logger.error(
                        "Tool frequency hard limit reached — forcing stop",
                        extra={
                            "thread_id": thread_id,
                            "run_id": run_id,
                            "tool_name": name,
                            "count": tc_count,
                        },
                    )
                    return _TOOL_FREQ_HARD_STOP_MSG.format(tool_name=name, count=tc_count), True

                if tc_count >= self.tool_freq_warn:
                    warned = self._tool_freq_warned[scope_key]
                    if warning is None and name not in warned:
                        warning = _TOOL_FREQ_WARNING_MSG.format(tool_name=name, count=tc_count)
                        warning_kind = ("tool", name)

            if warning is not None and warning_kind is not None:
                kind, key = warning_kind
                if kind == "hash":
                    self._warned[scope_key].add(key)
                    logger.warning(
                        "Repetitive tool calls detected — injecting warning",
                        extra={"thread_id": thread_id, "run_id": run_id, "call_hash": call_hash, "count": count, "tools": tool_names},
                    )
                else:
                    # The selected name may have decayed below the threshold
                    # later in the same oversized batch.  Do not leave a stale
                    # suppression mark in that case.
                    if freq.get(key, 0) >= self.tool_freq_warn:
                        self._tool_freq_warned[scope_key].add(key)
                    logger.warning(
                        "Tool frequency warning — too many calls to same tool type",
                        extra={"thread_id": thread_id, "run_id": run_id, "tool_name": key, "count": freq.get(key, 0)},
                    )
                return warning, False

            # --- Layer 0: per-run total tool-call backstop (warn) ---
            # Lowest priority: only reached when no hash / per-tool-type signal
            # fired this turn, so a wrap-up nudge lands before the hard limit.
            if total_count >= self.total_call_warn and scope_key not in self._total_warned:
                self._total_warned.add(scope_key)
                logger.warning(
                    "Total tool-call warning — many calls without a final answer",
                    extra={
                        "thread_id": thread_id,
                        "run_id": run_id,
                        "count": total_count,
                    },
                )
                return _TOTAL_CALLS_WARNING_MSG.format(count=total_count), False

        return None, False

    @staticmethod
    def _append_text(content: str | list | None, text: str) -> str | list:
        """Append *text* to AIMessage content, handling str, list, and None.

        When content is a list of content blocks (e.g. Anthropic thinking mode),
        we append a new ``{"type": "text", ...}`` block instead of concatenating
        a string to a list, which would raise ``TypeError``.
        """
        if content is None:
            return text
        if isinstance(content, list):
            return [*content, {"type": "text", "text": f"\n\n{text}"}]
        if isinstance(content, str):
            return content + f"\n\n{text}"
        # Fallback: coerce unexpected types to str to avoid TypeError
        return str(content) + f"\n\n{text}"

    def _emit(self, event: dict[str, Any]) -> None:
        """Emit a stream event via stream_callback (preferred) or get_stream_writer() fallback."""
        if self.stream_callback is not None:
            try:
                self.stream_callback(event)
            except Exception:
                logger.debug("stream_callback raised on %s, ignoring", event.get("type"), exc_info=True)
            return
        try:
            from langgraph.config import get_stream_writer
            writer = get_stream_writer()
            writer(event)
        except Exception:
            pass

    def _apply(self, state: AgentState, runtime: Runtime) -> dict | None:
        warning, hard_stop = self._track_and_check(state, runtime)

        if hard_stop:
            message = warning or _HARD_STOP_MSG
            context = getattr(runtime, "context", None)
            if isinstance(context, dict):
                context.setdefault("stop_reason", TOOL_CALL_LIMIT_STOP_REASON)
            self._emit(
                {
                    "type": "loop_hard_stop",
                    "message": message,
                    "reason": TOOL_CALL_LIMIT_STOP_REASON,
                    "incomplete": True,
                }
            )

            # Strip tool_calls while retaining explicit incomplete metadata.
            messages = state.get("messages", [])
            last_msg = messages[-1]
            content = self._append_text(last_msg.content, message)
            stripped_msg = clone_ai_message_with_tool_calls(
                last_msg,
                [],
                content=content,
            )
            additional_kwargs = dict(stripped_msg.additional_kwargs or {})
            additional_kwargs[AGENT_TERMINATION_KEY] = {
                "reason": TOOL_CALL_LIMIT_STOP_REASON,
                "incomplete": True,
                "message": message,
            }
            stripped_msg = stripped_msg.model_copy(
                update={"additional_kwargs": additional_kwargs}
            )
            return {"messages": [stripped_msg]}

        if warning:
            self._emit({"type": "loop_warning", "message": warning})
            # Defer injection to ``wrap_model_call`` so the HumanMessage is
            # appended at the end of the message list rather than between an
            # AIMessage tool_calls and its corresponding ToolMessages.
            self._queue_pending_warning(runtime, warning)
            return None

        return None

    def _clear_current_run_pending_warnings(self, runtime: Runtime) -> None:
        """Drop pending warnings owned by current (thread, run)."""
        pending_key = self._pending_key(runtime)
        with self._lock:
            self._drop_pending_warning_key_locked(pending_key)

    @staticmethod
    def _format_warning_message(warnings: list[str]) -> str:
        """Merge pending warnings into one prompt message."""
        deduped = list(dict.fromkeys(warnings))
        return "\n\n".join(deduped)

    def _drain_pending_warnings(self, runtime: Runtime) -> list[str]:
        pending_key = self._pending_key(runtime)
        with self._lock:
            warnings = self._pending_warnings.pop(pending_key, [])
            self._pending_warning_touch_order.pop(pending_key, None)
        return warnings

    def _restore_pending_warnings(
        self,
        runtime: Runtime,
        warnings: list[str],
    ) -> None:
        """Restore warnings consumed by a model call that raised."""
        if not warnings:
            return
        pending_key = self._pending_key(runtime)
        with self._lock:
            queued = self._pending_warnings[pending_key]
            queued[:0] = [warning for warning in warnings if warning not in queued]
            del queued[_MAX_PENDING_WARNINGS_PER_RUN:]
            self._touch_pending_warning_key_locked(pending_key)
            self._prune_pending_warning_state_locked(protected_key=pending_key)

    def _inject_warnings(
        self,
        request: ModelRequest,
        warnings: list[str],
    ) -> ModelRequest:
        """Append queued loop warnings (if any) to the outgoing message list.

        The warning lands after every existing message — including the
        ToolMessage responses to the previous AIMessage(tool_calls). That
        preserves the assistant tool_calls -> tool_messages pairing required
        by OpenAI/Moonshot, avoids Anthropic's mid-stream SystemMessage
        restriction (we use HumanMessage), and never mutates an existing
        AIMessage.
        """
        if not warnings:
            return request
        new_messages = [
            *request.messages,
            HumanMessage(content=self._format_warning_message(warnings), name="loop_warning"),
        ]
        return request.override(messages=new_messages)

    @override
    def before_agent(self, state: AgentState, runtime: Runtime) -> dict | None:
        """Keep the graph hook without mutating other concurrent run scopes."""
        return None

    @override
    async def abefore_agent(self, state: AgentState, runtime: Runtime) -> dict | None:
        return None

    @override
    def after_model(self, state: AgentState, runtime: Runtime) -> dict | None:
        return self._apply(state, runtime)

    @override
    async def aafter_model(self, state: AgentState, runtime: Runtime) -> dict | None:
        return self._apply(state, runtime)

    @override
    def after_agent(self, state: AgentState, runtime: Runtime) -> dict | None:
        """Clear undrained pending warnings at run end."""
        self._clear_current_run_pending_warnings(runtime)
        self._release_fallback_run_id(runtime)
        return None

    @override
    async def aafter_agent(self, state: AgentState, runtime: Runtime) -> dict | None:
        self._clear_current_run_pending_warnings(runtime)
        self._release_fallback_run_id(runtime)
        return None

    @override
    def wrap_model_call(
        self,
        request: ModelRequest,
        handler: Callable[[ModelRequest], ModelResponse],
    ) -> ModelCallResult:
        warnings = self._drain_pending_warnings(request.runtime)
        try:
            return handler(self._inject_warnings(request, warnings))
        except Exception:
            self._restore_pending_warnings(request.runtime, warnings)
            raise

    @override
    async def awrap_model_call(
        self,
        request: ModelRequest,
        handler: Callable[[ModelRequest], Awaitable[ModelResponse]],
    ) -> ModelCallResult:
        warnings = self._drain_pending_warnings(request.runtime)
        try:
            return await handler(self._inject_warnings(request, warnings))
        except Exception:
            self._restore_pending_warnings(request.runtime, warnings)
            raise

    def reset(self, thread_id: str | None = None) -> None:
        """Clear tracking state. If thread_id given, clear only that thread."""
        with self._lock:
            if thread_id:
                for mapping in (
                    self._history,
                    self._warned,
                    self._tool_freq,
                    self._tool_name_history,
                    self._tool_freq_warned,
                    self._total_calls,
                ):
                    for key in list(mapping):
                        if key[0] == thread_id:
                            mapping.pop(key, None)
                self._total_warned = {
                    key for key in self._total_warned if key[0] != thread_id
                }
                pending_keys = set(self._pending_warnings) | set(
                    self._pending_warning_touch_order
                )
                for key in pending_keys:
                    if key[0] == thread_id:
                        self._drop_pending_warning_key_locked(key)
            else:
                self._history.clear()
                self._warned.clear()
                self._tool_freq.clear()
                self._tool_name_history.clear()
                self._tool_freq_warned.clear()
                self._total_calls.clear()
                self._total_warned.clear()
                self._pending_warnings.clear()
                self._pending_warning_touch_order.clear()
                self._fallback_run_ids.clear()
