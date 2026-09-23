from __future__ import annotations

import asyncio
import json
import subprocess
import threading
from pathlib import Path
from types import SimpleNamespace

import pytest

import deerflow.sandbox.aio as aio


class FakeDocker:
    def __init__(self) -> None:
        self.containers: dict[str, bool] = {}
        self.calls: list[list[str]] = []
        self.fail_remove: set[str] = set()
        self.inspect_error = False
        self.next_id = 0
        self.names: dict[str, str] = {}
        self.owners: dict[str, str] = {}
        self.create_mode = "ok"
        self.pending_creation: tuple[str, str] | None = None

    def create_pending(self):
        name, owner = self.pending_creation
        self.next_id += 1
        container_id = f"{self.next_id:064x}"
        self.containers[container_id] = True
        self.names[name] = container_id
        self.owners[container_id] = owner
        return container_id

    def run(self, args, *, check=True):
        self.calls.append(args)
        stdout = ""
        stderr = ""
        code = 0
        if args[0] == "run":
            self.pending_creation = (args[args.index("--name") + 1], args[args.index("--label") + 1].split("=", 1)[1])
            if self.create_mode == "failed_absent":
                raise subprocess.CalledProcessError(125, args, stderr="fake creation failure")
            if self.create_mode != "timeout_absent":
                stdout = self.create_pending()
            if self.create_mode.startswith("timeout"):
                raise subprocess.TimeoutExpired(args, 60)
            if self.create_mode == "invalid_stdout":
                stdout = "unexpected Docker output"
        elif args[0] == "inspect":
            container_id = self.names.get(args[-1], args[-1])
            if self.inspect_error:
                stderr, code = "Cannot connect to the Docker daemon", 1
            elif container_id not in self.containers:
                stderr, code = f"Error: No such object: {args[-1]}", 1
            elif args[2] == "{{json .}}":
                stdout = json.dumps({"Id": container_id, "Config": {"Labels": {"deerflow.sandbox.owner": self.owners[container_id]}}})
            else:
                stdout = str(self.containers[container_id]).lower()
        elif args[0] == "rm":
            if args[-1] in self.fail_remove:
                stderr, code = "permission denied", 1
            else:
                self.containers.pop(args[-1], None)
        else:
            raise AssertionError(args)
        return subprocess.CompletedProcess(args, code, stdout, stderr)


@pytest.fixture
def provider_factory(monkeypatch):
    def forbidden_subprocess(*args, **kwargs):
        raise AssertionError("Lifecycle tests must never invoke Docker or WSL")

    monkeypatch.setattr(subprocess, "run", forbidden_subprocess)
    monkeypatch.setattr(aio.AioSandboxProvider, "_verify_docker_available", lambda self: None)
    monkeypatch.setattr(aio.AioSandboxProvider, "_build_run_args", lambda self, *args, **kwargs: [
        "run", "--name", kwargs["container_name"], "--label", f"deerflow.sandbox.owner={kwargs['owner_token']}",
    ])
    monkeypatch.setattr("deerflow.skills.projection.get_skill_projection", lambda _: SimpleNamespace(revision="rev", path=Path("unused")))

    def create(*, replicas=1, idle_timeout=0, command_timeout=17):
        config = SimpleNamespace(
            sandbox=SimpleNamespace(image=None, replicas=replicas, idle_timeout=idle_timeout,
                                    container_prefix=None, environment=None, mounts=None,
                                    bash_command_timeout=command_timeout),
            skills=SimpleNamespace(container_path="/mnt/skills"),
        )
        monkeypatch.setattr("deerflow.config.get_app_config", lambda: config)
        provider = aio.AioSandboxProvider()
        docker = FakeDocker()
        provider._run_docker = docker.run
        return provider, docker

    return create


