"""Small, strict ACP v2 client used by the standalone Buzz sidecar."""

from __future__ import annotations

import asyncio
import json
import logging
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

logger = logging.getLogger(__name__)


class ACPV2Error(RuntimeError):
    """ACP transport, protocol, or agent error."""


@dataclass(slots=True)
class _TextProjection:
    order: list[str] = field(default_factory=list)
    messages: dict[str, list[dict[str, Any]]] = field(default_factory=dict)

    def _content(self, message_id: str) -> list[dict[str, Any]]:
        if message_id not in self.messages:
            self.order.append(message_id)
            self.messages[message_id] = []
        return self.messages[message_id]

    def apply(self, update: Mapping[str, Any]) -> None:
        kind = update.get("sessionUpdate")
        message_id = update.get("messageId")
        if not isinstance(message_id, str) or not message_id:
            return
        if kind == "agent_message_chunk":
            content = update.get("content")
            if isinstance(content, dict):
                self._content(message_id).append(dict(content))
            return
        if kind != "agent_message":
            return
        if "content" not in update:
            return
        content = update.get("content")
        target = self._content(message_id)
        if content is None:
            target.clear()
        elif isinstance(content, list):
            target[:] = [dict(block) for block in content if isinstance(block, dict)]

    def text(self) -> str:
        parts: list[str] = []
        for message_id in self.order:
            for block in self.messages.get(message_id, []):
                kind = block.get("type")
                if kind == "text" and isinstance(block.get("text"), str):
                    parts.append(block["text"])
                elif kind == "resource_link" and isinstance(block.get("uri"), str):
                    name = block.get("name") or block["uri"]
                    parts.append(f"\n[{name}]({block['uri']})\n")
        return "".join(parts).strip()


@dataclass(slots=True)
class _TurnTracker:
    projection: _TextProjection = field(default_factory=_TextProjection)
    running: bool = False
    done: asyncio.Future[dict[str, Any]] | None = None
    on_update: Callable[[dict[str, Any]], None] | None = None

    def apply(self, update: Mapping[str, Any]) -> None:
        kind = update.get("sessionUpdate")
        if kind == "state_update":
            state = update.get("state")
            if state == "running":
                self.running = True
                return
            if state == "idle" and self.running and self.done is not None:
                if not self.done.done():
                    self.done.set_result(dict(update))
                return
        if self.running:
            self.projection.apply(update)

    def emit(self, update: Mapping[str, Any]) -> None:
        if self.on_update is None:
            return
        try:
            self.on_update(dict(update))
        except Exception:
            logger.exception("ACP v2 update observer callback failed")


