import fnmatch
import os
import re
from dataclasses import dataclass, field
from pathlib import Path, PurePosixPath

IGNORE_PATTERNS = [
    ".git",
    ".svn",
    ".hg",
    ".bzr",
    "node_modules",
    "__pycache__",
    ".venv",
    "venv",
    ".env",
    "env",
    ".tox",
    ".nox",
    ".eggs",
    "*.egg-info",
    "site-packages",
    "dist",
    "build",
    ".next",
    ".nuxt",
    ".output",
    ".turbo",
    "target",
    "out",
    ".idea",
    ".vscode",
    "*.swp",
    "*.swo",
    "*~",
    ".project",
    ".classpath",
    ".settings",
    ".DS_Store",
    "Thumbs.db",
    "desktop.ini",
    "*.lnk",
    "*.log",
    "*.tmp",
    "*.temp",
    "*.bak",
    "*.cache",
    ".cache",
    "logs",
    ".coverage",
    "coverage",
    ".nyc_output",
    "htmlcov",
    ".pytest_cache",
    ".mypy_cache",
    ".ruff_cache",
]

DEFAULT_MAX_FILE_SIZE_BYTES = 1_000_000
DEFAULT_LINE_SUMMARY_LENGTH = 200


@dataclass(frozen=True)
class GrepMatch:
    path: str
    line_number: int
    line: str


@dataclass
class SearchCoverage:
    """Coverage of eligible paths, after the documented ignore patterns."""

    searched_files: int = 0
    skipped_large_files: int = 0
    skipped_binary_files: int = 0
    skipped_long_lines: int = 0
    skipped_symlinks: int = 0
    read_errors: int = 0
    complete: bool = True


@dataclass
class GlobResult:
    matches: list[str]
    truncated: bool
    coverage: SearchCoverage = field(default_factory=SearchCoverage)


@dataclass
class GrepResult:
    matches: list[GrepMatch]
    truncated: bool
    coverage: SearchCoverage = field(default_factory=SearchCoverage)


def should_ignore_name(name: str) -> bool:
    for pattern in IGNORE_PATTERNS:
        if fnmatch.fnmatchcase(name, pattern):
            return True
    return False


def should_ignore_path(path: str) -> bool:
    return any(should_ignore_name(segment) for segment in path.replace("\\", "/").split("/") if segment)


def path_matches(pattern: str, rel_path: str) -> bool:
    path = PurePosixPath(rel_path)
    if path.match(pattern):
        return True
    if pattern.startswith("**/"):
        return path.match(pattern[3:])
    return False


def truncate_line(line: str, max_chars: int = DEFAULT_LINE_SUMMARY_LENGTH) -> str:
    line = line.rstrip("\n\r")
    if len(line) <= max_chars:
        return line
    return line[: max_chars - 3] + "..."


def is_binary_file(path: Path, sample_size: int = 8192) -> bool:
    with path.open("rb") as handle:
        return b"\0" in handle.read(sample_size)


def find_glob_matches(root: Path, pattern: str, *, include_dirs: bool = False, max_results: int = 200) -> tuple[list[str], bool]:
    result = find_glob_result(root, pattern, include_dirs=include_dirs, max_results=max_results)
    return result.matches, result.truncated


def _walk_error(error: OSError) -> None:
    raise error


def find_glob_result(root: Path, pattern: str, *, include_dirs: bool = False, max_results: int = 200) -> GlobResult:
    if max_results < 1:
        raise ValueError("max_results must be >= 1")
    matches: list[str] = []
    coverage = SearchCoverage()
    root = root.resolve(strict=True)
    if not root.is_dir():
        raise NotADirectoryError(root)

    for current_root, dirs, files in os.walk(root, onerror=_walk_error):
        kept_dirs = []
        for name in sorted(dirs):
            if should_ignore_name(name):
                continue
            if (Path(current_root) / name).is_symlink():
                coverage.skipped_symlinks += 1
                coverage.complete = False
                continue
            kept_dirs.append(name)
        dirs[:] = kept_dirs
        rel_dir = Path(current_root).relative_to(root)
        if include_dirs:
            for name in dirs:
                rel_path = (rel_dir / name).as_posix()
                if path_matches(pattern, rel_path):
                    if len(matches) >= max_results:
                        coverage.complete = False
                        return GlobResult(matches, True, coverage)
                    matches.append(str(Path(current_root) / name))
        for name in sorted(files):
            if should_ignore_name(name):
                continue
            if (Path(current_root) / name).is_symlink():
                coverage.skipped_symlinks += 1
                coverage.complete = False
                continue
            coverage.searched_files += 1
            rel_path = (rel_dir / name).as_posix()
            if path_matches(pattern, rel_path):
                if len(matches) >= max_results:
                    coverage.complete = False
                    return GlobResult(matches, True, coverage)
                matches.append(str(Path(current_root) / name))
    return GlobResult(matches, False, coverage)


