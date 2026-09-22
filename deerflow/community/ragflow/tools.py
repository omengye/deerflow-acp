"""Operator-scoped, read-only RAGFlow knowledge search tool."""

from __future__ import annotations

import asyncio
import logging
import re
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any

from langchain.tools import ToolRuntime
from langchain_core.tools import StructuredTool
from pydantic import (
    AnyHttpUrl,
    BaseModel,
    ConfigDict,
    Field,
    SecretStr,
    ValidationError,
    field_validator,
)

from deerflow.config import get_app_config

from .client import (
    RAGFlowAPIError,
    RAGFlowClient,
    RAGFlowConnectionError,
    RAGFlowProtocolError,
)
from .formatting import format_retrieval_result
from .scope import KnowledgeScope, scope_from_runtime
from .sources import format_sources, source_artifact

logger = logging.getLogger(__name__)

_MAX_PARALLEL_GROUPS = 4
_DATASET_ID_RE = re.compile(
    r"(?<![0-9A-Fa-f])(?:[0-9A-Fa-f]{32}|[0-9A-Fa-f]{8}(?:-[0-9A-Fa-f]{4}){3}-[0-9A-Fa-f]{12})(?![0-9A-Fa-f])"
)
_warned: set[str] = set()


@dataclass(frozen=True, slots=True)
class _Dataset:
    id: str
    name: str
    embedding_model: str
    chunk_count: int | None


class _Settings(BaseModel):
    model_config = ConfigDict(validate_default=True)

    datasets: list[str] | None = Field(default=None, max_length=100)
    base_url: AnyHttpUrl = Field(default="http://localhost:9380")
    api_key: SecretStr | None = None
    timeout: float = Field(default=30, gt=0, le=600, allow_inf_nan=False)
    page_size: int = Field(default=8, ge=1, le=100)
    similarity_threshold: float = Field(
        default=0.2,
        ge=0,
        le=1,
        allow_inf_nan=False,
    )
    vector_similarity_weight: float = Field(
        default=0.3,
        ge=0,
        le=1,
        allow_inf_nan=False,
    )
    top_k: int = Field(default=256, ge=1, le=1024)
    max_chars_per_chunk: int = Field(default=800, ge=1, le=100_000)
    max_total_chars: int = Field(default=8000, ge=1, le=1_000_000)

    @field_validator("datasets")
    @classmethod
    def _normalize_datasets(cls, value: list[str] | None) -> list[str] | None:
        if value is None:
            return None
        if not value:
            raise ValueError(
                "datasets must not be empty; omit it to use all accessible datasets"
            )
        normalized: list[str] = []
        for raw in value:
            dataset_id = raw.strip()
            if not dataset_id or len(dataset_id) > 256:
                raise ValueError("dataset IDs must contain 1 to 256 characters")
            if dataset_id not in normalized:
                normalized.append(dataset_id)
        return normalized

    @field_validator("base_url")
    @classmethod
    def _reject_url_credentials(cls, value: AnyHttpUrl) -> AnyHttpUrl:
        if value.username is not None or value.password is not None:
            raise ValueError("base_url must not contain credentials")
        return value


def _api_key(settings: _Settings) -> str | None:
    value = settings.api_key
    if value is None:
        return None
    key = value.get_secret_value().strip()
    return key or None


def _redact(value: object, api_key: str | None, *, dataset_ids: bool) -> str:
    text = str(value)
    if api_key:
        text = text.replace(api_key, "[REDACTED]")
    return _DATASET_ID_RE.sub("[DATASET_ID]", text) if dataset_ids else text


def _redact_result_values(value: Any, api_key: str | None) -> Any:
    """Redact raw string values before JSON escaping or snapshot serialization."""
    if isinstance(value, str):
        return _redact(value, api_key, dataset_ids=False)
    if isinstance(value, Mapping):
        return {key: _redact_result_values(item, api_key) for key, item in value.items()}
    if isinstance(value, list):
        return [_redact_result_values(item, api_key) for item in value]
    return value


