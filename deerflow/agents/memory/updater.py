"""Memory updater for reading, writing, and updating memory data."""

import asyncio
import atexit
import concurrent.futures
import copy
import json
import logging
import math
import re
import uuid
from collections.abc import Awaitable, Callable
from typing import Any

from deerflow.agents.memory.prompt import (
    MEMORY_UPDATE_PROMPT,
    format_conversation_for_update,
)
from deerflow.agents.memory.storage import (
    create_empty_memory,
    get_memory_storage,
    utc_now_iso_z,
)
from deerflow.config.memory_config import get_memory_config
from deerflow.models import aclose_chat_model, create_chat_model

logger = logging.getLogger(__name__)

_SYNC_MEMORY_UPDATER_EXECUTOR = concurrent.futures.ThreadPoolExecutor(
    max_workers=4,
    thread_name_prefix="memory-updater-sync",
)
atexit.register(lambda: _SYNC_MEMORY_UPDATER_EXECUTOR.shutdown(wait=False))


def _create_empty_memory() -> dict[str, Any]:
    """Backward-compatible wrapper around the storage-layer empty-memory factory."""
    return create_empty_memory()


def _save_memory_to_file(memory_data: dict[str, Any], agent_name: str | None = None) -> bool:
    """Backward-compatible wrapper around the configured memory storage save path."""
    return get_memory_storage().save(memory_data, agent_name)


def get_memory_data(agent_name: str | None = None) -> dict[str, Any]:
    """Get the current memory data via storage provider."""
    return get_memory_storage().load(agent_name)


def _mutate_memory(
    mutator: Callable[[dict[str, Any]], dict[str, Any] | None],
    agent_name: str | None = None,
) -> dict[str, Any] | None:
    """Run a storage-provider read-modify-write operation."""
    storage = get_memory_storage()
    mutate = getattr(storage, "mutate", None)
    if callable(mutate):
        return mutate(mutator, agent_name)
    # Backward compatibility for duck-typed/custom providers written before
    # MemoryStorage.mutate was introduced. Such providers should implement it
    # to obtain transactional guarantees.
    current = copy.deepcopy(storage.reload(agent_name))
    updated = mutator(current)
    if updated is None or not storage.save(updated, agent_name):
        return None
    return updated


def reload_memory_data(agent_name: str | None = None) -> dict[str, Any]:
    """Reload memory data via storage provider."""
    return get_memory_storage().reload(agent_name)


def import_memory_data(memory_data: dict[str, Any], agent_name: str | None = None) -> dict[str, Any]:
    """Persist imported memory data via storage provider.

    Args:
        memory_data: Full memory payload to persist.
        agent_name: If provided, imports into per-agent memory.

    Returns:
        The saved memory data after storage normalization.

    Raises:
        OSError: If persisting the imported memory fails.
    """
    imported = copy.deepcopy(memory_data)
    result = _mutate_memory(lambda _current: copy.deepcopy(imported), agent_name)
    if result is None:
        raise OSError("Failed to save imported memory data")
    return get_memory_storage().load(agent_name)


def clear_memory_data(agent_name: str | None = None) -> dict[str, Any]:
    """Clear all stored memory data and persist an empty structure."""
    cleared_memory = create_empty_memory()
    result = _mutate_memory(lambda _current: copy.deepcopy(cleared_memory), agent_name)
    if result is None:
        raise OSError("Failed to save cleared memory data")
    return result


def _validate_confidence(confidence: float) -> float:
    """Validate persisted fact confidence so stored JSON stays standards-compliant."""
    if not math.isfinite(confidence) or confidence < 0 or confidence > 1:
        raise ValueError("confidence")
    return confidence


_FACT_CLASSIFICATION_FIELDS = ("scope", "durability", "authority")
_TASK_LOCAL_CONTENT_RE = re.compile(
    r"(?:\b(?:this|current)\s+(?:task|thread|project|repository|repo|pr|commit|file|workspace)\b|"
    r"\b(?:for|in)\s+this\s+(?:task|thread|project|repository|repo|pr)\b|"
    r"(?:当前|本次|这次|这个)(?:任务|线程|项目|仓库|文件|提交|PR|工作区))",
    re.IGNORECASE,
)
_TRANSACTIONAL_CONTENT_RE = re.compile(
    r"(?:\b(?:authori[sz]e[sd]?|allow(?:ed)?|permit(?:ted)?)\b.{0,40}"
    r"\b(?:edit|delete|remove|push|publish|deploy|close|merge|force[- ]?push)\b|"
    r"(?:授权|允许|准许|同意).{0,20}(?:编辑|修改|删除|移除|推送|发布|部署|关闭|合并|强推))",
    re.IGNORECASE,
)
_EPHEMERAL_PATH_RE = re.compile(
    r"(?:/mnt/(?:user-data|data)/|(?:[A-Za-z]:[\\/])|(?:^|\s)/(?:tmp|var/tmp)/|"
    r"\b[0-9a-f]{7,40}\b)",
    re.IGNORECASE,
)


