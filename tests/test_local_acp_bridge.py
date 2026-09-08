from __future__ import annotations

import json
import os
import socket
import subprocess
import sys
import time
from pathlib import Path

import pytest


def _bridge_binary(project_root: Path) -> Path:
    configured = os.getenv("DEERFLOW_ACP_BRIDGE_BIN")
    if configured:
        return Path(configured)
    suffix = ".exe" if os.name == "nt" else ""
    return project_root / "bridge" / "target" / "release" / f"deerflow-acp{suffix}"


def _wait_for(path: Path, process: subprocess.Popen[str], timeout: float = 20) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if path.exists():
            return
        if process.poll() is not None:
            stderr = process.stderr.read() if process.stderr is not None else ""
            pytest.fail(f"ACP daemon exited before becoming ready: {stderr}")
        time.sleep(0.05)
    pytest.fail(f"Timed out waiting for {path}")


def _open_bridge(
    bridge: Path,
    config: Path,
    runtime_dir: Path,
    cwd: Path,
    *,
    protocol: str = "v1",
) -> subprocess.Popen[str]:
    command = [str(bridge)]
    if protocol != "v1":
        command.extend(["--protocol", protocol])
    command.extend(
        [
            "--config",
            str(config),
            "--runtime-dir",
            str(runtime_dir),
            "--no-auto-start",
        ]
    )
    process = subprocess.Popen(
        command,
        cwd=cwd,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        encoding="utf-8",
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
    )
    assert process.stdin is not None
    assert process.stdout is not None
    return process


def _request(
    process: subprocess.Popen[str],
    request_id: int,
    method: str,
    params: dict[str, object],
) -> dict[str, object]:
    assert process.stdin is not None
    assert process.stdout is not None
    process.stdin.write(
        json.dumps(
            {"jsonrpc": "2.0", "id": request_id, "method": method, "params": params}
        )
        + "\n"
    )
    process.stdin.flush()
    while True:
        response = json.loads(process.stdout.readline())
        if response.get("id") == request_id:
            return response


def _close_bridge(process: subprocess.Popen[str]) -> None:
    assert process.stdin is not None
    process.stdin.close()
    return_code = process.wait(timeout=20)
    stderr = process.stderr.read() if process.stderr is not None else ""
    assert return_code == 0, stderr


