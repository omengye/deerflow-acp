import posixpath
import re
import shlex
from contextlib import ExitStack
from pathlib import Path

from langchain.tools import ToolRuntime, tool

from deerflow.agents.thread_state import AgentContext, ThreadDataState, ThreadState
from deerflow.config import get_app_config
from deerflow.config.paths import VIRTUAL_PATH_PREFIX
from deerflow.sandbox.exceptions import (
    SandboxError,
    SandboxNotFoundError,
    SandboxRuntimeError,
)
from deerflow.sandbox.file_operation_lock import get_file_operation_lock
from deerflow.sandbox.sandbox import Sandbox
from deerflow.sandbox.sandbox_provider import (
    get_sandbox_provider,
    workspace_mount_path_from_thread_data,
)
from deerflow.sandbox.search import GrepMatch
from deerflow.sandbox.security import (
    LOCAL_HOST_BASH_DISABLED_MESSAGE,
    is_host_bash_allowed,
)

_ABSOLUTE_PATH_PATTERN = re.compile(r"(?<![:\w])(?<!:/)/(?:[^\s\"'`;&|<>()]+)")
_FILE_URL_PATTERN = re.compile(r"\bfile://\S+", re.IGNORECASE)
_URL_WITH_SCHEME_PATTERN = re.compile(r"^[a-z][a-z0-9+.-]*://", re.IGNORECASE)
_URL_IN_COMMAND_PATTERN = re.compile(r"\b[a-z][a-z0-9+.-]*://[^\s\"'`;&|<>()]+", re.IGNORECASE)
_DOTDOT_PATH_SEGMENT_PATTERN = re.compile(r"(?:^|[/\\=])\.\.(?:$|[/\\])")
_LOCAL_BASH_SYSTEM_PATH_PREFIXES = (
    "/bin/",
    "/usr/bin/",
    "/usr/sbin/",
    "/sbin/",
    "/opt/homebrew/bin/",
    "/dev/",
)

_DEFAULT_SKILLS_CONTAINER_PATH = "/mnt/skills"
_ACP_WORKSPACE_VIRTUAL_PATH = "/mnt/acp-workspace"
_DEFAULT_GLOB_MAX_RESULTS = 200
_MAX_GLOB_MAX_RESULTS = 1000
_DEFAULT_GREP_MAX_RESULTS = 100
_MAX_GREP_MAX_RESULTS = 500
_LOCAL_BASH_CWD_COMMANDS = {"cd", "pushd"}
_LOCAL_BASH_COMMAND_WRAPPERS = {"command", "builtin"}
_LOCAL_BASH_COMMAND_PREFIX_KEYWORDS = {"!", "{", "case", "do", "elif", "else", "for", "if", "select", "then", "time", "until", "while"}
_LOCAL_BASH_COMMAND_END_KEYWORDS = {"}", "done", "esac", "fi"}
_LOCAL_BASH_ROOT_PATH_COMMANDS = {
    "awk",
    "cat",
    "cp",
    "du",
    "find",
    "grep",
    "head",
    "less",
    "ln",
    "ls",
    "more",
    "mv",
    "rm",
    "sed",
    "tail",
    "tar",
}
_SHELL_COMMAND_SEPARATORS = {";", "&&", "||", "|", "|&", "&", "(", ")"}
_SHELL_REDIRECTION_OPERATORS = {
    "<",
    ">",
    "<<",
    ">>",
    "<<<",
    "<>",
    ">&",
    "<&",
    "&>",
    "&>>",
    ">|",
}


def _get_skills_container_path() -> str:
    """Get the skills container path from config, with fallback to default.

    Result is cached after the first successful config load.  If config loading
    fails the default is returned *without* caching so that a later call can
    pick up the real value once the config is available.
    """
    cached = getattr(_get_skills_container_path, "_cached", None)
    if cached is not None:
        return cached
    try:
        from deerflow.config import get_app_config

        value = get_app_config().skills.container_path
        _get_skills_container_path._cached = value  # type: ignore[attr-defined]
        return value
    except Exception:
        return _DEFAULT_SKILLS_CONTAINER_PATH


def _get_skills_host_path(
    runtime: ToolRuntime[AgentContext, ThreadState] | None = None,
) -> str | None:
    """Get the run's enabled/allowlisted Skill projection host path."""
    if runtime is not None and runtime.state is not None:
        sandbox_state = runtime.state.get("sandbox")
        if sandbox_state is not None:
            value = sandbox_state.get("skills_path")
            if value and Path(value).is_dir():
                return str(value)
    try:
        from deerflow.skills.projection import get_skill_projection

        return str(get_skill_projection().path)
    except Exception:
        pass
    return None


def _is_skills_path(path: str) -> bool:
    """Check if a path is under the skills container path."""
    skills_prefix = _get_skills_container_path()
    return path == skills_prefix or path.startswith(f"{skills_prefix}/")


def _resolve_skills_path(
    path: str,
    runtime: ToolRuntime[AgentContext, ThreadState] | None = None,
) -> str:
    """Resolve a virtual skills path to a host filesystem path.

    Args:
        path: Virtual skills path (e.g. /mnt/skills/public/bootstrap/SKILL.md)

    Returns:
        Resolved host path.

    Raises:
        FileNotFoundError: If skills directory is not configured or doesn't exist.
    """
    skills_container = _get_skills_container_path()
    skills_host = _get_skills_host_path(runtime)
    if skills_host is None:
        raise FileNotFoundError(f"Skills directory not available for path: {path}")

    if path == skills_container:
        return skills_host

    relative = path[len(skills_container) :].lstrip("/")
    return _join_path_preserving_style(skills_host, relative)


def _is_acp_workspace_path(path: str) -> bool:
    """Check if a path is under the ACP workspace virtual path."""
    return path == _ACP_WORKSPACE_VIRTUAL_PATH or path.startswith(f"{_ACP_WORKSPACE_VIRTUAL_PATH}/")


def _get_custom_mounts():
    """Get custom volume mounts from sandbox config.

    Result is cached after the first successful config load.  If config loading
    fails an empty list is returned *without* caching so that a later call can
    pick up the real value once the config is available.
    """
    cached = getattr(_get_custom_mounts, "_cached", None)
    if cached is not None:
        return cached
    try:
        from pathlib import Path

        from deerflow.config import get_app_config

        config = get_app_config()
        mounts = []
        if config.sandbox and config.sandbox.mounts:
            # Only include mounts whose host_path exists, consistent with
            # LocalSandboxProvider._setup_path_mappings() which also filters
            # by host_path.exists().
            mounts = [m for m in config.sandbox.mounts if Path(m.host_path).exists()]
        _get_custom_mounts._cached = mounts  # type: ignore[attr-defined]
        return mounts
    except Exception:
        # If config loading fails, return an empty list without caching so that
        # a later call can retry once the config is available.
        return []


def _is_custom_mount_path(path: str) -> bool:
    """Check if path is under a custom mount container_path."""
    for mount in _get_custom_mounts():
        if path == mount.container_path or path.startswith(f"{mount.container_path}/"):
            return True
    return False


def _get_custom_mount_for_path(path: str):
    """Get the mount config matching this path (longest prefix first)."""
    best = None
    for mount in _get_custom_mounts():
        if path == mount.container_path or path.startswith(f"{mount.container_path}/"):
            if best is None or len(mount.container_path) > len(best.container_path):
                best = mount
    return best


def _extract_thread_id_from_thread_data(thread_data: "ThreadDataState | None") -> str | None:
    """Extract thread_id from explicit state or the legacy workspace layout.

    The workspace_path has the form
    ``{base_dir}/threads/{thread_id}/user-data/workspace``, so
    ``Path(workspace_path).parent.parent.name`` yields the thread_id.
    """
    if thread_data is None:
        return None
    explicit_thread_id = thread_data.get("thread_id")
    if explicit_thread_id:
        return str(explicit_thread_id)
    workspace_path = thread_data.get("workspace_path")
    if not workspace_path:
        return None
    try:
        # {base_dir}/threads/{thread_id}/user-data/workspace → parent.parent = threads/{thread_id}
        return Path(workspace_path).parent.parent.name
    except Exception:
        return None


