"""ACP-native permission broker and LangChain tool middleware."""

from __future__ import annotations

import asyncio
import logging
import threading
from collections.abc import Awaitable, Callable, Mapping
from typing import Any, cast, override

from acp import schema
from langchain.agents import AgentState
from langchain.agents.middleware import AgentMiddleware
from langchain_core.messages import ToolMessage
from langgraph.prebuilt.tool_node import ToolCallRequest
from langgraph.types import Command

from .policy import LocalACPCapabilityPolicy, tool_kind
from .session_store import (
    DEFAULT_SESSION_APPROVAL_MODE,
    SESSION_APPROVAL_MODES,
    SessionApprovalMode,
    normalize_session_approval_mode,
)

logger = logging.getLogger(__name__)

PermissionHandler = Callable[
    [list[schema.PermissionOption], str, schema.ToolCallUpdate],
    Awaitable[schema.RequestPermissionResponse],
]
SessionOwnerResolver = Callable[[str], str | None]
_DEFAULT_CONNECTION_ID = "__default_acp_connection__"


class ACPPermissionBroker:
    """Route permission requests from main or subagent event loops to ACP."""

    def __init__(
        self,
        policy: LocalACPCapabilityPolicy,
        session_owner: SessionOwnerResolver | None = None,
    ) -> None:
        self.policy = policy
        self._session_owner = session_owner
        self._handlers: dict[
            str, tuple[PermissionHandler, asyncio.AbstractEventLoop]
        ] = {}
        self._state_lock = threading.Lock()
        self._always_allowed: set[tuple[str, str]] = set()
        self._always_rejected: set[tuple[str, str]] = set()
        self._session_approval_modes: dict[str, SessionApprovalMode] = {}

    def bind(
        self,
        handler: PermissionHandler,
        connection_id: str = _DEFAULT_CONNECTION_ID,
    ) -> None:
        registration = (handler, asyncio.get_running_loop())
        with self._state_lock:
            self._handlers[connection_id] = registration

    def unbind(self, connection_id: str = _DEFAULT_CONNECTION_ID) -> None:
        with self._state_lock:
            self._handlers.pop(connection_id, None)

    def clear_session(self, session_id: str) -> None:
        with self._state_lock:
            self._session_approval_modes.pop(session_id, None)
            self._always_allowed = {
                item for item in self._always_allowed if item[0] != session_id
            }
            self._always_rejected = {
                item for item in self._always_rejected if item[0] != session_id
            }

    def set_session_approval_mode(
        self,
        session_id: str,
        mode: SessionApprovalMode,
        *,
        reset_decisions: bool = False,
    ) -> None:
        """Sync policy, optionally revoking choices even when the mode is unchanged."""
        if mode not in SESSION_APPROVAL_MODES:
            raise ValueError(f"Unsupported session approval mode: {mode}")
        normalized = normalize_session_approval_mode(mode)
        with self._state_lock:
            previous = self._session_approval_modes.get(
                session_id,
                DEFAULT_SESSION_APPROVAL_MODE,
            )
            self._session_approval_modes[session_id] = normalized
            if reset_decisions or previous != normalized:
                self._always_allowed = {
                    item for item in self._always_allowed if item[0] != session_id
                }
                self._always_rejected = {
                    item for item in self._always_rejected if item[0] != session_id
                }

    def session_approval_mode(self, session_id: str) -> SessionApprovalMode:
        with self._state_lock:
            return self._session_approval_modes.get(
                session_id,
                DEFAULT_SESSION_APPROVAL_MODE,
            )

    def _known_decision(self, session_id: str, name: str) -> bool | None:
        """Return a policy/cache decision, or ``None`` when the client must decide."""
        if not self.policy.requires_permission(name):
            return True

        decision_key = (session_id, name)
        with self._state_lock:
            mode = self._session_approval_modes.get(
                session_id,
                DEFAULT_SESSION_APPROVAL_MODE,
            )
            if mode == "allow_always":
                return True
            if mode == "reject_always":
                return False
            if decision_key in self._always_allowed:
                return True
            if decision_key in self._always_rejected:
                return False
        return None

    async def request(self, session_id: str, tool_call: Mapping[str, Any]) -> bool:
        name = str(tool_call.get("name") or "tool")
        known_decision = self._known_decision(session_id, name)
        if known_decision is not None:
            return known_decision

        decision_key = (session_id, name)

        connection_id = (
            self._session_owner(session_id)
            if self._session_owner is not None
            else _DEFAULT_CONNECTION_ID
        )
        with self._state_lock:
            registration = (
                self._handlers.get(connection_id) if connection_id is not None else None
            )
        if registration is None:
            logger.warning(
                "Denying tool %s because no ACP permission handler is connected", name
            )
            return False
        handler, handler_loop = registration
        if not handler_loop.is_running():
            logger.warning(
                "Denying tool %s because its ACP client is disconnected", name
            )
            return False

        tool_call_id = str(tool_call.get("id") or "missing_tool_call_id")
        update = schema.ToolCallUpdate(
            tool_call_id=tool_call_id,
            title=name,
            kind=tool_kind(name),
            status="pending",
            raw_input=tool_call.get("args") or {},
        )
        options = [
            schema.PermissionOption(
                option_id=f"{tool_call_id}:allow_once",
                name="Allow once",
                kind="allow_once",
            ),
            schema.PermissionOption(
                option_id=f"{tool_call_id}:allow_always",
                name=f"Always allow {name} in this session",
                kind="allow_always",
            ),
            schema.PermissionOption(
                option_id=f"{tool_call_id}:reject_once",
                name="Reject",
                kind="reject_once",
            ),
            schema.PermissionOption(
                option_id=f"{tool_call_id}:reject_always",
                name=f"Always reject {name} in this session",
                kind="reject_always",
            ),
        ]

        async def invoke() -> schema.RequestPermissionResponse:
            return await handler(options, session_id, update)

        try:
            if asyncio.get_running_loop() is handler_loop:
                response = await invoke()
            else:
                future = asyncio.run_coroutine_threadsafe(invoke(), handler_loop)
                response = await asyncio.wrap_future(future)
        except Exception:
            logger.exception("ACP permission request failed for tool %s", name)
            return False

        outcome = response.outcome
        if getattr(outcome, "outcome", None) != "selected":
            return False
        option_id = str(getattr(outcome, "option_id", ""))
        if option_id.endswith(":allow_always"):
            with self._state_lock:
                self._always_allowed.add(decision_key)
            return True
        if option_id.endswith(":allow_once"):
            return True
        if option_id.endswith(":reject_always"):
            with self._state_lock:
                self._always_rejected.add(decision_key)
        return False

    def request_sync(self, session_id: str, tool_call: Mapping[str, Any]) -> bool:
        name = str(tool_call.get("name") or "tool")
        known_decision = self._known_decision(session_id, name)
        if known_decision is not None:
            return known_decision
        connection_id = (
            self._session_owner(session_id)
            if self._session_owner is not None
            else _DEFAULT_CONNECTION_ID
        )
        with self._state_lock:
            registration = (
                self._handlers.get(connection_id) if connection_id is not None else None
            )
        if registration is None:
            return False
        _, loop = registration
        if not loop.is_running():
            return False
        try:
            current_loop = asyncio.get_running_loop()
        except RuntimeError:
            current_loop = None
        if current_loop is loop:
            logger.error(
                "Cannot synchronously request ACP permission from its own event loop"
            )
            return False
        return asyncio.run_coroutine_threadsafe(
            self.request(session_id, tool_call), loop
        ).result()


