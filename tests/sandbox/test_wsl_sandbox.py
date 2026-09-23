"""Unit tests for WslSandbox / LocalWslProvider.

All tests mock command execution so they run on Linux CI as well as on Windows.
The provider tests also mock ``platform.system`` since the provider rejects
non-Windows hosts at construction time.
"""

from __future__ import annotations

from pathlib import Path
from types import SimpleNamespace
from unittest.mock import MagicMock, patch

import pytest

from deerflow.sandbox.local import (
    LocalWslProvider,
    WslDistroNotFoundError,
    WslSandbox,
    WslUnavailableError,
)
from deerflow.sandbox.local.local_sandbox import PathMapping
from deerflow.sandbox.command import COMMAND_CLEANUP_SECONDS, CommandResult
from deerflow.sandbox.provider_paths import (
    WSL_SANDBOX_PROVIDER_PATH,
    is_host_fs_sandbox_provider_path,
    is_local_sandbox_provider_path,
    is_wsl_sandbox_provider_path,
    normalize_sandbox_provider_path,
)
from deerflow.sandbox.tools import (
    _HOST_FS_SANDBOX_IDS,
    is_host_fs_sandbox,
    is_local_sandbox,
)


# ── Static path translation ──────────────────────────────────────────────


@pytest.mark.parametrize(
    ("windows", "expected"),
    [
        ("D:\\foo\\bar.py", "/mnt/d/foo/bar.py"),
        ("C:\\Users\\me\\proj", "/mnt/c/Users/me/proj"),
        ("E:\\", "/mnt/e/"),
        ("D:\\Tools\\deer-flow with spaces\\x.py", "/mnt/d/Tools/deer-flow with spaces/x.py"),
        ("D:/Foo/bar", "/mnt/d/Foo/bar"),
        ("\\\\?\\D:\\Tools\\deer-flow\\x.py", "/mnt/d/Tools/deer-flow/x.py"),
        ("//?/D:/Tools/deer-flow/x.py", "/mnt/d/Tools/deer-flow/x.py"),
    ],
)
def test_windows_to_wsl_basic(windows: str, expected: str) -> None:
    assert WslSandbox._windows_path_to_wsl(windows) == expected


def test_windows_to_wsl_rejects_unc() -> None:
    with pytest.raises(ValueError, match="UNC"):
        WslSandbox._windows_path_to_wsl("\\\\wsl$\\Ubuntu\\home\\me")
    with pytest.raises(ValueError, match="UNC"):
        WslSandbox._windows_path_to_wsl("//server/share/foo")
    with pytest.raises(ValueError, match="UNC"):
        WslSandbox._windows_path_to_wsl("\\\\?\\UNC\\server\\share\\foo")
    with pytest.raises(ValueError, match="UNC"):
        WslSandbox._windows_path_to_wsl("//?/UNC/server/share/foo")


def test_windows_to_wsl_rejects_relative_or_non_drive() -> None:
    with pytest.raises(ValueError):
        WslSandbox._windows_path_to_wsl("foo\\bar")
    with pytest.raises(ValueError):
        WslSandbox._windows_path_to_wsl("D:foo")  # drive-relative
    with pytest.raises(ValueError):
        WslSandbox._windows_path_to_wsl(":\\foo")


@pytest.mark.parametrize(
    ("wsl_path", "expected"),
    [
        ("/mnt/d/foo/bar.py", "D:\\foo\\bar.py"),
        ("/mnt/c", "C:"),
        ("/mnt/D/Users/me", "D:\\Users\\me"),  # uppercase drive letter still works
        ("/mnt/d/Tools/deer-flow/x.py", "D:\\Tools\\deer-flow\\x.py"),
    ],
)
def test_wsl_to_windows_basic(wsl_path: str, expected: str) -> None:
    assert WslSandbox._wsl_path_to_windows(wsl_path) == expected


