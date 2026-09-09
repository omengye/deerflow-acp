import subprocess

import pytest

from deerflow.sandbox.aio import AioSandbox
from deerflow.sandbox.local.list_dir import list_dir


def test_local_list_dir_distinguishes_empty_missing_and_file(tmp_path):
    empty = tmp_path / "empty"
    empty.mkdir()
    assert list_dir(str(empty)) == []

    with pytest.raises(FileNotFoundError):
        list_dir(str(tmp_path / "missing"))

    regular_file = tmp_path / "file.txt"
    regular_file.write_text("x", encoding="utf-8")
    with pytest.raises(NotADirectoryError):
        list_dir(str(regular_file))


@pytest.mark.parametrize(
    ("returncode", "exception"),
    [
        (2, FileNotFoundError),
        (3, NotADirectoryError),
        (4, PermissionError),
        (5, RuntimeError),
    ],
)
def test_aio_list_dir_preserves_remote_error_semantics(monkeypatch, returncode, exception):
    sandbox = AioSandbox("id", "container")
    calls = 0

    def fake_exec(*_args, **_kwargs):
        nonlocal calls
        calls += 1
        return subprocess.CompletedProcess([], returncode, stdout="", stderr="failure")

    monkeypatch.setattr(sandbox, "_docker_exec", fake_exec)
    with pytest.raises(exception):
        sandbox.list_dir("/target")
    assert calls == 1