class ACPPermissionMiddleware(AgentMiddleware[AgentState]):
    """Prevent protected tools from running before the ACP client approves."""

    def __init__(self, broker: ACPPermissionBroker) -> None:
        super().__init__()
        self.broker = broker

    @staticmethod
    def _session_id(request: ToolCallRequest) -> str:
        runtime = getattr(request, "runtime", None)
        context = getattr(runtime, "context", None)
        if isinstance(context, dict) and context.get("thread_id"):
            return str(context["thread_id"])
        config = getattr(runtime, "config", {}) if runtime is not None else {}
        return str((config.get("configurable") or {}).get("thread_id") or "")

    @staticmethod
    def _denied(request: ToolCallRequest) -> ToolMessage:
        name = str(request.tool_call.get("name") or "tool")
        return ToolMessage(
            content=f"Permission denied for tool '{name}'. Do not retry it unless the user changes their decision.",
            tool_call_id=str(request.tool_call.get("id") or "missing_tool_call_id"),
            name=name,
            status="error",
        )

    @override
    def wrap_tool_call(
        self,
        request: ToolCallRequest,
        handler: Callable[[ToolCallRequest], ToolMessage | Command],
    ) -> ToolMessage | Command:
        session_id = self._session_id(request)
        if not session_id or not self.broker.request_sync(
            session_id, cast(Mapping[str, Any], request.tool_call)
        ):
            return self._denied(request)
        return handler(request)

    @override
    async def awrap_tool_call(
        self,
        request: ToolCallRequest,
        handler: Callable[[ToolCallRequest], Awaitable[ToolMessage | Command]],
    ) -> ToolMessage | Command:
        session_id = self._session_id(request)
        if not session_id or not await self.broker.request(
            session_id, cast(Mapping[str, Any], request.tool_call)
        ):
            return self._denied(request)
        return await handler(request)
