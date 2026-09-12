from __future__ import annotations

from pathlib import Path

import pytest
from buzz_deerflow_adapter.app import AdapterApp
from buzz_deerflow_adapter.buzz_cli import BuzzCLIError, BuzzDeliveryUnknownError
from buzz_deerflow_adapter.config import AdapterConfig
from buzz_deerflow_adapter.models import BuzzChannel, BuzzMessage


class _FakeACP:
    def __init__(self) -> None:
        self.prompt_calls = 0

    async def attach_or_create(self, _existing: str | None) -> str:
        return "session-1"

    async def prompt(self, _session_id: str, _prompt: str) -> str:
        self.prompt_calls += 1
        return "saved reply"

    async def close(self) -> None:
        return None

    async def open(self) -> None:
        return None


class _FakeBuzz:
    def __init__(self, *, failures: int = 0, delivery_unknown: bool = False) -> None:
        self.failures = failures
        self.delivery_unknown = delivery_unknown
        self.send_calls = 0

    async def send_message(self, _channel: str, _reply_to: str, content: str):
        assert content == "saved reply"
        self.send_calls += 1
        if self.delivery_unknown:
            raise BuzzDeliveryUnknownError("maybe delivered")
        if self.send_calls <= self.failures:
            raise RuntimeError("known failed send")
        return {"accepted": True}


class _PagedBuzz:
    def __init__(self) -> None:
        self.messages = [
            BuzzMessage(
                channel_id="11111111-1111-1111-1111-111111111111",
                event_id=f"{index:064x}",
                created_at=index,
                author_pubkey="b" * 64,
                kind=9,
                content=f"message {index}",
            )
            for index in range(1, 401)
        ]

    async def get_messages(
        self, _channel, *, since=None, before=None, limit=200, kinds=None
    ):
        del kinds
        lower = 0 if since is None else since
        upper = 2**63 - 1 if before is None else before
        matches = [
            message for message in self.messages if lower <= message.created_at <= upper
        ]
        return matches[-limit:]


class _LifecycleBuzz:
    def __init__(self) -> None:
        self.operations: list[tuple[str, str]] = []

    async def set_profile(self, **kwargs):
        self.operations.append(("profile", kwargs["name"]))
        return {"accepted": True}

    async def update_channel_name(self, channel_id: str, name: str):
        self.operations.append((channel_id, name))
        return {"accepted": True}

    async def list_channels(self):
        return []

    async def set_presence(self, status: str):
        self.operations.append(("presence", status))
        return {"accepted": True}

    async def set_status(self, text: str = "", *, emoji: str = "", clear=False):
        del emoji
        self.operations.append(("status", "clear" if clear else text))
        return {"accepted": True}


def _app(tmp_path: Path, *, max_attempts: int = 5) -> AdapterApp:
    config = AdapterConfig(
        relay_url="wss://relay.example",
        agent_pubkey="a" * 64,
        auto_discover_channels=False,
        channels=["11111111-1111-1111-1111-111111111111"],
        include_dms=False,
        deerflow_command="fake-deerflow",
        workspace=tmp_path,
        state_path=tmp_path / "state.sqlite3",
        max_message_attempts=max_attempts,
        transport_retry_base_seconds=0,
        transport_retry_max_seconds=0,
    )
    return AdapterApp(config)


def _enqueue(app: AdapterApp) -> None:
    app.state.enqueue(
        [
            BuzzMessage(
                channel_id="11111111-1111-1111-1111-111111111111",
                event_id="c" * 64,
                created_at=100,
                author_pubkey="b" * 64,
                kind=9,
                content="hello",
                tags=(("p", "a" * 64),),
            )
        ],
        session_scope="thread",
    )


async def test_delivery_retry_reuses_persisted_response(tmp_path: Path) -> None:
    app = _app(tmp_path)
    acp = _FakeACP()
    buzz = _FakeBuzz(failures=1)
    app.acp = acp  # type: ignore[assignment]
    app.buzz = buzz  # type: ignore[assignment]
    _enqueue(app)
    try:
        await app._process_pending()
        await app._process_pending()
        assert acp.prompt_calls == 1
        assert buzz.send_calls == 2
        row = (
            app.state._conn()
            .execute("SELECT status, attempts, response_content FROM inbox_messages")
            .fetchone()
        )
        assert tuple(row) == ("done", 1, None)
    finally:
        app.state.close()


async def test_delivery_unknown_is_quarantined(tmp_path: Path) -> None:
    app = _app(tmp_path)
    app.acp = _FakeACP()  # type: ignore[assignment]
    app.buzz = _FakeBuzz(delivery_unknown=True)  # type: ignore[assignment]
    _enqueue(app)
    try:
        await app._process_pending()
        row = (
            app.state._conn()
            .execute("SELECT status, attempts FROM inbox_messages")
            .fetchone()
        )
        assert tuple(row) == ("delivery_unknown", 1)
    finally:
        app.state.close()


