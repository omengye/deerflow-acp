import asyncio
from pathlib import Path

from langchain_core.tools import StructuredTool

from deerflow.config.app_config import AppConfig, reset_app_config, set_app_config
from deerflow.config.sandbox_config import SandboxConfig
from deerflow.sandbox.local.local_sandbox_provider import LocalSandboxProvider
from deerflow.skills.security_scanner import _extract_json_object
from deerflow.tools.builtins.tool_search import DeferredToolRegistry, get_deferred_registry, reset_deferred_registry, set_deferred_registry
from deerflow.tools.sync import make_sync_tool_wrapper


def test_security_scanner_extracts_fenced_and_balanced_json() -> None:
    assert _extract_json_object('```json\n{"decision":"allow","reason":"ok"}\n```') == {
        "decision": "allow",
        "reason": "ok",
    }
    assert _extract_json_object('prefix {"decision":"warn","reason":"brace } in string"} suffix') == {
        "decision": "warn",
        "reason": "brace } in string",
    }


def test_local_sandbox_provider_scopes_user_data_by_thread(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setenv("DEER_FLOW_HOME", str(tmp_path))
    skills_root = tmp_path / "skills"
    skills_root.mkdir()
    config = AppConfig(sandbox=SandboxConfig(use="test"))
    config.skills.path = str(skills_root)
    set_app_config(config)
    try:
        provider = LocalSandboxProvider()
        first_id = provider.acquire("thread-1")
        second_id = provider.acquire("thread-2")

        assert first_id.startswith("local:thread-1:")
        assert second_id.startswith("local:thread-2:")

        first = provider.get(first_id)
        second = provider.get(second_id)
        assert first is not None
        assert second is not None
        assert first is provider.get(first_id)
        assert first is not second

        first_workspace = first._resolve_path("/mnt/user-data/workspace/report.txt")
        second_workspace = second._resolve_path("/mnt/user-data/workspace/report.txt")

        assert str(tmp_path / "threads" / "thread-1" / "user-data" / "workspace") in first_workspace
        assert str(tmp_path / "threads" / "thread-2" / "user-data" / "workspace") in second_workspace
        assert first_workspace != second_workspace
    finally:
        reset_app_config()


def test_sync_tool_wrapper_runs_inside_existing_event_loop() -> None:
    async def sample(value: int) -> int:
        return value + 1

    wrapped = make_sync_tool_wrapper(sample, "sample")

    async def invoke() -> int:
        return wrapped(41)

    assert asyncio.run(invoke()) == 42


def test_deferred_registry_promotions_can_be_preserved() -> None:
    reset_deferred_registry()

    def noop() -> str:
        return "ok"

    registry = DeferredToolRegistry()
    registry.register(StructuredTool.from_function(noop, name="fake_tool", description="fake"))
    set_deferred_registry(registry)

    registry.promote({"fake_tool"})
    assert get_deferred_registry() is registry
    assert len(registry) == 0

    reset_deferred_registry()