def _get_acp_workspace_host_path(thread_id: str | None = None) -> str | None:
    """Get the ACP workspace host filesystem path.

    When *thread_id* is provided, returns the per-thread workspace
    ``{base_dir}/threads/{thread_id}/acp-workspace/`` (not cached — the
    directory is created on demand by ``invoke_acp_agent_tool``).

    Falls back to the global ``{base_dir}/acp-workspace/`` when *thread_id*
    is ``None``; that result is cached after the first successful resolution.
    Returns ``None`` if the directory does not exist.
    """
    if thread_id is not None:
        try:
            from deerflow.config.paths import get_paths

            host_path = get_paths().acp_workspace_dir(thread_id)
            if host_path.exists():
                return str(host_path)
        except Exception:
            pass
        return None

    cached = getattr(_get_acp_workspace_host_path, "_cached", None)
    if cached is not None:
        return cached
    try:
        from deerflow.config.paths import get_paths

        host_path = get_paths().base_dir / "acp-workspace"
        if host_path.exists():
            value = str(host_path)
            _get_acp_workspace_host_path._cached = value  # type: ignore[attr-defined]
            return value
    except Exception:
        pass
    return None


def _resolve_acp_workspace_path(path: str, thread_id: str | None = None) -> str:
    """Resolve a virtual ACP workspace path to a host filesystem path.

    Args:
        path: Virtual path (e.g. /mnt/acp-workspace/hello_world.py)
        thread_id: Current thread ID for per-thread workspace resolution.
                   When ``None``, falls back to the global workspace.

    Returns:
        Resolved host path.

    Raises:
        FileNotFoundError: If ACP workspace directory does not exist.
        PermissionError: If path traversal is detected.
    """
    _reject_path_traversal(path)

    host_path = _get_acp_workspace_host_path(thread_id)
    if host_path is None:
        raise FileNotFoundError(f"ACP workspace directory not available for path: {path}")

    if path == _ACP_WORKSPACE_VIRTUAL_PATH:
        return host_path

    relative = path[len(_ACP_WORKSPACE_VIRTUAL_PATH) :].lstrip("/")
    resolved = _join_path_preserving_style(host_path, relative)

    if "/" in host_path and "\\" not in host_path:
        base_path = posixpath.normpath(host_path)
        candidate_path = posixpath.normpath(resolved)
        try:
            if posixpath.commonpath([base_path, candidate_path]) != base_path:
                raise PermissionError("Access denied: path traversal detected")
        except ValueError:
            raise PermissionError("Access denied: path traversal detected") from None
        return resolved

    resolved_path = Path(resolved).resolve()
    try:
        resolved_path.relative_to(Path(host_path).resolve())
    except ValueError:
        raise PermissionError("Access denied: path traversal detected")

    return str(resolved_path)


def _get_mcp_allowed_paths() -> list[str]:
    """Get the list of allowed paths from MCP config for file system server."""
    allowed_paths = []
    try:
        from deerflow.config.extensions_config import get_extensions_config

        extensions_config = get_extensions_config()

        for _, server in extensions_config.mcp_servers.items():
            if not server.enabled:
                continue

            # Only check the filesystem server
            args = server.args or []
            # Check if args has server-filesystem package
            has_filesystem = any("server-filesystem" in arg for arg in args)
            if not has_filesystem:
                continue
            # Unpack the allowed file system paths in config
            for arg in args:
                if not arg.startswith("-") and arg.startswith("/"):
                    allowed_paths.append(arg.rstrip("/") + "/")

    except Exception:
        pass

    return allowed_paths


def _get_tool_config_int(name: str, key: str, default: int) -> int:
    try:
        tool_config = get_app_config().get_tool_config(name)
        if tool_config is not None and key in tool_config.model_extra:
            value = tool_config.model_extra.get(key)
            if isinstance(value, int):
                return value
    except Exception:
        pass
    return default


def _clamp_max_results(value: int, *, default: int, upper_bound: int) -> int:
    if value <= 0:
        return default
    return min(value, upper_bound)


def _resolve_max_results(name: str, requested: int, *, default: int, upper_bound: int) -> int:
    requested_max_results = _clamp_max_results(requested, default=default, upper_bound=upper_bound)
    configured_max_results = _clamp_max_results(
        _get_tool_config_int(name, "max_results", default),
        default=default,
        upper_bound=upper_bound,
    )
    return min(requested_max_results, configured_max_results)


def _resolve_local_read_path(
    path: str,
    thread_data: ThreadDataState,
) -> str:
    validate_local_tool_path(path, thread_data, read_only=True)
    if _is_skills_path(path) or _is_acp_workspace_path(path) or _is_custom_mount_path(path):
        # Mounted paths belong to the sandbox provider. Its acquire-time
        # PathMapping captures the authoritative projection and identity;
        # reconstructing a host path here can select a different mapping.
        return path
    return _resolve_and_validate_user_data_path(path, thread_data)


def _format_glob_results(root_path: str, matches: list[str], truncated: bool) -> str:
    if not matches:
        return f"No files matched under {root_path}"

    lines = [f"Found {len(matches)} paths under {root_path}"]
    if truncated:
        lines[0] += f" (showing first {len(matches)})"
    lines.extend(f"{index}. {path}" for index, path in enumerate(matches, start=1))
    if truncated:
        lines.append("Results truncated. Narrow the path or pattern to see fewer matches.")
    return "\n".join(lines)


def _format_grep_results(root_path: str, matches: list[GrepMatch], truncated: bool) -> str:
    if not matches:
        return f"No matches found under {root_path}"

    lines = [f"Found {len(matches)} matches under {root_path}"]
    if truncated:
        lines[0] += f" (showing first {len(matches)})"
    lines.extend(f"{match.path}:{match.line_number}: {match.line}" for match in matches)
    if truncated:
        lines.append("Results truncated. Narrow the path or add a glob filter.")
    return "\n".join(lines)


def _path_variants(path: str) -> set[str]:
    return {path, path.replace("\\", "/"), path.replace("/", "\\")}


def _path_separator_for_style(path: str) -> str:
    return "\\" if "\\" in path and "/" not in path else "/"


def _join_path_preserving_style(base: str, relative: str) -> str:
    if not relative:
        return base
    separator = _path_separator_for_style(base)
    normalized_relative = relative.replace("\\" if separator == "/" else "/", separator).lstrip("/\\")
    stripped_base = base.rstrip("/\\")
    return f"{stripped_base}{separator}{normalized_relative}"


def _sanitize_error(error: Exception, runtime: "ToolRuntime[AgentContext, ThreadState] | None" = None) -> str:
    """Sanitize an error message to avoid leaking host filesystem paths.

    In local-sandbox mode, resolved host paths in the error string are masked
    back to their virtual equivalents so that user-visible output never exposes
    the host directory layout.
    """
    msg = f"{type(error).__name__}: {error}"
    if runtime is not None and is_local_sandbox(runtime):
        thread_data = get_thread_data(runtime)
        msg = mask_local_paths_in_output(msg, thread_data, runtime)
    return msg


def replace_virtual_path(path: str, thread_data: ThreadDataState | None) -> str:
    """Replace virtual /mnt/user-data paths with actual thread data paths.

    Mapping:
        /mnt/user-data/workspace/* -> thread_data['workspace_path']/*
        /mnt/user-data/uploads/* -> thread_data['uploads_path']/*
        /mnt/user-data/outputs/* -> thread_data['outputs_path']/*

    Args:
        path: The path that may contain virtual path prefix.
        thread_data: The thread data containing actual paths.

    Returns:
        The path with virtual prefix replaced by actual path.
    """
    if thread_data is None:
        return path

    mappings = _thread_virtual_to_actual_mappings(thread_data)
    if not mappings:
        return path

    # Longest-prefix-first replacement with segment-boundary checks.
    for virtual_base, actual_base in sorted(mappings.items(), key=lambda item: len(item[0]), reverse=True):
        if path == virtual_base:
            return actual_base
        if path.startswith(f"{virtual_base}/"):
            rest = path[len(virtual_base) :].lstrip("/")
            result = _join_path_preserving_style(actual_base, rest)
            if path.endswith("/") and not result.endswith(("/", "\\")):
                result += _path_separator_for_style(actual_base)
            return result

    return path


def _thread_virtual_to_actual_mappings(thread_data: ThreadDataState) -> dict[str, str]:
    """Build virtual-to-actual path mappings for a thread."""
    mappings: dict[str, str] = {}

    workspace = thread_data.get("workspace_path")
    uploads = thread_data.get("uploads_path")
    outputs = thread_data.get("outputs_path")

    if workspace:
        mappings[f"{VIRTUAL_PATH_PREFIX}/workspace"] = workspace
    if uploads:
        mappings[f"{VIRTUAL_PATH_PREFIX}/uploads"] = uploads
    if outputs:
        mappings[f"{VIRTUAL_PATH_PREFIX}/outputs"] = outputs

    # Also map the virtual root when all known dirs share the same parent.
    actual_dirs = [Path(p) for p in (workspace, uploads, outputs) if p]
    if actual_dirs:
        common_parent = str(Path(actual_dirs[0]).parent)
        if all(str(path.parent) == common_parent for path in actual_dirs):
            mappings[VIRTUAL_PATH_PREFIX] = common_parent

    return mappings


