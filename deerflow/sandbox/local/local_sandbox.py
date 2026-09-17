import errno
import locale
import logging
import ntpath
import os
import shutil
import subprocess
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import NamedTuple

from deerflow.config.paths import VIRTUAL_PATH_PREFIX
from deerflow.sandbox.env_policy import build_sandbox_subprocess_env
from deerflow.sandbox.local.list_dir import list_dir
from deerflow.sandbox.sandbox import Sandbox
from deerflow.sandbox.search import GrepMatch, find_glob_matches, find_grep_matches

logger = logging.getLogger(__name__)

_MAX_DOWNLOAD_SIZE = 100 * 1024 * 1024  # 100 MB
DEFAULT_COMMAND_TIMEOUT_SECONDS = 600
_COMMAND_CAPTURE_LIMIT_BYTES = 10 * 1024 * 1024
_PIPE_DRAIN_JOIN_TIMEOUT_SECONDS = 0.2
_POWERSHELL_UTF8_PREAMBLE = (
    "try{[Console]::InputEncoding=[System.Text.Encoding]::UTF8}catch{};"
    "try{[Console]::OutputEncoding=[System.Text.Encoding]::UTF8}catch{};"
    "$OutputEncoding=[System.Text.Encoding]::UTF8;"
)


class _BoundedPipeCapture:
    """Drain a subprocess pipe while retaining only bounded output."""

    def __init__(
        self,
        *,
        limit_bytes: int = _COMMAND_CAPTURE_LIMIT_BYTES,
        encoding: str = "utf-8",
        normalize_newlines: bool = False,
    ) -> None:
        self._limit_bytes = max(0, limit_bytes)
        self._encoding = encoding
        self._normalize_newlines = normalize_newlines
        self._chunks: list[bytes] = []
        self._kept_bytes = 0
        self._total_bytes = 0
        self._lock = threading.Lock()

    def append(self, chunk: bytes) -> None:
        with self._lock:
            self._total_bytes += len(chunk)
            if self._kept_bytes >= self._limit_bytes:
                return
            remaining = self._limit_bytes - self._kept_bytes
            kept = chunk[:remaining]
            self._chunks.append(kept)
            self._kept_bytes += len(kept)

    def read(self) -> str:
        with self._lock:
            data = b"".join(self._chunks)
            truncated = self._total_bytes > self._kept_bytes
            total_bytes = self._total_bytes
            kept_bytes = self._kept_bytes

        output = data.decode(self._encoding, errors="replace")
        if self._normalize_newlines:
            output = output.replace("\r\n", "\n").replace("\r", "\n")
        if truncated:
            output += (
                f"\n... [output truncated after {kept_bytes} of {total_bytes} "
                "bytes; remaining output discarded] ..."
            )
        return output


@dataclass(frozen=True)
class PathMapping:
    """A path mapping from a container path to a local path with optional read-only flag."""

    container_path: str
    local_path: str
    read_only: bool = False


class ResolvedPath(NamedTuple):
    path: str
    mapping: PathMapping | None


