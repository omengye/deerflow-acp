from __future__ import annotations

import json
from pathlib import Path

import pytest
import yaml
from langchain_core.tools import BaseTool

from deerflow import config_tool
from deerflow.acp.config import LocalACPConfig
from deerflow.config.app_config import AppConfig
from deerflow.config.skills_config import SkillsConfig
from deerflow.reflection import resolve_variable


def _layout(tmp_path: Path) -> tuple[Path, Path, Path]:
    resources = tmp_path / "resources"
    resources.mkdir()
    source_template = Path(__file__).resolve().parents[1] / "resources" / "default-config.yaml"
    (resources / "default-config.yaml").write_bytes(source_template.read_bytes())
    skill_dir = resources / "skills" / "public" / "sample-skill"
    skill_dir.mkdir(parents=True)
    (skill_dir / "SKILL.md").write_text(
        "---\nname: sample-skill\ndescription: Sample portable skill\n---\n\nInstructions.\n",
        encoding="utf-8",
    )
    user_data = tmp_path / "user-data"
    config_path = user_data / "config" / "config.yaml"
    config_tool.ensure_layout(config_path, user_data, resources)
    return config_path, user_data, resources


def test_layout_snapshot_and_save_preserve_secrets_and_mcp(tmp_path: Path) -> None:
    config_path, user_data, _ = _layout(tmp_path)
    raw = yaml.safe_load(config_path.read_text(encoding="utf-8"))
    raw["models"][0]["api_key"] = "literal-secret"
    raw["subagents"]["future_setting"] = {"keep": True}
    raw["subagents"]["agents"] = {
        "general-purpose": {"model": "openai", "future_override": 7}
    }
    raw["sandbox"]["environment"] = {
        "SERVICE_API_KEY": "sandbox-secret",
        "SAFE_VALUE": "visible",
    }
    raw["sandbox"]["future_setting"] = {"keep": True}
    raw["tools"][0]["api_key"] = "tool-secret"
    raw["skill_evolution"]["future_setting"] = {"keep": True}
    raw["skill_evolution"]["discovery"]["future_threshold"] = 9
    config_tool._atomic_write_yaml(config_path, raw)
    extensions_path = config_path.parent / "extensions_config.json"
    extensions_path.write_text(
        json.dumps({"mcpServers": {"keep": {"command": "example"}}, "skills": {}}),
        encoding="utf-8",
    )

    current = config_tool.snapshot(config_path, user_data)
    assert current["models"][0]["api_key"] == ""
    assert current["models"][0]["api_key_literal"] is True
    assert current["skills"][0]["enabled"] is True
    assert current["subagents"]["enabled"] is True
    assert {item["name"] for item in current["subagents"]["builtin_agents"]} >= {
        "general-purpose",
        "bash",
    }
    assert (
        current["sandbox"]["advanced"]["environment"]["SERVICE_API_KEY"]
        == config_tool._REDACTED_VALUE
    )
    assert current["sandbox"]["advanced"]["environment"]["SAFE_VALUE"] == "visible"
    assert current["tools"][0]["api_key"] == config_tool._REDACTED_VALUE
    assert current["skill_evolution"]["enabled"] is False
    assert current["runtime"]["goal_auto_continue"] is False
    assert current["runtime"]["goal_max_continuations"] == 3
    assert current["runtime"]["goal_max_no_progress_continuations"] == 2

    current["skills"][0]["enabled"] = False
    current["subagents"]["timeout_seconds"] = 1200
    current["subagents"]["max_turns"] = 30
    current["sandbox"]["allow_host_tools"] = True
    current["skill_evolution"]["enabled"] = True
    current["runtime"]["goal_auto_continue"] = True
    current["runtime"]["goal_max_continuations"] = 4
    current["runtime"]["goal_max_no_progress_continuations"] = 1
    current["skill_evolution"]["mode"] = "auto_patch"
    current["skill_evolution"]["discovery"]["enabled"] = True
    current["skill_evolution"]["monitoring"]["probation_uses"] = 6
    saved = config_tool.save(config_path, user_data, config_tool.SaveDocument.model_validate(current))

    persisted = yaml.safe_load(config_path.read_text(encoding="utf-8"))
    assert persisted["models"][0]["api_key"] == "literal-secret"
    assert persisted["subagents"]["timeout_seconds"] == 1200
    assert persisted["subagents"]["max_turns"] == 30
    assert persisted["subagents"]["future_setting"] == {"keep": True}
    assert persisted["subagents"]["agents"]["general-purpose"]["future_override"] == 7
    assert persisted["sandbox"]["allow_host_tools"] is True
    assert persisted["sandbox"]["environment"]["SERVICE_API_KEY"] == "sandbox-secret"
    assert persisted["sandbox"]["future_setting"] == {"keep": True}
    assert persisted["tools"][0]["api_key"] == "tool-secret"
    assert persisted["local_acp"]["goal_auto_continue"] is True
    assert persisted["local_acp"]["goal_max_continuations"] == 4
    assert persisted["local_acp"]["goal_max_no_progress_continuations"] == 1
    assert persisted["skill_evolution"]["enabled"] is True
    assert persisted["skill_evolution"]["mode"] == "auto_patch"
    assert persisted["skill_evolution"]["discovery"]["enabled"] is True
    assert persisted["skill_evolution"]["monitoring"]["probation_uses"] == 6
    assert persisted["skill_evolution"]["future_setting"] == {"keep": True}
    assert persisted["skill_evolution"]["discovery"]["future_threshold"] == 9
    extensions = json.loads(extensions_path.read_text(encoding="utf-8"))
    assert extensions["mcpServers"]["keep"]["command"] == "example"
    assert extensions["skills"]["sample-skill"]["enabled"] is False
    assert Path(saved["backup"]).is_dir()


