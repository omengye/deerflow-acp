"""Configuration for pluggable long-term memory backends."""

import logging
from typing import Any, Literal

from pydantic import BaseModel, Field, model_validator

logger = logging.getLogger(__name__)

MemoryMode = Literal["middleware", "tool"]

_LEGACY_BACKEND_KEYS = (
    "storage_path",
    "storage_class",
    "debounce_seconds",
    "model_name",
    "max_facts",
    "fact_confidence_threshold",
    "fact_dedup_enabled",
    "fact_dedup_similarity_threshold",
    "max_injection_tokens",
    "retrieval_enabled",
    "retrieval_top_k",
    "retrieval_index_path",
)


class MemoryConfig(BaseModel):
    """Configuration for global memory mechanism."""

    enabled: bool = Field(
        default=True,
        description="Whether to enable memory mechanism",
    )
    manager_class: str = Field(
        default="deermem",
        min_length=1,
        description=(
            "MemoryManager backend: 'deermem', 'mem0', or a dotted custom "
            "class path. Unknown short names fail validation."
        ),
    )
    mode: MemoryMode = Field(
        default="middleware",
        description="Inject memory dynamically in middleware or expose it only through tools.",
    )
    backend_config: dict[str, Any] = Field(
        default_factory=dict,
        description="Backend-specific configuration.",
    )
    shutdown_flush_timeout_seconds: float = Field(
        default=30.0,
        ge=0.1,
        le=300.0,
        description="Maximum graceful-shutdown time for pending memory writes.",
    )
    storage_path: str = Field(
        default="",
        description=(
            "Path to store memory data. "
            "If empty, defaults to `{base_dir}/memory.json` (see Paths.memory_file). "
            "Absolute paths are used as-is. "
            "Relative paths are resolved against `Paths.base_dir` "
            "(not the backend working directory). "
            "Note: if you previously set this to `.deer-flow/memory.json`, "
            "the file will now be resolved as `{base_dir}/.deer-flow/memory.json`; "
            "migrate existing data or use an absolute path to preserve the old location."
        ),
    )
    storage_class: str = Field(
        default="deerflow.agents.memory.storage.FileMemoryStorage",
        description="The class path for memory storage provider",
    )
    debounce_seconds: int = Field(
        default=30,
        ge=1,
        le=300,
        description="Seconds to wait before processing queued updates (debounce)",
    )
    model_name: str | None = Field(
        default=None,
        description="Model name to use for memory updates (None = use default model)",
    )
    max_facts: int = Field(
        default=100,
        ge=10,
        le=500,
        description="Maximum number of facts to store",
    )
    fact_confidence_threshold: float = Field(
        default=0.7,
        ge=0.0,
        le=1.0,
        description="Minimum confidence threshold for storing facts",
    )
    fact_dedup_enabled: bool = Field(
        default=False,
        description="Merge deterministic near-duplicate facts in the same category",
    )
    fact_dedup_similarity_threshold: float = Field(
        default=0.7,
        ge=0.5,
        le=1.0,
        description="Minimum bounded token-Jaccard similarity for fact deduplication",
    )
    injection_enabled: bool = Field(
        default=True,
        description="Whether to inject memory into system prompt",
    )
    max_injection_tokens: int = Field(
        default=2000,
        ge=100,
        le=8000,
        description="Maximum tokens to use for memory injection",
    )
    retrieval_enabled: bool = Field(
        default=True,
        description="Use a local FTS5/BM25 index to inject query-relevant facts",
    )
    retrieval_top_k: int = Field(
        default=12,
        ge=1,
        le=100,
        description="Maximum number of relevant facts injected per model call",
    )
    retrieval_index_path: str = Field(
        default="",
        description=(
            "Path to the SQLite FTS5 index. Empty uses "
            "`{base_dir}/memory-fts5.sqlite3`; relative paths resolve against base_dir."
        ),
    )

    @model_validator(mode="before")
    @classmethod
    def _migrate_legacy_backend_fields(cls, data: Any) -> Any:
        """Keep the old flat schema working while filling ``backend_config``.

        Explicit nested values win when both forms are supplied.  Flat fields
        remain populated so existing extensions and the Admin API keep their
        source-compatible attribute and serialization behavior.
        """
        if not isinstance(data, dict):
            return data
        migrated = dict(data)
        raw_backend = migrated.get("backend_config")
        backend = dict(raw_backend) if isinstance(raw_backend, dict) else {}
        manager = str(migrated.get("manager_class") or "deermem").strip()
        if manager != "deermem":
            # These names belong to the legacy DeerMem schema.  A custom
            # manager is free to use keys such as ``model_name`` or
            # ``storage_path`` with unrelated semantics; copying those values
            # into DeerMem's flat compatibility fields would make Admin
            # validation reject otherwise valid custom configuration.
            migrated["backend_config"] = backend
            return migrated
        for key in _LEGACY_BACKEND_KEYS:
            if key in backend:
                if key in migrated and migrated[key] != backend[key]:
                    logger.warning(
                        "Both memory.%s and memory.backend_config.%s are set; "
                        "the nested value wins.",
                        key,
                        key,
                    )
                migrated[key] = backend[key]
            elif key in migrated:
                backend[key] = migrated[key]
        migrated["backend_config"] = backend
        return migrated

    @model_validator(mode="after")
    def _validate_backend(self) -> "MemoryConfig":
        manager = self.manager_class.strip()
        self.manager_class = manager
        if manager not in {"deermem", "mem0"} and not (
            "." in manager or ":" in manager
        ):
            raise ValueError(
                f"Unknown memory manager {manager!r}; use 'deermem', 'mem0', "
                "or a dotted custom class path"
            )
        if manager == "mem0":
            from deerflow.agents.memory.backends.mem0.config import Mem0Config

            self.backend_config = Mem0Config.model_validate(
                self.backend_config
            ).model_dump()
        elif manager == "deermem":
            # Canonicalize defaults too.  Without this, ``MemoryConfig()`` and
            # a round-trip of its own model_dump differ only because the latter
            # makes flat default fields explicit, causing needless manager
            # teardown on otherwise unchanged config reloads.
            self.backend_config = {
                **{
                    key: getattr(self, key)
                    for key in _LEGACY_BACKEND_KEYS
                    if key not in self.backend_config
                },
                **self.backend_config,
            }
        return self


