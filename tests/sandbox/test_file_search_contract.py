"""One real filesystem contract for Local/WSL and AIO's offline Python seam.

No container or WSL process is started. The remote seam executes the actual
application-owned helper, rather than returning precomputed search answers.
"""

from __future__ import annotations

import json
import re
import subprocess
import sys
from dataclasses import asdict
from pathlib import Path

import pytest

from deerflow.sandbox.aio import AioSandbox
from deerflow.sandbox.file_io import DirectoryPage, FileVersionMismatchError
from deerflow.sandbox.local.local_sandbox import LocalSandbox, PathMapping
from deerflow.sandbox.local.wsl_sandbox import WslSandbox


@pytest.fixture(params=["local", "wsl", "aio"])
def sandbox(request, monkeypatch):
    if request.param == "local":
        return LocalSandbox("contract")
    if request.param == "wsl":
        return WslSandbox("contract")
    instance = AioSandbox("contract", "never-started")

    def execute_helper(argv, *, input_data=None, text=True, **_kwargs):
        assert argv[:2] == ["python3", "-"]
        return subprocess.run([sys.executable, "-", *argv[2:]], input=input_data,
                              text=text, capture_output=True, timeout=15, check=False)

    monkeypatch.setattr(instance, "_docker_exec", execute_helper)
    return instance


@pytest.fixture
def tree(tmp_path):
    root = tmp_path / "project [one]'s 中文"
    root.mkdir()
    (root / "nested").mkdir()
    (root / "node_modules").mkdir()
    (root / "-résumé '中文'.txt").write_text("header\nneedle exact\nNeedle upper\n", encoding="utf-8")
    (root / "nested" / "result.txt").write_text("needle nested\n", encoding="utf-8")
    (root / "node_modules" / "ignored.txt").write_text("needle ignored\n", encoding="utf-8")
    return root


def test_search_contract_handles_paths_case_line_numbers_and_ignores(sandbox, tree):
    result = sandbox.glob_result(str(tree), "**/*.txt")
    assert {Path(p).relative_to(tree).as_posix() for p in result.matches} == {"-résumé '中文'.txt", "nested/result.txt"}
    assert not result.truncated and result.coverage.complete
    result = sandbox.grep_result(str(tree), "needle", case_sensitive=True)
    assert {(Path(m.path).relative_to(tree).as_posix(), m.line_number, m.line) for m in result.matches} == {
        ("-résumé '中文'.txt", 2, "needle exact"), ("nested/result.txt", 1, "needle nested")}
    assert result.coverage.searched_files == 2
    assert result.coverage.complete
    result = sandbox.grep_result(str(tree / "-résumé '中文'.txt"), "needle", literal=True)
    assert [m.line_number for m in result.matches] == [2, 3]
    result = sandbox.grep_result(str(tree), "needle", glob="nested/*.txt")
    assert len(result.matches) == 1 and result.matches[0].line_number == 1
    assert sandbox.glob(str(tree), "**/*.missing") == ([], False)
    assert sandbox.grep(str(tree), "not-present") == ([], False)


@pytest.mark.parametrize("operation", ["glob", "grep"])
def test_exact_limit_is_complete_and_one_extra_match_is_truncated(sandbox, tmp_path, operation):
    for index in range(2):
        (tmp_path / f"{index}.txt").write_text("needle\n", encoding="utf-8")
    function = getattr(sandbox, operation)
    pattern = "*.txt" if operation == "glob" else "needle"
    assert len(function(str(tmp_path), pattern, max_results=2)[0]) == 2
    assert function(str(tmp_path), pattern, max_results=2)[1] is False
    (tmp_path / "2.txt").write_text("needle\n", encoding="utf-8")
    matches, truncated = function(str(tmp_path), pattern, max_results=2)
    assert len(matches) == 2 and truncated


