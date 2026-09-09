"""Client manager — singleton DeerFlowClient with checkpointer lifecycle."""
import asyncio
import atexit
import logging
import sqlite3
import threading
from collections.abc import Coroutine
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from app.config import settings
from deerflow.runtime import (
    ConflictError,
    DisconnectMode,
    MemoryStreamBridge,
    RunManager,
    RunRecord,
    RunStatus,
    make_stream_bridge,
)

logger = logging.getLogger(__name__)


_client_manager = None
_lock = threading.Lock()

_PUBLIC_ARTIFACT_PREFIX = "/mnt/user-data/outputs/"
_LOOP_EVENT_TYPES = frozenset({"loop_warning", "loop_hard_stop"})
_MAX_RECORDED_LOOP_EVENTS = 100


def _record_run_artifacts(record: RunRecord, data: Any) -> None:
    """Accumulate public artifact paths declared by this run's stream state."""
    if not isinstance(data, dict):
        return
    if data.get("name") != "present_files" or data.get("status") == "error":
        return
    artifacts = data.get("presented_artifacts")
    if not isinstance(artifacts, list):
        return

    recorded = record.metadata.setdefault("artifacts", [])
    if not isinstance(recorded, list):
        recorded = []
        record.metadata["artifacts"] = recorded
    seen = {path for path in recorded if isinstance(path, str)}
    for path in artifacts:
        if (
            isinstance(path, str)
            and path.startswith(_PUBLIC_ARTIFACT_PREFIX)
            and path not in seen
        ):
            recorded.append(path)
            seen.add(path)


def _record_loop_event(record: RunRecord, event_type: str, data: Any) -> None:
    """Persist a bounded, non-sensitive loop event in the in-memory run record."""
    if not isinstance(data, dict):
        return
    resolved_type = data.get("type") if event_type == "custom" else event_type
    if resolved_type not in _LOOP_EVENT_TYPES:
        return

    entry: dict[str, Any] = {
        "type": resolved_type,
        "recorded_at": datetime.now(UTC).isoformat(),
    }
    for key in ("reason", "incomplete", "task_id", "subagent_type", "_agent_name"):
        value = data.get(key)
        if isinstance(value, (str, bool)):
            entry[key.lstrip("_")] = value
    message = data.get("message")
    if isinstance(message, str) and message.strip():
        entry["message"] = message.strip()[:2000]

    events = record.metadata.setdefault("loop_events", [])
    if not isinstance(events, list):
        events = []
        record.metadata["loop_events"] = events
    signature = tuple(entry.get(key) for key in ("type", "task_id", "message", "reason"))
    if any(
        isinstance(existing, dict)
        and tuple(existing.get(key) for key in ("type", "task_id", "message", "reason"))
        == signature
        for existing in events
    ):
        return
    events.append(entry)
    if len(events) > _MAX_RECORDED_LOOP_EVENTS:
        del events[: len(events) - _MAX_RECORDED_LOOP_EVENTS]


def _rmtree_via_root_container(paths, thread_id: str) -> None:
    """Remove a thread directory containing root-owned files via a root container.

    The AIO sandbox runs as root, so it leaves root-owned files (the
    daemon-created workdir, image-init scaffolding) inside the bind-mounted
    thread directory that the backend process cannot delete. We mount the
    *parent* directory into a throwaway root container and ``rm -rf`` the
    target so the deletion runs with the same privileges that created the
    files. ``host_thread_dir`` is used so the Docker daemon resolves the mount
    source correctly even when the backend runs inside a container.
    """
    import os
    import subprocess

    host_thread_dir = paths.host_thread_dir(thread_id)
    parent = os.path.dirname(host_thread_dir)
    name = os.path.basename(host_thread_dir)
    if not parent or not name:
        raise RuntimeError(f"Refusing to clean up unexpected thread path: {host_thread_dir!r}")
    subprocess.run(
        ["docker", "run", "--rm", "-v", f"{parent}:/target", "alpine", "rm", "-rf", f"/target/{name}"],
        shell=False,
        capture_output=True,
        timeout=60,
        check=True,
    )