def test_memory_snapshot_save_and_paths_use_portable_deerflow_home(tmp_path: Path) -> None:
    config_path, user_data, _ = _layout(tmp_path)
    raw = yaml.safe_load(config_path.read_text(encoding="utf-8"))
    raw["memory"]["future_setting"] = {"keep": True}
    raw["memory"]["backend_config"]["future_backend_setting"] = 7
    config_tool._atomic_write_yaml(config_path, raw)

    current = config_tool.snapshot(config_path, user_data)
    assert current["memory"]["enabled"] is True
    assert current["memory"]["manager_class"] == "deermem"
    assert current["memory"]["mode"] == "middleware"
    assert current["memory"]["advanced"]["future_setting"] == {"keep": True}
    assert current["memory"]["backend_advanced"]["future_backend_setting"] == 7
    assert Path(current["paths"]["memory"]) == (
        user_data / "data" / "deerflow" / "memory.json"
    ).resolve()
    assert Path(current["paths"]["memory_index"]) == (
        user_data / "data" / "deerflow" / "memory-fts5.sqlite3"
    ).resolve()

    current["memory"].update(
        {
            "mode": "tool",
            "model_name": "openai",
            "debounce_seconds": 12,
            "retrieval_top_k": 20,
            "fact_dedup_enabled": True,
            "fact_dedup_similarity_threshold": 0.82,
            "storage_path": "profile/memory.json",
            "retrieval_index_path": "profile/memory.sqlite3",
        }
    )
    saved = config_tool.save(
        config_path,
        user_data,
        config_tool.SaveDocument.model_validate(current),
    )

    persisted = yaml.safe_load(config_path.read_text(encoding="utf-8"))["memory"]
    assert persisted["manager_class"] == "deermem"
    assert persisted["mode"] == "tool"
    assert persisted["future_setting"] == {"keep": True}
    assert persisted["backend_config"]["model_name"] == "openai"
    assert persisted["backend_config"]["debounce_seconds"] == 12
    assert persisted["backend_config"]["retrieval_top_k"] == 20
    assert persisted["backend_config"]["fact_dedup_enabled"] is True
    assert persisted["backend_config"]["fact_dedup_similarity_threshold"] == 0.82
    assert persisted["backend_config"]["future_backend_setting"] == 7
    assert Path(saved["paths"]["memory"]) == (
        user_data / "data" / "deerflow" / "profile" / "memory.json"
    ).resolve()


def test_missing_memory_section_uses_defaults_and_is_written_on_save(tmp_path: Path) -> None:
    config_path, user_data, _ = _layout(tmp_path)
    raw = yaml.safe_load(config_path.read_text(encoding="utf-8"))
    raw.pop("memory")
    config_tool._atomic_write_yaml(config_path, raw)

    current = config_tool.snapshot(config_path, user_data)
    assert current["memory"]["enabled"] is True
    assert current["memory"]["storage_path"] == ""
    config_tool.save(
        config_path,
        user_data,
        config_tool.SaveDocument.model_validate(current),
    )

    persisted = yaml.safe_load(config_path.read_text(encoding="utf-8"))
    assert persisted["memory"]["manager_class"] == "deermem"
    assert persisted["memory"]["backend_config"]["storage_path"] == ""


def test_memory_rejects_unknown_model_and_remote_manager(tmp_path: Path) -> None:
    config_path, user_data, _ = _layout(tmp_path)
    current = config_tool.snapshot(config_path, user_data)
    current["memory"]["model_name"] = "missing-model"
    with pytest.raises(ValueError, match="memory extraction model"):
        config_tool.save(
            config_path,
            user_data,
            config_tool.SaveDocument.model_validate(current),
        )

    current = config_tool.snapshot(config_path, user_data)
    current["memory"]["manager_class"] = "mem0"
    with pytest.raises(ValueError, match="only local DeerMem"):
        config_tool.SaveDocument.model_validate(current)


def test_subagent_collections_support_deletion(tmp_path: Path) -> None:
    config_path, user_data, _ = _layout(tmp_path)
    raw = yaml.safe_load(config_path.read_text(encoding="utf-8"))
    raw["subagents"]["agents"] = {"general-purpose": {"model": "openai"}}
    raw["subagents"]["custom_agents"] = {
        "temporary": {
            "description": "Temporary",
            "system_prompt": "Do temporary work",
        }
    }
    config_tool._atomic_write_yaml(config_path, raw)

    current = config_tool.snapshot(config_path, user_data)
    current["subagents"]["agents"] = {}
    current["subagents"]["custom_agents"] = {}
    config_tool.save(
        config_path,
        user_data,
        config_tool.SaveDocument.model_validate(current),
    )

    persisted = yaml.safe_load(config_path.read_text(encoding="utf-8"))
    assert persisted["subagents"]["agents"] == {}
    assert persisted["subagents"]["custom_agents"] == {}


