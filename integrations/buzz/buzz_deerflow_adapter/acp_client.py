"""Persistent ACP v1 client for DeerFlow Portable."""

from __future__ import annotations

import asyncio
import logging
from contextlib import AsyncExitStack
from pathlib import Path
from typing import Any

import acp
from acp import PROTOCOL_VERSION, Client, text_block
from acp.schema import (
    AgentMessageChunk,
    AllowedOutcome,
    ClientCapabilities,
    Implementation,
    ResourceContentBlock,
    TextContentBlock,
)

logger = logging.getLogger(__name__)


class _AdapterACPClient(Client):
    def __init__(self) -> None:
        self.captures: dict[str, list[str]] = {}

    async def session_update(self, session_id: str, update: Any, **kwargs: Any) -> None:
        del kwargs
        capture = self.captures.get(session_id)
        if capture is None or not isinstance(update, AgentMessageChunk):
            return
        content = update.content
        if isinstance(content, TextContentBlock):
            capture.append(content.text)
        elif isinstance(content, ResourceContentBlock):
            capture.append(f"\n[{content.name}]({content.uri})\n")

    async def request_permission(
        self, options: list[Any], session_id: str, tool_call: Any, **kwargs: Any
    ) -> acp.RequestPermissionResponse:
        del session_id, tool_call, kwargs
        for preferred in ("allow_once", "allow_always"):
            for option in options:
                if getattr(option, "kind", None) != preferred:
                    continue
                option_id = getattr(option, "option_id", None)
                if option_id is not None:
                    return acp.RequestPermissionResponse(
                        outcome=AllowedOutcome(outcome="selected", optionId=option_id)
                    )
        return acp.RequestPermissionResponse(
            outcome=acp.schema.DeniedOutcome(outcome="cancelled")
        )


class DeerFlowACPClient:
    def __init__(
        self,
        command: str,
        args: list[str],
        workspace: Path,
        *,
        timeout_seconds: float = 600,
    ) -> None:
        self.command = command
        self.args = list(args)
        self.workspace = workspace
        self.timeout_seconds = timeout_seconds
        self._client = _AdapterACPClient()
        self._stack: AsyncExitStack | None = None
        self._connection: Any = None
        self._attached_sessions: set[str] = set()

    async def open(self) -> None:
        if self._stack is not None:
            return
        stack = AsyncExitStack()
        try:
            connection, _process = await stack.enter_async_context(
                acp.spawn_agent_process(
                    self._client,
                    self.command,
                    *self.args,
                    cwd=self.workspace,
                )
            )
            async with asyncio.timeout(self.timeout_seconds):
                await connection.initialize(
                    protocol_version=PROTOCOL_VERSION,
                    client_capabilities=ClientCapabilities(),
                    client_info=Implementation(
                        name="buzz-deerflow-adapter",
                        title="Buzz DeerFlow Adapter",
                        version="0.1.0",
                    ),
                )
        except BaseException:
            await stack.aclose()
            raise
        self._stack = stack
        self._connection = connection

    async def close(self) -> None:
        stack, self._stack = self._stack, None
        self._connection = None
        self._attached_sessions.clear()
        if stack is not None:
            await stack.aclose()

    async def attach_or_create(self, existing_session_id: str | None) -> str:
        if self._connection is None:
            raise RuntimeError("ACP client is not open")
        if existing_session_id in self._attached_sessions:
            return existing_session_id
        if existing_session_id:
            try:
                async with asyncio.timeout(self.timeout_seconds):
                    await self._connection.load_session(
                        cwd=str(self.workspace),
                        session_id=existing_session_id,
                        mcp_servers=[],
                    )
                self._attached_sessions.add(existing_session_id)
                return existing_session_id
            except Exception as exc:  # noqa: BLE001 - invalid persisted sessions fall back
                logger.info(
                    "Could not load ACP session %s; creating a replacement: %s",
                    existing_session_id,
                    type(exc).__name__,
                )
        async with asyncio.timeout(self.timeout_seconds):
            response = await self._connection.new_session(
                cwd=str(self.workspace), mcp_servers=[]
            )
        self._attached_sessions.add(response.session_id)
        return response.session_id

    async def prompt(self, session_id: str, prompt: str) -> str:
        if self._connection is None:
            raise RuntimeError("ACP client is not open")
        chunks: list[str] = []
        self._client.captures[session_id] = chunks
        try:
            async with asyncio.timeout(self.timeout_seconds):
                await self._connection.prompt(
                    session_id=session_id,
                    prompt=[text_block(prompt)],
                )
            await asyncio.sleep(0)
            return "".join(chunks).strip()
        finally:
            self._client.captures.pop(session_id, None)