# ── Command-string translation ───────────────────────────────────────────


def _make_sandbox(path_mappings: list[PathMapping] | None = None) -> WslSandbox:
    return WslSandbox("wsl", distro="Ubuntu-22.04", path_mappings=path_mappings)


def test_translate_command_basic_drive_path() -> None:
    sandbox = _make_sandbox()
    assert (
        sandbox._translate_windows_paths_in_command("cat D:\\foo\\x.py")
        == "cat /mnt/d/foo/x.py"
    )


@pytest.mark.parametrize(
    "command",
    [
        r"cd '\\?\D:\Tools\deer-flow\workspace' && pwd",
        "cd '//?/D:/Tools/deer-flow/workspace' && pwd",
    ],
)
def test_translate_command_strips_verbatim_drive_prefix(command: str) -> None:
    sandbox = _make_sandbox()
    assert sandbox._translate_windows_paths_in_command(command) == (
        "cd '/mnt/d/Tools/deer-flow/workspace' && pwd"
    )


def test_translate_command_preserves_pipes_and_operators() -> None:
    sandbox = _make_sandbox()
    cmd = "cat D:\\foo\\x.py | grep hello && echo done"
    expected = "cat /mnt/d/foo/x.py | grep hello && echo done"
    assert sandbox._translate_windows_paths_in_command(cmd) == expected


def test_translate_command_preserves_redirections() -> None:
    sandbox = _make_sandbox()
    cmd = "echo $X > D:\\out.txt 2>&1"
    expected = "echo $X > /mnt/d/out.txt 2>&1"
    assert sandbox._translate_windows_paths_in_command(cmd) == expected


def test_translate_command_inside_double_quotes() -> None:
    sandbox = _make_sandbox()
    cmd = "python -c \"open('D:\\\\foo\\\\x.py')\""
    # Drive prefix gets translated; backslashes inside the Python literal
    # are still backslashes because the parent resolver runs first; we only
    # translate the drive-letter prefix and convert backslashes to slashes
    # within the matched span.
    out = sandbox._translate_windows_paths_in_command(cmd)
    assert "/mnt/d/" in out
    assert "D:" not in out  # drive form removed


def test_translate_command_passes_through_unc_paths() -> None:
    # UNC paths inside command strings are NOT auto-rejected, because backslash
    # pairs commonly appear as escape sequences in Python/shell literals.
    # Strict UNC rejection happens only in the static `_windows_path_to_wsl`
    # helper (covered by test_windows_to_wsl_rejects_unc).
    sandbox = _make_sandbox()
    cmd = "ls \\\\wsl$\\Ubuntu\\home"
    # Drive-letter regex doesn't match UNC paths; command is left unchanged.
    assert sandbox._translate_windows_paths_in_command(cmd) == cmd


def test_translate_command_lowercases_drive_letter() -> None:
    sandbox = _make_sandbox()
    assert (
        sandbox._translate_windows_paths_in_command("cat E:\\X.txt")
        == "cat /mnt/e/X.txt"
    )


def test_translate_command_does_not_touch_urls() -> None:
    sandbox = _make_sandbox()
    cmd = "curl https://example.com/foo && cat D:\\x"
    assert (
        sandbox._translate_windows_paths_in_command(cmd)
        == "curl https://example.com/foo && cat /mnt/d/x"
    )


# ── Output translation ───────────────────────────────────────────────────


def test_translate_output_basic() -> None:
    sandbox = _make_sandbox()
    assert (
        sandbox._translate_wsl_paths_in_output("found at /mnt/d/foo/x.py")
        == "found at D:\\foo\\x.py"
    )


def test_translate_output_does_not_touch_user_data_virtual_path() -> None:
    sandbox = _make_sandbox()
    # /mnt/user-data is the virtual prefix; must not be partially matched
    # as ``/mnt/u`` followed by ``ser-data``.
    text = "writing to /mnt/user-data/workspace/x.py"
    assert sandbox._translate_wsl_paths_in_output(text) == text


