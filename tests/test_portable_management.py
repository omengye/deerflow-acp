from __future__ import annotations

import asyncio
from pathlib import Path
from types import SimpleNamespace

import pytest

from deerflow import config_tool as ct
from deerflow.acp.config import LocalACPConfig
from deerflow.acp.runtime import LocalACPRuntime
from deerflow.acp.session_coordinator import ACPSessionCoordinator, SessionBusyError
from deerflow.agents.memory.backends.deermem import memory_bucket
from deerflow.portable_management import _redact_log, inspect, restore


def layout(tmp_path: Path):
    root = tmp_path / "user-data"
    config = root / "config" / "config.yaml"
    ct.ensure_layout(config, root, Path(__file__).parents[1] / "resources")
    return config, root


def test_old_desktop_save_preserves_missing_goal_fields(tmp_path):
    config, root = layout(tmp_path)
    raw = ct._load_yaml(config)
    expected = {
        "goal_auto_continue": True,
        "goal_max_continuations": 7,
        "goal_max_no_progress_continuations": 5,
    }
    raw.setdefault("local_acp", {}).update(expected)
    ct._atomic_write_yaml(config, raw)
    doc = ct.snapshot(config, root)
    for key in expected:
        doc["runtime"].pop(key)
    ct.save(config, root, ct.SaveDocument.model_validate(doc))
    saved = ct._load_yaml(config)["local_acp"]
    assert {key: saved[key] for key in expected} == expected


def test_validate_never_creates_backup_or_writes(tmp_path):
    config, root = layout(tmp_path)
    before = config.read_bytes()
    doc = ct.SaveDocument.model_validate(ct.snapshot(config, root))
    assert ct.save(config, root, doc, validate_only=True) == {"valid": True}
    assert config.read_bytes() == before
    assert not list((root / "backups").iterdir())


def test_backup_preview_restore_and_revision_conflict(tmp_path):
    config, root = layout(tmp_path)
    raw = ct._load_yaml(config)
    raw["models"][0]["api_key"] = "backup-test-secret"
    ct._atomic_write_yaml(config, raw)
    doc = ct.snapshot(config, root)
    doc["models"][0]["display_name"] = "Modified"
    result = ct.save(config, root, ct.SaveDocument.model_validate(doc))
    name = Path(result["backup"]).name
    preview = inspect(config, root, {"backup": name})
    assert "models" in preview["preview"]["changed_sections"]
    assert "backup-test-secret" not in str(preview)
    with pytest.raises(RuntimeError, match="配置已变化"):
        restore(config, root, {"backup": name, "revision": "old"})
    restore(config, root, {"backup": name, "revision": ct._sha256(config)})
    saved = ct._load_yaml(config)
    assert saved["models"][0].get("display_name") != "Modified"
    assert saved["models"][0]["api_key"] == "backup-test-secret"
    assert len(list((root / "backups").iterdir())) == 2


def test_backup_path_traversal_rejected(tmp_path):
    config, root = layout(tmp_path)
    with pytest.raises(ValueError):
        inspect(config, root, {"backup": "../../config"})


def test_log_redaction(monkeypatch):
    monkeypatch.setenv("PORTABLE_TEST_SECRET", "resolved-secret")
    raw = {"api_key": "literal-secret", "token": "$PORTABLE_TEST_SECRET"}
    text = _redact_log(
        "literal-secret resolved-secret Bearer other-secret token=another", raw
    )
    for secret in ("literal-secret", "resolved-secret", "other-secret", "another"):
        assert secret not in text


@pytest.mark.asyncio
async def test_drain_blocks_new_prompts_until_resumed():
    c = ACPSessionCoordinator()
    c.attach("s", "client")
    c.set_draining(True)
    with pytest.raises(SessionBusyError):
        c.begin_prompt("s", "client", asyncio.current_task())
    assert c.activity()["active_operations"] == 0
    c.set_draining(False)
    c.begin_prompt("s", "client", asyncio.current_task())
    assert c.activity()["active_operations"] == 1
    c.set_draining(True)
    c.end_prompt("s", "client", asyncio.current_task())
    assert c.activity()["active_operations"] == 0


