from __future__ import annotations

import pytest

from deerflow.community.ragflow.sources import budget_sources, format_sources, get_sources, source_artifact


def _format(chunk):
    return format_sources({"chunks": [{"dataset_id": "dataset", "content": "Captured excerpt", **chunk}]}, dataset_names_by_id={"dataset": "Dataset"}, max_chars_per_chunk=200, max_total_chars=1000)


@pytest.mark.parametrize("invalid", [True, 123, [], {}, "", "   ", "x" * 257])
def test_malformed_locators_never_claim_verifiable_provider_location(invalid):
    _, artifact = _format({"document_id": invalid, "id": invalid})
    source = get_sources(artifact)[0]
    assert source["locator_available"] is False
    assert source["document_id"] is None
    assert source["chunk_id"] is None
    assert source["excerpt"] == "Captured excerpt"


def test_alternate_chunk_locator_is_accepted_and_invalid_dataset_is_skipped():
    _, artifact = _format({"document_id": "doc", "id": None, "chunk_id": "chunk"})
    assert get_sources(artifact)[0]["locator_available"] is True
    assert get_sources(artifact)[0]["chunk_id"] == "chunk"
    _, malformed = _format({"dataset_id": ["dataset"]})
    assert get_sources(malformed) == []


@pytest.mark.parametrize("page", [True, False, 0, -1, 1_000_001, "1"])
def test_invalid_pages_are_not_rendered_as_verified_page_numbers(page):
    text, artifact = _format({"document_id": "doc", "id": "chunk", "positions": [[page]]})
    assert "page" not in get_sources(artifact)[0]
    assert " · page " not in text


def test_valid_page_after_malformed_position_is_retained():
    _, artifact = _format({"positions": [[True], [], (3, 1, 2)]})
    assert get_sources(artifact)[0]["page"] == 3


@pytest.mark.parametrize("reference", [
    "[Source {id}](/mnt/user-data/outputs/knowledge-{id}.md)",
    "[1](/mnt/user-data/outputs/knowledge-{id}.md)",
    "/mnt/user-data/outputs/knowledge-{id}.md",
])
def test_dropped_sources_remove_all_summary_link_forms_without_losing_body(reference):
    identifier = "a" * 32
    artifact = source_artifact([{"id": identifier, "excerpt": "Large evidence " * 200, "document_name": "Book"}])
    artifact["summary"] = "Task completed. " + reference.format(id=identifier) + " Important conclusion."
    text, result = budget_sources("", artifact, 300)
    assert "Task completed." in text and "Important conclusion." in text
    assert identifier not in text and "knowledge-" not in text
    assert get_sources(result) == []
    assert len(text) <= 300


def test_empty_sources_and_tiny_budgets_never_leave_partial_citation_paths():
    identifier = "b" * 32
    summary = "Answer " + f"[1](/mnt/user-data/outputs/knowledge-{identifier}.md)" + " continues " * 40
    artifact = source_artifact([])
    artifact["summary"] = summary
    for limit in (1, 15, 35, 60, 100):
        text, result = budget_sources(summary, artifact, limit)
        assert len(text) <= limit
        assert "knowledge-" not in text and identifier not in text and "[Source" not in text
        assert get_sources(result) == []


def test_summary_truncation_cannot_slice_through_a_retained_reference():
    identifier = "c" * 32
    artifact = source_artifact([{"id": identifier, "excerpt": "Evidence", "document_name": "Book"}])
    artifact["summary"] = "A" * 39 + f"[1](/mnt/user-data/outputs/knowledge-{identifier}.md)" + "B" * 200
    text, result = budget_sources("", artifact, 160)
    assert "knowledge-" not in result.get("summary", "")
    assert "[Source" not in result.get("summary", "")
    for source in get_sources(result):
        assert source["excerpt"] in text and source["id"] in text
    assert len(text) <= 160
