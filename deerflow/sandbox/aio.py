from __future__ import annotations

import errno
import logging
import os
import re
import shlex
import subprocess
import threading
import time
import uuid
from collections import OrderedDict
from collections.abc import Collection, Iterator
from contextlib import contextmanager, nullcontext
from dataclasses import dataclass
from pathlib import Path

from deerflow.config.paths import VIRTUAL_PATH_PREFIX, get_paths
from deerflow.sandbox.sandbox import Sandbox
from deerflow.sandbox.sandbox_provider import SandboxProvider
from deerflow.sandbox.search import GrepMatch
from deerflow.sandbox.command import COMMAND_CLEANUP_SECONDS, CommandResult, linux_supervisor_args, run_host_command, validate_command_timeout

logger = logging.getLogger(__name__)

_MAX_DOWNLOAD_SIZE = 100 * 1024 * 1024  # 100 MB

DEFAULT_IMAGE = "enterprise-public-cn-beijing.cr.volces.com/vefaas-public/all-in-one-sandbox:latest"
DEFAULT_REPLICAS = 3
DEFAULT_CONTAINER_PREFIX = "deer-flow-sandbox"
DEFAULT_IDLE_TIMEOUT_SECONDS = 600
DEFAULT_COMMAND_TIMEOUT_SECONDS = 600

_SAFE_THREAD_ID_RE = re.compile(r"^[A-Za-z0-9_-]+$")


@dataclass
class _SandboxRecord:
    sandbox: AioSandbox
    thread_id: str
    last_used: float
    skills_revision: str
    active_operations: int = 0
    execution_uncertain: bool = False
    retiring: bool = False
    owner_token: str | None = None
    creation_pending: bool = False
    creation_may_complete: bool = False


class _OperationLease:
    def __init__(self, provider: AioSandboxProvider, record: _SandboxRecord) -> None:
        self._provider = provider
        self._record = record

    def mark_uncertain(self) -> None:
        """Keep capacity reserved when remote command termination is unknown."""
        with self._provider._lock:
            self._record.execution_uncertain = True


