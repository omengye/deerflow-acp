from __future__ import annotations

import asyncio
import json
import os
from pathlib import Path
from typing import Any

import pytest

from deerflow.acp.config import LocalACPConfig
from deerflow.acp import daemon as daemon_module
from deerflow.acp import daemon_endpoint
from deerflow.acp.daemon import ACPDaemon
from deerflow.acp.daemon_endpoint import (
    DaemonAlreadyRunning,
    DaemonEndpoint,
    SingleInstanceLock,
    get_runtime_dir,
)
from deerflow.acp.session_store import LocalACPSessionStore


class FakeRuntime:
    async def astream(self, *args: Any, **kwargs: Any):
        del kwargs
        if len(args) > 1 and args[1] == "wait-for-cancel":
            await asyncio.Future()
        if False:
            yield None

    async def history(self, session_id: str) -> list[dict[str, Any]]:
        del session_id
        return []

    async def bind_client_mcp(self, session_id: str, servers: Any) -> None:
        del session_id, servers

    async def release_session(self, session_id: str) -> None:
        del session_id


def make_config(tmp_path: Path) -> LocalACPConfig:
    return LocalACPConfig(
        config_path=tmp_path / "config.yaml",
        checkpointer_path=tmp_path / "checkpoints.db",
        session_store_path=tmp_path / "sessions.db",
    )


async def connect(
    endpoint: DaemonEndpoint, command: str
) -> tuple[asyncio.StreamReader, asyncio.StreamWriter, str]:
    reader, writer = await asyncio.open_connection(endpoint.host, endpoint.port)
    writer.write(f"DFACP/1 {endpoint.token} {command}\n".encode())
    await writer.drain()
    response = (await reader.readline()).decode().strip()
    return reader, writer, response


async def manage(endpoint: DaemonEndpoint, request: Any) -> dict[str, Any]:
    reader, writer, response = await connect(endpoint, "MANAGE")
    assert response == "OK"
    writer.write(json.dumps(request).encode() + b"\n")
    await writer.drain()
    payload = json.loads(await reader.readline())
    assert await reader.read() == b""
    writer.close()
    await writer.wait_closed()
    return payload


def test_endpoint_roundtrip_runtime_override_and_single_instance_lock(
    tmp_path: Path,
) -> None:
    runtime_dir = get_runtime_dir(tmp_path / "runtime")
    assert runtime_dir == (tmp_path / "runtime").resolve()

    endpoint_path = runtime_dir / "endpoint.json"
    endpoint = DaemonEndpoint(
        host="127.0.0.1",
        port=1234,
        token="secret",
        pid=42,
        build_id="test",
        config_path=str(tmp_path / "config.yaml"),
    )
    endpoint.publish(endpoint_path)
    assert DaemonEndpoint.load(endpoint_path) == endpoint

    first = SingleInstanceLock(runtime_dir / "daemon.lock")
    second = SingleInstanceLock(runtime_dir / "daemon.lock")
    first.acquire()
    try:
        with pytest.raises(DaemonAlreadyRunning):
            second.acquire()
    finally:
        first.release()
    second.acquire()
    second.release()


