from __future__ import annotations

import re
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import pytest

from deerflow.sandbox.aio import AioSandbox, AioSandboxProvider
from deerflow.sandbox.command import CommandResult
from deerflow.sandbox.provider_paths import (
    AIO_SANDBOX_PROVIDER_PATH,
    normalize_sandbox_provider_path,
)


def _completed(stdout: str = "", stderr: str = "", returncode: int = 0):
    import subprocess

    return subprocess.CompletedProcess(args=["docker"], returncode=returncode, stdout=stdout, stderr=stderr)


def test_provider_path_aliases_normalize_aio() -> None:
    assert normalize_sandbox_provider_path("aio") == AIO_SANDBOX_PROVIDER_PATH
    assert normalize_sandbox_provider_path("docker") == AIO_SANDBOX_PROVIDER_PATH
    assert normalize_sandbox_provider_path("docker-sandbox") == AIO_SANDBOX_PROVIDER_PATH


def test_aio_sandbox_execute_uses_docker_exec() -> None:
    sandbox = AioSandbox("aio-test", "deer-flow-sandbox-aio-test")
    with (
        patch("deerflow.sandbox.aio.run_host_command", side_effect=[CommandResult("/bin/bash", 0), CommandResult("ok\n", 0)]) as run,
        patch("deerflow.sandbox.aio.linux_supervisor_args", return_value=(["supervised"], "token")) as supervisor,
    ):
        assert sandbox.execute_command("echo ok") == "ok\n"

    assert run.call_args_list[-1].args[0] == ["docker", "exec", "-i", "deer-flow-sandbox-aio-test", "supervised"]
    assert supervisor.call_args.args == ("/bin/bash", "echo ok", sandbox.timeout)
    assert run.call_args.kwargs["remote_token"] == "token"


@pytest.mark.parametrize("operation", ["glob", "grep"])
@pytest.mark.parametrize(
    ("returncode", "error_type"),
    [
        (2, FileNotFoundError),
        (3, NotADirectoryError),
        (4, PermissionError),
        (5, OSError),
        (127, OSError),
    ],
)
def test_remote_search_preserves_failure_type(operation, returncode, error_type) -> None:
    sandbox = AioSandbox("aio-test", "container")
    result = _completed(stderr="remote failure", returncode=returncode)
    with patch.object(sandbox, "_docker_exec", return_value=result):
        with pytest.raises(error_type):
            if operation == "glob":
                sandbox.glob("/mnt/user-data/workspace", "*.py")
            else:
                sandbox.grep("/mnt/user-data/workspace", "needle")


def test_remote_grep_reports_invalid_regex_separately() -> None:
    sandbox = AioSandbox("aio-test", "container")
    result = _completed(stderr="unterminated character set", returncode=6)
    with patch.object(sandbox, "_docker_exec", return_value=result):
        with pytest.raises(re.error, match="Invalid grep pattern"):
            sandbox.grep("/mnt/user-data/workspace", "[")


def test_remote_search_keeps_genuine_empty_results() -> None:
    sandbox = AioSandbox("aio-test", "container")
    with patch.object(
        sandbox,
        "_docker_exec",
        side_effect=[
            _completed(stdout='{"truncated": false, "matches": [], "coverage": {}}'),
            _completed(stdout='{"truncated": false, "matches": [], "coverage": {}}'),
        ],
    ):
        assert sandbox.glob("/mnt/user-data/workspace", "*.missing") == ([], False)
        assert sandbox.grep("/mnt/user-data/workspace", "missing") == ([], False)


