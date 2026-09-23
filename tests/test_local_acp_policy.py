from __future__ import annotations

import asyncio
from types import SimpleNamespace

import pytest
from acp import schema

from deerflow.acp.config import LocalACPConfig
from deerflow.acp.permission import ACPPermissionBroker
from deerflow.acp.policy import (
    SCHEDULED_TASK_TOOLS,
    LocalACPCapabilityPolicy,
    tool_kind,
)
from deerflow.acp.session_coordinator import ACPSessionCoordinator
from deerflow.agents.lead_agent.prompt import apply_prompt_template
from deerflow.client import DeerFlowClient
from deerflow.config.app_config import AppConfig, reset_app_config, set_app_config
from deerflow.config.sandbox_config import SandboxConfig
from deerflow.tools.builtins.tool_search import (
    DeferredToolRegistry,
    get_deferred_registry,
    reset_deferred_registry,
    set_deferred_registry,
    tool_search,
)


@pytest.fixture
def app_config():
    set_app_config(AppConfig(sandbox=SandboxConfig(use="deerflow.sandbox.local:LocalSandboxProvider")))
    try:
        yield
    finally:
        reset_app_config()


def _config(tmp_path, **overrides):
    values = {
        "config_path": tmp_path / "config.yaml",
        "checkpointer_path": tmp_path / "checkpoints.db",
        "session_store_path": tmp_path / "sessions.db",
    }
    values.update(overrides)
    return LocalACPConfig(**values)


def test_policy_keeps_prompt_tools_and_capabilities_consistent(tmp_path) -> None:
    policy = LocalACPCapabilityPolicy.from_config(
        _config(tmp_path, subagent_enabled=True, permission_mode="dangerous")
    )

    excluded = policy.excluded_tool_names(enable_bash=False)

    assert "task" not in excluded
    assert "bash" in excluded
    assert "invoke_acp_agent" in excluded
    assert SCHEDULED_TASK_TOOLS <= excluded
    assert "Internal DeerFlow subagents are available" in policy.prompt_overlay()
    assert policy.manifest(enable_bash=False)["session_close"] is True


def test_policy_disables_subagents_when_task_is_filtered(tmp_path) -> None:
    allowlist_policy = LocalACPCapabilityPolicy.from_config(
        _config(tmp_path, subagent_enabled=True, tool_allowlist=("read_file",))
    )
    denylist_policy = LocalACPCapabilityPolicy.from_config(
        _config(tmp_path, subagent_enabled=True, tool_denylist=("task",))
    )

    for policy in (allowlist_policy, denylist_policy):
        assert policy.subagents_enabled is False
        assert "task" in policy.excluded_tool_names(enable_bash=False)
        assert "Internal subagents are unavailable" in policy.prompt_overlay()


def test_tool_kind_matches_local_task_tools() -> None:
    assert tool_kind("ls") == "read"
    assert tool_kind("glob") == "search"
    assert tool_kind("str_replace") == "edit"
    assert tool_kind("web_search") == "fetch"


def test_permission_mode_all_includes_normally_safe_tools(tmp_path) -> None:
    policy = LocalACPCapabilityPolicy.from_config(
        _config(tmp_path, permission_mode="all")
    )

    assert policy.requires_permission("read_file") is True
    assert policy.requires_permission("task") is True


def test_prompt_omits_instructions_for_filtered_tools() -> None:
    prompt = apply_prompt_template(
        subagent_enabled=True,
        available_skills=set(),
        available_tool_names={"task"},
        current_date="2042-03-04, Tuesday",
    )

    assert "<subagent_system>" in prompt
    assert "<clarification_system>" not in prompt
    assert "`list_uploaded_files`" not in prompt
    assert "`read_file` tool" not in prompt
    assert "presented using `present_files`" not in prompt
    assert "web_search, web_fetch" not in prompt
    assert "invoke_acp_agent" not in prompt
    assert prompt.endswith("<current_date>2042-03-04, Tuesday</current_date>")


@pytest.mark.usefixtures("app_config")
def test_client_filters_deferred_registry_with_tool_policy(monkeypatch) -> None:
    allowed = SimpleNamespace(name="mcp_allowed", description="Allowed MCP tool")
    denied = SimpleNamespace(name="mcp_denied", description="Denied MCP tool")
    registry = DeferredToolRegistry()
    registry.register(allowed)
    registry.register(denied)
    set_deferred_registry(registry)
    monkeypatch.setattr(
        "deerflow.tools.get_available_tools",
        lambda **_kwargs: [tool_search, allowed, denied],
    )

    try:
        client = DeerFlowClient(excluded_tool_names={"mcp_denied"})
        tools = client._get_tools(model_name=None, subagent_enabled=False)

        assert [tool.name for tool in tools] == ["tool_search", "mcp_allowed"]
        filtered_registry = get_deferred_registry()
        assert filtered_registry is not None
        assert filtered_registry.deferred_names == {"mcp_allowed"}
    finally:
        reset_deferred_registry()


