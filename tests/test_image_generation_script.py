from __future__ import annotations

import base64
import importlib.util
from pathlib import Path

import pytest


SCRIPT = (
    Path(__file__).resolve().parents[1]
    / "skills"
    / "public"
    / "image-generation"
    / "scripts"
    / "generate.py"
)
SPEC = importlib.util.spec_from_file_location("deerflow_image_generation", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
image_generation = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(image_generation)


class _Response:
    def __init__(self, payload=None, *, content: bytes = b"") -> None:
        self._payload = payload or {}
        self.content = content

    def raise_for_status(self) -> None:
        return None

    def json(self):
        return self._payload


def _set_openai_env(monkeypatch) -> None:
    monkeypatch.delenv("GEMINI_API_KEY", raising=False)
    monkeypatch.setenv("IMAGE_GENERATION_PROVIDER", "openai")
    monkeypatch.setenv("IMAGE_GENERATION_API_KEY", "test-key")
    monkeypatch.setenv("IMAGE_GENERATION_BASE_URL", "https://images.example.test/v1/")


def test_openai_generation_posts_json_and_writes_base64(tmp_path, monkeypatch) -> None:
    _set_openai_env(monkeypatch)
    prompt_file = tmp_path / "prompt.json"
    prompt_file.write_text('{"prompt":"draw a deer"}', encoding="utf-8")
    output = tmp_path / "nested" / "image.webp"
    captured = {}

    def post(url, **kwargs):
        captured.update(url=url, **kwargs)
        return _Response(
            {"data": [{"b64_json": base64.b64encode(b"image-bytes").decode()}]}
        )

    monkeypatch.setattr(image_generation.requests, "post", post)

    result = image_generation.generate_image(
        str(prompt_file),
        [],
        str(output),
        "16:9",
    )

    assert result == f"Successfully generated image to {output}"
    assert output.read_bytes() == b"image-bytes"
    assert captured["url"] == "https://images.example.test/v1/images/generations"
    assert captured["headers"]["Authorization"] == "Bearer test-key"
    assert captured["json"]["size"] == "1536x1024"
    assert captured["json"]["output_format"] == "webp"
    assert captured["timeout"] == 180


def test_openai_reference_images_use_multipart_edits(tmp_path, monkeypatch) -> None:
    _set_openai_env(monkeypatch)
    prompt_file = tmp_path / "prompt.txt"
    prompt_file.write_text("edit this", encoding="utf-8")
    reference = tmp_path / "reference.jpg"
    reference.write_bytes(b"reference-bytes")
    output = tmp_path / "edited.jpg"
    captured = {}

    def post(url, **kwargs):
        file_part = kwargs["files"][0]
        captured.update(
            url=url,
            data=kwargs["data"],
            field=file_part[0],
            filename=file_part[1][0],
            file_bytes=file_part[1][1].read(),
            mime=file_part[1][2],
        )
        return _Response(
            {"data": [{"image_base64": base64.b64encode(b"edited").decode()}]}
        )

    monkeypatch.setattr(image_generation.requests, "post", post)

    image_generation.generate_image(
        str(prompt_file),
        [str(reference)],
        str(output),
        "1:1",
    )

    assert captured == {
        "url": "https://images.example.test/v1/images/edits",
        "data": {
            "model": "gpt-image-2.5-flare",
            "prompt": "edit this",
            "n": 1,
            "size": "1024x1024",
            "output_format": "jpeg",
        },
        "field": "image[]",
        "filename": "reference.jpg",
        "file_bytes": b"reference-bytes",
        "mime": "image/jpeg",
    }
    assert output.read_bytes() == b"edited"


def test_openai_url_response_is_downloaded(tmp_path, monkeypatch) -> None:
    _set_openai_env(monkeypatch)
    output = tmp_path / "image.png"
    monkeypatch.setattr(
        image_generation.requests,
        "post",
        lambda *_args, **_kwargs: _Response(
            {"data": [{"url": "https://cdn.example.test/image.png"}]}
        ),
    )
    downloads = []

    def get(url, **kwargs):
        downloads.append((url, kwargs))
        return _Response(content=b"downloaded")

    monkeypatch.setattr(image_generation.requests, "get", get)

    image_generation._generate_image_openai("draw", [], str(output), "1:1")

    assert output.read_bytes() == b"downloaded"
    assert downloads == [("https://cdn.example.test/image.png", {"timeout": 120})]


def test_dall_e_rejects_unsupported_output_and_reference_edit(tmp_path, monkeypatch) -> None:
    _set_openai_env(monkeypatch)
    monkeypatch.setenv("IMAGE_GENERATION_MODEL", "dall-e-3")

    with pytest.raises(ValueError, match="must use a .png"):
        image_generation._generate_image_openai(
            "draw", [], str(tmp_path / "image.jpg"), "1:1"
        )
    with pytest.raises(ValueError, match="editing is not supported"):
        image_generation._generate_image_openai(
            "draw", [str(tmp_path / "ref.png")], str(tmp_path / "image.png"), "1:1"
        )


def test_provider_selection_prefers_existing_gemini_credentials(monkeypatch) -> None:
    monkeypatch.delenv("IMAGE_GENERATION_PROVIDER", raising=False)
    monkeypatch.setenv("GEMINI_API_KEY", "gemini")
    monkeypatch.setenv("IMAGE_GENERATION_API_KEY", "openai")

    assert image_generation._resolve_provider() == "gemini"
