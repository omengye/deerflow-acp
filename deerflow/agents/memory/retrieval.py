"""SQLite FTS5/BM25 retrieval for long-term memory facts.

The JSON memory document remains the source of truth. This module maintains a
rebuildable side index and fails open: an unavailable or corrupt FTS5 database
must never prevent an agent run or a memory save.
"""

from __future__ import annotations

import hashlib
import json
import logging
import math
import re
import sqlite3
import threading
from collections import Counter
from pathlib import Path
from typing import Any

from deerflow.config.memory_config import get_memory_config
from deerflow.config.paths import get_paths

logger = logging.getLogger(__name__)

_INDEX_LOCK = threading.RLock()
_LATIN_TOKEN_RE = re.compile(r"[A-Za-z0-9_][A-Za-z0-9_.+#-]*")
_CJK_RUN_RE = re.compile(r"[\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff]+")


def _agent_key(agent_name: str | None) -> str:
    return agent_name or "__global__"


def _index_path() -> Path:
    configured = get_memory_config().retrieval_index_path
    if configured:
        path = Path(configured)
        return path if path.is_absolute() else get_paths().base_dir / path
    return get_paths().base_dir / "memory-fts5.sqlite3"


def _tokens(text: str) -> list[str]:
    """Return deterministic English and CJK tokens without extra dependencies."""
    tokens = [match.group(0).casefold() for match in _LATIN_TOKEN_RE.finditer(text)]
    for match in _CJK_RUN_RE.finditer(text):
        run = match.group(0)
        tokens.extend(run)
        if len(run) > 1:
            tokens.extend(run[index : index + 2] for index in range(len(run) - 1))
    # Preserve order while limiting pathological input expansion.
    return list(dict.fromkeys(token for token in tokens if token))[:512]


def _indexable_text(text: str) -> str:
    return " ".join(_tokens(text))


def _query_expression(query: str) -> str:
    quoted = [f'"{token.replace(chr(34), chr(34) * 2)}"' for token in _tokens(query)[:64]]
    return " OR ".join(quoted)


