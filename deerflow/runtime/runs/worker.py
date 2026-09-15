"""Background agent execution.

Runs an agent graph inside an ``asyncio.Task``, publishing events to
a :class:`StreamBridge` as they are produced.

Uses ``graph.astream(stream_mode=[...])`` which gives correct full-state
snapshots for ``values`` mode, proper ``{node: writes}`` for ``updates``,
and ``(chunk, metadata)`` tuples for ``messages`` mode.

Note: ``events`` mode is not supported through the gateway — it requires
``graph.astream_events()`` which cannot simultaneously produce ``values``
snapshots.  The JS open-source LangGraph API server works around this via
internal checkpoint callbacks that are not exposed in the Python public API.
"""

from __future__ import annotations

import asyncio
import copy
import inspect
import logging
from dataclasses import dataclass
from typing import Any, Literal

from langgraph.types import Overwrite

from deerflow.runtime.checkpoint_lock import acquire_checkpoint_thread_lock
from deerflow.runtime.checkpoint_mode import (
    INTERNAL_CHECKPOINT_MODE_KEY,
    aensure_checkpoint_mode_compatible,
    frozen_checkpoint_channel_mode,
    inject_checkpoint_mode,
)
from deerflow.runtime.checkpoint_state import (
    CheckpointStateAccessor,
    build_state_mutation_graph,
    graph_reducer_channels,
    graph_state_schema,
    graph_writable_channels,
)
from deerflow.runtime.serialization import serialize
from deerflow.runtime.stream_bridge import StreamBridge
from deerflow.runtime.assembly import run_in_assembly_executor
from deerflow.tracing.metadata import inject_langfuse_metadata
from deerflow.tracing.naming import resolve_root_run_name

from .manager import RunManager, RunRecord
from .schemas import RunStatus

logger = logging.getLogger(__name__)

@dataclass(frozen=True)
class RollbackPoint:
    config: dict[str, Any]
    state_values: dict[str, Any]
    messages: tuple[Any, ...]
    pending_writes: tuple[tuple[str, str, Any], ...]

# Valid stream_mode values for LangGraph's graph.astream()
_VALID_LG_MODES = {"values", "updates", "checkpoints", "tasks", "debug", "messages", "custom"}


