from __future__ import annotations

import hashlib
from types import SimpleNamespace

import pytest

from app.routers import uploads


class _ArtifactClient:
    def __init__(
        self,
        content: bytes,
        mime_type: str = "text/plain",
        expected_path: str = "mnt/user-data/outputs/report.txt",
    ) -> None:
        self.content = content
        self.mime_type = mime_type
        self.expected_path = expected_path

    def get_artifact(self, thread_id: str, path: str) -> tuple[bytes, str]:
        assert thread_id == "thread-1"
        assert path == self.expected_path
        return self.content, self.mime_type


async def test_artifact_response_includes_sha256_etag(monkeypatch) -> None:
    content = b"artifact contents"
    client = _ArtifactClient(content)
    monkeypatch.setattr(
        uploads,
        "get_client_manager",
        lambda: SimpleNamespace(get_client=lambda: client),
    )

    response = await uploads.get_artifact(
        "thread-1",
        "mnt/user-data/outputs/report.txt",
    )

    expected = hashlib.sha256(content).hexdigest()
    assert response.headers["etag"] == f'"{expected}"'
    assert response.body == content
    assert "content-disposition" not in response.headers


async def test_download_keeps_content_disposition_and_sha256_etag(monkeypatch) -> None:
    content = b"download me"
    client = _ArtifactClient(content)
    monkeypatch.setattr(
        uploads,
        "get_client_manager",
        lambda: SimpleNamespace(get_client=lambda: client),
    )

    response = await uploads.get_artifact(
        "thread-1",
        "mnt/user-data/outputs/report.txt",
        download=True,
    )

    expected = hashlib.sha256(content).hexdigest()
    assert response.headers["etag"] == f'"{expected}"'
    assert response.headers["content-disposition"].startswith("attachment;")


@pytest.mark.parametrize(
    "mime_type",
    [
        "text/html",
        "text/xml",
        "application/xml",
        "text/xsl",
        "application/xhtml+xml",
        "image/svg+xml",
        "application/rss+xml",
        "APPLICATION/ATOM+XML; charset=utf-8",
    ],
)
async def test_active_artifacts_are_always_attachments(monkeypatch, mime_type: str) -> None:
    path = "mnt/user-data/outputs/active.xml"
    client = _ArtifactClient(b"<root/>", mime_type, expected_path=path)
    monkeypatch.setattr(
        uploads,
        "get_client_manager",
        lambda: SimpleNamespace(get_client=lambda: client),
    )

    response = await uploads.get_artifact("thread-1", path)

    assert response.headers["content-disposition"].startswith("attachment;")


@pytest.mark.parametrize(
    "mime_type",
    [None, "text/plain", "application/json", "image/png", "application/xml-dtd"],
)
def test_passive_mime_types_remain_inline(mime_type: str | None) -> None:
    assert not uploads._is_active_content_mime_type(mime_type)
