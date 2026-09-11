"""Bounded desktop diagnostics, explicit model probes and configuration recovery."""

from __future__ import annotations

import asyncio
import os
import re
import time
from pathlib import Path
from typing import Any

from deerflow import config_tool as ct


def _backup(root: Path, name: str) -> Path:
    if not re.fullmatch(r"\d{8}-\d{6}-\d{6}", name):
        raise ValueError("Invalid backup identifier")
    path = (root / "backups" / name).resolve()
    if path.parent != (root / "backups").resolve() or not path.is_dir():
        raise ValueError("Backup not found")
    return path


def _size(path: Path) -> int:
    return (
        sum(
            p.stat().st_size
            for p in path.rglob("*")
            if p.is_file() and not p.is_symlink()
        )
        if path.exists()
        else 0
    )


def _redact_log(value: str, raw: dict[str, Any]) -> str:
    def secrets(node: Any, key: str = ""):
        if isinstance(node, dict):
            for k, v in node.items():
                yield from secrets(v, k)
        elif isinstance(node, list):
            for v in node:
                yield from secrets(v, key)
        elif isinstance(node, str) and ct._is_sensitive_key(key):
            if node.startswith("$"):
                from deerflow.config.app_config import AppConfig

                try:
                    node = str(AppConfig.resolve_env_variables(node))
                except (KeyError, ValueError):
                    return
            if node:
                yield node

    for secret in sorted(set(secrets(raw)), key=len, reverse=True):
        value = value.replace(secret, "[REDACTED]")
    value = re.sub(
        r"(?i)(bearer\s+|(?:api[_-]?key|token|authorization)[\"']?\s*[:=]\s*[\"']?)[^\s,\"'}]+",
        r"\1[REDACTED]",
        value,
    )
    return value


def inspect(config: Path, root: Path, request: dict[str, Any]) -> dict[str, Any]:
    raw = ct._load_yaml(config)
    backup_root = root / "backups"
    backups = (
        sorted(
            (
                p.name
                for p in backup_root.iterdir()
                if p.is_dir() and re.fullmatch(r"\d{8}-\d{6}-\d{6}", p.name)
            ),
            reverse=True,
        )
        if backup_root.exists()
        else []
    )
    report: dict[str, Any] = {
        "checks": {
            "config_exists": config.is_file(),
            "config_writable": os.access(config, os.W_OK),
            "model_count": len(raw.get("models", [])),
            "local_provider": (raw.get("sandbox") or {}).get("use"),
        },
        "storage_bytes": {
            name: _size(root / name)
            for name in ("data", "skills", "backups", "runtime")
        },
        "backups": backups,
    }
    log = root / "runtime" / "acp" / "daemon.log"
    if log.is_file():
        with log.open("rb") as stream:
            stream.seek(max(0, log.stat().st_size - 32768))
            report["log_tail"] = _redact_log(
                stream.read().decode("utf-8", errors="replace"), raw
            )
    if request.get("backup"):
        selected = _backup(root, str(request["backup"]))
        previous = ct._load_yaml(selected / config.name)
        report["preview"] = {
            "backup": selected.name,
            "changed_sections": sorted(
                k for k in set(raw) | set(previous) if raw.get(k) != previous.get(k)
            ),
            "models": [m.get("name") for m in previous.get("models", [])],
            "agents": [a["name"] for a in ct._agent_documents(selected / "agents")],
            "note": "恢复模型、权限、工具、记忆参数、Agent 和 Skill 设置；不会删除会话、记忆事实或产物。恢复前自动备份当前配置。",
        }
    return report


def restore(config: Path, root: Path, request: dict[str, Any]) -> dict[str, Any]:
    selected = _backup(root, str(request.get("backup", "")))
    previous_path = selected / config.name
    previous = ct._load_yaml(previous_path)
    # Reuse all validation and secret handling rather than blindly copying files.
    current = ct.snapshot(config, root)
    if request.get("revision") != current["config_revision"]:
        raise RuntimeError("配置已变化，请重新预览后恢复")
    restored = ct.snapshot(previous_path, root)
    restored["config_revision"] = current["config_revision"]
    restored["extensions_revision"] = current["extensions_revision"]
    existing_agents = {a["name"] for a in current["agents"]}
    restored["agents"] = ct._agent_documents(selected / "agents")
    for agent in restored["agents"]:
        agent["original_name"] = (
            agent["name"] if agent["name"] in existing_agents else ""
        )
    for model, raw_model in zip(
        restored["models"], previous.get("models", []), strict=True
    ):
        model["api_key"] = raw_model.get("api_key", "")
        model["clear_api_key"] = "api_key" not in raw_model
    restored["sandbox"]["advanced"] = previous.get("sandbox", {})
    restored["tools"] = previous.get("tools", [])
    restored["tool_groups"] = previous.get("tool_groups", [])
    # Skill locations in old backups are relative to the live config directory.
    old_extensions_path = selected / Path(current["paths"]["extensions"]).name
    old_extensions = ct._load_json_object(old_extensions_path)
    restored["skills"] = current["skills"]
    for skill in restored["skills"]:
        skill["enabled"] = (
            old_extensions.get("skills", {}).get(skill["name"], {}).get("enabled", True)
        )
    result = ct.save(config, root, ct.SaveDocument.model_validate(restored))
    return {"restored": selected.name, "backup": result["backup"]}


async def _test_model(config: Path, request: dict[str, Any]) -> dict[str, Any]:
    from langchain_core.messages import HumanMessage

    from deerflow.config.app_config import AppConfig, set_app_config
    from deerflow.models import aclose_chat_model, create_chat_model

    raw = ct._load_yaml(config)
    incoming = request.get("model")
    if not isinstance(incoming, dict):
        raise ValueError("Missing model configuration")  # noqa: TRY004 -- JSON service validation error
    validated = ct._validated_models([incoming], raw.get("models", []))[0]
    # Use the same environment resolver as normal startup.
    raw["models"] = [AppConfig.resolve_env_variables(validated)]
    raw["default_model"] = validated["name"]
    set_app_config(AppConfig.model_validate(raw))
    model = None
    started = time.monotonic()
    try:
        async with asyncio.timeout(30):
            model = create_chat_model(
                validated["name"], thinking_enabled=False, max_tokens=16, max_retries=0
            )
            await model.ainvoke([HumanMessage(content="Reply OK.")])
        return {
            "ok": True,
            "latency_ms": round((time.monotonic() - started) * 1000),
            "note": "模型文本请求成功；能力声明仍需按服务商说明设置",
        }
    except Exception as exc:  # noqa: BLE001 -- provider errors are intentionally sanitized
        # Provider exceptions can echo request credentials and headers.
        return {
            "ok": False,
            "error_type": type(exc).__name__,
            "note": "请求失败，请检查凭据、地址、模型 ID 和网络；测试最长 30 秒",
        }
    finally:
        if model is not None:
            await aclose_chat_model(model)


def dispatch(
    command: str, config: Path, root: Path, request: dict[str, Any]
) -> dict[str, Any]:
    if command == "inspect":
        return inspect(config, root, request)
    if command == "restore":
        return restore(config, root, request)
    if command == "test-model":
        return asyncio.run(_test_model(config, request))
    raise ValueError("Unsupported portable operation")