def test_translate_output_handles_multiple_drives() -> None:
    sandbox = _make_sandbox()
    text = "see /mnt/c/Users and /mnt/d/Tools"
    assert sandbox._translate_wsl_paths_in_output(text) == "see C:\\Users and D:\\Tools"


def test_round_trip_windows_wsl() -> None:
    sandbox = _make_sandbox()
    win = "cat D:\\Tools\\foo.py"
    wsl = sandbox._translate_windows_paths_in_command(win)
    back = sandbox._translate_wsl_paths_in_output(wsl)
    assert back == win


# ── argv builder ─────────────────────────────────────────────────────────


def test_build_wsl_argv_default_distro() -> None:
    sandbox = WslSandbox("wsl")
    assert sandbox._build_wsl_argv("echo hi") == [
        "wsl.exe",
        "--",
        "bash",
        "-lc",
        "echo hi",
    ]


def test_build_wsl_argv_with_distro_and_user() -> None:
    sandbox = WslSandbox(
        "wsl",
        distro="Ubuntu-22.04",
        wsl_user="me",
        wsl_shell="bash",
    )
    assert sandbox._build_wsl_argv("ls /home") == [
        "wsl.exe",
        "-d",
        "Ubuntu-22.04",
        "-u",
        "me",
        "--",
        "bash",
        "-lc",
        "ls /home",
    ]


def test_build_wsl_argv_custom_shell() -> None:
    sandbox = WslSandbox("wsl", distro="Ubuntu-22.04", wsl_shell="zsh")
    argv = sandbox._build_wsl_argv("echo hi")
    assert argv[-3] == "zsh"
    assert argv[-2] == "-lc"


# ── execute_command (mocked command runner) ──────────────────────────────


def test_execute_command_sets_wsl_utf8_env_and_utf8_encoding() -> None:
    sandbox = WslSandbox("wsl", distro="Ubuntu-22.04")
    with patch("deerflow.sandbox.local.wsl_sandbox.run_host_command", return_value=CommandResult("ok\n", 0)) as mock_run:
        sandbox.execute_command("echo ok")

    args, kwargs = mock_run.call_args
    assert kwargs["env"]["WSL_UTF8"] == "1"
    assert kwargs["encoding"] == "utf-8"
    assert kwargs["timeout"] == sandbox.command_timeout_seconds + COMMAND_CLEANUP_SECONDS
    assert kwargs["command_timeout"] == sandbox.command_timeout_seconds
    assert kwargs["remote_token"]


def test_execute_command_passes_translated_command_to_wsl() -> None:
    sandbox = WslSandbox("wsl", distro="Ubuntu-22.04")
    with (
        patch("deerflow.sandbox.local.wsl_sandbox.run_host_command", return_value=CommandResult("", 0)) as mock_run,
        patch("deerflow.sandbox.local.wsl_sandbox.linux_supervisor_args", return_value=(["supervised"], "token")) as supervisor,
    ):
        sandbox.execute_command("cat D:\\foo\\x.py")

    assert supervisor.call_args.args[1] == "cat /mnt/d/foo/x.py"
    assert mock_run.call_args.args[0] == ["wsl.exe", "-d", "Ubuntu-22.04", "--", "supervised"]


def test_execute_command_passes_translated_verbatim_path_to_wsl() -> None:
    sandbox = WslSandbox("wsl", distro="Ubuntu-22.04")
    with (
        patch("deerflow.sandbox.local.wsl_sandbox.run_host_command", return_value=CommandResult("", 0)),
        patch("deerflow.sandbox.local.wsl_sandbox.linux_supervisor_args", return_value=(["supervised"], "token")) as supervisor,
    ):
        sandbox.execute_command(r"cd '\\?\D:\foo' && pwd")

    assert supervisor.call_args.args[1] == "cd '/mnt/d/foo' && pwd"