def _thread_actual_to_virtual_mappings(thread_data: ThreadDataState) -> dict[str, str]:
    """Build actual-to-virtual mappings for output masking."""
    return {actual: virtual for virtual, actual in _thread_virtual_to_actual_mappings(thread_data).items()}


def mask_local_paths_in_output(
    output: str,
    thread_data: ThreadDataState | None,
    runtime: ToolRuntime[AgentContext, ThreadState] | None = None,
) -> str:
    """Mask host absolute paths from local sandbox output using virtual paths.

    Handles user-data paths (per-thread), skills paths, and ACP workspace paths (global).
    """
    result = output

    # Mask skills host paths
    skills_host = _get_skills_host_path(runtime)
    skills_container = _get_skills_container_path()
    if skills_host:
        raw_base = str(Path(skills_host))
        resolved_base = str(Path(skills_host).resolve())
        for base in _path_variants(raw_base) | _path_variants(resolved_base):
            escaped = re.escape(base).replace(r"\\", r"[/\\]")
            pattern = re.compile(escaped + r"(?:[/\\][^\s\"';&|<>():]*)?")

            def replace_skills(match: re.Match, _base: str = base) -> str:
                matched_path = match.group(0)
                if matched_path == _base:
                    return skills_container
                relative = matched_path[len(_base) :].lstrip("/\\").replace("\\", "/")
                return f"{skills_container}/{relative}" if relative else skills_container

            result = pattern.sub(replace_skills, result)

    # Mask ACP workspace host paths
    _thread_id = _extract_thread_id_from_thread_data(thread_data)
    acp_host = _get_acp_workspace_host_path(_thread_id)
    if acp_host:
        raw_base = str(Path(acp_host))
        resolved_base = str(Path(acp_host).resolve())
        for base in _path_variants(raw_base) | _path_variants(resolved_base):
            escaped = re.escape(base).replace(r"\\", r"[/\\]")
            pattern = re.compile(escaped + r"(?:[/\\][^\s\"';&|<>():]*)?")

            def replace_acp(match: re.Match, _base: str = base) -> str:
                matched_path = match.group(0)
                if matched_path == _base:
                    return _ACP_WORKSPACE_VIRTUAL_PATH
                relative = matched_path[len(_base) :].lstrip("/\\").replace("\\", "/")
                return f"{_ACP_WORKSPACE_VIRTUAL_PATH}/{relative}" if relative else _ACP_WORKSPACE_VIRTUAL_PATH

            result = pattern.sub(replace_acp, result)

    # Custom mount host paths are masked by LocalSandbox._reverse_resolve_paths_in_output()

    # Mask user-data host paths
    if thread_data is None:
        return result

    mappings = _thread_actual_to_virtual_mappings(thread_data)
    if not mappings:
        return result

    for actual_base, virtual_base in sorted(mappings.items(), key=lambda item: len(item[0]), reverse=True):
        raw_base = str(Path(actual_base))
        resolved_base = str(Path(actual_base).resolve())
        for base in _path_variants(raw_base) | _path_variants(resolved_base):
            escaped_actual = re.escape(base).replace(r"\\", r"[/\\]")
            pattern = re.compile(escaped_actual + r"(?:[/\\][^\s\"';&|<>():]*)?")

            def replace_match(match: re.Match, _base: str = base, _virtual: str = virtual_base) -> str:
                matched_path = match.group(0)
                if matched_path == _base:
                    return _virtual
                relative = matched_path[len(_base) :].lstrip("/\\").replace("\\", "/")
                return f"{_virtual}/{relative}" if relative else _virtual

            result = pattern.sub(replace_match, result)

    return result


def _reject_path_traversal(path: str) -> None:
    """Reject paths that contain '..' segments to prevent directory traversal."""
    # Normalise to forward slashes, then check for '..' segments.
    normalised = path.replace("\\", "/")
    for segment in normalised.split("/"):
        if segment == "..":
            raise PermissionError("Access denied: path traversal detected")


def validate_local_tool_path(path: str, thread_data: ThreadDataState | None, *, read_only: bool = False) -> None:
    """Validate that a virtual path is allowed for local-sandbox access.

    This function is a security gate — it checks whether *path* may be
    accessed and raises on violation. It does **not** resolve the virtual
    path to a host path. Sandbox-backed readers keep mounted paths virtual
    and let the provider's acquire-time mount table resolve them.

    Allowed virtual-path families:
      - ``/mnt/user-data/*``  — always allowed (read + write)
      - ``/mnt/skills/*``     — allowed only when *read_only* is True
      - ``/mnt/acp-workspace/*`` — allowed only when *read_only* is True
      - Custom mount paths (from config.yaml) — respects per-mount ``read_only`` flag

    Args:
        path: The virtual path to validate.
        thread_data: Thread data (must be present for local sandbox).
        read_only: When True, skills and ACP workspace paths are permitted.

    Raises:
        SandboxRuntimeError: If thread data is missing.
        PermissionError: If the path is not allowed or contains traversal.
    """
    if thread_data is None:
        raise SandboxRuntimeError("Thread data not available for local sandbox")

    _reject_path_traversal(path)

    # Skills paths — read-only access only
    if _is_skills_path(path):
        if not read_only:
            raise PermissionError(f"Write access to skills path is not allowed: {path}")
        return

    # ACP workspace paths — read-only access only
    if _is_acp_workspace_path(path):
        if not read_only:
            raise PermissionError(f"Write access to ACP workspace is not allowed: {path}")
        return

    # User-data paths
    if path.startswith(f"{VIRTUAL_PATH_PREFIX}/"):
        return

    # Custom mount paths — respect read_only config
    if _is_custom_mount_path(path):
        mount = _get_custom_mount_for_path(path)
        if mount and mount.read_only and not read_only:
            raise PermissionError(f"Write access to read-only mount is not allowed: {path}")
        return

    raise PermissionError(f"Only paths under {VIRTUAL_PATH_PREFIX}/, {_get_skills_container_path()}/, {_ACP_WORKSPACE_VIRTUAL_PATH}/, or configured mount paths are allowed")


def _validate_resolved_user_data_path(resolved: Path, thread_data: ThreadDataState) -> None:
    """Verify that a resolved host path stays inside allowed per-thread roots.

    Raises PermissionError if the path escapes workspace/uploads/outputs.
    """
    configured_roots = [
        (key, Path(value))
        for key in ("workspace_path", "uploads_path", "outputs_path")
        if (value := thread_data.get(key)) is not None
    ]

    if not configured_roots:
        raise SandboxRuntimeError("No allowed local sandbox directories configured")

    for key, configured_root in configured_roots:
        root = configured_root.resolve()
        try:
            resolved.relative_to(root)
            if (
                key == "workspace_path"
                and not thread_data.get("workspace_path_managed", True)
                and not configured_root.is_dir()
            ):
                raise FileNotFoundError(
                    f"ACP workspace directory is unavailable: {configured_root}"
                )
            return
        except ValueError:
            continue

    raise PermissionError("Access denied: path traversal detected")


def _resolve_and_validate_user_data_path(path: str, thread_data: ThreadDataState) -> str:
    """Resolve a /mnt/user-data virtual path and validate it stays in bounds.

    Returns the resolved host path string.
    """
    resolved_str = replace_virtual_path(path, thread_data)
    resolved = Path(resolved_str).resolve()
    _validate_resolved_user_data_path(resolved, thread_data)
    return str(resolved)


def _validate_destructive_virtual_path(path: str) -> None:
    """Require destructive operations to target a child of an allowed root."""

    _reject_path_traversal(path)
    normalized = path.replace("\\", "/").rstrip("/")
    user_roots = (
        f"{VIRTUAL_PATH_PREFIX}/workspace",
        f"{VIRTUAL_PATH_PREFIX}/uploads",
        f"{VIRTUAL_PATH_PREFIX}/outputs",
    )
    if any(normalized.startswith(root + "/") for root in user_roots):
        return
    for mount in _get_custom_mounts():
        root = mount.container_path.replace("\\", "/").rstrip("/")
        if normalized.startswith(root + "/"):
            return
    raise PermissionError(
        "Delete and move operations must target a child path under "
        f"{VIRTUAL_PATH_PREFIX}/workspace, uploads, outputs, or a configured mount"
    )


def _resolve_and_validate_mutation_path(
    path: str,
    thread_data: ThreadDataState,
) -> str:
    """Resolve a mutable user-data path while preserving its final symlink.

    Read/write resolution follows final symlinks and then checks the target.
    Delete/move must instead operate on the symlink entry itself, while still
    resolving and validating every parent component.
    """

    candidate = Path(replace_virtual_path(path, thread_data))
    resolved_parent = candidate.parent.resolve()
    _validate_resolved_user_data_path(resolved_parent, thread_data)
    preserved = resolved_parent / candidate.name

    if candidate.is_symlink():
        return str(preserved)
    if candidate.exists():
        resolved = candidate.resolve()
        _validate_resolved_user_data_path(resolved, thread_data)
        return str(resolved)
    return str(preserved)


