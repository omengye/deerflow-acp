"""Memory storage providers."""

import abc
import copy
import json
import logging
import os
import threading
import uuid
from collections.abc import Callable
from contextlib import contextmanager
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from deerflow.config.agents_config import AGENT_NAME_PATTERN
from deerflow.config.memory_config import get_memory_config
from deerflow.config.paths import get_paths

logger = logging.getLogger(__name__)


@contextmanager
def _memory_file_write_guard(file_path: Path):
    """Coordinate read-modify-write transactions across Linux workers."""
    file_path.parent.mkdir(parents=True, exist_ok=True)
    lock_file = None
    if os.name == "posix":
        import fcntl

        lock_path = file_path.with_name(f".{file_path.name}.lock")
        try:
            lock_file = lock_path.open("a+b")
            fcntl.flock(lock_file.fileno(), fcntl.LOCK_EX)
        except OSError:
            if lock_file is not None:
                lock_file.close()
            raise
    try:
        yield
    finally:
        if lock_file is not None:
            try:
                import fcntl

                fcntl.flock(lock_file.fileno(), fcntl.LOCK_UN)
            finally:
                lock_file.close()


def utc_now_iso_z() -> str:
    """Current UTC time as ISO-8601 with ``Z`` suffix (matches prior naive-UTC output)."""
    return datetime.now(UTC).isoformat().removesuffix("+00:00") + "Z"


def create_empty_memory() -> dict[str, Any]:
    """Create an empty memory structure."""
    return {
        "version": "1.0",
        "lastUpdated": utc_now_iso_z(),
        "user": {
            "workContext": {"summary": "", "updatedAt": ""},
            "personalContext": {"summary": "", "updatedAt": ""},
            "topOfMind": {"summary": "", "updatedAt": ""},
        },
        "history": {
            "recentMonths": {"summary": "", "updatedAt": ""},
            "earlierContext": {"summary": "", "updatedAt": ""},
            "longTermBackground": {"summary": "", "updatedAt": ""},
        },
        "facts": [],
    }


class MemoryStorage(abc.ABC):
    """Abstract base class for memory storage providers."""

    @abc.abstractmethod
    def load(self, agent_name: str | None = None) -> dict[str, Any]:
        """Load memory data for the given agent."""
        pass

    @abc.abstractmethod
    def reload(self, agent_name: str | None = None) -> dict[str, Any]:
        """Force reload memory data for the given agent."""
        pass

    @abc.abstractmethod
    def save(self, memory_data: dict[str, Any], agent_name: str | None = None) -> bool:
        """Save memory data for the given agent."""
        pass

    def mutate(
        self,
        mutator: Callable[[dict[str, Any]], dict[str, Any] | None],
        agent_name: str | None = None,
    ) -> dict[str, Any] | None:
        """Apply a read-modify-write operation.

        Custom providers may override this with a transactional/CAS operation.
        The default preserves compatibility but cannot promise cross-process
        atomicity.
        """
        current = copy.deepcopy(self.reload(agent_name))
        updated = mutator(current)
        if updated is None or not self.save(updated, agent_name):
            return None
        return updated


