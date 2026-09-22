"""Caller-owned per-turn retrieval restrictions; never model tool arguments."""

from __future__ import annotations

from collections.abc import Mapping
from typing import Annotated, Any

from langchain_core.messages import AIMessage, HumanMessage
from pydantic import BaseModel, ConfigDict, Field, StringConstraints

Identifier = Annotated[str, StringConstraints(strip_whitespace=True, min_length=1, max_length=256, pattern=r"^[A-Za-z0-9_.:-]+$")]


class KnowledgeDocument(BaseModel):
    model_config = ConfigDict(extra="forbid")
    dataset_id: Identifier
    document_id: Identifier


class KnowledgeScope(BaseModel):
    """None inherits operator scope; an explicit empty list selects nothing."""

    model_config = ConfigDict(extra="forbid")
    dataset_ids: list[Identifier] | None = Field(default=None, max_length=100)
    documents: list[KnowledgeDocument] | None = Field(default=None, max_length=1000)


def normalize_scope(value: Any) -> dict[str, Any] | None:
    if value is None:
        return None
    return KnowledgeScope.model_validate(value).model_dump()


def scope_from_runtime(runtime: Any) -> dict[str, Any] | None:
    context = getattr(runtime, "context", None)
    if isinstance(context, Mapping) and "knowledge_scope" in context:
        return normalize_scope(context["knowledge_scope"])
    config = getattr(runtime, "config", None) or {}
    metadata = config.get("metadata") or {}
    if "knowledge_scope" in metadata:
        return normalize_scope(metadata["knowledge_scope"])
    # A normal new turn resets the scope. A direct clarification reply retains
    # the original user's restriction unless the caller explicitly overrides it.
    state = getattr(runtime, "state", None) or {}
    messages = state.get("messages", [])
    humans = [i for i, m in enumerate(messages) if isinstance(m, HumanMessage)]
    if not humans:
        return None
    for position in range(len(humans) - 1, -1, -1):
        latest = messages[humans[position]]
        if "knowledge_scope" in latest.additional_kwargs:
            return normalize_scope(latest.additional_kwargs["knowledge_scope"])
        if position == 0 or not any(
            isinstance(m, AIMessage) and any(c.get("name") == "ask_clarification" for c in m.tool_calls)
            for m in messages[humans[position - 1] + 1:humans[position]]
        ):
            break
    return None
