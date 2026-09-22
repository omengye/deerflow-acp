from __future__ import annotations

import json
from pathlib import Path
from types import SimpleNamespace

import pytest

from deerflow.agents.lead_agent import prompt
from deerflow.config.skills_config import SkillsConfig
from deerflow.skills.catalog import catalog_from_signature
from deerflow.skills.describe import build_describe_skill_tool
from deerflow.skills.types import Skill


def _skill(name: str, description: str) -> Skill:
    return Skill(name, description, None, Path(name), Path(name) / "SKILL.md", Path(name), "custom", True)


@pytest.fixture
def discovery(monkeypatch):
    skills = [_skill("charts", "Create charts with 中文标题图表 support"), _skill("translate", "Translate 中文文档")]
    config = SimpleNamespace(skills=SkillsConfig(discovery_mode="on"), skill_evolution=SimpleNamespace(enabled=False))
    monkeypatch.setattr("deerflow.config.get_app_config", lambda: config)
    monkeypatch.setattr(prompt, "_enabled_skills_cache", skills)
    yield skills, config
    catalog_from_signature.cache_clear()
    prompt._get_cached_skills_prompt_section.cache_clear()


def test_intent_search_is_literal_ranked_and_supports_chinese():
    catalog = catalog_from_signature((
        ("translation", "Translate 中文文档", "public", "/translate/SKILL.md"),
        ("charts", "Create charts with 中文标题图表", "public", "/charts/SKILL.md"),
        ("reports", "Write reports", "public", "/reports/SKILL.md"),
    ))
    assert catalog.search("中文图表")[0].name == "charts"
    assert catalog.search("create charts")[0].name == "charts"
    assert catalog.search(".*") == []
    assert catalog.search("+") == []
    assert [item.name for item in catalog.search("+chart unsupported")] == ["charts"]


def test_explicit_selection_is_not_truncated_or_reinterpreted():
    names = [f"skill-{index}-" + "x" * 50 for index in range(8)]
    catalog = catalog_from_signature(tuple((name, "workflow", "public", "/" + name) for name in names))
    assert [item.name for item in catalog.search("select:" + ",".join(names))] == names
    assert catalog.search("$" + names[-1])[0].name == names[-1]
    assert catalog.search(names[-1])[0].name == names[-1]
    assert len(catalog.search("workflow")) == 5


def test_auto_threshold_counts_agent_visible_skills_and_default_is_legacy(discovery):
    _, config = discovery
    config.skills = SkillsConfig()
    assert "<available_skills>" in prompt.get_skills_prompt_section()
    assert "<description>" in prompt.get_skills_prompt_section()
    config.skills = SkillsConfig(discovery_mode="auto", discovery_threshold=2)
    assert "<skill_index>" in prompt.get_skills_prompt_section()
    assert "<description>" not in prompt.get_skills_prompt_section()
    scoped = prompt.get_skills_prompt_section({"charts"})
    assert "<available_skills>" in scoped
    assert "translate" not in scoped
    assert prompt.get_skills_prompt_section(set()) == ""
    config.skills.enabled = False
    assert prompt.get_skills_prompt_section() == ""


def test_discovery_tools_keep_independent_scope_and_follow_metadata_updates(discovery):
    skills, config = discovery
    allowed = {"charts"}
    charts = build_describe_skill_tool(allowed)
    translation = build_describe_skill_tool({"translate"})
    allowed.add("translate")
    assert json.loads(charts.invoke({"query": "select:charts,translate"}))["skills"][0]["name"] == "charts"
    assert len(json.loads(charts.invoke({"query": "select:charts,translate"}))["skills"]) == 1
    assert json.loads(translation.invoke({"query": "charts"}))["skills"] == []
    before = prompt.get_skill_discovery_catalog({"charts"})
    assert before is prompt.get_skill_discovery_catalog({"charts"})
    skills[0] = _skill("charts", "Updated chart instructions")
    after = json.loads(charts.invoke({"query": "charts"}))["skills"][0]
    assert after["description"] == "Updated chart instructions"
    assert after["location"] == "/mnt/skills/custom/charts/SKILL.md"
    assert before.entries[0].description != after["description"]
    config.skills.enabled = False
    assert json.loads(charts.invoke({"query": "charts"}))["skills"] == []


@pytest.mark.asyncio
async def test_evolution_refresh_invalidates_catalog_and_existing_tool(discovery, monkeypatch):
    _, _ = discovery
    tool = build_describe_skill_tool({"charts"})
    old = prompt.get_skill_discovery_catalog({"charts"})
    monkeypatch.setattr(prompt, "_load_enabled_skills_sync", lambda: [_skill("charts", "Fresh published workflow")])
    await prompt.refresh_skills_system_prompt_cache_async()
    refreshed = prompt.get_skill_discovery_catalog({"charts"})
    assert refreshed is not old
    assert json.loads(tool.invoke({"query": "charts"}))["skills"][0]["description"] == "Fresh published workflow"

