import runpy
from pathlib import Path
from types import SimpleNamespace

import pytest

import deerflow.config.paths as paths_module
import deerflow.sandbox.sandbox_provider as sandbox_provider_module
import deerflow.skills.projection as projection_module
from deerflow.agents.middlewares.thread_data_middleware import ThreadDataMiddleware
from deerflow.config.app_config import AppConfig, reset_app_config, set_app_config
from deerflow.config.sandbox_config import SandboxConfig
from deerflow.config.skills_config import SkillsConfig
from deerflow.sandbox.local.local_sandbox import LocalSandbox
from deerflow.sandbox.local.local_sandbox_provider import LocalSandboxProvider
from deerflow.sandbox.tools import (
    ensure_sandbox_initialized,
    ensure_thread_directories_exist,
)
from deerflow.tools.builtins.present_file_tool import _normalize_presented_filepath


@pytest.fixture
def local_provider(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setenv("DEER_FLOW_HOME", str(tmp_path / "deerflow-state"))
    skills_dir = tmp_path / "skills"
    skills_dir.mkdir()
    set_app_config(AppConfig(
        sandbox=SandboxConfig(use="deerflow.sandbox.local:LocalSandboxProvider"),
        skills=SkillsConfig(path=str(skills_dir)),
    ))
    paths_module._paths = None
    projection_module._projection_cache.clear()
    projection_module._last_projection_gc = 0.0
    provider = None
    try:
        provider = LocalSandboxProvider()
        yield provider
    finally:
        if provider is not None:
            provider.reset()
        projection_module._projection_cache.clear()
        projection_module._last_projection_gc = 0.0
        paths_module._paths = None
        reset_app_config()


def test_thread_data_middleware_binds_acp_workspace_and_maps_outputs_below_it(
    tmp_path: Path,
) -> None:
    internal_root = tmp_path / "deerflow-state"
    project = tmp_path / "client-project"
    project.mkdir()
    middleware = ThreadDataMiddleware(base_dir=str(internal_root), lazy_init=True)
    runtime = SimpleNamespace(
        context={
            "thread_id": "acp-session",
            "workspace_path": str(project),
        }
    )

    result = middleware.before_agent({}, runtime)

    assert result is not None
    thread_data = result["thread_data"]
    assert thread_data["thread_id"] == "acp-session"
    assert thread_data["workspace_path"] == str(project)
    assert thread_data["workspace_path_managed"] is False
    assert thread_data["uploads_path"] == str(
        internal_root / "threads" / "acp-session" / "user-data" / "uploads"
    )
    assert thread_data["outputs_path"] == str(project / "outputs")


def test_thread_data_middleware_keeps_managed_outputs_for_non_acp_thread(
    tmp_path: Path,
) -> None:
    internal_root = tmp_path / "deerflow-state"
    middleware = ThreadDataMiddleware(base_dir=str(internal_root), lazy_init=True)
    runtime = SimpleNamespace(context={"thread_id": "web-session"})

    result = middleware.before_agent({}, runtime)

    assert result is not None
    thread_data = result["thread_data"]
    assert thread_data["workspace_path_managed"] is True
    assert thread_data["outputs_path"] == str(
        internal_root / "threads" / "web-session" / "user-data" / "outputs"
    )


def test_removed_acp_workspace_is_not_recreated(tmp_path: Path) -> None:
    internal_root = tmp_path / "deerflow-state"
    project = tmp_path / "client-project"
    project.mkdir()
    middleware = ThreadDataMiddleware(base_dir=str(internal_root), lazy_init=True)
    middleware_runtime = SimpleNamespace(
        context={"thread_id": "acp-session", "workspace_path": str(project)}
    )
    result = middleware.before_agent({}, middleware_runtime)
    assert result is not None
    project.rmdir()
    runtime = SimpleNamespace(
        context={"thread_id": "acp-session"},
        state={
            "sandbox": {"sandbox_id": "local"},
            "thread_data": result["thread_data"],
        },
    )

    ensure_thread_directories_exist(runtime)

    assert not project.exists()
    assert Path(result["thread_data"]["uploads_path"]).is_dir()
    assert not Path(result["thread_data"]["outputs_path"]).exists()


def test_local_provider_maps_virtual_workspace_to_acp_cwd(
    local_provider: LocalSandboxProvider,
    tmp_path: Path,
) -> None:
    project = tmp_path / "client-project"
    project.mkdir()

    sandbox_id = local_provider.acquire(
        "acp-session",
        workspace_path=str(project),
    )
    sandbox = local_provider.get(sandbox_id)

    assert isinstance(sandbox, LocalSandbox)
    assert Path(sandbox._resolve_path("/mnt/user-data/workspace/note.txt")) == (
        project / "note.txt"
    ).resolve()
    assert Path(sandbox._resolve_path("/mnt/user-data/outputs/report.txt")) == (
        project / "outputs" / "report.txt"
    ).resolve()

    sandbox.write_file("/mnt/user-data/workspace/note.txt", "written by tool")
    assert (project / "note.txt").read_text(encoding="utf-8") == "written by tool"

    script = "from pathlib import Path\nPath('/mnt/user-data/workspace/generated.txt').write_text('ok')\n"
    sandbox.write_file("/mnt/user-data/workspace/generate.py", script)
    stored_script = (project / "generate.py").read_text(encoding="utf-8")
    expected_target = (project / "generated.txt").resolve().as_posix()
    internal_target = (
        paths_module.get_paths().sandbox_work_dir("acp-session") / "generated.txt"
    ).resolve().as_posix()

    assert expected_target in stored_script
    assert internal_target not in stored_script
    runpy.run_path(str(project / "generate.py"))
    assert (project / "generated.txt").read_text(encoding="utf-8") == "ok"
    assert not Path(internal_target).exists()

    sandbox.write_file("/mnt/user-data/outputs/report.txt", "final output")
    assert (project / "outputs" / "report.txt").read_text(encoding="utf-8") == (
        "final output"
    )


def test_local_provider_cache_is_scoped_by_acp_workspace(
    local_provider: LocalSandboxProvider,
    tmp_path: Path,
) -> None:
    first_workspace = tmp_path / "first-project"
    second_workspace = tmp_path / "second-project"
    first_workspace.mkdir()
    second_workspace.mkdir()

    first_id = local_provider.acquire(
        "same-session",
        workspace_path=str(first_workspace),
    )
    second_id = local_provider.acquire(
        "same-session",
        workspace_path=str(second_workspace),
    )

    assert first_id != second_id
    first = local_provider.get(first_id)
    second = local_provider.get(second_id)
    assert isinstance(first, LocalSandbox)
    assert isinstance(second, LocalSandbox)
    assert Path(first._resolve_path("/mnt/user-data/workspace")) == first_workspace.resolve()
    assert Path(second._resolve_path("/mnt/user-data/workspace")) == second_workspace.resolve()


def test_lazy_sandbox_initialization_rebinds_checkpoint_to_acp_workspace(
    local_provider: LocalSandboxProvider,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    project = tmp_path / "client-project"
    project.mkdir()
    stale_id = local_provider.acquire("acp-session")
    monkeypatch.setattr(
        sandbox_provider_module,
        "_default_sandbox_provider",
        local_provider,
    )
    runtime = SimpleNamespace(
        context={"thread_id": "acp-session"},
        config={},
        state={
            "thread_data": {
                "thread_id": "acp-session",
                "workspace_path": str(project),
                "workspace_path_managed": False,
            },
            "sandbox": {
                "sandbox_id": stale_id,
                "skills_revision": stale_id.rsplit(":", 1)[-1],
            },
        },
    )

    sandbox = ensure_sandbox_initialized(runtime)

    assert isinstance(sandbox, LocalSandbox)
    assert runtime.state["sandbox"]["sandbox_id"] != stale_id
    assert runtime.state["sandbox"]["workspace_path"] == str(project.resolve())
    assert Path(sandbox._resolve_path("/mnt/user-data/workspace")) == project.resolve()


def test_local_provider_rejects_removed_acp_workspace_without_recreating_it(
    local_provider: LocalSandboxProvider,
    tmp_path: Path,
) -> None:
    project = tmp_path / "client-project"
    project.mkdir()
    local_provider.acquire("acp-session", workspace_path=str(project))
    project.rmdir()

    with pytest.raises(ValueError, match="does not exist"):
        local_provider.acquire("acp-session", workspace_path=str(project))

    assert not project.exists()


def test_present_files_resolves_acp_outputs_below_workspace(tmp_path: Path) -> None:
    project = tmp_path / "client-project"
    outputs = project / "outputs"
    outputs.mkdir(parents=True)
    report = outputs / "report.md"
    report.write_text("report", encoding="utf-8")
    runtime = SimpleNamespace(
        context={"thread_id": "acp-session"},
        config={},
        state={"thread_data": {"outputs_path": str(outputs)}},
    )

    assert _normalize_presented_filepath(
        runtime,
        "/mnt/user-data/outputs/report.md",
    ) == "/mnt/user-data/outputs/report.md"
    assert _normalize_presented_filepath(runtime, str(report)) == (
        "/mnt/user-data/outputs/report.md"
    )

    with pytest.raises(ValueError, match="traversal"):
        _normalize_presented_filepath(
            runtime,
            "/mnt/user-data/outputs/../outside.txt",
        )