def test_native_bridge_real_acp_roundtrip_with_two_live_clients(tmp_path: Path) -> None:
    project_root = Path(__file__).resolve().parents[1]
    bridge = _bridge_binary(project_root)
    if not bridge.is_file():
        pytest.skip(
            "Build bridge/Cargo.toml in release mode to run the native bridge test"
        )

    config = tmp_path / "config.yaml"
    config.write_text(
        """
api:
  data_dir: ./data
local_acp:
  checkpointer_path: ./data/checkpoints.db
  session_store_path: ./data/sessions.db
  accept_client_mcp_servers: true
sandbox:
  use: deerflow.sandbox.local:LocalSandboxProvider
""".strip()
        + "\n",
        encoding="utf-8",
    )
    runtime_dir = tmp_path / "runtime"
    endpoint = runtime_dir / "endpoint.json"
    environment = os.environ.copy()
    environment["PYTHONPATH"] = os.pathsep.join(
        [str(project_root), environment.get("PYTHONPATH", "")]
    ).rstrip(os.pathsep)
    daemon = subprocess.Popen(
        [
            sys.executable,
            "-m",
            "deerflow.acp.daemon",
            "--config",
            str(config),
            "--runtime-dir",
            str(runtime_dir),
            "--no-warmup",
        ],
        cwd=project_root,
        env=environment,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
        text=True,
        encoding="utf-8",
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
    )
    bridges: list[subprocess.Popen[str]] = []
    try:
        _wait_for(endpoint, daemon)
        first = _open_bridge(bridge, config, runtime_dir, tmp_path)
        second = _open_bridge(bridge, config, runtime_dir, tmp_path)
        bridges.extend((first, second))
        for request_id, process in enumerate((first, second), start=1):
            initialized = _request(
                process,
                request_id,
                "initialize",
                {
                    "protocolVersion": 1,
                    "clientCapabilities": {},
                    "clientInfo": {"name": f"bridge-test-{request_id}", "version": "1"},
                },
            )
            assert initialized["result"]["protocolVersion"] == 1  # type: ignore[index]
        first_created = _request(
            first, 10, "session/new", {"cwd": str(tmp_path), "mcpServers": []}
        )
        second_created = _request(
            second, 11, "session/new", {"cwd": str(tmp_path), "mcpServers": []}
        )
        first_session = first_created["result"]["sessionId"]  # type: ignore[index]
        second_session = second_created["result"]["sessionId"]  # type: ignore[index]
        assert first_session != second_session

        status = subprocess.run(
            [
                str(bridge),
                "--status",
                "--config",
                str(config),
                "--runtime-dir",
                str(runtime_dir),
            ],
            capture_output=True,
            text=True,
            encoding="utf-8",
            timeout=10,
            check=False,
        )
        assert status.returncode == 0, status.stderr
        assert "running pid=" in status.stdout
        assert "connections=2" in status.stdout

        _close_bridge(first)
        bridges.remove(first)
        listed = _request(second, 12, "session/list", {"cwd": str(tmp_path)})
        listed_ids = {
            item["sessionId"]  # type: ignore[index]
            for item in listed["result"]["sessions"]  # type: ignore[index]
        }
        assert {first_session, second_session} <= listed_ids

        _close_bridge(second)
        bridges.remove(second)

        stopped = subprocess.run(
            [
                str(bridge),
                "--stop-daemon",
                "--config",
                str(config),
                "--runtime-dir",
                str(runtime_dir),
            ],
            capture_output=True,
            text=True,
            encoding="utf-8",
            timeout=15,
            check=False,
        )
        assert stopped.returncode == 0, stopped.stderr
        assert stopped.stdout.strip() == "stopped"
        assert daemon.wait(timeout=20) == 0
        assert not endpoint.exists()
    finally:
        for process in bridges:
            if process.poll() is None:
                process.terminate()
                process.wait(timeout=10)
        if daemon.poll() is None:
            daemon.terminate()
            try:
                daemon.wait(timeout=10)
            except subprocess.TimeoutExpired:
                daemon.kill()
                daemon.wait(timeout=10)


