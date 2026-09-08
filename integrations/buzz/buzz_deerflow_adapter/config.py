"""TOML and command-line configuration for the Buzz adapter."""

from __future__ import annotations

import argparse
import os
import tomllib
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

from nostr_sdk import Keys

_HEX_CHARS = frozenset("0123456789abcdefABCDEF")


def _table(data: dict[str, Any], name: str) -> dict[str, Any]:
    value = data.get(name, {})
    if not isinstance(value, dict):
        raise TypeError(f"[{name}] must be a TOML table")
    return value


def _strings(value: Any, *, field_name: str) -> list[str]:
    if value is None:
        return []
    if not isinstance(value, list) or not all(isinstance(item, str) for item in value):
        raise ValueError(f"{field_name} must be an array of strings")
    return list(value)


def _integers(value: Any, *, field_name: str) -> list[int]:
    if value is None:
        return []
    if not isinstance(value, list) or not all(
        isinstance(item, int) and not isinstance(item, bool) for item in value
    ):
        raise ValueError(f"{field_name} must be an array of integers")
    return list(value)


def _string_map(value: Any, *, field_name: str) -> dict[str, str]:
    if value is None:
        return {}
    if not isinstance(value, dict) or not all(
        isinstance(key, str) and isinstance(item, str) for key, item in value.items()
    ):
        raise ValueError(f"{field_name} must be a table of string values")
    return dict(value)