class FileMemoryStorage(MemoryStorage):
    """File-based memory storage provider."""

    def __init__(self):
        """Initialize the file memory storage."""
        # Per-agent memory cache: keyed by agent_name (None = global)
        # Value: (memory_data, file_mtime)
        self._memory_cache: dict[str | None, tuple[dict[str, Any], float | None]] = {}
        # Guards all reads and writes to _memory_cache across concurrent callers.
        self._cache_lock = threading.Lock()
        # Covers the complete read-modify-write critical section. RLock permits
        # mutate() to call the private save helper without deadlocking.
        self._mutation_lock = threading.RLock()

    def _validate_agent_name(self, agent_name: str) -> None:
        """Validate that the agent name is safe to use in filesystem paths.

        Uses the repository's established AGENT_NAME_PATTERN to ensure consistency
        across the codebase and prevent path traversal or other problematic characters.
        """
        if not agent_name:
            raise ValueError("Agent name must be a non-empty string.")
        if not AGENT_NAME_PATTERN.match(agent_name):
            raise ValueError(f"Invalid agent name {agent_name!r}: names must match {AGENT_NAME_PATTERN.pattern}")

    def _get_memory_file_path(self, agent_name: str | None = None) -> Path:
        """Get the path to the memory file."""
        if agent_name is not None:
            self._validate_agent_name(agent_name)
            if agent_name.startswith("acpmem-"):
                return get_paths().base_dir / "memory-scopes" / agent_name / "memory.json"
            return get_paths().agent_memory_file(agent_name)

        config = get_memory_config()
        if config.storage_path:
            p = Path(config.storage_path)
            return p if p.is_absolute() else get_paths().base_dir / p
        return get_paths().memory_file

    def _load_memory_from_file(
        self,
        agent_name: str | None = None,
        *,
        file_path: Path | None = None,
        strict: bool = False,
    ) -> dict[str, Any]:
        """Load memory data from file."""
        file_path = file_path or self._get_memory_file_path(agent_name)

        if not file_path.exists():
            return create_empty_memory()

        try:
            with open(file_path, encoding="utf-8") as f:
                data = json.load(f)
            return data
        except (json.JSONDecodeError, OSError) as e:
            if strict:
                raise
            logger.warning("Failed to load memory file: %s", e)
            return create_empty_memory()

    def load(self, agent_name: str | None = None) -> dict[str, Any]:
        """Load memory data (cached with file modification time check)."""
        file_path = self._get_memory_file_path(agent_name)

        try:
            current_mtime = file_path.stat().st_mtime if file_path.exists() else None
        except OSError:
            current_mtime = None

        with self._cache_lock:
            cached = self._memory_cache.get(agent_name)
            if cached is not None and cached[1] == current_mtime:
                return cached[0]

        memory_data = self._load_memory_from_file(agent_name)

        with self._cache_lock:
            self._memory_cache[agent_name] = (memory_data, current_mtime)

        return memory_data

    def reload(self, agent_name: str | None = None) -> dict[str, Any]:
        """Reload memory data from file, forcing cache invalidation."""
        file_path = self._get_memory_file_path(agent_name)
        memory_data = self._load_memory_from_file(agent_name)

        try:
            mtime = file_path.stat().st_mtime if file_path.exists() else None
        except OSError:
            mtime = None

        with self._cache_lock:
            self._memory_cache[agent_name] = (memory_data, mtime)
        return memory_data

    def _save_unlocked(
        self,
        memory_data: dict[str, Any],
        agent_name: str | None = None,
        *,
        file_path: Path | None = None,
    ) -> bool:
        """Save memory data while ``_mutation_lock`` is already held."""
        file_path = file_path or self._get_memory_file_path(agent_name)
        temp_path: Path | None = None

        try:
            file_path.parent.mkdir(parents=True, exist_ok=True)
            # Shallow-copy before adding lastUpdated so the caller's dict is not
            # mutated as a side-effect, and the cache reference is not silently
            # updated before the file write succeeds.
            memory_data = {**memory_data, "lastUpdated": utc_now_iso_z()}

            previous_mode = file_path.stat().st_mode if file_path.exists() else None
            temp_path = file_path.with_suffix(f".{uuid.uuid4().hex}.tmp")
            with open(temp_path, "w", encoding="utf-8") as f:
                json.dump(memory_data, f, indent=2, ensure_ascii=False)
                f.flush()
                os.fsync(f.fileno())

            if previous_mode is not None:
                os.chmod(temp_path, previous_mode)
            temp_path.replace(file_path)
            temp_path = None
            if os.name == "posix":
                try:
                    directory_fd = os.open(file_path.parent, os.O_RDONLY)
                    try:
                        os.fsync(directory_fd)
                    finally:
                        os.close(directory_fd)
                except OSError:
                    # Replacement already committed; retain success and report
                    # only the weaker crash-durability guarantee.
                    logger.warning("Memory file replaced but parent directory fsync failed", exc_info=True)

            try:
                mtime = file_path.stat().st_mtime
            except OSError:
                mtime = None

            with self._cache_lock:
                self._memory_cache[agent_name] = (memory_data, mtime)
            try:
                from deerflow.agents.memory.retrieval import rebuild_memory_index

                rebuild_memory_index(memory_data, agent_name)
            except Exception:
                # The JSON document is authoritative. Index failure is a
                # recoverable performance degradation and must not fail save.
                logger.warning("Failed to update memory retrieval index", exc_info=True)
            logger.info("Memory saved to %s", file_path)
            return True
        except OSError as e:
            logger.error("Failed to save memory file: %s", e)
            return False
        finally:
            if temp_path is not None:
                try:
                    temp_path.unlink(missing_ok=True)
                except OSError:
                    logger.warning("Failed to remove temporary memory file %s", temp_path, exc_info=True)

    def save(self, memory_data: dict[str, Any], agent_name: str | None = None) -> bool:
        """Save memory data to file and update cache."""
        with self._mutation_lock:
            file_path = self._get_memory_file_path(agent_name)
            try:
                with _memory_file_write_guard(file_path):
                    return self._save_unlocked(memory_data, agent_name, file_path=file_path)
            except OSError as exc:
                logger.error("Failed to lock memory file %s: %s", file_path, exc)
                return False

    def mutate(
        self,
        mutator: Callable[[dict[str, Any]], dict[str, Any] | None],
        agent_name: str | None = None,
    ) -> dict[str, Any] | None:
        """Atomically apply a read-modify-write operation across Linux workers."""
        with self._mutation_lock:
            file_path = self._get_memory_file_path(agent_name)
            try:
                with _memory_file_write_guard(file_path):
                    # A failed/corrupt read must never be interpreted as an
                    # empty memory document and overwrite recoverable data.
                    current = copy.deepcopy(
                        self._load_memory_from_file(
                            agent_name,
                            file_path=file_path,
                            strict=True,
                        )
                    )
                    updated = mutator(current)
                    if updated is None or not self._save_unlocked(updated, agent_name, file_path=file_path):
                        return None
                    return updated
            except (json.JSONDecodeError, OSError) as exc:
                logger.error(
                    "Failed to mutate memory file %s (%s)",
                    file_path,
                    type(exc).__name__,
                )
                return None