class LocalSandbox(Sandbox):
    @staticmethod
    def _shell_name(shell: str) -> str:
        """Return the executable name for a shell path or command."""
        return shell.replace("\\", "/").rsplit("/", 1)[-1].lower()

    @staticmethod
    def _is_powershell(shell: str) -> bool:
        """Return whether the selected shell is a PowerShell executable."""
        return LocalSandbox._shell_name(shell) in {"powershell", "powershell.exe", "pwsh", "pwsh.exe"}

    @staticmethod
    def _is_cmd_shell(shell: str) -> bool:
        """Return whether the selected shell is cmd.exe."""
        return LocalSandbox._shell_name(shell) in {"cmd", "cmd.exe"}

    @staticmethod
    def _find_first_available_shell(candidates: tuple[str, ...]) -> str | None:
        """Return the first executable shell path or command found from candidates."""
        for shell in candidates:
            if os.path.isabs(shell):
                if os.path.isfile(shell) and os.access(shell, os.X_OK):
                    return shell
                continue

            shell_from_path = shutil.which(shell)
            if shell_from_path is not None:
                return shell_from_path

        return None

    def __init__(
        self,
        id: str,
        path_mappings: list[PathMapping] | None = None,
        *,
        command_timeout_seconds: float = DEFAULT_COMMAND_TIMEOUT_SECONDS,
        command_capture_limit_bytes: int = _COMMAND_CAPTURE_LIMIT_BYTES,
    ) -> None:
        """
        Initialize local sandbox with optional path mappings.

        Args:
            id: Sandbox identifier
            path_mappings: List of path mappings with optional read-only flag.
                          Skills directory is read-only by default.
        """
        super().__init__(id)
        self.path_mappings = path_mappings or []
        if command_timeout_seconds <= 0:
            raise ValueError("command_timeout_seconds must be positive")
        if command_capture_limit_bytes < 0:
            raise ValueError("command_capture_limit_bytes cannot be negative")
        self.command_timeout_seconds = command_timeout_seconds
        self.command_capture_limit_bytes = command_capture_limit_bytes
        # Track files written through write_file so read_file only
        # reverse-resolves paths in agent-authored content.
        self._agent_written_paths: set[str] = set()

    def _is_read_only_path(self, resolved_path: str) -> bool:
        """Check if a resolved path is under a read-only mount.

        When multiple mappings match (nested mounts), prefer the most specific
        mapping (i.e. the one whose local_path is the longest prefix of the
        resolved path), similar to how ``_resolve_path`` handles container paths.
        """
        resolved = str(Path(resolved_path).resolve())

        best_mapping: PathMapping | None = None
        best_prefix_len = -1

        for mapping in self.path_mappings:
            local_resolved = str(Path(mapping.local_path).resolve())
            if resolved == local_resolved or resolved.startswith(local_resolved + os.sep):
                prefix_len = len(local_resolved)
                if prefix_len > best_prefix_len:
                    best_prefix_len = prefix_len
                    best_mapping = mapping

        if best_mapping is None:
            return False

        return best_mapping.read_only

    def _find_path_mapping(self, path: str) -> tuple[PathMapping, str] | None:
        path_str = str(path)

        for mapping in sorted(self.path_mappings, key=lambda m: len(m.container_path.rstrip("/") or "/"), reverse=True):
            container_path = mapping.container_path.rstrip("/") or "/"
            if container_path == "/":
                if path_str.startswith("/"):
                    return mapping, path_str.lstrip("/")
                continue

            if path_str == container_path or path_str.startswith(container_path + "/"):
                relative = path_str[len(container_path) :].lstrip("/")
                return mapping, relative

        return None

    def _resolve_path_with_mapping(self, path: str) -> ResolvedPath:
        """
        Resolve container path to actual local path using mappings.

        Args:
            path: Path that might be a container path

        Returns:
            Resolved local path and the matched mapping, if any
        """
        path_str = str(path)

        mapping_match = self._find_path_mapping(path_str)
        if mapping_match is None:
            return ResolvedPath(path_str, None)

        mapping, relative = mapping_match
        local_root = Path(mapping.local_path).resolve()
        resolved_path = (local_root / relative).resolve() if relative else local_root

        try:
            resolved_path.relative_to(local_root)
        except ValueError as exc:
            raise PermissionError(errno.EACCES, "Access denied: path escapes mounted directory", path_str) from exc

        return ResolvedPath(str(resolved_path), mapping)

    def _resolve_path(self, path: str) -> str:
        return self._resolve_path_with_mapping(path).path

    def _is_resolved_path_read_only(self, resolved: ResolvedPath) -> bool:
        return bool(resolved.mapping and resolved.mapping.read_only) or self._is_read_only_path(resolved.path)

    def _reverse_resolve_path(self, path: str) -> str:
        """
        Reverse resolve local path back to container path using mappings.

        Args:
            path: Local path that might need to be mapped to container path

        Returns:
            Container path if mapping exists, otherwise original path
        """
        normalized_path = path.replace("\\", "/")
        path_str = os.path.realpath(normalized_path)

        # Try each mapping (longest local path first for more specific matches)
        for mapping in sorted(self.path_mappings, key=lambda m: len(m.local_path), reverse=True):
            local_path_resolved = os.path.realpath(mapping.local_path)
            comparison_path = os.path.normcase(path_str)
            comparison_root = os.path.normcase(local_path_resolved)
            if comparison_path == comparison_root or comparison_path.startswith(comparison_root + os.sep):
                # Replace the local path prefix with container path
                relative = path_str[len(local_path_resolved) :].lstrip(os.sep).replace(os.sep, "/")
                resolved = f"{mapping.container_path}/{relative}" if relative else mapping.container_path
                return resolved

        # No mapping found, return original path
        return path_str

    def _reverse_resolve_paths_in_output(self, output: str) -> str:
        """
        Reverse resolve local paths back to container paths in output string.

        Args:
            output: Output string that may contain local paths

        Returns:
            Output with local paths resolved to container paths
        """
        import re

        # Sort mappings by local path length (longest first) for correct prefix matching
        sorted_mappings = sorted(self.path_mappings, key=lambda m: len(m.local_path), reverse=True)

        if not sorted_mappings:
            return output

        # Create pattern that matches absolute paths
        # Match paths like /Users/... or other absolute paths
        result = output
        for mapping in sorted_mappings:
            # Escape the local path for use in regex
            escaped_local = re.escape(os.path.realpath(mapping.local_path)).replace(r"\\", r"[/\\]")
            # Forward path resolution emits '/' even on Windows. Match either
            # separator inside the root and at the path boundary, without
            # treating a similarly prefixed sibling directory as a child.
            pattern = re.compile(
                escaped_local + r"(?=$|[/\\])(?:[/\\][^\s\"';&|<>():]*)?",
                re.IGNORECASE if os.name == "nt" else 0,
            )

            def replace_match(match: re.Match) -> str:
                matched_path = match.group(0)
                return self._reverse_resolve_path(matched_path)

            result = pattern.sub(replace_match, result)

        return result

    def _resolve_paths_in_command(self, command: str) -> str:
        """
        Resolve container paths to local paths in a command string.

        Args:
            command: Command string that may contain container paths

        Returns:
            Command with container paths resolved to local paths
        """
        import re

        # Sort mappings by length (longest first) for correct prefix matching
        sorted_mappings = sorted(self.path_mappings, key=lambda m: len(m.container_path), reverse=True)

        # Build regex pattern to match all container paths
        # Match container path followed by optional path components
        if not sorted_mappings:
            return command

        # Create pattern that matches any of the container paths.
        # The lookahead (?=/|$|...) ensures we only match at a path-segment boundary,
        # preventing /mnt/skills from matching inside /mnt/skills-extra.
        patterns = [re.escape(m.container_path) + r"(?=/|$|[\s\"';&|<>()])(?:/[^\s\"';&|<>()]*)?" for m in sorted_mappings]
        pattern = re.compile("|".join(f"({p})" for p in patterns))

        def replace_match(match: re.Match) -> str:
            matched_path = match.group(0)
            return self._resolve_path(matched_path)

        return pattern.sub(replace_match, command)

    def _resolve_paths_in_content(self, content: str) -> str:
        """Resolve container paths to local paths in arbitrary file content.

        Unlike ``_resolve_paths_in_command`` which uses shell-aware boundary
        characters, this method treats the content as plain text and resolves
        every occurrence of a container path prefix.  Resolved paths are
        normalized to forward slashes to avoid backslash-escape issues on
        Windows hosts (e.g. ``C:\\Users\\..`` breaking Python string literals).

        Args:
            content: File content that may contain container paths.

        Returns:
            Content with container paths resolved to local paths (forward slashes).
        """
        import re

        sorted_mappings = sorted(self.path_mappings, key=lambda m: len(m.container_path), reverse=True)
        if not sorted_mappings:
            return content

        patterns = [re.escape(m.container_path) + r"(?=/|$|[^\w./-])(?:/[^\s\"';&|<>()]*)?" for m in sorted_mappings]
        pattern = re.compile("|".join(f"({p})" for p in patterns))

        def replace_match(match: re.Match) -> str:
            matched_path = match.group(0)
            resolved = self._resolve_path(matched_path)
            # Normalize to forward slashes so that Windows backslash paths
            # don't create invalid escape sequences in source files.
            return resolved.replace("\\", "/")

        return pattern.sub(replace_match, content)

    @staticmethod
    def _get_shell() -> str:
        """Detect available shell executable with fallback."""
        shell = LocalSandbox._find_first_available_shell(("/bin/zsh", "/bin/bash", "/bin/sh", "sh"))
        if shell is not None:
            return shell

        if os.name == "nt":
            system_root = os.environ.get("SystemRoot", r"C:\Windows")
            shell = LocalSandbox._find_first_available_shell(
                (
                    "pwsh",
                    "pwsh.exe",
                    "powershell",
                    "powershell.exe",
                    ntpath.join(system_root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
                    "cmd.exe",
                )
            )
            if shell is not None:
                return shell

            raise RuntimeError("No suitable shell executable found. Tried /bin/zsh, /bin/bash, /bin/sh, `sh` on PATH, then PowerShell and cmd.exe fallbacks for Windows.")

        raise RuntimeError("No suitable shell executable found. Tried /bin/zsh, /bin/bash, /bin/sh, and `sh` on PATH.")

    @staticmethod
    def _format_timeout_notice(timeout: float) -> str:
        seconds = float(timeout)
        amount = str(int(seconds)) if seconds.is_integer() else f"{seconds:g}"
        unit = "second" if seconds == 1 else "seconds"
        return (
            f"Command timed out after {amount} {unit} and was terminated. "
            "Run long-lived processes in the background and redirect their output."
        )

    @staticmethod
    def _drain_pipe(fd: int, capture: _BoundedPipeCapture) -> None:
        try:
            while chunk := os.read(fd, 8192):
                capture.append(chunk)
        except OSError:
            logger.debug("Subprocess output pipe closed while draining", exc_info=True)
        finally:
            try:
                os.close(fd)
            except OSError:
                pass

    def _start_pipe_drain(
        self,
        fd: int,
        name: str,
        *,
        encoding: str,
    ) -> tuple[_BoundedPipeCapture, threading.Thread]:
        capture = _BoundedPipeCapture(
            limit_bytes=self.command_capture_limit_bytes,
            encoding=encoding,
            normalize_newlines=True,
        )
        thread = threading.Thread(
            target=self._drain_pipe,
            args=(fd, capture),
            name=name,
            daemon=True,
        )
        thread.start()
        return capture, thread

    def _run_windows_command(
        self,
        args: list[str],
        *,
        encoding: str | None = None,
    ) -> tuple[str, str, int, bool]:
        """Run a Windows command with bounded capture and tree termination."""
        stdout_read_fd, stdout_write_fd = os.pipe()
        stderr_read_fd, stderr_write_fd = os.pipe()
        try:
            process = subprocess.Popen(
                args,
                shell=False,
                stdin=subprocess.DEVNULL,
                stdout=stdout_write_fd,
                stderr=stderr_write_fd,
                creationflags=subprocess.CREATE_NEW_PROCESS_GROUP,
                env=build_sandbox_subprocess_env(),
            )
        except Exception:
            for fd in (
                stdout_read_fd,
                stdout_write_fd,
                stderr_read_fd,
                stderr_write_fd,
            ):
                try:
                    os.close(fd)
                except OSError:
                    pass
            raise
        finally:
            for fd in (stdout_write_fd, stderr_write_fd):
                try:
                    os.close(fd)
                except OSError:
                    pass

        output_encoding = encoding or locale.getpreferredencoding(False)
        stdout_capture, stdout_thread = self._start_pipe_drain(
            stdout_read_fd,
            "deerflow-bash-stdout-drain",
            encoding=output_encoding,
        )
        stderr_capture, stderr_thread = self._start_pipe_drain(
            stderr_read_fd,
            "deerflow-bash-stderr-drain",
            encoding=output_encoding,
        )

        timed_out = False
        try:
            try:
                process.wait(timeout=self.command_timeout_seconds)
            except subprocess.TimeoutExpired:
                timed_out = True
                self._terminate_windows_process_tree(process)
            returncode = process.returncode if process.returncode is not None else 0
        finally:
            join_timeout = 10 if timed_out else _PIPE_DRAIN_JOIN_TIMEOUT_SECONDS
            for thread in (stdout_thread, stderr_thread):
                thread.join(timeout=join_timeout)
                if thread.is_alive():
                    logger.debug("Subprocess output drain thread still active after command returned")

        return stdout_capture.read(), stderr_capture.read(), returncode, timed_out

    @staticmethod
    def _terminate_windows_process_tree(process: subprocess.Popen) -> None:
        """Terminate a Windows process and its descendants, then reap it."""
        system_root = os.environ.get("SystemRoot", r"C:\Windows")
        taskkill = ntpath.join(system_root, "System32", "taskkill.exe")
        try:
            result = subprocess.run(
                [taskkill, "/PID", str(process.pid), "/T", "/F"],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                timeout=10,
                check=False,
            )
            if result.returncode != 0 and process.poll() is None:
                try:
                    process.kill()
                except OSError:
                    logger.debug("Windows process %s already exited", process.pid)
        except (OSError, subprocess.TimeoutExpired):
            logger.debug(
                "Failed to terminate Windows process tree for pid %s",
                process.pid,
                exc_info=True,
            )
            if process.poll() is None:
                try:
                    process.kill()
                except OSError:
                    logger.debug("Windows process %s already exited", process.pid)

        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            logger.warning("Process tree for pid %s did not exit after taskkill", process.pid)

    def execute_command(self, command: str) -> str:
        # Resolve container paths in command before execution
        resolved_command = self._resolve_paths_in_command(command)
        shell = self._get_shell()

        if os.name == "nt":
            if self._is_powershell(shell):
                args = [
                    shell,
                    "-NoProfile",
                    "-Command",
                    f"{_POWERSHELL_UTF8_PREAMBLE}{resolved_command}",
                ]
                stdout, stderr, returncode, timed_out = self._run_windows_command(
                    args,
                    encoding="utf-8",
                )
            elif self._is_cmd_shell(shell):
                args = [shell, "/c", resolved_command]
                stdout, stderr, returncode, timed_out = self._run_windows_command(args)
            else:
                args = [shell, "-c", resolved_command]
                stdout, stderr, returncode, timed_out = self._run_windows_command(args)
        else:
            args = [shell, "-c", resolved_command]
            result = subprocess.run(
                args,
                shell=False,
                capture_output=True,
                text=True,
                timeout=self.command_timeout_seconds,
                env=build_sandbox_subprocess_env(),
            )
            stdout, stderr, returncode, timed_out = (
                result.stdout,
                result.stderr,
                result.returncode,
                False,
            )
        output = stdout
        if stderr:
            output += f"\nStd Error:\n{stderr}" if output else stderr
        if timed_out:
            notice = self._format_timeout_notice(self.command_timeout_seconds)
            output += f"\n{notice}" if output else notice
        elif returncode != 0:
            output += f"\nExit Code: {returncode}"

        final_output = output if output else "(no output)"
        # Reverse resolve local paths back to container paths in output
        return self._reverse_resolve_paths_in_output(final_output)

    def list_dir(self, path: str, max_depth=2) -> list[str]:
        resolved_path = self._resolve_path(path)
        entries = list_dir(resolved_path, max_depth)
        # Reverse resolve local paths back to container paths and preserve
        # list_dir's trailing "/" marker for directories.
        result: list[str] = []
        for entry in entries:
            is_dir = entry.endswith(("/", "\\"))
            reversed_entry = self._reverse_resolve_path(entry.rstrip("/\\")) if is_dir else self._reverse_resolve_path(entry)
            result.append(f"{reversed_entry}/" if is_dir and not reversed_entry.endswith("/") else reversed_entry)
        return result

    def read_file(
        self,
        path: str,
        start_line: int | None = None,
        end_line: int | None = None,
    ) -> str:
        resolved_path = self._resolve_path(path)
        should_slice = start_line is not None or end_line is not None
        try:
            with open(resolved_path, encoding="utf-8") as f:
                if not should_slice:
                    content = f.read()
                else:
                    start = max(start_line or 1, 1)
                    selected: list[str] = []
                    for line_number, line in enumerate(f, start=1):
                        if line_number < start:
                            continue
                        if end_line is not None and line_number > end_line:
                            break
                        selected.append(line.rstrip("\r\n"))
                    content = "\n".join(selected)
            # Only reverse-resolve paths in files that were previously written
            # by write_file (agent-authored content). User-uploaded files,
            # external tool output, and other non-agent content should not be
            # silently rewritten — see discussion on PR #1935.
            if resolved_path in self._agent_written_paths:
                content = self._reverse_resolve_paths_in_output(content)
            return content
        except OSError as e:
            # Re-raise with the original path for clearer error messages, hiding internal resolved paths
            raise type(e)(e.errno, e.strerror, path) from None

    def write_file(self, path: str, content: str, append: bool = False) -> None:
        resolved = self._resolve_path_with_mapping(path)
        resolved_path = resolved.path
        if self._is_resolved_path_read_only(resolved):
            raise OSError(errno.EROFS, "Read-only file system", path)
        try:
            dir_path = os.path.dirname(resolved_path)
            if dir_path:
                os.makedirs(dir_path, exist_ok=True)
            # Resolve container paths in content to local paths
            # using the content-specific resolver (forward-slash safe)
            resolved_content = self._resolve_paths_in_content(content)
            mode = "a" if append else "w"
            with open(resolved_path, mode, encoding="utf-8") as f:
                f.write(resolved_content)
            # Track this path so read_file knows to reverse-resolve on read.
            # Only agent-written files get reverse-resolved; user uploads and
            # external tool output are left untouched.
            self._agent_written_paths.add(resolved_path)
        except OSError as e:
            # Re-raise with the original path for clearer error messages, hiding internal resolved paths
            raise type(e)(e.errno, e.strerror, path) from None

    def delete_path(self, path: str, *, recursive: bool = False) -> None:
        resolved = self._resolve_path_with_mapping(path)
        if self._is_resolved_path_read_only(resolved):
            raise OSError(errno.EROFS, "Read-only file system", path)

        target = Path(resolved.path)
        try:
            if target.is_symlink() or target.is_file():
                target.unlink()
            elif target.is_dir():
                if recursive:
                    shutil.rmtree(target)
                else:
                    target.rmdir()
            else:
                raise FileNotFoundError(errno.ENOENT, "Path not found", path)
            normalized = str(target)
            self._agent_written_paths = {
                written
                for written in self._agent_written_paths
                if written != normalized and not written.startswith(normalized + os.sep)
            }
        except OSError as e:
            raise type(e)(e.errno, e.strerror, path) from None

    def move_path(
        self,
        source: str,
        destination: str,
        *,
        overwrite: bool = False,
    ) -> None:
        resolved_source = self._resolve_path_with_mapping(source)
        resolved_destination = self._resolve_path_with_mapping(destination)
        if self._is_resolved_path_read_only(resolved_source):
            raise OSError(errno.EROFS, "Read-only file system", source)
        if self._is_resolved_path_read_only(resolved_destination):
            raise OSError(errno.EROFS, "Read-only file system", destination)

        source_path = Path(resolved_source.path)
        destination_path = Path(resolved_destination.path)
        try:
            if not source_path.exists() and not source_path.is_symlink():
                raise FileNotFoundError(errno.ENOENT, "Path not found", source)
            if os.path.normcase(os.path.abspath(source_path)) == os.path.normcase(
                os.path.abspath(destination_path)
            ):
                raise FileExistsError(
                    errno.EEXIST,
                    "Source and destination are the same path",
                    destination,
                )
            if destination_path.exists() or destination_path.is_symlink():
                if not overwrite:
                    raise FileExistsError(errno.EEXIST, "Destination already exists", destination)
                if destination_path.is_dir() and not destination_path.is_symlink():
                    raise IsADirectoryError(
                        errno.EISDIR,
                        "Refusing to overwrite an existing directory",
                        destination,
                    )
                destination_path.unlink()

            destination_path.parent.mkdir(parents=True, exist_ok=True)
            shutil.move(str(source_path), str(destination_path))

            source_prefix = str(source_path)
            destination_prefix = str(destination_path)
            updated_paths: set[str] = set()
            for written in self._agent_written_paths:
                if written == source_prefix:
                    updated_paths.add(destination_prefix)
                elif written.startswith(source_prefix + os.sep):
                    updated_paths.add(destination_prefix + written[len(source_prefix) :])
                else:
                    updated_paths.add(written)
            self._agent_written_paths = updated_paths
        except OSError as e:
            filename = destination if isinstance(e, (FileExistsError, IsADirectoryError)) else source
            raise type(e)(e.errno, e.strerror, filename) from None

    def glob(self, path: str, pattern: str, *, include_dirs: bool = False, max_results: int = 200) -> tuple[list[str], bool]:
        resolved_path = Path(self._resolve_path(path))
        matches, truncated = find_glob_matches(resolved_path, pattern, include_dirs=include_dirs, max_results=max_results)
        return [self._reverse_resolve_path(match) for match in matches], truncated

    def grep(
        self,
        path: str,
        pattern: str,
        *,
        glob: str | None = None,
        literal: bool = False,
        case_sensitive: bool = False,
        max_results: int = 100,
    ) -> tuple[list[GrepMatch], bool]:
        resolved_path = Path(self._resolve_path(path))
        matches, truncated = find_grep_matches(
            resolved_path,
            pattern,
            glob_pattern=glob,
            literal=literal,
            case_sensitive=case_sensitive,
            max_results=max_results,
        )
        return [
            GrepMatch(
                path=self._reverse_resolve_path(match.path),
                line_number=match.line_number,
                line=match.line,
            )
            for match in matches
        ], truncated

    def update_file(self, path: str, content: bytes) -> None:
        resolved = self._resolve_path_with_mapping(path)
        resolved_path = resolved.path
        if self._is_resolved_path_read_only(resolved):
            raise OSError(errno.EROFS, "Read-only file system", path)
        try:
            dir_path = os.path.dirname(resolved_path)
            if dir_path:
                os.makedirs(dir_path, exist_ok=True)
            with open(resolved_path, "wb") as f:
                f.write(content)
        except OSError as e:
            # Re-raise with the original path for clearer error messages, hiding internal resolved paths
            raise type(e)(e.errno, e.strerror, path) from None

    def download_file(self, path: str) -> bytes:
        """Return raw bytes for *path* under ``/mnt/user-data``.

        Paths outside the virtual user-data prefix are rejected to prevent
        clients from exfiltrating arbitrary host files. Downloads are capped
        at 100 MB so a runaway agent cannot OOM the server.
        """
        normalised = path.replace("\\", "/")
        stripped_path = normalised.lstrip("/")
        allowed_prefix = VIRTUAL_PATH_PREFIX.lstrip("/")
        if stripped_path != allowed_prefix and not stripped_path.startswith(f"{allowed_prefix}/"):
            logger.error("Refused download outside allowed directory: path=%s, allowed_prefix=%s", path, VIRTUAL_PATH_PREFIX)
            raise PermissionError(errno.EACCES, f"Access denied: path must be under '{VIRTUAL_PATH_PREFIX}'", path)

        resolved_path = self._resolve_path(path)
        try:
            file_size = os.path.getsize(resolved_path)
            if file_size > _MAX_DOWNLOAD_SIZE:
                raise OSError(errno.EFBIG, f"File exceeds maximum download size of {_MAX_DOWNLOAD_SIZE} bytes", path)
            with open(resolved_path, "rb") as f:
                return f.read()
        except OSError as e:
            # Re-raise with the original path for clearer error messages, hiding internal resolved paths
            raise type(e)(e.errno, e.strerror, path) from None
