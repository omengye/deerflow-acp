"""Bounded, resumable filesystem reads shared by local and remote sandboxes.

Only the standard library is used: the same implementation is sent to the
container's Python interpreter instead of maintaining a second shell algorithm.
"""

from __future__ import annotations

import base64
import codecs
import fnmatch
import hashlib
import heapq
import json
import os
import stat as stat_types
from dataclasses import dataclass
from pathlib import Path

MAX_READ_BYTES = 1024 * 1024
MAX_DIRECTORY_PAGE = 1000


class FileVersionMismatchError(OSError):
    """The file or directory changed; continuation must restart."""


@dataclass(frozen=True)
class FileChunk:
    content: str
    path: str
    offset: int
    next_offset: int | None
    bytes_read: int
    size: int
    version: str
    truncated: bool
    end_line: int | None = None
    # Providers may project application-written paths for editing/display.
    # Source bytes and continuation offsets always describe ``content``.
    display_content: str | None = None


@dataclass(frozen=True)
class DirectoryPage:
    entries: list[str]
    next_cursor: str | None
    truncated: bool
    version: str


def _version(stat: os.stat_result) -> str:
    # Windows fstat and stat expose different creation/change-time semantics
    # on some Python/filesystem combinations. mtime remains comparable; inode
    # identity and size still detect replacements and truncations there.
    identity = (stat.st_dev, stat.st_ino, stat.st_size, stat.st_mtime_ns,
                stat.st_ctime_ns if os.name != "nt" else None)
    return hashlib.sha256(repr(identity).encode()).hexdigest()


def _seek_line(handle, line: int) -> None:
    current = 1
    while current < line:
        part = handle.readline(65536)
        if not part:
            break
        current += part.count(b"\n")


def _line_at_offset(handle, offset: int) -> int:
    handle.seek(0)
    remaining = offset
    line = 1
    while remaining:
        part = handle.read(min(65536, remaining))
        if not part:
            break
        line += part.count(b"\n")
        remaining -= len(part)
    return line


def read_file_chunk(
    path: str,
    *,
    offset: int = 0,
    max_bytes: int = 65536,
    expected_version: str | None = None,
    start_line: int | None = None,
    end_line: int | None = None,
) -> FileChunk:
    """Read at most ``max_bytes`` while preserving UTF-8 continuation boundaries.

    Offsets refer to source bytes, including CRLF bytes. ``end_line`` may be
    repeated on continuation calls. Memory use does not depend on file/line size.
    The version identifies an opened file's inode, size and modification times;
    this is optimistic change detection, not a transactional filesystem snapshot.
    """
    if offset < 0:
        raise ValueError("offset must be >= 0")
    if not 4 <= max_bytes <= MAX_READ_BYTES:
        raise ValueError(f"max_bytes must be between 4 and {MAX_READ_BYTES}")
    if start_line is not None and start_line < 1:
        raise ValueError("start_line must be >= 1")
    if end_line is not None and end_line < 1:
        raise ValueError("end_line must be >= 1")
    if offset and start_line is not None:
        raise ValueError("Use offset or start_line, not both")
    if end_line is not None and (start_line or 1) > end_line:
        raise ValueError("start_line must not exceed end_line")
    if stat_types.S_ISDIR(os.stat(path).st_mode):
        # Windows open(directory, 'rb') reports EACCES, unlike POSIX EISDIR.
        raise IsADirectoryError(path)
    with open(path, "rb") as handle:
        before = os.fstat(handle.fileno())
        version = _version(before)
        if expected_version is not None and expected_version != version:
            raise FileVersionMismatchError("File changed; restart reading from offset=0")
        if offset > before.st_size:
            raise ValueError("offset exceeds file size")
        if offset:
            line = _line_at_offset(handle, offset) if end_line is not None else 1
            handle.seek(offset)
        else:
            _seek_line(handle, start_line or 1)
            line = start_line or 1
        actual_offset = handle.tell()
        raw = handle.read(max_bytes)
        range_complete = end_line is not None and line > end_line
        if range_complete:
            raw = b""
        elif end_line is not None:
            remaining_lines = end_line - line + 1
            position = 0
            for _ in range(remaining_lines):
                newline = raw.find(b"\n", position)
                if newline < 0:
                    break
                position = newline + 1
            else:
                raw = raw[:position]
                range_complete = True
        at_eof = actual_offset + len(raw) >= before.st_size
        if not at_eof and not range_complete:
            # Prefer a complete-line page. Byte continuation remains available
            # for a single line larger than the budget, including UTF-8 text.
            last_newline = raw.rfind(b"\n")
            if last_newline >= 0:
                raw = raw[:last_newline + 1]
            elif raw.endswith(b"\r"):
                # Keep CRLF together so text clients may normalize newlines
                # without a dangling CR at a page boundary.
                raw = raw[:-1]
        decoder = codecs.getincrementaldecoder("utf-8")(errors="replace")
        content = decoder.decode(raw, final=at_eof or range_complete)
        pending, _ = decoder.getstate()
        consumed = len(raw) - len(pending)
        next_offset = actual_offset + consumed
        if _version(os.fstat(handle.fileno())) != version or _version(os.stat(path)) != version:
            raise FileVersionMismatchError("File changed during reading; restart from offset=0")
    more = not range_complete and next_offset < before.st_size
    return FileChunk(content, str(path), actual_offset, next_offset if more else None,
                     consumed, before.st_size, version, more, end_line)