def test_execute_command_translates_output_back() -> None:
    sandbox = WslSandbox("wsl", distro="Ubuntu-22.04")
    with patch("deerflow.sandbox.local.wsl_sandbox.run_host_command", return_value=CommandResult("found at /mnt/d/foo/x.py\n", 0)):
        out = sandbox.execute_command("echo ignored")
    assert "D:\\foo\\x.py" in out
    assert "/mnt/d/foo" not in out


def test_execute_command_appends_exit_code_on_failure() -> None:
    sandbox = WslSandbox("wsl", distro="Ubuntu-22.04")
    with patch("deerflow.sandbox.local.wsl_sandbox.run_host_command", return_value=CommandResult("boom\nExit Code: 2", 2)):
        result = sandbox.execute_command_result("false")
    assert "boom" in result.output
    assert "Exit Code: 2" in result.output
    assert result.exit_code == 2
    assert not result.succeeded


def test_execute_command_no_output_returns_marker() -> None:
    sandbox = WslSandbox("wsl", distro="Ubuntu-22.04")
    with patch("deerflow.sandbox.local.wsl_sandbox.run_host_command", return_value=CommandResult("(no output)", 0)):
        out = sandbox.execute_command("true")
    assert out == "(no output)"


def test_execute_command_wsl_missing_raises_clear_error() -> None:
    sandbox = WslSandbox("wsl", distro="Ubuntu-22.04")
    with patch("deerflow.sandbox.local.wsl_sandbox.run_host_command", side_effect=FileNotFoundError("wsl.exe")):
        with pytest.raises(WslUnavailableError, match="not installed"):
            sandbox.execute_command("echo hi")


def test_execute_command_timeout_returns_formatted_message() -> None:
    sandbox = WslSandbox("wsl", distro="Ubuntu-22.04")
    timed_out = CommandResult("partial\nCommand timed out after 600 seconds and was terminated.\nExit Code: 124", 124, "timed_out")
    with patch("deerflow.sandbox.local.wsl_sandbox.run_host_command", return_value=timed_out):
        result = sandbox.execute_command_result("sleep 10000")
    assert "partial" in result.output
    assert result.status == "timed_out"
    assert result.termination_confirmed
    assert not result.succeeded


# ── Provider (Windows-only path) ─────────────────────────────────────────


def _patched_windows_provider_env(
    *,
    list_stdout: bytes = "Ubuntu-22.04\nUbuntu-20.04\n".encode("utf-16-le"),
    status_returncode: int = 0,
    list_returncode: int = 0,
    distro: str | None = "Ubuntu-22.04",
):
    """Return a (patches, fake_config) tuple ready for `with contextlib.ExitStack`."""

    def fake_run(args, **kwargs):  # noqa: ANN001
        if "--status" in args:
            return SimpleNamespace(returncode=status_returncode, stdout=b"", stderr=b"")
        if "-l" in args and "-q" in args:
            return SimpleNamespace(returncode=list_returncode, stdout=list_stdout, stderr=b"")
        return SimpleNamespace(returncode=0, stdout=b"", stderr=b"")

    fake_config = SimpleNamespace(
        sandbox=SimpleNamespace(
            wsl_distro=distro,
            wsl_user=None,
            wsl_shell="bash",
            wsl_mount_prefix="/mnt",
        ),
    )
    return fake_run, fake_config


def _reset_provider_singleton() -> None:
    import deerflow.sandbox.local.local_wsl_provider as mod

    mod._singleton = None


def test_provider_rejects_non_windows_platform() -> None:
    _reset_provider_singleton()
    with patch("platform.system", return_value="Linux"):
        with pytest.raises(RuntimeError, match="only supported on Windows"):
            LocalWslProvider()


