"""Automatic retention cleanup for portable ACP session state."""

from __future__ import annotations

import asyncio
import logging
from typing import Any

logger = logging.getLogger(__name__)


async def cleanup_expired_sessions(
    config: Any,
    store: Any,
    runtime: Any,
    *,
    compact: bool = False,
) -> list[str]:
    """Remove expired, unattached sessions and their LangGraph checkpoints."""

    if not config.session_cleanup_enabled:
        return []
    candidates = await store.expired_session_ids(
        closed_retention_days=config.closed_session_retention_days,
        inactive_retention_days=config.inactive_session_retention_days,
    )
    coordinator = runtime.session_coordinator
    reserved = [
        session_id
        for session_id in candidates
        if coordinator.reserve_cleanup(session_id)
    ]
    if not reserved:
        return []
    try:
        for session_id in reserved:
            try:
                await runtime.release_session(session_id)
            except Exception:
                logger.warning(
                    "Failed to release resources for expired ACP session %s",
                    session_id,
                    exc_info=True,
                )
        purged = await runtime.purge_checkpoints(reserved)
        await store.delete_sessions(purged)
        if purged and compact:
            await runtime.compact_checkpoints()
    finally:
        for session_id in reserved:
            coordinator.release_cleanup(session_id)
    if purged:
        logger.info("Purged %d expired ACP session(s)", len(purged))
    return purged


async def run_session_cleanup_loop(config: Any, store: Any, runtime: Any) -> None:
    """Run retention cleanup periodically until the owner cancels the task."""

    if not config.session_cleanup_enabled:
        return
    while True:
        await asyncio.sleep(config.session_cleanup_interval_seconds)
        try:
            await cleanup_expired_sessions(config, store, runtime)
        except asyncio.CancelledError:
            raise
        except Exception:
            logger.exception("Portable ACP session cleanup failed")