class AioSandbox(Sandbox):
    """Docker-backed sandbox that exposes mounted paths directly in-container."""

    def __init__(self, id: str, container_name: str, *, timeout: float = DEFAULT_COMMAND_TIMEOUT_SECONDS, container_user: str | None = None) -> None:
        super().__init__(id)
        self.container_name = container_name
        self.timeout = validate_command_timeout(timeout)
        self._container_user = container_user
        self._operation_factory = nullcontext
        self._command_shell: str | None = None

    def _exec_argv(self, args: list[str]) -> list[str]:
        return ["docker", "exec", "-i", *(["-u", self._container_user] if self._container_user else []), self.container_name, *args]

    def _docker_exec(
        self,
        args: list[str],
        *,
        input_data: str | bytes | None = None,
        text: bool = True,
        check: bool = False,
    ) -> subprocess.CompletedProcess:
        with self._operation_factory() as lease:
            try:
                result = subprocess.run(
                    self._exec_argv(args), input=input_data, shell=False,
                    capture_output=True, text=text, timeout=self.timeout, check=check,
                )
            except FileNotFoundError:
                # The host could not start the Docker executable: no remote
                # operation was launched and there is no occupancy to retain.
                raise
            except (OSError, subprocess.CalledProcessError) as exc:
                # An interrupted transport does not establish whether a remote
                # mutation finished. TimeoutExpired is handled by the provider's
                # operation context using the same uncertainty flag.
                known_protocol_failure = isinstance(exc, subprocess.CalledProcessError) and 2 <= exc.returncode <= 9
                if not known_protocol_failure and hasattr(lease, "mark_uncertain"):
                    lease.mark_uncertain()
                raise
            diagnostic = result.stderr or ""
            if isinstance(diagnostic, bytes):
                diagnostic = diagnostic.decode("utf-8", errors="replace")
            transport_failure = result.returncode < 0 or (
                result.returncode != 0 and not 2 <= result.returncode <= 9
                and any(marker in diagnostic.lower() for marker in (
                    "error during connect", "error response from daemon", "error from daemon",
                    "connection reset by peer", "broken pipe", "unexpected eof",
                    "context deadline exceeded", "context canceled", "transport is closing",
                    "error waiting for container", "unable to upgrade to tcp",
                ))
            )
            if transport_failure:
                if hasattr(lease, "mark_uncertain"):
                    lease.mark_uncertain()
                # A nonzero result would trigger write_file's interpreter
                # fallback and could repeat an append already started remotely.
                raise RuntimeError("Docker transport ended without confirming the remote operation; the operation was not retried")
            return result

    @staticmethod
    def _output_from_result(result: subprocess.CompletedProcess) -> str:
        stdout = result.stdout or ""
        stderr = result.stderr or ""
        if isinstance(stdout, bytes):
            stdout = stdout.decode("utf-8", errors="replace")
        if isinstance(stderr, bytes):
            stderr = stderr.decode("utf-8", errors="replace")

        output = stdout
        if stderr:
            output += f"\nStd Error:\n{stderr}" if output else stderr
        if result.returncode != 0:
            output += f"\nExit Code: {result.returncode}"
        return output if output else "(no output)"

    @staticmethod
    def _quote(path: str) -> str:
        return shlex.quote(path)

    def execute_command(self, command: str) -> str:
        return self.execute_command_result(command).output

    def execute_command_result(self, command: str, *, cancel_event: threading.Event | None = None) -> CommandResult:
        with self._operation_factory() as lease:
            if cancel_event is not None and cancel_event.is_set():
                return CommandResult("Command cancelled before execution.", None, "cancelled", True)
            if self._command_shell is None:
                # Resolve the shell before user code. A user's exit 126/127 is
                # never a reason to execute that command a second time.
                probe = run_host_command(
                    self._exec_argv(["/bin/sh", "-c", "if [ -x /bin/bash ]; then printf /bin/bash; elif [ -x /bin/sh ]; then printf /bin/sh; else exit 127; fi"]),
                    timeout=min(self.timeout, 10), cancel_event=cancel_event,
                )
                if not probe.succeeded:
                    if not probe.termination_confirmed and hasattr(lease, "mark_uncertain"):
                        lease.mark_uncertain()
                    return probe
                shell = probe.output.strip()
                if shell not in {"/bin/bash", "/bin/sh"}:
                    return CommandResult("Error: Could not identify a supported container shell.", 127)
                self._command_shell = shell
            supervised, token = linux_supervisor_args(self._command_shell, command, self.timeout)
            try:
                result = run_host_command(
                    self._exec_argv(supervised), timeout=self.timeout + COMMAND_CLEANUP_SECONDS,
                    cancel_event=cancel_event, remote_token=token, command_timeout=self.timeout,
                )
            except BaseException:
                # A transport/runner exception after launch cannot prove that
                # remote children stopped. Keep the provider generation pinned.
                if hasattr(lease, "mark_uncertain"):
                    lease.mark_uncertain()
                raise
            if not result.termination_confirmed and hasattr(lease, "mark_uncertain"):
                lease.mark_uncertain()
            return result

    def read_file(
        self,
        path: str,
        start_line: int | None = None,
        end_line: int | None = None,
    ) -> str:
        from deerflow.sandbox.file_io import MAX_READ_BYTES

        # Preserve the legacy full-string contract while sharing typed errors,
        # bounded transport pages and optimistic version checking.
        chunk = self.read_file_chunk(path, max_bytes=MAX_READ_BYTES,
                                     start_line=start_line, end_line=end_line)
        content = [chunk.content]
        while chunk.next_offset is not None:
            chunk = self.read_file_chunk(path, offset=chunk.next_offset, max_bytes=MAX_READ_BYTES,
                                         expected_version=chunk.version, end_line=end_line)
            content.append(chunk.content)
        return "".join(content).replace("\r\n", "\n").replace("\r", "\n")

    def list_dir(self, path: str, max_depth=2) -> list[str]:
        script = r"""
import os, sys, fnmatch
root = os.path.realpath(sys.argv[1])
max_depth = int(sys.argv[2])
ignore = sys.argv[3].split("\x1f") if sys.argv[3] else []
try:
    os.stat(root)
except FileNotFoundError:
    print("path does not exist", file=sys.stderr)
    sys.exit(2)
except PermissionError:
    print("permission denied", file=sys.stderr)
    sys.exit(4)
if not os.path.isdir(root):
    print("path is not a directory", file=sys.stderr)
    sys.exit(3)

def ignored(name):
    return any(fnmatch.fnmatch(name, pat) for pat in ignore)

result = []

def within(candidate):
    try:
        return os.path.commonpath([root, os.path.realpath(candidate)]) == root
    except OSError:
        return False

def walk(current, depth):
    if depth > max_depth:
        return
    entries = sorted(os.listdir(current))
    for name in entries:
        if ignored(name):
            continue
        item = os.path.join(current, name)
        try:
            real = os.path.realpath(item)
            if not within(real):
                continue
            is_dir = os.path.isdir(real)
        except FileNotFoundError:
            continue
        result.append(real + ("/" if is_dir else ""))
        if is_dir and depth < max_depth:
            walk(real, depth + 1)

try:
    walk(root, 1)
except PermissionError:
    print("permission denied", file=sys.stderr)
    sys.exit(4)
except OSError as exc:
    print(str(exc), file=sys.stderr)
    sys.exit(5)
else:
    print("\n".join(result))
"""
        from deerflow.sandbox.search import IGNORE_PATTERNS

        result = self._docker_exec(["python3", "-", path, str(max_depth), "\x1f".join(IGNORE_PATTERNS)], input_data=script)
        if result.returncode in (126, 127):
            result = self._docker_exec(["python", "-", path, str(max_depth), "\x1f".join(IGNORE_PATTERNS)], input_data=script)
        if result.returncode == 2:
            raise FileNotFoundError(path)
        if result.returncode == 3:
            raise NotADirectoryError(path)
        if result.returncode == 4:
            raise PermissionError(path)
        if result.returncode != 0:
            stderr = (result.stderr or "").strip()
            raise RuntimeError(f"Failed to list sandbox directory {path!r}: {stderr or f'exit code {result.returncode}'}")
        if not result.stdout:
            return []
        return [line for line in result.stdout.splitlines() if line]

    def write_file(self, path: str, content: str, append: bool = False) -> None:
        quoted_path = self._quote(path)
        mode = "ab" if append else "wb"
        script = (
            "import os, sys\n"
            f"path = {path!r}\n"
            "parent = os.path.dirname(path)\n"
            "if parent:\n"
            "    os.makedirs(parent, exist_ok=True)\n"
            f"with open(path, {mode!r}) as f:\n"
            "    f.write(sys.stdin.buffer.read())\n"
        )
        result = self._docker_exec(["python3", "-c", script], input_data=content.encode("utf-8"), text=False)
        if result.returncode != 0:
            result = self._docker_exec(
                ["/bin/sh", "-c", f"mkdir -p $(dirname {quoted_path}) && cat {'>>' if append else '>'} {quoted_path}"],
                input_data=content.encode("utf-8"),
                text=False,
            )
        if result.returncode != 0:
            raise OSError(f"Failed to write file {path}: {self._output_from_result(result)}")

    def delete_path(self, path: str, *, recursive: bool = False) -> None:
        script = r"""
import os, shutil, sys
path = sys.argv[1]
recursive = sys.argv[2] == "1"
if os.path.islink(path) or os.path.isfile(path):
    os.unlink(path)
elif os.path.isdir(path):
    if recursive:
        shutil.rmtree(path)
    else:
        os.rmdir(path)
else:
    raise FileNotFoundError(path)
"""
        result = self._docker_exec(
            ["python3", "-", path, "1" if recursive else "0"],
            input_data=script,
        )
        if result.returncode != 0:
            raise OSError(f"Failed to delete path {path}: {self._output_from_result(result)}")

    def move_path(
        self,
        source: str,
        destination: str,
        *,
        overwrite: bool = False,
    ) -> None:
        script = r"""
import os, shutil, sys
source, destination = sys.argv[1], sys.argv[2]
overwrite = sys.argv[3] == "1"
if not os.path.lexists(source):
    raise FileNotFoundError(source)
if os.path.abspath(source) == os.path.abspath(destination):
    raise FileExistsError("source and destination are the same path")
if os.path.lexists(destination):
    if not overwrite:
        raise FileExistsError(destination)
    if os.path.isdir(destination) and not os.path.islink(destination):
        raise IsADirectoryError(destination)
    os.unlink(destination)
parent = os.path.dirname(destination)
if parent:
    os.makedirs(parent, exist_ok=True)
shutil.move(source, destination)
"""
        result = self._docker_exec(
            ["python3", "-", source, destination, "1" if overwrite else "0"],
            input_data=script,
        )
        if result.returncode != 0:
            raise OSError(
                f"Failed to move path {source} to {destination}: "
                f"{self._output_from_result(result)}"
            )

    def _remote_file_io(self, operation: str, path: str, **options):
        import json

        from deerflow.sandbox.remote_file_io import remote_file_io_script

        request = json.dumps({"operation": operation, "path": path, "options": options}, ensure_ascii=True)
        result = self._docker_exec(["python3", "-", request], input_data=remote_file_io_script())
        if result.returncode in (126, 127):
            result = self._docker_exec(["python", "-", request], input_data=remote_file_io_script())
        self._raise_remote_search_error(result, path, operation=operation)
        try:
            payload = json.loads(result.stdout)
            if not isinstance(payload, dict):
                raise ValueError("expected an object")
            return payload
        except (TypeError, ValueError) as exc:
            raise OSError(errno.EIO, f"Remote {operation} returned an invalid response", path) from exc

    def read_file_chunk(self, path: str, *, offset: int = 0, max_bytes: int = 65536,
                        expected_version: str | None = None, start_line: int | None = None,
                        end_line: int | None = None):
        from deerflow.sandbox.file_io import FileChunk

        payload = self._remote_file_io("read_file_chunk", path, offset=offset, max_bytes=max_bytes,
                                       expected_version=expected_version, start_line=start_line, end_line=end_line)
        try:
            return FileChunk(**payload)
        except (TypeError, KeyError, ValueError) as exc:
            raise OSError(errno.EIO, "Invalid remote file chunk", path) from exc

    def list_dir_page(self, path: str, *, max_depth: int = 2, limit: int = 200,
                      cursor: str | None = None):
        from deerflow.sandbox.file_io import DirectoryPage

        payload = self._remote_file_io("list_dir_page", path, max_depth=max_depth, limit=limit, cursor=cursor)
        try:
            return DirectoryPage(**payload)
        except (TypeError, KeyError, ValueError) as exc:
            raise OSError(errno.EIO, "Invalid remote directory page", path) from exc

    def glob(self, path: str, pattern: str, *, include_dirs: bool = False, max_results: int = 200) -> tuple[list[str], bool]:
        result = self.glob_result(path, pattern, include_dirs=include_dirs, max_results=max_results)
        return result.matches, result.truncated

    def glob_result(self, path: str, pattern: str, *, include_dirs: bool = False, max_results: int = 200):
        from deerflow.sandbox.search import GlobResult, SearchCoverage

        payload = self._remote_file_io("glob_result", path, pattern=pattern, include_dirs=include_dirs, max_results=max_results)
        try:
            return GlobResult(payload["matches"], payload["truncated"], SearchCoverage(**payload["coverage"]))
        except (TypeError, KeyError, ValueError) as exc:
            raise OSError(errno.EIO, "Invalid remote glob result", path) from exc

    def grep(self, path: str, pattern: str, *, glob: str | None = None, literal: bool = False,
             case_sensitive: bool = False, max_results: int = 100) -> tuple[list[GrepMatch], bool]:
        result = self.grep_result(path, pattern, glob=glob, literal=literal,
                                  case_sensitive=case_sensitive, max_results=max_results)
        return result.matches, result.truncated

    def grep_result(self, path: str, pattern: str, *, glob: str | None = None, literal: bool = False,
                    case_sensitive: bool = False, max_results: int = 100):
        from deerflow.sandbox.search import GrepResult, SearchCoverage

        payload = self._remote_file_io("grep_result", path, pattern=pattern, glob_pattern=glob, literal=literal,
                                       case_sensitive=case_sensitive, max_results=max_results)
        try:
            return GrepResult([GrepMatch(**item) for item in payload["matches"]], payload["truncated"],
                              SearchCoverage(**payload["coverage"]))
        except (TypeError, KeyError, ValueError) as exc:
            raise OSError(errno.EIO, "Invalid remote grep result", path) from exc

    @staticmethod
    def _raise_remote_search_error(
        result: subprocess.CompletedProcess,
        path: str,
        *,
        operation: str,
    ) -> None:
        """Map the remote helper protocol without disguising execution errors."""
        if result.returncode == 0:
            return
        stderr = result.stderr or ""
        if isinstance(stderr, bytes):
            stderr = stderr.decode("utf-8", errors="replace")
        detail = stderr.strip() or f"exit code {result.returncode}"
        if result.returncode == 2:
            raise FileNotFoundError(errno.ENOENT, detail, path)
        if result.returncode == 3:
            raise NotADirectoryError(errno.ENOTDIR, detail, path)
        if result.returncode == 4:
            raise PermissionError(errno.EACCES, detail, path)
        if result.returncode == 6:
            raise re.error(f"Invalid grep pattern: {detail}")
        if result.returncode == 7:
            from deerflow.sandbox.file_io import FileVersionMismatchError

            raise FileVersionMismatchError(detail)
        if result.returncode == 8:
            raise ValueError(detail)
        if result.returncode == 9:
            raise IsADirectoryError(errno.EISDIR, detail, path)
        raise OSError(
            errno.EIO,
            f"Remote {operation} failed for {path!r}: {detail}",
            path,
        )

    def update_file(self, path: str, content: bytes) -> None:
        quoted_path = self._quote(path)
        result = self._docker_exec(
            ["/bin/sh", "-c", f"mkdir -p $(dirname {quoted_path}) && cat > {quoted_path}"],
            input_data=content,
            text=False,
        )
        if result.returncode != 0:
            raise OSError(f"Failed to update file {path}: {self._output_from_result(result)}")

    def download_file(self, path: str) -> bytes:
        """Return raw bytes for *path* from the container under ``/mnt/user-data``.

        Paths outside the virtual user-data prefix are rejected before
        spawning the docker exec to prevent agents from exfiltrating
        arbitrary container files. Output is capped at 100 MB.
        """
        normalised = path.replace("\\", "/")
        stripped_path = normalised.lstrip("/")
        allowed_prefix = VIRTUAL_PATH_PREFIX.lstrip("/")
        if stripped_path != allowed_prefix and not stripped_path.startswith(f"{allowed_prefix}/"):
            logger.error("Refused download outside allowed directory: path=%s, allowed_prefix=%s", path, VIRTUAL_PATH_PREFIX)
            raise PermissionError(errno.EACCES, f"Access denied: path must be under '{VIRTUAL_PATH_PREFIX}'", path)

        # ``cat -- path`` ensures paths starting with '-' are treated literally.
        result = self._docker_exec(["cat", "--", path], text=False)
        if result.returncode != 0:
            stderr = result.stderr or b""
            if isinstance(stderr, bytes):
                stderr = stderr.decode("utf-8", errors="replace")
            if "No such file" in stderr or result.returncode == 1:
                raise FileNotFoundError(errno.ENOENT, stderr.strip() or "File not found", path)
            raise OSError(errno.EIO, stderr.strip() or "docker cat failed", path)

        data = result.stdout or b""
        if isinstance(data, str):
            data = data.encode("utf-8", errors="replace")
        if len(data) > _MAX_DOWNLOAD_SIZE:
            raise OSError(errno.EFBIG, f"File exceeds maximum download size of {_MAX_DOWNLOAD_SIZE} bytes", path)
        return data


