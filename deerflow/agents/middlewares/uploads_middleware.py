"""Middleware to inject uploaded files information into agent context.

Only files attached to the *current* message get full metadata (size, path, and
document outline).  Files uploaded earlier in the thread are announced as a bare
name list; the model calls ``list_uploaded_files`` to fetch their details on
demand.  Injecting every historical outline on every turn made upload metadata
grow with ``file count x conversation turns``.

The block is injected through ``wrap_model_call`` rather than ``before_agent``
so it never enters the persisted message history.  Writing it back to state (as
this middleware previously did, reusing the message id) checkpointed one block
per turn, so a long thread accumulated many near-duplicate blocks in the
history that the model then had to re-read on every subsequent request.
"""

import logging
import re
from collections.abc import Awaitable, Callable
from pathlib import Path
from typing import NotRequired, override

from langchain.agents import AgentState
from langchain.agents.middleware import AgentMiddleware
from langchain.agents.middleware.types import ModelCallResult, ModelRequest, ModelResponse
from langchain_core.messages import HumanMessage
from langgraph.runtime import Runtime

from deerflow.agents.image_inputs import (
    INPUT_IMAGES_KEY,
    normalize_input_image_metadata,
)
from deerflow.config.paths import Paths, get_paths
from deerflow.utils.file_conversion import CONVERTIBLE_EXTENSIONS, extract_outline

logger = logging.getLogger(__name__)


_OUTLINE_PREVIEW_LINES = 5

# Historical files are listed by name only, but a thread can still accumulate
# more uploads than is useful to name in every prompt.  Beyond this many, the
# list is truncated and the model is pointed at ``list_uploaded_files``.
_MAX_HISTORICAL_NAMES = 30

# Matches a whole injected block, including blocks left in the persisted history
# by the older before_agent implementation.
_UPLOAD_BLOCK_RE = re.compile(r"<uploaded_files>[\s\S]*?</uploaded_files>\n*", re.IGNORECASE)


def extract_outline_for_file(file_path: Path) -> tuple[list[dict], list[str]]:
    """Return the document outline and fallback preview for *file_path*.

    Looks for a sibling ``<stem>.md`` file produced by the upload conversion
    pipeline.  For a file that is already Markdown, ``with_suffix(".md")``
    resolves to the file itself, so its own headings are used.

    Returns:
        (outline, preview) where:
        - outline: list of ``{title, line}`` dicts (plus optional sentinel).
          Empty when no headings are found or no .md exists.
        - preview: first few non-empty lines of the .md, used as a content
          anchor when outline is empty so the agent has some context.
          Empty when outline is non-empty (no fallback needed).
    """
    md_path = file_path.with_suffix(".md")
    if not md_path.is_file():
        return [], []

    outline = extract_outline(md_path)
    if outline:
        logger.debug("Extracted %d outline entries from %s", len(outline), file_path.name)
        return outline, []

    # outline is empty — read the first few non-empty lines as a content preview
    preview: list[str] = []
    try:
        with md_path.open(encoding="utf-8") as f:
            for line in f:
                stripped = line.strip()
                if stripped:
                    preview.append(stripped)
                if len(preview) >= _OUTLINE_PREVIEW_LINES:
                    break
    except Exception:
        logger.debug("Failed to read preview lines from %s", md_path, exc_info=True)
    return [], preview


def _strip_upload_block_text(text: str) -> str:
    """Remove any ``<uploaded_files>...</uploaded_files>`` blocks from *text*."""
    return _UPLOAD_BLOCK_RE.sub("", text).strip()


def _strip_upload_blocks_from_content(content: str | list) -> tuple[str | list, bool]:
    """Return *content* with any persisted ``<uploaded_files>`` blocks removed.

    Handles both plain-string content and multimodal (list) content. For list
    content, only ``type == "text"`` blocks are inspected; other blocks (e.g.
    images) are kept exactly as-is. A text block that becomes empty once its
    block is stripped is dropped entirely rather than left as a hollow ``""``
    text element.

    Returns ``(new_content, changed)`` — ``changed`` is False when nothing
    needed stripping, in which case ``new_content`` is *content* unchanged.
    """
    if isinstance(content, str):
        if "<uploaded_files>" not in content:
            return content, False
        stripped = _strip_upload_block_text(content)
        return (stripped, True) if stripped != content else (content, False)

    if isinstance(content, list):
        new_blocks: list = []
        changed = False
        for block in content:
            if isinstance(block, dict) and block.get("type") == "text":
                text = block.get("text")
                if isinstance(text, str) and "<uploaded_files>" in text:
                    stripped_text = _strip_upload_block_text(text)
                    changed = True
                    if stripped_text:
                        new_blocks.append({**block, "text": stripped_text})
                    continue  # empty after stripping -- drop the block
            new_blocks.append(block)
        return (new_blocks, True) if changed else (content, False)

    return content, False


