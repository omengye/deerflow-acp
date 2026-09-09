import asyncio

from deerflow.subagents.config import SubagentConfig
from deerflow.subagents.executor import SubagentExecutor
from deerflow.tools.builtins.task_tool import _snapshot_uploaded_files


def test_upload_boundary_validation_and_copy_isolation():
    parent = {"uploaded_files": [{"filename": "report.pdf", "size": 10}]}
    snapshot = _snapshot_uploaded_files(parent)
    assert snapshot == parent["uploaded_files"]
    assert snapshot is not parent["uploaded_files"]
    parent["uploaded_files"][0]["filename"] = "changed.pdf"
    assert snapshot[0]["filename"] == "report.pdf"

    assert _snapshot_uploaded_files({}) is None
    assert _snapshot_uploaded_files({"uploaded_files": [{"filename": "../escape"}]}) is None


def test_executor_seeds_independent_uploaded_file_state(monkeypatch):
    uploaded = [{"filename": "report.pdf", "size": 10}]
    executor = SubagentExecutor(
        config=SubagentConfig(
            name="test",
            description="test",
            system_prompt="test",
            skills=[],
        ),
        tools=[],
        uploaded_files=uploaded,
    )

    async def no_skills():
        return []

    monkeypatch.setattr(executor, "_load_skills", no_skills)
    first = asyncio.run(executor._build_initial_state("task"))
    first["uploaded_files"][0]["filename"] = "child-change.pdf"
    second = asyncio.run(executor._build_initial_state("task"))

    assert second["uploaded_files"] == [{"filename": "report.pdf", "size": 10}]
    assert uploaded == [{"filename": "report.pdf", "size": 10}]
