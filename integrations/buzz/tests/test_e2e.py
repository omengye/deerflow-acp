from __future__ import annotations

import asyncio
import base64
import hashlib
import json
import sys
from pathlib import Path

import pytest
from buzz_deerflow_adapter.app import AdapterApp
from buzz_deerflow_adapter.config import AdapterConfig
from nostr_sdk import Keys


@pytest.mark.parametrize(
    "with_attachments, attachment_only", [(False, False), (True, False), (True, True)]
)
async def test_buzz_event_to_acp_prompt_to_threaded_reply(
    tmp_path: Path, monkeypatch, with_attachments: bool, attachment_only: bool
) -> None:
    fixtures = Path(__file__).parent / "fixtures"
    channel_id = "11111111-1111-1111-1111-111111111111"
    event_id = "c" * 64
    agent_keys = Keys.generate()
    agent_pubkey = agent_keys.public_key().to_hex()
    channels = tmp_path / "channels.json"
    inbox = tmp_path / "inbox.json"
    sent = tmp_path / "sent.jsonl"
    captured = tmp_path / "prompts.jsonl"
    tags = [["h", channel_id], ["p", agent_pubkey]]
    if with_attachments:
        blobs = {}
        for data, mime, name in [
            (b"\x89PNG\r\n\x1a\n" + b"x" * 100000, "image/png", "截图.png"),
            (b"%PDF-1.7\nreport", "application/pdf", "报告.pdf"),
        ]:
            digest = hashlib.sha256(data).hexdigest()
            url = f"https://buzz.sprwhisp.cc/media/{digest}"
            tags.append(
                [
                    "imeta",
                    f"url {url}",
                    f"m {mime}",
                    f"filename {name}",
                    f"x {digest}",
                    f"size {len(data)}",
                ]
            )
            blobs[url] = base64.b64encode(data).decode()
        media = tmp_path / "media.json"
        media.write_text(json.dumps(blobs))
        monkeypatch.setenv("FAKE_BUZZ_MEDIA", str(media))
    monkeypatch.setenv("FAKE_ACP_PROMPTS", str(captured))
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
                    "content": "" if attachment_only else "hello from Buzz",
                    "tags": tags,
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
        if not attachment_only:
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
        blocks = json.loads(captured.read_text(encoding="utf-8"))["prompt"]
        if with_attachments:
            assert [block["type"] for block in blocks] == [
                "text",
                "image",
                "resource_link",
            ]
            assert blocks[2]["name"] == "报告.pdf"
            assert len(base64.b64decode(blocks[1]["data"])) > 65536
        else:
            assert len(blocks) == 1
    finally:
        await app.close()
