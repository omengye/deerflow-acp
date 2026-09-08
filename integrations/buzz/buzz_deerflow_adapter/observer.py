"""Best-effort Buzz NIP-AO telemetry publisher for ACP v2 turns."""

from __future__ import annotations

import asyncio
import json
import logging
import os
import ssl
from contextlib import suppress
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any

import certifi
from nostr_sdk import (
    EventBuilder,
    Keys,
    Kind,
    Nip44Version,
    PublicKey,
    Tag,
    nip44_encrypt,
)
from websockets.asyncio.client import connect

logger = logging.getLogger(__name__)

KIND_NIP42_AUTH = 22242
KIND_PRESENCE_UPDATE = 20001
KIND_AGENT_OBSERVER = 24200
_HEX_CHARS = frozenset("0123456789abcdefABCDEF")
_OBSERVED_UPDATE_KINDS = {
    "agent_thought_chunk",
    "plan",
    "state_update",
    "tool_call",
    "tool_call_update",
}
_SENSITIVE_TOOL_FIELDS = {
    "content",
    "rawInput",
    "rawOutput",
    "raw_input",
    "raw_output",
}


class ObserverConfigError(ValueError):
    """The observer cannot establish an authenticated agent-owner channel."""


def _is_hex_pubkey(value: str) -> bool:
    return len(value) == 64 and all(char in _HEX_CHARS for char in value)


def parse_auth_tag(raw: str) -> list[str] | None:
    if not raw.strip():
        return None
    try:
        value = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise ObserverConfigError("BUZZ_AUTH_TAG is not valid JSON") from exc
    if (
        not isinstance(value, list)
        or len(value) < 2
        or not all(isinstance(item, str) for item in value)
        or value[0] != "auth"
        or not _is_hex_pubkey(value[1])
    ):
        raise ObserverConfigError(
            "BUZZ_AUTH_TAG must be an auth tag whose second value is the owner pubkey"
        )
    return value


def resolve_owner_pubkey(explicit: str, auth_tag: list[str] | None) -> str:
    if explicit:
        if not _is_hex_pubkey(explicit):
            raise ObserverConfigError("observer owner pubkey must be 64 hex characters")
        if auth_tag is not None and auth_tag[1].casefold() != explicit.casefold():
            raise ObserverConfigError(
                "observability.owner_pubkey does not match BUZZ_AUTH_TAG"
            )
        return explicit.casefold()
    if auth_tag is not None:
        return auth_tag[1].casefold()
    return ""


@dataclass(slots=True)
class ObserverFrame:
    seq: int
    kind: str
    channel_id: str
    session_id: str
    turn_id: str
    payload: dict[str, Any]

    def as_payload(self) -> dict[str, Any]:
        return {
            "seq": self.seq,
            "timestamp": datetime.now(UTC)
            .isoformat(timespec="milliseconds")
            .replace("+00:00", "Z"),
            "kind": self.kind,
            "agentIndex": 0,
            "channelId": self.channel_id,
            "sessionId": self.session_id,
            "turnId": self.turn_id,
            "payload": self.payload,
        }


