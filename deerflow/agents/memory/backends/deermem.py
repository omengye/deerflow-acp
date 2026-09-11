"""Adapter exposing the existing DeerMem implementation as MemoryManager."""

from __future__ import annotations

import hashlib
import html
import importlib
import json
import os
import sqlite3
from pathlib import Path
from typing import Any

from deerflow.agents.memory.manager import MemoryManager
from deerflow.config.memory_config import MemoryConfig


def memory_bucket(agent_name: str | None, user_id: str | None) -> str | None:
    """Isolate ACP workspace/session memory without reassigning legacy facts."""
    if not user_id or not user_id.startswith(("acp-workspace:", "acp-session:")):
        return agent_name
    digest = hashlib.sha256(json.dumps([agent_name, user_id]).encode()).hexdigest()[:32]
    return f"acpmem-{digest}"


def _probe_retrieval_support(config: MemoryConfig) -> None:
    if not config.retrieval_enabled:
        return
    from deerflow.config.paths import get_paths

    configured_index = (
        Path(config.retrieval_index_path)
        if config.retrieval_index_path
        else get_paths().base_dir / "memory-fts5.sqlite3"
    )
    index_path = configured_index if configured_index.is_absolute() else get_paths().base_dir / configured_index
    index_parent = index_path.parent
    while not index_parent.exists() and index_parent != index_parent.parent:
        index_parent = index_parent.parent
    if not index_parent.exists() or not os.access(
        index_parent, os.W_OK | os.X_OK
    ):
        raise OSError(f"Memory retrieval index parent is not writable: {index_parent}")
    if index_path.exists() and not index_path.is_file():
        raise OSError(f"Memory retrieval index path is not a file: {index_path}")
    if index_path.exists() and not os.access(index_path, os.R_OK | os.W_OK):
        raise OSError(f"Memory retrieval index is not readable and writable: {index_path}")
    try:
        with sqlite3.connect(":memory:") as connection:
            connection.execute("CREATE VIRTUAL TABLE memory_probe USING fts5(content)")
    except sqlite3.Error as exc:
        raise RuntimeError("SQLite FTS5 support is required for DeerMem retrieval") from exc