def _strip_persisted_blocks(messages: list) -> list:
    """Drop ``<uploaded_files>`` blocks already baked into the message history.

    Threads created before injection moved to ``wrap_model_call`` have one block
    checkpointed per turn.  Those stale copies would otherwise be re-sent on
    every request forever, so they are removed from the model-bound view.  The
    persisted checkpoint is left untouched; only what the model sees is cleaned.

    Covers multimodal (list) content too: the older ``before_agent``
    implementation could inject the block into an image+text message just as
    readily as a plain-text one, so a thread with such a message would
    otherwise keep re-sending that stale block on every request forever.
    """
    cleaned: list | None = None

    for index, message in enumerate(messages):
        if not isinstance(message, HumanMessage):
            continue

        new_content, changed = _strip_upload_blocks_from_content(message.content)
        if not changed:
            continue

        if cleaned is None:
            cleaned = list(messages)
        cleaned[index] = HumanMessage(
            content=new_content,
            id=message.id,
            additional_kwargs=message.additional_kwargs,
        )

    return cleaned if cleaned is not None else messages


def _last_human_message_index(messages: list) -> int | None:
    """Return the index of the most recent ``HumanMessage`` in *messages*, if any.

    Searches from the end rather than assuming the last message is human: mid-turn
    the model is called repeatedly with tool results appended, and after an
    interrupted-then-resumed turn the tail of state can be a ``ToolMessage``
    rather than the human turn that started it.
    """
    return next(
        (i for i in range(len(messages) - 1, -1, -1) if isinstance(messages[i], HumanMessage)),
        None,
    )


def converted_markdown_name(file_path: Path) -> str | None:
    """Return the sibling ``.md`` filename for *file_path*, if one was generated.

    Outline line numbers refer to the converted Markdown, not the original
    binary document, so callers must show this path alongside the outline for
    the line numbers to be usable with ``read_file``.  Returns ``None`` for a
    file that is already Markdown (the outline refers to itself) or when no
    conversion output exists.
    """
    md_path = file_path.with_suffix(".md")
    if md_path == file_path or not md_path.is_file():
        return None
    return md_path.name


def _is_conversion_artifact(file_path: Path, names: set[str]) -> bool:
    """Return True when *file_path* is a ``.md`` produced from a sibling upload.

    ``convert_file_to_markdown`` writes its output next to the source file as
    ``<stem>.md``, so a converted PDF leaves both ``report.pdf`` and
    ``report.md`` in the uploads directory.  Listing both doubles the injected
    metadata and repeats the same outline twice, so the derived ``.md`` is
    hidden whenever its source document is present.  A ``.md`` the user
    uploaded directly has no such sibling and is still listed.
    """
    if file_path.suffix.lower() != ".md":
        return False
    stem = file_path.stem
    return any(f"{stem}{ext}" in names for ext in CONVERTIBLE_EXTENSIONS)


class UploadsMiddlewareState(AgentState):
    """State schema for uploads middleware."""

    uploaded_files: NotRequired[list[dict] | None]


