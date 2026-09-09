from types import SimpleNamespace

from langchain_core.messages import AIMessage

from langchain.agents.middleware import AgentMiddleware

from deerflow.agents.middlewares.loop_detection_middleware import (
    AGENT_TERMINATION_KEY,
    LoopDetectionMiddleware,
    TOOL_CALL_LIMIT_STOP_REASON,
    _derive_total_call_limits,
    calibrate_loop_detection,
    count_steps_per_turn,
)


def _runtime(**context):
    return SimpleNamespace(context=context)


def _state(tool_name: str, index: int):
    return {
        "messages": [
            AIMessage(
                content="",
                tool_calls=[
                    {
                        "name": tool_name,
                        "args": {"query": f"item-{index}"},
                        "id": f"call-{index}",
                    }
                ],
            )
        ]
    }


def test_tool_frequency_limit_is_scoped_to_run_id():
    middleware = LoopDetectionMiddleware(
        warn_threshold=100,
        hard_limit=100,
        tool_freq_warn=100,
        tool_freq_hard_limit=3,
    )

    for index in range(10):
        result = middleware.after_model(
            _state("lookup", index),
            _runtime(thread_id="thread-a", loop_detection_scope_id=f"thread-a:run-{index}"),
        )
        assert result is None


def test_tool_frequency_limit_still_applies_within_same_run():
    middleware = LoopDetectionMiddleware(
        warn_threshold=100,
        hard_limit=100,
        tool_freq_warn=100,
        tool_freq_hard_limit=3,
    )

    assert middleware.after_model(_state("lookup", 1), _runtime(thread_id="thread-a", loop_detection_scope_id="thread-a:run-1")) is None
    assert middleware.after_model(_state("lookup", 2), _runtime(thread_id="thread-a", loop_detection_scope_id="thread-a:run-1")) is None

    result = middleware.after_model(
        _state("lookup", 3),
        _runtime(thread_id="thread-a", loop_detection_scope_id="thread-a:run-1"),
    )

    assert result is not None
    assert "messages" in result


def test_soft_warning_in_batch_cannot_mask_later_hard_stop():
    middleware = LoopDetectionMiddleware(
        warn_threshold=100,
        hard_limit=100,
        tool_freq_warn=2,
        tool_freq_hard_limit=3,
    )
    runtime = _runtime(thread_id="thread-a", loop_detection_scope_id="thread-a:run-1")

    middleware.after_model(_state("hard_tool", 0), runtime)
    middleware.after_model(
        {
            "messages": [
                AIMessage(
                    content="",
                    tool_calls=[
                        {"name": "warn_tool", "args": {"query": "a"}, "id": "warn-1"},
                        {"name": "hard_tool", "args": {"query": "b"}, "id": "hard-1"},
                    ],
                )
            ]
        },
        runtime,
    )

    result = middleware.after_model(
        {
            "messages": [
                AIMessage(
                    content="",
                    tool_calls=[
                        {"name": "warn_tool", "args": {"query": "c"}, "id": "warn-2"},
                        {"name": "hard_tool", "args": {"query": "d"}, "id": "hard-2"},
                    ],
                )
            ]
        },
        runtime,
    )

    assert result is not None
    assert result["messages"][0].tool_calls == []
    assert "hard_tool" in result["messages"][0].content
    assert "warn_tool" not in middleware._tool_freq_warned.get("thread-a:run-1", set())


def test_tool_frequency_uses_sliding_window_and_rearms_warning():
    middleware = LoopDetectionMiddleware(
        window_size=3,
        warn_threshold=100,
        hard_limit=100,
        tool_freq_warn=2,
        tool_freq_hard_limit=3,
    )
    runtime = _runtime(thread_id="thread-a", loop_detection_scope_id="thread-a:run-1")

    for index, name in enumerate(("lookup", "other", "lookup")):
        middleware.after_model(_state(name, index), runtime)
    assert "lookup" in middleware._tool_freq_warned["thread-a:run-1"]

    middleware.after_model(_state("other", 3), runtime)
    assert "lookup" not in middleware._tool_freq_warned["thread-a:run-1"]

    middleware.after_model(_state("lookup", 4), runtime)
    assert "lookup" in middleware._tool_freq_warned["thread-a:run-1"]


