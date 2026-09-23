"""Owned command execution and machine-readable outcomes.

Remote Linux commands run under a per-command supervisor. Closing its input
or sending a cancellation request stops that command's process group; it never
terminates a shared container or WSL distribution.
"""

from __future__ import annotations

import json
import math
import os
import signal
import subprocess
import threading
import time
import uuid
from dataclasses import dataclass
from typing import Literal

CommandStatus = Literal["completed", "timed_out", "cancelled", "unknown"]
DEFAULT_CAPTURE_LIMIT_BYTES = 10 * 1024 * 1024
COMMAND_CLEANUP_SECONDS = 5.0


@dataclass(frozen=True)
class CommandResult:
    output: str
    exit_code: int | None
    status: CommandStatus = "completed"
    termination_confirmed: bool = True

    @property
    def succeeded(self) -> bool:
        return self.status == "completed" and self.exit_code == 0 and self.termination_confirmed

    def to_dict(self) -> dict[str, object]:
        return {
            "output": self.output,
            "exit_code": self.exit_code,
            "status": self.status,
            "termination_confirmed": self.termination_confirmed,
        }


def validate_command_timeout(value: float) -> float:
    timeout = float(value)
    if not math.isfinite(timeout) or timeout <= 0:
        raise ValueError("command timeout must be positive and finite")
    return timeout


def format_command_output(stdout: str, stderr: str, exit_code: int | None, status: CommandStatus, *, timeout: float | None = None) -> str:
    output = stdout
    if stderr:
        output += f"\nStd Error:\n{stderr}" if output else stderr
    if status == "timed_out":
        notice = f"Command timed out after {timeout:g} seconds and was terminated." if timeout is not None else "Command timed out and was terminated."
        output = (output + "\n" if output else "") + notice + "\nExit Code: 124"
    elif status == "cancelled":
        output = (output + "\n" if output else "") + "Command cancelled and terminated.\nExit Code: 130"
    elif status == "unknown":
        output = (output + "\n" if output else "") + "Error: Command outcome is unknown; termination could not be confirmed. The command was not retried."
    elif exit_code not in (0, None):
        output += f"\nExit Code: {exit_code}"
    return output if output else "(no output)"


class _Capture:
    def __init__(self, limit: int):
        self.limit = max(0, limit)
        self.kept = bytearray()
        self.total = 0
        self.tail = bytearray()
        self.lock = threading.Lock()

    def drain(self, stream) -> None:
        try:
            while data := os.read(stream.fileno(), 65536):
                with self.lock:
                    self.total += len(data)
                    self.kept.extend(data[:max(0, self.limit - len(self.kept))])
                    self.tail.extend(data)
                    del self.tail[:-8192]
        except (OSError, ValueError):
            pass
        finally:
            # Only the reader closes its pipe. Closing it on the caller thread
            # can wait for an inherited writer indefinitely on Windows.
            stream.close()

    def text(self, encoding: str) -> str:
        with self.lock:
            result = self.kept.decode(encoding, errors="replace")
            if self.total > len(self.kept):
                result += f"\n... [output truncated after {len(self.kept)} of {self.total} bytes; remaining output discarded] ..."
            return result


def _terminate_host_tree(process: subprocess.Popen) -> bool:
    tree_confirmed = True
    if os.name == "nt":
        executable = os.path.join(os.environ.get("SystemRoot", r"C:\Windows"), "System32", "taskkill.exe")
        try:
            result = subprocess.run([executable, "/PID", str(process.pid), "/T", "/F"], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=COMMAND_CLEANUP_SECONDS, check=False)
            tree_confirmed = result.returncode == 0
        except (OSError, subprocess.TimeoutExpired):
            tree_confirmed = False
    else:
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        except OSError:
            return False
        try:
            process.wait(timeout=0.5)
        except subprocess.TimeoutExpired:
            pass
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        except OSError:
            return False
    if process.poll() is None:
        try:
            process.kill()
        except OSError:
            pass
    try:
        process.wait(timeout=COMMAND_CLEANUP_SECONDS)
    except subprocess.TimeoutExpired:
        return False
    return tree_confirmed and process.poll() is not None


