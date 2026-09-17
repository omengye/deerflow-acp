import os
import sys
import time

import pytest

from deerflow.config.sandbox_config import SandboxConfig
from deerflow.sandbox.local.local_sandbox import LocalSandbox, _BoundedPipeCapture


def test_sandbox_config_exposes_host_command_timeout() -> None:
    assert SandboxConfig(use="test").bash_command_timeout == 600
    assert SandboxConfig(use="test", bash_command_timeout=7).bash_command_timeout == 7


def test_bounded_pipe_capture_reports_discarded_bytes() -> None:
    capture = _BoundedPipeCapture(limit_bytes=5)

    capture.append(b"hello")
    capture.append(b" world")

    output = capture.read()
    assert output.startswith("hello")
    assert "5 of 11 bytes" in output


def test_bounded_pipe_capture_normalizes_windows_newlines() -> None:
    capture = _BoundedPipeCapture(normalize_newlines=True)

    capture.append(b"one\r\ntwo\rthree")

    assert capture.read() == "one\ntwo\nthree"


@pytest.mark.skipif(os.name != "nt", reason="Windows process-tree behavior")
def test_windows_command_capture_is_bounded() -> None:
    sandbox = LocalSandbox(
        "test",
        command_timeout_seconds=5,
        command_capture_limit_bytes=64,
    )

    stdout, stderr, returncode, timed_out = sandbox._run_windows_command(
        [sys.executable, "-c", "print('x' * 1000)"]
    )

    assert stderr == ""
    assert returncode == 0
    assert timed_out is False
    assert stdout.startswith("x" * 64)
    assert "output truncated after 64" in stdout


@pytest.mark.skipif(os.name != "nt", reason="Windows process-tree behavior")
def test_windows_command_timeout_terminates_process() -> None:
    sandbox = LocalSandbox("test", command_timeout_seconds=0.1)
    started = time.monotonic()

    stdout, stderr, _returncode, timed_out = sandbox._run_windows_command(
        [sys.executable, "-c", "import time; print('started', flush=True); time.sleep(30)"]
    )

    assert timed_out is True
    assert time.monotonic() - started < 10
    assert stdout == "started\n"
    assert stderr == ""


def test_timeout_notice_is_user_actionable() -> None:
    notice = LocalSandbox._format_timeout_notice(1.5)

    assert "1.5 seconds" in notice
    assert "background" in notice


def test_powershell_command_forces_utf8_console_and_pipe_decoding(monkeypatch) -> None:
    sandbox = LocalSandbox("test")
    calls: list[tuple[list[str], str | None]] = []

    def fake_run(args, *, encoding=None):
        calls.append((args, encoding))
        return "你好", "", 0, False

    monkeypatch.setattr("deerflow.sandbox.local.local_sandbox.os.name", "nt")
    monkeypatch.setattr(sandbox, "_get_shell", lambda: "pwsh.exe")
    monkeypatch.setattr(sandbox, "_run_windows_command", fake_run)

    assert sandbox.execute_command("Write-Output 你好") == "你好"
    args, encoding = calls[0]
    assert args[:3] == ["pwsh.exe", "-NoProfile", "-Command"]
    assert "[Console]::InputEncoding=[System.Text.Encoding]::UTF8" in args[3]
    assert "[Console]::OutputEncoding=[System.Text.Encoding]::UTF8" in args[3]
    assert args[3].endswith("Write-Output 你好")
    assert encoding == "utf-8"
