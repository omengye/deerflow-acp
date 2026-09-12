"""Small data models shared by the Buzz adapter components."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

_HEX_CHARS = frozenset("0123456789abcdefABCDEF")


def _is_hex_id(value: str) -> bool:
    return len(value) == 64 and all(char in _HEX_CHARS for char in value)


def _normalized_tags(value: Any) -> tuple[tuple[str, ...], ...]:
    if not isinstance(value, list):
        return ()
    tags: list[tuple[str, ...]] = []
    for item in value:
        if isinstance(item, list) and all(isinstance(part, str) for part in item):
            tags.append(tuple(item))
    return tuple(tags)


@dataclass(frozen=True, slots=True)
class BuzzChannel:
    channel_id: str
    name: str = ""
    is_dm: bool = False


@dataclass(frozen=True, slots=True)
class BuzzMessage:
    channel_id: str
    event_id: str
    created_at: int
    author_pubkey: str
    kind: int
    content: str
    tags: tuple[tuple[str, ...], ...] = ()
    is_dm: bool = False

    @classmethod
    def from_event(
        cls, channel: BuzzChannel, value: dict[str, Any]
    ) -> BuzzMessage | None:
        event_id = value.get("id")
        author = value.get("pubkey")
        content = value.get("content")
        created_at = value.get("created_at")
        kind = value.get("kind")
        if (
            not isinstance(event_id, str)
            or not _is_hex_id(event_id)
            or not isinstance(author, str)
            or not _is_hex_id(author)
            or not isinstance(content, str)
            or not isinstance(created_at, int)
            or isinstance(created_at, bool)
            or created_at < 0
            or not isinstance(kind, int)
            or isinstance(kind, bool)
        ):
            return None
        return cls(
            channel_id=channel.channel_id,
            event_id=event_id.lower(),
            created_at=created_at,
            author_pubkey=author.lower(),
            kind=kind,
            content=content,
            tags=_normalized_tags(value.get("tags")),
            is_dm=channel.is_dm,
        )

    @property
    def key(self) -> str:
        return self.event_id

    @property
    def reply_to(self) -> str:
        return self.event_id

    def mentions(self, pubkey: str) -> bool:
        expected = pubkey.casefold()
        return any(
            len(tag) >= 2 and tag[0] == "p" and tag[1].casefold() == expected
            for tag in self.tags
        )

    @property
    def thread_root(self) -> str | None:
        """Return a canonical NIP-10 root for an actual threaded reply."""

        root: str | None = None
        has_reply = False
        unmarked: list[str] = []
        for tag in self.tags:
            if len(tag) < 2 or tag[0] != "e" or not _is_hex_id(tag[1]):
                continue
            event_id = tag[1].lower()
            marker = tag[3] if len(tag) >= 4 else ""
            if marker == "root":
                root = event_id
            elif marker == "reply":
                has_reply = True
            elif not marker:
                unmarked.append(event_id)
        if has_reply:
            return root or (unmarked[0] if unmarked else None)
        if len(unmarked) >= 2:
            return unmarked[0]
        return None

    def conversation_key(self, session_scope: str) -> str:
        if self.is_dm or session_scope == "channel":
            return f"channel:{self.channel_id}"
        return f"thread:{self.channel_id}:{self.thread_root or self.event_id}"


@dataclass(frozen=True, slots=True)
class PendingMessage:
    key: str
    conversation_key: str
    channel_id: str
    event_id: str
    created_at: int
    author_pubkey: str
    kind: int
    content: str
    is_dm: bool
    attempts: int
    response_content: str | None
    tags: tuple[tuple[str, ...], ...] = ()