def test_native_bridge_v2_session_lifecycle_across_connections(tmp_path: Path) -> None:
    project_root = Path(__file__).resolve().parents[1]
    bridge = _bridge_binary(project_root)
    if not bridge.is_file():
        pytest.skip(
            "Build bridge/Cargo.toml in release mode to run the native bridge test"
        )

    config = tmp_path / "config.yaml"
    config.write_text(
        "api:\n  data_dir: ./data\nlocal_acp:\n"
        "  checkpointer_path: ./data/checkpoints.db\n"
        "  session_store_path: ./data/sessions.db\n"
        "sandbox:\n  use: deerflow.sandbox.local:LocalSandboxProvider\n",
        encoding="utf-8",
    )
    runtime_dir = tmp_path / "runtime"
    endpoint = runtime_dir / "endpoint.json"
    environment = os.environ.copy()
    environment["PYTHONPATH"] = os.pathsep.join(
        [str(project_root), environment.get("PYTHONPATH", "")]
    ).rstrip(os.pathsep)
    daemon = subprocess.Popen(
        [
            sys.executable,
            "-m",
            "deerflow.acp.daemon",
            "--config",
            str(config),
            "--runtime-dir",
            str(runtime_dir),
            "--no-warmup",
        ],
        cwd=project_root,
        env=environment,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
        text=True,
        encoding="utf-8",
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
    )
    bridges: list[subprocess.Popen[str]] = []
    try:
        _wait_for(endpoint, daemon)
        first = _open_bridge(
            bridge, config, runtime_dir, tmp_path, protocol="v2"
        )
        bridges.append(first)
        initialized = _request(
            first,
            1,
            "initialize",
            {
                "protocolVersion": 2,
                "capabilities": {},
                "info": {"name": "bridge-v2-test", "version": "1"},
            },
        )
        assert initialized["result"]["protocolVersion"] == 2  # type: ignore[index]
        assert "session" in initialized["result"]["capabilities"]  # type: ignore[index]

        created = _request(
            first,
            2,
            "session/new",
            {
                "cwd": str(tmp_path),
                "additionalDirectories": [],
                "mcpServers": [],
            },
        )
        session_id = created["result"]["sessionId"]  # type: ignore[index]
        listed = _request(
            first, 20, "session/list", {"cwd": str(tmp_path)}
        )
        listed_ids = {
            item["sessionId"]  # type: ignore[index]
            for item in listed["result"]["sessions"]  # type: ignore[index]
        }
        assert session_id in listed_ids
        _close_bridge(first)
        bridges.remove(first)

        second = _open_bridge(
            bridge, config, runtime_dir, tmp_path, protocol="v2"
        )
        bridges.append(second)
        _request(
            second,
            3,
            "initialize",
            {
                "protocolVersion": 2,
                "capabilities": {},
                "info": {"name": "bridge-v2-resume-test", "version": "1"},
            },
        )
        resumed = _request(
            second,
            4,
            "session/resume",
            {
                "sessionId": session_id,
                "cwd": str(tmp_path),
                "additionalDirectories": [],
                "mcpServers": [],
            },
        )
        assert resumed["result"] == {}
        closed = _request(
            second, 5, "session/close", {"sessionId": session_id}
        )
        assert closed["result"] == {}
        _close_bridge(second)
        bridges.remove(second)

        stopped = subprocess.run(
            [
                str(bridge),
                "--stop-daemon",
                "--config",
                str(config),
                "--runtime-dir",
                str(runtime_dir),
            ],
            capture_output=True,
            text=True,
            encoding="utf-8",
            timeout=15,
            check=False,
        )
        assert stopped.returncode == 0, stopped.stderr
        assert daemon.wait(timeout=20) == 0
        assert not endpoint.exists()
    finally:
        for process in bridges:
            if process.poll() is None:
                process.terminate()
                process.wait(timeout=10)
        if daemon.poll() is None:
            daemon.terminate()
            try:
                daemon.wait(timeout=10)
            except subprocess.TimeoutExpired:
                daemon.kill()
                daemon.wait(timeout=10)


def test_native_bridge_auto_starts_and_stops_daemon(tmp_path: Path) -> None:
    project_root = Path(__file__).resolve().parents[1]
    bridge = _bridge_binary(project_root)
    if not bridge.is_file():
        pytest.skip("Native bridge release binary is required")

    config = tmp_path / "config.yaml"
    config.write_text(
        "api:\n  data_dir: ./data\nlocal_acp:\n  session_store_path: ./data/sessions.db\n"
        "sandbox:\n  use: deerflow.sandbox.local:LocalSandboxProvider\n",
        encoding="utf-8",
    )
    runtime_dir = tmp_path / "runtime"
    environment = os.environ.copy()
    environment["DEER_FLOW_ACP_DAEMON_WARMUP"] = "0"
    environment["DEER_FLOW_ACP_DAEMON_START_TIMEOUT_MS"] = "20000"

    started = subprocess.run(
        [
            str(bridge),
            "--start-daemon",
            "--python",
            sys.executable,
            "--config",
            str(config),
            "--runtime-dir",
            str(runtime_dir),
        ],
        env=environment,
        capture_output=True,
        text=True,
        encoding="utf-8",
        timeout=30,
        check=False,
    )
    try:
        assert started.returncode == 0, started.stderr
        assert "running pid=" in started.stdout
    finally:
        stopped = subprocess.run(
            [
                str(bridge),
                "--stop-daemon",
                "--config",
                str(config),
                "--runtime-dir",
                str(runtime_dir),
            ],
            env=environment,
            capture_output=True,
            text=True,
            encoding="utf-8",
            timeout=20,
            check=False,
        )
        assert stopped.returncode == 0, stopped.stderr