async def test_invalid_attachment_sends_persisted_error_without_model(
    tmp_path: Path,
) -> None:
    app = _app(tmp_path)
    acp = _FakeACP()
    app.acp = acp
    replies = []

    class ReplyBuzz:
        async def send_message(self, channel, event_id, content):
            replies.append(content)
            return {"accepted": True}

    app.buzz = ReplyBuzz()
    _enqueue(app)
    app.state._conn().execute(
        "UPDATE inbox_messages SET tags_json = ?",
        ('[["imeta", "url file:///private.txt"]]',),
    )
    app.state._conn().commit()
    try:
        await app._process_pending()
        assert acp.prompt_calls == 0
        assert "could not process the attachments" in replies[0]
        assert not app.state.pending()
    finally:
        app.state.close()


def test_eligibility_filters_self_echo_mentions_and_allowlist(tmp_path: Path) -> None:
    app = _app(tmp_path)
    base = {
        "channel_id": "11111111-1111-1111-1111-111111111111",
        "event_id": "c" * 64,
        "created_at": 100,
        "kind": 9,
        "content": "hello",
    }
    try:
        assert not app._eligible(
            BuzzMessage(author_pubkey="a" * 64, tags=(("p", "a" * 64),), **base)
        )
        assert not app._eligible(BuzzMessage(author_pubkey="b" * 64, **base))
        assert app._eligible(
            BuzzMessage(author_pubkey="b" * 64, tags=(("p", "a" * 64),), **base)
        )
        assert app._eligible(BuzzMessage(author_pubkey="b" * 64, is_dm=True, **base))
    finally:
        app.state.close()


async def test_offline_backlog_is_read_across_cli_pages(tmp_path: Path) -> None:
    app = _app(tmp_path)
    app.buzz = _PagedBuzz()  # type: ignore[assignment]
    try:
        messages = await app._read_channel_messages(
            BuzzChannel("11111111-1111-1111-1111-111111111111"), since=0
        )
        assert len(messages) == 400
        assert messages[0].created_at == 1
        assert messages[-1].created_at == 400
    finally:
        app.state.close()


async def test_page_cap_refuses_to_advance_past_unread_backlog(
    tmp_path: Path,
) -> None:
    app = _app(tmp_path)
    app.buzz = _PagedBuzz()  # type: ignore[assignment]
    object.__setattr__(app.config, "max_poll_pages", 1)
    try:
        with pytest.raises(BuzzCLIError, match="cursor was not advanced"):
            await app._read_channel_messages(
                BuzzChannel("11111111-1111-1111-1111-111111111111"), since=0
            )
        assert app.state.get_cursor("11111111-1111-1111-1111-111111111111") is None
    finally:
        app.state.close()


async def test_lifecycle_sets_profile_channel_presence_and_status(
    tmp_path: Path,
) -> None:
    app = _app(tmp_path)
    buzz = _LifecycleBuzz()
    app.buzz = buzz  # type: ignore[assignment]
    object.__setattr__(app.config, "profile_name", "DeerFlow")
    object.__setattr__(
        app.config,
        "channel_names",
        {"11111111-1111-1111-1111-111111111111": "DeerFlow 工作区"},
    )
    try:
        await app._announce_started()
        await app._announce_stopped()
        assert buzz.operations == [
            ("profile", "DeerFlow"),
            ("11111111-1111-1111-1111-111111111111", "DeerFlow 工作区"),
            ("presence", "online"),
            ("status", "DeerFlow ready"),
            ("status", "clear"),
            ("presence", "offline"),
        ]
    finally:
        app.state.close()


async def test_lifecycle_skips_channel_rename_when_name_already_matches(
    tmp_path: Path,
) -> None:
    app = _app(tmp_path)
    buzz = _LifecycleBuzz()
    channel_id = "11111111-1111-1111-1111-111111111111"

    async def list_channels():
        return [BuzzChannel(channel_id=channel_id, name="DeerFlow 工作区")]

    buzz.list_channels = list_channels  # type: ignore[method-assign]
    app.buzz = buzz  # type: ignore[assignment]
    object.__setattr__(app.config, "profile_name", "")
    object.__setattr__(app.config, "presence_enabled", False)
    object.__setattr__(app.config, "status_enabled", False)
    object.__setattr__(
        app.config,
        "channel_names",
        {channel_id: "DeerFlow 工作区"},
    )
    try:
        await app._announce_started()
        assert buzz.operations == []
    finally:
        app.state.close()
