from __future__ import annotations

import json
import importlib
from types import SimpleNamespace

from deerflow.tools.builtins import list_uploaded_files_tool

_tool_module = importlib.import_module(
    "deerflow.tools.builtins.list_uploaded_files_tool"
)


def test_uploaded_file_filters_apply_before_listing_cap(tmp_path, monkeypatch) -> None:
    uploads = tmp_path / "uploads"
    uploads.mkdir()
    for index in range(55):
        (uploads / f"unrelated-{index:02d}.txt").write_text("x", encoding="utf-8")
    (uploads / "Quarterly-Report.PDF").write_text("report", encoding="utf-8")

    monkeypatch.setattr(
        _tool_module,
        "get_paths",
        lambda: SimpleNamespace(sandbox_uploads_dir=lambda _thread_id: uploads),
    )
    runtime = SimpleNamespace(
        context={"thread_id": "thread-1"},
        config={},
        state={},
    )

    payload = json.loads(
        list_uploaded_files_tool.func(
            runtime=runtime,
            query="quarterly",
            extensions=["*.pdf"],
        )
    )

    assert payload["count"] == 1
    assert payload["files"][0]["filename"] == "Quarterly-Report.PDF"


def test_uploaded_file_filters_return_clear_empty_result(tmp_path, monkeypatch) -> None:
    uploads = tmp_path / "uploads"
    uploads.mkdir()
    (uploads / "notes.txt").write_text("x", encoding="utf-8")
    monkeypatch.setattr(
        _tool_module,
        "get_paths",
        lambda: SimpleNamespace(sandbox_uploads_dir=lambda _thread_id: uploads),
    )
    runtime = SimpleNamespace(context={"thread_id": "thread-1"}, config={}, state={})

    payload = json.loads(
        list_uploaded_files_tool.func(runtime=runtime, extensions=["pdf"])
    )

    assert payload == {
        "files": [],
        "count": 0,
        "hint": "No uploaded files matched the given filters.",
    }
