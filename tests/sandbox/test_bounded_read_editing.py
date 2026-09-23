from __future__ import annotations

from types import SimpleNamespace

import pytest

from deerflow.sandbox.local.local_sandbox import LocalSandbox, PathMapping
from deerflow.sandbox.local.wsl_sandbox import WslSandbox
from deerflow.sandbox.tools import read_file_tool, str_replace_tool


@pytest.fixture(params=[LocalSandbox, WslSandbox])
def file_sandbox(request, tmp_path, monkeypatch):
    root = tmp_path / "workspace"
    root.mkdir()
    sandbox = request.param("editing", path_mappings=[PathMapping("/mnt/data", str(root))])
    monkeypatch.setattr("deerflow.sandbox.tools.ensure_sandbox_initialized", lambda _: sandbox)
    monkeypatch.setattr("deerflow.sandbox.tools.ensure_thread_directories_exist", lambda _: None)
    monkeypatch.setattr("deerflow.sandbox.tools.is_local_sandbox", lambda _: False)
    config = SimpleNamespace(sandbox=SimpleNamespace(read_file_output_max_chars=50000))
    monkeypatch.setattr("deerflow.config.app_config.get_app_config", lambda: config)
    return sandbox, root


def test_default_bounded_read_remains_usable_for_string_replacement(file_sandbox):
    sandbox, _root = file_sandbox
    path = "/mnt/data/agent.txt"
    original = "input = /mnt/data/target.txt"
    sandbox.write_file(path, original)
    runtime = SimpleNamespace(state={}, context={}, config={})
    shown = read_file_tool.func(runtime, "read", path)
    assert shown == sandbox.read_file(path) == original
    assert str_replace_tool.func(runtime, "replace", path, shown, "updated") == "OK"
    assert sandbox.read_file(path) == "updated"


def test_bounded_read_preserves_user_uploaded_host_path_content(file_sandbox):
    sandbox, root = file_sandbox
    original = f"User-supplied text: {root.as_posix()}/target.txt"
    (root / "upload.txt").write_text(original, encoding="utf-8")
    runtime = SimpleNamespace(state={}, context={}, config={})
    assert read_file_tool.func(runtime, "read", "/mnt/data/upload.txt") == original
    assert sandbox.read_file_chunk("/mnt/data/upload.txt").content == original


@pytest.mark.parametrize("budget", [4, 11, 37])
def test_source_byte_continuations_do_not_split_or_repeat_virtualized_paths(file_sandbox, budget):
    sandbox, root = file_sandbox
    path = "/mnt/data/paths.txt"
    original = "前缀🙂 /mnt/data/first.txt /mnt/data/nested/second.txt end"
    sandbox.write_file(path, original)
    source = (root / "paths.txt").read_bytes()
    assert len(source) > len(original.encode("utf-8"))
    pieces = []
    offset, version = 0, None
    for _ in range(300):
        chunk = sandbox.read_file_chunk(path, offset=offset, expected_version=version, max_bytes=budget)
        assert chunk.offset == offset
        assert chunk.bytes_read <= budget
        display = getattr(chunk, "display_content", None)
        pieces.append(display if display is not None else chunk.content)
        if chunk.next_offset is None:
            assert chunk.offset + chunk.bytes_read == len(source)
            break
        assert chunk.next_offset == chunk.offset + chunk.bytes_read
        assert chunk.next_offset > offset
        offset, version = chunk.next_offset, chunk.version
    else:
        pytest.fail("Continuation did not finish")
    assert "".join(pieces) == original