class _JsonRpcProcess:
    def __init__(
        self,
        command: str,
        args: list[str],
        cwd: Path,
        *,
        permission_handler,
    ) -> None:
        self.command = command
        self.args = list(args)
        self.cwd = cwd
        self.permission_handler = permission_handler
        self.process: asyncio.subprocess.Process | None = None
        self._pending: dict[int | str, asyncio.Future[Any]] = {}
        self._turns: dict[str, _TurnTracker] = {}
        self._next_id = 0
        self._write_lock = asyncio.Lock()
        self._reader_task: asyncio.Task[None] | None = None
        self._stderr_task: asyncio.Task[None] | None = None

    async def open(self) -> None:
        if self.process is not None:
            return
        process = await asyncio.create_subprocess_exec(
            self.command,
            *self.args,
            cwd=self.cwd,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        self.process = process
        self._reader_task = asyncio.create_task(
            self._read_stdout(process), name="deerflow-acp-v2-stdout"
        )
        self._stderr_task = asyncio.create_task(
            self._read_stderr(process), name="deerflow-acp-v2-stderr"
        )

    async def close(self) -> None:
        process, self.process = self.process, None
        reader, self._reader_task = self._reader_task, None
        stderr, self._stderr_task = self._stderr_task, None
        if process is not None:
            if process.stdin is not None:
                process.stdin.close()
                try:
                    await process.stdin.wait_closed()
                except (BrokenPipeError, ConnectionResetError):
                    pass
            try:
                await asyncio.wait_for(process.wait(), timeout=5)
            except TimeoutError:
                process.terminate()
                try:
                    await asyncio.wait_for(process.wait(), timeout=5)
                except TimeoutError:
                    process.kill()
                    await process.wait()
        for task in (reader, stderr):
            if task is not None:
                task.cancel()
        if reader is not None or stderr is not None:
            await asyncio.gather(
                *(task for task in (reader, stderr) if task is not None),
                return_exceptions=True,
            )
        self._fail_all(ACPV2Error("ACP v2 connection closed"))

    def install_turn(
        self,
        session_id: str,
        on_update: Callable[[dict[str, Any]], None] | None = None,
    ) -> _TurnTracker:
        if session_id in self._turns:
            raise ACPV2Error(f"session {session_id} already has an active prompt")
        tracker = _TurnTracker(
            done=asyncio.get_running_loop().create_future(), on_update=on_update
        )
        self._turns[session_id] = tracker
        return tracker

    def remove_turn(self, session_id: str) -> None:
        self._turns.pop(session_id, None)

    async def request(self, method: str, params: Mapping[str, Any]) -> Any:
        process = self._require_process()
        self._next_id += 1
        request_id = self._next_id
        future: asyncio.Future[Any] = asyncio.get_running_loop().create_future()
        self._pending[request_id] = future
        try:
            await self._write(
                {
                    "jsonrpc": "2.0",
                    "id": request_id,
                    "method": method,
                    "params": dict(params),
                }
            )
            if process.returncode is not None:
                raise ACPV2Error(
                    f"ACP v2 process exited with code {process.returncode}"
                )
            return await future
        finally:
            self._pending.pop(request_id, None)

    async def notify(self, method: str, params: Mapping[str, Any]) -> None:
        self._require_process()
        await self._write({"jsonrpc": "2.0", "method": method, "params": dict(params)})

    async def _write(self, message: Mapping[str, Any]) -> None:
        process = self._require_process()
        if process.stdin is None:
            raise ACPV2Error("ACP v2 stdin is unavailable")
        payload = json.dumps(message, separators=(",", ":"), ensure_ascii=False)
        async with self._write_lock:
            process.stdin.write(payload.encode("utf-8") + b"\n")
            try:
                await process.stdin.drain()
            except (BrokenPipeError, ConnectionResetError) as exc:
                raise ACPV2Error("ACP v2 process stopped reading stdin") from exc

    def _require_process(self) -> asyncio.subprocess.Process:
        if self.process is None:
            raise ACPV2Error("ACP v2 client is not open")
        return self.process

    async def _read_stdout(self, process: asyncio.subprocess.Process) -> None:
        assert process.stdout is not None
        failure: BaseException | None = None
        try:
            while line := await process.stdout.readline():
                try:
                    frame = json.loads(line)
                except json.JSONDecodeError as exc:
                    raise ACPV2Error("ACP v2 process emitted invalid JSON") from exc
                messages = frame if isinstance(frame, list) else [frame]
                for message in messages:
                    if isinstance(message, dict):
                        await self._dispatch(message)
        except asyncio.CancelledError:
            raise
        except BaseException as exc:  # noqa: BLE001 - propagated to every waiter
            failure = exc
        finally:
            if failure is None and self.process is process:
                failure = ACPV2Error(
                    f"ACP v2 process exited with code {await process.wait()}"
                )
            if failure is not None:
                self._fail_all(failure)

    async def _read_stderr(self, process: asyncio.subprocess.Process) -> None:
        assert process.stderr is not None
        while line := await process.stderr.readline():
            logger.info("deerflow-acp-v2: %s", line.decode("utf-8", "replace").rstrip())

    async def _dispatch(self, message: dict[str, Any]) -> None:
        if "id" in message and ("result" in message or "error" in message):
            future = self._pending.get(message.get("id"))
            if future is None or future.done():
                return
            error = message.get("error")
            if isinstance(error, dict):
                code = error.get("code", -32000)
                detail = error.get("data", error.get("message", "Agent error"))
                future.set_exception(ACPV2Error(f"ACP error {code}: {detail}"))
            else:
                future.set_result(message.get("result"))
            return

        method = message.get("method")
        params = message.get("params")
        if not isinstance(method, str) or not isinstance(params, dict):
            return
        if method == "session/update":
            session_id = params.get("sessionId")
            update = params.get("update")
            if isinstance(session_id, str) and isinstance(update, dict):
                tracker = self._turns.get(session_id)
                if tracker is not None:
                    tracker.apply(update)
                    tracker.emit(update)
            return
        if "id" not in message:
            return
        if method == "session/request_permission":
            try:
                result = self.permission_handler(params)
                if asyncio.iscoroutine(result):
                    result = await result
                await self._write(
                    {"jsonrpc": "2.0", "id": message["id"], "result": result}
                )
            except BaseException as exc:  # noqa: BLE001 - turn into JSON-RPC error
                await self._write(
                    {
                        "jsonrpc": "2.0",
                        "id": message["id"],
                        "error": {
                            "code": -32603,
                            "message": "Permission handler failed",
                            "data": str(exc),
                        },
                    }
                )
            return
        await self._write(
            {
                "jsonrpc": "2.0",
                "id": message["id"],
                "error": {"code": -32601, "message": f"Method not found: {method}"},
            }
        )

    def _fail_all(self, error: BaseException) -> None:
        for future in list(self._pending.values()):
            if not future.done():
                future.set_exception(error)
        for tracker in list(self._turns.values()):
            if tracker.done is not None and not tracker.done.done():
                tracker.done.set_exception(error)


class DeerFlowACPV2Client:
    """Persistent ACP v2 client with v2 state-update completion semantics."""

    def __init__(
        self,
        command: str,
        args: list[str],
        workspace: Path,
        *,
        timeout_seconds: float = 600,
    ) -> None:
        self.command = command
        self.args = list(args)
        self.workspace = workspace
        self.timeout_seconds = timeout_seconds
        self._transport = _JsonRpcProcess(
            command,
            args,
            workspace,
            permission_handler=self._permission_response,
        )
        self._attached_sessions: set[str] = set()

    async def open(self) -> None:
        if self._transport.process is not None:
            return
        await self._transport.open()
        try:
            async with asyncio.timeout(self.timeout_seconds):
                response = await self._transport.request(
                    "initialize",
                    {
                        "protocolVersion": 2,
                        "info": {
                            "name": "buzz-deerflow-adapter",
                            "title": "Buzz DeerFlow Adapter",
                            "version": "0.2.0",
                        },
                        "capabilities": {},
                    },
                )
            if not isinstance(response, dict) or response.get("protocolVersion") != 2:
                raise ACPV2Error("DeerFlow did not negotiate ACP protocol version 2")
            capabilities = response.get("capabilities")
            if not isinstance(capabilities, dict) or not isinstance(
                capabilities.get("session"), dict
            ):
                raise ACPV2Error("DeerFlow ACP v2 did not advertise session support")
            logger.info("DeerFlow ACP connected: protocol=v2")
        except BaseException:
            await self._transport.close()
            raise

    async def close(self) -> None:
        self._attached_sessions.clear()
        await self._transport.close()

    async def attach_or_create(self, existing_session_id: str | None) -> str:
        if existing_session_id in self._attached_sessions:
            return existing_session_id
        if existing_session_id:
            try:
                async with asyncio.timeout(self.timeout_seconds):
                    await self._transport.request(
                        "session/resume",
                        {
                            "sessionId": existing_session_id,
                            "cwd": str(self.workspace),
                            "additionalDirectories": [],
                            "mcpServers": [],
                        },
                    )
                self._attached_sessions.add(existing_session_id)
                return existing_session_id
            except Exception as exc:  # noqa: BLE001 - invalid persisted session fallback
                logger.info(
                    "Could not resume ACP v2 session %s; creating a replacement: %s",
                    existing_session_id,
                    type(exc).__name__,
                )
        async with asyncio.timeout(self.timeout_seconds):
            response = await self._transport.request(
                "session/new",
                {
                    "cwd": str(self.workspace),
                    "additionalDirectories": [],
                    "mcpServers": [],
                },
            )
        if not isinstance(response, dict) or not isinstance(
            response.get("sessionId"), str
        ):
            raise ACPV2Error("session/new response did not contain sessionId")
        session_id = response["sessionId"]
        self._attached_sessions.add(session_id)
        return session_id

    async def prompt(
        self,
        session_id: str,
        prompt: str,
        *,
        on_update: Callable[[dict[str, Any]], None] | None = None,
    ) -> str:
        tracker = self._transport.install_turn(session_id, on_update=on_update)
        deadline = time.monotonic() + self.timeout_seconds
        try:
            await asyncio.wait_for(
                self._transport.request(
                    "session/prompt",
                    {
                        "sessionId": session_id,
                        "prompt": [{"type": "text", "text": prompt}],
                    },
                ),
                timeout=max(0.001, deadline - time.monotonic()),
            )
            if tracker.done is None:
                raise AssertionError("turn tracker has no completion future")
            idle = await asyncio.wait_for(
                asyncio.shield(tracker.done),
                timeout=max(0.001, deadline - time.monotonic()),
            )
        except TimeoutError:
            await self._cancel_and_drain(session_id, tracker)
            raise TimeoutError(
                f"DeerFlow ACP v2 prompt exceeded {self.timeout_seconds:g}s"
            ) from None
        finally:
            self._transport.remove_turn(session_id)

        stop_reason = idle.get("stopReason")
        if stop_reason == "_deerflow_error":
            meta = idle.get("_meta")
            detail = meta.get("deerflowError") if isinstance(meta, dict) else None
            raise ACPV2Error(f"DeerFlow prompt failed: {detail or 'unknown error'}")
        if stop_reason == "cancelled":
            raise ACPV2Error("DeerFlow prompt was cancelled")
        return tracker.projection.text()

    async def _cancel_and_drain(self, session_id: str, tracker: _TurnTracker) -> None:
        try:
            await self._transport.notify("session/cancel", {"sessionId": session_id})
        except Exception:  # noqa: BLE001 - cancellation is best effort
            return
        if tracker.done is None or tracker.done.done():
            return
        try:
            await asyncio.wait_for(asyncio.shield(tracker.done), timeout=15)
        except Exception:  # noqa: BLE001 - cancellation is best effort
            logger.warning(
                "ACP v2 session %s did not become idle after cancel", session_id
            )

    @staticmethod
    def _permission_response(params: Mapping[str, Any]) -> dict[str, Any]:
        options = params.get("options")
        if isinstance(options, list):
            for preferred in ("allow_once", "allow_always"):
                for option in options:
                    if not isinstance(option, dict) or option.get("kind") != preferred:
                        continue
                    option_id = option.get("optionId")
                    if isinstance(option_id, str):
                        return {
                            "outcome": {
                                "outcome": "selected",
                                "optionId": option_id,
                            }
                        }
        return {"outcome": {"outcome": "cancelled"}}
