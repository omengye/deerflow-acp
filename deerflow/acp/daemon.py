"""Long-lived local ACP daemon used by the native stdio bridge."""

from __future__ import annotations

import argparse
import asyncio
import hmac
import importlib.metadata
import json
import logging
import os
import secrets
import signal
import sys
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from logging.handlers import RotatingFileHandler
from pathlib import Path
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from .agent import DeerFlowACPAgent

import acp

from .config import LocalACPConfig
from .daemon_endpoint import (
    ENDPOINT_FILENAME,
    LOCK_FILENAME,
    DaemonAlreadyRunning,
    DaemonEndpoint,
    SingleInstanceLock,
    ensure_runtime_dir,
    get_runtime_dir,
)
from .proposal_control import handle_proposal_management_request
from .runtime import LocalACPRuntime
from .session_cleanup import cleanup_expired_sessions, run_session_cleanup_loop
from .session_store import LocalACPSessionStore

logger = logging.getLogger(__name__)
_HANDSHAKE_VERSION = "DFACP/1"
_HANDSHAKE_LIMIT = 4096
_HANDSHAKE_TIMEOUT_SECONDS = 3.0
_WRITER_CLOSE_TIMEOUT_SECONDS = 1.0
_ACTIVE_TASK_CLOSE_TIMEOUT_SECONDS = 3.0
_MANAGEMENT_REQUEST_LIMIT = 64 * 1024
_MANAGEMENT_REQUEST_TIMEOUT_SECONDS = 10.0


@dataclass(slots=True)
class _ACPConnectionState:
    connection_id: str
    task: asyncio.Task[Any]
    writer: asyncio.StreamWriter
    agent: DeerFlowACPAgent | None = None


def _build_id() -> str:
    try:
        return importlib.metadata.version("deerflow-api")
    except importlib.metadata.PackageNotFoundError:
        return "dev"


def _env_enabled(name: str, default: bool = True) -> bool:
    raw = os.getenv(name)
    if raw is None:
        return default
    return raw.strip().lower() not in {"0", "false", "no", "off"}


def _abort_writer(writer: asyncio.StreamWriter) -> None:
    """Abort a socket transport without waiting for the peer to cooperate."""

    transport = getattr(writer, "transport", None)
    abort = getattr(transport, "abort", None)
    if callable(abort):
        abort()
    else:
        writer.close()


async def _close_writer(writer: asyncio.StreamWriter) -> None:
    writer.close()
    try:
        await asyncio.wait_for(
            writer.wait_closed(), timeout=_WRITER_CLOSE_TIMEOUT_SECONDS
        )
    except TimeoutError:
        logger.debug(
            "Timed out waiting for ACP socket peer to close; aborting transport"
        )
        _abort_writer(writer)
    except (ConnectionError, OSError, RuntimeError):
        pass


def _default_agent_factory(
    config: LocalACPConfig,
    store: LocalACPSessionStore,
    runtime: LocalACPRuntime,
    *,
    connection_id: str | None = None,
) -> DeerFlowACPAgent:
    from .agent import DeerFlowACPAgent

    return DeerFlowACPAgent(
        config,
        store,
        runtime,
        connection_id=connection_id,
    )