def test_provider_init_verifies_wsl_and_distro() -> None:
    _reset_provider_singleton()
    fake_run, fake_config = _patched_windows_provider_env()
    with (
        patch("platform.system", return_value="Windows"),
        patch("subprocess.run", side_effect=fake_run) as mock_run,
        patch(
            "deerflow.sandbox.local.local_wsl_provider.build_host_fs_path_mappings",
            return_value=[],
        ),
        patch("deerflow.config.get_app_config", return_value=fake_config),
    ):
        provider = LocalWslProvider()

    assert provider._distro == "Ubuntu-22.04"
    invocations = [call.args[0] for call in mock_run.call_args_list]
    assert ["wsl.exe", "--status"] in invocations
    assert ["wsl.exe", "-l", "-q"] in invocations


def test_provider_init_raises_if_distro_missing() -> None:
    _reset_provider_singleton()
    fake_run, fake_config = _patched_windows_provider_env(
        list_stdout="Ubuntu-20.04\n".encode("utf-16-le"),
    )
    with (
        patch("platform.system", return_value="Windows"),
        patch("subprocess.run", side_effect=fake_run),
        patch(
            "deerflow.sandbox.local.local_wsl_provider.build_host_fs_path_mappings",
            return_value=[],
        ),
        patch("deerflow.config.get_app_config", return_value=fake_config),
    ):
        with pytest.raises(WslDistroNotFoundError, match="Ubuntu-22.04"):
            LocalWslProvider()


def test_provider_init_raises_when_wsl_exe_missing() -> None:
    _reset_provider_singleton()
    _, fake_config = _patched_windows_provider_env()
    with (
        patch("platform.system", return_value="Windows"),
        patch("subprocess.run", side_effect=FileNotFoundError("wsl.exe")),
        patch(
            "deerflow.sandbox.local.local_wsl_provider.build_host_fs_path_mappings",
            return_value=[],
        ),
        patch("deerflow.config.get_app_config", return_value=fake_config),
    ):
        with pytest.raises(WslUnavailableError, match="not found"):
            LocalWslProvider()


def test_provider_acquire_singleton_and_get() -> None:
    _reset_provider_singleton()
    fake_run, fake_config = _patched_windows_provider_env()
    fake_config.skills = SimpleNamespace(container_path="/mnt/skills")
    with (
        patch("platform.system", return_value="Windows"),
        patch("subprocess.run", side_effect=fake_run),
        patch(
            "deerflow.sandbox.local.local_wsl_provider.build_host_fs_path_mappings",
            return_value=[],
        ),
        patch("deerflow.config.get_app_config", return_value=fake_config),
    ):
        provider = LocalWslProvider()
        with patch(
            "deerflow.skills.projection.get_skill_projection",
            return_value=SimpleNamespace(path=Path("projected-skills"), revision="a" * 24),
        ):
            sid_a = provider.acquire("t1")
            sid_b = provider.acquire("t2")

    assert sid_a == f"wsl:{'a' * 24}"
    assert sid_a == sid_b
    assert provider.get(sid_a) is not None
    assert provider.get("local") is None


def test_provider_lru_bounds_revision_cache() -> None:
    _reset_provider_singleton()
    fake_run, fake_config = _patched_windows_provider_env()
    fake_config.skills = SimpleNamespace(container_path="/mnt/skills")
    projections = [
        SimpleNamespace(path=Path(f"skills-{index}"), revision=f"{index:024x}")
        for index in range(3)
    ]
    with (
        patch("platform.system", return_value="Windows"),
        patch("subprocess.run", side_effect=fake_run),
        patch(
            "deerflow.sandbox.local.local_wsl_provider.build_host_fs_path_mappings",
            return_value=[],
        ),
        patch("deerflow.config.get_app_config", return_value=fake_config),
    ):
        provider = LocalWslProvider()
        provider._max_cached_sandboxes = 2
        with patch(
            "deerflow.skills.projection.get_skill_projection",
            side_effect=projections,
        ):
            sandbox_ids = [provider.acquire("thread") for _ in projections]

    assert len(provider._sandboxes) == 2
    assert provider.get(sandbox_ids[0]) is None
    assert provider.get(sandbox_ids[1]) is not None
    assert provider.get(sandbox_ids[2]) is not None


