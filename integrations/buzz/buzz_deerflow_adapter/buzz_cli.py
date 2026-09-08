"""Async wrapper for Buzz's agent-first JSON CLI."""

from __future__ import annotations

import asyncio
import json
import os
from collections.abc import Mapping
from typing import Any

from .models import BuzzChannel, BuzzMessage


class BuzzCLIError(RuntimeError):
    """The Buzz CLI rejected or could not complete an operation."""


class BuzzTransportError(BuzzCLIError):
    """A read or provably pre-delivery write can be retried safely."""


class BuzzDeliveryUnknownError(BuzzCLIError):
    """The relay may have stored a write, so resending could duplicate it."""


def _error_payload(stderr: str, stdout: str) -> dict[str, Any] | None:
    for source in (stderr, stdout):
        for line in reversed(source.splitlines()):
            try:
                value = json.loads(line)
            except json.JSONDecodeError:
                continue
            if isinstance(value, dict) and isinstance(value.get("error"), str):
                return value
    return None


def _json_array(output: str, operation: str) -> list[dict[str, Any]]:
    try:
        payload = json.loads(output)
    except json.JSONDecodeError as exc:
        raise BuzzCLIError(f"Buzz {operation} returned invalid JSON") from exc
    if not isinstance(payload, list):
        raise BuzzCLIError(f"Buzz {operation} returned a non-array JSON value")
    return [item for item in payload if isinstance(item, dict)]


def _json_object(output: str, operation: str) -> dict[str, Any]:
    try:
        payload = json.loads(output)
    except json.JSONDecodeError as exc:
        raise BuzzCLIError(f"Buzz {operation} returned invalid JSON") from exc
    if not isinstance(payload, dict):
        raise BuzzCLIError(f"Buzz {operation} returned a non-object JSON value")
    return payload


