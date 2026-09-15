from pathlib import Path

from deerflow.agents.middlewares.uploads_middleware import extract_outline_for_file
from deerflow.utils.file_conversion import (
    OUTLINE_PREVIEW_MAX_CHARS,
    OUTLINE_TITLE_MAX_CHARS,
    extract_outline,
)


def test_extract_outline_ignores_fenced_code(tmp_path: Path) -> None:
    document = tmp_path / "document.md"
    document.write_text(
        "# Visible\n"
        "```python\n"
        "# Hidden hash\n"
        "**ITEM 1. HIDDEN BOLD**\n"
        "```\n"
        "## Visible Again\n"
        "~~~text\n"
        "**1** **Hidden Split**\n"
        "~~~~\n",
        encoding="utf-8",
    )

    assert extract_outline(document) == [
        {"title": "Visible", "line": 1},
        {"title": "Visible Again", "line": 6},
    ]


def test_extract_outline_honours_fence_closing_rules(tmp_path: Path) -> None:
    document = tmp_path / "document.md"
    document.write_text(
        "   ````lang\n"
        "```\n"
        "# Still Hidden\n"
        "```` trailing\n"
        "## Also Hidden\n"
        "   ````\n"
        "# Visible\n"
        "```\n"
        "# Hidden Until EOF\n",
        encoding="utf-8",
    )

    assert extract_outline(document) == [{"title": "Visible", "line": 7}]


def test_extract_outline_only_accepts_valid_atx_headings(tmp_path: Path) -> None:
    document = tmp_path / "headings.md"
    document.write_text(
        "# Valid one\n"
        "   ###### Valid six ####\n"
        "#NoSpace\n"
        "####### Too many\n"
        "    # Indented code\n",
        encoding="utf-8",
    )

    assert extract_outline(document) == [
        {"title": "Valid one", "line": 1},
        {"title": "Valid six", "line": 2},
    ]


def test_outline_titles_and_fallback_preview_have_character_budgets(tmp_path: Path) -> None:
    heading = tmp_path / "heading.md"
    heading.write_text("# " + "x" * 500, encoding="utf-8")
    outline = extract_outline(heading)
    assert len(outline[0]["title"]) == OUTLINE_TITLE_MAX_CHARS
    assert outline[0]["title"].endswith("… (truncated)")

    preview = tmp_path / "preview.md"
    preview.write_text("y" * 5000, encoding="utf-8")
    outline_result, preview_result = extract_outline_for_file(preview)
    assert outline_result == []
    assert sum(map(len, preview_result)) <= OUTLINE_PREVIEW_MAX_CHARS
    assert preview_result[-1].endswith("… (truncated)")
