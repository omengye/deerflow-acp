"""LLM error handling middleware with retry/backoff and user-facing fallbacks."""

from __future__ import annotations

import asyncio
import logging
import random
import threading
import time
from collections import deque
from collections.abc import Awaitable, Callable
from contextlib import asynccontextmanager, contextmanager
from contextvars import ContextVar
from email.utils import parsedate_to_datetime
from typing import Any, override

from langchain.agents import AgentState
from langchain.agents.middleware import AgentMiddleware
from langchain.agents.middleware.types import (
    ModelCallResult,
    ModelRequest,
    ModelResponse,
)
from langchain_core.messages import AIMessage
from langgraph.errors import GraphBubbleUp

from deerflow.config import get_app_config

logger = logging.getLogger(__name__)

_RETRIABLE_STATUS_CODES = {408, 409, 425, 429, 500, 502, 503, 504}
_BUSY_PATTERNS = (
    "server busy",
    "temporarily unavailable",
    # Some OpenAI-compatible gateways incorrectly translate an upstream 5xx
    # into a 400 invalid_request_error.  Match the explicit upstream failure
    # message without making ordinary client-side 400 responses retriable.
    "upstream request failed",
    "try again later",
    "please retry",
    "please try again",
    "overloaded",
    "high demand",
    "rate limit",
    "负载较高",
    "服务繁忙",
    "稍后重试",
    "请稍后重试",
)
_QUOTA_PATTERNS = (
    "insufficient_quota",
    "quota",
    "billing",
    "credit",
    "payment",
    "余额不足",
    "超出限额",
    "额度不足",
    "欠费",
)
_AUTH_PATTERNS = (
    "authentication",
    "unauthorized",
    "invalid api key",
    "invalid_api_key",
    "permission",
    "forbidden",
    "access denied",
    "无权",
    "未授权",
)
_CONTENT_POLICY_PATTERNS = (
    "data_inspection_failed",
    "datainspectionfailed",
    "content_filter",
    "content_policy",
    "inappropriate content",
    "内容违规",
    "违规内容",
    "内容审核",
)

_BURST_RATE_PATTERNS = (
    "limit_burst_rate",
    "burst rate",
    "burst-rate",
    "request rate increased too quickly",
    "slope limit",
)


class LLMConcurrencyTimeoutError(TimeoutError):
    """Raised when a model call cannot obtain the process-wide slot in time."""


class _AsyncWaiter:
    __slots__ = ("loop", "event", "granted")

    def __init__(self, loop: asyncio.AbstractEventLoop, event: asyncio.Event) -> None:
        self.loop = loop
        self.event = event
        self.granted = False


class _ProcessWideLimiter:
    """Cancellation-safe limiter shared by sync calls and multiple event loops."""

    def __init__(self, limit: int) -> None:
        self._lock = threading.Lock()
        self._cond = threading.Condition(self._lock)
        self._in_flight = 0
        self._limit = max(0, limit)
        self._async_waiters: deque[_AsyncWaiter] = deque()

    @property
    def limit(self) -> int:
        return self._limit

    @property
    def in_flight(self) -> int:
        with self._lock:
            return self._in_flight

    @property
    def queued(self) -> int:
        with self._lock:
            return len(self._async_waiters)

    def acquire_sync(self, timeout_seconds: float | None = None) -> bool:
        deadline = None if timeout_seconds is None else time.monotonic() + timeout_seconds
        with self._cond:
            while not self._try_acquire_locked():
                remaining = None if deadline is None else deadline - time.monotonic()
                if remaining is not None and remaining <= 0:
                    return False
                self._cond.wait(timeout=remaining)
            return True

    def release(self) -> None:
        with self._cond:
            while self._async_waiters:
                waiter = self._async_waiters.popleft()
                waiter.granted = True
                if self._wake_locked(waiter):
                    return
            if self._in_flight > 0:
                self._in_flight -= 1
            self._cond.notify()

    async def acquire_async(self) -> None:
        loop = asyncio.get_running_loop()
        waiter = _AsyncWaiter(loop=loop, event=asyncio.Event())
        with self._cond:
            if self._try_acquire_locked():
                return
            self._async_waiters.append(waiter)
        try:
            await waiter.event.wait()
        except asyncio.CancelledError:
            with self._cond:
                if waiter.granted:
                    self._handoff_granted_permit_locked()
                else:
                    try:
                        self._async_waiters.remove(waiter)
                    except ValueError:
                        # A concurrent release dequeued it and reserved the
                        # permit immediately before cancellation was observed.
                        if waiter.granted:
                            self._handoff_granted_permit_locked()
            raise

    def _try_acquire_locked(self) -> bool:
        if self._in_flight < self._limit:
            self._in_flight += 1
            return True
        return False

    def _handoff_granted_permit_locked(self) -> None:
        while self._async_waiters:
            waiter = self._async_waiters.popleft()
            waiter.granted = True
            if self._wake_locked(waiter):
                return
        if self._in_flight > 0:
            self._in_flight -= 1
        self._cond.notify()

    @staticmethod
    def _wake_locked(waiter: _AsyncWaiter) -> bool:
        try:
            waiter.loop.call_soon_threadsafe(waiter.event.set)
            return True
        except RuntimeError:
            return False


