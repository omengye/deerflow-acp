from __future__ import annotations

import asyncio
import json
import sys
from pathlib import Path

from raft_deerflow_adapter.app import AdapterApp
from raft_deerflow_adapter.config import AdapterConfig


async def test_wake_to_raft_check_to_acp_prompt_to_raft_send(
    tmp_path: Path, monkeypatch
) -> None:
    fixtures = Path(__file__).parent / "fixtures"
    inbox = tmp_path / "inbox.txt"
    inbox.write_text(
        "[target=#general msg=deadbeef time=2026-09-01 01:02:03Z "
        "type=human] @alice: hello from Raft\n"
        "No more new inbox messages.\n",
        encoding="utf-8",
    )
    sent = tmp_path / "sent.jsonl"
    monkeypatch.setenv("FAKE_RAFT_INBOX", str(inbox))
    monkeypatch.setenv("FAKE_RAFT_CHECKED", str(tmp_path / "checked"))
    monkeypatch.setenv("FAKE_RAFT_READY", str(tmp_path / "ready"))
    monkeypatch.setenv("FAKE_RAFT_SENT", str(sent))

    config = AdapterConfig(
        raft_command=sys.executable,
        raft_args=[str(fixtures / "fake_raft_cli.py")],
        raft_profile="test-profile",
        raft_agent_id="agent-1",
        start_bridge=True,
        poll_interval_seconds=30,
        bridge_restart_seconds=0.1,
        deerflow_command=sys.executable,
        deerflow_args=[str(fixtures / "fake_acp_agent.py")],
        workspace=tmp_path,
        acp_timeout_seconds=10,
        state_path=tmp_path / "state.sqlite3",
        wake_host="127.0.0.1",
        wake_port=0,
        wake_token="test-token",
        runtime_session="test-runtime",
    ).validated()
    app = AdapterApp(config)
    await app.start()
    try:
        async with asyncio.timeout(15):
            while not sent.exists() or not sent.read_text(encoding="utf-8").strip():
                await asyncio.sleep(0.05)
        payload = json.loads(sent.read_text(encoding="utf-8").splitlines()[0])
        assert payload["target"] == "#general:deadbeef"
        assert "--target-confirmed" not in payload["args"]
        assert "hello from Raft" in payload["content"]
        assert "Fake DeerFlow received" in payload["content"]

        # The Raft CLI already acked the inbox row; local durable state proves
        # the adapter completed the ACP turn and Raft reply before marking done.
        async with asyncio.timeout(5):
            while True:
                row = app.state._conn().execute(
                    "SELECT status, attempts FROM inbox_messages"
                ).fetchone()
                if row is not None and row["status"] == "done":
                    break
                await asyncio.sleep(0.01)
        assert tuple(row) == ("done", 0)
        session = app.state._conn().execute(
            "SELECT conversation_key, session_id FROM sessions"
        ).fetchone()
        assert session["conversation_key"] == "#general:deadbeef"
        assert session["session_id"].startswith("session-")

        activity = app.activity.drain(100)["events"]
        names = [event["hookEventName"] for event in activity]
        assert names == [
            "SessionStart",
            "UserPromptSubmit",
            "ThinkingStart",
            "ThinkingEnd",
            "PreToolUse",
            "PostToolUse",
            "ModelResponseStart",
            "Stop",
        ]
        serialized = json.dumps(activity)
        assert "private reasoning" not in serialized
        assert "must not be exported" not in serialized
    finally:
        await app.close()
