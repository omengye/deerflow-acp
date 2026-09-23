"""Thread (session) management endpoints."""
import asyncio
import logging
import re
import uuid
from typing import Any

from fastapi import APIRouter, Body, HTTPException, Query
from pydantic import BaseModel, Field

from app.dependencies import get_client_manager
from app.schemas import ThreadResponse, ThreadDetail
from app.thread_cleanup import ThreadCleanupInProgressError

logger = logging.getLogger(__name__)

router = APIRouter(tags=["threads"])

# Conservative thread_id format: UUIDs and bare alphanumerics with `-`/`_`,
# at most 128 chars. Prevents path-traversal / injection through path params.
_THREAD_ID_RE = re.compile(r"^[A-Za-z0-9_-]{1,128}$")
_MAX_LIST_LIMIT = 1000
_CLEANUP_PAGE_SIZE = 500


def _validate_thread_id(thread_id: str) -> None:
    if not thread_id or not _THREAD_ID_RE.match(thread_id):
        raise HTTPException(status_code=400, detail="Invalid thread_id format")


@router.post("/threads", response_model=ThreadResponse)
async def create_thread():
    """Create a new session (returns a fresh UUID)."""
    thread_id = str(uuid.uuid4())
    await get_client_manager().touch_thread_activity(thread_id, source="create")
    return ThreadResponse(thread_id=thread_id)


@router.get("/threads", response_model=list[ThreadResponse])
async def list_threads(limit: int = Query(default=10, ge=1, le=_MAX_LIST_LIMIT)):
    """List recent sessions."""
    manager = get_client_manager()
    client = manager.get_client()

    try:
        result = client.list_threads(limit=limit)
        threads = []
        thread_list = result.get("thread_list", result.get("threads", []))
        for t in thread_list:
            threads.append(ThreadResponse(
                thread_id=t.get("thread_id", ""),
                title=t.get("title"),
                created_at=t.get("created_at"),
            ))
        return threads
    except Exception:
        logger.exception("Unhandled error")
        raise HTTPException(status_code=500, detail="Internal server error")


@router.get("/threads/{thread_id}", response_model=ThreadDetail)
async def get_thread(thread_id: str):
    """Get full session state including message history."""
    _validate_thread_id(thread_id)
    manager = get_client_manager()
    client = manager.get_client()

    try:
        thread = client.get_thread(thread_id)
        if not thread:
            raise HTTPException(status_code=404, detail=f"Thread {thread_id} not found")

        # Support both checkpoint-based and values-based thread formats
        if "checkpoints" in thread:
            # Get the latest checkpoint values
            checkpoints = thread["checkpoints"]
            if checkpoints:
                latest = checkpoints[-1]
                values = latest.get("values", {})
                messages = values.get("messages", [])
                artifacts = values.get("artifacts", [])
                title = values.get("title")
            else:
                messages = []
                artifacts = []
                title = None
        else:
            values = thread.get("values", {})
            messages = values.get("messages", [])
            artifacts = values.get("artifacts", [])
            title = values.get("title")

        serialized = []
        for msg in messages:
            if hasattr(msg, "model_dump"):
                serialized.append(msg.model_dump())
            elif hasattr(msg, "dict"):
                serialized.append(msg.dict())
            elif isinstance(msg, dict):
                serialized.append(msg)
            else:
                serialized.append({"content": str(msg)})

        # Check if currently running
        status = "running" if manager.is_thread_running(thread_id) else "idle"

        return ThreadDetail(
            thread_id=thread_id,
            messages=serialized,
            artifacts=artifacts if isinstance(artifacts, list) else [],
            title=title,
            status=status,
        )
    except HTTPException:
        raise
    except Exception:
        logger.exception("Unhandled error")
        raise HTTPException(status_code=500, detail="Internal server error")


class ThreadUpdateRequest(BaseModel):
    title: str | None = None
    metadata: dict[str, Any] | None = None


@router.put("/threads/{thread_id}")
async def update_thread(thread_id: str, req: ThreadUpdateRequest = Body()):
    """Update session metadata (title, custom tags, etc.)."""
    _validate_thread_id(thread_id)
    manager = get_client_manager()

    metadata = {}
    if req.title:
        metadata["title"] = req.title
    if req.metadata:
        metadata.update(req.metadata)

    if not metadata:
        raise HTTPException(status_code=400, detail="At least one field (title/metadata) is required")

    try:
        await manager.touch_thread_activity(thread_id, source="metadata_update")
    except ThreadCleanupInProgressError as exc:
        raise HTTPException(status_code=409, detail=str(exc)) from exc
    result = await asyncio.to_thread(manager.update_thread_metadata, thread_id, metadata)
    if not result.get("success"):
        raise HTTPException(status_code=500, detail=result.get("detail", "Update failed"))

    return result


@router.delete("/threads/{thread_id}")
async def delete_thread(thread_id: str):
    """Delete a session completely (checkpointer data + file system)."""
    _validate_thread_id(thread_id)
    manager = get_client_manager()

    result = await asyncio.to_thread(manager.delete_thread_completely, thread_id)
    if not result.get("success"):
        if result.get("running"):
            raise HTTPException(status_code=409, detail=result.get("detail", "Thread is running"))
        raise HTTPException(status_code=500, detail=result.get("detail", "Delete failed"))
    return result


class ThreadStatusResponse(BaseModel):
    thread_id: str
    status: str  # idle / running
    title: str | None = None
    metadata: dict[str, Any] | None = None


@router.get("/threads/{thread_id}/status", response_model=ThreadStatusResponse)
async def get_thread_status(thread_id: str):
    """Get session running status."""
    _validate_thread_id(thread_id)
    manager = get_client_manager()
    client = manager.get_client()

    # Check if currently executing
    if manager.is_thread_running(thread_id):
        return ThreadStatusResponse(thread_id=thread_id, status="running")

    # Try to get the latest checkpoint to confirm existence
    try:
        thread = client.get_thread(thread_id)
        title = None
        custom_metadata = None
        if thread.get("checkpoints"):
            latest = thread["checkpoints"][-1]
            title = latest.get("values", {}).get("title")
            # Extract custom metadata fields (non-reserved keys)
            raw_meta = latest.get("metadata", {})
            reserved = {"source", "step", "parents", "ls_integration", "thread_id"}
            custom_metadata = {k: v for k, v in raw_meta.items() if k not in reserved}
            if not custom_metadata:
                custom_metadata = None
        return ThreadStatusResponse(thread_id=thread_id, status="idle", title=title, metadata=custom_metadata)
    except Exception:
        raise HTTPException(status_code=404, detail=f"Thread {thread_id} not found")


class CleanupRequest(BaseModel):
    older_than_days: int = Field(default=7, ge=1, le=3650)
    status_filter: str | None = None  # None = all, "idle" = only idle
    max_pages: int = Field(default=20, ge=1, le=100)  # safety bound before the service cap


@router.post("/threads/cleanup")
async def cleanup_threads(req: CleanupRequest = Body()):
    """Start indexed session cleanup in the background (legacy endpoint)."""
    manager = get_client_manager()
    service = manager.thread_cleanup_service
    if service is None:
        raise HTTPException(status_code=409, detail="Thread cleanup requires the SQLite checkpointer")
    return await service.start_run(
        trigger="legacy_api",
        inactive_days=req.older_than_days,
        limit=min(max(1, req.max_pages) * _CLEANUP_PAGE_SIZE, service.config.max_deletions_per_run),
    )
