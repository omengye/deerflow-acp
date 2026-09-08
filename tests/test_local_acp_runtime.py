from __future__ import annotations

import asyncio
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest
from acp import schema
from langchain_core.messages import HumanMessage
from langgraph.checkpoint.memory import InMemorySaver

import deerflow.acp.runtime as runtime_module
from deerflow.acp.client_mcp import normalize_client_mcp_servers
from deerflow.acp.config import LocalACPConfig
from deerflow.acp.runtime import LocalACPRuntime
from deerflow.acp.session_store import LocalACPSession
from deerflow.agents.goal_state import GoalEvaluation
from deerflow.client import DeerFlowClient, StreamEvent


def _make_runtime_session(tmp_path: Path, session_id: str) -> LocalACPSession:
    return LocalACPSession(
        session_id=session_id,
        cwd=str(tmp_path),
        title=None,
        updated_at="",
        model_name=None,
        thinking_enabled=True,
        subagent_enabled=False,
        plan_mode=False,
        max_concurrent_subagents=1,
        recursion_limit=100,
    )


def _configure_local_sandbox(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(
        "deerflow.config.get_app_config",
        lambda: SimpleNamespace(
            sandbox=SimpleNamespace(
                use="deerflow.sandbox.local:LocalSandboxProvider"
            )
        ),
    )


@pytest.mark.asyncio
async def test_runtime_compacts_sqlite_checkpoints(tmp_path: Path) -> None:
    config = LocalACPConfig(
        config_path=tmp_path / "config.yaml",
        checkpointer_path=tmp_path / "checkpoints.db",
        session_store_path=tmp_path / "sessions.db",
    )
    runtime = LocalACPRuntime(config)
    await runtime.open()
    try:
        assert await runtime.compact_checkpoints() is True
    finally:
        await runtime.close()


@pytest.mark.asyncio
async def test_runtime_warmup_builds_and_reuses_default_client(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    instances: list[FakeClient] = []

    class FakeClient:
        def __init__(self, **kwargs: Any) -> None:
            self.kwargs = kwargs
            self.warmup_calls = 0
            instances.append(self)

        def warmup(self) -> None:
            self.warmup_calls += 1

    monkeypatch.setattr(runtime_module, "DeerFlowClient", FakeClient)
    config = LocalACPConfig(
        config_path=tmp_path / "config.yaml",
        checkpointer_path=tmp_path / "checkpoints.db",
        session_store_path=tmp_path / "sessions.db",
        model_name="test-model",
        thinking_enabled=False,
        subagent_enabled=True,
        plan_mode=False,
        max_concurrent_subagents=3,
        recursion_limit=25,
        agent_name="test-agent",
    )
    runtime = LocalACPRuntime(config)
    checkpointer = object()
    runtime._checkpointer = checkpointer

    await runtime.warmup()
    await runtime.warmup()

    assert len(instances) == 1
    assert instances[0].warmup_calls == 2
    kwargs = instances[0].kwargs
    assert kwargs["config_path"] == str(config.config_path)
    assert kwargs["checkpointer"] is checkpointer
    assert kwargs["model_name"] == "test-model"
    assert kwargs["thinking_enabled"] is False
    assert kwargs["subagent_enabled"] is True
    assert kwargs["max_concurrent_subagents"] == 3
    assert kwargs["agent_name"] == "test-agent"
    # DeerFlowClient must derive full/delta mode and snapshot frequency from
    # config.yaml. Local ACP must not silently force full checkpoints.
    assert "checkpoint_channel_mode" not in kwargs
    assert "task" not in kwargs["excluded_tool_names"]
    assert "invoke_acp_agent" in kwargs["excluded_tool_names"]
    assert "create_scheduled_task" in kwargs["excluded_tool_names"]
    assert kwargs["system_prompt_overlay"]
    assert (
        "further delegation is unavailable" in kwargs["subagent_system_prompt_overlay"]
    )
    assert len(kwargs["middlewares"]) == 1
    assert kwargs["subagent_middlewares"] == kwargs["middlewares"]


@pytest.mark.asyncio
async def test_runtime_builds_a_new_client_after_session_model_changes(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    instances: list[FakeClient] = []

    class FakeClient:
        def __init__(self, **kwargs: Any) -> None:
            self.kwargs = kwargs
            instances.append(self)

    monkeypatch.setattr(runtime_module, "DeerFlowClient", FakeClient)
    config = LocalACPConfig(
        config_path=tmp_path / "config.yaml",
        checkpointer_path=tmp_path / "checkpoints.db",
        session_store_path=tmp_path / "sessions.db",
    )
    runtime = LocalACPRuntime(config)
    runtime._checkpointer = object()
    session = LocalACPSession(
        session_id="model-switch-session",
        cwd=str(tmp_path),
        title=None,
        updated_at="",
        model_name="model-a",
        thinking_enabled=True,
        subagent_enabled=False,
        plan_mode=True,
        max_concurrent_subagents=1,
        recursion_limit=200,
    )

    first = await runtime._client_for(session)
    session.model_name = "model-b"
    second = await runtime._client_for(session)

    assert first is not second
    assert [instance.kwargs["model_name"] for instance in instances] == [
        "model-a",
        "model-b",
    ]


@pytest.mark.asyncio
async def test_runtime_includes_bash_only_when_explicitly_enabled(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    instances: list[FakeClient] = []

    class FakeClient:
        def __init__(self, **kwargs: Any) -> None:
            self.kwargs = kwargs
            instances.append(self)

        def warmup(self) -> None:
            pass

    monkeypatch.setattr(runtime_module, "DeerFlowClient", FakeClient)
    config = LocalACPConfig(
        config_path=tmp_path / "config.yaml",
        checkpointer_path=tmp_path / "checkpoints.db",
        session_store_path=tmp_path / "sessions.db",
        enable_bash=True,
    )
    runtime = LocalACPRuntime(config)
    runtime._checkpointer = object()

    await runtime.warmup()

    assert instances[0].kwargs["excluded_tool_names"] == {
        "invoke_acp_agent",
        "task",
        "task_status",
        "create_scheduled_task",
        "list_scheduled_tasks",
        "set_scheduled_task_enabled",
        "delete_scheduled_task",
        "list_scheduled_task_runs",
    }


@pytest.mark.asyncio
async def test_runtime_injects_client_mcp_tools_into_a_session_only(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    instances: list[FakeClient] = []
    loaded_configs: list[Any] = []
    injected_tool = type("FakeTool", (), {"name": "codeg_read_file"})()

    class FakeClient:
        def __init__(self, **kwargs: Any) -> None:
            self.kwargs = kwargs
            instances.append(self)

    async def fake_get_mcp_tools(config: Any) -> list[Any]:
        loaded_configs.append(config)
        return [injected_tool]

    monkeypatch.setattr(runtime_module, "DeerFlowClient", FakeClient)
    monkeypatch.setattr("deerflow.mcp.tools.get_mcp_tools", fake_get_mcp_tools)
    config = LocalACPConfig(
        config_path=tmp_path / "config.yaml",
        checkpointer_path=tmp_path / "checkpoints.db",
        session_store_path=tmp_path / "sessions.db",
        accept_client_mcp_servers=True,
    )
    runtime = LocalACPRuntime(config)
    runtime._checkpointer = object()
    session = LocalACPSession(
        session_id="session-with-codeg-mcp",
        cwd=str(tmp_path),
        title=None,
        updated_at="",
        model_name=None,
        thinking_enabled=True,
        subagent_enabled=True,
        plan_mode=True,
        max_concurrent_subagents=3,
        recursion_limit=200,
    )
    binding = normalize_client_mcp_servers(
        [
            schema.McpServerStdio(
                name="codeg",
                command="codeg-mcp",
                args=[],
                env=[],
            )
        ],
        enabled=True,
    )
    assert binding is not None

    await runtime.bind_client_mcp(session.session_id, binding)
    client = await runtime._client_for(session)

    assert client.kwargs["additional_mcp_tools"] == [injected_tool]
    assert client.kwargs["subagent_enabled"] is False
    assert client.kwargs["max_concurrent_subagents"] == 1
    assert loaded_configs == [binding.extensions_config]
    assert any(key[0] == "client-mcp" for key in runtime._clients)

    await runtime.release_client_mcp(session.session_id)
    assert session.session_id not in runtime._client_mcp_bindings
    assert not any(key[0] == "client-mcp" for key in runtime._clients)


@pytest.mark.asyncio
async def test_runtime_retries_failed_client_mcp_scope_cleanup(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    attempts: list[str] = []

    class FakeSessionPool:
        async def close_scope(self, session_id: str) -> None:
            attempts.append(session_id)
            if len(attempts) == 1:
                raise RuntimeError("temporary cleanup failure")

    monkeypatch.setattr(
        "deerflow.mcp.session_pool.get_session_pool", lambda: FakeSessionPool()
    )
    runtime = LocalACPRuntime(
        LocalACPConfig(
            config_path=tmp_path / "config.yaml",
            checkpointer_path=tmp_path / "checkpoints.db",
            session_store_path=tmp_path / "sessions.db",
            accept_client_mcp_servers=True,
        )
    )
    binding = normalize_client_mcp_servers(
        [schema.McpServerStdio(name="retry-mcp", command="retry-mcp", args=[], env=[])],
        enabled=True,
    )
    assert binding is not None
    session_id = "cleanup-retry-session"
    await runtime.bind_client_mcp(session_id, binding)

    with pytest.raises(RuntimeError, match="temporary cleanup failure"):
        await runtime.release_client_mcp(session_id)
    assert session_id in runtime._client_mcp_bindings

    await runtime.release_client_mcp(session_id)
    assert attempts == [session_id, session_id]
    assert session_id not in runtime._client_mcp_bindings


@pytest.mark.asyncio
async def test_runtime_passes_session_cwd_as_workspace_path(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    calls: list[dict[str, Any]] = []

    class FakeClient:
        def __init__(self, **kwargs: Any) -> None:
            self.kwargs = kwargs

        async def astream(self, message: str, **kwargs: Any):
            calls.append({"message": message, **kwargs})
            if False:
                yield None

    monkeypatch.setattr(runtime_module, "DeerFlowClient", FakeClient)
    monkeypatch.setattr(
        "deerflow.config.get_app_config",
        lambda: type(
            "Config",
            (),
            {
                "sandbox": type(
                    "SandboxConfig",
                    (),
                    {"use": "deerflow.sandbox.local:LocalSandboxProvider"},
                )()
            },
        )(),
    )
    config = LocalACPConfig(
        config_path=tmp_path / "config.yaml",
        checkpointer_path=tmp_path / "checkpoints.db",
        session_store_path=tmp_path / "sessions.db",
    )
    runtime = LocalACPRuntime(config)
    runtime._checkpointer = object()
    session = LocalACPSession(
        session_id="workspace-session",
        cwd=str(tmp_path),
        title=None,
        updated_at="",
        model_name=None,
        thinking_enabled=True,
        subagent_enabled=False,
        plan_mode=False,
        max_concurrent_subagents=1,
        recursion_limit=100,
        approval_mode="allow_always",
    )

    async for _ in runtime.astream(
        session,
        "inspect files",
        live_event_callback=lambda _event: None,  # type: ignore[arg-type]
    ):
        pass

    assert len(calls) == 1
    assert (
        runtime.permission_broker.session_approval_mode("workspace-session")
        == "allow_always"
    )
    assert callable(calls[0].pop("live_event_callback"))
    assert calls == [
        {
            "message": "inspect files",
            "thread_id": "workspace-session",
            "workspace_path": str(tmp_path),
            "user_id": runtime._memory_user_id(session, str(tmp_path)),
        }
    ]


@pytest.mark.asyncio
async def test_goal_runtime_continues_then_stops_after_repeated_no_progress(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    calls: list[str | HumanMessage] = []

    class FakeClient:
        async def astream(
            self,
            message: str | HumanMessage,
            **kwargs: Any,
        ):
            del kwargs
            calls.append(message)
            if False:
                yield None

    async def fake_evaluate(*args: Any, **kwargs: Any) -> GoalEvaluation:
        del args, kwargs
        return GoalEvaluation(
            satisfied=False,
            blocker="goal_not_met_yet",
            reason="More work remains.",
            evidence_summary="No new visible evidence.",
        )

    _configure_local_sandbox(monkeypatch)
    monkeypatch.setattr(
        runtime_module,
        "create_goal_evaluator_model",
        lambda **_kwargs: object(),
    )
    monkeypatch.setattr(runtime_module, "evaluate_goal_completion", fake_evaluate)
    runtime = LocalACPRuntime(
        LocalACPConfig(
            config_path=tmp_path / "config.yaml",
            checkpointer_path=tmp_path / "checkpoints.db",
            session_store_path=tmp_path / "sessions.db",
            goal_auto_continue=True,
            goal_max_continuations=8,
            goal_max_no_progress_continuations=2,
        )
    )
    runtime._checkpointer = InMemorySaver()

    async def client_for(_session: LocalACPSession) -> FakeClient:
        return FakeClient()

    monkeypatch.setattr(runtime, "_client_for", client_for)
    session = _make_runtime_session(tmp_path, "goal-no-progress")
    await runtime.set_goal(session.session_id, "finish the task")

    events = [
        event
        async for event in runtime.astream(
            session,
            "finish the task",
            live_event_callback=lambda _event: None,  # type: ignore[arg-type]
        )
    ]

    assert calls[0] == "finish the task"
    assert len(calls) == 3
    assert all(isinstance(message, HumanMessage) for message in calls[1:])
    assert all(
        message.additional_kwargs.get("hide_from_ui") is True
        for message in calls[1:]
        if isinstance(message, HumanMessage)
    )
    statuses = [event.data for event in events if event.type == "custom"]
    assert [status["status"] for status in statuses] == [
        "continuing",
        "continuing",
        "paused",
    ]
    assert statuses[-1]["stand_down_reason"] == "no_progress_limit"
    goal = await runtime.get_goal(session.session_id)
    assert goal is not None
    assert goal["continuation_count"] == 2
    assert goal["no_progress_count"] == 2
    assert goal["last_evaluation"]["stand_down_reason"] == "no_progress_limit"


@pytest.mark.asyncio
async def test_goal_runtime_clears_goal_when_evaluator_reports_completion(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    calls: list[str | HumanMessage] = []

    class FakeClient:
        async def astream(
            self,
            message: str | HumanMessage,
            **kwargs: Any,
        ):
            del kwargs
            calls.append(message)
            if False:
                yield None

    async def fake_evaluate(*args: Any, **kwargs: Any) -> GoalEvaluation:
        del args, kwargs
        return GoalEvaluation(
            satisfied=True,
            blocker="none",
            reason="All requested work is complete.",
            evidence_summary="Tests passed.",
        )

    _configure_local_sandbox(monkeypatch)
    monkeypatch.setattr(
        runtime_module,
        "create_goal_evaluator_model",
        lambda **_kwargs: object(),
    )
    monkeypatch.setattr(runtime_module, "evaluate_goal_completion", fake_evaluate)
    runtime = LocalACPRuntime(
        LocalACPConfig(
            config_path=tmp_path / "config.yaml",
            checkpointer_path=tmp_path / "checkpoints.db",
            session_store_path=tmp_path / "sessions.db",
            goal_auto_continue=True,
        )
    )
    runtime._checkpointer = InMemorySaver()

    async def client_for(_session: LocalACPSession) -> FakeClient:
        return FakeClient()

    monkeypatch.setattr(runtime, "_client_for", client_for)
    session = _make_runtime_session(tmp_path, "goal-completed")
    await runtime.set_goal(session.session_id, "finish the task")

    events = [
        event
        async for event in runtime.astream(
            session,
            "finish the task",
            live_event_callback=lambda _event: None,  # type: ignore[arg-type]
        )
    ]

    assert calls == ["finish the task"]
    assert await runtime.get_goal(session.session_id) is None
    assert [event.data["status"] for event in events] == ["completed"]


@pytest.mark.asyncio
async def test_goal_runtime_evaluates_but_does_not_continue_when_disabled(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    calls: list[str | HumanMessage] = []

    class FakeClient:
        async def astream(
            self,
            message: str | HumanMessage,
            **kwargs: Any,
        ):
            del kwargs
            calls.append(message)
            if False:
                yield None

    async def fake_evaluate(*args: Any, **kwargs: Any) -> GoalEvaluation:
        del args, kwargs
        return GoalEvaluation(
            satisfied=False,
            blocker="goal_not_met_yet",
            reason="More work remains.",
            evidence_summary="Partial result.",
        )

    _configure_local_sandbox(monkeypatch)
    monkeypatch.setattr(
        runtime_module,
        "create_goal_evaluator_model",
        lambda **_kwargs: object(),
    )
    monkeypatch.setattr(runtime_module, "evaluate_goal_completion", fake_evaluate)
    runtime = LocalACPRuntime(
        LocalACPConfig(
            config_path=tmp_path / "config.yaml",
            checkpointer_path=tmp_path / "checkpoints.db",
            session_store_path=tmp_path / "sessions.db",
            goal_auto_continue=False,
        )
    )
    runtime._checkpointer = InMemorySaver()

    async def client_for(_session: LocalACPSession) -> FakeClient:
        return FakeClient()

    monkeypatch.setattr(runtime, "_client_for", client_for)
    session = _make_runtime_session(tmp_path, "goal-no-auto-continue")
    await runtime.set_goal(session.session_id, "finish the task")

    events = [
        event
        async for event in runtime.astream(
            session,
            "finish the task",
            live_event_callback=lambda _event: None,  # type: ignore[arg-type]
        )
    ]

    assert calls == ["finish the task"]
    assert events[0].data["status"] == "paused"
    assert events[0].data["stand_down_reason"] == "auto_continue_disabled"
    goal = await runtime.get_goal(session.session_id)
    assert goal is not None
    assert goal["continuation_count"] == 0


@pytest.mark.asyncio
async def test_goal_runtime_records_model_failure_without_running_evaluator(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    class FakeClient:
        async def astream(
            self,
            message: str | HumanMessage,
            **kwargs: Any,
        ):
            del message, kwargs
            yield StreamEvent(
                type="custom",
                data={"type": "llm_failure", "message": "provider failed"},
            )

    def fail_if_evaluator_created(*args: Any, **kwargs: Any) -> Any:
        del args, kwargs
        raise AssertionError("goal evaluator must not run after a failed turn")

    _configure_local_sandbox(monkeypatch)
    monkeypatch.setattr(
        runtime_module,
        "create_goal_evaluator_model",
        fail_if_evaluator_created,
    )
    runtime = LocalACPRuntime(
        LocalACPConfig(
            config_path=tmp_path / "config.yaml",
            checkpointer_path=tmp_path / "checkpoints.db",
            session_store_path=tmp_path / "sessions.db",
            goal_auto_continue=True,
        )
    )
    runtime._checkpointer = InMemorySaver()

    async def client_for(_session: LocalACPSession) -> FakeClient:
        return FakeClient()

    monkeypatch.setattr(runtime, "_client_for", client_for)
    session = _make_runtime_session(tmp_path, "goal-run-failed")
    await runtime.set_goal(session.session_id, "finish the task")

    events = [
        event
        async for event in runtime.astream(
            session,
            "finish the task",
            live_event_callback=lambda _event: None,  # type: ignore[arg-type]
        )
    ]

    assert [event.data["type"] for event in events] == [
        "llm_failure",
        "goal_status",
    ]
    assert events[-1].data["stand_down_reason"] == "run_failed"
    goal = await runtime.get_goal(session.session_id)
    assert goal is not None
    assert goal["last_evaluation"]["blocker"] == "run_failed"


@pytest.mark.asyncio
async def test_stateless_runtime_preserves_llm_failure_compatibility(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    class FakeClient:
        async def astream(self, _message: str, **_kwargs: Any):
            yield StreamEvent(
                type="custom",
                data={"type": "llm_failure", "message": "provider failed"},
            )

    _configure_local_sandbox(monkeypatch)
    runtime = LocalACPRuntime(
        LocalACPConfig(
            config_path=tmp_path / "config.yaml",
            checkpointer_path=tmp_path / "checkpoints.db",
            session_store_path=tmp_path / "sessions.db",
        )
    )

    async def client_for(_session: LocalACPSession) -> FakeClient:
        return FakeClient()

    monkeypatch.setattr(runtime, "_client_for", client_for)
    session = _make_runtime_session(tmp_path, "stateless-run-failed")

    events = [
        event
        async for event in runtime.astream(
            session,
            "task",
            live_event_callback=lambda _event: None,  # type: ignore[arg-type]
        )
    ]

    assert [event.data["type"] for event in events] == ["llm_failure"]


@pytest.mark.asyncio
async def test_goal_runtime_records_raised_agent_error_before_propagating(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    class FakeClient:
        async def astream(self, _message: str, **_kwargs: Any):
            if False:
                yield None
            raise RuntimeError("tool crashed")

    _configure_local_sandbox(monkeypatch)
    runtime = LocalACPRuntime(
        LocalACPConfig(
            config_path=tmp_path / "config.yaml",
            checkpointer_path=tmp_path / "checkpoints.db",
            session_store_path=tmp_path / "sessions.db",
            goal_auto_continue=True,
        )
    )
    runtime._checkpointer = InMemorySaver()

    async def client_for(_session: LocalACPSession) -> FakeClient:
        return FakeClient()

    monkeypatch.setattr(runtime, "_client_for", client_for)
    session = _make_runtime_session(tmp_path, "goal-raised-error")
    await runtime.set_goal(session.session_id, "finish the task")

    with pytest.raises(RuntimeError, match="tool crashed"):
        async for _ in runtime.astream(
            session,
            "task",
            live_event_callback=lambda _event: None,  # type: ignore[arg-type]
        ):
            pass

    goal = await runtime.get_goal(session.session_id)
    assert goal is not None
    assert goal["last_evaluation"]["blocker"] == "run_failed"


@pytest.mark.asyncio
async def test_history_hides_internal_goal_continuations_and_returns_goal(
    tmp_path: Path,
) -> None:
    visible = HumanMessage(content="visible", id="visible-message")
    hidden = HumanMessage(
        content="hidden continuation",
        id="hidden-message",
        additional_kwargs={"hide_from_ui": True},
    )
    goal = {"objective": "finish", "status": "active"}

    class FakeCheckpointer:
        async def aget_tuple(self, _config: dict[str, Any]) -> Any:
            return SimpleNamespace(
                checkpoint={
                    "channel_values": {
                        "messages": [visible, hidden],
                        "goal": goal,
                    }
                }
            )

    runtime = LocalACPRuntime(
        LocalACPConfig(
            config_path=tmp_path / "config.yaml",
            checkpointer_path=tmp_path / "checkpoints.db",
            session_store_path=tmp_path / "sessions.db",
        )
    )
    runtime._checkpointer = FakeCheckpointer()

    expected_messages = [
        {"type": "human", "content": "visible", "id": "visible-message"}
    ]
    assert await runtime.history("history-goal") == expected_messages
    state = await runtime.history_state("history-goal")
    assert state["messages"] == expected_messages
    assert state["goal"] == goal


@pytest.mark.asyncio
async def test_runtime_queues_distinct_sessions_and_cancels_waiter_cleanly(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    started: list[str] = []
    first_started = asyncio.Event()
    release_first = asyncio.Event()

    class FakeClient:
        async def astream(self, message: str, **kwargs: Any):
            del kwargs
            started.append(message)
            if message == "first":
                first_started.set()
                await release_first.wait()
            if False:
                yield None

    monkeypatch.setattr(
        "deerflow.config.get_app_config",
        lambda: SimpleNamespace(
            sandbox=SimpleNamespace(use="deerflow.sandbox.local:LocalSandboxProvider")
        ),
    )
    runtime = LocalACPRuntime(
        LocalACPConfig(
            config_path=tmp_path / "config.yaml",
            checkpointer_path=tmp_path / "checkpoints.db",
            session_store_path=tmp_path / "sessions.db",
            max_active_runs=1,
        )
    )
    fake_client = FakeClient()

    async def client_for(_session: LocalACPSession) -> FakeClient:
        return fake_client

    monkeypatch.setattr(runtime, "_client_for", client_for)

    def session(session_id: str) -> LocalACPSession:
        return LocalACPSession(
            session_id=session_id,
            cwd=str(tmp_path),
            title=None,
            updated_at="",
            model_name=None,
            thinking_enabled=True,
            subagent_enabled=False,
            plan_mode=False,
            max_concurrent_subagents=1,
            recursion_limit=100,
        )

    async def consume(target: LocalACPSession, message: str) -> None:
        async for _ in runtime.astream(
            target,
            message,
            live_event_callback=lambda _event: None,  # type: ignore[arg-type]
        ):
            pass

    first = asyncio.create_task(consume(session("session-a"), "first"))
    await first_started.wait()
    waiting = asyncio.create_task(consume(session("session-b"), "second"))
    await asyncio.sleep(0)
    assert started == ["first"]

    waiting.cancel()
    with pytest.raises(asyncio.CancelledError):
        await waiting
    assert not first.done()

    release_first.set()
    await first
    assert started == ["first"]


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "provider_path",
    [
        "deerflow.sandbox.aio:AioSandboxProvider",
        "deerflow.sandbox.local:LocalWslProvider",
    ],
)
async def test_runtime_rejects_non_local_provider_for_client_cwd(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, provider_path: str
) -> None:
    monkeypatch.setattr(
        "deerflow.config.get_app_config",
        lambda: type(
            "Config",
            (),
            {
                "sandbox": type(
                    "SandboxConfig",
                    (),
                    {"use": provider_path},
                )()
            },
        )(),
    )
    runtime = LocalACPRuntime(
        LocalACPConfig(
            config_path=tmp_path / "config.yaml",
            checkpointer_path=tmp_path / "checkpoints.db",
            session_store_path=tmp_path / "sessions.db",
        )
    )
    runtime._checkpointer = object()
    session = LocalACPSession(
        session_id="unsupported-workspace",
        cwd=str(tmp_path),
        title=None,
        updated_at="",
        model_name=None,
        thinking_enabled=True,
        subagent_enabled=False,
        plan_mode=False,
        max_concurrent_subagents=1,
        recursion_limit=100,
    )

    with pytest.raises(
        RuntimeError, match="Portable ACP supports only LocalSandboxProvider"
    ):
        async for _ in runtime.astream(
            session,
            "inspect files",
            live_event_callback=lambda _event: None,  # type: ignore[arg-type]
        ):
            pass


def test_embedded_client_excludes_unsafe_local_acp_tools(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(
        "deerflow.tools.get_available_tools",
        lambda **_kwargs: [
            SimpleNamespace(name="read_file"),
            SimpleNamespace(name="bash"),
            SimpleNamespace(name="task"),
            SimpleNamespace(name="task_status"),
            SimpleNamespace(name="invoke_acp_agent"),
        ],
    )
    client = object.__new__(DeerFlowClient)
    client._additional_mcp_tools = []
    client._excluded_tool_names = frozenset(
        {"bash", "task", "task_status", "invoke_acp_agent"}
    )
    client._tool_groups = None
    client._allowed_tool_names = None

    tools = client._get_tools(model_name=None, subagent_enabled=True)

    assert [tool.name for tool in tools] == ["read_file"]