class ACPDaemon:
    """Serve multiple local ACP clients while reusing the expensive runtime."""

    def __init__(
        self,
        config: LocalACPConfig,
        store: LocalACPSessionStore,
        runtime: LocalACPRuntime,
        runtime_dir: Path,
        *,
        agent_factory: Callable[..., DeerFlowACPAgent] | None = None,
        management_handler: Callable[
            [dict[str, Any]], Awaitable[dict[str, Any]]
        ] = handle_proposal_management_request,
        token: str | None = None,
    ) -> None:
        self.config = config
        import hashlib
        self.config_revision = hashlib.sha256(config.config_path.read_bytes()).hexdigest() if config.config_path.is_file() else ""
        self.store = store
        self.runtime = runtime
        self.warmup_state = "ready"
        self.runtime_dir = ensure_runtime_dir(runtime_dir)
        self.endpoint_path = self.runtime_dir / ENDPOINT_FILENAME
        self.token = token or secrets.token_urlsafe(32)
        self.agent_factory = agent_factory or _default_agent_factory
        self.management_handler = management_handler
        self.endpoint: DaemonEndpoint | None = None
        self._server: asyncio.Server | None = None
        self._state_lock = asyncio.Lock()
        self._close_lock = asyncio.Lock()
        self._management_lock = asyncio.Lock()
        self._handlers: dict[asyncio.Task[Any], asyncio.StreamWriter] = {}
        self._connections: dict[str, _ACPConnectionState] = {}
        self._next_connection_id = 0
        self._stop_requested = asyncio.Event()
        self._closed = False

    @property
    def stop_requested(self) -> asyncio.Event:
        return self._stop_requested

    def request_stop(self) -> None:
        self._stop_requested.set()

    async def start(self) -> DaemonEndpoint:
        if self._server is not None:
            assert self.endpoint is not None
            return self.endpoint
        self._server = await asyncio.start_server(
            self._handle_connection, "127.0.0.1", 0
        )
        socket = self._server.sockets[0]
        host, port = socket.getsockname()[:2]
        endpoint = DaemonEndpoint(
            host=str(host),
            port=int(port),
            token=self.token,
            pid=os.getpid(),
            build_id=_build_id(),
            config_path=str(self.config.config_path.resolve()),
        )
        endpoint.publish(self.endpoint_path)
        self.endpoint = endpoint
        logger.info("ACP daemon ready on %s:%d", endpoint.host, endpoint.port)
        return endpoint

    async def wait(self) -> None:
        await self._stop_requested.wait()

    async def close(self) -> None:
        async with self._close_lock:
            async with self._state_lock:
                if self._closed:
                    return
                self._closed = True
                server = self._server
                self._server = None
                handlers = [
                    task
                    for task in self._handlers
                    if task is not asyncio.current_task() and not task.done()
                ]
                writers = list(
                    {id(writer): writer for writer in self._handlers.values()}.values()
                )

            if server is not None:
                server.close()
            # On Windows, StreamWriter.wait_closed() may wait indefinitely for
            # native bridges that are simultaneously waiting for daemon EOF.
            for writer in writers:
                _abort_writer(writer)
            for task in handlers:
                task.cancel()
            if handlers:
                try:
                    await asyncio.wait_for(
                        asyncio.gather(*handlers, return_exceptions=True),
                        timeout=_ACTIVE_TASK_CLOSE_TIMEOUT_SECONDS,
                    )
                except TimeoutError:
                    logger.warning(
                        "Timed out waiting for %d ACP connection handler(s) to close",
                        len(handlers),
                    )
            if server is not None:
                try:
                    await asyncio.wait_for(
                        server.wait_closed(),
                        timeout=_ACTIVE_TASK_CLOSE_TIMEOUT_SECONDS,
                    )
                except TimeoutError:
                    logger.warning("Timed out waiting for the ACP listener to close")
            try:
                current = DaemonEndpoint.load(self.endpoint_path)
            except (
                FileNotFoundError,
                KeyError,
                TypeError,
                ValueError,
                json.JSONDecodeError,
            ):
                current = None
            if (
                current is not None
                and current.pid == os.getpid()
                and current.token == self.token
            ):
                self.endpoint_path.unlink(missing_ok=True)
            logger.info("ACP daemon stopped")

    async def _read_handshake(
        self, reader: asyncio.StreamReader
    ) -> tuple[str, str] | None:
        try:
            raw = await asyncio.wait_for(
                reader.readline(), timeout=_HANDSHAKE_TIMEOUT_SECONDS
            )
        except TimeoutError:
            return None
        if not raw or len(raw) > _HANDSHAKE_LIMIT:
            return None
        try:
            version, token, command = raw.decode("utf-8").strip().split(" ", 2)
        except (UnicodeDecodeError, ValueError):
            return None
        if version != _HANDSHAKE_VERSION or not hmac.compare_digest(token, self.token):
            return None
        command_parts = command.split()
        if len(command_parts) != 1:
            return None
        return command_parts[0].upper(), token

    async def _reply(self, writer: asyncio.StreamWriter, value: str) -> None:
        writer.write(value.encode("utf-8") + b"\n")
        await writer.drain()

    async def _handle_management_request(
        self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter
    ) -> None:
        await self._reply(writer, "OK")
        try:
            raw = await asyncio.wait_for(
                reader.readline(), timeout=_MANAGEMENT_REQUEST_TIMEOUT_SECONDS
            )
            if not raw:
                raise ValueError("Management request is empty.")
            if len(raw) > _MANAGEMENT_REQUEST_LIMIT or not raw.endswith(b"\n"):
                raise ValueError("Management request exceeds the 64 KiB limit.")
            request = json.loads(raw)
            if not isinstance(request, dict):
                raise ValueError("Management request must be a JSON object.")
            async with self._management_lock:
                operation = str(request.get("operation", ""))
                if operation.startswith(("daemon.", "session.", "memory.")):
                    from .management import handle_runtime_management
                    response = await handle_runtime_management(self, request)
                else:
                    response = await self.management_handler(request)
            if not isinstance(response, dict):
                raise TypeError("Management handler returned an invalid response.")
        except ValueError as exc:
            response = {
                "ok": False,
                "error": str(exc),
                "code": "invalid_request",
            }
        except Exception as exc:
            logger.exception("ACP management request failed")
            response = {"ok": False, "error": str(exc), "code": "internal_error"}
        await self._reply(
            writer,
            json.dumps(response, ensure_ascii=False, separators=(",", ":")),
        )

    async def _handle_connection(
        self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter
    ) -> None:
        task = asyncio.current_task()
        if task is None:
            await _close_writer(writer)
            return
        async with self._state_lock:
            if self._closed:
                rejected_during_close = True
            else:
                rejected_during_close = False
                self._handlers[task] = writer
        if rejected_during_close:
            await _close_writer(writer)
            return

        try:
            handshake = await self._read_handshake(reader)
            if handshake is None:
                await self._reply(writer, "UNAUTHORIZED")
                return
            command, _token = handshake
            if command == "STATUS":
                async with self._state_lock:
                    active_count = len(self._connections)
                await self._reply(
                    writer,
                    f"OK {os.getpid()} {_build_id()} connections={active_count}",
                )
                return
            if command == "STOP":
                await self._reply(writer, "OK")
                self.request_stop()
                return
            if command == "MANAGE":
                await self._handle_management_request(reader, writer)
                return
            if command != "ACP":
                await self._reply(writer, "ERROR unsupported-command")
                return

            connection: _ACPConnectionState | None = None
            async with self._state_lock:
                if self._closed:
                    rejection = "STOPPING"
                elif len(self._connections) >= self.config.max_active_connections:
                    rejection = "BUSY"
                else:
                    rejection = None
                    self._next_connection_id += 1
                    connection_id = f"acp-{self._next_connection_id}"
                    connection = _ACPConnectionState(connection_id, task, writer)
                    self._connections[connection_id] = connection
            if rejection is not None:
                await self._reply(writer, rejection)
                return

            assert connection is not None
            agent: DeerFlowACPAgent | None = None
            try:
                agent = self.agent_factory(
                    self.config,
                    self.store,
                    self.runtime,
                    connection_id=connection.connection_id,
                )
                connection.agent = agent
                await self._reply(writer, "OK")
                async with self._state_lock:
                    active_count = len(self._connections)
                logger.info(
                    "ACP connection %s opened (%d/%d)",
                    connection.connection_id,
                    active_count,
                    self.config.max_active_connections,
                )
                await acp.run_agent(
                    agent,
                    input_stream=writer,
                    output_stream=reader,
                    # The local Rust v2 facade maps v2 session/close onto the
                    # SDK 0.10 v1 close route, which that SDK still gates as
                    # unstable. Other unstable methods remain explicit
                    # method-not-found handlers in DeerFlowACPAgent.
                    use_unstable_protocol=True,
                )
            except (ConnectionError, asyncio.IncompleteReadError):
                logger.debug("ACP bridge %s disconnected", connection.connection_id)
            finally:
                try:
                    if agent is not None:
                        await agent.shutdown()
                finally:
                    async with self._state_lock:
                        self._connections.pop(connection.connection_id, None)
                        remaining = len(self._connections)
                    logger.info(
                        "ACP connection %s closed (%d remaining)",
                        connection.connection_id,
                        remaining,
                    )
        finally:
            async with self._state_lock:
                self._handlers.pop(task, None)
            await _close_writer(writer)