def _settings_or_error() -> tuple[_Settings | None, str | None]:
    config = get_app_config().get_tool_config("knowledge_search")
    if config is None:
        return None, (
            "Error: knowledge_search is not configured; add its RAGFlow "
            "settings to config.yaml."
        )
    try:
        settings = _Settings.model_validate(config.model_extra or {})
    except ValidationError:
        logger.warning("RAGFlow knowledge_search configuration is invalid")
        return None, "Error: Invalid RAGFlow settings; check config.yaml."
    if not _api_key(settings):
        if "api_key" not in _warned:
            _warned.add("api_key")
            logger.warning("RAGFlow API key is not configured")
        return None, (
            "Error: RAGFlow API key is not configured; set "
            "knowledge_search.api_key (prefer $RAGFLOW_API_KEY)."
        )
    return settings, None


def _build_client(settings: _Settings) -> RAGFlowClient:
    key = _api_key(settings)
    if key is None:
        raise ValueError("RAGFlow API key is missing")
    return RAGFlowClient(
        base_url=str(settings.base_url).rstrip("/"),
        api_key=key,
        timeout=settings.timeout,
    )


def _dataset_from_payload(
    payload: Mapping[str, object],
    *,
    expected_id: str | None = None,
) -> _Dataset | None:
    raw_id = payload.get("id")
    if not isinstance(raw_id, str) or not raw_id.strip():
        return None
    dataset_id = raw_id.strip()
    if expected_id is not None and dataset_id != expected_id:
        return None

    raw_count = payload.get("chunk_count")
    count = (
        raw_count
        if isinstance(raw_count, int)
        and not isinstance(raw_count, bool)
        and raw_count >= 0
        else None
    )
    raw_model = payload.get("embedding_model")
    embedding_model = raw_model.strip() if isinstance(raw_model, str) else ""
    if not embedding_model and count != 0:
        raise RAGFlowProtocolError(
            "RAGFlow returned a searchable dataset without embedding model metadata."
        )
    return _Dataset(
        id=dataset_id,
        name=str(payload.get("name") or "Unknown dataset").strip(),
        embedding_model=embedding_model,
        chunk_count=count,
    )


async def _resolve_datasets(
    client: RAGFlowClient,
    settings: _Settings,
) -> tuple[list[_Dataset] | None, str | None]:
    if settings.datasets is None:
        payloads = await client.list_datasets()
        resolved: dict[str, _Dataset] = {}
        for payload in payloads:
            dataset = _dataset_from_payload(payload)
            if dataset is not None:
                resolved.setdefault(dataset.id, dataset)
        if not resolved:
            return None, "Error: No accessible RAGFlow datasets were found."
        return list(resolved.values()), None

    resolved: list[_Dataset] = []
    for position, dataset_id in enumerate(settings.datasets, start=1):
        payloads = await client.list_datasets(dataset_id=dataset_id)
        dataset = next(
            (
                candidate
                for payload in payloads
                if (
                    candidate := _dataset_from_payload(
                        payload,
                        expected_id=dataset_id,
                    )
                )
                is not None
            ),
            None,
        )
        if dataset is None:
            logger.warning(
                "Configured RAGFlow dataset is unavailable "
                "(position=%d, dataset_id=%s)",
                position,
                dataset_id,
            )
            return None, (
                f"Error: knowledge_search.datasets entry {position} was not "
                "found or is inaccessible; check config.yaml."
            )
        resolved.append(dataset)
    return resolved, None


def _dataset_groups(datasets: list[_Dataset]) -> list[list[str]]:
    groups: dict[str, list[str]] = {}
    for dataset in datasets:
        if dataset.chunk_count == 0:
            continue
        groups.setdefault(dataset.embedding_model, []).append(dataset.id)
    return [groups[name] for name in sorted(groups)]


