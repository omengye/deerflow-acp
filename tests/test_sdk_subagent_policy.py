import importlib
import json
from types import SimpleNamespace

import pytest
from langchain.agents.middleware.types import ModelRequest, ModelResponse
from langchain_core.messages import AIMessage, ToolMessage

from deerflow.agents.factory import create_deerflow_agent
from deerflow.agents.features import RuntimeFeatures
from deerflow.agents.middlewares.pii_redaction_middleware import PiiRedactionMiddleware
from deerflow.config.pii_redaction_config import PiiRedactionConfig
from deerflow.config.skills_config import SkillsConfig
from deerflow.subagents.config import SubagentConfig
from deerflow.subagents.executor import SubagentExecutor, SubagentStatus

task_module = importlib.import_module("deerflow.tools.builtins.task_tool")
executor_module = importlib.import_module("deerflow.subagents.executor")


@pytest.mark.parametrize("mode,allowed", [("on", ["allowed"]), ("off", ["allowed"]), ("on", [])])
async def test_sdk_native_task_keeps_explicit_catalog_scope_and_pii_across_forwarding(monkeypatch, tmp_path, mode, allowed):
    for name in ("allowed", "blocked"):
        directory = tmp_path / "public" / name
        directory.mkdir(parents=True)
        (directory / "SKILL.md").write_text(f"---\nname: {name}\ndescription: SDK {name} metadata\n---\nBody\n", encoding="utf-8")
    graph = {}
    monkeypatch.setattr("langchain.agents.create_agent", lambda **kwargs: graph.update(kwargs) or graph)
    pii = PiiRedactionConfig(enabled=True)
    skills = SkillsConfig(path=str(tmp_path), container_path="/sdk/skills", discovery_mode=mode)
    expected_names = list(allowed)
    create_deerflow_agent(
        model=object(), features=RuntimeFeatures(sandbox=False, subagent=True),
        skills_config=skills, available_skills=allowed, pii_redaction=pii,
    )
    task = next(tool for tool in graph["tools"] if tool.name == "task")
    assert "_sdk_policy" not in task.args_schema.model_json_schema()["properties"]
    # Post-construction mutation and runtime metadata must not relax the policy.
    allowed.append("blocked")
    skills.path = str(tmp_path / "wrong")
    pii.enabled = False
    captured = {}
    tool_options = {}

    class CaptureExecutor:
        def __init__(self, **kwargs):
            captured.update(kwargs)

        def execute_async(self, *_args, **_kwargs):
            return "sdk-child"

    def tools(**kwargs):
        tool_options.update(kwargs)
        return [SimpleNamespace(name="describe_skill")]

    monkeypatch.setattr(task_module, "get_available_subagent_names", lambda: ["general-purpose"])
    monkeypatch.setattr(task_module, "get_subagent_config", lambda _name: SubagentConfig(name="general-purpose", description="child", system_prompt="work"))
    monkeypatch.setattr(task_module, "SubagentExecutor", CaptureExecutor)
    monkeypatch.setattr("deerflow.tools.get_available_tools", tools)
    monkeypatch.setattr(task_module, "cleanup_background_task", lambda _id: None)
    monkeypatch.setattr(task_module, "get_background_task_result", lambda _id: SimpleNamespace(status=SubagentStatus.COMPLETED, result="done", error=None, ai_messages=[]))
    runtime = SimpleNamespace(
        state={"sandbox": {"available_skills": expected_names}}, context={},
        config={"metadata": {
            "available_skills": ["allowed", "blocked", "global-only"],
            "subagent_middlewares": [PiiRedactionMiddleware(PiiRedactionConfig(enabled=False))],
        }},
    )
    result = await task.coroutine(runtime=runtime, description="SDK task", prompt="work", subagent_type="general-purpose", tool_call_id="call")
    assert result == "Task Succeeded. Result: done"
    assert captured["config"].skills == expected_names
    assert [entry.name for entry in captured["skill_catalog"].entries] == expected_names
    assert captured["pii_redaction"].enabled
    assert tool_options["include_skill_tool"] is False
    describe = [tool for tool in captured["tools"] if tool.name == "describe_skill"]
    assert bool(describe) is bool(mode == "on" and expected_names)
    if describe:
        output = json.loads(describe[0].invoke({"query": "select:allowed,blocked,global-only"}))
        assert [entry["name"] for entry in output["skills"]] == expected_names
        assert output["skills"][0]["location"] == "/sdk/skills/public/allowed/SKILL.md"

    def forbidden(*_args, **_kwargs):
        raise AssertionError("SDK child tried to read global skill metadata")

    monkeypatch.setattr("deerflow.skills.loader.load_skills", forbidden)
    monkeypatch.setattr("deerflow.agents.lead_agent.prompt.get_skills_prompt_section", forbidden)
    child = SubagentExecutor(**captured)
    state = await child._build_initial_state("work")
    serialized = "\n".join(message.content for message in state["messages"])
    assert "blocked" not in serialized and "global-only" not in serialized
    assert ("SDK allowed metadata" in serialized) is bool(expected_names)
    assert "sandbox" not in state

    child_graph = {}
    monkeypatch.setattr(executor_module, "create_chat_model", lambda **_kwargs: object())
    monkeypatch.setattr(executor_module, "get_app_config", lambda: SimpleNamespace(get_model_config=lambda _name: None, get_default_model_name=lambda: "default", pii_redaction=PiiRedactionConfig()))
    monkeypatch.setattr("deerflow.agents.middlewares.tool_error_handling_middleware.build_subagent_runtime_middlewares", lambda **_kwargs: [])
    monkeypatch.setattr(executor_module, "create_agent", lambda **kwargs: child_graph.update(kwargs) or child_graph)
    child._create_agent()
    boundary = next(middleware for middleware in child_graph["middleware"] if isinstance(middleware, PiiRedactionMiddleware))
    seen = []
    boundary.wrap_model_call(
        ModelRequest(model=object(), messages=[ToolMessage(content="alice@example.com", tool_call_id="lookup")]),
        lambda request: seen.append(request) or ModelResponse(result=[AIMessage(content="done")]),
    )
    assert seen[0].messages[0].content == "[REDACTED_EMAIL]"