class UploadsMiddleware(AgentMiddleware[UploadsMiddlewareState]):
    """Middleware to inject uploaded files information into the agent context.

    Reads file metadata from the current message's additional_kwargs.files
    (set by the frontend after upload) and prepends an <uploaded_files> block
    to the last human message so the model knows which files are available.

    Files attached to the current message are described in full (size, path,
    outline).  Earlier uploads are named only; the model calls
    ``list_uploaded_files`` for their details.  The block is applied per model
    call and is not written back to the message history.
    """

    state_schema = UploadsMiddlewareState

    def __init__(self, base_dir: str | None = None):
        """Initialize the middleware.

        Args:
            base_dir: Base directory for thread data. Defaults to Paths resolution.
        """
        super().__init__()
        self._paths = Paths(base_dir) if base_dir else get_paths()

    def _format_file_entry(self, file: dict, lines: list[str]) -> None:
        """Append a single file entry (name, size, path, optional outline) to lines."""
        size_kb = file["size"] / 1024
        size_str = f"{size_kb:.1f} KB" if size_kb < 1024 else f"{size_kb / 1024:.1f} MB"
        lines.append(f"- {file['filename']} ({size_str})")
        lines.append(f"  Path: {file['path']}")
        markdown_path = file.get("markdown_path")
        if markdown_path:
            lines.append(f"  Converted Markdown (read this one): {markdown_path}")
        outline = file.get("outline") or []
        if outline:
            truncated = outline[-1].get("truncated", False)
            visible = [e for e in outline if not e.get("truncated")]
            target = "the converted Markdown" if markdown_path else "the file"
            lines.append(f"  Document outline — line numbers refer to {target} (use `read_file` with line ranges):")
            for entry in visible:
                lines.append(f"    L{entry['line']}: {entry['title']}")
            if truncated:
                lines.append(f"    ... (showing first {len(visible)} headings; use `read_file` to explore further)")
        else:
            preview = file.get("outline_preview") or []
            if preview:
                lines.append("  No structural headings detected. Document begins with:")
                for text in preview:
                    lines.append(f"    > {text}")
            lines.append("  Use `grep` to search for keywords (e.g. `grep(pattern='keyword', path='/mnt/user-data/uploads/')`).")
        lines.append("")

    def _create_files_message(self, new_files: list[dict], historical_names: list[str]) -> str:
        """Create a formatted message listing uploaded files.

        Args:
            new_files: Files uploaded in the current message. Each file dict may
                contain an optional ``outline`` key — a list of ``{title, line}``
                dicts extracted from the converted Markdown file.
            historical_names: Filenames uploaded in previous messages. Listed by
                name only; details are fetched via ``list_uploaded_files``.

        Returns:
            Formatted string inside <uploaded_files> tags.
        """
        lines = ["<uploaded_files>"]

        if new_files:
            lines.append("The following files were uploaded in this message:")
            lines.append("")
            for file in new_files:
                self._format_file_entry(file, lines)

        if historical_names:
            shown = historical_names[:_MAX_HISTORICAL_NAMES]
            lines.append("Also uploaded earlier in this conversation and still available:")
            lines.append(f"  {', '.join(shown)}")
            if len(historical_names) > len(shown):
                lines.append(f"  ... and {len(historical_names) - len(shown)} more")
            lines.append("Call `list_uploaded_files` for their paths, then pass a filename to get that file's outline.")
            lines.append("")

        lines.append("To work with these files:")
        if new_files:
            lines.append("- Read from the file first — use the outline line numbers and `read_file` to locate relevant sections.")
        else:
            lines.append("- Use `list_uploaded_files` to see what is available, then call it with a filename for that file's outline before `read_file`.")
        lines.append("- Use `grep` to search for keywords when you are not sure which section to look at")
        lines.append("  (e.g. `grep(pattern='revenue', path='/mnt/user-data/uploads/')`).")
        lines.append("- Use `glob` to find files by name pattern")
        lines.append("  (e.g. `glob(pattern='**/*.md', path='/mnt/user-data/uploads/')`).")
        lines.append("- Only fall back to web search if the file content is clearly insufficient to answer the question.")
        lines.append("</uploaded_files>")

        return "\n".join(lines)

    def _files_from_kwargs(self, message: HumanMessage, uploads_dir: Path | None = None) -> list[dict] | None:
        """Extract file info from message additional_kwargs.files.

        The frontend sends uploaded file metadata in additional_kwargs.files
        after a successful upload. Each entry has: filename, size (bytes),
        path (virtual path), status.

        Args:
            message: The human message to inspect.
            uploads_dir: Physical uploads directory used to verify file existence.
                         When provided, entries whose files no longer exist are skipped.

        Returns:
            List of file dicts with virtual paths, or None if the field is absent or empty.
        """
        kwargs_files = (message.additional_kwargs or {}).get("files")
        if not isinstance(kwargs_files, list) or not kwargs_files:
            return None

        files = []
        for f in kwargs_files:
            if not isinstance(f, dict):
                continue
            filename = f.get("filename") or ""
            if not filename or Path(filename).name != filename:
                continue
            if uploads_dir is not None and not (uploads_dir / filename).is_file():
                continue
            files.append(
                {
                    "filename": filename,
                    "size": int(f.get("size") or 0),
                    "path": f"/mnt/user-data/uploads/{filename}",
                    "extension": Path(filename).suffix,
                }
            )
        return files if files else None

    def _resolve_thread_id(self, runtime: Runtime | None) -> str | None:
        """Resolve the thread id from runtime context or the ambient RunnableConfig."""
        thread_id = (runtime.context or {}).get("thread_id") if runtime is not None else None
        if thread_id:
            return thread_id
        try:
            from langgraph.config import get_config

            return get_config().get("configurable", {}).get("thread_id")
        except RuntimeError:
            return None  # get_config() raises outside a runnable context (e.g. unit tests)

    def _collect_files(self, last_message: HumanMessage, runtime: Runtime | None) -> tuple[list[dict], list[str]]:
        """Return (new_files_with_outlines, historical_filenames).

        Outlines are extracted only for files attached to *last_message*.
        Historical uploads are returned as names so the prompt stays small; the
        model fetches their details through ``list_uploaded_files``.
        """
        thread_id = self._resolve_thread_id(runtime)
        uploads_dir = self._paths.sandbox_uploads_dir(thread_id) if thread_id else None

        new_files = self._files_from_kwargs(last_message, uploads_dir) or []
        new_filenames = {f["filename"] for f in new_files}
        direct_image_filenames = {
            Path(str(image["virtual_path"])).name
            for image in normalize_input_image_metadata(
                (last_message.additional_kwargs or {}).get(INPUT_IMAGES_KEY)
            )
        }
        current_filenames = new_filenames | direct_image_filenames

        historical_names: list[str] = []
        if uploads_dir and uploads_dir.is_dir():
            entries = [p for p in uploads_dir.iterdir() if p.is_file()]
            names = {p.name for p in entries}
            historical_names = sorted(
                p.name
                for p in entries
                if p.name not in current_filenames
                and not _is_conversion_artifact(p, names)
            )

        if uploads_dir:
            for file in new_files:
                phys_path = uploads_dir / file["filename"]
                outline, preview = extract_outline_for_file(phys_path)
                file["outline"] = outline
                file["outline_preview"] = preview
                md_name = converted_markdown_name(phys_path)
                if md_name:
                    file["markdown_path"] = f"/mnt/user-data/uploads/{md_name}"

        return new_files, historical_names

    def _build_injected_messages(self, messages: list, runtime: Runtime | None) -> list | None:
        """Return *messages* with an <uploaded_files> block on the last human turn.

        Targets the most recent HumanMessage rather than the final message: within
        a turn the model is called repeatedly with tool results appended, and the
        file context must stay visible for all of those calls.

        Returns ``None`` when there is nothing to inject, so callers can pass the
        original list through untouched.
        """
        messages = _strip_persisted_blocks(messages)

        last_index = _last_human_message_index(messages)
        if last_index is None:
            return None

        last_message = messages[last_index]

        new_files, historical_names = self._collect_files(last_message, runtime)
        if not new_files and not historical_names:
            return None

        logger.debug("New files: %s, historical: %d", [f["filename"] for f in new_files], len(historical_names))

        files_message = self._create_files_message(new_files, historical_names)

        original_content = last_message.content
        if isinstance(original_content, str):
            updated_content = f"{files_message}\n\n{original_content}"
        elif isinstance(original_content, list):
            # Multimodal content: prepend as a text block, keep image blocks intact.
            updated_content = [{"type": "text", "text": f"{files_message}\n\n"}, *original_content]
        else:
            return None

        # Preserve additional_kwargs (including files metadata) so the frontend
        # can read structured file info from the streamed message.
        patched = list(messages)
        patched[last_index] = HumanMessage(
            content=updated_content,
            id=last_message.id,
            additional_kwargs=last_message.additional_kwargs,
        )
        return patched

    @override
    def before_agent(self, state: UploadsMiddlewareState, runtime: Runtime) -> dict | None:
        """Record metadata for files attached to the current message.

        Only the ``uploaded_files`` state key is written — the prompt block itself
        is injected per model call by :meth:`wrap_model_call` so it never enters
        the persisted history.

        Locates the human turn the same way :meth:`_build_injected_messages`
        does (most recent ``HumanMessage``, not just the final message) so the
        two stay consistent — e.g. after an interrupted-then-resumed turn
        leaves a ``ToolMessage`` at the tail of state.
        """
        messages = state.get("messages") or []
        if not messages:
            return {"uploaded_files": []}

        last_index = _last_human_message_index(messages)
        if last_index is None:
            return {"uploaded_files": []}
        last_message = messages[last_index]

        new_files, _ = self._collect_files(last_message, runtime)
        if not new_files:
            # A file from the previous run is historical now. Clear the
            # persisted current-run snapshot so list_uploaded_files and child
            # agents no longer exclude it.
            return {"uploaded_files": []}
        return {"uploaded_files": new_files}

    @override
    def wrap_model_call(
        self,
        request: ModelRequest,
        handler: Callable[[ModelRequest], ModelResponse],
    ) -> ModelCallResult:
        patched = self._build_injected_messages(request.messages, getattr(request, "runtime", None))
        if patched is not None:
            request = request.override(messages=patched)
        return handler(request)

    @override
    async def awrap_model_call(
        self,
        request: ModelRequest,
        handler: Callable[[ModelRequest], Awaitable[ModelResponse]],
    ) -> ModelCallResult:
        patched = self._build_injected_messages(request.messages, getattr(request, "runtime", None))
        if patched is not None:
            request = request.override(messages=patched)
        return await handler(request)
