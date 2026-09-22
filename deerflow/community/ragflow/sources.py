"""Bounded citation snapshots shared by tools, streams and client adapters."""

from __future__ import annotations

import hashlib
import re
import uuid
from collections.abc import Mapping
from pathlib import Path
from typing import Any

ARTIFACT_TYPE = "deerflow.knowledge_sources"
_SOURCE_ID = re.compile(r"^[a-f0-9]{32}$")
_SOURCE_REFERENCE = re.compile(r"\[Source ([a-f0-9]{32})\](?:\([^\n)]*\))?")
_KNOWLEDGE_LINK = re.compile(r"\[[^\]\n]*\]\(<?/mnt/user-data/outputs/knowledge-([a-f0-9]{32})\.md>?\)")
_KNOWLEDGE_PATH = re.compile(r"/mnt/user-data/outputs/knowledge-([a-f0-9]{32})\.md")


def _locator(value: object) -> str | None:
    return value if isinstance(value, str) and value.strip() and len(value) <= 256 else None


def _summary_references(text: str) -> str:
    """Normalize source links before truncation so paths cannot be cut in half."""
    text = _KNOWLEDGE_LINK.sub(lambda match: f"[Source {match[1]}]", text)
    text = _SOURCE_REFERENCE.sub(lambda match: f"[Source {match[1]}]", text)
    return _KNOWLEDGE_PATH.sub(lambda match: f"[Source {match[1]}]", text)


def _fit_text(text: str, limit: int, *, marker: str = "\n[Task summary truncated.]") -> str:
    if limit <= 0 or len(text) <= limit:
        return text
    suffix = marker if limit >= len(marker) * 2 else ""
    prefix = text[:limit - len(suffix)]
    start = prefix.rfind("[")
    if start >= 0 and "]" not in prefix[start:] and ("[Source ".startswith(prefix[start:]) or prefix[start:].startswith("[Source ")):
        prefix = prefix[:start]
    return prefix + suffix


def _label(value: object) -> str:
    text = " ".join(str(value).split())[:300]
    return re.sub(r"([\\`*_{}\[\]()<>#!|])", r"\\\1", text)


def source_block(source: Mapping[str, Any]) -> str:
    source_id = str(source["id"])
    label = f"Source {source_id}"
    path = source.get("path")
    citation = f"[{label}]({path})" if path else f"[{label}]"
    page = source.get("page")
    location = f" · page {_label(page)}" if page is not None else ""
    return f"{citation} {_label(source.get('document_name', 'Unknown document'))}{location}\n{source['excerpt']}"


def source_artifact(sources: list[dict[str, Any]]) -> dict[str, Any]:
    return {"type": ARTIFACT_TYPE, "version": 1, "sources": sources}


def get_sources(artifact: Any) -> list[dict[str, Any]]:
    if not isinstance(artifact, dict) or artifact.get("type") != ARTIFACT_TYPE:
        return []
    sources = artifact.get("sources")
    if not isinstance(sources, list):
        return []
    result = []
    seen = set()
    for item in sources[:1000]:
        if not isinstance(item, dict) or not isinstance(item.get("id"), str) or not _SOURCE_ID.fullmatch(item["id"]):
            continue
        if item["id"] in seen or not isinstance(item.get("excerpt"), str):
            continue
        source = dict(item)
        expected_path = f"/mnt/user-data/outputs/knowledge-{item['id']}.md"
        if source.get("path") != expected_path:
            source.pop("path", None)
        seen.add(item["id"])
        result.append(source)
    return result


def sources_from_message(message: Any) -> list[dict[str, Any]]:
    """Read sources from SDK wire messages or serialized LangChain checkpoints."""
    if isinstance(message, Mapping):
        kind = message.get("type")
        artifact = message.get("artifact")
        records = message.get("knowledge_sources")
    else:
        kind = getattr(message, "type", None)
        artifact = getattr(message, "artifact", None)
        records = None
    if kind not in {"tool", "ToolMessage", "ToolMessageChunk"}:
        return []
    if isinstance(records, list):
        return get_sources(source_artifact(records))
    return get_sources(artifact)


def sources_from_messages(messages: Any) -> list[dict[str, Any]]:
    """Build a bounded deduplicated snapshot without consulting application config."""
    if not isinstance(messages, list):
        return []
    result: dict[str, dict[str, Any]] = {}
    for message in messages:
        for source in sources_from_message(message):
            result.setdefault(source["id"], source)
            if len(result) >= 1000:
                return list(result.values())
    return list(result.values())


