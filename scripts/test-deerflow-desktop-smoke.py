#!/usr/bin/env python3
"""Exercise an extracted DeerFlow Desktop package without user data or remote APIs.

Uses only Python's standard library. Copies immutable package files into a new
repository .build-cache directory; never launches or modifies the supplied
package in place. All chat requests target an in-process loopback fake OpenAI
server. Logs and isolated state are retained for inspection.
"""
from __future__ import annotations

import argparse
import base64
import collections
import copy
import ctypes
import hashlib
import http.server
import json
import os
from pathlib import Path
import queue
import secrets
import shutil
import socket
import struct
import subprocess
import threading
import time
import traceback
import uuid

NIL = str(uuid.UUID(int=0))
REPLY = "DeerFlow Desktop local smoke reply."
MAX_MESSAGE = 48 * 1024 * 1024
CREATE_FLAGS = getattr(subprocess, "CREATE_NO_WINDOW", 0)


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def below(path: Path, root: Path) -> bool:
    return path.resolve().is_relative_to(root.resolve())


class JsonProcess:
    def __init__(self, args, env, cwd, logs: Path, name: str):
        self.name = name
        self.lines = queue.Queue()
        self.stderr = (logs / f"{name}.stderr.log").open("wb")
        self.stdout = (logs / f"{name}.stdout.log").open("wb")
        self.process = subprocess.Popen(
            [str(arg) for arg in args], cwd=cwd, env=env,
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=self.stderr,
            creationflags=CREATE_FLAGS,
        )
        self.reader = threading.Thread(target=self._read, daemon=True)
        self.reader.start()

    def _read(self):
        try:
            for line in iter(self.process.stdout.readline, b""):
                self.stdout.write(line)
                self.stdout.flush()
                self.lines.put(line)
        finally:
            self.lines.put(None)

    def receive(self, timeout=150):
        try:
            line = self.lines.get(timeout=timeout)
        except queue.Empty as exc:
            raise TimeoutError(f"{self.name} did not answer within {timeout}s") from exc
        if line is None:
            raise EOFError(f"{self.name} closed stdout (exit={self.process.poll()})")
        return json.loads(line.decode("utf-8-sig"))

    def send(self, message):
        self.process.stdin.write(json.dumps(message, ensure_ascii=False).encode("utf-8") + b"\n")
        self.process.stdin.flush()

    def stop(self):
        if self.process.poll() is None:
            self.process.stdin.close()
            try:
                self.process.wait(timeout=4)
            except subprocess.TimeoutExpired:
                self.process.terminate()
                try:
                    self.process.wait(timeout=4)
                except subprocess.TimeoutExpired:
                    self.process.kill()
                    self.process.wait(timeout=4)
        self.reader.join(timeout=2)
        self.stderr.close()
        self.stdout.close()


class WebSocket:
    """Minimal RFC 6455 client for the actual Waku /v1 JSON transport."""
    def __init__(self, address):
        host, port = address.rsplit(":", 1)
        self.socket = socket.create_connection((host.strip("[]"), int(port)), timeout=150)
        self.buffer = bytearray()
        key = base64.b64encode(secrets.token_bytes(16)).decode()
        request = (f"GET /v1 HTTP/1.1\r\nHost: {address}\r\nUpgrade: websocket\r\n"
                   f"Connection: Upgrade\r\nSec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n")
        self.socket.sendall(request.encode("ascii"))
        while b"\r\n\r\n" not in self.buffer:
            data = self.socket.recv(4096)
            require(bool(data), "Daemon closed during WebSocket handshake")
            self.buffer.extend(data)
            require(len(self.buffer) < 65536, "Oversized WebSocket headers")
        headers, remainder = bytes(self.buffer).split(b"\r\n\r\n", 1)
        self.buffer = bytearray(remainder)
        require(headers.splitlines()[0].split()[1] == b"101", f"WebSocket upgrade failed: {headers!r}")
        fields = dict(line.decode().split(":", 1) for line in headers.splitlines()[1:])
        fields = {key.lower(): value.strip() for key, value in fields.items()}
        accept = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
        require(fields.get("sec-websocket-accept") == accept, "WebSocket accept hash mismatch")

    def _read(self, count):
        while len(self.buffer) < count:
            data = self.socket.recv(max(4096, count - len(self.buffer)))
            if not data:
                raise EOFError("Waku daemon closed the WebSocket")
            self.buffer.extend(data)
        result = bytes(self.buffer[:count])
        del self.buffer[:count]
        return result

    def _send_frame(self, opcode, payload):
        mask = secrets.token_bytes(4)
        count = len(payload)
        header = bytes([0x80 | opcode])
        if count < 126:
            header += bytes([0x80 | count])
        elif count < 65536:
            header += b"\xfe" + struct.pack("!H", count)
        else:
            header += b"\xff" + struct.pack("!Q", count)
        self.socket.sendall(header + mask + bytes(value ^ mask[i % 4] for i, value in enumerate(payload)))

    def send(self, data):
        self._send_frame(1, json.dumps(data, ensure_ascii=False).encode("utf-8"))

    def receive(self, timeout=150):
        self.socket.settimeout(timeout)
        chunks = bytearray()
        while True:
            flags, length = self._read(2)
            opcode, final = flags & 15, bool(flags & 0x80)
            masked, count = bool(length & 0x80), length & 127
            if count == 126:
                count = struct.unpack("!H", self._read(2))[0]
            elif count == 127:
                count = struct.unpack("!Q", self._read(8))[0]
            require(count <= MAX_MESSAGE, "Oversized daemon WebSocket frame")
            mask = self._read(4) if masked else b""
            payload = self._read(count)
            if masked:
                payload = bytes(value ^ mask[i % 4] for i, value in enumerate(payload))
            if opcode == 8:
                raise EOFError("Waku daemon sent a close frame")
            if opcode == 9:
                self._send_frame(10, payload)
                continue
            if opcode == 10:
                continue
            require(opcode in (0, 1), f"Unexpected WebSocket opcode {opcode}")
            chunks.extend(payload)
            require(len(chunks) <= MAX_MESSAGE, "Oversized daemon message")
            if final:
                return json.loads(chunks.decode("utf-8"))

    def close(self):
        try:
            self._send_frame(8, b"")
        except OSError:
            pass
        self.socket.close()


