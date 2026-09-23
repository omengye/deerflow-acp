"""WSL-backed sandbox implementation.

``WslSandbox`` runs ``bash`` inside a WSL2 distribution while keeping all file
I/O on the Windows host filesystem. It inherits ``LocalSandbox`` so file-system
tools (read_file, write_file, list_dir, glob, grep, update_file) behave
identically; only :py:meth:`execute_command` is overridden to drive ``wsl.exe``.

Path translation happens in three coordinate systems:

* virtual (agent-facing)  -- ``/mnt/user-data/foo.py``
* Windows (host)          -- ``D:\\Tools\\deerflow-api\\data\\threads\\<tid>\\user-data\\foo.py``
* WSL (inside the distro) -- ``/mnt/d/Tools/deerflow-api/data/threads/<tid>/user-data/foo.py``

``LocalSandbox._resolve_paths_in_command`` handles virtual -> Windows. This
class adds Windows -> WSL on the way in and the inverse pair on the way out.
"""

from __future__ import annotations

import re
import threading
from typing import ClassVar

from deerflow.sandbox.env_policy import build_sandbox_subprocess_env
from deerflow.sandbox.command import COMMAND_CLEANUP_SECONDS, CommandResult, linux_supervisor_args, run_host_command
from deerflow.sandbox.local.local_sandbox import LocalSandbox, PathMapping
from deerflow.sandbox.local.wsl_exceptions import WslUnavailableError

# Capture drive-letter prefixed Windows paths inside a shell command, including
# Windows verbatim forms such as ``\\?\D:\foo``.  The
# negative lookbehind guards against matching URL schemes (``http://...``) or
# identifier-like ``X:`` tokens following alphanumerics.  The trailing path
# character class is intentionally identical to LocalSandbox._resolve_paths_in_command
# so the two regex passes operate on the same tokens.
_WINDOWS_PATH_IN_COMMAND_RE = re.compile(
    r"(?<![\w/])(?:\\\\\?\\|//\?/)?([A-Za-z]):([\\/][^\s\"';&|<>()]*)"
)

# Capture ``/mnt/<letter>`` style WSL mount paths in command output.  The
# lookahead after the drive letter ensures we only match at a path boundary,
# preventing partial matches inside paths like ``/mnt/user-data`` (which
# starts with ``/mnt/u`` but is not a drive mount).
_WSL_MOUNT_IN_OUTPUT_RE = re.compile(
    r"(?<![\w/])/mnt/([a-zA-Z])(?=/|$|[\s\"';&|<>()])(/[^\s\"';&|<>()]*)?"
)


