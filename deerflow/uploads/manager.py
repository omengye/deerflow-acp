"""Shared upload management logic.

Pure business logic — no FastAPI/HTTP dependencies.
Both Gateway and Client delegate to these functions.
"""

import os
import re
import shutil
from pathlib import Path
from urllib.parse import quote

from deerflow.config.paths import VIRTUAL_PATH_PREFIX, get_paths


class PathTraversalError(ValueError):
    """Raised when a path escapes its allowed base directory."""


# thread_id must be alphanumeric, hyphens, underscores, or dots only.
_SAFE_THREAD_ID = re.compile(r"^[a-zA-Z0-9._-]+$")
_MAX_FILENAME_BYTES = 255


def validate_thread_id(thread_id: str) -> None:
    """Reject thread IDs containing characters unsafe for filesystem paths.

    Raises:
        ValueError: If thread_id is empty or contains unsafe characters.
    """
    if not thread_id or not _SAFE_THREAD_ID.match(thread_id):
        raise ValueError(f"Invalid thread_id: {thread_id!r}")


def get_uploads_dir(thread_id: str) -> Path:
    """Return the uploads directory path for a thread (no side effects)."""
    validate_thread_id(thread_id)
    return get_paths().sandbox_uploads_dir(thread_id)


def ensure_uploads_dir(thread_id: str) -> Path:
    """Return the uploads directory for a thread, creating it if needed."""
    paths = get_paths()
    base = paths.sandbox_uploads_dir(thread_id)
    base.mkdir(parents=True, exist_ok=True)
    current = paths.base_dir
    for part in base.relative_to(paths.base_dir).parts:
        current /= part
        if current.is_symlink():
            raise PathTraversalError("Uploads directory cannot contain symbolic-link components")
    if not base.is_dir():
        raise PathTraversalError("Uploads directory must be a real directory")
    try:
        base.resolve().relative_to(paths.base_dir.resolve())
    except ValueError:
        raise PathTraversalError("Uploads directory escapes the DeerFlow data directory") from None
    return base


def normalize_filename(filename: str) -> str:
    """Sanitize a filename by extracting its basename.

    Strips any directory components and rejects traversal patterns.

    Args:
        filename: Raw filename from user input (may contain path components).

    Returns:
        Safe filename (basename only).

    Raises:
        ValueError: If filename is empty or resolves to a traversal pattern.
    """
    if not filename:
        raise ValueError("Filename is empty")
    safe = Path(filename).name
    if not safe or safe in {".", ".."}:
        raise ValueError(f"Filename is unsafe: {filename!r}")
    # Reject backslashes — on Linux Path.name keeps them as literal chars,
    # but they indicate a Windows-style path that should be stripped or rejected.
    if "\\" in safe:
        raise ValueError(f"Filename contains backslash: {filename!r}")
    if len(safe.encode("utf-8")) > _MAX_FILENAME_BYTES:
        raise ValueError(f"Filename too long: {len(safe)} chars")
    return safe


def _fit_utf8_bytes(text: str, budget: int) -> str:
    """Truncate *text* without splitting a UTF-8 code point."""
    encoded = text.encode("utf-8")
    if len(encoded) <= budget:
        return text
    return encoded[:budget].decode("utf-8", errors="ignore")


def _numbered_filename(name: str, counter: int) -> str:
    """Return a collision suffix variant that still fits a filesystem name."""
    if counter == 0:
        return name

    path = Path(name)
    tag = f"_{counter}"
    tag_size = len(tag.encode("utf-8"))
    if tag_size >= _MAX_FILENAME_BYTES:
        raise ValueError("Filename collision counter is too large")

    suffix = path.suffix
    stem_budget = _MAX_FILENAME_BYTES - tag_size - len(suffix.encode("utf-8"))
    if stem_budget < 1:
        # An unusually long extension leaves no room for a stem. Preserve as
        # much of the original name as possible and put the uniqueness tag at
        # the end rather than returning another invalid filename.
        fitted = _fit_utf8_bytes(path.stem + suffix, _MAX_FILENAME_BYTES - tag_size)
        return f"{fitted}{tag}"
    return f"{_fit_utf8_bytes(path.stem, stem_budget)}{tag}{suffix}"


def claim_unique_filename(name: str, seen: set[str]) -> str:
    """Generate a unique filename by appending ``_N`` suffix on collision.

    Automatically adds the returned name to *seen* so callers don't need to.

    Args:
        name: Candidate filename.
        seen: Set of filenames already claimed (mutated in place).

    Returns:
        A filename not present in *seen* (already added to *seen*).
    """
    if name not in seen:
        seen.add(name)
        return name
    counter = 1
    candidate = _numbered_filename(name, counter)
    while candidate in seen:
        counter += 1
        candidate = _numbered_filename(name, counter)
    seen.add(candidate)
    return candidate