def test_runtime_dir_uses_portable_product_root(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.delenv("DEER_FLOW_ACP_RUNTIME_DIR", raising=False)
    monkeypatch.setattr(daemon_endpoint, "_portable_root", lambda: tmp_path)
    assert get_runtime_dir() == (tmp_path / "user-data" / "runtime" / "acp").resolve()


@pytest.mark.asyncio
async def test_daemon_rejects_non_local_sandbox_before_opening_store(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    prepared = False

    class FakeConfig:
        def prepare_environment(self) -> None:
            nonlocal prepared
            prepared = True

    class RejectingRuntime:
        def __init__(self, _config: FakeConfig) -> None:
            pass

        def validate_sandbox_provider(self) -> None:
            raise RuntimeError("Portable ACP supports only LocalSandboxProvider")

    monkeypatch.setattr(
        daemon_module.LocalACPConfig,
        "from_file",
        staticmethod(lambda _path: FakeConfig()),
    )
    monkeypatch.setattr(daemon_module, "LocalACPRuntime", RejectingRuntime)

    with pytest.raises(
        RuntimeError, match="Portable ACP supports only LocalSandboxProvider"
    ):
        await daemon_module._run_daemon(None, tmp_path / "runtime", warmup=False)

    assert prepared is True


@pytest.mark.asyncio
async def test_daemon_accepts_multiple_clients_status_stop_and_reconnect(
    tmp_path: Path,
) -> None:
    config = make_config(tmp_path)
    store = LocalACPSessionStore(config.session_store_path)
    store.setup()
    daemon = ACPDaemon(
        config, store, FakeRuntime(), tmp_path / "runtime", token="test-token"
    )  # type: ignore[arg-type]
    endpoint = await daemon.start()
    assert DaemonEndpoint.load(daemon.endpoint_path) == endpoint

    status_reader, status_writer, status = await connect(endpoint, "STATUS")
    assert status.startswith("OK ")
    assert await status_reader.read() == b""
    status_writer.close()
    await status_writer.wait_closed()

    first_reader, first_writer, response = await connect(endpoint, "ACP")
    assert response == "OK"
    first_writer.write(
        json.dumps(
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "initialize",
                "params": {
                    "protocolVersion": 1,
                    "clientCapabilities": {},
                    "clientInfo": {"name": "test", "version": "1"},
                },
            }
        ).encode()
        + b"\n"
    )
    await first_writer.drain()
    initialized = json.loads(await first_reader.readline())
    assert initialized["result"]["protocolVersion"] == 1

    second_reader, second_writer, second = await connect(endpoint, "ACP")
    assert second == "OK"
    second_writer.write(
        json.dumps(
            {
                "jsonrpc": "2.0",
                "id": 2,
                "method": "initialize",
                "params": {
                    "protocolVersion": 1,
                    "clientCapabilities": {},
                    "clientInfo": {"name": "test-2", "version": "1"},
                },
            }
        ).encode()
        + b"\n"
    )
    await second_writer.drain()
    second_initialized = json.loads(await second_reader.readline())
    assert second_initialized["result"]["protocolVersion"] == 1

    count_reader, count_writer, count_status = await connect(endpoint, "STATUS")
    assert "connections=2" in count_status
    assert await count_reader.read() == b""
    count_writer.close()
    await count_writer.wait_closed()

    first_writer.close()
    await first_writer.wait_closed()
    for _ in range(100):
        if len(daemon._connections) == 1:
            break
        await asyncio.sleep(0.01)
    assert len(daemon._connections) == 1

    reconnect_reader, reconnect_writer, reconnect = await connect(endpoint, "ACP")
    assert reconnect == "OK"
    reconnect_writer.close()
    await reconnect_writer.wait_closed()
    await reconnect_reader.read()

    second_writer.close()
    await second_writer.wait_closed()
    await second_reader.read()

    stop_reader, stop_writer, stopped = await connect(endpoint, "STOP")
    assert stopped == "OK"
    assert await stop_reader.read() == b""
    stop_writer.close()
    await stop_writer.wait_closed()
    await asyncio.wait_for(daemon.stop_requested.wait(), timeout=1)

    await daemon.close()
    store.close()
    assert not daemon.endpoint_path.exists()


@pytest.mark.asyncio
async def test_v2_facade_prompt_acknowledges_then_runs_to_idle(tmp_path: Path) -> None:
    project_root = Path(__file__).resolve().parents[1]
    bridge = (
        project_root
        / "bridge"
        / "target"
        / "release"
        / ("deerflow-acp.exe" if os.name == "nt" else "deerflow-acp")
    )
    if not bridge.is_file():
        pytest.skip("Native bridge release binary is required")

    config = make_config(tmp_path)
    config.config_path.write_text("{}\n", encoding="utf-8")
    store = LocalACPSessionStore(config.session_store_path)
    store.setup()
    daemon = ACPDaemon(
        config, store, FakeRuntime(), tmp_path / "runtime", token="test-token"
    )  # type: ignore[arg-type]
    await daemon.start()
    process = await asyncio.create_subprocess_exec(
        str(bridge),
        "--protocol",
        "v2",
        "--config",
        str(config.config_path),
        "--runtime-dir",
        str(tmp_path / "runtime"),
        "--no-auto-start",
        cwd=tmp_path,
        stdin=asyncio.subprocess.PIPE,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
    )
    assert process.stdin is not None
    assert process.stdout is not None

    async def request(
        request_id: int,
        method: str,
        params: dict[str, Any],
        notifications: list[dict[str, Any]] | None = None,
    ) -> dict[str, Any]:
        process.stdin.write(
            json.dumps(
                {
                    "jsonrpc": "2.0",
                    "id": request_id,
                    "method": method,
                    "params": params,
                }
            ).encode()
            + b"\n"
        )
        await process.stdin.drain()
        while True:
            line = await asyncio.wait_for(process.stdout.readline(), timeout=5)
            assert line, await process.stderr.read() if process.stderr else b""
            frame = json.loads(line)
            if frame.get("id") == request_id:
                return frame
            if notifications is not None:
                notifications.append(frame)

    async def notify(method: str, params: dict[str, Any]) -> None:
        process.stdin.write(
            json.dumps(
                {"jsonrpc": "2.0", "method": method, "params": params}
            ).encode()
            + b"\n"
        )
        await process.stdin.drain()

    try:
        initialized = await request(
            1,
            "initialize",
            {
                "protocolVersion": 2,
                "capabilities": {},
                "info": {"name": "daemon-v2-test", "version": "1"},
            },
        )
        assert initialized["result"]["protocolVersion"] == 2
        created = await request(
            2,
            "session/new",
            {
                "cwd": str(tmp_path),
                "additionalDirectories": [],
                "mcpServers": [],
            },
        )
        session_id = created["result"]["sessionId"]

        notifications: list[dict[str, Any]] = []
        acknowledged = await request(
            3,
            "session/prompt",
            {
                "sessionId": session_id,
                "prompt": [{"type": "text", "text": "hello"}],
            },
            notifications,
        )
        assert acknowledged["result"] == {}

        running = False
        idle = False
        while not idle:
            if notifications:
                frame = notifications.pop(0)
            else:
                line = await asyncio.wait_for(process.stdout.readline(), timeout=5)
                assert line
                frame = json.loads(line)
            if frame.get("method") != "session/update":
                continue
            update = frame["params"]["update"]
            if update.get("sessionUpdate") != "state_update":
                continue
            if update.get("state") == "running":
                running = True
            elif update.get("state") == "idle" and running:
                idle = True
                assert update["stopReason"] == "end_turn"
        assert running

        cancel_notifications: list[dict[str, Any]] = []
        cancel_acknowledged = await request(
            4,
            "session/prompt",
            {
                "sessionId": session_id,
                "prompt": [{"type": "text", "text": "wait-for-cancel"}],
            },
            cancel_notifications,
        )
        assert cancel_acknowledged["result"] == {}
        cancel_running = False
        while not cancel_running:
            if cancel_notifications:
                frame = cancel_notifications.pop(0)
            else:
                line = await asyncio.wait_for(process.stdout.readline(), timeout=5)
                assert line
                frame = json.loads(line)
            if frame.get("method") != "session/update":
                continue
            update = frame["params"]["update"]
            cancel_running = (
                update.get("sessionUpdate") == "state_update"
                and update.get("state") == "running"
            )

        await notify("session/cancel", {"sessionId": session_id})
        while True:
            line = await asyncio.wait_for(process.stdout.readline(), timeout=5)
            assert line
            frame = json.loads(line)
            if frame.get("method") != "session/update":
                continue
            update = frame["params"]["update"]
            if (
                update.get("sessionUpdate") == "state_update"
                and update.get("state") == "idle"
            ):
                assert update["stopReason"] == "cancelled"
                break

        closed = await request(
            5, "session/close", {"sessionId": session_id}
        )
        assert closed["result"] == {}
    finally:
        process.stdin.close()
        await process.stdin.wait_closed()
        await asyncio.wait_for(process.wait(), timeout=10)
        await daemon.close()
        store.close()


@pytest.mark.asyncio
async def test_daemon_enforces_connection_capacity_without_blocking_control_commands(
    tmp_path: Path,
) -> None:
    config = LocalACPConfig(
        config_path=tmp_path / "config.yaml",
        checkpointer_path=tmp_path / "checkpoints.db",
        session_store_path=tmp_path / "sessions.db",
        max_active_connections=1,
    )
    store = LocalACPSessionStore(config.session_store_path)
    store.setup()
    daemon = ACPDaemon(
        config, store, FakeRuntime(), tmp_path / "runtime", token="test-token"
    )  # type: ignore[arg-type]
    endpoint = await daemon.start()

    first_reader, first_writer, first = await connect(endpoint, "ACP")
    assert first == "OK"

    busy_reader, busy_writer, busy = await connect(endpoint, "ACP")
    assert busy == "BUSY"
    assert await busy_reader.read() == b""
    busy_writer.close()
    await busy_writer.wait_closed()

    status_reader, status_writer, status = await connect(endpoint, "STATUS")
    assert "connections=1" in status
    assert await status_reader.read() == b""
    status_writer.close()
    await status_writer.wait_closed()

    first_writer.close()
    await first_writer.wait_closed()
    await first_reader.read()
    await daemon.close()
    store.close()


@pytest.mark.asyncio
async def test_daemon_rejects_bad_token(tmp_path: Path) -> None:
    config = make_config(tmp_path)
    store = LocalACPSessionStore(config.session_store_path)
    store.setup()
    daemon = ACPDaemon(
        config, store, FakeRuntime(), tmp_path / "runtime", token="right"
    )  # type: ignore[arg-type]
    endpoint = await daemon.start()
    reader, writer = await asyncio.open_connection(endpoint.host, endpoint.port)
    writer.write(b"DFACP/1 wrong STATUS\n")
    await writer.drain()
    assert await reader.readline() == b"UNAUTHORIZED\n"
    writer.close()
    await writer.wait_closed()
    await daemon.close()
    store.close()


@pytest.mark.asyncio
async def test_daemon_management_json_roundtrip_and_invalid_request(
    tmp_path: Path,
) -> None:
    calls: list[dict[str, Any]] = []

    async def handler(request: dict[str, Any]) -> dict[str, Any]:
        calls.append(request)
        return {"ok": True, "data": {"echo": request}}

    config = make_config(tmp_path)
    store = LocalACPSessionStore(config.session_store_path)
    store.setup()
    daemon = ACPDaemon(
        config,
        store,
        FakeRuntime(),
        tmp_path / "runtime",
        token="test-token",
        management_handler=handler,
    )  # type: ignore[arg-type]
    endpoint = await daemon.start()

    request = {"operation": "proposal.list", "status": "pending_review"}
    assert await manage(endpoint, request) == {
        "ok": True,
        "data": {"echo": request},
    }
    assert calls == [request]

    reader, writer, response = await connect(endpoint, "MANAGE")
    assert response == "OK"
    writer.write(b"not-json\n")
    await writer.drain()
    invalid = json.loads(await reader.readline())
    assert invalid["ok"] is False
    assert invalid["code"] == "invalid_request"
    writer.close()
    await writer.wait_closed()

    await daemon.close()
    store.close()


@pytest.mark.asyncio
async def test_daemon_close_aborts_an_active_acp_bridge(tmp_path: Path) -> None:
    config = make_config(tmp_path)
    store = LocalACPSessionStore(config.session_store_path)
    store.setup()
    daemon = ACPDaemon(
        config, store, FakeRuntime(), tmp_path / "runtime", token="test-token"
    )  # type: ignore[arg-type]
    endpoint = await daemon.start()

    reader, writer, response = await connect(endpoint, "ACP")
    assert response == "OK"

    await asyncio.wait_for(daemon.close(), timeout=1)

    try:
        assert await asyncio.wait_for(reader.read(), timeout=1) == b""
    except ConnectionResetError:
        pass
    writer.close()
    store.close()
    assert daemon._connections == {}
    assert daemon._handlers == {}
    assert not daemon.endpoint_path.exists()
