import json
from types import SimpleNamespace

import pytest
from langchain.agents.middleware.types import ModelRequest, ModelResponse
from langchain_core.messages import AIMessage, HumanMessage, SystemMessage, ToolMessage
from langgraph.types import Command

from deerflow.agents.middlewares.pii_redaction_middleware import (
    PiiRedactionMiddleware,
    configure_pii_redaction,
    redact_messages,
    redact_text,
)
from deerflow.agents.middlewares.summarization_middleware import DeerFlowSummarizationMiddleware
from deerflow.agents.middlewares.title_middleware import TitleMiddleware
from deerflow.config.pii_redaction_config import PiiRedactionConfig

ENABLED = PiiRedactionConfig(enabled=True)


@pytest.mark.parametrize(
    "text, expected",
    [
        ("邮箱alice@example.com请联系", "邮箱[REDACTED_EMAIL]请联系"),
        ("call 13800138000 now", "call [REDACTED_PHONE] now"),
        ("电话13800138000请联系", "电话[REDACTED_PHONE]请联系"),
        ("手机+86 138-0013-8000", "手机[REDACTED_PHONE]"),
        ("Call +1 202 555 0101", "Call [REDACTED_PHONE]"),
        ("身份证11010519491231002X已核验", "身份证[REDACTED_CN_ID]已核验"),
        ("card 4111 1111 1111 1111", "card [REDACTED_BANK_CARD]"),
        ("key sk-abcdefghijklmnopqrstuvwxyz123456", "key [REDACTED_API_KEY]"),
        ('api_key="abcdefgh1234567890"', 'api_key="[REDACTED_API_KEY]"'),
        ('{"api_key": "dummyCredential1234567890"}', '{"api_key": "[REDACTED_API_KEY]"}'),
        ("Authorization: Bearer abcdefghijklmnop1234", "Authorization: Bearer [REDACTED_API_KEY]"),
        ("sk-4111111111111111abcdefgh", "[REDACTED_API_KEY]"),
    ],
)
def test_supported_sensitive_text(text, expected):
    assert redact_text(text, ENABLED) == expected
    assert redact_text(expected, ENABLED) == expected


@pytest.mark.parametrize("text", ["4111 1111 1111 1112", "0000000000000000", "abc13800138000xyz", "count 12345", "138001380001"])
def test_invalid_or_embedded_numbers_are_preserved(text):
    assert redact_text(text, ENABLED) == text


def test_id_checksum_and_birth_date_are_required_before_redaction():
    config = PiiRedactionConfig(enabled=True, bank_card=False, phone=False)
    for value in ("110105194912310021", "11010519491331002X"):
        assert redact_text(value, config) == value


def test_default_off_entity_switches_and_existing_placeholders():
    text = "[REDACTED_EMAIL] [EMAIL_1] alice@example.com 13800138000"
    assert redact_text(text, PiiRedactionConfig()) == text
    assert redact_text(text, PiiRedactionConfig(enabled=True, email=False)) == "[REDACTED_EMAIL] [EMAIL_1] alice@example.com [REDACTED_PHONE]"


@pytest.mark.parametrize("command_result", [False, True])
async def test_model_view_preserves_stored_user_and_tool_metadata_artifacts(command_result):
    artifact = {"type": "deerflow.knowledge_sources", "sources": [{"id": "source", "excerpt": "alice@example.com"}]}
    user = HumanMessage(content=[{"type": "text", "text": "alice@example.com"}, {"type": "image_url", "image_url": {"url": "data:image/png;base64,unchanged"}}], id="human")
    tool = ToolMessage(content="phone 13800138000", tool_call_id="call", id="tool", name="search", status="success", artifact=artifact, additional_kwargs={"receipt": "keep"}, response_metadata={"source": "keep"})
    command = Command(update={"messages": [tool], "other": "keep"}, goto="next") if command_result else None
    messages = [user, *(command.update["messages"] if command is not None else [tool])]
    request = ModelRequest(model=object(), messages=messages, state={"messages": messages}, system_message=SystemMessage(content="contact alice@example.com"))
    seen = []

    async def handler(updated):
        seen.append(updated)
        return ModelResponse(result=[AIMessage(content="done")])

    await PiiRedactionMiddleware(ENABLED).awrap_model_call(request, handler)
    updated = seen[0]
    assert updated.messages[0].content[0]["text"] == "[REDACTED_EMAIL]"
    assert updated.messages[0].content[1] == user.content[1]
    assert updated.messages[1].content == "phone [REDACTED_PHONE]"
    assert updated.messages[1].artifact is artifact
    assert updated.messages[1].additional_kwargs == tool.additional_kwargs
    assert updated.messages[1].response_metadata == tool.response_metadata
    assert updated.messages[1].tool_call_id == "call"
    assert updated.messages[1].id == "tool"
    assert updated.messages[1].status == "success"
    assert updated.system_message.content == "contact [REDACTED_EMAIL]"
    assert request.messages is messages
    assert request.state["messages"][0].content[0]["text"] == "alice@example.com"
    assert tool.content == "phone 13800138000"
    assert artifact["sources"][0]["excerpt"] == "alice@example.com"
    if command is not None:
        assert command.update["messages"][0] is tool
        assert command.goto == "next"
        assert command.update["other"] == "keep"