async def run_agent(
    bridge: StreamBridge,
    run_manager: RunManager,
    record: RunRecord,
    *,
    checkpointer: Any,
    store: Any | None = None,
    agent_factory: Any,
    graph_input: dict,
    config: dict,
    stream_modes: list[str] | None = None,
    stream_subgraphs: bool = False,
    interrupt_before: list[str] | Literal["*"] | None = None,
    interrupt_after: list[str] | Literal["*"] | None = None,
) -> None:
    """Execute an agent in the background, publishing events to *bridge*."""

    run_id = record.run_id
    thread_id = record.thread_id
    requested_modes: set[str] = set(stream_modes or ["values"])
    pre_run_checkpoint_id: str | None = None
    rollback_point: RollbackPoint | None = None
    snapshot_capture_failed = False
    accessor: CheckpointStateAccessor | None = None
    configured_mode = (config.get("configurable") or {}).get(
        INTERNAL_CHECKPOINT_MODE_KEY
    )
    mode = frozen_checkpoint_channel_mode() or (
        configured_mode if configured_mode in {"full", "delta"} else "full"
    )
    inject_checkpoint_mode(config, mode)
    checkpoint_lock = await acquire_checkpoint_thread_lock(thread_id)

    # Track whether "events" was requested but skipped
    if "events" in requested_modes:
        logger.info(
            "Run %s: 'events' stream_mode not supported in gateway (requires astream_events + checkpoint callbacks). Skipping.",
            run_id,
        )

    try:
        # 1. Mark running
        await run_manager.set_status(run_id, RunStatus.running)

        # 2. Publish metadata — useStream needs both run_id AND thread_id
        await bridge.publish(
            run_id,
            "metadata",
            {
                "run_id": run_id,
                "thread_id": thread_id,
            },
        )

        # 3. Build the agent
        from langchain_core.runnables import RunnableConfig
        from langgraph.runtime import Runtime

        # Inject runtime context so middlewares can access thread_id and run_id
        # (langgraph-cli does this automatically; we must do it manually).
        # ``run_id`` is exposed so middlewares like LoopDetectionMiddleware
        # can scope per-run state (pending warnings) without leaking across
        # requests on the same thread.
        runtime = Runtime(context={"thread_id": thread_id, "run_id": run_id}, store=store)
        # If the caller already set a ``context`` key (LangGraph >= 0.6.0
        # prefers it over ``configurable`` for thread-level data), make
        # sure ``thread_id`` / ``run_id`` are available there too.
        if "context" in config and isinstance(config["context"], dict):
            config["context"].setdefault("thread_id", thread_id)
            config["context"].setdefault("run_id", run_id)
        config.setdefault("configurable", {})["__pregel_runtime"] = runtime

        # Tag the run for Langfuse: session_id == thread_id, user_id is pulled
        # from record.metadata when the caller supplied it, and run_name is
        # resolved to a human-readable agent name when configured.
        record_metadata = record.metadata if isinstance(record.metadata, dict) else {}
        user_id_value = record_metadata.get("user_id") if record_metadata else None
        user_id = str(user_id_value) if user_id_value is not None else None
        if user_id is not None:
            runtime.context["user_id"] = user_id
            if "context" in config and isinstance(config["context"], dict):
                config["context"].setdefault("user_id", user_id)
        environment = record_metadata.get("environment") if record_metadata else None
        inject_langfuse_metadata(
            config,
            thread_id=thread_id,
            user_id=user_id,
            assistant_id=record.assistant_id,
            environment=str(environment) if environment is not None else None,
        )
        config.setdefault("run_name", resolve_root_run_name(config, record.assistant_id))

        runnable_config = RunnableConfig(**config)
        agent = await run_in_assembly_executor(
            agent_factory,
            config=runnable_config,
        )

        # 4. Attach checkpointer and store
        if checkpointer is not None:
            agent.checkpointer = checkpointer
        if store is not None:
            agent.store = store

        if checkpointer is not None:
            accessor = CheckpointStateAccessor.bind(
                agent, checkpointer, store=store, mode=mode
            )
            checkpoint_config = {
                "configurable": {"thread_id": thread_id, "checkpoint_ns": ""}
            }
            try:
                await aensure_checkpoint_mode_compatible(
                    checkpointer, checkpoint_config, mode
                )
                rollback_point = await _capture_rollback_point(
                    accessor, checkpointer, checkpoint_config
                )
                if rollback_point is not None:
                    pre_run_checkpoint_id = rollback_point.config[
                        "configurable"
                    ].get("checkpoint_id")
            except Exception:
                snapshot_capture_failed = True
                logger.warning(
                    "Could not capture pre-run checkpoint snapshot for run %s",
                    run_id,
                    exc_info=True,
                )
                raise

        # 5. Set interrupt nodes
        if interrupt_before:
            agent.interrupt_before_nodes = interrupt_before
        if interrupt_after:
            agent.interrupt_after_nodes = interrupt_after

        # 6. Build LangGraph stream_mode list
        #    "events" is NOT a valid astream mode — skip it
        #    "messages-tuple" maps to LangGraph's "messages" mode
        lg_modes: list[str] = []
        for m in requested_modes:
            if m == "messages-tuple":
                lg_modes.append("messages")
            elif m == "events":
                # Skipped — see log above
                continue
            elif m in _VALID_LG_MODES:
                lg_modes.append(m)
        if not lg_modes:
            lg_modes = ["values"]

        # Deduplicate while preserving order
        seen: set[str] = set()
        deduped: list[str] = []
        for m in lg_modes:
            if m not in seen:
                seen.add(m)
                deduped.append(m)
        lg_modes = deduped

        logger.info("Run %s: streaming with modes %s (requested: %s)", run_id, lg_modes, requested_modes)

        # 7. Stream using graph.astream
        if len(lg_modes) == 1 and not stream_subgraphs:
            # Single mode, no subgraphs: astream yields raw chunks
            single_mode = lg_modes[0]
            async for chunk in agent.astream(graph_input, config=runnable_config, stream_mode=single_mode):
                if record.abort_event.is_set():
                    logger.info("Run %s abort requested — stopping", run_id)
                    break
                sse_event = _lg_mode_to_sse_event(single_mode)
                await bridge.publish(run_id, sse_event, serialize(chunk, mode=single_mode))
        else:
            # Multiple modes or subgraphs: astream yields tuples
            async for item in agent.astream(
                graph_input,
                config=runnable_config,
                stream_mode=lg_modes,
                subgraphs=stream_subgraphs,
            ):
                if record.abort_event.is_set():
                    logger.info("Run %s abort requested — stopping", run_id)
                    break

                mode, chunk = _unpack_stream_item(item, lg_modes, stream_subgraphs)
                if mode is None:
                    continue

                sse_event = _lg_mode_to_sse_event(mode)
                await bridge.publish(run_id, sse_event, serialize(chunk, mode=mode))

        # 8. Final status
        if record.abort_event.is_set():
            action = record.abort_action
            if action == "rollback":
                await run_manager.set_status(run_id, RunStatus.error, error="Rolled back by user")
                try:
                    await _rollback_to_pre_run_checkpoint(
                        accessor=accessor,
                        checkpointer=checkpointer,
                        thread_id=thread_id,
                        run_id=run_id,
                        rollback_point=rollback_point,
                        snapshot_capture_failed=snapshot_capture_failed,
                    )
                    logger.info("Run %s rolled back to pre-run checkpoint %s", run_id, pre_run_checkpoint_id)
                except Exception:
                    logger.warning("Failed to rollback checkpoint for run %s", run_id, exc_info=True)
            else:
                await run_manager.set_status(run_id, RunStatus.interrupted)
        else:
            await run_manager.set_status(run_id, RunStatus.success)

    except asyncio.CancelledError:
        action = record.abort_action
        if action == "rollback":
            await run_manager.set_status(run_id, RunStatus.error, error="Rolled back by user")
            try:
                await _rollback_to_pre_run_checkpoint(
                    accessor=accessor,
                    checkpointer=checkpointer,
                    thread_id=thread_id,
                    run_id=run_id,
                    rollback_point=rollback_point,
                    snapshot_capture_failed=snapshot_capture_failed,
                )
                logger.info("Run %s was cancelled and rolled back", run_id)
            except Exception:
                logger.warning("Run %s cancellation rollback failed", run_id, exc_info=True)
        else:
            await run_manager.set_status(run_id, RunStatus.interrupted)
            logger.info("Run %s was cancelled", run_id)

    except Exception as exc:
        error_msg = f"{exc}"
        logger.exception("Run %s failed: %s", run_id, error_msg)
        await run_manager.set_status(run_id, RunStatus.error, error=error_msg)
        await bridge.publish(
            run_id,
            "error",
            {
                "message": error_msg,
                "name": type(exc).__name__,
            },
        )

    finally:
        try:
            await bridge.publish_end(run_id)
            asyncio.create_task(bridge.cleanup(run_id, delay=60))
        finally:
            checkpoint_lock.release()


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