@pytest.mark.asyncio
async def test_permission_broker_supports_allow_always_per_session(tmp_path) -> None:
    policy = LocalACPCapabilityPolicy.from_config(
        _config(tmp_path, permission_mode="dangerous")
    )
    broker = ACPPermissionBroker(policy)
    calls: list[str] = []

    async def handler(options, session_id, tool_call):
        calls.append(tool_call.tool_call_id)
        allow_always = next(
            option for option in options if option.kind == "allow_always"
        )
        return schema.RequestPermissionResponse(
            outcome=schema.AllowedOutcome(
                outcome="selected", option_id=allow_always.option_id
            )
        )

    broker.bind(handler)

    assert await broker.request(
        "session-1", {"id": "call-1", "name": "bash", "args": {"command": "date"}}
    )
    assert await broker.request(
        "session-1", {"id": "call-2", "name": "bash", "args": {"command": "pwd"}}
    )
    assert await broker.request(
        "session-1", {"id": "call-3", "name": "read_file", "args": {"path": "x"}}
    )
    assert calls == ["call-1"]

    # Turn startup syncing the unchanged policy must retain per-tool approvals.
    broker.set_session_approval_mode("session-1", "ask")
    assert await broker.request("session-1", {"id": "same-mode", "name": "bash", "args": {}})
    assert calls == ["call-1"]

    # An explicit user selection of the same Ask value revokes legacy approvals.
    broker.set_session_approval_mode("session-1", "ask", reset_decisions=True)
    assert await broker.request("session-1", {"id": "explicit-ask", "name": "bash", "args": {}})
    assert calls == ["call-1", "explicit-ask"]

    # Changing the session-wide policy clears remembered per-tool decisions.
    broker.set_session_approval_mode("session-1", "allow_always")
    broker.set_session_approval_mode("session-1", "ask")
    assert await broker.request(
        "session-1", {"id": "call-4", "name": "bash", "args": {"command": "ls"}}
    )
    assert calls == ["call-1", "explicit-ask", "call-4"]


@pytest.mark.asyncio
async def test_permission_broker_supports_session_wildcard_modes(tmp_path) -> None:
    policy = LocalACPCapabilityPolicy.from_config(
        _config(tmp_path, permission_mode="dangerous")
    )
    broker = ACPPermissionBroker(policy)

    broker.set_session_approval_mode("session-1", "allow_always")
    assert broker.session_approval_mode("session-1") == "allow_always"
    assert await broker.request(
        "session-1", {"id": "call-1", "name": "bash", "args": {}}
    )
    assert broker.request_sync(
        "session-1", {"id": "call-2", "name": "write_file", "args": {}}
    )

    broker.set_session_approval_mode("session-1", "reject_always")
    assert not await broker.request(
        "session-1", {"id": "call-3", "name": "bash", "args": {}}
    )
    # Safe tools stay available under reject_always; only protected tools are gated.
    assert await broker.request(
        "session-1", {"id": "call-4", "name": "read_file", "args": {}}
    )

    broker.clear_session("session-1")
    assert broker.session_approval_mode("session-1") == "ask"


@pytest.mark.asyncio
async def test_permission_broker_routes_sync_subagent_calls_to_connection_loop(
    tmp_path,
) -> None:
    policy = LocalACPCapabilityPolicy.from_config(
        _config(tmp_path, permission_mode="dangerous")
    )
    broker = ACPPermissionBroker(policy)
    handler_loop = asyncio.get_running_loop()
    calls: list[tuple[str, asyncio.AbstractEventLoop]] = []

    async def handler(options, session_id, tool_call):
        calls.append((session_id, asyncio.get_running_loop()))
        allow_once = next(option for option in options if option.kind == "allow_once")
        return schema.RequestPermissionResponse(
            outcome=schema.AllowedOutcome(
                outcome="selected", option_id=allow_once.option_id
            )
        )

    broker.bind(handler)

    async def request_from_subagent_loop() -> bool:
        return broker.request_sync(
            "session-1",
            {"id": "call-1", "name": "write_file", "args": {"path": "x"}},
        )

    allowed = await asyncio.to_thread(lambda: asyncio.run(request_from_subagent_loop()))

    assert allowed is True
    assert calls == [("session-1", handler_loop)]


@pytest.mark.asyncio
async def test_permission_broker_routes_each_session_to_its_connection(
    tmp_path,
) -> None:
    policy = LocalACPCapabilityPolicy.from_config(
        _config(tmp_path, permission_mode="dangerous")
    )
    coordinator = ACPSessionCoordinator()
    coordinator.attach("session-a", "connection-a")
    coordinator.attach("session-b", "connection-b")
    broker = ACPPermissionBroker(policy, session_owner=coordinator.owner)
    calls: list[tuple[str, str]] = []

    def handler_for(connection_id: str):
        async def handler(options, session_id, tool_call):
            calls.append((connection_id, session_id))
            allow_once = next(
                option for option in options if option.kind == "allow_once"
            )
            return schema.RequestPermissionResponse(
                outcome=schema.AllowedOutcome(
                    outcome="selected", option_id=allow_once.option_id
                )
            )

        return handler

    broker.bind(handler_for("connection-a"), "connection-a")
    broker.bind(handler_for("connection-b"), "connection-b")

    assert await broker.request(
        "session-a", {"id": "call-a", "name": "bash", "args": {}}
    )
    assert await broker.request(
        "session-b", {"id": "call-b", "name": "bash", "args": {}}
    )
    broker.unbind("connection-a")
    assert not await broker.request(
        "session-a", {"id": "call-a2", "name": "bash", "args": {}}
    )
    assert await broker.request(
        "session-b", {"id": "call-b2", "name": "bash", "args": {}}
    )

    assert calls == [
        ("connection-a", "session-a"),
        ("connection-b", "session-b"),
        ("connection-b", "session-b"),
    ]


@pytest.mark.usefixtures("app_config")
def test_deerflow_client_applies_agent_profile_defaults(monkeypatch) -> None:
    profile = SimpleNamespace(
        model="profile-model",
        tool_groups=["web"],
        skills=["research"],
    )
    monkeypatch.setattr("deerflow.client.load_agent_config", lambda _name: profile)

    client = DeerFlowClient(agent_name="researcher")

    assert client._profile_model_name == "profile-model"
    assert client._tool_groups == ["web"]
    assert client._available_skills == {"research"}
