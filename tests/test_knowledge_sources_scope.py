from types import SimpleNamespace

import httpx
import pytest
from langchain_core.messages import AIMessage, HumanMessage, ToolMessage

from app.routers.chat import _chat_kwargs_from_agui, _chat_kwargs_from_request
from app.schemas import AguiRunAgentInput, ChatRequest
from deerflow.agents.middlewares.tool_output_budget_middleware import _patch_tool_message
from deerflow.client import DeerFlowClient
from deerflow.community.ragflow import tools
from deerflow.community.ragflow.client import RAGFlowClient, RAGFlowProtocolError
from deerflow.community.ragflow.scope import KnowledgeScope, scope_from_runtime
from deerflow.community.ragflow.sources import budget_sources, format_sources, get_sources
from deerflow.config.tool_config import ToolConfig
from deerflow.config.tool_output_config import ToolOutputConfig


def _runtime(scope=None, *, path=None):
    return SimpleNamespace(context={"knowledge_scope": scope}, config={}, state={"thread_data": {"outputs_path": path}})


class FakeProvider:
    def __init__(self):
        self.calls = []

    async def list_datasets(self, *, dataset_id=None):
        return [{"id": "allowed", "name": "Docs", "embedding_model": "embed", "chunk_count": 3}]

    async def validate_documents(self, dataset_id, document_ids):
        self.calls.append(("validate", dataset_id, document_ids))
        if set(document_ids) - {"doc"}:
            raise RAGFlowProtocolError("Selected knowledge documents are unavailable or inaccessible.")

    async def retrieve(self, query, **kwargs):
        self.calls.append(("retrieve", kwargs))
        return {"chunks": [
            {"dataset_id": "allowed", "document_id": "doc", "id": "chunk", "document_keyword": "manual.pdf", "content": "Quoted evidence."},
            {"dataset_id": "other", "document_id": "secret", "id": "chunk", "content": "Cross-scope secret"},
            {"dataset_id": "allowed", "document_id": "other-doc", "id": "other-chunk", "content": "Other document"},
        ]}


def _install(monkeypatch):
    provider = FakeProvider()
    config = ToolConfig(name="knowledge_search", group="knowledge", use="test:tool", datasets=["allowed"], api_key="secret-key")
    monkeypatch.setattr(tools, "get_app_config", lambda: SimpleNamespace(get_tool_config=lambda _: config))
    monkeypatch.setattr(tools, "_build_client", lambda _: provider)
    return provider


@pytest.mark.parametrize("scope", [{"dataset_ids": []}, {"documents": []}])
async def test_explicit_empty_selection_never_broadens(monkeypatch, scope):
    provider = _install(monkeypatch)
    text, artifact = await tools._search_result("q", runtime=_runtime(scope), citations=True)
    assert text == "No relevant content found."
    assert not provider.calls and not get_sources(artifact)


async def test_outside_operator_scope_rejected_before_retrieval(monkeypatch):
    provider = _install(monkeypatch)
    text, _ = await tools._search_result("q", runtime=_runtime({"dataset_ids": ["forbidden"]}), citations=True)
    assert "outside" in text and not provider.calls


async def test_scoped_documents_filter_provider_output_and_create_clickable_snapshot(monkeypatch, tmp_path):
    provider = _install(monkeypatch)
    scope = {"documents": [{"dataset_id": "allowed", "document_id": "doc"}]}
    text, artifact = await tools._search_result("q", runtime=_runtime(scope, path=str(tmp_path)), citations=True)
    sources = get_sources(artifact)
    assert len(sources) == 1
    assert "Cross-scope" not in text and "Other document" not in text
    assert provider.calls[0] == ("validate", "allowed", ["doc"])
    assert provider.calls[1][1]["document_ids"] == ["doc"]
    source = sources[0]
    assert source["path"] in text and source["locator_available"]
    saved = (tmp_path / f"knowledge-{source['id']}.md").read_text(encoding="utf-8")
    assert source["excerpt"] in saved and source["excerpt_sha256"] in saved
    assert "secret-key" not in saved