def _decode_cursor(cursor: str) -> dict:
    try:
        if len(cursor) > 16384:
            raise ValueError
        value = json.loads(base64.urlsafe_b64decode(cursor.encode()).decode())
        if not isinstance(value, dict) or value.get("v") != 1:
            raise ValueError
        return value
    except (ValueError, TypeError, UnicodeError) as exc:
        raise ValueError("Invalid directory cursor") from exc


def list_dir_page(
    path: str,
    *,
    max_depth: int = 2,
    limit: int = 200,
    cursor: str | None = None,
    ignore_patterns: tuple[str, ...] | list[str] = (),
) -> DirectoryPage:
    """Select a stable page with O(limit + depth) retained traversal state.

    Each page scans the requested tree to select the next lexicographic paths
    and check its version; it never stores the entire tree. Symlink directories
    are listed when they stay within the root, but are never traversed.
    """
    if not 1 <= limit <= MAX_DIRECTORY_PAGE:
        raise ValueError(f"limit must be between 1 and {MAX_DIRECTORY_PAGE}")
    if not 1 <= max_depth <= 32:
        raise ValueError("max_depth must be between 1 and 32")
    root = Path(path).resolve(strict=True)
    if not root.is_dir():
        raise NotADirectoryError(path)
    identity = hashlib.sha256(json.dumps({"root": str(root), "depth": max_depth, "ignore": list(ignore_patterns)}, sort_keys=True).encode()).hexdigest()
    prior = _decode_cursor(cursor) if cursor else None
    if prior is not None and (prior.get("query") != identity or not isinstance(prior.get("after"), str)):
        raise ValueError("Directory cursor belongs to a different query")
    after = prior["after"] if prior else ""
    fingerprint = int.from_bytes(hashlib.sha256((str(root) + _version(root.stat())).encode()).digest(), "big")

    def walk(directory: Path, depth: int):
        nonlocal fingerprint
        before = _version(directory.stat())
        with os.scandir(directory) as entries:
            for entry in entries:
                if any(fnmatch.fnmatchcase(entry.name, pattern) for pattern in ignore_patterns):
                    continue
                item = Path(entry.path)
                # Windows DirEntry caches enumeration metadata which can
                # lag behind a just-completed child write. Use a fresh
                # lstat for stable identities and version checks.
                try:
                    stat = item.lstat()
                    # A dangling link is itself a valid directory entry. Do
                    # not mistake its absent target for a missing root.
                    target = item.resolve(strict=not item.is_symlink())
                except FileNotFoundError as exc:
                    raise FileVersionMismatchError("Directory changed during listing; restart without cursor") from exc
                relative = item.relative_to(root).as_posix()
                fingerprint ^= int.from_bytes(hashlib.sha256((relative + _version(stat)).encode()).digest(), "big")
                if not target.is_relative_to(root):
                    continue
                is_dir = target.is_dir()
                key = relative + ("/" if is_dir else "")
                if key > after:
                    yield key, str(item) + ("/" if is_dir else "")
                if is_dir and not item.is_symlink() and depth < max_depth:
                    yield from walk(item, depth + 1)
        if _version(directory.stat()) != before:
            raise FileVersionMismatchError("Directory changed during listing; restart without cursor")

    selected = heapq.nsmallest(limit + 1, walk(root, 1), key=lambda item: item[0])
    version = f"{fingerprint:064x}"
    if prior is not None and prior.get("version") != version:
        raise FileVersionMismatchError("Directory changed; restart listing without cursor")
    more = len(selected) > limit
    selected = selected[:limit]
    next_cursor = None
    if more:
        payload = {"v": 1, "query": identity, "after": selected[-1][0], "version": version}
        next_cursor = base64.urlsafe_b64encode(json.dumps(payload, separators=(",", ":")).encode()).decode()
    return DirectoryPage([item[1] for item in selected], next_cursor, more, version)
