"""Bounded FIFO request admission shared across threads and event loops.

The limiter intentionally retains no executor worker, timer task, or event-loop
primitive. Short polling sleeps make async waits cancellable while a normal
threading lock keeps one process-wide schedule safe to share with synchronous
callers and callers running on independent asyncio event loops.
"""

import asyncio
import threading
import time
from collections import deque
from time import monotonic

from langchain_core.rate_limiters import BaseRateLimiter

from deerflow.config.model_config import RequestAdmissionConfig


class AdmissionError(RuntimeError):
    """Local admission failed before an upstream model request was dispatched."""


class RequestAdmission(BaseRateLimiter):
    """Pace requests at a fixed interval through a bounded FIFO queue."""

    def __init__(self, config: RequestAdmissionConfig) -> None:
        self.config = config
        self._interval = 60 / config.requests_per_minute
        self._next = 0.0
        self._lock = threading.Lock()
        self._waiters: deque[object] = deque()

    def _try(self, ticket: object) -> bool:
        with self._lock:
            now = monotonic()
            if self._waiters and self._waiters[0] is not ticket:
                return False
            if now < self._next:
                return False
            self._next = now + self._interval
            return True

    def _try_or_enqueue(self, *, blocking: bool) -> tuple[bool, object | None]:
        """Atomically admit now or reserve a FIFO position before handoff."""
        with self._lock:
            now = monotonic()
            if not self._waiters and now >= self._next:
                self._next = now + self._interval
                return True, None
            if not blocking:
                return False, None
            if len(self._waiters) >= self.config.max_queue_size:
                raise AdmissionError(
                    "LLM admission queue is full; reduce workload or increase queue capacity."
                )
            ticket = object()
            self._waiters.append(ticket)
            return False, ticket

    def _remove(self, ticket: object) -> None:
        with self._lock:
            self._waiters.remove(ticket)

    def _delay(self, deadline: float) -> float:
        now = monotonic()
        remaining = deadline - now
        if remaining <= 0:
            raise AdmissionError(
                "LLM admission timed out before dispatch; increase max_wait_seconds or reduce workload."
            )
        with self._lock:
            until_next = self._next - now
        # The cap is a polling maximum, not a minimum: high-RPM policies must
        # be able to wake faster than 50ms. When a schedule is already due, a
        # non-head waiter still yields instead of busy-spinning.
        return min(
            0.05,
            self._interval,
            until_next if until_next > 0 else self._interval,
            remaining,
        )

    def acquire(self, *, blocking: bool = True) -> bool:
        acquired, ticket = self._try_or_enqueue(blocking=blocking)
        if acquired:
            return True
        if ticket is None:
            return False

        deadline = monotonic() + self.config.max_wait_seconds
        try:
            while True:
                delay = self._delay(deadline)
                if self._try(ticket):
                    return True
                time.sleep(delay)
        finally:
            self._remove(ticket)

    async def aacquire(self, *, blocking: bool = True) -> bool:
        acquired, ticket = self._try_or_enqueue(blocking=blocking)
        if acquired:
            return True
        if ticket is None:
            return False

        deadline = monotonic() + self.config.max_wait_seconds
        try:
            while True:
                delay = self._delay(deadline)
                if self._try(ticket):
                    return True
                await asyncio.sleep(delay)
        finally:
            self._remove(ticket)


_registry_lock = threading.Lock()
_registry: dict[tuple[str, str], RequestAdmission] = {}


def get_request_admission(
    model_name: str,
    config: RequestAdmissionConfig,
) -> RequestAdmission:
    """Return the immutable process-wide limiter for a model or shared group."""
    # Keep explicit groups in a separate namespace so a group never collides
    # with an unrelated model that happens to have the same name.
    key = ("group", config.group) if config.group else ("model", model_name)
    with _registry_lock:
        existing = _registry.get(key)
        if existing is not None:
            if existing.config != config:
                raise ValueError(
                    "Model request admission policy changed or conflicts within a shared "
                    "group; align settings and restart the service."
                )
            return existing

        limiter = RequestAdmission(config)
        _registry[key] = limiter
        return limiter