# ── Aliases / gate helpers ────────────────────────────────────────────────


def test_provider_path_aliases_normalize() -> None:
    assert normalize_sandbox_provider_path("wsl") == WSL_SANDBOX_PROVIDER_PATH
    assert normalize_sandbox_provider_path("local-wsl") == WSL_SANDBOX_PROVIDER_PATH
    assert normalize_sandbox_provider_path("local_wsl") == WSL_SANDBOX_PROVIDER_PATH


def test_provider_path_classification() -> None:
    assert is_wsl_sandbox_provider_path("wsl") is True
    assert is_wsl_sandbox_provider_path(WSL_SANDBOX_PROVIDER_PATH) is True
    assert is_wsl_sandbox_provider_path("deerflow.sandbox.local.local_wsl_provider:LocalWslProvider") is True

    assert is_wsl_sandbox_provider_path("local") is False
    assert is_local_sandbox_provider_path("local") is True
    assert is_local_sandbox_provider_path("wsl") is False

    assert is_host_fs_sandbox_provider_path("local") is True
    assert is_host_fs_sandbox_provider_path("wsl") is True
    assert is_host_fs_sandbox_provider_path("deerflow.sandbox.aio:AioSandboxProvider") is False


def test_wsl_bash_requires_the_same_explicit_opt_in_as_local() -> None:
    from deerflow.sandbox.security import is_host_bash_allowed

    disabled = SimpleNamespace(
        sandbox=SimpleNamespace(use="wsl", allow_host_bash=False)
    )
    enabled = SimpleNamespace(
        sandbox=SimpleNamespace(use="wsl", allow_host_bash=True)
    )

    assert is_host_bash_allowed(disabled) is False
    assert is_host_bash_allowed(enabled) is True


def _runtime_with_sandbox_id(sandbox_id: str | None) -> MagicMock:
    runtime = MagicMock()
    state: dict = {}
    if sandbox_id is not None:
        state["sandbox"] = {"sandbox_id": sandbox_id}
    runtime.state = state
    return runtime


def test_is_host_fs_sandbox_accepts_local_and_wsl() -> None:
    assert is_host_fs_sandbox(_runtime_with_sandbox_id("local")) is True
    assert is_host_fs_sandbox(_runtime_with_sandbox_id("wsl")) is True
    assert is_host_fs_sandbox(_runtime_with_sandbox_id("aio")) is False
    assert is_host_fs_sandbox(_runtime_with_sandbox_id(None)) is False
    assert is_host_fs_sandbox(None) is False


def test_is_local_sandbox_back_compat_alias() -> None:
    # Verify the historical name still resolves to the broader check.
    assert is_local_sandbox is is_host_fs_sandbox
    assert is_local_sandbox(_runtime_with_sandbox_id("wsl")) is True


def test_host_fs_sandbox_ids_set_is_frozen() -> None:
    assert _HOST_FS_SANDBOX_IDS == frozenset({"local", "wsl"})


# ── File ops smoke test (inherited from LocalSandbox) ────────────────────


def test_inherited_file_ops_work_against_tmp_path(tmp_path) -> None:
    """WslSandbox should expose the same file-ops behaviour as LocalSandbox."""
    mapping = PathMapping(container_path="/mnt/user-data", local_path=str(tmp_path), read_only=False)
    sandbox = WslSandbox("wsl", path_mappings=[mapping])

    sandbox.write_file("/mnt/user-data/hello.txt", "hello")
    assert sandbox.read_file("/mnt/user-data/hello.txt") == "hello"

    listing = sandbox.list_dir("/mnt/user-data")
    assert any("hello.txt" in entry for entry in listing)
