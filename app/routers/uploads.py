"""File upload and artifact endpoints."""
import hashlib
import logging
import tempfile
from pathlib import Path
from urllib.parse import quote

from fastapi import APIRouter, File, HTTPException, UploadFile
from fastapi.responses import Response

from app.config import settings
from app.dependencies import get_client_manager
from app.thread_cleanup import ThreadCleanupInProgressError
from deerflow.uploads.manager import PathTraversalError, claim_unique_filename, normalize_filename

logger = logging.getLogger(__name__)

router = APIRouter(tags=["uploads"])

# 64 KiB streaming chunks — bounds peak memory while copying uploads.
_UPLOAD_COPY_CHUNK = 64 * 1024

# Browsers render these formats as active documents.  Serving an agent-created
# artifact inline on the API origin would let markup execute with that origin's
# privileges, so these types are always downloads even without ?download=true.
_ACTIVE_CONTENT_MIME_TYPES = {
    "text/html",
    "application/xhtml+xml",
    "image/svg+xml",
    "text/xml",
    "application/xml",
    "text/xsl",
}


def _is_active_content_mime_type(mime_type: str | None) -> bool:
    if not isinstance(mime_type, str):
        return False
    normalized = mime_type.split(";", 1)[0].strip().lower()
    return normalized in _ACTIVE_CONTENT_MIME_TYPES or normalized.endswith("+xml")


def _content_disposition(filename: str) -> str:
    """Build an RFC 6266-compliant Content-Disposition header for downloads.

    Falls back to an ASCII-only filename plus a UTF-8 ``filename*`` so clients
    that ignore RFC 5987 still get a usable name without raising encoding
    errors when the response is serialized.
    """
    ascii_name = filename.encode("ascii", "replace").decode("ascii").replace('"', "_")
    return f'attachment; filename="{ascii_name}"; filename*=UTF-8\'\'{quote(filename)}'


def _normalize_extension_set(raw: list[str]) -> set[str]:
    """Lowercase + ensure leading dot for the configured extension whitelist."""
    out: set[str] = set()
    for item in raw:
        ext = item.strip().lower()
        if not ext:
            continue
        if not ext.startswith("."):
            ext = "." + ext
        out.add(ext)
    return out


def _check_extension_allowed(filename: str) -> None:
    allowed = _normalize_extension_set(settings.allowed_upload_extensions)
    if not allowed:
        return
    suffix = Path(filename).suffix.lower()
    if suffix not in allowed:
        raise HTTPException(
            status_code=415,
            detail=f"File type {suffix or '<none>'} is not allowed",
        )


def _stream_copy_with_limit(src, dst, *, max_bytes: int, filename: str) -> None:
    """Copy ``src`` to ``dst`` while enforcing a per-file size cap.

    Uses ``read``/``write`` in fixed chunks so we never load whole files into
    memory.  Raises 413 the moment the cap is exceeded; callers run inside a
    ``TemporaryDirectory`` so any partial bytes on disk are cleaned up.
    """
    written = 0
    while True:
        chunk = src.read(_UPLOAD_COPY_CHUNK)
        if not chunk:
            return
        written += len(chunk)
        if written > max_bytes:
            raise HTTPException(
                status_code=413,
                detail=(
                    f"File '{filename}' exceeds the per-file size limit of "
                    f"{settings.max_upload_size_mb} MB"
                ),
            )
        dst.write(chunk)


