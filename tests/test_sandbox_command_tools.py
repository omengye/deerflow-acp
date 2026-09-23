"""Execution receipts, acceptance, and cancellation at the actual tool boundary."""
import asyncio
import threading
from types import SimpleNamespace

import pytest
from langchain.tools import ToolRuntime
from langchain_core.messages import AIMessage, ToolMessage

from deerflow.sandbox.command import CommandResult
from deerflow.sandbox.tools import bash_tool
from deerflow.subagents.acceptance_checks import check_acceptance_criteria


def runtime():
    return ToolRuntime(state={}, context={}, config={}, stream_writer=lambda _: None, tool_call_id="cmd-1", store=None)


def setup_tool(monkeypatch, sandbox, limit=20000):
    monkeypatch.setattr("deerflow.sandbox.tools.ensure_sandbox_initialized", lambda _: sandbox)
    monkeypatch.setattr("deerflow.sandbox.tools.is_local_sandbox", lambda _: False)
    monkeypatch.setattr("deerflow.sandbox.tools.ensure_thread_directories_exist", lambda _: None)
    monkeypatch.setattr("deerflow.sandbox.tools.get_app_config", lambda: SimpleNamespace(
        sandbox=SimpleNamespace(bash_output_max_chars=limit),
    ))


def call():
    return {"name": "bash", "id": "cmd-1", "type": "tool_call", "args": {
        "runtime": runtime(), "description": "run tests", "command": "pytest -q",
    }}


def verdict(message):
    request = AIMessage(content="", tool_calls=[{
        "id": "cmd-1", "name": "bash", "args": {"command": "pytest -q"},
    }])
    return check_acceptance_criteria(
        ["tests_passed:pytest -q"], thread_data=None, messages=[request, message],
    )["leaves"][0]["holds"]


@pytest.mark.parametrize("exit_code,status,confirmed,accepted", [
    (0, "completed", True, True),
    (1, "completed", True, False),
    (None, "timed_out", True, False),
    (None, "cancelled", True, False),
    (None, "unknown", False, False),
    (0, "completed", False, False),
])
def test_acceptance_uses_execution_outcome_not_passing_stdout(monkeypatch, exit_code, status, confirmed, accepted):
    result = CommandResult("1 passed in 0.01s", exit_code, status, confirmed)
    sandbox = SimpleNamespace(execute_command_result=lambda *a, **k: result)
    setup_tool(monkeypatch, sandbox)
    message = bash_tool.invoke(call())
    assert isinstance(message, ToolMessage)
    assert message.status == ("success" if accepted else "error")
    assert message.artifact["sandbox_command"]["status"] == status
    assert verdict(message) is accepted


def test_plain_legacy_text_does_not_attest_success(monkeypatch):
    setup_tool(monkeypatch, SimpleNamespace(execute_command=lambda _: "1 passed"))
    message = bash_tool.invoke(call())
    assert message.status == "error"
    assert message.artifact["sandbox_command"]["status"] == "unknown"
    assert verdict(message) is False
    assert verdict(ToolMessage(content="1 passed", tool_call_id="cmd-1", name="bash")) is False


def test_receipt_survives_output_budget_and_is_bound_to_exact_command(monkeypatch):
    result = CommandResult("1 passed\n" + "x" * 1000, 0, "completed", True)
    setup_tool(monkeypatch, SimpleNamespace(execute_command_result=lambda *a, **k: result), limit=200)
    message = bash_tool.invoke(call())
    assert len(message.content) <= 200
    assert message.artifact["sandbox_command"]["exit_code"] == 0
    assert verdict(message) is True
    message.artifact["sandbox_command"]["command"] = "different-command"
    assert verdict(message) is False


@pytest.mark.asyncio
async def test_repeated_cancel_drains_command_before_return(monkeypatch):
    started, cancelling, finish = (threading.Event() for _ in range(3))
    ledger = []

    def execute(command, *, cancel_event):
        started.set()
        assert cancel_event.wait(3)
        cancelling.set()
        assert finish.wait(3)
        ledger.append("cleaned")
        return CommandResult("cancelled", None, "cancelled", True)

    setup_tool(monkeypatch, SimpleNamespace(execute_command_result=execute))
    task = asyncio.create_task(bash_tool.ainvoke(call()))
    try:
        for _ in range(300):
            if started.is_set():
                break
            await asyncio.sleep(0.01)
        assert started.is_set()
        task.cancel()
        for _ in range(300):
            if cancelling.is_set():
                break
            await asyncio.sleep(0.01)
        assert cancelling.is_set()
        task.cancel()
        await asyncio.sleep(0.01)
        assert not task.done()
    finally:
        finish.set()
    with pytest.raises(asyncio.CancelledError):
        await asyncio.wait_for(task, 3)
    assert ledger == ["cleaned"]


@pytest.mark.asyncio
async def test_graph_cancellation_waits_for_owned_bash_cleanup(monkeypatch):
    """The graph must keep its caller's workspace guard until the tool drains."""
    from langgraph.graph import END, START, StateGraph

    started, cancelling, finish = (threading.Event() for _ in range(3))
    cleaned = []

    def execute(command, *, cancel_event):
        started.set()
        assert cancel_event.wait(5)
        cancelling.set()
        assert finish.wait(5)
        cleaned.append(True)
        return CommandResult("cancelled", None, "cancelled", True)

    setup_tool(monkeypatch, SimpleNamespace(execute_command_result=execute))

    async def command_node(state):
        await bash_tool.ainvoke(call())
        return state

    builder = StateGraph(dict)
    builder.add_node("command", command_node)
    builder.add_edge(START, "command")
    builder.add_edge("command", END)
    task = asyncio.create_task(builder.compile().ainvoke({}))
    try:
        async with asyncio.timeout(3):
            while not started.is_set():
                await asyncio.sleep(0.01)
        task.cancel()
        async with asyncio.timeout(3):
            while not cancelling.is_set():
                await asyncio.sleep(0.01)
        await asyncio.sleep(0.02)
        assert not task.done()
    finally:
        finish.set()
    with pytest.raises(asyncio.CancelledError):
        await asyncio.wait_for(task, 3)
    assert cleaned == [True]