async def test_missing_document_fails_closed(monkeypatch):
    provider = _install(monkeypatch)
    text, _ = await tools._search_result("q", runtime=_runtime({"documents": [{"dataset_id": "allowed", "document_id": "missing"}]}), citations=True)
    assert "unavailable" in text
    assert all(call[0] != "retrieve" for call in provider.calls)


def test_scope_is_not_a_model_tool_argument_and_api_preserves_null_vs_omitted():
    assert set(tools.knowledge_search_tool.tool_call_schema.model_fields) == {"query"}
    assert "knowledge_scope" not in _chat_kwargs_from_request(ChatRequest(message="q"))
    assert _chat_kwargs_from_request(ChatRequest(message="q", knowledge_scope=None))["knowledge_scope"] is None
    req = AguiRunAgentInput(threadId="t", runId="r", knowledgeScope={"dataset_ids": []})
    assert _chat_kwargs_from_agui(req)["knowledge_scope"]["dataset_ids"] == []
    with pytest.raises(ValueError):
        KnowledgeScope(dataset_ids=["../secret"])


def test_clarification_inherits_only_immediately_previous_turn_scope():
    scope = {"dataset_ids": ["allowed"], "documents": None}
    first = HumanMessage(content="q", additional_kwargs={"knowledge_scope": scope})
    question = AIMessage(content="Which?", tool_calls=[{"name": "ask_clarification", "args": {}, "id": "q"}])
    reply = HumanMessage(content="This one")
    runtime = SimpleNamespace(context={}, config={}, state={"messages": [first, question, reply]})
    assert scope_from_runtime(runtime) == scope
    runtime.state["messages"] = [first, AIMessage(content="done"), reply]
    assert scope_from_runtime(runtime) is None
    runtime.state["messages"] = [first, question, reply.model_copy(update={"additional_kwargs": {"knowledge_scope": None}})]
    assert scope_from_runtime(runtime) is None


def test_budgets_keep_whole_excerpts_and_matching_artifacts(tmp_path):
    result = {"chunks": [{"dataset_id": "d", "document_id": "doc", "id": str(i), "document_keyword": "book", "content": f"Evidence {i}: " + "x" * 150} for i in range(6)]}
    text, artifact = format_sources(result, dataset_names_by_id={"d": "D"}, max_chars_per_chunk=200, max_total_chars=500, outputs_path=str(tmp_path))
    sources = get_sources(artifact)
    assert 0 < len(sources) < 6 and len(text) <= 500
    assert len(list(tmp_path.glob("*.md"))) == len(sources)
    for source in sources:
        assert source["excerpt"] in text and source["id"] in text
    small, smaller = budget_sources(text, artifact, 100)
    assert len(small) <= 100 and not get_sources(smaller)
    msg = ToolMessage(content=text, name="knowledge_search", tool_call_id="tc", artifact=artifact)
    patched = _patch_tool_message(msg, ToolOutputConfig(externalize_min_chars=100, fallback_max_chars=100), None)
    assert len(patched.content) <= 100 and not get_sources(patched.artifact)
    wire = DeerFlowClient._tool_message_event(msg).data
    assert wire["knowledge_sources"] == sources
    assert DeerFlowClient._serialize_message(msg)["knowledge_sources"] == sources


async def test_document_validation_uses_pages_for_large_selection():
    requests = []

    def handler(request):
        requests.append(request)
        page = int(request.url.params["page"])
        docs = [{"id": f"doc{i}"} for i in range((page - 1) * 100, min(page * 100, 250))]
        return httpx.Response(200, json={"code": 0, "data": {"docs": docs, "total": 250}})

    client = RAGFlowClient(base_url="https://ragflow.test", api_key="k", transport=httpx.MockTransport(handler))
    await client.validate_documents("dataset", [f"doc{i}" for i in range(250)])
    assert len(requests) == 3
    assert all(len(str(request.url)) < 150 for request in requests)
