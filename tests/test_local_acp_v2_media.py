"""Real v2 bridge + daemon + image ingestion, with no model/network requests."""

from __future__ import annotations

import base64
import os
from pathlib import Path
from types import SimpleNamespace

import pytest

from deerflow.acp.config import LocalACPConfig
from deerflow.acp.daemon import ACPDaemon
from deerflow.acp.session_store import LocalACPSessionStore


class MediaRuntime:
    def __init__(self):
        self.images = []
        self.messages = []

    async def astream(self, session, message, *, input_images=None, **kwargs):
        self.images.append(input_images or [])
        self.messages.append(message)
        if False:
            yield None

    async def history(self, session_id):
        return []

    async def release_session(self, session_id):
        pass

    async def bind_client_mcp(self, session_id, binding):
        pass

    async def release_client_mcp(self, session_id):
        pass


@pytest.mark.parametrize("vision, image_count", [(False, 0), (True, 1), (True, 2)])
async def test_v2_negotiates_capabilities_and_delivers_media(
    tmp_path: Path, monkeypatch, vision: bool, image_count: int
):
    root = Path(__file__).resolve().parents[1]
    bridge = Path(
        os.getenv(
            "DEERFLOW_ACP_BRIDGE_BIN",
            str(root / "bridge/target/release/deerflow-acp.exe"),
        )
    )
    if not bridge.exists():
        pytest.skip(
            "Build the native bridge before running the v2 media integration test"
        )
    monkeypatch.syspath_prepend(str(root / "integrations/buzz"))
    from buzz_deerflow_adapter.acp_v2_client import DeerFlowACPV2Client

    model = SimpleNamespace(
        name="test-model", supports_vision=vision, display_name=None, description=None
    )
    app_config = SimpleNamespace(
        models=[model],
        get_default_model_name=lambda: model.name,
        get_model_config=lambda _: model,
    )
    monkeypatch.setattr("deerflow.acp.agent.get_app_config", lambda: app_config)
    uploads = tmp_path / "uploads"
    uploads.mkdir()
    monkeypatch.setattr(
        "deerflow.agents.image_inputs.ensure_uploads_dir", lambda _: uploads
    )
    config_path = tmp_path / "config.yaml"
    config_path.write_text("local_acp: {}\n", encoding="utf-8")
    config = LocalACPConfig(
        config_path=config_path,
        checkpointer_path=tmp_path / "checkpoints.db",
        session_store_path=tmp_path / "sessions.db",
    )
    runtime = MediaRuntime()
    store = LocalACPSessionStore(config.session_store_path)
    store.setup()
    daemon = ACPDaemon(config, store, runtime, tmp_path / "runtime")
    await daemon.start()
    client = DeerFlowACPV2Client(
        str(bridge),
        [
            "--protocol",
            "v2",
            "--config",
            str(config_path),
            "--runtime-dir",
            str(tmp_path / "runtime"),
            "--no-auto-start",
        ],
        tmp_path,
        timeout_seconds=45,
    )
    try:
        await client.open()
        assert client.supports_images is vision
        session = await client.attach_or_create(None)
        document = tmp_path / "report.pdf"
        document.write_bytes(b"%PDF-1.7\nreport")
        blocks = [
            {"type": "text", "text": "inspect these attachments"},
            {
                "type": "resource_link",
                "uri": document.as_uri(),
                "name": "report.pdf",
                "mimeType": "application/pdf",
                "size": document.stat().st_size,
            },
        ]
        size = (20 if image_count == 2 else 1) * 1024 * 1024
        image_data = b"\x89PNG\r\n\x1a\n" + b"x" * (size - 8)
        for _ in range(image_count):
            blocks.append(
                {
                    "type": "image",
                    "data": base64.b64encode(image_data).decode(),
                    "mimeType": "image/png",
                }
            )
        await client.prompt(session, blocks)
        assert "report.pdf" in runtime.messages[0]
        assert "/mnt/user-data/workspace/" in runtime.messages[0]
        if vision:
            assert len(runtime.images[0]) == image_count
            assert runtime.images[0][0]["size"] == len(image_data)
            stored = uploads / Path(runtime.images[0][0]["virtual_path"]).name
            assert stored.read_bytes() == image_data
        else:
            assert runtime.images == [[]]
    finally:
        await client.close()
        await daemon.close()
        store.close()