def test_capacity_never_evicts_running_operation(provider_factory):
    provider, docker = provider_factory()
    sandbox_id = provider.acquire("a")
    sandbox = provider.get(sandbox_id)
    with sandbox._operation_factory():
        with pytest.raises(RuntimeError, match="busy"):
            provider.acquire("b")
        with pytest.raises(RuntimeError, match="busy"):
            provider.release_thread("a")
        assert docker.containers[sandbox.container_name]
    provider.acquire("b")
    assert sandbox_id not in provider._records
    assert sandbox.container_name not in docker.containers


def test_idle_cleanup_skips_running_operation_and_uses_finish_time(provider_factory, monkeypatch):
    now = [100.0]
    monkeypatch.setattr(aio.time, "monotonic", lambda: now[0])
    provider, docker = provider_factory(replicas=3, idle_timeout=10)
    sandbox_id = provider.acquire("a")
    sandbox = provider.get(sandbox_id)
    with sandbox._operation_factory():
        now[0] = 120.0
        provider.acquire("b")
        assert sandbox_id in provider._records
    now[0] = 121.0
    provider.acquire("c")
    assert sandbox_id in provider._records
    now[0] = 140.0
    # Time alone does not trigger cleanup; it runs on the next acquisition.
    assert sandbox.container_name in docker.containers
    provider.acquire("d")
    assert sandbox.container_name not in docker.containers


def test_cancelled_awaiter_does_not_release_worker_occupation(provider_factory):
    provider, docker = provider_factory()
    sandbox_id = provider.acquire("a")
    sandbox = provider.get(sandbox_id)
    entered, finish = threading.Event(), threading.Event()

    def worker():
        with sandbox._operation_factory():
            entered.set()
            assert finish.wait(3)

    async def scenario():
        task = asyncio.create_task(asyncio.to_thread(worker))
        assert await asyncio.to_thread(entered.wait, 2)
        try:
            task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await task
            with pytest.raises(RuntimeError, match="busy"):
                provider.acquire("b")
            assert docker.containers[sandbox.container_name]
        finally:
            finish.set()

    asyncio.run(scenario())
    assert provider._records[sandbox_id].active_operations == 0
    provider.acquire("b")


def test_failed_removal_retains_record_projection_and_capacity(provider_factory):
    provider, docker = provider_factory()
    sandbox_id = provider.acquire("a")
    container_id = provider.get(sandbox_id).container_name
    docker.fail_remove.add(container_id)
    with pytest.raises(RuntimeError, match="removal was not confirmed"):
        provider.acquire("b")
    assert len(docker.containers) == 1
    assert sandbox_id in provider._records
    assert provider.active_skill_revisions() == {"rev"}
    docker.fail_remove.clear()
    provider.acquire("b")
    assert len(docker.containers) == 1
    assert sandbox_id not in provider._records


@pytest.mark.parametrize("stopped", [False, True])
def test_dead_cached_container_is_replaced_without_replaying_commands(provider_factory, stopped):
    provider, docker = provider_factory()
    sandbox_id = provider.acquire("a")
    old = provider.get(sandbox_id)
    if stopped:
        docker.containers[old.container_name] = False
    else:
        docker.containers.pop(old.container_name)
    assert provider.get(sandbox_id) is None
    assert provider.acquire("a") == sandbox_id
    current = provider.get(sandbox_id)
    assert current.container_name != old.container_name
    assert all(call[0] != "exec" for call in docker.calls)
    with pytest.raises(RuntimeError, match="no longer available"):
        with old._operation_factory():
            pytest.fail("Stale object must not execute on the new generation")
    provider._remove_record_locked(sandbox_id, aio._SandboxRecord(old, "a", 0, "rev"))
    assert current.container_name in docker.containers


def test_failed_health_check_does_not_destroy_or_replace_container(provider_factory):
    provider, docker = provider_factory()
    sandbox_id = provider.acquire("a")
    docker.inspect_error = True
    with pytest.raises(RuntimeError, match="Could not inspect"):
        provider.get(sandbox_id)
    assert sandbox_id in provider._records
    assert len(docker.containers) == 1
    assert not any(call[0] == "rm" for call in docker.calls)


