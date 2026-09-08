from __future__ import annotations

import asyncio
import json
import sys
from pathlib import Path

from nostr_sdk import Keys

from buzz_deerflow_adapter.app import AdapterApp
from buzz_deerflow_adapter.config import AdapterConfig


async def test_buzz_event_to_acp_prompt_to_threaded_reply(
    tmp_path: Path, monkeypatch
) -> None:
    fixtures = Path(__file__).parent / "fixtures"
    channel_id = "11111111-1111-1111-1111-111111111111"
    event_id = "c" * 64
    agent_keys = Keys.generate()
    agent_pubkey = agent_keys.public_key().to_hex()
    channels = tmp_path / "channels.json"
    inbox = tmp_path / "inbox.json"
    sent = tmp_path / "sent.jsonl"
    channels.write_text(
        json.dumps([{"channel_id": channel_id, "name": "General"}]),
        encoding="utf-8",
    )
    inbox.write_text(
        json.dumps(
            [
                {
                    "channel_id": channel_id,
                    "id": event_id,
                    "pubkey": "b" * 64,
                    "created_at": 100,
                    "kind": 9,
                    "content": "hello from Buzz",
                    "tags": [["h", channel_id], ["p", agent_pubkey]],
                }
            ]
        ),
        encoding="utf-8",
    )
    monkeypatch.setenv("BUZZ_PRIVATE_KEY", agent_keys.secret_key().to_bech32())
    monkeypatch.setenv("FAKE_BUZZ_CHANNELS", str(channels))
    monkeypatch.setenv("FAKE_BUZZ_DMS", str(tmp_path / "missing-dms.json"))
    monkeypatch.setenv("FAKE_BUZZ_INBOX", str(inbox))
    monkeypatch.setenv("FAKE_BUZZ_SENT", str(sent))

    config = AdapterConfig(
        buzz_command=sys.executable,
        buzz_args=[str(fixtures / "fake_buzz_cli.py")],
        relay_url="wss://buzz.sprwhisp.cc",
        agent_pubkey=agent_pubkey,
        include_dms=False,
        replay_existing=True,
        poll_interval_seconds=30,
        deerflow_command=sys.executable,
        deerflow_args=[
            str(fixtures / "fake_acp_v2_agent.py"),
            "--protocol",
            "v2",
        ],
        deerflow_protocol="v2",
        workspace=tmp_path,
        acp_timeout_seconds=10,
        state_path=tmp_path / "state.sqlite3",
    ).validated()
    app = AdapterApp(config)
    await app.start()
    try:
        async with asyncio.timeout(15):
            while not sent.exists() or not sent.read_text(encoding="utf-8").strip():
                await asyncio.sleep(0.05)
        payload = json.loads(sent.read_text(encoding="utf-8").splitlines()[0])
        assert payload["channel_id"] == channel_id
        assert payload["reply_to"] == event_id
        assert "hello from Buzz" in payload["content"]
        assert "Fake DeerFlow received" in payload["content"]

        async with asyncio.timeout(5):
            while True:
                row = (
                    app.state._conn()
                    .execute("SELECT status, attempts FROM inbox_messages")
                    .fetchone()
                )
                if row is not None and row["status"] == "done":
                    break
                await asyncio.sleep(0.01)
        assert tuple(row) == ("done", 0)
        session = (
            app.state._conn()
            .execute("SELECT conversation_key, session_id FROM sessions")
            .fetchone()
        )
        assert session["conversation_key"] == f"thread:{channel_id}:{event_id}"
        assert session["session_id"].startswith("session-")
    finally:
        await app.close()