def _resolve_local_mutation_path(
    path: str,
    thread_data: ThreadDataState,
) -> str:
    _validate_destructive_virtual_path(path)
    validate_local_tool_path(path, thread_data)
    if _is_custom_mount_path(path):
        return path
    return _resolve_and_validate_mutation_path(path, thread_data)


def _is_non_file_url_token(token: str) -> bool:
    """Return True for URL tokens that should not be interpreted as paths."""
    values = [token]
    if "=" in token:
        values.append(token.split("=", 1)[1])

    for value in values:
        match = _URL_WITH_SCHEME_PATTERN.match(value)
        if match and not value.lower().startswith("file://"):
            return True
    return False


def _non_file_url_spans(command: str) -> list[tuple[int, int]]:
    spans = []
    for match in _URL_IN_COMMAND_PATTERN.finditer(command):
        if not match.group().lower().startswith("file://"):
            spans.append(match.span())
    return spans


def _is_in_spans(position: int, spans: list[tuple[int, int]]) -> bool:
    return any(start <= position < end for start, end in spans)


def _has_dotdot_path_segment(token: str) -> bool:
    if _is_non_file_url_token(token):
        return False
    return bool(_DOTDOT_PATH_SEGMENT_PATTERN.search(token))


def _split_shell_tokens(command: str) -> list[str]:
    try:
        normalized = command.replace("\r\n", "\n").replace("\r", "\n").replace("\n", " ; ")
        lexer = shlex.shlex(normalized, posix=True, punctuation_chars=True)
        lexer.whitespace_split = True
        lexer.commenters = ""
        return list(lexer)
    except ValueError:
        # The shell will reject malformed quoting later; keep validation
        # best-effort instead of turning syntax errors into security messages.
        return command.split()


def _is_shell_command_separator(token: str) -> bool:
    return token in _SHELL_COMMAND_SEPARATORS


def _is_shell_redirection_operator(token: str) -> bool:
    return token in _SHELL_REDIRECTION_OPERATORS


