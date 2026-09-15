"""Fail-closed deterministic checks for decidable delegation criteria."""

from __future__ import annotations

import os
import re
import stat
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any, TypedDict

from langchain_core.messages import AIMessage, ToolMessage

from deerflow.subagents.report_contract import normalize_acceptance_criteria

_FILE_LEAF_RE = re.compile(
    r"^file:(?P<path>.+?)\s+(?P<mode>exists|non-empty)$",
    re.IGNORECASE,
)
_FILE_WRITTEN_RE = re.compile(r"^file_written:(?P<path>.+)$", re.IGNORECASE)
_TESTS_PASSED_RE = re.compile(r"^tests_passed:(?P<command>.+)$", re.IGNORECASE)
_TEST_PASS_RE = re.compile(
    r"\b[1-9]\d*\s+passed\b|^OK$|test result: ok|^ok\s+\S|"
    r"\bBUILD SUCCESS(?:FUL)?\b|\ball tests passed\b",
    re.IGNORECASE | re.MULTILINE,
)
_TEST_ZERO_RE = re.compile(
    r"\b0\s+passed\b|\[no test files\]|\[no tests to run\]|\bRan 0 tests\b",
    re.IGNORECASE,
)
_TEST_FAIL_RE = re.compile(
    r"\b[1-9]\d*\s+failed\b|\b[1-9]\d*\s+errors?\b|^FAILED\b|"
    r"^ERROR\s+\S|test result: FAILED|^FAIL\s+\S|\bBUILD FAILURE\b",
    re.IGNORECASE | re.MULTILINE,
)
_WINDOWS_DRIVE_RE = re.compile(r"^[A-Za-z]:")
_WINDOWS_DRIVE_ABSOLUTE_RE = re.compile(r"^[A-Za-z]:[/\\]")
_POWERSHELL_DRIVE_RE = re.compile(r"^[^/\\:]+:")
_WINDOWS_SHORT_NAME_RE = re.compile(
    r"^[A-Za-z0-9$%_'@~`!(){}^#&-]{1,6}~[1-9]"
    r"(?:\.[A-Za-z0-9$%_'@~`!(){}^#&-]{1,3})?$"
)
_CMD_ENV_RE = re.compile(r"%[^%\r\n]+%|![^!\r\n]+!")
_BRACE_EXPANSION_RE = re.compile(r"\{[^{}\r\n]*(?:,|\.\.)[^{}\r\n]*\}")
_TYPOGRAPHIC_QUOTES = frozenset("‘’‚‛“”„")


def _has_ambiguous_whitespace(value: str) -> bool:
    return "\r" in value or any(
        character.isspace() and character not in " \t\n\f\v"
        for character in value
    )


def _has_unsafe_windows_path_spelling(value: str) -> bool:
    """Reject Windows aliases that need shell/filesystem provenance."""
    if _has_ambiguous_whitespace(value) or any(
        quote in value for quote in _TYPOGRAPHIC_QUOTES
    ):
        return True
    if _CMD_ENV_RE.search(value) or "$" in value or _BRACE_EXPANSION_RE.search(value):
        return True

    slash_path = value.replace("\\", "/")
    if slash_path.startswith("//"):
        # UNC and Win32 device namespaces can alias scoped local paths.
        return True
    if value.startswith("\\"):
        # Root-relative paths depend on the current drive.
        return True
    if _WINDOWS_DRIVE_RE.match(value) and not _WINDOWS_DRIVE_ABSOLUTE_RE.match(value):
        return True
    if _POWERSHELL_DRIVE_RE.match(value) and not _WINDOWS_DRIVE_RE.match(value):
        # Includes provider-qualified and named-PSDrive spellings.
        return True

    path_without_drive = slash_path[2:] if _WINDOWS_DRIVE_RE.match(slash_path) else slash_path
    # Alternate data streams can hide behind an otherwise scoped file.
    path_without_nodeid = path_without_drive.split("::", 1)[0]
    if ":" in path_without_nodeid:
        return True
    return any(
        _WINDOWS_SHORT_NAME_RE.fullmatch(component)
        for component in path_without_nodeid.split("/")
        if component
    )


def _command_is_unambiguous(command: str) -> bool:
    """Accept only command spellings stable across supported Windows shells."""
    if _has_ambiguous_whitespace(command):
        return False
    if any(quote in command for quote in _TYPOGRAPHIC_QUOTES):
        return False
    if _CMD_ENV_RE.search(command) or "$" in command or _BRACE_EXPANSION_RE.search(command):
        return False
    if re.search(r"(?:^|\s)@\S", command):
        return False

    # The evidence does not retain shell provenance. Backslash-prefixed tokens
    # may be root-relative/UNC/device paths or shell escapes, so require the
    # portable forward-slash spelling for deterministic Windows paths.
    if re.search(r"(?:^|\s|[\"'])\\", command):
        return False

    for token in re.split(r"[ \t\n\f\v]+", command):
        token = token.strip("\"'")
        if not token:
            continue
        if _has_unsafe_windows_path_spelling(token):
            return False
    return True