def test_sandbox_and_tools_validate_references_and_redacted_secrets(tmp_path: Path) -> None:
    config_path, user_data, _ = _layout(tmp_path)
    current = config_tool.snapshot(config_path, user_data)
    current["tools"][0]["group"] = "missing-group"
    with pytest.raises(ValueError, match="unknown tool groups"):
        config_tool.save(
            config_path,
            user_data,
            config_tool.SaveDocument.model_validate(current),
        )

    current = config_tool.snapshot(config_path, user_data)
    current["tools"].append(
        {
            "name": "new-secret-tool",
            "group": "file:read",
            "use": "example.module:tool",
            "api_key": config_tool._REDACTED_VALUE,
        }
    )
    with pytest.raises(ValueError, match="redacted value without an existing secret"):
        config_tool.save(
            config_path,
            user_data,
            config_tool.SaveDocument.model_validate(current),
        )


def test_skill_evolution_rejects_invalid_limits_and_unknown_models(tmp_path: Path) -> None:
    config_path, user_data, _ = _layout(tmp_path)
    current = config_tool.snapshot(config_path, user_data)
    current["skill_evolution"]["discovery"]["repeat_threshold"] = 1
    with pytest.raises(ValueError):
        config_tool.SaveDocument.model_validate(current)

    current = config_tool.snapshot(config_path, user_data)
    current["skill_evolution"]["generation_model_name"] = "missing-model"
    with pytest.raises(ValueError, match="must reference a configured model"):
        config_tool.save(
            config_path,
            user_data,
            config_tool.SaveDocument.model_validate(current),
        )


def test_subagents_reject_invalid_limits_and_unknown_models(tmp_path: Path) -> None:
    config_path, user_data, _ = _layout(tmp_path)
    current = config_tool.snapshot(config_path, user_data)
    current["subagents"]["timeout_seconds"] = 0
    with pytest.raises(ValueError):
        config_tool.SaveDocument.model_validate(current)

    current = config_tool.snapshot(config_path, user_data)
    current["subagents"]["agents"] = {
        "general-purpose": {"model": "missing-model"}
    }
    with pytest.raises(ValueError, match="must reference a configured model"):
        config_tool.save(
            config_path,
            user_data,
            config_tool.SaveDocument.model_validate(current),
        )


def test_save_rejects_stale_revision(tmp_path: Path) -> None:
    config_path, user_data, _ = _layout(tmp_path)
    current = config_tool.snapshot(config_path, user_data)
    config_path.write_text(config_path.read_text(encoding="utf-8") + "\n", encoding="utf-8")
    with pytest.raises(RuntimeError, match="changed after it was loaded"):
        config_tool.save(config_path, user_data, config_tool.SaveDocument.model_validate(current))


def test_save_rejects_new_agent_that_would_overwrite_existing_directory(tmp_path: Path) -> None:
    config_path, user_data, _ = _layout(tmp_path)
    agents_dir = user_data / "data" / "deerflow" / "agents" / "existing"
    agents_dir.mkdir(parents=True)
    (agents_dir / "config.yaml").write_text(
        "name: existing\ndescription: keep\n", encoding="utf-8"
    )
    current = config_tool.snapshot(config_path, user_data)
    current["agents"] = [
        {
            "original_name": "",
            "name": "existing",
            "description": "replacement",
            "model": None,
            "tool_groups": [],
            "skills": None,
            "soul": "",
        }
    ]
    with pytest.raises(ValueError, match="already exists"):
        config_tool.save(
            config_path,
            user_data,
            config_tool.SaveDocument.model_validate(current),
        )


def test_relative_skill_path_uses_active_config_directory(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    config_path = tmp_path / "config" / "config.yaml"
    config_path.parent.mkdir()
    config_path.touch()
    monkeypatch.setenv("DEER_FLOW_CONFIG_PATH", str(config_path))
    assert SkillsConfig(path="../skills").get_skills_path() == (tmp_path / "skills").resolve()


def test_portable_template_validates_and_resolves_default_tools(tmp_path: Path) -> None:
    config_path, user_data, _ = _layout(tmp_path)
    app_config = AppConfig.from_file(str(config_path))
    local_config = LocalACPConfig.from_file(str(config_path))

    assert app_config.get_default_model_name() == "openai"
    assert app_config.memory.enabled is True
    assert app_config.memory.manager_class == "deermem"
    assert app_config.memory.retrieval_enabled is True
    assert local_config.checkpointer_path == (user_data / "data" / "acp-checkpoints.db").resolve()
    assert local_config.goal_auto_continue is False
    assert local_config.goal_max_continuations == 3
    assert local_config.goal_max_no_progress_continuations == 2
    for tool in app_config.tools:
        assert resolve_variable(tool.use, BaseTool).name == tool.name