def _is_shell_assignment(token: str) -> bool:
    name, separator, _ = token.partition("=")
    if not separator or not name:
        return False
    return bool(re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", name))


def _is_allowed_local_bash_absolute_path(path: str, allowed_paths: list[str], *, allow_system_paths: bool) -> bool:
    # Check for MCP filesystem server allowed paths
    if any(path.startswith(allowed_path) or path == allowed_path.rstrip("/") for allowed_path in allowed_paths):
        _reject_path_traversal(path)
        return True

    if path == VIRTUAL_PATH_PREFIX or path.startswith(f"{VIRTUAL_PATH_PREFIX}/"):
        _reject_path_traversal(path)
        return True

    # Allow skills container path (resolved by tools.py before passing to sandbox)
    if _is_skills_path(path):
        _reject_path_traversal(path)
        return True

    # Allow ACP workspace path (path-traversal check only)
    if _is_acp_workspace_path(path):
        _reject_path_traversal(path)
        return True

    # Allow custom mount container paths
    if _is_custom_mount_path(path):
        _reject_path_traversal(path)
        return True

    if allow_system_paths and any(path == prefix.rstrip("/") or path.startswith(prefix) for prefix in _LOCAL_BASH_SYSTEM_PATH_PREFIXES):
        return True

    return False


def _next_cd_target(tokens: list[str], start_index: int) -> tuple[str | None, int]:
    index = start_index
    while index < len(tokens):
        token = tokens[index]
        if _is_shell_command_separator(token):
            return None, index
        if _is_shell_redirection_operator(token):
            index += 2
            continue
        if token == "--":
            index += 1
            continue
        if token in {"-L", "-P", "-e", "-@"}:
            index += 1
            continue
        if token.startswith("-") and token != "-":
            index += 1
            continue
        return token, index + 1
    return None, index


def _validate_local_bash_cwd_target(command_name: str, target: str | None, allowed_paths: list[str]) -> None:
    if target is None or target == "-":
        raise PermissionError(f"Unsafe working directory change in command: {command_name}. Use paths under {VIRTUAL_PATH_PREFIX}")
    if target.startswith(("$", "`")):
        raise PermissionError(f"Unsafe working directory change in command: {command_name} {target}. Use paths under {VIRTUAL_PATH_PREFIX}")
    if target.startswith("~"):
        raise PermissionError(f"Unsafe working directory change in command: {command_name} {target}. Use paths under {VIRTUAL_PATH_PREFIX}")
    if target.startswith("/"):
        _reject_path_traversal(target)
        if not _is_allowed_local_bash_absolute_path(target, allowed_paths, allow_system_paths=False):
            raise PermissionError(f"Unsafe working directory change in command: {command_name} {target}. Use paths under {VIRTUAL_PATH_PREFIX}")


def _looks_like_unsafe_cwd_target(target: str | None) -> bool:
    if target is None:
        return False
    return target == "-" or target.startswith(("$", "`", "~", "/", "..")) or _has_dotdot_path_segment(target)


def _validate_local_bash_root_path_args(command_name: str, tokens: list[str], start_index: int) -> None:
    if command_name not in _LOCAL_BASH_ROOT_PATH_COMMANDS:
        return

    index = start_index
    while index < len(tokens):
        token = tokens[index]
        if _is_shell_command_separator(token):
            return
        if _is_shell_redirection_operator(token):
            index += 2
            continue
        if token == "/" and not _is_non_file_url_token(token):
            raise PermissionError(f"Unsafe absolute paths in command: /. Use paths under {VIRTUAL_PATH_PREFIX}")
        index += 1


def _validate_local_bash_shell_tokens(command: str, allowed_paths: list[str]) -> None:
    """Conservatively reject relative path escapes missed by absolute-path scanning."""
    if re.search(r"\$\([^)]*\b(?:cd|pushd)\b", command):
        raise PermissionError(f"Unsafe working directory change in command substitution. Use paths under {VIRTUAL_PATH_PREFIX}")

    tokens = _split_shell_tokens(command)

    for token in tokens:
        if _is_shell_command_separator(token) or _is_shell_redirection_operator(token):
            continue
        if _has_dotdot_path_segment(token):
            raise PermissionError("Access denied: path traversal detected")

    at_command_start = True
    index = 0
    while index < len(tokens):
        token = tokens[index]

        if _is_shell_command_separator(token):
            at_command_start = True
            index += 1
            continue

        if _is_shell_redirection_operator(token):
            index += 1
            continue

        if at_command_start and _is_shell_assignment(token):
            index += 1
            continue

        command_name = token.rsplit("/", 1)[-1]
        if at_command_start and command_name in _LOCAL_BASH_COMMAND_PREFIX_KEYWORDS | _LOCAL_BASH_COMMAND_END_KEYWORDS:
            index += 1
            continue

        if not at_command_start:
            index += 1
            continue

        at_command_start = False
        if command_name in _LOCAL_BASH_COMMAND_WRAPPERS and index + 1 < len(tokens):
            wrapped_name = tokens[index + 1].rsplit("/", 1)[-1]
            if wrapped_name in _LOCAL_BASH_CWD_COMMANDS:
                target, next_index = _next_cd_target(tokens, index + 2)
                _validate_local_bash_cwd_target(wrapped_name, target, allowed_paths)
                index = next_index
                continue
            _validate_local_bash_root_path_args(wrapped_name, tokens, index + 2)

        if command_name not in _LOCAL_BASH_CWD_COMMANDS:
            _validate_local_bash_root_path_args(command_name, tokens, index + 1)
            index += 1
            continue

        target, next_index = _next_cd_target(tokens, index + 1)
        _validate_local_bash_cwd_target(command_name, target, allowed_paths)
        index = next_index


def resolve_and_validate_user_data_path(path: str, thread_data: ThreadDataState) -> str:
    """Resolve a /mnt/user-data virtual path and validate it stays in bounds."""
    return _resolve_and_validate_user_data_path(path, thread_data)


def validate_local_bash_command_paths(command: str, thread_data: ThreadDataState | None) -> None:
    """Validate absolute paths in local-sandbox bash commands.

    This validation is only a best-effort guard for the explicit
    ``sandbox.allow_host_bash: true`` opt-in. It is not a secure sandbox
    boundary and must not be treated as isolation from the host filesystem.

    In local mode, commands must use virtual paths under /mnt/user-data for
    user data access. ACP workspace paths and custom mount container paths
    (configured in config.yaml) are allowed with path-traversal checks only.
    Skills are intentionally excluded because host bash cannot enforce an
    immutable content-addressed projection.
    A small allowlist of common system path prefixes is kept for executable
    and device references (e.g. /bin/sh, /dev/null).
    """
    if thread_data is None:
        raise SandboxRuntimeError("Thread data not available for local sandbox")

    # The same OS user that launches host bash can undo projection chmod/ACL
    # protections, corrupting a content-addressed directory shared by later
    # sandboxes. Keep Skill access on the read-only file/search tools. Docker
    # sandboxes do not take this branch and retain their real ``:ro`` mount.
    skills_container = _get_skills_container_path().rstrip("/") or "/"
    skills_reference = re.compile(
        rf"(?<![A-Za-z0-9_]){re.escape(skills_container)}(?=$|[/\s\"';&|<>()])"
    )
    if skills_reference.search(command):
        raise PermissionError(
            "Host bash cannot access /mnt/skills; use read_file, ls, glob, or grep"
        )

    # Block file:// URLs which bypass the absolute-path regex but allow local file exfiltration
    file_url_match = _FILE_URL_PATTERN.search(command)
    if file_url_match:
        raise PermissionError(f"Unsafe file:// URL in command: {file_url_match.group()}. Use paths under {VIRTUAL_PATH_PREFIX}")

    unsafe_paths: list[str] = []
    allowed_paths = _get_mcp_allowed_paths()
    _validate_local_bash_shell_tokens(command, allowed_paths)
    url_spans = _non_file_url_spans(command)

    for match in _ABSOLUTE_PATH_PATTERN.finditer(command):
        if _is_in_spans(match.start(), url_spans):
            continue
        absolute_path = match.group()
        if _is_allowed_local_bash_absolute_path(absolute_path, allowed_paths, allow_system_paths=True):
            continue

        unsafe_paths.append(absolute_path)

    if unsafe_paths:
        unsafe = ", ".join(sorted(dict.fromkeys(unsafe_paths)))
        raise PermissionError(f"Unsafe absolute paths in command: {unsafe}. Use paths under {VIRTUAL_PATH_PREFIX}")


def replace_virtual_paths_in_command(
    command: str,
    thread_data: ThreadDataState | None,
    runtime: ToolRuntime[AgentContext, ThreadState] | None = None,
) -> str:
    """Replace all virtual paths (/mnt/user-data, /mnt/skills, /mnt/acp-workspace) in a command string.

    Args:
        command: The command string that may contain virtual paths.
        thread_data: The thread data containing actual paths.

    Returns:
        The command with all virtual paths replaced.
    """
    result = command

    # Replace skills paths
    skills_container = _get_skills_container_path()
    skills_host = _get_skills_host_path(runtime)
    if skills_host and skills_container in result:
        skills_pattern = re.compile(rf"{re.escape(skills_container)}(/[^\s\"';&|<>()]*)?")

        def replace_skills_match(match: re.Match) -> str:
            return _resolve_skills_path(match.group(0), runtime)

        result = skills_pattern.sub(replace_skills_match, result)

    # Replace ACP workspace paths
    _thread_id = _extract_thread_id_from_thread_data(thread_data)
    acp_host = _get_acp_workspace_host_path(_thread_id)
    if acp_host and _ACP_WORKSPACE_VIRTUAL_PATH in result:
        acp_pattern = re.compile(rf"{re.escape(_ACP_WORKSPACE_VIRTUAL_PATH)}(/[^\s\"';&|<>()]*)?")

        def replace_acp_match(match: re.Match, _tid: str | None = _thread_id) -> str:
            return _resolve_acp_workspace_path(match.group(0), _tid)

        result = acp_pattern.sub(replace_acp_match, result)

    # Custom mount paths are resolved by LocalSandbox._resolve_paths_in_command()

    # Replace user-data paths
    if VIRTUAL_PATH_PREFIX in result and thread_data is not None:
        pattern = re.compile(rf"{re.escape(VIRTUAL_PATH_PREFIX)}(/[^\s\"';&|<>()]*)?")

        def replace_user_data_match(match: re.Match) -> str:
            return replace_virtual_path(match.group(0), thread_data)

        result = pattern.sub(replace_user_data_match, result)

    return result


def _apply_cwd_prefix(command: str, thread_data: ThreadDataState | None) -> str:
    """Prepend 'cd <workspace> &&' so relative paths are anchored to the thread workspace.

    Args:
        command: The bash command to execute.
        thread_data: The thread data containing the workspace path.

    Returns:
        The command prefixed with 'cd <workspace> &&' if workspace_path is available,
        otherwise the original command unchanged.
    """
    if thread_data and (workspace := thread_data.get("workspace_path")):
        return f"cd {shlex.quote(workspace)} && {command}"
    return command


def get_thread_data(runtime: ToolRuntime[AgentContext, ThreadState] | None) -> ThreadDataState | None:
    """Extract thread_data from runtime state."""
    if runtime is None:
        return None
    if runtime.state is None:
        return None
    return runtime.state.get("thread_data")


_HOST_FS_SANDBOX_IDS: frozenset[str] = frozenset({"local", "wsl"})


def is_host_fs_sandbox(runtime: ToolRuntime[AgentContext, ThreadState] | None) -> bool:
    """Check if the current sandbox stores files on the host filesystem.

    Both LocalSandbox (host-direct) and WslSandbox (Windows host filesystem,
    bash inside WSL VM) need virtual-path resolution, host-path masking, and
    pre-execution path validation. AioSandbox-style providers (Docker, k8s)
    already mount /mnt/user-data natively in the container, so the tool layer
    skips translation for them.
    """
    if runtime is None:
        return False
    if runtime.state is None:
        return False
    sandbox_state = runtime.state.get("sandbox")
    if sandbox_state is None:
        return False
    sandbox_id = sandbox_state.get("sandbox_id")
    if sandbox_id in _HOST_FS_SANDBOX_IDS:
        return True
    return isinstance(sandbox_id, str) and (
        sandbox_id.startswith("local:") or sandbox_id.startswith("wsl:")
    )


# Back-compat alias. The historical name implied "local-only" semantics; the
# helper now also covers WslSandbox, since both share the host-filesystem
# behaviour that gates path translation/validation in the tool layer.
is_local_sandbox = is_host_fs_sandbox


def sandbox_from_runtime(runtime: ToolRuntime[AgentContext, ThreadState] | None = None) -> Sandbox:
    """Extract sandbox instance from tool runtime.

    DEPRECATED: Use ensure_sandbox_initialized() for lazy initialization support.
    This function assumes sandbox is already initialized and will raise error if not.

    Raises:
        SandboxRuntimeError: If runtime is not available or sandbox state is missing.
        SandboxNotFoundError: If sandbox with the given ID cannot be found.
    """
    if runtime is None:
        raise SandboxRuntimeError("Tool runtime not available")
    if runtime.state is None:
        raise SandboxRuntimeError("Tool runtime state not available")
    sandbox_state = runtime.state.get("sandbox")
    if sandbox_state is None:
        raise SandboxRuntimeError("Sandbox state not initialized in runtime")
    sandbox_id = sandbox_state.get("sandbox_id")
    if sandbox_id is None:
        raise SandboxRuntimeError("Sandbox ID not found in state")
    sandbox = get_sandbox_provider().get(sandbox_id)
    if sandbox is None:
        raise SandboxNotFoundError(f"Sandbox with ID '{sandbox_id}' not found", sandbox_id=sandbox_id)

    if runtime.context is not None:
        runtime.context["sandbox_id"] = sandbox_id  # Ensure sandbox_id is in context for downstream use
    return sandbox


def ensure_sandbox_initialized(runtime: ToolRuntime[AgentContext, ThreadState] | None = None) -> Sandbox:
    """Ensure sandbox is initialized, acquiring lazily if needed.

    On first call, acquires a sandbox from the provider and stores it in runtime state.
    Subsequent calls return the existing sandbox.

    Thread-safety is guaranteed by the provider's internal locking mechanism.

    Args:
        runtime: Tool runtime containing state and context.

    Returns:
        Initialized sandbox instance.

    Raises:
        SandboxRuntimeError: If runtime is not available or thread_id is missing.
        SandboxNotFoundError: If sandbox acquisition fails.
    """
    if runtime is None:
        raise SandboxRuntimeError("Tool runtime not available")

    if runtime.state is None:
        raise SandboxRuntimeError("Tool runtime state not available")

    raw_skills = runtime.context.get("available_skills") if runtime.context else None
    if raw_skills is None and runtime.config:
        raw_skills = runtime.config.get("metadata", {}).get("available_skills")
    available_skills = list(raw_skills) if raw_skills is not None else None
    from deerflow.skills.projection import get_skill_projection

    projection = get_skill_projection(available_skills)
    workspace_path = workspace_mount_path_from_thread_data(
        runtime.state.get("thread_data")
    )

    # Check if the persisted sandbox is still bound to the current Skill view
    # and the current external workspace mount.
    sandbox_state = runtime.state.get("sandbox")
    if sandbox_state is not None:
        sandbox_id = sandbox_state.get("sandbox_id")
        if (
            sandbox_id is not None
            and sandbox_state.get("skills_revision") == projection.revision
            and sandbox_state.get("workspace_path") == workspace_path
        ):
            sandbox = get_sandbox_provider().get(sandbox_id)
            if sandbox is not None:
                if runtime.context is not None:
                    runtime.context["sandbox_id"] = sandbox_id  # Ensure sandbox_id is in context for releasing in after_agent
                return sandbox
            # Sandbox was released, fall through to acquire new one

    # Lazy acquisition: get thread_id and acquire sandbox
    thread_id = runtime.context.get("thread_id") if runtime.context else None
    if thread_id is None:
        thread_id = runtime.config.get("configurable", {}).get("thread_id") if runtime.config else None
    if thread_id is None:
        raise SandboxRuntimeError("Thread ID not available in runtime context")

    provider = get_sandbox_provider()
    if workspace_path is None:
        sandbox_id = provider.acquire(
            thread_id,
            available_skills=available_skills,
        )
    else:
        sandbox_id = provider.acquire(
            thread_id,
            available_skills=available_skills,
            workspace_path=workspace_path,
        )

    # Update runtime state - this persists across tool calls
    runtime.state["sandbox"] = {
        "sandbox_id": sandbox_id,
        "skills_revision": projection.revision,
        "skills_path": str(projection.path),
        "available_skills": available_skills,
        "workspace_path": workspace_path,
    }

    # Retrieve and return the sandbox
    sandbox = provider.get(sandbox_id)
    if sandbox is None:
        raise SandboxNotFoundError("Sandbox not found after acquisition", sandbox_id=sandbox_id)

    if runtime.context is not None:
        runtime.context["sandbox_id"] = sandbox_id  # Ensure sandbox_id is in context for releasing in after_agent
    return sandbox


def ensure_thread_directories_exist(runtime: ToolRuntime[AgentContext, ThreadState] | None) -> None:
    """Ensure thread data directories (workspace, uploads, outputs) exist.

    This function is called lazily when any sandbox tool is first used.
    For local sandbox, it creates the directories on the filesystem.
    For other sandboxes (like aio), directories are already mounted in the container.

    Args:
        runtime: Tool runtime containing state and context.
    """
    if runtime is None:
        return

    # Only create directories for local sandbox
    if not is_local_sandbox(runtime):
        return

    thread_data = get_thread_data(runtime)
    if thread_data is None:
        return

    # Check if directories have already been created
    if runtime.state.get("thread_directories_created"):
        return

    # uploads/outputs are DeerFlow-managed. A workspace supplied by ACP is an
    # existing client project and must never be silently recreated if removed.
    import os

    managed_keys = ["uploads_path", "outputs_path"]
    workspace_is_managed = thread_data.get("workspace_path_managed", True)
    if workspace_is_managed:
        managed_keys.insert(0, "workspace_path")
    for key in managed_keys:
        path = thread_data.get(key)
        if path:
            if key == "outputs_path" and not workspace_is_managed:
                workspace_path = thread_data.get("workspace_path")
                if workspace_path and not os.path.isdir(workspace_path):
                    try:
                        Path(path).resolve().relative_to(Path(workspace_path).resolve())
                    except ValueError:
                        pass
                    else:
                        continue
            os.makedirs(path, exist_ok=True)

    # Mark as created to avoid redundant operations
    runtime.state["thread_directories_created"] = True


def _truncate_bash_output(output: str, max_chars: int) -> str:
    """Middle-truncate bash output, preserving head and tail (50/50 split).

    bash output may have errors at either end (stderr/stdout ordering is
    non-deterministic), so both ends are preserved equally.

    The returned string (including the truncation marker) is guaranteed to be
    no longer than max_chars characters. Pass max_chars=0 to disable truncation
    and return the full output unchanged.
    """
    if max_chars == 0:
        return output
    if len(output) <= max_chars:
        return output
    total_len = len(output)
    # Compute the exact worst-case marker length: skipped chars is at most
    # total_len, so this is a tight upper bound.
    marker_max_len = len(f"\n... [middle truncated: {total_len} chars skipped] ...\n")
    kept = max(0, max_chars - marker_max_len)
    if kept == 0:
        return output[:max_chars]
    head_len = kept // 2
    tail_len = kept - head_len
    skipped = total_len - kept
    marker = f"\n... [middle truncated: {skipped} chars skipped] ...\n"
    return f"{output[:head_len]}{marker}{output[-tail_len:] if tail_len > 0 else ''}"


def _truncate_read_file_output(
    output: str,
    max_chars: int,
    line_offset: int = 0,
) -> str:
    """Head-truncate read_file output, preserving the beginning of the file.

    Source code and documents are read top-to-bottom; the head contains the
    most context (imports, class definitions, function signatures).

    The returned string (including the truncation marker) is guaranteed to be
    no longer than max_chars characters. Pass max_chars=0 to disable truncation
    and return the full output unchanged.

    ``line_offset`` is the zero-based absolute line number of ``output``'s
    first line. Ranged reads use it so the continuation hint always speaks in
    the absolute line numbers accepted by ``read_file``.
    """
    if max_chars == 0:
        return output
    if len(output) <= max_chars:
        return output
    total = len(output)
    total_lines = output.count("\n") + (0 if output.endswith("\n") else 1)
    bound = total + max(line_offset, 0)
    marker_max_len = len(
        f"\n... [truncated: showing first {total} of {total} chars "
        f"(cut lands in line {bound} of {bound}). Use start_line={bound} — "
        "optionally with end_line — to continue without a gap] ..."
    )
    kept = max(0, max_chars - marker_max_len)
    if kept == 0:
        return output[:max_chars]
    cut_line = line_offset + output[:kept].count("\n") + 1
    last_line = line_offset + total_lines
    marker = (
        f"\n... [truncated: showing first {kept} of {total} chars "
        f"(cut lands in line {cut_line} of {last_line}). "
        f"Use start_line={cut_line} — optionally with end_line — "
        "to continue without a gap] ..."
    )
    return f"{output[:kept]}{marker}"


def _truncate_ls_output(output: str, max_chars: int) -> str:
    """Head-truncate ls output, preserving the beginning of the listing.

    Directory listings are read top-to-bottom; the head shows the most
    relevant structure.

    The returned string (including the truncation marker) is guaranteed to be
    no longer than max_chars characters. Pass max_chars=0 to disable truncation
    and return the full output unchanged.
    """
    if max_chars == 0:
        return output
    if len(output) <= max_chars:
        return output
    total = len(output)
    marker_max_len = len(f"\n... [truncated: showing first {total} of {total} chars. Use a more specific path to see fewer results] ...")
    kept = max(0, max_chars - marker_max_len)
    if kept == 0:
        return output[:max_chars]
    marker = f"\n... [truncated: showing first {kept} of {total} chars. Use a more specific path to see fewer results] ..."
    return f"{output[:kept]}{marker}"


@tool("bash", parse_docstring=True)
def bash_tool(runtime: ToolRuntime[AgentContext, ThreadState], description: str, command: str) -> str:
    """Execute a bash command in a Linux environment.


    - Use `python` to run Python code.
    - Prefer a thread-local virtual environment in `/mnt/user-data/workspace/.venv`.
    - Use `python -m pip` (inside the virtual environment) to install Python packages.

    Args:
        description: Explain why you are running this command in short words. ALWAYS PROVIDE THIS PARAMETER FIRST.
        command: The bash command to execute. Always use absolute paths for files and directories.
    """
    try:
        sandbox = ensure_sandbox_initialized(runtime)
        if is_local_sandbox(runtime):
            if not is_host_bash_allowed():
                return f"Error: {LOCAL_HOST_BASH_DISABLED_MESSAGE}"
            ensure_thread_directories_exist(runtime)
            thread_data = get_thread_data(runtime)
            validate_local_bash_command_paths(command, thread_data)
            command = replace_virtual_paths_in_command(command, thread_data, runtime)
            command = _apply_cwd_prefix(command, thread_data)
            output = sandbox.execute_command(command)
            try:
                from deerflow.config.app_config import get_app_config

                sandbox_cfg = get_app_config().sandbox
                max_chars = sandbox_cfg.bash_output_max_chars if sandbox_cfg else 20000
            except Exception:
                max_chars = 20000
            return _truncate_bash_output(mask_local_paths_in_output(output, thread_data, runtime), max_chars)
        ensure_thread_directories_exist(runtime)
        try:
            from deerflow.config.app_config import get_app_config

            sandbox_cfg = get_app_config().sandbox
            max_chars = sandbox_cfg.bash_output_max_chars if sandbox_cfg else 20000
        except Exception:
            max_chars = 20000
        return _truncate_bash_output(sandbox.execute_command(command), max_chars)
    except SandboxError as e:
        return f"Error: {e}"
    except PermissionError as e:
        return f"Error: {e}"
    except Exception as e:
        return f"Error: Unexpected error executing command: {_sanitize_error(e, runtime)}"


@tool("ls", parse_docstring=True)
def ls_tool(runtime: ToolRuntime[AgentContext, ThreadState], description: str, path: str) -> str:
    """List the contents of a directory up to 2 levels deep in tree format.

    Args:
        description: Explain why you are listing this directory in short words. ALWAYS PROVIDE THIS PARAMETER FIRST.
        path: The **absolute** path to the directory to list.
    """
    requested_path = path
    try:
        sandbox = ensure_sandbox_initialized(runtime)
        ensure_thread_directories_exist(runtime)
        thread_data = None
        if is_local_sandbox(runtime):
            thread_data = get_thread_data(runtime)
            assert thread_data is not None
            path = _resolve_local_read_path(path, thread_data)
        children = sandbox.list_dir(path)
        if not children:
            return "(empty)"
        output = "\n".join(children)
        if thread_data is not None:
            output = mask_local_paths_in_output(output, thread_data, runtime)
        try:
            from deerflow.config.app_config import get_app_config

            sandbox_cfg = get_app_config().sandbox
            max_chars = sandbox_cfg.ls_output_max_chars if sandbox_cfg else 20000
        except Exception:
            max_chars = 20000
        return _truncate_ls_output(output, max_chars)
    except SandboxError as e:
        return f"Error: {e}"
    except FileNotFoundError:
        return f"Error: Directory not found: {requested_path}"
    except PermissionError:
        return f"Error: Permission denied: {requested_path}"
    except Exception as e:
        return f"Error: Unexpected error listing directory: {_sanitize_error(e, runtime)}"


@tool("glob", parse_docstring=True)
def glob_tool(
    runtime: ToolRuntime[AgentContext, ThreadState],
    description: str,
    pattern: str,
    path: str,
    include_dirs: bool = False,
    max_results: int = _DEFAULT_GLOB_MAX_RESULTS,
) -> str:
    """Find files or directories that match a glob pattern under a root directory.

    Args:
        description: Explain why you are searching for these paths in short words. ALWAYS PROVIDE THIS PARAMETER FIRST.
        pattern: The glob pattern to match relative to the root path, for example `**/*.py`.
        path: The **absolute** root directory to search under.
        include_dirs: Whether matching directories should also be returned. Default is False.
        max_results: Maximum number of paths to return. Default is 200.
    """
    requested_path = path
    try:
        sandbox = ensure_sandbox_initialized(runtime)
        ensure_thread_directories_exist(runtime)
        effective_max_results = _resolve_max_results(
            "glob",
            max_results,
            default=_DEFAULT_GLOB_MAX_RESULTS,
            upper_bound=_MAX_GLOB_MAX_RESULTS,
        )
        thread_data = None
        if is_local_sandbox(runtime):
            thread_data = get_thread_data(runtime)
            if thread_data is None:
                raise SandboxRuntimeError("Thread data not available for local sandbox")
            path = _resolve_local_read_path(path, thread_data)
        matches, truncated = sandbox.glob(path, pattern, include_dirs=include_dirs, max_results=effective_max_results)
        if thread_data is not None:
            matches = [mask_local_paths_in_output(match, thread_data, runtime) for match in matches]
        return _format_glob_results(requested_path, matches, truncated)
    except SandboxError as e:
        return f"Error: {e}"
    except FileNotFoundError:
        return f"Error: Directory not found: {requested_path}"
    except NotADirectoryError:
        return f"Error: Path is not a directory: {requested_path}"
    except PermissionError:
        return f"Error: Permission denied: {requested_path}"
    except Exception as e:
        return f"Error: Unexpected error searching paths: {_sanitize_error(e, runtime)}"


@tool("grep", parse_docstring=True)
def grep_tool(
    runtime: ToolRuntime[AgentContext, ThreadState],
    description: str,
    pattern: str,
    path: str,
    glob: str | None = None,
    literal: bool = False,
    case_sensitive: bool = False,
    max_results: int = _DEFAULT_GREP_MAX_RESULTS,
) -> str:
    """Search for matching lines inside a text file or files under a root directory.

    Args:
        description: Explain why you are searching file contents in short words. ALWAYS PROVIDE THIS PARAMETER FIRST.
        pattern: The string or regex pattern to search for.
        path: The **absolute** file or root directory to search.
        glob: Optional glob filter for candidate files, for example `**/*.py`.
        literal: Whether to treat `pattern` as a plain string. Default is False.
        case_sensitive: Whether matching is case-sensitive. Default is False.
        max_results: Maximum number of matching lines to return. Default is 100.
    """
    requested_path = path
    try:
        sandbox = ensure_sandbox_initialized(runtime)
        ensure_thread_directories_exist(runtime)
        effective_max_results = _resolve_max_results(
            "grep",
            max_results,
            default=_DEFAULT_GREP_MAX_RESULTS,
            upper_bound=_MAX_GREP_MAX_RESULTS,
        )
        thread_data = None
        if is_local_sandbox(runtime):
            thread_data = get_thread_data(runtime)
            if thread_data is None:
                raise SandboxRuntimeError("Thread data not available for local sandbox")
            path = _resolve_local_read_path(path, thread_data)
        matches, truncated = sandbox.grep(
            path,
            pattern,
            glob=glob,
            literal=literal,
            case_sensitive=case_sensitive,
            max_results=effective_max_results,
        )
        if thread_data is not None:
            matches = [
                GrepMatch(
                    path=mask_local_paths_in_output(match.path, thread_data, runtime),
                    line_number=match.line_number,
                    line=match.line,
                )
                for match in matches
            ]
        return _format_grep_results(requested_path, matches, truncated)
    except SandboxError as e:
        return f"Error: {e}"
    except FileNotFoundError:
        return f"Error: Directory not found: {requested_path}"
    except NotADirectoryError:
        return f"Error: Path is not a directory: {requested_path}"
    except re.error as e:
        return f"Error: Invalid regex pattern: {e}"
    except PermissionError:
        return f"Error: Permission denied: {requested_path}"
    except Exception as e:
        return f"Error: Unexpected error searching file contents: {_sanitize_error(e, runtime)}"


@tool("read_file", parse_docstring=True)
def read_file_tool(
    runtime: ToolRuntime[AgentContext, ThreadState],
    description: str,
    path: str,
    start_line: int | None = None,
    end_line: int | None = None,
) -> str:
    """Read the contents of a text file. Use this to examine source code, configuration files, logs, or any text-based file.

    Args:
        description: Explain why you are reading this file in short words. ALWAYS PROVIDE THIS PARAMETER FIRST.
        path: The **absolute** path to the file to read.
        start_line: Optional starting line number (1-indexed, inclusive). Omit to start at the first line.
        end_line: Optional ending line number (1-indexed, inclusive). Omit to read through the last line.
    """
    requested_path = path
    try:
        if start_line is not None and start_line < 1:
            return "(start_line must be >= 1)"
        effective_start = start_line or 1
        if end_line is not None and end_line < 1:
            return "(end_line must be >= 1)"
        if end_line is not None and effective_start > end_line:
            return "(start_line > end_line — no lines in range)"
        sandbox = ensure_sandbox_initialized(runtime)
        ensure_thread_directories_exist(runtime)
        if is_local_sandbox(runtime):
            thread_data = get_thread_data(runtime)
            assert thread_data is not None
            path = _resolve_local_read_path(path, thread_data)
        if start_line is not None or end_line is not None:
            content = sandbox.read_file(path, start_line=start_line, end_line=end_line)
        else:
            content = sandbox.read_file(path)
        if not content:
            return "(empty)"
        try:
            from deerflow.config.app_config import get_app_config

            sandbox_cfg = get_app_config().sandbox
            max_chars = sandbox_cfg.read_file_output_max_chars if sandbox_cfg else 50000
        except Exception:
            max_chars = 50000
        return _truncate_read_file_output(
            content,
            max_chars,
            line_offset=effective_start - 1,
        )
    except SandboxError as e:
        return f"Error: {e}"
    except FileNotFoundError:
        return f"Error: File not found: {requested_path}"
    except PermissionError:
        return f"Error: Permission denied reading file: {requested_path}"
    except IsADirectoryError:
        return f"Error: Path is a directory, not a file: {requested_path}"
    except Exception as e:
        return f"Error: Unexpected error reading file: {_sanitize_error(e, runtime)}"


def read_current_file_content(
    runtime: ToolRuntime[AgentContext, ThreadState],
    path: str,
) -> str:
    """Read complete current content for version-gate hashing."""
    sandbox = ensure_sandbox_initialized(runtime)
    ensure_thread_directories_exist(runtime)
    if is_local_sandbox(runtime):
        thread_data = get_thread_data(runtime)
        if thread_data is None:
            raise SandboxRuntimeError("Thread data not available for local sandbox")
        path = _resolve_local_read_path(path, thread_data)
    return sandbox.read_file(path)


@tool("write_file", parse_docstring=True)
def write_file_tool(
    runtime: ToolRuntime[AgentContext, ThreadState],
    description: str,
    path: str,
    content: str,
    append: bool = False,
) -> str:
    """Write text content to a file.

    Args:
        description: Explain why you are writing to this file in short words. ALWAYS PROVIDE THIS PARAMETER FIRST.
        path: The **absolute** path to the file to write to. ALWAYS PROVIDE THIS PARAMETER SECOND.
        content: The content to write to the file. ALWAYS PROVIDE THIS PARAMETER THIRD.
    """
    requested_path = path
    try:
        sandbox = ensure_sandbox_initialized(runtime)
        ensure_thread_directories_exist(runtime)
        if is_local_sandbox(runtime):
            thread_data = get_thread_data(runtime)
            validate_local_tool_path(path, thread_data)
            assert thread_data is not None
            if not _is_custom_mount_path(path):
                path = _resolve_and_validate_user_data_path(path, thread_data)
            # Custom mount paths are resolved by LocalSandbox._resolve_path()
        with get_file_operation_lock(sandbox, path):
            sandbox.write_file(path, content, append)
        return "OK"
    except SandboxError as e:
        return f"Error: {e}"
    except PermissionError:
        return f"Error: Permission denied writing to file: {requested_path}"
    except IsADirectoryError:
        return f"Error: Path is a directory, not a file: {requested_path}"
    except OSError as e:
        return f"Error: Failed to write file '{requested_path}': {_sanitize_error(e, runtime)}"
    except Exception as e:
        return f"Error: Unexpected error writing file: {_sanitize_error(e, runtime)}"


@tool("str_replace", parse_docstring=True)
def str_replace_tool(
    runtime: ToolRuntime[AgentContext, ThreadState],
    description: str,
    path: str,
    old_str: str,
    new_str: str,
    replace_all: bool = False,
) -> str:
    """Replace a substring in a file with another substring.
    If `replace_all` is False (default), the substring to replace must appear **exactly once** in the file.

    Args:
        description: Explain why you are replacing the substring in short words. ALWAYS PROVIDE THIS PARAMETER FIRST.
        path: The **absolute** path to the file to replace the substring in. ALWAYS PROVIDE THIS PARAMETER SECOND.
        old_str: The substring to replace. ALWAYS PROVIDE THIS PARAMETER THIRD.
        new_str: The new substring. ALWAYS PROVIDE THIS PARAMETER FOURTH.
        replace_all: Whether to replace all occurrences of the substring. If False, only the first occurrence will be replaced. Default is False.
    """
    requested_path = path
    try:
        sandbox = ensure_sandbox_initialized(runtime)
        ensure_thread_directories_exist(runtime)
        if is_local_sandbox(runtime):
            thread_data = get_thread_data(runtime)
            validate_local_tool_path(path, thread_data)
            assert thread_data is not None
            if not _is_custom_mount_path(path):
                path = _resolve_and_validate_user_data_path(path, thread_data)
            # Custom mount paths are resolved by LocalSandbox._resolve_path()
        with get_file_operation_lock(sandbox, path):
            if old_str == "":
                return (
                    "Error: old_str must not be empty. To insert new content into a "
                    "file, use the write_file tool instead."
                )
            content = sandbox.read_file(path)
            if not content:
                return f"Error: String to replace not found in file: {requested_path}"
            if old_str not in content:
                return f"Error: String to replace not found in file: {requested_path}"
            if replace_all:
                content = content.replace(old_str, new_str)
            else:
                content = content.replace(old_str, new_str, 1)
            sandbox.write_file(path, content)
        return "OK"
    except SandboxError as e:
        return f"Error: {e}"
    except FileNotFoundError:
        return f"Error: File not found: {requested_path}"
    except PermissionError:
        return f"Error: Permission denied accessing file: {requested_path}"
    except Exception as e:
        return f"Error: Unexpected error replacing string: {_sanitize_error(e, runtime)}"


@tool("delete_path", parse_docstring=True)
def delete_path_tool(
    runtime: ToolRuntime[AgentContext, ThreadState],
    description: str,
    path: str,
    recursive: bool = False,
) -> str:
    """Delete a file, symlink, or directory from an allowed workspace path.

    Directories must be empty unless ``recursive`` is true. The workspace,
    uploads, outputs, and configured mount roots themselves cannot be deleted.

    Args:
        description: Explain why you are deleting this path in short words. ALWAYS PROVIDE THIS PARAMETER FIRST.
        path: The **absolute** virtual path to delete. ALWAYS PROVIDE THIS PARAMETER SECOND.
        recursive: Delete a non-empty directory tree. Default is False.
    """

    requested_path = path
    try:
        _validate_destructive_virtual_path(path)
        sandbox = ensure_sandbox_initialized(runtime)
        ensure_thread_directories_exist(runtime)
        if is_local_sandbox(runtime):
            thread_data = get_thread_data(runtime)
            if thread_data is None:
                raise SandboxRuntimeError("Thread data not available for local sandbox")
            path = _resolve_local_mutation_path(path, thread_data)
        with get_file_operation_lock(sandbox, path):
            sandbox.delete_path(path, recursive=recursive)
        return "OK"
    except SandboxError as e:
        return f"Error: {e}"
    except FileNotFoundError:
        return f"Error: Path not found: {requested_path}"
    except PermissionError:
        return f"Error: Permission denied deleting path: {requested_path}"
    except OSError as e:
        return f"Error: Failed to delete path '{requested_path}': {_sanitize_error(e, runtime)}"
    except Exception as e:
        return f"Error: Unexpected error deleting path: {_sanitize_error(e, runtime)}"


@tool("move_path", parse_docstring=True)
def move_path_tool(
    runtime: ToolRuntime[AgentContext, ThreadState],
    description: str,
    source: str,
    destination: str,
    overwrite: bool = False,
) -> str:
    """Move or rename a file, symlink, or directory within allowed paths.

    Parent directories for the destination are created automatically. Existing
    files or symlinks are replaced only when ``overwrite`` is true. Existing
    directories are never overwritten implicitly.

    Args:
        description: Explain why you are moving this path in short words. ALWAYS PROVIDE THIS PARAMETER FIRST.
        source: The **absolute** virtual source path. ALWAYS PROVIDE THIS PARAMETER SECOND.
        destination: The **absolute** virtual destination path. ALWAYS PROVIDE THIS PARAMETER THIRD.
        overwrite: Replace an existing file or symlink at the destination. Default is False.
    """

    requested_source = source
    requested_destination = destination
    try:
        _validate_destructive_virtual_path(source)
        _validate_destructive_virtual_path(destination)
        sandbox = ensure_sandbox_initialized(runtime)
        ensure_thread_directories_exist(runtime)
        if is_local_sandbox(runtime):
            thread_data = get_thread_data(runtime)
            if thread_data is None:
                raise SandboxRuntimeError("Thread data not available for local sandbox")
            source = _resolve_local_mutation_path(source, thread_data)
            destination = _resolve_local_mutation_path(destination, thread_data)

        with ExitStack() as stack:
            for operation_path in sorted({source, destination}):
                stack.enter_context(get_file_operation_lock(sandbox, operation_path))
            sandbox.move_path(source, destination, overwrite=overwrite)
        return "OK"
    except SandboxError as e:
        return f"Error: {e}"
    except FileNotFoundError:
        return f"Error: Source path not found: {requested_source}"
    except FileExistsError:
        return f"Error: Destination already exists: {requested_destination}"
    except PermissionError:
        return (
            "Error: Permission denied moving path: "
            f"{requested_source} -> {requested_destination}"
        )
    except OSError as e:
        return (
            f"Error: Failed to move path '{requested_source}' to "
            f"'{requested_destination}': {_sanitize_error(e, runtime)}"
        )
    except Exception as e:
        return f"Error: Unexpected error moving path: {_sanitize_error(e, runtime)}"