_LIMITER_LOCK = threading.Lock()
_PROCESS_LIMITER: _ProcessWideLimiter | None = None
_CAP_RESOLVED = False
_LLM_SLOT_OWNER: ContextVar[tuple[str, int] | None] = ContextVar("llm_slot_owner", default=None)
_LLM_SLOT_DEPTH: ContextVar[int] = ContextVar("llm_slot_depth", default=0)


def _apply_configured_cap(limit: int) -> None:
    global _CAP_RESOLVED, _PROCESS_LIMITER
    if _CAP_RESOLVED:
        return
    with _LIMITER_LOCK:
        if _CAP_RESOLVED:
            return
        _CAP_RESOLVED = True
        if limit > 0:
            _PROCESS_LIMITER = _ProcessWideLimiter(limit)


def _ensure_process_limiter() -> _ProcessWideLimiter | None:
    if not _CAP_RESOLVED:
        try:
            _apply_configured_cap(get_app_config().llm_call.max_concurrent_calls)
        except (FileNotFoundError, RuntimeError, AttributeError):
            _apply_configured_cap(0)
    return _PROCESS_LIMITER


def _reset_process_limiter_for_tests() -> None:
    global _CAP_RESOLVED, _PROCESS_LIMITER
    with _LIMITER_LOCK:
        _PROCESS_LIMITER = None
        _CAP_RESOLVED = False


@contextmanager
def llm_call_slot_sync(timeout_seconds: float | None = None):
    """Acquire the shared LLM slot for a direct synchronous model call."""
    owner = ("thread", threading.get_ident())
    if _LLM_SLOT_OWNER.get() == owner:
        depth_token = _LLM_SLOT_DEPTH.set(_LLM_SLOT_DEPTH.get() + 1)
        try:
            yield
        finally:
            _LLM_SLOT_DEPTH.reset(depth_token)
        return

    limiter = _ensure_process_limiter()
    if limiter is None:
        yield
        return
    if timeout_seconds is None:
        timeout_seconds = get_app_config().llm_call.queue_timeout_seconds
    started = time.monotonic()
    if not limiter.acquire_sync(timeout_seconds):
        raise LLMConcurrencyTimeoutError(
            f"Timed out after {timeout_seconds}s waiting for an LLM concurrency slot"
        )
    waited = time.monotonic() - started
    if waited >= 0.1:
        logger.info(
            "LLM call acquired process-wide slot after %.3fs (in_flight=%d, limit=%d)",
            waited,
            limiter.in_flight,
            limiter.limit,
        )
    owner_token = _LLM_SLOT_OWNER.set(owner)
    depth_token = _LLM_SLOT_DEPTH.set(1)
    try:
        yield
    finally:
        _LLM_SLOT_DEPTH.reset(depth_token)
        _LLM_SLOT_OWNER.reset(owner_token)
        limiter.release()