@dataclass(frozen=True, slots=True)
class AdapterConfig:
    buzz_command: str = "buzz"
    buzz_args: list[str] = field(default_factory=list)
    relay_url: str = ""
    agent_pubkey: str = ""
    profile_name: str = ""
    profile_avatar: str = ""
    profile_about: str = ""
    profile_nip05: str = ""
    channel_names: dict[str, str] = field(default_factory=dict)
    presence_enabled: bool = True
    presence_heartbeat_seconds: float = 60.0
    status_enabled: bool = True
    status_ready: str = "DeerFlow ready"
    status_working: str = "DeerFlow working"
    status_error: str = "DeerFlow error"
    status_emoji: str = ""
    buzz_cli_timeout_seconds: float = 60.0
    channels: list[str] = field(default_factory=list)
    auto_discover_channels: bool = True
    include_dms: bool = True
    require_mention: bool = True
    allowed_pubkeys: list[str] = field(default_factory=list)
    message_kinds: list[int] = field(default_factory=lambda: [9])
    replay_existing: bool = False
    poll_interval_seconds: float = 4.0
    max_poll_pages: int = 20
    transport_retry_attempts: int = 3
    transport_retry_base_seconds: float = 1.0
    transport_retry_max_seconds: float = 4.0
    max_message_attempts: int = 5
    session_scope: str = "thread"
    deerflow_command: str = ""
    deerflow_args: list[str] = field(default_factory=list)
    deerflow_protocol: str = "v2"
    workspace: Path = field(default_factory=Path.cwd)
    acp_timeout_seconds: float = 600.0
    observer_enabled: bool = False
    observer_owner_pubkey: str = ""
    observer_queue_size: int = 256
    observer_publish_timeout_seconds: float = 10.0
    observer_include_raw_tool_data: bool = False
    progress_messages_enabled: bool = False
    progress_update_interval_seconds: float = 0.5
    progress_max_tools: int = 12
    state_path: Path = Path("data/buzz-deerflow-adapter.sqlite3")
    log_level: str = "INFO"

    @classmethod
    def from_toml(cls, path: Path) -> AdapterConfig:
        resolved = path.expanduser().resolve()
        with resolved.open("rb") as handle:
            data = tomllib.load(handle)
        base = resolved.parent
        buzz = _table(data, "buzz")
        profile = buzz.get("profile", {})
        if not isinstance(profile, dict):
            raise TypeError("[buzz.profile] must be a TOML table")
        lifecycle = buzz.get("lifecycle", {})
        if not isinstance(lifecycle, dict):
            raise TypeError("[buzz.lifecycle] must be a TOML table")
        deerflow = _table(data, "deerflow")
        state = _table(data, "state")
        adapter = _table(data, "adapter")
        observability = _table(data, "observability")

        workspace = Path(deerflow.get("workspace", "."))
        if not workspace.is_absolute():
            workspace = base / workspace
        state_path = Path(state.get("path", "data/buzz-deerflow-adapter.sqlite3"))
        if not state_path.is_absolute():
            state_path = base / state_path

        return cls(
            buzz_command=str(buzz.get("command", "buzz")),
            buzz_args=_strings(buzz.get("args"), field_name="buzz.args"),
            relay_url=str(buzz.get("relay_url", os.getenv("BUZZ_RELAY_URL", ""))),
            agent_pubkey=str(
                buzz.get("agent_pubkey", os.getenv("BUZZ_AGENT_PUBKEY", ""))
            ),
            profile_name=str(profile.get("name", "")),
            profile_avatar=str(profile.get("avatar", "")),
            profile_about=str(profile.get("about", "")),
            profile_nip05=str(profile.get("nip05", "")),
            channel_names=_string_map(
                buzz.get("channel_names"), field_name="buzz.channel_names"
            ),
            presence_enabled=bool(lifecycle.get("presence_enabled", True)),
            presence_heartbeat_seconds=float(
                lifecycle.get("presence_heartbeat_seconds", 60)
            ),
            status_enabled=bool(lifecycle.get("status_enabled", True)),
            status_ready=str(lifecycle.get("status_ready", "DeerFlow ready")),
            status_working=str(lifecycle.get("status_working", "DeerFlow working")),
            status_error=str(lifecycle.get("status_error", "DeerFlow error")),
            status_emoji=str(lifecycle.get("status_emoji", "")),
            buzz_cli_timeout_seconds=float(buzz.get("timeout_seconds", 60)),
            channels=_strings(buzz.get("channels"), field_name="buzz.channels"),
            auto_discover_channels=bool(buzz.get("auto_discover_channels", True)),
            include_dms=bool(buzz.get("include_dms", True)),
            require_mention=bool(buzz.get("require_mention", True)),
            allowed_pubkeys=_strings(
                buzz.get("allowed_pubkeys"), field_name="buzz.allowed_pubkeys"
            ),
            message_kinds=_integers(
                buzz.get("message_kinds", [9]), field_name="buzz.message_kinds"
            ),
            replay_existing=bool(adapter.get("replay_existing", False)),
            poll_interval_seconds=float(adapter.get("poll_interval_seconds", 4)),
            max_poll_pages=int(adapter.get("max_poll_pages", 20)),
            transport_retry_attempts=int(adapter.get("transport_retry_attempts", 3)),
            transport_retry_base_seconds=float(
                adapter.get("transport_retry_base_seconds", 1)
            ),
            transport_retry_max_seconds=float(
                adapter.get("transport_retry_max_seconds", 4)
            ),
            max_message_attempts=int(adapter.get("max_message_attempts", 5)),
            session_scope=str(adapter.get("session_scope", "thread")),
            deerflow_command=str(deerflow.get("command", "")),
            deerflow_args=_strings(deerflow.get("args"), field_name="deerflow.args"),
            deerflow_protocol=str(deerflow.get("protocol", "v2")).casefold(),
            workspace=workspace.resolve(),
            acp_timeout_seconds=float(deerflow.get("timeout_seconds", 600)),
            observer_enabled=bool(observability.get("enabled", False)),
            observer_owner_pubkey=str(
                observability.get("owner_pubkey", os.getenv("BUZZ_ACP_AGENT_OWNER", ""))
            ),
            observer_queue_size=int(observability.get("queue_size", 256)),
            observer_publish_timeout_seconds=float(
                observability.get("publish_timeout_seconds", 10)
            ),
            observer_include_raw_tool_data=bool(
                observability.get("include_raw_tool_data", False)
            ),
            progress_messages_enabled=bool(
                observability.get("progress_messages", False)
            ),
            progress_update_interval_seconds=float(
                observability.get("progress_update_interval_seconds", 0.5)
            ),
            progress_max_tools=int(observability.get("progress_max_tools", 12)),
            state_path=state_path.resolve(),
            log_level=str(adapter.get("log_level", "INFO")),
        ).validated()

    def validated(self) -> AdapterConfig:
        if not self.buzz_command.strip():
            raise ValueError("buzz.command must not be empty")
        parsed = urlsplit(self.relay_url)
        if parsed.scheme not in {"ws", "wss", "http", "https"} or not parsed.netloc:
            raise ValueError("buzz.relay_url or BUZZ_RELAY_URL must be a relay URL")
        private_key = os.getenv("BUZZ_PRIVATE_KEY", "").strip()
        if not private_key:
            raise ValueError("BUZZ_PRIVATE_KEY is required")
        try:
            private_key_pubkey = Keys.parse(private_key).public_key().to_hex()
        except Exception as exc:
            raise ValueError(
                "BUZZ_PRIVATE_KEY is not a valid Nostr private key"
            ) from exc
        if len(self.agent_pubkey) != 64 or any(
            char not in _HEX_CHARS for char in self.agent_pubkey
        ):
            raise ValueError(
                "buzz.agent_pubkey or BUZZ_AGENT_PUBKEY must be a 64-character hex pubkey"
            )
        if private_key_pubkey.casefold() != self.agent_pubkey.casefold():
            raise ValueError(
                "BUZZ_PRIVATE_KEY does not match buzz.agent_pubkey; refusing to "
                "start with the wrong Buzz identity"
            )
        for field_name, values in (("buzz.allowed_pubkeys", self.allowed_pubkeys),):
            for value in values:
                if len(value) != 64 or any(char not in _HEX_CHARS for char in value):
                    raise ValueError(
                        f"{field_name} values must be 64-character hex pubkeys"
                    )
        for channel_id in self.channels:
            try:
                uuid.UUID(channel_id)
            except ValueError as exc:
                raise ValueError("buzz.channels values must be UUIDs") from exc
        for channel_id, name in self.channel_names.items():
            try:
                uuid.UUID(channel_id)
            except ValueError as exc:
                raise ValueError(
                    "buzz.channel_names keys must be channel UUIDs"
                ) from exc
            if not name.strip():
                raise ValueError("buzz.channel_names values must not be empty")
        if self.profile_name and len(self.profile_name) > 100:
            raise ValueError("buzz.profile.name must not exceed 100 characters")
        if (
            not self.auto_discover_channels
            and not self.channels
            and not self.include_dms
        ):
            raise ValueError("configure buzz.channels or enable channel/DM discovery")
        if not self.message_kinds or any(kind <= 0 for kind in self.message_kinds):
            raise ValueError("buzz.message_kinds must contain positive integers")
        if any(kind != 9 for kind in self.message_kinds):
            raise ValueError(
                "the first adapter release supports only Buzz message kind 9"
            )
        if self.session_scope not in {"channel", "thread"}:
            raise ValueError("adapter.session_scope must be 'channel' or 'thread'")
        if not self.deerflow_command.strip():
            raise ValueError("deerflow.command must point to deerflow-acp.exe")
        if self.deerflow_protocol not in {"v1", "v2"}:
            raise ValueError("deerflow.protocol must be 'v1' or 'v2'")
        selected_protocol: str | None = None
        for index, argument in enumerate(self.deerflow_args):
            if argument != "--protocol":
                continue
            if index + 1 >= len(self.deerflow_args):
                raise ValueError("deerflow.args --protocol requires a value")
            selected_protocol = self.deerflow_args[index + 1].casefold()
        if self.deerflow_protocol == "v2" and selected_protocol not in {"2", "v2"}:
            raise ValueError(
                'deerflow.args must include "--protocol", "v2" when '
                'deerflow.protocol = "v2"'
            )
        if selected_protocol is not None:
            normalized = {"1": "v1", "v1": "v1", "2": "v2", "v2": "v2"}.get(
                selected_protocol
            )
            if normalized != self.deerflow_protocol:
                raise ValueError(
                    "deerflow.protocol must match the --protocol value in deerflow.args"
                )
        if not self.workspace.exists() or not self.workspace.is_dir():
            raise ValueError(
                f"deerflow.workspace must be an existing directory: {self.workspace}"
            )
        if self.poll_interval_seconds <= 0:
            raise ValueError("adapter.poll_interval_seconds must be positive")
        if self.buzz_cli_timeout_seconds <= 0:
            raise ValueError("buzz.timeout_seconds must be positive")
        if self.max_poll_pages <= 0:
            raise ValueError("adapter.max_poll_pages must be positive")
        if self.transport_retry_attempts <= 0:
            raise ValueError("adapter.transport_retry_attempts must be positive")
        if self.transport_retry_base_seconds < 0:
            raise ValueError(
                "adapter.transport_retry_base_seconds must not be negative"
            )
        if self.transport_retry_max_seconds < self.transport_retry_base_seconds:
            raise ValueError(
                "adapter.transport_retry_max_seconds must be greater than or equal to "
                "adapter.transport_retry_base_seconds"
            )
        if self.max_message_attempts <= 0:
            raise ValueError("adapter.max_message_attempts must be positive")
        if self.acp_timeout_seconds <= 0:
            raise ValueError("deerflow.timeout_seconds must be positive")
        if self.presence_heartbeat_seconds <= 0:
            raise ValueError(
                "buzz.lifecycle.presence_heartbeat_seconds must be positive"
            )
        if (
            self.observer_enabled or self.progress_messages_enabled
        ) and self.deerflow_protocol != "v2":
            raise ValueError("Buzz observability requires deerflow.protocol = 'v2'")
        if self.observer_owner_pubkey and (
            len(self.observer_owner_pubkey) != 64
            or any(char not in _HEX_CHARS for char in self.observer_owner_pubkey)
        ):
            raise ValueError(
                "observability.owner_pubkey must be a 64-character hex pubkey"
            )
        if self.observer_queue_size <= 0 or self.observer_queue_size > 800:
            raise ValueError("observability.queue_size must be between 1 and 800")
        if self.observer_publish_timeout_seconds <= 0:
            raise ValueError("observability.publish_timeout_seconds must be positive")
        if self.progress_update_interval_seconds < 0:
            raise ValueError(
                "observability.progress_update_interval_seconds must not be negative"
            )
        if self.progress_max_tools <= 0 or self.progress_max_tools > 50:
            raise ValueError(
                "observability.progress_max_tools must be between 1 and 50"
            )
        return self


def parse_args(argv: list[str] | None = None) -> tuple[AdapterConfig, bool]:
    parser = argparse.ArgumentParser(
        description="Connect Buzz to DeerFlow Portable ACP"
    )
    parser.add_argument("--config", required=True, type=Path)
    parser.add_argument(
        "--once",
        action="store_true",
        help="Discover and drain Buzz once, without starting the polling loop",
    )
    args = parser.parse_args(argv)
    return AdapterConfig.from_toml(args.config), bool(args.once)