def _normalize_gate_label(value: Any) -> str | None:
    if not isinstance(value, str):
        return None
    normalized = value.strip().lower()
    return normalized or None


def _content_scope_gate_reason(content: Any) -> str | None:
    """Reject strong task-local or transactional content even if mislabeled."""
    if not isinstance(content, str) or not content.strip():
        return "missing"
    if _TRANSACTIONAL_CONTENT_RE.search(content):
        return "authority"
    if _TASK_LOCAL_CONTENT_RE.search(content) or _EPHEMERAL_PATH_RE.search(content):
        return "scope"
    return None


def _fact_scope_gate_reason(fact: dict[str, Any]) -> str | None:
    if any(_normalize_gate_label(fact.get(field)) is None for field in _FACT_CLASSIFICATION_FIELDS):
        return "missing"
    if _normalize_gate_label(fact.get("scope")) != "user":
        return "scope"
    if _normalize_gate_label(fact.get("durability")) != "durable":
        return "durability"
    if _normalize_gate_label(fact.get("authority")) != "descriptive":
        return "authority"
    return _content_scope_gate_reason(fact.get("content"))


def _summary_scope_gate_reason(section_data: dict[str, Any]) -> str | None:
    scope = _normalize_gate_label(section_data.get("scope"))
    authority = _normalize_gate_label(section_data.get("authority"))
    if scope is None or authority is None:
        return "missing"
    if scope != "user":
        return "scope"
    if authority != "descriptive":
        return "authority"
    return _content_scope_gate_reason(section_data.get("summary"))


def _removal_scope_gate_reason(removal: dict[str, Any]) -> str | None:
    scope = _normalize_gate_label(removal.get("scope"))
    reason = removal.get("reason")
    if scope is None or not isinstance(reason, str) or not reason.strip():
        return "missing"
    if scope != "user":
        return "scope"
    return _content_scope_gate_reason(reason)


def create_memory_fact(
    content: str,
    category: str = "context",
    confidence: float = 0.5,
    agent_name: str | None = None,
) -> dict[str, Any]:
    """Create a new fact and persist the updated memory data."""
    normalized_content = content.strip()
    if not normalized_content:
        raise ValueError("content")

    normalized_category = category.strip() or "context"
    validated_confidence = _validate_confidence(confidence)
    content_key = _fact_content_key(normalized_content)

    def mutate(memory_data: dict[str, Any]) -> dict[str, Any]:
        facts = list(memory_data.get("facts", []))
        existing_keys = {_fact_content_key(fact.get("content")) for fact in facts if isinstance(fact, dict)}
        if content_key in existing_keys:
            raise ValueError("duplicate content")
        facts.append(
            {
                "id": f"fact_{uuid.uuid4().hex[:8]}",
                "content": normalized_content,
                "category": normalized_category,
                "confidence": validated_confidence,
                "createdAt": utc_now_iso_z(),
                "source": "manual",
            }
        )
        memory_data["facts"] = facts
        return memory_data

    updated_memory = _mutate_memory(mutate, agent_name)
    if updated_memory is None:
        raise OSError("Failed to save memory data after creating fact")
    return updated_memory


def delete_memory_fact(fact_id: str, agent_name: str | None = None) -> dict[str, Any]:
    """Delete a fact by its id and persist the updated memory data."""
    def mutate(memory_data: dict[str, Any]) -> dict[str, Any]:
        facts = memory_data.get("facts", [])
        updated_facts = [fact for fact in facts if fact.get("id") != fact_id]
        if len(updated_facts) == len(facts):
            raise KeyError(fact_id)
        memory_data["facts"] = updated_facts
        return memory_data

    updated_memory = _mutate_memory(mutate, agent_name)
    if updated_memory is None:
        raise OSError(f"Failed to save memory data after deleting fact '{fact_id}'")
    return updated_memory


