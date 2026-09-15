from __future__ import annotations

import importlib.util
from pathlib import Path
from types import SimpleNamespace


SCRIPT = Path(__file__).parents[1] / "skills" / "public" / "video-generation" / "scripts" / "generate.py"


def _load_script():
    spec = importlib.util.spec_from_file_location("deerflow_video_generate", SCRIPT)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_generate_video_forwards_aspect_ratio(tmp_path, monkeypatch) -> None:
    module = _load_script()
    prompt = tmp_path / "prompt.txt"
    prompt.write_text("A quiet lake", encoding="utf-8")
    captured: dict = {}

    def fake_post(_url, *, headers, json):
        captured["request"] = json
        return SimpleNamespace(json=lambda: {"name": "operations/1"})

    def fake_get(_url, *, headers):
        return SimpleNamespace(
            json=lambda: {
                "done": True,
                "response": {
                    "generateVideoResponse": {
                        "generatedSamples": [{"video": {"uri": "https://video.example/out"}}]
                    }
                },
            }
        )

    monkeypatch.setenv("GEMINI_API_KEY", "test-key")
    monkeypatch.setattr(module.requests, "post", fake_post)
    monkeypatch.setattr(module.requests, "get", fake_get)
    monkeypatch.setattr(module, "download", lambda _url, _output: None)

    module.generate_video(str(prompt), [], str(tmp_path / "out.mp4"), "9:16")

    assert captured["request"]["parameters"] == {"aspectRatio": "9:16"}
