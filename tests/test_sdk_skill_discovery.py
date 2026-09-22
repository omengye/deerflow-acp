import json

import pytest

from deerflow.agents.factory import create_deerflow_agent
from deerflow.agents.features import RuntimeFeatures
from deerflow.config.skills_config import SkillsConfig
from deerflow.skills.describe import build_describe_skill_tool, build_explicit_skill_catalog_provider


def _skill(root, name, description="Explicit SDK skill"):
    folder = root / "public" / name
    folder.mkdir(parents=True)
    (folder / "SKILL.md").write_text(f"---\nname: {name}\ndescription: {description}\n---\nBody is not returned.\n", encoding="utf-8")


def test_sdk_tool_invocation_uses_only_explicit_directory_and_frozen_scope(monkeypatch, tmp_path):
    _skill(tmp_path, "allowed")
    _skill(tmp_path, "blocked")

    def forbidden(*_args, **_kwargs):
        raise AssertionError("unexpected global configuration lookup")

    monkeypatch.setattr("deerflow.config.get_app_config", forbidden)
    monkeypatch.setattr("deerflow.config.extensions_config.ExtensionsConfig.from_file", forbidden)
    monkeypatch.setattr("deerflow.agents.lead_agent.prompt.get_skill_discovery_catalog", forbidden)
    captured = {}
    monkeypatch.setattr("langchain.agents.create_agent", lambda **kwargs: captured.update(kwargs) or captured)
    config = SkillsConfig(path=str(tmp_path), container_path="/sdk/skills", discovery_mode="on")
    allowed = ["allowed"]
    create_deerflow_agent(model=object(), features=RuntimeFeatures(sandbox=False), skills_config=config, available_skills=allowed)
    config.path = str(tmp_path / "wrong")
    allowed.append("blocked")
    tool = next(tool for tool in captured["tools"] if tool.name == "describe_skill")
    result = json.loads(tool.invoke({"query": "select:allowed,blocked"}))
    assert [skill["name"] for skill in result["skills"]] == ["allowed"]
    assert result["skills"][0]["location"] == "/sdk/skills/public/allowed/SKILL.md"


def test_explicit_sdk_extensions_file_can_disable_skills(tmp_path):
    _skill(tmp_path, "enabled")
    _skill(tmp_path, "disabled")
    extension_file = tmp_path / "extensions.json"
    extension_file.write_text(json.dumps({"skills": {"disabled": {"enabled": False}}}), encoding="utf-8")
    provider = build_explicit_skill_catalog_provider(SkillsConfig(path=str(tmp_path), extensions_file=str(extension_file)))
    tool = build_describe_skill_tool(catalog_provider=provider)
    result = json.loads(tool.invoke({"query": "select:enabled,disabled"}))
    assert [skill["name"] for skill in result["skills"]] == ["enabled"]


def test_sdk_discovery_requires_explicit_roots():
    with pytest.raises(ValueError, match="explicit skills_config.path"):
        build_explicit_skill_catalog_provider(SkillsConfig(discovery_mode="on"))