def update_memory_fact(
    fact_id: str,
    content: str | None = None,
    category: str | None = None,
    confidence: float | None = None,
    agent_name: str | None = None,
) -> dict[str, Any]:
    """Update an existing fact and persist the updated memory data."""
    def mutate(memory_data: dict[str, Any]) -> dict[str, Any]:
        updated_facts: list[dict[str, Any]] = []
        found = False
        replacement_key: str | None = None
        if content is not None:
            normalized_content = content.strip()
            if not normalized_content:
                raise ValueError("content")
            replacement_key = _fact_content_key(normalized_content)
            duplicate = any(
                fact.get("id") != fact_id and _fact_content_key(fact.get("content")) == replacement_key
                for fact in memory_data.get("facts", [])
                if isinstance(fact, dict)
            )
            if duplicate:
                raise ValueError("duplicate content")

        for fact in memory_data.get("facts", []):
            if fact.get("id") == fact_id:
                found = True
                updated_fact = dict(fact)
                if content is not None:
                    updated_fact["content"] = content.strip()
                if category is not None:
                    updated_fact["category"] = category.strip() or "context"
                if confidence is not None:
                    updated_fact["confidence"] = _validate_confidence(confidence)
                updated_facts.append(updated_fact)
            else:
                updated_facts.append(fact)

        if not found:
            raise KeyError(fact_id)
        memory_data["facts"] = updated_facts
        return memory_data

    updated_memory = _mutate_memory(mutate, agent_name)
    if updated_memory is None:
        raise OSError(f"Failed to save memory data after updating fact '{fact_id}'")
    return updated_memory


def _extract_text(content: Any) -> str:
    """Extract plain text from LLM response content (str or list of content blocks).

    Modern LLMs may return structured content as a list of blocks instead of a
    plain string, e.g. [{"type": "text", "text": "..."}]. Using str() on such
    content produces Python repr instead of the actual text, breaking JSON
    parsing downstream.

    String chunks are concatenated without separators to avoid corrupting
    chunked JSON/text payloads. Dict-based text blocks are treated as full text
    blocks and joined with newlines for readability.
    """
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        pieces: list[str] = []
        pending_str_parts: list[str] = []

        def flush_pending_str_parts() -> None:
            if pending_str_parts:
                pieces.append("".join(pending_str_parts))
                pending_str_parts.clear()

        for block in content:
            if isinstance(block, str):
                pending_str_parts.append(block)
            elif isinstance(block, dict):
                flush_pending_str_parts()
                text_val = block.get("text")
                if isinstance(text_val, str):
                    pieces.append(text_val)

        flush_pending_str_parts()
        return "\n".join(pieces)
    return str(content)


def _run_async_update_sync(coro: Awaitable[bool]) -> bool:
    """Run an async memory update from sync code, including nested-loop contexts."""
    handed_off = False

    try:
        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            loop = None

        if loop is not None and loop.is_running():
            future = _SYNC_MEMORY_UPDATER_EXECUTOR.submit(asyncio.run, coro)
            handed_off = True
            return future.result()

        handed_off = True
        return asyncio.run(coro)
    except Exception:
        if not handed_off:
            close = getattr(coro, "close", None)
            if callable(close):
                try:
                    close()
                except Exception:
                    logger.debug(
                        "Failed to close un-awaited memory update coroutine",
                        exc_info=True,
                    )

        logger.exception("Failed to run async memory update from sync context")
        return False


# Matches sentences that describe a file-upload *event* rather than general
# file-related work.  Deliberately narrow to avoid removing legitimate facts
# such as "User works with CSV files" or "prefers PDF export".
_UPLOAD_SENTENCE_RE = re.compile(
    r"[^.!?]*\b(?:"
    r"upload(?:ed|ing)?(?:\s+\w+){0,3}\s+(?:file|files?|document|documents?|attachment|attachments?)"
    r"|file\s+upload"
    r"|/mnt/user-data/uploads/"
    r"|<uploaded_files>"
    r")[^.!?]*[.!?]?\s*",
    re.IGNORECASE,
)


