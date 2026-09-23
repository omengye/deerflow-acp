from __future__ import annotations

import os
from pathlib import Path
from types import SimpleNamespace

import pytest
import yaml

from deerflow.sandbox import tools as sandbox_tools
from deerflow.sandbox.local.local_sandbox import LocalSandbox
from deerflow.sandbox.tools import (
    delete_path_tool,
    move_path_tool,
    read_file_tool,
    write_file_tool,
)


def _runtime(tmp_path: Path) -> SimpleNamespace:
    workspace = tmp_path / "project"
    uploads = tmp_path / "internal" / "uploads"
    outputs = tmp_path / "internal" / "outputs"
    workspace.mkdir(parents=True)
    uploads.mkdir(parents=True)
    outputs.mkdir(parents=True)
    return SimpleNamespace(
        context={"thread_id": "acp-session"},
        config={},
        state={
            "sandbox": {"sandbox_id": "local"},
            "thread_data": {
                "thread_id": "acp-session",
                "workspace_path": str(workspace),
                "workspace_path_managed": False,
                "uploads_path": str(uploads),
                "outputs_path": str(outputs),
            },
        },
    )


@pytest.fixture
def local_runtime(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    runtime = _runtime(tmp_path)
    sandbox = LocalSandbox("local")
    monkeypatch.setattr(
        sandbox_tools,
        "ensure_sandbox_initialized",
        lambda _runtime: sandbox,
    )
    return runtime


def _workspace(runtime: SimpleNamespace) -> Path:
    return Path(runtime.state["thread_data"]["workspace_path"])


def test_file_tools_read_and_write_the_bound_external_workspace(local_runtime) -> None:
    write_result = write_file_tool.func(
        local_runtime,
        description="create a project note",
        path="/mnt/user-data/workspace/note.txt",
        content="bound workspace",
        append=False,
    )
    read_result = read_file_tool.func(
        local_runtime,
        description="read the project note",
        path="/mnt/user-data/workspace/note.txt",
        start_line=None,
        end_line=None,
    )

    assert write_result == "OK"
    assert read_result == "bound workspace"
    assert (_workspace(local_runtime) / "note.txt").read_text(encoding="utf-8") == "bound workspace"


def test_file_tool_does_not_recreate_a_removed_external_workspace(local_runtime) -> None:
    workspace = _workspace(local_runtime)
    workspace.rmdir()

    result = write_file_tool.func(
        local_runtime,
        description="must not recreate a missing client project",
        path="/mnt/user-data/workspace/note.txt",
        content="unexpected",
        append=False,
    )

    assert result.startswith("Error:")
    assert not workspace.exists()


@pytest.mark.parametrize("filename", ["config.example.yaml"])
def test_path_mutation_tools_are_registered_in_project_configs(filename: str) -> None:
    project_root = Path(__file__).resolve().parents[2]
    raw = yaml.safe_load((project_root / filename).read_text(encoding="utf-8"))
    tools = {item["name"]: item["use"] for item in raw["tools"]}

    assert tools["move_path"] == "deerflow.sandbox.tools:move_path_tool"
    assert tools["delete_path"] == "deerflow.sandbox.tools:delete_path_tool"


def test_move_path_renames_files_and_creates_destination_parents(local_runtime) -> None:
    workspace = _workspace(local_runtime)
    source = workspace / "draft.txt"
    source.write_text("draft", encoding="utf-8")

    result = move_path_tool.func(
        local_runtime,
        description="organize the document",
        source="/mnt/user-data/workspace/draft.txt",
        destination="/mnt/user-data/workspace/archive/final.txt",
        overwrite=False,
    )

    assert result == "OK"
    assert not source.exists()
    assert (workspace / "archive" / "final.txt").read_text(encoding="utf-8") == "draft"


def test_move_path_requires_explicit_overwrite(local_runtime) -> None:
    workspace = _workspace(local_runtime)
    source = workspace / "source.txt"
    destination = workspace / "destination.txt"
    source.write_text("new", encoding="utf-8")
    destination.write_text("old", encoding="utf-8")

    rejected = move_path_tool.func(
        local_runtime,
        description="rename without data loss",
        source="/mnt/user-data/workspace/source.txt",
        destination="/mnt/user-data/workspace/destination.txt",
        overwrite=False,
    )
    accepted = move_path_tool.func(
        local_runtime,
        description="replace the old destination",
        source="/mnt/user-data/workspace/source.txt",
        destination="/mnt/user-data/workspace/destination.txt",
        overwrite=True,
    )

    assert rejected == "Error: Destination already exists: /mnt/user-data/workspace/destination.txt"
    assert accepted == "OK"
    assert destination.read_text(encoding="utf-8") == "new"


def test_move_path_never_deletes_a_source_moved_onto_itself(local_runtime) -> None:
    source = _workspace(local_runtime) / "same.txt"
    source.write_text("keep", encoding="utf-8")

    result = move_path_tool.func(
        local_runtime,
        description="reject the same source and destination",
        source="/mnt/user-data/workspace/same.txt",
        destination="/mnt/user-data/workspace/same.txt",
        overwrite=True,
    )

    assert result == "Error: Destination already exists: /mnt/user-data/workspace/same.txt"
    assert source.read_text(encoding="utf-8") == "keep"


def test_move_path_moves_directory_trees(local_runtime) -> None:
    workspace = _workspace(local_runtime)
    source = workspace / "drafts"
    source.mkdir()
    (source / "note.txt").write_text("note", encoding="utf-8")

    result = move_path_tool.func(
        local_runtime,
        description="rename the folder",
        source="/mnt/user-data/workspace/drafts",
        destination="/mnt/user-data/workspace/notes",
        overwrite=False,
    )

    assert result == "OK"
    assert not source.exists()
    assert (workspace / "notes" / "note.txt").is_file()


def test_delete_path_requires_recursive_for_nonempty_directories(local_runtime) -> None:
    workspace = _workspace(local_runtime)
    target = workspace / "obsolete"
    target.mkdir()
    (target / "old.txt").write_text("old", encoding="utf-8")

    rejected = delete_path_tool.func(
        local_runtime,
        description="remove the old folder",
        path="/mnt/user-data/workspace/obsolete",
        recursive=False,
    )
    assert rejected.startswith("Error: Failed to delete path")
    assert target.exists()

    accepted = delete_path_tool.func(
        local_runtime,
        description="remove the old folder tree",
        path="/mnt/user-data/workspace/obsolete",
        recursive=True,
    )

    assert accepted == "OK"
    assert not target.exists()


def test_delete_path_cannot_delete_workspace_root_or_traverse(local_runtime) -> None:
    root_result = delete_path_tool.func(
        local_runtime,
        description="unsafe root delete",
        path="/mnt/user-data/workspace",
        recursive=True,
    )
    traversal_result = delete_path_tool.func(
        local_runtime,
        description="unsafe traversal",
        path="/mnt/user-data/workspace/../outputs/report.txt",
        recursive=False,
    )

    assert "Permission denied" in root_result
    assert "Permission denied" in traversal_result
    assert _workspace(local_runtime).is_dir()


def test_delete_symlink_does_not_follow_target_outside_workspace(local_runtime) -> None:
    workspace = _workspace(local_runtime)
    outside = workspace.parent / "outside"
    outside.mkdir()
    protected = outside / "protected.txt"
    protected.write_text("keep", encoding="utf-8")
    link = workspace / "outside-link"
    try:
        os.symlink(outside, link, target_is_directory=True)
    except (OSError, NotImplementedError):
        pytest.skip("Creating directory symlinks is unavailable")

    result = delete_path_tool.func(
        local_runtime,
        description="remove only the link",
        path="/mnt/user-data/workspace/outside-link",
        recursive=True,
    )

    assert result == "OK"
    assert not link.exists()
    assert protected.read_text(encoding="utf-8") == "keep"


def test_move_destination_cannot_escape_through_symlink(local_runtime) -> None:
    workspace = _workspace(local_runtime)
    outside = workspace.parent / "outside"
    outside.mkdir()
    source = workspace / "source.txt"
    source.write_text("data", encoding="utf-8")
    link = workspace / "outside-link"
    try:
        os.symlink(outside, link, target_is_directory=True)
    except (OSError, NotImplementedError):
        pytest.skip("Creating directory symlinks is unavailable")

    result = move_path_tool.func(
        local_runtime,
        description="attempt an unsafe move",
        source="/mnt/user-data/workspace/source.txt",
        destination="/mnt/user-data/workspace/outside-link/escaped.txt",
        overwrite=False,
    )

    assert "Permission denied" in result
    assert source.is_file()
    assert not (outside / "escaped.txt").exists()