def _install_signal_handlers(daemon: ACPDaemon) -> None:
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(sig, daemon.request_stop)
        except (NotImplementedError, RuntimeError):
            # Windows console Ctrl+C is still handled by KeyboardInterrupt in main().
            pass


async def _run_daemon(
    config_path: str | None,
    runtime_dir: Path,
    *,
    warmup: bool,
) -> None:
    config = LocalACPConfig.from_file(config_path)
    config.prepare_environment()
    # Saving a draft must not auto-reload global model/tool settings mid-turn.
    from deerflow.config.app_config import get_app_config, set_app_config
    set_app_config(get_app_config())
    from deerflow.config.agents_config import freeze_agent_catalog
    freeze_agent_catalog()
    runtime = LocalACPRuntime(config)
    runtime._pinned_config = get_app_config()
    runtime.validate_sandbox_provider()
    store = LocalACPSessionStore(config.session_store_path)
    store.setup()
    daemon = ACPDaemon(config, store, runtime, runtime_dir)
    warmup_task: asyncio.Task[None] | None = None
    cleanup_task: asyncio.Task[None] | None = None
    try:
        await runtime.open()
        await cleanup_expired_sessions(config, store, runtime, compact=True)
        cleanup_task = asyncio.create_task(
            run_session_cleanup_loop(config, store, runtime)
        )
        await daemon.start()
        _install_signal_handlers(daemon)
        if warmup:
            daemon.warmup_state = "warming"
            async def _do_warmup() -> None:
                try:
                    logger.info("Warming DeerFlow agent graph in background")
                    await runtime.warmup()
                    daemon.warmup_state = "ready"
                    logger.info("DeerFlow agent graph warmup complete")
                except asyncio.CancelledError:
                    pass
                except Exception:
                    daemon.warmup_state = "failed"
                    logger.exception("Background agent graph warmup failed")

            warmup_task = asyncio.create_task(_do_warmup())
        await daemon.wait()
    finally:
        if cleanup_task is not None and not cleanup_task.done():
            cleanup_task.cancel()
            try:
                await cleanup_task
            except asyncio.CancelledError:
                pass
        if warmup_task is not None and not warmup_task.done():
            warmup_task.cancel()
            try:
                await warmup_task
            except asyncio.CancelledError:
                pass
        await daemon.close()
        await runtime.close()
        store.close()
        try:
            from deerflow.sandbox.sandbox_provider import shutdown_sandbox_provider

            shutdown_sandbox_provider()
        except Exception:
            logger.exception("Failed to shut down sandbox provider")