@pytest.mark.parametrize("via_timeout", [False, True])
def test_unconfirmed_execution_keeps_capacity_until_container_is_gone(provider_factory, via_timeout):
    provider, docker = provider_factory()
    sandbox_id = provider.acquire("a")
    sandbox = provider.get(sandbox_id)
    if via_timeout:
        with pytest.raises(subprocess.TimeoutExpired):
            with sandbox._operation_factory():
                raise subprocess.TimeoutExpired("fake-exec", 1)
    else:
        with sandbox._operation_factory() as lease:
            lease.mark_uncertain()
    with pytest.raises(RuntimeError, match="busy"):
        provider.acquire("b")
    with pytest.raises(RuntimeError, match="unconfirmed"):
        provider.release_thread("a")
    assert not any(call[0] == "rm" for call in docker.calls)
    docker.containers.pop(sandbox.container_name)
    provider.acquire("b")
    assert sandbox_id not in provider._records


def test_shutdown_is_retryable_and_retains_failed_resources(provider_factory):
    provider, docker = provider_factory()
    sandbox_id = provider.acquire("a")
    sandbox = provider.get(sandbox_id)
    docker.fail_remove.add(sandbox.container_name)
    with pytest.raises(RuntimeError, match="removal was not confirmed"):
        provider.shutdown()
    assert sandbox_id in provider._records
    with pytest.raises(RuntimeError, match="shutting down"):
        provider.acquire("b")
    docker.fail_remove.clear()
    provider.shutdown()
    provider.shutdown()
    assert not provider._records
    assert not docker.containers


def test_creation_uses_configured_timeout_and_never_deletes_unknown_names(provider_factory):
    provider, docker = provider_factory(command_timeout=23)
    sandbox_id = provider.acquire("a")
    sandbox = provider.get(sandbox_id)
    assert sandbox.timeout == 23
    assert sandbox.container_name == "1".zfill(64)
    assert not any(call[0] == "rm" for call in docker.calls)


def test_creation_timeout_reserves_capacity_until_owned_container_appears(provider_factory):
    provider, docker = provider_factory()
    docker.create_mode = "timeout_absent"
    with pytest.raises(RuntimeError, match="creation may still be in progress"):
        provider.acquire("a")
    sandbox_id, record = next(iter(provider._records.items()))
    assert record.creation_pending
    with pytest.raises(RuntimeError, match="busy"):
        provider.acquire("b")
    assert len(provider._records) == 1
    assert not any(call[0] == "rm" for call in docker.calls)
    owned_id = docker.create_pending()
    docker.create_mode = "ok"
    assert provider.acquire("a") == sandbox_id
    assert provider.get(sandbox_id).container_name == owned_id
    assert not record.creation_pending
    provider.release_thread("a")
    assert owned_id not in docker.containers


def test_creation_timeout_with_owned_container_keeps_immutable_identity(provider_factory):
    provider, docker = provider_factory()
    docker.create_mode = "timeout_present"
    with pytest.raises(RuntimeError, match="creation may still be in progress"):
        provider.acquire("a")
    sandbox_id, record = next(iter(provider._records.items()))
    assert not record.creation_pending
    assert record.sandbox.container_name == "1".zfill(64)
    provider.shutdown()
    assert not provider._records
    assert not docker.containers


def test_invalid_creation_output_is_reconciled_by_owner_label(provider_factory):
    provider, docker = provider_factory()
    docker.create_mode = "invalid_stdout"
    sandbox_id = provider.acquire("a")
    assert provider.get(sandbox_id).container_name == "1".zfill(64)
    assert len(docker.containers) == 1


def test_creation_reconciliation_does_not_adopt_or_delete_wrong_owner(provider_factory):
    provider, docker = provider_factory()
    docker.create_mode = "timeout_absent"
    with pytest.raises(RuntimeError):
        provider.acquire("a")
    wrong_id = docker.create_pending()
    docker.owners[wrong_id] = "another-owner"
    with pytest.raises(RuntimeError, match="does not match"):
        provider.acquire("a")
    assert len(provider._records) == 1
    assert wrong_id in docker.containers
    assert not any(call[0] == "rm" for call in docker.calls)


