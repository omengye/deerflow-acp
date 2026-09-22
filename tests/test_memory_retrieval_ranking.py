from __future__ import annotations

from copy import deepcopy

import pytest

from deerflow.agents.memory import retrieval
from deerflow.agents.memory.backends.deermem import DeerMemManager
from deerflow.agents.memory.prompt import _count_tokens, format_memory_for_injection
from deerflow.config.memory_config import MemoryConfig


def _fact(identifier: str, content: str, confidence: float = 0.9) -> dict:
    return {"id": identifier, "content": content, "confidence": confidence, "category": "preference"}


def test_injection_keeps_retrieval_order_and_legacy_context_still_uses_confidence(monkeypatch):
    memory = {"facts": [_fact("relevant", "Python automation preference", 0.7), _fact("generic", "Generic reminder", 0.99)]}
    original = deepcopy(memory)
    config = MemoryConfig()
    monkeypatch.setattr("deerflow.config.memory_config.get_memory_config", lambda: config)
    monkeypatch.setattr("deerflow.agents.memory.updater.get_memory_data", lambda _agent: memory)
    monkeypatch.setattr(retrieval, "search_memory_facts", lambda *_args: memory["facts"])
    relevant = DeerMemManager(config).get_context(query="Python automation")
    assert relevant.index("Python automation") < relevant.index("Generic reminder")
    legacy = DeerMemManager(config).get_context()
    assert legacy.index("Generic reminder") < legacy.index("Python automation")
    assert memory == original


def test_injection_budget_preserves_first_retrieved_chinese_fact():
    first = _fact("first", "用户偏好中文技术说明", 0.7)
    later = _fact("later", "用户喜欢简短工作报告", 0.99)
    first_text = format_memory_for_injection({"facts": [first]}, preserve_fact_order=True)
    budget = _count_tokens(first_text) + 1
    result = format_memory_for_injection({"facts": [first, later]}, max_tokens=budget, preserve_fact_order=True)
    assert first["content"] in result
    assert later["content"] not in result
    assert _count_tokens(result) <= budget


@pytest.mark.parametrize("query,duplicate,distinct", [
    ("Python", "Python automation scripts", "Python deployments Kubernetes monitoring"),
    ("中文", "中文技术交流说明", "中文文档排版和插图"),
])
def test_mmr_retrieves_extra_candidates_and_diversifies_without_storage_changes(tmp_path, monkeypatch, query, duplicate, distinct):
    config = MemoryConfig(retrieval_index_path=str(tmp_path / "ranking.sqlite3"), retrieval_top_k=2)
    monkeypatch.setattr(retrieval, "get_memory_config", lambda: config)
    memory = {"facts": [_fact("a", duplicate), _fact("b", duplicate), _fact("c", distinct)]}
    original = deepcopy(memory)
    plain = retrieval.search_memory_facts(query, memory)
    assert {item["id"] for item in plain} == {"a", "b"}
    config.retrieval_mmr_enabled = True
    config.retrieval_mmr_lambda = 0.0
    diversified = retrieval.search_memory_facts(query, memory)
    assert diversified[0]["id"] == plain[0]["id"]
    assert diversified[1]["id"] == "c"
    assert retrieval.search_memory_facts(query, memory) == diversified
    assert retrieval.search_memory_facts(query, memory, top_k=1)[0]["id"] == plain[0]["id"]
    config.retrieval_mmr_lambda = 1.0
    assert retrieval.search_memory_facts(query, memory) == plain
    assert memory == original


def test_mmr_configuration_is_opt_in_and_round_trips_backend_fields():
    assert MemoryConfig().retrieval_mmr_enabled is False
    config = MemoryConfig(backend_config={"retrieval_mmr_enabled": True, "retrieval_mmr_lambda": 0.4})
    assert config.retrieval_mmr_enabled is True
    assert config.retrieval_mmr_lambda == 0.4
    assert MemoryConfig.model_validate(config.model_dump()) == config
    with pytest.raises(ValueError):
        MemoryConfig(retrieval_mmr_lambda=1.1)