def test_native_bridge_concurrent_auto_start_converges_on_one_daemon(
    tmp_path: Path,
) -> None:
    project_root = Path(__file__).resolve().parents[1]
    bridge = _bridge_binary(project_root)
    if not bridge.is_file():
        pytest.skip("Native bridge binary is required")

    config = tmp_path / "config.yaml"
    config.write_text(
        "api:\n  data_dir: ./data\nlocal_acp:\n  session_store_path: ./data/sessions.db\n"
        "sandbox:\n  use: deerflow.sandbox.local:LocalSandboxProvider\n",
        encoding="utf-8",
    )
    runtime_dir = tmp_path / "runtime"
    environment = os.environ.copy()
    environment["DEER_FLOW_ACP_DAEMON_WARMUP"] = "0"
    environment["DEER_FLOW_ACP_DAEMON_START_TIMEOUT_MS"] = "20000"
    command = [
        str(bridge),
        "--start-daemon",
        "--python",
        sys.executable,
        "--config",
        str(config),
        "--runtime-dir",
        str(runtime_dir),
    ]
    starters = [
        subprocess.Popen(
            command,
            env=environment,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            encoding="utf-8",
            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
        )
        for _ in range(2)
    ]
    try:
        outputs = [process.communicate(timeout=30) for process in starters]
        for process, (stdout, stderr) in zip(starters, outputs, strict=True):
            assert process.returncode == 0, stderr
            assert "running pid=" in stdout
        endpoint = json.loads(
            (runtime_dir / "endpoint.json").read_text(encoding="utf-8")
        )
        assert all(f"pid={endpoint['pid']}" in stdout for stdout, _stderr in outputs)
    finally:
        stopped = subprocess.run(
            [
                str(bridge),
                "--stop-daemon",
                "--config",
                str(config),
                "--runtime-dir",
                str(runtime_dir),
            ],
            env=environment,
            capture_output=True,
            text=True,
            encoding="utf-8",
            timeout=20,
            check=False,
        )
        assert stopped.returncode == 0, stopped.stderr


def test_native_bridge_does_not_delete_temporarily_unresponsive_endpoint(
    tmp_path: Path,
) -> None:
    project_root = Path(__file__).resolve().parents[1]
    bridge = _bridge_binary(project_root)
    if not bridge.is_file():
        pytest.skip("Native bridge release binary is required")

    config = tmp_path / "config.yaml"
    config.write_text("api:\n  data_dir: ./data\n", encoding="utf-8")
    runtime_dir = tmp_path / "runtime"
    runtime_dir.mkdir()
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        unavailable_port = probe.getsockname()[1]
    endpoint = runtime_dir / "endpoint.json"
    payload = {
        "host": "127.0.0.1",
        "port": unavailable_port,
        "token": "temporarily-unresponsive",
        "pid": os.getpid(),
        "build_id": "test",
        "config_path": str(config.resolve()),
    }
    endpoint.write_text(json.dumps(payload), encoding="utf-8")

    result = subprocess.run(
        [
            str(bridge),
            "--config",
            str(config),
            "--runtime-dir",
            str(runtime_dir),
            "--no-auto-start",
        ],
        stdin=subprocess.DEVNULL,
        capture_output=True,
        text=True,
        encoding="utf-8",
        timeout=10,
        check=False,
    )

    assert result.returncode != 0
    assert endpoint.exists()
    assert json.loads(endpoint.read_text(encoding="utf-8")) == payload