def _strip_upload_mentions_from_memory(memory_data: dict[str, Any]) -> dict[str, Any]:
    """Remove sentences about file uploads from all memory summaries and facts.

    Uploaded files are session-scoped; persisting upload events in long-term
    memory causes the agent to search for non-existent files in future sessions.
    """
    # Scrub summaries in user/history sections
    for section in ("user", "history"):
        section_data = memory_data.get(section, {})
        for val in section_data.values():
            if isinstance(val, dict) and "summary" in val:
                cleaned = _UPLOAD_SENTENCE_RE.sub("", val["summary"]).strip()
                cleaned = re.sub(r"  +", " ", cleaned)
                val["summary"] = cleaned

    # Also remove any facts that describe upload events
    facts = memory_data.get("facts", [])
    if facts:
        memory_data["facts"] = [f for f in facts if not _UPLOAD_SENTENCE_RE.search(f.get("content", ""))]

    return memory_data


def _fact_content_key(content: Any) -> str | None:
    if not isinstance(content, str):
        return None
    stripped = content.strip()
    if not stripped:
        return None
    return " ".join(stripped.casefold().split())


def _parse_json_with_repair(text: str) -> dict[str, Any]:
    """Parse JSON from LLM response, using json-repair as a fallback for malformed output."""
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        pass

    try:
        from json_repair import repair_json  # type: ignore[import-untyped]

        repaired = repair_json(text, return_objects=True)
        if isinstance(repaired, dict):
            return repaired
        raise json.JSONDecodeError("json-repair returned a non-dict object", text, 0)
    except ImportError:
        raise
    except json.JSONDecodeError:
        raise
    except Exception as exc:
        raise json.JSONDecodeError(str(exc), text, 0) from exc


