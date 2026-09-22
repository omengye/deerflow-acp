from __future__ import annotations

import asyncio
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock

import acp
import pytest
from buzz_deerflow_adapter.acp_client import DeerFlowACPClient
from buzz_deerflow_adapter.acp_errors import ACPPromptError, ACPPromptTimeoutError


async def test_v1_prompt_outlives_request_timeout(tmp_path: Path) -> None:
    client = DeerFlowACPClient("unused", [], tmp_path, timeout_seconds=0.01)

    async def prompt(**kwargs):
        await asyncio.sleep(0.03)
        client._client.captures[kwargs["session_id"]].append("done")
        return SimpleNamespace(stop_reason="end_turn")

    client._connection = SimpleNamespace(prompt=prompt)
    assert (
        await asyncio.wait_for(client.prompt("session-1", "hello"), timeout=1) == "done"
    )


async def test_v1_prompt_timeout_cancels_without_retry(tmp_path: Path) -> None:
    client = DeerFlowACPClient("unused", [], tmp_path, prompt_timeout_seconds=0.01)

    async def prompt(**kwargs):
        await asyncio.Event().wait()

    cancel = AsyncMock()
    client._connection = SimpleNamespace(prompt=prompt, cancel=cancel)
    with pytest.raises(ACPPromptTimeoutError):
        await client.prompt("session-1", "hello")
    cancel.assert_awaited_once_with(session_id="session-1")
    assert not client._client.captures


async def test_v1_terminal_error_does_not_allow_automatic_retry(tmp_path: Path) -> None:
    client = DeerFlowACPClient("unused", [], tmp_path)
    client._connection = SimpleNamespace(
        prompt=AsyncMock(
            side_effect=acp.RequestError.internal_error({"details": "task timed out"})
        )
    )
    with pytest.raises(ACPPromptError, match="DeerFlow prompt failed"):
        await client.prompt("session-1", "hello")