@router.post("/threads/{thread_id}/uploads")
async def upload_files(thread_id: str, files: list[UploadFile] = File(...)):
    """Upload files to a thread. Auto-converts documents to Markdown."""
    if not files:
        raise HTTPException(status_code=400, detail="No files provided")

    if len(files) > settings.max_uploads_per_request:
        raise HTTPException(
            status_code=413,
            detail=(
                f"Too many files in one request "
                f"(limit: {settings.max_uploads_per_request})"
            ),
        )

    max_bytes = settings.max_upload_size_mb * 1024 * 1024
    manager = get_client_manager()
    client = manager.get_client()

    try:
        with tempfile.TemporaryDirectory(prefix="deerflow-upload-") as tmpdir:
            tmp_paths: list[Path] = []
            seen_names: set[str] = set()

            for upload in files:
                # Accept the normalized filename so harmless characters
                # (whitespace, UTF-8) do not cause spurious 400s.
                filename = normalize_filename(upload.filename or "")
                if not filename:
                    raise HTTPException(status_code=400, detail=f"Invalid filename: {upload.filename!r}")

                _check_extension_allowed(filename)

                tmp_name = claim_unique_filename(filename, seen_names)
                tmp_path = Path(tmpdir) / tmp_name
                with open(tmp_path, "wb") as out:
                    _stream_copy_with_limit(
                        upload.file, out, max_bytes=max_bytes, filename=filename
                    )
                tmp_paths.append(tmp_path)

            await manager.touch_thread_activity(thread_id, source="upload")
            result = client.upload_files(thread_id, tmp_paths)
            return result
    except HTTPException:
        raise
    except (PathTraversalError, ValueError) as e:
        raise HTTPException(status_code=400, detail=str(e))
    except FileNotFoundError as e:
        raise HTTPException(status_code=404, detail=str(e))
    except ThreadCleanupInProgressError as e:
        raise HTTPException(status_code=409, detail=str(e)) from e
    except Exception:
        logger.exception("Unhandled error in upload_files (thread=%s)", thread_id)
        raise HTTPException(status_code=500, detail="Internal server error")


@router.get("/threads/{thread_id}/uploads")
async def list_uploads(thread_id: str):
    """List uploaded files for a thread."""
    manager = get_client_manager()
    client = manager.get_client()

    try:
        return client.list_uploads(thread_id)
    except (PathTraversalError, ValueError) as e:
        raise HTTPException(status_code=400, detail=str(e))
    except Exception:
        logger.exception("Unhandled error in list_uploads (thread=%s)", thread_id)
        raise HTTPException(status_code=500, detail="Internal server error")


@router.delete("/threads/{thread_id}/uploads/{filename}")
async def delete_upload(thread_id: str, filename: str):
    """Delete an uploaded file."""
    manager = get_client_manager()
    client = manager.get_client()

    try:
        await manager.touch_thread_activity(thread_id, source="upload_delete")
        client.delete_upload(thread_id, filename)
        return {"success": True, "message": f"Deleted {filename}"}
    except FileNotFoundError as e:
        raise HTTPException(status_code=404, detail=str(e))
    except (PathTraversalError, ValueError) as e:
        raise HTTPException(status_code=400, detail=str(e))
    except ThreadCleanupInProgressError as e:
        raise HTTPException(status_code=409, detail=str(e)) from e
    except Exception:
        logger.exception("Unhandled error in delete_upload (thread=%s, filename=%s)", thread_id, filename)
        raise HTTPException(status_code=500, detail="Internal server error")


@router.get("/threads/{thread_id}/artifacts/{path:path}")
async def get_artifact(thread_id: str, path: str, download: bool = False):
    """Download/view an artifact file produced by the agent."""
    manager = get_client_manager()
    client = manager.get_client()

    try:
        content, mime_type = client.get_artifact(thread_id, path)
        # The API already has the artifact bytes in memory, so expose a
        # content-addressed revision without another filesystem read.
        headers = {"ETag": f'"{hashlib.sha256(content).hexdigest()}"'}
        if download or _is_active_content_mime_type(mime_type):
            headers["Content-Disposition"] = _content_disposition(Path(path).name)
        return Response(content=content, media_type=mime_type, headers=headers)
    except FileNotFoundError as e:
        raise HTTPException(status_code=404, detail=str(e))
    except (PathTraversalError, ValueError) as e:
        raise HTTPException(status_code=400, detail=str(e))
    except Exception:
        logger.exception("Unhandled error in get_artifact (thread=%s, path=%s)", thread_id, path)
        raise HTTPException(status_code=500, detail="Internal server error")