def test_replayed_tool_arguments_and_reasoning_are_redacted_only_in_model_copy():
    raw_arguments = json.dumps({"email": "alice@example.com", "phone": 13800138000})
    message = AIMessage(
        content=[{"type": "tool_use", "id": "call", "name": "lookup", "input": {"email": "alice@example.com"}}],
        tool_calls=[{"id": "call", "name": "lookup", "args": {"email": "alice@example.com", "phone": 13800138000}}],
        additional_kwargs={
            "reasoning_content": "contact alice@example.com",
            "tool_calls": [{"id": "call", "type": "function", "function": {"name": "lookup", "arguments": raw_arguments}}],
            "function_call": {"name": "lookup", "arguments": raw_arguments},
            "signature": "opaque",
        },
    )
    request = ModelRequest(model=object(), messages=[message])
    captured = []
    PiiRedactionMiddleware(ENABLED).wrap_model_call(request, lambda value: captured.append(value) or ModelResponse(result=[]))
    updated = captured[0].messages[0]
    assert updated.tool_calls[0]["args"] == {"email": "[REDACTED_EMAIL]", "phone": "[REDACTED_PHONE]"}
    assert updated.content[0]["input"]["email"] == "[REDACTED_EMAIL]"
    assert "alice@example.com" not in updated.additional_kwargs["tool_calls"][0]["function"]["arguments"]
    assert "13800138000" not in updated.additional_kwargs["function_call"]["arguments"]
    assert updated.additional_kwargs["reasoning_content"] == "contact [REDACTED_EMAIL]"
    assert updated.additional_kwargs["signature"] == "opaque"
    assert message.tool_calls[0]["args"]["phone"] == 13800138000
    assert message.additional_kwargs["function_call"]["arguments"] == raw_arguments


def test_disabled_middleware_passes_original_request_through():
    request = ModelRequest(model=object(), messages=[HumanMessage(content="alice@example.com")])
    captured = []
    PiiRedactionMiddleware().wrap_model_call(request, lambda value: captured.append(value) or ModelResponse(result=[]))
    assert captured == [request]


def test_structured_secret_fields_and_responses_reasoning_summary_are_redacted():
    source = AIMessage(
        content=[{"type": "reasoning", "summary": [{"type": "summary_text", "text": "alice@example.com"}], "encrypted_content": "opaque"}],
        tool_calls=[{"id": "call", "name": "lookup", "args": {"api_key": "dummyCredential1234567890", "nested": {"access_token": "dummyAccessToken12345"}}}],
    )
    updated = redact_messages([source], ENABLED)[0]
    assert updated.content[0]["summary"][0]["text"] == "[REDACTED_EMAIL]"
    assert updated.content[0]["encrypted_content"] == "opaque"
    assert updated.tool_calls[0]["args"] == {"api_key": "[REDACTED_API_KEY]", "nested": {"access_token": "[REDACTED_API_KEY]"}}
    assert source.tool_calls[0]["args"]["api_key"] == "dummyCredential1234567890"
    assert source.content[0]["summary"][0]["text"] == "alice@example.com"


@pytest.mark.parametrize("use_async", [False, True])
async def test_source_budget_rebuild_is_redacted_at_final_model_boundary(use_async):
    from langchain.agents import create_agent
    from langchain_core.language_models.fake_chat_models import FakeMessagesListChatModel

    from deerflow.agents.middlewares.tool_output_budget_middleware import ToolOutputBudgetMiddleware
    from deerflow.community.ragflow.sources import source_artifact, source_block
    from deerflow.config.tool_output_config import ToolOutputConfig

    seen = []

    class CaptureModel(FakeMessagesListChatModel):
        def _generate(self, messages, *args, **kwargs):
            seen.append(messages)
            return super()._generate(messages, *args, **kwargs)

    first = {"id": "a" * 32, "document_name": "Contacts", "excerpt": "Email alice@example.com; phone 13800138000."}
    second = {"id": "b" * 32, "document_name": "Long appendix", "excerpt": "Long text " * 100}
    artifact = source_artifact([first, second])
    original_text = "\n\n".join(source_block(source) for source in (first, second))
    tool_message = ToolMessage(content=original_text, tool_call_id="lookup", name="search_knowledge", artifact=artifact)
    messages = [
        HumanMessage(content="Find contact details"),
        AIMessage(content="", tool_calls=[{"id": "lookup", "name": "search_knowledge", "args": {}}]),
        tool_message,
    ]
    budget = ToolOutputBudgetMiddleware(ToolOutputConfig(externalize_min_chars=500, fallback_max_chars=500))
    agent = create_agent(
        model=CaptureModel(responses=[AIMessage(content="done")]),
        middleware=configure_pii_redaction([budget], ENABLED),
    )
    result = await agent.ainvoke({"messages": messages}) if use_async else agent.invoke({"messages": messages})
    model_tool = next(message for message in seen[0] if isinstance(message, ToolMessage))
    assert "[REDACTED_EMAIL]" in model_tool.content
    assert "[REDACTED_PHONE]" in model_tool.content
    assert "alice@example.com" not in model_tool.content
    assert "13800138000" not in model_tool.content
    assert "Additional sources omitted" in model_tool.content
    assert model_tool.artifact["sources"] == [first]
    # The model boundary filters the rebuilt text, while citation evidence and
    # persisted message history retain the actual retrieved source verbatim.
    stored_tool = next(message for message in result["messages"] if isinstance(message, ToolMessage))
    assert stored_tool.content == original_text
    assert stored_tool.artifact == artifact
    assert artifact["sources"][0]["excerpt"] == first["excerpt"]
    assert tool_message.content == original_text


