"""Configuration bootstrap for the local stdio ACP process."""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

import yaml
from dotenv import load_dotenv

_PROJECT_ROOT = Path(__file__).resolve().parents[2]

# Files whose presence identifies a portable distribution root. Stored as a
# tuple of path segments so it can be joined onto candidate roots with /.
_PORTABLE_MARKER = ("resources", "default-config.yaml")


def _portable_root() -> Path | None:
    """Locate the portable distribution root for this installation.

    Resolution order:
    1. ``DEER_FLOW_PORTABLE_ROOT`` environment variable (set by wrappers).
    2. Layout probe: when this module lives under a ``site-packages``
       directory (portable runtime), walk upward looking for the
       ``resources/default-config.yaml`` marker that identifies the root.
    Returns None for source/editable installs, where the project root's
       ``config.yaml`` remains the correct fallback.
    """
    marker_path = Path(*_PORTABLE_MARKER)
    raw = os.getenv("DEER_FLOW_PORTABLE_ROOT")
    if raw:
        root = Path(raw).expanduser().resolve()
        if (root / marker_path).is_file():
            return root
        return None
    module_path = Path(__file__).resolve()
    for parent in module_path.parents:
        if parent.name != "site-packages":
            continue
        for ancestor in parent.parents:
            if (ancestor / marker_path).is_file():
                return ancestor
        break
    return None


def _portable_config_path(root: Path) -> Path:
    """Resolve (and seed on first run) the user config inside a portable root."""
    config_dir = root / "user-data" / "config"
    config_path = config_dir / "config.yaml"
    if config_path.is_file():
        return config_path
    template = root / Path(*_PORTABLE_MARKER)
    if template.is_file():
        config_dir.mkdir(parents=True, exist_ok=True)
        config_path.write_bytes(template.read_bytes())
    return config_path


def _resolve_config_path(config_path: str | None) -> Path:
    """Resolve config without consulting the ACP client's process cwd."""

    raw = config_path or os.getenv("DEER_FLOW_CONFIG_PATH")
    if raw:
        path = Path(raw).expanduser()
        if not path.is_absolute():
            path = _PROJECT_ROOT / path
        path = path.resolve()
        if not path.exists():
            raise FileNotFoundError(f"DeerFlow configuration file not found: {path}")
        return path
    root = _portable_root()
    if root is not None:
        path = _portable_config_path(root)
        if path.is_file():
            return path
        raise FileNotFoundError(
            f"DeerFlow configuration file not found: {path} (portable root: {root})"
        )
    path = _PROJECT_ROOT / "config.yaml"
    if not path.exists():
        raise FileNotFoundError(f"DeerFlow configuration file not found: {path}")
    return path


def _resolve_path(value: str | Path, base_dir: Path) -> Path:
    path = Path(value).expanduser()
    if not path.is_absolute():
        path = base_dir / path
    return path.resolve()


def _value(section: dict[str, Any], key: str, fallback: Any) -> Any:
    value = section.get(key)
    return fallback if value is None else value


def _env_int(name: str, fallback: int) -> int:
    raw = os.getenv(name)
    if raw is None:
        return fallback
    try:
        return int(raw)
    except ValueError as exc:
        raise ValueError(f"{name} must be an integer") from exc


def _env_float(name: str, fallback: float) -> float:
    raw = os.getenv(name)
    if raw is None:
        return fallback
    try:
        return float(raw)
    except ValueError as exc:
        raise ValueError(f"{name} must be a number") from exc


def _as_bool(value: Any, *, name: str) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        normalized = value.strip().lower()
        if normalized in {"1", "true", "yes", "on"}:
            return True
        if normalized in {"0", "false", "no", "off"}:
            return False
    raise ValueError(f"{name} must be a boolean")


def _string_list(value: Any, *, name: str) -> tuple[str, ...] | None:
    if value is None:
        return None
    if not isinstance(value, list) or not all(isinstance(item, str) for item in value):
        raise ValueError(f"{name} must be a list of strings or null")
    normalized = tuple(dict.fromkeys(item.strip() for item in value if item.strip()))
    return normalized


