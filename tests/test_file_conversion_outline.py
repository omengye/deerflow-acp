from pathlib import Path

from deerflow.utils.file_conversion import extract_outline


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
