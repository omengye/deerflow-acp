"""Shared classification rules for files inside a skill package."""

from __future__ import annotations

from pathlib import PurePath, PurePosixPath


CODE_SUFFIXES = frozenset(
    {
        ".bash",
        ".cjs",
        ".js",
        ".mjs",
        ".php",
        ".pl",
        ".ps1",
        ".py",
        ".rb",
        ".sh",
        ".ts",
        ".zsh",
    }
)

_EXECUTABLE_MAGIC_PREFIXES = (
    b"\x7fELF",
    b"MZ",
    b"\xfe\xed\xfa\xce",
    b"\xfe\xed\xfa\xcf",
    b"\xce\xfa\xed\xfe",
    b"\xcf\xfa\xed\xfe",
    b"\xca\xfe\xba\xbe",
    b"\xbe\xba\xfe\xca",
    b"\xca\xfe\xba\xbf",
    b"\xbf\xba\xfe\xca",
)


def _posix(path: str | PurePath) -> PurePosixPath:
    return PurePosixPath(str(path).replace("\\", "/"))


def is_code_path(path: str | PurePath) -> bool:
    """Classify code by directory or extension, regardless of package depth."""
    posix = _posix(path)
    return (
        bool(posix.parts) and posix.parts[0] == "scripts"
    ) or posix.suffix.lower() in CODE_SUFFIXES


def is_code_file(path: str | PurePath, head: bytes) -> bool:
    """Also treat extensionless files with a shebang as executable code."""
    posix = _posix(path)
    return is_code_path(posix) or (not posix.suffix and head.startswith(b"#!"))


def is_executable_binary_prefix(prefix: bytes) -> bool:
    """Detect ELF, PE/DOS, and Mach-O executable formats."""
    return prefix.startswith(_EXECUTABLE_MAGIC_PREFIXES)
