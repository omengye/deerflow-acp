from __future__ import annotations

import asyncio
import base64
import json
import os
import sqlite3
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import acp
import pytest
from acp import RequestError, schema

from deerflow.acp.agent import DeerFlowACPAgent
from deerflow.acp.config import LocalACPConfig
from deerflow.acp.event_mapper import ACPEventMapper
from deerflow.acp.session_store import LocalACPSessionStore


class FakeConnection:
    def __init__(self) -> None:
        self.updates: list[tuple[str, Any]] = []

    async def session_update(self, session_id: str, update: Any) -> None:
        self.updates.append((session_id, update))


class FakeRuntime:
    def __init__(self, events: list[Any] | None = None) -> None:
        self.events = events or []
        self.history_messages: list[dict[str, Any]] = []
        self.started = asyncio.Event()
        self.block = False
        self.client_mcp_bindings: dict[str, Any] = {}
        self.released_client_mcp: list[str] = []
        self.prompt_messages: list[str] = []
        self.prompt_images: list[list[dict[str, str | int]]] = []
        self.goal: dict[str, Any] | None = None
        self.goal_set_calls: list[tuple[str, str]] = []
        self.goal_clear_calls: list[str] = []

    async def bind_client_mcp(self, session_id: str, binding: Any) -> None:
        if binding is None:
            self.client_mcp_bindings.pop(session_id, None)
        else:
            self.client_mcp_bindings[session_id] = binding

    async def release_client_mcp(self, session_id: str) -> None:
        self.client_mcp_bindings.pop(session_id, None)
        self.released_client_mcp.append(session_id)

    async def get_goal(self, session_id: str) -> dict[str, Any] | None:
        del session_id
        return self.goal

    async def set_goal(self, session_id: str, objective: str) -> dict[str, Any]:
        normalized = " ".join(objective.strip().split())
        self.goal_set_calls.append((session_id, normalized))
        self.goal = {
            "objective": normalized,
            "auto_continue": False,
            "continuation_count": 0,
            "max_continuations": 3,
        }
        return self.goal

    async def clear_goal(self, session_id: str) -> None:
        self.goal_clear_calls.append(session_id)
        self.goal = None

    async def astream(
        self,
        session: Any,
        message: str,
        *,
        live_event_callback: Any,
        input_images: list[dict[str, str | int]] | None = None,
    ):
        del session
        self.prompt_messages.append(message)
        self.prompt_images.append(input_images or [])
        self.started.set()
        if self.block:
            await asyncio.Event().wait()
        for event in self.events:
            if isinstance(event, dict) and event.get("live"):
                await live_event_callback(event["data"])
            else:
                yield event

    async def history(self, session_id: str) -> list[dict[str, Any]]:
        del session_id
        return self.history_messages

    async def history_state(self, session_id: str) -> dict[str, Any]:
        del session_id
        return {
            "messages": self.history_messages,
            "todos": [],
            "artifacts": [],
            "title": None,
            "goal": self.goal,
        }


def make_config(tmp_path: Path, **overrides: Any) -> LocalACPConfig:
    values: dict[str, Any] = {
        "config_path": tmp_path / "config.yaml",
        "checkpointer_path": tmp_path / "checkpoints.db",
        "session_store_path": tmp_path / "sessions.db",
        "run_timeout_seconds": 10,
        "session_page_size": 2,
    }
    values.update(overrides)
    return LocalACPConfig(**values)


@pytest.fixture(autouse=True)
def configured_models(monkeypatch: pytest.MonkeyPatch) -> SimpleNamespace:
    models = [
        SimpleNamespace(
            name="model-a",
            display_name="Model A",
            description="First test model",
            supports_vision=False,
        ),
        SimpleNamespace(
            name="model-b",
            display_name=None,
            description=None,
            supports_vision=False,
        ),
    ]
    app_config = SimpleNamespace(
        models=models,
        get_default_model_name=lambda: "model-b",
        get_model_config=lambda name: next(
            (model for model in models if model.name == name),
            None,
        ),
    )
    monkeypatch.setattr("deerflow.acp.agent.get_app_config", lambda: app_config)
    return app_config


