from __future__ import annotations

from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest
from langchain_core.messages import AIMessage, HumanMessage
from langgraph.checkpoint.memory import InMemorySaver
from langgraph.checkpoint.sqlite.aio import AsyncSqliteSaver
from langgraph.graph import START, StateGraph

import deerflow.runtime.goal as goal_module
from deerflow.acp.config import LocalACPConfig
from deerflow.agents.goal_state import GoalEvaluation
from deerflow.agents.thread_state import ThreadState
from deerflow.config.app_config import AppConfig, reset_app_config, set_app_config
from deerflow.config.sandbox_config import SandboxConfig
from deerflow.runtime.goal import (
    GoalWriteConflict,
    attach_goal_evaluation,
    build_goal_state,
    compute_no_progress_count,
    evaluate_goal_completion,
    goal_stand_down_reason,
    goal_thread_lock,
    make_goal_continuation_message,
    parse_goal_command,
    parse_goal_evaluation_response,
    read_goal_snapshot,
    should_continue_goal,
    visible_conversation_signature,
    write_thread_goal,
)


def test_parse_goal_command_has_reserved_three_way_semantics() -> None:
    assert parse_goal_command("ordinary chat") is None
    assert parse_goal_command("/goal").kind == "status"  # type: ignore[union-attr]
    assert parse_goal_command(" /GOAL reset ").kind == "clear"  # type: ignore[union-attr]
    command = parse_goal_command("/goal  make every test pass ")
    assert command is not None
    assert command.kind == "set"
    assert command.objective == "make every test pass"


def test_build_goal_state_normalizes_and_bounds_continuations() -> None:
    goal = build_goal_state(
        "  finish   the work ",
        auto_continue=True,
        max_continuations=99,
        max_no_progress_continuations=99,
        now="2026-08-27T00:00:00Z",
    )

    assert goal["objective"] == "finish the work"
    assert goal["auto_continue"] is True
    assert goal["max_continuations"] == 8
    assert goal["max_no_progress_continuations"] == 8
    assert goal["continuation_count"] == 0


def test_parse_goal_evaluation_fails_closed_for_unknown_blocker() -> None:
    result = parse_goal_evaluation_response(
        '```json\n{"satisfied": false, "blocker": "invented", '
        '"reason": "not enough", "evidence_summary": "none"}\n```'
    )

    assert result == {
        "satisfied": False,
        "blocker": "missing_evidence",
        "reason": "not enough",
        "evidence_summary": "none",
    }