@pytest.mark.asyncio
async def test_cancelled_queue_releases_slot_and_reports_wait(tmp_path):
    runtime = LocalACPRuntime(
        LocalACPConfig(
            config_path=tmp_path / "c",
            checkpointer_path=tmp_path / "p",
            session_store_path=tmp_path / "s",
            max_active_runs=1,
        )
    )
    events = []

    async def callback(event):
        events.append(event)

    async with runtime.run_slot(callback):

        async def waiting():
            async with runtime.run_slot(callback):
                pytest.fail("cancelled waiter must not execute")

        waiter = asyncio.create_task(waiting())
        await asyncio.sleep(0.02)
        assert runtime.queued_runs == 1
        waiter.cancel()
        with pytest.raises(asyncio.CancelledError):
            await waiter
        assert runtime.active_runs == 1
    assert runtime.active_runs == runtime.queued_runs == 0
    assert any(e["type"] == "queue_status" for e in events)
    async with asyncio.timeout(0.2):
        async with runtime.run_slot(callback):
            assert runtime.active_runs == 1


def test_memory_bucket_isolation_preserves_legacy():
    assert memory_bucket("agent", None) == "agent"
    assert memory_bucket(None, None) is None
    a = memory_bucket("agent", "acp-workspace:a")
    assert a == memory_bucket("agent", "acp-workspace:a")
    assert a != memory_bucket("agent", "acp-workspace:b")
    assert a != memory_bucket("other", "acp-workspace:a")
    assert a != memory_bucket("agent", "acp-session:a")


def test_partial_save_rolls_back_config(tmp_path, monkeypatch):
    config, root = layout(tmp_path)
    before = config.read_bytes()
    doc = ct.snapshot(config, root)
    doc["models"][0]["display_name"] = "must not persist"

    def fail(*args, **kwargs):
        raise OSError("simulated disk failure")

    monkeypatch.setattr(ct, "_atomic_write_json", fail)
    with pytest.raises(RuntimeError, match="已恢复"):
        ct.save(config, root, ct.SaveDocument.model_validate(doc))
    assert config.read_bytes() == before


@pytest.mark.asyncio
async def test_model_probe_uses_unsaved_environment_key_without_saving(
    tmp_path, monkeypatch
):
    from deerflow.config.app_config import get_app_config, reset_app_config
    from deerflow.portable_management import _test_model

    config, root = layout(tmp_path)
    before = config.read_bytes()
    model = ct.snapshot(config, root)["models"][0]
    model["api_key"] = "$PORTABLE_MODEL_TEST_KEY"
    monkeypatch.setenv("PORTABLE_MODEL_TEST_KEY", "fake-probe-key")
    calls = []

    class FakeModel:
        async def ainvoke(self, messages):
            calls.append(messages)

    def create(name, **kwargs):
        assert get_app_config().get_model_config(name).api_key == "fake-probe-key"
        assert kwargs["max_tokens"] == 16
        return FakeModel()

    async def close(model):
        pass

    monkeypatch.setattr("deerflow.models.create_chat_model", create)
    monkeypatch.setattr("deerflow.models.aclose_chat_model", close)
    try:
        result = await _test_model(config, {"model": model})
        assert result["ok"]
        assert len(calls) == 1
        assert config.read_bytes() == before
    finally:
        reset_app_config()


@pytest.mark.asyncio
async def test_live_management_refuses_attached_session_deletion():
    from deerflow.acp.management import handle_runtime_management

    coordinator = ACPSessionCoordinator()
    coordinator.attach("s", "client")

    class Store:
        async def get(self, *args, **kwargs):
            return object()

    daemon = SimpleNamespace(
        store=Store(), runtime=SimpleNamespace(session_coordinator=coordinator)
    )
    result = await handle_runtime_management(
        daemon, {"operation": "session.delete", "session_id": "s"}
    )
    assert result["ok"] is False
    assert "客户端" in result["error"]
