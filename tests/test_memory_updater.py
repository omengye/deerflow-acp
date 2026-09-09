import json
from types import SimpleNamespace
from typing import Any

from langchain_core.messages import HumanMessage

from deerflow.agents.memory import updater as memory_updater
from deerflow.agents.memory.storage import create_empty_memory


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