def _path_is_within(path: Path, root: Path) -> bool:
    if os.name == "nt":
        candidate = os.path.normcase(str(path))
        boundary = os.path.normcase(str(root))
        return candidate == boundary or candidate.startswith(boundary + os.sep)
    try:
        path.relative_to(root)
        return True
    except ValueError:
        return False


class AcceptanceLeaf(TypedDict):
    criterion: str
    family: str
    checked: bool
    holds: bool
    detail: str


class AcceptanceVerdict(TypedDict):
    source: str
    requirement: str
    leaves: list[AcceptanceLeaf]
    unchecked: list[str]
    all_hold: bool


def _leaf(
    criterion: str,
    family: str,
    *,
    checked: bool,
    holds: bool,
    detail: str,
) -> AcceptanceLeaf:
    return AcceptanceLeaf(
        criterion=criterion,
        family=family,
        checked=checked,
        holds=holds if checked else False,
        detail=" ".join(detail.split())[:160],
    )


def _scoped_file(
    raw_path: str,
    thread_data: Mapping[str, Any] | None,
) -> Path | None:
    if not thread_data:
        return None
    workspace_raw = thread_data.get("workspace_path")
    outputs_raw = thread_data.get("outputs_path")
    workspace = (
        Path(workspace_raw).resolve() if isinstance(workspace_raw, str) else None
    )
    outputs = Path(outputs_raw).resolve() if isinstance(outputs_raw, str) else None
    roots = [root for root in (workspace, outputs) if root is not None]
    if not roots:
        return None

    raw_value = raw_path.strip(" \t\n\f\v")
    if _has_unsafe_windows_path_spelling(raw_value):
        return None
    value = raw_value.replace("\\", "/")
    if value.startswith("/mnt/user-data/workspace/") and workspace is not None:
        candidate = workspace / value.removeprefix("/mnt/user-data/workspace/")
    elif value.startswith("/mnt/user-data/outputs/") and outputs is not None:
        candidate = outputs / value.removeprefix("/mnt/user-data/outputs/")
    elif value.startswith("/") or re.match(r"^[A-Za-z]:/", value):
        candidate = Path(value)
    elif workspace is not None:
        candidate = workspace / value
    else:
        return None

    try:
        lexical = Path(os.path.abspath(candidate))
        resolved = lexical.resolve(strict=False)
    except (OSError, ValueError):
        return None
    for root in roots:
        if _path_is_within(lexical, root) and _path_is_within(resolved, root):
            return lexical
    return None


def _check_file(
    criterion: str,
    family: str,
    raw_path: str,
    thread_data: Mapping[str, Any] | None,
    *,
    require_non_empty: bool,
    messages: Sequence[Any] = (),
) -> AcceptanceLeaf:
    path = _scoped_file(raw_path, thread_data)
    if path is None:
        return _leaf(
            criterion,
            family,
            checked=False,
            holds=False,
            detail="path is outside the shared workspace/outputs boundary",
        )
    try:
        metadata = os.lstat(path)
    except FileNotFoundError:
        return _leaf(
            criterion,
            family,
            checked=True,
            holds=False,
            detail="file does not exist",
        )
    except OSError:
        return _leaf(
            criterion,
            family,
            checked=False,
            holds=False,
            detail="file metadata could not be read",
        )
    if not stat.S_ISREG(metadata.st_mode) or stat.S_ISLNK(metadata.st_mode):
        return _leaf(
            criterion,
            family,
            checked=True,
            holds=False,
            detail="path is not a regular file",
        )
    if require_non_empty and metadata.st_size <= 0:
        return _leaf(
            criterion,
            family,
            checked=True,
            holds=False,
            detail="file exists but is empty",
        )
    if family == "file_written":
        recorded_paths: list[Path] = []
        calls: dict[str, str] = {}
        for message in messages:
            if isinstance(message, AIMessage):
                for call in message.tool_calls:
                    args = call.get("args")
                    if call.get("name") not in {
                        "write_file",
                        "str_replace",
                    } or not isinstance(args, dict):
                        continue
                    call_id = call.get("id")
                    call_path = args.get("path")
                    if isinstance(call_id, str) and isinstance(call_path, str):
                        calls[call_id] = call_path
            elif isinstance(message, ToolMessage):
                call_path = calls.get(str(message.tool_call_id))
                content = (
                    message.content
                    if isinstance(message.content, str)
                    else str(message.content)
                )
                if (
                    call_path is not None
                    and getattr(message, "status", None) != "error"
                    and not content.startswith("Error:")
                ):
                    resolved_call_path = _scoped_file(call_path, thread_data)
                    if resolved_call_path is not None:
                        recorded_paths.append(resolved_call_path)
        if path not in recorded_paths:
            return _leaf(
                criterion,
                family,
                checked=False,
                holds=False,
                detail="no successful file mutation receipt matches this path",
            )
    return _leaf(
        criterion,
        family,
        checked=True,
        holds=True,
        detail=f"regular file, {metadata.st_size} bytes",
    )