def test_total_call_hard_limit_stops_long_diverse_runs():
    """Every call differs and no single tool repeats enough — only the per-run
    total backstop can stop it before the graph recursion_limit aborts."""
    middleware = LoopDetectionMiddleware(
        warn_threshold=1000,
        hard_limit=1000,
        tool_freq_warn=1000,
        tool_freq_hard_limit=1000,
        total_call_warn=60,
        total_call_hard_limit=90,
    )
    runtime = _runtime(thread_id="thread-a", loop_detection_scope_id="thread-a:run-1")

    # First 89 diverse calls: no forced stop (a wrap-up warning is queued at 60).
    for index in range(89):
        assert middleware.after_model(_state("lookup", index), runtime) is None

    # 90th call trips the total hard limit and strips tool_calls to force a final answer.
    result = middleware.after_model(_state("lookup", 89), runtime)
    assert result is not None
    message = result["messages"][0]
    assert message.tool_calls == []
    termination = message.additional_kwargs[AGENT_TERMINATION_KEY]
    assert termination["reason"] == TOOL_CALL_LIMIT_STOP_REASON
    assert termination["incomplete"] is True
    assert "Total tool calls reached 90" in termination["message"]
    assert runtime.context["stop_reason"] == TOOL_CALL_LIMIT_STOP_REASON


def test_total_call_warn_is_queued_before_hard_limit():
    middleware = LoopDetectionMiddleware(
        warn_threshold=1000,
        hard_limit=1000,
        tool_freq_warn=1000,
        tool_freq_hard_limit=1000,
        total_call_warn=60,
        total_call_hard_limit=90,
    )
    runtime = _runtime(thread_id="thread-a", loop_detection_scope_id="thread-a:run-1")

    for index in range(59):
        assert middleware.after_model(_state("lookup", index), runtime) is None
        assert not any(middleware._pending_warnings.values())

    # 60th call queues a deferred wrap-up warning (returned as None from after_model).
    assert middleware.after_model(_state("lookup", 59), runtime) is None
    assert any(middleware._pending_warnings.values())


def test_total_call_limits_track_recursion_budget():
    # Default budget: recursion 200 at 5 steps/turn -> 40 turns -> warn 22, hard 32.
    assert _derive_total_call_limits(200) == (22, 32)
    assert _derive_total_call_limits(None) == (22, 32)
    # Linkage: caps track recursion_limit AND per-turn cost.
    assert _derive_total_call_limits(200, steps_per_turn=2) == (55, 80)  # 100 turns
    assert _derive_total_call_limits(100, steps_per_turn=5) == (11, 16)  # 20 turns
    # The hard cap stays comfortably below the achievable turns (recursion/steps).
    warn, hard = _derive_total_call_limits(200, steps_per_turn=5)
    assert hard < 200 // 5
    # Degenerate inputs stay ordered (1 <= warn < hard).
    w, h = _derive_total_call_limits(2, steps_per_turn=5)
    assert 1 <= w < h
    assert _derive_total_call_limits(0) == (22, 32)  # invalid -> default


def test_count_steps_per_turn_counts_model_phase_nodes():
    class Before(AgentMiddleware):
        def before_model(self, state, runtime):  # noqa: ARG002
            return None

    class After(AgentMiddleware):
        def after_model(self, state, runtime):  # noqa: ARG002
            return None

    class Plain(AgentMiddleware):
        pass

    # model + tools (2) + 1 before_model node + 1 after_model node = 4.
    assert count_steps_per_turn([Before(), After(), Plain()]) == 4
    # LoopDetectionMiddleware itself overrides after_model -> contributes a node.
    assert count_steps_per_turn([LoopDetectionMiddleware()]) == 3


def test_calibrate_loop_detection_uses_real_chain_cost():
    class After(AgentMiddleware):
        def after_model(self, state, runtime):  # noqa: ARG002
            return None

    loop = LoopDetectionMiddleware()
    # Chain has 2 after_model nodes (After + LoopDetection) -> 4 steps/turn.
    chain = [After(), loop]
    calibrate_loop_detection(chain, recursion_limit=200)
    # 200 / 4 = 50 turns -> warn 27, hard 40.
    assert (loop.total_call_warn, loop.total_call_hard_limit) == _derive_total_call_limits(200, steps_per_turn=4)
    assert loop.total_call_hard_limit < 200 // 4


def test_middleware_derives_caps_from_recursion_limit():
    # recursion 100 at default 5 steps/turn -> 20 turns -> warn 11, hard 16.
    middleware = LoopDetectionMiddleware(recursion_limit=100)
    assert middleware.total_call_warn == 11
    assert middleware.total_call_hard_limit == 16

    # Explicit values still override the derived ones.
    overridden = LoopDetectionMiddleware(recursion_limit=100, total_call_hard_limit=10)
    assert overridden.total_call_hard_limit == 10