def _chunks(result: Mapping[str, object]) -> list[Mapping[str, object]]:
    raw = result.get("chunks")
    return (
        [item for item in raw if isinstance(item, Mapping)]
        if isinstance(raw, list)
        else []
    )


def _merge_results(
    results: list[dict[str, object]],
    *,
    page_size: int,
) -> dict[str, object]:
    groups = [_chunks(result) for result in results]
    merged: list[Mapping[str, object]] = []
    hide_scores = len(groups) > 1
    for rank in range(max((len(group) for group in groups), default=0)):
        for group in groups:
            if rank >= len(group):
                continue
            chunk = group[rank]
            if hide_scores and "similarity" in chunk:
                chunk = {
                    key: value for key, value in chunk.items() if key != "similarity"
                }
            merged.append(chunk)
            if len(merged) >= page_size:
                break
        if len(merged) >= page_size:
            break

    selected_document_ids = [
        str(chunk["document_id"])
        for chunk in merged
        if chunk.get("document_id") is not None
    ]
    aggregates: dict[str, Mapping[str, object]] = {}
    for result in results:
        raw = result.get("doc_aggs")
        values = raw.values() if isinstance(raw, Mapping) else raw
        if not isinstance(values, list) and not hasattr(values, "__iter__"):
            continue
        for item in values:
            if not isinstance(item, Mapping) or item.get("doc_id") is None:
                continue
            aggregates.setdefault(str(item["doc_id"]), item)

    return {
        "chunks": merged,
        "doc_aggs": [
            aggregates[document_id]
            for document_id in dict.fromkeys(selected_document_ids)
            if document_id in aggregates
        ],
    }


async def _retrieve(
    client: RAGFlowClient,
    settings: _Settings,
    query: str,
    groups: list[list[str]],
    documents: dict[str, list[str]] | None = None,
) -> dict[str, object]:
    semaphore = asyncio.Semaphore(_MAX_PARALLEL_GROUPS)

    async def one_group(dataset_ids: list[str]) -> dict[str, object]:
        async with semaphore:
            result = await client.retrieve(
                query,
                dataset_ids=dataset_ids,
                page_size=settings.page_size,
                similarity_threshold=settings.similarity_threshold,
                vector_similarity_weight=settings.vector_similarity_weight,
                top_k=settings.top_k,
                **({"document_ids": [doc for dataset in dataset_ids for doc in documents.get(dataset, [])]} if documents is not None else {}),
            )
            if documents is not None:
                result = {**result, "chunks": [
                    chunk for chunk in _chunks(result)
                    if chunk.get("dataset_id") in dataset_ids
                    and chunk.get("document_id") in documents.get(chunk.get("dataset_id"), [])
                ]}
            return result

    return _merge_results(
        await asyncio.gather(*(one_group(group) for group in groups)),
        page_size=settings.page_size,
    )


def _tool_error(error: Exception, settings: _Settings) -> str:
    key = _api_key(settings)
    detail = _redact(error, key, dataset_ids=True)
    if isinstance(error, RAGFlowAPIError):
        logger.warning("RAGFlow rejected retrieval (code=%s)", error.code)
        return f"Error: {detail}"
    if isinstance(error, RAGFlowConnectionError):
        base_url = _redact(settings.base_url, key, dataset_ids=True)
        logger.warning("RAGFlow connection failed (%s)", type(error).__name__)
        return f"Error: Unable to connect to RAGFlow ({base_url}): {detail}"
    if isinstance(error, RAGFlowProtocolError):
        logger.warning("RAGFlow returned an invalid response")
        return f"Error: RAGFlow request failed: {detail}"
    logger.warning(
        "Unexpected RAGFlow retrieval failure (%s)", type(error).__name__
    )
    return "Error: An unexpected RAGFlow retrieval error occurred."