def find_grep_matches(
    root: Path,
    pattern: str,
    *,
    glob_pattern: str | None = None,
    literal: bool = False,
    case_sensitive: bool = False,
    max_results: int = 100,
    max_file_size: int = DEFAULT_MAX_FILE_SIZE_BYTES,
    line_summary_length: int = DEFAULT_LINE_SUMMARY_LENGTH,
) -> tuple[list[GrepMatch], bool]:
    result = find_grep_result(root, pattern, glob_pattern=glob_pattern, literal=literal,
                              case_sensitive=case_sensitive, max_results=max_results,
                              max_file_size=max_file_size, line_summary_length=line_summary_length)
    return result.matches, result.truncated


def find_grep_result(
    root: Path,
    pattern: str,
    *,
    glob_pattern: str | None = None,
    literal: bool = False,
    case_sensitive: bool = False,
    max_results: int = 100,
    max_file_size: int = DEFAULT_MAX_FILE_SIZE_BYTES,
    line_summary_length: int = DEFAULT_LINE_SUMMARY_LENGTH,
) -> GrepResult:
    if max_results < 1:
        raise ValueError("max_results must be >= 1")
    matches: list[GrepMatch] = []
    coverage = SearchCoverage()
    root = root.resolve(strict=True)
    root_is_file = root.is_file()
    if not root_is_file and not root.is_dir():
        raise NotADirectoryError(root)

    regex_source = re.escape(pattern) if literal else pattern
    flags = 0 if case_sensitive else re.IGNORECASE
    regex = re.compile(regex_source, flags)

    # Skip lines longer than this to prevent ReDoS on minified / no-newline files.
    _max_line_chars = line_summary_length * 10

    def candidate_files():
        if root_is_file:
            yield root, root.name
            return
        for current_root, dirs, files in os.walk(root, onerror=_walk_error):
            kept_dirs = []
            for name in sorted(dirs):
                if should_ignore_name(name):
                    continue
                if (Path(current_root) / name).is_symlink():
                    coverage.skipped_symlinks += 1
                    coverage.complete = False
                    continue
                kept_dirs.append(name)
            dirs[:] = kept_dirs
            rel_dir = Path(current_root).relative_to(root)
            for name in sorted(files):
                if not should_ignore_name(name):
                    yield Path(current_root) / name, (rel_dir / name).as_posix()

    for candidate_path, rel_path in candidate_files():
        if glob_pattern is not None and not path_matches(glob_pattern, rel_path):
            continue
        if not root_is_file and candidate_path.is_symlink():
            coverage.skipped_symlinks += 1
            coverage.complete = False
            continue
        file_path = candidate_path.resolve(strict=True)
        if not root_is_file and not file_path.is_relative_to(root):
            coverage.skipped_symlinks += 1
            coverage.complete = False
            continue
        if file_path.stat().st_size > max_file_size:
            coverage.skipped_large_files += 1
            coverage.complete = False
            continue
        if is_binary_file(file_path):
            coverage.skipped_binary_files += 1
            coverage.complete = False
            continue
        with file_path.open(encoding="utf-8", errors="replace") as handle:
            coverage.searched_files += 1
            line_number = 0
            while True:
                line = handle.readline(_max_line_chars + 1)
                if not line:
                    break
                line_number += 1
                if len(line) > _max_line_chars:
                    coverage.skipped_long_lines += 1
                    coverage.complete = False
                    while line and not line.endswith("\n"):
                        line = handle.readline(65536)
                    continue
                if regex.search(line):
                    if len(matches) >= max_results:
                        coverage.complete = False
                        return GrepResult(matches, True, coverage)
                    matches.append(GrepMatch(str(file_path), line_number, truncate_line(line, line_summary_length)))
    return GrepResult(matches, False, coverage)