@asynccontextmanager
async def llm_call_slot_async(timeout_seconds: float | None = None):
    """Acquire the shared LLM slot without binding it to one event loop."""
    task = asyncio.current_task()
    owner = ("task", id(task))
    if _LLM_SLOT_OWNER.get() == owner:
        depth_token = _LLM_SLOT_DEPTH.set(_LLM_SLOT_DEPTH.get() + 1)
        try:
            yield
        finally:
            _LLM_SLOT_DEPTH.reset(depth_token)
        return

    limiter = _ensure_process_limiter()
    if limiter is None:
        yield
        return
    if timeout_seconds is None:
        timeout_seconds = get_app_config().llm_call.queue_timeout_seconds
    started = time.monotonic()
    acquired = False
    owner_token = None
    depth_token = None
    try:
        try:
            async with asyncio.timeout(timeout_seconds):
                await limiter.acquire_async()
                acquired = True
        except TimeoutError as exc:
            raise LLMConcurrencyTimeoutError(
                f"Timed out after {timeout_seconds}s waiting for an LLM concurrency slot"
            ) from exc
        waited = time.monotonic() - started
        if waited >= 0.1:
            logger.info(
                "LLM call acquired process-wide slot after %.3fs (in_flight=%d, limit=%d)",
                waited,
                limiter.in_flight,
                limiter.limit,
            )
        owner_token = _LLM_SLOT_OWNER.set(owner)
        depth_token = _LLM_SLOT_DEPTH.set(1)
        yield
    finally:
        if depth_token is not None:
            _LLM_SLOT_DEPTH.reset(depth_token)
        if owner_token is not None:
            _LLM_SLOT_OWNER.reset(owner_token)
        if acquired:
            limiter.release()