@dataclass(frozen=True, slots=True)
class LocalACPArtifactConfig:
    """S3-compatible artifact publishing settings for remote ACP clients."""

    endpoint_url: str
    bucket: str
    access_key: str
    secret_key: str
    region: str = "us-east-1"
    prefix: str = "acp-artifacts"
    presigned_get_expires_seconds: int = 900
    max_file_size_bytes: int = 200 * 1024 * 1024
    addressing_style: str = "path"
    verify_ssl: bool = True


@dataclass(frozen=True, slots=True)
class LocalACPConfig:
    """Resolved settings for one local ACP stdio process."""

    config_path: Path
    checkpointer_path: Path
    session_store_path: Path
    deerflow_home: Path | None = None
    deerflow_host_base_dir: Path | None = None
    extensions_config_path: Path | None = None
    max_active_connections: int = 16
    max_active_runs: int = 2
    run_timeout_seconds: float = 600.0
    queue_timeout_seconds: float = 600.0
    session_page_size: int = 50
    model_name: str | None = None
    thinking_enabled: bool = True
    subagent_enabled: bool = False
    plan_mode: bool = True
    max_concurrent_subagents: int = 2
    recursion_limit: int = 200
    goal_auto_continue: bool = False
    goal_max_continuations: int = 3
    goal_max_no_progress_continuations: int = 2
    agent_name: str | None = None
    enable_bash: bool = False
    accept_client_mcp_servers: bool = False
    permission_mode: str = "dangerous"
    tool_allowlist: tuple[str, ...] | None = None
    tool_denylist: tuple[str, ...] = ()
    memory_scope: str = "workspace"
    prompt_overlay: str = ""
    resource_link_max_size_bytes: int = 25 * 1024 * 1024
    session_cleanup_enabled: bool = True
    inactive_session_retention_days: int = 30
    session_cleanup_interval_seconds: float = 3600.0
    closed_session_retention_days: int = 30
    artifacts: LocalACPArtifactConfig | None = None

    @classmethod
    def from_file(cls, config_path: str | None = None) -> "LocalACPConfig":
        resolved = _resolve_config_path(config_path)
        # The API process loads the project .env during app bootstrap, but the
        # standalone ACP daemon does not import that package. Load the .env
        # beside config.yaml here so every ACP entrypoint has the same secrets
        # and environment overrides. Explicit process variables keep priority.
        load_dotenv(resolved.parent / ".env", override=False)
        with resolved.open(encoding="utf-8") as stream:
            raw = yaml.safe_load(stream) or {}
        if not isinstance(raw, dict):
            raise ValueError(f"Configuration root must be an object: {resolved}")

        api = raw.get("api") or raw.get("api_service") or {}
        local = raw.get("local_acp") or {}
        if not isinstance(api, dict) or not isinstance(local, dict):
            raise ValueError("api and local_acp configuration sections must be objects")
        artifact_raw = local.get("artifacts") or {}
        if not isinstance(artifact_raw, dict):
            raise ValueError("local_acp.artifacts must be an object")

        base_dir = resolved.parent
        data_dir = _resolve_path(str(_value(api, "data_dir", "./data")), base_dir)
        checkpointer_raw = os.getenv(
            "DEER_FLOW_ACP_CHECKPOINTER_PATH",
            str(_value(local, "checkpointer_path", data_dir / "acp-checkpoints.db")),
        )
        session_store_raw = os.getenv(
            "DEER_FLOW_ACP_SESSION_STORE_PATH",
            str(_value(local, "session_store_path", data_dir / "acp-sessions.db")),
        )

        max_active_connections = _env_int(
            "DEER_FLOW_ACP_MAX_ACTIVE_CONNECTIONS",
            int(_value(local, "max_active_connections", 16)),
        )
        max_active_runs = _env_int(
            "DEER_FLOW_ACP_MAX_ACTIVE_RUNS",
            int(_value(local, "max_active_runs", 2)),
        )
        run_timeout = _env_float(
            "DEER_FLOW_ACP_RUN_TIMEOUT",
            float(
                _value(
                    local,
                    "run_timeout_seconds",
                    _value(api, "chat_request_timeout", 600.0),
                )
            ),
        )
        queue_timeout = _env_float("DEER_FLOW_ACP_QUEUE_TIMEOUT", float(_value(local, "queue_timeout_seconds", 600.0)))
        if not 0 < queue_timeout <= 86400:
            raise ValueError("local_acp.queue_timeout_seconds must be between 0 and 86400")
        page_size = int(_value(local, "session_page_size", 50))
        recursion_limit = int(
            _value(local, "recursion_limit", _value(api, "recursion_limit", 200))
        )
        goal_max_continuations = _env_int(
            "DEER_FLOW_ACP_GOAL_MAX_CONTINUATIONS",
            int(_value(local, "goal_max_continuations", 3)),
        )
        goal_max_no_progress_continuations = _env_int(
            "DEER_FLOW_ACP_GOAL_MAX_NO_PROGRESS_CONTINUATIONS",
            int(_value(local, "goal_max_no_progress_continuations", 2)),
        )
        max_concurrent_subagents = int(_value(local, "max_concurrent_subagents", 2))
        permission_raw = _value(local, "permission_mode", "dangerous")
        # PyYAML follows YAML 1.1 here and parses an unquoted `off` as False.
        # Treat that representation as the documented permission mode instead
        # of letting daemon startup fail before publishing its endpoint.
        permission_mode = (
            "off" if permission_raw is False else str(permission_raw).strip().lower()
        )
        memory_scope = str(_value(local, "memory_scope", "workspace")).strip().lower()
        session_cleanup_enabled = _as_bool(
            os.getenv(
                "DEER_FLOW_ACP_SESSION_CLEANUP_ENABLED",
                _value(local, "session_cleanup_enabled", True),
            ),
            name="local_acp.session_cleanup_enabled",
        )
        inactive_retention_days = int(
            _value(local, "inactive_session_retention_days", 30)
        )
        cleanup_interval_seconds = _env_float(
            "DEER_FLOW_ACP_SESSION_CLEANUP_INTERVAL",
            float(_value(local, "session_cleanup_interval_seconds", 3600.0)),
        )
        closed_retention_days = int(_value(local, "closed_session_retention_days", 30))
        resource_link_max_size_mb = int(_value(local, "resource_link_max_size_mb", 25))
        tool_allowlist = _string_list(
            local.get("tool_allowlist"), name="local_acp.tool_allowlist"
        )
        tool_denylist = (
            _string_list(local.get("tool_denylist", []), name="local_acp.tool_denylist")
            or ()
        )
        prompt_overlay = str(_value(local, "prompt_overlay", "")).strip()
        prompt_overlay_file = local.get("prompt_overlay_file")
        if prompt_overlay_file:
            overlay_path = _resolve_path(str(prompt_overlay_file), base_dir)
            prompt_overlay = overlay_path.read_text(encoding="utf-8").strip()
        if len(prompt_overlay) > 65_536:
            raise ValueError(
                "local_acp.prompt_overlay must be at most 65536 characters"
            )
        deerflow_home_raw = (
            api.get("deerflow_home")
            or os.getenv("DEER_FLOW_HOME")
            or data_dir / "deerflow"
        )
        host_base_raw = api.get("deerflow_host_base_dir") or os.getenv(
            "DEER_FLOW_HOST_BASE_DIR"
        )
        extensions_raw = api.get("extensions_config_path") or os.getenv(
            "DEER_FLOW_EXTENSIONS_CONFIG_PATH"
        )

        if not 1 <= max_active_connections <= 128:
            raise ValueError(
                "local_acp.max_active_connections must be between 1 and 128"
            )
        if max_active_runs < 1:
            raise ValueError("local_acp.max_active_runs must be at least 1")
        if run_timeout <= 0:
            raise ValueError("local_acp.run_timeout_seconds must be greater than 0")
        if not 1 <= page_size <= 500:
            raise ValueError("local_acp.session_page_size must be between 1 and 500")
        if recursion_limit < 10:
            raise ValueError("local_acp.recursion_limit must be at least 10")
        if not 0 <= goal_max_continuations <= 8:
            raise ValueError(
                "local_acp.goal_max_continuations must be between 0 and 8"
            )
        if not 0 <= goal_max_no_progress_continuations <= 8:
            raise ValueError(
                "local_acp.goal_max_no_progress_continuations must be between 0 and 8"
            )
        if not 1 <= max_concurrent_subagents <= 4:
            raise ValueError(
                "local_acp.max_concurrent_subagents must be between 1 and 4"
            )
        if permission_mode not in {"off", "dangerous", "all"}:
            raise ValueError("local_acp.permission_mode must be off, dangerous, or all")
        if memory_scope not in {"global", "workspace", "session"}:
            raise ValueError(
                "local_acp.memory_scope must be global, workspace, or session"
            )
        if not 0 <= closed_retention_days <= 3650:
            raise ValueError(
                "local_acp.closed_session_retention_days must be between 0 and 3650"
            )
        if not 1 <= inactive_retention_days <= 3650:
            raise ValueError(
                "local_acp.inactive_session_retention_days must be between 1 and 3650"
            )
        if not 60 <= cleanup_interval_seconds <= 86_400:
            raise ValueError(
                "local_acp.session_cleanup_interval_seconds must be between "
                "60 and 86400"
            )
        if not 1 <= resource_link_max_size_mb <= 2048:
            raise ValueError(
                "local_acp.resource_link_max_size_mb must be between 1 and 2048"
            )

        artifact_enabled = _as_bool(
            os.getenv(
                "DEER_FLOW_ACP_ARTIFACTS_ENABLED",
                _value(artifact_raw, "enabled", False),
            ),
            name="local_acp.artifacts.enabled",
        )
        artifact_config: LocalACPArtifactConfig | None = None
        if artifact_enabled:
            endpoint_url = (
                str(
                    os.getenv(
                        "DEER_FLOW_ACP_ARTIFACT_ENDPOINT",
                        _value(artifact_raw, "endpoint_url", ""),
                    )
                )
                .strip()
                .rstrip("/")
            )
            bucket = str(
                os.getenv(
                    "DEER_FLOW_ACP_ARTIFACT_BUCKET",
                    _value(artifact_raw, "bucket", ""),
                )
            ).strip()
            access_key = os.getenv("DEER_FLOW_ACP_ARTIFACT_ACCESS_KEY", "").strip()
            secret_key = os.getenv("DEER_FLOW_ACP_ARTIFACT_SECRET_KEY", "").strip()
            if urlparse(endpoint_url).scheme not in {"http", "https"}:
                raise ValueError(
                    "local_acp.artifacts.endpoint_url must be an http(s) URL"
                )
            if not bucket:
                raise ValueError("local_acp.artifacts.bucket must not be empty")
            if not access_key or not secret_key:
                raise ValueError(
                    "DEER_FLOW_ACP_ARTIFACT_ACCESS_KEY and "
                    "DEER_FLOW_ACP_ARTIFACT_SECRET_KEY are required when artifacts are enabled"
                )
            prefix = str(_value(artifact_raw, "prefix", "acp-artifacts")).strip("/")
            if not prefix or any(part in {"", ".", ".."} for part in prefix.split("/")):
                raise ValueError("local_acp.artifacts.prefix is invalid")
            expires = _env_int(
                "DEER_FLOW_ACP_ARTIFACT_GET_EXPIRES",
                int(_value(artifact_raw, "presigned_get_expires_seconds", 900)),
            )
            max_size_mb = _env_int(
                "DEER_FLOW_ACP_ARTIFACT_MAX_FILE_SIZE_MB",
                int(_value(artifact_raw, "max_file_size_mb", 200)),
            )
            if not 60 <= expires <= 604_800:
                raise ValueError(
                    "local_acp.artifacts.presigned_get_expires_seconds must be between 60 and 604800"
                )
            if max_size_mb < 1:
                raise ValueError(
                    "local_acp.artifacts.max_file_size_mb must be at least 1"
                )
            addressing_style = str(
                _value(artifact_raw, "addressing_style", "path")
            ).strip()
            if addressing_style not in {"path", "virtual", "auto"}:
                raise ValueError(
                    "local_acp.artifacts.addressing_style must be path, virtual, or auto"
                )
            artifact_config = LocalACPArtifactConfig(
                endpoint_url=endpoint_url,
                bucket=bucket,
                access_key=access_key,
                secret_key=secret_key,
                region=str(_value(artifact_raw, "region", "us-east-1")).strip()
                or "us-east-1",
                prefix=prefix,
                presigned_get_expires_seconds=expires,
                max_file_size_bytes=max_size_mb * 1024 * 1024,
                addressing_style=addressing_style,
                verify_ssl=_as_bool(
                    os.getenv(
                        "DEER_FLOW_ACP_ARTIFACT_VERIFY_SSL",
                        _value(artifact_raw, "verify_ssl", True),
                    ),
                    name="local_acp.artifacts.verify_ssl",
                ),
            )

        result = cls(
            config_path=resolved,
            checkpointer_path=_resolve_path(checkpointer_raw, base_dir),
            session_store_path=_resolve_path(session_store_raw, base_dir),
            deerflow_home=_resolve_path(str(deerflow_home_raw), base_dir),
            deerflow_host_base_dir=(
                _resolve_path(str(host_base_raw), base_dir) if host_base_raw else None
            ),
            extensions_config_path=(
                _resolve_path(str(extensions_raw), base_dir) if extensions_raw else None
            ),
            max_active_connections=max_active_connections,
            max_active_runs=max_active_runs,
            run_timeout_seconds=run_timeout,
            queue_timeout_seconds=queue_timeout,
            session_page_size=page_size,
            model_name=_value(local, "model_name", api.get("model_name")),
            thinking_enabled=_as_bool(
                _value(
                    local, "thinking_enabled", _value(api, "thinking_enabled", True)
                ),
                name="local_acp.thinking_enabled",
            ),
            subagent_enabled=_as_bool(
                _value(local, "subagent_enabled", False),
                name="local_acp.subagent_enabled",
            ),
            plan_mode=_as_bool(
                _value(local, "plan_mode", _value(api, "plan_mode", True)),
                name="local_acp.plan_mode",
            ),
            max_concurrent_subagents=max_concurrent_subagents,
            recursion_limit=recursion_limit,
            goal_auto_continue=_as_bool(
                os.getenv(
                    "DEER_FLOW_ACP_GOAL_AUTO_CONTINUE",
                    _value(local, "goal_auto_continue", False),
                ),
                name="local_acp.goal_auto_continue",
            ),
            goal_max_continuations=goal_max_continuations,
            goal_max_no_progress_continuations=(
                goal_max_no_progress_continuations
            ),
            agent_name=_value(local, "agent_name", api.get("agent_name")),
            enable_bash=_as_bool(
                os.getenv(
                    "DEER_FLOW_ACP_ENABLE_BASH",
                    _value(local, "enable_bash", False),
                ),
                name="local_acp.enable_bash",
            ),
            accept_client_mcp_servers=_as_bool(
                os.getenv(
                    "DEER_FLOW_ACP_ACCEPT_CLIENT_MCP_SERVERS",
                    _value(local, "accept_client_mcp_servers", False),
                ),
                name="local_acp.accept_client_mcp_servers",
            ),
            permission_mode=permission_mode,
            tool_allowlist=tool_allowlist,
            tool_denylist=tool_denylist,
            memory_scope=memory_scope,
            prompt_overlay=prompt_overlay,
            resource_link_max_size_bytes=resource_link_max_size_mb * 1024 * 1024,
            session_cleanup_enabled=session_cleanup_enabled,
            inactive_session_retention_days=inactive_retention_days,
            session_cleanup_interval_seconds=cleanup_interval_seconds,
            closed_session_retention_days=closed_retention_days,
            artifacts=artifact_config,
        )
        return result

    def prepare_environment(self) -> None:
        """Make path resolution independent from the ACP client's cwd."""

        os.environ["DEER_FLOW_CONFIG_PATH"] = str(self.config_path)
        if self.deerflow_home is not None:
            os.environ["DEER_FLOW_HOME"] = str(self.deerflow_home)
        if self.deerflow_host_base_dir is not None:
            os.environ["DEER_FLOW_HOST_BASE_DIR"] = str(self.deerflow_host_base_dir)
        if self.extensions_config_path is not None:
            os.environ["DEER_FLOW_EXTENSIONS_CONFIG_PATH"] = str(
                self.extensions_config_path
            )

        self.checkpointer_path.parent.mkdir(parents=True, exist_ok=True)
        self.session_store_path.parent.mkdir(parents=True, exist_ok=True)
        if self.deerflow_home is not None:
            self.deerflow_home.mkdir(parents=True, exist_ok=True)