def test_aio_provider_mounts_thread_data_and_skills(tmp_path, monkeypatch) -> None:
    base_dir = tmp_path / "state"
    skills_dir = tmp_path / "skills"
    skills_dir.mkdir()
    monkeypatch.setenv("DEER_FLOW_HOME", str(base_dir))
    import deerflow.config.paths as paths_mod

    paths_mod._paths = None

    config = SimpleNamespace(
        sandbox=SimpleNamespace(
            image="sandbox-image:test",
            replicas=2,
            container_prefix="df-test",
            idle_timeout=600,
            environment={"TOKEN": "$TOKEN_VALUE", "STATIC": "x"},
            mounts=[],
            security_opt=["seccomp:unconfined"],
        ),
        skills=SimpleNamespace(
            get_skills_path=lambda: skills_dir,
            container_path="/mnt/skills",
        ),
    )
    monkeypatch.setenv("TOKEN_VALUE", "secret")

    calls: list[list[str]] = []

    def fake_run(cmd, **kwargs):
        calls.append(cmd)
        if cmd[:3] == ["docker", "version", "--format"]:
            return _completed(stdout="25.0.0")
        if cmd[:2] == ["docker", "inspect"]:
            return _completed(stdout="true")
        return _completed(stdout="a" * 64)

    with patch("deerflow.config.get_app_config", return_value=config):
        with patch("subprocess.run", side_effect=fake_run):
            provider = AioSandboxProvider()
            sandbox_id = provider.acquire("thread_1")

    assert sandbox_id.startswith("aio-thread_1-")
    run_cmd = next(cmd for cmd in calls if cmd[:2] == ["docker", "run"])
    assert "--name" in run_cmd
    assert run_cmd[run_cmd.index("--name") + 1].startswith(f"df-test-{sandbox_id}-")
    assert f"{base_dir / 'threads' / 'thread_1' / 'user-data'}:/mnt/user-data:rw" in run_cmd
    assert f"{base_dir / 'threads' / 'thread_1' / 'acp-workspace'}:/mnt/acp-workspace:rw" in run_cmd
    assert any(mount.endswith(":/mnt/skills:ro") for mount in run_cmd)
    assert all(str(skills_dir) not in mount for mount in run_cmd if mount.endswith(":/mnt/skills:ro"))
    assert "--security-opt" in run_cmd
    assert "seccomp:unconfined" in run_cmd
    assert "TOKEN=secret" in run_cmd
    assert "STATIC=x" in run_cmd
    assert "--user" not in run_cmd


def test_aio_provider_auto_user_applies_only_to_exec(tmp_path, monkeypatch) -> None:
    monkeypatch.setenv("DEER_FLOW_HOME", str(tmp_path / "state"))
    monkeypatch.setattr("os.getuid", lambda: 1234, raising=False)
    monkeypatch.setattr("os.getgid", lambda: 5678, raising=False)
    import deerflow.config.paths as paths_mod

    paths_mod._paths = None
    config = SimpleNamespace(
        sandbox=SimpleNamespace(
            image="sandbox-image:test",
            replicas=1,
            container_prefix="df-test",
            idle_timeout=600,
            environment={},
            mounts=[],
            security_opt=[],
            container_user="auto",
        ),
        skills=SimpleNamespace(
            get_skills_path=lambda: Path("missing"),
            container_path="/mnt/skills",
        ),
    )
    calls: list[list[str]] = []

    def fake_run(cmd, **kwargs):
        calls.append(cmd)
        if cmd[:3] == ["docker", "version", "--format"]:
            return _completed(stdout="25.0.0")
        if cmd[:2] == ["docker", "inspect"]:
            return _completed(stdout="true")
        if cmd[:3] == ["docker", "exec", "-i"]:
            return _completed(stdout="ok\n")
        return _completed(stdout="a" * 64)

    with patch("deerflow.config.get_app_config", return_value=config):
        with (
            patch("subprocess.run", side_effect=fake_run),
            patch("deerflow.sandbox.aio.run_host_command", side_effect=[CommandResult("/bin/bash", 0), CommandResult("ok\n", 0)]) as run,
        ):
            provider = AioSandboxProvider()
            sandbox_id = provider.acquire("thread_1")
            sandbox = provider.get(sandbox_id)
            assert sandbox is not None
            assert sandbox.execute_command("echo ok") == "ok\n"

    run_cmd = next(cmd for cmd in calls if cmd[:2] == ["docker", "run"])
    exec_cmd = run.call_args.args[0]
    assert "--user" not in run_cmd
    assert exec_cmd[:6] == ["docker", "exec", "-i", "-u", "1234:5678", "a" * 64]


def test_aio_provider_rejects_unsafe_thread_id(tmp_path, monkeypatch) -> None:
    monkeypatch.setenv("DEER_FLOW_HOME", str(tmp_path / "state"))
    import deerflow.config.paths as paths_mod

    paths_mod._paths = None
    config = SimpleNamespace(
        sandbox=SimpleNamespace(
            image="sandbox-image:test",
            replicas=1,
            container_prefix="df-test",
            idle_timeout=600,
            environment={},
            mounts=[],
            security_opt=[],
        ),
        skills=SimpleNamespace(
            get_skills_path=lambda: Path("missing"),
            container_path="/mnt/skills",
        ),
    )

    def fake_run(cmd, **kwargs):
        return _completed(stdout="25.0.0")

    with patch("deerflow.config.get_app_config", return_value=config):
        with patch("subprocess.run", side_effect=fake_run):
            provider = AioSandboxProvider()
            with pytest.raises(ValueError):
                provider.acquire("../escape")