async def _call_checkpointer_method(checkpointer: Any, async_name: str, sync_name: str, *args: Any, **kwargs: Any) -> Any:
    """Call a checkpointer method, supporting async and sync variants."""
    method = getattr(checkpointer, async_name, None) or getattr(checkpointer, sync_name, None)
    if method is None:
        raise AttributeError(f"Missing checkpointer method: {async_name}/{sync_name}")
    result = method(*args, **kwargs)
    if inspect.isawaitable(result):
        return await result
    return result


async def _capture_rollback_point(
    accessor: CheckpointStateAccessor,
    checkpointer: Any,
    read_config: dict[str, Any],
) -> RollbackPoint | None:
    """Capture materialized state; raw delta checkpoints contain sentinels."""
    snapshot = await accessor.aget(read_config)
    snapshot_config = getattr(snapshot, "config", None) or {}
    configurable = snapshot_config.get("configurable") or {}
    if not configurable.get("checkpoint_id"):
        return None
    checkpoint_tuple = await _call_checkpointer_method(
        checkpointer, "aget_tuple", "get_tuple", snapshot_config
    )
    raw_values = getattr(snapshot, "values", None) or {}
    messages = raw_values.get("messages") if isinstance(raw_values, dict) else None
    state_values = (
        copy.deepcopy(
            {key: value for key, value in raw_values.items() if key != "messages"}
        )
        if accessor.mode == "delta" and isinstance(raw_values, dict)
        else {}
    )
    return RollbackPoint(
        config={
            "configurable": {
                "thread_id": configurable.get("thread_id"),
                "checkpoint_ns": configurable.get("checkpoint_ns") or "",
                "checkpoint_id": configurable.get("checkpoint_id"),
            }
        },
        state_values=state_values,
        messages=tuple(messages or ()),
        pending_writes=tuple(
            getattr(checkpoint_tuple, "pending_writes", ()) or ()
        ),
    )


def _complete_state_replacement_values(
    *,
    mutation_graph: Any,
    selected_values: dict[str, Any],
    current_values: dict[str, Any],
    run_id: str,
) -> dict[str, Any]:
    writable_fields = graph_writable_channels(mutation_graph)
    reducer_fields = graph_reducer_channels(mutation_graph)
    if writable_fields is None or reducer_fields is None:
        raise RuntimeError(
            f"Run {run_id} could not inspect the state schema for rollback"
        )
    replacements: dict[str, Any] = {}
    for field_name in writable_fields:
        if field_name in selected_values:
            replacement = copy.deepcopy(selected_values[field_name])
        elif field_name in current_values:
            channel = mutation_graph.channels.get(field_name)
            replacement = (
                copy.deepcopy(channel.get())
                if channel is not None and channel.is_available()
                else None
            )
        else:
            continue
        replacements[field_name] = (
            Overwrite(replacement)
            if field_name in reducer_fields
            else replacement
        )
    return replacements


