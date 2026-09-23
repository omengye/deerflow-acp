from __future__ import annotations

import ctypes
import json
import os
import sys
import threading
import time
from contextlib import contextmanager
from unittest.mock import patch

import pytest

from deerflow.sandbox.aio import AioSandbox
from deerflow.sandbox.command import CommandResult, format_command_output, linux_supervisor_args, run_host_command


def test_result_requires_confirmed_zero_exit():
    assert CommandResult("ok", 0).succeeded
    for result in (CommandResult("unknown", 0, "unknown", False), CommandResult("failed", 1), CommandResult("cancelled", 0, "cancelled"), CommandResult("ambiguous", 0, termination_confirmed=False)):
        assert not result.succeeded
    assert CommandResult("ok", 0).to_dict() == {"output": "ok", "exit_code": 0, "status": "completed", "termination_confirmed": True}
    assert format_command_output("\n  text\n", "", 0, "completed") == "\n  text\n"


def test_real_process_preserves_exit_code_and_bounds_output():
    result = run_host_command([sys.executable, "-c", "import sys; print('x'*20000); print('stderr', file=sys.stderr); sys.exit(127)"], timeout=5, capture_limit=1000)
    assert result.status == "completed"
    assert result.exit_code == 127
    assert not result.succeeded
    assert result.termination_confirmed
    assert "stderr" in result.output
    assert "output truncated" in result.output
    assert len(result.output) < 1500


def test_inherited_pipe_does_not_block_caller_or_claim_confirmed_completion():
    # The isolated child self-terminates shortly after this check. No user
    # processes are inspected or signalled by this normal-completion case.
    code = "import subprocess,sys; subprocess.Popen([sys.executable,'-c','import time; time.sleep(3)'])"
    started = time.monotonic()
    result = run_host_command([sys.executable, "-c", code], timeout=2)
    assert time.monotonic() - started < 2.5
    assert result.status == "unknown"
    assert not result.termination_confirmed


@pytest.mark.skipif(os.name != "nt", reason="Windows text-mode compatibility")
def test_windows_capture_normalizes_crlf_without_stripping_body():
    result = run_host_command([sys.executable, "-c", "import sys; sys.stdout.buffer.write(b'  text\\r\\n\\r\\n')"], timeout=2)
    assert result.output == "  text\n\n"


def _alive(pid):
    if os.name != "nt":
        try:
            os.kill(pid, 0)
            return True
        except ProcessLookupError:
            return False
    kernel = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel.OpenProcess.restype = ctypes.c_void_p
    kernel.OpenProcess.argtypes = [ctypes.c_ulong, ctypes.c_int, ctypes.c_ulong]
    kernel.GetExitCodeProcess.argtypes = [ctypes.c_void_p, ctypes.POINTER(ctypes.c_ulong)]
    kernel.CloseHandle.argtypes = [ctypes.c_void_p]
    handle = kernel.OpenProcess(0x1000, False, pid)
    if not handle:
        return False
    try:
        code = ctypes.c_ulong()
        return bool(kernel.GetExitCodeProcess(handle, ctypes.byref(code)) and code.value == 259)
    finally:
        kernel.CloseHandle(handle)


@pytest.mark.parametrize("cancel", [False, True])
def test_real_owned_process_tree_is_drained_on_timeout_or_cancel(tmp_path, cancel):
    pidfile = tmp_path / "child.pid"
    code = "import subprocess,sys,time; from pathlib import Path; child=subprocess.Popen([sys.executable,'-c','import time; time.sleep(30)']); Path(sys.argv[1]).write_text(str(child.pid)); print('started',flush=True); time.sleep(30)"
    event = threading.Event()

    def request_cancel():
        until = time.monotonic() + 3
        while not pidfile.exists() and time.monotonic() < until:
            time.sleep(0.01)
        event.set()

    requester = threading.Thread(target=request_cancel) if cancel else None
    if requester:
        requester.start()
    result = run_host_command([sys.executable, "-c", code, str(pidfile)], timeout=5 if cancel else 0.5, cancel_event=event)
    if requester:
        requester.join(timeout=1)
    assert result.status == ("cancelled" if cancel else "timed_out")
    assert result.exit_code == (130 if cancel else 124)
    assert result.termination_confirmed
    assert not _alive(int(pidfile.read_text()))


def test_pre_cancelled_command_never_spawns():
    event = threading.Event()
    event.set()
    with patch("deerflow.sandbox.command.subprocess.Popen") as spawn:
        result = run_host_command(["not-run"], timeout=1, cancel_event=event)
    assert result.status == "cancelled"
    spawn.assert_not_called()