def test_failed_creation_with_confirmed_absence_releases_capacity(provider_factory):
    provider, docker = provider_factory()
    docker.create_mode = "failed_absent"
    with pytest.raises(RuntimeError, match="fake creation failure"):
        provider.acquire("a")
    assert not provider._records
    docker.create_mode = "ok"
    provider.acquire("b")
    assert len(provider._records) == 1


@pytest.mark.parametrize("failure", [
    subprocess.TimeoutExpired("docker exec", 1),
    OSError("transport pipe disconnected"),
    subprocess.CalledProcessError(1, ["docker", "exec"], stderr="transport disconnected"),
])
def test_file_operation_transport_failure_keeps_capacity(provider_factory, monkeypatch, failure):
    provider, docker = provider_factory()
    sandbox_id = provider.acquire("a")
    sandbox = provider.get(sandbox_id)

    def broken_transport(*args, **kwargs):
        raise failure

    monkeypatch.setattr(subprocess, "run", broken_transport)
    with pytest.raises(type(failure)):
        sandbox._docker_exec(["cat", "/tmp/example"])
    assert provider._records[sandbox_id].active_operations == 0
    assert provider._records[sandbox_id].execution_uncertain
    with pytest.raises(RuntimeError, match="busy"):
        provider.acquire("b")
    assert sandbox.container_name in docker.containers


def test_missing_docker_executable_does_not_reserve_execution(provider_factory, monkeypatch):
    provider, docker = provider_factory()
    sandbox_id = provider.acquire("a")
    sandbox = provider.get(sandbox_id)

    def missing_executable(*args, **kwargs):
        raise FileNotFoundError("docker")

    monkeypatch.setattr(subprocess, "run", missing_executable)
    with pytest.raises(FileNotFoundError):
        sandbox._docker_exec(["cat", "/tmp/example"])
    assert not provider._records[sandbox_id].execution_uncertain
    provider.acquire("b")
    assert sandbox.container_name not in docker.containers


@pytest.mark.parametrize("returncode,stderr", [
    (-15, b""),
    (1, b"Error response from daemon: transport is closing"),
    (1, "read connection: connection reset by peer"),
    (125, "unexpected EOF"),
])
def test_docker_transport_exit_pins_lease_and_never_replays_file_append(provider_factory, monkeypatch, returncode, stderr):
    provider, docker = provider_factory()
    sandbox_id = provider.acquire("a")
    sandbox = provider.get(sandbox_id)
    calls = []

    def transport_failure(argv, **kwargs):
        calls.append(argv)
        return subprocess.CompletedProcess(argv, returncode, b"", stderr)

    monkeypatch.setattr(subprocess, "run", transport_failure)
    with pytest.raises(RuntimeError, match="not retried"):
        sandbox.write_file("/tmp/result.txt", "possibly appended", append=True)
    assert len(calls) == 1
    assert provider._records[sandbox_id].execution_uncertain
    with pytest.raises(RuntimeError, match="busy"):
        provider.acquire("b")
    assert sandbox.container_name in docker.containers


@pytest.mark.parametrize("returncode", range(2, 10))
def test_normal_remote_file_protocol_error_does_not_pin_capacity(provider_factory, monkeypatch, returncode):
    provider, _docker = provider_factory()
    sandbox_id = provider.acquire("a")
    sandbox = provider.get(sandbox_id)
    monkeypatch.setattr(subprocess, "run", lambda argv, **kwargs:
                        subprocess.CompletedProcess(argv, returncode, "", "remote file protocol failure"))
    result = sandbox._docker_exec(["python3", "-", "{}"])
    assert result.returncode == returncode
    assert not provider._records[sandbox_id].execution_uncertain
    provider.acquire("b")