class WslSandbox(LocalSandbox):
    """Sandbox that executes commands inside a WSL distro."""

    EXECUTE_TIMEOUT_SECONDS: ClassVar[int] = 600

    def __init__(
        self,
        id: str,
        *,
        distro: str | None = None,
        wsl_user: str | None = None,
        wsl_shell: str = "bash",
        mount_prefix: str = "/mnt",
        path_mappings: list[PathMapping] | None = None,
        command_timeout_seconds: float = EXECUTE_TIMEOUT_SECONDS,
    ) -> None:
        super().__init__(id, path_mappings=path_mappings, command_timeout_seconds=command_timeout_seconds)
        self.distro = distro
        self.wsl_user = wsl_user
        self.wsl_shell = wsl_shell
        self.mount_prefix = mount_prefix.rstrip("/") or "/mnt"

    # ── Path translation helpers ──────────────────────────────────────────

    @staticmethod
    def _windows_path_to_wsl(path: str, mount_prefix: str = "/mnt") -> str:
        """Translate ``D:\\foo\\bar`` into ``/mnt/d/foo/bar``.

        Raises ``ValueError`` for UNC paths and for inputs that do not start
        with ``<drive-letter>:``.
        """
        if path.startswith(("\\\\?\\UNC\\", "//?/UNC/")):
            raise ValueError(f"UNC paths are not supported in WSL sandbox: {path}")
        if path.startswith(("\\\\?\\", "//?/")):
            path = path[4:]
        if path.startswith(("\\\\", "//")):
            raise ValueError(f"UNC paths are not supported in WSL sandbox: {path}")
        if len(path) < 2 or path[1] != ":" or not path[0].isalpha():
            raise ValueError(f"Not a drive-letter absolute path: {path}")
        drive = path[0].lower()
        remainder = path[2:].replace("\\", "/")
        if remainder and not remainder.startswith("/"):
            # ``D:foo`` (drive-relative) is not a well-defined absolute path in WSL.
            raise ValueError(f"Drive-relative paths are not supported: {path}")
        return f"{mount_prefix.rstrip('/')}/{drive}{remainder}"

    @staticmethod
    def _wsl_path_to_windows(path: str, mount_prefix: str = "/mnt") -> str:
        """Translate ``/mnt/d/foo/bar`` back into ``D:\\foo\\bar``."""
        prefix = mount_prefix.rstrip("/") + "/"
        if not path.startswith(prefix):
            raise ValueError(f"Not under WSL mount prefix {mount_prefix!r}: {path}")
        rest = path[len(prefix) :]
        if not rest:
            raise ValueError(f"Missing drive letter after mount prefix: {path}")
        drive = rest[0]
        if not drive.isalpha():
            raise ValueError(f"Invalid drive letter {drive!r} in {path}")
        tail = rest[1:]
        if tail and not tail.startswith("/"):
            raise ValueError(f"Malformed WSL mount path: {path}")
        windows_tail = tail.replace("/", "\\")
        return f"{drive.upper()}:{windows_tail}"

    def _translate_windows_paths_in_command(self, command: str) -> str:
        """Rewrite all drive-letter Windows paths in *command* to WSL form.

        Drive-letter verbatim paths (``\\\\?\\D:\\...``) are normalized as a
        unit so the Windows-only prefix cannot leak into the Linux command.
        UNC paths (``\\\\server\\share\\...``) are not detected here because
        backslash pairs frequently appear as escape syntax inside shell-quoted
        Python literals (``"open('D:\\\\foo')"``).  Callers needing strict UNC
        rejection should validate the resolved path via
        :py:meth:`_windows_path_to_wsl`.
        """
        prefix = self.mount_prefix.rstrip("/") or "/mnt"

        def _replace(match: re.Match[str]) -> str:
            drive = match.group(1).lower()
            tail = match.group(2).replace("\\", "/")
            return f"{prefix}/{drive}{tail}"

        return _WINDOWS_PATH_IN_COMMAND_RE.sub(_replace, command)

    def _translate_wsl_paths_in_output(self, output: str) -> str:
        """Rewrite ``/mnt/<drive>/...`` segments in *output* back to Windows paths."""
        prefix = self.mount_prefix.rstrip("/") or "/mnt"
        # If the configured mount prefix is non-default, build a fresh regex.
        if prefix == "/mnt":
            pattern = _WSL_MOUNT_IN_OUTPUT_RE
        else:
            pattern = re.compile(
                re.escape(prefix) + r"/([a-zA-Z])(?=/|$|[\s\"';&|<>()])(/[^\s\"';&|<>()]*)?"
            )

        def _replace(match: re.Match[str]) -> str:
            drive = match.group(1).upper()
            tail = (match.group(2) or "").replace("/", "\\")
            return f"{drive}:{tail}"

        return pattern.sub(_replace, output)

    # ── wsl.exe invocation ────────────────────────────────────────────────

    def _build_wsl_argv(self, bash_command: str) -> list[str]:
        """Compose the argv passed to ``subprocess.run`` for *bash_command*."""
        argv: list[str] = ["wsl.exe"]
        if self.distro:
            argv += ["-d", self.distro]
        if self.wsl_user:
            argv += ["-u", self.wsl_user]
        argv += ["--", self.wsl_shell, "-lc", bash_command]
        return argv

    def execute_command(self, command: str) -> str:
        return self.execute_command_result(command).output

    def execute_command_result(self, command: str, *, cancel_event: threading.Event | None = None) -> CommandResult:
        # virtual -> Windows  (inherited)
        resolved = self._resolve_paths_in_command(command)
        # Windows -> WSL  (new)
        wsl_command = self._translate_windows_paths_in_command(resolved)

        supervised, token = linux_supervisor_args(self.wsl_shell, wsl_command, self.command_timeout_seconds)
        argv = ["wsl.exe"]
        if self.distro:
            argv += ["-d", self.distro]
        if self.wsl_user:
            argv += ["-u", self.wsl_user]
        argv += ["--", *supervised]
        env = build_sandbox_subprocess_env(overrides={"WSL_UTF8": "1"})

        try:
            result = run_host_command(
                argv, timeout=self.command_timeout_seconds + COMMAND_CLEANUP_SECONDS,
                cancel_event=cancel_event, env=env, encoding="utf-8",
                capture_limit=self.command_capture_limit_bytes, remote_token=token,
                command_timeout=self.command_timeout_seconds,
            )
        except FileNotFoundError as exc:
            raise WslUnavailableError(
                "WSL is not installed or wsl.exe is not on PATH. Install Windows "
                "Subsystem for Linux and a distro before using LocalWslProvider."
            ) from exc
        # WSL -> Windows -> virtual
        windows_output = self._translate_wsl_paths_in_output(result.output)
        return CommandResult(self._reverse_resolve_paths_in_output(windows_output), result.exit_code, result.status, result.termination_confirmed)