def run_host_command(
    argv: list[str], *, timeout: float, cancel_event: threading.Event | None = None,
    env: dict[str, str] | None = None, encoding: str = "utf-8",
    capture_limit: int = DEFAULT_CAPTURE_LIMIT_BYTES, remote_token: str | None = None,
    command_timeout: float | None = None,
) -> CommandResult:
    """Capture bounded output, own the child until completion or cleanup.

    For a remote supervisor, only its per-command final receipt confirms remote
    termination. Killing the host transport process alone never makes that claim.
    """
    timeout = validate_command_timeout(timeout)
    if cancel_event is not None and cancel_event.is_set():
        return CommandResult("Command cancelled before execution.", None, "cancelled", True)
    process = subprocess.Popen(
        argv, shell=False, stdin=subprocess.PIPE if remote_token else subprocess.DEVNULL,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env,
        start_new_session=os.name != "nt",
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0) if os.name == "nt" else 0,
    )
    captures = [_Capture(capture_limit), _Capture(capture_limit)]
    streams = [process.stdout, process.stderr]
    readers = [threading.Thread(target=capture.drain, args=(stream,), daemon=True) for capture, stream in zip(captures, streams, strict=True)]
    for reader in readers:
        reader.start()
    started = time.monotonic()
    requested: CommandStatus | None = None
    confirmed = True
    streams_drained = False
    try:
        while process.poll() is None:
            if cancel_event is not None and cancel_event.is_set():
                requested = "cancelled"
            elif time.monotonic() - started >= timeout:
                requested = "timed_out"
            if requested is not None:
                if remote_token:
                    try:
                        process.stdin.write((requested + "\n").encode("ascii"))
                        process.stdin.flush()
                        process.stdin.close()
                    except (OSError, ValueError):
                        pass
                    try:
                        process.wait(timeout=COMMAND_CLEANUP_SECONDS)
                    except subprocess.TimeoutExpired:
                        _terminate_host_tree(process)
                else:
                    confirmed = _terminate_host_tree(process)
                break
            try:
                process.wait(timeout=0.05)
            except subprocess.TimeoutExpired:
                pass
    except BaseException:
        if remote_token and process.stdin is not None:
            try:
                process.stdin.close()
            except OSError:
                pass
        _terminate_host_tree(process)
        raise
    finally:
        if process.stdin is not None and not process.stdin.closed:
            process.stdin.close()
        for reader in readers:
            reader.join(timeout=0.5)
        streams_drained = all(not reader.is_alive() for reader in readers)

    stdout, stderr = (capture.text(encoding) for capture in captures)
    if os.name == "nt":
        stdout = stdout.replace("\r\n", "\n").replace("\r", "\n")
        stderr = stderr.replace("\r\n", "\n").replace("\r", "\n")
    status: CommandStatus = requested or "completed"
    exit_code = process.returncode
    if remote_token:
        marker = f"__DEERFLOW_COMMAND_{remote_token}__"
        tail = captures[1].tail.decode("utf-8", errors="replace")
        _, found, payload = tail.rpartition(marker)
        try:
            receipt = json.loads(payload.strip()) if found else None
        except (ValueError, TypeError):
            receipt = None
        if isinstance(receipt, dict) and receipt.get("status") in {"completed", "timed_out", "cancelled", "unknown"}:
            status = receipt["status"]
            confirmed = receipt.get("termination_confirmed") is True
            exit_code = receipt.get("exit_code")
            if not isinstance(exit_code, int) or isinstance(exit_code, bool):
                exit_code = None
            stderr = stderr.split(marker, 1)[0].rstrip("\n")
        else:
            # A transport error cannot prove whether a command started.
            status, confirmed, exit_code = "unknown", False, None
        if not confirmed:
            status = "unknown"
    if not streams_drained:
        confirmed = False
    if not confirmed:
        status = "unknown"
    if status == "timed_out":
        exit_code = 124
    elif status == "cancelled":
        exit_code = 130
    output = format_command_output(stdout, stderr, exit_code, status, timeout=command_timeout or timeout)
    return CommandResult(output, exit_code, status, confirmed)


