from __future__ import annotations

from pathlib import Path

import pytest
from nostr_sdk import Keys

from buzz_deerflow_adapter.config import AdapterConfig


def _identity(monkeypatch) -> str:
    keys = Keys.generate()
    monkeypatch.setenv("BUZZ_PRIVATE_KEY", keys.secret_key().to_bech32())
    return keys.public_key().to_hex()


def test_loads_portable_acp_config(tmp_path: Path, monkeypatch) -> None:
    pubkey = _identity(monkeypatch)
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    config_path = tmp_path / "adapter.toml"
    config_path.write_text(
        f"""
[buzz]
relay_url = "wss://buzz.sprwhisp.cc"
agent_pubkey = "{pubkey}"
channels = ["11111111-1111-1111-1111-111111111111"]
channel_names = {{ "11111111-1111-1111-1111-111111111111" = "DeerFlow 工作区" }}
auto_discover_channels = false
include_dms = false

[buzz.profile]
name = "DeerFlow"

[buzz.lifecycle]
presence_heartbeat_seconds = 30

[deerflow]
command = "deerflow-acp.exe"
protocol = "v2"
args = ["--protocol", "v2"]
workspace = "{workspace.as_posix()}"

[observability]
enabled = true
owner_pubkey = "{"b" * 64}"
progress_messages = true

[state]
path = "./state.sqlite3"
""",
        encoding="utf-8",
    )

    config = AdapterConfig.from_toml(config_path)
    assert config.relay_url == "wss://buzz.sprwhisp.cc"
    assert config.deerflow_protocol == "v2"
    assert config.deerflow_args == ["--protocol", "v2"]
    assert config.profile_name == "DeerFlow"
    assert config.channel_names == {
        "11111111-1111-1111-1111-111111111111": "DeerFlow 工作区"
    }
    assert config.presence_heartbeat_seconds == 30
    assert config.observer_enabled is True
    assert config.observer_owner_pubkey == "b" * 64
    assert config.progress_messages_enabled is True
    assert config.workspace == workspace.resolve()
    assert config.state_path == (tmp_path / "state.sqlite3").resolve()


def test_private_key_is_required_from_environment(tmp_path: Path, monkeypatch) -> None:
    monkeypatch.delenv("BUZZ_PRIVATE_KEY", raising=False)
    with pytest.raises(ValueError, match="BUZZ_PRIVATE_KEY"):
        AdapterConfig(
            relay_url="wss://buzz.sprwhisp.cc",
            agent_pubkey="a" * 64,
            deerflow_command="deerflow-acp.exe",
            workspace=tmp_path,
        ).validated()


def test_rejects_unknown_deerflow_protocol(tmp_path: Path, monkeypatch) -> None:
    pubkey = _identity(monkeypatch)
    with pytest.raises(ValueError, match="deerflow.protocol"):
        AdapterConfig(
            relay_url="wss://buzz.sprwhisp.cc",
            agent_pubkey=pubkey,
            deerflow_command="deerflow-acp.exe",
            deerflow_protocol="v3",
            workspace=tmp_path,
        ).validated()


def test_v2_requires_matching_bridge_argument(tmp_path: Path, monkeypatch) -> None:
    pubkey = _identity(monkeypatch)
    with pytest.raises(ValueError, match="deerflow.args"):
        AdapterConfig(
            relay_url="wss://buzz.sprwhisp.cc",
            agent_pubkey=pubkey,
            deerflow_command="deerflow-acp.exe",
            deerflow_protocol="v2",
            workspace=tmp_path,
        ).validated()


def test_rejects_private_key_for_a_different_identity(
    tmp_path: Path, monkeypatch
) -> None:
    _identity(monkeypatch)
    with pytest.raises(ValueError, match="does not match"):
        AdapterConfig(
            relay_url="wss://buzz.sprwhisp.cc",
            agent_pubkey="a" * 64,
            deerflow_command="deerflow-acp.exe",
            workspace=tmp_path,
        ).validated()
