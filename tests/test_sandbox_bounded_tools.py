import re
from types import SimpleNamespace

import pytest
from langchain.tools import ToolRuntime

from deerflow.sandbox.local.local_sandbox import LocalSandbox, PathMapping
from deerflow.sandbox.tools import grep_tool, ls_tool, read_file_tool


@pytest.fixture
def setup_bounded(tmp_path, monkeypatch):
    sandbox = LocalSandbox("bounded", [PathMapping("/mnt/data", str(tmp_path))])
    monkeypatch.setattr("deerflow.sandbox.tools.ensure_sandbox_initialized", lambda _: sandbox)
    monkeypatch.setattr("deerflow.sandbox.tools.ensure_thread_directories_exist", lambda _: None)
    monkeypatch.setattr("deerflow.sandbox.tools.is_local_sandbox", lambda _: False)
    config = SimpleNamespace(sandbox=SimpleNamespace(read_file_output_max_chars=500, ls_output_max_chars=2000))
    monkeypatch.setattr("deerflow.config.app_config.get_app_config", lambda: config)
    return tmp_path


def runtime():
    return ToolRuntime(state={}, context={}, config={}, stream_writer=lambda _: None, tool_call_id="file-1", store=None)


def test_tool_continues_long_multibyte_line_without_bash(setup_bounded):
    text = "中文🌲abc" * 90
    (setup_bounded / "large.json").write_text(text, encoding="utf-8")
    offset, version = 0, None
    parts = []
    for _ in range(100):
        result = read_file_tool.func(runtime(), "read", "/mnt/data/large.json", offset=offset, expected_version=version)
        assert len(result.content) <= 500
        metadata = result.artifact["file_read"]
        parts.append(result.content.split("\n... [truncated:")[0])
        if not metadata["truncated"]:
            break
        assert metadata["next_offset"] > offset
        offset, version = metadata["next_offset"], metadata["version"]
    else:
        pytest.fail("continuation failed to finish")
    assert "".join(parts) == text


def test_tool_rejects_changed_file_during_continuation(setup_bounded):
    path = setup_bounded / "data.txt"
    path.write_text("a" * 1000, encoding="utf-8")
    first = read_file_tool.func(runtime(), "read", "/mnt/data/data.txt")
    evidence = first.artifact["file_read"]
    path.write_text("changed", encoding="utf-8")
    result = read_file_tool.func(runtime(), "read", "/mnt/data/data.txt", offset=evidence["next_offset"], expected_version=evidence["version"])
    assert isinstance(result, str) and result.startswith("Error:")
    assert "chang" in result.lower()


def test_directory_tool_pages_without_dropping_entries(setup_bounded):
    for i in range(7):
        (setup_bounded / f"file-{i}.txt").write_text("x", encoding="utf-8")
    cursor = None
    entries = []
    for _ in range(10):
        result = ls_tool.func(runtime(), "list", "/mnt/data", max_entries=2, cursor=cursor)
        entries.extend(re.findall(r"/mnt/data/file-\d.txt", result.content))
        cursor = result.artifact["directory_page"]["next_cursor"]
        if cursor is None:
            break
    assert entries == [f"/mnt/data/file-{i}.txt" for i in range(7)]


def test_search_text_explains_partial_coverage(setup_bounded):
    (setup_bounded / "long.txt").write_text("needle" + "x" * 3000, encoding="utf-8")
    result = grep_tool.func(SimpleNamespace(), "search", "needle", "/mnt/data")
    assert "coverage is partial" in result
    assert "long lines" in result


def test_byte_continuation_requires_version_without_restarting_line_window(setup_bounded):
    result = read_file_tool.func(runtime(), "read", "/mnt/data/missing", offset=3)
    assert "requires expected_version" in result
    result = read_file_tool.func(runtime(), "read", "/mnt/data/missing", offset=3, expected_version="v", start_line=1)
    assert "omit start_line" in result