class LLMErrorHandlingMiddleware(AgentMiddleware[AgentState]):
    """Retry transient LLM errors and surface graceful assistant messages."""

    retry_max_attempts: int = 3
    retry_base_delay_ms: int = 1000
    retry_cap_delay_ms: int = 8000
    burst_retry_base_delay_ms: int = 5000
    concurrency_queue_timeout_seconds: float = 60.0

    circuit_failure_threshold: int = 5
    circuit_recovery_timeout_sec: int = 60

    def __init__(self, **kwargs: Any) -> None:
        super().__init__(**kwargs)

        # Load Circuit Breaker configs from app config if available, fall back to defaults
        try:
            app_config = get_app_config()
            self.circuit_failure_threshold = app_config.circuit_breaker.failure_threshold
            self.circuit_recovery_timeout_sec = app_config.circuit_breaker.recovery_timeout_sec
            self.retry_max_attempts = app_config.llm_call.retry_max_attempts
            self.retry_base_delay_ms = app_config.llm_call.retry_base_delay_ms
            self.retry_cap_delay_ms = app_config.llm_call.retry_cap_delay_ms
            self.burst_retry_base_delay_ms = app_config.llm_call.burst_retry_base_delay_ms
            self.concurrency_queue_timeout_seconds = app_config.llm_call.queue_timeout_seconds
            _apply_configured_cap(app_config.llm_call.max_concurrent_calls)
        except (FileNotFoundError, RuntimeError):
            # Gracefully fall back to class defaults in test environments
            pass

        # Circuit Breaker state
        self._circuit_lock = threading.Lock()
        self._circuit_failure_count = 0
        self._circuit_open_until = 0.0
        self._circuit_state = "closed"
        self._circuit_probe_in_flight = False
        self._circuit_probe_token: object | None = None

    def _check_circuit(self, *, probe_token: object | None = None) -> bool:
        """Returns True if circuit is OPEN (fast fail), False otherwise."""
        with self._circuit_lock:
            now = time.time()

            if self._circuit_state == "open":
                if now < self._circuit_open_until:
                    return True
                self._circuit_state = "half_open"
                self._circuit_probe_in_flight = False
                self._circuit_probe_token = None

            if self._circuit_state == "half_open":
                if self._circuit_probe_in_flight:
                    return True
                self._circuit_probe_in_flight = True
                self._circuit_probe_token = probe_token
                return False

            return False

    def _record_success(self, *, probe_token: object | None = None) -> None:
        with self._circuit_lock:
            if self._circuit_state == "open":
                return
            if self._circuit_state == "half_open" and self._circuit_probe_token is not probe_token:
                return
            if self._circuit_state != "closed" or self._circuit_failure_count > 0:
                logger.info("Circuit breaker reset (Closed). LLM service recovered.")
            self._circuit_failure_count = 0
            self._circuit_open_until = 0.0
            self._circuit_state = "closed"
            self._circuit_probe_in_flight = False
            self._circuit_probe_token = None

    def _record_failure(self, *, probe_token: object | None = None) -> None:
        with self._circuit_lock:
            if self._circuit_state == "half_open":
                if self._circuit_probe_token is not probe_token:
                    return
                self._circuit_open_until = time.time() + self.circuit_recovery_timeout_sec
                self._circuit_state = "open"
                self._circuit_probe_in_flight = False
                self._circuit_probe_token = None
                logger.error(
                    "Circuit breaker probe failed (Open). Will probe again after %ds.",
                    self.circuit_recovery_timeout_sec,
                )
                return

            if self._circuit_state == "open":
                return

            self._circuit_failure_count += 1
            if self._circuit_failure_count >= self.circuit_failure_threshold:
                self._circuit_open_until = time.time() + self.circuit_recovery_timeout_sec
                if self._circuit_state != "open":
                    self._circuit_state = "open"
                    self._circuit_probe_in_flight = False
                    self._circuit_probe_token = None
                    logger.error(
                        "Circuit breaker tripped (Open). Threshold reached (%d). Will probe after %ds.",
                        self.circuit_failure_threshold,
                        self.circuit_recovery_timeout_sec,
                    )

    def _release_half_open_probe(self, *, probe_token: object | None = None) -> None:
        with self._circuit_lock:
            if self._circuit_state == "half_open" and self._circuit_probe_token is probe_token:
                self._circuit_probe_in_flight = False
                self._circuit_probe_token = None

    def _classify_error(self, exc: BaseException) -> tuple[bool, str]:
        detail = _extract_error_detail(exc)
        lowered = detail.lower()
        error_code = _extract_error_code(exc)
        status_code = _extract_status_code(exc)

        if isinstance(exc, LLMConcurrencyTimeoutError):
            return False, "concurrency_limit"

        if _matches_any(lowered, _QUOTA_PATTERNS) or _matches_any(str(error_code).lower(), _QUOTA_PATTERNS):
            return False, "quota"
        if _matches_any(lowered, _AUTH_PATTERNS):
            return False, "auth"
        if _matches_any(lowered, _CONTENT_POLICY_PATTERNS) or _matches_any(str(error_code).lower(), _CONTENT_POLICY_PATTERNS):
            return False, "content_policy"
        if status_code == 429 and (
            _matches_any(lowered, _BURST_RATE_PATTERNS)
            or _matches_any(str(error_code).lower(), _BURST_RATE_PATTERNS)
        ):
            return True, "burst_rate"

        exc_name = exc.__class__.__name__
        if exc_name in {
            "APITimeoutError",
            "APIConnectionError",
            "InternalServerError",
            "ReadError",  # httpx.ReadError: connection dropped mid-stream
            "RemoteProtocolError",  # httpx: server closed connection unexpectedly
        }:
            return True, "transient"
        if status_code in _RETRIABLE_STATUS_CODES:
            return True, "transient"
        if _matches_any(lowered, _BUSY_PATTERNS):
            return True, "busy"

        return False, "generic"

    def _max_attempts_for(self, reason: str) -> int:
        return min(self.retry_max_attempts, 2) if reason == "burst_rate" else self.retry_max_attempts

    def _bounded_model_call_sync(
        self,
        request: ModelRequest,
        handler: Callable[[ModelRequest], ModelResponse],
    ) -> ModelResponse:
        with llm_call_slot_sync(self.concurrency_queue_timeout_seconds):
            return handler(request)

    async def _bounded_model_call(
        self,
        request: ModelRequest,
        handler: Callable[[ModelRequest], Awaitable[ModelResponse]],
    ) -> ModelResponse:
        async with llm_call_slot_async(self.concurrency_queue_timeout_seconds):
            return await handler(request)

    def _build_retry_delay_ms(self, attempt: int, exc: BaseException, reason: str = "transient") -> int:
        retry_after = _extract_retry_after_ms(exc)
        if retry_after is not None:
            return min(retry_after, self.retry_cap_delay_ms)
        base_delay = self.burst_retry_base_delay_ms if reason == "burst_rate" else self.retry_base_delay_ms
        backoff = base_delay * (2 ** max(0, attempt - 1))
        capped_backoff = min(backoff, self.retry_cap_delay_ms)
        # Full jitter (AWS-recommended): sample uniformly from [0, capped_backoff] so
        # concurrent callers hit by the same burst-rate 429 (e.g. provider "slope"
        # limits) don't realign their retries on the same instant.
        return int(random.uniform(0, capped_backoff))

    def _build_retry_message(self, attempt: int, wait_ms: int, reason: str) -> str:
        seconds = max(1, round(wait_ms / 1000))
        reason_text = {
            "busy": "provider is busy",
            "burst_rate": "provider is throttling request burst rate",
        }.get(reason, "provider request failed temporarily")
        return f"LLM request retry {attempt}/{self._max_attempts_for(reason)}: {reason_text}. Retrying in {seconds}s."

    def _build_circuit_breaker_message(self) -> str:
        return "The configured LLM provider is currently unavailable due to continuous failures. Circuit breaker is engaged to protect the system. Please wait a moment before trying again."

    def _build_user_message(self, exc: BaseException, reason: str) -> str:
        detail = _extract_error_detail(exc)
        if reason == "quota":
            return "The configured LLM provider rejected the request because the account is out of quota, billing is unavailable, or usage is restricted. Please fix the provider account and try again."
        if reason == "auth":
            return "The configured LLM provider rejected the request because authentication or access is invalid. Please check the provider credentials and try again."
        if reason == "content_policy":
            return "The configured LLM provider rejected this request because the input was flagged by its content safety policy. Please revise the message and try again."
        if reason == "burst_rate":
            return "The configured LLM provider is temporarily throttling requests because the request rate increased too quickly. Please wait a moment and try again."
        if reason == "concurrency_limit":
            return "The server is handling the configured maximum number of model calls. The request timed out waiting for capacity; please try again shortly."
        if reason in {"busy", "transient"}:
            return "The configured LLM provider is temporarily unavailable after multiple retries. Please wait a moment and continue the conversation."
        return f"LLM request failed: {detail}"

    def _emit_retry_event(self, attempt: int, wait_ms: int, reason: str) -> None:
        try:
            from langgraph.config import get_stream_writer

            writer = get_stream_writer()
            writer(
                {
                    "type": "llm_retry",
                    "attempt": attempt,
                    "max_attempts": self._max_attempts_for(reason),
                    "wait_ms": wait_ms,
                    "reason": reason,
                    "message": self._build_retry_message(attempt, wait_ms, reason),
                }
            )
        except Exception:
            logger.debug("Failed to emit llm_retry event", exc_info=True)

    def _emit_failure_event(self, exc: BaseException | None, reason: str, *, retriable: bool) -> None:
        """Emit a machine-readable terminal failure for run-status tracking."""
        try:
            from langgraph.config import get_stream_writer

            message = self._build_circuit_breaker_message() if exc is None else self._build_user_message(exc, reason)
            get_stream_writer()(
                {
                    "type": "llm_failure",
                    "reason": reason,
                    "retriable": retriable,
                    "message": message,
                }
            )
        except Exception:
            logger.debug("Failed to emit llm_failure event", exc_info=True)

    @override
    def wrap_model_call(
        self,
        request: ModelRequest,
        handler: Callable[[ModelRequest], ModelResponse],
    ) -> ModelCallResult:
        probe_token = object()
        if self._check_circuit(probe_token=probe_token):
            self._emit_failure_event(None, "circuit_open", retriable=True)
            return AIMessage(content=self._build_circuit_breaker_message())

        attempt = 1
        while True:
            try:
                response = self._bounded_model_call_sync(request, handler)
                self._record_success(probe_token=probe_token)
                return response
            except GraphBubbleUp:
                # Preserve LangGraph control-flow signals (interrupt/pause/resume).
                self._release_half_open_probe(probe_token=probe_token)
                raise
            except Exception as exc:
                retriable, reason = self._classify_error(exc)
                if reason == "content_policy":
                    logger.warning(
                        "LLM call rejected by content policy (attempt %d): %s",
                        attempt,
                        _extract_error_detail(exc),
                    )
                    self._release_half_open_probe(probe_token=probe_token)
                    self._emit_failure_event(exc, reason, retriable=False)
                    return AIMessage(content=self._build_user_message(exc, reason))
                max_attempts = self._max_attempts_for(reason)
                if retriable and attempt < max_attempts:
                    wait_ms = self._build_retry_delay_ms(attempt, exc, reason)
                    logger.warning(
                        "Transient LLM error on attempt %d/%d; retrying in %dms: %s",
                        attempt,
                        max_attempts,
                        wait_ms,
                        _extract_error_detail(exc),
                    )
                    self._emit_retry_event(attempt, wait_ms, reason)
                    time.sleep(wait_ms / 1000)
                    attempt += 1
                    continue
                logger.warning(
                    "LLM call failed after %d attempt(s): %s",
                    attempt,
                    _extract_error_detail(exc),
                    exc_info=exc,
                )
                if retriable and reason != "burst_rate":
                    self._record_failure(probe_token=probe_token)
                else:
                    self._release_half_open_probe(probe_token=probe_token)
                self._emit_failure_event(exc, reason, retriable=retriable)
                return AIMessage(content=self._build_user_message(exc, reason))

    @override
    async def awrap_model_call(
        self,
        request: ModelRequest,
        handler: Callable[[ModelRequest], Awaitable[ModelResponse]],
    ) -> ModelCallResult:
        probe_token = object()
        if self._check_circuit(probe_token=probe_token):
            self._emit_failure_event(None, "circuit_open", retriable=True)
            return AIMessage(content=self._build_circuit_breaker_message())

        attempt = 1
        try:
            while True:
                try:
                    response = await self._bounded_model_call(request, handler)
                    self._record_success(probe_token=probe_token)
                    return response
                except GraphBubbleUp:
                    # Preserve LangGraph control-flow signals (interrupt/pause/resume).
                    self._release_half_open_probe(probe_token=probe_token)
                    raise
                except Exception as exc:
                # asyncio shutdown — propagate instead of masking as a user-facing LLM error.
                    if isinstance(exc, RuntimeError) and "Event loop is closed" in str(exc):
                        raise
                    retriable, reason = self._classify_error(exc)
                    if reason == "content_policy":
                        logger.warning(
                            "LLM call rejected by content policy (attempt %d): %s",
                            attempt,
                            _extract_error_detail(exc),
                        )
                        self._release_half_open_probe(probe_token=probe_token)
                        self._emit_failure_event(exc, reason, retriable=False)
                        return AIMessage(content=self._build_user_message(exc, reason))
                    max_attempts = self._max_attempts_for(reason)
                    if retriable and attempt < max_attempts:
                        wait_ms = self._build_retry_delay_ms(attempt, exc, reason)
                        logger.warning(
                            "Transient LLM error on attempt %d/%d; retrying in %dms: %s",
                            attempt,
                            max_attempts,
                            wait_ms,
                            _extract_error_detail(exc),
                        )
                        self._emit_retry_event(attempt, wait_ms, reason)
                        await asyncio.sleep(wait_ms / 1000)
                        attempt += 1
                        continue
                    logger.warning(
                        "LLM call failed after %d attempt(s): %s",
                        attempt,
                        _extract_error_detail(exc),
                        exc_info=exc,
                    )
                    if retriable and reason != "burst_rate":
                        self._record_failure(probe_token=probe_token)
                    else:
                        self._release_half_open_probe(probe_token=probe_token)
                    self._emit_failure_event(exc, reason, retriable=retriable)
                    return AIMessage(content=self._build_user_message(exc, reason))
        except asyncio.CancelledError:
            # Cancellation can arrive during provider admission/call, retry
            # event delivery, or backoff.  It is not a provider failure, and
            # only this request may release the recovery probe it owns.
            self._release_half_open_probe(probe_token=probe_token)
            raise