# The supervisor remains outside the child's process group. A unique receipt
# prevents ordinary command output from masquerading as an execution outcome.
# Linux subreaper mode permits reaping orphaned children after group termination.
LINUX_COMMAND_SUPERVISOR = r'''
import ctypes, json, os, select, signal, subprocess, sys, threading, time
shell, command, deadline, token = sys.argv[1:]
deadline = float(deadline)
try:
    subreaper = ctypes.CDLL(None).prctl(36, 1, 0, 0, 0) == 0
except Exception:
    subreaper = False
child = None
status = "completed"
code = None
confirmed = True
threads = []
def copy_output(source, destination):
    try:
        while data := os.read(source.fileno(), 65536):
            destination.write(data)
            destination.flush()
    except (OSError, BrokenPipeError):
        pass
def group_alive():
    try:
        os.killpg(child.pid, 0)
        return True
    except ProcessLookupError:
        return False
def reap():
    while True:
        try:
            pid, _ = os.waitpid(-1, os.WNOHANG)
            if not pid:
                return
        except ChildProcessError:
            return
def owned_children():
    result = set()
    for task in os.listdir("/proc/self/task"):
        try:
            with open("/proc/self/task/" + task + "/children") as handle:
                result.update(int(pid) for pid in handle.read().split())
        except FileNotFoundError:
            continue
    return result
def kill_owned(pid):
    # Adopted setsid/double-fork children remain ours under subreaper mode.
    # A pidfd plus a parent check prevents signalling a recycled unrelated PID.
    descriptor = None
    try:
        descriptor = os.pidfd_open(pid)
        with open("/proc/" + str(pid) + "/stat") as handle:
            stat = handle.read().rsplit(")", 1)[1].split()
        if int(stat[1]) != os.getpid():
            return
        signal.pidfd_send_signal(descriptor, signal.SIGKILL)
    except ProcessLookupError:
        pass
    except FileNotFoundError:
        pass
    finally:
        if descriptor is not None:
            os.close(descriptor)
try:
    child = subprocess.Popen([shell, "-lc", command], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    for source, dest in ((child.stdout, sys.stdout.buffer), (child.stderr, sys.stderr.buffer)):
        thread = threading.Thread(target=copy_output, args=(source, dest), daemon=True)
        thread.start()
        threads.append(thread)
    expires = time.monotonic() + deadline
    while child.poll() is None:
        if time.monotonic() >= expires:
            status = "timed_out"
            break
        readable, _, _ = select.select([sys.stdin], [], [], 0.05)
        if readable:
            request = os.read(0, 1024)
            status = "timed_out" if request.startswith(b"timed_out") else "cancelled"
            break
    code = child.poll()
except Exception as error:
    print("Command supervisor: " + str(error), file=sys.stderr)
    code = 127 if isinstance(error, FileNotFoundError) else 1
finally:
    if child is not None:
        if group_alive():
            try:
                os.killpg(child.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            until = time.monotonic() + 0.5
            while time.monotonic() < until and child.poll() is None:
                time.sleep(0.02)
            try:
                os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        try:
            child.wait(timeout=1)
        except subprocess.TimeoutExpired:
            confirmed = False
        if code is None and status == "completed":
            code = child.returncode
        until = time.monotonic() + 1.5
        try:
            while time.monotonic() < until:
                reap()
                children = owned_children()
                if not children and not group_alive():
                    break
                if subreaper:
                    for pid in children:
                        kill_owned(pid)
                time.sleep(0.02)
            reap()
            confirmed = confirmed and subreaper and not group_alive() and not owned_children()
        except (OSError, AttributeError, ValueError):
            confirmed = False
        for thread in threads:
            thread.join(timeout=0.25)
    if not confirmed:
        status = "unknown"
    print("\n__DEERFLOW_COMMAND_" + token + "__" + json.dumps({"exit_code": code, "status": status, "termination_confirmed": confirmed}), file=sys.stderr, flush=True)
'''


def linux_supervisor_args(shell: str, command: str, timeout: float) -> tuple[list[str], str]:
    token = uuid.uuid4().hex
    return ["python3", "-u", "-c", LINUX_COMMAND_SUPERVISOR, shell, command, str(validate_command_timeout(timeout)), token], token
