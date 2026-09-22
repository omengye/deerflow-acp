from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest
from fastapi import HTTPException

from app.routers import uploads
from deerflow.client import DeerFlowClient
from deerflow.uploads import manager as upload_manager


def _symlink_or_skip(link: Path, target: Path) -> None:
    try:
        link.symlink_to(target)
    except OSError as exc:
        if getattr(exc, "winerror", None) == 1314:
            pytest.skip("Windows symlinks require Developer Mode or elevation")
        raise


@pytest.mark.parametrize("entrypoint", ["helper", "sdk", "api"])
async def test_upload_delete_never_deletes_a_symlink_target(tmp_path, monkeypatch, entrypoint):
    victim = tmp_path / "victim.pdf"
    victim.write_bytes(b"keep-pdf")
    companion = tmp_path / "victim.md"
    companion.write_text("keep-conversion", encoding="utf-8")
    alias = tmp_path / "alias.pdf"
    _symlink_or_skip(alias, victim)
    client = DeerFlowClient.__new__(DeerFlowClient)
    monkeypatch.setattr("deerflow.client.get_uploads_dir", lambda _thread: tmp_path)
    monkeypatch.setattr(
        uploads,
        "get_client_manager",
        lambda: SimpleNamespace(get_client=lambda: client, touch_thread_activity=AsyncMock()),
    )

    if entrypoint == "helper":
        with pytest.raises(FileNotFoundError):
            upload_manager.delete_file_safe(tmp_path, "alias.pdf", convertible_extensions={".pdf"})
    elif entrypoint == "sdk":
        with pytest.raises(FileNotFoundError):
            client.delete_upload("thread-1", "alias.pdf")
    else:
        with pytest.raises(HTTPException) as raised:
            await uploads.delete_upload("thread-1", "alias.pdf")
        assert raised.value.status_code == 404

    assert victim.read_bytes() == b"keep-pdf"
    assert companion.read_text(encoding="utf-8") == "keep-conversion"
    assert alias.is_symlink()


def test_upload_delete_rejects_outside_symlink_and_keeps_file(tmp_path):
    uploads_dir = tmp_path / "uploads"
    uploads_dir.mkdir()
    outside = tmp_path / "outside.txt"
    outside.write_text("keep", encoding="utf-8")
    _symlink_or_skip(uploads_dir / "alias.txt", outside)
    with pytest.raises(upload_manager.PathTraversalError):
        upload_manager.delete_file_safe(uploads_dir, "alias.txt")
    assert outside.read_text(encoding="utf-8") == "keep"


def test_upload_delete_removes_regular_file_and_its_conversion(tmp_path):
    (tmp_path / "report.pdf").write_bytes(b"pdf")
    (tmp_path / "report.md").write_text("converted", encoding="utf-8")
    result = upload_manager.delete_file_safe(tmp_path, "report.pdf", convertible_extensions={".pdf"})
    assert result["success"] is True
    assert not (tmp_path / "report.pdf").exists()
    assert not (tmp_path / "report.md").exists()


def test_upload_delete_rejects_symlink_before_unlink_without_platform_privilege(tmp_path, monkeypatch):
    # Also exercise the guard on Windows hosts where real symlink tests skip.
    alias = tmp_path / "alias.pdf"
    alias.write_bytes(b"stand-in")
    original = Path.is_symlink
    monkeypatch.setattr(Path, "is_symlink", lambda path: path == alias or original(path))
    with pytest.raises(FileNotFoundError):
        upload_manager.delete_file_safe(tmp_path, alias.name)
    assert alias.read_bytes() == b"stand-in"