def budget_sources(content: str, artifact: Any, max_chars: int) -> tuple[str, dict[str, Any]]:
    """Keep whole citation blocks and their snapshots together under a budget."""
    sources = get_sources(artifact)
    selected = []
    blocks = []
    raw_summary = artifact.get("summary") if isinstance(artifact, dict) else None
    summary = _summary_references(raw_summary) if isinstance(raw_summary, str) and raw_summary else ""
    if not sources:
        text = summary or _summary_references(content)
        text = _SOURCE_REFERENCE.sub("[source omitted]", text)
        text = _fit_text(text, max_chars)
        result = source_artifact([])
        if summary:
            result["summary"] = text
        return text, result
    if summary:
        cap = max(0, max_chars // 2) if max_chars > 0 else len(summary)
        summary = _fit_text(summary, cap) if cap else ""
        blocks.append(summary)
    marker = "\n\n[Additional sources omitted; narrow the query.]"
    for source in sources:
        block = source_block(source)
        if max_chars > 0 and len("\n\n".join([*blocks, block])) + len(marker) > max_chars:
            continue
        blocks.append(block)
        selected.append(source)
    if not selected and isinstance(raw_summary, str) and raw_summary:
        # If no evidence fits, return its reserved space to the actual task
        # result instead of discarding half the answer for an empty source list.
        summary = _SOURCE_REFERENCE.sub("[source omitted]", _summary_references(raw_summary))
        allowance = max_chars - len(marker) if max_chars > len(marker) else max_chars
        summary = _fit_text(summary, allowance)
        blocks = [summary]
    if summary:
        retained_ids = {source["id"] for source in selected}
        summary = _SOURCE_REFERENCE.sub(
            lambda match: f"[Source {match[1]}]" if match[1] in retained_ids else "[source omitted]",
            summary,
        )
        blocks[0] = summary
    text = "\n\n".join(blocks)
    if len(selected) < len(sources):
        text += marker
    if not selected and not summary:
        text = "Source budget too small; narrow the query."
        if max_chars > 0:
            text = text[:max_chars]
    if max_chars > 0 and len(text) > max_chars:
        text = text[:max_chars]
    result = source_artifact(selected)
    if summary:
        result["summary"] = summary
    return text, result


def format_sources(
    result: Mapping[str, Any],
    *,
    dataset_names_by_id: Mapping[str, str],
    max_chars_per_chunk: int,
    max_total_chars: int,
    outputs_path: str | None = None,
) -> tuple[str, dict[str, Any]]:
    sources: list[dict[str, Any]] = []
    chunks = result.get("chunks")
    for chunk in chunks[:1000] if isinstance(chunks, list) else []:
        if not isinstance(chunk, Mapping):
            continue
        dataset = _locator(chunk.get("dataset_id"))
        if dataset is None or dataset not in dataset_names_by_id:
            # Missing/cross-scope locators never become a source claim.
            continue
        raw = chunk.get("content")
        if not isinstance(raw, str) or not raw.strip():
            continue
        excerpt = raw.strip()
        truncated = len(excerpt) > max_chars_per_chunk
        if truncated:
            excerpt = excerpt[:max(0, max_chars_per_chunk - 1)] + "…"
        source_id = uuid.uuid4().hex
        document_id = _locator(chunk.get("document_id"))
        chunk_id = _locator(chunk.get("id")) or _locator(chunk.get("chunk_id"))
        source = {
            "id": source_id,
            "dataset_id": dataset,
            "document_id": document_id,
            "chunk_id": chunk_id,
            "document_name": str(chunk.get("document_keyword") or "Unknown document")[:300],
            "excerpt": excerpt,
            "excerpt_truncated": truncated,
            "excerpt_sha256": hashlib.sha256(excerpt.encode("utf-8")).hexdigest(),
            "locator_available": document_id is not None and chunk_id is not None,
        }
        positions = chunk.get("positions")
        if isinstance(positions, list):
            pages = [position[0] for position in positions[:100] if isinstance(position, (list, tuple)) and position and isinstance(position[0], int) and not isinstance(position[0], bool) and 1 <= position[0] <= 1_000_000]
            if pages:
                source["page"] = pages[0]
        if outputs_path:
            source["path"] = f"/mnt/user-data/outputs/knowledge-{source_id}.md"
        sources.append(source)
    if not sources:
        return "No verifiable source excerpts found.", source_artifact([])
    text, artifact = budget_sources("", source_artifact(sources), max_total_chars)
    selected = get_sources(artifact)
    # Persist only sources that fit the model-visible output. Exclusive create
    # cannot follow a planted file link; no provider filename becomes a path.
    for source in selected:
        if "path" not in source:
            continue
        try:
            directory = Path(outputs_path)
            if directory.is_symlink() or not directory.is_dir():
                raise OSError("Output directory is unavailable")
            import json

            snapshot = "# Retrieved knowledge excerpt\n\n" + source_block(source) + "\n\n```json\n" + json.dumps({k: v for k, v in source.items() if k not in {"excerpt", "path"}}, ensure_ascii=False, indent=2) + "\n```\n"
            with (directory / f"knowledge-{source['id']}.md").open("x", encoding="utf-8") as file:
                file.write(snapshot)
        except OSError:
            source.pop("path", None)
    return budget_sources(text, source_artifact(selected), max_total_chars)