class Waku:
    def __init__(self, address, token, protocol, log):
        self.ws = WebSocket(address)
        self.events = collections.deque()
        self.log = log
        self.ws.send({"type": "hello", "protocolVersion": protocol,
                      "token": token, "clientId": str(uuid.uuid4()), "resumeFrom": []})
        hello = self.ws.receive()
        require(hello.get("type") == "hello" and hello.get("protocolVersion") == protocol, f"Hello failed: {hello}")
        self.log("Waku WebSocket Hello", protocol=protocol)

    def rpc(self, command, session=NIL, runtime=NIL):
        request = str(uuid.uuid4())
        self.ws.send({"type": "request", "requestId": request, "sessionId": session,
                      "runtimeId": runtime, "command": command})
        deadline = time.monotonic() + 160
        while time.monotonic() < deadline:
            message = self.ws.receive(max(1, deadline - time.monotonic()))
            if message.get("type") == "event":
                self.events.append(message)
            if message.get("type") == "response" and message.get("requestId") == request:
                outcome = message["outcome"]
                require(outcome.get("status") == "ok", f"RPC {command['type']} failed: {outcome.get('error')}")
                return outcome["payload"]
        raise TimeoutError(f"RPC {command['type']} timed out")

    def deerflow(self, operation, data=None):
        start = time.monotonic()
        response = self.rpc({"type": "deerFlow", "operation": operation, "input": data or {}})
        require(response.get("type") == "deerFlow", f"Wrong DeerFlow response: {response}")
        self.log(f"DeerFlow {operation}", elapsed_ms=round((time.monotonic() - start) * 1000, 1))
        return response["data"]

    def event(self, runtime, kind, timeout=120, collect=None, on_permission=None):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            message = self.events.popleft() if self.events else self.ws.receive(max(1, deadline - time.monotonic()))
            if message.get("type") != "event" or message.get("runtimeId") != runtime:
                continue
            event = message["event"]
            if collect is not None:
                collect.append(event)
            if event["kind"] == "error":
                raise AssertionError(f"ACP driver error: {event['payload']}")
            if event["kind"] == kind:
                return event["payload"]
            if event["kind"] == "permission" and on_permission is not None:
                on_permission(event["payload"])
            if event["kind"] == "processExited" and kind != "processExited":
                raise AssertionError("ACP runtime exited before the expected event")
        raise TimeoutError(f"Missing {kind} for runtime {runtime}")


class ACP:
    def __init__(self, process):
        self.process = process
        self.sequence = 0

    def rpc(self, method, params):
        self.sequence += 1
        request = self.sequence
        self.process.send({"jsonrpc": "2.0", "id": request, "method": method, "params": params})
        while True:
            message = self.process.receive()
            if "method" in message and "id" in message:
                self.process.send({"jsonrpc": "2.0", "id": message["id"],
                                   "error": {"code": -32601, "message": "Smoke client has no tools"}})
            if message.get("id") == request and ("result" in message or "error" in message):
                require("error" not in message, f"ACP {method} failed: {message.get('error')}")
                return message["result"]

    def initialize(self):
        response = self.rpc("initialize", {"protocolVersion": 1, "clientCapabilities": {"terminal": False},
                                           "clientInfo": {"name": "deerflow-desktop-smoke", "version": "1"}})
        require(response["protocolVersion"] == 1, "Bridge did not negotiate ACP v1")
        return response


