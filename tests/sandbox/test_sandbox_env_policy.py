from __future__ import annotations

import os
import sys
from unittest.mock import patch

import pytest

from deerflow.sandbox.env_policy import build_sandbox_subprocess_env
from deerflow.sandbox.command import CommandResult
from deerflow.sandbox.local.local_sandbox import LocalSandbox
from deerflow.sandbox.local.wsl_sandbox import WslSandbox


@pytest.mark.parametrize(
    "blocked_name",
    [
        "SSH_AUTH_SOCK",
        "SSH_AGENT_PID",
        "SSH_ASKPASS",
        "SSH_ASKPASS_REQUIRE",
        "GIT_ASKPASS",
    ],
)
def test_host_credential_pointers_are_removed(blocked_name: str) -> None:
    result = build_sandbox_subprocess_env(
        {
            blocked_name: "host-secret-pointer",
            "PATH": "safe-path",
        }
    )

    assert blocked_name not in result
    assert result["PATH"] == "safe-path"


def test_explicit_sandbox_override_can_reintroduce_a_required_value() -> None:
    result = build_sandbox_subprocess_env(
        {"SSH_AUTH_SOCK": "host-agent"},
        overrides={"SSH_AUTH_SOCK": "request-scoped-agent"},
    )

    assert result["SSH_AUTH_SOCK"] == "request-scoped-agent"


def test_wsl_command_uses_scrubbed_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("SSH_AUTH_SOCK", "host-agent")
    sandbox = WslSandbox("wsl", distro="Ubuntu")

    with patch(
        "deerflow.sandbox.local.wsl_sandbox.run_host_command",
        return_value=CommandResult("", 0),
    ) as run:
        sandbox.execute_command("true")

    environment = run.call_args.kwargs["env"]
    assert "SSH_AUTH_SOCK" not in environment
    assert environment["WSL_UTF8"] == "1"


@pytest.mark.skipif(os.name != "nt", reason="Windows subprocess integration")
def test_windows_local_command_cannot_see_host_ssh_agent(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("SSH_AUTH_SOCK", "host-agent")
    sandbox = LocalSandbox("test", command_timeout_seconds=5)

    stdout, stderr, returncode, timed_out = sandbox._run_windows_command(
        [
            sys.executable,
            "-c",
            "import os; print(os.getenv('SSH_AUTH_SOCK', 'missing'))",
        ]
    )

    assert stdout == "missing\n"
    assert stderr == ""
    assert returncode == 0
    assert timed_out is False