class DeerMemManager(MemoryManager):
    def __init__(self, config: MemoryConfig | None = None) -> None:
        self._config = config

    @classmethod
    def from_config(cls, config: MemoryConfig) -> DeerMemManager:
        return cls(config)

    @classmethod
    def validate_config(cls, config: MemoryConfig) -> None:
        from deerflow.agents.memory.storage import MemoryStorage

        if config.mode == "tool" and not config.retrieval_enabled:
            raise ValueError("DeerMem tool mode requires backend_config.retrieval_enabled=true")
        module_name, class_name = config.storage_class.rsplit(".", 1)
        storage_class = getattr(importlib.import_module(module_name), class_name)
        if not isinstance(storage_class, type) or not issubclass(storage_class, MemoryStorage):
            raise TypeError(
                f"Configured memory storage {config.storage_class!r} must subclass MemoryStorage"
            )

    def add(
        self,
        *,
        messages: list[Any],
        thread_id: str,
        agent_name: str | None = None,
        user_id: str | None = None,
        **metadata: Any,
    ) -> bool:
        from deerflow.agents.memory.updater import MemoryUpdater

        return MemoryUpdater().update_memory(
            messages=messages,
            thread_id=thread_id,
            agent_name=memory_bucket(agent_name, user_id),
            correction_detected=bool(metadata.get("correction_detected", False)),
            reinforcement_detected=bool(
                metadata.get("reinforcement_detected", False)
            ),
        )

    def get_context(
        self,
        *,
        query: str = "",
        thread_id: str | None = None,
        agent_name: str | None = None,
        user_id: str | None = None,
    ) -> str:
        from deerflow.agents.memory.prompt import format_memory_for_injection
        from deerflow.agents.memory.updater import get_memory_data
        from deerflow.config.memory_config import get_memory_config

        config = get_memory_config()
        if not config.enabled or not config.injection_enabled:
            return ""
        agent_name = memory_bucket(agent_name, user_id)
        data = get_memory_data(agent_name)
        if query and config.retrieval_enabled:
            from deerflow.agents.memory.retrieval import search_memory_facts

            data = {
                **data,
                "facts": search_memory_facts(query, data, agent_name),
            }

        escaped = _escape_memory_data(data)
        return format_memory_for_injection(
            escaped,
            max_tokens=config.max_injection_tokens,
        )

    def warm(self) -> None:
        from deerflow.agents.memory.storage import get_memory_storage

        get_memory_storage().load()

    def probe(self) -> None:
        """Strictly verify the configured storage without mutating global state."""
        from deerflow.agents.memory.storage import FileMemoryStorage, MemoryStorage
        from deerflow.config.paths import get_paths

        config = self._config or MemoryConfig()
        self.validate_config(config)
        module_name, class_name = config.storage_class.rsplit(".", 1)
        storage_class: type[MemoryStorage] = getattr(importlib.import_module(module_name), class_name)

        if storage_class is FileMemoryStorage:
            configured = Path(config.storage_path) if config.storage_path else get_paths().memory_file
            memory_path = configured if configured.is_absolute() else get_paths().base_dir / configured
            if memory_path.exists():
                with memory_path.open(encoding="utf-8") as file:
                    data = json.load(file)
                if not isinstance(data, dict):
                    raise ValueError(f"Memory file {memory_path} must contain a JSON object")
            parent = memory_path.parent
            while not parent.exists() and parent != parent.parent:
                parent = parent.parent
            if not parent.exists() or not os.access(parent, os.W_OK | os.X_OK):
                raise OSError(f"Memory path parent is not writable: {parent}")
            _probe_retrieval_support(config)
            return

        storage = storage_class()
        loaded = storage.load()
        if not isinstance(loaded, dict):
            raise TypeError("Custom memory storage load() must return a dictionary")
        _probe_retrieval_support(config)

    def search(
        self,
        query: str,
        *,
        thread_id: str | None = None,
        agent_name: str | None = None,
        user_id: str | None = None,
        limit: int | None = None,
    ) -> list[dict[str, Any]]:
        from deerflow.agents.memory.retrieval import search_memory_facts
        from deerflow.agents.memory.updater import get_memory_data

        agent_name = memory_bucket(agent_name, user_id)
        facts = search_memory_facts(query, get_memory_data(agent_name), agent_name)
        return facts[:limit] if limit is not None else facts

    def cache_signature(self, agent_name: str | None = None) -> str | None:
        payload = json.dumps(
            self.get_memory(agent_name),
            sort_keys=True,
            ensure_ascii=False,
            default=str,
        )
        return hashlib.sha256(payload.encode("utf-8")).hexdigest()

    def get_memory(self, agent_name: str | None = None) -> dict[str, Any]:
        from deerflow.agents.memory.updater import get_memory_data

        return get_memory_data(agent_name)

    def import_memory(
        self, memory_data: dict[str, Any], agent_name: str | None = None
    ) -> dict[str, Any]:
        from deerflow.agents.memory.updater import import_memory_data

        return import_memory_data(memory_data, agent_name=agent_name)

    def reload_memory(self, agent_name: str | None = None) -> dict[str, Any]:
        from deerflow.agents.memory.updater import reload_memory_data

        return reload_memory_data(agent_name)

    def clear_memory(self, agent_name: str | None = None) -> dict[str, Any]:
        from deerflow.agents.memory.updater import clear_memory_data

        # DeerMem's durable buckets are agent-scoped and currently ignore the
        # runtime user id, so every queued user for this agent targets the same
        # data that is about to be cleared.
        self.cancel_by_agent(agent_name, all_users=True)
        result = clear_memory_data(agent_name)
        # Close the window in which a producer can enqueue while the durable
        # clear is in progress.
        self.cancel_by_agent(agent_name, all_users=True)
        return result

    def create_fact(self, **kwargs: Any) -> dict[str, Any]:
        from deerflow.agents.memory.updater import create_memory_fact

        return create_memory_fact(**kwargs)

    def update_fact(self, **kwargs: Any) -> dict[str, Any]:
        from deerflow.agents.memory.updater import update_memory_fact

        return update_memory_fact(**kwargs)

    def delete_fact(self, fact_id: str, **kwargs: Any) -> dict[str, Any]:
        from deerflow.agents.memory.updater import delete_memory_fact

        return delete_memory_fact(fact_id, **kwargs)


def _escape_memory_data(value: Any) -> Any:
    if isinstance(value, dict):
        return {key: _escape_memory_data(item) for key, item in value.items()}
    if isinstance(value, list):
        return [_escape_memory_data(item) for item in value]
    if isinstance(value, str):
        return html.escape(value, quote=False)
    return value
