from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

from buzz_deerflow_adapter.buzz_cli import (
    BuzzCLI,
    BuzzDeliveryUnknownError,
    BuzzTransportError,
)
from buzz_deerflow_adapter.models import BuzzChannel

EVENT = "c" * 64
AUTHOR = "b" * 64


def _cli(monkeypatch: pytest.MonkeyPatch) -> BuzzCLI:
    fixture = Path(__file__).parent / "fixtures" / "fake_buzz_cli.py"
    monkeypatch.setenv("BUZZ_PRIVATE_KEY", "test-only")
    return BuzzCLI(sys.executable, [str(fixture)], "wss://relay.example")


async def test_lists_channels_and_dms(tmp_path: Path, monkeypatch) -> None:
    channels = tmp_path / "channels.json"
    dms = tmp_path / "dms.json"
    channels.write_text(
        json.dumps([{"channel_id": "channel-1", "name": "General"}]),
        encoding="utf-8",
    )
    dms.write_text(json.dumps([{"dm_id": "dm-1"}]), encoding="utf-8")
    monkeypatch.setenv("FAKE_BUZZ_CHANNELS", str(channels))
    monkeypatch.setenv("FAKE_BUZZ_DMS", str(dms))
    cli = _cli(monkeypatch)

    assert (await cli.list_channels())[0].name == "General"
    assert (await cli.list_dms())[0].is_dm is True


async def test_get_messages_parses_full_nostr_events(
    tmp_path: Path, monkeypatch
) -> None:
    inbox = tmp_path / "inbox.json"
    inbox.write_text(
        json.dumps(
            [
                {
                    "channel_id": "channel-1",
                    "id": EVENT,
                    "pubkey": AUTHOR,
                    "created_at": 101,
                    "kind": 9,
                    "content": "hello",
                    "tags": [["p", "a" * 64]],
                }
            ]
        ),
        encoding="utf-8",
    )
    monkeypatch.setenv("FAKE_BUZZ_INBOX", str(inbox))
    messages = await _cli(monkeypatch).get_messages(BuzzChannel("channel-1"), since=100)
    assert len(messages) == 1
    assert messages[0].event_id == EVENT
    assert messages[0].content == "hello"


async def test_send_uses_stdin_and_reply_anchor(tmp_path: Path, monkeypatch) -> None:
    sent = tmp_path / "sent.jsonl"
    monkeypatch.setenv("FAKE_BUZZ_SENT", str(sent))
    await _cli(monkeypatch).send_message("channel-1", EVENT, "`safe` $body")

    payload = json.loads(sent.read_text(encoding="utf-8"))
    assert payload["channel_id"] == "channel-1"
    assert payload["reply_to"] == EVENT
    assert payload["content"] == "`safe` $body"
    assert payload["args"][payload["args"].index("--content") + 1] == "-"


async def test_profile_presence_status_and_channel_name_use_supported_commands(
    tmp_path: Path, monkeypatch
) -> None:
    operations = tmp_path / "operations.jsonl"
    monkeypatch.setenv("FAKE_BUZZ_OPERATIONS", str(operations))
    cli = _cli(monkeypatch)

    await cli.set_profile(name="DeerFlow", about="Portable agent")
    await cli.set_presence("online")
    await cli.set_status("working", emoji="🔧")
    await cli.update_channel_name("channel-1", "DeerFlow 工作区")
    await cli.set_status(clear=True)
    await cli.edit_message(EVENT, "updated progress")

    calls = [json.loads(line) for line in operations.read_text().splitlines()]
    assert calls[0][:2] == ["users", "set-profile"]
    assert calls[1] == ["users", "set-presence", "--status", "online"]
    assert calls[2][:2] == ["users", "set-status"]
    assert calls[3] == [
        "channels",
        "update",
        "--channel",
        "channel-1",
        "--name",
        "DeerFlow 工作区",
    ]
    assert calls[4] == ["users", "set-status", "--clear"]
    assert calls[5] == [
        "messages",
        "edit",
        "--event",
        EVENT,
        "--content",
        "updated progress",
    ]


async def test_cli_classifies_retryable_and_unknown_delivery(monkeypatch) -> None:
    monkeypatch.setenv("FAKE_BUZZ_GET_MODE", "transport_error")
    with pytest.raises(BuzzTransportError):
        await _cli(monkeypatch).get_messages(BuzzChannel("channel-1"))

    monkeypatch.delenv("FAKE_BUZZ_GET_MODE")
    monkeypatch.setenv("FAKE_BUZZ_SEND_MODE", "delivery_unknown")
    with pytest.raises(BuzzDeliveryUnknownError):
        await _cli(monkeypatch).send_message("channel-1", EVENT, "reply")


async def test_timeout_is_ambiguous_only_for_send(monkeypatch) -> None:
    monkeypatch.setenv("FAKE_BUZZ_GET_MODE", "hang")
    cli = _cli(monkeypatch)
    cli.timeout_seconds = 0.05
    with pytest.raises(BuzzTransportError, match="timed out"):
        await cli.get_messages(BuzzChannel("channel-1"))

    monkeypatch.delenv("FAKE_BUZZ_GET_MODE")
    monkeypatch.setenv("FAKE_BUZZ_SEND_MODE", "hang")
    with pytest.raises(BuzzDeliveryUnknownError, match="timed out"):
        await cli.send_message("channel-1", EVENT, "reply")
