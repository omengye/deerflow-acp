from __future__ import annotations

import asyncio
import base64
import json
import sys
from pathlib import Path

import pytest
from buzz_deerflow_adapter.acp_v2_client import (
    ACPV2Error,
    DeerFlowACPV2Client,
    _TextProjection,
    _TurnTracker,
)


def test_snapshot_replaces_prior_chunks() -> None:
    projection = _TextProjection()
    projection.apply(
        {
            "sessionUpdate": "agent_message_chunk",
            "messageId": "answer",
            "content": {"type": "text", "text": "draft"},
        }
    )
    projection.apply(
        {
            "sessionUpdate": "agent_message",
            "messageId": "answer",
            "content": [{"type": "text", "text": "final"}],
        }
    )
    assert projection.text() == "final"


async def test_tracker_ignores_ready_idle_before_running() -> None:
    tracker = _TurnTracker()
    tracker.done = asyncio.get_running_loop().create_future()
    tracker.apply({"sessionUpdate": "state_update", "state": "idle"})
    assert not tracker.done.done()
    tracker.apply({"sessionUpdate": "state_update", "state": "running"})
    tracker.apply({"sessionUpdate": "state_update", "state": "idle"})
    assert (await tracker.done)["state"] == "idle"


async def test_v2_client_acks_then_waits_for_idle(tmp_path: Path) -> None:
    fixture = Path(__file__).parent / "fixtures" / "fake_acp_v2_agent.py"
    client = DeerFlowACPV2Client(
        sys.executable,
        [str(fixture)],
        tmp_path,
        timeout_seconds=5,
    )
    await client.open()
    try:
        session_id = await client.attach_or_create(None)
        updates: list[dict] = []
        answer = await client.prompt(session_id, "hello", on_update=updates.append)
        assert session_id.startswith("session-")
        assert answer == "Fake DeerFlow received: hello"
        assert any(update.get("sessionUpdate") == "tool_call" for update in updates)
        assert any(
            update.get("sessionUpdate") == "tool_call_update" for update in updates
        )
    finally:
        await client.close()


async def test_v2_client_surfaces_terminal_error(tmp_path: Path) -> None:
    fixture = Path(__file__).parent / "fixtures" / "fake_acp_v2_agent.py"
    client = DeerFlowACPV2Client(
        sys.executable,
        [str(fixture)],
        tmp_path,
        timeout_seconds=5,
    )
    await client.open()
    try:
        session_id = await client.attach_or_create(None)
        with pytest.raises(ACPV2Error, match="fake failure"):
            await client.prompt(session_id, "fail")
    finally:
        await client.close()


async def test_v2_client_answers_permission_requests(tmp_path: Path) -> None:
    fixture = Path(__file__).parent / "fixtures" / "fake_acp_v2_agent.py"
    client = DeerFlowACPV2Client(
        sys.executable,
        [str(fixture)],
        tmp_path,
        timeout_seconds=5,
    )
    await client.open()
    try:
        session_id = await client.attach_or_create(None)
        answer = await client.prompt(session_id, "permission")
        assert answer == "Fake DeerFlow received: permission"
    finally:
        await client.close()


async def test_v2_client_cancels_a_timed_out_turn(tmp_path: Path) -> None:
    fixture = Path(__file__).parent / "fixtures" / "fake_acp_v2_agent.py"
    client = DeerFlowACPV2Client(
        sys.executable,
        [str(fixture)],
        tmp_path,
        timeout_seconds=5,
    )
    await client.open()
    try:
        session_id = await client.attach_or_create(None)
        client.timeout_seconds = 0.05
        with pytest.raises(TimeoutError, match="exceeded"):
            await client.prompt(session_id, "hang")
    finally:
        await client.close()


def test_permission_handler_prefers_allow_once() -> None:
    response = DeerFlowACPV2Client._permission_response(
        {
            "options": [
                {"optionId": "always", "kind": "allow_always"},
                {"optionId": "once", "kind": "allow_once"},
            ]
        }
    )
    assert response == {"outcome": {"outcome": "selected", "optionId": "once"}}


async def test_v2_client_preserves_large_image_and_file_blocks(
    tmp_path: Path, monkeypatch
) -> None:
    fixture = Path(__file__).parent / "fixtures" / "fake_acp_v2_agent.py"
    captured = tmp_path / "prompts.jsonl"
    monkeypatch.setenv("FAKE_ACP_PROMPTS", str(captured))
    client = DeerFlowACPV2Client(
        sys.executable, [str(fixture)], tmp_path, timeout_seconds=5
    )
    blocks = [
        {"type": "text", "text": "compare attachments"},
        {
            "type": "image",
            "mimeType": "image/png",
            "data": base64.b64encode(b"x" * 200000).decode(),
        },
        {
            "type": "resource_link",
            "uri": (tmp_path / "report.pdf").as_uri(),
            "name": "report.pdf",
            "mimeType": "application/pdf",
            "size": 42,
        },
    ]
    await client.open()
    try:
        assert client.supports_images
        session = await client.attach_or_create(None)
        assert "compare attachments" in await client.prompt(session, blocks)
        assert json.loads(captured.read_text())["prompt"] == blocks
    finally:
        await client.close()


async def test_v2_client_rejects_unadvertised_images_before_prompt(
    tmp_path: Path, monkeypatch
) -> None:
    monkeypatch.setenv("FAKE_ACP_NO_IMAGES", "1")
    fixture = Path(__file__).parent / "fixtures" / "fake_acp_v2_agent.py"
    client = DeerFlowACPV2Client(
        sys.executable, [str(fixture)], tmp_path, timeout_seconds=5
    )
    await client.open()
    try:
        session = await client.attach_or_create(None)
        with pytest.raises(ACPV2Error, match="did not advertise image"):
            await client.prompt(
                session, [{"type": "image", "data": "eA==", "mimeType": "image/png"}]
            )
        assert "still usable" in await client.prompt(session, "still usable")
    finally:
        await client.close()