def _bash_executions(messages: Sequence[Any]) -> list[tuple[str, str, bool]]:
    calls: dict[str, str] = {}
    executions: list[tuple[str, str, bool]] = []
    for message in messages:
        if isinstance(message, AIMessage):
            for call in message.tool_calls:
                args = call.get("args")
                if call.get("name") != "bash" or not isinstance(args, dict):
                    continue
                command = args.get("command")
                call_id = call.get("id")
                if isinstance(call_id, str) and isinstance(command, str):
                    calls[call_id] = command.strip()
        elif isinstance(message, ToolMessage):
            command = calls.get(str(message.tool_call_id))
            if command is None:
                continue
            content = (
                message.content
                if isinstance(message.content, str)
                else str(message.content)
            )
            succeeded = getattr(
                message, "status", None
            ) != "error" and not content.startswith("Error:")
            executions.append((command, content, succeeded))
    return executions


def _check_tests(
    criterion: str,
    command: str,
    messages: Sequence[Any],
) -> AcceptanceLeaf:
    expected = command.strip()
    if not _command_is_unambiguous(expected):
        return _leaf(
            criterion,
            "tests_passed",
            checked=False,
            holds=False,
            detail="command uses shell- or filesystem-ambiguous syntax",
        )
    matching = [item for item in _bash_executions(messages) if item[0] == expected]
    matching = [item for item in matching if _command_is_unambiguous(item[0])]
    if not matching:
        return _leaf(
            criterion,
            "tests_passed",
            checked=False,
            holds=False,
            detail="no exact recorded bash execution matches this command",
        )
    _, output, succeeded = matching[-1]
    has_failure = bool(_TEST_FAIL_RE.search(output) or _TEST_ZERO_RE.search(output))
    has_pass = bool(_TEST_PASS_RE.search(output))
    return _leaf(
        criterion,
        "tests_passed",
        checked=True,
        holds=succeeded and has_pass and not has_failure,
        detail=(
            "recorded command returned a passing test summary"
            if succeeded and has_pass and not has_failure
            else "recorded command lacks a clean, non-zero passing test summary"
        ),
    )


def check_acceptance_criteria(
    acceptance_criteria: list[str] | None,
    *,
    thread_data: Mapping[str, Any] | None,
    messages: Sequence[Any],
) -> AcceptanceVerdict:
    """Evaluate only safe, decidable leaves and mark all others unchecked."""
    criteria = normalize_acceptance_criteria(acceptance_criteria)
    leaves: list[AcceptanceLeaf] = []
    for criterion in criteria:
        file_match = _FILE_LEAF_RE.fullmatch(criterion)
        written_match = _FILE_WRITTEN_RE.fullmatch(criterion)
        tests_match = _TESTS_PASSED_RE.fullmatch(criterion)
        if file_match:
            mode = file_match.group("mode").casefold()
            leaves.append(
                _check_file(
                    criterion,
                    "file_non_empty" if mode == "non-empty" else "file_exists",
                    file_match.group("path"),
                    thread_data,
                    require_non_empty=mode == "non-empty",
                )
            )
        elif written_match:
            leaves.append(
                _check_file(
                    criterion,
                    "file_written",
                    written_match.group("path"),
                    thread_data,
                    require_non_empty=False,
                    messages=messages,
                )
            )
        elif tests_match:
            leaves.append(
                _check_tests(criterion, tests_match.group("command"), messages)
            )
        else:
            leaves.append(
                _leaf(
                    criterion,
                    "undecidable",
                    checked=False,
                    holds=False,
                    detail="criterion has no deterministic checker",
                )
            )
    unchecked = [leaf["criterion"] for leaf in leaves if not leaf["checked"]]
    return AcceptanceVerdict(
        source="acceptance_checklist",
        requirement="delegation_acceptance_criteria",
        leaves=leaves,
        unchecked=unchecked,
        all_hold=bool(leaves)
        and all(leaf["checked"] and leaf["holds"] for leaf in leaves),
    )


def render_acceptance_section(verdict: AcceptanceVerdict) -> str:
    lines = [
        "Acceptance checklist (execution evidence only; not final task acceptance):"
    ]
    for leaf in verdict["leaves"]:
        state = "HOLDS" if leaf["checked"] and leaf["holds"] else "DOES NOT HOLD"
        if not leaf["checked"]:
            state = "UNVERIFIED"
        lines.append(f"- {state}: {leaf['criterion']} ({leaf['detail']})")
    return "\n".join(lines)