class FakeOpenAI(http.server.ThreadingHTTPServer):
    daemon_threads = True
    def __init__(self, log, request_log):
        self._requests = []
        self._tool_plans = {}
        self._requests_lock = threading.Lock()
        self.log = log
        self.request_log = request_log
        super().__init__(("127.0.0.1", 0), FakeHandler)

    def record_request(self, body):
        # Record only synthetic test bodies, never request headers/credentials.
        with self._requests_lock:
            self._requests.append(copy.deepcopy(body))
            with self.request_log.open("a", encoding="utf-8") as stream:
                stream.write(json.dumps(body, ensure_ascii=False) + "\n")

    @property
    def requests_seen(self):
        with self._requests_lock:
            return len(self._requests)

    def requests_since(self, index):
        with self._requests_lock:
            return copy.deepcopy(self._requests[index:])

    def plan_write(self, filename, content):
        require(Path(filename).name == filename and filename not in ("", ".", ".."),
                "Synthetic writes must use a single workspace filename")
        marker = f"smoke-write-{uuid.uuid4().hex}"
        call = {"id": f"call_{uuid.uuid4().hex}", "type": "function", "function": {
            "name": "write_file", "arguments": json.dumps({
                "description": "Write the isolated desktop approval smoke fixture",
                "path": f"/mnt/user-data/workspace/{filename}", "content": content,
            })}}
        with self._requests_lock:
            self._tool_plans[marker] = call
        return marker

    def planned_tool_call(self, body):
        messages = body.get("messages", [])
        latest_user = next((index for index in range(len(messages) - 1, -1, -1)
                            if messages[index].get("role") == "user"), None)
        if latest_user is None:
            return None
        text = message_text(messages[latest_user])
        with self._requests_lock:
            call = next((copy.deepcopy(call) for marker, call in self._tool_plans.items()
                         if marker in text), None)
        if call is None:
            return None
        # The final assistant reply must follow the actual tool result, including
        # a denied result. Never keep reissuing a denied write in another turn.
        if any(message.get("role") == "tool" and message.get("tool_call_id") == call["id"]
               for message in messages[latest_user + 1:]):
            return None
        # Title/background requests can quote the same user marker without
        # offering tools. Keep those requests as ordinary synthetic replies;
        # the real turn's file/permission assertions still require tool use.
        if not any(tool.get("function", {}).get("name") == "write_file"
                   for tool in body.get("tools", [])):
            return None
        return call


def message_text(message):
    content = message.get("content")
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "".join(block.get("text", "") for block in content
                       if isinstance(block, dict) and isinstance(block.get("text"), str))
    return ""


class FakeHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_POST(self):
        size = int(self.headers.get("Content-Length", "0"))
        if size > 8 * 1024 * 1024 or self.path not in ("/v1/chat/completions", "/chat/completions"):
            self.send_error(400)
            return
        body = json.loads(self.rfile.read(size))
        self.server.record_request(body)
        self.server.log("Local fake OpenAI request", path=self.path, model=body.get("model"), stream=body.get("stream", False))
        common = {"id": "chatcmpl-smoke", "created": int(time.time()), "model": "smoke-local"}
        tool_call = self.server.planned_tool_call(body)
        finish_reason = "tool_calls" if tool_call else "stop"
        message = {"role": "assistant", "content": None if tool_call else REPLY}
        if tool_call:
            message["tool_calls"] = [tool_call]
        if body.get("stream"):
            delta = copy.deepcopy(message)
            if tool_call:
                delta["tool_calls"][0]["index"] = 0
            pieces = [
                {**common, "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": delta, "finish_reason": None}]},
                {**common, "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": {}, "finish_reason": finish_reason}], "usage": {"prompt_tokens": 8, "completion_tokens": 8, "total_tokens": 16}},
            ]
            payload = ("".join("data: " + json.dumps(piece) + "\n\n" for piece in pieces) + "data: [DONE]\n\n").encode()
            media = "text/event-stream"
        else:
            payload = json.dumps({**common, "object": "chat.completion", "choices": [{"index": 0, "message": message, "finish_reason": finish_reason}], "usage": {"prompt_tokens": 8, "completion_tokens": 8, "total_tokens": 16}}).encode()
            media = "application/json"
        self.send_response(200)
        self.send_header("Content-Type", media)
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(payload)
        self.wfile.flush()