class _Model:
    def __init__(self):
        self.prompts = []

    def invoke(self, prompt, **_kwargs):
        self.prompts.append(prompt)
        return AIMessage(content="A summary")

    async def ainvoke(self, prompt, **_kwargs):
        return self.invoke(prompt, **_kwargs)


async def test_summary_primary_fallback_and_title_bypass_use_redacted_prompts(monkeypatch):
    model = _Model()
    summary = DeerFlowSummarizationMiddleware(model=model, token_counter=lambda messages: len(messages), trim_tokens_to_summarize=None, pii_redaction=ENABLED)
    messages = [HumanMessage(content="alice@example.com 13800138000"), AIMessage(content="lookup complete")]
    fallback = _Model()
    summary._create_summary(messages)
    await summary._acreate_summary(messages)
    summary._create_summary(messages, model=fallback)
    await summary._acreate_summary(messages, model=fallback)
    for prompt in model.prompts + fallback.prompts:
        assert "alice@example.com" not in prompt
        assert "13800138000" not in prompt
        assert "[REDACTED_EMAIL]" in prompt
    assert messages[0].content == "alice@example.com 13800138000"
    monkeypatch.setattr("deerflow.agents.middlewares.title_middleware.create_chat_model", lambda **_kwargs: model)
    from deerflow.config.title_config import TitleConfig

    monkeypatch.setattr("deerflow.agents.middlewares.title_middleware.get_title_config", lambda: TitleConfig())
    await TitleMiddleware(pii_redaction=ENABLED)._agenerate_title_result({"messages": messages})
    assert "alice@example.com" not in model.prompts[-1]
    assert "[REDACTED_EMAIL]" in model.prompts[-1]


def test_chain_configures_auxiliary_copies_without_mutating_caller_instances():
    from deerflow.agents.middlewares.clarification_middleware import ClarificationMiddleware

    title = TitleMiddleware()
    summary = DeerFlowSummarizationMiddleware(model=_Model(), token_counter=lambda messages: len(messages))
    original = [title, summary, ClarificationMiddleware()]
    updated = configure_pii_redaction(original, ENABLED)
    assert isinstance(updated[-2], PiiRedactionMiddleware)
    assert isinstance(updated[-1], ClarificationMiddleware)
    assert updated[0].pii_redaction.enabled
    assert updated[1].pii_redaction.enabled
    assert not title.pii_redaction.enabled
    assert not summary.pii_redaction.enabled
    assert original == [title, summary, original[-1]]


def test_sdk_explicit_pii_and_skill_discovery_do_not_load_global_config(monkeypatch, tmp_path):
    from deerflow.agents.factory import create_deerflow_agent
    from deerflow.agents.features import RuntimeFeatures
    from deerflow.config.skills_config import SkillsConfig

    def forbidden(*_args, **_kwargs):
        raise AssertionError("SDK factory loaded global config")

    captured = {}
    monkeypatch.setattr("deerflow.config.get_app_config", forbidden)
    monkeypatch.setattr("langchain.agents.create_agent", lambda **kwargs: captured.update(kwargs) or captured)
    observed_scopes = []
    tool = SimpleNamespace(name="describe_skill")
    monkeypatch.setattr("deerflow.skills.describe.build_describe_skill_tool", lambda scope, **kwargs: observed_scopes.append(scope) or tool)
    create_deerflow_agent(model=object(), features=RuntimeFeatures(sandbox=False, auto_title=True), pii_redaction=ENABLED, skills_config=SkillsConfig(path=str(tmp_path), discovery_mode="on"), available_skills=[])
    assert observed_scopes == [set()]
    assert captured["tools"].count(tool) == 1
    assert any(isinstance(middleware, PiiRedactionMiddleware) for middleware in captured["middleware"])
    assert next(middleware for middleware in captured["middleware"] if isinstance(middleware, TitleMiddleware)).pii_redaction.enabled
    create_deerflow_agent(model=object(), tools=[tool], features=RuntimeFeatures(sandbox=False), skills_config=SkillsConfig(discovery_mode="on"))
    assert observed_scopes == [set()]
    assert captured["tools"].count(tool) == 1


def test_sdk_full_takeover_requires_explicit_middleware():
    from deerflow.agents.factory import create_deerflow_agent

    with pytest.raises(ValueError, match="full middleware takeover"):
        create_deerflow_agent(model=object(), middleware=[], pii_redaction=ENABLED)