def test_config_paths_do_not_depend_on_client_cwd(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config_dir = tmp_path / "deerflow-config"
    config_dir.mkdir()
    config_path = config_dir / "config.yaml"
    config_path.write_text(
        """
api:
  data_dir: ./state
  deerflow_home: ./state/home
local_acp:
  checkpointer_path: ./state/checkpoints.db
  session_store_path: ./state/sessions.db
  max_active_connections: 7
  thinking_enabled: false
  subagent_enabled: true
  max_concurrent_subagents: 2
  permission_mode: all
  tool_allowlist: [read_file, task]
  tool_denylist: [web_fetch]
  memory_scope: session
  prompt_overlay: Server-owned instruction
  resource_link_max_size_mb: 7
  session_cleanup_enabled: false
  inactive_session_retention_days: 14
  session_cleanup_interval_seconds: 120
  closed_session_retention_days: 0
  enable_bash: true
  accept_client_mcp_servers: true
""",
        encoding="utf-8",
    )
    client_workspace = tmp_path / "client-workspace"
    client_workspace.mkdir()
    monkeypatch.chdir(client_workspace)
    monkeypatch.delenv("DEER_FLOW_CONFIG_PATH", raising=False)
    monkeypatch.delenv("DEER_FLOW_HOME", raising=False)
    monkeypatch.delenv("DEER_FLOW_ACP_ENABLE_BASH", raising=False)

    config = LocalACPConfig.from_file(str(config_path))

    assert config.checkpointer_path == config_dir / "state" / "checkpoints.db"
    assert config.session_store_path == config_dir / "state" / "sessions.db"
    assert config.max_active_connections == 7
    assert config.deerflow_home == config_dir / "state" / "home"
    assert config.thinking_enabled is False
    assert config.subagent_enabled is True
    assert config.max_concurrent_subagents == 2
    assert config.permission_mode == "all"
    assert config.tool_allowlist == ("read_file", "task")
    assert config.tool_denylist == ("web_fetch",)
    assert config.memory_scope == "session"
    assert config.prompt_overlay == "Server-owned instruction"
    assert config.resource_link_max_size_bytes == 7 * 1024 * 1024
    assert config.session_cleanup_enabled is False
    assert config.inactive_session_retention_days == 14
    assert config.session_cleanup_interval_seconds == 120
    assert config.closed_session_retention_days == 0
    assert config.enable_bash is True
    assert config.accept_client_mcp_servers is True
    assert "DEER_FLOW_CONFIG_PATH" not in os.environ


def test_config_accepts_unquoted_yaml_off_permission_mode(tmp_path: Path) -> None:
    config_path = tmp_path / "config.yaml"
    config_path.write_text(
        """
local_acp:
  permission_mode: off
""",
        encoding="utf-8",
    )

    config = LocalACPConfig.from_file(str(config_path))

    assert config.permission_mode == "off"


def test_config_loads_optional_rustfs_artifact_settings(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config_path = tmp_path / "config.yaml"
    config_path.write_text(
        """
api:
  data_dir: ./data
local_acp:
  artifacts:
    enabled: true
    endpoint_url: https://rustfs.example.test
    bucket: deerflow-acp
    prefix: test-artifacts
    max_file_size_mb: 12
""",
        encoding="utf-8",
    )
    monkeypatch.setenv("DEER_FLOW_ACP_ARTIFACT_ACCESS_KEY", "access")
    monkeypatch.setenv("DEER_FLOW_ACP_ARTIFACT_SECRET_KEY", "secret")

    config = LocalACPConfig.from_file(str(config_path))

    assert config.artifacts is not None
    assert config.artifacts.endpoint_url == "https://rustfs.example.test"
    assert config.artifacts.bucket == "deerflow-acp"
    assert config.artifacts.prefix == "test-artifacts"
    assert config.artifacts.max_file_size_bytes == 12 * 1024 * 1024


def test_config_loads_artifact_credentials_from_sibling_dotenv(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config_path = tmp_path / "config.yaml"
    config_path.write_text(
        """
local_acp:
  artifacts:
    enabled: true
    endpoint_url: https://rustfs.example.test
    bucket: deerflow-acp
""",
        encoding="utf-8",
    )
    (tmp_path / ".env").write_text(
        "DEER_FLOW_ACP_ARTIFACT_ACCESS_KEY=dotenv-access\n"
        "DEER_FLOW_ACP_ARTIFACT_SECRET_KEY=dotenv-secret\n",
        encoding="utf-8",
    )
    monkeypatch.delenv("DEER_FLOW_ACP_ARTIFACT_ACCESS_KEY", raising=False)
    monkeypatch.delenv("DEER_FLOW_ACP_ARTIFACT_SECRET_KEY", raising=False)

    config = LocalACPConfig.from_file(str(config_path))

    assert config.artifacts is not None
    assert config.artifacts.access_key == "dotenv-access"
    assert config.artifacts.secret_key == "dotenv-secret"


@pytest.fixture
def store(tmp_path: Path):
    result = LocalACPSessionStore(tmp_path / "sessions.db")
    result.setup()
    try:
        yield result
    finally:
        result.close()


@pytest.mark.asyncio
async def test_initialize_advertises_text_only_stable_capabilities(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    agent = DeerFlowACPAgent(make_config(tmp_path), store, FakeRuntime())

    response = await agent.initialize(protocol_version=1)

    assert response.protocol_version == 1
    assert response.agent_capabilities.load_session is True
    assert response.agent_capabilities.session_capabilities.close is not None
    assert response.agent_capabilities.prompt_capabilities.image is False
    assert response.agent_capabilities.prompt_capabilities.audio is False
    assert response.agent_capabilities.prompt_capabilities.embedded_context is False
    assert response.agent_capabilities.mcp_capabilities.http is False
    assert response.agent_capabilities.mcp_capabilities.sse is False
    assert response.agent_capabilities.session_capabilities.list is not None
    assert response.agent_capabilities.session_capabilities.resume is None
    assert response.agent_capabilities.session_capabilities.fork is None
    assert response.auth_methods == []


@pytest.mark.asyncio
async def test_initialize_advertises_remote_transports_when_client_mcp_is_enabled(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    agent = DeerFlowACPAgent(
        make_config(tmp_path, accept_client_mcp_servers=True),
        store,
        FakeRuntime(),
    )

    response = await agent.initialize(protocol_version=1)

    assert response.agent_capabilities.mcp_capabilities.http is True
    assert response.agent_capabilities.mcp_capabilities.sse is True


@pytest.mark.asyncio
async def test_initialize_advertises_images_when_a_vision_model_is_configured(
    tmp_path: Path,
    store: LocalACPSessionStore,
    configured_models: SimpleNamespace,
) -> None:
    configured_models.models[0].supports_vision = True
    agent = DeerFlowACPAgent(make_config(tmp_path), store, FakeRuntime())

    response = await agent.initialize(protocol_version=1)

    assert response.agent_capabilities.prompt_capabilities.image is True


@pytest.mark.asyncio
async def test_new_session_rejects_client_directories_and_mcp(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    agent = DeerFlowACPAgent(make_config(tmp_path), store, FakeRuntime())

    with pytest.raises(RequestError) as directories_error:
        await agent.new_session(
            cwd=str(tmp_path), additional_directories=[str(tmp_path / "other")]
        )
    assert directories_error.value.code == -32602

    with pytest.raises(RequestError) as mcp_error:
        await agent.new_session(cwd=str(tmp_path), mcp_servers=[object()])
    assert mcp_error.value.code == -32602

    response = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])
    session = await store.get(response.session_id)
    assert session is not None
    assert session.cwd == str(tmp_path)
    assert response.modes.current_mode_id == "plan"
    assert {option.id for option in response.config_options or []} == {
        "model",
        "thinking_enabled",
        "tool_approval",
    }
    assert all(option.type == "select" for option in response.config_options or [])
    model_option = next(
        option for option in response.config_options or [] if option.id == "model"
    )
    assert model_option.current_value == "model-b"
    assert [(item.name, item.value) for item in model_option.options] == [
        ("Model A", "model-a"),
        ("model-b", "model-b"),
    ]
    thinking_option = next(
        option
        for option in response.config_options or []
        if option.id == "thinking_enabled"
    )
    assert {item.value for item in thinking_option.options} == {"on", "off"}


@pytest.mark.asyncio
async def test_session_cwd_must_be_an_existing_absolute_directory(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    agent = DeerFlowACPAgent(make_config(tmp_path), store, FakeRuntime())
    regular_file = tmp_path / "not-a-directory.txt"
    regular_file.write_text("x", encoding="utf-8")

    for invalid_cwd in (
        "relative/project",
        str(tmp_path / "missing"),
        str(regular_file),
    ):
        with pytest.raises(RequestError) as error:
            await agent.new_session(cwd=invalid_cwd, mcp_servers=[])
        assert error.value.code == -32602


@pytest.mark.asyncio
async def test_load_session_rejects_a_different_workspace(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    other = tmp_path / "other"
    other.mkdir()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, FakeRuntime())
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    with pytest.raises(RequestError) as error:
        await agent.load_session(
            cwd=str(other),
            session_id=created.session_id,
            mcp_servers=[],
        )

    assert error.value.code == -32602
    assert "does not match cwd" in str(error.value.data)


@pytest.mark.asyncio
async def test_load_session_canonicalizes_a_legacy_stored_workspace(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    child = tmp_path / "child"
    child.mkdir()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, FakeRuntime())
    created = await store.create(
        cwd=str(child / ".."),
        defaults=agent._defaults(),
    )

    await agent.load_session(
        cwd=str(tmp_path),
        session_id=created.session_id,
        mcp_servers=[],
    )

    loaded = await store.get(created.session_id)
    assert loaded is not None
    assert loaded.cwd == str(tmp_path.resolve())


@pytest.mark.asyncio
async def test_list_sessions_normalizes_cwd_before_filtering(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    agent = DeerFlowACPAgent(make_config(tmp_path), store, FakeRuntime())
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    listed = await agent.list_sessions(cwd=str(tmp_path) + os.sep + ".")

    assert [item.session_id for item in listed.sessions] == [created.session_id]


@pytest.mark.asyncio
async def test_trusted_stdio_client_mcp_is_session_scoped_and_released(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    runtime = FakeRuntime()
    agent = DeerFlowACPAgent(
        make_config(tmp_path, accept_client_mcp_servers=True),
        store,
        runtime,
    )
    server = schema.McpServerStdio(
        name="codeg",
        command="codeg-mcp",
        args=["--stdio"],
        env=[schema.EnvVariable(name="CODEG_SESSION", value="local")],
    )

    created = await agent.new_session(
        cwd=str(tmp_path),
        mcp_servers=[server],
    )

    binding = runtime.client_mcp_bindings[created.session_id]
    config = binding.extensions_config.mcp_servers["codeg"]
    assert config.type == "stdio"
    assert config.command == "codeg-mcp"
    assert config.args == ["--stdio"]
    assert config.env == {"CODEG_SESSION": "local"}

    await agent.shutdown()
    assert created.session_id not in runtime.client_mcp_bindings
    assert runtime.released_client_mcp == [created.session_id]


@pytest.mark.asyncio
async def test_trusted_sse_client_mcp_is_session_scoped_and_preserves_headers(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    runtime = FakeRuntime()
    agent = DeerFlowACPAgent(
        make_config(tmp_path, accept_client_mcp_servers=True),
        store,
        runtime,
    )
    server = schema.SseMcpServer(
        type="sse",
        name="remote",
        url="https://example.test/mcp/sse",
        headers=[schema.HttpHeader(name="Authorization", value="Bearer test-token")],
    )

    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[server])

    binding = runtime.client_mcp_bindings[created.session_id]
    config = binding.extensions_config.mcp_servers["remote"]
    assert config.type == "sse"
    assert config.url == "https://example.test/mcp/sse"
    assert config.headers == {"Authorization": "Bearer test-token"}
    assert config.tool_name_prefix is True

    await agent.shutdown()
    assert created.session_id not in runtime.client_mcp_bindings
    assert runtime.released_client_mcp == [created.session_id]


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "server",
    [
        schema.McpServerStdio(
            name="local",
            command="example-mcp",
            args=[],
            env=[],
        ),
        schema.SseMcpServer(
            type="sse",
            name="sse-remote",
            url="https://example.test/mcp/sse",
            headers=[],
        ),
        schema.HttpMcpServer(
            type="http",
            name="http-remote",
            url="https://example.test/mcp",
            headers=[],
        ),
    ],
    ids=["stdio", "sse", "http"],
)
async def test_client_mcp_transports_require_master_switch(
    tmp_path: Path,
    store: LocalACPSessionStore,
    server: Any,
) -> None:
    agent = DeerFlowACPAgent(make_config(tmp_path), store, FakeRuntime())

    with pytest.raises(RequestError) as error:
        await agent.new_session(cwd=str(tmp_path), mcp_servers=[server])

    assert error.value.code == -32602
    assert "accept_client_mcp_servers" in str(error.value.data)


@pytest.mark.asyncio
async def test_trusted_http_client_mcp_is_session_scoped_and_preserves_headers(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    runtime = FakeRuntime()
    agent = DeerFlowACPAgent(
        make_config(tmp_path, accept_client_mcp_servers=True),
        store,
        runtime,
    )
    server = schema.HttpMcpServer(
        type="http",
        name="remote",
        url="https://example.test/mcp",
        headers=[schema.HttpHeader(name="Authorization", value="Bearer test-token")],
    )

    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[server])

    binding = runtime.client_mcp_bindings[created.session_id]
    config = binding.extensions_config.mcp_servers["remote"]
    assert config.type == "http"
    assert config.url == "https://example.test/mcp"
    assert config.headers == {"Authorization": "Bearer test-token"}
    assert config.tool_name_prefix is True

    await agent.shutdown()
    assert created.session_id not in runtime.client_mcp_bindings
    assert runtime.released_client_mcp == [created.session_id]


@pytest.mark.asyncio
@pytest.mark.parametrize("transport", ["sse", "http"])
@pytest.mark.parametrize(
    ("url", "headers", "message"),
    [
        ("file:///tmp/mcp", [], "valid http(s) URL"),
        (
            "https://example.test/mcp/sse",
            [
                schema.HttpHeader(name="Authorization", value="one"),
                schema.HttpHeader(name="authorization", value="two"),
            ],
            "duplicate HTTP header",
        ),
    ],
)
async def test_trusted_remote_client_mcp_rejects_invalid_config(
    tmp_path: Path,
    store: LocalACPSessionStore,
    transport: str,
    url: str,
    headers: list[schema.HttpHeader],
    message: str,
) -> None:
    agent = DeerFlowACPAgent(
        make_config(tmp_path, accept_client_mcp_servers=True),
        store,
        FakeRuntime(),
    )
    if transport == "sse":
        server: Any = schema.SseMcpServer(
            type="sse",
            name="remote",
            url=url,
            headers=headers,
        )
    else:
        server = schema.HttpMcpServer(
            type="http",
            name="remote",
            url=url,
            headers=headers,
        )

    with pytest.raises(RequestError) as error:
        await agent.new_session(cwd=str(tmp_path), mcp_servers=[server])

    assert error.value.code == -32602
    assert message in str(error.value.data)


@pytest.mark.asyncio
async def test_session_modes_config_list_and_load_history(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    runtime = FakeRuntime()
    runtime.history_messages = [
        {"type": "human", "id": "user-1", "content": "question"},
        {
            "type": "ai",
            "id": "agent-1",
            "content": "answer",
            "reasoning_content": "thought",
        },
        {"type": "tool", "id": "tool-1", "content": "hidden"},
    ]
    connection = FakeConnection()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    agent.on_connect(connection)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    approval_option = next(
        option
        for option in created.config_options or []
        if option.id == "tool_approval"
    )
    assert approval_option.category == "_permissions"
    assert approval_option.current_value == "ask"
    assert [option.value for option in approval_option.options] == [
        "ask",
        "allow_always",
        "reject_always",
    ]

    await agent.set_session_mode("default", created.session_id)
    options = await agent.set_config_option(
        "thinking_enabled", created.session_id, "off"
    )
    assert (
        next(
            option
            for option in options.config_options
            if option.id == "thinking_enabled"
        ).current_value
        == "off"
    )
    options = await agent.set_config_option("model", created.session_id, "model-a")
    assert (
        next(
            option for option in options.config_options if option.id == "model"
        ).current_value
        == "model-a"
    )
    options = await agent.set_config_option(
        "tool_approval",
        created.session_id,
        "allow_always",
    )
    assert (
        next(
            option
            for option in options.config_options
            if option.id == "tool_approval"
        ).current_value
        == "allow_always"
    )
    stored = await store.get(created.session_id)
    assert stored is not None
    assert stored.model_name == "model-a"
    assert stored.approval_mode == "allow_always"
    with pytest.raises(RequestError) as model_error:
        await agent.set_config_option("model", created.session_id, "missing-model")
    assert model_error.value.code == -32602
    with pytest.raises(RequestError):
        await agent.set_config_option("subagent_enabled", created.session_id, "on")
    with pytest.raises(RequestError) as approval_error:
        await agent.set_config_option(
            "tool_approval",
            created.session_id,
            "allow_everything_forever",
        )
    assert approval_error.value.code == -32602

    listed = await agent.list_sessions(cwd=str(tmp_path))
    assert [item.session_id for item in listed.sessions] == [created.session_id]

    loaded = await agent.load_session(
        cwd=str(tmp_path),
        session_id=created.session_id,
        mcp_servers=[],
    )
    assert loaded.modes.current_mode_id == "default"
    assert (
        next(
            option for option in loaded.config_options or [] if option.id == "model"
        ).current_value
        == "model-a"
    )
    assert (
        next(
            option
            for option in loaded.config_options or []
            if option.id == "tool_approval"
        ).current_value
        == "allow_always"
    )
    assert [type(update) for _, update in connection.updates] == [
        schema.UserMessageChunk,
        schema.AgentThoughtChunk,
        schema.AgentMessageChunk,
        schema.ToolCallStart,
        schema.ToolCallProgress,
    ]


@pytest.mark.asyncio
async def test_tool_approval_option_is_hidden_when_permissions_are_off(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    agent = DeerFlowACPAgent(
        make_config(tmp_path, permission_mode="off"),
        store,
        FakeRuntime(),
    )
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    assert "tool_approval" not in {
        option.id for option in created.config_options or []
    }
    with pytest.raises(RequestError) as error:
        await agent.set_config_option(
            "tool_approval",
            created.session_id,
            "allow_always",
        )
    assert error.value.code == -32602


@pytest.mark.asyncio
async def test_prompt_maps_events_usage_and_title(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    events = [
        SimpleNamespace(
            type="messages-tuple",
            data={
                "type": "ai",
                "id": "msg-1",
                "content": "hello",
                "reasoning_content": "think",
            },
        ),
        SimpleNamespace(
            type="messages-tuple",
            data={
                "type": "ai",
                "id": "msg-1",
                "content": "",
                "tool_calls": [
                    {"id": "call-1", "name": "web_search", "args": {"q": "x"}}
                ],
            },
        ),
        SimpleNamespace(
            type="messages-tuple",
            data={
                "type": "tool",
                "tool_call_id": "call-1",
                "name": "web_search",
                "content": "result",
            },
        ),
        SimpleNamespace(
            type="values",
            data={
                "title": "Task title",
                "todos": [{"content": "Research", "status": "completed"}],
                "artifacts": [],
            },
        ),
        {
            "live": True,
            "data": {
                "type": "task_started",
                "task_id": "sub-1",
                "description": "Summarize",
            },
        },
        {
            "live": True,
            "data": {"type": "token_chunk", "task_id": "sub-1", "content": "working"},
        },
        {
            "live": True,
            "data": {"type": "task_completed", "task_id": "sub-1", "result": "done"},
        },
        SimpleNamespace(
            type="end",
            data={"usage": {"input_tokens": 3, "output_tokens": 4, "total_tokens": 7}},
        ),
    ]
    runtime = FakeRuntime(events)
    connection = FakeConnection()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    agent.on_connect(connection)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    response = await agent.prompt(
        [acp.text_block("do it")],
        created.session_id,
        message_id="11111111-1111-1111-1111-111111111111",
    )

    assert response.stop_reason == "end_turn"
    assert response.usage.total_tokens == 7
    assert response.user_message_id == "11111111-1111-1111-1111-111111111111"
    updates = [update for _, update in connection.updates]
    assert any(isinstance(update, schema.AgentMessageChunk) for update in updates)
    assert any(isinstance(update, schema.AgentThoughtChunk) for update in updates)
    assert any(isinstance(update, schema.AgentPlanUpdate) for update in updates)
    assert any(isinstance(update, schema.SessionInfoUpdate) for update in updates)
    assert sum(isinstance(update, schema.ToolCallStart) for update in updates) == 2
    assert sum(isinstance(update, schema.ToolCallProgress) for update in updates) >= 3
    session = await store.get(created.session_id)
    assert session is not None and session.title == "Task title"


@pytest.mark.asyncio
async def test_prompt_reports_tool_call_limit_as_max_turn_requests(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    runtime = FakeRuntime(
        [
            {
                "live": True,
                "data": {
                    "type": "task_limit_reached",
                    "task_id": "sub-1",
                    "reason": "tool_call_limit",
                    "error": "Tool-call limit reached",
                    "result": "partial work",
                    "incomplete": True,
                },
            },
            SimpleNamespace(
                type="end",
                data={
                    "stop_reason": "tool_call_limit",
                    "usage": {
                        "input_tokens": 3,
                        "output_tokens": 4,
                        "total_tokens": 7,
                    },
                },
            ),
        ]
    )
    connection = FakeConnection()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    agent.on_connect(connection)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    response = await agent.prompt(
        [acp.text_block("do it")],
        created.session_id,
    )

    assert response.stop_reason == "max_turn_requests"
    progress = [
        update
        for _, update in connection.updates
        if isinstance(update, schema.ToolCallProgress)
    ]
    assert progress[-1].status == "failed"


@pytest.mark.asyncio
async def test_goal_set_command_persists_and_starts_normalized_task(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    runtime = FakeRuntime()
    connection = FakeConnection()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    agent.on_connect(connection)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    response = await agent.prompt(
        [acp.text_block("/goal   finish the implementation   and tests  ")],
        created.session_id,
    )

    assert response.stop_reason == "end_turn"
    assert runtime.goal_set_calls == [
        (created.session_id, "finish the implementation and tests")
    ]
    assert runtime.prompt_messages == ["finish the implementation and tests"]


@pytest.mark.asyncio
async def test_goal_status_command_does_not_start_model_run(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    runtime = FakeRuntime()
    runtime.goal = {
        "objective": "make every test pass",
        "auto_continue": True,
        "continuation_count": 1,
        "max_continuations": 3,
        "last_evaluation": {"reason": "Two tests still fail."},
    }
    connection = FakeConnection()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    agent.on_connect(connection)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    response = await agent.prompt(
        [acp.text_block("/goal")],
        created.session_id,
    )

    assert response.usage.total_tokens == 0
    assert runtime.prompt_messages == []
    updates = [update for _, update in connection.updates]
    status = next(
        update for update in updates if isinstance(update, schema.AgentMessageChunk)
    )
    assert "Active goal: make every test pass" in status.content.text
    assert "1/3 used" in status.content.text
    assert "Two tests still fail" in status.content.text


@pytest.mark.asyncio
async def test_goal_clear_command_clears_without_model_run(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    runtime = FakeRuntime()
    runtime.goal = {"objective": "old goal"}
    connection = FakeConnection()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    agent.on_connect(connection)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    response = await agent.prompt(
        [acp.text_block("/goal RESET")],
        created.session_id,
    )

    assert response.usage.total_tokens == 0
    assert runtime.goal_clear_calls == [created.session_id]
    assert runtime.prompt_messages == []
    updates = [update for _, update in connection.updates]
    cleared = next(
        update for update in updates if isinstance(update, schema.AgentMessageChunk)
    )
    assert cleared.content.text == "Goal cleared."


@pytest.mark.asyncio
async def test_goal_command_with_resource_is_treated_as_an_ordinary_prompt(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    resource = tmp_path / "input.txt"
    resource.write_text("hello", encoding="utf-8")
    runtime = FakeRuntime()
    runtime.goal = {"objective": "keep this goal"}
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    await agent.prompt(
        [
            acp.text_block("/goal clear"),
            acp.resource_link_block("input.txt", resource.as_uri(), size=5),
        ],
        created.session_id,
    )

    assert runtime.goal_clear_calls == []
    assert runtime.goal == {"objective": "keep this goal"}
    assert runtime.prompt_messages[0].startswith("/goal clear")
    assert "/mnt/user-data/workspace/input.txt" in runtime.prompt_messages[0]


@pytest.mark.asyncio
async def test_goal_survives_prompt_cancellation(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    runtime = FakeRuntime()
    runtime.block = True
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])
    prompt_task = asyncio.create_task(
        agent.prompt(
            [acp.text_block("/goal finish the task")],
            created.session_id,
        )
    )
    await runtime.started.wait()

    await agent.cancel(created.session_id)
    response = await prompt_task

    assert response.stop_reason == "cancelled"
    assert runtime.goal is not None
    assert runtime.goal["objective"] == "finish the task"


@pytest.mark.asyncio
async def test_goal_survives_prompt_timeout(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    runtime = FakeRuntime()
    runtime.block = True
    agent = DeerFlowACPAgent(
        make_config(tmp_path, run_timeout_seconds=0.01),
        store,
        runtime,
    )
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    with pytest.raises(RequestError) as error:
        await agent.prompt(
            [acp.text_block("/goal finish the task")],
            created.session_id,
        )

    assert "timed out" in str(error.value.data)
    assert runtime.goal is not None
    assert runtime.goal["objective"] == "finish the task"


@pytest.mark.asyncio
async def test_load_session_reports_restored_goal(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    runtime = FakeRuntime()
    runtime.goal = {
        "objective": "finish the task",
        "created_at": "2026-08-27T00:00:00Z",
    }
    connection = FakeConnection()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    agent.on_connect(connection)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    await agent.load_session(
        cwd=str(tmp_path),
        session_id=created.session_id,
        mcp_servers=[],
    )

    restored = [
        update
        for _, update in connection.updates
        if isinstance(update, schema.AgentMessageChunk)
    ]
    assert [update.content.text for update in restored] == [
        "Active goal restored: finish the task"
    ]


@pytest.mark.asyncio
async def test_prompt_rejects_outside_resources_and_one_active_prompt_per_session(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    runtime = FakeRuntime()
    runtime.block = True
    connection = FakeConnection()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    agent.on_connect(connection)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    first = asyncio.create_task(
        agent.prompt([acp.text_block("wait")], created.session_id)
    )
    await runtime.started.wait()
    with pytest.raises(RequestError) as busy_error:
        await agent.prompt([acp.text_block("second")], created.session_id)
    assert busy_error.value.code == -32001
    with pytest.raises(RequestError) as model_busy_error:
        await agent.set_config_option("model", created.session_id, "model-a")
    assert model_busy_error.value.code == -32001

    await agent.cancel(created.session_id)
    response = await first
    assert response.stop_reason == "cancelled"

    with pytest.raises(RequestError) as type_error:
        await agent.prompt(
            [acp.resource_link_block("file", "file:///tmp/file")],
            created.session_id,
        )
    assert type_error.value.code == -32602


@pytest.mark.asyncio
async def test_prompt_keeps_session_busy_until_final_save_completes(
    tmp_path: Path,
    store: LocalACPSessionStore,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    runtime = FakeRuntime()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    agent.on_connect(FakeConnection())
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])
    original_save = store.save
    save_started = asyncio.Event()
    allow_save = asyncio.Event()

    async def blocking_save(session: Any) -> None:
        save_started.set()
        await allow_save.wait()
        await original_save(session)

    monkeypatch.setattr(store, "save", blocking_save)
    prompt_task = asyncio.create_task(
        agent.prompt([acp.text_block("finish")], created.session_id)
    )
    await save_started.wait()

    with pytest.raises(RequestError) as config_busy_error:
        await agent.set_config_option("model", created.session_id, "model-a")
    assert config_busy_error.value.code == -32001
    with pytest.raises(RequestError) as close_busy_error:
        await agent.close_session(created.session_id)
    assert close_busy_error.value.code == -32001

    allow_save.set()
    response = await prompt_task
    assert response.stop_reason == "end_turn"


@pytest.mark.asyncio
async def test_two_connections_lease_distinct_sessions_and_cancel_independently(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    runtime = FakeRuntime()
    runtime.block = True
    first_connection = FakeConnection()
    second_connection = FakeConnection()
    first_agent = DeerFlowACPAgent(
        make_config(tmp_path), store, runtime, connection_id="connection-a"
    )
    second_agent = DeerFlowACPAgent(
        make_config(tmp_path), store, runtime, connection_id="connection-b"
    )
    first_agent.on_connect(first_connection)
    second_agent.on_connect(second_connection)
    first_session = await first_agent.new_session(cwd=str(tmp_path), mcp_servers=[])
    second_session = await second_agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    first_prompt = asyncio.create_task(
        first_agent.prompt([acp.text_block("first")], first_session.session_id)
    )
    second_prompt = asyncio.create_task(
        second_agent.prompt([acp.text_block("second")], second_session.session_id)
    )
    for _ in range(100):
        if len(runtime.prompt_messages) == 2:
            break
        await asyncio.sleep(0.01)
    assert runtime.prompt_messages == ["first", "second"]

    await first_agent.cancel(first_session.session_id)
    first_response = await first_prompt
    assert first_response.stop_reason == "cancelled"
    assert not second_prompt.done()

    await second_agent.cancel(second_session.session_id)
    second_response = await second_prompt
    assert second_response.stop_reason == "cancelled"
    await first_agent.shutdown()
    await second_agent.shutdown()


@pytest.mark.asyncio
async def test_session_lease_blocks_other_connection_until_disconnect(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    runtime = FakeRuntime()
    first_agent = DeerFlowACPAgent(
        make_config(tmp_path), store, runtime, connection_id="connection-a"
    )
    second_agent = DeerFlowACPAgent(
        make_config(tmp_path), store, runtime, connection_id="connection-b"
    )
    second_agent.on_connect(FakeConnection())
    created = await first_agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    with pytest.raises(RequestError, match="attached to another ACP client"):
        await second_agent.load_session(
            cwd=str(tmp_path), session_id=created.session_id, mcp_servers=[]
        )
    with pytest.raises(RequestError, match="attached"):
        await second_agent.prompt([acp.text_block("wrong owner")], created.session_id)

    await first_agent.shutdown()
    loaded = await second_agent.load_session(
        cwd=str(tmp_path), session_id=created.session_id, mcp_servers=[]
    )
    assert loaded.modes.current_mode_id == "plan"
    assert runtime.session_coordinator.owner(created.session_id) == "connection-b"
    await second_agent.shutdown()


@pytest.mark.asyncio
async def test_disconnect_releases_only_its_client_mcp_sessions(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    runtime = FakeRuntime()
    config = make_config(tmp_path, accept_client_mcp_servers=True)
    first_agent = DeerFlowACPAgent(config, store, runtime, connection_id="connection-a")
    second_agent = DeerFlowACPAgent(
        config, store, runtime, connection_id="connection-b"
    )
    first = await first_agent.new_session(
        cwd=str(tmp_path),
        mcp_servers=[
            schema.McpServerStdio(
                name="first-mcp", command="first-mcp", args=[], env=[]
            )
        ],
    )
    second = await second_agent.new_session(
        cwd=str(tmp_path),
        mcp_servers=[
            schema.McpServerStdio(
                name="second-mcp", command="second-mcp", args=[], env=[]
            )
        ],
    )

    await first_agent.shutdown()

    assert first.session_id not in runtime.client_mcp_bindings
    assert second.session_id in runtime.client_mcp_bindings
    assert runtime.released_client_mcp == [first.session_id]
    await second_agent.shutdown()


@pytest.mark.asyncio
async def test_prompt_accepts_workspace_resource_links(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    resource = tmp_path / "input.txt"
    resource.write_text("hello", encoding="utf-8")
    runtime = FakeRuntime()
    connection = FakeConnection()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    agent.on_connect(connection)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    response = await agent.prompt(
        [acp.resource_link_block("input.txt", resource.as_uri(), size=5)],
        created.session_id,
    )

    assert response.stop_reason == "end_turn"
    assert (
        '"workspace_path": "/mnt/user-data/workspace/input.txt"'
        in runtime.prompt_messages[0]
    )


@pytest.mark.asyncio
async def test_prompt_persists_native_image_and_passes_only_metadata(
    tmp_path: Path,
    store: LocalACPSessionStore,
    configured_models: SimpleNamespace,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    configured_models.models[1].supports_vision = True
    uploads = tmp_path / "uploads"
    monkeypatch.setattr(
        "deerflow.agents.image_inputs.ensure_uploads_dir",
        lambda _thread_id: uploads.mkdir(parents=True, exist_ok=True) or uploads,
    )
    image_bytes = b"\x89PNG\r\n\x1a\nacp-native-image"
    encoded = base64.b64encode(image_bytes).decode("ascii")
    runtime = FakeRuntime()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    response = await agent.prompt(
        [acp.text_block("What is in this image?"), acp.image_block(encoded, "image/png")],
        created.session_id,
    )

    assert response.stop_reason == "end_turn"
    assert encoded not in runtime.prompt_messages[0]
    assert len(runtime.prompt_images[0]) == 1
    metadata = runtime.prompt_images[0][0]
    assert metadata["mime_type"] == "image/png"
    assert metadata["size"] == len(image_bytes)
    assert str(metadata["virtual_path"]).startswith("/mnt/user-data/uploads/acp-image-")
    stored = uploads / Path(str(metadata["virtual_path"])).name
    assert stored.read_bytes() == image_bytes


@pytest.mark.asyncio
async def test_prompt_copies_local_image_resource_into_session_uploads(
    tmp_path: Path,
    store: LocalACPSessionStore,
    configured_models: SimpleNamespace,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    configured_models.models[1].supports_vision = True
    resource = tmp_path / "diagram.png"
    image_bytes = b"\x89PNG\r\n\x1a\nresource-image"
    resource.write_bytes(image_bytes)
    uploads = tmp_path / "uploads"
    monkeypatch.setattr(
        "deerflow.agents.image_inputs.ensure_uploads_dir",
        lambda _thread_id: uploads.mkdir(parents=True, exist_ok=True) or uploads,
    )
    runtime = FakeRuntime()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    await agent.prompt(
        [
            acp.resource_link_block(
                "diagram.png",
                resource.as_uri(),
                mime_type="image/png",
                size=len(image_bytes),
            )
        ],
        created.session_id,
    )

    assert len(runtime.prompt_images[0]) == 1
    metadata = runtime.prompt_images[0][0]
    assert metadata["name"] == "diagram.png"
    assert (uploads / Path(str(metadata["virtual_path"])).name).read_bytes() == image_bytes
    assert "/mnt/user-data/uploads/" in runtime.prompt_messages[0]


@pytest.mark.asyncio
async def test_prompt_rejects_image_for_non_vision_session_model(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    runtime = FakeRuntime()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])
    encoded = base64.b64encode(b"\x89PNG\r\n\x1a\nimage").decode("ascii")

    with pytest.raises(RequestError) as error:
        await agent.prompt(
            [acp.image_block(encoded, "image/png")],
            created.session_id,
        )

    assert error.value.code == -32602
    assert "does not support image input" in str(error.value.data)
    assert runtime.prompt_messages == []


@pytest.mark.asyncio
async def test_prompt_accepts_image_for_agent_profile_vision_model(
    tmp_path: Path,
    store: LocalACPSessionStore,
    configured_models: SimpleNamespace,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    configured_models.models[0].supports_vision = True
    monkeypatch.setattr(
        "deerflow.acp.agent.load_agent_config",
        lambda _name: SimpleNamespace(model="model-a"),
    )
    runtime = FakeRuntime()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])
    await agent.set_config_option("agent_profile", created.session_id, "vision-profile")
    encoded = base64.b64encode(b"\x89PNG\r\n\x1a\nimage").decode("ascii")

    response = await agent.prompt(
        [acp.image_block(encoded, "image/png")],
        created.session_id,
    )

    assert response.stop_reason == "end_turn"
    assert len(runtime.prompt_images[0]) == 1


@pytest.mark.asyncio
async def test_explicit_non_vision_model_overrides_agent_profile_vision_model(
    tmp_path: Path,
    store: LocalACPSessionStore,
    configured_models: SimpleNamespace,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    configured_models.models[0].supports_vision = True
    monkeypatch.setattr(
        "deerflow.acp.agent.load_agent_config",
        lambda _name: SimpleNamespace(model="model-a"),
    )
    runtime = FakeRuntime()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])
    await agent.set_config_option("agent_profile", created.session_id, "vision-profile")
    await agent.set_config_option("model", created.session_id, "model-b")
    encoded = base64.b64encode(b"\x89PNG\r\n\x1a\nimage").decode("ascii")

    with pytest.raises(RequestError) as error:
        await agent.prompt(
            [acp.image_block(encoded, "image/png")],
            created.session_id,
        )

    assert error.value.code == -32602
    assert "model-b" in str(error.value.data)
    assert runtime.prompt_messages == []


@pytest.mark.asyncio
async def test_prompt_rejects_remote_image_resource_until_safe_download_exists(
    tmp_path: Path,
    store: LocalACPSessionStore,
    configured_models: SimpleNamespace,
) -> None:
    configured_models.models[1].supports_vision = True
    agent = DeerFlowACPAgent(make_config(tmp_path), store, FakeRuntime())
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    with pytest.raises(RequestError) as error:
        await agent.prompt(
            [
                acp.resource_link_block(
                    "remote.png",
                    "https://example.test/remote.png",
                    mime_type="image/png",
                )
            ],
            created.session_id,
        )

    assert error.value.code == -32602
    assert "not downloaded" in str(error.value.data)


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("data", "mime_type", "message"),
    [
        ("not-base64", "image/png", "valid base64"),
        (
            base64.b64encode(b"\xff\xd8\xffjpeg").decode("ascii"),
            "image/png",
            "contents are image/jpeg",
        ),
    ],
)
async def test_prompt_rejects_invalid_native_images(
    tmp_path: Path,
    store: LocalACPSessionStore,
    configured_models: SimpleNamespace,
    data: str,
    mime_type: str,
    message: str,
) -> None:
    configured_models.models[1].supports_vision = True
    agent = DeerFlowACPAgent(make_config(tmp_path), store, FakeRuntime())
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    with pytest.raises(RequestError) as error:
        await agent.prompt(
            [acp.image_block(data, mime_type)],
            created.session_id,
        )

    assert error.value.code == -32602
    assert message in str(error.value.data)


@pytest.mark.asyncio
async def test_prompt_limits_images_per_turn_before_writing_files(
    tmp_path: Path,
    store: LocalACPSessionStore,
    configured_models: SimpleNamespace,
) -> None:
    configured_models.models[1].supports_vision = True
    agent = DeerFlowACPAgent(make_config(tmp_path), store, FakeRuntime())
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])
    encoded = base64.b64encode(b"\x89PNG\r\n\x1a\nimage").decode("ascii")

    with pytest.raises(RequestError) as error:
        await agent.prompt(
            [acp.image_block(encoded, "image/png") for _ in range(9)],
            created.session_id,
        )

    assert error.value.code == -32602
    assert "at most 8 images" in str(error.value.data)


@pytest.mark.asyncio
async def test_close_session_marks_it_unavailable_and_releases_resources(
    tmp_path: Path,
    store: LocalACPSessionStore,
) -> None:
    runtime = FakeRuntime()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    response = await agent.close_session(created.session_id)

    assert isinstance(response, schema.CloseSessionResponse)
    assert await store.get(created.session_id) is None
    with pytest.raises(RequestError):
        await agent.load_session(
            cwd=str(tmp_path), session_id=created.session_id, mcp_servers=[]
        )


@pytest.mark.asyncio
async def test_close_session_succeeds_after_durable_close_when_cleanup_fails(
    tmp_path: Path,
    store: LocalACPSessionStore,
    caplog: pytest.LogCaptureFixture,
) -> None:
    runtime = FakeRuntime()
    agent = DeerFlowACPAgent(make_config(tmp_path), store, runtime)
    created = await agent.new_session(cwd=str(tmp_path), mcp_servers=[])

    async def fail_cleanup(session_id: str) -> None:
        del session_id
        raise RuntimeError("cleanup failed")

    runtime.release_client_mcp = fail_cleanup  # type: ignore[method-assign]
    response = await agent.close_session(created.session_id)

    assert isinstance(response, schema.CloseSessionResponse)
    assert await store.get(created.session_id) is None
    assert runtime.session_coordinator.owner(created.session_id) is None
    assert "resource cleanup failed" in caplog.text


@pytest.mark.asyncio
async def test_event_mapper_deduplicates_cumulative_reasoning_and_artifacts() -> None:
    updates: list[Any] = []

    async def send(update: Any) -> None:
        updates.append(update)

    block = acp.resource_link_block(
        "report.txt", "file:///tmp/report.txt", mime_type="text/plain"
    )
    mapper = ACPEventMapper(
        "session-1",
        send,
        artifact_resolver=lambda path: block if path == "/out" else None,
    )
    await mapper.handle(
        SimpleNamespace(
            type="messages-tuple",
            data={"type": "ai", "id": "x", "content": "", "reasoning_content": "one"},
        )
    )
    await mapper.handle(
        SimpleNamespace(
            type="messages-tuple",
            data={
                "type": "ai",
                "id": "x",
                "content": "",
                "reasoning_content": "one two",
            },
        )
    )
    values = SimpleNamespace(
        type="values", data={"artifacts": ["/out", "/out"], "todos": []}
    )
    await mapper.handle(values)
    await mapper.handle(values)

    thoughts = [
        update.content.text
        for update in updates
        if isinstance(update, schema.AgentThoughtChunk)
    ]
    resources = [
        update for update in updates if isinstance(update, schema.AgentMessageChunk)
    ]
    plans = [update for update in updates if isinstance(update, schema.AgentPlanUpdate)]
    assert thoughts == ["one", " two"]
    assert len(resources) == 1
    assert len(plans) == 1


@pytest.mark.asyncio
async def test_event_mapper_waits_for_async_artifact_publication() -> None:
    updates: list[Any] = []
    published: list[str] = []

    async def send(update: Any) -> None:
        updates.append(update)

    async def resolve(path: str):
        await asyncio.sleep(0)
        published.append(path)
        return acp.resource_link_block("report.txt", "https://rustfs.test/report.txt")

    mapper = ACPEventMapper("session-1", send, artifact_resolver=resolve)
    await mapper.handle(
        SimpleNamespace(type="values", data={"artifacts": ["/out"], "todos": []})
    )

    assert published == ["/out"]
    resources = [
        update for update in updates if isinstance(update, schema.AgentMessageChunk)
    ]
    assert len(resources) == 1
    assert resources[0].content.uri == "https://rustfs.test/report.txt"


@pytest.mark.asyncio
async def test_event_mapper_resolves_local_artifact_from_acp_workspace_outputs(
    tmp_path: Path,
) -> None:
    updates: list[Any] = []
    outputs = tmp_path / "outputs"
    outputs.mkdir()
    report = outputs / "report.txt"
    report.write_text("report", encoding="utf-8")

    async def send(update: Any) -> None:
        updates.append(update)

    mapper = ACPEventMapper(
        "session-1",
        send,
        outputs_path=str(outputs),
    )
    await mapper.handle(
        SimpleNamespace(
            type="values",
            data={"artifacts": ["/mnt/user-data/outputs/report.txt"], "todos": []},
        )
    )

    resources = [
        update for update in updates if isinstance(update, schema.AgentMessageChunk)
    ]
    assert len(resources) == 1
    assert resources[0].content.uri == report.resolve().as_uri()


@pytest.mark.asyncio
async def test_event_mapper_can_start_subagent_from_progress_or_terminal_event() -> (
    None
):
    updates: list[Any] = []

    async def send(update: Any) -> None:
        updates.append(update)

    mapper = ACPEventMapper("session-1", send)
    await mapper.handle_live(
        {"type": "token_chunk", "task_id": "late-1", "content": "progress"}
    )
    await mapper.handle_live(
        {"type": "task_completed", "task_id": "late-1", "result": "done"}
    )
    await mapper.close_open_tools(cancelled=False)
    await mapper.handle_live(
        {"type": "token_chunk", "task_id": "late-1", "content": "too late"}
    )

    assert sum(isinstance(update, schema.ToolCallStart) for update in updates) == 1
    progress = [
        update for update in updates if isinstance(update, schema.ToolCallProgress)
    ]
    assert [update.status for update in progress] == ["in_progress", "completed"]


@pytest.mark.asyncio
async def test_event_mapper_fails_open_tools_after_loop_hard_stop() -> None:
    updates: list[Any] = []

    async def send(update: Any) -> None:
        updates.append(update)

    mapper = ACPEventMapper("session-1", send)
    await mapper.handle_live(
        {"type": "task_started", "task_id": "sub-1", "description": "work"}
    )
    await mapper.handle_live(
        {
            "type": "loop_hard_stop",
            "reason": "tool_call_limit",
            "incomplete": True,
        }
    )
    await mapper.close_open_tools(
        cancelled=False,
        failure_message=mapper.stop_reason,
    )

    progress = [
        update for update in updates if isinstance(update, schema.ToolCallProgress)
    ]
    assert mapper.stop_reason == "max_turn_requests"
    assert progress[-1].status == "failed"


@pytest.mark.asyncio
async def test_event_mapper_adds_subagent_usage_to_prompt_usage() -> None:
    updates: list[Any] = []

    async def send(update: Any) -> None:
        updates.append(update)

    mapper = ACPEventMapper("session-1", send)
    await mapper.handle_live(
        {
            "type": "token_usage",
            "task_id": "sub-1",
            "input_tokens": 5,
            "output_tokens": 7,
            "total_tokens": 12,
        }
    )
    await mapper.handle(
        SimpleNamespace(
            type="end",
            data={"usage": {"input_tokens": 3, "output_tokens": 4, "total_tokens": 7}},
        )
    )

    assert mapper.usage == {"input_tokens": 8, "output_tokens": 11, "total_tokens": 19}


@pytest.mark.asyncio
async def test_event_mapper_accumulates_lead_usage_across_goal_turns() -> None:
    async def send(_update: Any) -> None:
        pass

    mapper = ACPEventMapper("session-1", send)
    await mapper.handle(
        SimpleNamespace(
            type="end",
            data={"usage": {"input_tokens": 3, "output_tokens": 4, "total_tokens": 7}},
        )
    )
    await mapper.handle(
        SimpleNamespace(
            type="end",
            data={"usage": {"input_tokens": 5, "output_tokens": 6, "total_tokens": 11}},
        )
    )

    assert mapper.usage == {"input_tokens": 8, "output_tokens": 10, "total_tokens": 18}


@pytest.mark.asyncio
async def test_event_mapper_reports_goal_continuation_status() -> None:
    updates: list[Any] = []

    async def send(update: Any) -> None:
        updates.append(update)

    mapper = ACPEventMapper("session-1", send)
    await mapper.handle(
        SimpleNamespace(
            type="custom",
            data={
                "type": "goal_status",
                "status": "continuing",
                "objective": "finish the task",
                "continuation_count": 1,
                "max_continuations": 3,
                "reason": "One test remains.",
                "evaluator_usage": {
                    "input_tokens": 8,
                    "output_tokens": 4,
                    "total_tokens": 12,
                },
            },
        )
    )

    assert len(updates) == 1
    assert isinstance(updates[0], schema.AgentMessageChunk)
    assert updates[0].content.text == (
        "Goal not complete; continuing (1/3). One test remains."
    )
    assert mapper.usage == {
        "input_tokens": 8,
        "output_tokens": 4,
        "total_tokens": 12,
    }


@pytest.mark.asyncio
async def test_event_mapper_sends_usage_update_on_end() -> None:
    updates: list[Any] = []

    async def send(update: Any) -> None:
        updates.append(update)

    mapper = ACPEventMapper("session-1", send)
    await mapper.handle(
        SimpleNamespace(
            type="end",
            data={
                "usage": {"input_tokens": 3, "output_tokens": 4, "total_tokens": 7},
                "last_usage": {
                    "input_tokens": 900,
                    "output_tokens": 100,
                    "total_tokens": 1000,
                },
                "context_window": 4096,
            },
        )
    )

    usage_updates = [
        update for update in updates if isinstance(update, schema.UsageUpdate)
    ]
    assert len(usage_updates) == 1
    assert usage_updates[0].session_update == "usage_update"
    assert usage_updates[0].size == 4096
    assert usage_updates[0].used == 1000
    # Lead usage accounting is unaffected by the extra telemetry fields.
    assert mapper.usage == {"input_tokens": 3, "output_tokens": 4, "total_tokens": 7}


@pytest.mark.asyncio
async def test_event_mapper_skips_usage_update_without_context_window() -> None:
    updates: list[Any] = []

    async def send(update: Any) -> None:
        updates.append(update)

    mapper = ACPEventMapper("session-1", send)
    # No context_window configured (or no model call this turn) -> no update.
    await mapper.handle(
        SimpleNamespace(
            type="end",
            data={
                "usage": {"input_tokens": 3, "output_tokens": 4, "total_tokens": 7},
                "last_usage": {"input_tokens": 900, "output_tokens": 100},
            },
        )
    )
    await mapper.handle(
        SimpleNamespace(
            type="end",
            data={
                "usage": {"input_tokens": 3, "output_tokens": 4, "total_tokens": 7},
                "context_window": 4096,
            },
        )
    )

    assert not [u for u in updates if isinstance(u, schema.UsageUpdate)]


@pytest.mark.asyncio
async def test_session_store_persists_and_pages(tmp_path: Path) -> None:
    path = tmp_path / "sessions.db"
    first = LocalACPSessionStore(path)
    first.setup()
    defaults = {
        "model_name": None,
        "thinking_enabled": True,
        "subagent_enabled": False,
        "plan_mode": False,
        "max_concurrent_subagents": 2,
        "recursion_limit": 100,
        "agent_name": None,
    }
    one = await first.create(cwd=str(tmp_path), defaults=defaults)
    one.approval_mode = "allow_always"
    await first.save(one)
    await first.create(cwd=str(tmp_path), defaults=defaults)
    await first.create(cwd=str(tmp_path / "different"), defaults=defaults)
    page, cursor = await first.list(cwd=None, cursor=None, limit=2)
    assert len(page) == 2 and cursor is not None
    second_page, next_cursor = await first.list(cwd=None, cursor=cursor, limit=2)
    assert len(second_page) == 1 and next_cursor is None
    first.close()

    reopened = LocalACPSessionStore(path)
    reopened.setup()
    loaded = await reopened.get(one.session_id)
    assert loaded is not None and loaded.cwd == str(tmp_path)
    assert loaded.approval_mode == "allow_always"
    reopened.close()


@pytest.mark.asyncio
async def test_session_store_defaults_legacy_approval_mode_to_ask(
    tmp_path: Path,
) -> None:
    path = tmp_path / "sessions.db"
    store = LocalACPSessionStore(path)
    store.setup()
    defaults = {
        "model_name": None,
        "thinking_enabled": True,
        "subagent_enabled": False,
        "plan_mode": False,
        "max_concurrent_subagents": 1,
        "recursion_limit": 100,
        "agent_name": None,
    }
    session = await store.create(cwd=str(tmp_path), defaults=defaults)
    legacy_config = session.config_dict()
    legacy_config.pop("approval_mode", None)
    connection = sqlite3.connect(path)
    try:
        connection.execute(
            "UPDATE acp_sessions SET config_json = ? WHERE session_id = ?",
            (json.dumps(legacy_config), session.session_id),
        )
        connection.commit()
    finally:
        connection.close()

    loaded = await store.get(session.session_id)

    assert loaded is not None
    assert loaded.approval_mode == "ask"
    store.close()


@pytest.mark.asyncio
async def test_session_store_purges_closed_metadata(tmp_path: Path) -> None:
    store = LocalACPSessionStore(tmp_path / "sessions.db")
    store.setup()
    defaults = {
        "model_name": None,
        "thinking_enabled": True,
        "subagent_enabled": False,
        "plan_mode": False,
        "max_concurrent_subagents": 2,
        "recursion_limit": 100,
        "agent_name": None,
    }
    session = await store.create(cwd=str(tmp_path), defaults=defaults)
    assert await store.mark_closed(session.session_id)

    purged = await store.purge_closed(retention_days=0)

    assert purged == [session.session_id]
    assert await store.get(session.session_id, include_closed=True) is None
    store.close()