def isolated_environment(root):
    # Build an allowlist, rather than inheriting provider credentials or PYTHONPATH.
    keep = {"SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "NUMBER_OF_PROCESSORS",
            "PROCESSOR_ARCHITECTURE", "PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMDATA", "OS"}
    env = {key: value for key, value in os.environ.items() if key.upper() in keep}
    profile = root / "smoke-profile"
    for name in ("home", "temp", "roaming", "local", "cache", "config"):
        (profile / name).mkdir(parents=True, exist_ok=True)
    env.update({
        "PATH": os.pathsep.join([str(root / "runtime"), str(root), str(Path(env.get("SYSTEMROOT", "C:/Windows")) / "System32")]),
        "HOME": str(profile / "home"), "USERPROFILE": str(profile / "home"),
        "APPDATA": str(profile / "roaming"), "LOCALAPPDATA": str(profile / "local"),
        "XDG_CONFIG_HOME": str(profile / "config"), "XDG_CACHE_HOME": str(profile / "cache"),
        "TEMP": str(profile / "temp"), "TMP": str(profile / "temp"),
        "DEER_FLOW_PORTABLE_ROOT": str(root), "DEER_FLOW_CONFIG_PATH": str(root / "user-data/config/config.yaml"),
        "DEER_FLOW_ACP_RUNTIME_DIR": str(root / "user-data/runtime/acp"),
        "DEER_FLOW_ACP_PYTHON": str(root / "runtime/python.exe"),
        "PYTHONUTF8": "1", "PYTHONIOENCODING": "utf-8", "PYTHONDONTWRITEBYTECODE": "1",
        "PYTHONNOUSERSITE": "1", "WAKU_APP_EXECUTABLE": str(root / "deerflow-desktop.exe"),
        "LANGCHAIN_TRACING_V2": "false", "LANGSMITH_TRACING": "false", "NO_PROXY": "127.0.0.1,localhost",
    })
    return env


