import json
from types import SimpleNamespace
from typing import Any

from langchain_core.messages import HumanMessage

from deerflow.agents.memory import updater as memory_updater
from deerflow.agents.memory.storage import create_empty_memory
from deerflow.config.memory_config import MemoryConfig


class _FakeStorage:
    def __init__(self, latest_memory: dict[str, Any]) -> None:
        self.latest_memory = latest_memory
        self.saved_memory: dict[str, Any] | None = None

    def reload(self, agent_name: str | None = None) -> dict[str, Any]:
        return self.latest_memory

    def save(self, memory_data: dict[str, Any], agent_name: str | None = None) -> bool:
        self.saved_memory = memory_data
        return True


def test_finalize_update_applies_llm_patch_to_latest_memory(monkeypatch) -> None:
    latest_memory = create_empty_memory()
    latest_memory["facts"].append(
        {
            "id": "fact_manual",
            "content": "Manual fact added while LLM was generating",
            "category": "context",
            "confidence": 1.0,
            "createdAt": "2026-01-01T00:00:00Z",
            "source": "manual",
        }
    )
    storage = _FakeStorage(latest_memory)
    monkeypatch.setattr(memory_updater, "get_memory_storage", lambda: storage)

    response_content = json.dumps(
        {
            "user": {},
            "history": {},
            "newFacts": [
                {
                    "content": "LLM generated fact",
                    "category": "context",
                    "confidence": 0.9,
                    "scope": "user",
                    "durability": "durable",
                    "authority": "descriptive",
                }
            ],
            "factsToRemove": [],
        }
    )

    assert memory_updater.MemoryUpdater()._finalize_update(
        response_content=response_content,
        thread_id="thread-1",
        agent_name=None,
    )

    assert storage.saved_memory is not None
    saved_contents = {fact["content"] for fact in storage.saved_memory["facts"]}
    assert "Manual fact added while LLM was generating" in saved_contents
    assert "LLM generated fact" in saved_contents


async def test_memory_model_uses_queued_thread_as_runtime_header(monkeypatch) -> None:
    class _Model:
        def __init__(self) -> None:
            self._deerflow_runtime_headers = {"x-opencode-session": "thread_id"}
            self.bound: dict[str, Any] | None = None

        def bind(self, **kwargs):
            self.bound = kwargs
            return self

        async def ainvoke(self, *_args, **_kwargs):
            return SimpleNamespace(content="{}")

    model = _Model()
    updater = memory_updater.MemoryUpdater()
    monkeypatch.setattr(
        updater,
        "_prepare_update_prompt",
        lambda **_kwargs: ({}, "memory prompt"),
    )
    monkeypatch.setattr(updater, "_get_model", lambda: model)
    monkeypatch.setattr(updater, "_finalize_update", lambda **_kwargs: True)

    assert await updater.aupdate_memory(
        [HumanMessage(content="remember this")],
        thread_id="memory-thread",
    )
    assert model.bound == {
        "extra_headers": {"x-opencode-session": "memory-thread"}
    }


def _fact(content: str, *, category: str = "preference", confidence: float = 0.9):
    return {
        "content": content,
        "category": category,
        "confidence": confidence,
        "scope": "user",
        "durability": "durable",
        "authority": "descriptive",
    }


def _memory_with_fact(*, category: str = "preference") -> dict[str, Any]:
    memory = create_empty_memory()
    memory["facts"] = [
        {
            "id": "fact_existing",
            "content": "User prefers concise technical explanations",
            "category": category,
            "confidence": 0.8,
            "createdAt": "2026-01-01T00:00:00Z",
            "source": "thread-old",
        }
    ]
    return memory


def _enable_fact_dedup(monkeypatch, *, enabled: bool = True) -> None:
    config = MemoryConfig(
        fact_dedup_enabled=enabled,
        fact_dedup_similarity_threshold=0.7,
    )
    monkeypatch.setattr(memory_updater, "get_memory_config", lambda: config)


def test_near_duplicate_fact_merges_and_preserves_identity(monkeypatch) -> None:
    _enable_fact_dedup(monkeypatch)
    memory = _memory_with_fact()

    updated = memory_updater.MemoryUpdater()._apply_updates(
        memory,
        {
            "newFacts": [
                _fact(
                    "User strongly prefers concise technical explanations",
                    confidence=0.95,
                )
            ]
        },
        thread_id="thread-new",
    )

    assert len(updated["facts"]) == 1
    merged = updated["facts"][0]
    assert merged["id"] == "fact_existing"
    assert merged["content"] == "User prefers concise technical explanations"
    assert merged["createdAt"] == "2026-01-01T00:00:00Z"
    assert merged["confidence"] == 0.95
    assert merged["source"] == "thread-new"


def test_near_duplicate_with_lower_confidence_keeps_existing_source(monkeypatch) -> None:
    _enable_fact_dedup(monkeypatch)
    memory = _memory_with_fact()

    updated = memory_updater.MemoryUpdater()._apply_updates(
        memory,
        {"newFacts": [_fact("User prefers concise technical explanations always", confidence=0.7)]},
        thread_id="thread-new",
    )

    assert len(updated["facts"]) == 1
    assert updated["facts"][0]["confidence"] == 0.8
    assert updated["facts"][0]["source"] == "thread-old"


def test_fact_dedup_does_not_cross_categories_or_merge_corrections(monkeypatch) -> None:
    _enable_fact_dedup(monkeypatch)
    memory = _memory_with_fact()

    updated = memory_updater.MemoryUpdater()._apply_updates(
        memory,
        {
            "newFacts": [
                _fact("User strongly prefers concise technical explanations", category="behavior"),
                _fact("User always prefers concise technical explanations", category="correction"),
            ]
        },
        thread_id="thread-new",
    )

    assert [fact["category"] for fact in updated["facts"]] == [
        "preference",
        "behavior",
        "correction",
    ]


def test_fact_dedup_disabled_preserves_append_behavior(monkeypatch) -> None:
    _enable_fact_dedup(monkeypatch, enabled=False)
    memory = _memory_with_fact()

    updated = memory_updater.MemoryUpdater()._apply_updates(
        memory,
        {"newFacts": [_fact("User strongly prefers concise technical explanations")]},
    )

    assert len(updated["facts"]) == 2


def test_fact_similarity_supports_cjk_bigrams() -> None:
    assert memory_updater._fact_content_similarity(
        "用户偏好简洁的技术说明",
        "用户 偏好 简洁 的 技术 说明",
    ) == 1.0
