"""Built-in tool for on-demand discovery of uploaded files.

The uploads middleware only injects metadata for files attached to the current
message.  Files uploaded earlier in the thread are announced as a bare name
list, so the model uses this tool to fetch their details when a question
actually calls for them.  A bare listing call (no ``filename``) returns only
name/size/path for every file -- document outlines are extracted only for the
one file the model names, so a listing call never pays the outline-extraction
cost for files that turn out to be irrelevant.  Keeping outlines out of both
the per-turn prompt and the plain listing is what stops upload metadata from
growing with ``file count x conversation turns``.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from langchain.tools import ToolRuntime, tool
from langgraph.config import get_config

from deerflow.agents.thread_state import AgentContext, ThreadState
from deerflow.config.paths import get_paths

# Cap on how many files a single listing may describe.  A thread can accumulate
# many uploads; without a cap the tool result would reintroduce the unbounded
# payload this tool exists to avoid.
_MAX_LISTED_FILES = 50


def _get_thread_id(runtime: ToolRuntime[AgentContext, ThreadState] | None) -> str | None:
    """Resolve the current thread id from runtime context or RunnableConfig."""
    if runtime is not None:
        if runtime.context and runtime.context.get("thread_id"):
            return runtime.context.get("thread_id")
        runtime_config = getattr(runtime, "config", None) or {}
        thread_id = runtime_config.get("configurable", {}).get("thread_id")
        if thread_id:
            return thread_id
    try:
        return get_config().get("configurable", {}).get("thread_id")
    except Exception:
        return None


def _format_size(size_bytes: int) -> str:
    """Render a byte count the same way the uploads middleware does."""
    size_kb = size_bytes / 1024
    return f"{size_kb:.1f} KB" if size_kb < 1024 else f"{size_kb / 1024:.1f} MB"


def _describe_file(file_path: Path, *, include_outline: bool) -> dict[str, Any]:
    """Build a metadata entry for one uploaded file."""
    from deerflow.agents.middlewares.uploads_middleware import converted_markdown_name, extract_outline_for_file

    stat = file_path.stat()
    entry: dict[str, Any] = {
        "filename": file_path.name,
        "size": _format_size(stat.st_size),
        "path": f"/mnt/user-data/uploads/{file_path.name}",
    }

    # Outline line numbers index the converted Markdown, so the model needs that
    # path to use them with read_file.
    md_name = converted_markdown_name(file_path)
    if md_name:
        entry["markdown_path"] = f"/mnt/user-data/uploads/{md_name}"
        entry["read_this"] = entry["markdown_path"]

    if not include_outline:
        return entry

    outline, preview = extract_outline_for_file(file_path)
    if outline:
        visible = [e for e in outline if not e.get("truncated")]
        entry["outline"] = [{"line": e["line"], "title": e["title"]} for e in visible]
        entry["outline_refers_to"] = entry.get("markdown_path", entry["path"])
        if outline[-1].get("truncated"):
            entry["outline_truncated"] = True
            entry["hint"] = f"Showing first {len(visible)} headings; use read_file with line ranges to explore further."
    elif preview:
        entry["outline"] = []
        entry["begins_with"] = preview
        entry["hint"] = "No structural headings detected. Use grep to search for keywords."
    else:
        entry["outline"] = []
        entry["hint"] = "No converted Markdown available. Use read_file or grep directly on the file."

    return entry


@tool("list_uploaded_files", parse_docstring=True)
def list_uploaded_files_tool(
    runtime: ToolRuntime[AgentContext, ThreadState],
    filename: str | None = None,
) -> str:
    """List files the user uploaded in this conversation, with optional document outlines.

    Files attached to the current message are already described in the
    `<uploaded_files>` block. Use this tool to inspect files uploaded in *earlier*
    messages, which are listed there by name only.

    When to use this tool:
    - The user refers to a file uploaded earlier and you need its path or structure
    - You want a document outline to pick which sections to `read_file`
    - You need to confirm which files are still available

    When NOT to use this tool:
    - The file is already described in the current `<uploaded_files>` block
    - You already know the path and just want the contents (use `read_file`)

    Args:
        filename: Inspect one file and return its full document outline. Omit to
            list every uploaded file's name, size, and path only -- pass a
            filename afterwards to get that file's outline.
    """
    thread_id = _get_thread_id(runtime)
    if not thread_id:
        return json.dumps({"error": "No active conversation thread; uploaded files are unavailable."}, ensure_ascii=False)

    try:
        uploads_dir = get_paths().sandbox_uploads_dir(thread_id)
    except ValueError as exc:
        return json.dumps({"error": str(exc)}, ensure_ascii=False)

    current_run_filenames: set[str] = set()
    state = getattr(runtime, "state", None) if runtime is not None else None
    uploaded_files = state.get("uploaded_files") if isinstance(state, dict) else None
    if isinstance(uploaded_files, list):
        current_run_filenames = {
            entry["filename"]
            for entry in uploaded_files
            if isinstance(entry, dict) and isinstance(entry.get("filename"), str)
        }

    if filename is not None:
        # Reject path components so a crafted name cannot escape the uploads dir.
        safe_name = Path(filename).name
        if not safe_name or safe_name != filename:
            return json.dumps({"error": f"Invalid filename: {filename!r}"}, ensure_ascii=False)
        if safe_name in current_run_filenames:
            return json.dumps({"error": f"File belongs to the current upload batch: {safe_name}"}, ensure_ascii=False)

        target = uploads_dir / safe_name
        if not target.is_file():
            return json.dumps({"error": f"Uploaded file not found: {safe_name}"}, ensure_ascii=False)

        return json.dumps({"file": _describe_file(target, include_outline=True)}, ensure_ascii=False, indent=2)

    if not uploads_dir.is_dir():
        return json.dumps({"files": [], "count": 0}, ensure_ascii=False)

    paths = sorted(
        (p for p in uploads_dir.iterdir() if p.is_file() and p.name not in current_run_filenames),
        key=lambda p: p.name,
    )
    # Metadata only here -- outline extraction is opt-in per file (below) so a
    # bare listing call never re-extracts outlines for every historical file.
    files = [_describe_file(p, include_outline=False) for p in paths[:_MAX_LISTED_FILES]]

    result: dict[str, Any] = {"files": files, "count": len(files)}
    if len(paths) > _MAX_LISTED_FILES:
        result["truncated"] = True
        result["total"] = len(paths)
        result["hint"] = (
            f"Showing {_MAX_LISTED_FILES} of {len(paths)} files (metadata only); "
            "call with a filename to inspect a specific one and see its outline."
        )
    else:
        result["hint"] = "Metadata only. Call with a filename to get that file's document outline."

    return json.dumps(result, ensure_ascii=False, indent=2)