def stop_verified_python(endpoint, interpreter, config, log):
    """Last-resort Windows cleanup; never terminate an unverified PID."""
    if os.name != "nt" or not endpoint.is_file():
        return
    data = json.loads(endpoint.read_text(encoding="utf-8"))
    require(Path(data["config_path"]).resolve() == config.resolve(), "Cleanup endpoint points outside smoke config")
    from ctypes import wintypes
    kernel = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel.OpenProcess.argtypes = [wintypes.DWORD, wintypes.BOOL, wintypes.DWORD]
    kernel.OpenProcess.restype = wintypes.HANDLE
    kernel.QueryFullProcessImageNameW.argtypes = [wintypes.HANDLE, wintypes.DWORD, wintypes.LPWSTR, ctypes.POINTER(wintypes.DWORD)]
    kernel.TerminateProcess.argtypes = [wintypes.HANDLE, wintypes.UINT]
    kernel.CloseHandle.argtypes = [wintypes.HANDLE]
    handle = kernel.OpenProcess(0x1000 | 0x0001, False, int(data["pid"]))
    if not handle:
        return
    try:
        size = wintypes.DWORD(32768)
        path = ctypes.create_unicode_buffer(size.value)
        require(kernel.QueryFullProcessImageNameW(handle, 0, path, ctypes.byref(size)), "Could not verify cleanup PID")
        require(Path(path.value).resolve() == interpreter.resolve(), "Refusing to terminate a process outside copied smoke runtime")
        require(kernel.TerminateProcess(handle, 1), "Failed to stop smoke Python daemon")
        log("Force-stopped verified smoke Python daemon", pid=data["pid"])
    finally:
        kernel.CloseHandle(handle)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--package", required=True, type=Path, help="Fresh extracted Windows Desktop package")
    parser.add_argument("--no-chat", action="store_true", help="Skip the optional local fake-model conversation checks")
    args = parser.parse_args()
    require(os.name == "nt", "This smoke test targets the Windows portable package")
    package = args.package.resolve(strict=True)
    repo = Path(__file__).resolve().parents[1]
    cache = repo / ".build-cache" / "desktop-smoke"
    run = cache / (time.strftime("%Y%m%d-%H%M%S") + "-" + uuid.uuid4().hex[:8])
    root, logs = run / "package", run / "logs"
    require(below(root, cache), "Smoke output escaped repository build cache")
    require(not below(root, package), "Smoke output must not be inside the supplied package")
    for item in ("runtime/python.exe", "resources/default-config.yaml", "waku-daemon.exe", "deerflow-acp.exe", "deerflow-desktop.exe"):
        require((package / item).is_file(), f"Incomplete package: {item}")
    root.mkdir(parents=True)
    logs.mkdir()
    report = {"package": str(package), "smoke_root": str(root), "ok": False, "checks": [], "cleanup_errors": []}
    output_lock = threading.Lock()
    def log(check, **detail):
        entry = {"time": time.strftime("%H:%M:%S"), "check": check, **detail}
        with output_lock:
            report["checks"].append(entry)
            with (logs / "checks.jsonl").open("a", encoding="utf-8") as stream:
                stream.write(json.dumps(entry, ensure_ascii=False) + "\n")
        print(json.dumps(entry, ensure_ascii=False), flush=True)
    print(f"Isolated smoke logs: {logs}", flush=True)
    processes, waku, fake, env = [], None, None, None
    bridge_args = []
    error = None
    try:
        # No user-data, root config.yaml, .env, or files outside these immutable trees.
        for directory in ("runtime", "resources"):
            for current, dirs, files in os.walk(package / directory, followlinks=False):
                for name in dirs + files:
                    source = Path(current) / name
                    require(not source.is_symlink() and not source.is_junction(), f"Package contains a reparse point: {source}")
            shutil.copytree(package / directory, root / directory)
        for name in ("waku-daemon.exe", "deerflow-acp.exe", "deerflow-desktop.exe"):
            shutil.copy2(package / name, root / name)
        log("Copied immutable package allowlist")
        env = isolated_environment(root)
        config = root / "user-data/config/config.yaml"
        runtime = root / "user-data/runtime/acp"
        interpreter = root / "runtime/python.exe"
        bridge_args = [root / "deerflow-acp.exe", "--config", config, "--python", interpreter, "--runtime-dir", runtime]
        token = secrets.token_hex(24)
        daemon_env = {**env, "WAKU_DAEMON_TOKEN": token}
        process = JsonProcess([root / "waku-daemon.exe", "--bind", "127.0.0.1:0", "--parent-pid", os.getpid()], daemon_env, root, logs, "waku-daemon")
        processes.append(process)
        ready = process.receive(30)
        require(ready["pid"] == process.process.pid, "Readiness PID mismatch")
        waku = Waku(ready["address"], token, ready["protocolVersion"], log)
        snapshot = waku.deerflow("snapshot")
        require(below(Path(snapshot["paths"]["config"]), root), "Snapshot config escaped isolated package")
        require(snapshot["models"], "Default snapshot contains no model")
        document = copy.deepcopy(snapshot)
        for model in document["models"]:
            model.update(api_key="", clear_api_key=True, base_url="http://127.0.0.1:9/v1")
        document["memory"]["enabled"] = False
        document["skill_evolution"]["enabled"] = False
        document["skills_enabled"] = False
        document["subagents"]["enabled"] = False
        document["runtime"]["subagent_enabled"] = False
        require(waku.deerflow("validate", document).get("valid") is True, "No-key config validation failed")
        saved = waku.deerflow("save", document)
        require(all(not model.get("api_key_configured") for model in saved["models"]), "No-key save retained a credential")
        require(not waku.deerflow("status").get("running"), "Fresh isolated service unexpectedly running")
        invalid = copy.deepcopy(saved)
        invalid["models"][0]["name"] = ""
        try:
            waku.deerflow("save-and-apply", invalid)
        except AssertionError:
            pass
        else:
            raise AssertionError("Invalid save-and-apply was accepted")
        require(not waku.deerflow("status").get("running"), "Invalid save started the runtime")
        require(waku.deerflow("snapshot")["config_revision"] == saved["config_revision"], "Invalid save changed the config")
        log("Invalid save-and-apply preserves config and stopped runtime")
        if not args.no_chat:
            fake = FakeOpenAI(log, logs / "model-requests.jsonl")
            threading.Thread(target=fake.serve_forever, daemon=True).start()
            document = copy.deepcopy(saved)
            document["models"] = [{"original_name": "", "name": "smoke-local", "display_name": "Local smoke model",
                "use_path": "langchain_openai:ChatOpenAI", "model": "smoke-local", "api_key": "smoke-local-placeholder",
                "clear_api_key": False, "base_url": f"http://127.0.0.1:{fake.server_port}/v1",
                "supports_thinking": False, "supports_reasoning_effort": False, "supports_vision": False, "advanced": {}}]
            document["default_model"] = "smoke-local"
            document["runtime"]["model_name"] = "smoke-local"
            saved = waku.deerflow("save", document)
        def settled():
            deadline = time.monotonic() + 150
            while time.monotonic() < deadline:
                state = waku.deerflow("status")
                require(not state.get("apply_error"), f"Service apply failed: {state.get('apply_error')}")
                if not state.get("applying") and state.get("running"):
                    return state
                time.sleep(0.5)
            raise TimeoutError("Service did not start/apply within 150 seconds")
        waku.deerflow("start")
        state = settled()
        old_generation = state.get("applied_generation")
        def acp_client(name):
            process = JsonProcess(bridge_args, env, root, logs, name)
            processes.append(process)
            client = ACP(process)
            client.initialize()
            return client
        workspace = root / "smoke-workspace"
        workspace.mkdir()
        acp = acp_client("acp-before-apply")
        original = acp.rpc("session/new", {"cwd": str(workspace), "mcpServers": []})["sessionId"]
        probe = acp.rpc("session/new", {"cwd": str(workspace), "mcpServers": []})["sessionId"]
        acp.rpc("session/close", {"sessionId": probe})
        listed = acp.rpc("session/list", {})["sessions"]
        require(original in {item["sessionId"] for item in listed} and probe not in {item["sessionId"] for item in listed}, "Closing a probe corrupted the session catalog")
        acp.rpc("session/load", {"sessionId": original, "cwd": str(workspace), "mcpServers": []})
        try:
            waku.deerflow("manage", {"operation": "session.delete", "session_id": original})
        except AssertionError as exc:
            require("客户端" in str(exc), "Unexpected attached-session deletion error")
        else:
            raise AssertionError("Management deleted an attached session")
        log("ACP initialize/new/list/load/close", original_session=original)
        session = str(uuid.uuid4())
        def start_chat(cursor=None):
            runtime_id = str(uuid.uuid4())
            prompt_marker = f"smoke-turn-{runtime_id}"
            options = {"provider": "deerFlow", "binary": str(root / "deerflow-acp.exe"), "cwd": str(workspace),
                       "mode": "ask", "model": "smoke-local", "reasoningEffort": "off", "serviceTier": None,
                       "contextWindow": None, "agentPreset": None, "computerUseEnabled": False, "providerCursor": cursor}
            response = waku.rpc({"type": "start", "options": options}, session, runtime_id)
            require(response["type"] == "started", "Waku Start did not create a runtime")
            actual_cursor = waku.event(runtime_id, "connected")
            if cursor:
                require(actual_cursor == cursor, "Reconnect changed the native ACP session ID")
            request_start = fake.requests_seen
            waku.rpc({"type": "prompt", "prompt": f"{prompt_marker}: Reply with the local smoke test phrase only.",
                      "turnId": str(uuid.uuid4()), "messageId": str(uuid.uuid4())}, session, runtime_id)
            events = []
            finished = waku.event(runtime_id, "turnFinished", collect=events)
            require(finished.get("success") is True, f"Local fake-model turn failed: {finished}")
            text = "".join(event["payload"] for event in events if event["kind"] == "textDelta")
            require(REPLY in text, f"Missing synthetic assistant reply: {text!r}")
            # Match this turn's unique user marker, so an unrelated background
            # request cannot satisfy the restored conversation assertion.
            turn_messages = None
            current_user_index = None
            for request in fake.requests_since(request_start):
                messages = request.get("messages", [])
                indices = [index for index, message in enumerate(messages)
                           if message.get("role") == "user" and prompt_marker in message_text(message)]
                if indices:
                    turn_messages, current_user_index = messages, indices[-1]
                    break
            require(turn_messages is not None, "The local model did not receive this turn's user message")
            if cursor:
                require(any(message.get("role") == "assistant" and REPLY in message_text(message)
                            for message in turn_messages[:current_user_index]),
                        "The post-apply model request lost the first turn's assistant history")
                log("Post-apply request retains prior assistant history", native_session=actual_cursor["sessionId"],
                    model_message_count=len(turn_messages))
            log("Waku Start/Prompt/TextDelta/TurnFinished", runtime_id=runtime_id, native_session=actual_cursor["sessionId"])
            return runtime_id, actual_cursor
        approval_session = str(uuid.uuid4())
        def start_approval_session(cursor=None):
            runtime_id = str(uuid.uuid4())
            # A restored session must retain the backend's Ask setting even
            # though a stale desktop snapshot requests Full Access again.
            options = {"provider": "deerFlow", "binary": str(root / "deerflow-acp.exe"), "cwd": str(workspace),
                       "mode": "fullAccess", "model": "smoke-local", "reasoningEffort": "off", "serviceTier": None,
                       "contextWindow": None, "agentPreset": None, "computerUseEnabled": False, "providerCursor": cursor}
            response = waku.rpc({"type": "start", "options": options}, approval_session, runtime_id)
            require(response["type"] == "started", "Approval smoke did not create a runtime")
            events = []
            actual_cursor = waku.event(runtime_id, "connected", collect=events)
            if cursor:
                require(actual_cursor == cursor, "Approval reconnect changed the native ACP session ID")
            modes = [event["payload"] for event in events if event["kind"] == "toolApprovalChanged"]
            actual_mode = modes[-1] if modes else waku.event(runtime_id, "toolApprovalChanged")
            expected_mode = "ask" if cursor else "allow_always"
            require(actual_mode == expected_mode,
                    f"Backend approval mode {actual_mode!r} differs from {expected_mode!r}")
            log("Backend approval mode confirmed", native_session=actual_cursor["sessionId"],
                mode=actual_mode, restored=bool(cursor))
            return runtime_id, actual_cursor

        def set_approval(runtime_id, mode):
            response = waku.rpc({"type": "setToolApproval", "mode": mode}, approval_session, runtime_id)
            require(response.get("type") == "toolApproval" and response.get("mode") == mode,
                    f"Backend did not acknowledge the requested approval mode: {response}")
            log("Approval change acknowledged", mode=mode)

        def write_turn(runtime_id, name, decision=None, prior_history=False):
            path = workspace / name
            require(below(path, workspace) and not path.exists(), "Approval fixture must be new and isolated")
            content = f"Isolated approval fixture {uuid.uuid4()}\n"
            marker = fake.plan_write(name, content)
            request_start = fake.requests_seen
            permission_count = 0
            def respond(permission):
                nonlocal permission_count
                permission_count += 1
                require(decision is not None, "Full Access/cached approval unexpectedly requested permission")
                require(permission_count == 1, "A single synthetic write requested permission repeatedly")
                option = next((option for option in permission.get("options", [])
                               if option["id"].endswith(":" + decision)), None)
                require(option is not None, f"Missing permission choice {decision}: {permission}")
                response = waku.rpc({"type": "respond", "requestId": permission["requestId"],
                                     "optionId": option["id"]}, approval_session, runtime_id)
                require(response.get("type") == "ack", "Permission response was not acknowledged")
            waku.rpc({"type": "prompt", "prompt": f"{marker}: Perform the isolated smoke write, then report completion.",
                      "turnId": str(uuid.uuid4()), "messageId": str(uuid.uuid4())}, approval_session, runtime_id)
            events = []
            finished = waku.event(runtime_id, "turnFinished", collect=events, on_permission=respond)
            require(finished.get("success") is True, f"Approval smoke turn failed: {finished}")
            require(permission_count == (0 if decision is None else 1),
                    f"Expected approval decision {decision!r}; received {permission_count} requests")
            requests = fake.requests_since(request_start)
            turn_messages = next((request.get("messages", []) for request in requests
                                  if any(tool.get("function", {}).get("name") == "write_file"
                                         for tool in request.get("tools", []))
                                  and any(message.get("role") == "user" and marker in message_text(message)
                                          for message in request.get("messages", []))), None)
            require(turn_messages is not None, "Synthetic write did not reach the local model")
            if prior_history:
                user_index = next(index for index, message in enumerate(turn_messages)
                                  if message.get("role") == "user" and marker in message_text(message))
                require(any(message.get("role") == "assistant" and REPLY in message_text(message)
                            for message in turn_messages[:user_index]),
                        "Approval mode reconnect lost the previous assistant history")
            denied = decision in ("reject_once", "reject_always")
            if denied:
                require(not path.exists(), f"Rejected write changed the workspace: {path}")
            else:
                require(path.is_file() and path.read_text(encoding="utf-8") == content,
                        f"Allowed write did not produce the exact isolated fixture: {path}")
            log("Tool approval enforced on real write_file", filename=name, decision=decision or "automatic",
                permission_count=permission_count, wrote=not denied, restored_history=prior_history)

        if fake:
            old_runtime, cursor = start_chat()
            approval_runtime, approval_cursor = start_approval_session()
            write_turn(approval_runtime, "full-access.txt")
            set_approval(approval_runtime, "ask")
            write_turn(approval_runtime, "ask-rejected.txt", "reject_once")
            write_turn(approval_runtime, "ask-allowed-once.txt", "allow_once")
            write_turn(approval_runtime, "ask-allowed-always.txt", "allow_always")
            write_turn(approval_runtime, "cached-allow.txt")
            set_approval(approval_runtime, "ask")
            write_turn(approval_runtime, "ask-cleared-allow-cache.txt", "reject_once")
            write_turn(approval_runtime, "ask-rejected-always.txt", "reject_always")
            set_approval(approval_runtime, "ask")
            write_turn(approval_runtime, "ask-cleared-reject-cache.txt", "allow_once")
            log("Explicit Ask clears both allow-always and reject-always tool caches")
        changed = copy.deepcopy(saved)
        changed["models"][0]["display_name"] = "Applied smoke model"
        applied = waku.deerflow("save-and-apply", changed)
        saved = applied["document"]
        require(saved["models"][0]["display_name"] == "Applied smoke model", "Save-and-apply did not return the saved model")
        require(not applied["status"].get("apply_error"), "Save-and-apply could not schedule the restart")
        state = settled()
        require(state["config_revision"] == saved["config_revision"], "Runtime did not apply the saved revision")
        if old_generation is not None:
            require(state["applied_generation"] > old_generation, "Apply generation did not advance")
        acp.process.process.wait(timeout=15)
        log("Apply closed the old idle ACP connection")
        acp_after = acp_client("acp-after-apply")
        require(original in {item["sessionId"] for item in acp_after.rpc("session/list", {})["sessions"]}, "Apply lost stored sessions")
        acp_after.rpc("session/load", {"sessionId": original, "cwd": str(workspace), "mcpServers": []})
        acp_after.rpc("session/close", {"sessionId": original})
        log("ACP session restored after apply")
        deleted = waku.deerflow("manage", {"operation": "session.delete", "session_id": original})
        require(original in deleted["deleted"], "Management did not confirm history deletion")
        require(original not in {item["session_id"] for item in waku.deerflow("manage", {"operation": "session.list"})["sessions"]}, "Deleted history reappeared in the list")
        retried = waku.deerflow("manage", {"operation": "session.delete", "session_id": original})
        require(retried.get("already_deleted") is True, "History deletion was not retryable")
        log("History deletion confirmed and retryable")
        desktop_id, project_id = str(uuid.uuid4()), str(uuid.uuid4())
        now = int(time.time())
        deleted_row = {"id": desktop_id, "project_id": project_id, "title": "Delete regression",
                       "provider": "deerFlow", "runtime_mode": "ask", "status": "idle",
                       "created_at": now, "updated_at": now,
                       "provider_cursor": {"provider": "deerFlow", "sessionId": original}}
        old_save = {"type": "saveTaskState", "projects": [{"id": project_id, "name": "Smoke", "path": str(workspace), "created_at": now}],
                    "liveSessionIds": [desktop_id], "sessions": [deleted_row]}
        waku.rpc(old_save)
        require(desktop_id in {item["id"] for item in waku.rpc({"type": "loadTaskState"})["sessions"]}, "Desktop fixture was not persisted")
        require(waku.rpc({"type": "removeSession"}, desktop_id)["type"] == "ack", "Desktop deletion was not acknowledged")
        waku.rpc(old_save)
        require(desktop_id not in {item["id"] for item in waku.rpc({"type": "loadTaskState"})["sessions"]}, "Stale save resurrected a deleted desktop row")
        log("Desktop deletion acknowledged; stale save cannot resurrect it")
        if fake:
            deadline = time.monotonic() + 5
            while True:
                attached = waku.rpc({"type": "attachSession"}, session)
                if attached.get("runtimeId") is None:
                    break
                require(time.monotonic() < deadline, "Waku backend retained a dead runtime after apply")
                time.sleep(0.2)
            current_runtime, restored = start_chat(cursor)
            waku.rpc({"type": "closeSession"}, session, current_runtime)
            approval_runtime, restored_approval = start_approval_session(approval_cursor)
            write_turn(approval_runtime, "restored-ask-rejected.txt", "reject_once", prior_history=True)
            waku.rpc({"type": "closeSession"}, approval_session, approval_runtime)
            require(fake.requests_seen >= 2, "Expected both turns to reach the local fake server")
        acp_after.process.stop()
        waku.deerflow("stop")
        require(not waku.deerflow("status").get("running"), "Stop did not stop the service")
        # Exercise the daemon's real discovery path after Stop; checking status
        # alone would miss a queued catalog bridge that auto-starts the service.
        probe = waku.rpc({"type": "probeProvider", "provider": "deerFlow",
                          "binaryOverride": str(root / "deerflow-acp.exe"),
                          "discoverModels": True, "probeVersion": False})
        require(probe.get("type") == "providerProbe", "Unexpected model-discovery response")
        provider = probe["probe"]
        require(provider.get("provider") == "deerFlow" and provider.get("installed") is True,
                "Model discovery did not probe the installed DeerFlow bridge")
        require(Path(provider["path"]).resolve() == (root / "deerflow-acp.exe").resolve(),
                "Model discovery used a bridge outside the isolated package")
        require(not waku.deerflow("status").get("running"), "Model discovery restarted the stopped service")
        log("Stopped service stays stopped after actual model discovery", cached_models=len(provider.get("models", [])))
        time.sleep(1)
        require(not waku.deerflow("status").get("running"), "Stopped service restarted unexpectedly")
        log("Stop remains stopped")
        waku.ws.close()
        process.stop()
        process = JsonProcess([root / "waku-daemon.exe", "--bind", "127.0.0.1:0", "--parent-pid", os.getpid()], daemon_env, root, logs, "waku-daemon-restarted")
        processes.append(process)
        ready = process.receive(30)
        waku = Waku(ready["address"], token, ready["protocolVersion"], log)
        require(desktop_id not in {item["id"] for item in waku.rpc({"type": "loadTaskState"})["sessions"]}, "Deleted desktop row reappeared after restart")
        log("Desktop deletion survives daemon restart")
        report["ok"] = True
    except BaseException as exc:
        error = exc
        report["error"] = f"{type(exc).__name__}: {exc}"
        (logs / "failure.txt").write_text(traceback.format_exc(), encoding="utf-8")
    finally:
        if waku:
            try:
                waku.deerflow("cancel-apply")
                waku.ws.send({"type": "shutdown"})
            except Exception as exc:
                report["cleanup_errors"].append(f"Waku shutdown: {exc}")
            try:
                waku.ws.close()
            except Exception:
                pass
        for process in reversed(processes):
            try:
                process.stop()
            except Exception as exc:
                report["cleanup_errors"].append(f"{process.name}: {exc}")
        if bridge_args and env:
            try:
                # Explicit isolated config/runtime arguments prevent touching another daemon.
                result = subprocess.run([str(arg) for arg in bridge_args] + ["--stop-daemon"], cwd=root, env=env,
                    capture_output=True, timeout=40, creationflags=CREATE_FLAGS)
                (logs / "cleanup-bridge.log").write_bytes(result.stdout + result.stderr)
                if result.returncode:
                    stop_verified_python(root / "user-data/runtime/acp/endpoint.json", root / "runtime/python.exe",
                                         root / "user-data/config/config.yaml", log)
            except Exception as exc:
                report["cleanup_errors"].append(f"Python daemon: {exc}")
        if fake:
            fake.shutdown()
            fake.server_close()
        if report["cleanup_errors"]:
            report["ok"] = False
        (logs / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
    print(json.dumps({"ok": report["ok"], "logs": str(logs), "error": report.get("error"),
                      "cleanup_errors": report["cleanup_errors"]}, ensure_ascii=False), flush=True)
    return 0 if report["ok"] and error is None else 1


if __name__ == "__main__":
    raise SystemExit(main())
