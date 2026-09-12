"""Turn signed Buzz imeta attachments into local ACP prompt content.

Downloads use Buzz's authenticated, relay-origin-only media command. Original
tags remain in the inbox; verified files are cached per ACP session and event.
"""

from __future__ import annotations

import base64
import hashlib
import os
import re
import tempfile
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import urlsplit

from .buzz_cli import BuzzCLI, BuzzCLIError, BuzzTransportError

MAX_ATTACHMENTS = 8
MAX_IMAGE_BYTES = 20 * 1024 * 1024
MAX_FILE_BYTES = 25 * 1024 * 1024
MAX_TOTAL_BYTES = 40 * 1024 * 1024
IMAGE_MIMES = {"image/jpeg", "image/png", "image/webp", "image/gif"}


class AttachmentError(ValueError):
    """An attachment cannot be safely or faithfully supplied to DeerFlow."""


@dataclass(frozen=True, slots=True)
class Attachment:
    url: str
    name: str
    mime_type: str
    sha256: str
    size: int

    @property
    def is_image(self) -> bool:
        return self.mime_type in IMAGE_MIMES


def parse_attachments(tags: tuple[tuple[str, ...], ...]) -> list[Attachment]:
    result: list[Attachment] = []
    seen: set[str] = set()
    for tag in tags:
        if not tag or tag[0] != "imeta":
            continue
        fields: dict[str, str] = {}
        for field in tag[1:]:
            key, separator, value = field.partition(" ")
            if separator:
                if key in fields and fields[key] != value:
                    raise AttachmentError(f"Conflicting attachment metadata: {key}")
                fields[key] = value
        url = fields.get("url", "")
        mime = (
            fields.get("m", "application/octet-stream").split(";", 1)[0].strip().lower()
        )
        digest = fields.get("x", "").lower()
        raw_size = fields.get("size", "")
        if not re.fullmatch(r"[0-9a-f]{64}", digest) or not re.fullmatch(
            r"[0-9]{1,12}", raw_size
        ):
            raise AttachmentError(
                "Buzz attachments require SHA-256 (x) and byte size (size)"
            )
        parsed = urlsplit(url)
        if (
            parsed.scheme not in {"http", "https"}
            or not parsed.hostname
            or parsed.username
            or parsed.password
        ):
            raise AttachmentError(
                "Buzz attachment URL must be an HTTP(S) relay media URL"
            )
        if not re.fullmatch(rf"/media/{digest}(?:\.[a-z0-9]{{1,8}})?", parsed.path):
            raise AttachmentError(
                "Buzz attachment URL must identify its declared SHA-256"
            )
        if mime.startswith(("audio/", "video/")):
            raise AttachmentError("Audio and video attachments are not supported")
        if mime.startswith("image/") and mime not in IMAGE_MIMES:
            raise AttachmentError("Supported image formats are JPG, PNG, WebP, and GIF")
        size = int(raw_size)
        limit = MAX_IMAGE_BYTES if mime in IMAGE_MIMES else MAX_FILE_BYTES
        if size < 1 or size > limit:
            raise AttachmentError(
                f"Attachment size must be between 1 and {limit} bytes"
            )
        name = fields.get("filename") or parsed.path.rsplit("/", 1)[-1]
        # Prefixing the hash below also avoids Windows reserved device names.
        name = (
            re.sub(r'[<>:"/\\|?*\x00-\x1f\x7f]', "_", name).strip(" .")[:120]
            or "attachment"
        )
        if url in seen:
            if any(
                item.url == url
                and (item.mime_type, item.sha256, item.size) != (mime, digest, size)
                for item in result
            ):
                raise AttachmentError("Conflicting duplicate attachment")
            continue
        seen.add(url)
        result.append(Attachment(url, name, mime, digest, size))
    if (
        len(result) > MAX_ATTACHMENTS
        or sum(item.size for item in result) > MAX_TOTAL_BYTES
    ):
        raise AttachmentError(
            "A message may contain at most 8 attachments totaling 40 MiB"
        )
    return result


def _image_mime(data: bytes) -> str | None:
    if data.startswith(b"\xff\xd8\xff"):
        return "image/jpeg"
    if data.startswith(b"\x89PNG\r\n\x1a\n"):
        return "image/png"
    if data.startswith((b"GIF87a", b"GIF89a")):
        return "image/gif"
    if data.startswith(b"RIFF") and data[8:12] == b"WEBP":
        return "image/webp"
    return None


def _verified_bytes(path: Path, attachment: Attachment) -> bytes:
    if path.stat().st_size != attachment.size:
        raise AttachmentError("Downloaded attachment size does not match Buzz metadata")
    data = path.read_bytes()
    if hashlib.sha256(data).hexdigest() != attachment.sha256:
        raise AttachmentError(
            "Downloaded attachment SHA-256 does not match Buzz metadata"
        )
    detected_image = _image_mime(data)
    if (
        attachment.is_image or detected_image
    ) and detected_image != attachment.mime_type:
        raise AttachmentError("Downloaded image format does not match its MIME type")
    return data


async def prepare_attachments(
    attachments: list[Attachment],
    *,
    buzz: BuzzCLI,
    workspace: Path,
    session_id: str,
    event_id: str,
) -> list[dict]:
    """Persist verified files and return image/resource_link blocks in tag order."""
    workspace = workspace.resolve(strict=True)
    scope = hashlib.sha256(session_id.encode()).hexdigest()
    event = hashlib.sha256(event_id.encode()).hexdigest()
    directory = workspace / ".buzz-attachments" / scope / event
    if not directory.resolve().is_relative_to(workspace):
        raise AttachmentError(
            "Attachment cache must remain inside the session workspace"
        )
    directory.mkdir(parents=True, exist_ok=True)
    blocks: list[dict] = []
    for attachment in attachments:
        path = directory / f"{attachment.sha256}-{attachment.name}"
        if not path.resolve().is_relative_to(directory.resolve()):
            raise AttachmentError("Attachment cache path escapes its event directory")
        data: bytes | None = None
        if path.exists():
            try:
                data = _verified_bytes(path, attachment)
            except AttachmentError:
                pass  # Re-download a modified or incomplete cache entry.
        if data is None:
            with tempfile.TemporaryDirectory(
                prefix=".download-", dir=directory
            ) as staging:
                temporary = Path(staging) / "payload"
                try:
                    await buzz.download_media(
                        attachment.url, temporary, max_bytes=attachment.size
                    )
                except BuzzTransportError:
                    raise
                except BuzzCLIError as exc:
                    raise AttachmentError(
                        f"Buzz attachment download failed: {exc}"
                    ) from exc
                data = _verified_bytes(temporary, attachment)
                os.replace(temporary, path)
        if attachment.is_image:
            blocks.append(
                {
                    "type": "image",
                    "data": base64.b64encode(data).decode("ascii"),
                    "mimeType": attachment.mime_type,
                    "_meta": {"name": attachment.name},
                }
            )
        else:
            blocks.append(
                {
                    "type": "resource_link",
                    "uri": path.as_uri(),
                    "name": attachment.name,
                    "mimeType": attachment.mime_type,
                    "size": attachment.size,
                }
            )
    return blocks