def test_coverage_reports_large_binary_and_long_line_skips(sandbox, tmp_path):
    (tmp_path / "large.txt").write_bytes(b"needle" + b"x" * 1_000_000)
    (tmp_path / "binary.dat").write_bytes(b"needle\0binary")
    (tmp_path / "long.txt").write_text("needle" + "x" * 2200 + "\nneedle visible\n", encoding="utf-8")
    result = sandbox.grep_result(str(tmp_path), "needle")
    assert [(m.line_number, m.line) for m in result.matches] == [(2, "needle visible")]
    assert result.truncated is False
    assert asdict(result.coverage) == {"searched_files": 1, "skipped_large_files": 1,
        "skipped_binary_files": 1, "skipped_long_lines": 1, "skipped_symlinks": 0,
        "read_errors": 0, "complete": False}


@pytest.mark.parametrize("operation", ["glob", "grep", "list_dir_page", "read_file_chunk"])
def test_missing_paths_are_errors(sandbox, tmp_path, operation):
    args = (str(tmp_path / "missing"), "x") if operation in {"glob", "grep"} else (str(tmp_path / "missing"),)
    with pytest.raises(FileNotFoundError):
        getattr(sandbox, operation)(*args)


def test_invalid_regex_and_empty_directory_contract(sandbox, tmp_path):
    with pytest.raises(re.error):
        sandbox.grep(str(tmp_path), "[")
    assert sandbox.glob(str(tmp_path), "*.txt") == ([], False)
    page = sandbox.list_dir_page(str(tmp_path))
    assert page.entries == [] and page.next_cursor is None and not page.truncated


def test_byte_continuations_recover_utf8_long_line_and_crlf(sandbox, tmp_path):
    path = tmp_path / "中文.txt"
    content = "前言\r\n" + "你好🙂" * 70 + "\r\ntail\n"
    path.write_bytes(content.encode("utf-8"))
    recovered = []
    offset, version = 0, None
    for _ in range(300):
        chunk = sandbox.read_file_chunk(str(path), offset=offset, max_bytes=13, expected_version=version)
        assert chunk.bytes_read <= 13
        assert len(chunk.content.encode("utf-8")) == chunk.bytes_read
        assert chunk.offset == offset
        recovered.append(chunk.content)
        if chunk.next_offset is None:
            assert not chunk.truncated
            break
        assert chunk.next_offset > offset
        offset, version = chunk.next_offset, chunk.version
    else:
        pytest.fail("UTF-8 continuation did not finish")
    assert "".join(recovered) == content


def test_line_range_byte_continuation_stays_inside_requested_range(sandbox, tmp_path):
    path = tmp_path / "range.txt"
    lines = ["first", "中文🙂" * 20, "third", "last"]
    path.write_bytes(("\n".join(lines) + "\n").encode())
    chunk = sandbox.read_file_chunk(str(path), start_line=2, end_line=3, max_bytes=11)
    recovered = [chunk.content]
    while chunk.next_offset is not None:
        chunk = sandbox.read_file_chunk(str(path), offset=chunk.next_offset, max_bytes=11,
                                         expected_version=chunk.version, end_line=3)
        recovered.append(chunk.content)
    assert "".join(recovered) == lines[1] + "\n" + lines[2] + "\n"


def test_file_change_rejects_continuation(sandbox, tmp_path):
    path = tmp_path / "version.txt"
    path.write_text("abcdefghijk", encoding="utf-8")
    first = sandbox.read_file_chunk(str(path), max_bytes=4)
    path.write_text("replacement content", encoding="utf-8")
    with pytest.raises(FileVersionMismatchError):
        sandbox.read_file_chunk(str(path), offset=first.next_offset, expected_version=first.version)