class NipAOObserver:
    """Publishes encrypted kind-24200 observer frames without blocking ACP reads."""

    def __init__(
        self,
        *,
        relay_url: str,
        agent_pubkey: str,
        owner_pubkey: str,
        private_key: str,
        auth_tag: list[str] | None,
        queue_size: int = 256,
        publish_timeout_seconds: float = 10,
        include_raw_tool_data: bool = False,
    ) -> None:
        self.relay_url = relay_url
        self.agent_pubkey = agent_pubkey.casefold()
        self.owner_pubkey = owner_pubkey.casefold()
        self.auth_tag = auth_tag
        self.publish_timeout_seconds = publish_timeout_seconds
        self.include_raw_tool_data = include_raw_tool_data
        try:
            self._keys = Keys.parse(private_key)
        except Exception as exc:
            raise ObserverConfigError(
                "BUZZ_PRIVATE_KEY is not a valid Nostr key"
            ) from exc
        derived = self._keys.public_key().to_hex().casefold()
        if derived != self.agent_pubkey:
            raise ObserverConfigError(
                "BUZZ_PRIVATE_KEY does not match buzz.agent_pubkey"
            )
        self._owner = PublicKey.parse(owner_pubkey)
        self._queue: asyncio.Queue[ObserverFrame] = asyncio.Queue(maxsize=queue_size)
        self._seq: dict[str, int] = {}
        self._worker_task: asyncio.Task[None] | None = None
        self._ws: Any = None
        self._publish_lock = asyncio.Lock()
        self._dropped = 0

    @classmethod
    def from_environment(
        cls,
        *,
        relay_url: str,
        agent_pubkey: str,
        owner_pubkey: str = "",
        queue_size: int = 256,
        publish_timeout_seconds: float = 10,
        include_raw_tool_data: bool = False,
    ) -> NipAOObserver | None:
        auth_tag = parse_auth_tag(os.getenv("BUZZ_AUTH_TAG", ""))
        owner = resolve_owner_pubkey(owner_pubkey, auth_tag)
        if not owner:
            return None
        private_key = os.getenv("BUZZ_PRIVATE_KEY", "").strip()
        if not private_key:
            raise ObserverConfigError("BUZZ_PRIVATE_KEY is required")
        return cls(
            relay_url=relay_url,
            agent_pubkey=agent_pubkey,
            owner_pubkey=owner,
            private_key=private_key,
            auth_tag=auth_tag,
            queue_size=queue_size,
            publish_timeout_seconds=publish_timeout_seconds,
            include_raw_tool_data=include_raw_tool_data,
        )

    async def open(self) -> None:
        if self._worker_task is None:
            self._worker_task = asyncio.create_task(
                self._worker(), name="buzz-nip-ao-observer"
            )

    async def close(self) -> None:
        worker, self._worker_task = self._worker_task, None
        if worker is not None:
            try:
                async with asyncio.timeout(min(self.publish_timeout_seconds, 3)):
                    await self._queue.join()
            except TimeoutError:
                logger.warning(
                    "Discarding %d queued Buzz observer frame(s) during shutdown",
                    self._queue.qsize(),
                )
            worker.cancel()
            await asyncio.gather(worker, return_exceptions=True)
        await self._disconnect()

    def turn_started(self, channel_id: str, session_id: str, turn_id: str) -> None:
        self._enqueue(
            "turn_started",
            channel_id,
            session_id,
            turn_id,
            {"sourceEventId": turn_id},
        )

    def acp_update(
        self,
        channel_id: str,
        session_id: str,
        turn_id: str,
        update: dict[str, Any],
    ) -> None:
        if update.get("sessionUpdate") not in _OBSERVED_UPDATE_KINDS:
            return
        projected = dict(update)
        if not self.include_raw_tool_data:
            projected = self._without_raw_tool_data(projected)
        self._enqueue(
            "acp_read",
            channel_id,
            session_id,
            turn_id,
            {
                "jsonrpc": "2.0",
                "method": "session/update",
                "params": {"sessionId": session_id, "update": projected},
            },
        )

    def session_resolved(
        self,
        channel_id: str,
        session_id: str,
        turn_id: str,
        *,
        outcome: str,
        detail: str = "",
    ) -> None:
        payload: dict[str, Any] = {"outcome": outcome}
        if detail:
            payload["detail"] = detail[:1000]
        self._enqueue("session_resolved", channel_id, session_id, turn_id, payload)

    async def set_presence(self, status: str) -> None:
        """Publish presence over the observer's persistent WebSocket."""
        await self._publish_event(self._build_presence_event(status))

    def _enqueue(
        self,
        kind: str,
        channel_id: str,
        session_id: str,
        turn_id: str,
        payload: dict[str, Any],
    ) -> None:
        sequence = self._seq.get(session_id, 0) + 1
        self._seq[session_id] = sequence
        frame = ObserverFrame(sequence, kind, channel_id, session_id, turn_id, payload)
        if self._queue.full():
            try:
                self._queue.get_nowait()
                self._queue.task_done()
            except asyncio.QueueEmpty:
                pass
            self._dropped += 1
            if self._dropped == 1 or self._dropped % 100 == 0:
                logger.warning(
                    "Buzz observer queue full; dropped %d old frame(s)", self._dropped
                )
        self._queue.put_nowait(frame)

    @staticmethod
    def _without_raw_tool_data(value: Any) -> Any:
        if isinstance(value, dict):
            return {
                key: NipAOObserver._without_raw_tool_data(item)
                for key, item in value.items()
                if key not in _SENSITIVE_TOOL_FIELDS
            }
        if isinstance(value, list):
            return [NipAOObserver._without_raw_tool_data(item) for item in value]
        if isinstance(value, str) and len(value) > 4000:
            return value[:4000] + "…"
        return value

    async def _worker(self) -> None:
        failures = 0
        while True:
            frame = await self._queue.get()
            try:
                await self._publish(frame)
            except asyncio.CancelledError:
                raise
            except Exception as exc:  # noqa: BLE001 - telemetry is best effort
                failures += 1
                await self._disconnect()
                log = logger.error if failures >= 3 else logger.warning
                log("Buzz observer publish failed (%d consecutive): %s", failures, exc)
                await asyncio.sleep(min(2 ** min(failures - 1, 4), 15))
            else:
                if failures:
                    logger.info(
                        "Buzz observer publishing recovered after %d failure(s)",
                        failures,
                    )
                failures = 0
            finally:
                self._queue.task_done()

    async def _publish(self, frame: ObserverFrame) -> None:
        await self._publish_event(self._build_event(frame))

    async def _publish_event(self, event: dict[str, Any]) -> None:
        event_id = str(event["id"])
        async with self._publish_lock:
            for attempt in range(2):
                try:
                    ws = await self._connection()
                    await ws.send(json.dumps(["EVENT", event], separators=(",", ":")))
                    await self._wait_for_ok(ws, event_id)
                    return
                except Exception:
                    await self._disconnect()
                    if attempt:
                        raise

    def _build_presence_event(self, status: str) -> dict[str, Any]:
        if status not in {"online", "away", "offline"}:
            raise ValueError("presence status must be online, away, or offline")
        event = (
            EventBuilder(Kind(KIND_PRESENCE_UPDATE), status)
            .tags([Tag.parse(["status", status])])
            .sign_with_keys(self._keys)
        )
        return json.loads(event.as_json())

    def _build_event(self, frame: ObserverFrame) -> dict[str, Any]:
        plaintext = json.dumps(
            frame.as_payload(), separators=(",", ":"), ensure_ascii=False
        )
        if len(plaintext.encode("utf-8")) > 65_535:
            raise ValueError("observer frame exceeds the NIP-AO plaintext limit")
        ciphertext = nip44_encrypt(
            self._keys.secret_key(), self._owner, plaintext, Nip44Version.V2
        )
        tags = [
            Tag.parse(["p", self.owner_pubkey]),
            Tag.parse(["agent", self.agent_pubkey]),
            Tag.parse(["frame", "telemetry"]),
        ]
        if frame.channel_id:
            tags.append(Tag.parse(["h", frame.channel_id]))
        event = (
            EventBuilder(Kind(KIND_AGENT_OBSERVER), ciphertext)
            .tags(tags)
            .sign_with_keys(self._keys)
        )
        return json.loads(event.as_json())

    async def _connection(self) -> Any:
        if self._ws is not None:
            return self._ws
        ws = await connect(
            self.relay_url,
            ssl=ssl.create_default_context(cafile=certifi.where()),
            open_timeout=self.publish_timeout_seconds,
            close_timeout=2,
            ping_interval=20,
            ping_timeout=20,
            max_size=2**20,
        )
        try:
            async with asyncio.timeout(self.publish_timeout_seconds):
                while True:
                    message = self._decode(await ws.recv())
                    if message and message[0] == "AUTH" and len(message) >= 2:
                        await self._authenticate(ws, str(message[1]))
                        break
                    if message and message[0] == "NOTICE":
                        logger.debug("Buzz relay notice before auth: %s", message[1:])
        except BaseException:
            await ws.close()
            raise
        self._ws = ws
        return ws

    async def _authenticate(self, ws: Any, challenge: str) -> None:
        tags = [
            Tag.parse(["relay", self.relay_url]),
            Tag.parse(["challenge", challenge]),
        ]
        if self.auth_tag is not None:
            tags.append(Tag.parse(self.auth_tag))
        auth_event = (
            EventBuilder(Kind(KIND_NIP42_AUTH), "")
            .tags(tags)
            .sign_with_keys(self._keys)
        )
        payload = json.loads(auth_event.as_json())
        await ws.send(json.dumps(["AUTH", payload], separators=(",", ":")))
        await self._wait_for_ok(ws, str(payload["id"]))

    async def _wait_for_ok(self, ws: Any, event_id: str) -> None:
        async with asyncio.timeout(self.publish_timeout_seconds):
            while True:
                message = self._decode(await ws.recv())
                if not message:
                    continue
                if message[0] == "OK" and len(message) >= 4:
                    if str(message[1]) != event_id:
                        continue
                    if message[2] is True:
                        return
                    raise RuntimeError(str(message[3] or "Buzz relay rejected event"))
                if message[0] == "NOTICE":
                    raise RuntimeError(
                        str(message[1] if len(message) > 1 else "notice")
                    )

    @staticmethod
    def _decode(raw: Any) -> list[Any] | None:
        if isinstance(raw, bytes):
            raw = raw.decode("utf-8", errors="replace")
        try:
            value = json.loads(raw)
        except (TypeError, json.JSONDecodeError):
            return None
        return value if isinstance(value, list) else None

    async def _disconnect(self) -> None:
        ws, self._ws = self._ws, None
        if ws is not None:
            with suppress(Exception):
                await ws.close()
