"""Immutable, bounded intent search over already-authorized skill metadata."""

from __future__ import annotations

import re
import unicodedata
from dataclasses import dataclass
from functools import lru_cache

_WORDS = re.compile(r"[^\W_]+", re.UNICODE)
_CJK = re.compile(r"[\u3400-\u4dbf\u4e00-\u9fff]+")
_SEPARATORS = re.compile(r"[-_./]+")
MAX_QUERY_CHARS = 256
MAX_QUERY_TERMS = 32
MAX_RESULTS = 5


def _normalize(text: str) -> str:
    return " ".join(_SEPARATORS.sub(" ", unicodedata.normalize("NFKC", text).casefold()).split())


def _terms(text: str) -> tuple[str, ...]:
    terms: list[str] = []
    for word in _WORDS.findall(text):
        if word in {"a", "i", "the", "to", "for", "and"}:
            continue
        terms.append(word)
        for run in _CJK.findall(word):
            terms.extend(run[index:index + 2] for index in range(len(run) - 1))
    return tuple(dict.fromkeys(terms))[:MAX_QUERY_TERMS]


def _contains(text: str, term: str) -> bool:
    return term in _WORDS.findall(text) if len(term) == 1 and term.isascii() else term in text


@dataclass(frozen=True)
class SkillMetadata:
    name: str
    description: str
    category: str
    location: str
    search_name: str
    search_description: str

    def as_dict(self) -> dict[str, str]:
        return {key: getattr(self, key) for key in ("name", "description", "category", "location")}


@dataclass(frozen=True)
class SkillCatalog:
    entries: tuple[SkillMetadata, ...]

    def search(self, query: str) -> list[SkillMetadata]:
        query = query.strip()
        if not query:
            return []
        # Explicit selections are never cut off by free-text search limits.
        if query.startswith("select:"):
            wanted = {name.strip() for name in query[7:].split(",")}
            return [entry for entry in self.entries if entry.name in wanted]
        exact = [entry for entry in self.entries if entry.name == query.removeprefix("$")]
        if exact:
            return exact

        query = query[:MAX_QUERY_CHARS]
        candidates = self.entries
        required = query.startswith("+")
        if required:
            parts = query[1:].split(None, 1)
            if not parts or not _WORDS.search(parts[0]):
                return []
            name_term = _normalize(parts[0])
            candidates = tuple(entry for entry in candidates if name_term in entry.search_name)
            query = parts[1] if len(parts) == 2 else ""
        normalized = _normalize(query)
        terms = _terms(normalized)
        if not terms:
            return list(candidates[:MAX_RESULTS]) if required else []

        scored: list[tuple[tuple[int, ...], SkillMetadata]] = []
        for entry in candidates:
            name_hits = sum(_contains(entry.search_name, term) for term in terms)
            hits = sum(_contains(entry.search_name, term) or _contains(entry.search_description, term) for term in terms)
            if hits or required:
                score = (int(entry.search_name == normalized), hits, int(normalized in entry.search_name), name_hits, int(normalized in entry.search_description))
                scored.append((score, entry))
        scored.sort(key=lambda pair: pair[0], reverse=True)
        return [entry for _, entry in scored[:MAX_RESULTS]]


@lru_cache(maxsize=64)
def catalog_from_signature(signature: tuple[tuple[str, str, str, str], ...]) -> SkillCatalog:
    """Cache normalized metadata; content changes naturally produce a new key."""
    return SkillCatalog(tuple(SkillMetadata(name, description, category, location, _normalize(name), _normalize(description)) for name, description, category, location in signature))