def test_goal_evaluator_uses_an_owned_async_http_client(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured: dict[str, Any] = {}
    evaluator = object()

    def fake_create_chat_model(**kwargs: Any) -> object:
        captured.update(kwargs)
        return evaluator

    monkeypatch.setattr(goal_module, "create_chat_model", fake_create_chat_model)

    result = goal_module.create_goal_evaluator_model(model_name="test-model")

    assert result is evaluator
    assert captured == {
        "name": "test-model",
        "thinking_enabled": False,
        "disable_keepalive": True,
    }


@pytest.mark.asyncio
async def test_evaluator_uses_visible_evidence_and_ignores_hidden_messages() -> None:
    captured: dict[str, Any] = {}

    class FakeModel:
        async def ainvoke(self, messages: list[Any], config: dict[str, Any]) -> Any:
            captured["messages"] = messages
            captured["config"] = config
            return SimpleNamespace(
                content=(
                    '{"satisfied": false, "blocker": "goal_not_met_yet", '
                    '"reason": "one test remains", '
                    '"evidence_summary": "the assistant reported one failure"}'
                ),
                usage_metadata={
                    "input_tokens": 8,
                    "output_tokens": 4,
                    "total_tokens": 12,
                },
            )

    goal = build_goal_state("pass all tests", auto_continue=True)
    messages = [
        HumanMessage(content="run the tests"),
        HumanMessage(
            content="internal continuation",
            additional_kwargs={"hide_from_ui": True},
        ),
        AIMessage(content="One test still fails."),
    ]
    usage: dict[str, int] = {}
    set_app_config(AppConfig(sandbox=SandboxConfig(use="test")))
    try:
        result = await evaluate_goal_completion(
            goal,
            messages,
            model=FakeModel(),
            usage_callback=usage.update,
        )
    finally:
        reset_app_config()

    assert result["blocker"] == "goal_not_met_yet"
    evaluator_prompt = captured["messages"][1].content
    assert "One test still fails" in evaluator_prompt
    assert "internal continuation" not in evaluator_prompt
    assert captured["config"]["run_name"] == "goal_evaluator"
    assert usage == {"input_tokens": 8, "output_tokens": 4, "total_tokens": 12}


def test_hidden_continuation_is_not_visible_evaluator_evidence() -> None:
    goal = build_goal_state("finish", auto_continue=True)
    evaluation = GoalEvaluation(
        satisfied=False,
        blocker="goal_not_met_yet",
        reason="more work remains",
        evidence_summary="partial result",
    )
    continuation = make_goal_continuation_message(goal, evaluation)

    assert continuation.additional_kwargs["hide_from_ui"] is True
    assert continuation.additional_kwargs["deerflow_goal_continuation"] is True
    assert "<goal_continuation>" in continuation.content
    assert visible_conversation_signature([continuation]) == "[]"


def test_no_progress_breaker_stops_repeated_identical_evidence() -> None:
    goal = build_goal_state(
        "finish",
        auto_continue=True,
        max_no_progress_continuations=2,
    )
    evaluation = GoalEvaluation(
        satisfied=False,
        blocker="goal_not_met_yet",
        reason="continue",
        evidence_summary="partial",
    )
    first = attach_goal_evaluation(
        goal,
        evaluation,
        no_progress_count=0,
        evidence_signature="same",
    )
    first_repeat = compute_no_progress_count(
        first,
        evaluation,
        evidence_signature="same",
    )
    second = attach_goal_evaluation(
        first,
        evaluation,
        no_progress_count=first_repeat,
        evidence_signature="same",
    )
    second_repeat = compute_no_progress_count(
        second,
        evaluation,
        evidence_signature="same",
    )

    assert first_repeat == 1
    assert second_repeat == 2
    assert (
        should_continue_goal(
            second,
            evaluation,
            no_progress_count=second_repeat,
        )
        is False
    )


@pytest.mark.parametrize(
    "blocker",
    [
        "missing_evidence",
        "needs_user_input",
        "run_failed",
        "external_wait",
    ],
)
def test_non_continuable_goal_blockers_stand_down(blocker: Any) -> None:
    goal = build_goal_state("finish", auto_continue=True)
    evaluation = GoalEvaluation(
        satisfied=False,
        blocker=blocker,
        reason="blocked",
        evidence_summary="",
    )

    assert should_continue_goal(goal, evaluation, no_progress_count=0) is False
    assert (
        goal_stand_down_reason(
            goal,
            evaluation,
            no_progress_count=0,
        )
        == blocker
    )


@pytest.mark.asyncio
async def test_goal_checkpoint_round_trip_clear_and_conflict() -> None:
    saver = InMemorySaver()
    thread_id = "goal-checkpoint"
    goal = build_goal_state("finish", auto_continue=False)

    async with goal_thread_lock(thread_id):
        await write_thread_goal(
            saver,
            thread_id,
            goal,
            create_if_missing=True,
        )
    first = await read_goal_snapshot(saver, thread_id)
    assert first.goal == goal

    replacement = build_goal_state(
        "replacement",
        auto_continue=True,
        now="2026-08-27T01:00:00Z",
    )
    async with goal_thread_lock(thread_id):
        await write_thread_goal(saver, thread_id, replacement)
    with pytest.raises(GoalWriteConflict):
        async with goal_thread_lock(thread_id):
            await write_thread_goal(
                saver,
                thread_id,
                goal,
                expected_checkpoint_id=first.checkpoint_id,
            )

    async with goal_thread_lock(thread_id):
        await write_thread_goal(saver, thread_id, None)
    assert (await read_goal_snapshot(saver, thread_id)).goal is None


@pytest.mark.asyncio
async def test_goal_checkpoint_round_trip_with_portable_sqlite(
    tmp_path: Path,
) -> None:
    database = tmp_path / "acp-checkpoints.db"
    async with AsyncSqliteSaver.from_conn_string(str(database)) as saver:
        await saver.setup()
        goal = build_goal_state("finish", auto_continue=True)
        async with goal_thread_lock("sqlite-goal"):
            await write_thread_goal(
                saver,
                "sqlite-goal",
                goal,
                create_if_missing=True,
            )
        assert (await read_goal_snapshot(saver, "sqlite-goal")).goal == goal

        async with goal_thread_lock("sqlite-goal"):
            await write_thread_goal(saver, "sqlite-goal", None)
        assert (await read_goal_snapshot(saver, "sqlite-goal")).goal is None


@pytest.mark.asyncio
async def test_goal_checkpoint_survives_normal_langgraph_turn_and_clear() -> None:
    saver = InMemorySaver()
    graph_builder = StateGraph(ThreadState)
    graph_builder.add_node("noop", lambda _state: {})
    graph_builder.add_edge(START, "noop")
    graph = graph_builder.compile(checkpointer=saver)
    thread_id = "goal-graph-round-trip"
    config = {"configurable": {"thread_id": thread_id}}
    goal = build_goal_state("finish", auto_continue=True)

    async with goal_thread_lock(thread_id):
        await write_thread_goal(
            saver,
            thread_id,
            goal,
            create_if_missing=True,
        )
    first = await graph.ainvoke(
        {"messages": [HumanMessage(content="first turn")]},
        config,
    )
    assert first["goal"] == goal

    async with goal_thread_lock(thread_id):
        await write_thread_goal(saver, thread_id, None)
    second = await graph.ainvoke(
        {"messages": [HumanMessage(content="second turn")]},
        config,
    )
    assert "goal" not in second or second["goal"] is None


def test_local_acp_goal_config_parses_explicit_settings(tmp_path: Path) -> None:
    config_path = tmp_path / "config.yaml"
    config_path.write_text(
        """
api:
  data_dir: ./data
local_acp:
  goal_auto_continue: true
  goal_max_continuations: 5
  goal_max_no_progress_continuations: 1
""".strip(),
        encoding="utf-8",
    )

    config = LocalACPConfig.from_file(str(config_path))

    assert config.goal_auto_continue is True
    assert config.goal_max_continuations == 5
    assert config.goal_max_no_progress_continuations == 1


def test_local_acp_goal_config_rejects_unbounded_continuations(
    tmp_path: Path,
) -> None:
    config_path = tmp_path / "config.yaml"
    config_path.write_text(
        "local_acp:\n  goal_max_continuations: 9\n",
        encoding="utf-8",
    )

    with pytest.raises(ValueError, match="must be between 0 and 8"):
        LocalACPConfig.from_file(str(config_path))
