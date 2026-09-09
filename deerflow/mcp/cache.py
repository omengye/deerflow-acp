"""Cross-event-loop cache for MCP tools."""

from __future__ import annotations

import asyncio
import hashlib
import logging
import threading
from pathlib import Path

from langchain_core.tools import BaseTool

logger = logging.getLogger(__name__)

_mcp_tools_cache: list[BaseTool] | None = None
_cache_initialized = False
_init_lock = threading.RLock()
_init_condition = threading.Condition(_init_lock)
_initializing_generation: int | None = None
_cache_generation = 0
_config_signature: tuple[str, int, int, str] | None = None


def _get_config_signature() -> tuple[str, int, int, str] | None:
    """Return a content-aware signature for the resolved MCP config."""
    from deerflow.config.extensions_config import ExtensionsConfig

    config_path = ExtensionsConfig.resolve_config_path()
    if config_path is None:
        return None
    path = Path(config_path)
    try:
        payload = path.read_bytes()
        stat = path.stat()
    except FileNotFoundError:
        return None
    return (str(path.resolve()), stat.st_mtime_ns, stat.st_size, hashlib.sha256(payload).hexdigest())


def _is_cache_stale() -> bool:
    if not _cache_initialized:
        return False
    return _get_config_signature() != _config_signature


def _wait_for_initialization(generation: int | None) -> None:
    """Wait without binding synchronization state to an asyncio loop."""
    with _init_condition:
        _init_condition.wait_for(lambda: _cache_initialized or _initializing_generation != generation)


def _reset_state_locked() -> None:
    global _mcp_tools_cache, _cache_initialized, _config_signature, _cache_generation
    _mcp_tools_cache = None
    _cache_initialized = False
    _config_signature = None
    _cache_generation += 1
    _init_condition.notify_all()


def _retire_pool_and_reset_locked():
    """Swap the session-pool singleton before new wrappers may be built."""
    from deerflow.mcp.session_pool import reset_session_pool

    retired_pool = reset_session_pool()
    _reset_state_locked()
    return retired_pool


async def initialize_mcp_tools() -> list[BaseTool]:
    """Initialize one cache generation, safely across threads/event loops."""
    global _mcp_tools_cache, _cache_initialized, _config_signature
    global _initializing_generation

    while True:
        with _init_condition:
            if _cache_initialized:
                return _mcp_tools_cache or []
            if _initializing_generation is None:
                claim_generation = _cache_generation
                before_signature = _get_config_signature()
                _initializing_generation = claim_generation
                break
            waiting_generation = _initializing_generation
        await asyncio.to_thread(_wait_for_initialization, waiting_generation)

    from deerflow.mcp.tools import get_mcp_tools

    loaded_tools: list[BaseTool] | None = None
    after_signature = None
    succeeded = False
    try:
        logger.info("Initializing MCP tools...")
        loaded_tools = await get_mcp_tools()
        after_signature = _get_config_signature()
        succeeded = True
    finally:
        if not succeeded:
            with _init_condition:
                if _initializing_generation == claim_generation:
                    _initializing_generation = None
                _init_condition.notify_all()

    retired_pool = None
    with _init_condition:
        try:
            if _cache_generation != claim_generation:
                logger.info("MCP cache was reset during initialization; discarding result")
                return []
            if before_signature != after_signature:
                logger.warning("MCP config changed during initialization; discarding result")
                retired_pool = _retire_pool_and_reset_locked()
            else:
                _mcp_tools_cache = loaded_tools
                _cache_initialized = True
                _config_signature = after_signature
                logger.info("MCP tools initialized: %d tool(s)", len(loaded_tools or []))
                return _mcp_tools_cache or []
        finally:
            if _initializing_generation == claim_generation:
                _initializing_generation = None
            _init_condition.notify_all()

    if retired_pool is not None:
        retired_pool.close_all_sync()
    return []


def get_cached_mcp_tools() -> list[BaseTool]:
    """Return cached tools, lazily loading and invalidating when needed."""
    while True:
        retired_pool = None
        with _init_condition:
            if _is_cache_stale():
                logger.info("MCP config changed; retiring cached tools and sessions")
                retired_pool = _retire_pool_and_reset_locked()
            if _cache_initialized:
                return _mcp_tools_cache or []
            if _initializing_generation is not None:
                current_generation = _initializing_generation
                _init_condition.wait_for(
                    lambda: _cache_initialized or _initializing_generation != current_generation
                )
                continue

        if retired_pool is not None:
            retired_pool.close_all_sync()

        try:
            try:
                loop = asyncio.get_running_loop()
            except RuntimeError:
                loop = None
            if loop is not None and loop.is_running():
                import concurrent.futures

                with concurrent.futures.ThreadPoolExecutor(max_workers=1) as executor:
                    executor.submit(asyncio.run, initialize_mcp_tools()).result()
            else:
                asyncio.run(initialize_mcp_tools())
        except Exception:
            logger.exception("Failed to lazy-initialize MCP tools")
            return []


def reset_mcp_tools_cache() -> None:
    """Invalidate cached tools and atomically retire persistent sessions."""
    retired_pool = None
    try:
        with _init_condition:
            retired_pool = _retire_pool_and_reset_locked()
    except Exception:
        logger.debug("MCP session pool retirement during cache reset failed", exc_info=True)
        with _init_condition:
            _reset_state_locked()

    if retired_pool is not None:
        try:
            retired_pool.close_all_sync()
        except Exception:
            logger.debug("MCP session pool cleanup during cache reset failed", exc_info=True)
    logger.info("MCP tools cache reset")