def _configure_logging(runtime_dir: Path, level: str) -> None:
    numeric_level = getattr(logging, level.upper(), logging.INFO)
    formatter = logging.Formatter("%(asctime)s %(levelname)s %(name)s: %(message)s")
    root = logging.getLogger()
    root.setLevel(numeric_level)
    stderr_handler = logging.StreamHandler(sys.stderr)
    stderr_handler.setFormatter(formatter)
    file_handler = RotatingFileHandler(
        runtime_dir / "daemon.log",
        maxBytes=5 * 1024 * 1024,
        backupCount=2,
        encoding="utf-8",
    )
    file_handler.setFormatter(formatter)
    root.handlers.clear()
    root.addHandler(stderr_handler)
    root.addHandler(file_handler)


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Run the long-lived local DeerFlow ACP daemon"
    )
    parser.add_argument("--config", help="Path to DeerFlow config.yaml")
    parser.add_argument(
        "--runtime-dir", help="Override the per-user daemon discovery directory"
    )
    parser.add_argument(
        "--no-warmup", action="store_true", help="Skip agent graph warmup"
    )
    parser.add_argument("--log-level", default="INFO")
    return parser


def main() -> None:
    args = _parser().parse_args()
    runtime_dir = ensure_runtime_dir(get_runtime_dir(args.runtime_dir))
    lock = SingleInstanceLock(runtime_dir / LOCK_FILENAME)
    try:
        with lock:
            # Acquire the cross-process lock before opening the rotating log;
            # simultaneous editor startups may briefly spawn multiple daemon
            # candidates, but only the winner should own daemon.log.
            _configure_logging(runtime_dir, args.log_level)
            # Holding the lock proves that a pre-existing endpoint is stale.
            (runtime_dir / ENDPOINT_FILENAME).unlink(missing_ok=True)
            asyncio.run(
                _run_daemon(
                    args.config,
                    runtime_dir,
                    warmup=(
                        not args.no_warmup
                        and _env_enabled("DEER_FLOW_ACP_DAEMON_WARMUP")
                    ),
                )
            )
    except DaemonAlreadyRunning as exc:
        logger.error("%s", exc)
        raise SystemExit(2) from exc
    except KeyboardInterrupt:
        pass
    except BaseException:
        # Startup failures (bad config, missing keys, import errors) otherwise
        # surface only on stderr, which the bridge redirects to NUL when it
        # spawns the daemon -- leaving clients with an opaque timeout. Log the
        # traceback to daemon.log so the failure is diagnosable.
        logger.exception("ACP daemon failed to start")
        raise


if __name__ == "__main__":
    main()