def _memory_signature(memory_data: dict[str, Any]) -> str:
    facts = [
        {
            "id": fact.get("id"),
            "content": fact.get("content"),
            "category": fact.get("category"),
            "confidence": fact.get("confidence"),
            "sourceError": fact.get("sourceError"),
        }
        for fact in memory_data.get("facts", [])
        if isinstance(fact, dict)
    ]
    payload = json.dumps(facts, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def _connect() -> sqlite3.Connection:
    path = _index_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    connection = sqlite3.connect(path, timeout=10)
    try:
        connection.execute("PRAGMA busy_timeout=10000")
        connection.execute(
            """
            CREATE TABLE IF NOT EXISTS memory_fts_meta (
                agent_key TEXT PRIMARY KEY,
                signature TEXT NOT NULL
            )
            """
        )
        columns = {
            str(row[1])
            for row in connection.execute("PRAGMA table_info(memory_facts_fts)").fetchall()
        }
        expected_columns = {
            "agent_key",
            "fact_id",
            "indexed_content",
            "display_content",
            "category",
            "confidence",
            "source_error",
        }
        if columns and not expected_columns.issubset(columns):
            # The index is disposable. Recreate older schemas in place rather
            # than failing every query after an application upgrade.
            connection.execute("DROP TABLE memory_facts_fts")
        connection.execute(
            """
            CREATE VIRTUAL TABLE IF NOT EXISTS memory_facts_fts USING fts5(
                agent_key UNINDEXED,
                fact_id UNINDEXED,
                indexed_content,
                display_content UNINDEXED,
                category UNINDEXED,
                confidence UNINDEXED,
                source_error UNINDEXED,
                tokenize='unicode61'
            )
            """
        )
        return connection
    except BaseException:
        connection.close()
        raise


def rebuild_memory_index(memory_data: dict[str, Any], agent_name: str | None = None) -> bool:
    """Synchronize one agent's FTS rows with its current JSON facts."""
    if not get_memory_config().retrieval_enabled:
        return False
    key = _agent_key(agent_name)
    signature = _memory_signature(memory_data)
    try:
        with _INDEX_LOCK, _connect() as connection:
            current = connection.execute(
                "SELECT signature FROM memory_fts_meta WHERE agent_key = ?",
                (key,),
            ).fetchone()
            if current is not None and current[0] == signature:
                return True

            connection.execute("DELETE FROM memory_facts_fts WHERE agent_key = ?", (key,))
            rows: list[tuple[str, str, str, str, str, str, str]] = []
            for fact in memory_data.get("facts", []):
                if not isinstance(fact, dict):
                    continue
                content = fact.get("content")
                fact_id = fact.get("id")
                if not isinstance(content, str) or not content.strip() or not isinstance(fact_id, str):
                    continue
                indexed_content = _indexable_text(content)
                if not indexed_content:
                    continue
                rows.append(
                    (
                        key,
                        fact_id,
                        indexed_content,
                        content.strip(),
                        str(fact.get("category", "context")),
                        str(fact.get("confidence", 0.5)),
                        str(fact.get("sourceError", "")),
                    )
                )
            connection.executemany(
                """
                INSERT INTO memory_facts_fts(
                    agent_key, fact_id, indexed_content, display_content, category, confidence,
                    source_error
                ) VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                rows,
            )
            connection.execute(
                """
                INSERT INTO memory_fts_meta(agent_key, signature) VALUES (?, ?)
                ON CONFLICT(agent_key) DO UPDATE SET signature = excluded.signature
                """,
                (key, signature),
            )
        return True
    except (OSError, sqlite3.Error):
        logger.warning("Failed to rebuild memory FTS5 index", exc_info=True)
        return False


def search_memory_facts(
    query: str,
    memory_data: dict[str, Any],
    agent_name: str | None = None,
    *,
    top_k: int | None = None,
) -> list[dict[str, Any]]:
    """Return BM25-ranked facts relevant to ``query``."""
    config = get_memory_config()
    if not config.retrieval_enabled or not isinstance(query, str) or not query.strip():
        return []
    expression = _query_expression(query)
    if not expression:
        return []
    limit = top_k if top_k is not None else config.retrieval_top_k
    if limit <= 0:
        return []
    candidate_limit = min(int(limit) * 4, 400) if config.retrieval_mmr_enabled else int(limit)

    rows = None
    if rebuild_memory_index(memory_data, agent_name):
        try:
            with _INDEX_LOCK, _connect() as connection:
                rows = connection.execute(
                    """
                    SELECT fact_id, display_content, category, confidence, source_error,
                           bm25(memory_facts_fts) AS rank
                    FROM memory_facts_fts
                    WHERE memory_facts_fts MATCH ? AND agent_key = ?
                    ORDER BY rank ASC
                    LIMIT ?
                    """,
                    (expression, _agent_key(agent_name), candidate_limit),
                ).fetchall()
        except (OSError, sqlite3.Error):
            logger.warning("Failed to query memory FTS5 index", exc_info=True)

    if rows is None:
        # The JSON facts are authoritative. Keep retrieval available on Python
        # builds without FTS5 and when the disposable side index cannot be used.
        results = _search_without_index(query, memory_data, candidate_limit)
    else:
        results = []
        for fact_id, content, category, confidence, source_error, rank in rows:
            try:
                parsed_confidence = float(confidence)
            except (TypeError, ValueError):
                parsed_confidence = 0.5
            result = {
                "id": fact_id,
                "content": content,
                "category": category,
                "confidence": parsed_confidence,
                "bm25_score": -float(rank),
            }
            if source_error:
                result["sourceError"] = source_error
            results.append(result)
    if config.retrieval_mmr_enabled:
        return _diversify_facts(results, limit=int(limit), relevance_weight=config.retrieval_mmr_lambda)
    return results


def _search_without_index(query: str, memory_data: dict[str, Any], limit: int) -> list[dict[str, Any]]:
    """Rank the current JSON facts with BM25 when the SQLite index is unusable."""
    query_tokens = set(_tokens(query)[:64])
    documents: list[tuple[dict[str, Any], set[str]]] = []
    frequencies: Counter[str] = Counter()
    for fact in memory_data.get("facts", []):
        if not isinstance(fact, dict):
            continue
        content = fact.get("content")
        if not isinstance(content, str) or not content.strip() or not isinstance(fact.get("id"), str):
            continue
        tokens = set(_tokens(content))
        if not tokens:
            continue
        documents.append((fact, tokens))
        frequencies.update(tokens & query_tokens)

    if not documents:
        return []
    average_length = sum(len(tokens) for _, tokens in documents) / len(documents)
    results: list[dict[str, Any]] = []
    for fact, tokens in documents:
        matches = query_tokens & tokens
        if not matches:
            continue
        length_factor = 1.2 * (0.25 + 0.75 * len(tokens) / average_length)
        score = sum(
            math.log1p((len(documents) - frequencies[token] + 0.5) / (frequencies[token] + 0.5))
            * 2.2
            / (1.0 + length_factor)
            for token in matches
        )
        try:
            confidence = float(fact.get("confidence", 0.5))
        except (TypeError, ValueError):
            confidence = 0.5
        result = {
            "id": fact["id"],
            "content": fact["content"].strip(),
            "category": str(fact.get("category", "context")),
            "confidence": confidence,
            "bm25_score": score,
        }
        source_error = str(fact.get("sourceError", ""))
        if source_error:
            result["sourceError"] = source_error
        results.append(result)
    results.sort(key=lambda fact: fact["bm25_score"], reverse=True)
    return results[:limit]


def _diversify_facts(facts: list[dict[str, Any]], *, limit: int, relevance_weight: float) -> list[dict[str, Any]]:
    """Select a deterministic MMR prefix without embeddings or storage changes.

    Candidates already arrive in BM25 order. Token sets are built once, and
    selection stops at the requested count. Equal scores keep retrieval order.
    """
    if len(facts) <= 1 or limit <= 0:
        return facts[:max(0, limit)]
    token_sets = [set(_tokens(str(fact.get("content", ""))[:8192])) for fact in facts]
    scores = [max(0.0, float(fact.get("bm25_score", 0.0))) for fact in facts]
    maximum = max(scores) or 1.0
    relevance = [score / maximum for score in scores]
    remaining = list(range(len(facts)))
    selected: list[int] = []
    redundancy = [0.0] * len(facts)
    while remaining and len(selected) < limit:
        # Always anchor the result in the best retrieval match, including when
        # callers intentionally set lambda to zero for maximum diversity.
        chosen = remaining[0] if not selected else max(
            remaining,
            key=lambda index: relevance_weight * relevance[index] - (1.0 - relevance_weight) * redundancy[index],
        )
        remaining.remove(chosen)
        selected.append(chosen)
        for index in remaining:
            union = token_sets[index] | token_sets[chosen]
            overlap = len(token_sets[index] & token_sets[chosen]) / len(union) if union else 0.0
            redundancy[index] = max(redundancy[index], overlap)
    return [facts[index] for index in selected]