def test_remote_transport_without_receipt_is_unknown():
    result = run_host_command([sys.executable, "-c", "print('partial')"], timeout=2, remote_token="audit")
    assert result.status == "unknown"
    assert not result.termination_confirmed
    assert "partial" in result.output


def test_remote_receipt_is_parsed_even_after_output_cap():
    receipt = {"status": "timed_out", "exit_code": -15, "termination_confirmed": True}
    script = "import sys; print('x'*5000,file=sys.stderr); print('__DEERFLOW_COMMAND_audit__'+sys.argv[1],file=sys.stderr)"
    result = run_host_command([sys.executable, "-c", script, json.dumps(receipt)], timeout=2, remote_token="audit", capture_limit=100, command_timeout=1)
    assert result.status == "timed_out"
    assert result.exit_code == 124
    assert result.termination_confirmed
    assert "after 1 seconds" in result.output
    assert "__DEERFLOW_COMMAND" not in result.output


@pytest.mark.parametrize("exit_code", [126, 127])
def test_aio_user_command_is_never_replayed_and_lease_covers_cleanup(exit_code):
    sandbox = AioSandbox("audit", "container", timeout=3)
    active = []

    @contextmanager
    def operation():
        active.append(True)
        try:
            yield None
        finally:
            active.pop()

    sandbox._operation_factory = operation
    calls = []

    def run(argv, **kwargs):
        assert active
        calls.append((argv, kwargs))
        return CommandResult("/bin/bash", 0) if len(calls) == 1 else CommandResult("partial", exit_code)

    with patch("deerflow.sandbox.aio.run_host_command", side_effect=run):
        result = sandbox.execute_command_result("user command")
    assert result.exit_code == exit_code
    assert len(calls) == 2  # One harmless shell probe, one user command.
    assert calls[1][0][-3] == "user command"
    assert not active


def test_aio_unknown_termination_marks_lease_uncertain():
    sandbox = AioSandbox("audit", "container")
    sandbox._command_shell = "/bin/bash"
    marked = []

    class Lease:
        def mark_uncertain(self):
            marked.append(True)

    @contextmanager
    def operation():
        yield Lease()

    sandbox._operation_factory = operation
    with patch("deerflow.sandbox.aio.run_host_command", return_value=CommandResult("lost", None, "unknown", False)):
        result = sandbox.execute_command_result("work")
    assert not result.termination_confirmed
    assert marked == [True]


@pytest.mark.parametrize("error", [OSError("transport lost"), RuntimeError("runner failed")])
def test_aio_runner_exception_keeps_generation_pinned(error):
    sandbox = AioSandbox("audit", "container")
    sandbox._command_shell = "/bin/bash"
    marked = []

    class Lease:
        def mark_uncertain(self):
            marked.append(True)

    @contextmanager
    def operation():
        yield Lease()

    sandbox._operation_factory = operation
    with patch("deerflow.sandbox.aio.run_host_command", side_effect=error):
        with pytest.raises(type(error)):
            sandbox.execute_command_result("work")
    assert marked == [True]


@pytest.mark.skipif(os.name == "nt", reason="The Linux supervisor integration requires a native POSIX test host")
@pytest.mark.parametrize("cancel", [False, True])
def test_linux_supervisor_owns_and_reaps_process_group(cancel):
    event = threading.Event()
    argv, token = linux_supervisor_args("/bin/sh", "sleep 30 & wait", 0.3 if not cancel else 10)
    timer = threading.Timer(0.3, event.set) if cancel else None
    if timer:
        timer.start()
    result = run_host_command(argv, timeout=15, command_timeout=0.3 if not cancel else 10, remote_token=token, cancel_event=event)
    if timer:
        timer.cancel()
    assert result.status == ("cancelled" if cancel else "timed_out")
    assert result.termination_confirmed


@pytest.mark.skipif(os.name == "nt", reason="Linux subreaper/pidfd integration requires a native POSIX test host")
def test_linux_supervisor_reaps_setsid_descendant():
    import shlex

    code = "import subprocess,sys; p=subprocess.Popen([sys.executable,'-c','import time; time.sleep(30)'],start_new_session=True); print(p.pid,flush=True)"
    argv, token = linux_supervisor_args("/bin/sh", "python3 -c " + shlex.quote(code), 5)
    result = run_host_command(argv, timeout=10, remote_token=token)
    assert result.succeeded, result.output
    assert not _alive(int(result.output.strip()))