# Global configuration instance
_memory_config: MemoryConfig = MemoryConfig()


def get_memory_config() -> MemoryConfig:
    """Get the current memory configuration."""
    return _memory_config


def _apply_memory_config(config: MemoryConfig) -> None:
    """Drain the old backend and atomically publish a changed configuration."""
    global _memory_config
    if config == _memory_config:
        return

    previous = _memory_config
    queue = None
    try:
        from deerflow.agents.memory.queue import get_memory_queue

        queue = get_memory_queue()
        queue.pause()
        if not queue.flush(timeout_seconds=previous.shutdown_flush_timeout_seconds):
            raise TimeoutError(
                "Timed out draining pending memory writes before applying new configuration"
            )
    except Exception:
        if queue is not None:
            queue.resume()
        raise

    try:
        _memory_config = config
        try:
            from deerflow.agents.memory.storage import reset_memory_storage

            reset_memory_storage()
        except Exception:
            logger.debug("Could not reset memory storage", exc_info=True)
        try:
            from deerflow.agents.memory.manager import reset_memory_manager

            reset_memory_manager()
        except Exception:
            logger.debug("Could not reset memory manager", exc_info=True)
    finally:
        if queue is not None:
            queue.resume()


def set_memory_config(config: MemoryConfig) -> None:
    """Set the memory configuration after safely draining queued writes."""
    _apply_memory_config(config)


def load_memory_config_from_dict(config_dict: dict) -> None:
    """Load memory configuration from a dictionary."""
    loaded = MemoryConfig(**config_dict)
    # ``reload_app_config`` is also called while constructing cached clients.
    # Avoid closing/recreating a stateful remote manager when the memory
    # section did not actually change.
    if loaded == _memory_config:
        return
    _apply_memory_config(loaded)
