from __future__ import annotations

import json

import pytest
from nostr_sdk import Keys, nip44_decrypt

from buzz_deerflow_adapter.observer import (
    NipAOObserver,
    ObserverConfigError,
    ObserverFrame,
    parse_auth_tag,
    resolve_owner_pubkey,
)


def _observer() -> tuple[NipAOObserver, Keys, Keys]:
    agent = Keys.generate()
    owner = Keys.generate()
    observer = NipAOObserver(
        relay_url="wss://relay.example",
        agent_pubkey=agent.public_key().to_hex(),
        owner_pubkey=owner.public_key().to_hex(),
        private_key=agent.secret_key().to_bech32(),
        auth_tag=None,
    )
    return observer, agent, owner


def test_observer_builds_encrypted_nip_ao_event() -> None:
    observer, agent, owner = _observer()
    frame = ObserverFrame(
        seq=1,
        kind="acp_read",
        channel_id="11111111-1111-1111-1111-111111111111",
        session_id="session-1",
        turn_id="c" * 64,
        payload={"jsonrpc": "2.0", "method": "session/update"},
    )

    event = observer._build_event(frame)
    plaintext = nip44_decrypt(owner.secret_key(), agent.public_key(), event["content"])
    payload = json.loads(plaintext)

    assert event["kind"] == 24200
    assert ["p", owner.public_key().to_hex()] in event["tags"]
    assert ["agent", agent.public_key().to_hex()] in event["tags"]
    assert ["frame", "telemetry"] in event["tags"]
    assert payload["kind"] == "acp_read"
    assert payload["channelId"] == frame.channel_id


def test_observer_builds_presence_for_persistent_connection() -> None:
    observer, agent, _ = _observer()
    event = observer._build_presence_event("online")

    assert event["kind"] == 20001
    assert event["pubkey"] == agent.public_key().to_hex()
    assert event["content"] == "online"
    assert ["status", "online"] in event["tags"]

    with pytest.raises(ValueError, match="presence status"):
        observer._build_presence_event("busy")


def test_observer_strips_raw_tool_input_and_result_by_default() -> None:
    observer, _, _ = _observer()
    observer.acp_update(
        "channel",
        "session",
        "turn",
        {
            "sessionUpdate": "tool_call",
            "title": "shell",
            "rawInput": {"command": "secret"},
            "content": [{"type": "text", "text": "secret result"}],
        },
    )
    frame = observer._queue.get_nowait()
    projected = frame.payload["params"]["update"]
    assert projected["title"] == "shell"
    assert "rawInput" not in projected
    assert "content" not in projected


def test_auth_tag_resolves_owner_and_rejects_mismatch() -> None:
    owner = "b" * 64
    tag = parse_auth_tag(json.dumps(["auth", owner, "x", "signature"]))
    assert tag is not None
    assert resolve_owner_pubkey("", tag) == owner
    with pytest.raises(ObserverConfigError, match="does not match"):
        resolve_owner_pubkey("c" * 64, tag)
