from __future__ import annotations

from pathlib import Path

from buzz_deerflow_adapter.models import BuzzMessage
from buzz_deerflow_adapter.state import AdapterState


def _message(event_id: str = "c" * 64) -> BuzzMessage:
    return BuzzMessage(
        channel_id="11111111-1111-1111-1111-111111111111",
        event_id=event_id,
        created_at=100,
        author_pubkey="b" * 64,
        kind=9,
        content="hello",
        tags=(("p", "a" * 64),),
    )


def test_enqueue_deduplicates_event_ids(tmp_path: Path) -> None:
    state = AdapterState(tmp_path / "state.sqlite3")
    try:
        assert state.enqueue([_message()], session_scope="thread") == 1
        assert state.enqueue([_message()], session_scope="thread") == 0
        assert len(state.pending()) == 1
    finally:
        state.close()


def test_generated_reply_survives_reopen(tmp_path: Path) -> None:
    path = tmp_path / "state.sqlite3"
    state = AdapterState(path)
    state.enqueue([_message()], session_scope="thread")
    state.save_response("c" * 64, "durable reply")
    state.close()

    reopened = AdapterState(path)
    try:
        assert reopened.pending()[0].response_content == "durable reply"
    finally:
        reopened.close()


def test_cursor_and_session_are_persistent(tmp_path: Path) -> None:
    path = tmp_path / "state.sqlite3"
    state = AdapterState(path)
    state.put_cursor("channel-1", 100)
    state.put_cursor("channel-1", 90)
    state.put_session("channel:channel-1", "session-1", tmp_path)
    state.close()

    reopened = AdapterState(path)
    try:
        assert reopened.get_cursor("channel-1") == 100
        assert reopened.get_session("channel:channel-1", tmp_path) == "session-1"
    finally:
        reopened.close()