def test_directory_pages_have_no_duplicates_and_reject_changed_tree(sandbox, tree):
    entries = []
    cursor = None
    first = None
    while True:
        page = sandbox.list_dir_page(str(tree), limit=1, cursor=cursor)
        first = first or page
        assert len(page.entries) == 1
        entries.extend(page.entries)
        if page.next_cursor is None:
            break
        assert page.truncated
        cursor = page.next_cursor
    assert len(entries) == len(set(entries)) == 3
    assert entries == sorted(entries)
    assert "node_modules" not in "".join(entries)
    (tree / "nested" / "added.txt").write_text("new", encoding="utf-8")
    with pytest.raises(FileVersionMismatchError):
        sandbox.list_dir_page(str(tree), limit=1, cursor=first.next_cursor)


@pytest.mark.parametrize("sandbox_class", [LocalSandbox, WslSandbox])
def test_bounded_reads_work_on_read_only_virtual_mount_and_cursor_hides_host_path(tmp_path, sandbox_class):
    (tmp_path / "one.txt").write_text("中文 content", encoding="utf-8")
    (tmp_path / "two.txt").write_text("other", encoding="utf-8")
    sandbox = sandbox_class("readonly", path_mappings=[PathMapping("/mnt/skills", str(tmp_path), read_only=True)])
    chunk = sandbox.read_file_chunk("/mnt/skills/one.txt", max_bytes=8)
    assert chunk.path == "/mnt/skills/one.txt"
    assert chunk.content == "中文 c"
    page = sandbox.list_dir_page("/mnt/skills", limit=1)
    assert page.entries == ["/mnt/skills/one.txt"]
    import base64
    assert str(tmp_path) not in base64.urlsafe_b64decode(page.next_cursor).decode()
    with pytest.raises(OSError):
        sandbox.write_file("/mnt/skills/one.txt", "denied")


def test_remote_partial_result_failure_is_never_success(monkeypatch):
    sandbox = AioSandbox("contract", "never-started")
    monkeypatch.setattr(sandbox, "_docker_exec", lambda *_args, **_kwargs:
                        subprocess.CompletedProcess([], 4, stdout=json.dumps({"matches": []}), stderr="denied"))
    with pytest.raises(PermissionError):
        sandbox.grep_result("/any", "needle")


def test_unreadable_search_file_never_becomes_an_empty_result(sandbox, tmp_path, monkeypatch):
    path = tmp_path / "denied.txt"
    path.write_text("needle", encoding="utf-8")
    if isinstance(sandbox, AioSandbox):
        execute = sandbox._docker_exec

        def denied_exec(argv, *, input_data=None, **kwargs):
            injected = "def denied_open(*args, **kwargs):\n    raise PermissionError('synthetic read denial')\nPath.open = denied_open\n"
            input_data = input_data.replace('request = json.loads(sys.argv[1])', injected + 'request = json.loads(sys.argv[1])')
            return execute(argv, input_data=input_data, **kwargs)

        monkeypatch.setattr(sandbox, "_docker_exec", denied_exec)
    else:
        original = Path.open

        def denied_open(candidate, *args, **kwargs):
            if candidate == path:
                raise PermissionError("synthetic read denial")
            return original(candidate, *args, **kwargs)

        monkeypatch.setattr(Path, "open", denied_open)
    with pytest.raises(PermissionError):
        sandbox.grep(str(tmp_path), "needle")


def test_directory_page_accepts_dangling_internal_symlink(sandbox, tmp_path):
    link = tmp_path / "dangling"
    try:
        link.symlink_to(tmp_path / "missing-target")
    except OSError:
        pytest.skip("This host does not permit symlink creation")
    page = sandbox.list_dir_page(str(tmp_path))
    assert page.entries == [str(link)]
    assert not page.truncated