async def _rollback_to_pre_run_checkpoint(
    *,
    accessor: CheckpointStateAccessor | None,
    checkpointer: Any,
    thread_id: str,
    run_id: str,
    rollback_point: RollbackPoint | None,
    snapshot_capture_failed: bool,
) -> None:
    """Restore pre-run state through a mode-matched state mutation graph."""
    if checkpointer is None:
        logger.info("Run %s rollback requested but no checkpointer is configured", run_id)
        return
    if snapshot_capture_failed:
        logger.warning(
            "Run %s rollback skipped: pre-run checkpoint capture failed", run_id
        )
        return
    if rollback_point is None:
        await _call_checkpointer_method(
            checkpointer, "adelete_thread", "delete_thread", thread_id
        )
        logger.info("Run %s rollback reset thread %s to empty state", run_id, thread_id)
        return
    if accessor is None:
        raise RuntimeError(f"Run {run_id} rollback has no checkpoint state accessor")

    schema = graph_state_schema(accessor.graph)
    if schema is None:
        raise RuntimeError(f"Run {run_id} rollback could not resolve graph state schema")
    mutation_graph = build_state_mutation_graph(
        "rollback_restore", accessor.mode, schema
    )
    mutation_accessor = CheckpointStateAccessor.bind(
        mutation_graph, checkpointer, mode=accessor.mode
    )

    if accessor.mode == "delta":
        restore_config: dict[str, Any] = {
            "configurable": {"thread_id": thread_id, "checkpoint_ns": ""}
        }
        current = await accessor.aget(restore_config)
        raw_current = getattr(current, "values", None) or {}
        current_values = dict(raw_current) if isinstance(raw_current, dict) else {}
        selected_values = copy.deepcopy(rollback_point.state_values)
        selected_values["messages"] = list(rollback_point.messages)
        replacement_values = _complete_state_replacement_values(
            mutation_graph=mutation_graph,
            selected_values=selected_values,
            current_values=current_values,
            run_id=run_id,
        )
    else:
        restore_config = rollback_point.config
        replacement_values = {
            "messages": Overwrite(list(rollback_point.messages))
        }

    restored_config = await mutation_accessor.aupdate(
        restore_config,
        replacement_values,
        as_node="rollback_restore",
    )
    if not isinstance(restored_config, dict) or not (
        restored_config.get("configurable") or {}
    ).get("checkpoint_id"):
        raise RuntimeError(f"Run {run_id} rollback restore returned invalid config")

    writes_by_task: dict[str, list[tuple[str, Any]]] = {}
    for item in rollback_point.pending_writes:
        if not isinstance(item, (tuple, list)) or len(item) != 3:
            raise RuntimeError(
                f"Run {run_id} rollback pending_write is not a 3-tuple: {item!r}"
            )
        task_id, channel, value = item
        if not isinstance(channel, str):
            raise RuntimeError(
                f"Run {run_id} rollback pending_write channel is invalid: {channel!r}"
            )
        writes_by_task.setdefault(str(task_id), []).append((channel, value))
    for task_id, writes in writes_by_task.items():
        await _call_checkpointer_method(
            checkpointer,
            "aput_writes",
            "put_writes",
            restored_config,
            writes,
            task_id=task_id,
        )


def _lg_mode_to_sse_event(mode: str) -> str:
    """Map LangGraph internal stream_mode name to SSE event name.

    LangGraph's ``astream(stream_mode="messages")`` produces message
    tuples.  The AG-UI consumer in ``chat.py`` (and downstream clients)
    filters on the SSE event name ``"messages-tuple"``, so we must
    translate the LangGraph-internal ``"messages"`` mode to that name.
    """
    if mode == "messages":
        return "messages-tuple"
    return mode


def _unpack_stream_item(
    item: Any,
    lg_modes: list[str],
    stream_subgraphs: bool,
) -> tuple[str | None, Any]:
    """Unpack a multi-mode or subgraph stream item into (mode, chunk).

    Returns ``(None, None)`` if the item cannot be parsed.
    """
    if stream_subgraphs:
        if isinstance(item, tuple) and len(item) == 3:
            _ns, mode, chunk = item
            return str(mode), chunk
        if isinstance(item, tuple) and len(item) == 2:
            mode, chunk = item
            return str(mode), chunk
        return None, None

    if isinstance(item, tuple) and len(item) == 2:
        mode, chunk = item
        return str(mode), chunk

    # Fallback: single-element output from first mode
    return lg_modes[0] if lg_modes else None, item
