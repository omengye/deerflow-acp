import threading
from abc import ABC, abstractmethod
from collections.abc import Collection
from pathlib import Path

from deerflow.config import get_app_config
from deerflow.reflection import resolve_class
from deerflow.sandbox.provider_paths import normalize_sandbox_provider_path
from deerflow.sandbox.sandbox import Sandbox


class SandboxProvider(ABC):
    """Abstract base class for sandbox providers"""

    uses_thread_data_mounts: bool = False

    @abstractmethod
    def acquire(
        self,
        thread_id: str | None = None,
        *,
        available_skills: Collection[str] | None = None,
        workspace_path: str | None = None,
    ) -> str:
        """Acquire a sandbox environment and return its ID.

        ``workspace_path`` is an optional existing host directory that should
        back ``/mnt/user-data/workspace`` for this sandbox. Providers that
        cannot safely expose host directories should reject it explicitly.

        Returns:
            The ID of the acquired sandbox environment.
        """
        pass

    @abstractmethod
    def get(self, sandbox_id: str) -> Sandbox | None:
        """Get a sandbox environment by ID.

        Args:
            sandbox_id: The ID of the sandbox environment to retain.
        """
        pass

    @abstractmethod
    def release(self, sandbox_id: str) -> None:
        """Release a sandbox environment.

        Args:
            sandbox_id: The ID of the sandbox environment to destroy.
        """
        pass

    def release_thread(self, thread_id: str) -> None:
        """Release every sandbox resource scoped to one conversation.

        Providers without per-thread resources may keep the default no-op.
        """
        del thread_id

    def active_skill_revisions(self) -> set[str]:
        """Return Skill projections currently reachable by provider resources."""
        return set()


def normalize_workspace_mount_path(workspace_path: str | None) -> str | None:
    """Validate and canonicalize an external workspace mount source."""

    if workspace_path is None:
        return None
    if not isinstance(workspace_path, str) or not workspace_path.strip():
        raise ValueError("Sandbox workspace_path must be a non-empty path")

    path = Path(workspace_path).expanduser()
    if not path.is_absolute():
        raise ValueError("Sandbox workspace_path must be absolute")
    try:
        resolved = path.resolve(strict=True)
    except FileNotFoundError as exc:
        raise ValueError(
            f"Sandbox workspace_path does not exist: {workspace_path}"
        ) from exc
    if not resolved.is_dir():
        raise ValueError(
            f"Sandbox workspace_path is not a directory: {workspace_path}"
        )
    return str(resolved)


def workspace_mount_path_from_thread_data(thread_data: object) -> str | None:
    """Return the canonical external workspace requested by thread state."""

    if not isinstance(thread_data, dict):
        return None
    if thread_data.get("workspace_path_managed", True):
        return None
    value = thread_data.get("workspace_path")
    return normalize_workspace_mount_path(value if isinstance(value, str) else None)


_default_sandbox_provider: SandboxProvider | None = None
_provider_lock = threading.RLock()


def get_sandbox_provider(**kwargs) -> SandboxProvider:
    """Get the sandbox provider singleton.

    Returns a cached singleton instance. Use `reset_sandbox_provider()` to clear
    the cache, or `shutdown_sandbox_provider()` to properly shutdown and clear.

    Returns:
        A sandbox provider instance.
    """
    global _default_sandbox_provider
    with _provider_lock:
        if _default_sandbox_provider is None:
            config = get_app_config()
            cls = resolve_class(normalize_sandbox_provider_path(config.sandbox.use), SandboxProvider)
            _default_sandbox_provider = cls(**kwargs)
        return _default_sandbox_provider


def get_existing_sandbox_provider() -> SandboxProvider | None:
    """Return the initialized provider without creating new resources."""
    with _provider_lock:
        return _default_sandbox_provider


def reset_sandbox_provider() -> None:
    """Reset the sandbox provider singleton.

    This clears the cached instance without calling shutdown.
    The next call to `get_sandbox_provider()` will create a new instance.
    Useful for testing or when switching configurations.

    Note: If the provider has active sandboxes, they will be orphaned.
    Use `shutdown_sandbox_provider()` for proper cleanup.
    """
    global _default_sandbox_provider
    with _provider_lock:
        if _default_sandbox_provider is not None and hasattr(_default_sandbox_provider, "reset"):
            _default_sandbox_provider.reset()
        _default_sandbox_provider = None


def shutdown_sandbox_provider() -> None:
    """Shutdown and reset the sandbox provider.

    This properly shuts down the provider (releasing all sandboxes)
    before clearing the singleton. Call this when the application
    is shutting down or when you need to completely reset the sandbox system.
    """
    global _default_sandbox_provider
    with _provider_lock:
        if _default_sandbox_provider is not None:
            if hasattr(_default_sandbox_provider, "shutdown"):
                _default_sandbox_provider.shutdown()
            _default_sandbox_provider = None


def set_sandbox_provider(provider: SandboxProvider) -> None:
    """Set a custom sandbox provider instance.

    This allows injecting a custom or mock provider for testing purposes.

    Args:
        provider: The SandboxProvider instance to use.
    """
    global _default_sandbox_provider
    with _provider_lock:
        _default_sandbox_provider = provider