_storage_instance: MemoryStorage | None = None
_storage_lock = threading.Lock()


def get_memory_storage() -> MemoryStorage:
    """Get the configured memory storage instance."""
    global _storage_instance
    if _storage_instance is not None:
        return _storage_instance

    with _storage_lock:
        if _storage_instance is not None:
            return _storage_instance

        config = get_memory_config()
        storage_class_path = config.storage_class

        try:
            module_path, class_name = storage_class_path.rsplit(".", 1)
            import importlib

            module = importlib.import_module(module_path)
            storage_class = getattr(module, class_name)

            # Validate that the configured storage is a MemoryStorage implementation
            if not isinstance(storage_class, type):
                raise TypeError(f"Configured memory storage '{storage_class_path}' is not a class: {storage_class!r}")
            if not issubclass(storage_class, MemoryStorage):
                raise TypeError(f"Configured memory storage '{storage_class_path}' is not a subclass of MemoryStorage")

            _storage_instance = storage_class()
        except Exception as e:
            logger.error(
                "Failed to load memory storage %s, falling back to FileMemoryStorage: %s",
                storage_class_path,
                e,
            )
            _storage_instance = FileMemoryStorage()

    return _storage_instance


def reset_memory_storage() -> None:
    """Reset the cached memory storage instance after configuration changes."""
    global _storage_instance
    with _storage_lock:
        _storage_instance = None
