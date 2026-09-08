"""Process-local ownership and operation state for ACP sessions."""

from __future__ import annotations

import asyncio
import threading
from dataclasses import dataclass
from typing import Any, Literal

SessionPhase = Literal[
    "idle",
    "loading",
    "mutating",
    "running",
    "disconnecting",
]


class SessionCoordinationError(RuntimeError):
    """Base class for invalid session ownership or lifecycle transitions."""


class SessionAttachedElsewhereError(SessionCoordinationError):
    """Raised when a second ACP connection tries to attach a live session."""


class SessionNotAttachedError(SessionCoordinationError):
    """Raised when a connection operates on a session it has not attached."""


class SessionBusyError(SessionCoordinationError):
    """Raised when a session already has an incompatible operation in flight."""


@dataclass(slots=True)
class SessionBinding:
    connection_id: str
    phase: SessionPhase = "idle"
    task: asyncio.Task[Any] | None = None


class ACPSessionCoordinator:
    """Coordinate transient ACP connection ownership without persisting it.

    The daemon runs its ACP handlers on one asyncio loop, while permission
    middleware can query ownership from worker threads. A small ordinary lock
    keeps those lookups safe and is only held for in-memory state changes.
    """

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._bindings: dict[str, SessionBinding] = {}
        self._cleanup_reservations: set[str] = set()

    def attach(self, session_id: str, connection_id: str) -> bool:
        """Attach a session and return whether a new lease was acquired."""

        with self._lock:
            if session_id in self._cleanup_reservations:
                raise SessionBusyError(f"Session {session_id} is being cleaned up")
            binding = self._bindings.get(session_id)
            if binding is None:
                self._bindings[session_id] = SessionBinding(connection_id)
                return True
            if binding.connection_id != connection_id:
                raise SessionAttachedElsewhereError(
                    f"Session {session_id} is attached to another ACP client"
                )
            if binding.phase == "disconnecting":
                raise SessionBusyError(f"Session {session_id} is disconnecting")
            return False

    def reserve_cleanup(self, session_id: str) -> bool:
        """Reserve an unattached session for destructive cleanup."""

        with self._lock:
            if (
                session_id in self._bindings
                or session_id in self._cleanup_reservations
            ):
                return False
            self._cleanup_reservations.add(session_id)
            return True

    def release_cleanup(self, session_id: str) -> None:
        with self._lock:
            self._cleanup_reservations.discard(session_id)

    def owner(self, session_id: str) -> str | None:
        with self._lock:
            binding = self._bindings.get(session_id)
            return binding.connection_id if binding is not None else None

    def require_attached(self, session_id: str, connection_id: str) -> None:
        with self._lock:
            self._require_attached_locked(session_id, connection_id)

    def begin_operation(
        self,
        session_id: str,
        connection_id: str,
        phase: Literal["loading", "mutating"],
    ) -> None:
        with self._lock:
            binding = self._require_attached_locked(session_id, connection_id)
            if binding.phase != "idle":
                raise SessionBusyError(
                    f"Session {session_id} is busy ({binding.phase})"
                )
            binding.phase = phase

    def end_operation(
        self,
        session_id: str,
        connection_id: str,
        phase: Literal["loading", "mutating"],
    ) -> None:
        with self._lock:
            binding = self._bindings.get(session_id)
            if (
                binding is not None
                and binding.connection_id == connection_id
                and binding.phase == phase
            ):
                binding.phase = "idle"

    def begin_prompt(
        self,
        session_id: str,
        connection_id: str,
        task: asyncio.Task[Any],
    ) -> None:
        with self._lock:
            binding = self._require_attached_locked(session_id, connection_id)
            if binding.phase != "idle":
                raise SessionBusyError(
                    f"Session {session_id} is busy ({binding.phase})"
                )
            binding.phase = "running"
            binding.task = task

    def end_prompt(
        self,
        session_id: str,
        connection_id: str,
        task: asyncio.Task[Any],
    ) -> None:
        with self._lock:
            binding = self._bindings.get(session_id)
            if (
                binding is not None
                and binding.connection_id == connection_id
                and binding.phase == "running"
                and binding.task is task
            ):
                binding.phase = "idle"
                binding.task = None

    def prompt_task(
        self, session_id: str, connection_id: str
    ) -> asyncio.Task[Any] | None:
        with self._lock:
            binding = self._require_attached_locked(session_id, connection_id)
            return binding.task

    def detach(self, session_id: str, connection_id: str) -> bool:
        with self._lock:
            binding = self._bindings.get(session_id)
            if binding is None:
                return False
            if binding.connection_id != connection_id:
                raise SessionAttachedElsewhereError(
                    f"Session {session_id} is attached to another ACP client"
                )
            if binding.task is not None:
                raise SessionBusyError(f"Session {session_id} has an active prompt")
            self._bindings.pop(session_id, None)
            return True

    def begin_disconnect(
        self, connection_id: str
    ) -> tuple[list[str], list[asyncio.Task[Any]]]:
        session_ids: list[str] = []
        tasks: list[asyncio.Task[Any]] = []
        with self._lock:
            for session_id, binding in self._bindings.items():
                if binding.connection_id != connection_id:
                    continue
                session_ids.append(session_id)
                if binding.task is not None and not binding.task.done():
                    tasks.append(binding.task)
                binding.phase = "disconnecting"
        return session_ids, tasks

    def finish_disconnect(self, connection_id: str) -> list[str]:
        with self._lock:
            session_ids = [
                session_id
                for session_id, binding in self._bindings.items()
                if binding.connection_id == connection_id
            ]
            for session_id in session_ids:
                self._bindings.pop(session_id, None)
            return session_ids

    def phase(self, session_id: str) -> SessionPhase | None:
        """Return the current phase for diagnostics and tests."""

        with self._lock:
            binding = self._bindings.get(session_id)
            return binding.phase if binding is not None else None

    def _require_attached_locked(
        self, session_id: str, connection_id: str
    ) -> SessionBinding:
        binding = self._bindings.get(session_id)
        if binding is None:
            raise SessionNotAttachedError(
                f"Session {session_id} is not attached to this ACP client"
            )
        if binding.connection_id != connection_id:
            raise SessionAttachedElsewhereError(
                f"Session {session_id} is attached to another ACP client"
            )
        if binding.phase == "disconnecting":
            raise SessionBusyError(f"Session {session_id} is disconnecting")
        return binding
