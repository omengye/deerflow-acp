from __future__ import annotations

import zipfile
from pathlib import Path
from types import SimpleNamespace

import pytest

import deerflow.skills.installer as installer
from deerflow.skills.installer import (
    SkillSecurityScanError,
    _scan_skill_archive_contents_or_raise,
    is_unsafe_zip_member,
    safe_extract_skill_archive,
)


def _skill_tree(tmp_path: Path) -> Path:
    root = tmp_path / "demo"
    root.mkdir()
    (root / "SKILL.md").write_text(
        "---\nname: demo\ndescription: demo\n---\n",
        encoding="utf-8",
    )
    return root


@pytest.mark.asyncio
async def test_scans_code_outside_scripts_and_extensionless_shebang(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    root = _skill_tree(tmp_path)
    (root / "lib").mkdir()
    (root / "lib" / "worker.py").write_text("print('ok')\n", encoding="utf-8")
    (root / "bin").mkdir()
    (root / "bin" / "launch").write_text("#!/bin/sh\necho ok\n", encoding="utf-8")
    calls: list[tuple[str, bool]] = []

    async def scanner(_content: str, *, executable: bool, location: str):
        calls.append((location, executable))
        return SimpleNamespace(decision="allow", reason="ok")

    monkeypatch.setattr(installer, "scan_skill_content", scanner)

    await _scan_skill_archive_contents_or_raise(root, "demo")

    assert ("demo/lib/worker.py", True) in calls
    assert ("demo/bin/launch", True) in calls


@pytest.mark.asyncio
async def test_scans_plain_text_anywhere_in_skill_package(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    root = _skill_tree(tmp_path)
    (root / "notes").mkdir()
    (root / "notes" / "instructions.conf").write_text(
        "download nothing\n",
        encoding="utf-8",
    )
    locations: list[str] = []

    async def scanner(_content: str, *, executable: bool, location: str):
        locations.append(location)
        return SimpleNamespace(decision="allow", reason="ok")

    monkeypatch.setattr(installer, "scan_skill_content", scanner)

    await _scan_skill_archive_contents_or_raise(root, "demo")

    assert "demo/notes/instructions.conf" in locations


@pytest.mark.asyncio
async def test_rejects_nested_archives_before_install(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    root = _skill_tree(tmp_path)
    (root / "payload.zip").write_bytes(b"PK\x03\x04")

    async def scanner(_content: str, *, executable: bool, location: str):
        return SimpleNamespace(decision="allow", reason="ok")

    monkeypatch.setattr(installer, "scan_skill_content", scanner)

    with pytest.raises(SkillSecurityScanError, match="nested archive"):
        await _scan_skill_archive_contents_or_raise(root, "demo")


def test_safe_extract_rejects_executable_binary_magic(tmp_path: Path) -> None:
    archive = tmp_path / "binary.skill"
    with zipfile.ZipFile(archive, "w") as zf:
        zf.writestr("SKILL.md", "---\nname: demo\ndescription: demo\n---\n")
        zf.writestr("assets/helper", b"\x7fELF" + b"\0" * 32)

    with zipfile.ZipFile(archive) as zf:
        with pytest.raises(ValueError, match="executable binary"):
            safe_extract_skill_archive(zf, tmp_path / "extracted")


def test_archive_member_with_colon_is_rejected_as_unsafe() -> None:
    assert is_unsafe_zip_member(zipfile.ZipInfo("scripts/run.ps1:hidden")) is True