class BuzzCLI:
    def __init__(
        self,
        command: str,
        command_args: list[str],
        relay_url: str,
        *,
        timeout_seconds: float = 60,
        env: Mapping[str, str] | None = None,
    ) -> None:
        self.command = command
        self.command_args = list(command_args)
        self.relay_url = relay_url
        inherited = dict(os.environ if env is None else env)
        inherited["BUZZ_RELAY_URL"] = relay_url
        self.env = inherited
        self.timeout_seconds = timeout_seconds

    def _base(self) -> list[str]:
        return [self.command, *self.command_args]

    async def _run(
        self,
        *args: str,
        stdin: str | None = None,
        delivery_may_be_ambiguous: bool = False,
    ) -> tuple[str, str]:
        process = await asyncio.create_subprocess_exec(
            *self._base(),
            *args,
            stdin=asyncio.subprocess.PIPE if stdin is not None else None,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            env=self.env,
        )
        try:
            async with asyncio.timeout(self.timeout_seconds):
                stdout, stderr = await process.communicate(
                    stdin.encode("utf-8") if stdin is not None else None
                )
        except TimeoutError as exc:
            if process.returncode is None:
                process.kill()
                await process.wait()
            error_type = (
                BuzzDeliveryUnknownError
                if delivery_may_be_ambiguous
                else BuzzTransportError
            )
            raise error_type(
                f"Buzz CLI timed out after {self.timeout_seconds:g} seconds"
            ) from exc
        except BaseException:
            if process.returncode is None:
                process.kill()
                await process.wait()
            raise
        out = stdout.decode("utf-8", errors="replace")
        err = stderr.decode("utf-8", errors="replace")
        if process.returncode != 0:
            payload = _error_payload(err, out)
            detail = (
                str(payload.get("message", ""))
                if payload is not None
                else "\n".join(part for part in (err.strip(), out.strip()) if part)
            )
            detail = detail or f"exit code {process.returncode}"
            category = payload.get("error") if payload is not None else None
            if category == "delivery_unknown":
                raise BuzzDeliveryUnknownError(detail)
            if payload is not None and payload.get("retryable") is True:
                raise BuzzTransportError(detail)
            raise BuzzCLIError(detail)
        return out, err

    async def list_channels(self) -> list[BuzzChannel]:
        stdout, _ = await self._run("channels", "list", "--member", "--limit", "500")
        channels: list[BuzzChannel] = []
        for item in _json_array(stdout, "channels list"):
            channel_id = item.get("channel_id")
            if not isinstance(channel_id, str) or not channel_id:
                continue
            name = item.get("name")
            channels.append(
                BuzzChannel(
                    channel_id=channel_id,
                    name=name if isinstance(name, str) else "",
                    is_dm=False,
                )
            )
        return channels

    async def set_profile(
        self,
        *,
        name: str = "",
        avatar: str = "",
        about: str = "",
        nip05: str = "",
    ) -> dict[str, Any]:
        args = ["users", "set-profile"]
        for flag, value in (
            ("--name", name),
            ("--avatar", avatar),
            ("--about", about),
            ("--nip05", nip05),
        ):
            if value:
                args.extend((flag, value))
        if len(args) == 2:
            return {}
        stdout, _ = await self._run(*args)
        return _json_object(stdout, "users set-profile")

    async def set_presence(self, status: str) -> dict[str, Any]:
        stdout, _ = await self._run("users", "set-presence", "--status", status)
        return _json_object(stdout, "users set-presence")

    async def set_status(
        self, text: str = "", *, emoji: str = "", clear: bool = False
    ) -> dict[str, Any]:
        args = ["users", "set-status"]
        if clear:
            args.append("--clear")
        else:
            args.extend(("--text", text))
            if emoji:
                args.extend(("--emoji", emoji))
        stdout, _ = await self._run(*args)
        return _json_object(stdout, "users set-status")

    async def update_channel_name(self, channel_id: str, name: str) -> dict[str, Any]:
        stdout, _ = await self._run(
            "channels", "update", "--channel", channel_id, "--name", name
        )
        return _json_object(stdout, "channels update")

    async def list_dms(self) -> list[BuzzChannel]:
        stdout, _ = await self._run("dms", "list", "--limit", "200")
        channels: list[BuzzChannel] = []
        for item in _json_array(stdout, "dms list"):
            channel_id = item.get("dm_id")
            if isinstance(channel_id, str) and channel_id:
                channels.append(
                    BuzzChannel(channel_id=channel_id, name="DM", is_dm=True)
                )
        return channels

    async def get_messages(
        self,
        channel: BuzzChannel,
        *,
        since: int | None = None,
        before: int | None = None,
        limit: int = 200,
        kinds: list[int] | None = None,
    ) -> list[BuzzMessage]:
        args = [
            "messages",
            "get",
            "--channel",
            channel.channel_id,
            "--limit",
            str(max(1, min(limit, 200))),
        ]
        if since is not None:
            args.extend(("--since", str(max(0, since))))
        if before is not None:
            args.extend(("--before", str(max(0, before))))
        if kinds:
            args.extend(("--kinds", ",".join(str(kind) for kind in kinds)))
        stdout, _ = await self._run(*args)
        messages = [
            message
            for item in _json_array(stdout, "messages get")
            if (message := BuzzMessage.from_event(channel, item)) is not None
        ]
        messages.sort(key=lambda message: (message.created_at, message.event_id))
        return messages

    async def send_message(
        self, channel_id: str, reply_to: str, content: str
    ) -> dict[str, Any]:
        stdout, _ = await self._run(
            "messages",
            "send",
            "--channel",
            channel_id,
            "--content",
            "-",
            "--reply-to",
            reply_to,
            stdin=content,
            delivery_may_be_ambiguous=True,
        )
        payload = _json_object(stdout, "messages send")
        if payload.get("accepted") is False:
            raise BuzzCLIError(str(payload.get("message") or "Buzz rejected the reply"))
        return payload

    async def edit_message(self, event_id: str, content: str) -> dict[str, Any]:
        stdout, _ = await self._run(
            "messages",
            "edit",
            "--event",
            event_id,
            "--content",
            content,
        )
        payload = _json_object(stdout, "messages edit")
        if payload.get("accepted") is False:
            raise BuzzCLIError(str(payload.get("message") or "Buzz rejected the edit"))
        return payload