class AioSandboxProvider(SandboxProvider):
    """Docker CLI based sandbox provider.

    Each thread gets one long-running container. Thread data is bind-mounted at
    /mnt/user-data, skills are mounted read-only, and configured custom mounts
    are passed through with their declared read-only mode. Idle cleanup is
    opportunistic on acquisition; there is no background timer. Running or
    unconfirmed operations retain their capacity until they finish.
    """

    uses_thread_data_mounts = False

    def __init__(self) -> None:
        from deerflow.config import get_app_config

        config = get_app_config()
        sandbox_cfg = config.sandbox
        self.image = sandbox_cfg.image or DEFAULT_IMAGE
        self.replicas = sandbox_cfg.replicas or DEFAULT_REPLICAS
        self.container_prefix = sandbox_cfg.container_prefix or DEFAULT_CONTAINER_PREFIX
        self.idle_timeout = DEFAULT_IDLE_TIMEOUT_SECONDS if sandbox_cfg.idle_timeout is None else sandbox_cfg.idle_timeout
        self.command_timeout = getattr(sandbox_cfg, "bash_command_timeout", DEFAULT_COMMAND_TIMEOUT_SECONDS)
        self.environment = sandbox_cfg.environment or {}
        self.mounts = sandbox_cfg.mounts or []
        self.security_opt = getattr(sandbox_cfg, "security_opt", None) or []
        raw_user = getattr(sandbox_cfg, "container_user", "auto")
        if raw_user == "auto":
            self.container_user: str | None = _host_uid_gid()
            self.container_run_user: str | None = None
        else:
            self.container_user = raw_user or None
            self.container_run_user = self.container_user
        self.skills_container_path = config.skills.container_path
        self._lock = threading.Lock()
        self._records: OrderedDict[str, _SandboxRecord] = OrderedDict()
        self._closing = False
        self._verify_docker_available()

    def _verify_docker_available(self) -> None:
        try:
            result = subprocess.run(["docker", "version", "--format", "{{.Server.Version}}"], shell=False, capture_output=True, text=True, timeout=15)
        except FileNotFoundError as exc:
            raise RuntimeError("Docker CLI was not found on PATH. Install Docker before using AioSandboxProvider.") from exc
        if result.returncode != 0:
            raise RuntimeError(f"Docker daemon is not available: {(result.stderr or result.stdout).strip()}")

    @staticmethod
    def _safe_thread_id(thread_id: str | None) -> str:
        value = thread_id or "default"
        if not _SAFE_THREAD_ID_RE.fullmatch(value):
            raise ValueError(f"Invalid thread_id {value!r}: only alphanumeric characters, hyphens, and underscores are allowed.")
        return value

    def _sandbox_id(self, thread_id: str, skills_revision: str) -> str:
        return f"aio-{thread_id}-{skills_revision}"

    def _container_name(self, sandbox_id: str) -> str:
        return f"{self.container_prefix}-{sandbox_id}"

    def _run_docker(self, args: list[str], *, check: bool = True) -> subprocess.CompletedProcess:
        return subprocess.run(["docker", *args], shell=False, capture_output=True, text=True, timeout=60, check=check)

    def _build_run_args(
        self,
        thread_id: str,
        sandbox_id: str,
        *,
        skills_path: str,
        skills_revision: str,
        container_name: str | None = None,
        owner_token: str | None = None,
    ) -> list[str]:
        paths = get_paths()
        paths.ensure_thread_dirs(thread_id)
        args = [
            "run",
            "-d",
            "--name",
            container_name or self._container_name(sandbox_id),
            "--label",
            "deerflow.sandbox.provider=aio",
            "--label",
            f"deerflow.sandbox.id={sandbox_id}",
            "--workdir",
            "/mnt/user-data/workspace",
            "-v",
            f"{paths.host_sandbox_user_data_dir(thread_id)}:/mnt/user-data:rw",
            "-v",
            f"{paths.host_acp_workspace_dir(thread_id)}:/mnt/acp-workspace:rw",
        ]
        if owner_token is not None:
            args.extend(["--label", f"deerflow.sandbox.owner={owner_token}"])
        if Path(skills_path).exists():
            host_projection = paths.host_skill_projection_dir(skills_revision)
            args.extend(["-v", f"{host_projection}:{self.skills_container_path}:ro"])
        for mount in self.mounts:
            if not os.path.exists(mount.host_path):
                continue
            mode = "ro" if mount.read_only else "rw"
            args.extend(["-v", f"{mount.host_path}:{mount.container_path}:{mode}"])
        for opt in self.security_opt:
            if opt:
                args.extend(["--security-opt", opt])
        if self.container_run_user:
            args.extend(["--user", self.container_run_user])
        for key, value in self.environment.items():
            resolved = os.environ.get(value[1:], "") if isinstance(value, str) and value.startswith("$") else value
            args.extend(["-e", f"{key}={resolved}"])
        args.extend([self.image, "sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 3600; done"])
        return args

    def _start_container(
        self,
        thread_id: str,
        sandbox_id: str,
        *,
        skills_path: str,
        skills_revision: str,
    ) -> AioSandbox:
        owner_token = uuid.uuid4().hex
        name = f"{self._container_name(sandbox_id)}-{owner_token}"
        sandbox = AioSandbox(sandbox_id, name, timeout=self.command_timeout, container_user=self.container_user)
        record = _SandboxRecord(
            sandbox=sandbox, thread_id=thread_id, last_used=time.monotonic(),
            skills_revision=skills_revision, owner_token=owner_token, creation_pending=True,
        )
        # Reserve capacity before the request. A timed-out Docker CLI can leave
        # a creation request still running at the daemon.
        self._records[sandbox_id] = record
        sandbox._operation_factory = lambda: self._operation(sandbox_id, record)
        try:
            result = self._run_docker(
                _as_str_list(
                    self._build_run_args(
                        thread_id,
                        sandbox_id,
                        skills_path=skills_path,
                        skills_revision=skills_revision,
                        container_name=name,
                        owner_token=owner_token,
                    )
                )
            )
        except (subprocess.CalledProcessError, subprocess.TimeoutExpired, OSError) as exc:
            record.creation_may_complete = isinstance(exc, subprocess.TimeoutExpired)
            try:
                self._reconcile_creation_locked(sandbox_id, record)
            except Exception:
                logger.warning("Sandbox creation remains unconfirmed for %s", name, exc_info=True)
            if isinstance(exc, subprocess.TimeoutExpired):
                diagnostic = "Docker timed out; creation may still be in progress"
            elif isinstance(exc, subprocess.CalledProcessError):
                diagnostic = (exc.stderr or exc.stdout or "Docker returned a failure").strip()
            else:
                diagnostic = str(exc)
            raise RuntimeError(f"Failed to start sandbox container {name}: {diagnostic}") from exc
        # Docker IDs are immutable. Never execute or delete a later generation
        # merely because it has reused the same human-readable container name.
        container_id = result.stdout.strip()
        if not re.fullmatch(r"[0-9a-f]{64}", container_id):
            self._reconcile_creation_locked(sandbox_id, record)
            if record.creation_pending or self._records.get(sandbox_id) is not record:
                raise RuntimeError(f"Docker did not return a confirmed container ID for {name}")
        else:
            sandbox.container_name = container_id
            record.creation_pending = False
        return sandbox

    def _reconcile_creation_locked(self, sandbox_id: str, record: _SandboxRecord) -> None:
        """Resolve one reserved creation without touching another owner's container."""
        import json

        result = self._run_docker(["inspect", "--format", "{{json .}}", record.sandbox.container_name], check=False)
        if result.returncode != 0:
            diagnostic = (result.stderr or result.stdout or "").strip()
            if "no such object:" in diagnostic.lower() or "no such container:" in diagnostic.lower():
                if not record.creation_may_complete:
                    del self._records[sandbox_id]
                return
            raise RuntimeError(f"Could not reconcile sandbox creation: {diagnostic}")
        try:
            identity = json.loads(result.stdout)
            container_id = identity["Id"]
            owner_token = identity["Config"]["Labels"].get("deerflow.sandbox.owner")
        except (AttributeError, TypeError, KeyError, ValueError) as exc:
            raise RuntimeError("Invalid sandbox creation identity") from exc
        if owner_token != record.owner_token or not isinstance(container_id, str) or not re.fullmatch(r"[0-9a-f]{64}", container_id):
            raise RuntimeError("Sandbox creation identity does not match its owner; capacity remains reserved")
        record.sandbox.container_name = container_id
        record.creation_pending = False
        record.creation_may_complete = False

    def _reconcile_uncertain_locked(self) -> None:
        for sandbox_id, record in list(self._records.items()):
            if record.creation_pending:
                self._reconcile_creation_locked(sandbox_id, record)
            elif record.execution_uncertain and not record.active_operations:
                if self._container_running(record.sandbox.container_name) is not True:
                    self._remove_record_locked(sandbox_id, record)

    def _container_running(self, container_id: str) -> bool | None:
        """Observe state without starting, renewing, or executing in a container."""
        result = self._run_docker(["inspect", "--format", "{{.State.Running}}", container_id], check=False)
        if result.returncode != 0:
            diagnostic = (result.stderr or result.stdout or "").strip()
            if "no such object:" in diagnostic.lower() or "no such container:" in diagnostic.lower():
                return None
            raise RuntimeError(f"Could not inspect sandbox {container_id}: {diagnostic}")
        state = result.stdout.strip().lower()
        if state not in {"true", "false"}:
            raise RuntimeError(f"Invalid container state for sandbox {container_id}: {state!r}")
        return state == "true"

    @contextmanager
    def _operation(self, sandbox_id: str, record: _SandboxRecord) -> Iterator[_OperationLease]:
        with self._lock:
            if self._closing or self._records.get(sandbox_id) is not record or record.retiring or record.creation_pending:
                raise RuntimeError("Sandbox is no longer available; acquire a new sandbox before executing")
            if record.execution_uncertain:
                raise RuntimeError("Sandbox has an operation whose termination is unconfirmed")
            record.active_operations += 1
        lease = _OperationLease(self, record)
        try:
            yield lease
        except subprocess.TimeoutExpired:
            lease.mark_uncertain()
            raise
        finally:
            with self._lock:
                record.active_operations -= 1
                record.last_used = time.monotonic()
                if self._records.get(sandbox_id) is record:
                    self._records.move_to_end(sandbox_id)

    def _remove_record_locked(self, sandbox_id: str, record: _SandboxRecord) -> None:
        if self._records.get(sandbox_id) is not record:
            return
        if record.creation_pending:
            self._reconcile_creation_locked(sandbox_id, record)
            if self._records.get(sandbox_id) is not record:
                return
            if record.creation_pending:
                raise RuntimeError("Sandbox creation is unconfirmed; capacity remains reserved")
        if record.active_operations:
            raise RuntimeError("Sandbox is busy; retry cleanup after its operations finish")
        if record.execution_uncertain and self._container_running(record.sandbox.container_name) is True:
            raise RuntimeError("Sandbox termination is unconfirmed; capacity remains reserved")
        record.retiring = True
        self._remove_container(record.sandbox.container_name)
        # Keep the record, its Skill projection and its capacity on any failure.
        del self._records[sandbox_id]

    def _live_record_locked(self, sandbox_id: str) -> _SandboxRecord | None:
        record = self._records.get(sandbox_id)
        if record is None:
            return None
        if record.creation_pending:
            self._reconcile_creation_locked(sandbox_id, record)
            if self._records.get(sandbox_id) is not record:
                return None
            if record.creation_pending:
                raise RuntimeError("Sandbox creation is unconfirmed; retry after Docker resolves the request")
        if record.retiring or self._container_running(record.sandbox.container_name) is not True:
            self._remove_record_locked(sandbox_id, record)
            return None
        record.last_used = time.monotonic()
        self._records.move_to_end(sandbox_id)
        return record

    def _evict_if_needed(self) -> None:
        while len(self._records) >= self.replicas and self._records:
            candidate = next(
                ((sandbox_id, record) for sandbox_id, record in self._records.items()
                 if not record.active_operations and not record.execution_uncertain and not record.creation_pending),
                None,
            )
            if candidate is None:
                raise RuntimeError("Sandbox capacity is full and all sandboxes are busy; retry when an operation finishes")
            self._remove_record_locked(*candidate)

    def _remove_container(self, container_name: str) -> None:
        result = self._run_docker(["rm", "-f", container_name], check=False)
        if self._container_running(container_name) is not None:
            diagnostic = (result.stderr or result.stdout or "").strip()
            raise RuntimeError(f"Sandbox removal was not confirmed for {container_name}: {diagnostic}")

    def _cleanup_idle_locked(self) -> None:
        if self.idle_timeout == 0:
            return
        now = time.monotonic()
        expired = [
            sandbox_id
            for sandbox_id, record in self._records.items()
            if not record.active_operations and not record.execution_uncertain and not record.creation_pending and now - record.last_used > self.idle_timeout
        ]
        for sandbox_id in expired:
            self._remove_record_locked(sandbox_id, self._records[sandbox_id])

    def acquire(
        self,
        thread_id: str | None = None,
        *,
        available_skills: Collection[str] | None = None,
        workspace_path: str | None = None,
    ) -> str:
        from deerflow.skills.projection import get_skill_projection

        if workspace_path is not None:
            raise ValueError("AioSandboxProvider does not support external workspace mounts")

        safe_thread_id = self._safe_thread_id(thread_id)
        projection = get_skill_projection(available_skills)
        sandbox_id = self._sandbox_id(safe_thread_id, projection.revision)
        with self._lock:
            if self._closing:
                raise RuntimeError("Sandbox provider is shutting down")
            self._reconcile_uncertain_locked()
            self._cleanup_idle_locked()
            record = self._live_record_locked(sandbox_id)
            if record is not None:
                return sandbox_id
            self._evict_if_needed()
            self._start_container(
                safe_thread_id,
                sandbox_id,
                skills_path=str(projection.path),
                skills_revision=projection.revision,
            )
            return sandbox_id

    def get(self, sandbox_id: str) -> Sandbox | None:
        with self._lock:
            if self._closing:
                return None
            record = self._live_record_locked(sandbox_id)
            if record is None:
                return None
            return record.sandbox

    def release(self, sandbox_id: str) -> None:
        with self._lock:
            record = self._records.get(sandbox_id)
            if record is not None:
                record.last_used = time.monotonic()
                self._records.move_to_end(sandbox_id)

    def release_thread(self, thread_id: str) -> None:
        """Remove idle revisions, retaining busy or failed resources for retry."""
        safe_thread_id = self._safe_thread_id(thread_id)
        with self._lock:
            records = [
                (sandbox_id, record)
                for sandbox_id, record in list(self._records.items())
                if record.thread_id == safe_thread_id
            ]
            failures = []
            for sandbox_id, record in records:
                try:
                    self._remove_record_locked(sandbox_id, record)
                except Exception as exc:
                    failures.append(str(exc))
            if failures:
                raise RuntimeError("Failed to release sandbox resources: " + "; ".join(failures))

    def active_skill_revisions(self) -> set[str]:
        with self._lock:
            return {record.skills_revision for record in self._records.values()}

    def shutdown(self) -> None:
        with self._lock:
            self._closing = True
            failures = []
            for sandbox_id, record in list(self._records.items()):
                try:
                    self._remove_record_locked(sandbox_id, record)
                except Exception as exc:
                    failures.append(str(exc))
            if failures:
                raise RuntimeError("Failed to shut down sandbox resources: " + "; ".join(failures))


def _as_str_list(values: list[object]) -> list[str]:
    return [str(value) for value in values]


def _host_uid_gid() -> str | None:
    getuid = getattr(os, "getuid", None)
    getgid = getattr(os, "getgid", None)
    if getuid is None or getgid is None:
        return None
    return f"{getuid()}:{getgid()}"