class MemoryUpdater:
    """Updates memory using LLM based on conversation context."""

    def __init__(self, model_name: str | None = None):
        """Initialize the memory updater.

        Args:
            model_name: Optional model name to use. If None, uses config or default.
        """
        self._model_name = model_name

    def _get_model(self):
        """Get the model for memory updates."""
        config = get_memory_config()
        model_name = self._model_name or config.model_name
        return create_chat_model(name=model_name, thinking_enabled=False, disable_keepalive=True)

    def _build_correction_hint(
        self,
        correction_detected: bool,
        reinforcement_detected: bool,
    ) -> str:
        """Build optional prompt hints for correction and reinforcement signals."""
        correction_hint = ""
        if correction_detected:
            correction_hint = (
                "IMPORTANT: Explicit correction signals were detected in this conversation. "
                "Pay special attention to what the agent got wrong, what the user corrected, "
                "and record the correct approach as a fact with category "
                '"correction" and confidence >= 0.95 when appropriate.'
            )
        if reinforcement_detected:
            reinforcement_hint = (
                "IMPORTANT: Positive reinforcement signals were detected in this conversation. "
                "The user explicitly confirmed the agent's approach was correct or helpful. "
                "Record the confirmed approach, style, or preference as a fact with category "
                '"preference" or "behavior" and confidence >= 0.9 when appropriate.'
            )
            correction_hint = (correction_hint + "\n" + reinforcement_hint).strip() if correction_hint else reinforcement_hint

        return correction_hint

    def _prepare_update_prompt(
        self,
        messages: list[Any],
        agent_name: str | None,
        correction_detected: bool,
        reinforcement_detected: bool,
    ) -> tuple[dict[str, Any], str] | None:
        """Load memory and build the update prompt for a conversation."""
        config = get_memory_config()
        if not config.enabled or not messages:
            return None

        current_memory = get_memory_data(agent_name)
        conversation_text = format_conversation_for_update(messages)
        if not conversation_text.strip():
            return None

        correction_hint = self._build_correction_hint(
            correction_detected=correction_detected,
            reinforcement_detected=reinforcement_detected,
        )
        prompt = MEMORY_UPDATE_PROMPT.format(
            current_memory=json.dumps(current_memory, indent=2),
            conversation=conversation_text,
            correction_hint=correction_hint,
        )
        return current_memory, prompt

    def _finalize_update(
        self,
        response_content: Any,
        thread_id: str | None,
        agent_name: str | None,
    ) -> bool:
        """Parse the model response, apply updates, and persist memory."""
        response_text = _extract_text(response_content).strip()

        if response_text.startswith("```"):
            lines = response_text.split("\n")
            last_line = lines[-1].strip()
            response_text = "\n".join(lines[1:-1] if last_line == "```" else lines[1:]).strip()

        update_data = _parse_json_with_repair(response_text)

        def mutate(latest_memory: dict[str, Any]) -> dict[str, Any]:
            updated_memory = self._apply_updates(latest_memory, update_data, thread_id)
            return _strip_upload_mentions_from_memory(updated_memory)

        # Apply the LLM patch to a fresh snapshot inside the provider's complete
        # read-modify-write critical section. The model may have spent seconds
        # generating while manual edits or another worker persisted changes.
        return _mutate_memory(mutate, agent_name) is not None

    async def aupdate_memory(
        self,
        messages: list[Any],
        thread_id: str | None = None,
        agent_name: str | None = None,
        correction_detected: bool = False,
        reinforcement_detected: bool = False,
    ) -> bool:
        """Update memory asynchronously based on conversation messages."""
        model: Any = None
        try:
            prepared = await asyncio.to_thread(
                self._prepare_update_prompt,
                messages=messages,
                agent_name=agent_name,
                correction_detected=correction_detected,
                reinforcement_detected=reinforcement_detected,
            )
            if prepared is None:
                return False

            _current_memory, prompt = prepared
            model = self._get_model()
            from deerflow.agents.middlewares.llm_error_handling_middleware import (
                llm_call_slot_async,
            )
            from deerflow.agents.middlewares.runtime_headers_middleware import (
                bind_runtime_headers,
            )

            request_model = bind_runtime_headers(
                model,
                runtime_values={"thread_id": thread_id},
            )
            async with llm_call_slot_async():
                response = await request_model.ainvoke(prompt, config={"run_name": "memory_agent"})
            return await asyncio.to_thread(
                self._finalize_update,
                response_content=response.content,
                thread_id=thread_id,
                agent_name=agent_name,
            )
        except json.JSONDecodeError as e:
            logger.warning("Failed to parse LLM response for memory update: %s", e)
            return False
        except Exception:
            logger.exception("Memory update failed")
            return False
        finally:
            # _run_async_update_sync wraps this in asyncio.run() on a worker
            # thread; the loop closes right after this coroutine returns.
            # Drain the model's httpx pool first so no transport survives into
            # the post-close GC sweep (root cause of `Event loop is closed`).
            await aclose_chat_model(model)

    def update_memory(
        self,
        messages: list[Any],
        thread_id: str | None = None,
        agent_name: str | None = None,
        correction_detected: bool = False,
        reinforcement_detected: bool = False,
    ) -> bool:
        """Synchronously update memory via the async updater path.

        Args:
            messages: List of conversation messages.
            thread_id: Optional thread ID for tracking source.
            agent_name: If provided, updates per-agent memory. If None, updates global memory.
            correction_detected: Whether recent turns include an explicit correction signal.
            reinforcement_detected: Whether recent turns include a positive reinforcement signal.

        Returns:
            True if update was successful, False otherwise.
        """
        return _run_async_update_sync(
            self.aupdate_memory(
                messages=messages,
                thread_id=thread_id,
                agent_name=agent_name,
                correction_detected=correction_detected,
                reinforcement_detected=reinforcement_detected,
            )
        )

    def _apply_updates(
        self,
        current_memory: dict[str, Any],
        update_data: dict[str, Any],
        thread_id: str | None = None,
    ) -> dict[str, Any]:
        """Apply LLM-generated updates to memory.

        Args:
            current_memory: Current memory data.
            update_data: Updates from LLM.
            thread_id: Optional thread ID for tracking.

        Returns:
            Updated memory data.
        """
        config = get_memory_config()
        now = utc_now_iso_z()
        gate_rejections: dict[str, int] = {}

        def reject(reason: str) -> None:
            gate_rejections[reason] = gate_rejections.get(reason, 0) + 1

        # Update user sections
        user_updates = update_data.get("user", {})
        if not isinstance(user_updates, dict):
            user_updates = {}
        for section in ["workContext", "personalContext", "topOfMind"]:
            section_data = user_updates.get(section, {})
            if not isinstance(section_data, dict):
                continue
            if section_data.get("shouldUpdate") and section_data.get("summary"):
                rejection_reason = _summary_scope_gate_reason(section_data)
                if rejection_reason is not None:
                    reject(f"summary_{rejection_reason}")
                    continue
                current_memory["user"][section] = {
                    "summary": section_data["summary"],
                    "updatedAt": now,
                }

        # Update history sections
        history_updates = update_data.get("history", {})
        if not isinstance(history_updates, dict):
            history_updates = {}
        for section in ["recentMonths", "earlierContext", "longTermBackground"]:
            section_data = history_updates.get(section, {})
            if not isinstance(section_data, dict):
                continue
            if section_data.get("shouldUpdate") and section_data.get("summary"):
                rejection_reason = _summary_scope_gate_reason(section_data)
                if rejection_reason is not None:
                    reject(f"summary_{rejection_reason}")
                    continue
                current_memory["history"][section] = {
                    "summary": section_data["summary"],
                    "updatedAt": now,
                }

        # Remove facts
        facts_to_remove: set[str] = set()
        raw_removals = update_data.get("factsToRemove", [])
        if isinstance(raw_removals, list):
            for removal in raw_removals:
                # Legacy string removals deliberately fail closed because they
                # carry no user-level scope or contradiction reason.
                if not isinstance(removal, dict):
                    reject("removal_missing")
                    continue
                fact_id = removal.get("id")
                rejection_reason = _removal_scope_gate_reason(removal)
                if not isinstance(fact_id, str) or not fact_id.strip():
                    rejection_reason = rejection_reason or "missing"
                if rejection_reason is not None:
                    reject(f"removal_{rejection_reason}")
                    continue
                facts_to_remove.add(fact_id.strip())
        if facts_to_remove:
            current_memory["facts"] = [f for f in current_memory.get("facts", []) if f.get("id") not in facts_to_remove]

        # Add new facts
        current_memory.setdefault("facts", [])
        existing_fact_keys = {fact_key for fact_key in (_fact_content_key(fact.get("content")) for fact in current_memory.get("facts", [])) if fact_key is not None}
        new_facts = update_data.get("newFacts", [])
        if not isinstance(new_facts, list):
            new_facts = []
        for fact in new_facts:
            if not isinstance(fact, dict):
                reject("fact_missing")
                continue
            rejection_reason = _fact_scope_gate_reason(fact)
            if rejection_reason is not None:
                reject(f"fact_{rejection_reason}")
                continue
            confidence = fact.get("confidence", 0.5)
            if isinstance(confidence, bool):
                reject("fact_confidence")
                continue
            try:
                confidence = float(confidence)
            except (TypeError, ValueError):
                reject("fact_confidence")
                continue
            if not math.isfinite(confidence) or confidence < config.fact_confidence_threshold:
                continue
            raw_content = fact.get("content", "")
            if not isinstance(raw_content, str) or not raw_content.strip():
                reject("fact_missing")
                continue
            normalized_content = raw_content.strip()
            fact_key = _fact_content_key(normalized_content)
            if fact_key is not None and fact_key in existing_fact_keys:
                continue

            category = fact.get("category", "context")
            normalized_category = category.strip() if isinstance(category, str) and category.strip() else "context"
            fact_entry = {
                "id": f"fact_{uuid.uuid4().hex[:8]}",
                "content": normalized_content,
                "category": normalized_category,
                "confidence": confidence,
                "createdAt": now,
                "source": thread_id or "unknown",
            }
            source_error = fact.get("sourceError")
            if isinstance(source_error, str):
                normalized_source_error = source_error.strip()
                if normalized_source_error:
                    fact_entry["sourceError"] = normalized_source_error
            current_memory["facts"].append(fact_entry)
            if fact_key is not None:
                existing_fact_keys.add(fact_key)

        # Enforce max facts limit
        if len(current_memory["facts"]) > config.max_facts:
            # Sort by confidence and keep top ones
            current_memory["facts"] = sorted(
                current_memory["facts"],
                key=lambda f: f.get("confidence", 0),
                reverse=True,
            )[: config.max_facts]

        if gate_rejections:
            logger.info("Rejected out-of-scope memory updates: %s", gate_rejections)
        return current_memory


def update_memory_from_conversation(
    messages: list[Any],
    thread_id: str | None = None,
    agent_name: str | None = None,
    correction_detected: bool = False,
    reinforcement_detected: bool = False,
) -> bool:
    """Convenience function to update memory from a conversation.

    Args:
        messages: List of conversation messages.
        thread_id: Optional thread ID.
        agent_name: If provided, updates per-agent memory. If None, updates global memory.
        correction_detected: Whether recent turns include an explicit correction signal.
        reinforcement_detected: Whether recent turns include a positive reinforcement signal.

    Returns:
        True if successful, False otherwise.
    """
    updater = MemoryUpdater()
    return updater.update_memory(messages, thread_id, agent_name, correction_detected, reinforcement_detected)
