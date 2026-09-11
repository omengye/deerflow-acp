"""JSON command service used by the portable DeerFlow configuration UI.

The desktop application deliberately does not parse or write YAML itself.  This
module owns validation, redaction, optimistic concurrency, backups and atomic
writes so the same rules can later be reused by another Agent configuration UI.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import sys
import tempfile
from contextlib import contextmanager
from copy import deepcopy
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import yaml
from pydantic import BaseModel, Field, ValidationError, field_validator

from deerflow.config.agents_config import AgentConfig, validate_agent_name
from deerflow.config.memory_config import MemoryConfig
from deerflow.config.model_config import ModelConfig
from deerflow.config.sandbox_config import SandboxConfig
from deerflow.config.skill_evolution_config import SkillEvolutionConfig
from deerflow.config.subagents_config import SubagentsAppConfig
from deerflow.config.tool_config import ToolConfig, ToolGroupConfig
from deerflow.skills.parser import parse_skill_file
from deerflow.subagents.builtins import BUILTIN_SUBAGENTS

_ENV_REFERENCE = re.compile(r"^\$[A-Za-z_][A-Za-z0-9_]*$|^\$\{.+\}$")
_REDACTED_VALUE = "__DEERFLOW_REDACTED__"


class RuntimeDocument(BaseModel):
    model_name: str | None = None
    agent_name: str | None = None
    thinking_enabled: bool = True
    plan_mode: bool = True
    subagent_enabled: bool = False
    max_concurrent_subagents: int = Field(default=2, ge=1, le=4)
    max_active_connections: int = Field(default=16, ge=1, le=128)
    max_active_runs: int = Field(default=2, ge=1, le=128)
    run_timeout_seconds: float = Field(default=600, gt=0)
    queue_timeout_seconds: float = Field(default=600, gt=0, le=86400)
    session_cleanup_enabled: bool = True
    inactive_session_retention_days: int = Field(default=30, ge=1, le=3650)
    closed_session_retention_days: int = Field(default=30, ge=0, le=3650)
    session_cleanup_interval_seconds: float = Field(default=3600, ge=60, le=86400)
    goal_auto_continue: bool = False
    goal_max_continuations: int = Field(default=3, ge=0, le=8)
    goal_max_no_progress_continuations: int = Field(default=2, ge=0, le=8)
    permission_mode: str = "dangerous"
    memory_scope: str = "workspace"
    enable_bash: bool = False
    tool_allowlist: list[str] | None = None
    tool_denylist: list[str] = Field(default_factory=list)
    prompt_overlay: str = Field(default="", max_length=65_536)

    @field_validator("permission_mode")
    @classmethod
    def _permission_mode(cls, value: str) -> str:
        value = value.strip().lower()
        if value not in {"off", "dangerous", "all"}:
            raise ValueError("must be off, dangerous, or all")
        return value

    @field_validator("memory_scope")
    @classmethod
    def _memory_scope(cls, value: str) -> str:
        value = value.strip().lower()
        if value not in {"global", "workspace", "session"}:
            raise ValueError("must be global, workspace, or session")
        return value


class MemoryDocument(BaseModel):
    """Editable local DeerMem settings plus lossless source mappings."""

    enabled: bool = True
    manager_class: str = "deermem"
    mode: str = "middleware"
    injection_enabled: bool = True
    shutdown_flush_timeout_seconds: float = Field(default=30, ge=0.1, le=300)
    storage_path: str = ""
    storage_class: str = "deerflow.agents.memory.storage.FileMemoryStorage"
    debounce_seconds: int = Field(default=30, ge=1, le=300)
    model_name: str | None = None
    max_facts: int = Field(default=100, ge=10, le=500)
    fact_confidence_threshold: float = Field(default=0.7, ge=0, le=1)
    max_injection_tokens: int = Field(default=2000, ge=100, le=8000)
    retrieval_enabled: bool = True
    retrieval_top_k: int = Field(default=12, ge=1, le=100)
    retrieval_index_path: str = ""
    advanced: dict[str, Any] = Field(default_factory=dict)
    backend_advanced: dict[str, Any] = Field(default_factory=dict)

    @field_validator("manager_class")
    @classmethod
    def _local_manager_only(cls, value: str) -> str:
        value = value.strip()
        if value != "deermem":
            raise ValueError("portable configuration currently supports only local DeerMem")
        return value

    @field_validator("mode")
    @classmethod
    def _memory_mode(cls, value: str) -> str:
        value = value.strip().lower()
        if value not in {"middleware", "tool"}:
            raise ValueError("must be middleware or tool")
        return value


class SkillEvolutionDocument(SkillEvolutionConfig):
    """Editable evolution settings plus the source mapping for lossless writes."""

    advanced: dict[str, Any] = Field(default_factory=dict)


class SubagentsDocument(SubagentsAppConfig):
    """Editable Subagent settings plus metadata and source for lossless writes."""

    builtin_agents: list[dict[str, Any]] = Field(default_factory=list)
    advanced: dict[str, Any] = Field(default_factory=dict)


class SandboxDocument(BaseModel):
    """High-level sandbox safety controls plus redacted provider settings."""

    use: str = Field(min_length=1)
    allow_host_bash: bool = False
    allow_host_tools: bool = False
    advanced: dict[str, Any] = Field(default_factory=dict)


class SaveDocument(BaseModel):
    config_revision: str
    extensions_revision: str
    default_model: str
    models: list[dict[str, Any]]
    runtime: RuntimeDocument
    memory: MemoryDocument
    agents: list[dict[str, Any]]
    subagents: SubagentsDocument
    sandbox: SandboxDocument
    tool_groups: list[dict[str, Any]]
    tools: list[dict[str, Any]]
    skills_enabled: bool = True
    skills: list[dict[str, Any]]
    skill_evolution: SkillEvolutionDocument


def _sha256(path: Path) -> str:
    if not path.exists():
        return ""
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _load_yaml(path: Path) -> dict[str, Any]:
    try:
        data = yaml.safe_load(path.read_text(encoding="utf-8")) or {}
    except (OSError, UnicodeDecodeError, yaml.YAMLError) as exc:
        raise ValueError(f"Unable to read {path}: {exc}") from exc
    if not isinstance(data, dict):
        raise ValueError(f"{path} must contain a YAML object")
    return data


def _load_json_object(path: Path) -> dict[str, Any]:
    if not path.exists():
        return {}
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError(f"Unable to read {path}: {exc}") from exc
    if not isinstance(data, dict):
        raise ValueError(f"{path} must contain a JSON object")
    return data


def _resolve_from_config(config_path: Path, value: str | Path) -> Path:
    path = Path(value).expanduser()
    if not path.is_absolute():
        path = config_path.parent / path
    return path.resolve()


def _extensions_path(config_path: Path, data: dict[str, Any]) -> Path:
    api = data.get("api") if isinstance(data.get("api"), dict) else {}
    skills = data.get("skills") if isinstance(data.get("skills"), dict) else {}
    raw = api.get("extensions_config_path") or skills.get("extensions_file")
    return _resolve_from_config(config_path, str(raw or "./extensions_config.json"))


def _skills_path(config_path: Path, user_data: Path, data: dict[str, Any]) -> Path:
    skills = data.get("skills") if isinstance(data.get("skills"), dict) else {}
    raw = skills.get("path")
    return _resolve_from_config(config_path, str(raw)) if raw else user_data / "skills"


def _deerflow_home(config_path: Path, user_data: Path, data: dict[str, Any]) -> Path:
    api = data.get("api") if isinstance(data.get("api"), dict) else {}
    raw = api.get("deerflow_home")
    return _resolve_from_config(config_path, str(raw)) if raw else user_data / "data" / "deerflow"


def _atomic_write_text(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    previous_mode = path.stat().st_mode if path.exists() else None
    temporary: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            "w", encoding="utf-8", newline="\n", suffix=".tmp", delete=False, dir=path.parent
        ) as stream:
            temporary = Path(stream.name)
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
        if previous_mode is not None:
            os.chmod(temporary, previous_mode)
        os.replace(temporary, path)
        temporary = None
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def _atomic_write_yaml(path: Path, data: dict[str, Any]) -> None:
    _atomic_write_text(path, yaml.safe_dump(data, allow_unicode=True, sort_keys=False))


def _atomic_write_json(path: Path, data: dict[str, Any]) -> None:
    _atomic_write_text(path, json.dumps(data, ensure_ascii=False, indent=2) + "\n")


def _copy_initial_skills(resources: Path, destination: Path) -> None:
    source = resources / "skills"
    if not source.is_dir() or any(destination.glob("*")):
        return
    destination.mkdir(parents=True, exist_ok=True)
    for category in ("public", "custom"):
        category_source = source / category
        if category_source.is_dir():
            shutil.copytree(category_source, destination / category, dirs_exist_ok=True)


def ensure_layout(config_path: Path, user_data: Path, resources: Path) -> None:
    for relative in ("config", "data", "skills", "logs", "backups", "runtime/acp"):
        (user_data / relative).mkdir(parents=True, exist_ok=True)
    if not config_path.exists():
        template = resources / "default-config.yaml"
        if not template.is_file():
            raise FileNotFoundError(f"Default configuration template not found: {template}")
        data = _load_yaml(template)
        api = data.setdefault("api", {})
        skills = data.setdefault("skills", {})
        api["data_dir"] = "../data"
        api["deerflow_home"] = "../data/deerflow"
        api["extensions_config_path"] = "./extensions_config.json"
        skills["path"] = "../skills"
        skills["extensions_file"] = "./extensions_config.json"
        _atomic_write_yaml(config_path, data)
    data = _load_yaml(config_path)
    extensions = _extensions_path(config_path, data)
    if not extensions.exists():
        _atomic_write_json(extensions, {"skills": {}})
    skills_path = _skills_path(config_path, user_data, data)
    skills_path.mkdir(parents=True, exist_ok=True)
    _copy_initial_skills(resources, skills_path)
    _deerflow_home(config_path, user_data, data).joinpath("agents").mkdir(parents=True, exist_ok=True)


def _redacted_model(raw: dict[str, Any]) -> dict[str, Any]:
    known = {
        "name",
        "display_name",
        "description",
        "use",
        "model",
        "api_key",
        "base_url",
        "supports_thinking",
        "supports_reasoning_effort",
        "supports_vision",
    }
    api_key = raw.get("api_key")
    env_reference = api_key if isinstance(api_key, str) and _ENV_REFERENCE.fullmatch(api_key.strip()) else ""
    return {
        "original_name": str(raw.get("name") or ""),
        "name": str(raw.get("name") or ""),
        "display_name": str(raw.get("display_name") or ""),
        "description": str(raw.get("description") or ""),
        "use_path": str(raw.get("use") or ""),
        "model": str(raw.get("model") or ""),
        "api_key": env_reference,
        "api_key_configured": api_key not in (None, ""),
        "api_key_literal": bool(api_key not in (None, "") and not env_reference),
        "clear_api_key": False,
        "base_url": str(raw.get("base_url") or ""),
        "supports_thinking": bool(raw.get("supports_thinking", False)),
        "supports_reasoning_effort": bool(raw.get("supports_reasoning_effort", False)),
        "supports_vision": bool(raw.get("supports_vision", False)),
        "advanced": {key: deepcopy(value) for key, value in raw.items() if key not in known},
    }


def _is_sensitive_key(key: str) -> bool:
    normalized = key.strip().lower().replace("-", "_")
    exact = {
        "api_key",
        "access_key",
        "secret_key",
        "private_key",
        "app_secret",
        "client_secret",
        "password",
        "token",
        "access_token",
        "refresh_token",
        "verification_token",
        "credential",
        "credentials",
    }
    return normalized in exact or normalized.endswith(
        ("_api_key", "_access_key", "_secret", "_password", "_token")
    )


def _redact_sensitive(value: Any, key: str = "") -> Any:
    if _is_sensitive_key(key) and value not in (None, ""):
        if not (isinstance(value, str) and _ENV_REFERENCE.fullmatch(value.strip())):
            return _REDACTED_VALUE
    if isinstance(value, dict):
        return {
            str(item_key): _redact_sensitive(item_value, str(item_key))
            for item_key, item_value in value.items()
        }
    if isinstance(value, list):
        return [_redact_sensitive(item) for item in value]
    return deepcopy(value)


def _restore_redacted(incoming: Any, existing: Any) -> Any:
    if incoming == _REDACTED_VALUE:
        return deepcopy(existing) if existing is not None else _REDACTED_VALUE
    if isinstance(incoming, dict):
        previous = existing if isinstance(existing, dict) else {}
        return {
            key: _restore_redacted(value, previous.get(key))
            for key, value in incoming.items()
        }
    if isinstance(incoming, list):
        previous = existing if isinstance(existing, list) else []
        return [
            _restore_redacted(value, previous[index] if index < len(previous) else None)
            for index, value in enumerate(incoming)
        ]
    return deepcopy(incoming)


def _contains_redacted(value: Any) -> bool:
    if value == _REDACTED_VALUE:
        return True
    if isinstance(value, dict):
        return any(_contains_redacted(item) for item in value.values())
    if isinstance(value, list):
        return any(_contains_redacted(item) for item in value)
    return False


def _restore_named_items(
    incoming: list[dict[str, Any]], existing: list[dict[str, Any]]
) -> list[dict[str, Any]]:
    existing_by_name = {
        str(item.get("name")): item for item in existing if isinstance(item, dict)
    }
    return [
        _restore_redacted(item, existing_by_name.get(str(item.get("name"))))
        for item in incoming
    ]


def _runtime_document(data: dict[str, Any]) -> dict[str, Any]:
    local = data.get("local_acp") if isinstance(data.get("local_acp"), dict) else {}
    api = data.get("api") if isinstance(data.get("api"), dict) else {}
    return RuntimeDocument(
        model_name=local.get("model_name"),
        agent_name=local.get("agent_name"),
        thinking_enabled=local.get("thinking_enabled", api.get("thinking_enabled", True)),
        plan_mode=local.get("plan_mode", api.get("plan_mode", True)),
        subagent_enabled=local.get("subagent_enabled", False),
        max_concurrent_subagents=local.get("max_concurrent_subagents", 2),
        max_active_connections=local.get("max_active_connections", 16),
        max_active_runs=local.get("max_active_runs", 2),
        run_timeout_seconds=local.get("run_timeout_seconds", 600),
        queue_timeout_seconds=local.get("queue_timeout_seconds", 600),
        session_cleanup_enabled=local.get("session_cleanup_enabled", True),
        inactive_session_retention_days=local.get("inactive_session_retention_days", 30),
        closed_session_retention_days=local.get("closed_session_retention_days", 30),
        session_cleanup_interval_seconds=local.get("session_cleanup_interval_seconds", 3600),
        goal_auto_continue=local.get("goal_auto_continue", False),
        goal_max_continuations=local.get("goal_max_continuations", 3),
        goal_max_no_progress_continuations=local.get(
            "goal_max_no_progress_continuations",
            2,
        ),
        permission_mode="off" if local.get("permission_mode") is False else local.get("permission_mode", "dangerous"),
        memory_scope=local.get("memory_scope", "workspace"),
        enable_bash=local.get("enable_bash", False),
        tool_allowlist=local.get("tool_allowlist"),
        tool_denylist=local.get("tool_denylist", []),
        prompt_overlay=local.get("prompt_overlay", ""),
    ).model_dump()


_MEMORY_BACKEND_FIELDS = {
    "storage_path",
    "storage_class",
    "debounce_seconds",
    "model_name",
    "max_facts",
    "fact_confidence_threshold",
    "max_injection_tokens",
    "retrieval_enabled",
    "retrieval_top_k",
    "retrieval_index_path",
}
_MEMORY_TOP_LEVEL_FIELDS = {
    "enabled",
    "manager_class",
    "mode",
    "injection_enabled",
    "shutdown_flush_timeout_seconds",
    "backend_config",
    *_MEMORY_BACKEND_FIELDS,
}


def _memory_document(data: dict[str, Any]) -> dict[str, Any]:
    raw: dict[str, Any] = (
        data.get("memory") if isinstance(data.get("memory"), dict) else {}
    )
    config = MemoryConfig.model_validate(raw)
    if config.manager_class != "deermem":
        raise ValueError(
            "Portable deerflow-config currently supports only local DeerMem; "
            f"found memory.manager_class={config.manager_class!r}"
        )
    backend = config.backend_config
    return MemoryDocument(
        enabled=config.enabled,
        manager_class=config.manager_class,
        mode=config.mode,
        injection_enabled=config.injection_enabled,
        shutdown_flush_timeout_seconds=config.shutdown_flush_timeout_seconds,
        storage_path=config.storage_path,
        storage_class=config.storage_class,
        debounce_seconds=config.debounce_seconds,
        model_name=config.model_name,
        max_facts=config.max_facts,
        fact_confidence_threshold=config.fact_confidence_threshold,
        max_injection_tokens=config.max_injection_tokens,
        retrieval_enabled=config.retrieval_enabled,
        retrieval_top_k=config.retrieval_top_k,
        retrieval_index_path=config.retrieval_index_path,
        advanced={
            key: deepcopy(value)
            for key, value in raw.items()
            if key not in _MEMORY_TOP_LEVEL_FIELDS
        },
        backend_advanced={
            key: deepcopy(value)
            for key, value in backend.items()
            if key not in _MEMORY_BACKEND_FIELDS
        },
    ).model_dump()


def _memory_data_path(base_dir: Path, configured: str, default_name: str) -> Path:
    path = Path(configured or default_name).expanduser()
    return (path if path.is_absolute() else base_dir / path).resolve()


def _sandbox_document(data: dict[str, Any]) -> dict[str, Any]:
    raw = data.get("sandbox")
    raw = raw if isinstance(raw, dict) else {}
    validated = SandboxConfig.model_validate(raw)
    advanced = {
        key: deepcopy(value)
        for key, value in raw.items()
        if key not in {"use", "allow_host_bash", "allow_host_tools"}
    }
    return {
        "use": validated.use,
        "allow_host_bash": validated.allow_host_bash,
        "allow_host_tools": validated.allow_host_tools,
        "advanced": _redact_sensitive(advanced),
    }


def _skill_evolution_document(data: dict[str, Any]) -> dict[str, Any]:
    raw = data.get("skill_evolution")
    raw = raw if isinstance(raw, dict) else {}
    validated = SkillEvolutionConfig.model_validate(raw)
    return {**validated.model_dump(), "advanced": deepcopy(raw)}


def _subagents_document(data: dict[str, Any]) -> dict[str, Any]:
    raw = data.get("subagents")
    raw = raw if isinstance(raw, dict) else {}
    validated = SubagentsAppConfig.model_validate(raw)
    return {
        **validated.model_dump(),
        "builtin_agents": [
            {
                "name": name,
                "description": builtin.description,
                "default_model": builtin.model,
            }
            for name, builtin in BUILTIN_SUBAGENTS.items()
        ],
        "advanced": deepcopy(raw),
    }


def _deep_update(target: dict[str, Any], updates: dict[str, Any]) -> None:
    """Recursively apply known settings while retaining unexposed fields."""

    for key, value in updates.items():
        if isinstance(value, dict) and isinstance(target.get(key), dict):
            _deep_update(target[key], value)
        else:
            target[key] = deepcopy(value)


def _agent_documents(agents_dir: Path) -> list[dict[str, Any]]:
    output: list[dict[str, Any]] = []
    for directory in sorted(agents_dir.iterdir()) if agents_dir.exists() else []:
        config_file = directory / "config.yaml"
        if not directory.is_dir() or not config_file.is_file():
            continue
        try:
            raw = _load_yaml(config_file)
            raw.setdefault("name", directory.name)
            agent = AgentConfig.model_validate(raw)
            soul_file = directory / "SOUL.md"
            output.append(
                {
                    "original_name": directory.name,
                    **agent.model_dump(),
                    "tool_groups": agent.tool_groups or [],
                    "skills": agent.skills,
                    "soul": soul_file.read_text(encoding="utf-8") if soul_file.is_file() else "",
                }
            )
        except Exception as exc:
            output.append(
                {
                    "original_name": directory.name,
                    "name": directory.name,
                    "description": f"Invalid agent config: {exc}",
                    "model": None,
                    "tool_groups": [],
                    "skills": None,
                    "soul": "",
                    "invalid": True,
                }
            )
    return output


def _skill_documents(skills_path: Path, extensions: dict[str, Any]) -> list[dict[str, Any]]:
    states = extensions.get("skills") if isinstance(extensions.get("skills"), dict) else {}
    output: list[dict[str, Any]] = []
    for category in ("public", "custom"):
        root = skills_path / category
        if not root.is_dir():
            continue
        for skill_file in sorted(root.rglob("SKILL.md")):
            skill = parse_skill_file(skill_file, category, skill_file.parent.relative_to(root))
            if skill is None:
                continue
            state = states.get(skill.name) if isinstance(states.get(skill.name), dict) else {}
            output.append(
                {
                    "name": skill.name,
                    "description": skill.description,
                    "category": skill.category,
                    "enabled": bool(state.get("enabled", True)),
                    "path": str(skill.skill_dir),
                }
            )
    return output


def snapshot(config_path: Path, user_data: Path) -> dict[str, Any]:
    data = _load_yaml(config_path)
    extensions_path = _extensions_path(config_path, data)
    extensions = _load_json_object(extensions_path)
    skills_path = _skills_path(config_path, user_data, data)
    agents_dir = _deerflow_home(config_path, user_data, data) / "agents"
    memory = _memory_document(data)
    deerflow_home = _deerflow_home(config_path, user_data, data)
    raw_models = data.get("models") if isinstance(data.get("models"), list) else []
    return {
        "config_revision": _sha256(config_path),
        "extensions_revision": _sha256(extensions_path),
        "default_model": str(data.get("default_model") or (raw_models[0].get("name") if raw_models else "")),
        "models": [_redacted_model(model) for model in raw_models if isinstance(model, dict)],
        "runtime": _runtime_document(data),
        "memory": memory,
        "agents": _agent_documents(agents_dir),
        "subagents": _subagents_document(data),
        "sandbox": _sandbox_document(data),
        "tool_groups": _redact_sensitive(
            data.get("tool_groups") if isinstance(data.get("tool_groups"), list) else []
        ),
        "tools": _redact_sensitive(
            data.get("tools") if isinstance(data.get("tools"), list) else []
        ),
        "skills_enabled": bool((data.get("skills") or {}).get("enabled", True)),
        "skills": _skill_documents(skills_path, extensions),
        "skill_evolution": _skill_evolution_document(data),
        "paths": {
            "config": str(config_path),
            "extensions": str(extensions_path),
            "skills": str(skills_path),
            "agents": str(agents_dir),
            "user_data": str(user_data),
            "memory": str(
                _memory_data_path(
                    deerflow_home, str(memory["storage_path"]), "memory.json"
                )
            ),
            "memory_index": str(
                _memory_data_path(
                    deerflow_home,
                    str(memory["retrieval_index_path"]),
                    "memory-fts5.sqlite3",
                )
            ),
        },
    }


def _validated_models(incoming: list[dict[str, Any]], existing: list[dict[str, Any]]) -> list[dict[str, Any]]:
    existing_by_name = {str(item.get("name")): item for item in existing if isinstance(item, dict)}
    output: list[dict[str, Any]] = []
    names: set[str] = set()
    for item in incoming:
        original = str(item.get("original_name") or "")
        previous = existing_by_name.get(original, {})
        advanced = item.get("advanced") or {}
        if not isinstance(advanced, dict):
            raise ValueError(f"Model {item.get('name')!r} advanced settings must be an object")
        raw = deepcopy(advanced)
        raw.update(
            {
                "name": str(item.get("name") or "").strip(),
                "use": str(item.get("use_path") or "").strip(),
                "model": str(item.get("model") or "").strip(),
                "supports_thinking": bool(item.get("supports_thinking", False)),
                "supports_reasoning_effort": bool(item.get("supports_reasoning_effort", False)),
                "supports_vision": bool(item.get("supports_vision", False)),
            }
        )
        for key in ("display_name", "description", "base_url"):
            value = str(item.get(key) or "").strip()
            if value:
                raw[key] = value
        api_key = str(item.get("api_key") or "").strip()
        if item.get("clear_api_key"):
            raw.pop("api_key", None)
        elif api_key:
            raw["api_key"] = api_key
        elif "api_key" in previous:
            raw["api_key"] = previous["api_key"]
        model = ModelConfig.model_validate(raw)
        if not model.name or model.name in names:
            raise ValueError(f"Model names must be non-empty and unique: {model.name!r}")
        names.add(model.name)
        output.append(model.model_dump(exclude_none=True))
    if not output:
        raise ValueError("At least one model is required")
    return output


def _validated_agents(incoming: list[dict[str, Any]], model_names: set[str]) -> list[tuple[str, str, dict[str, Any], str]]:
    output: list[tuple[str, str, dict[str, Any], str]] = []
    names: set[str] = set()
    for item in incoming:
        original = str(item.get("original_name") or "")
        name = validate_agent_name(str(item.get("name") or "").strip())
        if name is None or name.lower() in names:
            raise ValueError(f"Agent names must be non-empty and unique: {name!r}")
        names.add(name.lower())
        model_name = str(item.get("model") or "").strip() or None
        if model_name and model_name not in model_names:
            raise ValueError(f"Agent {name!r} references unknown model {model_name!r}")
        agent = AgentConfig(
            name=name,
            description=str(item.get("description") or ""),
            model=model_name,
            tool_groups=[str(value) for value in item.get("tool_groups") or []] or None,
            skills=(None if item.get("skills") is None else [str(value) for value in item.get("skills") or []]),
        )
        output.append((original, name, agent.model_dump(exclude_none=True), str(item.get("soul") or "")))
    return output


def _backup_files(user_data: Path, paths: list[Path], agents_dir: Path) -> Path:
    stamp = datetime.now(UTC).strftime("%Y%m%d-%H%M%S-%f")
    backup = user_data / "backups" / stamp
    backup.mkdir(parents=True, exist_ok=False)
    for path in paths:
        if path.is_file():
            shutil.copy2(path, backup / path.name)
    if agents_dir.is_dir():
        shutil.copytree(agents_dir, backup / "agents")
    return backup


@contextmanager
def _save_lock(user_data: Path):
    """Serialize desktop writers before checking optimistic revisions."""
    user_data.mkdir(parents=True, exist_ok=True)
    with (user_data / ".config-tool.lock").open("a+b") as stream:
        stream.seek(0, 2)
        if stream.tell() == 0:
            stream.write(b"0")
            stream.flush()
        stream.seek(0)
        if os.name == "nt":
            import msvcrt
            msvcrt.locking(stream.fileno(), msvcrt.LK_LOCK, 1)
        else:
            import fcntl
            fcntl.flock(stream.fileno(), fcntl.LOCK_EX)
        try:
            yield
        finally:
            stream.seek(0)
            if os.name == "nt":
                msvcrt.locking(stream.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                fcntl.flock(stream.fileno(), fcntl.LOCK_UN)


def save(config_path: Path, user_data: Path, document: SaveDocument, *, validate_only: bool = False) -> dict[str, Any]:
    with _save_lock(user_data):
        return _save_locked(config_path, user_data, document, validate_only=validate_only)


def _save_locked(config_path: Path, user_data: Path, document: SaveDocument, *, validate_only: bool = False) -> dict[str, Any]:
    data = _load_yaml(config_path)
    extensions_path = _extensions_path(config_path, data)
    if _sha256(config_path) != document.config_revision or _sha256(extensions_path) != document.extensions_revision:
        raise RuntimeError("Configuration changed after it was loaded; reload it before saving")

    existing_models = data.get("models") if isinstance(data.get("models"), list) else []
    models = _validated_models(document.models, existing_models)
    model_names = {str(model["name"]) for model in models}
    if document.default_model not in model_names:
        raise ValueError("The default model must reference a configured model")

    existing_sandbox = data.get("sandbox") if isinstance(data.get("sandbox"), dict) else {}
    sandbox = _restore_redacted(
        {
            **deepcopy(document.sandbox.advanced),
            "use": document.sandbox.use.strip(),
            "allow_host_bash": document.sandbox.allow_host_bash,
            "allow_host_tools": document.sandbox.allow_host_tools,
        },
        existing_sandbox,
    )
    if not sandbox.get("use") or _contains_redacted(sandbox):
        raise ValueError("Sandbox settings contain an invalid provider or redacted value")
    SandboxConfig.model_validate(sandbox)

    existing_groups = data.get("tool_groups") if isinstance(data.get("tool_groups"), list) else []
    existing_tools = data.get("tools") if isinstance(data.get("tools"), list) else []
    tool_groups = _restore_named_items(document.tool_groups, existing_groups)
    tools = _restore_named_items(document.tools, existing_tools)
    if _contains_redacted(tool_groups) or _contains_redacted(tools):
        raise ValueError("Tool settings contain a redacted value without an existing secret")
    validated_groups = [ToolGroupConfig.model_validate(item) for item in tool_groups]
    group_names = [group.name.strip() for group in validated_groups]
    if any(not name for name in group_names) or len(set(group_names)) != len(group_names):
        raise ValueError("Tool group names must be non-empty and unique")
    validated_tools = [ToolConfig.model_validate(item) for item in tools]
    for tool in validated_tools:
        if not re.fullmatch(r"[A-Za-z_][\w.]*:[A-Za-z_][\w]*", tool.use.strip()):
            raise ValueError(f"Tool {tool.name!r}: implementation must be module:function")
    tool_names = [tool.name.strip() for tool in validated_tools]
    if any(not name for name in tool_names) or len(set(tool_names)) != len(tool_names):
        raise ValueError("Tool names must be non-empty and unique")
    missing_groups = sorted(
        {tool.group for tool in validated_tools if tool.group not in set(group_names)}
    )
    if missing_groups:
        raise ValueError(
            f"Tools reference unknown tool groups: {', '.join(missing_groups)}"
        )

    runtime = document.runtime
    if runtime.model_name and runtime.model_name not in model_names:
        raise ValueError("The ACP model must reference a configured model")

    agents_dir = _deerflow_home(config_path, user_data, data) / "agents"
    agents = _validated_agents(document.agents, model_names)
    agent_names = {name for _, name, _, _ in agents}
    if runtime.agent_name and runtime.agent_name not in agent_names:
        raise ValueError("The ACP Agent must reference a configured custom Agent")
    memory_document = document.memory
    if memory_document.model_name and memory_document.model_name not in model_names:
        raise ValueError("The memory extraction model must reference a configured model")
    existing_memory: dict[str, Any] = (
        data.get("memory") if isinstance(data.get("memory"), dict) else {}
    )
    existing_backend: dict[str, Any] = (
        existing_memory.get("backend_config")
        if isinstance(existing_memory.get("backend_config"), dict)
        else {}
    )
    memory_backend = _restore_redacted(
        {
            **deepcopy(memory_document.backend_advanced),
            "storage_path": memory_document.storage_path.strip(),
            "storage_class": memory_document.storage_class.strip(),
            "debounce_seconds": memory_document.debounce_seconds,
            "model_name": memory_document.model_name,
            "max_facts": memory_document.max_facts,
            "fact_confidence_threshold": memory_document.fact_confidence_threshold,
            "max_injection_tokens": memory_document.max_injection_tokens,
            "retrieval_enabled": memory_document.retrieval_enabled,
            "retrieval_top_k": memory_document.retrieval_top_k,
            "retrieval_index_path": memory_document.retrieval_index_path.strip(),
        },
        existing_backend,
    )
    memory = _restore_redacted(
        {
            **deepcopy(memory_document.advanced),
            "enabled": memory_document.enabled,
            "manager_class": "deermem",
            "mode": memory_document.mode,
            "injection_enabled": memory_document.injection_enabled,
            "shutdown_flush_timeout_seconds": memory_document.shutdown_flush_timeout_seconds,
            "backend_config": memory_backend,
        },
        existing_memory,
    )
    if _contains_redacted(memory):
        raise ValueError("Memory settings contain a redacted value without an existing secret")
    memory_config = MemoryConfig.model_validate(memory)
    from deerflow.agents.memory import validate_memory_manager_config

    validate_memory_manager_config(memory_config)
    subagents = deepcopy(document.subagents.advanced)
    subagents_settings = document.subagents.model_dump(
        exclude={"advanced", "builtin_agents"}
    )
    # These two dictionaries are user-managed collections, so deletion in the
    # editor must replace the previous collection instead of deep-merging it.
    subagents["agents"] = deepcopy(subagents_settings.pop("agents"))
    subagents["custom_agents"] = deepcopy(
        subagents_settings.pop("custom_agents")
    )
    _deep_update(subagents, subagents_settings)
    validated_subagents = SubagentsAppConfig.model_validate(subagents)
    for name, override in validated_subagents.agents.items():
        if override.model and override.model != "inherit" and override.model not in model_names:
            raise ValueError(
                f"Subagent override {name!r} must reference a configured model"
            )
    for name, custom in validated_subagents.custom_agents.items():
        if custom.model and custom.model != "inherit" and custom.model not in model_names:
            raise ValueError(
                f"Custom Subagent {name!r} must reference a configured model"
            )
    evolution = deepcopy(document.skill_evolution.advanced)
    _deep_update(
        evolution,
        document.skill_evolution.model_dump(exclude={"advanced"}),
    )
    SkillEvolutionConfig.model_validate(evolution)
    for field in (
        "generation_model_name",
        "moderation_model_name",
        "evaluation_model_name",
    ):
        model_name = evolution.get(field)
        if model_name and model_name not in model_names:
            raise ValueError(
                f"Skill evolution {field} must reference a configured model"
            )
    existing_agent_names = {
        entry.name for entry in agents_dir.iterdir() if entry.is_dir()
    }
    for original, name, _, _ in agents:
        if not original and name in existing_agent_names:
            raise ValueError(f"Agent directory {name!r} already exists")
        if original and original != name and name in existing_agent_names:
            raise ValueError(
                f"Cannot rename Agent {original!r} to existing directory {name!r}"
            )

    candidate = deepcopy(data)
    candidate["models"] = models
    candidate["default_model"] = document.default_model
    candidate["sandbox"] = sandbox
    candidate["tool_groups"] = tool_groups
    candidate["tools"] = tools
    candidate["memory"] = memory
    local = candidate.setdefault("local_acp", {})
    # Older desktop clients must not reset fields they do not understand.
    for key, value in runtime.model_dump(exclude_unset=True).items():
        local[key] = value
    skills_config = candidate.setdefault("skills", {})
    skills_config["enabled"] = document.skills_enabled
    candidate["subagents"] = subagents
    candidate["skill_evolution"] = evolution

    # Validate the complete selected sections before any file is replaced.
    RuntimeDocument.model_validate(local)
    for model in models:
        ModelConfig.model_validate(model)

    extensions = _load_json_object(extensions_path)
    skill_states = extensions.get("skills") if isinstance(extensions.get("skills"), dict) else {}
    known_skill_names = {str(item.get("name")) for item in document.skills}
    for item in document.skills:
        name = str(item.get("name") or "").strip()
        if not name:
            raise ValueError("Skill name cannot be empty")
        previous = skill_states.get(name) if isinstance(skill_states.get(name), dict) else {}
        skill_states[name] = {**previous, "enabled": bool(item.get("enabled", True))}
    # Preserve unknown states as well as all mcpServers/middlewares/extra fields.
    extensions["skills"] = {**{k: v for k, v in skill_states.items() if k not in known_skill_names}, **skill_states}

    if validate_only:
        return {"valid": True}
    backup = _backup_files(user_data, [config_path, extensions_path], agents_dir)
    originals = {path: path.read_text(encoding="utf-8") if path.is_file() else None for path in (config_path, extensions_path)}
    try:
        _atomic_write_yaml(config_path, candidate)
        _atomic_write_json(extensions_path, extensions)

        existing_dirs = {entry.name: entry for entry in agents_dir.iterdir() if entry.is_dir()}
        incoming_originals = {original for original, _, _, _ in agents if original}
        for original, name, agent_data, soul in agents:
            target = agents_dir / name
            if original and original != name and original in existing_dirs:
                os.replace(existing_dirs[original], target)
            target.mkdir(parents=True, exist_ok=True)
            _atomic_write_yaml(target / "config.yaml", agent_data)
            soul_path = target / "SOUL.md"
            if soul.strip():
                _atomic_write_text(soul_path, soul.rstrip() + "\n")
            else:
                soul_path.unlink(missing_ok=True)
        for existing_name, existing_path in existing_dirs.items():
            if existing_name not in incoming_originals:
                archived = backup / "removed-agents" / existing_name
                archived.parent.mkdir(parents=True, exist_ok=True)
                if existing_path.exists():
                    os.replace(existing_path, archived)

    except Exception as error:
        try:
            for path, content in originals.items():
                if content is None:
                    path.unlink(missing_ok=True)
                else:
                    _atomic_write_text(path, content)
            if agents_dir.exists():
                os.replace(agents_dir, backup / "failed-save-agents")
            if (backup / "agents").exists():
                shutil.copytree(backup / "agents", agents_dir)
            else:
                agents_dir.mkdir(parents=True, exist_ok=True)
        except Exception as rollback_error:
            raise RuntimeError(f"保存失败，自动恢复也失败；恢复备份位于 {backup}: {rollback_error}") from error
        raise RuntimeError(f"保存失败，已恢复保存前配置：{error}") from error

    result = snapshot(config_path, user_data)
    result["backup"] = str(backup)
    return result


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="DeerFlow portable configuration JSON service")
    parser.add_argument("--config", required=True, type=Path)
    parser.add_argument("--user-data", required=True, type=Path)
    parser.add_argument("--resources", required=True, type=Path)
    parser.add_argument("command", choices=("init", "snapshot", "save", "validate", "inspect", "restore", "test-model"))
    return parser


def main() -> None:
    args = _parser().parse_args()
    try:
        config_path = args.config.expanduser().resolve()
        user_data = args.user_data.expanduser().resolve()
        resources = args.resources.expanduser().resolve()
        ensure_layout(config_path, user_data, resources)
        if args.command == "init":
            payload: dict[str, Any] = {"initialized": True, "paths": {"config": str(config_path), "user_data": str(user_data)}}
        elif args.command == "snapshot":
            payload = snapshot(config_path, user_data)
        elif args.command in {"save", "validate"}:
            raw = json.load(sys.stdin)
            payload = save(config_path, user_data, SaveDocument.model_validate(raw), validate_only=args.command == "validate")
        else:
            from deerflow.portable_management import dispatch
            raw = json.load(sys.stdin)
            payload = dispatch(args.command, config_path, user_data, raw)
        print(json.dumps({"ok": True, "data": payload}, ensure_ascii=False))
    except (OSError, ValueError, RuntimeError, ValidationError, json.JSONDecodeError) as exc:
        error = (
            json.dumps(exc.errors(include_input=False), ensure_ascii=False)
            if isinstance(exc, ValidationError)
            else str(exc)
        )
        print(json.dumps({"ok": False, "error": error}, ensure_ascii=False))
        raise SystemExit(1) from exc


if __name__ == "__main__":
    main()
