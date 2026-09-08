from __future__ import annotations

from buzz_deerflow_adapter.models import BuzzChannel, BuzzMessage

AGENT = "a" * 64
AUTHOR = "b" * 64
EVENT = "c" * 64
ROOT = "d" * 64
PARENT = "e" * 64


def _message(*, tags=(), is_dm: bool = False) -> BuzzMessage:
    return BuzzMessage(
        channel_id="11111111-1111-1111-1111-111111111111",
        event_id=EVENT,
        created_at=100,
        author_pubkey=AUTHOR,
        kind=9,
        content="hello",
        tags=tags,
        is_dm=is_dm,
    )


def test_event_parser_normalizes_ids_and_tags() -> None:
    channel = BuzzChannel("11111111-1111-1111-1111-111111111111")
    message = BuzzMessage.from_event(
        channel,
        {
            "id": EVENT.upper(),
            "pubkey": AUTHOR.upper(),
            "created_at": 100,
            "kind": 9,
            "content": "hello",
            "tags": [["p", AGENT], ["invalid", 1]],
        },
    )
    assert message is not None
    assert message.event_id == EVENT
    assert message.author_pubkey == AUTHOR
    assert message.tags == (("p", AGENT),)


def test_mentions_uses_signed_p_tag_not_visible_text() -> None:
    assert _message(tags=(("p", AGENT),)).mentions(AGENT)
    assert not _message().mentions(AGENT)


def test_nested_reply_uses_nip10_root_for_thread_session() -> None:
    message = _message(
        tags=(
            ("e", ROOT, "", "root"),
            ("e", PARENT, "", "reply"),
        )
    )
    assert message.thread_root == ROOT
    assert message.conversation_key("thread").endswith(ROOT)


def test_top_level_message_opens_its_own_thread_session() -> None:
    message = _message(tags=(("e", ROOT, "", "root"),))
    assert message.thread_root is None
    assert message.conversation_key("thread").endswith(EVENT)


def test_dm_and_channel_scope_share_one_session_per_channel() -> None:
    assert _message(is_dm=True).conversation_key("thread").startswith("channel:")
    assert _message().conversation_key("channel").startswith("channel:")
