from __future__ import annotations

from types import SimpleNamespace

import pytest
import yaml
from pydantic import ValidationError

from deerflow.config.agents_config import AgentConfig
from deerflow.tools.builtins.setup_agent_tool import setup_agent


@pytest.mark.parametrize(
    "value",
    ["代码审查助手", "مراجع الكود", "می\u200cروم", "👩‍💻", "e\u0301"],
)
def test_agent_display_name_accepts_safe_unicode(value: str) -> None:
    assert AgentConfig(name="reviewer", display_name=f"  {value}  ").display_name == value


@pytest.mark.parametrize(
    "value",
    ["line\nbreak", "a\u200fb", "\u200b", "\ufe0f", "\u0301", "🦌" * 101],
)
def test_agent_display_name_rejects_controls_invisible_only_and_overlong(value: str) -> None:
    with pytest.raises(ValidationError):
        AgentConfig(name="reviewer", display_name=value)


def test_agent_identifier_remains_ascii_slug() -> None:
    from deerflow.config.agents_config import validate_agent_name

    with pytest.raises(ValueError):
        validate_agent_name("代码审查助手")


def test_agent_memory_is_enabled_by_default_and_can_be_disabled() -> None:
    assert AgentConfig(name="default-policy").memory_enabled is True
    assert AgentConfig(name="stateless", memory_enabled=False).memory_enabled is False


def test_setup_agent_preserves_existing_display_name(tmp_path, monkeypatch) -> None:
    agent_dir = tmp_path / "agents" / "reviewer"
    agent_dir.mkdir(parents=True)
    config_path = agent_dir / "config.yaml"
    config_path.write_text(
        yaml.safe_dump(
            {
                "name": "reviewer",
                "display_name": "代码审查助手",
                "description": "old",
                "memory_enabled": False,
            },
            allow_unicode=True,
        ),
        encoding="utf-8",
    )
    paths = SimpleNamespace(
        base_dir=tmp_path,
        agent_dir=lambda name: tmp_path / "agents" / name,
    )
    monkeypatch.setattr(
        "deerflow.tools.builtins.setup_agent_tool.get_paths",
        lambda: paths,
    )
    runtime = SimpleNamespace(
        context={"agent_name": "reviewer"},
        tool_call_id="setup-1",
    )

    setup_agent.func(
        soul="Updated soul",
        description="updated",
        skills=[],
        runtime=runtime,
    )

    persisted = yaml.safe_load(config_path.read_text(encoding="utf-8"))
    assert persisted["display_name"] == "代码审查助手"
    assert persisted["description"] == "updated"
    assert persisted["memory_enabled"] is False