def _matches_any(detail: str, patterns: tuple[str, ...]) -> bool:
    return any(pattern in detail for pattern in patterns)


def _extract_error_code(exc: BaseException) -> Any:
    for attr in ("code", "error_code"):
        value = getattr(exc, attr, None)
        if value not in (None, ""):
            return value

    body = getattr(exc, "body", None)
    if isinstance(body, dict):
        error = body.get("error")
        if isinstance(error, dict):
            for key in ("code", "type"):
                value = error.get(key)
                if value not in (None, ""):
                    return value
    return None


def _extract_status_code(exc: BaseException) -> int | None:
    for attr in ("status_code", "status"):
        value = getattr(exc, attr, None)
        if isinstance(value, int):
            return value
    response = getattr(exc, "response", None)
    status = getattr(response, "status_code", None)
    return status if isinstance(status, int) else None


def _extract_retry_after_ms(exc: BaseException) -> int | None:
    response = getattr(exc, "response", None)
    headers = getattr(response, "headers", None)
    if headers is None:
        return None

    raw = None
    header_name = ""
    for key in ("retry-after-ms", "Retry-After-Ms", "retry-after", "Retry-After"):
        header_name = key
        if hasattr(headers, "get"):
            raw = headers.get(key)
        if raw:
            break
    if not raw:
        return None

    try:
        multiplier = 1 if "ms" in header_name.lower() else 1000
        return max(0, int(float(raw) * multiplier))
    except (TypeError, ValueError):
        try:
            target = parsedate_to_datetime(str(raw))
            delta = target.timestamp() - time.time()
            return max(0, int(delta * 1000))
        except (TypeError, ValueError, OverflowError):
            return None


def _extract_error_detail(exc: BaseException) -> str:
    detail = str(exc).strip()
    if detail:
        return detail
    message = getattr(exc, "message", None)
    if isinstance(message, str) and message.strip():
        return message.strip()
    return exc.__class__.__name__