class ClientManager:
    """Manages the shared DeerFlowClient instance."""

    def __init__(self):
        self._client = None
        self._checkpointer = None
        self._sync_sqlite_conn: sqlite3.Connection | None = None
        self._storage_checked = False
        self._init_lock = asyncio.Lock()
        self._async_checkpointer = None
        self._async_checkpointer_cm = None
        self._async_checkpoint_cache_cm = None
        self._async_init_lock = asyncio.Lock()
        self._async_client_lock = asyncio.Lock()
        self._client_cache_lock = threading.RLock()
        self._config_generation = 0
        self._client_map: dict[tuple[object, ...], Any] = {}  # config_key -> DeerFlowClient
        self._async_client_map: dict[tuple[object, ...], Any] = {}  # config_key -> DeerFlowClient with async checkpointer
        self._running_threads: set[str] = set()  # thread_ids currently running
        self._running_thread_counts: dict[str, int] = {}
        self._artifact_archive_threads: set[str] = set()
        self._deleting_threads: set[str] = set()
        self._thread_lock = threading.Lock()
        self.run_manager = RunManager()
        self._stream_bridge_cm = None
        self.stream_bridge = MemoryStreamBridge(queue_maxsize=512)
        self._reconnect_grace_seconds = 600
        self._completed_replay_seconds = 1800
        self._run_metadata_retention_seconds = 3600
        self._stream_subscribers: dict[str, int] = {}
        self._disconnect_expiry_handles: dict[str, asyncio.TimerHandle] = {}
        self._completed_expiry_handles: dict[str, asyncio.TimerHandle] = {}
        self._metadata_cleanup_handles: dict[str, asyncio.TimerHandle] = {}
        self._lifecycle_tasks: set[asyncio.Task] = set()
        self._service_loop: asyncio.AbstractEventLoop | None = None
        self.scheduler_service = None
        self.thread_cleanup_service = None
        self.feishu_channel = None

    def feishu_status(self) -> dict[str, Any]:
        """Return current Feishu channel runtime status."""
        feishu = settings.feishu
        return {
            "enabled": bool(feishu and feishu.enabled),
            "configured": bool(feishu and feishu.app_id),
            "running": self.feishu_channel is not None,
            "app_id": feishu.app_id if feishu and feishu.app_id else "",
        }

    async def start_feishu_channel(self, *, raise_on_error: bool = False) -> dict[str, Any]:
        """Start the Feishu WebSocket channel from current API settings."""
        feishu = settings.feishu
        if not feishu or not feishu.enabled:
            return {**self.feishu_status(), "status": "disabled"}
        if not feishu.app_id:
            return {**self.feishu_status(), "status": "not_configured", "error": "app_id is required"}
        if self.feishu_channel is not None:
            return {**self.feishu_status(), "status": "running"}

        try:
            from app.channels.feishu import FeishuChannel

            channel = FeishuChannel(
                app_id=feishu.app_id,
                app_secret=feishu.app_secret,
                verification_token=feishu.verification_token,
            )
            await channel.start(asyncio.get_running_loop())
            self.feishu_channel = channel
            return {**self.feishu_status(), "status": "started"}
        except ImportError as exc:
            message = 'lark-oapi not installed; install with: uv pip install "deerflow-api[feishu]"'
            if raise_on_error:
                raise RuntimeError(message) from exc
            logger.warning(message)
            return {**self.feishu_status(), "status": "error", "error": message}
        except Exception as exc:
            if raise_on_error:
                raise
            logger.exception("Failed to start Feishu channel")
            return {**self.feishu_status(), "status": "error", "error": str(exc)}

    async def stop_feishu_channel(self) -> dict[str, Any]:
        """Stop the current Feishu channel if one is running."""
        channel = self.feishu_channel
        if channel is None:
            return {**self.feishu_status(), "status": "stopped"}
        try:
            await channel.astop()
        finally:
            self.feishu_channel = None
        return {**self.feishu_status(), "status": "stopped"}

    async def restart_feishu_channel(self, *, raise_on_error: bool = False) -> dict[str, Any]:
        """Restart Feishu channel so updated config can take effect immediately."""
        await self.stop_feishu_channel()
        return await self.start_feishu_channel(raise_on_error=raise_on_error)

    async def startup(self):
        """Initialize the DeerFlowClient on startup."""
        from app.config import ensure_data_dirs, validate_api_exposure
        from deerflow.config.app_config import get_app_config, reload_app_config

        self._service_loop = asyncio.get_running_loop()
        validate_api_exposure(settings)
        ensure_data_dirs()
        if settings.auth_enabled and not settings.api_keys:
            raise RuntimeError("DEER_FLOW_AUTH_ENABLED is true but DEER_FLOW_API_KEYS is empty")

        if settings.config_path:
            reload_app_config(settings.config_path)

        try:
            config = get_app_config()
        except Exception as e:
            raise RuntimeError(
                f"Failed to load DeerFlow config: {e}\n"
                f"Set DEER_FLOW_CONFIG_PATH or place config.yaml in the project root."
            ) from e

        if self._stream_bridge_cm is None:
            self._stream_bridge_cm = make_stream_bridge(config.stream_bridge)
            self.stream_bridge = await self._stream_bridge_cm.__aenter__()
        if config.stream_bridge is not None:
            self._reconnect_grace_seconds = config.stream_bridge.reconnect_grace_seconds
            self._completed_replay_seconds = config.stream_bridge.completed_replay_seconds
            self._run_metadata_retention_seconds = max(
                config.stream_bridge.run_metadata_retention_seconds,
                self._completed_replay_seconds,
            )

        self._assert_storage_ready()

        if config.memory.enabled:
            from deerflow.agents.memory import memory_manager_lease

            # Eager construction makes unknown/custom backends and mem0
            # fail-fast connectivity errors startup failures instead of the
            # first user's model-call failure.
            def warm_memory_manager() -> None:
                with memory_manager_lease() as manager:
                    manager.warm()

            await asyncio.to_thread(warm_memory_manager)

        if settings.checkpointer_type == "sqlite":
            from app.thread_cleanup import ThreadCleanupService

            checkpoint_path = Path(settings.checkpointer_path)
            self.thread_cleanup_service = ThreadCleanupService(
                db_path=checkpoint_path,
                manager=self,
                config=settings.thread_cleanup,
            )
            await self.thread_cleanup_service.start()

        # Delivery must be ready before the scheduler claims overdue work.
        # start_feishu_channel is idempotent and returns a disabled/not-configured
        # status when the optional channel is not in use.
        await self.start_feishu_channel()

        if settings.scheduler_enabled:
            from deerflow.runtime.scheduler import SchedulerService, SchedulerStore

            scheduler_db_path = Path(settings.scheduler_db_path)
            if not scheduler_db_path.is_absolute():
                scheduler_db_path = Path(settings.config_path).parent / scheduler_db_path
            store = SchedulerStore(scheduler_db_path)
            self.scheduler_service = SchedulerService(
                store=store,
                manager=self,
                poll_interval_seconds=settings.scheduler_poll_interval_seconds,
                default_timezone=settings.scheduler_timezone,
                recursion_limit_resolver=lambda: settings.scheduler_recursion_limit,
                max_concurrent_runs=settings.scheduler_max_concurrent_runs,
                max_attempts=settings.scheduler_max_attempts,
                retry_base_seconds=settings.scheduler_retry_base_seconds,
                claim_lease_seconds=settings.scheduler_claim_lease_seconds,
                shutdown_grace_seconds=settings.scheduler_shutdown_grace_seconds,
                run_retention_days=settings.scheduler_run_retention_days,
                max_runs_per_task=settings.scheduler_max_runs_per_task,
            )
            await self.scheduler_service.start()

    def get_client(self, **overrides) -> Any:
        """Get or create a DeerFlowClient instance."""
        from deerflow.client import DeerFlowClient

        generation = self._config_generation
        key = (
            generation,
            settings.checkpointer_type,
            settings.checkpoint_channel_mode,
            settings.checkpoint_delta.snapshot_frequency,
            settings.model_name,
            settings.thinking_enabled,
            settings.subagent_enabled,
            settings.plan_mode,
            settings.max_concurrent_subagents,
            settings.recursion_limit,
            frozenset(overrides.items()),
        )

        with self._client_cache_lock:
            cached = self._client_map.get(key)
            if cached is not None:
                return cached
            kwargs: dict[str, Any] = {
                "config_path": settings.config_path or None,
                "checkpointer": self._get_checkpointer(),
                "checkpoint_channel_mode": settings.checkpoint_channel_mode,
                "checkpoint_snapshot_frequency": settings.checkpoint_delta.snapshot_frequency,
                "model_name": settings.model_name,
                "thinking_enabled": settings.thinking_enabled,
                "subagent_enabled": settings.subagent_enabled,
                "plan_mode": settings.plan_mode,
                "max_concurrent_subagents": settings.max_concurrent_subagents,
                "recursion_limit": settings.recursion_limit,
            }
            kwargs.update(overrides)
            client = DeerFlowClient(**kwargs)
            if generation == self._config_generation:
                self._client_map[key] = client
            return client

    async def get_async_client(self, **overrides) -> Any:
        """Get or create a DeerFlowClient instance backed by an async checkpointer."""
        from deerflow.client import DeerFlowClient

        with self._client_cache_lock:
            generation = self._config_generation
            key = (
                generation,
                "async",
                settings.checkpointer_type,
                settings.checkpoint_channel_mode,
                settings.checkpoint_delta.snapshot_frequency,
                settings.model_name,
                settings.thinking_enabled,
                settings.subagent_enabled,
                settings.plan_mode,
                settings.max_concurrent_subagents,
                settings.recursion_limit,
                frozenset(overrides.items()),
            )
            cached = self._async_client_map.get(key)
            if cached is not None:
                return cached
        async with self._async_client_lock:
            with self._client_cache_lock:
                cached = self._async_client_map.get(key)
                if cached is not None:
                    return cached
            kwargs: dict[str, Any] = {
                "config_path": settings.config_path or None,
                "checkpointer": await self._get_async_checkpointer(),
                "checkpoint_channel_mode": settings.checkpoint_channel_mode,
                "checkpoint_snapshot_frequency": settings.checkpoint_delta.snapshot_frequency,
                "model_name": settings.model_name,
                "thinking_enabled": settings.thinking_enabled,
                "subagent_enabled": settings.subagent_enabled,
                "plan_mode": settings.plan_mode,
                "max_concurrent_subagents": settings.max_concurrent_subagents,
                "recursion_limit": settings.recursion_limit,
            }
            kwargs.update(overrides)
            client = DeerFlowClient(**kwargs)
            # The generation comparison and insertion must be one critical
            # section with reload_runtime_config(). Otherwise a reload can
            # clear the map between these operations and an old client can be
            # inserted back into the freshly invalidated cache.
            with self._client_cache_lock:
                if generation == self._config_generation:
                    self._async_client_map[key] = client
            return client

    def get_checkpointer(self):
        """Get the shared checkpointer for direct operations."""
        return self._get_checkpointer()

    def _get_checkpointer(self):
        """Create the appropriate checkpointer based on settings."""
        if settings.checkpointer_type == "none":
            return None

        if settings.checkpointer_type == "memory":
            from langgraph.checkpoint.memory import InMemorySaver
            if self._checkpointer is None:
                saver = InMemorySaver()
                if settings.checkpoint_channel_mode == "delta":
                    from deerflow.runtime.checkpoint_cache import (
                        MemoryCheckpointHistoryCache,
                        checkpoint_cache_key_prefix,
                    )
                    from deerflow.runtime.checkpointer import CachedHistorySaver

                    cache = MemoryCheckpointHistoryCache(
                        max_entries=settings.checkpoint_cache.max_entries
                    )
                    prefix = checkpoint_cache_key_prefix(
                        settings.checkpoint_cache, "memory"
                    )
                    saver = CachedHistorySaver(saver, cache, key_prefix=prefix)
                self._checkpointer = saver
            return self._checkpointer

        if self._checkpointer is not None:
            return self._checkpointer

        from langgraph.checkpoint.sqlite import SqliteSaver

        db_path = Path(settings.checkpointer_path)
        db_path.parent.mkdir(parents=True, exist_ok=True)

        conn = sqlite3.connect(str(db_path), check_same_thread=False)
        try:
            saver = SqliteSaver(conn)
            try:
                saver.setup()
            except Exception:
                conn.close()
                raise
            self._sync_sqlite_conn = conn
            if settings.checkpoint_channel_mode == "delta":
                from deerflow.runtime.checkpoint_cache import (
                    MemoryCheckpointHistoryCache,
                    checkpoint_cache_key_prefix,
                )
                from deerflow.runtime.checkpointer import CachedHistorySaver

                cache = MemoryCheckpointHistoryCache(
                    max_entries=settings.checkpoint_cache.max_entries
                )
                prefix = checkpoint_cache_key_prefix(
                    settings.checkpoint_cache, f"sqlite:{db_path.resolve()}"
                )
                saver = CachedHistorySaver(saver, cache, key_prefix=prefix)
            self._checkpointer = saver
            atexit.register(conn.close)
            return saver
        except Exception:
            try:
                conn.close()
            except Exception:
                pass
            if not settings.allow_memory_fallback:
                raise
            logger.warning(
                "SQLite checkpointer initialization failed; falling back to memory because DEER_FLOW_ALLOW_MEMORY_FALLBACK=true",
                exc_info=True,
            )
            from langgraph.checkpoint.memory import InMemorySaver
            self._checkpointer = InMemorySaver()
            return self._checkpointer

    async def _get_async_checkpointer(self):
        """Create or return the shared async checkpointer for async graph execution."""
        if settings.checkpointer_type == "none":
            return None

        if self._async_checkpointer is not None:
            return self._async_checkpointer

        async with self._async_init_lock:
            # Double-check inside the lock to avoid racing initialization.
            if self._async_checkpointer is not None:
                return self._async_checkpointer

            if settings.checkpointer_type == "memory":
                from langgraph.checkpoint.memory import InMemorySaver

                saver = InMemorySaver()
                if settings.checkpoint_channel_mode == "delta":
                    from deerflow.runtime.checkpoint_cache import (
                        checkpoint_cache_key_prefix,
                        make_async_checkpoint_cache,
                    )
                    from deerflow.runtime.checkpointer import CachedHistorySaver

                    cache_cm = make_async_checkpoint_cache(
                        settings.checkpoint_cache,
                        serde=saver.serde,
                    )
                    cache = await cache_cm.__aenter__()
                    self._async_checkpoint_cache_cm = cache_cm
                    prefix = checkpoint_cache_key_prefix(
                        settings.checkpoint_cache, "memory"
                    )
                    saver = CachedHistorySaver(saver, cache, key_prefix=prefix)
                self._async_checkpointer = saver
                return self._async_checkpointer

            from langgraph.checkpoint.sqlite.aio import AsyncSqliteSaver

            db_path = Path(settings.checkpointer_path)
            await asyncio.to_thread(db_path.parent.mkdir, parents=True, exist_ok=True)

            cm = AsyncSqliteSaver.from_conn_string(str(db_path))
            saver = None
            cache_cm = None
            try:
                saver = await cm.__aenter__()
                await saver.setup()
                if settings.checkpoint_channel_mode == "delta":
                    from deerflow.runtime.checkpoint_cache import (
                        checkpoint_cache_key_prefix,
                        make_async_checkpoint_cache,
                    )
                    from deerflow.runtime.checkpointer import CachedHistorySaver

                    cache_cm = make_async_checkpoint_cache(
                        settings.checkpoint_cache,
                        serde=saver.serde,
                    )
                    cache = await cache_cm.__aenter__()
                    prefix = checkpoint_cache_key_prefix(
                        settings.checkpoint_cache, f"sqlite:{db_path.resolve()}"
                    )
                    saver = CachedHistorySaver(saver, cache, key_prefix=prefix)
            except Exception:
                # Roll back partial initialization so a retry can succeed and we
                # do not leak an open SQLite connection.
                try:
                    if cache_cm is not None:
                        await cache_cm.__aexit__(None, None, None)
                    await cm.__aexit__(None, None, None)
                except Exception:
                    logger.warning("async checkpointer cleanup failed during init", exc_info=True)
                raise
            self._async_checkpointer_cm = cm
            self._async_checkpoint_cache_cm = cache_cm
            self._async_checkpointer = saver
            return self._async_checkpointer

    def _assert_storage_ready(self) -> None:
        """Validate persistent directories/checkpointer at startup."""
        if self._storage_checked:
            return
        self._assert_dir_writable(Path(settings.data_dir))
        if settings.checkpointer_type == "sqlite":
            db_path = Path(settings.checkpointer_path)
            db_path.parent.mkdir(parents=True, exist_ok=True)
            self._assert_dir_writable(db_path.parent)
            conn = sqlite3.connect(str(db_path))
            try:
                from langgraph.checkpoint.sqlite import SqliteSaver

                SqliteSaver(conn).setup()
            finally:
                conn.close()
        self._storage_checked = True

    @staticmethod
    def _assert_dir_writable(path: Path) -> None:
        path.mkdir(parents=True, exist_ok=True)
        probe = path / ".deerflow-write-check"
        try:
            probe.write_text("ok", encoding="utf-8")
            probe.unlink(missing_ok=True)
        except OSError as exc:
            raise RuntimeError(f"Directory is not writable: {path}") from exc

    def readiness_check(self) -> dict[str, Any]:
        """Return readiness status for /health/ready."""
        checks: dict[str, Any] = {}
        ok = True

        try:
            from deerflow.config.app_config import get_app_config

            config = get_app_config()
            checks["config"] = {"ok": True, "models": len(config.models)}
        except Exception as exc:
            ok = False
            checks["config"] = {"ok": False, "error": str(exc)}

        try:
            self._assert_dir_writable(Path(settings.data_dir))
            checks["data_dir"] = {"ok": True, "path": str(Path(settings.data_dir))}
        except Exception as exc:
            ok = False
            checks["data_dir"] = {"ok": False, "error": str(exc)}

        try:
            if settings.checkpointer_type == "sqlite":
                self._get_checkpointer()
                checks["checkpointer"] = {
                    "ok": True,
                    "type": settings.checkpointer_type,
                    "path": str(Path(settings.checkpointer_path)),
                    "channel_mode": settings.checkpoint_channel_mode,
                    "snapshot_frequency": settings.checkpoint_delta.snapshot_frequency,
                }
            else:
                checks["checkpointer"] = {"ok": True, "type": settings.checkpointer_type}
        except Exception as exc:
            ok = False
            checks["checkpointer"] = {"ok": False, "error": str(exc)}

        if settings.auth_enabled and not settings.api_keys:
            ok = False
            checks["auth"] = {"ok": False, "error": "auth enabled without API keys"}
        else:
            checks["auth"] = {"ok": True, "enabled": settings.auth_enabled}

        try:
            from deerflow.agents.memory import memory_manager_lease
            from deerflow.config.memory_config import get_memory_config

            memory_config = get_memory_config()
            if memory_config.enabled:
                with memory_manager_lease():
                    pass
            checks["memory"] = {
                "ok": True,
                "enabled": memory_config.enabled,
                "manager_class": memory_config.manager_class,
                "mode": memory_config.mode,
            }
        except Exception as exc:
            ok = False
            # Readiness is unauthenticated and custom backends control their
            # exception text.  Keep credentials out of the public response;
            # the authenticated Admin backend test returns a sanitized detail.
            checks["memory"] = {
                "ok": False,
                "error": f"Memory backend initialization failed ({type(exc).__name__})",
            }

        return {"status": "ok" if ok else "error", "checks": checks}

    def reload_runtime_config(
        self,
        *,
        include_extensions: bool = True,
        reset_clients: bool = True,
        extensions_config_path: str | None = None,
    ) -> dict[str, Any]:
        """Reload file-backed runtime config and invalidate cached clients."""
        from deerflow.config.app_config import reload_app_config

        config = reload_app_config(settings.config_path or None)
        extensions_reloaded = False
        mcp_cache_reset = False

        if include_extensions:
            from deerflow.config.extensions_config import reload_extensions_config

            reload_extensions_config(extensions_config_path)
            extensions_reloaded = True
            try:
                from deerflow.mcp.cache import reset_mcp_tools_cache

                reset_mcp_tools_cache()
                mcp_cache_reset = True
            except Exception:
                logger.debug("Failed to reset MCP tools cache after admin reload", exc_info=True)

        clients_reset = False
        if reset_clients:
            with self._client_cache_lock:
                self._config_generation += 1
                self._client_map.clear()
                self._async_client_map.clear()
            clients_reset = True

        with self._thread_lock:
            active_threads = len(self._running_threads)

        result: dict[str, Any] = {
            "success": True,
            "config_version": getattr(config, "model_extra", {}).get("config_version")
            if getattr(config, "model_extra", None)
            else None,
            "models_count": len(getattr(config, "models", []) or []),
            "extensions_reloaded": extensions_reloaded,
            "mcp_cache_reset": mcp_cache_reset,
            "clients_reset": clients_reset,
            "config_generation": self._config_generation,
            "active_threads": active_threads,
        }
        if active_threads:
            result["warning"] = "Reload applies to new runs; active runs continue with their existing client state."
        return result

    async def start_client_stream_run(
        self,
        *,
        thread_id: str,
        message: str,
        kwargs: dict[str, Any],
        request_id: str | None = None,
        run_id: str | None = None,
        entrypoint: str = "chat_stream",
        on_disconnect: str = "cancel",
        multitask_strategy: str = "reject",
        idempotency_actor: str | None = None,
        idempotency_key: str | None = None,
        payload_fingerprint: str | None = None,
    ) -> RunRecord:
        """Create a run and stream DeerFlowClient events through the bridge."""
        try:
            await self.touch_thread_activity(thread_id, source=entrypoint)
        except Exception as exc:
            from app.thread_cleanup import ThreadCleanupInProgressError

            if isinstance(exc, ThreadCleanupInProgressError):
                raise ConflictError(str(exc)) from exc
            raise

        created_result = await self.run_manager.create_or_reject(
            thread_id,
            run_id=run_id,
            on_disconnect=DisconnectMode.cancel if on_disconnect == "cancel" else DisconnectMode.continue_,
            multitask_strategy=multitask_strategy,
            metadata={"request_id": request_id, "entrypoint": entrypoint},
            kwargs=kwargs,
            idempotency_actor=idempotency_actor,
            idempotency_key=idempotency_key,
            payload_fingerprint=payload_fingerprint,
            _return_created=True,
        )
        record, created = created_result
        if not created:
            return record
        if not self.mark_thread_running(thread_id):
            await self.run_manager.cleanup(record.run_id, delay=0)
            raise ConflictError(f"Thread {thread_id} is currently being deleted")
        producer = self._produce_client_stream(
            record=record,
            message=message,
            kwargs=kwargs,
            request_id=request_id,
        )
        try:
            task = asyncio.create_task(producer)
        except BaseException:
            producer.close()
            self.mark_thread_done(thread_id)
            await self.run_manager.cleanup(record.run_id, delay=0)
            raise
        record.task = task
        return record

    async def _produce_client_stream(
        self,
        *,
        record: RunRecord,
        message: str,
        kwargs: dict[str, Any],
        request_id: str | None,
    ) -> None:
        """Background producer for /chat/stream style runs."""
        run_id = record.run_id
        thread_id = record.thread_id
        llm_failure: dict[str, Any] | None = None
        try:
            await self.run_manager.set_status(run_id, RunStatus.running)
            await self.stream_bridge.publish(
                run_id,
                "metadata",
                {
                    "run_id": run_id,
                    "thread_id": thread_id,
                    "request_id": request_id,
                },
            )
            # Build a live_event_callback that bypasses LangGraph's per-node
            # custom-stream buffer and publishes events directly to the bridge.
            # This is used by task_tool to push subagent progress events in
            # real time while the tool coroutine is still running (polling).
            bridge = self.stream_bridge

            async def _live_event_callback(event: dict[str, Any]) -> None:
                _record_loop_event(record, str(event.get("type") or "custom"), event)
                await bridge.publish(run_id, event.get("type", "custom"), event)

            client = await self.get_async_client(**kwargs)
            _agent_name = client.agent_name
            async with asyncio.timeout(settings.chat_request_timeout):
                async for event in client.astream(message, thread_id=thread_id, live_event_callback=_live_event_callback):
                    if record.abort_event.is_set():
                        break
                    data = event.data
                    if isinstance(data, dict) and data.get("type") == "llm_failure":
                        llm_failure = data
                        record.metadata["llm_failure_reason"] = str(data.get("reason") or "unknown")
                        record.metadata["llm_failure_retriable"] = bool(data.get("retriable", False))
                    _record_run_artifacts(record, data)
                    if isinstance(data, dict):
                        data = {**data, "_agent_name": _agent_name}
                    _record_loop_event(record, event.type, data)
                    await self.stream_bridge.publish(run_id, event.type, data)

            if record.abort_event.is_set():
                await self.run_manager.set_status(run_id, RunStatus.interrupted)
            elif llm_failure is not None:
                await self.run_manager.set_status(
                    run_id,
                    RunStatus.error,
                    error=str(llm_failure.get("message") or "LLM request failed"),
                )
            else:
                await self.run_manager.set_status(run_id, RunStatus.success)
        except asyncio.TimeoutError:
            logger.warning("run timed out after %.0fs (run=%s thread=%s)", settings.chat_request_timeout, run_id, thread_id)
            await self.stream_bridge.publish(run_id, "error", {"error": "Run timed out"})
            await self.run_manager.set_status(run_id, RunStatus.timeout, error="Run timed out")
        except asyncio.CancelledError:
            logger.info("run cancelled (run=%s thread=%s)", run_id, thread_id)
            await self.stream_bridge.publish(run_id, "error", {"error": "Run cancelled"})
            await self.run_manager.set_status(run_id, RunStatus.interrupted)
            raise
        except Exception as exc:
            if isinstance(exc, RuntimeError) and "Event loop is closed" in str(exc):
                logger.debug("Stream producer stopped: event loop closed (run=%s thread=%s)", run_id, thread_id)
            else:
                logger.exception("Unhandled error in run producer (run=%s thread=%s)", run_id, thread_id)
                try:
                    await self.stream_bridge.publish(run_id, "error", {"error": "Internal server error"})
                    await self.run_manager.set_status(run_id, RunStatus.error, error="Internal server error")
                except RuntimeError as bridge_err:
                    if "Event loop is closed" not in str(bridge_err):
                        raise
        finally:
            self.mark_thread_done(thread_id)
            try:
                await self.stream_bridge.publish_end(run_id)
                self._schedule_completed_run_retention(run_id)
            except RuntimeError as exc:
                if "Event loop is closed" not in str(exc):
                    raise
                logger.debug("Cleanup skipped: event loop closed (run=%s)", run_id)

    async def cancel_run(self, run_id: str, *, action: str = "interrupt") -> bool:
        """Cancel an in-flight run."""
        return await self.run_manager.cancel(run_id, action=action)

    async def attach_run_stream(self, run_id: str) -> None:
        """Register an SSE subscriber and cancel a pending disconnect expiry."""
        self._stream_subscribers[run_id] = self._stream_subscribers.get(run_id, 0) + 1
        handle = self._disconnect_expiry_handles.pop(run_id, None)
        if handle is not None:
            handle.cancel()

    async def detach_run_stream(self, run_id: str) -> None:
        """Detach an SSE subscriber and start the reconnect grace window."""
        remaining = max(0, self._stream_subscribers.get(run_id, 0) - 1)
        if remaining:
            self._stream_subscribers[run_id] = remaining
            return
        self._stream_subscribers.pop(run_id, None)

        record = self.run_manager.get(run_id)
        if record is None or record.on_disconnect != DisconnectMode.continue_:
            return
        if record.status not in {RunStatus.pending, RunStatus.running}:
            return

        previous = self._disconnect_expiry_handles.pop(run_id, None)
        if previous is not None:
            previous.cancel()
        loop = asyncio.get_running_loop()
        self._disconnect_expiry_handles[run_id] = loop.call_later(
            self._reconnect_grace_seconds,
            self._on_disconnect_grace_elapsed,
            run_id,
        )

    def _on_disconnect_grace_elapsed(self, run_id: str) -> None:
        self._disconnect_expiry_handles.pop(run_id, None)
        if self._stream_subscribers.get(run_id, 0) > 0:
            return
        record = self.run_manager.get(run_id)
        if record is None or record.status not in {RunStatus.pending, RunStatus.running}:
            return
        self._spawn_lifecycle_task(self._expire_run_replay(run_id, reason="disconnect_timeout"))

    async def _expire_run_replay(self, run_id: str, *, reason: str) -> None:
        record = self.run_manager.get(run_id)
        if record is not None:
            record.metadata["replay_expired"] = True
            record.metadata["replay_expired_reason"] = reason
        await self.stream_bridge.expire(run_id)
        logger.info("Run replay expired: run=%s reason=%s", run_id, reason)

    def _schedule_completed_run_retention(self, run_id: str) -> None:
        disconnect_handle = self._disconnect_expiry_handles.pop(run_id, None)
        if disconnect_handle is not None:
            disconnect_handle.cancel()

        completed_handle = self._completed_expiry_handles.pop(run_id, None)
        if completed_handle is not None:
            completed_handle.cancel()
        metadata_handle = self._metadata_cleanup_handles.pop(run_id, None)
        if metadata_handle is not None:
            metadata_handle.cancel()

        loop = asyncio.get_running_loop()
        self._completed_expiry_handles[run_id] = loop.call_later(
            self._completed_replay_seconds,
            self._on_completed_replay_elapsed,
            run_id,
        )
        self._metadata_cleanup_handles[run_id] = loop.call_later(
            self._run_metadata_retention_seconds,
            self._on_run_metadata_elapsed,
            run_id,
        )

    def _on_completed_replay_elapsed(self, run_id: str) -> None:
        self._completed_expiry_handles.pop(run_id, None)
        self._spawn_lifecycle_task(self._expire_run_replay(run_id, reason="completed_retention"))

    def _on_run_metadata_elapsed(self, run_id: str) -> None:
        self._metadata_cleanup_handles.pop(run_id, None)
        self._spawn_lifecycle_task(self._cleanup_run_lifecycle(run_id))

    async def _cleanup_run_lifecycle(self, run_id: str) -> None:
        await self.stream_bridge.cleanup(run_id)
        await self.run_manager.cleanup(run_id, delay=0)
        self._stream_subscribers.pop(run_id, None)

    def _spawn_lifecycle_task(self, coroutine: Coroutine[Any, Any, Any]) -> None:
        task = asyncio.create_task(coroutine)
        self._lifecycle_tasks.add(task)
        task.add_done_callback(self._on_lifecycle_task_done)

    def _on_lifecycle_task_done(self, task: asyncio.Task) -> None:
        self._lifecycle_tasks.discard(task)
        if task.cancelled():
            return
        error = task.exception()
        if error is not None:
            logger.error(
                "Run lifecycle task failed",
                exc_info=(type(error), error, error.__traceback__),
            )

    def mark_thread_running(self, thread_id: str) -> bool:
        with self._thread_lock:
            if (
                thread_id in self._deleting_threads
                or thread_id in self._artifact_archive_threads
            ):
                return False
            self._running_thread_counts[thread_id] = (
                self._running_thread_counts.get(thread_id, 0) + 1
            )
            self._running_threads.add(thread_id)
            return True

    def mark_thread_done(self, thread_id: str):
        with self._thread_lock:
            count = self._running_thread_counts.get(thread_id, 0)
            if count > 1:
                self._running_thread_counts[thread_id] = count - 1
            else:
                self._running_thread_counts.pop(thread_id, None)
                self._running_threads.discard(thread_id)

    def try_reserve_artifact_archive(self, thread_id: str) -> bool:
        """Atomically exclude run/deletion writers while an archive is built."""
        with self._thread_lock:
            if (
                thread_id in self._running_threads
                or thread_id in self._deleting_threads
                or thread_id in self._artifact_archive_threads
            ):
                return False
            self._artifact_archive_threads.add(thread_id)
            self._running_threads.add(thread_id)
            return True

    def release_artifact_archive(self, thread_id: str) -> None:
        with self._thread_lock:
            self._artifact_archive_threads.discard(thread_id)
            if self._running_thread_counts.get(thread_id, 0) == 0:
                self._running_threads.discard(thread_id)

    def is_thread_running(self, thread_id: str) -> bool:
        with self._thread_lock:
            return thread_id in self._running_threads

    async def touch_thread_activity(self, thread_id: str, *, source: str = "runtime") -> None:
        service = self.thread_cleanup_service
        if service is not None:
            await service.touch_thread(thread_id, source=source)

    async def configure_thread_cleanup(self, config) -> None:
        service = self.thread_cleanup_service
        if service is None:
            if settings.checkpointer_type != "sqlite":
                raise RuntimeError("Thread cleanup requires the SQLite checkpointer")
            from app.thread_cleanup import ThreadCleanupService

            checkpoint_path = Path(settings.checkpointer_path)
            service = ThreadCleanupService(db_path=checkpoint_path, manager=self, config=config)
            self.thread_cleanup_service = service
            await service.start()
        else:
            await service.reconfigure(config)

    def delete_thread_completely(self, thread_id: str) -> dict[str, Any]:
        """Delete both checkpointer data and file system data for a thread.

        Atomically refuses to delete a thread that is currently running.
        """
        import shutil

        # Only the state transition is protected by the lock. SQLite deletion,
        # filesystem removal and the Docker fallback can all be slow and must
        # not block unrelated run-state checks.
        with self._thread_lock:
            if thread_id in self._running_threads:
                return {"success": False, "running": True, "detail": f"Thread {thread_id} is currently running"}
            if thread_id in self._deleting_threads:
                return {"success": False, "running": True, "deleting": True, "detail": f"Thread {thread_id} is already being deleted"}
            self._deleting_threads.add(thread_id)

        try:
            # 0. Retire every process-local or durable resource that can keep
            # acting on this conversation. These synchronous bridges are safe
            # here because deletion itself runs on a worker thread.
            try:
                from deerflow.mcp.session_pool import get_session_pool

                asyncio.run(get_session_pool().close_scope(thread_id))
            except Exception:
                logger.warning("MCP scope cleanup failed for %s", thread_id, exc_info=True)
                return {
                    "success": False,
                    "detail": f"Failed to close MCP sessions for {thread_id}",
                }

            scheduler = self.scheduler_service
            if scheduler is not None:
                try:
                    service_loop = self._service_loop
                    if service_loop is not None and service_loop.is_running():
                        asyncio.run_coroutine_threadsafe(
                            scheduler.delete_tasks_for_thread(thread_id),
                            service_loop,
                        ).result(timeout=settings.chat_request_timeout)
                    else:
                        asyncio.run(scheduler.delete_tasks_for_thread(thread_id))
                except Exception:
                    logger.warning("scheduler cleanup failed for %s", thread_id, exc_info=True)
                    return {
                        "success": False,
                        "detail": f"Failed to delete scheduled tasks for {thread_id}",
                    }
            elif settings.scheduler_db_path:
                try:
                    from deerflow.runtime.scheduler import SchedulerStore

                    scheduler_db_path = Path(settings.scheduler_db_path)
                    if not scheduler_db_path.is_absolute():
                        scheduler_db_path = Path(settings.config_path).parent / scheduler_db_path
                    if scheduler_db_path.exists():
                        scheduler_store = SchedulerStore(scheduler_db_path)
                        scheduler_store.setup()
                        asyncio.run(scheduler_store.delete_tasks_for_thread(thread_id))
                except Exception:
                    logger.warning("persisted scheduler cleanup failed for %s", thread_id, exc_info=True)
                    return {
                        "success": False,
                        "detail": f"Failed to delete persisted scheduled tasks for {thread_id}",
                    }

            try:
                from deerflow.sandbox.sandbox_provider import get_existing_sandbox_provider

                sandbox_provider = get_existing_sandbox_provider()
                if sandbox_provider is not None:
                    sandbox_provider.release_thread(thread_id)
            except Exception:
                logger.warning("sandbox cleanup failed for %s", thread_id, exc_info=True)
                return {
                    "success": False,
                    "detail": f"Failed to release sandbox resources for {thread_id}",
                }

            # 1. Delete from checkpointer
            checkpointer = self.get_checkpointer()
            if checkpointer is not None and hasattr(checkpointer, "delete_thread"):
                try:
                    checkpointer.delete_thread(thread_id)
                except Exception as exc:
                    logger.warning("checkpointer delete_thread failed for %s", thread_id, exc_info=True)
                    return {
                        "success": False,
                        "detail": f"Failed to delete checkpoint data for {thread_id}: {type(exc).__name__}",
                    }

            # 2. Delete file system data
            try:
                from deerflow.config.paths import get_paths
                paths = get_paths()
                thread_dir = paths.thread_dir(thread_id)
                if thread_dir.exists():
                    try:
                        shutil.rmtree(thread_dir)
                    except PermissionError:
                        # The sandbox container starts as root (the image's init
                        # needs root to boot), so it leaves root-owned files in the
                        # bind-mounted thread directory — the daemon-created workdir
                        # and image-init scaffolding (e.g. .openhands/skills). The
                        # backend process cannot delete those, so fall back to a
                        # throwaway root container to remove the whole directory.
                        _rmtree_via_root_container(paths, thread_id)
            except Exception as exc:
                logger.warning("filesystem cleanup failed for %s", thread_id, exc_info=True)
                return {
                    "success": False,
                    "detail": f"Checkpoint data deleted but filesystem cleanup failed for {thread_id}: {type(exc).__name__}",
                }

            service = self.thread_cleanup_service
            if service is not None:
                try:
                    service.forget_thread_sync(thread_id)
                except Exception:
                    logger.warning("thread activity cleanup failed for %s", thread_id, exc_info=True)
            return {"success": True, "message": f"Deleted thread {thread_id}"}
        finally:
            with self._thread_lock:
                self._deleting_threads.discard(thread_id)

    def update_thread_metadata(self, thread_id: str, metadata: dict[str, Any]) -> dict[str, Any]:
        """Update thread metadata via direct SQLite write.

        Updates the metadata JSON on the latest checkpoint for the thread.
        """
        import json
        if settings.checkpointer_type != "sqlite":
            return {"success": False, "detail": "Only SQLite supports metadata update"}

        db_path = Path(settings.checkpointer_path)
        if not db_path.exists():
            return {"success": False, "detail": "Checkpointer DB not found"}

        conn = sqlite3.connect(str(db_path), check_same_thread=False)
        conn.row_factory = sqlite3.Row
        try:
            cursor = conn.cursor()
            # Find the latest checkpoint by selecting the one with highest step in metadata
            cursor.execute("""
                SELECT thread_id, checkpoint_ns, checkpoint_id, metadata
                FROM checkpoints
                WHERE thread_id = ?
                ORDER BY json_extract(metadata, '$.step') DESC
                LIMIT 1
            """, (thread_id,))
            row = cursor.fetchone()
            if not row:
                return {"success": False, "detail": f"No checkpoints found for {thread_id}"}

            # Merge new metadata into existing. The DB row may be corrupted
            # (truncated write, manual edit, schema mismatch); surface a clear
            # error rather than the raw JSONDecodeError trace.
            try:
                existing = json.loads(row["metadata"]) if row["metadata"] else {}
            except json.JSONDecodeError:
                logger.exception(
                    "Corrupted metadata JSON for thread %s; resetting", thread_id
                )
                existing = {}
            existing.update(metadata)
            merged = json.dumps(existing)

            cursor.execute("""
                UPDATE checkpoints
                SET metadata = ?
                WHERE thread_id = ? AND checkpoint_ns = ? AND checkpoint_id = ?
            """, [merged, thread_id, row["checkpoint_ns"], row["checkpoint_id"]])
            conn.commit()
            return {"success": True, "message": f"Updated metadata for {thread_id}"}
        except Exception:
            # Log full exception server-side; return generic message to caller.
            logger.exception("update_thread_metadata failed for %s", thread_id)
            return {"success": False, "detail": "Failed to update thread metadata"}
        finally:
            conn.close()

    async def shutdown(self):
        """Cleanup on shutdown."""
        if self.thread_cleanup_service is not None:
            try:
                await self.thread_cleanup_service.stop()
            except Exception:
                logger.warning("Error stopping thread cleanup service", exc_info=True)
            finally:
                self.thread_cleanup_service = None

        if self.scheduler_service is not None:
            try:
                await self.scheduler_service.stop()
            except Exception:
                logger.warning("Error stopping scheduler service", exc_info=True)
            finally:
                self.scheduler_service = None

        # No new deliveries can start after the scheduler has drained/cancelled
        # all tracked dispatches.
        await self.stop_feishu_channel()

        try:
            from deerflow.mcp.session_pool import get_session_pool, reset_session_pool

            await get_session_pool().close_all()
            reset_session_pool()
        except Exception:
            logger.warning("Error during MCP session cleanup", exc_info=True)

        try:
            from deerflow.agents.memory import (
                get_memory_manager,
                reset_memory_manager,
            )
            from deerflow.config.memory_config import get_memory_config

            memory_config = get_memory_config()
            if memory_config.enabled:
                manager = get_memory_manager()
                drained = False
                try:
                    result = await asyncio.wait_for(
                        asyncio.to_thread(manager.shutdown_flush),
                        # Built-in managers apply the configured timeout inside
                        # the queue.  A small scheduling grace keeps this outer
                        # guard useful for custom managers without racing a
                        # normal condition wake-up at the exact deadline.
                        timeout=memory_config.shutdown_flush_timeout_seconds + 1.0,
                    )
                    drained = result is not False
                except TimeoutError:
                    logger.warning(
                        "Memory shutdown flush exceeded %.1fs",
                        memory_config.shutdown_flush_timeout_seconds,
                    )
                finally:
                    if drained:
                        # Retire through the lease-aware factory.  A model call
                        # that still holds a manager lease can finish before
                        # the underlying HTTP client is closed.
                        await asyncio.to_thread(reset_memory_manager)
                    else:
                        # Do not invalidate a remote client while an in-flight
                        # queue worker may still be using it.  Process teardown
                        # will reclaim it after the best-effort write finishes.
                        logger.warning(
                            "Memory manager left open because queued writes did not drain"
                        )
        except Exception:
            logger.warning("Error during memory manager shutdown", exc_info=True)

        self._client_map.clear()
        self._async_client_map.clear()
        try:
            from deerflow.sandbox.sandbox_provider import shutdown_sandbox_provider

            shutdown_sandbox_provider()
        except Exception:
            logger.warning("Error during sandbox provider cleanup", exc_info=True)
        if self._async_checkpoint_cache_cm is not None:
            try:
                await self._async_checkpoint_cache_cm.__aexit__(None, None, None)
            except Exception:
                logger.warning("Error closing async checkpoint cache", exc_info=True)
            finally:
                self._async_checkpoint_cache_cm = None
        if self._async_checkpointer_cm is not None:
            try:
                await self._async_checkpointer_cm.__aexit__(None, None, None)
            except Exception:
                logger.warning("Error during async checkpointer cleanup", exc_info=True)
            finally:
                self._async_checkpointer_cm = None
                self._async_checkpointer = None
        if self._sync_sqlite_conn is not None:
            try:
                self._sync_sqlite_conn.close()
            except Exception:
                logger.warning("Error closing sync SQLite connection", exc_info=True)
            finally:
                self._sync_sqlite_conn = None
        for handles in (
            self._disconnect_expiry_handles,
            self._completed_expiry_handles,
            self._metadata_cleanup_handles,
        ):
            for handle in handles.values():
                handle.cancel()
            handles.clear()
        if self._lifecycle_tasks:
            for task in self._lifecycle_tasks:
                task.cancel()
            await asyncio.gather(*self._lifecycle_tasks, return_exceptions=True)
            self._lifecycle_tasks.clear()

        if self._stream_bridge_cm is not None:
            try:
                await self._stream_bridge_cm.__aexit__(None, None, None)
            except Exception:
                logger.warning("Error closing stream bridge", exc_info=True)
            finally:
                self._stream_bridge_cm = None
                self.stream_bridge = MemoryStreamBridge(queue_maxsize=512)
        self._checkpointer = None
        self._running_threads.clear()
        self._artifact_archive_threads.clear()
        self._stream_subscribers.clear()
        self._service_loop = None


def get_client_manager() -> ClientManager:
    global _client_manager
    if _client_manager is None:
        with _lock:
            if _client_manager is None:
                _client_manager = ClientManager()
    return _client_manager