async def _search_result(query: str, *, runtime: Any = None, citations: bool = False) -> tuple[str, dict]:
    """Search the intersection of caller selection and operator configuration."""
    empty = source_artifact([])
    query = query.strip()
    if not query:
        return "Error: query must not be empty.", empty
    if len(query) > 20_000:
        return "Error: query exceeds 20000 characters.", empty
    settings, config_error = _settings_or_error()
    if settings is None:
        return config_error or "Error: Invalid RAGFlow settings.", empty

    client = _build_client(settings)
    try:
        datasets, resolution_error = await _resolve_datasets(client, settings)
        if resolution_error:
            return resolution_error, empty
        if not datasets:
            return "Error: No RAGFlow datasets could be resolved.", empty
        raw_scope = scope_from_runtime(runtime)
        documents = None
        if raw_scope is not None:
            scope = KnowledgeScope.model_validate(raw_scope)
            allowed = {dataset.id for dataset in datasets}
            selected = set(scope.dataset_ids) if scope.dataset_ids is not None else allowed
            if not selected <= allowed:
                return "Error: Selected knowledge scope is outside the permitted datasets.", empty
            if scope.documents is not None:
                documents = {}
                for document in scope.documents:
                    if document.dataset_id not in selected:
                        return "Error: Selected knowledge documents are outside the permitted scope.", empty
                    documents.setdefault(document.dataset_id, []).append(document.document_id)
                selected &= documents.keys()
                semaphore = asyncio.Semaphore(_MAX_PARALLEL_GROUPS)

                async def validate(dataset_id: str) -> None:
                    async with semaphore:
                        await client.validate_documents(dataset_id, list(dict.fromkeys(documents[dataset_id])))

                await asyncio.gather(*(validate(dataset_id) for dataset_id in selected))
            datasets = [dataset for dataset in datasets if dataset.id in selected]
        groups = _dataset_groups(datasets)
        if not groups:
            return "No relevant content found.", empty
        result = await _retrieve(client, settings, query, groups, documents)
        if citations:
            # Redact credentials before creating either visible text or its snapshot.
            safe_result = _redact_result_values(result, _api_key(settings))
            state = getattr(runtime, "state", None) or {}
            outputs = (state.get("thread_data") or {}).get("outputs_path")
            return format_sources(
                safe_result,
                dataset_names_by_id={item.id: _redact(item.name, _api_key(settings), dataset_ids=False) for item in datasets},
                max_chars_per_chunk=settings.max_chars_per_chunk,
                max_total_chars=settings.max_total_chars,
                outputs_path=outputs if isinstance(outputs, str) else None,
            )
        rendered = format_retrieval_result(
            result,
            dataset_names_by_id={item.id: item.name for item in datasets},
            max_chars_per_chunk=settings.max_chars_per_chunk,
            max_total_chars=settings.max_total_chars,
        )
        return _redact(rendered, _api_key(settings), dataset_ids=False), empty
    except Exception as error:  # noqa: BLE001 - tool failures become safe model-visible text
        return _tool_error(error, settings), empty


async def knowledge_search(query: str) -> str:
    """Compatibility text API using only the operator-configured scope."""
    content, _ = await _search_result(query)
    return content


async def _knowledge_search(query: str, runtime: ToolRuntime) -> tuple[str, dict]:
    """Retrieve citation-numbered chunks from configured private documents.

    Args:
        query: A specific question or search phrase.
    """
    return await _search_result(query, runtime=runtime, citations=True)


knowledge_search_tool = StructuredTool.from_function(
    coroutine=_knowledge_search,
    name="knowledge_search",
    description=(
        "Search operator-approved RAGFlow datasets and return compact, "
        "source excerpts. Cite the returned Source links exactly when using them. "
        "Per-turn scope is supplied by the caller, not tool arguments. "
        "Structured sources and linked snapshots retain provider locators; API credentials are omitted."
    ),
    response_format="content_and_artifact",
    parse_docstring=True,
)