async def test_sdk_allowlist_without_explicit_catalog_fails_closed_for_child(monkeypatch):
    graph = {}
    monkeypatch.setattr("langchain.agents.create_agent", lambda **kwargs: graph.update(kwargs) or graph)
    create_deerflow_agent(model=object(), features=RuntimeFeatures(sandbox=False, subagent=True), available_skills=["same-name-global"])
    task = next(tool for tool in graph["tools"] if tool.name == "task")
    forwarded = {}

    class CaptureExecutor:
        def __init__(self, **kwargs):
            forwarded.update(kwargs)

        def execute_async(self, *_args, **_kwargs):
            return "child"

    monkeypatch.setattr(task_module, "get_available_subagent_names", lambda: ["general-purpose"])
    monkeypatch.setattr(task_module, "get_subagent_config", lambda _name: SubagentConfig(name="general-purpose", description="child", system_prompt="work"))
    monkeypatch.setattr(task_module, "SubagentExecutor", CaptureExecutor)
    monkeypatch.setattr("deerflow.tools.get_available_tools", lambda **_kwargs: [])
    monkeypatch.setattr(task_module, "cleanup_background_task", lambda _id: None)
    monkeypatch.setattr(task_module, "get_background_task_result", lambda _id: SimpleNamespace(status=SubagentStatus.COMPLETED, result="done", error=None, ai_messages=[]))
    await task.coroutine(runtime=SimpleNamespace(state={}, context={}, config={"metadata": {}}), description="SDK task", prompt="work", subagent_type="general-purpose", tool_call_id="call")
    assert forwarded["config"].skills == []
    assert forwarded["skill_catalog"].entries == ()