def copy_file_exclusive(source: Path, destination_dir: Path, preferred_name: str | None = None) -> Path:
    """Copy *source* without overwriting or following an existing destination.

    The destination is created with exclusive-create semantics (``xb``). This
    makes the final filesystem operation authoritative even when another upload
    process races with filename selection, and an existing symbolic link is
    treated as a collision rather than followed. A numbered filename is retried
    until one can be claimed.
    """
    source = Path(source)
    safe_name = normalize_filename(preferred_name or source.name)
    if destination_dir.is_symlink() or not destination_dir.is_dir():
        raise PathTraversalError("Upload destination must be a real directory")

    counter = 0
    while True:
        destination = destination_dir / _numbered_filename(safe_name, counter)
        try:
            output = destination.open("xb")
        except FileExistsError:
            counter += 1
            continue

        try:
            with output, source.open("rb") as input_file:
                shutil.copyfileobj(input_file, output, length=64 * 1024)
        except BaseException:
            destination.unlink(missing_ok=True)
            raise
        return destination


def write_text_file_exclusive(destination_dir: Path, preferred_name: str, text: str) -> Path:
    """Write UTF-8 text using the same collision/symlink guarantees as uploads."""
    safe_name = normalize_filename(preferred_name)
    if destination_dir.is_symlink() or not destination_dir.is_dir():
        raise PathTraversalError("Upload destination must be a real directory")

    counter = 0
    while True:
        destination = destination_dir / _numbered_filename(safe_name, counter)
        try:
            with destination.open("x", encoding="utf-8", newline="") as output:
                output.write(text)
        except FileExistsError:
            counter += 1
            continue
        except BaseException:
            destination.unlink(missing_ok=True)
            raise
        return destination


def validate_path_traversal(path: Path, base: Path) -> None:
    """Verify that *path* is inside *base*.

    Raises:
        PathTraversalError: If a path traversal is detected.
    """
    try:
        path.resolve().relative_to(base.resolve())
    except ValueError:
        raise PathTraversalError("Path traversal detected") from None


def list_files_in_dir(directory: Path) -> dict:
    """List files (not directories) in *directory*.

    Args:
        directory: Directory to scan.

    Returns:
        Dict with "files" list (sorted by name) and "count".
        Each file entry has ``size`` as *int* (bytes).  Call
        :func:`enrich_file_listing` to stringify sizes and add
        virtual / artifact URLs.
    """
    if not directory.is_dir():
        return {"files": [], "count": 0}

    files = []
    with os.scandir(directory) as entries:
        for entry in sorted(entries, key=lambda e: e.name):
            if not entry.is_file(follow_symlinks=False):
                continue
            st = entry.stat(follow_symlinks=False)
            files.append(
                {
                    "filename": entry.name,
                    "size": st.st_size,
                    "path": entry.path,
                    "extension": Path(entry.name).suffix,
                    "modified": st.st_mtime,
                }
            )
    return {"files": files, "count": len(files)}


def delete_file_safe(base_dir: Path, filename: str, *, convertible_extensions: set[str] | None = None) -> dict:
    """Delete a file inside *base_dir* after path-traversal validation.

    If *convertible_extensions* is provided and the file's extension matches,
    the companion ``.md`` file is also removed (if it exists).

    Args:
        base_dir: Directory containing the file.
        filename: Name of file to delete.
        convertible_extensions: Lowercase extensions (e.g. ``{".pdf", ".docx"}``)
            whose companion markdown should be cleaned up.

    Returns:
        Dict with success and message.

    Raises:
        FileNotFoundError: If the file does not exist.
        PathTraversalError: If path traversal is detected.
    """
    # Validate the resolved target, but unlink the requested entry itself.
    # Resolving before unlink would turn an in-directory symlink into a delete
    # of the upload it aliases (including that upload's converted companion).
    file_path = base_dir / filename
    validate_path_traversal(file_path, base_dir)

    if file_path.is_symlink() or not file_path.is_file():
        raise FileNotFoundError(f"File not found: {filename}")

    file_path.unlink()

    # Clean up companion markdown generated during upload conversion.
    if convertible_extensions and file_path.suffix.lower() in convertible_extensions:
        file_path.with_suffix(".md").unlink(missing_ok=True)

    return {"success": True, "message": f"Deleted {filename}"}


def upload_artifact_url(thread_id: str, filename: str) -> str:
    """Build the artifact URL for a file in a thread's uploads directory.

    *filename* is percent-encoded so that spaces, ``#``, ``?`` etc. are safe.
    """
    return f"/api/threads/{thread_id}/artifacts{VIRTUAL_PATH_PREFIX}/uploads/{quote(filename, safe='')}"


def upload_virtual_path(filename: str) -> str:
    """Build the virtual path for a file in the uploads directory."""
    return f"{VIRTUAL_PATH_PREFIX}/uploads/{filename}"


def enrich_file_listing(result: dict, thread_id: str) -> dict:
    """Add virtual paths, artifact URLs, and stringify sizes on a listing result.

    Mutates *result* in place and returns it for convenience.
    """
    for f in result["files"]:
        filename = f["filename"]
        f["size"] = str(f["size"])
        f["virtual_path"] = upload_virtual_path(filename)
        f["artifact_url"] = upload_artifact_url(thread_id, filename)
    return result
