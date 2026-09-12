from __future__ import annotations

import base64
import hashlib
import json
import sys
from pathlib import Path
from urllib.parse import unquote, urlsplit
from urllib.request import url2pathname

import pytest
from buzz_deerflow_adapter.attachments import (
    AttachmentError,
    parse_attachments,
    prepare_attachments,
)
from buzz_deerflow_adapter.buzz_cli import BuzzCLI, BuzzCLIError


def imeta(
    data: bytes, mime: str = "image/png", name: str = "picture.png"
) -> tuple[str, ...]:
    digest = hashlib.sha256(data).hexdigest()
    return (
        "imeta",
        f"url https://relay.example/media/{digest}",
        f"m {mime}",
        f"x {digest}",
        f"size {len(data)}",
        f"filename {name}",
    )


def test_parse_preserves_names_and_deduplicates() -> None:
    tag = imeta(b"data", "application/pdf", "报告.pdf")
    items = parse_attachments((tag, tag))
    assert len(items) == 1
    assert items[0].name == "报告.pdf"
    assert not items[0].is_image


@pytest.mark.parametrize(
    "field",
    [
        "size 0",
        "size 999999999",
        "x invalid",
        "url file:///secret",
        "m image/svg+xml",
        "m audio/wav",
    ],
)
def test_parse_rejects_unsupported_or_invalid_attachments(field: str) -> None:
    key = field.partition(" ")[0]
    tag = tuple(part for part in imeta(b"data") if not part.startswith(key + " ")) + (
        field,
    )
    with pytest.raises(AttachmentError):
        parse_attachments((tag,))


def test_limits_all_attachments_before_download() -> None:
    with pytest.raises(AttachmentError, match="at most 8"):
        parse_attachments(tuple(imeta(str(i).encode()) for i in range(9)))


async def test_authenticated_binary_download_cache_and_resource_links(
    tmp_path: Path, monkeypatch
) -> None:
    image_data = b"\x89PNG\r\n\x1a\n" + b"\x80\x00\xff" * 70000
    file_data = b"%PDF-1.7\nexample\xff\x00"
    tags = (imeta(image_data), imeta(file_data, "application/pdf", "../报告.pdf"))
    items = parse_attachments(tags)
    blobs = tmp_path / "media.json"
    operations = tmp_path / "operations.jsonl"
    blobs.write_text(
        json.dumps(
            {
                item.url: base64.b64encode(data).decode()
                for item, data in zip(items, (image_data, file_data))
            }
        )
    )
    monkeypatch.setenv("FAKE_BUZZ_MEDIA", str(blobs))
    monkeypatch.setenv("FAKE_BUZZ_OPERATIONS", str(operations))
    fixture = Path(__file__).parent / "fixtures" / "fake_buzz_cli.py"
    buzz = BuzzCLI(sys.executable, [str(fixture)], "wss://relay.example")
    kwargs = {
        "buzz": buzz,
        "workspace": tmp_path,
        "session_id": "session-1",
        "event_id": "event-1",
    }
    blocks = await prepare_attachments(items, **kwargs)
    assert base64.b64decode(blocks[0]["data"]) == image_data
    resource = Path(url2pathname(unquote(urlsplit(blocks[1]["uri"]).path)))
    assert resource.is_relative_to(tmp_path)
    assert resource.read_bytes() == file_data
    assert blocks[1]["type"] == "resource_link"
    assert len(operations.read_text().splitlines()) == 2
    assert await prepare_attachments(items, **kwargs) == blocks
    assert len(operations.read_text().splitlines()) == 2
    resource.write_bytes(b"tampered")
    assert await prepare_attachments(items, **kwargs) == blocks
    assert len(operations.read_text().splitlines()) == 3


@pytest.mark.parametrize("download", [b"wrong size", b"abcdefgh"])
async def test_download_rejects_size_and_hash_mismatch(
    tmp_path: Path, download: bytes
) -> None:
    items = parse_attachments((imeta(b"12345678", "application/pdf", "x.pdf"),))

    class FakeBuzz:
        async def download_media(self, _url, output, *, max_bytes):
            output.write_bytes(download)

    with pytest.raises(AttachmentError, match="does not match"):
        await prepare_attachments(
            items, buzz=FakeBuzz(), workspace=tmp_path, session_id="s", event_id="e"
        )
    assert not list(tmp_path.rglob("*.pdf"))


async def test_binary_download_is_bounded(tmp_path: Path, monkeypatch) -> None:
    fixture = Path(__file__).parent / "fixtures" / "fake_buzz_cli.py"
    blobs = tmp_path / "media.json"
    blobs.write_text(json.dumps({"blob": base64.b64encode(b"x" * 100000).decode()}))
    monkeypatch.setenv("FAKE_BUZZ_MEDIA", str(blobs))
    buzz = BuzzCLI(sys.executable, [str(fixture)], "wss://relay.example")
    with pytest.raises(BuzzCLIError, match="download size limit"):
        await buzz.download_media("blob", tmp_path / "payload", max_bytes=1000)
    assert (tmp_path / "payload").stat().st_size <= 1000