def test_local_directory_page_keeps_mapped_link_name_and_excludes_escape(tmp_path):
    mount = tmp_path / "mount"
    mount.mkdir()
    outside = tmp_path / "outside.txt"
    outside.write_text("private", encoding="utf-8")
    dangling = mount / "dangling"
    escaped = mount / "outside-link"
    try:
        dangling.symlink_to(mount / "missing-target")
        escaped.symlink_to(outside)
    except OSError:
        pytest.skip("This host does not permit symlink creation")

    sandbox = LocalSandbox("mapped-links", [PathMapping("/mnt/skills", str(mount))])
    page = sandbox.list_dir_page("/mnt/skills")
    assert page.entries == ["/mnt/skills/dangling"]
    with pytest.raises(PermissionError):
        sandbox.read_file("/mnt/skills/outside-link")


def test_local_directory_page_projects_link_name_without_following_target(tmp_path, monkeypatch):
    from deerflow.sandbox.local import local_sandbox as local_module

    link = tmp_path / "dangling"
    target = tmp_path / "missing-target"
    original_realpath = local_module.os.path.realpath

    def resolve_link(candidate, *args, **kwargs):
        if Path(candidate) == link:
            return str(target)
        return original_realpath(candidate, *args, **kwargs)

    monkeypatch.setattr(local_module.os.path, "realpath", resolve_link)
    monkeypatch.setattr(local_module, "list_dir_page", lambda *_args, **_kwargs:
                        DirectoryPage([str(link)], None, False, "version"))
    sandbox = LocalSandbox("mapped-links", [PathMapping("/mnt/skills", str(tmp_path))])
    assert sandbox.list_dir_page("/mnt/skills").entries == ["/mnt/skills/dangling"]


def test_legacy_remote_read_keeps_content_and_classifies_errors(tmp_path, monkeypatch):
    sandbox = AioSandbox("contract", "never-started")

    def execute(argv, *, input_data=None, **_kwargs):
        return subprocess.run([sys.executable, "-", *argv[2:]], input=input_data,
                              text=True, capture_output=True, timeout=15, check=False)

    monkeypatch.setattr(sandbox, "_docker_exec", execute)
    path = tmp_path / "read.txt"
    path.write_bytes("first\r\n中文\r\nlast\r\n".encode())
    assert sandbox.read_file(str(path)) == "first\n中文\nlast\n"
    assert sandbox.read_file(str(path), start_line=2, end_line=2) == "中文\n"
    with pytest.raises(FileNotFoundError):
        sandbox.read_file(str(tmp_path / "missing"))
    with pytest.raises(IsADirectoryError):
        sandbox.read_file(str(tmp_path))
    monkeypatch.setattr(sandbox, "_docker_exec", lambda *_args, **_kwargs:
                        subprocess.CompletedProcess([], 4, stdout="", stderr="denied"))
    with pytest.raises(PermissionError):
        sandbox.read_file(str(path))


def test_large_line_reads_are_bounded_at_the_file_handle(tmp_path, monkeypatch):
    import builtins

    from deerflow.sandbox import file_io

    path = tmp_path / "large.txt"
    path.write_bytes(b"x" * 2_000_000 + b"\nsecond\n")
    original = builtins.open
    sizes = []

    class Reader:
        def __init__(self, handle):
            self.handle = handle

        def __enter__(self):
            self.handle.__enter__()
            return self

        def __exit__(self, *args):
            return self.handle.__exit__(*args)

        def __getattr__(self, key):
            return getattr(self.handle, key)

        def read(self, size=-1):
            sizes.append(size)
            assert 0 <= size <= 65536
            return self.handle.read(size)

        def readline(self, size=-1):
            sizes.append(size)
            assert 0 <= size <= 65536
            return self.handle.readline(size)

    monkeypatch.setattr(file_io, "open", lambda *args, **kwargs: Reader(original(*args, **kwargs)), raising=False)
    chunk = file_io.read_file_chunk(str(path), max_bytes=4096)
    assert chunk.content == "x" * 4096 and chunk.next_offset == 4096
    assert chunk.size == 2_000_008
    selected = file_io.read_file_chunk(str(path), start_line=2, max_bytes=4096)
    assert selected.content == "second\n" and selected.next_offset is None
    assert sizes and -1 not in sizes
